# Particles

Particles lets you write small, self-contained, capability-sandboxed programs ("particles") that expose typed tools to an LLM. The common case: you want to plug a remote system into an LLM, but it has no MCP server, or it has one that dumps so much into the model's context that the rest of the conversation suffers. A `particle` ships just the handful of tools you actually want exposed, with hard isolation between the LLM-facing API and the credentials behind it — so you can wire a polished adapter into your harness instead of an off-the-shelf MCP you can't trim.

A `particle` is one ESM module or Python file: a manifest, a few tools, an explicit list of capabilities (HTTP hosts it can reach), and a declared set of credentials it needs. The same artifact works two ways: drop it into Claude Desktop / Cursor / any MCP client via `particle serve-mcp`, or call its tools straight from the shell with `particle run` — useful for Claude Code skills and ad-hoc scripts. Every particle also gets a per-particle key/value store out of the box — no declaration needed.

Six things make it different from "just a script":

- **Inputs validated before your handler runs.** Each tool declares a JSON-Schema input. The runtime checks the call before your code sees it, so a missing or wrong-typed argument surfaces a clear error rather than a stack trace from inside the handler.
- **Sandboxed by capability.** Particles run in a WebAssembly sandbox with no implicit access to anything — not the network, not the filesystem, not environment variables. If the manifest doesn't list `http.allowedHosts: ["api.github.com"]`, neither your code nor a transitive npm dep can reach `api.github.com`. The host grants only what the manifest declares; everything else is denied. This is meaningful supply-chain protection: a compromised package upstream of your particle can't exfiltrate anything it doesn't already have explicit permission to touch.
- **Secure credential storage.** Secrets are encrypted at rest with a key held in the OS keychain, and never made available to code running inside the particle. For OAuth, API keys, and Basic auth, the host fills in the real value as the request leaves the sandbox — your handler only ever sees an opaque placeholder. Signing keys work the same way: `signing.sign(name, data)` returns a signature without the key ever reaching JS. A malicious npm dep scanning memory or `process.env` gets nothing. Setup is a one-time CLI prompt.
- **No local toolchain.** The CLI bundles npm resolution, TypeScript typechecking, and esbuild. No Node install, no `node_modules`, no build config — `particle build` reads `Particlefile.ts`, resolves every `npm:` import declared in source, and produces a single self-contained artifact.
- **Easy for an LLM to write.** A `particle` is one file with one default export and a small set of fields (`name`, `capabilities`, `tools`). An agent that hits "I need a tool to do X" mid-task can write a `Particlefile.ts`, run `particle build`, and have that tool available through its own MCP or CLI surface for the rest of the session — and every session after. The sandbox means a hastily-written particle can't reach beyond what its manifest declared, even if the model misjudged what it was writing.
- **Easy to share.** `particle build --pack` produces a self-contained `.particle` archive; the receiver installs it with `particle import <file-or-url>`. Before anything runs, they see exactly which capabilities the particle requests — hosts, credentials — and either accept or decline. The sandbox enforces those capabilities at runtime, so installing someone else's particle doesn't require trusting their code — just trusting that the manifest accurately describes the worst case.

## A particle, top to bottom

Particles can be written in **TypeScript / JavaScript** or **Python**. The runtime contract and capability model are identical; the choice is author preference. The build pipeline picks the engine from the source filename — `Particlefile.{ts,js}` → JS runtime (QuickJS), `Particlefile.py` → Python runtime (CPython compiled to `wasm32-wasip2`). Examples below are TypeScript; see `examples/github-py/Particlefile.py` for the equivalent in Python.

A particle lives in `Particlefile.ts` (or `.js`) at the root of a directory:

```ts
import { credentials } from "@partite-ai/particle-credentials";

export default {
  name: "github-tools",
  description: "A few GitHub API tools.",
  version: "0.1.0",

  capabilities: {
    http: { allowedHosts: ["api.github.com"] },
  },

  credentials: {
    github: {
      hosts: ["api.github.com"],
      required: true,
      methods: {
        pat: {
          type: "apikey",
          location: { kind: "auth-scheme", scheme: "Bearer" },
        },
      },
    },
  },

  tools: {
    get_repo: {
      description: "Fetch repository metadata.",
      inputSchema: {
        type: "object",
        properties: {
          owner: { type: "string" },
          repo:  { type: "string" },
        },
        required: ["owner", "repo"],
      },
      handler: async ({ owner, repo }: { owner: string; repo: string }) => {
        const fetcher = await credentials.fetcher("github");
        const res = await fetcher(`https://api.github.com/repos/${owner}/${repo}`);
        return res.json();
      },
    },
  },
};
```

The richer `examples/github` particle adds OAuth 2 as an alternate auth method for the same `github` credential, a few more tools, and a `ping` health check.

## Quick start

```sh
cd examples/github

