//! Wraps `deno_npm` as a WASI Preview 2 component exporting `particle:build/installer`.
//!
//! `resolve_and_fetch` walks a list of top-level npm dep requests, drives
//! `deno_npm`'s resolver against the public npm registry over `wasi:http`,
//! then downloads each resolved tarball and returns the flat package set
//! (each entry carrying its tarball bytes, its `package.json` extracted from
//! the tarball, and indices into the result list for its direct deps).

#![allow(clippy::needless_lifetimes)]

use std::cell::RefCell;
use std::collections::HashMap;
use std::sync::Arc;

use async_trait::async_trait;
use base64::Engine;
use deno_npm::registry::{
    NpmPackageInfo, NpmPackageVersionDistInfoIntegrity, NpmRegistryApi,
    NpmRegistryPackageInfoLoadError,
};
use deno_npm::resolution::{AddPkgReqsOptions, NpmResolutionSnapshot, NpmVersionResolver};
use deno_npm::{NpmPackageId, NpmResolutionPackage};
use deno_semver::package::PackageReq;
use sha1::Sha1;
use sha2::{Digest, Sha512};

wit_bindgen::generate!({
    world: "component",
    path: "wit",
});

use exports::particle::build::installer::{
    DepRequest, Guest, InstallerError, ResolvedDep,
};

const REGISTRY: &str = "https://registry.npmjs.org";
const PACKUMENT_ACCEPT: &str = "application/vnd.npm.install-v1+json";

struct Component;

impl Guest for Component {
    fn resolve_and_fetch(deps: Vec<DepRequest>) -> Result<Vec<ResolvedDep>, InstallerError> {
        wstd::runtime::block_on(resolve_and_fetch_async(deps))
    }
}

export!(Component);

async fn resolve_and_fetch_async(
    deps: Vec<DepRequest>,
) -> Result<Vec<ResolvedDep>, InstallerError> {
    let pkg_reqs: Vec<PackageReq> = deps
        .into_iter()
        .map(|d| {
            let spec = format!("{}@{}", d.name, d.version_range);
            PackageReq::from_str(&spec).map_err(|e| {
                InstallerError::ResolutionError(format!("invalid req {spec:?}: {e}"))
            })
        })
        .collect::<Result<_, _>>()?;

    let api = WasiRegistryApi::default();
    let version_resolver = NpmVersionResolver {
        link_packages: Default::default(),
        newest_dependency_date_options: Default::default(),
        overrides: Default::default(),
    };

    let snapshot = NpmResolutionSnapshot::new(Default::default());
    let result = snapshot
        .add_pkg_reqs(
            &api,
            AddPkgReqsOptions {
                package_reqs: &pkg_reqs,
                version_resolver: &version_resolver,
                should_dedup: true,
            },
            None,
        )
        .await;

    let snapshot: NpmResolutionSnapshot = result
        .dep_graph_result
        .map_err(|e| InstallerError::ResolutionError(format!("resolve failed: {e}")))?;

    let pkgs: Vec<&NpmResolutionPackage> = snapshot.all_packages_for_every_system().collect();

    let mut id_to_idx: HashMap<NpmPackageId, u32> = HashMap::with_capacity(pkgs.len());
    for (i, pkg) in pkgs.iter().enumerate() {
        id_to_idx.insert(pkg.id.clone(), i as u32);
    }

    let mut out = Vec::with_capacity(pkgs.len());
    for pkg in &pkgs {
        let dist = pkg.dist.as_ref().ok_or_else(|| {
            InstallerError::ResolutionError(format!(
                "package {} has no dist (workspace-link?)",
                pkg.id.as_serialized()
            ))
        })?;

        let tarball_bytes = http_get(&dist.tarball, None).await?;
        verify_integrity(&tarball_bytes, dist.integrity(), &pkg.id.as_serialized())?;
        let package_json = extract_package_json(&tarball_bytes, &pkg.id.as_serialized())?;

        let integrity_str = match dist.integrity() {
            NpmPackageVersionDistInfoIntegrity::Integrity { algorithm, base64_hash } => {
                format!("{algorithm}-{base64_hash}")
            }
            NpmPackageVersionDistInfoIntegrity::UnknownIntegrity(s) => s.to_string(),
            NpmPackageVersionDistInfoIntegrity::LegacySha1Hex(hex) => format!("sha1-{hex}"),
            NpmPackageVersionDistInfoIntegrity::None => String::new(),
        };

        let mut transitive: Vec<u32> = pkg
            .dependencies
            .values()
            .filter_map(|id| id_to_idx.get(id).copied())
            .collect();
        transitive.sort_unstable();
        transitive.dedup();

        out.push(ResolvedDep {
            name: pkg.id.nv.name.to_string(),
            version: pkg.id.nv.version.to_string(),
            integrity: integrity_str,
            tarball_bytes,
            package_json,
            transitive,
        });
    }

    Ok(out)
}

// -----------------------------------------------------------------------------
// HTTP via wstd
// -----------------------------------------------------------------------------

async fn http_get(url: &str, accept: Option<&str>) -> Result<Vec<u8>, InstallerError> {
    use wstd::http::{Client, Method, Request};
    use wstd::io::{empty, AsyncRead};

    let mut builder = Request::builder().method(Method::GET).uri(url);
    if let Some(accept) = accept {
        builder = builder.header("Accept", accept);
    }
    let request = builder
        .body(empty())
        .map_err(|e| InstallerError::NetworkError(format!("build req {url}: {e}")))?;

    let mut response = Client::new()
        .send(request)
        .await
        .map_err(|e| InstallerError::NetworkError(format!("send {url}: {e}")))?;

    let status = response.status();
    if !status.is_success() {
        return Err(InstallerError::NetworkError(format!(
            "GET {url} -> HTTP {status}"
        )));
    }

    let mut buf = Vec::new();
    response
        .body_mut()
        .read_to_end(&mut buf)
        .await
        .map_err(|e| InstallerError::NetworkError(format!("read body {url}: {e}")))?;
    Ok(buf)
}

