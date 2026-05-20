"""Per-particle key/value store.

Wraps `wit_world.imports.kv`. Names follow the WIT (`get`, `set`,
`delete`, `list`) — `list` shadows the builtin in user code, so users
should `from particle import kv` and call `kv.list(...)` rather than
`from particle.kv import list`.
"""

from wit_world.imports import kv as _kv

__all__ = ["get", "set", "delete", "list"]


def get(key: str) -> str | None:
    """Return the stored value for `key`, or None if absent."""
    return _kv.get(key)


def set(key: str, value: str) -> None:
    """Store `value` under `key`. Overwrites."""
    _kv.set(key, value)


def delete(key: str) -> None:
    """Remove `key` if present. No-op if absent."""
    _kv.delete(key)


# Scoping `list` under the `kv` module (i.e. `kv.list(...)`) keeps it
# from shadowing the builtin at user-call sites — recommend authors
# call as `kv.list(...)`, not `from particle.kv import list`.
def list(prefix: str):
    """Return every key with the given prefix."""
    return _kv.list(prefix)
