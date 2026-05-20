"""Outbound HTTP for Python particles (mirrors @partite-ai/particle-http).

Every outbound HTTP request a particle makes must go through this
module — directly via `fetch()` / `fetcher()`, or transparently via
the urllib monkey-patch installed at module load. Going through
wasi:http (rather than wasi:sockets) is what gives the host:

  - the chance to enforce the manifest's HTTP allowlist,
  - credential placeholder substitution at the wire boundary,
  - audit visibility into where particles connect.

Raw socket access is intentionally not provided. wasi:sockets is not
imported by the runtime's world, so even a user trying to use the
`socket` module gets an unbound import error.
"""

import asyncio
import json as _json
import urllib.parse
import urllib.request as _urllib_request

from componentize_py_types import Err, Ok
from wit_world.imports import types as _http_types
from wit_world.imports.types import (
    Fields,
    Method_Connect,
    Method_Delete,
    Method_Get,
    Method_Head,
    Method_Options,
    Method_Patch,
    Method_Post,
    Method_Put,
    Method_Trace,
    OutgoingRequest,
    Scheme_Http,
    Scheme_Https,
    Scheme_Other,
)

from particle import credentials as _credentials
from poll_loop import PollLoop as _PollLoop, Sink as _Sink, Stream as _Stream
from poll_loop import send as _send

__all__ = ["Response", "fetch", "fetcher", "install_urllib_patch"]


# -- Response ------------------------------------------------------------


class Response:
    """A read-eagerly HTTP response.

    Bytes are buffered in full before this object is returned to the
    caller; particles deal in small JSON payloads and the convenience
    of `.json()` / `.text` outweighs streaming. Callers who need
    streaming can drop down to `poll_loop.send` + `poll_loop.Stream`.
    """

    def __init__(self, status: int, headers: list[tuple[str, bytes]], body: bytes):
        self.status_code = status
        # Lower-case header names to give a forgiving lookup, but
        # preserve the original casing in the `.headers` list for
        # callers that need it.
        self.headers = headers
        self._header_lookup = {k.lower(): v for k, v in headers}
        self.body = body

    def header(self, name: str) -> bytes | None:
        return self._header_lookup.get(name.lower())

    @property
    def text(self) -> str:
        return self.body.decode("utf-8", errors="replace")

    def json(self):
        return _json.loads(self.body)

    @property
    def ok(self) -> bool:
        return 200 <= self.status_code < 300


# -- Public API ----------------------------------------------------------


def fetch(
    url: str,
    *,
    method: str = "GET",
    headers: dict | None = None,
    body: bytes | str | None = None,
) -> Response:
    """Synchronously issue an HTTP request through wasi:http.

    Synchronous on the outside, async on the inside: the wasi:http
    bindings are poll-based, so we spin up a one-shot asyncio loop
    backed by wasi:io/poll. Particles run one tool call at a time
    inside an instance (design doc §6 concurrency), so the sync
    surface is what handler authors actually want.
    """
    return _run_sync(_fetch_async(url, method=method, headers=headers, body=body))


def fetcher(credential_name: str):
    """Return a `fetch`-like callable that applies the named
    credential's placeholder to every outgoing request — the Python
    analog of `credentials.fetcher(name)` in the JS DSL.
    """
    info = _credentials.get_placeholder(credential_name)
    apply = info.apply

    def call(url: str, *, method: str = "GET", headers: dict | None = None,
             body: bytes | str | None = None) -> Response:
        url2, headers2 = _apply_placeholder(url, headers or {}, info.placeholder, apply)
        return fetch(url2, method=method, headers=headers2, body=body)

    return call


# -- urllib monkey-patch -------------------------------------------------