# Build the particle and register it in the local state DB.
# Walks credential setup the first time — pick OAuth or a PAT.
particle build

# Call the registered particle directly.
particle ping  github-tools
particle run   github-tools get_repo --owner=octocat --repo=hello-world

# Or expose every tool as an MCP stdio server — drop this into
# Claude Desktop, a Claude Code skill, Cursor, or any other
# MCP-aware client.
particle serve-mcp github-tools
```

The state DB lives at `<user-config-dir>/particle/state.db` (override with `--db`). Subsequent builds with the same `(name, version)` skip credential prompts.

A typical MCP-client config (Claude Desktop, Cursor, etc.) wires the particle in directly:

```json
{
  "mcpServers": {
    "github-tools": {
      "command": "particle",
      "args": ["serve-mcp", "github-tools"]
    }
  }
}
```

## Commands

| Command | What it does |
|---|---|
| `particle build [--pack]` | Build the particle in CWD. Default: register it. `--pack` writes a `<name>-<version>.particle` archive instead. |
| `particle import <file-or-url>` | Read an archive produced by `--pack`, run it through the same setup flow, and register it. |
| `particle list` / `ls` | List every registered particle and its configured auth method. |
| `particle delete <name>[@version]` / `rm` | Remove a registered particle. Without `@version`: every version. |
| `particle reconfigure <name>` | Re-prompt for credentials. Lets you switch auth methods. |
| `particle ping <name>[@version]` | Call the particle's `ping` health-check. |
| `particle run <name>[@version] <tool> [tool-flags]` | Call a tool. `--help` after the tool name lists the schema-derived flags. `--mount name=path` (before the name) maps a filesystem mount for this run. |
| `particle serve-mcp <name>[@version]` | Stdio MCP server. `--only-tools=a,b` and `--exclude-tools=c,d` filter what's exposed; `--mount name=path` maps a mount for the session. |
| `particle mount <name>[@version] [<mount> <host-path>]` | With just a particle name, list its declared mounts and current mappings. With a mount name + host path, save a persistent mapping. |
| `particle unmount <name>[@version] <mount>` | Remove a saved mount mapping. |
| `particle link <name>[@version] <path>` | Create a small executable at `<path>` that runs `particle run <name>`, forwarding its arguments — a sh shim on Unix, a launcher `.exe` on Windows. |

Version is optional anywhere it appears — omitted resolves to the highest registered semver.

## Capabilities at a glance

A particle only sees what its `capabilities` declare. Importing a host capability module (`@partite-ai/particle-credentials`, `@partite-ai/particle-oauth`, `@partite-ai/particle-signing`) for a category not authorized by the manifest is a build error.

- `http: { allowedHosts: [...] }` — outbound HTTP. Anything else fails with a "destination prohibited" error. Host matching is case-insensitive.
- `filesystem: { mounts: {...}, temp: {...} }` — host directory access, off by default. See [Filesystem](#filesystem) below. Raw sockets and other categories are tracked under "What's next".

A per-particle key/value store is available via `@partite-ai/particle-kv` — it's a built-in, not a capability you declare. Two particles using the same key see independent values; one particle's storage is invisible to another.

## Filesystem

By default a particle sees no host filesystem at all — only its own bundle. Declaring `capabilities.filesystem` opts into two kinds of directory access:

- **Mounts** — host directories the *user* maps in. Each declares a `description` (shown in the install prompt and `particle mount` listings), the absolute `path` it appears at inside the sandbox, an `access` of `"readonly"` or `"readwrite"`, and optional `required`. The handler just reads/writes the declared path; the runtime enforces read-only by rejecting writes. The user supplies the real directory — persistently with `particle mount <particle> <mount> <host-path>`, or per-run with `--mount <mount>=<host-path>`. A `required` mount that's never mapped fails at *run* time, not install.
- **Temp mounts** — scratch space the host provisions automatically: a fresh empty directory each run, capped at `maxSize`, cleared when the particle exits. Always read-write; the user never maps these. `maxSize` is a byte count, optionally with a `KB`/`MB`/`GB` suffix (`"10MB"`).

No capability module to import — the handler uses ordinary filesystem APIs (`node:fs/promises` in JS, `open`/`pathlib` in Python), which the runtime routes to the mounted directories via `wasi:filesystem`. A write outside a mount, or into a read-only one, fails.

```ts
capabilities: {
  filesystem: {
    mounts: {
      source: { description: "Files to read", path: "/mnt/source", access: "readonly", required: true },
      dest:   { description: "Where to write", path: "/mnt/dest",   access: "readwrite", required: true },
    },
    temp: {
      work: { description: "Scratch space", path: "/tmp/work", maxSize: "10MB" },
    },
  },
},
```

`examples/file-copy` is a complete runnable particle built around two mounts. Manage a particle's mappings with `particle mount` / `particle unmount`; both list and the install flow surface every mount in the permission summary so the user sees exactly which directories are requested before accepting.

## Credentials

`credentials` is its own top-level field on the default export — it describes what secret material the particle needs, not a permission gate. Each entry names a logical credential (e.g., `github`, `openai`):

- `hosts: [...]` — pins the credential to a set of HTTP destinations. The host substitutes the real value only on requests bound for one of those hosts; a stray placeholder in a request to anywhere else transmits literally, surfacing the bug rather than leaking the secret. Every host here must also appear in `capabilities.http.allowedHosts`. Omit `hosts` for credentials accessed entirely through the JS-side API (signing keys, raw values).
- `required: true | false` — whether setup refuses to register the particle until a method is configured. Defaults to `false`.
- `methods: { <name>: { type, ... } }` — one or more alternative authentication methods. Supported types: `basic`, `oauth2`, `apikey`, `signing-key`, `raw`. The user picks one at setup; only that one is provisioned, and switching later atomically replaces it.

Tools call `credentials.fetcher("<credName>")` to get a `fetch`-shaped function bound to that credential — the same call regardless of which method the user picked, as long as the methods land at the same wire location. When the manifest mixes methods with different shapes (e.g., a header-based PAT alongside a query-param key), `credentials.getConfiguredMethod("<credName>")` reports which method is active so the tool can branch.

Secrets are encrypted at rest with a key in the OS keychain. When no keychain is reachable — typically a headless Linux box with no D-Bus / Secret Service — the CLI prints a warning and falls back to storing secrets *in cleartext* inside the state DB so credential operations can still proceed. This is an availability-over-confidentiality tradeoff; on those hosts, protect the DB file (it's created `0600`) or supply your own keychain. Either way the secret is never exposed to code running inside the particle.

## TypeScript types

`js-types/particle/index.d.ts` defines the shape of the default export. To get type-checking on your particle:

```ts
import type { Particle } from "@partite-ai/particle";

