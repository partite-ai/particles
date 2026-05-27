//! `_runtime_host` — Python-side import surface for particle:host/*.
//!
//! User Python imports `particle.credentials`, `particle.kv`,
//! `particle.oauth`, `particle.signing`, `particle.http` — those
//! Python modules forward into `_runtime_host`, which is THIS module
//! built with PyO3 macros. PyO3 generates the `PyMethodDef` tables
//! that map Rust functions to Python callables; lib.rs registers the
//! module via `append_to_inittab!` BEFORE `Py_Initialize`.
//!
//! The implementation here is intentionally thin: every Python-visible
//! fn marshals args and calls the corresponding wit-bindgen-generated
//! `particle::host::*` function. Errors lift the WIT variant tags into
//! a single `RuntimeHostError` Python exception with `.kind` /
//! `.detail` attributes so user code gets one recognizable type.
//!
//! Why a single `_runtime_host` rather than `_credentials_host`,
//! `_kv_host`, etc.: keeping it one module lets the inittab
//! registration be a single call, and avoids replicating the
//! exception class hierarchy in every submodule. The Python-side
//! `particle.<topic>` shims do the surface-level slicing.

use pyo3::exceptions::PyException;
use pyo3::prelude::*;
use pyo3::types::{PyBytes, PyDict, PyList};

use crate::particle::host::{credentials as h_creds, kv as h_kv, oauth as h_oauth, signing as h_signing};
use crate::wasi::http::{outgoing_handler as h_out, types as h_types};
use crate::wasi::io::streams as h_streams;

// -- error mapping ---------------------------------------------------------
//
// Every Python-visible host fn either returns a value or raises
// `RuntimeHostError(kind=<variant-tag>, detail=<payload>)`. The `kind`
// string is the WIT variant tag (kebab-case, matching the WIT file
// verbatim so it's grep-able). `detail` carries the payload string for
// variants that have one — `storage-error("...")`, `type-mismatch("...")`,
// etc. Variants without payloads (`not-configured`, `quota-exceeded`)
// leave `detail` empty.
//
// We also surface `.message` and inherit from PyException so `str(exc)`
// returns the formatted "kind: detail" pair — useful for the trap-mode
// test that just substring-matches the message.

pyo3::create_exception!(_runtime_host, RuntimeHostError, PyException);

/// Build the exception and turn it into PyErr in one go.
fn raise(kind: &str, detail: &str) -> PyErr {
    Python::attach(|py| -> PyErr {
        let msg = if detail.is_empty() {
            kind.to_string()
        } else {
            format!("{kind}: {detail}")
        };
        let exc = RuntimeHostError::new_err(msg);
        // Attach structured attrs. Best-effort — if any setattr fails
        // we still return a usable exception with the descriptive
        // message above.
        {
            let value = exc.value(py);
            let _ = value.setattr("kind", kind);
            let _ = value.setattr("detail", detail);
        }
        exc
    })
}

// -- credentials ------------------------------------------------------------

fn cred_err(e: h_creds::CredentialError) -> PyErr {
    match e {
        h_creds::CredentialError::NotConfigured => raise("not-configured", ""),
        h_creds::CredentialError::StorageError(d) => raise("storage-error", &d),
        h_creds::CredentialError::TypeMismatch(d) => raise("type-mismatch", &d),
    }
}

fn apply_kind_str(k: h_creds::ApplyKind) -> &'static str {
    match k {
        h_creds::ApplyKind::Basic => "basic",
        h_creds::ApplyKind::Bearer => "bearer",
        h_creds::ApplyKind::Header => "header",
        h_creds::ApplyKind::AuthScheme => "auth-scheme",
        h_creds::ApplyKind::QueryParam => "query-param",
    }
}

/// Returns `{"placeholder": str, "apply": {"kind": str, "name": str|None, "scheme": str|None}}`.
/// Python-side particle.credentials wraps this in a PlaceholderInfo
/// dataclass — we return a plain dict so the Python side decides the
/// public surface.
#[pyfunction]
fn _credentials_get_placeholder<'py>(py: Python<'py>, name: &str) -> PyResult<Bound<'py, PyAny>> {
    let info = h_creds::get_placeholder(name).map_err(cred_err)?;
    let dict = pyo3::types::PyDict::new(py);
    dict.set_item("placeholder", info.placeholder)?;
    let apply = pyo3::types::PyDict::new(py);
    apply.set_item("kind", apply_kind_str(info.apply.kind))?;
    apply.set_item("name", info.apply.name)?;
    apply.set_item("scheme", info.apply.scheme)?;
    dict.set_item("apply", apply)?;
    Ok(dict.into_any())
}

