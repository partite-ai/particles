//! Version solving with pubgrub.
//!
//! The async/sync impedance is the only architectural wrinkle: pubgrub's
//! `DependencyProvider` is synchronous (`&self`, no `await`s), but our
//! PyPI client is async (built on wasi:http, which is poll-based). And
//! `wstd::block_on` panics on nested calls, so we can't just `block_on`
//! inside the trait methods.
//!
//! Pattern: **outer async loop drives a sync provider over a cache**.
//!
//!   1. Outer loop pre-fetches release indices for every top-level
//!      package (so pubgrub starts with something).
//!   2. Run pubgrub against `CachedProvider`. When pubgrub asks for
//!      data the cache doesn't have, the provider returns a typed
//!      error (`Error::NeedReleaseIndex` / `NeedVersionInfo`).
//!   3. Outer loop fetches the missing piece and retries pubgrub.
//!   4. Repeat until pubgrub returns `Ok(solution)` or a non-recoverable
//!      error.
//!
//! The retry pattern means pubgrub may re-do work each iteration. For
//! the v1 use case (a handful of top-level reqs from a Particlefile,
//! shallow transitives) that's fine — resolution typically converges
//! in a few iterations.
//!
//! Wheel-filter integration: a version that has no `*-none-any` wheel
//! is treated by `choose_version` as if it doesn't exist (skipped),
//! letting pubgrub backtrack to an older satisfying version. If *every*
//! satisfying version of a top-level dep lacks a pure-Python wheel, we
//! propagate `NoPurePythonWheel` rather than returning `None` from
//! `choose_version` — surfaces a clear error rather than the cryptic
//! pubgrub "no solution" message.

use crate::particle_index;
use crate::pypi::{self, PyPiFile, ReleaseIndex, VersionInfo};
use crate::wheel_tag;
use crate::Error;

use pep440_rs::Version;
use pep508_rs::Requirement;
use pubgrub::{
    Dependencies, DependencyConstraints, DependencyProvider, PackageResolutionStatistics, Range,
    SelectedDependencies,
};
use sha2::Digest;
use std::cell::RefCell;
use std::collections::BTreeMap;
use std::convert::Infallible;
use std::str::FromStr;

use crate::exports::particle::build::pip_installer::ResolvedWheel;

/// One node in the resolution graph after pubgrub picks a solution.
/// `file` carries the URL + digest fetch_all needs. Host-vouched
/// files (from the particle index) are flagged via
/// `file.host_vouched` so fetch_all can use the file's own sha256
/// verbatim (`sha256:<hex>`) instead of recomputing.
#[derive(Debug)]
pub struct Resolved {
    pub name: String,
    pub version: String,
    pub file: PyPiFile,
}

/// Package identifier in the pubgrub graph. We model the user's
/// requirements as transitive deps of a synthetic Root package; that's
/// the standard pubgrub pattern for "resolve this set."
#[derive(Clone, PartialEq, Eq, Hash, Debug, PartialOrd, Ord)]
pub enum Package {
    Root,
    Named(String),
}

impl std::fmt::Display for Package {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Package::Root => f.write_str("<root>"),
            Package::Named(n) => f.write_str(n),
        }
    }
}

/// Sentinel version for Root. pubgrub demands a starting (package,
/// version); we never store Root in the result.
fn root_version() -> Version {
    Version::from_str("0.0.0").unwrap()
}

// -----------------------------------------------------------------------------
// Cache
// -----------------------------------------------------------------------------

/// Per-package fetched metadata. Holds PyPI's release index and,
/// for packages also present in the particle wheels index, the
/// files we fetched from there (keyed by version). Particle-index
/// versions can refer to versions that exist on PyPI (we add to
/// PyPI's release files for that version, marking them
/// `host_vouched`) or to versions that PyPI doesn't ship — in the
/// latter case we synthesize a release entry so pubgrub treats them
/// as choosable.
#[derive(Default)]
struct Cache {
    release_indices: BTreeMap<String, ReleaseIndex>,
    version_infos: BTreeMap<(String, String), VersionInfo>,
}

