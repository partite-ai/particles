// Generates src/lib-bundle.ts — exports three constants the typecheck
// wasm consumes:
//
//   - TS_LIB_FILES         : the `lib.*.d.ts` files TypeScript asks for
//                            when resolving --lib entries (DOM excluded)
//   - TYPES_NODE_FILES     : a filtered copy of @types/node, restricted
//                            to the Node modules wasm-rquickjs actually
//                            provides at runtime
//   - PARTICLE_GLOBALS_DTS : the type declarations for the host-provided
//                            `@partite-ai/particle-*` modules
//
// Why not import these at runtime? TypeScript ships its lib.*.d.ts files
// (and @types/node ships its module files) as plain text on disk; doing
// it via glob would tie esbuild config into the build. A small generator
// script keeps the typecheck JS source uncluttered.
//
// Why drop lib.dom.d.ts? Particles run server-side — the QuickJS
// runtime ships fetch / Request / Response / Headers / URL /
// URLSearchParams / Blob / FormData / TextEncoder / etc. via
// wasm-rquickjs's web-platform polyfills. lib.webworker.d.ts is the
// closest TS-native description of that environment without the
// 600 KB of Window/Document/HTMLElement cruft DOM brings.
//
// Why filter @types/node? Including the full package would let users
// type-check successfully against modules the runtime doesn't support
// (sea, wasi, sqlite, native worker_threads behavior, ...). Filtering
// down to `nodebuiltins.Names` (the authoritative Go-side list of
// runtime-provided modules) means `import x from "child_process"` is
// only valid if the runtime actually has it.
//
// Run from the typecheck component directory:
//   node build-lib-bundle.mjs
//
// Driven by the typecheck Makefile target.

import { readdirSync, readFileSync, writeFileSync, mkdirSync, statSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, "..", "..");

// -----------------------------------------------------------------------------
// TS_LIB_FILES — the `lib.*.d.ts` subset
// -----------------------------------------------------------------------------

const libDir = "node_modules/typescript/lib";

const tsLibFiles = readdirSync(libDir).filter((f) => {
  if (!f.startsWith("lib.") || !f.endsWith(".d.ts")) return false;
  if (f.includes("dom")) return false; // see header comment
  return true;
});

const libMap = {};
let libTotal = 0;
for (const f of tsLibFiles.sort()) {
  const data = readFileSync(join(libDir, f), "utf8");
  libMap[f] = data;
  libTotal += data.length;
}

// -----------------------------------------------------------------------------
// TYPES_NODE_FILES — filtered @types/node
// -----------------------------------------------------------------------------

// Parse the canonical Names list out of nodebuiltins.go. Single source
// of truth; if the Go list changes, the typecheck filter follows on
// the next make.
const nbGo = readFileSync(join(repoRoot, "internal/nodebuiltins/nodebuiltins.go"), "utf8");
const namesBlock = nbGo.match(/var Names = map\[string\]struct\{\}\{([\s\S]*?)\n\}/);
if (!namesBlock) throw new Error("could not locate `var Names` in nodebuiltins.go");
const nodebuiltinNames = new Set(
  [...namesBlock[1].matchAll(/"([^"]+)":/g)].map((m) => m[1]),
);

// `@types/node` files that aren't module-specific — globals,
// compatibility shims, web platform globals already provided by the
// QuickJS runtime. Always included regardless of the Names filter.
const alwaysIncludeTop = new Set([
  "globals.d.ts",
  "globals.typedarray.d.ts",
  "buffer.buffer.d.ts",
  "package.json",
  // Note: index.d.ts is regenerated below; not included verbatim.
]);
const alwaysIncludeDirs = new Set([
  "compatibility",
  "web-globals",
  "ts5.6",
  "ts5.7",
]);