#[pyfunction]
fn _credentials_get_raw(name: &str) -> PyResult<String> {
    h_creds::get_raw(name).map_err(cred_err)
}

#[pyfunction]
fn _credentials_get_configured_method(name: &str) -> PyResult<Option<String>> {
    h_creds::get_configured_method(name).map_err(cred_err)
}

/// Convenience: `is_configured(name)` returns False when the host
/// reports `not-configured`, True otherwise (including when a method
/// is explicitly set or when the credential has no method concept,
/// e.g. raw). Other errors propagate as RuntimeHostError.
#[pyfunction]
fn _credentials_is_configured(name: &str) -> PyResult<bool> {
    match h_creds::get_configured_method(name) {
        Ok(_) => Ok(true),
        Err(h_creds::CredentialError::NotConfigured) => Ok(false),
        Err(e) => Err(cred_err(e)),
    }
}

// -- kv ---------------------------------------------------------------------

fn kv_err(e: h_kv::KvError) -> PyErr {
    match e {
        h_kv::KvError::StorageError(d) => raise("storage-error", &d),
        h_kv::KvError::QuotaExceeded => raise("quota-exceeded", ""),
    }
}

#[pyfunction]
fn _kv_get(key: &str) -> PyResult<Option<String>> {
    h_kv::get(key).map_err(kv_err)
}

#[pyfunction]
fn _kv_set(key: &str, value: &str) -> PyResult<()> {
    h_kv::set(key, value).map_err(kv_err)
}

#[pyfunction]
fn _kv_delete(key: &str) -> PyResult<()> {
    h_kv::delete(key).map_err(kv_err)
}

#[pyfunction]
fn _kv_list(prefix: &str) -> PyResult<Vec<String>> {
    // wit-bindgen renames %list -> list_ in Rust because `list` is a
    // reserved keyword.
    h_kv::list(prefix).map_err(kv_err)
}

// -- oauth ------------------------------------------------------------------

fn oauth_err(e: h_oauth::OauthError) -> PyErr {
    match e {
        h_oauth::OauthError::NotConfigured => raise("not-configured", ""),
        h_oauth::OauthError::NotOauth => raise("not-oauth", ""),
        h_oauth::OauthError::RefreshFailed(d) => raise("refresh-failed", &d),
    }
}

#[pyfunction]
fn _oauth_refresh(name: &str) -> PyResult<()> {
    h_oauth::refresh(name).map_err(oauth_err)
}

// -- signing ----------------------------------------------------------------

fn signing_err(e: h_signing::SigningError) -> PyErr {
    match e {
        h_signing::SigningError::NotConfigured => raise("not-configured", ""),
        h_signing::SigningError::NotSigningKey => raise("not-signing-key", ""),
        h_signing::SigningError::InvalidInput(d) => raise("invalid-input", &d),
    }
}

#[pyfunction]
fn _signing_sign<'py>(py: Python<'py>, name: &str, data: &[u8]) -> PyResult<Bound<'py, PyBytes>> {
    let sig = h_signing::sign(name, data).map_err(signing_err)?;
    Ok(PyBytes::new(py, &sig))
}

#[pyfunction]
fn _signing_verify(name: &str, data: &[u8], signature: &[u8]) -> PyResult<bool> {
    h_signing::verify(name, data, signature).map_err(signing_err)
}

// -- http -------------------------------------------------------------------
//
// One blocking `_http_request(method, url, headers, body) -> dict` that does
// the full wasi:http request/response state machine in Rust:
//
//   1. Parse URL into scheme/authority/path-with-query.
//   2. Build Fields from caller-supplied headers (add Host if absent).
//   3. Create OutgoingRequest, set method/scheme/authority/path.
//   4. Take the body's OutputStream BEFORE handle() consumes the request.
//   5. Call outgoing_handler::handle(req) → FutureIncomingResponse.
//   6. Write body bytes through the stream in `check_write`-sized chunks.
//   7. Drop the stream, finish() the OutgoingBody.
//   8. Block on the future via Pollable::block until `get()` yields a result.
//   9. Read the IncomingBody's stream to EOF.
//  10. Return {"status": int, "headers": [[name, bytes]], "body": bytes}.
//
// Errors raise RuntimeHostError with kind = the wasi:http error-code variant
// (e.g. "DNS-error", "connection-refused", "HTTP-protocol-error") so user
// code can branch on .kind exactly like the credentials/kv/oauth bridges.

