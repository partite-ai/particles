//! Particle wheels index — a PEP 503 simple-format repo of
//! wasm-cross-compiled wheels for packages whose published PyPI
//! wheels are all platform-tagged (cryptography, cffi, …).
//!
//! Published at https://partite-ai.github.io/particle-python-wheels/
//! and queried at `<base>/simple/<normalized-name>/`. Each per-package
//! page is HTML with one `<a>` tag per file:
//!
//!     <a href="../packages/cryptography-48.0.0-cp314-abi3-any.whl#sha256=...">
//!         cryptography-48.0.0-cp314-abi3-any.whl</a>
//!
//! The href fragment (`#sha256=…`) is the file's hash, per PEP 503.
//!
//! Routing in the resolver:
//!   - 404 from the per-package URL → `Ok(None)`. Package isn't in
//!     the particle index. Fall through to PyPI-only resolution.
//!   - Any other error → propagate.
//!   - 200 with files → we mark each file `host_vouched = true` so
//!     `pick_pure_wheel` accepts it without the platform-tag filter.
//!
//! We don't fetch `requires-dist` from the particle index — it isn't
//! exposed by PEP 503 simple. The resolver continues to ask PyPI's
//! JSON API for transitive deps. Every package we'd publish in the
//! particle index is also on PyPI (we're shipping cross-builds of
//! existing PyPI packages), so this is safe.
//!
//! HTML parser scope: PEP 503 mandates a deliberately tiny format
//! — `<a>` tags with hrefs. We do a hand-rolled scanner rather than
//! pulling in a full HTML crate; the format is regular enough that
//! attribute extraction is ~30 lines.

use crate::pypi::{self, PyPiDigests, PyPiFile, ReleaseIndex};
use crate::Error;

use std::collections::BTreeMap;

const PARTICLE_INDEX_BASE: &str = "https://partite-ai.github.io/particle-python-wheels/simple";

/// Fetch the particle index entry for `name`, or return `Ok(None)`
/// if the package isn't in the index. Errors propagate.
pub async fn fetch_release_index(name: &str) -> Result<Option<ReleaseIndex>, Error> {
    let url = format!("{}/{}/", PARTICLE_INDEX_BASE, pypi::normalize_for_url(name));
    let html = match pypi::http_get_text_or_404(&url).await? {
        None => return Ok(None),
        Some(s) => s,
    };

    let files = parse_simple_html(&html, &url)?;
    if files.is_empty() {
        // 200 with no files is effectively the same as 404 for our
        // purposes — nothing usable here, defer to PyPI.
        return Ok(None);
    }

    // Group files by version. Filenames look like
    // `{dist}-{version}-{python-tag}-{abi}-{platform}.whl`; version
    // is the second `-`-separated segment.
    let mut releases: BTreeMap<String, Vec<PyPiFile>> = BTreeMap::new();
    for file in files {
        let version = match wheel_filename_version(&file.filename) {
            Some(v) => v,
            None => continue, // ignore non-wheel files in the simple index
        };
        releases.entry(version).or_default().push(file);
    }
    if releases.is_empty() {
        return Ok(None);
    }
    Ok(Some(ReleaseIndex { releases }))
}

/// Extract the version segment from a PEP 427 wheel filename.
fn wheel_filename_version(filename: &str) -> Option<String> {
    if !filename.ends_with(".whl") {
        return None;
    }
    let stem = &filename[..filename.len() - 4];
    let parts: Vec<&str> = stem.split('-').collect();
    // `<dist>-<version>-<pytag>-<abi>-<platform>` → 5 parts.
    // Distributions may contain `-` in the name but PEP 427 forbids
    // it in the wheel filename — names get `_`-escaped. So 5 parts is
    // the exact expected count.
    if parts.len() != 5 {
        return None;
    }
    Some(parts[1].to_string())
}

