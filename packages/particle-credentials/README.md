# @partite-ai/particle-credentials

TypeScript types for the `@partite-ai/particle-credentials` host
capability consumed by [Particle](https://github.com/partite-ai/particles)
particles.

This package ships **types only**. The runtime implementation is
provided by the Particle wasm runtime; outside that runtime
`require`-ing the package throws.

```ts
import { credentials } from "@partite-ai/particle-credentials";

const method = credentials.getConfiguredMethod();
if (method === null) throw new Error("unauthenticated");

const fetcher = await credentials.fetcher(method);
const res = await fetcher("https://api.example.com/me");
```

## API

- `credentials.fetcher(name)` — fetch-shaped wrapper bound to a
  `basic` / `oauth2` / `apikey` credential. The host substitutes the
  real value at the wasi:http boundary; the JS handler only ever
  sees an opaque placeholder.
- `credentials.getRaw(name)` — raw value of a `raw`-typed credential.
- `credentials.getConfiguredMethod()` — name of the method the user
  picked at setup, or `null` when no method is configured.

See the [Particle README](https://github.com/partite-ai/particles) for
the manifest schema and the broader credential model.
