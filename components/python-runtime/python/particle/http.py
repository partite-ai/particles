"""Outbound HTTP for Python particles.

Every outbound HTTP request a particle makes must go through this
module — directly via `fetch()`, or transparently via the urllib /
urllib3 / httpx hooks (see `_urllib_compat`, `_urllib3_compat`,
`_httpx_compat`). Going through wasi:http (rather than wasi:sockets)
is what gives the host:

  - the chance to enforce the manifest's HTTP allowlist,
  - credential placeholder substitution at the wire boundary,
  - audit visibility into where particles connect.

Raw socket access is intentionally not provided. wasi:sockets is not
imported by the runtime's world, and `particle._socket_guard`
replaces `socket.socket` with an OSError-raising stub so libraries
that probe for socket support at import time fail catchably.

The wasi:http state machine (OutgoingRequest → OutgoingBody → write
→ FutureIncomingResponse → poll → IncomingResponse → IncomingBody
→ read) is implemented in Rust inside `_runtime_host._http_request`
which presents a single blocking call. This module marshals headers
to the list shape the bridge expects and lifts the dict result back
into a `Response` object.
"""

import json as _json
import urllib.parse

import _runtime_host

from . import credentials as _credentials

__all__ = ["Response", "fetch", "async_fetch"]


class Response:
    """A read-eagerly HTTP response.

    Bytes are buffered in full before this object is returned to the
    caller; particles deal in small JSON payloads and the convenience
    of `.json()` / `.text` outweighs streaming. The Rust bridge reads
    the IncomingBody to EOF before returning, so there's no streaming
    surface to expose anyway.
    """

    def __init__(self, status: int, headers: list, body: bytes):
        # `headers` is the list-of-[name, value-bytes] coming out of
        # _runtime_host._http_request. We keep the original casing in
        # `.headers` for callers that need it AND a lower-cased lookup
        # dict for forgiving access.
        self.status_code = status
        self.headers = [(k, v) for k, v in headers]
        self._header_lookup = {k.lower(): v for k, v in self.headers}
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


def fetch(
    url: str,
    *,
    method: str = "GET",
    headers: dict | None = None,
    body: bytes | str | None = None,
    credential_name: str | None = None,
) -> Response:
    """Synchronously issue an HTTP request through wasi:http.

    `credential_name`, when set, attaches the named credential's
    placeholder to the request at the location its `apply-spec`
    dictates (Authorization header, custom header, query param,
    ...). The host substitutes the real secret at the wire boundary
    on the way out — the placeholder string never leaves the
    sandbox unsubstituted, and the secret never enters Python
    memory.
    """
    if credential_name is not None:
        info = _credentials.get_placeholder(credential_name)
        url, headers = _apply_placeholder(
            url, headers or {}, info.placeholder, info.apply,
        )

    # Normalize args for the Rust bridge: header dict → list of
    # (name, value-bytes) tuples (PyO3's `(String, &[u8])` extractor
    # rejects lists), body → bytes (empty if None).
    hdr_pairs: list = []
    for k, v in (headers or {}).items():
        hdr_pairs.append((str(k), _encode_header(v)))
    body_bytes = _normalize_body(body)

    out = _runtime_host._http_request(method.upper(), url, hdr_pairs, body_bytes)
    return Response(
        status=out["status"],
        headers=out["headers"],
        body=out["body"],
    )


async def async_fetch(
    url: str,
    *,
    method: str = "GET",
    headers: dict | None = None,
    body: bytes | str | None = None,
    credential_name: str | None = None,
) -> Response:
    """Asynchronous counterpart to `fetch`. Drives wasi:http through
    `particle._wasi_async.WasiHttpFuture` so the in-flight request
    yields to the asyncio event loop between submit and response —
    other coroutines (including other `async_fetch` calls in an
    `asyncio.gather`) can make progress in parallel.

    Semantically equivalent to `fetch` in every other respect:
    credential placeholders flow through the same wasi:http
    boundary, headers / body are normalized identically, and the
    returned `Response` is the same shape.
    """
    # Lazy import to keep the loop module off the cold path for
    # particles that never touch async.
    from ._wasi_async import WasiHttpFuture

    if credential_name is not None:
        info = _credentials.get_placeholder(credential_name)
        url, headers = _apply_placeholder(
            url, headers or {}, info.placeholder, info.apply,
        )

    hdr_pairs: list = []
    for k, v in (headers or {}).items():
        hdr_pairs.append((str(k), _encode_header(v)))
    body_bytes = _normalize_body(body)

    out = await WasiHttpFuture(method, url, hdr_pairs, body_bytes)
    return Response(
        status=out["status"],
        headers=out["headers"],
        body=out["body"],
    )


def _normalize_body(body) -> bytes:
    if body is None:
        return b""
    if isinstance(body, bytes):
        return body
    if isinstance(body, bytearray) or isinstance(body, memoryview):
        return bytes(body)
    if isinstance(body, str):
        return body.encode("utf-8")
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
    apply,
) -> tuple:
    """Place the credential placeholder at the location the host
    expects.

    The ApplyKind enum is set host-side from the credential's
    configured method (Basic / Bearer / custom header / arbitrary
    auth-scheme / query param); this function only re-shapes the
    URL or headers accordingly. The host then substitutes the real
    secret for the placeholder at the wasi:http boundary.
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
