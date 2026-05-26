"""Host-side signing for Python particles.

The signing key never enters Python memory — the host performs the
HMAC / HMAC-verify operation and returns only the result. Use for
HMAC webhook signing, AWS SigV4 string-to-sign, JWT signing, etc.
"""

import _runtime_host

__all__ = ["sign", "verify"]


def sign(name: str, data: bytes) -> bytes:
    """Sign `data` with the named signing-key credential. Returns
    the raw signature bytes; the particle formats them per its
    upstream protocol.
    """
    return _runtime_host._signing_sign(name, bytes(data))


def verify(name: str, data: bytes, signature: bytes) -> bool:
    """Constant-time verify `signature` against `data`. Returns True
    on match, False otherwise. Raises if `name` isn't a signing-key
    credential.
    """
    return _runtime_host._signing_verify(name, bytes(data), bytes(signature))
