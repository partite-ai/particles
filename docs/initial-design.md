# Particle — Initial Design

**Status:** Approved (high-level). Detailed design docs for individual components will follow.
**Date:** 2026-05-06

This document captures the high-level design for Particle. It is intentionally not implementation-prescriptive — Go API surfaces, WASM hosting layer specifics, and per-component implementation details are deferred to follow-on design passes. This doc establishes the system's shape, boundaries, and contracts.

## Contents

1. [System Overview](#1-system-overview)
2. [WIT Interfaces and Component Boundaries](#2-wit-interfaces-and-component-boundaries)
3. [Build-Time Components](#3-build-time-components)
4. [Particlefile DSL](#4-particlefile-dsl)
5. [Build Pipeline](#5-build-pipeline)
6. [Runtime Execution Model](#6-runtime-execution-model)
7. [Credential Model](#7-credential-model)
8. [Health / Ping](#8-health--ping)
9. [CLI, Scope, and Phasing](#9-cli-scope-and-phasing)

---

## 1. System Overview

### What it is

Particle is a Go library (with a CLI built on it) that builds and runs **particles** — tiny tool-providing programs written in JavaScript or TypeScript. Particles run on a minimal JavaScript runtime (QuickJS) hosted as a WebAssembly Component. Storage, credentials, and other host capabilities are pluggable via Go interfaces, so the same particle can run locally under the CLI or under a larger system (cloud-backed credentials, multi-tenant storage, etc.) with no source changes.

### Architectural anchor: one runtime, many particles

There is **one** runtime artifact in the system: `particle-runtime.wasm`. It contains rquickjs, the host-import stubs, the boot sequence, and nothing particle-specific. Built once, distributed once. (Build-time manifest extraction lives in a separate component, `particle-introspect.wasm` — see §3.)

To run a particle, the Go host **instantiates** this runtime image and **mounts** the particle's code into it as a virtual filesystem (`wasi:filesystem`). Multiple instances of the same image run concurrently, each with a different mounted FS. No per-particle WASM compilation. No per-particle composition.

A "particle" is a tarball of:

```
manifest.json    — name, capabilities, tool defs (with JSON Schemas)
bundle.js        — esbuild output: all handlers + bundled npm deps
Particle.lock    — resolved deps + integrity hashes
build-info.json  — source hashes, runtime version, build timestamp
```

The Go host extracts this tarball, exposes its contents via wasi:filesystem to the runtime instance, and the runtime reads `bundle.js` at boot.

### Library-first, with the CLI as one consumer

```
┌─────────────────────────────────────────────────────────────┐
│  Consumer (CLI tool, or cloud platform, or test harness)    │
│  Provides Go impls of:                                      │
│    CredentialStore  KVStore  EnvProvider                    │
│    HTTPPolicy       SocketsPolicy                           │
└───────────────────────┬─────────────────────────────────────┘
                        │ Go interfaces
                        ▼
┌─────────────────────────────────────────────────────────────┐
│  particle-core (Go library)                                 │
│  ─ build pipeline (sources → tarball)                       │
│  ─ runtime hosting (load tarball, instantiate WASM)         │
│  ─ WIT bridge (Go ifaces → particle:* imports)              │
│  ─ WASM hosting layer (Go-component runtime)                │
└───────────────────────┬─────────────────────────────────────┘
                        │ instantiates with hostImpls
                        ▼
┌─────────────────────────────────────────────────────────────┐
│  particle-runtime.wasm (one image, many instances)          │
│  Imports: particle:credentials, particle:kv,                │
│           particle:oauth, particle:signing,                 │
│           wasi:http, wasi:sockets, wasi:filesystem,         │
│           wasi:cli/environment, wasi:cli/stderr             │
│  Exports: particle:tools, particle:health                   │
└─────────────────────────────────────────────────────────────┘
```

The CLI is a thin wrapper that wires up local-default implementations of the host interfaces and exposes commands. A cloud system would wire up its own implementations of the same Go interfaces.

The Go-side WASM hosting layer (the bridge between Go and WASM Component Model) is left intentionally unspecified in this doc; implementation choice is deferred.

### Tools-first transport via host adapters

The runtime exports a transport-agnostic `tools` WIT interface. Two host adapters wrap it:

- `particle-mcp` reads MCP-JSON-RPC from stdin, maps to `tools/list` and `tools/call`.
- `particle-cli` parses argv against the manifest's JSON Schemas, maps to `call-tool`.

Both adapters are part of the CLI binary. Adding a new transport (HTTP, gRPC, etc.) means writing another adapter; the particle never changes.

### No enforced directory layout

A particle is whatever produces a manifest + bundle. The common case is a single `Particlefile.{js,ts}` with inline tool definitions. Larger particles split however authors prefer. esbuild traces imports; the build doesn't care about layout.

### Phase 1 scope

Tools-only. Capabilities: HTTP, sockets (gated), credentials, KV, env. Commands: `particle run`, `particle test`, `particle setup`, `particle credentials *`, `particle mcp`, `particle build`, `particle ping`. Local-default backends in the CLI; cross-platform credential support beyond the CLI's defaults can come via additional Go-interface implementations without API churn.

---

## 2. WIT Interfaces and Component Boundaries

### Component map

There is exactly one runtime artifact (`particle-runtime.wasm`). Everything else is Go code in the host process that implements WIT-defined interfaces. The runtime cannot tell the difference between a Go-implemented host capability and a separate `.wasm` component implementing the same WIT — only the WIT contract matters.

```
                        runtime imports
particle-runtime.wasm ──────────────────►  ┌──────────────────────────┐
   (single artifact)                       │ Host (Go process)        │
                                           │                          │
                        runtime exports    │ Implements:              │
particle-runtime.wasm ◄──────────────────  │   particle:credentials   │
                                           │   particle:oauth         │
                                           │   particle:signing       │
                                           │   particle:kv            │
                                           │   wasi:cli/environment   │
                                           │     (manifest-filtered)  │
                                           │   wasi:filesystem (FS    │
                                           │     view of tarball)     │
                                           │   wasi:http (with policy)│
                                           │   wasi:sockets (gated)   │
                                           │                          │
                                           │ Calls:                   │
                                           │   particle:tools         │
                                           │   particle:health        │
                                           └──────────────────────────┘
```

### WIT interface set

All under package `particle:host@0.1.0`. Versioning: major-version bumps for breaking changes; minor for additive. Final WIT syntax may differ slightly based on tooling.

#### `particle:tools` (runtime exports)

```wit
interface tools {
  record tool-def {
    name: string,
    description: string,
    input-schema-json: string,    // JSON Schema, encoded as UTF-8 JSON
  }

  variant tool-error {
    not-found,
    invalid-arguments(string),
    handler-error(string),
    capability-denied(string),
  }

  list-tools: func() -> list<tool-def>;
  call-tool: func(name: string, arguments-json: string) -> result<string, tool-error>;
}
```

JSON values cross the boundary as UTF-8 strings. Schemas, args, and results are all serialized — keeps the contract narrow.

#### `particle:health` (runtime exports)

```wit
interface health {
  enum status { ok, degraded, unhealthy }

  record ping-result {
    status: status,
    message: option<string>,
    details: option<string>,    // JSON, free-form
  }

  variant health-error {
    not-implemented,            // particle did not declare ping
    handler-error(string),
  }

  ping: func() -> result<ping-result, health-error>;
}
```

Optional. If the particle does not declare a `ping` function in its default export, the runtime returns `not-implemented`. Used for health-checking thin adapter particles whose underlying foreign API may be unreachable.

#### `particle:credentials` (host exports)

```wit
interface credentials {
  variant credential-error {
    not-configured,
    storage-error(string),
    type-mismatch(string),       // wrong API for credential type
  }

  enum apply-kind {
    basic,                       // -> Authorization: Basic <placeholder>
    bearer,                      // -> Authorization: Bearer <placeholder>
    header,                      // -> <name>: <placeholder>
    auth-scheme,                 // -> Authorization: <scheme> <placeholder>
    query-param,                 // -> ?<name>=<placeholder>
  }

  record apply-spec {
    kind: apply-kind,
    name: option<string>,
    scheme: option<string>,
  }

  record placeholder-info {
    placeholder: string,
    apply: apply-spec,
  }

  /// For basic, oauth2, apikey (substitution-based types).
  get-placeholder: func(name: string) -> result<placeholder-info, credential-error>;

  /// Only for type: "raw" credentials. Returns the actual stored value.
  get-raw: func(name: string) -> result<string, credential-error>;
}
```

#### `particle:oauth` (host exports)

```wit
interface oauth {
  variant oauth-error {
    not-configured,
    not-oauth,
    refresh-failed(string),
  }

  /// Force a refresh regardless of cached expiry. Used when upstream
  /// returns 401/403 on a token we thought was still valid.
  refresh: func(name: string) -> result<_, oauth-error>;
}
```

#### `particle:signing` (host exports)

```wit
interface signing {
  variant signing-error {
    not-configured,
    not-signing-key,
    invalid-input(string),
  }

  sign: func(name: string, data: list<u8>) -> result<list<u8>, signing-error>;
  verify: func(name: string, data: list<u8>, signature: list<u8>)
    -> result<bool, signing-error>;
}
```

#### `particle:kv` (host exports)

```wit
interface kv {
  variant kv-error {
    storage-error(string),
    quota-exceeded,
  }
  get: func(key: string) -> result<option<string>, kv-error>;
  set: func(key: string, value: string) -> result<_, kv-error>;
  delete: func(key: string) -> result<_, kv-error>;
  list: func(prefix: string) -> result<list<string>, kv-error>;
}
```

KV is per-particle scoped: the host namespaces keys internally by particle ID. The JS sees a flat keyspace. String values only.

#### Env vars (no custom interface — standard WASI)

Particles read env vars via `process.env` (which the QuickJS engine routes through `wasi:cli/environment`). The host's `wasi:cli/environment` implementation filters what gets exposed based on the manifest's `capabilities.env` declaration — undeclared vars don't reach the particle. No custom `particle:env` WIT interface; one less WIT to version.

#### `wasi:http` (host exports)

Standard wasi:http. Policy enforcement is invisible to JS: a denied URL produces a normal network error. The host's wasi:http implementation internally consults a Go `HTTPPolicy` interface before sending requests. The Particlefile manifest declares `capabilities.http.allowedHosts` for the policy to consult.

#### `wasi:sockets` (host exports, gated)

Standard wasi:sockets, gated by capability declaration. If a particle does not declare `capabilities.sockets`, the host wires up a deny-all SocketsPolicy implementation. If it does declare it, the policy enforces an allowlist. Listening sockets are not supported.

#### `wasi:filesystem` (host exports)

The host provides a read-only virtual filesystem rooted at `/particle/`, populated from the particle tarball. No write access. No paths outside `/particle/`. Used during runtime execution and for the build's typecheck phase (mounting source + node_modules).

#### `wasi:cli/stderr`, `wasi:clocks`, `wasi:random`

Standard WASI 0.2. The runtime gets stderr (for `console.*`), clocks, and random. No `stdin`/`stdout` from WASI — those belong to the host adapters.

### Boundary principles

- **Each capability gets its own WIT interface.** Maps cleanly to "implement just the pieces you need." A consumer can wire credentials to a cloud KMS while keeping kv local. Versions independently. Future "this becomes its own .wasm" stays open without runtime changes.
- **Tools and health are the only runtime exports.** The runtime is a tool server; that's its only outward-facing contract.
- **HTTP via wasi:http, not custom interfaces.** Standard `fetch()` works; popular npm packages work unmodified.
- **Filesystem via wasi:filesystem, not a custom "load bundle" interface.** Lets us reuse standard JS module loading (the bundled JS can `import` other files inside the bundle via standard ESM).
- **No `wasi:sockets` by default.** Sockets bypass HTTP policy; gated behind explicit capability.

---

## 3. Build-Time Components

### Component set

The build pipeline turns a source directory into a particle tarball. It's orchestrated by Go code in particle-core, calling out to a mix of WASM components and Go libraries.

```
build-time WASM components:
  deno-npm.wasm              (Rust → WASM)        npm dep resolve + fetch
  particle-typecheck.wasm    (QuickJS+tsc → WASM) TypeScript type checking
  particle-introspect.wasm   (QuickJS → WASM)     extracts manifest from bundle

build-time Go libraries:
  esbuild                                         JS bundling
  import-scan                                     parses npm: specs (uses esbuild's parser API)
```

### Why this split

A piece becomes its own WASM component when it's most naturally written in a non-Go language, **or** when it's a different program — even if it's JS-on-QuickJS, like the runtime, the typechecker, and the introspect component all are. Components are per-purpose, not per-language.

esbuild has a stable Go API and is written in Go; wrapping it in WASM adds bridging tax for no benefit. Orchestration in Go gives natural access to file I/O, caching, parallelism, and stable Go interfaces for plugging in alternate implementations.

`particle-introspect.wasm` is its own component rather than a "mode" of `particle-runtime.wasm` because the two have fundamentally different boot/serve logic. Introspect never invokes a tool handler — it reads the bundle's default-export metadata, validates it, and returns the manifest JSON. Splitting it out keeps the hot-path runtime image free of build-time logic and lets the two evolve independently. The two share the QuickJS toolchain (wasm-rquickjs) but the introspect component imports *no* `particle:host/*` interfaces: since handlers are never called, the bundle's `import { credentials } from "particle:credentials"` statements only need to *resolve*, not work. The introspect component registers JS-level no-op stubs for every `particle:*` module before evaluating the bundle, so resolution happens entirely inside QuickJS without any host wiring.

### `deno-npm.wasm` — Rust component

Wraps the `deno_npm` resolution + tarball-fetch crates. Imports `wasi:http` for registry calls.

```wit
package particle:build@0.1.0;

interface installer {
  record dep-request {
    name: string,
    version-range: string,
  }

  record resolved-dep {
    name: string,
    version: string,
    integrity: string,
    tarball-bytes: list<u8>,
    package-json: string,
    transitive: list<u32>,    // indices into the resolved-dep list
  }

  variant installer-error {
    network-error(string),
    resolution-error(string),
    integrity-mismatch(string),
  }

  resolve-and-fetch: func(deps: list<dep-request>)
    -> result<list<resolved-dep>, installer-error>;
}
```

### `particle-typecheck.wasm` — QuickJS-hosted tsc

Built once: bundle `npm:typescript@^5` with a thin wrapper exposing a `check` function, package as a QuickJS-hosted WASM component using the same wasm-rquickjs toolchain as the runtime — but as a separate artifact.

```wit
interface typecheck {
  enum severity { error, warning, info }

  record diagnostic {
    file: string,
    line: u32,
    column: u32,
    severity: severity,
    code: u32,
    message: string,
  }

  record check-options {
    root-files: list<string>,
    strict: bool,
    target: string,
  }

  variant check-error {
    config-error(string),
    internal-error(string),
  }

  check: func(opts: check-options) -> result<list<diagnostic>, check-error>;
}
```

Imports: `wasi:filesystem` (source + node_modules). Nothing else.

A future Go-native implementation (e.g., `tsgo` once it stabilizes) would expose the same Go-side `TypeChecker` interface; swap is mechanical and the build pipeline stays unchanged.

### `particle-introspect.wasm` — QuickJS-hosted manifest extractor

A separate QuickJS-hosted component, built with the same wasm-rquickjs toolchain as the runtime but a distinct artifact. Its job is to evaluate the freshly built `bundle.js` and return the manifest JSON.

```wit
package particle:introspect@0.1.0;

interface introspect {
  variant introspect-error {
    bundle-load-error(string),
    invalid-manifest(string),
  }

  /// Returns the manifest as JSON: { name, description, version,
  /// capabilities, tools: [{ name, description, inputSchema }] }.
  manifest: func() -> result<string, introspect-error>;
}

world component {
  // No particle:host/* imports. The component stubs particle:*
  // modules inside QuickJS — see explanation below.
  export introspect;
}
```

For each call:

- The host instantiates the component (only standard wasi:* imports needed)
- Mounts the freshly bundled JS as the virtual FS (`bundle.js` only)
- Calls `manifest()`, which registers no-op stubs for every `particle:*` module specifier inside QuickJS, then evaluates `bundle.js`, reads the `default` export, validates the structure, and returns name/description/capabilities and the tools list (with `inputSchema` serialized)

Why a separate component, not "introspect mode" of the runtime? The boot/serve logic is fundamentally different — the runtime dispatches tool calls and runs handler code; introspect never invokes a handler. Keeping them split means the hot-path runtime image stays free of build-time logic, the two evolve independently, and each gets a tighter contract.

Why no `particle:host/*` imports? The introspect component is fully self-contained: it never invokes a handler, so the `particle:*` modules in the user's bundle never *run* — they only need to *resolve*. The component's JS source registers a permissive no-op stub for each `particle:*` specifier before evaluating the bundle, so resolution succeeds entirely inside QuickJS. The host wires only standard wasi:* imports.

Why use a QuickJS component at all rather than a Go-side AST walker? Fidelity. The bundle is real ESM with potential dynamic computation in the default-export object; the only thing that reliably knows what the export contains is a JS engine evaluating it. Sharing the wasm-rquickjs toolchain with the runtime keeps that fidelity essentially free.

### Build artifacts

Final tarball produced by the build pipeline:

```
manifest.json     — output of introspect phase
bundle.js         — output of bundling phase
Particle.lock     — resolved dep tree + integrity
build-info.json   — runtime version, build timestamp, source hashes, sourcemap
```

This is the unit consumed at runtime.

---

## 4. Particlefile DSL

### Shape

A particle is whatever produces a manifest + bundle. The conventional entry point is `Particlefile.{js,ts}` — a regular ES module whose default export describes the particle.

**Minimal single-file particle:**

```js
import yaml from "npm:yaml@^2.3.0";

export default {
  name: "yaml-tools",
  description: "Parse and format YAML.",
  version: "0.1.0",
  capabilities: {},
  tools: {
    parse_yaml: {
      description: "Parse a YAML string into JSON",
      inputSchema: {
        type: "object",
        properties: { input: { type: "string", description: "The YAML text" } },
        required: ["input"],
      },
      handler: async ({ input }) => ({ result: yaml.parse(input) }),
    },
  },
};
```

That's the whole surface. No DSL builder calls, no decorators, no class hierarchy — just a default export.

**Multi-file particle (when it grows):**

```js
// Particlefile.js
import parse_yaml from "./tools/parse_yaml.js";
import format_yaml from "./tools/format_yaml.js";

export default {
  name: "yaml-tools",
  description: "Parse and format YAML.",
  version: "0.1.0",
  capabilities: {
    credentials: {
      github_token: {
        type: "apikey",
        description: "GitHub PAT",
        required: false,
      },
    },
  },
  tools: { parse_yaml, format_yaml },
};

// tools/parse_yaml.js
import yaml from "npm:yaml@^2.3.0";

export default {
  description: "Parse a YAML string into JSON",
  inputSchema: { type: "object", properties: { input: { type: "string" } }, required: ["input"] },
  handler: async ({ input }) => ({ result: yaml.parse(input) }),
};
```

esbuild traces the imports; the build doesn't care where files live.

### The default export schema

```ts
type Particle = {
  name: string;                          // kebab-case
  description: string;
  version: string;                       // semver
  capabilities: Capabilities;
  tools: Record<string, ToolDef>;
  ping?: () => Promise<PingResult> | PingResult;   // optional
  tests?: TestCase[];                    // optional, for `particle test`
};

type ToolDef = {
  description: string;
  inputSchema: object;                   // JSON Schema (object root)
  handler: (args: unknown) => unknown | Promise<unknown>;
};

type PingResult = {
  status: "ok" | "degraded" | "unhealthy";
  message?: string;
  details?: unknown;
};

type TestCase = {
  name: string;
  tool: string;
  args: unknown;
  expect?: unknown;                      // deep-equal compare
  expectError?: { kind: ToolErrorKind; messageMatches?: string };
};
```

Capability declarations are detailed in [§7](#7-credential-model) (credentials), here's the overall shape:

```ts
type Capabilities = {
  http?:        { allowedHosts?: string[] };
  sockets?:     {
    allowedEndpoints: { host: string; port: number }[];
  };
  credentials?: Record<string, CredentialDecl>;   // see §7
  kv?:          {};                       // presence enables the import
  env?:         Record<string, EnvDecl>;
};
```

A capability that doesn't appear in the manifest is denied. Importing a `particle:*` interface without declaring its capability is a build error.

### Imports — the four namespaces

Every import in particle source resolves to exactly one of:

1. **`npm:pkg@version`** — npm package. Version range required. Subpath supported (`npm:lodash@4/get`).
2. **`particle:<capability>`** — host-provided capability. One of `particle:credentials`, `particle:oauth`, `particle:signing`, `particle:kv`. Env vars are not in this namespace; particles read them via `process.env`.
3. **`./relative/path.js`** — local file in the particle source tree.
4. **`/absolute/in-bundle/path`** — discouraged for human authors, but esbuild will resolve them.

No bare specifiers. The `npm:` prefix is mandatory.

### JSON Schema for tool params

`inputSchema` is JSON Schema Draft 2020-12, root being `type: "object"`. The build pipeline validates the schema itself at build time. The host validates incoming arguments against `inputSchema` before invoking the handler.

**Why JSON Schema (not Zod or similar):**
- It's the lingua franca of LLM tool calling — what MCP, OpenAI, and Anthropic SDKs already speak.
- No translation layer to maintain.
- LLMs write it fluently.

### Handler conventions

- **Return value**: any JSON-serializable value. The runtime serializes it.
- **Errors**: thrown errors become `tool-error::handler-error` with the message; stack traces logged to stderr but not returned.
- **Async**: handlers may be async; the runtime awaits.
- **Concurrency**: a single particle instance handles one tool call at a time.

### TypeScript

esbuild handles `.ts` files natively (syntax stripping). Particles can use `.ts` or `.js` freely. Type-checking runs as a separate phase via `particle-typecheck.wasm` (default-on, opt-out via `--no-type-check`). We ship `.d.ts` files for `particle:credentials`, `particle:oauth`, `particle:signing`, `particle:kv`, and a `Particle` / `ToolDef` types package.

### Build-time validation

The introspect step rejects the build if:

- `name` is not kebab-case
- `version` is not valid semver
- An `inputSchema` is not a valid JSON Schema
- A handler isn't a function
- A capability is imported but not declared
- An npm specifier lacks a version range
- A local import resolves outside the particle source tree
- A credential type and its declared fields don't match (see §7)
- `particle:oauth` is imported with no `oauth2` credentials declared
- `particle:signing` is imported with no `signing-key` credentials declared
- `credentials.getRaw` is called but no `raw` credentials are declared

### What's not in the DSL

- No imperative builder (`particle.tool(...)`). Declarative export is more analyzable.
- No lifecycle hooks. Each tool call is self-contained.
- No inter-tool calls. Factor into a shared local module.
- No tool middleware / interceptors.

---

## 5. Build Pipeline

### Phases

The pipeline turns a source directory into a particle tarball. Six phases, each cacheable:

```
Phase 1: import-scan          (Go)         — extract npm: specs from sources
Phase 2: resolve-and-fetch    (deno-npm.wasm) — resolve transitive deps, fetch tarballs
Phase 3: typecheck            (particle-typecheck.wasm) — tsc, default-on
Phase 4: bundle               (esbuild Go lib) — produce bundle.js
Phase 5: manifest-extract     (particle-introspect.wasm) — extract manifest from bundle
Phase 6: pack                 (Go)         — tar { manifest.json, bundle.js, lockfile, info }
```

### Phase details

**Phase 1: import-scan (Go).** Walks the source tree, parses every `.js`/`.ts` file via esbuild's Go parser API. Visits every `import` declaration and `import()` expression with a string-literal argument. Extracts npm: specifiers (each → `dep-request`); records which `particle:*` capabilities are imported.

Errors raised: bare specifier without `npm:` prefix; `npm:` specifier without version; computed `import()`; local import outside source tree.

**Phase 2: resolve-and-fetch (deno-npm.wasm).** The Go orchestrator instantiates `deno-npm.wasm` and calls `installer.resolve-and-fetch`. The component talks to the npm registry via wasi:http, walks the tree, fetches and verifies each tarball, returns the resolved tree. The orchestrator writes a virtual node_modules layout (memfs or temp dir) and generates `Particle.lock`.

Cache key: SHA-256 of the sorted dep-request list. Lockfile is consulted before re-resolving; `--frozen-lockfile` fails on mismatches.

**Phase 3: typecheck (particle-typecheck.wasm).** The Go orchestrator instantiates `particle-typecheck.wasm`, mounts source at `/src/` and resolved npm at `/node_modules/`, calls `typecheck.check`. Diagnostics returned. Build fails on any error-severity diagnostic. Skipped if `--no-type-check`.

Cache key: SHA-256 of (source-tree-hash + lockfile-hash).

**Phase 4: bundle (esbuild Go library).** Direct Go API call. Entry: `Particlefile.{js,ts}`. Format: ESM. External: `particle:*`. Custom resolver plugin: when esbuild encounters `npm:foo@range`, look up in the resolved tree and return the actual file path. Bare specifiers inside packages resolve normally via the resolved tree. Output: `bundle.js` + sourcemap + metafile.

Cache key: same as typecheck.

**Phase 5: manifest-extract (particle-introspect.wasm).** The orchestrator instantiates `particle-introspect.wasm` with `bundle.js` mounted at `/particle/bundle.js`. The component registers JS-level no-op stubs for every `particle:*` module specifier (so the bundle's `import { credentials } from "particle:credentials"` resolves without ever crossing the WIT boundary), evaluates the bundle, reads `default` export, validates structure, validates each `inputSchema` against the JSON Schema meta-schema, cross-references declared capabilities against imports actually used. Returns the manifest as JSON. No `particle:host/*` imports needed — the host wires only standard wasi:*.

Cache key: SHA-256 of bundle.js.

**Phase 6: pack (Go).** Assembles the final tarball: `manifest.json`, `bundle.js`, `Particle.lock`, `build-info.json`. For in-process callers (cloud platform), the artifact can be returned as `[]byte` or `fs.FS` instead of a tar file.

### Caching summary

| Phase             | Cache key                              |
|-------------------|----------------------------------------|
| import-scan       | source-tree-hash                       |
| resolve-and-fetch | sorted dep-request list hash           |
| typecheck         | source-tree-hash + lockfile-hash       |
| bundle            | source-tree-hash + lockfile-hash       |
| manifest-extract  | bundle-hash                            |

Trivial source edits with cached dep tree: subsecond rebuilds.

### Performance targets (informal, small particle: ~200 LOC, 2-3 deps)

| Phase             | Cold       | Warm     |
|-------------------|------------|----------|
| import-scan       | 50ms       | 50ms     |
| resolve-and-fetch | 2-5s       | <50ms    |
| typecheck         | 5-15s      | <50ms    |
| bundle            | 100-500ms  | <50ms    |
| manifest-extract  | 200-500ms  | <50ms    |
| pack              | 50ms       | 50ms     |
| **Total**         | **~10-20s cold** | **<300ms warm** |

Warm rebuilds dominate the iteration loop; cold builds dominated by typecheck + npm fetch.

### Failure modes and DX

Every phase produces structured errors with file/line where possible, phase identifier, and suggested fix when the error pattern is known. Examples:

```
particle build failed: import-scan
  Particlefile.ts:3:8
    import yaml from "npm:yaml";
                                ^
    npm specifier missing version range. Use e.g. "npm:yaml@^2.3.0".
```

```
particle build failed: typecheck
  tools/parse_yaml.ts:12:18
    handler: async ({ input }: { input: number }) => {
                              ^
    Type 'number' is not assignable to inputSchema property 'input' of type 'string'.
```

```
particle build failed: manifest-extract
  Particlefile.ts: imports "particle:credentials" but no credentials are declared.
  Add to your manifest:
    capabilities: { credentials: { /* ... */ } }
```

The orchestrator wraps phase errors uniformly so the CLI and any other consumer present them consistently.

---

## 6. Runtime Execution Model

This section describes what happens *inside* the runtime when a particle is loaded and a tool is invoked. The Go-side API surface for orchestrating this is deferred to a later design pass.

### Conceptual lifecycle

1. **Load.** Runtime image is instantiated. The host wires imports:
   - `wasi:filesystem` → virtual FS backed by the particle tarball, mounted at `/particle/`
   - `particle:credentials`, `particle:oauth`, `particle:signing`, `particle:kv` → host-provided implementations
   - `wasi:cli/environment` → host-provided impl that filters env vars per the manifest
   - `wasi:http` → host implementation that consults HTTPPolicy
   - `wasi:sockets` → host implementation that consults SocketsPolicy (deny-all when not declared)
   - `wasi:cli/stderr`, `wasi:clocks`, `wasi:random` → standard WASI

2. **Boot.** The runtime starts up:
   - Reads `/particle/bundle.js`
   - Evaluates the module
   - Reads `default` export
   - Builds an internal name → handler map for fast tool dispatch
   - Returns control to the host

3. **Serve.** The runtime exports `tools.list-tools`, `tools.call-tool`, and `health.ping`.

4. **Tear down.** The instance is disposed; memory freed.

### Handler invocation

When `call-tool(name, argsJSON)` arrives:

1. Look up handler for `name`. Not found → `tool-error::not-found`.
2. Parse `argsJSON`. Already validated host-side; runtime trusts it.
3. Call `handler(args)` in try/catch.
4. If the return value is a Promise, await it via QuickJS's job queue.
5. JSON.stringify the return value. Throws on circular/non-serializable → `tool-error::handler-error`.
6. Return the string.

If the handler throws:
- Capture `error.message` and `error.stack`
- Log full stack to wasi:cli/stderr
- Return only `tool-error::handler-error(message)` — stack stays in stderr, not in the API result

### Argument validation: host-side only

All JSON Schema validation runs in Go before the call enters WASM. The runtime trusts incoming arguments — it does not re-validate. Reasons:

- Schema validation in QuickJS adds significant per-call overhead.
- The host already has the schemas; validating there is essentially free.
- Defense-in-depth in the runtime would only protect against host bugs.

The host's validation library compiles each tool's schema once at load time and reuses the compiled validator for every subsequent call.

If validation fails, the host returns `tool-error::invalid-arguments(message)` directly — the runtime is never invoked.

### Error model

```
tool-error variant:
  not-found
    raised when:  tool name unknown
  invalid-arguments(message)
    raised when:  args fail JSON Schema (host-side, before WASM entry)
  handler-error(message)
    raised when:  JS handler throws
  capability-denied(message)
    raised when:  policy blocks an HTTP/sockets call, or credential not configured
```

`capability-denied`: when `fetch()` is blocked by HTTPPolicy, the underlying wasi:http call returns an error. The JS handler's natural reaction is to throw — which would normally surface as `handler-error`. The host adapter recognizes the host-emitted denial signal (an error with a known type/code) and remaps it to `capability-denied` so callers can distinguish "your tool blew up" from "the host blocked an action."

Same pattern for credentials: `credentials.getPlaceholder(name)` failing with `not-configured` is a denial signal, remapped at adapter level.

### Concurrency inside an instance

JavaScript is single-threaded. Within a single runtime instance, tool calls serialize. The host should treat each instance as serving one call at a time. Cross-instance concurrency is the host's responsibility (out of scope for this doc).

### Logging

- `console.log/info/warn/error` from JS routes to `wasi:cli/stderr`.
- The host captures stderr per instance.
- No structured logging in v1. Free-form text only. A `particle:log` capability for structured fields is a Phase 2 candidate.

### Adapter error mapping

| Error                    | `particle-mcp` (MCP code)              | `particle-cli` (exit + stderr)      |
|--------------------------|----------------------------------------|--------------------------------------|
| not-found                | -32601 (method not found)              | 64 (EX_USAGE)                        |
| invalid-arguments(msg)   | -32602 (invalid params), msg in data   | 64, msg + suggestion                 |
| handler-error(msg)       | -32603 (internal error), msg in data   | 1, msg                               |
| capability-denied(msg)   | -32002 (custom; access denied)         | 77 (EX_NOPERM), msg                  |

---

## 7. Credential Model

This section consolidates the credential design across types, sealing (placeholder substitution), OAuth, and setup.

### Principle 1: typed credentials, narrow access models

Each credential is one of five types. Each type has its own narrow access model. Most types use placeholder substitution at the wasi:http boundary; one type (signing-key) exposes operations only; one type (raw) is an explicit, warned-about fallback.

The credential type is declared in the manifest. The host enforces type-specific behavior — the particle author cannot bypass the model.

### Principle 2: manifest declares *what*, host captures *how*

Provider URLs, client IDs, client secrets, redirect URIs — none of these belong in source code. The manifest names credentials and describes their shape (type, flows supported, scopes); the host's setup command interactively gathers deployment-specific configuration and stores it alongside any tokens.

This makes particles portable: the same particle works against any OAuth app or API instance — dev, prod, fork, self-hosted — without source changes.

### Principle 3: targeted substitution

For substitution-based types, the host knows from the manifest + setup config exactly where each placeholder should appear. The wasi:http impl checks only that location. A placeholder appearing anywhere else is transmitted literally — never substituted. This blocks exfiltration via unintended channels.

### The five credential types

```
type           manifest declaration                 runtime API                    substitution scope
─────────────  ─────────────────────────────────  ─────────────────────────────  ──────────────────────────
basic          { type: "basic" }                  credentials.fetcher(name)      Authorization: Basic <ph>
oauth2         { type: "oauth2", flows, scopes,   credentials.fetcher(name)      Authorization: Bearer <ph>
               provider? }                        oauth.refresh(name)
apikey         { type: "apikey" }                 credentials.fetcher(name)      configured location only
signing-key    { type: "signing-key", algorithm } signing.sign(name, data)       none — operations only
raw            { type: "raw" }                    credentials.getRaw(name)       none — actual value returned
```

#### `basic`

Setup captures username + password. Storage holds both fields. The fetcher injects `Authorization: Basic <base64(user:pass)>`. Substitution scope: Authorization header only.

```js
const db = await credentials.fetcher("my_db");
const res = await db("https://db.example.com/api");
```

#### `oauth2`

Setup runs the OAuth flow and stores the bundle (access token, refresh token, expiry, scopes, provider config). The fetcher adds `Authorization: Bearer <access-token>` with transparent refresh on expiry.

```js
const gh = await credentials.fetcher("github_oauth");
const res = await gh("https://api.github.com/user");

// 401/403 recovery:
if (res.status === 401) {
  await oauth.refresh("github_oauth");
  const retry = await gh("https://api.github.com/...");
}
```

**Manifest declaration:**

```js
github_oauth: {
  type: "oauth2",
  description: "GitHub account access",
  flows: ["authorization-code-pkce", "device-code"],   // user picks at setup
  scopes: ["repo", "read:user"],                       // application requirement
  provider: "github",                                  // optional well-known hint
  required: true,
}
```

**Manifest declares:** type, description, supported flows, scopes, optional well-known provider hint.
**Setup captures:** authorizationUrl, tokenUrl, revocationUrl, clientId, clientSecret (only when flow requires; PKCE eliminates the need), chosen flow (when manifest offers multiple).

For known providers (`provider: "github"`, etc.), setup pre-fills URLs from a built-in registry; user can override (e.g., for GitHub Enterprise).

**v1 OAuth flows:** Authorization Code + PKCE (browser-based) and Device Code (headless/SSH). Client Credentials and others are deferred.

**Refresh semantics:**
- Transparent (via `credentials.fetcher`): if `now > expires_at - threshold`, refresh and substitute the new token.
- Explicit (via `oauth.refresh`): force a refresh regardless of cached expiry.
- On refresh failure: `credential-error::not-configured` with message pointing to `particle setup`.

**Concurrency:** per `(particle, credential-name)` mutex serializes refreshes.

**Re-auth on scope change:** if a manifest's scopes are not a subset of the stored token's scopes, setup forces re-auth.

**Revocation:** `particle credentials remove` calls the provider's revocation endpoint when configured, then deletes the bundle locally.

#### `apikey` — configurable-location key

Setup prompts for both the value and its location:

```
Where does this key appear in requests?
  1) Header (e.g., X-API-Key: <value>)
  2) Authorization header with scheme (e.g., Authorization: Token <value>)
  3) Query parameter (e.g., ?api_key=<value>)
> 2
Authorization scheme prefix: token
Key value: ****
```

Storage holds the key and the location config. The fetcher places it correctly at runtime.

```js
const apikey = await credentials.fetcher("my_apikey");
const res = await apikey("https://api.example.com/users");
```

Substitution scope: only at the configured location. If the JS bypasses the fetcher and constructs raw `fetch()` with a placeholder in the wrong location, no substitution happens; the request fails server-side, surfacing the bug.

#### `signing-key` — operations, never substitution

For HMAC signing, AWS SigV4, JWT signing, etc. The credential never enters JS memory; cryptographic operations happen in the host.

**Manifest:**

```js
my_hmac: {
  type: "signing-key",
  algorithm: "hmac-sha256",      // or "hmac-sha512"
  description: "Webhook signing secret",
  required: true,
}
```

**Runtime:**

```js
import { signing } from "particle:signing";

const data = new TextEncoder().encode(payload);
const signature = await signing.sign("my_hmac", data);
// signature is bytes; particle uses them per the protocol
```

**v1 supported algorithms:** `hmac-sha256`, `hmac-sha512`. RSA/ECDSA are Phase 2 (key formats and possibly passphrase-protected keys warrant separate design).

#### `raw` — explicit, warned fallback

For cases none of the above cover. Setup shows an explicit warning:

```
[5/5] my_raw_secret (Custom protocol authentication) — RAW

  ⚠  WARNING: 'raw' credentials are returned to your particle's JavaScript in their
              actual value. They will be visible to all code in the particle, including
              transitive npm dependencies. Use a more specific type (basic, oauth2,
              apikey, signing-key) where possible.

              Continue? [y/N]:
```

```js
import { credentials } from "particle:credentials";
const value = await credentials.getRaw("my_raw_secret");   // actual string
```

No substitution. Importing `getRaw` requires at least one `type: "raw"` credential declared — this makes raw access auditable: a manifest review surfaces every particle that needs it.

### Sealed credentials: the JS-side fetcher

The fetcher is a JS function provided by the runtime — a wrapper around standard `fetch()`. The WIT layer is narrow and exposes only the placeholder + an application descriptor. Request/response shapes never cross the WIT boundary.

```js
// What the user imports:
import { credentials } from "particle:credentials";

// API exposed:
credentials.fetcher(name)   // → async (url, init?) => Response (matches fetch())
credentials.getRaw(name)    // → string (raw type only)
```

`credentials.fetcher(name)` (runtime-internal, not user-written):

```js
async function fetcher(name) {
  const info = await wit.credentials.getPlaceholder(name);
  return async (url, init = {}) => {
    const decorated = applyPlaceholder(url, init, info);
    return fetch(decorated.url, decorated.init);
  };
}

function applyPlaceholder(url, init, info) {
  const headers = new Headers(init.headers);
  switch (info.apply.kind) {
    case "basic":       headers.set("Authorization", `Basic ${info.placeholder}`); break;
    case "bearer":      headers.set("Authorization", `Bearer ${info.placeholder}`); break;
    case "header":      headers.set(info.apply.name, info.placeholder); break;
    case "auth-scheme": headers.set("Authorization", `${info.apply.scheme} ${info.placeholder}`); break;
    case "query-param": {
      const u = new URL(url);
      u.searchParams.set(info.apply.name, info.placeholder);
      return { url: u.toString(), init };
    }
  }
  return { url, init: { ...init, headers } };
}
```

Standard wasi:http takes the request and substitutes the placeholder when the request leaves the host.

### Targeted substitution at the wasi:http boundary

When `getPlaceholder(name)` is called, the host stores `(instance_id, placeholder) → { credential_name, apply: <apply-spec> }`. For each placeholder issued in this instance, the host knows exactly where it should appear.

When wasi:http processes an outgoing request, it iterates over the placeholders registered for the current instance:

| apply.kind          | Location checked         | Match pattern                           | Substitution                                |
|---------------------|--------------------------|-----------------------------------------|---------------------------------------------|
| `basic`             | `Authorization` header   | exactly `Basic <placeholder>`           | replace placeholder with `base64(user:pass)`|
| `bearer`            | `Authorization` header   | exactly `Bearer <placeholder>`          | replace placeholder with current access token|
| `header(name)`      | header `<name>`          | header value equals `<placeholder>`     | replace value with actual key               |
| `auth-scheme(s)`    | `Authorization` header   | exactly `<s> <placeholder>`             | replace placeholder with actual key         |
| `query-param(name)` | URL query param `<name>` | param value equals `<placeholder>`      | replace value with actual key               |

If the match pattern doesn't fit, no substitution. The placeholder transmits literally; the server rejects the request — a clear signal the JS placed it in the wrong location.

**Why targeted substitution is more secure:**
- A malicious npm package writing `X-Steal-Token: <placeholder>` gets the literal placeholder on the wire, not the credential.
- A bug putting the placeholder in a body gets `body: __particle_cred_abc123` — debuggable, never leaks.
- Only the declared, manifest-rooted location ever resolves.

**Why it's more efficient:** O(declared credentials) per request — one constant-time lookup per credential. No per-request regex sweeps.

**OAuth refresh:** for OAuth credentials, the placeholder maps internally to the credential *name*; substitution loads the bundle, refreshes if needed, substitutes the current access token. The placeholder string itself never changes for the instance lifetime.

### Storage shape

`CredentialStore` becomes typed (not just string in/out). Conceptual shape (final Go API surface deferred):

```
StaticCredential   { Value }
BasicCredential    { Username, Password }
OAuthCredential    { Flow, AuthorizationURL, TokenURL, RevocationURL,
                     ClientID, ClientSecret, Scopes,
                     AccessToken, RefreshToken, ExpiresAt, TokenType }
ApiKeyCredential   { Key, Location: { Kind, Name? Scheme? } }
SigningKey         { Algorithm, Key }
RawCredential      { Value }
```

Backend implementations handle each type appropriately. Particle-core encapsulates OAuth-specific logic (refresh, revocation, flow execution) so backends stay simple.

### Setup UX

`particle setup <path>` walks the particle's `capabilities.credentials`, prompting for each unset `required: true` credential. Type-specific prompts:

- **basic:** username + password
- **oauth2:** flow choice (if multiple supported), provider URLs (pre-filled if `provider` hint), client ID, client secret if needed, then runs the flow
- **apikey:** location choice + value
- **signing-key:** key value
- **raw:** explicit warning + value

Setup is idempotent. Re-running shows current state and lets the user update.

### Auxiliary commands

- `particle credentials list <path>` — names + (set)/(unset)
- `particle credentials set <path> <name>` — set or update one
- `particle credentials remove <path> <name>` — delete one (calls revocation endpoint for OAuth)

### Cross-particle scope

**Per Phase 1:** credentials are per-particle. Two particles needing GitHub OAuth each have their own. Cross-particle sharing is a Phase 2 candidate — adding a `shared: true` flag is a non-breaking change.

### What's deferred

- RSA/ECDSA signing keys (need additional work around key formats).
- Body substitution for esoteric protocols (use `raw` until proven necessary).
- Per-call credential-scope narrowing.
- Cross-particle credential sharing.
- Token introspection / userinfo as particle-visible API (particles can do their own `/userinfo` fetch if needed).

---

## 8. Health / Ping

Most particles will be thin adapters over a foreign API (GitHub, Slack, OpenAI, etc.). Operators want to verify "is the particle's upstream actually reachable, with valid credentials?" without invoking a real tool that might have side effects.

### Particle-side declaration

Optional sibling to `tools` in the default export:

```js
export default {
  name: "github-tools",
  // ...
  tools: { /* ... */ },
  ping: async () => {
    try {
      const gh = await credentials.fetcher("github_oauth");
      const res = await gh("https://api.github.com/user");
      if (res.ok) return { status: "ok" };
      if (res.status === 401) return { status: "unhealthy", message: "Token rejected" };
      return { status: "degraded", message: `HTTP ${res.status}` };
    } catch (e) {
      return { status: "unhealthy", message: e.message };
    }
  },
};
```

### Why a separate interface, not a tool

- Operator concern, not LLM concern. Shouldn't appear in the MCP tools list.
- Distinct semantics: no input args, structured `status` enum, no JSON Schema.
- Different consumers: CLI exposes via `particle ping <path>`; cloud platforms wire to liveness/readiness probes.

### Behavior

- If `ping` is not declared in the particle, `health.ping` returns `not-implemented`.
- `particle ping <path>` runs it; exits 0 on `ok`, 1 on `degraded`, 2 on `unhealthy`.
- Not exposed in MCP in v1. (MCP has its own protocol-level `ping` for liveness — different concept.) Revisit if real demand emerges.

---

## 9. CLI, Scope, and Phasing

### CLI commands (Phase 1)

The `particle` CLI is a thin Go binary on `particle-core`. It wires up local-default implementations of host interfaces and exposes:

#### `particle run <path> <tool> [--flag value...]`

Build (incrementally) and invoke `<tool>` with arguments parsed from CLI flags against the tool's `inputSchema`.

- `<path>`: directory containing `Particlefile.{js,ts}`. Defaults to `.`.
- Build is implicit; warm cache is sub-second.
- Flags: `--no-type-check`, `--no-cache`, `--frozen-lockfile`.
- Output: tool result as JSON on stdout. Logs on stderr.

#### `particle test <path>`

Run the particle's declared `tests` array. Each test:
- Builds the particle
- Instantiates the runtime
- Calls the named tool with `args`
- Compares result against `expect` (deep equality) or verifies `expectError`
- Reports pass/fail with diffs

Returns exit 0 if all pass, 1 otherwise.

#### `particle setup <path>`

Walks `capabilities.credentials`, prompting for each unset `required: true` credential. Type-specific prompts (see §7).

#### `particle credentials list|set|remove <path> [<name>]`

Manage individual credentials post-setup.

#### `particle ping <path>`

Run the particle's `ping`. Exits 0/1/2 per status.

#### `particle mcp <path>`

Run the particle as an MCP server reading MCP-JSON-RPC from stdin. The entry point users wire into MCP-compatible clients:

```json
{ "mcpServers": { "yaml-tools": { "command": "particle", "args": ["mcp", "/path/to/yaml-tools"] } } }
```

#### `particle build <path> [-o <out.tar>]`

Run the build pipeline and write the tarball to disk. Useful for CI, distribution, pre-warming caches before deploy.

### Out of scope for Phase 1

- Watch mode / hot reload
- Pooling / multi-instance management surface
- Resource-limit configuration surface
- MCP resources and prompts
- Cross-platform credential storage beyond the CLI's local defaults
- Particle distribution / registry / install
- Inter-particle composition (a particle as a dep of another)
- Inline imperative DSL (`particle.tool(...)`) — declarative export only
- Bare specifier imports — `npm:` prefix mandatory
- Lifecycle hooks
- Worker threads / shared memory in particles
- Body substitution for sealed credentials
- RSA/ECDSA signing keys

### Phasing

**Phase 1 (this design):**

- `particle-runtime.wasm` (rquickjs)
- `particle-introspect.wasm` (rquickjs, build-time manifest extraction)
- `deno-npm.wasm`
- `particle-typecheck.wasm`
- `particle-core` Go library (build pipeline + runtime hosting + WIT bridge)
- `particle` CLI: `run`, `test`, `setup`, `credentials *`, `mcp`, `build`, `ping`
- Capabilities: HTTP, sockets (gated), credentials (5 types), KV, env
- OAuth flows: Authorization Code + PKCE, Device Code
- Signing-key algorithms: HMAC-SHA256, HMAC-SHA512

**Phase 2 candidates** (not promised; known follow-ups):

- Watch mode for `particle run`
- `tsgo` integration as faster typecheck backend (drop-in replacement for `particle-typecheck.wasm`)
- MCP `resources` and `prompts`
- Particle pooling and resource-limit configuration in the library API
- `particle:log` structured-logging capability
- Particle distribution mechanism (registry, install, version pinning)
- Inter-particle imports
- `--type-check` integration with editor LSP for live diagnostics
- RSA/ECDSA signing keys
- Cross-particle credential sharing
- Token introspection / userinfo as a particle-visible API
- Body substitution for sealed credentials (opt-in, path-scoped)

### Open questions to resolve in follow-up design passes

- **Go-side WASM hosting layer.** Implementation choice (wazero, wasmtime-go, custom) deferred.
- **Go-side library API surface.** Builder, Runtime, Instance, hostImpls wiring — separate design.
- **Runtime resource-limit knobs.** Memory cap, call timeout — defer.
- **Specific local-default backends in the CLI.** Implementation detail, not part of this design.
- **JSON Schema validation library** (Go side). Pick during implementation.

---

## Appendix: Glossary

- **Particle.** A unit of executable JavaScript/TypeScript that exposes a set of tools (and optionally a ping handler), packaged as a tarball runnable on the runtime.
- **Particlefile.** The conventional entry-point file (`Particlefile.{js,ts}`) of a particle; its `default` export defines the particle.
- **Tool.** A named, JSON-Schema-described function exposed by a particle.
- **Capability.** A category of host functionality (credentials, kv, env, http, sockets) that a particle declares it needs in its manifest.
- **Runtime.** `particle-runtime.wasm` — the rquickjs-based WASM Component that executes particles. One artifact, many instances.
- **Introspect component.** `particle-introspect.wasm` — a separate rquickjs-based component used at build time (Phase 5) to evaluate the bundle and extract its manifest.
- **Host.** The Go process (CLI or library consumer) that instantiates the runtime, provides capability implementations, and orchestrates building and execution.
- **Adapter.** A small layer that wraps the runtime's `tools` interface for a specific transport (`particle-mcp`, `particle-cli`).
- **Sealed credential.** A credential whose actual value is never returned to JS — only an opaque placeholder, substituted at the wasi:http boundary.
- **Fetcher.** A JS function returned by `credentials.fetcher(name)` that wraps `fetch()` to apply a credential's placeholder at the correct location for its type.