// `<basename>.d.ts` is a per-module file; map its base to the
// builtin name we filter against. Most files are `<name>.d.ts`
// directly; a few have extra dots (`buffer.buffer.d.ts`,
// `inspector.generated.d.ts`) where we want to match the first
// segment.
function moduleNameFromFile(f) {
  if (!f.endsWith(".d.ts")) return null;
  const stem = f.slice(0, -".d.ts".length);
  return stem.split(".")[0];
}

const typesNodeDir = "node_modules/@types/node";
const typesNodeFiles = {};
let typesTotal = 0;

function walk(rel) {
  const abs = rel ? join(typesNodeDir, rel) : typesNodeDir;
  for (const entry of readdirSync(abs)) {
    const childRel = rel ? `${rel}/${entry}` : entry;
    const childAbs = join(abs, entry);
    const st = statSync(childAbs);
    if (st.isDirectory()) {
      // Top-level subdir filter: include if always-include or if
      // the directory name matches a supported module.
      if (rel === "") {
        if (!alwaysIncludeDirs.has(entry) && !nodebuiltinNames.has(entry)) continue;
      }
      walk(childRel);
      continue;
    }
    // File filter: only at top level. Inside an included
    // subdirectory we keep everything.
    if (rel === "") {
      if (entry === "index.d.ts") continue; // regenerated below
      if (!alwaysIncludeTop.has(entry)) {
        const mod = moduleNameFromFile(entry);
        if (!mod || !nodebuiltinNames.has(mod)) continue;
      }
    }
    const data = readFileSync(childAbs, "utf8");
    typesNodeFiles[childRel] = data;
    typesTotal += data.length;
  }
}
walk("");

// Synthesize an index.d.ts that triple-slash-references only the
// files we kept, in a stable order. Top-level files first (globals,
// compatibility refs, then modules), then sub-module entrypoints.
// Mirrors @types/node's own structure minus the unsupported refs.
function buildSyntheticIndex() {
  const refs = ["es2020", "esnext.disposable"].map(
    (lib) => `/// <reference lib="${lib}" />`,
  );
  const paths = Object.keys(typesNodeFiles)
    .filter((p) => p.endsWith(".d.ts"))
    .sort();
  for (const p of paths) {
    refs.push(`/// <reference path="${p}" />`);
  }
  return refs.join("\n") + "\n";
}
typesNodeFiles["index.d.ts"] = buildSyntheticIndex();
typesTotal += typesNodeFiles["index.d.ts"].length;

// -----------------------------------------------------------------------------
// Emit
// -----------------------------------------------------------------------------

const particleGlobals = readFileSync("src/particle-globals.d.ts", "utf8");

const out = `// Generated by build-lib-bundle.mjs. DO NOT EDIT.
//
// TS_LIB_FILES         : subset of TypeScript's bundled lib.*.d.ts
//                        files (DOM excluded).
// TYPES_NODE_FILES     : filtered @types/node — only the modules the
//                        runtime provides (per internal/nodebuiltins).
// PARTICLE_GLOBALS_DTS : @partite-ai/particle-* module declarations.
//
// All three back the typecheck CompilerHost so lib / type resolution
// works without filesystem access at runtime.
//
// eslint-disable
export const TS_LIB_FILES: Record<string, string> = ${JSON.stringify(libMap)};

export const TYPES_NODE_FILES: Record<string, string> = ${JSON.stringify(typesNodeFiles)};

export const PARTICLE_GLOBALS_DTS: string = ${JSON.stringify(particleGlobals)};
`;

mkdirSync("src", { recursive: true });
writeFileSync("src/lib-bundle.ts", out);

console.log(
  `wrote src/lib-bundle.ts:\n` +
    `  ${tsLibFiles.length} TS libs            (${(libTotal / 1024).toFixed(0)} KB)\n` +
    `  ${Object.keys(typesNodeFiles).length} @types/node entries (${(typesTotal / 1024).toFixed(0)} KB, filtered from ${nodebuiltinNames.size} names)`,
);
