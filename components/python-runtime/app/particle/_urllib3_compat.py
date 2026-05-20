"""
urllib3 auto-hook — routes urllib3-based clients (urllib3 directly,
`requests`, etc.) through `particle.http.fetch` instead of raw sockets.

We don't bundle urllib3. The strategy mirrors urllib3's own
contrib/emscripten module
(https://github.com/urllib3/urllib3/blob/main/src/urllib3/contrib/emscripten/__init__.py):
replace urllib3's `HTTPConnection` / `HTTPSConnection` and the
matching `ConnectionCls` attributes on the pool classes with our own
duck-typed classes that submit each request via wasi:http.

Auto-detect via `sys.meta_path`: a watcher sits in the import chain
and, the first time anything imports `urllib3`, wraps the loader so
`inject_into_urllib3()` runs immediately after urllib3 finishes
exec_module — before any pool / connection has been constructed. A
particle that never imports urllib3 pays nothing.

The patch is best-effort. If urllib3's internals shift in a future
release we don't lock against, the worst case is the patch silently
no-ops (its exception handler swallows the failure) and the user
gets the same socket-create error they'd see without the hook.
"""

import importlib.abc
import sys

__all__ = ["inject_into_urllib3", "install_watcher"]

_patched = False


def _build_connection_classes():
    """Construct the connection + response classes lazily, after
    urllib3 has finished loading. Putting this inside a function
    means we never import urllib3 ourselves — the imports only run
    if our watcher has fired."""
    import urllib3
    import urllib3.connection
    from urllib3._collections import HTTPHeaderDict
    from urllib3.exceptions import HTTPError
    from urllib3.response import BaseHTTPResponse

    from particle import http as particle_http

    class ParticleHTTPResponse(BaseHTTPResponse):
        """urllib3 BaseHTTPResponse wrapping a `particle.http.Response`.

        urllib3 / requests treat the body as a stream backed by a
        socket; here it's pre-buffered (particle.http.fetch reads
        the whole body before returning). We expose just the surface
        callers actually exercise — `.read()`, `.stream()`, `.data`,
        headers — and stub the streaming-specific properties to plain
        "done" values."""

        def __init__(self, *, body, headers, status, connection, request_url):
            hdr = HTTPHeaderDict()
            for k, v in headers:
                if isinstance(v, bytes):
                    v = v.decode("latin-1")
                hdr.add(k, v)
            super().__init__(
                headers=hdr,
                status=status,
                version=11,
                version_string="HTTP/1.1",
                reason=None,
                decode_content=True,
                request_url=request_url,
                retries=None,
            )
            self._body_bytes = body
            self._initial_body = body  # backing for the `data` property
            self._consumed = False
            self._connection = connection
            # urllib3 uses these to decide whether to keep reading;
            # since our body is fully buffered, the remaining count
            # is exact and certain.
            self.length_remaining = len(body)
            self.length_is_certain = True

        @property
        def data(self):
            # urllib3 callers (and requests, via r.content) expect a
            # buffered body that survives multiple reads. We always
            # have the full body in memory, so return it directly.
            return self._initial_body

        # -- read / stream --
        def read(self, amt=None, decode_content=None, cache_content=False):
            if amt is None or amt < 0:
                data = self._body_bytes
                self._body_bytes = b""
                self._consumed = True
                self.length_remaining = 0
                return data
            data = self._body_bytes[:amt]
            self._body_bytes = self._body_bytes[amt:]
            self.length_remaining = len(self._body_bytes)
            if not self._body_bytes:
                self._consumed = True
            return data

        def stream(self, amt=2**16, decode_content=None):
            while True:
                chunk = self.read(amt, decode_content=decode_content)
                if not chunk:
                    return
                yield chunk

        def read_chunked(self, amt=None, decode_content=None):
            chunk = self.read(amt, decode_content=decode_content)
            if chunk:
                yield chunk

        # -- lifecycle --
        def close(self):
            self._body_bytes = b""
            self._consumed = True

        def release_conn(self):
            pass

        # -- properties urllib3 / requests probe --
        @property
        def connection(self):
            return self._connection

        @property
        def closed(self):
            return self._consumed

        def isclosed(self):
            return self._consumed

        def fileno(self):
            raise OSError("particle response has no fileno (not socket-backed)")

        def flush(self):
            pass

        def readable(self):
            return True

        def tell(self):
            return 0

        def supports_chunked_reads(self):
            return False

    class ParticleHTTPConnection:
        """Duck-typed replacement for urllib3.connection.HTTPConnection.

        Doesn't hold a socket; each `request()` call fans out to
        particle.http.fetch, which submits the request through
        wasi:http. urllib3 then calls `getresponse()` to retrieve
        what we buffered."""

        default_port = 80
        is_verified = False
        proxy_is_verified = None

        # urllib3 inspects this to decide retry behavior on some
        # error paths. We aren't socket-backed so chunked reads
        # aren't a thing — the buffered body is always whole.
        response_class = ParticleHTTPResponse

        def __init__(
            self,
            host,
            port=None,
            *,
            timeout=None,
            source_address=None,
            blocksize=8192,
            socket_options=None,
            proxy=None,
            proxy_config=None,
            **_kwargs,
        ):
            self.host = host
            self.port = port if port is not None else self.default_port
            self.scheme = "http"
            self.timeout = timeout
            self.blocksize = blocksize
            self.source_address = None
            self.socket_options = None
            self.proxy = None
            self.proxy_config = None
            self._closed = True
            self._buffered = None
            self._buffered_url = None

        # -- the urllib3 connection contract (no-ops where we can) --
        def set_tunnel(self, host, port=None, headers=None, scheme="http"):
            pass

        def connect(self):
            self._closed = False

        def close(self):
            self._closed = True
            self._buffered = None

        @property
        def is_closed(self):
            return self._closed

        @property
        def is_connected(self):
            return True

        @property
        def has_connected_to_proxy(self):
            return False

        def putrequest(self, *args, **kwargs):
            # Old-style http.client API some code still uses. We
            # don't model the staged request/headers/body construction;
            # callers that need it should use .request().
            raise NotImplementedError(
                "ParticleHTTPConnection only supports the high-level request() API"
            )

        # -- the one method that actually does work --
        def request(
            self,
            method,
            url,
            body=None,
            headers=None,
            *,
            chunked=False,
            preload_content=True,
            decode_content=True,
            enforce_content_length=True,
        ):
            self._closed = False
            full_url = url
            if full_url.startswith("/"):
                # urllib3 calls request with a path-only URL on a
                # connection that knows its host. Reconstitute the
                # absolute URL for wasi:http.
                if self.port in (80, 443):
                    port_str = ""
                else:
                    port_str = f":{self.port}"
                full_url = f"{self.scheme}://{self.host}{port_str}{url}"
            # Strip Accept-Encoding: we don't decompress at this layer,
            # so let the server send identity-encoded bytes that flow
            # straight to BaseHTTPResponse.data / .text without urllib3
            # / requests trying to decode something we passed through
            # opaquely.
            out_headers = {}
            for k, v in (headers or {}).items():
                if k.lower() != "accept-encoding":
                    out_headers[k] = v
            try:
                resp = particle_http.fetch(
                    full_url,
                    method=method,
                    headers=out_headers or None,
                    body=body,
                )
            except Exception as e:
                # Surface as HTTPException — urllib3 wraps these into
                # its retry / pool-error machinery cleanly.
                raise HTTPError(str(e)) from e
            self._buffered = resp
            self._buffered_url = full_url

        def getresponse(self):
            if self._buffered is None:
                raise HTTPError("getresponse() called before request()")
            return ParticleHTTPResponse(
                body=self._buffered.body,
                headers=self._buffered.headers,
                status=self._buffered.status_code,
                connection=self,
                request_url=self._buffered_url,
            )

    class ParticleHTTPSConnection(ParticleHTTPConnection):
        """Same connection — wasi:http handles TLS host-side, the
        guest never sees a TLS state machine. urllib3 hands us a pile
        of SSL/cert kwargs at construction; we swallow them."""

        default_port = 443
        is_verified = True
        _IGNORED_KWARGS = frozenset({
            "cert_reqs", "ca_certs", "ca_cert_dir", "ca_cert_data",
            "ssl_version", "ssl_minimum_version", "ssl_maximum_version",
            "assert_hostname", "assert_fingerprint", "server_hostname",
            "ssl_context", "cert_file", "key_file", "key_password",
        })

        def __init__(self, host, port=None, **kwargs):
            for k in self._IGNORED_KWARGS:
                kwargs.pop(k, None)
            super().__init__(host, port, **kwargs)
            self.scheme = "https"

        def set_cert(self, **_kwargs):
            pass

    return ParticleHTTPConnection, ParticleHTTPSConnection


