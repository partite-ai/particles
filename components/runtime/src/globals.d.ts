/**
 * Globals provided by the wasm-rquickjs runtime that our source uses.
 *
 * `lib: ["ES2022"]` in tsconfig.json deliberately excludes DOM/Node typings
 * — we declare exactly what the QuickJS runtime gives us, no more.
 */

declare const console: {
  log: (...args: unknown[]) => void;
  info: (...args: unknown[]) => void;
  warn: (...args: unknown[]) => void;
  error: (...args: unknown[]) => void;
};
