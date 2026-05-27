//! Non-blocking wasi:http primitives for Python's asyncio integration.
//!
//! `host_module._http_request` (blocking) is the sync convenience used
//! by `particle.http.fetch`. This module adds the parallel async surface
//! that lets `particle._wasi_async`'s asyncio event loop drive multiple
//! in-flight wasi:http requests concurrently via `wasi:io/poll`.
//!
//! The mental model (per-request):
//!   1. Python calls `_http_submit(...)`. We write the body inline (the
//!      common case is small JSON bodies — blocking these is fine), then
//!      `handle()` returns a FutureIncomingResponse. We park it in the
//!      handle table in `AwaitingResponse` state and hand back a u32.
//!   2. Python calls `_http_pollable(h)` to get a pollable handle, then
//!      `_io_poll([pollables], timeout_ms)` blocks until at least one
//!      fires. The selector's callback then calls `_http_advance(h)`.
//!   3. `_http_advance` is the state-machine step. From AwaitingResponse,
//!      it calls `future.get()`; if ready, it transitions to ReadingBody
//!      and starts a chunked drain. From ReadingBody, it calls
//!      `stream.read(CHUNK)` (non-blocking — pollable just fired); on
//!      `Closed` it transitions to Done; otherwise accumulates the chunk
//!      and the loop polls the stream's pollable again.
//!   4. When state is Done, Python calls `_http_complete(h)` to take the
//!      (status, headers, body) tuple and drop the handle entry.
//!
//! Pollables are owned by the host (returned as fresh resources each call
//! to `.subscribe()`). We park them in a separate pollable table so the
//! Python side can hold a u32 indefinitely without us re-creating them
//! across `_io_poll` calls. `_pollable_drop(p)` releases when Python is
//! done with a pollable (e.g. `selector.unregister`). Handle entries
//! transitively release their pollables on drop.
//!
//! Single-threaded: wasi has no threads, so thread-local + RefCell is
//! the right cell. No Mutex anywhere.

use std::cell::RefCell;
use std::collections::HashMap;

use pyo3::exceptions::PyValueError;
use pyo3::prelude::*;
use pyo3::types::{PyBytes, PyDict, PyList};

use crate::wasi::clocks::monotonic_clock as h_clock;
use crate::wasi::http::{outgoing_handler as h_out, types as h_types};
use crate::wasi::io::poll as h_poll;
use crate::wasi::io::streams as h_streams;

use crate::host_module::{format_http_error, http_err, split_url, to_wit_method, to_wit_scheme};

const READ_CHUNK: u64 = 64 * 1024;

// ---- handle tables --------------------------------------------------------

enum HttpState {
    /// Body sent, awaiting the first response byte. The pollable we
    /// hand out comes from `future.subscribe()`.
    AwaitingResponse { future: h_types::FutureIncomingResponse },
    /// Headers received; draining the response body. `stream` is the
    /// active InputStream; pollable comes from `stream.subscribe()`.
    /// `body_keep_alive` MUST stay alive while `stream` is in use —
    /// dropping the IncomingBody invalidates the InputStream.
    ReadingBody {
        status: u16,
        headers: Vec<(String, Vec<u8>)>,
        body_keep_alive: h_types::IncomingBody,
        stream: h_streams::InputStream,
        buf: Vec<u8>,
    },
    /// All data captured; ready for `_http_complete`.
    Done {
        status: u16,
        headers: Vec<(String, Vec<u8>)>,
        body: Vec<u8>,
    },
}

thread_local! {
    static HTTP_HANDLES: RefCell<HashMap<u32, HttpState>> = RefCell::new(HashMap::new());
    static POLLABLES:    RefCell<HashMap<u32, h_poll::Pollable>> = RefCell::new(HashMap::new());
    static NEXT_ID:      RefCell<u32> = const { RefCell::new(1) };
}

fn alloc_id() -> u32 {
    NEXT_ID.with(|c| {
        let mut n = c.borrow_mut();
        let id = *n;
        // u32 wrap is theoretically possible but a particle would have
        // to issue ~4 billion requests in one process to hit it. We
        // accept the wrap; collisions on a live key would surface as
        // typed errors below (handle-already-exists path is reached via
        // the insert returning Some).
        *n = n.wrapping_add(1);
        if *n == 0 {
            *n = 1;
        }
        id
    })
}

