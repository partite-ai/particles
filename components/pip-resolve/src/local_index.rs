//! Local wheel directory source.
//!
//! When the host sets `PARTICLES_PY_WHEEL_DIR`, it preopens that
//! directory at the guest path [`WHEELS_DIR`]. Its presence is the
//! only signal: we probe it, and when it's there we treat every `.whl`
//! inside as a highest-priority wheel source — consulted ahead of the
//! particle wheels index and PyPI ("look in /wheels first"). When the
//! dir isn't mounted (the common case), [`LocalIndex::load`] returns an
//! empty index and the resolver behaves exactly as before.
//!
//! Unlike PyPI, a local wheel carries no JSON metadata, so we read each
//! wheel's `Requires-Dist` directly out of its `*.dist-info/METADATA`
//! entry (a wheel is a zip archive). That lets a private package that
//! isn't published on PyPI at all still resolve its transitive deps.
//!
//! Local wheels are treated as host-vouched (any tag triple accepted,
//! bypassing the pure-Python filter): the user put them there
//! deliberately, same trust model as the particle wheels index.

use std::collections::BTreeMap;
use std::io::{Cursor, Read};

use crate::pypi::{normalize_for_url, PyPiDigests, PyPiFile};
use crate::Error;

/// Guest path the host preopens `PARTICLES_PY_WHEEL_DIR` at. Mirrored
/// on the Go side in internal/build/wacogo/pip_resolver.go.
pub const WHEELS_DIR: &str = "/wheels";

/// Scanned view of the local wheel directory. Empty when `/wheels`
/// isn't mounted.
#[derive(Default)]
pub struct LocalIndex {
    /// normalized dist name → (version string → local wheel file).
    /// First wheel seen wins for a given (name, version) pair.
    by_name: BTreeMap<String, BTreeMap<String, PyPiFile>>,
    /// (normalized name, version) → the wheel's `Requires-Dist` lines
    /// (PEP 508 strings), read from its METADATA.
    requires: BTreeMap<(String, String), Vec<String>>,
}

impl LocalIndex {
    /// Scan [`WHEELS_DIR`] if it's mounted. A missing/unmounted dir is
    /// not an error — it yields an empty index (resolver falls back to
    /// the network sources). Individual files that don't parse as
    /// wheels are skipped; a wheel we *do* recognize but can't read
    /// METADATA from is fatal, so a corrupt drop-in is surfaced.
    pub fn load() -> Result<LocalIndex, Error> {
        let mut idx = LocalIndex::default();
        let entries = match std::fs::read_dir(WHEELS_DIR) {
            Ok(entries) => entries,
            // Not mounted (PARTICLES_PY_WHEEL_DIR unset) → empty index,
            // resolver falls back to the network sources.
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(idx),
            // Any other error means the dir IS there but we couldn't
            // read it — surface it rather than silently degrading.
            Err(e) => {
                return Err(Error::ResolutionError(format!(
                    "opening {WHEELS_DIR} ({:?}): {e}",
                    e.kind()
                )))
            }
        };
        for entry in entries {
            let entry = entry.map_err(|e| {
                Error::ResolutionError(format!("reading {WHEELS_DIR}: {e}"))
            })?;
            let path = entry.path();
            let Some(filename) = path.file_name().and_then(|s| s.to_str()) else {
                continue;
            };
            if !filename.ends_with(".whl") {
                continue;
            }
            let Some((raw_name, version)) = parse_wheel_filename(filename) else {
                continue;
            };
            let name = normalize_for_url(&raw_name);
            let Some(path_str) = path.to_str() else {
                continue;
            };
            let requires = read_requires_dist(path_str).map_err(|e| {
                Error::ResolutionError(format!(
                    "local wheel {filename}: reading METADATA: {e}"
                ))
            })?;
            let file = PyPiFile {
                filename: filename.to_string(),
                url: String::new(),
                packagetype: "bdist_wheel".to_string(),
                yanked: false,
                digests: PyPiDigests {
                    sha256: String::new(),
                },
                host_vouched: true,
                local_path: Some(path_str.to_string()),
            };
            idx.by_name
                .entry(name.clone())
                .or_default()
                .entry(version.clone())
                .or_insert(file);
            idx.requires
                .entry((name, version))
                .or_insert(requires);
        }
        Ok(idx)
    }

    /// Local wheels available for `name` (PEP 503-normalized), keyed by
    /// version string. `None` when the dir ships nothing for it.
    pub fn files_for(&self, name: &str) -> Option<&BTreeMap<String, PyPiFile>> {
        self.by_name.get(name)
    }