// -----------------------------------------------------------------------------
// Resolution
// -----------------------------------------------------------------------------

/// Fetch PyPI's release index for `name` AND consult the particle
/// wheels index, merging any particle-index files into the result.
/// Particle index entries are marked `host_vouched = true` so
/// `choose_version` accepts them without the pure-Python wheel
/// filter. Versions present in the particle index but not on PyPI
/// get synthesized release entries so pubgrub can choose them.
async fn fetch_combined_release_index(name: &str) -> Result<ReleaseIndex, Error> {
    let mut pypi_idx = pypi::fetch_release_index(name).await?;
    if let Some(particle_idx) = particle_index::fetch_release_index(name).await? {
        for (version, particle_files) in particle_idx.releases {
            pypi_idx
                .releases
                .entry(version)
                .or_default()
                .extend(particle_files);
        }
    }
    Ok(pypi_idx)
}

/// Resolve `top_level` reqs to a flat list of pinned (name, version,
/// file). Drives pubgrub through the cache+retry loop described in
/// the module docstring. For each package we consult, the cache
/// holds the merged PyPI + particle-wheels-index release set; the
/// resolver prefers particle-index files (host-vouched) over
/// PyPI's pure-Python wheels for the same version.
pub async fn resolve(top_level: &[Requirement]) -> Result<Vec<Resolved>, Error> {
    let cache = RefCell::new(Cache::default());

    // Pre-fetch release indices for every top-level package.
    for req in top_level {
        let name = pypi::normalize_for_url(req.name.as_ref());
        if !cache.borrow().release_indices.contains_key(&name) {
            let idx = fetch_combined_release_index(&name).await?;
            cache.borrow_mut().release_indices.insert(name, idx);
        }
    }

    // Pubgrub-on-cache loop. Each iteration runs pubgrub end-to-end
    // and either succeeds, fails recoverably (needs more data), or
    // fails irrecoverably (real conflict or PyPI error we already
    // surfaced).
    let solution: SelectedDependencies<CachedProvider<'_>> = loop {
        let provider = CachedProvider {
            cache: &cache,
            top_level,
        };
        match pubgrub::resolve(&provider, Package::Root, root_version()) {
            Ok(sel) => break sel,
            Err(pubgrub::PubGrubError::ErrorRetrievingDependencies { source, .. }) => {
                match source {
                    ProviderError::NeedReleaseIndex(name) => {
                        let idx = fetch_combined_release_index(&name).await?;
                        cache.borrow_mut().release_indices.insert(name, idx);
                    }
                    ProviderError::NeedVersionInfo(name, version) => {
                        let info = pypi::fetch_version_info(&name, &version).await?;
                        cache
                            .borrow_mut()
                            .version_infos
                            .insert((name, version), info);
                    }
                    ProviderError::Fatal(e) => return Err(e),
                }
            }
            Err(pubgrub::PubGrubError::ErrorChoosingVersion { source, .. }) => {
                match source {
                    ProviderError::NeedReleaseIndex(name) => {
                        let idx = fetch_combined_release_index(&name).await?;
                        cache.borrow_mut().release_indices.insert(name, idx);
                    }
                    ProviderError::NeedVersionInfo(name, version) => {
                        let info = pypi::fetch_version_info(&name, &version).await?;
                        cache
                            .borrow_mut()
                            .version_infos
                            .insert((name, version), info);
                    }
                    ProviderError::Fatal(e) => return Err(e),
                }
            }
            Err(pubgrub::PubGrubError::NoSolution(reason)) => {
                // DerivationTree's only formatter is Debug — pubgrub
                // provides a richer reporter (`DefaultStringReporter`)
                // for user-facing diffs, but the Debug rendering is
                // good enough for v1 error surfaces.
                return Err(Error::ResolutionError(format!("no solution: {:?}", reason)));
            }
            Err(other) => {
                return Err(Error::ResolutionError(format!("pubgrub: {other}")));
            }
        }
    };

    // Materialize. Skip the synthetic Root entry. Iterate in stable
    // (alphabetic) order so Particle.lock is reproducible.
    let mut out = Vec::with_capacity(solution.len());
    let cache_ref = cache.borrow();
    for (pkg, version) in &solution {
        let name = match pkg {
            Package::Root => continue,
            Package::Named(n) => n.clone(),
        };
        let idx = cache_ref
            .release_indices
            .get(&name)
            .expect("cache hit guaranteed: provider populated this entry");
        let file = pick_usable_wheel(idx, version)
            .expect("provider only chose versions with a usable wheel");
        out.push(Resolved {
            name,
            version: version.to_string(),
            file,
        });
    }
    out.sort_by(|a, b| a.name.cmp(&b.name));
    Ok(out)
}

