"""
httpx auto-hook — routes httpx clients through wasi:http via a custom
`BaseTransport`. Same strategy as `_urllib3_compat`: watch
`sys.meta_path` for `httpx`, patch `HTTPTransport` /
`AsyncHTTPTransport` once it loads, before any `httpx.Client` /
`httpx.get(...)` call has constructed a transport.

httpx is harder to hook than urllib3 because its default transport
(`HTTPTransport`) drives `httpcore` directly — not urllib3 — so the
urllib3 patch doesn't reach it. We replace the transport class
itself; everything `httpx.Client(...)` constructs by default goes
through our wasi:http path.

No httpx dependency: the module-level watcher only fires if a user
particle imports it.
"""

import importlib.abc
import sys

__all__ = ["inject_into_httpx", "install_watcher"]

_patched = False


def _build_transport_classes():
    """Define Particle{,Async}HTTPTransport lazily, after httpx has
    finished loading. Keeps us from importing httpx ourselves."""
    import httpx

    from particle import http as particle_http

    def _request_to_fetch_args(request):
        """Lower a httpx.Request into the args particle.http.fetch
        expects: (url, method, headers).

        We strip `Accept-Encoding` — particle.http does no
        decompression and the response is returned to httpx
        opaquely; if we let the server gzip the body, `r.text`
        would yield garbage. Stripping forces identity encoding.
        """
        url = str(request.url)
        method = request.method
        headers = {}
        for name, value in request.headers.raw:
            if isinstance(name, bytes):
                name_str = name.decode("latin-1")
            else:
                name_str = name
            if name_str.lower() == "accept-encoding":
                continue
            if isinstance(value, bytes):
                value = value.decode("latin-1")
            headers[name_str] = value
        return url, method, headers

    def _build_response(resp, request):
        """Lift a particle.http.Response into an httpx.Response."""
        hdr_list = []
        for k, v in resp.headers:
            if isinstance(v, bytes):
                v = v.decode("latin-1")
            hdr_list.append((k, v))
        # `content=...` lets httpx auto-populate Content-Length if
        # absent; here the upstream headers already carry it, so
        # httpx leaves them alone. `extensions` are informational
        # and read by `response.http_version` / `.reason_phrase`.
        return httpx.Response(
            status_code=resp.status_code,
            headers=hdr_list,
            content=resp.body,
            request=request,
            extensions={
                "http_version": b"HTTP/1.1",
                "reason_phrase": b"",
            },
        )

    class ParticleHTTPTransport(httpx.BaseTransport):
        """Drop-in replacement for `httpx.HTTPTransport`.

        Discards every TLS / pooling / proxy / http2 kwarg that the
        real transport accepts — wasi:http governs all of that
        host-side, so configuring it from the guest would be
        misleading."""

        def __init__(self, *_args, **_kwargs):
            pass

        def handle_request(self, request):
            url, method, headers = _request_to_fetch_args(request)
            body_chunks = []
            for chunk in request.stream:
                body_chunks.append(chunk)
            body = b"".join(body_chunks) or None
            try:
                resp = particle_http.fetch(
                    url, method=method, headers=headers, body=body,
                )
            except Exception as e:
                raise httpx.TransportError(str(e)) from e
            return _build_response(resp, request)

        def close(self):
            pass

    class ParticleAsyncHTTPTransport(httpx.AsyncBaseTransport):
        """Async counterpart. particle.http.fetch is sync internally
        (it runs a one-shot PollLoop); we expose `async` to satisfy
        httpx.AsyncClient's contract but the actual I/O is
        sequential."""

        def __init__(self, *_args, **_kwargs):
            pass

        async def handle_async_request(self, request):
            url, method, headers = _request_to_fetch_args(request)
            body_chunks = []
            async for chunk in request.stream:
                body_chunks.append(chunk)
            body = b"".join(body_chunks) or None
            try:
                resp = particle_http.fetch(
                    url, method=method, headers=headers, body=body,
                )
            except Exception as e:
                raise httpx.TransportError(str(e)) from e
            return _build_response(resp, request)

        async def aclose(self):
            pass

    return ParticleHTTPTransport, ParticleAsyncHTTPTransport


def inject_into_httpx() -> None:
    """Replace httpx's default transport classes with ours. Mirrors
    `_urllib3_compat.inject_into_urllib3`. Idempotent."""
    global _patched
    if _patched:
        return
    try:
        import httpx
        from httpx import _client as httpx_client
    except ImportError:
        return

    SyncT, AsyncT = _build_transport_classes()

    # httpx.Client / AsyncClient look up `HTTPTransport` /
    # `AsyncHTTPTransport` by *name* in their own module namespace
    # (they import them at the top of `_client.py`). Patching only
    # the package attribute wouldn't affect that lookup, so we
    # rebind the names in `_client` directly. Then also publish on
    # the package so user code that does
    # `httpx.Client(transport=httpx.HTTPTransport())` gets ours.
    httpx_client.HTTPTransport = SyncT
    httpx_client.AsyncHTTPTransport = AsyncT
    httpx.HTTPTransport = SyncT
    httpx.AsyncHTTPTransport = AsyncT
    _patched = True


# -- Auto-detect: meta_path watcher ------------------------------------


class _HttpxPostLoadFinder(importlib.abc.MetaPathFinder):
    def find_spec(self, fullname, path, target=None):
        if fullname != "httpx":
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
    """Wraps the real loader; runs inject_into_httpx() after
    exec_module so the patch lands before any client / transport
    has been constructed."""

    def __init__(self, real):
        self._real = real

    def create_module(self, spec):
        if hasattr(self._real, "create_module"):
            return self._real.create_module(spec)
        return None

    def exec_module(self, module):
        self._real.exec_module(module)
        try:
            inject_into_httpx()
        except Exception:
            # If patching fails we leave the un-hooked httpx; the
            # user will see whatever httpx's own error path produces
            # (likely a wasi:sockets-derived failure, since the
            # socket guard re-raises as OSError).
            pass

    def __getattr__(self, name):
        return getattr(self._real, name)


def install_watcher() -> None:
    if "httpx" in sys.modules:
        inject_into_httpx()
        return
    if not any(isinstance(f, _HttpxPostLoadFinder) for f in sys.meta_path):
        sys.meta_path.insert(0, _HttpxPostLoadFinder())


install_watcher()
