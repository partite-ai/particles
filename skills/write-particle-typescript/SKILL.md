---
name: write-particle-typescript
description: Use when the user asks you to create, edit, or extend a Particle in TypeScript/JavaScript. Triggers include "write a particle", "create a particle", "Particlefile.ts/js", "make a particle for the <X> API". For Python particles (.py), use `write-particle-python` instead.
---

# Write a TypeScript/JavaScript particle

A particle is a single-file program that exposes one or more **tools**
(named operations with a JSON-Schema input) to a Particle runtime.
This skill tells you everything you need to produce one that builds
and runs.

## 1. File layout

One source file at the project root, named exactly:

- `Particlefile.ts` — preferred (full type-checking),
- `Particlefile.js` — works, but no types.

Nothing else is required. npm dependencies are declared inline with
standard `import` statements; the build resolves them. No
`package.json`, no `tsconfig.json`, no `node_modules` to manage.

## 2. The default export — the whole API

The file's **default export** is the particle declaration. Shape:

```ts
export default {
  name: "kebab-case-name",        // required, kebab-case
  description: "...",              // required, one line
  version: "0.1.0",                // required, semver

  capabilities: {
    // Outbound HTTP allow-list. Hosts not listed here are denied
    // at the wire boundary. Omit `http` entirely if the particle
    // makes no outbound requests.
    http: { allowedHosts: ["api.example.com"] },

    // Host-directory access, off by default. See "Filesystem" below.
    // filesystem: { mounts: { ... }, temp: { ... } },
  },

  // Optional. Declared per name; the user picks a method at setup.
  credentials: {
    example: {
      hosts: ["api.example.com"],   // bind substitution to these
      required: true,
      methods: {
        // see "Credential methods" below
      },
    },
  },

  // Required. Map of tool-name → { description, inputSchema, handler }.
  tools: {
    do_thing: {
      description: "What this tool does — one line, written for an LLM.",
      inputSchema: {
        type: "object",
        properties: {
          name: { type: "string", description: "..." },
        },
        required: ["name"],
      },
      handler: async ({ name }: { name: string }) => {
        return { result: `hello ${name}` };
      },
    },
  },

  // Optional. Returns liveness/readiness. Called by `particle ping`.
  ping: async () => ({ status: "ok" as const, message: "alive" }),
};
```

### Tools

- `description`: written for an LLM caller — short, action-oriented.
- `inputSchema`: JSON Schema Draft 2020-12, object-rooted. The host
  validates arguments against it *before* calling your handler, so
  you can trust the input shape inside the handler. Use `required`
  to mark mandatory fields; supply `default` for optionals.
- `handler`: sync or async. Return any JSON-serializable value.
  Throw to surface an error to the caller — the thrown message is
  what they see.

### Ping

Optional but recommended. Return shape:

```ts
{ status: "ok" | "degraded" | "unhealthy",
  message?: string,
  details?: string }
```

## 3. Runtime APIs available to handlers

These are the **only** non-stdlib things you can import. Each is a
host module provided by the runtime — they ship as type-only npm
packages, the implementation is injected at execution.

### Outbound HTTP

Use the global `fetch()`. It's the standard Web Fetch API, routed
through the host so the allow-list and credential substitution apply.

```ts
const res = await fetch("https://api.example.com/things", {
  method: "POST",
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify({ ... }),
});
if (!res.ok) throw new Error(`upstream ${res.status}`);
const data = await res.json();
```

Hosts not in `capabilities.http.allowedHosts` are denied. Plain
`fetch` does not apply credentials — use `credentials.fetcher` for
that.

### Filesystem

Off by default — a handler sees only its own bundle. Declare
`capabilities.filesystem` to get host-directory access, then read and
write with ordinary `node:fs` APIs (the runtime routes them through
`wasi:filesystem`). No capability module to import.

```ts
capabilities: {
  filesystem: {
    // Host directories the *user* maps to real paths — persistently
    // with `particle mount <particle> <name> <host-path>`, or per run
    // with `--mount <name>=<host-path>`.
    mounts: {
      data: {
        description: "Where reports are written",  // shown in the install prompt
        path: "/mnt/data",                          // absolute path inside the sandbox
        access: "readwrite",                        // "readonly" | "readwrite"
        required: true,                             // refuse to run until mapped (default false)
      },
    },
    // Scratch space the host provisions fresh each run and clears on
    // exit. Always read-write; the user never maps these.
    temp: {
      work: { description: "Scratch space", path: "/tmp/work", maxSize: "10MB" },
    },
  },
},
```

