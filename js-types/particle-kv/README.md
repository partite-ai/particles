# @partite-ai/particle-kv

TypeScript types for the `@partite-ai/particle-kv` built-in
consumed by [Particle](https://github.com/partite-ai/particles) particles.

Types only — the runtime implementation lives in the Particle wasm
runtime. The store is per-particle: two particles using the same key
see independent values. Unlike credentials or HTTP, KV is not a
capability you declare — every particle gets a key/value store
unconditionally.

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