pub(crate) fn http_err(kind: &str, detail: impl AsRef<str>) -> PyErr {
    raise(kind, detail.as_ref())
}

/// Format a wasi:http error-code variant into (kind, detail) strings.
/// We surface the variant *tag* (DNS-error, connection-refused, ...) as
/// the kind and the optional payload (or empty) as the detail. Keeps
/// the trap-mode "not allowed during get-manifest" detail string intact
/// for the manifest_extract trap test.
pub(crate) fn format_http_error(e: &h_types::ErrorCode) -> (&'static str, String) {
    use h_types::ErrorCode::*;
    match e {
        DnsTimeout => ("DNS-timeout", String::new()),
        DnsError(p) => ("DNS-error", p.rcode.clone().unwrap_or_default()),
        DestinationNotFound => ("destination-not-found", String::new()),
        DestinationUnavailable => ("destination-unavailable", String::new()),
        DestinationIpProhibited => ("destination-IP-prohibited", String::new()),
        DestinationIpUnroutable => ("destination-IP-unroutable", String::new()),
        ConnectionRefused => ("connection-refused", String::new()),
        ConnectionTerminated => ("connection-terminated", String::new()),
        ConnectionTimeout => ("connection-timeout", String::new()),
        ConnectionReadTimeout => ("connection-read-timeout", String::new()),
        ConnectionWriteTimeout => ("connection-write-timeout", String::new()),
        ConnectionLimitReached => ("connection-limit-reached", String::new()),
        TlsProtocolError => ("TLS-protocol-error", String::new()),
        TlsCertificateError => ("TLS-certificate-error", String::new()),
        TlsAlertReceived(p) => (
            "TLS-alert-received",
            p.alert_message.clone().unwrap_or_default(),
        ),
        HttpRequestDenied => ("HTTP-request-denied", String::new()),
        HttpRequestLengthRequired => ("HTTP-request-length-required", String::new()),
        HttpRequestBodySize(_) => ("HTTP-request-body-size", String::new()),
        HttpRequestMethodInvalid => ("HTTP-request-method-invalid", String::new()),
        HttpRequestUriInvalid => ("HTTP-request-URI-invalid", String::new()),
        HttpRequestUriTooLong => ("HTTP-request-URI-too-long", String::new()),
        HttpRequestHeaderSectionSize(_) => ("HTTP-request-header-section-size", String::new()),
        HttpRequestHeaderSize(_) => ("HTTP-request-header-size", String::new()),
        HttpRequestTrailerSectionSize(_) => ("HTTP-request-trailer-section-size", String::new()),
        HttpRequestTrailerSize(_) => ("HTTP-request-trailer-size", String::new()),
        HttpResponseIncomplete => ("HTTP-response-incomplete", String::new()),
        HttpResponseHeaderSectionSize(_) => ("HTTP-response-header-section-size", String::new()),
        HttpResponseHeaderSize(_) => ("HTTP-response-header-size", String::new()),
        HttpResponseBodySize(_) => ("HTTP-response-body-size", String::new()),
        HttpResponseTrailerSectionSize(_) => ("HTTP-response-trailer-section-size", String::new()),
        HttpResponseTrailerSize(_) => ("HTTP-response-trailer-size", String::new()),
        HttpResponseTransferCoding(s) => (
            "HTTP-response-transfer-coding",
            s.clone().unwrap_or_default(),
        ),
        HttpResponseContentCoding(s) => (
            "HTTP-response-content-coding",
            s.clone().unwrap_or_default(),
        ),
        HttpResponseTimeout => ("HTTP-response-timeout", String::new()),
        HttpUpgradeFailed => ("HTTP-upgrade-failed", String::new()),
        HttpProtocolError => ("HTTP-protocol-error", String::new()),
        LoopDetected => ("loop-detected", String::new()),
        ConfigurationError => ("configuration-error", String::new()),
        InternalError(s) => ("internal-error", s.clone().unwrap_or_default()),
    }
}