```ts
import { readFile, writeFile } from "node:fs/promises";

const input = await readFile("/mnt/data/in.json", "utf8");
await writeFile("/mnt/data/out.json", JSON.stringify(result));
```

- The handler opens the declared `path` directly; the mount *name*
  never appears inside the sandbox.
- Writing to a `readonly` mount, or reading/writing any path outside a
  declared mount, throws.
- A `required` mount the user never maps fails at **run** time, not
  build/install.
- `maxSize` is a byte count with an optional `KB`/`MB`/`GB` suffix
  (`"10MB"`); writes past it fail.

### Credentials

```ts
import { credentials } from "@partite-ai/particle-credentials";

// fetcher(name) returns a fetch-shaped function that auto-applies
// the credential at the host-configured location (header, query
// param, etc.) before the request leaves the sandbox.
const fetcher = await credentials.fetcher("example");
const res = await fetcher("https://api.example.com/me");

// Discover which method the user configured at setup ("oauth", "pat", ...).
const method = credentials.getConfiguredMethod("example"); // string | null

// Only for credentials declared with type: "raw".
const value = await credentials.getRaw("some-raw-cred");
```

`credentials.fetcher` works for `basic`, `oauth2`, and `apikey`
credentials. For `signing-key`, use `signing.sign`. For `raw`, use
`getRaw`.

### Key/value store

Scoped per-particle. Strings only — base64-encode binary if needed.

```ts
import { kv } from "@partite-ai/particle-kv";

await kv.set("cursor", "abc123");
const v = await kv.get("cursor");        // string | null
await kv.delete("cursor");
const keys = await kv.list("prefix:");    // string[]
```

### OAuth refresh

Only if you have an `oauth2` credential. The fetcher auto-refreshes
expired tokens; call this only when an upstream rejects a token you
*thought* was still valid.

```ts
import { oauth } from "@partite-ai/particle-oauth";
await oauth.refresh("example");
```

### Signing

Only for `signing-key` credentials. Key material never enters JS.

```ts
import { signing } from "@partite-ai/particle-signing";

const sig = await signing.sign("webhook-key", new TextEncoder().encode(payload));
const ok  = await signing.verify("webhook-key", data, sig);
```

## 4. Credential methods

Each credential has a `methods` map. The user picks one at setup. A
particle commonly offers multiple methods for the same provider.

### oauth2

```ts
oauth: {
  type: "oauth2",
  description: "Sign in via OAuth",
  flows: ["authorization-code", "device-code"],   // pick any of these three
  scopes: ["read", "write"],
  authorizationUrl: "https://provider/oauth/authorize",
  tokenUrl:         "https://provider/oauth/token",
  deviceAuthUrl:    "https://provider/oauth/device/code",  // required for "device-code"
},
```

Valid flow strings: `"authorization-code"`, `"authorization-code-pkce"`,
`"device-code"`.

### apikey

```ts
pat: {
  type: "apikey",
  description: "Use a personal access token",
  location: { kind: "auth-scheme", scheme: "Bearer" },
},
```

`location.kind` is one of:

- `"header"` + `name`: `<name>: <key>`
- `"auth-scheme"` + `scheme`: `Authorization: <scheme> <key>` (e.g. `Bearer`)
- `"query-param"` + `name`: appended as `?<name>=<key>`

Omit `location` entirely to let the importer prompt the user for it.

### basic

```ts
basic_auth: { type: "basic", description: "Username + password" },
```

Substitutes as `Authorization: Basic <base64>`.

### signing-key

```ts
webhook: { type: "signing-key", description: "...", algorithm: "hmac-sha256" },
```

Valid: `"hmac-sha256"`, `"hmac-sha512"`. Use via `signing.sign`.

### raw

```ts
api_key_blob: { type: "raw", description: "..." },
```

Use via `credentials.getRaw(name)`. Choose this only when you genuinely
need the raw bytes — the other types are safer because the value never
enters your code.

## 5. npm dependencies

Import any pure-ESM npm package directly:

```ts
import { z } from "zod";
import * as YAML from "yaml";
```