def inject_into_urllib3() -> None:
    """Replace urllib3's connection classes with ours. Mirrors
    `urllib3.contrib.emscripten.inject_into_urllib3`. Idempotent."""
    global _patched
    if _patched:
        return
    try:
        import urllib3
        import urllib3.connection
        from urllib3.connectionpool import HTTPConnectionPool, HTTPSConnectionPool
    except ImportError:
        return

    HTTPConn, HTTPSConn = _build_connection_classes()

    HTTPConnectionPool.ConnectionCls = HTTPConn
    HTTPSConnectionPool.ConnectionCls = HTTPSConn
    urllib3.connection.HTTPConnection = HTTPConn
    urllib3.connection.HTTPSConnection = HTTPSConn
    if hasattr(urllib3.connection, "VerifiedHTTPSConnection"):
        urllib3.connection.VerifiedHTTPSConnection = HTTPSConn
    _patched = True


# -- Auto-detect: meta_path watcher ------------------------------------
#
# We never import urllib3 ourselves. Instead, we sit in sys.meta_path:
# the first time anything else triggers `import urllib3`, our finder
# delegates to the next finder (the real one), then wraps the returned
# loader so `inject_into_urllib3()` runs right after urllib3's
# module-level code finishes.


class _Urllib3PostLoadFinder(importlib.abc.MetaPathFinder):
    def find_spec(self, fullname, path, target=None):
        if fullname != "urllib3":
            return None
        try:
            idx = sys.meta_path.index(self)
        except ValueError:
            return None
        # Delegate to every finder *after* ourselves so the real
        # loader for urllib3 still produces the spec.
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
    """Wraps the real loader; runs inject_into_urllib3() after
    exec_module so the patch lands before any urllib3 user constructs
    a connection or pool."""

    def __init__(self, real):
        self._real = real

    def create_module(self, spec):
        if hasattr(self._real, "create_module"):
            return self._real.create_module(spec)
        return None

    def exec_module(self, module):
        self._real.exec_module(module)
        try:
            inject_into_urllib3()
        except Exception:
            # Don't break urllib3 itself if our patch falls over —
            # the user gets the un-patched library, which fails on
            # socket-create with a clear wasi error.
            pass

    def __getattr__(self, name):
        return getattr(self._real, name)


def install_watcher() -> None:
    """Register the meta_path finder. If urllib3 is already in
    sys.modules (shouldn't happen at our import order), patch
    immediately. Idempotent."""
    if "urllib3" in sys.modules:
        inject_into_urllib3()
        return
    if not any(isinstance(f, _Urllib3PostLoadFinder) for f in sys.meta_path):
        sys.meta_path.insert(0, _Urllib3PostLoadFinder())


# Install on import. Cheap — just appends one finder to sys.meta_path.
install_watcher()
