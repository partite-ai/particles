"""
Particle Python runtime — bootstrap module.

The Rust shim in components/python-runtime/src/lib.rs owns the WIT
exports. After Py_Initialize it instantiates `Tools()`, `Health()`,
`Manifest()` and dispatches list_tools / call_tool / ping /
get_manifest through the CPython C API (PyObject_CallNoArgs, attribute
lookup), reading plain Python attributes off the returned objects to
fill in the WIT records.

That's a different shape from the componentize-py-based v1 (which had
`from wit_world.exports import Tools as ToolsProtocol` and let the
generated bindings drive the dispatch). For v2 we deliberately keep
this file free of any wit-generated imports — the Rust side does the
canon ABI marshalling, Python just returns Python objects.

The `particle/` sibling package is the user-facing helper module from
v1 (HTTP, credentials, kv, oauth, signing wrappers + the manifest
dataclasses). The non-manifest pieces (http/credentials/kv/oauth/
signing) reach into componentize-py-generated bindings that don't
exist in this build — they're commented out in particle/__init__.py
until they're ported to call the Rust shim's C API.
"""

import asyncio
import json
import os
import sys
import traceback


# -- exception classes the Rust shim dispatches on by class-name ---------
#
# Rust matches by `type(exc).__name__` (and a `kind` attribute as a
# fallback) so dropping new variants in here just means adding a class.
# The dataclass-like `detail` attribute, when set, carries the message
# + stack the Rust side lifts into the WIT `error-detail` record.

class NotFound(Exception):
    """Tool name was not registered in `particle.tools`."""
    kind = "not-found"


class InvalidArguments(Exception):
    kind = "invalid-arguments"

    def __init__(self, message: str, stack: str | None = None) -> None:
        super().__init__(message)
        self.message = message
        self.stack = stack


class HandlerError(Exception):
    kind = "handler-error"

    def __init__(self, message: str, stack: str | None = None) -> None:
        super().__init__(message)
        self.message = message
        self.stack = stack


class CapabilityDenied(Exception):
    kind = "capability-denied"

    def __init__(self, message: str, stack: str | None = None) -> None:
        super().__init__(message)
        self.message = message
        self.stack = stack


class NotImplementedHealth(Exception):
    """Mapped to health-error.not-implemented. Distinct from Python's
    NotImplementedError so we don't accidentally swallow user-raised
    NotImplementedError."""
    kind = "not-implemented"


# -- types the Rust side reads via getattr ---------------------------------

class ToolDef:
    """The Rust shim reads `.name`, `.description`, `.input_schema_json`
    off whatever objects list_tools returns. Using a plain class with
    three str attributes is enough — no dataclass, no typing import."""

    __slots__ = ("name", "description", "input_schema_json")

    def __init__(self, name: str, description: str, input_schema_json: str) -> None:
        self.name = name
        self.description = description
        self.input_schema_json = input_schema_json


class PingResult:
    __slots__ = ("status", "message", "details")

    def __init__(self, status: str, message: str | None, details: str | None) -> None:
        self.status = status      # "ok" / "degraded" / "unhealthy"
        self.message = message
        self.details = details


class HttpCapability:
    __slots__ = ("allowed_hosts",)

    def __init__(self, allowed_hosts: list[str]) -> None:
        self.allowed_hosts = allowed_hosts


class MountDecl:
    __slots__ = ("name", "description", "path", "access", "required")

    def __init__(self, name, description, path, access, required) -> None:
        self.name = name
        self.description = description
        self.path = path
        self.access = access
        self.required = required


class TempMountDecl:
    __slots__ = ("name", "description", "path", "max_size")

    def __init__(self, name, description, path, max_size) -> None:
        self.name = name
        self.description = description
        self.path = path
        self.max_size = max_size


class FilesystemCapability:
    __slots__ = ("mounts", "temp")

    def __init__(self, mounts, temp) -> None:
        self.mounts = mounts
        self.temp = temp


class CapabilitySet:
    __slots__ = ("http", "filesystem")

    def __init__(self, http: HttpCapability | None, filesystem: FilesystemCapability | None = None) -> None:
        self.http = http
        self.filesystem = filesystem


# Credential-method variants. Rust dispatches by `.kind` (a short
# string discriminator) since enum/variant detection from class type
# alone is fragile across reload boundaries.