/// Parse PEP 503 simple-format HTML, returning one PyPiFile per
/// `<a>` tag. Hrefs may be relative or absolute; we resolve against
/// `base_url`.
fn parse_simple_html(html: &str, base_url: &str) -> Result<Vec<PyPiFile>, Error> {
    let mut out = Vec::new();
    let mut cursor = 0;
    let bytes = html.as_bytes();
    while cursor < bytes.len() {
        // Find next "<a "
        let rest = &html[cursor..];
        let start = match find_anchor_start(rest) {
            Some(i) => cursor + i,
            None => break,
        };
        // Find end of opening tag '>'
        let tag_end = match html[start..].find('>') {
            Some(i) => start + i,
            None => break,
        };
        let tag_open = &html[start..tag_end];
        // Find closing </a>
        let after_open = tag_end + 1;
        let close = match html[after_open..].find("</a>") {
            Some(i) => after_open + i,
            None => break,
        };
        let text = html[after_open..close].trim();
        cursor = close + 4;

        let href = match extract_attr(tag_open, "href") {
            Some(h) => h,
            None => continue,
        };
        let (href_path, sha256) = split_href(&href);

        // Resolve relative href against base_url. PEP 503 simple
        // indexes commonly use paths like "../../packages/<name>.whl"
        // — full URL parsing here would be overkill; we handle the
        // two practical cases: absolute (starts with http or //) and
        // path-relative.
        let absolute_url = resolve_relative(base_url, &href_path);

        // Filename is the link text by convention. If text is empty,
        // fall back to the final path segment of the href.
        let filename = if text.is_empty() {
            href_path.rsplit('/').next().unwrap_or("").to_string()
        } else {
            text.to_string()
        };
        if filename.is_empty() {
            continue;
        }

        out.push(PyPiFile {
            filename,
            url: absolute_url,
            packagetype: "bdist_wheel".into(),
            yanked: false,
            digests: PyPiDigests { sha256 },
            host_vouched: true,
        });
    }
    Ok(out)
}

fn find_anchor_start(s: &str) -> Option<usize> {
    // PEP 503 emits `<a href=...>...</a>`. We accept any whitespace
    // after `<a`, and also tolerate uppercase `<A` defensively.
    let lower = s.to_ascii_lowercase();
    let mut from = 0;
    while let Some(i) = lower[from..].find("<a") {
        let pos = from + i;
        let after = pos + 2;
        if let Some(&c) = s.as_bytes().get(after) {
            if c == b' ' || c == b'\t' || c == b'\n' || c == b'\r' {
                return Some(pos);
            }
        }
        from = pos + 2;
    }
    None
}

/// Extract `name="value"` (or single-quoted) attribute value from a
/// tag's opening text. Returns None if absent. Doesn't handle
/// HTML-entity-decoded values — PEP 503 hrefs are URL-encoded ASCII
/// in practice.
fn extract_attr(tag: &str, name: &str) -> Option<String> {
    let needle = format!("{name}=");
    let lower = tag.to_ascii_lowercase();
    let mut from = 0;
    while let Some(i) = lower[from..].find(&needle) {
        let attr_start = from + i;
        // Ensure preceded by whitespace or start, so e.g. `data-href=`
        // doesn't match `href=`.
        if attr_start > 0 {
            let prev = tag.as_bytes()[attr_start - 1];
            if !(prev == b' ' || prev == b'\t' || prev == b'\n' || prev == b'\r') {
                from = attr_start + needle.len();
                continue;
            }
        }
        let after_eq = attr_start + needle.len();
        let bytes = tag.as_bytes();
        if after_eq >= bytes.len() {
            return None;
        }
        let quote = bytes[after_eq];
        if quote != b'"' && quote != b'\'' {
            // Unquoted attribute value — terminate at whitespace or >.
            let mut end = after_eq;
            while end < bytes.len() && !(bytes[end] == b' ' || bytes[end] == b'>') {
                end += 1;
            }
            return Some(tag[after_eq..end].to_string());
        }
        let val_start = after_eq + 1;
        let val_end = match tag[val_start..].find(quote as char) {
            Some(j) => val_start + j,
            None => return None,
        };
        return Some(tag[val_start..val_end].to_string());
    }
    None
}

/// Split `path?or#fragment` into (`path`, `sha256-or-empty`). PEP 503
/// puts the hash in the fragment as `#sha256=<hex>`.
fn split_href(href: &str) -> (String, String) {
    let (path, frag) = match href.split_once('#') {
        Some((p, f)) => (p.to_string(), f.to_string()),
        None => (href.to_string(), String::new()),
    };
    let sha256 = if let Some(hex) = frag.strip_prefix("sha256=") {
        hex.to_string()
    } else {
        String::new()
    };
    (path, sha256)
}

