# Particle

Particle lets you write small, self-contained, capability-sandboxed programs ("particles") that expose typed tools to an LLM. The common case: you want to plug a remote system into an LLM, but it has no MCP server, or it has one that dumps so much into the model's context that the rest of the conversation suffers. A particle ships just the handful of tools you actually want exposed, with hard isolation between the LLM-facing API and the credentials behind it — so you can wire a polished adapter into your harness instead of an off-the-shelf MCP you can't trim.

A particle is one ESM module: a manifest, a few tools, and an explicit list of capabilities (HTTP hosts it can reach, credentials it needs). The same artifact works two ways: drop it into Claude Desktop / Cursor / any MCP client via `particle serve-mcp`, or call its tools straight from the shell with `particle run` — useful for Claude Code skills and ad-hoc scripts. Every particle also gets a per-particle key/value store out of the box — no declaration needed.

Six things make it different from "just a script":

- **Inputs validated before your handler runs.** Each tool declares a JSON-Schema input. The runtime checks the call before your code sees it, so a missing or wrong-typed argument surfaces a clear error rather than a stack trace from inside the handler.
- **Sandboxed by capability.** Particles run in a WebAssembly sandbox with no implicit access to anything — not the network, not the filesystem, not environment variables. If the manifest doesn't list `http.allowedHosts: ["api.github.com"]`, neither your code nor a transitive npm dep can reach `api.github.com`. The host grants only what the manifest declares; everything else is denied. This is meaningful supply-chain protection: a compromised package upstream of your particle can't exfiltrate anything it doesn't already have explicit permission to touch.
- **Secure credential storage.** Secrets are encrypted at rest with a key held in the OS keychain, and never made available to code running inside the particle. For OAuth, API keys, and Basic auth, the host fills in the real value as the request leaves the sandbox — your handler only ever sees an opaque placeholder. Signing keys work the same way: `signing.sign(name, data)` returns a signature without the key ever reaching JS. A malicious npm dep scanning memory or `process.env` gets nothing. Setup is a one-time CLI prompt.
- **No local toolchain.** The CLI bundles npm resolution, TypeScript typechecking, and esbuild. No Node install, no `node_modules`, no build config — `particle build` reads `Particlefile.ts`, resolves every `npm:` import declared in source, and produces a single self-contained artifact.
- **Easy for an LLM to write.** A particle is one file with one default export and a small set of fields (`name`, `capabilities`, `tools`). An agent that hits "I need a tool to do X" mid-task can write a `Particlefile.ts`, run `particle build`, and have that tool available through its own MCP or CLI surface for the rest of the session — and every session after. The sandbox means a hastily-written particle can't reach beyond what its manifest declared, even if the model misjudged what it was writing.
- **Easy to share.** `particle build --pack` produces a self-contained `.particle` tarball; the receiver installs it with `particle import <file-or-url>`. Before anything runs, they see exactly which capabilities the particle requests — hosts, credentials — and either accept or decline. The sandbox enforces those capabilities at runtime, so installing someone else's particle doesn't require trusting their code — just trusting that the manifest accurately describes the worst case.

## A particle, top to bottom

A particle lives in `Particlefile.ts` (or `.js`) at the root of a directory:

```ts
import { credentials } from "@partite-ai/particle-credentials";

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
| `particle build [--pack]` | Build the particle in CWD. Default: register it. `--pack` writes a `<name>-<version>.particle` tarball instead. |
| `particle import <file-or-url>` | Read a tarball produced by `--pack`, run it through the same setup flow, and register it. |
| `particle list` / `ls` | List every registered particle and its configured auth method. |
| `particle delete <name>[@version]` / `rm` | Remove a registered particle. Without `@version`: every version. |
| `particle reconfigure <name>` | Re-prompt for credentials. Lets you switch auth methods. |
| `particle ping <name>[@version]` | Call the particle's `ping` health-check. |
| `particle run <name>[@version] <tool> [tool-flags]` | Call a tool. `--help` after the tool name lists the schema-derived flags. |
| `particle serve-mcp <name>[@version]` | Stdio MCP server. `--only-tools=a,b` and `--exclude-tools=c,d` filter what's exposed. |

Version is optional anywhere it appears — omitted resolves to the highest registered semver.

## Capabilities at a glance

A particle only sees what its `capabilities` declare. Importing a host capability module (`@partite-ai/particle-credentials`, `@partite-ai/particle-oauth`, `@partite-ai/particle-signing`) for a category not in the manifest is a build error.

- `http: { allowedHosts: [...] }` — outbound HTTP. Anything else fails with a "destination prohibited" error. Host matching is case-insensitive.
- `credentials: { required, methods: {...} }` — alternative auth methods (`basic`, `oauth2`, `apikey`, `signing-key`, `raw`). The user picks one at setup; only that one is provisioned. Tools call `credentials.getConfiguredMethod()` to discover which name to pass to `credentials.fetcher(name)`.

A per-particle key/value store is available via `@partite-ai/particle-kv` — it's a built-in, not a capability you declare. Two particles using the same key see independent values; one particle's storage is invisible to another.

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

## How this compares

**vs. Docker / VMs.** Containers and VMs also give you isolation, but at OS weight: image layers, a daemon, seconds-to-start cold paths, and a permissions model that's typically wired up at `docker run` time rather than declared inside the artifact. Particles start in milliseconds and bake the capability list into the artifact itself, so the user sees what's being asked for before anything launches. Trade-off: a particle runs JavaScript today, not arbitrary binaries (see "What's next" — that's expanding).

**vs. raw Node / Bun / Deno.** A bare script trusts its dependencies with everything: filesystem, network, environment. A particle's transitive deps inherit the same denied-by-default surface as the particle itself — if a fashionable npm package decides to phone home, it can't, because `api.evil.example` isn't in `allowedHosts`. Deno's `--allow-*` flags cover similar ground at the process level; particles attach the policy to the artifact, so the policy travels with the code and the *user* (not the author) controls whether to grant it.

## What's next

Particles are in active development. On the roadmap:

- **Filesystem access** as a capability, scoped to a host-nominated directory.
- **Outbound sockets** with a declared endpoint allow-list, mirroring the HTTP model.
- **Python particles** via a CPython-on-WASI runtime.
- **Any language that compiles to a WebAssembly Component** — Rust, Go, C, etc. The capability model and runtime contract are guest-language-agnostic; JavaScript is where we started, not where we stop.

## Building the project

The `particle` binary embeds three WebAssembly components used by the build pipeline (npm resolution, type-check, manifest extraction). To rebuild them, you'll need:

- Go 1.26+
- Rust + the `wasm32-wasip2` target
- [`wasm-rquickjs`](https://github.com/wasm-rquickjs/wasm-rquickjs)
- Node + npm (the typecheck wasm bundles the TypeScript compiler)

You may find the included devconatiner useful.

```sh
make            # builds every wasm component and copies them into the embed dirs
go build -o particle ./cmd/particle
```

## Status

Early. APIs, on-disk formats, and the credential setup flow may change as the project matures.