export default {
  // ...
} satisfies Particle;
```

A typed handler argument is recommended (the schema isn't projected into the type system; the runtime validates at the boundary):

```ts
handler: async ({ owner, repo }: { owner: string; repo: string }) => { /* ... */ }
```

## Where to read next

- `docs/initial-design.md` — full design spec for the runtime, build pipeline, capability model, and credential types.
- `examples/` — runnable particles: `is-odd` (no capabilities), `github` / `github-py` (HTTP + credentials, TS and Python), `file-copy` (filesystem mounts), `sheets-analytics`.
- `js-types/particle/index.d.ts` — the `Particle` type plus every capability and credential variant.

## How this compares

**vs. Docker / VMs.** Containers and VMs also give you isolation, but at OS weight: image layers, a daemon, seconds-to-start cold paths, and a permissions model that's typically wired up at `docker run` time rather than declared inside the artifact. Particles start in milliseconds and bake the capability list into the artifact itself, so the user sees what's being asked for before anything launches. Trade-off: a particle runs JavaScript today, not arbitrary binaries (see "What's next" — that's expanding).

**vs. raw Node / Bun / Deno.** A bare script trusts its dependencies with everything: filesystem, network, environment. A particle's transitive deps inherit the same denied-by-default surface as the particle itself — if a fashionable npm package decides to phone home, it can't, because `api.evil.example` isn't in `allowedHosts`. Deno's `--allow-*` flags cover similar ground at the process level; particles attach the policy to the artifact, so the policy travels with the code and the *user* (not the author) controls whether to grant it.

## What's next

Particles are in active development. On the roadmap:

- **Outbound sockets** with a declared allow-list, mirroring the HTTP model.


## Building the project

The `particle` binary embeds five WebAssembly components: three build-pipeline helpers (npm resolution, pip resolution, TypeScript type-check) and two runtimes (JS via QuickJS, Python via CPython). To rebuild the components you'll need:

- Go 1.26+
- Rust + the `wasm32-wasip2` target
- [`wasm-rquickjs`](https://github.com/wasm-rquickjs/wasm-rquickjs) (JS runtime + typecheck)
- The [WASI SDK](https://github.com/WebAssembly/wasi-sdk) at `/opt/wasi-sdk` (override with `WASI_SDK_PATH`) — `make python-lib` builds CPython 3.14 from source into a `wasm32-wasip2` library against its clang + sysroot, which the Python runtime component links in
- A host C toolchain + `make` (the CPython build also compiles a native host interpreter to bootstrap the cross-build)
- Node + npm (the typecheck wasm bundles the TypeScript compiler)

You may find the included devcontainer useful.

```sh
make            # builds every wasm component and copies them into the embed dirs
go build -o particle ./cmd/particle
```

## Status

Early. APIs, on-disk formats, and the credential setup flow may change as the project matures.