/// Resolve `href` against `base_url`. Handles three cases:
///   - absolute URL (starts with `http://`, `https://`, or `//`)
///   - path-absolute (`/foo/bar`)
///   - path-relative (`../packages/foo.whl`, `foo.whl`)
///
/// We don't implement full RFC 3986 URL resolution because the
/// inputs are bounded: a per-package PEP 503 page always has
/// `<base>/simple/<name>/` as its URL, and hrefs are either absolute
/// or relative to that path.
fn resolve_relative(base_url: &str, href: &str) -> String {
    if href.starts_with("http://") || href.starts_with("https://") {
        return href.to_string();
    }
    if let Some(rest) = href.strip_prefix("//") {
        // Inherit scheme from base.
        let scheme = if base_url.starts_with("https://") { "https:" } else { "http:" };
        return format!("{scheme}//{rest}");
    }

    // Find the base's `scheme://authority` and its directory portion.
    let (scheme_auth, base_path) = split_scheme_authority(base_url);

    if href.starts_with('/') {
        // Path-absolute: replace base's path entirely.
        return format!("{scheme_auth}{href}");
    }

    // Path-relative: walk `..` segments against base's directory.
    let mut segments: Vec<&str> = base_path
        .split('/')
        .filter(|s| !s.is_empty())
        .collect();
    // If base_path ends with '/', segments already reflects the
    // directory. If not, we'd drop the trailing filename — but for
    // PEP 503 per-package pages the URL is always `.../<name>/` so
    // it does end with '/'.

    for part in href.split('/') {
        match part {
            "" | "." => continue,
            ".." => {
                segments.pop();
            }
            other => segments.push(other),
        }
    }
    let path = format!("/{}", segments.join("/"));
    format!("{scheme_auth}{path}")
}

/// Split a URL into `(scheme://authority, path)`. The path includes
/// the leading `/`. Falls back to (entire-string, "/") if there's no
/// `://` — this shouldn't happen for our inputs but we don't want to
/// panic in production.
fn split_scheme_authority(url: &str) -> (String, &str) {
    let after_scheme = match url.find("://") {
        Some(i) => i + 3,
        None => return (url.to_string(), "/"),
    };
    let path_start = url[after_scheme..]
        .find('/')
        .map(|i| after_scheme + i)
        .unwrap_or(url.len());
    let scheme_auth = url[..path_start].to_string();
    let path = if path_start < url.len() {
        &url[path_start..]
    } else {
        "/"
    };
    (scheme_auth, path)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_relative_href_with_sha() {
        let html = r#"<!DOCTYPE html><html><body>
<a href="../packages/cryptography-48.0.0-cp314-abi3-any.whl#sha256=abc123">cryptography-48.0.0-cp314-abi3-any.whl</a><br>
</body></html>"#;
        let files = parse_simple_html(
            html,
            "https://example.com/simple/cryptography/",
        )
        .unwrap();
        assert_eq!(files.len(), 1);
        assert_eq!(files[0].filename, "cryptography-48.0.0-cp314-abi3-any.whl");
        assert_eq!(
            files[0].url,
            "https://example.com/simple/packages/cryptography-48.0.0-cp314-abi3-any.whl"
        );
        assert_eq!(files[0].digests.sha256, "abc123");
        assert!(files[0].host_vouched);
    }

    #[test]
    fn parses_absolute_href() {
        let html = r#"<a href="https://cdn.example.com/cffi-2.0.0-cp314-abi3-any.whl#sha256=def">cffi-2.0.0-cp314-abi3-any.whl</a>"#;
        let files = parse_simple_html(html, "https://example.com/simple/cffi/").unwrap();
        assert_eq!(files.len(), 1);
        assert_eq!(files[0].url, "https://cdn.example.com/cffi-2.0.0-cp314-abi3-any.whl");
    }

    #[test]
    fn version_from_wheel_filename() {
        assert_eq!(
            wheel_filename_version("cryptography-48.0.0-cp314-abi3-any.whl"),
            Some("48.0.0".into())
        );
        assert_eq!(
            wheel_filename_version("cryptography-48.0.0.tar.gz"),
            None
        );
        // Too few segments
        assert_eq!(wheel_filename_version("foo-1.0-py3-none.whl"), None);
    }

    #[test]
    fn ignores_non_a_tags() {
        let html = "<head><title>links</title></head><body>plain text</body>";
        let files = parse_simple_html(html, "https://example.com/simple/x/").unwrap();
        assert!(files.is_empty());
    }
}