/// Map a method string (case-insensitive) to a wasi:http Method variant.
/// Unknown methods become Method::Other(<original>) which the host can
/// either accept or reject — we don't pre-judge.
pub(crate) fn to_wit_method(s: &str) -> h_types::Method {
    match s.to_ascii_uppercase().as_str() {
        "GET" => h_types::Method::Get,
        "HEAD" => h_types::Method::Head,
        "POST" => h_types::Method::Post,
        "PUT" => h_types::Method::Put,
        "DELETE" => h_types::Method::Delete,
        "CONNECT" => h_types::Method::Connect,
        "OPTIONS" => h_types::Method::Options,
        "TRACE" => h_types::Method::Trace,
        "PATCH" => h_types::Method::Patch,
        _ => h_types::Method::Other(s.to_string()),
    }
}

/// scheme:// → wasi:http Scheme variant. We accept any scheme but emit
/// only http/https in the canonical variant — others use Scheme::Other.
pub(crate) fn to_wit_scheme(s: &str) -> h_types::Scheme {
    match s.to_ascii_lowercase().as_str() {
        "http" => h_types::Scheme::Http,
        "https" => h_types::Scheme::Https,
        _ => h_types::Scheme::Other(s.to_string()),
    }
}

/// Minimal URL split that yields (scheme, authority, path_with_query).
/// We avoid pulling the `url` crate in for ~1KB of work; the parser
/// here handles the shapes wasi:http actually accepts (scheme://
/// authority/path?query) and rejects degenerate URLs explicitly.
pub(crate) fn split_url(url: &str) -> Result<(String, String, String), String> {
    let (scheme, rest) = match url.find("://") {
        Some(i) => (&url[..i], &url[i + 3..]),
        None => return Err(format!("url missing scheme://: {url}")),
    };
    if scheme.is_empty() {
        return Err(format!("url missing scheme: {url}"));
    }
    // authority = up to first '/', '?', or '#'; rest is the path-with-query.
    let path_start = rest
        .find(|c: char| c == '/' || c == '?' || c == '#')
        .unwrap_or(rest.len());
    let authority = &rest[..path_start];
    if authority.is_empty() {
        return Err(format!("url missing authority: {url}"));
    }
    let path = &rest[path_start..];
    // Strip any fragment — wasi:http doesn't carry it across the wire.
    let path_no_frag = match path.find('#') {
        Some(i) => &path[..i],
        None => path,
    };
    let path_with_query = if path_no_frag.is_empty() {
        "/".to_string()
    } else if !path_no_frag.starts_with('/') {
        // `?query` only → prepend `/`.
        format!("/{path_no_frag}")
    } else {
        path_no_frag.to_string()
    };
    Ok((scheme.to_string(), authority.to_string(), path_with_query))
}

/// Block on a pollable until ready. The wasi:io semantics: each call to
/// `block` returns when the pollable is ready (semantically a `poll([p])`
/// over a single-element list); we don't loop here because the pollable
/// either becomes ready or the runtime traps.
fn pollable_block(p: &crate::wasi::io::poll::Pollable) {
    p.block()
}

