---
name: write-particle-python
description: Use when the user asks you to create, edit, or extend a Particle in Python. Triggers include "write a particle", "create a particle in Python", "Particlefile.py", "make a Python particle for the <X> API". For TypeScript/JavaScript particles, use `write-particle-typescript` instead.
---

# Write a Python particle

A particle is a single-file program that exposes one or more **tools**
(named operations with a JSON-Schema input) to a Particle runtime.
This skill tells you everything you need to produce one that builds
and runs.

## 1. File layout

One source file at the project root, named exactly `Particlefile.py`.
Nothing else is required. PyPI dependencies are declared in a PEP 723
inline metadata block at the top of the file; the build resolves them.
No `pyproject.toml`, no virtualenv to manage.

```python
# /// script
# requires-python = ">=3.12"
# dependencies = [
#   "idna>=3",       # optional — only if you use third-party packages
# ]
# ///
```

## 2. The module-level `particle` — the whole API

The file must assign a `Particle` instance to a module-level
`particle` variable. The runtime introspects this on build, and reads
the handlers off it on every tool call.

```python
from particle.manifest import (
    Particle, Tool, Http, Filesystem, Mount, TempMount,
    Credential, OAuth2, ApiKey, ApiKeyLocation, SigningKey, Basic, Raw,
)
from particle import http, credentials, kv, oauth, signing

def _do_thing(args):
    return {"result": f"hello {args['name']}"}

def _ping():
    return {"status": "ok", "message": "alive"}

particle = Particle(
    name="kebab-case-name",       # required, kebab-case
    description="...",             # required, one line
    version="0.1.0",               # required, semver

    # Outbound HTTP allow-list. Hosts not listed here are denied at
    # the wire boundary. Omit `http=` entirely if the particle makes
    # no outbound requests.
    http=Http(allowed_hosts=["api.example.com"]),

    # Host-directory access, off by default. See "Filesystem" below.
    # filesystem=Filesystem(mounts={...}, temp={...}),

    # Optional. Declared per name; user picks a method at setup.
    credentials={
        "example": Credential(
            hosts=["api.example.com"],
            required=True,
            methods={
                # see "Credential methods" below
            },
        ),
    },

    # Required. name → Tool(description, input_schema, handler).
    tools={
        "do_thing": Tool(
            description="What this tool does — one line, written for an LLM.",
            input_schema={
                "type": "object",
                "properties": {
                    "name": {"type": "string", "description": "..."},
                },
                "required": ["name"],
            },
            handler=_do_thing,
        ),
    },

    # Optional. Called by `particle ping`.
    ping=_ping,
)
```

### Tools

- `description`: written for an LLM caller — short, action-oriented.
- `input_schema`: JSON Schema Draft 2020-12, object-rooted. The host
  validates arguments against it *before* calling your handler, so
  you can trust the dict shape inside.
- `handler`: a plain callable taking one positional dict and returning
  any JSON-serializable value. May be `def` or `async def`; async
  handlers run on a one-shot poll loop. Raise to surface an error.

### Ping

Optional. Return a dict like:

```python
{"status": "ok" | "degraded" | "unhealthy",
 "message": "...",            # optional
 "details": ...}              # optional, JSON-serializable
```

## 3. Runtime APIs available to handlers

All host capabilities live under `from particle import ...`. These are
the only non-stdlib modules you need.

### Outbound HTTP

Four equivalent ways to make an HTTP call. Pick whichever matches
the idiom the rest of your code uses; they all route through the host
allow-list.

**`particle.http.fetch`** — the native API. Auto-applies a credential
when `credential_name=` is set.

```python
from particle import http

res = http.fetch(
    "https://api.example.com/things",
    method="POST",
    headers={"Content-Type": "application/json"},
    body=json.dumps({...}).encode(),
    credential_name="example",   # optional; host substitutes the secret
)
if not res.ok:
    raise RuntimeError(f"upstream {res.status_code}")
data = res.json()
```

