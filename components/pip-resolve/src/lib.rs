//! WASI Preview 2 component exporting `particle:build/pip-installer`.
//!
//! Parallel to components/deno-npm/src/lib.rs. The orchestrator in
//! internal/build calls `resolve_and_fetch` with the PEP 508
//! requirements extracted from a Particlefile.py's PEP 723 inline
//! metadata block; we return the transitive closure of pure-Python
//! wheels ready to drop into the particle's `_deps/site-packages`.
//!
//! Hard constraint, surfaced as a typed error: only `*-none-any`
//! wheels are accepted. The CPython interpreter baked into the
//! Python runtime image can't load native extensions, so a
//! platform-tagged wheel would build cleanly here and then break at
//! particle-instantiate time — fail loud at resolve.

#![allow(clippy::needless_lifetimes)]

mod particle_index;
mod pypi;
mod resolver;
mod wheel_tag;

wit_bindgen::generate!({
    world: "component",
    path: "wit",
});

use exports::particle::build::pip_installer::{
    Guest, PipError, PipRequest, ResolvedWheel,
};

struct Component;

impl Guest for Component {
    fn resolve_and_fetch(
        reqs: Vec<PipRequest>,
        python_version: String,
    ) -> Result<Vec<ResolvedWheel>, PipError> {
        wstd::runtime::block_on(resolve_and_fetch_async(reqs, python_version))
    }
}

export!(Component);

async fn resolve_and_fetch_async(
    reqs: Vec<PipRequest>,
    _python_version: String,
) -> Result<Vec<ResolvedWheel>, PipError> {
    let requirements: Vec<pep508_rs::Requirement> = reqs
        .into_iter()
        .map(|r| {
            pep508_rs::Requirement::from_str(&r.requirement).map_err(|e| {
                PipError::ResolutionError(format!(
                    "invalid requirement {:?}: {e}",
                    r.requirement
                ))
            })
        })
        .collect::<Result<_, _>>()?;

    let resolved = resolver::resolve(&requirements).await?;
    let wheels = resolver::fetch_all(&resolved).await?;
    Ok(wheels)
}

// Re-export error variants via a small helper so the resolver/wheel
// modules don't need to depend on the wit-bindgen-generated path
// themselves — keeps the module graph one-way.
pub(crate) use exports::particle::build::pip_installer::PipError as Error;

// pep508_rs's Requirement::from_str is `std::str::FromStr`; re-export
// the trait so users elsewhere in the crate can write
// `Requirement::from_str(...)` without an explicit import.
use std::str::FromStr;