def install_urllib_patch() -> None:
    """Route `urllib.request.urlopen` through wasi:http.

    Most third-party Python packages (requests, httpx in sync mode,
    raw urllib, etc.) end up at urllib.request.urlopen or
    http.client.HTTP(S)Connection.request. Patching urlopen catches
    the common case; we can extend to http.client if needed.

    Called at module import time so the patch is in place before user
    code's first `urllib.request.urlopen(...)`.
    """
    if getattr(_urllib_request, "_particle_patched", False):
        return

    _original_urlopen = _urllib_request.urlopen

    def _patched_urlopen(url, data=None, timeout=None, *args, **kwargs):
        # urllib accepts either a string URL or a Request object.
        if isinstance(url, _urllib_request.Request):
            req = url
            target = req.full_url
            method = req.get_method()
            req_headers = {k: v for k, v in req.header_items()}
            req_body = req.data if data is None else data
        else:
            target = url
            method = "POST" if data is not None else "GET"
            req_headers = {}
            req_body = data

        resp = fetch(target, method=method, headers=req_headers, body=req_body)
        return _UrllibResponseAdapter(resp, target)

    _urllib_request.urlopen = _patched_urlopen
    _urllib_request._particle_patched = True
    _urllib_request._particle_original_urlopen = _original_urlopen


class _UrllibResponseAdapter:
    """Looks like the file-like object urllib.request.urlopen returns.

    Real urllib responses are `http.client.HTTPResponse` instances;
    we expose the subset that's actually used in the wild —
    .read(), .getcode(), .geturl(), .headers (with .get()), context
    manager protocol. Good enough for requests, httpx-sync, etc.
    """

    def __init__(self, resp: Response, url: str):
        self._resp = resp
        self._url = url
        self._consumed = False

    def read(self, amt: int | None = None) -> bytes:
        if self._consumed:
            return b""
        self._consumed = True
        return self._resp.body if amt is None else self._resp.body[:amt]

    def getcode(self) -> int:
        return self._resp.status_code

    @property
    def status(self) -> int:
        return self._resp.status_code

    def geturl(self) -> str:
        return self._url

    @property
    def headers(self):
        return _UrllibHeaders(self._resp.headers)

    def info(self):
        return self.headers

    def __enter__(self):
        return self

    def __exit__(self, *_):
        return False

    def close(self):
        return None


class _UrllibHeaders:
    """Tiny shim mimicking `email.message.Message`-style header access
    so caller `.get("content-type", default)` keeps working.
    """

    def __init__(self, entries: list[tuple[str, bytes]]):
        self._entries = entries
        self._lookup = {k.lower(): v.decode("latin-1") for k, v in entries}

    def get(self, name: str, default=None):
        return self._lookup.get(name.lower(), default)

    def __getitem__(self, name: str):
        v = self._lookup.get(name.lower())
        if v is None:
            raise KeyError(name)
        return v

    def __contains__(self, name: str):
        return name.lower() in self._lookup

    def items(self):
        return [(k, v.decode("latin-1")) for k, v in self._entries]


# -- Internals -----------------------------------------------------------


_METHOD_MAP = {
    "GET": Method_Get(),
    "HEAD": Method_Head(),
    "POST": Method_Post(),
    "PUT": Method_Put(),
    "DELETE": Method_Delete(),
    "CONNECT": Method_Connect(),
    "OPTIONS": Method_Options(),
    "TRACE": Method_Trace(),
    "PATCH": Method_Patch(),
}


def _to_wit_method(method: str):
    m = _METHOD_MAP.get(method.upper())
    if m is None:
        raise ValueError(f"unsupported HTTP method {method!r}")
    return m


def _to_wit_scheme(scheme: str):
    s = scheme.lower()
    if s == "http":
        return Scheme_Http()
    if s == "https":
        return Scheme_Https()
    return Scheme_Other(scheme)


def _run_sync(coro):
    """Run an async coroutine to completion on a fresh PollLoop.

    componentize-py provides PollLoop (an asyncio event loop backed
    by wasi:io/poll); using it lets us await wasi:http futures while
    keeping the outer API synchronous, which is what particle tool
    handlers actually want.
    """
    loop = _PollLoop()
    asyncio.set_event_loop(loop)
    try:
        return loop.run_until_complete(coro)
    finally:
        asyncio.set_event_loop(None)


