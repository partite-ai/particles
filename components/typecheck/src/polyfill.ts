/**
 * Pre-import shim. This file runs before `import ts from "typescript"`
 * so its side effects are in place when the typescript module's top-
 * level initializers fire.
 *
 * TypeScript's bundled CommonJS reads Node.js globals (`__filename`,
 * `__dirname`, `process`) at module load time — e.g. inside
 * `getNodeSystem`'s case-sensitivity probe (typescript.js:8800). The
 * wasm-rquickjs node-compat shim covers `process` but not the path
 * globals; we provide benign defaults so the typescript module loads.
 *
 * No exports: the side effects above are the entire point.
 */

// eslint-disable-next-line @typescript-eslint/no-explicit-any
const g = globalThis as any;
if (typeof g.__filename === "undefined") g.__filename = "/typecheck.js";
if (typeof g.__dirname === "undefined") g.__dirname = "/";

export {};
