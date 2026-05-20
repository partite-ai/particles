"""Credentials access (mirrors @partite-ai/particle-credentials).

The substitution-based credential types (basic, oauth2, apikey) never
hand their actual value to Python — `get_placeholder(name)` returns
an opaque placeholder string + an apply-spec describing where the
host expects to substitute it. User code typically goes through
`particle.http.fetcher(name)` instead of calling `get_placeholder`
directly; the fetcher applies the placeholder to the right location
and the host substitutes when the request leaves wasm.

`get_raw(name)` returns the actual value — only valid for type-`raw`
credentials, where the manifest explicitly opted into raw access.
"""

from dataclasses import dataclass
from wit_world.imports import credentials as _credentials
from wit_world.imports.credentials import ApplyKind as _ApplyKind

__all__ = [
    "ApplyKind",
    "ApplySpec",
    "PlaceholderInfo",
    "get_placeholder",
    "get_raw",
    "get_configured_method",
]


# Re-export the enum under the Python convention.
ApplyKind = _ApplyKind


@dataclass
class ApplySpec:
    """Tells the host where a placeholder should appear in an outbound
    HTTP request. Matches `particle:host/credentials.apply-spec`."""
    kind: ApplyKind
    name: str | None = None
    scheme: str | None = None


@dataclass
class PlaceholderInfo:
    placeholder: str
    apply: ApplySpec


def get_placeholder(name: str) -> PlaceholderInfo:
    """Issue an opaque placeholder for credential `name`.

    The host expects the placeholder to appear at the location
    described by `apply`; placing it anywhere else means the request
    transmits the literal placeholder (and the server rejects it) —
    a security-by-design choice, see design doc §7.
    """
    info = _credentials.get_placeholder(name)
    return PlaceholderInfo(
        placeholder=info.placeholder,
        apply=ApplySpec(
            kind=info.apply.kind,
            name=info.apply.name,
            scheme=info.apply.scheme,
        ),
    )


def get_raw(name: str) -> str:
    """Return the actual value of a `type: "raw"` credential.

    Only valid for credentials the manifest declared as type `raw`.
    Other types raise a host-side type-mismatch error.
    """
    return _credentials.get_raw(name)


def get_configured_method(name: str) -> str | None:
    """Return the method name (e.g. "pat", "oauth") the user picked
    at setup for `name`, or None if no method is configured.
    """
    return _credentials.get_configured_method(name)
