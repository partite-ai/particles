"""
socket.socket → OSError stub.

The runtime's wasi world doesn't import wasi:sockets, but
componentize-py's frozen libpython still exposes a `socket.socket`
class whose constructor calls into the (un-wired) wasi binding.
Under wazero that produces a wasm trap — uncatchable from Python,
because the trap bypasses the interpreter's exception handler.

That's a problem for libraries that probe for sockets at import
time inside a try/except (urllib3's `_has_ipv6` at
util/connection.py is the canonical case): the probe traps, the
import dies, and the user sees a `wasi: not implemented` trace
instead of a clean ImportError or a working library.

We sidestep the trap by replacing `socket.socket` with a class
whose `__init__` raises `OSError` immediately. urllib3's IPv6
probe catches that, sets `HAS_IPV6 = False`, and the import
finishes. Anything else that actually wanted a socket fails in the
same recognizable, catchable way as it would on a sandboxed host
that just doesn't have networking — better than a wasm trap.

Loaded by particle/__init__.py before any user code runs, so the
patch is in place by the time the user's bundle executes (or
imports a library that does its probing at module scope).
"""

import socket as _socket_mod

__all__ = []


_OriginalSocket = _socket_mod.socket


class _BlockedSocket:
    """Drop-in for socket.socket whose constructor raises OSError.

    Defined as a plain class (not a subclass of the original) so the
    constructor can refuse arguments without invoking
    `_socket.socket.__init__`, which is what trips the wasi trap."""

    # Mirror the public attributes that libraries occasionally probe
    # at *class* level (vs. instance level) before constructing —
    # they're idempotent integer constants and accessing them is
    # safe.
    __slots__ = ()

    def __init__(self, *_args, **_kwargs):
        raise OSError(
            "socket.socket(...) is not available in particles — "
            "particles don't wire wasi:sockets. Use particle.http "
            "(or urllib / urllib3 / requests, which the runtime "
            "transparently routes through wasi:http)."
        )


# Preserve the original on the module so libraries that genuinely
# need to peek at the C type can still see it. We replace the
# public name only — the trap path is the constructor.
_socket_mod._particle_original_socket = _OriginalSocket
_socket_mod.socket = _BlockedSocket