    /// `Requires-Dist` for a locally-sourced (name, version), if the
    /// dir ships that exact version. Lets the resolver get transitive
    /// deps for a package that may not exist on PyPI at all.
    pub fn requires_dist(&self, name: &str, version: &str) -> Option<Vec<String>> {
        self.requires
            .get(&(name.to_string(), version.to_string()))
            .cloned()
    }
}

/// Parse a wheel filename into `(distribution, version)`. Per PEP 427
/// the filename is
/// `{distribution}-{version}(-{build})?-{python}-{abi}-{platform}.whl`,
/// and both distribution and version have their `-`s escaped, so the
/// first two `-`-separated fields are always name and version. Returns
/// `None` for anything that doesn't look like a wheel.
fn parse_wheel_filename(filename: &str) -> Option<(String, String)> {
    let stem = filename.strip_suffix(".whl")?;
    let parts: Vec<&str> = stem.split('-').collect();
    // distribution, version, (build?), python, abi, platform → ≥5.
    if parts.len() < 5 {
        return None;
    }
    Some((parts[0].to_string(), parts[1].to_string()))
}

/// Read the `Requires-Dist` headers out of a wheel's
/// `*.dist-info/METADATA` (the wheel is a zip archive). METADATA is an
/// RFC 822 header block followed by a blank line and the long
/// description; deps are headers, so we stop at the first blank line.
/// A wheel with no METADATA or no deps yields an empty list.
fn read_requires_dist(path: &str) -> Result<Vec<String>, Error> {
    // Read the whole wheel into memory and parse the zip from a Cursor:
    // zip parsing seeks (to find the end-of-central-directory record),
    // and seeking over the preopened wasi:filesystem descriptor returns
    // short reads. A single sequential std::fs::read is reliable, and an
    // in-memory Cursor gives the zip reader real Seek support.
    let bytes = std::fs::read(path)
        .map_err(|e| Error::ResolutionError(format!("read {path}: {e}")))?;
    let mut archive = zip::ZipArchive::new(Cursor::new(bytes))
        .map_err(|e| Error::ResolutionError(format!("open zip {path}: {e}")))?;

    // Locate the METADATA entry. Its name is `<dist>-<ver>.dist-info/
    // METADATA`; match on the suffix so we don't depend on the exact
    // dist-info dir name.
    let mut meta_name: Option<String> = None;
    for i in 0..archive.len() {
        let entry = archive
            .by_index(i)
            .map_err(|e| Error::ResolutionError(format!("zip entry {i}: {e}")))?;
        let name = entry.name();
        if name.ends_with(".dist-info/METADATA") {
            meta_name = Some(name.to_string());
            break;
        }
    }
    let Some(meta_name) = meta_name else {
        return Ok(Vec::new());
    };

    let mut entry = archive
        .by_name(&meta_name)
        .map_err(|e| Error::ResolutionError(format!("zip entry {meta_name}: {e}")))?;
    let mut content = String::new();
    entry
        .read_to_string(&mut content)
        .map_err(|e| Error::ResolutionError(format!("read {meta_name}: {e}")))?;

    let mut out = Vec::new();
    for line in content.lines() {
        // Header block ends at the first blank line; the long
        // description follows and never carries Requires-Dist.
        if line.is_empty() {
            break;
        }
        if let Some(rest) = line.strip_prefix("Requires-Dist:") {
            out.push(rest.trim().to_string());
        }
    }
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_wheel_filename_basic() {
        assert_eq!(
            parse_wheel_filename("idna-3.7-py3-none-any.whl"),
            Some(("idna".to_string(), "3.7".to_string()))
        );
    }

    #[test]
    fn parse_wheel_filename_with_build_tag() {
        // distribution, version, build, python, abi, platform.
        assert_eq!(
            parse_wheel_filename("foo-1.2.3-1-cp314-abi3-any.whl"),
            Some(("foo".to_string(), "1.2.3".to_string()))
        );
    }

    #[test]
    fn parse_wheel_filename_underscored_name() {
        assert_eq!(
            parse_wheel_filename("typing_extensions-4.12.2-py3-none-any.whl"),
            Some(("typing_extensions".to_string(), "4.12.2".to_string()))
        );
    }

    #[test]
    fn parse_wheel_filename_rejects_non_wheel() {
        assert_eq!(parse_wheel_filename("idna-3.7.tar.gz"), None);
        assert_eq!(parse_wheel_filename("not-a-wheel.txt"), None);
        assert_eq!(parse_wheel_filename("too-few.whl"), None);
    }
}
