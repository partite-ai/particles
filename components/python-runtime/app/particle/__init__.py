"""
`particle` — the user-facing helper package frozen into
particle-python-runtime.wasm. Thin, idiomatic wrappers around the
`wit_world.imports.*` bindings emitted by componentize-py, exposing
the host capabilities (HTTP, credentials, kv, oauth, signing) and
the manifest-declaration dataclasses.

User code in `Particlefile.py` imports straight from here:

    from particle import http, credentials, kv
    from particle.manifest import Particle, Tool, Http, ...

The wrappers exist so user code doesn't have to chase componentize-py
binding names that may change across versions, and so a Pythonic
API surface (snake_case, dataclasses, optional kwargs) sits in front
of the WIT-typed primitives the runtime actually calls.
"""

# Side-effect-only: replaces socket.socket with a class whose
# constructor raises OSError, so library code that probes for socket
# support at import time gets a catchable exception instead of a
# wasm trap. Must run before any submodule that might transitively
# touch socket — keep this at the top.
from . import _socket_guard    # noqa: F401
from . import http        # noqa: F401
from . import credentials # noqa: F401
from . import kv          # noqa: F401
from . import oauth       # noqa: F401
from . import signing     # noqa: F401
from . import manifest    # noqa: F401
# Side-effect-only: each `_*_compat` module installs a host hook so
# the matching client library Just Works from a particle. Loaded in
# order from closest-to-stdlib outward — urllib first (always
# present), then the conditional library hooks. urllib3 / httpx
# install sys.meta_path watchers that only fire if the user actually
# imports those libraries; this module costs nothing for particles
# that don't.
from . import _urllib_compat   # noqa: F401  (urllib.request.urlopen → wasi:http)
from . import _urllib3_compat  # noqa: F401  (urllib3 / requests → wasi:http)
from . import _httpx_compat    # noqa: F401  (httpx sync + async → wasi:http)
