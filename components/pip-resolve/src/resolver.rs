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
/// We keep the file we'll fetch attached so the post-resolution fetch
/// phase doesn't need a second PyPI lookup.
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

/// Per-call PyPI metadata cache. Lives behind a RefCell because the
/// pubgrub trait methods take `&self`.
#[derive(Default)]
struct Cache {
    release_indices: BTreeMap<String, ReleaseIndex>,
    version_infos: BTreeMap<(String, String), VersionInfo>,
}

// -----------------------------------------------------------------------------
// Resolution
// -----------------------------------------------------------------------------

/// Resolve `top_level` reqs to a flat list of pinned (name, version,
/// file). Drives pubgrub through the cache+retry loop described in the
/// module docstring.
pub async fn resolve(top_level: &[Requirement]) -> Result<Vec<Resolved>, Error> {
    let cache = RefCell::new(Cache::default());

    // Pre-fetch release indices for every top-level package so
    // pubgrub's first pass has something to chew on.
    for req in top_level {
        let name = pypi::normalize_for_url(req.name.as_ref());
        if !cache.borrow().release_indices.contains_key(&name) {
            let idx = pypi::fetch_release_index(&name).await?;
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
                        let idx = pypi::fetch_release_index(&name).await?;
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
                        let idx = pypi::fetch_release_index(&name).await?;
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
        // The provider already vetted that this (name, version) has a
        // pure-Python wheel — grab it again from the cache.
        let idx = cache_ref
            .release_indices
            .get(&name)
            .expect("cache hit guaranteed: provider populated this entry");
        let file = pick_pure_wheel(idx, version)
            .expect("provider only chose versions with a pure-Python wheel");
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

                // First pass: any version that's both in range AND
                // ships a pure-Python wheel. This is what pubgrub
                // normally wants.
                for v in &candidates {
                    if range.contains(v) && pick_pure_wheel(idx, v).is_some() {
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
                let any_pure = candidates.iter().any(|v| pick_pure_wheel(idx, v).is_some());
                if !any_pure {
                    return Err(ProviderError::Fatal(Error::NoPurePythonWheel(format!(
                        "{name}: every published wheel carries a compiled-ABI \
                         or platform tag; Python particles can use pure-Python \
                         wheels only"
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

                let mut deps: DependencyConstraints<Package, Range<Version>> =
                    DependencyConstraints::default();
                if let Some(rds) = &info.info.requires_dist {
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
                        // If the same dep appears more than once
                        // (multiple environment-marker entries that
                        // both apply), intersect the constraints.
                        let key = Package::Named(child_name);
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
                }
                Ok(Dependencies::Available(deps))
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

/// Find a pure-Python wheel for a specific version, if one exists.
/// Looks at the release index's files for the exact version key —
/// PyPI normalizes version strings in the JSON, so we match against
/// the version's `to_string()` form.
fn pick_pure_wheel(idx: &ReleaseIndex, version: &Version) -> Option<PyPiFile> {
    let key = version.to_string();
    let files = idx.releases.get(&key)?;
    if files.iter().all(|f| f.yanked) {
        return None;
    }
    files
        .iter()
        .find(|f| {
            f.packagetype == "bdist_wheel"
                && !f.yanked
                && wheel_tag::is_pure_python_wheel(&f.filename)
        })
        .cloned()
}

// -----------------------------------------------------------------------------
// Fetch phase (unchanged from greedy version — pubgrub picks; we fetch).
// -----------------------------------------------------------------------------

pub async fn fetch_all(items: &[Resolved]) -> Result<Vec<ResolvedWheel>, Error> {
    let mut out = Vec::with_capacity(items.len());
    for r in items {
        let bytes = pypi::fetch_wheel_bytes(&r.file).await?;
        let actual = sha256_hex(&bytes);
        if !actual.eq_ignore_ascii_case(&r.file.digests.sha256) {
            return Err(Error::IntegrityMismatch(format!(
                "{}@{}: expected sha256={}, got {}",
                r.name, r.version, r.file.digests.sha256, actual
            )));
        }
        out.push(ResolvedWheel {
            name: r.name.clone(),
            version: r.version.clone(),
            sha256: format!("sha256:{actual}"),
            filename: r.file.filename.clone(),
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
