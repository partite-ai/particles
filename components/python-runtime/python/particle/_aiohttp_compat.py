"""
aiohttp auto-hook — routes `aiohttp.ClientSession` requests through
`particle.http.async_fetch` (wasi:http via the asyncio-integrated
event loop) instead of aiohttp's TCPConnector + asyncio.Transport
stack, which would need real socket FDs we don't have.

Strategy: meta_path watcher, same shape as `_urllib3_compat` /
`_httplib2_compat`. When user code first imports `aiohttp`, we wrap
the loader so a small `inject_into_aiohttp()` runs after the module
is exec'd. The patch replaces `ClientSession._request` with an
async function that:

  1. Translates aiohttp's kwargs (`headers`, `data` / `json` /
     `params`) into the shape `particle.http.async_fetch` wants.
  2. Awaits `async_fetch`, which submits the request via
     `_runtime_host._http_submit` and yields back to the event loop
     until the wasi pollable fires.
  3. Returns a `_ParticleClientResponse` that quacks like
     `aiohttp.ClientResponse` for the surface most callers touch
     (`.status`, `.headers`, `.read()`, `.text()`, `.json()`,
     `.content` StreamReader, async context manager).

What we don't patch (and why)
-----------------------------
- `ws_connect` / WebSockets: needs wasi:http subprotocol upgrade,
  which we don't have. Calls will reach aiohttp's normal connector
  path and fail there with the usual socket error.
- Streaming uploads (`data=` async iterable): we error early. wasi:
  http's OutgoingBody write path is synchronous in our Rust shim;
  proper streaming would mean threading a write-pollable through
  the state machine.
- Trace configs / middlewares: still registered, but since we
  bypass the connector they don't fire. If a particle relies on
  `aiohttp.TraceConfig` for instrumentation, that observability is
  lost (open a bug).
"""

import asyncio
import importlib.abc
import json as _json
import sys

__all__ = ["inject_into_aiohttp", "install_watcher"]

_patched = False


