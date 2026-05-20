//! Wheel-tag filter.
//!
//! A wheel filename has the form
//! `{distribution}-{version}-{python tag}-{abi tag}-{platform tag}.whl`.
//! For our purposes there are exactly two acceptable shapes:
//!
//!   - python-tag = `py3` (or `py2.py3`, `pyN`), abi-tag = `none`,
//!     platform-tag = `any` — true pure-Python wheel
//!   - python-tag = anything, abi-tag = `none`, platform-tag = `any`
//!     — same property, looser python-tag matching
//!
//! Anything else (compiled ABI like `cp312`, platform tag like
//! `manylinux_2_28_x86_64`) gets rejected. See README/design doc for
//! the rationale: the CPython baked into the runtime image can't
//! load native extensions, so a platform-tagged wheel would build
//! cleanly and then fail at instantiate time.

/// True iff the given wheel filename is acceptable for a particle.
/// Strict: ABI must be `none` AND platform must be `any`.
pub fn is_pure_python_wheel(filename: &str) -> bool {
    // Strip `.whl`.
    let stem = match filename.strip_suffix(".whl") {
        Some(s) => s,
        None => return false,
    };
    // The trailing three components are the python-tag, abi-tag,
    // platform-tag. We don't validate the leading distribution/version
    // here — the caller already knows those from the PyPI metadata.
    let parts: Vec<&str> = stem.rsplitn(4, '-').collect();
    if parts.len() < 3 {
        return false;
    }
    // rsplitn yields in reverse: [platform, abi, python, (rest...)]
    let platform = parts[0];
    let abi = parts[1];
    let _python = parts[2];
    // Optional build tag (PEP 427) lives between version and
    // python-tag; we accept it implicitly by not validating
    // parts[3..]. The python tag itself can be `py3`, `py2.py3`,
    // `cp312` etc. — what matters is abi==none and platform==any.
    abi == "none" && platform == "any"
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn accepts_pure_python_wheels() {
        assert!(is_pure_python_wheel("httpx-0.27.0-py3-none-any.whl"));
        assert!(is_pure_python_wheel("six-1.16.0-py2.py3-none-any.whl"));
        assert!(is_pure_python_wheel("requests-2.31.0-py3-none-any.whl"));
    }

    #[test]
    fn rejects_compiled_wheels() {
        assert!(!is_pure_python_wheel(
            "markupsafe-3.0.3-cp312-cp312-manylinux_2_17_x86_64.whl"
        ));
        assert!(!is_pure_python_wheel(
            "lxml-5.0.0-cp311-cp311-macosx_11_0_arm64.whl"
        ));
        assert!(!is_pure_python_wheel("pyyaml-6.0-cp310-cp310-win_amd64.whl"));
    }

    #[test]
    fn rejects_non_wheel() {
        assert!(!is_pure_python_wheel("httpx-0.27.0.tar.gz"));
        assert!(!is_pure_python_wheel("not-a-wheel"));
    }

    #[test]
    fn rejects_abi3_wheels() {
        // abi3 is the stable CPython ABI — still native, can't load
        // in the CPython-on-wasi runtime.
        assert!(!is_pure_python_wheel(
            "cryptography-42.0.0-cp39-abi3-manylinux_2_28_x86_64.whl"
        ));
    }
}
