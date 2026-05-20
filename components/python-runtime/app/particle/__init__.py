"""
`particle` — the user-facing helper package frozen into
particle-python-runtime.wasm. Mirrors the JS-side
`@partite-ai/particle-credentials` / `-kv` / `-oauth` / `-signing` /
`-http` modules: thin Python wrappers around the
`wit_world.imports.*` bindings emitted by componentize-py.

User code in `Particlefile.py` imports straight from here:

    from particle import http, credentials, kv

The wrappers exist so user code doesn't have to chase componentize-py
binding names that may change across versions, and so the same idiomatic
Python API works regardless of underlying WIT layout.
"""

from . import http        # noqa: F401  (also installs the urllib monkey-patch)
from . import credentials # noqa: F401
from . import kv          # noqa: F401
from . import oauth       # noqa: F401
from . import signing     # noqa: F401
