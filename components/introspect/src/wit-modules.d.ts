/**
 * Ambient declarations for non-DOM modules our source touches.
 *
 * Unlike the runtime, the introspect component does NOT import any
 * `particle:host/*` interface — it provides JS-level no-op stubs for
 * every `particle:*` module instead, so the user's bundle resolves
 * without crossing the WIT boundary. See introspect.ts for details.
 *
 * What we do need: the node:fs/promises shim that wasm-rquickjs surfaces
 * over wasi:filesystem, used by loadParticle() to read /particle/bundle.js.
 */

declare module "node:fs/promises" {
  export function readFile(
    path: string,
    options?: { encoding?: string } | string,
  ): Promise<string | Uint8Array>;
}
