"""OAuth refresh for Python particles.

User code rarely needs to call this directly — `particle.http.fetch`
with `credential_name=...` substitutes the current access token
transparently, and the host refreshes on its own when the cached
token is expired. Use `refresh` only when an upstream returns 401/403
on a token we *thought* was still valid (i.e., revocation or
unsynced expiry).
"""

import _runtime_host

__all__ = ["refresh"]


def refresh(name: str) -> None:
    """Force-refresh credential `name`'s OAuth token.

    Raises `_runtime_host.RuntimeHostError` with `.kind` in
    {"not-configured", "not-oauth", "refresh-failed"} on failure.
    """
    _runtime_host._oauth_refresh(name)
