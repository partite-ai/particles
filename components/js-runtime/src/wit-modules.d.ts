/**
 * Ambient declarations for the WIT-imported modules our source touches.
 *
 * `wasm-rquickjs generate-dts --wit wit/ --output build/types` will emit
 * full `.d.ts` files for every interface in our WIT world; once that's
 * wired into the build, this file should be replaced with a triple-slash
 * reference to the generated types. For now we declare just the named
 * exports the host-shim consumes — typed loosely so the shim's adapter
 * code is the one place that knows the actual shapes.
 *
 * Note: we do NOT declare `wasi:*` modules here. The wasm-rquickjs engine
 * adds those imports based on which JS APIs we use (fetch, console,
 * node:fs, etc.) — they're not part of our app-level surface.
 */

declare module "particle:host/credentials@0.1.0" {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  export function getPlaceholder(name: string): any;
  export function getRaw(name: string): string;
  export function getConfiguredMethod(): string | undefined;
}

declare module "particle:host/oauth@0.1.0" {
  export function refresh(name: string): void;
}

declare module "particle:host/signing@0.1.0" {
  export function sign(name: string, data: Uint8Array): Uint8Array;
  export function verify(name: string, data: Uint8Array, signature: Uint8Array): boolean;
}

declare module "particle:host/kv@0.1.0" {
  export function get(key: string): string | undefined;
  export function set(key: string, value: string): void;
  // `delete` would be a reserved word as an import name; rebind.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const del: any;
  export { del as delete };
  export function list(prefix: string): string[];
}

// Env vars come from standard WASI (`wasi:cli/environment`), exposed as
// `process.env` by the QuickJS engine. No particle:host interface for them.

// node:fs (and friends) come from wasm-rquickjs's Node-compat shims and
// transitively pull in wasi:filesystem on the engine side.
declare module "node:fs/promises" {
  export function readFile(
    path: string,
    options?: { encoding?: string } | string,
  ): Promise<string | Uint8Array>;
}
