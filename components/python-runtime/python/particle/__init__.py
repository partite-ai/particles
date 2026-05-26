"""
`particle` — the user-facing helper package for the Python runtime.

The `_runtime_host` C extension module (built with PyO3 inside
components/python-runtime/src/host_module.rs) is registered with
CPython's inittab BEFORE Py_Initialize runs, so every submodule
here can `import _runtime_host` at module scope without ordering
gymnastics.

Submodule load order matters and is intentional:

  1. `_socket_guard` — replaces `socket.socket` with an OSError-
     raising stub. Must run BEFORE anything else that might touch
     `socket` (urllib's IPv6 probe, requests' connection-pool
     setup). The runtime doesn't wire wasi:sockets, so without this
     guard a probe would wasm-trap rather than catch.
  2. `manifest` — pure dataclasses (Particle, Tool, Http, ...), no
     host-binding imports.
  3. `http`, `credentials`, `kv`, `oauth`, `signing` — thin Python
     surfaces wrapping `_runtime_host` calls.
  4. `_urllib_compat`, `_urllib3_compat`, `_httpx_compat` — the
     auto-hooks that route urllib / urllib3 / requests / httpx
     through `particle.http`. Imported eagerly so the patches /
     meta_path watchers are in place before any user-bundle line
     runs.
"""

from . import _socket_guard  # noqa: F401  — must run first

from . import manifest    # noqa: F401

from . import http        # noqa: F401
from . import credentials # noqa: F401
from . import kv          # noqa: F401
from . import oauth       # noqa: F401
from . import signing     # noqa: F401

from . import _urllib_compat   # noqa: F401  — patches urllib.request.urlopen
from . import _urllib3_compat  # noqa: F401  — meta_path watcher for urllib3
from . import _httpx_compat    # noqa: F401  — meta_path watcher for httpx