// -----------------------------------------------------------------------------
// NpmRegistryApi backed by wasi:http
// -----------------------------------------------------------------------------

#[derive(Default)]
struct WasiRegistryApi {
    cache: RefCell<HashMap<String, Arc<NpmPackageInfo>>>,
}

#[async_trait(?Send)]
impl NpmRegistryApi for WasiRegistryApi {
    async fn package_info(
        &self,
        name: &str,
    ) -> Result<Arc<NpmPackageInfo>, NpmRegistryPackageInfoLoadError> {
        if let Some(info) = self.cache.borrow().get(name).cloned() {
            return Ok(info);
        }

        let url = format!("{REGISTRY}/{}", encode_package_name(name));
        let body = http_get(&url, Some(PACKUMENT_ACCEPT)).await.map_err(|e| {
            NpmRegistryPackageInfoLoadError::LoadError(Arc::new(std::io::Error::other(
                format!("packument fetch {url}: {e:?}"),
            )))
        })?;

        let info: NpmPackageInfo = serde_json::from_slice(&body).map_err(|e| {
            NpmRegistryPackageInfoLoadError::LoadError(Arc::new(std::io::Error::other(
                format!("packument parse {name}: {e}"),
            )))
        })?;

        let info = Arc::new(info);
        self.cache
            .borrow_mut()
            .insert(name.to_string(), info.clone());
        Ok(info)
    }
}

fn encode_package_name(name: &str) -> String {
    // Scoped packages (`@scope/name`) need the slash percent-encoded for the registry URL.
    name.replace('/', "%2f")
}

// -----------------------------------------------------------------------------
// Integrity verification
// -----------------------------------------------------------------------------

fn verify_integrity(
    bytes: &[u8],
    integrity: NpmPackageVersionDistInfoIntegrity<'_>,
    pkg_label: &str,
) -> Result<(), InstallerError> {
    match integrity {
        NpmPackageVersionDistInfoIntegrity::Integrity { algorithm, base64_hash } => {
            let actual = match algorithm {
                "sha512" => {
                    let mut h = Sha512::new();
                    h.update(bytes);
                    base64::engine::general_purpose::STANDARD.encode(h.finalize())
                }
                "sha1" => {
                    let mut h = Sha1::new();
                    h.update(bytes);
                    base64::engine::general_purpose::STANDARD.encode(h.finalize())
                }
                other => {
                    return Err(InstallerError::IntegrityMismatch(format!(
                        "{pkg_label}: unsupported integrity algorithm {other:?}"
                    )));
                }
            };
            if actual != base64_hash {
                return Err(InstallerError::IntegrityMismatch(format!(
                    "{pkg_label}: {algorithm} mismatch"
                )));
            }
        }
        NpmPackageVersionDistInfoIntegrity::LegacySha1Hex(expected_hex) => {
            let mut h = Sha1::new();
            h.update(bytes);
            let actual_hex = hex::encode(h.finalize());
            if !actual_hex.eq_ignore_ascii_case(expected_hex) {
                return Err(InstallerError::IntegrityMismatch(format!(
                    "{pkg_label}: sha1 mismatch"
                )));
            }
        }
        // No integrity to check against; this happens for some legacy packuments.
        // We accept the tarball as-is — the resolver still pinned the version + URL.
        NpmPackageVersionDistInfoIntegrity::UnknownIntegrity(_)
        | NpmPackageVersionDistInfoIntegrity::None => {}
    }
    Ok(())
}

// -----------------------------------------------------------------------------
// package.json extraction from gzipped tarball
// -----------------------------------------------------------------------------

fn extract_package_json(tarball_gz: &[u8], pkg_label: &str) -> Result<String, InstallerError> {
    use std::io::Read;

    let gz = flate2::read::GzDecoder::new(tarball_gz);
    let mut archive = tar::Archive::new(gz);

    let entries = archive.entries().map_err(|e| {
        InstallerError::ResolutionError(format!("{pkg_label}: read tarball: {e}"))
    })?;

    for entry in entries {
        let mut entry = entry.map_err(|e| {
            InstallerError::ResolutionError(format!("{pkg_label}: tarball entry: {e}"))
        })?;
        let path_bytes = entry.path_bytes();
        // npm tarballs nest everything under a top-level directory (conventionally
        // `package/`, but some publishers use other names like the package name).
        // The package.json is always at <top>/package.json, exactly one slash deep.
        if let Some(name) = std::str::from_utf8(&path_bytes).ok().and_then(strip_top_dir) {
            if name == "package.json" {
                let mut s = String::new();
                entry.read_to_string(&mut s).map_err(|e| {
                    InstallerError::ResolutionError(format!(
                        "{pkg_label}: read package.json: {e}"
                    ))
                })?;
                return Ok(s);
            }
        }
    }

    Err(InstallerError::ResolutionError(format!(
        "{pkg_label}: package.json not found in tarball"
    )))
}

fn strip_top_dir(path: &str) -> Option<&str> {
    // Skip a single leading `./` if present, then take everything after the first `/`.
    let p = path.strip_prefix("./").unwrap_or(path);
    p.split_once('/').map(|(_, rest)| rest)
}