`http.fetch` returns a `Response` with:
- `.status_code` (int), `.ok` (bool),
- `.headers` (list of `(name, bytes)` tuples), `.header(name)` lookup,
- `.body` (bytes), `.text` (str), `.json()` (parsed).

**`urllib.request.urlopen`** — works as-is. The runtime
monkey-patches it to route through wasi:http.

```python
import urllib.request
with urllib.request.urlopen("https://api.example.com/x") as r:
    body = r.read()
```

**`urllib3` / `requests`** — work as-is. The runtime injects a custom
connection class at urllib3 import time, so `requests.get(...)` etc.
just work. **They do not apply credentials automatically** — for
authenticated requests, either use `http.fetch(..., credential_name=...)`
or pull the placeholder and set the header yourself.

```python
import requests
r = requests.get("https://api.example.com/x")     # works
```

**`httpx`** — works as-is, both sync and async. Custom
`HTTPTransport` / `AsyncHTTPTransport` injected at import.

```python
import httpx
r = httpx.get("https://api.example.com/x")
```

**Raw sockets are not available.** `socket.socket()` raises `OSError`.

### Filesystem

Off by default — a handler sees only its own bundle. Declare a
`Filesystem` capability to get host-directory access, then read and
write with ordinary `open()` / `pathlib` (the runtime routes them
through `wasi:filesystem`).

```python
from particle.manifest import Filesystem, Mount, TempMount

filesystem=Filesystem(
    # Host directories the *user* maps to real paths — persistently
    # with `particle mount <particle> <name> <host-path>`, or per run
    # with `--mount <name>=<host-path>`.
    mounts={
        "data": Mount(
            description="Where reports are written",  # shown in the install prompt
            path="/mnt/data",                          # absolute path inside the sandbox
            access="readwrite",                        # "readonly" | "readwrite"
            required=True,                             # refuse to run until mapped (default False)
        ),
    },
    # Scratch space the host provisions fresh each run and clears on
    # exit. Always read-write; the user never maps these.
    temp={
        "work": TempMount(description="Scratch space", path="/tmp/work", max_size="10MB"),
    },
),
```

```python
with open("/mnt/data/in.json") as f:
    data = f.read()
with open("/mnt/data/out.json", "w") as f:
    f.write(result)
```

- The handler opens the declared `path` directly; the mount *name*
  never appears inside the sandbox.
- Writing to a `readonly` mount, or reading/writing any path outside a
  declared mount, raises `OSError`.
- A `required` mount the user never maps fails at **run** time, not
  build/install.
- `max_size` is a byte count with an optional `KB`/`MB`/`GB` suffix
  (`"10MB"`); writes past it fail.

### Credentials

```python
from particle import credentials

# The method the user picked at setup ("oauth", "pat", ...) or None.
method = credentials.get_configured_method("example")

# Raw value of a type-"raw" credential.
value = credentials.get_raw("some-raw-cred")

# Low-level: get an opaque placeholder + apply-spec describing where
# the host expects to substitute it. Almost always you want
# `http.fetch(..., credential_name=...)` instead, which does this
# internally.
info = credentials.get_placeholder("example")
```

### Key/value store

Scoped per-particle. Strings only — base64-encode binary if needed.
Declare it with `kv=KV(enabled=True)` on the `Particle` — without the
declaration every call raises a `not-declared` error (no user approval
is needed, just the declaration):

```python
from particle.manifest import Particle, KV
# particle = Particle(..., kv=KV(enabled=True))
```

```python
from particle import kv

kv.set("cursor", "abc123")
v = kv.get("cursor")             # str | None
kv.delete("cursor")
keys = kv.list("prefix:")        # list[str]   — note: shadows builtin
```

Import the module as `kv` (not `from particle.kv import list`) so
`list` doesn't shadow the builtin.

### OAuth refresh

Only if you have an `OAuth2` credential. Calls to `http.fetch` with
`credential_name=` auto-refresh expired tokens; call this only when
an upstream rejects a token you *thought* was still valid.