class CMBasic:
    kind = "basic"


class CMRaw:
    kind = "raw"


class CMOAuth2:
    kind = "oauth2"

    def __init__(
        self,
        flows: list[str],
        scopes: list[str],
        authorization_url: str,
        token_url: str,
        device_auth_url: str,
    ) -> None:
        self.flows = flows
        self.scopes = scopes
        self.authorization_url = authorization_url
        self.token_url = token_url
        self.device_auth_url = device_auth_url


class CMApiKey:
    kind = "apikey"

    def __init__(self, location) -> None:
        # location: dict-like with .kind / .name / .scheme, or None
        self.location = location


class ApiKeyLocationRec:
    __slots__ = ("kind", "name", "scheme")

    def __init__(self, kind: str, name: str | None, scheme: str | None) -> None:
        self.kind = kind
        self.name = name
        self.scheme = scheme


class CMSigningKey:
    kind = "signing-key"

    def __init__(self, algorithm: str) -> None:
        self.algorithm = algorithm


class CredentialMethodEntry:
    __slots__ = ("name", "description", "method")

    def __init__(self, name: str, description: str, method) -> None:
        self.name = name
        self.description = description
        self.method = method


class CredentialEntry:
    __slots__ = ("name", "hosts", "required", "methods")

    def __init__(self, name: str, hosts: list[str], required: bool, methods) -> None:
        self.name = name
        self.hosts = hosts
        self.required = required
        self.methods = methods


class ToolEntry:
    __slots__ = ("name", "description", "input_schema_json")

    def __init__(self, name: str, description: str, input_schema_json: str) -> None:
        self.name = name
        self.description = description
        self.input_schema_json = input_schema_json


class ParticleManifest:
    __slots__ = ("name", "description", "version", "capabilities", "credentials", "tools")

    def __init__(
        self,
        name: str,
        description: str,
        version: str,
        capabilities: CapabilitySet,
        credentials: list[CredentialEntry],
        tools: list[ToolEntry],
    ) -> None:
        self.name = name
        self.description = description
        self.version = version
        self.capabilities = capabilities
        self.credentials = credentials
        self.tools = tools


# -- user-bundle loader ----------------------------------------------------

_USER_MODULE_PATH = "/particle/bundle.py"
_USER_MODULE_DIR = os.path.dirname(_USER_MODULE_PATH)
_DEPS_SITE_PACKAGES = "/particle/_deps/site-packages"

_user_module = None
_load_error: BaseException | None = None


def _load_user_module():
    global _user_module, _load_error
    if _user_module is not None:
        return _user_module
    if _load_error is not None:
        raise _load_error

    # Defer importlib.* until first use — the skeleton's Py_Initialize
    # path doesn't need them, and pulling them at module-import time
    # would bloat the startup trace.
    import importlib
    import importlib.machinery
    import importlib.util

    try:
        if _USER_MODULE_DIR not in sys.path:
            sys.path.insert(0, _USER_MODULE_DIR)
        if os.path.isdir(_DEPS_SITE_PACKAGES) and _DEPS_SITE_PACKAGES not in sys.path:
            sys.path.insert(0, _DEPS_SITE_PACKAGES)
        # Eagerly import `particle` so its socket / ssl / urllib /
        # urllib3 / httpx / httplib2 shims register before any user
        # bundle line runs — many libraries (httplib2, requests, ...)
        # do `import ssl` / `import socket` at module load, which
        # would fail before our `from particle import ...` had a
        # chance to take effect.
        import particle  # noqa: F401
        loader = importlib.machinery.SourceFileLoader("bundle", _USER_MODULE_PATH)
        spec = importlib.util.spec_from_loader("bundle", loader)
        if spec is None:
            raise RuntimeError(f"could not build module spec for {_USER_MODULE_PATH}")
        module = importlib.util.module_from_spec(spec)
        loader.exec_module(module)
    except BaseException as e:
        _load_error = e
        raise

    _user_module = module
    return module


def _get_particle():
    """Return the module-level `particle = Particle(...)` instance the
    user defined in bundle.py."""
    mod = _load_user_module()
    obj = getattr(mod, "particle", None)
    if obj is None:
        raise RuntimeError(
            "bundle.py must define a module-level `particle = ...` "
            "(import Particle from `particle.manifest`)"
        )
    return obj