/// The actual blocking request. Pulled out of #[pyfunction] so we can
/// `?`-propagate Result over the multi-step wasi:http state machine and
/// have one place to translate to PyErr.
fn do_http_request(
    method: &str,
    url: &str,
    headers: Vec<(String, Vec<u8>)>,
    body: Vec<u8>,
) -> Result<(u16, Vec<(String, Vec<u8>)>, Vec<u8>), PyErr> {
    let (scheme, authority, path_with_query) =
        split_url(url).map_err(|m| http_err("HTTP-request-URI-invalid", m))?;

    // Build Fields. Add Host if the caller didn't.
    let mut entries: Vec<(String, Vec<u8>)> = Vec::with_capacity(headers.len() + 1);
    let mut have_host = false;
    for (k, v) in headers {
        if k.eq_ignore_ascii_case("host") {
            have_host = true;
        }
        entries.push((k, v));
    }
    if !have_host {
        entries.push(("host".into(), authority.as_bytes().to_vec()));
    }
    let fields = h_types::Fields::from_list(&entries)
        .map_err(|e| http_err("HTTP-request-header-invalid", format!("{e:?}")))?;

    let req = h_types::OutgoingRequest::new(fields);
    req.set_method(&to_wit_method(method))
        .map_err(|_| http_err("HTTP-request-method-invalid", method))?;
    req.set_scheme(Some(&to_wit_scheme(&scheme)))
        .map_err(|_| http_err("HTTP-request-URI-invalid", "set_scheme failed"))?;
    req.set_authority(Some(&authority))
        .map_err(|_| http_err("HTTP-request-URI-invalid", "set_authority failed"))?;
    req.set_path_with_query(Some(&path_with_query))
        .map_err(|_| http_err("HTTP-request-URI-invalid", "set_path_with_query failed"))?;

    // Take the body resource + write-side stream BEFORE handle() consumes req.
    let outgoing_body = req
        .body()
        .map_err(|_| http_err("HTTP-request-body-size", "OutgoingRequest::body failed"))?;
    let stream = outgoing_body
        .write()
        .map_err(|_| http_err("HTTP-request-body-size", "OutgoingBody::write failed"))?;

    // Send headers + start the response. After this point `req` is gone.
    let future = h_out::handle(req, None).map_err(|e| {
        let (k, d) = format_http_error(&e);
        http_err(k, d)
    })?;

    // Write body in check_write-sized chunks. blocking_write_and_flush
    // honors the runtime's flow-control without us having to poll.
    let mut offset = 0;
    while offset < body.len() {
        let mut budget = stream
            .check_write()
            .map_err(|e| http_err("HTTP-request-body-size", format!("check_write: {e:?}")))?;
        if budget == 0 {
            // Wait for room. blocking_write_and_flush handles this for us
            // when budget==0 (it blocks internally), but check_write of 0
            // followed by an immediate poll is the documented path.
            let p = stream.subscribe();
            pollable_block(&p);
            budget = stream
                .check_write()
                .map_err(|e| http_err("HTTP-request-body-size", format!("check_write: {e:?}")))?;
            if budget == 0 {
                return Err(http_err(
                    "HTTP-request-body-size",
                    "stream remained unwriteable after subscribe.block",
                ));
            }
        }
        let take = core::cmp::min(budget as usize, body.len() - offset);
        stream
            .blocking_write_and_flush(&body[offset..offset + take])
            .map_err(|e| http_err("HTTP-request-body-size", format!("write: {e:?}")))?;
        offset += take;
    }

    // Closing the OutputStream is what tells the runtime "headers + body
    // complete"; finish() on the body releases the request side cleanly.
    drop(stream);
    h_types::OutgoingBody::finish(outgoing_body, None).map_err(|e| {
        let (k, d) = format_http_error(&e);
        http_err(k, d)
    })?;

    // Wait for the response.
    let response = loop {
        if let Some(res) = future.get() {
            // `get` returns Some on the FIRST call; calling it again
            // panics on the runtime side. Unwrap that outer "only one
            // get" Result and propagate the inner.
            let inner = res
                .map_err(|_| http_err("HTTP-protocol-error", "future.get() called twice"))?;
            break inner.map_err(|e| {
                let (k, d) = format_http_error(&e);
                http_err(k, d)
            })?;
        }
        let p = future.subscribe();
        pollable_block(&p);
    };

    let status = response.status();
    let headers_out = response.headers().entries();
    let incoming_body = response
        .consume()
        .map_err(|_| http_err("HTTP-response-incomplete", "IncomingResponse::consume failed"))?;
    let in_stream = incoming_body
        .stream()
        .map_err(|_| http_err("HTTP-response-incomplete", "IncomingBody::stream failed"))?;

    // Read until EOF. blocking_read returns StreamError::Closed at EOF;
    // any other Err is a real failure.
    let mut body_out: Vec<u8> = Vec::new();
    const CHUNK: u64 = 64 * 1024;
    loop {
        match in_stream.blocking_read(CHUNK) {
            Ok(chunk) => {
                if chunk.is_empty() {
                    // No data and no error means the source signalled
                    // "ready, but nothing yet". Poll once to make
                    // progress; if blocking_read still yields empty we
                    // treat that as benign EOF.
                    let p = in_stream.subscribe();
                    pollable_block(&p);
                    let again = in_stream.blocking_read(CHUNK).unwrap_or_default();
                    if again.is_empty() {
                        break;
                    }
                    body_out.extend_from_slice(&again);
                } else {
                    body_out.extend_from_slice(&chunk);
                }
            }
            Err(h_streams::StreamError::Closed) => break,
            Err(h_streams::StreamError::LastOperationFailed(e)) => {
                let msg = e.to_debug_string();
                return Err(http_err("HTTP-response-incomplete", msg));
            }
        }
    }

    Ok((status, headers_out, body_out))
}

