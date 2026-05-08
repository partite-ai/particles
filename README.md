# Particle

Particle is a tool for writing, packaging, and running small JavaScript / TypeScript programs that expose typed tools to LLM clients (over the Model Context Protocol) or to a CLI. Each program — a "particle" — is a single ESM module with a manifest, a few tools, and an explicit list of capabilities (HTTP hosts it can reach, credentials it needs, key/value storage, etc.). Particles run inside a WebAssembly sandbox; the host enforces every capability at the WIT boundary.

Four things make it different from "just a script":

- **Type-safe tool surface.** Each tool has a JSON-Schema-validated argument shape. The runtime validates host-side before invoking JS, so a missing or wrong-typed argument produces a clear error rather than a JS exception.
- **Sandboxed by capability.** Particles run in a WebAssembly sandbox with no implicit access to the network, filesystem, or environment. Anything a particle can do is declared in its manifest; the host enforces every capability at the WIT boundary. A particle that didn't declare `http.allowedHosts: ["api.github.com"]` literally cannot reach api.github.com — not even from a transitive npm dep that tries to.
- **Credentials never enter the JS handler.** For OAuth, API-key, and HTTP-Basic auth, the runtime substitutes the real value at the wasi:http boundary right before the request leaves the host — the handler only ever sees an opaque placeholder. Signing keys never leave the host either: particles call `signing.sign(name, data)` and get a signature back. A compromised npm dep that scans memory or `process.env` doesn't see your tokens. Setup is a CLI prompt; secrets are encrypted at rest with an OS-keychain-stored key.
- **No local toolchain.** The CLI bundles npm resolution, TypeScript typechecking, and esbuild into its own WebAssembly components. You don't install Node, manage `node_modules`, or write a build config — `particle build` reads `Particlefile.ts`, resolves every `npm:` import declared in source, and produces a single self-contained artifact.

## A particle, top to bottom

A particle lives in `Particlefile.ts` (or `.js`) at the root of a directory:

```ts
import { credentials } from "particle:credentials";

export default {
  name: "github-tools",
  description: "A few GitHub API tools.",
  version: "0.1.0",

  capabilities: {
    http: { allowedHosts: ["api.github.com"] },
    credentials: {
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
        const fetcher = await credentials.fetcher("pat");
        const res = await fetcher(`https://api.github.com/repos/${owner}/${repo}`);
        return res.json();
      },
    },
  },
};
```

The richer `examples/github` particle adds OAuth 2 as an alternate auth method, a few more tools, and a `ping` health check.

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

A typical Claude skill config that wires up the particle:

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
| `particle build [--pack]` | Build the particle in CWD. Default: register it. `--pack` writes a `<name>-<version>.particle` tarball instead. |
| `particle import <file.particle>` | Read a tarball produced by `--pack`, run it through the same setup flow, and register it. |
| `particle list` / `ls` | List every registered particle and its configured auth method. |
| `particle delete <name>[@version]` / `rm` | Remove a registered particle. Without `@version`: every version. |
| `particle reconfigure <name>` | Re-prompt for credentials. Lets you switch auth methods. |
| `particle ping <name>[@version]` | Call the particle's `ping` health-check. |
| `particle run <name>[@version] <tool> [tool-flags]` | Call a tool. `--help` after the tool name lists the schema-derived flags. |
| `particle serve-mcp <name>[@version]` | Stdio MCP server. `--only-tools=a,b` and `--exclude-tools=c,d` filter what's exposed. |

Version is optional anywhere it appears — omitted resolves to the highest registered semver.

## Capabilities at a glance

A particle only sees what its `capabilities` declare. Importing `particle:*` for a category not in the manifest is a build error.

- `http: { allowedHosts: [...] }` — outbound HTTP. Anything else fails with a "destination prohibited" error.
- `credentials: { required, methods: {...} }` — alternative auth methods (`basic`, `oauth2`, `apikey`, `signing-key`, `raw`). The user picks one at setup; only that one is provisioned. Tools call `credentials.getConfiguredMethod()` to discover which name to pass to `credentials.fetcher(name)`.
- `kv: {}` — per-particle persistent key/value store via `particle:kv`.
- `env: { VAR: { description?, required? } }` — allowlist of env vars exposed through `process.env`. Undeclared vars don't reach the particle.

## TypeScript types

`types/particle.d.ts` defines the shape of the default export. To get type-checking on your particle:

```ts
import type { Particle } from "particle";

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
- `examples/` — runnable particles (`is-odd`, `github`).
- `types/particle.d.ts` — the `Particle` type plus every capability and credential variant.

## Building the project

The `particle` binary embeds three WebAssembly components used by the build pipeline (npm resolution, type-check, manifest extraction). To rebuild them, you'll need:

- Go 1.26+
- Rust + the `wasm32-wasip2` target
- [`wasm-rquickjs`](https://github.com/wasm-rquickjs/wasm-rquickjs)
- Node + npm (the typecheck wasm bundles the TypeScript compiler)

```sh
make            # builds every wasm component and copies them into the embed dirs
go build -o particle ./cmd/particle
```

## Status

Early. APIs, on-disk formats, and the credential setup flow may change as the project matures.
