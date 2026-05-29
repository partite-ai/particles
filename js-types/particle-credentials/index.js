// This package ships TypeScript types only. The runtime
// implementation is provided by the Particle wasm runtime at
// execution time via wasm-rquickjs's module-mock mechanism — see
// components/runtime/src/host-shim.ts in the Particle repo.
//
// Anything that loads this file outside that runtime (a plain
// Node import, a non-Particle bundler) hits this throw, which
// makes the misuse loud rather than silently shipping a no-op.
const message =
  "@partite-ai/particle-credentials is only callable inside the Particle runtime. " +
  "Run your particle with `particle run` / `particle serve-mcp`, or build it into a " +
  ".particle artifact.";

throw new Error(message);