The build does an import-scan and resolves them automatically — no
`package.json` needed. Constraints:

- The package must be pure-JS/TS (no native bindings, no Node-only APIs).
- Browser-compatible builds work best.
- Avoid `fs`, `child_process`, etc. — they don't exist in the runtime.

## 6. Critical rules

1. **No module-scope host calls.** Calling `fetch`, `kv.get`,
   `credentials.fetcher`, etc. at the top level of your file will
   fail at build time (the introspect phase runs your module under
   trap stores). Do every host call inside a `handler` or `ping`.

   ```ts
   // ✗ Will fail the build
   const fetcher = await credentials.fetcher("github");
   export default { tools: { ... } };

   // ✓ Inside the handler
   export default {
     tools: {
       go: { handler: async (args) => {
         const fetcher = await credentials.fetcher("github");
         // ...
       }},
     },
   };
   ```

2. **Declare every host you fetch.** A request to a host missing
   from `capabilities.http.allowedHosts` is denied. The error
   surfaces as a thrown exception in the handler.

3. **`credentials.<name>.hosts` must be a subset of the HTTP
   allow-list.** The build rejects credentials bound to hosts the
   particle can't reach.

4. **`name` must be kebab-case** (lowercase, hyphens). The registry
   key is `(name, version)`.

5. **`version` must be valid semver.** `0.1.0`, `1.2.3-rc.1`, etc.

6. **Tool handlers must return JSON-serializable values.** Returning
   `undefined` is fine (becomes `null`). Returning a class instance,
   `Date`, `BigInt`, or `Map` will fail to serialize.

## 7. Workflow

After writing `Particlefile.ts`:

```sh
particle build                          # type-check, bundle, introspect, register
particle ping <name>                    # verify ping (if defined)
particle run  <name>                    # list tools
particle run  <name> <tool> --help      # show tool's flags
particle run  <name> <tool> --foo=bar   # invoke it
```

`particle build` walks the user through credential setup interactively
the first time. If the build fails, the error names the phase
(`import-scan`, `typecheck`, `bundle`, `manifest-extract`) and the
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

```ts
import { credentials } from "@partite-ai/particle-credentials";

export default {
  name: "github-lookup",
  description: "Look up GitHub repository metadata.",
  version: "0.1.0",

  capabilities: { http: { allowedHosts: ["api.github.com"] } },

  credentials: {
    github: {
      hosts: ["api.github.com"],
      required: true,
      methods: {
        pat: {
          type: "apikey",
          description: "GitHub personal access token",
          location: { kind: "auth-scheme", scheme: "Bearer" },
        },
      },
    },
  },

  tools: {
    get_repo: {
      description: "Fetch metadata for a GitHub repository.",
      inputSchema: {
        type: "object",
        properties: {
          owner: { type: "string", description: "Owner login." },
          repo:  { type: "string", description: "Repository name." },
        },
        required: ["owner", "repo"],
      },
      handler: async ({ owner, repo }: { owner: string; repo: string }) => {
        const fetcher = await credentials.fetcher("github");
        const res = await fetcher(`https://api.github.com/repos/${owner}/${repo}`, {
          headers: { Accept: "application/vnd.github+json" },
        });
        if (!res.ok) throw new Error(`GitHub ${res.status}: ${await res.text()}`);
        const r = (await res.json()) as Record<string, unknown>;
        return {
          full_name: r.full_name,
          stars:     r.stargazers_count,
          url:       r.html_url,
        };
      },
    },
  },
};
```

## 9. Quick reference

| Need | Import / call |
|---|---|
| HTTP request | `await fetch(url, init)` |
| Authenticated HTTP | `const f = await credentials.fetcher(name); await f(url, init)` |
| Read/write a mounted dir | `node:fs/promises` on the declared `path` (needs `capabilities.filesystem`) |
| Per-particle state | `kv.get / set / delete / list` |
| Force OAuth refresh | `oauth.refresh(name)` |
| HMAC sign / verify | `signing.sign(name, data) / signing.verify(name, data, sig)` |
| Discover method picked at setup | `credentials.getConfiguredMethod(name)` |
| Raw credential bytes | `credentials.getRaw(name)` |

Imports always come from the `@partite-ai/particle-<area>` packages —
do not invent other names.
