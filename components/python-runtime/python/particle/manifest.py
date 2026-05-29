"""
`particle.manifest` — the user-facing types a Python particle author
uses to declare their particle at module scope.

Companion to the WIT contract in `wit/runtime.wit`: every class here
corresponds to a record / variant case in `particle:runtime/manifest`,
but with Pythonic naming (snake_case, dataclasses, short type names)
and a flattened shape (the WIT `capability-set` wrapper is gone —
`http` sits at the top level alongside `credentials` and `tools`).

Typical use::

    from particle.manifest import (
        Particle, Tool, Http,
        Credential, OAuth2, ApiKey, ApiKeyLocation,
    )

    particle = Particle(
        name="my-tools",
        description="...",
        version="0.1.0",
        http=Http(allowed_hosts=["api.example.com"]),
        credentials={
            "example": Credential(
                hosts=["api.example.com"],
                required=True,
                methods={"key": ApiKey(...)},
            ),
        },
        tools={
            "echo": Tool(
                description="echo the input",
                input_schema={"type": "object", ...},
                handler=lambda args: {"result": args["input"]},
            ),
        },
    )

The bootstrap reads the module-level `particle = Particle(...)`
assignment and translates it into the WIT records the host sees
through `get-manifest`.
"""

from dataclasses import dataclass, field
from typing import Any, Callable, Literal, Optional, Union


# ---- HTTP capability --------------------------------------------------------

@dataclass
class Http:
    """Outbound HTTP allow-list. Hosts not listed here are denied by
    the runtime's HTTP policy at request time. An empty list means
    no outbound HTTP is allowed at all."""
    allowed_hosts: list[str] = field(default_factory=list)


# ---- Filesystem capability --------------------------------------------------

@dataclass
class Mount:
    """One declared host-directory mount. The user maps `name` to a
    real directory at install / run time; the handler reads files at
    `path` inside the sandbox. `access` is "readonly" or "readwrite"."""
    description: str
    path: str
    access: Literal["readonly", "readwrite"] = "readonly"
    required: bool = False


@dataclass
class TempMount:
    """One scratch mount the host provisions fresh each run and clears
    on exit. Always read-write. `max_size` caps total file bytes
    ("10000", "10KB", "1MB", ...)."""
    description: str
    path: str
    max_size: str = ""


@dataclass
class Filesystem:
    """Filesystem mounts, flattened out of the WIT `capability-set`
    wrapper like `http`. `mounts` and `temp` are keyed by mount name."""
    mounts: dict[str, Mount] = field(default_factory=dict)
    temp: dict[str, TempMount] = field(default_factory=dict)


# ---- Credential methods -----------------------------------------------------
#
# Each class is one variant of the WIT `credential-method` union. The
# user picks which method(s) they want to support per credential by
# putting instances into `Credential.methods`. The host-side importer
# uses the per-method `description` (when set) to render setup prompts.

@dataclass
class Basic:
    """HTTP Basic auth — username + password."""
    description: str = ""


@dataclass
class Raw:
    """An opaque secret stored by the host. The particle reads it
    through `particle.credentials.get_raw(name)` and does whatever
    it needs with the bytes — there's no automatic substitution into
    outbound requests."""
    description: str = ""


OAuth2Flow = Literal[
    "authorization-code",
    "authorization-code-pkce",
    "device-code",
]


@dataclass
class OAuth2:
    """OAuth 2.0. Pin the flow(s) and endpoints the importer should
    drive. `scopes` is the access-token request scope list;
    `device_auth_url` is required only when `device-code` is in
    `flows`."""
    flows: list[OAuth2Flow]
    authorization_url: str
    token_url: str
    description: str = ""
    scopes: list[str] = field(default_factory=list)
    device_auth_url: str = ""


ApiKeyLocationKind = Literal["header", "auth-scheme", "query-param"]


@dataclass
class ApiKeyLocation:
    """Where the API key gets substituted into an outbound request.

    - `header` + `name`: arbitrary header (e.g. `X-API-Key`).
    - `auth-scheme` + `scheme`: `Authorization: <scheme> <key>`
      (e.g. `Bearer`).
    - `query-param` + `name`: appended as `?<name>=<key>`.
    """
    kind: ApiKeyLocationKind
    name: Optional[str] = None
    scheme: Optional[str] = None


@dataclass
class ApiKey:
    """API key. When `location` is omitted, the importer prompts the
    user to pick the location at setup time."""
    location: Optional[ApiKeyLocation] = None
    description: str = ""


SigningAlgorithm = Literal["hmac-sha256", "hmac-sha512"]


@dataclass
class SigningKey:
    """HMAC signing key, used through `particle.signing.sign()`. The
    key bytes live in the host keychain; the algorithm pins which
    HMAC variant the host uses to compute the signature."""
    algorithm: SigningAlgorithm
    description: str = ""


# The discriminated union over methods. Used as the value type in
# `Credential.methods` — the dict's key is the method's *name*, the
# value is one of these instances.
CredentialMethod = Union[Basic, OAuth2, ApiKey, SigningKey, Raw]


# ---- Credential entry -------------------------------------------------------

@dataclass
class Credential:
    """One named credential the importer asks the user to configure.

    `hosts` binds the credential to a subset of the particle's HTTP
    allow-list — substitution into outbound requests only fires for
    URLs whose host matches. Empty list = no HTTP binding (signing-
    key / raw credentials consumed entirely through the
    `particle.*` API).

    `required` makes the importer refuse to register the particle
    without a configured method.

    `methods` is a name → method-instance map. The user picks one
    method at setup; the names are arbitrary labels (e.g. `"oauth"`,
    `"pat"`).
    """
    methods: dict[str, CredentialMethod]
    hosts: list[str] = field(default_factory=list)
    required: bool = False


# ---- Tool entry -------------------------------------------------------------

@dataclass
class Tool:
    """One tool the particle exposes. `input_schema` is a JSON Schema
    (Draft 2020-12, object-rooted) the host validates against before
    invoking `handler`. The handler receives the parsed arguments
    dict and returns a JSON-serializable value (or raises to surface
    a `handler-error` to the caller)."""
    description: str
    input_schema: dict[str, Any]
    handler: Callable[..., Any]


# ---- Top-level particle -----------------------------------------------------

@dataclass
class Particle:
    """The complete particle definition. A Particlefile.py assigns one
    instance of this to a module-level `particle` and the runtime
    introspects it through `get-manifest`.

    `http` is the (currently sole) capability declaration — flattened
    out of the WIT `capability-set` wrapper for ergonomics. Future
    capability categories (sockets, env, ...) join as sibling
    optional fields.

    `ping`, when set, is the optional liveness handler returning a
    `{"status": "ok"|"degraded"|"unhealthy", "message"?: str,
    "details"?: any}` dict.
    """
    name: str
    description: str
    version: str
    http: Optional[Http] = None
    filesystem: Optional[Filesystem] = None
    credentials: dict[str, Credential] = field(default_factory=dict)
    tools: dict[str, Tool] = field(default_factory=dict)
    ping: Optional[Callable[[], Any]] = None
