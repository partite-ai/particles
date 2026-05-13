// This package ships TypeScript types only. The runtime
// implementation is provided by the Particle wasm runtime — see
// components/runtime/src/host-shim.ts in the Particle repo.
// Loading this file outside that runtime throws so the misuse
// is loud rather than silent.
throw new Error(
  "@partite-ai/particle-signing is only callable inside the Particle runtime. " +
    "Run your particle with `particle run` / `particle serve-mcp`, or build it into " +
    "a .particle artifact.",
);
