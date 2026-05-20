"""OAuth refresh (mirrors @partite-ai/particle-oauth).

User code rarely needs to call this directly — `particle.http.fetcher`
substitutes the current access token transparently, and the host
refreshes on its own when the cached token is expired. Use `refresh`
only when an upstream returns 401/403 on a token we *thought* was
still valid (i.e., revocation or unsynced expiry).
"""

from wit_world.imports import oauth as _oauth

__all__ = ["refresh"]


def refresh(name: str) -> None:
    """Force-refresh credential `name`'s OAuth token.

    Raises an `Err` wrapping a `wit_world.imports.oauth.OAuthError`
    variant if `name` isn't configured, isn't an OAuth credential,
    or the refresh request itself fails.
    """
    _oauth.refresh(name)
