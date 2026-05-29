# @partite-ai/particle

TypeScript types for the [Particle](https://github.com/partite-ai/particles)
manifest — the `Particle` value a `Particlefile.{ts,js}` default-exports.

This package ships **types only**. The `Particle` interface is consumed
with `import type`, so nothing is loaded at runtime.

```ts
import type { Particle } from "@partite-ai/particle";

export default {
  name: "yaml-tools",
  description: "Parse and format YAML",
  version: "0.1.0",
  capabilities: {},
  tools: {
    parse: {
      description: "Parse YAML to JSON",
      inputSchema: {
        type: "object",
        properties: { input: { type: "string" } },
        required: ["input"],
      },
      handler: async ({ input }: { input: string }) => ({ result: input }),
    },
  },
} satisfies Particle;
```

`satisfies Particle` type-checks the manifest's structure while keeping the
literal types of each field. Schema-derived argument typing is left to the
author — annotate each handler's parameter explicitly (as above).

See the [Particle README](https://github.com/partite-ai/particles) for the
full manifest model and the host capability packages
(`@partite-ai/particle-credentials`, `-oauth`, `-signing`, `-kv`).