// -----------------------------------------------------------------------------
// CachedProvider — sync, talks to the cache + the top-level constraints.
// -----------------------------------------------------------------------------

struct CachedProvider<'a> {
    cache: &'a RefCell<Cache>,
    top_level: &'a [Requirement],
}

/// Provider-layer errors. The recoverable variants tell the outer
/// loop "fetch X, then retry"; Fatal exits resolution.
#[derive(Debug)]
enum ProviderError {
    NeedReleaseIndex(String),
    NeedVersionInfo(String, String),
    Fatal(Error),
}

impl std::fmt::Display for ProviderError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ProviderError::NeedReleaseIndex(n) => write!(f, "need release index for {n}"),
            ProviderError::NeedVersionInfo(n, v) => write!(f, "need version info for {n}@{v}"),
            ProviderError::Fatal(e) => write!(f, "{e:?}"),
        }
    }
}

impl std::error::Error for ProviderError {}

impl<'a> DependencyProvider for CachedProvider<'a> {
    type P = Package;
    type V = Version;
    type VS = Range<Version>;
    type M = String;
    type Priority = (u8, std::cmp::Reverse<usize>);
    type Err = ProviderError;

    fn prioritize(
        &self,
        package: &Self::P,
        _range: &Self::VS,
        _stats: &PackageResolutionStatistics,
    ) -> Self::Priority {
        // Root first; then break ties by alphabetical order via the
        // reversed string length, which is a cheap, deterministic
        // pseudo-tiebreaker. (Real performance heuristics like
        // "fewest-candidates first" would need version counts; for
        // shallow particle graphs the simple order suffices.)
        match package {
            Package::Root => (0, std::cmp::Reverse(0)),
            Package::Named(n) => (1, std::cmp::Reverse(n.len())),
        }
    }

    fn choose_version(
        &self,
        package: &Self::P,
        range: &Self::VS,
    ) -> Result<Option<Self::V>, Self::Err> {
        match package {
            Package::Root => {
                // Root is virtual; its only "version" is the sentinel.
                if range.contains(&root_version()) {
                    Ok(Some(root_version()))
                } else {
                    Ok(None)
                }
            }
            Package::Named(name) => {
                let cache = self.cache.borrow();
                let idx = cache
                    .release_indices
                    .get(name)
                    .ok_or_else(|| ProviderError::NeedReleaseIndex(name.clone()))?;
                let mut candidates: Vec<Version> = idx
                    .releases
                    .keys()
                    .filter_map(|s| Version::from_str(s).ok())
                    .filter(|v| !v.is_pre() && !v.is_dev())
                    .collect();
                candidates.sort_by(|a, b| b.cmp(a));

                // First pass: pick any version in range that has a
                // usable wheel — either a particle-index file
                // (host-vouched, any tag triple) or PyPI's
                // pure-Python (`*-none-any.whl`).
                for v in &candidates {
                    if range.contains(v) && pick_usable_wheel(idx, v).is_some() {
                        return Ok(Some(v.clone()));
                    }
                }

                // No in-range pure-Python wheel. Two distinct
                // failure modes — we want to tell them apart so the
                // user gets the right error:
                //
                //   (a) Some version of this package somewhere has a
                //       pure wheel, just not under the current
                //       constraints → return None and let pubgrub
                //       backtrack the constraints.
                //
                //   (b) NO version of this package ever publishes a
                //       pure-Python wheel (pyyaml, lxml, …) → fatal
                //       NoPurePythonWheel. Pubgrub would otherwise
                //       report a cryptic "no solution," which sends
                //       the author hunting for the wrong fix.
                let any_usable = candidates.iter().any(|v| pick_usable_wheel(idx, v).is_some());
                if !any_usable {
                    return Err(ProviderError::Fatal(Error::NoPurePythonWheel(format!(
                        "{name}: no usable wheel — every published \
                         version carries a compiled-ABI or platform tag \
                         on PyPI and the particle wheels index doesn't \
                         ship a cross-build for this package"
                    ))));
                }
                Ok(None)
            }
        }
    }