```python
from particle import oauth
oauth.refresh("example")
```

### Signing

Only for `SigningKey` credentials. Key material never enters Python.

```python
from particle import signing
sig = signing.sign("webhook-key", payload_bytes)
ok  = signing.verify("webhook-key", data_bytes, signature_bytes)
```

## 4. Credential methods

Each `Credential` has a `methods={...}` map. The user picks one at
setup. The dataclass types come from `particle.manifest`.

### OAuth2

```python
"oauth": OAuth2(
    description="Sign in via OAuth",
    flows=["authorization-code", "device-code"],
    scopes=["read", "write"],
    authorization_url="https://provider/oauth/authorize",
    token_url="https://provider/oauth/token",
    device_auth_url="https://provider/oauth/device/code",  # for "device-code"
),
```

Valid flow strings: `"authorization-code"`, `"authorization-code-pkce"`,
`"device-code"`.

### ApiKey

```python
"pat": ApiKey(
    description="Use a personal access token",
    location=ApiKeyLocation(kind="auth-scheme", scheme="Bearer"),
),
```

`ApiKeyLocation.kind` is one of:

- `"header"` + `name`: `<name>: <key>`
- `"auth-scheme"` + `scheme`: `Authorization: <scheme> <key>` (e.g. `Bearer`)
- `"query-param"` + `name`: appended as `?<name>=<key>`

Omit `location` entirely to let the importer prompt the user at setup.

### Basic

```python
"basic_auth": Basic(description="Username + password"),
```

Substitutes as `Authorization: Basic <base64>`.

### SigningKey

```python
"webhook": SigningKey(description="...", algorithm="hmac-sha256"),
```

Valid: `"hmac-sha256"`, `"hmac-sha512"`. Use via `signing.sign`.

### Raw

```python
"api_key_blob": Raw(description="..."),
```

Use via `credentials.get_raw(name)`. Choose this only when you
genuinely need the raw bytes — the other types are safer because
the value never enters Python memory.

## 5. PyPI dependencies

Declare in the PEP 723 block at the top of the file:

```python
# /// script
# requires-python = ">=3.12"
# dependencies = [
#   "httpx>=0.27",
#   "pyjwt>=2",
# ]
# ///
```

Constraints:

- **Pure-Python wheels only.** Anything that needs a C extension
  (numpy, pydantic v1, lxml, ...) won't load.
- Standard library is largely available — `json`, `re`, `hashlib`,
  `hmac`, `base64`, `secrets`, `datetime`, `urllib.parse`, `asyncio`,
  `email`, etc. all work.
- `ssl`, raw `socket`, `subprocess` — not available.
- `open()` / `pathlib` work only for paths inside a declared
  filesystem mount (see "Filesystem" above) and the particle's own
  `/particle/` bundle; arbitrary host paths are denied.

## 6. Critical rules

1. **No module-scope host calls.** Calling `http.fetch`, `kv.get`,
   `credentials.get_placeholder`, etc. at the top of the file will
   fail at build time (the introspect phase runs your module under
   trap stores). Do every host call inside a `handler` or `ping`
   function.

   ```python
   # ✗ Will fail the build
   _cached_data = http.fetch("https://api.example.com/init")
   particle = Particle(...)

   # ✓ Inside the handler
   def _go(args):
       res = http.fetch("https://api.example.com/...")
       ...
   particle = Particle(tools={"go": Tool(..., handler=_go)})
   ```

2. **Declare every host you fetch.** A request to a host missing
   from `http=Http(allowed_hosts=[...])` is denied. The error
   surfaces as an exception in the handler.

3. **`Credential.hosts` must be a subset of the HTTP allow-list.**
   The build rejects credentials bound to hosts the particle can't
   reach.

4. **`name` must be kebab-case** (lowercase, hyphens). The registry
   key is `(name, version)`.

5. **`version` must be valid semver.** `0.1.0`, `1.2.3-rc.1`, etc.

