/**
 * Ambient declarations for the WIT-imported modules our source touches.
 *
 * `wasm-rquickjs generate-dts --wit wit/ --output build/types` will emit
 * full `.d.ts` files for every interface in our WIT world; once that's
 * wired into the build, this file should be replaced with a triple-slash
 * reference to the generated types. For now, declared as `any` so the
 * stubs compile without that codegen step.
 *
 * Note: we do NOT declare `wasi:*` modules here. The wasm-rquickjs engine
 * adds those imports based on which JS APIs we use (fetch, console,
 * node:fs, etc.) — they're not part of our app-level surface.
 */

declare module "particle:host/credentials@0.1.0" {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const credentials: any;
  export = credentials;
}

declare module "particle:host/oauth@0.1.0" {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const oauth: any;
  export = oauth;
}

declare module "particle:host/signing@0.1.0" {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const signing: any;
  export = signing;
}

declare module "particle:host/kv@0.1.0" {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const kv: any;
  export = kv;
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