    fn get_dependencies(
        &self,
        package: &Self::P,
        version: &Self::V,
    ) -> Result<Dependencies<Self::P, Self::VS, Self::M>, Self::Err> {
        match package {
            Package::Root => {
                // Root's "deps" are the user's top-level requirements.
                let mut deps: DependencyConstraints<Package, Range<Version>> =
                    DependencyConstraints::default();
                for req in self.top_level {
                    if should_skip_marker(req) {
                        continue;
                    }
                    let name = pypi::normalize_for_url(req.name.as_ref());
                    let r = requirement_to_range(req);
                    deps.insert(Package::Named(name), r);
                }
                Ok(Dependencies::Available(deps))
            }
            Package::Named(name) => {
                let cache = self.cache.borrow();
                let key = (name.clone(), version.to_string());
                let info = match cache.version_infos.get(&key) {
                    Some(info) => info,
                    None => {
                        return Err(ProviderError::NeedVersionInfo(
                            name.clone(),
                            version.to_string(),
                        ))
                    }
                };
                let empty = Vec::new();
                let rds = info.info.requires_dist.as_ref().unwrap_or(&empty);
                Ok(Dependencies::Available(parse_requires_dist(name, version, rds)?))
            }
        }
    }

    fn should_cancel(&self) -> Result<(), Self::Err> {
        Ok(())
    }
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

/// Convert a PEP 508 requirement's version specifier into a pubgrub
/// `Range<Version>`. pep440_rs already provides
/// `From<VersionSpecifiers> for Ranges<Version>`; we just need to
/// pull the specifier out of the requirement.
fn requirement_to_range(req: &Requirement) -> Range<Version> {
    match &req.version_or_url {
        Some(pep508_rs::VersionOrUrl::VersionSpecifier(specs)) => {
            Range::from(specs.clone())
        }
        _ => Range::full(),
    }
}

/// True if this requirement should be ignored by the resolver. v1
/// drops `extra == "..."` markers (we don't yet expose extras through
/// the WIT interface) and accepts everything else.
fn should_skip_marker(req: &Requirement) -> bool {
    if req.marker.is_true() {
        return false;
    }
    match req.marker.try_to_string() {
        Some(s) => s.contains("extra"),
        None => false,
    }
}

/// Decode a Requires-Dist list (PEP 508 strings) into pubgrub
/// `DependencyConstraints`. Same shape PyPI's `info.requires_dist`
/// uses, so locals and PyPI-sourced versions share this code path.
fn parse_requires_dist(
    name: &str,
    version: &Version,
    rds: &[String],
) -> Result<DependencyConstraints<Package, Range<Version>>, ProviderError> {
    let mut deps: DependencyConstraints<Package, Range<Version>> =
        DependencyConstraints::default();
    for rd in rds {
        let child = match Requirement::from_str(rd) {
            Ok(r) => r,
            Err(e) => {
                return Err(ProviderError::Fatal(Error::ResolutionError(format!(
                    "{name}@{version}: requires_dist entry {rd:?}: {e}"
                ))));
            }
        };
        if should_skip_marker(&child) {
            continue;
        }
        let child_name = pypi::normalize_for_url(child.name.as_ref());
        let r = requirement_to_range(&child);
        let key = Package::Named(child_name);
        // If the same dep appears more than once (multiple env-marker
        // entries that both apply), intersect the constraints.
        match deps.get(&key) {
            Some(prev) => {
                let merged = prev.intersection(&r);
                deps.insert(key, merged);
            }
            None => {
                deps.insert(key, r);
            }
        }
    }
    Ok(deps)
}

/// Find a usable wheel for a specific version. Two acceptance paths:
///
///   1. Host-vouched: any wheel from the particle wheels index. The
///      tag triple isn't checked — the host certifies these were
///      built for the right ABI (cryptography-48.0.0-cp314-abi3-any
///      and similar).
///   2. PyPI pure-Python: `<dist>-<version>-<pytag>-none-any.whl` on
///      PyPI. Anything narrower (cp314-cp314-manylinux_2_28_x86_64,
///      etc.) is rejected because the wasi-CPython we ship can't
///      load native extensions.
///
/// Host-vouched files are preferred when both kinds are present for
/// the same version, so a particle-index cross-build always wins
/// over PyPI's pure-Python alternative (this matters for packages
/// that ship both, e.g., when a future particle-index version
/// supersedes a slower pure-Python implementation).
fn pick_usable_wheel(idx: &ReleaseIndex, version: &Version) -> Option<PyPiFile> {
    let key = version.to_string();
    let files = idx.releases.get(&key)?;
    if files.iter().all(|f| f.yanked) {
        return None;
    }
    // Pass 1: host-vouched.
    if let Some(f) = files.iter().find(|f| {
        f.packagetype == "bdist_wheel" && !f.yanked && f.host_vouched
    }) {
        return Some(f.clone());
    }
    // Pass 2: PyPI pure-Python.
    files
        .iter()
        .find(|f| {
            f.packagetype == "bdist_wheel"
                && !f.yanked
                && !f.host_vouched
                && wheel_tag::is_pure_python_wheel(&f.filename)
        })
        .cloned()
}

// -----------------------------------------------------------------------------
// Fetch phase — pubgrub picked; we download. Host-vouched files
// from the particle index and PyPI files share the same path:
// follow the URL, verify the published sha256.
// -----------------------------------------------------------------------------

pub async fn fetch_all(items: &[Resolved]) -> Result<Vec<ResolvedWheel>, Error> {
    let mut out = Vec::with_capacity(items.len());
    for r in items {
        let file = &r.file;
        let bytes = pypi::fetch_wheel_bytes(file).await?;
        let actual = sha256_hex(&bytes);
        // Particle-index entries may publish without a hash (older
        // PEP 503 indexes didn't always carry one). When we have a
        // published digest we verify; when we don't, we trust the
        // bytes and stamp the computed hash.
        if !file.digests.sha256.is_empty()
            && !actual.eq_ignore_ascii_case(&file.digests.sha256)
        {
            return Err(Error::IntegrityMismatch(format!(
                "{}@{}: expected sha256={}, got {}",
                r.name, r.version, file.digests.sha256, actual
            )));
        }
        out.push(ResolvedWheel {
            name: r.name.clone(),
            version: r.version.clone(),
            sha256: format!("sha256:{actual}"),
            filename: file.filename.clone(),
            wheel_bytes: bytes,
        });
    }
    Ok(out)
}

fn sha256_hex(bytes: &[u8]) -> String {
    let mut hasher = sha2::Sha256::new();
    hasher.update(bytes);
    hex::encode(hasher.finalize())
}

// Compile-time sanity: pubgrub's Err bound is Error+'static; assert
// our ProviderError meets it. If pubgrub bumps the bound in a future
// version this assertion catches the breakage at compile time
// rather than via an inscrutable trait-bound error.
const _: fn() = || {
    fn assert_impl<E: std::error::Error + 'static>() {}
    assert_impl::<ProviderError>();
    let _ = std::any::TypeId::of::<Infallible>();
};