6. **Tool handlers must return JSON-serializable values.** Plain
   `dict`, `list`, `str`, `int`, `float`, `bool`, `None`. A dataclass
   or `datetime` will fail to serialize — convert before returning.

## 7. Workflow

After writing `Particlefile.py`:

```sh
particle build                          # parse, resolve deps, introspect, register
particle ping <name>                    # verify ping (if defined)
particle run  <name>                    # list tools
particle run  <name> <tool> --help      # show tool's flags
particle run  <name> <tool> --foo=bar   # invoke it
```

`particle build` walks the user through credential setup interactively
the first time. If the build fails, the error names the phase
(`import-scan`, `resolve-and-fetch`, `manifest-extract`) and the
specific problem.

If the particle declares filesystem mounts, map them before running
(or pass `--mount name=path` on `particle run` for a one-off):

```sh
particle mount <name> <mount-name> <host-path>   # save a persistent mapping
particle mount <name>                            # list mounts + current mappings
```

To expose the particle as a standalone executable (handy for Claude
Code skills and shell use), `particle link <name> ./<name>` writes a
launcher that forwards its args to `particle run <name>`.

## 8. Worked example

A minimal particle exposing a GitHub repo-lookup tool:

```python
# /// script
# requires-python = ">=3.12"
# dependencies = []
# ///
import json
from particle import http
from particle.manifest import Particle, Tool, Http, Credential, ApiKey, ApiKeyLocation

def _get_repo(args):
    res = http.fetch(
        f"https://api.github.com/repos/{args['owner']}/{args['repo']}",
        headers={"Accept": "application/vnd.github+json"},
        credential_name="github",
    )
    if not res.ok:
        raise RuntimeError(f"GitHub {res.status_code}: {res.text}")
    r = res.json()
    return {
        "full_name": r.get("full_name"),
        "stars":     r.get("stargazers_count"),
        "url":       r.get("html_url"),
    }

particle = Particle(
    name="github-lookup",
    description="Look up GitHub repository metadata.",
    version="0.1.0",
    http=Http(allowed_hosts=["api.github.com"]),
    credentials={
        "github": Credential(
            hosts=["api.github.com"],
            required=True,
            methods={
                "pat": ApiKey(
                    description="GitHub personal access token",
                    location=ApiKeyLocation(kind="auth-scheme", scheme="Bearer"),
                ),
            },
        ),
    },
    tools={
        "get_repo": Tool(
            description="Fetch metadata for a GitHub repository.",
            input_schema={
                "type": "object",
                "properties": {
                    "owner": {"type": "string", "description": "Owner login."},
                    "repo":  {"type": "string", "description": "Repository name."},
                },
                "required": ["owner", "repo"],
            },
            handler=_get_repo,
        ),
    },
)
```

## 9. Quick reference

| Need | Import / call |
|---|---|
| HTTP request | `http.fetch(url, method=..., headers=..., body=..., credential_name=...)` |
| Use urllib | `urllib.request.urlopen(url)` — works, no credential applied |
| Use requests | `requests.get(url)` — works, no credential applied |
| Use httpx | `httpx.get(url)` — works, sync + async |
| Read/write a mounted dir | `open()` / `pathlib` on the declared `path` (needs `Filesystem` capability) |
| Per-particle state | `kv.get / kv.set / kv.delete / kv.list` |
| Force OAuth refresh | `oauth.refresh(name)` |
| HMAC sign / verify | `signing.sign(name, bytes) / signing.verify(name, bytes, sig)` |
| Which method picked at setup | `credentials.get_configured_method(name)` |
| Raw credential value | `credentials.get_raw(name)` |
| Manifest declarations | `from particle.manifest import Particle, Tool, Http, Filesystem, Mount, TempMount, Credential, OAuth2, ApiKey, ApiKeyLocation, SigningKey, Basic, Raw` |

Imports always come from `particle` or `particle.manifest` — do not
invent other names.
