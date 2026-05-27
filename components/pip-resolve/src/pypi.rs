//! Thin PyPI JSON API client.
//!
//! Two endpoints, both documented at
//! https://docs.pypi.org/api/json/:
//!
//!   - `/pypi/<package>/json`            — full release listing
//!   - `/pypi/<package>/<version>/json`  — single-release metadata
//!
//! For resolution we only need the second form: it carries
//! `requires_dist` (PEP 508 strings for transitive deps), the list of
//! published files for that version, and each file's sha256. We pick
//! versions from the first form's `releases` map by iterating PEP 440
//! versions in descending order.
//!
//! Network access uses `wstd` (HTTP over wasi:http) — the host wires
//! `wasi:http` to a real HTTP client at component-instantiation time.

use crate::Error;
use serde::Deserialize;
use wstd::http::{Client, Method, Request};
use wstd::io::{empty, AsyncRead};

const PYPI_BASE: &str = "https://pypi.org/pypi";

/// One file (.whl or .tar.gz) published under a release.
#[derive(Clone, Debug, Deserialize)]
pub struct PyPiFile {
    pub filename: String,
    pub url: String,
    pub packagetype: String, // "bdist_wheel" or "sdist" usually
    #[serde(default)]
    pub yanked: bool,
    pub digests: PyPiDigests,
    /// True for files sourced from the particle wheels index (a
    /// PEP 503 simple repo of wasm-cross-compiled wheels). PyPI's
    /// JSON response won't carry this flag, so serde defaults to
    /// false; particle_index.rs sets it explicitly. The resolver
    /// uses this as the marker for "the host has vouched this wheel
    /// is loadable; skip the pure-Python wheel filter."
    #[serde(default)]
    pub host_vouched: bool,
}

#[derive(Clone, Debug, Deserialize)]
pub struct PyPiDigests {
    pub sha256: String,
}

/// `/pypi/<name>/json` — used to enumerate available versions.
#[derive(Debug, Deserialize)]
pub struct ReleaseIndex {
    /// Map from version string → files published under that version.
    /// Yanked or empty releases stay in the map; we filter at use.
    pub releases: std::collections::BTreeMap<String, Vec<PyPiFile>>,
}

/// `/pypi/<name>/<version>/json` — used to read transitive deps.
/// We discard the `urls` array on this endpoint (we already have the
/// chosen file from the release-index call); only `info.requires_dist`
/// is consumed.
#[derive(Debug, Deserialize)]
pub struct VersionInfo {
    pub info: VersionMeta,
}

#[derive(Debug, Deserialize)]
pub struct VersionMeta {
    /// PEP 508 dependency specifiers. Null when the project has no
    /// declared deps; default to empty when absent.
    #[serde(default)]
    pub requires_dist: Option<Vec<String>>,
}

pub async fn fetch_release_index(name: &str) -> Result<ReleaseIndex, Error> {
    let url = format!("{PYPI_BASE}/{}/json", normalize_for_url(name));
    let body = http_get_json(&url).await?;
    serde_json::from_slice(&body)
        .map_err(|e| Error::InvalidPypiResponse(format!("{name}: parse releases: {e}")))
}

pub async fn fetch_version_info(name: &str, version: &str) -> Result<VersionInfo, Error> {
    let url = format!(
        "{PYPI_BASE}/{}/{}/json",
        normalize_for_url(name),
        version
    );
    let body = http_get_json(&url).await?;
    serde_json::from_slice(&body)
        .map_err(|e| Error::InvalidPypiResponse(format!("{name}@{version}: parse info: {e}")))
}

pub async fn fetch_wheel_bytes(file: &PyPiFile) -> Result<Vec<u8>, Error> {
    http_get_raw(&file.url).await
}

