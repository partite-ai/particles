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
    and attribute access both return more stubs — libraries probe ssl
    at import time in shapes we can't predict (urllib3 calls
    `ssl.OPENSSL_VERSION.startswith("OpenSSL ")` for feature
    detection, requests does similar dance with SSLContext). Forwarding
    everything as a stub lets module init complete; our HTTP shims
    intercept the high-level request entrypoints before any actual
    ssl operation is reached, so the never-real return values don't
    matter at runtime.
    """

    __slots__ = ("_name",)

    def __init__(self, name: str):
        self._name = name

    def __repr__(self):
        return f"<ssl-stub {self._name!r}>"

    def __bool__(self):
        return True

    def __call__(self, *args, **kwargs):
        return _StubAttr(f"{self._name}(...)")

    def __getattr__(self, name):
        return _StubAttr(f"{self._name}.{name}")

    # Comparisons: urllib3 / requests do feature detection via
    # `ssl.OPENSSL_VERSION_INFO < (1, 1, 1)` and similar. The
    # consistent answer is "no, this stub isn't ordered relative to
    # any real value" — return False everywhere so callers skip the
    # legacy / defensive branches and take the modern path, which is
    # the one we end up intercepting anyway.
    def __lt__(self, other): return False
    def __le__(self, other): return False
    def __gt__(self, other): return False
    def __ge__(self, other): return False
    def __eq__(self, other): return isinstance(other, _StubAttr) and self._name == other._name
    def __ne__(self, other): return not self.__eq__(other)
    def __hash__(self): return hash(self._name)

    # Iteration & indexing: occasionally a library does
    # `for x in ssl.SOMETHING:` or `ssl.SOMETHING[0]`. Yield nothing /
    # return another stub respectively, so the surrounding control
    # flow stays sane at import time.
    def __iter__(self): return iter(())
    def __getitem__(self, _key): return _StubAttr(f"{self._name}[...]")
    def __len__(self): return 0


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
