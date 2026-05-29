# @partite-ai/particle-signing

TypeScript types for the `@partite-ai/particle-signing` host capability
consumed by [Particle](https://github.com/partite-ai/particles) particles.

Types only — the runtime implementation lives in the Particle wasm
runtime. Key material never enters the particle's address space; sign
and verify are host calls.

```ts
import { signing } from "@partite-ai/particle-signing";

const data = new TextEncoder().encode(payload);
const sig = await signing.sign("webhook-key", data);
// post `sig` as a header / body field per the upstream's protocol
```

## API

- `signing.sign(name, data)` — host-side signature. v1 supports
  HMAC-SHA-256 and HMAC-SHA-512.
- `signing.verify(name, data, signature)` — host-side verify.

See the [Particle README](https://github.com/partite-ai/particles) for
the manifest schema.
