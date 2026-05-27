"""
httplib2 auto-hook — routes httplib2-based clients (httplib2 directly,
`google-api-python-client` via its default transport, `oauth2client`'s
legacy stack, ...) through `particle.http.fetch` instead of raw
sockets.

We don't bundle httplib2. Strategy: a `sys.meta_path` watcher delegates
to the real loader and, the first time anything imports `httplib2`,
wraps `httplib2.Http.request` with a wasi:http-routed equivalent
before any caller can hold a reference. A particle that never imports
httplib2 pays nothing.

httplib2's `Http.request()` returns the pair `(Response, content)`
where `Response` is a dict-subclass with `.status`, `.reason`,
`.version`, and lowercased header keys (httplib2 historically
forced-lowercases header names). We reconstruct exactly that shape
from our `particle.http.Response` so callers that branch on
`response.status` or iterate `response.items()` keep working.

The patch is best-effort. If httplib2 ships a future release whose
`Http.request` signature shifts, the wrap silently no-ops and the
caller sees the unpatched library's socket-create OSError.
"""

import importlib.abc
import sys

__all__ = ["inject_into_httplib2", "install_watcher"]

_patched = False


def _build_response_class():
    """Construct the response subclass lazily — only after httplib2
    has finished loading so `httplib2.Response` is a known class."""
    import httplib2

    class _ParticleHttplib2Response(httplib2.Response):
        """Mirror httplib2.Response's contract: dict-subclass keyed
        by lowercased header name, with `.status`, `.reason`,
        `.version` attributes."""

        def __init__(self, particle_resp, *, final_uri):
            info = {}
            for k, v in particle_resp.headers:
                if isinstance(v, bytes):
                    v = v.decode("latin-1")
                # httplib2 lowercases header keys; later duplicates win
                # (matching email.message.Message.get behavior under
                # httplib2's compatibility layer).
                info[k.lower()] = v
            info["status"] = str(particle_resp.status_code)
            info["content-location"] = final_uri
            super().__init__(info)
            # httplib2 sets these from the raw HTTPResponse; we don't
            # have a wire-level handle, so report HTTP/1.1 and the
            # status integer directly.
            self.status = particle_resp.status_code
            self.reason = ""
            self.version = 11
            self.previous = None

    return _ParticleHttplib2Response


def inject_into_httplib2() -> None:
    """Replace `httplib2.Http.request` with a wasi:http-routed
    equivalent. Idempotent."""
    global _patched
    if _patched:
        return
    try:
        import httplib2
    except ImportError:
        return

    from particle import http as particle_http

    ResponseCls = _build_response_class()

    def _patched_request(
        self,
        uri,
        method="GET",
        body=None,
        headers=None,
        redirections=5,
        connection_type=None,
    ):
        # httplib2 callers pass headers as a plain dict (str → str).
        # Strip Accept-Encoding so identity-encoded bytes flow straight
        # back without httplib2 wanting to decompress; wasi:http handles
        # transfer encoding at the host edge.
        out_headers = {}
        for k, v in (headers or {}).items():
            if k.lower() == "accept-encoding":
                continue
            out_headers[k] = v

        # google-api-python-client and friends set their own auth via
        # the Credentials object; we don't attach a credential_name
        # here. Particle code that wants placeholder substitution can
        # use particle.http.fetch directly or register the credential
        # via a PlaceholderCredentials adapter that runs before we get
        # called.
        resp = particle_http.fetch(
            uri,
            method=method.upper() if isinstance(method, str) else method,
            headers=out_headers or None,
            body=body,
        )
        return ResponseCls(resp, final_uri=uri), resp.body

    httplib2.Http.request = _patched_request
    _patched = True


# -- Auto-detect: meta_path watcher ------------------------------------
#
# Same shape as `_urllib3_compat`: sit in sys.meta_path, delegate the
# real find_spec, then wrap the loader so `inject_into_httplib2()`
# fires the moment httplib2 finishes its module-level code.


class _Httplib2PostLoadFinder(importlib.abc.MetaPathFinder):
    def find_spec(self, fullname, path, target=None):
        if fullname != "httplib2":
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
            inject_into_httplib2()
        except Exception:
            # If the patch falls over, hand the user back the
            # unpatched library — its socket-create attempt will fail
            # with a clear wasi error.
            pass

    def __getattr__(self, name):
        return getattr(self._real, name)


def install_watcher() -> None:
    if "httplib2" in sys.modules:
        inject_into_httplib2()
        return
    if not any(isinstance(f, _Httplib2PostLoadFinder) for f in sys.meta_path):
        sys.meta_path.insert(0, _Httplib2PostLoadFinder())


install_watcher()