async fn http_get_json(url: &str) -> Result<Vec<u8>, Error> {
    let request = Request::builder()
        .method(Method::GET)
        .uri(url)
        .header("Accept", "application/json")
        .body(empty())
        .map_err(|e| Error::NetworkError(format!("build req {url}: {e}")))?;

    let mut response = Client::new()
        .send(request)
        .await
        .map_err(|e| Error::NetworkError(format!("send {url}: {e}")))?;

    let status = response.status();
    if !status.is_success() {
        return Err(Error::NetworkError(format!("GET {url} -> HTTP {status}")));
    }

    let mut buf = Vec::new();
    response
        .body_mut()
        .read_to_end(&mut buf)
        .await
        .map_err(|e| Error::NetworkError(format!("read {url}: {e}")))?;
    Ok(buf)
}

async fn http_get_raw(url: &str) -> Result<Vec<u8>, Error> {
    let request = Request::builder()
        .method(Method::GET)
        .uri(url)
        .body(empty())
        .map_err(|e| Error::NetworkError(format!("build req {url}: {e}")))?;

    let mut response = Client::new()
        .send(request)
        .await
        .map_err(|e| Error::NetworkError(format!("send {url}: {e}")))?;

    let status = response.status();
    if !status.is_success() {
        return Err(Error::NetworkError(format!("GET {url} -> HTTP {status}")));
    }

    let mut buf = Vec::new();
    response
        .body_mut()
        .read_to_end(&mut buf)
        .await
        .map_err(|e| Error::NetworkError(format!("read {url}: {e}")))?;
    Ok(buf)
}

/// Fetch a text resource, returning `Ok(None)` for 404. Used by the
/// particle wheels index: most packages aren't there, so a 404 must
/// be a routine fall-through case, not an error.
pub async fn http_get_text_or_404(url: &str) -> Result<Option<String>, Error> {
    let request = Request::builder()
        .method(Method::GET)
        .uri(url)
        .header("Accept", "text/html, application/vnd.pypi.simple.v1+html")
        .body(empty())
        .map_err(|e| Error::NetworkError(format!("build req {url}: {e}")))?;

    let mut response = Client::new()
        .send(request)
        .await
        .map_err(|e| Error::NetworkError(format!("send {url}: {e}")))?;

    let status = response.status();
    if status.as_u16() == 404 {
        return Ok(None);
    }
    if !status.is_success() {
        return Err(Error::NetworkError(format!("GET {url} -> HTTP {status}")));
    }

    let mut buf = Vec::new();
    response
        .body_mut()
        .read_to_end(&mut buf)
        .await
        .map_err(|e| Error::NetworkError(format!("read {url}: {e}")))?;
    let text = String::from_utf8(buf)
        .map_err(|e| Error::NetworkError(format!("decode {url} as utf-8: {e}")))?;
    Ok(Some(text))
}

/// PyPI's URL path expects the canonical project name. PEP 503
/// normalization (lowercased, runs of `_`, `-`, `.` collapsed to a
/// single `-`) is what `pip` itself uses for the Simple API and the
/// JSON API tolerates the same. Applying it here means a dep like
/// `PyJWT` resolves the same way `pip install pyjwt` does.
pub fn normalize_for_url(name: &str) -> String {
    let mut out = String::with_capacity(name.len());
    let mut prev_sep = false;
    for c in name.chars() {
        if c == '_' || c == '-' || c == '.' {
            if !prev_sep && !out.is_empty() {
                out.push('-');
            }
            prev_sep = true;
        } else {
            out.push(c.to_ascii_lowercase());
            prev_sep = false;
        }
    }
    while out.ends_with('-') {
        out.pop();
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn normalize_for_url_matches_pep_503() {
        assert_eq!(normalize_for_url("PyJWT"), "pyjwt");
        assert_eq!(normalize_for_url("python-dateutil"), "python-dateutil");
        assert_eq!(normalize_for_url("typing_extensions"), "typing-extensions");
        assert_eq!(normalize_for_url("Foo.Bar_Baz"), "foo-bar-baz");
        assert_eq!(normalize_for_url("Foo--Bar"), "foo-bar");
    }
}