fn store_handle(state: HttpState) -> u32 {
    let id = alloc_id();
    HTTP_HANDLES.with(|t| {
        t.borrow_mut().insert(id, state);
    });
    id
}

fn store_pollable(p: h_poll::Pollable) -> u32 {
    let id = alloc_id();
    POLLABLES.with(|t| {
        t.borrow_mut().insert(id, p);
    });
    id
}

fn with_handle<R>(
    h: u32,
    f: impl FnOnce(&mut HttpState) -> PyResult<R>,
) -> PyResult<R> {
    HTTP_HANDLES.with(|t| {
        let mut map = t.borrow_mut();
        let state = map.get_mut(&h).ok_or_else(|| {
            http_err("HTTP-handle-invalid", &format!("unknown http handle {h}"))
        })?;
        f(state)
    })
}

fn take_handle(h: u32) -> PyResult<HttpState> {
    HTTP_HANDLES.with(|t| {
        t.borrow_mut().remove(&h).ok_or_else(|| {
            http_err("HTTP-handle-invalid", &format!("unknown http handle {h}"))
        })
    })
}

// ---- submit ---------------------------------------------------------------

#[pyfunction]
#[pyo3(signature = (method, url, headers, body))]
pub fn _http_submit<'py>(
    _py: Python<'py>,
    method: &str,
    url: &str,
    headers: &Bound<'py, PyList>,
    body: &[u8],
) -> PyResult<u32> {
    let mut hs: Vec<(String, Vec<u8>)> = Vec::with_capacity(headers.len());
    for item in headers.iter() {
        let pair: (String, &[u8]) = item.extract()?;
        hs.push((pair.0, pair.1.to_vec()));
    }
    let future = build_and_send(method, url, hs, body)?;
    Ok(store_handle(HttpState::AwaitingResponse { future }))
}

/// Body writes still happen synchronously: most particle bodies are
/// small JSON payloads where polling per-chunk would be pointless
/// overhead. If we ever surface streaming uploads, this is the place to
/// move into the state machine.
fn build_and_send(
    method: &str,
    url: &str,
    headers: Vec<(String, Vec<u8>)>,
    body: &[u8],
) -> PyResult<h_types::FutureIncomingResponse> {
    let (scheme, authority, path_with_query) =
        split_url(url).map_err(|m| http_err("HTTP-request-URI-invalid", &m))?;

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
        .map_err(|e| http_err("HTTP-request-header-invalid", &format!("{e:?}")))?;

    let req = h_types::OutgoingRequest::new(fields);
    req.set_method(&to_wit_method(method))
        .map_err(|_| http_err("HTTP-request-method-invalid", method))?;
    req.set_scheme(Some(&to_wit_scheme(&scheme)))
        .map_err(|_| http_err("HTTP-request-URI-invalid", "set_scheme failed"))?;
    req.set_authority(Some(&authority))
        .map_err(|_| http_err("HTTP-request-URI-invalid", "set_authority failed"))?;
    req.set_path_with_query(Some(&path_with_query))
        .map_err(|_| http_err("HTTP-request-URI-invalid", "set_path_with_query failed"))?;

    let outgoing_body = req
        .body()
        .map_err(|_| http_err("HTTP-request-body-size", "OutgoingRequest::body failed"))?;
    let stream = outgoing_body
        .write()
        .map_err(|_| http_err("HTTP-request-body-size", "OutgoingBody::write failed"))?;

    let future = h_out::handle(req, None).map_err(|e| {
        let (k, d) = format_http_error(&e);
        http_err(k, &d)
    })?;

    let mut offset = 0;
    while offset < body.len() {
        let mut budget = stream
            .check_write()
            .map_err(|e| http_err("HTTP-request-body-size", &format!("check_write: {e:?}")))?;
        if budget == 0 {
            let p = stream.subscribe();
            p.block();
            budget = stream
                .check_write()
                .map_err(|e| http_err("HTTP-request-body-size", &format!("check_write: {e:?}")))?;
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
            .map_err(|e| http_err("HTTP-request-body-size", &format!("write: {e:?}")))?;
        offset += take;
    }

    drop(stream);
    h_types::OutgoingBody::finish(outgoing_body, None).map_err(|e| {
        let (k, d) = format_http_error(&e);
        http_err(k, &d)
    })?;

    Ok(future)
}

// ---- pollable for a handle's current state -------------------------------

#[pyfunction]
pub fn _http_pollable(h: u32) -> PyResult<u32> {
    with_handle(h, |state| match state {
        HttpState::AwaitingResponse { future } => Ok(store_pollable(future.subscribe())),
        HttpState::ReadingBody { stream, .. } => Ok(store_pollable(stream.subscribe())),
        HttpState::Done { .. } => Err(http_err(
            "HTTP-state-invalid",
            "handle is already complete; call _http_complete instead",
        )),
    })
}

// ---- advance state machine ------------------------------------------------

/// Returns:
///   0 — still pending (caller should poll the next pollable)
///   1 — done (caller should call _http_complete)
#[pyfunction]
pub fn _http_advance(h: u32) -> PyResult<u32> {
    // Two-phase: peek at the state, possibly transition. We can't hold a
    // &mut into the map across a state replacement, so transitions read
    // the old state out, build the new one, and put it back.
    let action = HTTP_HANDLES.with(|t| -> PyResult<&'static str> {
        let map = t.borrow();
        match map.get(&h) {
            Some(HttpState::AwaitingResponse { .. }) => Ok("await"),
            Some(HttpState::ReadingBody { .. }) => Ok("read"),
            Some(HttpState::Done { .. }) => Ok("done"),
            None => Err(http_err(
                "HTTP-handle-invalid",
                &format!("unknown http handle {h}"),
            )),
        }
    })?;

    match action {
        "await" => advance_awaiting(h),
        "read" => advance_reading(h),
        "done" => Ok(1),
        _ => unreachable!(),
    }
}