def _build_classes():
    """Construct the response / stream wrapper classes lazily, after
    aiohttp itself has finished loading. Imports go through the real
    aiohttp module — we only need a few type references."""

    import aiohttp
    from multidict import CIMultiDict, CIMultiDictProxy

    from particle.http import async_fetch as _async_fetch

    # -- StreamReader-shaped wrapper around a buffered body ----------

    class _ParticleStreamReader:
        """Minimal aiohttp.StreamReader stand-in. Whole response body
        is already in memory (`particle.http.async_fetch` reads to
        EOF), so we just slice the buffer."""

        def __init__(self, body: bytes):
            self._body = body
            self._pos = 0

        @property
        def total_bytes(self) -> int:
            return len(self._body)

        def at_eof(self) -> bool:
            return self._pos >= len(self._body)

        def exception(self):
            return None

        async def read(self, n: int = -1) -> bytes:
            if n is None or n < 0:
                chunk = self._body[self._pos :]
                self._pos = len(self._body)
                return chunk
            chunk = self._body[self._pos : self._pos + n]
            self._pos += len(chunk)
            return chunk

        async def readany(self) -> bytes:
            return await self.read(-1)

        async def readexactly(self, n: int) -> bytes:
            chunk = await self.read(n)
            if len(chunk) < n:
                raise asyncio.IncompleteReadError(chunk, n)
            return chunk

        async def readline(self) -> bytes:
            nl = self._body.find(b"\n", self._pos)
            if nl < 0:
                return await self.read(-1)
            chunk = self._body[self._pos : nl + 1]
            self._pos = nl + 1
            return chunk

        def iter_chunked(self, n: int):
            sr = self

            async def _gen():
                while not sr.at_eof():
                    chunk = await sr.read(n)
                    if not chunk:
                        return
                    yield chunk

            return _gen()

        def iter_any(self):
            return self.iter_chunked(65536)

        def iter_chunks(self):
            # aiohttp's iter_chunks yields (data, end_of_http_chunk).
            # We don't see HTTP chunk boundaries (body is buffered);
            # report end_of_http_chunk=True on each yield.
            sr = self

            async def _gen():
                if sr.at_eof():
                    return
                yield (await sr.read(-1), True)

            return _gen()

    # -- ClientResponse stand-in -------------------------------------

    class _ParticleClientResponse:
        """aiohttp.ClientResponse-shaped wrapper around a finished
        particle.http.Response. Implements the surface
        most callers touch."""

        def __init__(self, presp, method: str, url):
            self.method = method
            self.url = url  # yarl.URL or str — preserved as-is
            self.real_url = url
            self.status = presp.status_code
            self.reason = ""
            self.version = aiohttp.HttpVersion11
            self.cookies = http_cookies_from_headers(presp.headers)
            self._raw_headers = presp.headers
            self.headers = _decode_headers(presp.headers)
            self._body_bytes = presp.body
            self.content = _ParticleStreamReader(presp.body)
            self._released = False
            # aiohttp clients sometimes pluck content_type off the
            # response; pre-compute it so .content_type and friends work.
            ct = self.headers.get("Content-Type", "")
            self.content_type, _, params = ct.partition(";")
            self.content_type = self.content_type.strip().lower()
            self._charset = None
            for p in params.split(";"):
                p = p.strip()
                if p.lower().startswith("charset="):
                    self._charset = p.split("=", 1)[1].strip().strip('"').strip("'")
                    break

        @property
        def ok(self) -> bool:
            return 200 <= self.status < 400

        def raise_for_status(self):
            if self.status >= 400:
                raise aiohttp.ClientResponseError(
                    request_info=None,
                    history=(),
                    status=self.status,
                    message=self.reason or f"HTTP {self.status}",
                    headers=self.headers,
                )

        async def read(self) -> bytes:
            return self._body_bytes

        async def text(self, encoding: str | None = None) -> str:
            enc = encoding or self._charset or "utf-8"
            return self._body_bytes.decode(enc, errors="replace")

        async def json(self, *, encoding: str | None = None,
                       loads=_json.loads, content_type: str | None = "application/json"):
            # Mirror aiohttp's content-type assertion knob: pass
            # content_type=None to skip the check (some APIs serve
            # JSON with the wrong type).
            if content_type is not None:
                actual = self.content_type
                if actual != content_type and not actual.endswith("+json"):
                    raise aiohttp.ContentTypeError(
                        request_info=None,
                        history=(),
                        status=self.status,
                        message=f"unexpected content type {actual!r}",
                        headers=self.headers,
                    )
            text = await self.text(encoding=encoding)
            return loads(text)

        # -- lifecycle: no underlying connection, so these are cheap --

        def release(self):
            self._released = True

        async def __aenter__(self):
            return self

        async def __aexit__(self, *exc):
            self.release()
            return False

        def close(self):
            self._released = True

        @property
        def closed(self) -> bool:
            return self._released

    def http_cookies_from_headers(headers):
        # aiohttp exposes a SimpleCookie-shaped .cookies. We construct
        # one from any Set-Cookie headers; the client never persists
        # them (no shared cookie jar in this shim), but reads still
        # work for callers that inspect resp.cookies on a single
        # response.
        from http.cookies import SimpleCookie

        jar = SimpleCookie()
        for k, v in headers:
            if k.lower() == "set-cookie":
                if isinstance(v, bytes):
                    v = v.decode("latin-1")
                try:
                    jar.load(v)
                except Exception:
                    pass
        return jar

    def _decode_headers(pairs):
        d = CIMultiDict()
        for k, v in pairs:
            if isinstance(v, bytes):
                v = v.decode("latin-1")
            d.add(k, v)
        return CIMultiDictProxy(d)

    return _ParticleClientResponse, _ParticleStreamReader, _async_fetch


