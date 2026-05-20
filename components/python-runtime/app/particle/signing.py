"""Host-side signing for Python particles.

The signing key never enters Python memory — the host performs the
HMAC / HMAC-verify operation and returns only the result. Use for
HMAC webhook signing, AWS SigV4 string-to-sign, JWT signing, etc.
"""

from wit_world.imports import signing as _signing

__all__ = ["sign", "verify"]


def sign(name: str, data: bytes) -> bytes:
    """Sign `data` with the named signing-key credential. Returns
    the raw signature bytes; the particle formats them per its
    upstream protocol.
    """
    return _signing.sign(name, data)


def verify(name: str, data: bytes, signature: bytes) -> bool:
    """Constant-time verify `signature` against `data`. Returns True
    on match, False otherwise. Raises if `name` isn't a signing-key
    credential.
    """
    return _signing.verify(name, data, signature)