def _log_traceback(e: BaseException) -> None:
    """Mirror the stack to wasi:cli/stderr so the host's stderr
    capture sees it even when the Rust shim doesn't successfully
    fetch the WIT-level error string."""
    traceback.print_exception(type(e), e, e.__traceback__, file=sys.stderr)


def _describe(e: BaseException) -> str:
    msg = str(e) or repr(e)
    return f"{type(e).__name__}: {msg}"


def _format_stack(e: BaseException) -> str:
    return "".join(traceback.format_exception(type(e), e, e.__traceback__))


# -- WIT-export dispatchers (Rust side calls these) ------------------------


class Tools:
    """Rust calls `bootstrap.Tools().list_tools()` and
    `bootstrap.Tools().call_tool(name, args_json)`. Both return plain
    Python types; the Rust side reads them via the object protocol."""

    def list_tools(self):
        try:
            p = _get_particle()
        except BaseException as e:
            _log_traceback(e)
            return [ToolDef(
                name="__particle_load_error__",
                description=_describe(e),
                input_schema_json="{}",
            )]

        out = []
        tools_map = getattr(p, "tools", {}) or {}
        for name, tool in tools_map.items():
            out.append(ToolDef(
                name=name,
                description=getattr(tool, "description", "") or "",
                input_schema_json=json.dumps(getattr(tool, "input_schema", {}) or {}),
            ))
        return out

    def call_tool(self, name: str, arguments_json: str) -> str:
        try:
            p = _get_particle()
        except BaseException as e:
            _log_traceback(e)
            raise HandlerError(_describe(e), _format_stack(e))

        tools_map = getattr(p, "tools", {}) or {}
        tool = tools_map.get(name)
        if tool is None:
            raise NotFound(name)

        handler = getattr(tool, "handler", None)
        if not callable(handler):
            raise HandlerError(
                f"tool {name!r} has no callable handler",
                None,
            )

        try:
            args = json.loads(arguments_json)
        except Exception as e:
            raise InvalidArguments(
                f"argument JSON parse: {_describe(e)}",
                _format_stack(e),
            )

        try:
            result = handler(args)
            if asyncio.iscoroutine(result):
                result = asyncio.run(result)
        except BaseException as e:
            _log_traceback(e)
            raise HandlerError(_describe(e), _format_stack(e))

        try:
            return json.dumps(result)
        except Exception as e:
            raise HandlerError(
                f"result is not JSON-serializable: {_describe(e)}",
                _format_stack(e),
            )


class Health:
    """Rust calls `bootstrap.Health().ping()` — same dispatch shape as Tools."""

    def ping(self) -> PingResult:
        try:
            p = _get_particle()
        except BaseException as e:
            _log_traceback(e)
            raise HandlerError(_describe(e), _format_stack(e))

        ping_fn = getattr(p, "ping", None)
        if not callable(ping_fn):
            raise NotImplementedHealth("particle does not declare a ping handler")

        try:
            result = ping_fn()
            if asyncio.iscoroutine(result):
                result = asyncio.run(result)
        except BaseException as e:
            _log_traceback(e)
            raise HandlerError(_describe(e), _format_stack(e))

        return _to_ping_result(result)


def _to_ping_result(payload: object) -> PingResult:
    if isinstance(payload, PingResult):
        return payload
    if not isinstance(payload, dict):
        raise HandlerError(
            f"ping must return a dict or PingResult, got {type(payload).__name__}",
            None,
        )
    status_str = payload.get("status", "ok")
    if status_str not in ("ok", "degraded", "unhealthy"):
        raise HandlerError(f"ping returned unknown status {status_str!r}", None)
    message = payload.get("message")
    details = payload.get("details")
    if details is not None and not isinstance(details, str):
        details = json.dumps(details)
    return PingResult(status=status_str, message=message, details=details)


class Manifest:
    """Rust calls `bootstrap.Manifest().get_manifest()` — same dispatch shape."""

    # Two-level error variant: bundle-load-error wraps a bundle-import
    # failure, invalid-manifest wraps a malformed Particle object. Rust
    # dispatches on the `kind` attribute when present and otherwise
    # treats the exception as bundle-load-error.
    def get_manifest(self) -> ParticleManifest:
        try:
            p = _get_particle()
        except BaseException as e:
            _log_traceback(e)
            err = HandlerError(_describe(e), _format_stack(e))
            err.kind = "bundle-load-error"
            raise err

        try:
            return _build_manifest_record(p)
        except BaseException as e:
            _log_traceback(e)
            err = HandlerError(_describe(e), _format_stack(e))
            err.kind = "invalid-manifest"
            raise err