fn advance_awaiting(h: u32) -> PyResult<u32> {
    // Take the AwaitingResponse out so we can consume the future.
    let state = take_handle(h)?;
    let HttpState::AwaitingResponse { future } = state else {
        unreachable!("advance_awaiting called on non-AwaitingResponse state");
    };

    let response = match future.get() {
        None => {
            // Not ready yet. Put it back and tell caller to poll more.
            HTTP_HANDLES.with(|t| {
                t.borrow_mut()
                    .insert(h, HttpState::AwaitingResponse { future });
            });
            return Ok(0);
        }
        Some(res) => res
            .map_err(|_| http_err("HTTP-protocol-error", "future.get() called twice"))?
            .map_err(|e| {
                let (k, d) = format_http_error(&e);
                http_err(k, &d)
            })?,
    };

    let status = response.status();
    let headers = response.headers().entries();
    let incoming_body = response
        .consume()
        .map_err(|_| http_err("HTTP-response-incomplete", "IncomingResponse::consume failed"))?;
    let stream = incoming_body
        .stream()
        .map_err(|_| http_err("HTTP-response-incomplete", "IncomingBody::stream failed"))?;

    HTTP_HANDLES.with(|t| {
        t.borrow_mut().insert(
            h,
            HttpState::ReadingBody {
                status,
                headers,
                body_keep_alive: incoming_body,
                stream,
                buf: Vec::new(),
            },
        );
    });
    // Don't try to read immediately — the stream may not have any
    // bytes buffered yet, and a `read()` call on a fresh stream can
    // surface as a realloc-null trap rather than an empty Vec on some
    // hosts. Let the caller poll the stream's pollable first.
    Ok(0)
}

fn advance_reading(h: u32) -> PyResult<u32> {
    // Non-blocking read against the InputStream. Closed = EOF.
    let outcome = with_handle(h, |state| -> PyResult<&'static str> {
        let HttpState::ReadingBody { stream, buf, .. } = state else {
            unreachable!("advance_reading called on non-ReadingBody state");
        };
        match stream.read(READ_CHUNK) {
            Ok(chunk) => {
                if chunk.is_empty() {
                    // Reader is open but has no bytes right now. Caller
                    // re-polls the stream pollable for wakeup.
                    Ok("pending")
                } else {
                    buf.extend_from_slice(&chunk);
                    // More may be immediately available — but we still
                    // return "pending" so the caller drives one poll
                    // iteration per chunk. Simpler than spin-reading
                    // and keeps the event loop responsive.
                    Ok("pending")
                }
            }
            Err(h_streams::StreamError::Closed) => Ok("eof"),
            Err(h_streams::StreamError::LastOperationFailed(e)) => Err(http_err(
                "HTTP-response-incomplete",
                &e.to_debug_string(),
            )),
        }
    })?;

    if outcome == "eof" {
        // Drain the buffer out and transition to Done.
        let state = take_handle(h)?;
        let HttpState::ReadingBody {
            status, headers, buf, ..
        } = state
        else {
            unreachable!("eof outcome on non-ReadingBody state");
        };
        HTTP_HANDLES.with(|t| {
            t.borrow_mut().insert(
                h,
                HttpState::Done {
                    status,
                    headers,
                    body: buf,
                },
            );
        });
        Ok(1)
    } else {
        Ok(0)
    }
}