def inject_into_aiohttp() -> None:
    """Replace `aiohttp.ClientSession._request` with our wasi:http-
    routed equivalent. Idempotent."""
    global _patched
    if _patched:
        return
    try:
        import aiohttp
    except ImportError:
        return

    _ParticleClientResponse, _ParticleStreamReader, async_fetch = _build_classes()

    async def _patched_request(self, method, str_or_url, **kwargs):
        # ---- URL: accept str or yarl.URL --------------------------
        url = str(str_or_url)
        # `params` is appended as a query string.
        params = kwargs.get("params")
        if params:
            from urllib.parse import urlencode, urlparse, urlunparse, parse_qsl

            parts = urlparse(url)
            existing = parse_qsl(parts.query, keep_blank_values=True)
            if isinstance(params, dict):
                existing.extend(params.items())
            else:
                existing.extend(params)
            url = urlunparse(parts._replace(query=urlencode(existing, doseq=True)))

        # ---- Headers / body ---------------------------------------
        out_headers = {}
        for k, v in (kwargs.get("headers") or {}).items():
            if k.lower() == "accept-encoding":
                # See _urllib3_compat — let the server send identity-
                # encoded bytes; we don't decompress at this layer.
                continue
            out_headers[k] = v

        body = None
        if kwargs.get("json") is not None:
            body = _json.dumps(kwargs["json"]).encode("utf-8")
            out_headers.setdefault("Content-Type", "application/json")
        elif kwargs.get("data") is not None:
            data = kwargs["data"]
            if isinstance(data, (bytes, bytearray, memoryview)):
                body = bytes(data)
            elif isinstance(data, str):
                body = data.encode("utf-8")
            elif hasattr(data, "read"):
                # File-like; assume it gives us bytes.
                body = data.read()
                if isinstance(body, str):
                    body = body.encode("utf-8")
            elif isinstance(data, dict):
                # Treat as form data.
                from urllib.parse import urlencode

                body = urlencode(data, doseq=True).encode("utf-8")
                out_headers.setdefault(
                    "Content-Type", "application/x-www-form-urlencoded"
                )
            elif hasattr(data, "__aiter__"):
                raise NotImplementedError(
                    "aiohttp streaming uploads (async-iterable data) are not "
                    "supported by particle's wasi:http shim — buffer the body "
                    "before passing it"
                )
            elif isinstance(data, aiohttp.FormData):
                # FormData has a writer-based serialization; the high-
                # level shape is similar to urlencode for simple cases.
                # We delegate to its internal _gen_form_urlencoded /
                # _gen_form_data if available, else error.
                raise NotImplementedError(
                    "aiohttp.FormData is not yet supported by particle's "
                    "wasi:http shim — encode the body explicitly"
                )
            else:
                raise TypeError(
                    f"unsupported aiohttp data type: {type(data).__name__}"
                )

        # ---- Cookies ----------------------------------------------
        # If user passed cookies= and there's no Cookie header yet,
        # serialize them. Simple key=value form, semicolon-separated.
        cookies = kwargs.get("cookies")
        if cookies and "Cookie" not in {k.title() for k in out_headers}:
            if hasattr(cookies, "items"):
                pairs = list(cookies.items())
            else:
                pairs = list(cookies)
            out_headers["Cookie"] = "; ".join(f"{k}={v}" for k, v in pairs)

        # ---- Off we go --------------------------------------------
        try:
            resp = await async_fetch(
                url,
                method=method,
                headers=out_headers or None,
                body=body,
            )
        except Exception as e:
            # Wrap into aiohttp.ClientError so callers catching
            # the aiohttp exception hierarchy still catch it.
            if isinstance(e, aiohttp.ClientError):
                raise
            raise aiohttp.ClientConnectionError(str(e)) from e

        return _ParticleClientResponse(resp, method, str_or_url)

    aiohttp.ClientSession._request = _patched_request
    _patched = True


# -- Auto-detect: meta_path watcher ------------------------------------


class _AiohttpPostLoadFinder(importlib.abc.MetaPathFinder):
    def find_spec(self, fullname, path, target=None):
        if fullname != "aiohttp":
            return None
        try:
            idx = sys.meta_path.index(self)
        except ValueError:
            return None
        for finder in sys.meta_path[idx + 1 :]:
            if finder is self:
                continue
            try:
                spec = finder.find_spec(fullname, path, target)
            except (ImportError, AttributeError):
                continue
            if spec is None:
                continue
            if spec.loader is not None:
                spec.loader = _PostExecLoader(spec.loader)
            return spec
        return None


class _PostExecLoader:
    def __init__(self, real):
        self._real = real

    def create_module(self, spec):
        if hasattr(self._real, "create_module"):
            return self._real.create_module(spec)
        return None

    def exec_module(self, module):
        self._real.exec_module(module)
        try:
            inject_into_aiohttp()
        except Exception:
            pass

    def __getattr__(self, name):
        return getattr(self._real, name)


def install_watcher() -> None:
    if "aiohttp" in sys.modules:
        inject_into_aiohttp()
        return
    if not any(isinstance(f, _AiohttpPostLoadFinder) for f in sys.meta_path):
        sys.meta_path.insert(0, _AiohttpPostLoadFinder())


install_watcher()