async def _fetch_async(
    url: str,
    *,
    method: str,
    headers: dict | None,
    body: bytes | str | None,
) -> Response:
    parsed = urllib.parse.urlsplit(url)
    if not parsed.scheme or not parsed.netloc:
        raise ValueError(f"fetch: url is missing scheme/netloc: {url!r}")

    path_with_query = parsed.path or "/"
    if parsed.query:
        path_with_query += "?" + parsed.query

    field_entries: list[tuple[str, bytes]] = []
    has_host = False
    for k, v in (headers or {}).items():
        kn = k.lower()
        if kn == "host":
            has_host = True
        field_entries.append((k, _encode_header(v)))
    if not has_host:
        field_entries.append(("host", parsed.netloc.encode("latin-1")))

    fields = Fields.from_list(field_entries)
    req = OutgoingRequest(fields)
    req.set_method(_to_wit_method(method))
    req.set_scheme(_to_wit_scheme(parsed.scheme))
    req.set_authority(parsed.netloc)
    req.set_path_with_query(path_with_query)

    body_bytes = _normalize_body(body)
    out_body = req.body()
    sink = _Sink(out_body)

    # The wasi:http binding requires `send(request)` to be in flight
    # before we write the body (the runtime needs the headers committed
    # before any bytes flow). poll_loop.send awaits the future for us.
    send_task = asyncio.create_task(_send(req))
    if body_bytes:
        await sink.send(body_bytes)
    sink.close()

    incoming = await send_task

    status = incoming.status()
    resp_headers = incoming.headers().entries()
    body_obj = incoming.consume()
    stream = _Stream(body_obj)
    chunks: list[bytes] = []
    while True:
        chunk = await stream.next()
        if chunk is None:
            break
        chunks.append(chunk)

    return Response(status=status, headers=resp_headers, body=b"".join(chunks))


def _normalize_body(body) -> bytes:
    if body is None:
        return b""
    if isinstance(body, bytes):
        return body
    if isinstance(body, str):
        return body.encode("utf-8")
    # File-like — read once.
    if hasattr(body, "read"):
        data = body.read()
        if isinstance(data, str):
            data = data.encode("utf-8")
        return data
    raise TypeError(f"unsupported body type: {type(body).__name__}")


def _encode_header(v) -> bytes:
    if isinstance(v, bytes):
        return v
    if isinstance(v, str):
        return v.encode("latin-1")
    return str(v).encode("latin-1")


def _apply_placeholder(
    url: str,
    headers: dict,
    placeholder: str,
    apply: "_credentials.ApplySpec",
) -> tuple[str, dict]:
    """Place the credential placeholder at the location the host
    expects. Matches the JS-side applyPlaceholder in
    components/js-runtime/src/host-shim.ts.
    """
    kind = apply.kind
    new_headers = dict(headers)
    if kind == _credentials.ApplyKind.BASIC:
        new_headers["Authorization"] = f"Basic {placeholder}"
        return url, new_headers
    if kind == _credentials.ApplyKind.BEARER:
        new_headers["Authorization"] = f"Bearer {placeholder}"
        return url, new_headers
    if kind == _credentials.ApplyKind.HEADER:
        new_headers[apply.name or ""] = placeholder
        return url, new_headers
    if kind == _credentials.ApplyKind.AUTH_SCHEME:
        new_headers["Authorization"] = f"{apply.scheme or ''} {placeholder}"
        return url, new_headers
    if kind == _credentials.ApplyKind.QUERY_PARAM:
        parsed = urllib.parse.urlsplit(url)
        query = urllib.parse.parse_qsl(parsed.query, keep_blank_values=True)
        query.append((apply.name or "", placeholder))
        rebuilt = parsed._replace(query=urllib.parse.urlencode(query))
        return urllib.parse.urlunsplit(rebuilt), new_headers
    raise ValueError(f"unknown apply kind: {kind!r}")


# Install the monkey-patch at import time so it's in place before any
# user code runs (the bootstrap imports `particle.http` before loading
# the user bundle).
install_urllib_patch()
