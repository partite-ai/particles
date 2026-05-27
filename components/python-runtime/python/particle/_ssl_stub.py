"""
Stub for the `ssl` / `_ssl` stdlib modules — installed before any user
import so libraries that `import ssl` at module load (httplib2,
requests' optional certificate machinery, ...) don't trip on the
missing `_ssl` C extension.

Rationale: the wasi:p2 CPython build doesn't link OpenSSL — TLS
happens host-side at the wasi:http boundary, so guest code never
sees a TLS state machine. We could rebuild CPython with `_ssl`
enabled against a wasi-built OpenSSL, but every TLS API call would
then fail at the syscall layer anyway because the guest can't open
raw sockets. Stubbing is the simpler honest answer.

The stub is intentionally lazy: each attribute access returns a
sentinel that's truthy (so `getattr(ssl, "X", None) or ...` patterns
keep working), but calling the sentinel raises a clear error. Real
HTTP traffic is routed through `particle.http.fetch` long before any
of these would actually be invoked.
"""

import sys
import types

__all__ = ["install_ssl_stub"]

_installed = False


class _StubAttr:
    """A callable / attribute-accessible sentinel.

    Truthy (so `getattr(ssl, "X", None) or fallback()` keeps the
    first-hit; httplib2 does this for PROTOCOL_TLS_CLIENT). Calling
    it raises — no library that calls into the stub will work, but
    those code paths only run when we DIDN'T patch the high-level
    HTTP entrypoint, which is the actual bug to surface.
    """

    __slots__ = ("_name",)

    def __init__(self, name: str):
        self._name = name

    def __repr__(self):
        return f"<ssl-stub {self._name!r}>"

    def __bool__(self):
        return True

    def __call__(self, *args, **kwargs):
        raise RuntimeError(
            f"{self._name} is a particle stub — TLS lives host-side in "
            "wasi:http; guest-side ssl is not available."
        )

    def __getattr__(self, name):
        return _StubAttr(f"{self._name}.{name}")


class _StubModule(types.ModuleType):
    """ModuleType with a __getattr__ that hands out _StubAttrs on
    demand. Real ModuleType is needed (not a bare object) so things
    like `from ssl import SSLError` and `isinstance(m, ModuleType)`
    keep working."""

    def __getattr__(self, name):
        # Dunder lookups (importlib's machinery) should fail loudly
        # rather than return a stub — otherwise importlib gets a
        # truthy result for things like __path__ and tries to walk it.
        if name.startswith("__") and name.endswith("__"):
            raise AttributeError(name)
        return _StubAttr(f"{self.__name__}.{name}")


def install_ssl_stub() -> None:
    """Pre-populate sys.modules so `import ssl` / `import _ssl`
    short-circuits to our stubs. Replaces any existing entry — the
    stdlib `ssl.py` may have been pre-loaded by asyncio's import
    chain before we got here, in which case the half-initialized
    module needs to be evicted for our stub to take effect.
    Idempotent."""
    global _installed
    if _installed:
        return
    for name in ("ssl", "_ssl"):
        sys.modules[name] = _StubModule(name)
    _installed = True


install_ssl_stub()