def _build_manifest_record(p) -> ParticleManifest:
    """Lift the user-facing `Particle` dataclass into a Particle-
    Manifest the Rust side can recursively marshal."""
    http = getattr(p, "http", None)
    http_cap = None
    if http is not None:
        http_cap = HttpCapability(allowed_hosts=list(getattr(http, "allowed_hosts", []) or []))

    fs = getattr(p, "filesystem", None)
    fs_cap = None
    if fs is not None:
        mounts = [
            MountDecl(
                name=mname,
                description=getattr(m, "description", "") or "",
                path=getattr(m, "path", "") or "",
                access=getattr(m, "access", "readonly") or "readonly",
                required=bool(getattr(m, "required", False)),
            )
            for mname, m in (getattr(fs, "mounts", {}) or {}).items()
        ]
        temp = [
            TempMountDecl(
                name=tname,
                description=getattr(t, "description", "") or "",
                path=getattr(t, "path", "") or "",
                max_size=getattr(t, "max_size", "") or "",
            )
            for tname, t in (getattr(fs, "temp", {}) or {}).items()
        ]
        fs_cap = FilesystemCapability(mounts=mounts, temp=temp)

    capabilities = CapabilitySet(http=http_cap, filesystem=fs_cap)

    credentials = []
    creds_map = getattr(p, "credentials", {}) or {}
    for cname, cred in creds_map.items():
        credentials.append(_to_credential_entry(cname, cred))

    tools = []
    tools_map = getattr(p, "tools", {}) or {}
    for tname, tool in tools_map.items():
        tools.append(ToolEntry(
            name=tname,
            description=getattr(tool, "description", "") or "",
            input_schema_json=json.dumps(getattr(tool, "input_schema", {}) or {}),
        ))

    return ParticleManifest(
        name=getattr(p, "name", "") or "",
        description=getattr(p, "description", "") or "",
        version=getattr(p, "version", "") or "",
        capabilities=capabilities,
        credentials=credentials,
        tools=tools,
    )


def _to_credential_entry(cname: str, cred) -> CredentialEntry:
    methods = []
    methods_map = getattr(cred, "methods", {}) or {}
    for mname, method in methods_map.items():
        methods.append(CredentialMethodEntry(
            name=mname,
            description=getattr(method, "description", "") or "",
            method=_to_credential_method(cname, mname, method),
        ))
    return CredentialEntry(
        name=cname,
        hosts=list(getattr(cred, "hosts", []) or []),
        required=bool(getattr(cred, "required", False)),
        methods=methods,
    )


def _to_credential_method(cname: str, mname: str, method):
    """Dispatch on the user-facing class name (from particle.manifest).
    Returns one of the CM* variant classes the Rust side recognizes."""
    cls_name = type(method).__name__
    if cls_name == "Basic":
        return CMBasic()
    if cls_name == "Raw":
        return CMRaw()
    if cls_name == "OAuth2":
        return CMOAuth2(
            flows=list(getattr(method, "flows", []) or []),
            scopes=list(getattr(method, "scopes", []) or []),
            authorization_url=getattr(method, "authorization_url", "") or "",
            token_url=getattr(method, "token_url", "") or "",
            device_auth_url=getattr(method, "device_auth_url", "") or "",
        )
    if cls_name == "ApiKey":
        loc = getattr(method, "location", None)
        if loc is None:
            return CMApiKey(location=None)
        return CMApiKey(location=ApiKeyLocationRec(
            kind=getattr(loc, "kind", "") or "",
            name=getattr(loc, "name", None),
            scheme=getattr(loc, "scheme", None),
        ))
    if cls_name == "SigningKey":
        return CMSigningKey(algorithm=getattr(method, "algorithm", "") or "")
    raise ValueError(
        f"credentials.{cname}.methods.{mname}: unsupported method type "
        f"{cls_name} — use one of Basic, OAuth2, ApiKey, SigningKey, Raw from particle.manifest"
    )
