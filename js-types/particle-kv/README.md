# @partite-ai/particle-kv

TypeScript types for the `@partite-ai/particle-kv` built-in
consumed by [Particle](https://github.com/partite-ai/particles) particles.

Types only — the runtime implementation lives in the Particle wasm
runtime. The store is per-particle: two particles using the same key
see independent values. KV is a declared capability: add
`capabilities: { kv: { enabled: true } }` to your particle's default
export to use it. No user approval is required — the manifest
declaration is the sole gate; an undeclared particle's kv calls fail
with a `not-declared` error.

```ts
import { kv } from "@partite-ai/particle-kv";

await kv.set("last-run", new Date().toISOString());
const last = await kv.get("last-run"); // string | null
const keys = await kv.list("user:");   // string[]
```

## API

- `kv.get(key)` — `string | null`
- `kv.set(key, value)` — replace or create
- `kv.delete(key)` — idempotent
- `kv.list(prefix)` — keys with that prefix, unspecified order

See the [Particle README](https://github.com/partite-ai/particles) for
the manifest schema.
