# @partite-ai/particle-oauth

TypeScript types for the `@partite-ai/particle-oauth` host capability
consumed by [Particle](https://github.com/partite-ai/particle) particles.

Types only — the runtime implementation lives in the Particle wasm
runtime.

```ts
import { credentials } from "@partite-ai/particle-credentials";
import { oauth } from "@partite-ai/particle-oauth";

const fetcher = await credentials.fetcher("github");
let res = await fetcher("https://api.github.com/user");
if (res.status === 401) {
  await oauth.refresh("github");
  res = await fetcher("https://api.github.com/user");
}
```

## API

- `oauth.refresh(name)` — force-refresh the access token regardless of
  cached expiry. The next `credentials.fetcher` call uses the rotated
  token.

See the [Particle README](https://github.com/partite-ai/particle) for
the manifest schema.
