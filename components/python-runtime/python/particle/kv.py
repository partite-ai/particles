"""Per-particle key/value store.

Wraps `particle:host/kv` through the PyO3-built `_runtime_host`
extension module. Names follow the WIT (`get`, `set`, `delete`,
`list`) — `list` shadows the builtin in user code, so authors should
`from particle import kv` and call `kv.list(...)` rather than
`from particle.kv import list`.
"""

from typing import List, Optional

import _runtime_host

__all__ = ["get", "set", "delete", "list"]


def get(key: str) -> Optional[str]:
    """Return the stored value for `key`, or None if absent."""
    return _runtime_host._kv_get(key)


def set(key: str, value: str) -> None:
    """Store `value` under `key`. Overwrites."""
    _runtime_host._kv_set(key, value)


def delete(key: str) -> None:
    """Remove `key` if present. No-op if absent."""
    _runtime_host._kv_delete(key)


def list(prefix: str) -> List[str]:
    """Return every key with the given prefix."""
    return _runtime_host._kv_list(prefix)