// ---- complete: drain final result, drop handle ---------------------------

#[pyfunction]
pub fn _http_complete<'py>(py: Python<'py>, h: u32) -> PyResult<Bound<'py, PyDict>> {
    let state = take_handle(h)?;
    let HttpState::Done {
        status,
        headers,
        body,
    } = state
    else {
        return Err(http_err(
            "HTTP-state-invalid",
            "handle not yet complete; call _http_advance until it returns 1",
        ));
    };

    let out = PyDict::new(py);
    out.set_item("status", status as u32)?;
    let hpl = PyList::empty(py);
    for (k, v) in headers {
        let pair = PyList::empty(py);
        pair.append(k)?;
        pair.append(PyBytes::new(py, &v))?;
        hpl.append(pair)?;
    }
    out.set_item("headers", hpl)?;
    out.set_item("body", PyBytes::new(py, &body))?;
    Ok(out)
}

// ---- io poll --------------------------------------------------------------

/// Block until at least one pollable in `handles` is ready. Returns the
/// list of indices (into `handles`) that are ready.
///
/// `timeout_ms`, when non-None, adds an internal clock pollable to the
/// poll list with that duration. Its index is NOT returned to Python —
/// a timeout simply manifests as an empty return list. None = wait
/// forever (no clock pollable added).
#[pyfunction]
#[pyo3(signature = (handles, timeout_ms))]
pub fn _io_poll(handles: Vec<u32>, timeout_ms: Option<u64>) -> PyResult<Vec<u32>> {
    if handles.is_empty() && timeout_ms.is_none() {
        // poll([]) traps. Empty + no timeout would deadlock anyway —
        // surface as a typed error so the bug is visible.
        return Err(PyValueError::new_err(
            "_io_poll: empty handles list with no timeout would deadlock",
        ));
    }

    POLLABLES.with(|t| {
        let map = t.borrow();

        // Borrow each registered pollable. If any handle is unknown,
        // bail before constructing the borrowed list.
        let mut borrowed: Vec<&h_poll::Pollable> = Vec::with_capacity(handles.len() + 1);
        for h in &handles {
            let p = map.get(h).ok_or_else(|| {
                http_err("io-pollable-invalid", &format!("unknown pollable handle {h}"))
            })?;
            borrowed.push(p);
        }

        // Optionally add a clock pollable. It outlives the poll call;
        // dropping it after collects the resource.
        let clock_pollable = timeout_ms.map(|ms| h_clock::subscribe_duration(ms.saturating_mul(1_000_000)));
        if let Some(ref cp) = clock_pollable {
            borrowed.push(cp);
        }

        let ready = h_poll::poll(&borrowed);

        // Translate indices back, skipping the clock entry if present.
        let clock_idx = clock_pollable.as_ref().map(|_| handles.len() as u32);
        Ok(ready
            .into_iter()
            .filter(|i| Some(*i) != clock_idx)
            .map(|i| handles[i as usize])
            .collect())
    })
}

// ---- pollable lifecycle ---------------------------------------------------

#[pyfunction]
pub fn _pollable_drop(p: u32) -> PyResult<()> {
    POLLABLES.with(|t| {
        if t.borrow_mut().remove(&p).is_none() {
            return Err(http_err(
                "io-pollable-invalid",
                &format!("unknown pollable handle {p}"),
            ));
        }
        Ok(())
    })
}

/// Drop an http handle (cancels in-flight work). Idempotent — unknown
/// handles are silently ignored so users can call this in `__del__`
/// without ordering anxiety.
#[pyfunction]
pub fn _http_drop(h: u32) -> PyResult<()> {
    HTTP_HANDLES.with(|t| {
        t.borrow_mut().remove(&h);
    });
    Ok(())
}