/// Issue a blocking HTTP request. Returns a dict:
///
///   {"status": int, "headers": list[(str, bytes)], "body": bytes}
///
/// `headers` and `body` are caller-supplied. Empty headers / None body
/// produces a request with just the synthetic Host header and an empty
/// payload. Particles wanting credential substitution should compute
/// the placeholder via `_credentials_get_placeholder(name)` and inline
/// it into the headers/url BEFORE calling — the host substitutes the
/// real secret at the wire boundary.
#[pyfunction]
#[pyo3(signature = (method, url, headers, body))]
fn _http_request<'py>(
    py: Python<'py>,
    method: &str,
    url: &str,
    headers: &Bound<'py, PyList>,
    body: &[u8],
) -> PyResult<Bound<'py, PyDict>> {
    // Marshal headers (list of [name, value-bytes-or-str]).
    let mut hs: Vec<(String, Vec<u8>)> = Vec::with_capacity(headers.len());
    for item in headers.iter() {
        let pair: (String, &[u8]) = item.extract()?;
        hs.push((pair.0, pair.1.to_vec()));
    }
    let (status, hdrs, body_bytes) = do_http_request(method, url, hs, body.to_vec())?;

    let out = PyDict::new(py);
    out.set_item("status", status as u32)?;
    let hpl = PyList::empty(py);
    for (k, v) in hdrs {
        let pair = PyList::empty(py);
        pair.append(k)?;
        pair.append(PyBytes::new(py, &v))?;
        hpl.append(pair)?;
    }
    out.set_item("headers", hpl)?;
    out.set_item("body", PyBytes::new(py, &body_bytes))?;
    Ok(out)
}

// -- module init ------------------------------------------------------------

/// PyO3 generates `PyInit__runtime_host` from this. lib.rs registers
/// it via `pyo3::append_to_inittab!`, which addresses the fn by its
/// Rust path — the leading underscore in `_runtime_host` is preserved
/// verbatim into the Python module name. The explicit `#[pyo3(name = ...)]`
/// pins the Python-visible name in case PyO3's default-name behavior
/// strips the leading underscore (it has heuristics around private
/// idents).
#[pymodule]
#[pyo3(name = "_runtime_host")]
#[allow(non_snake_case)]
pub fn _runtime_host(m: &Bound<'_, PyModule>) -> PyResult<()> {
    m.add("RuntimeHostError", m.py().get_type::<RuntimeHostError>())?;

    m.add_function(wrap_pyfunction!(_credentials_get_placeholder, m)?)?;
    m.add_function(wrap_pyfunction!(_credentials_get_raw, m)?)?;
    m.add_function(wrap_pyfunction!(_credentials_get_configured_method, m)?)?;
    m.add_function(wrap_pyfunction!(_credentials_is_configured, m)?)?;

    m.add_function(wrap_pyfunction!(_kv_get, m)?)?;
    m.add_function(wrap_pyfunction!(_kv_set, m)?)?;
    m.add_function(wrap_pyfunction!(_kv_delete, m)?)?;
    m.add_function(wrap_pyfunction!(_kv_list, m)?)?;

    m.add_function(wrap_pyfunction!(_oauth_refresh, m)?)?;

    m.add_function(wrap_pyfunction!(_signing_sign, m)?)?;
    m.add_function(wrap_pyfunction!(_signing_verify, m)?)?;

    m.add_function(wrap_pyfunction!(_http_request, m)?)?;

    // Non-blocking HTTP primitives — backing for particle._wasi_async
    // (asyncio integration over wasi:io/poll). See async_http.rs.
    m.add_function(wrap_pyfunction!(crate::async_http::_http_submit, m)?)?;
    m.add_function(wrap_pyfunction!(crate::async_http::_http_pollable, m)?)?;
    m.add_function(wrap_pyfunction!(crate::async_http::_http_advance, m)?)?;
    m.add_function(wrap_pyfunction!(crate::async_http::_http_complete, m)?)?;
    m.add_function(wrap_pyfunction!(crate::async_http::_http_drop, m)?)?;
    m.add_function(wrap_pyfunction!(crate::async_http::_io_poll, m)?)?;
    m.add_function(wrap_pyfunction!(crate::async_http::_pollable_drop, m)?)?;

    Ok(())
}
