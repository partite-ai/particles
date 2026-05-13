/**
 * particle-typecheck — runs the bundled TypeScript compiler against the
 * particle source tree and resolved node_modules (mounted by the host at
 * /src/ and /node_modules/), and returns flat diagnostics.
 *
 * Compiled to a single ESM bundle by esbuild (with `typescript` inlined),
 * then wrapped into a WASI Preview 2 component by wasm-rquickjs.
 *
 * Skeleton: the structure, enum mappings, and diagnostic shape are real;
 * the host's CompilerHost is the default `ts.createCompilerHost` which
 * routes through `ts.sys` → wasm-rquickjs's `node:fs` shims → wasi:filesystem.
 */

// `./polyfill` MUST come before `typescript` — it sets `__filename` /
// `__dirname` globals that typescript's CommonJS bundle reads at
// module-init time. ESM hoists all imports, but execution order
// follows source order: polyfill runs, then typescript loads.
import "./polyfill";
import ts from "typescript";
import { TS_LIB_FILES, PARTICLE_GLOBALS_DTS } from "./lib-bundle";

// Synthetic path the libBundleHost serves the @partite-ai/particle-* module
// declarations from. The leading "/" keeps it absolute (TS
// canonicalizes paths and a relative one would resolve against
// CWD, which doesn't exist in the wasm sandbox).
const PARTICLE_GLOBALS_PATH = "/__particle_globals__.d.ts";

type Severity = "error" | "warning" | "info";

type Diagnostic = {
  file: string;
  line: number;
  column: number;
  severity: Severity;
  code: number;
  message: string;
};

type CheckOptions = {
  rootFiles: string[];
  strict: boolean;
  target: string;
};

type CheckError =
  | { tag: "config-error"; val: string }
  | { tag: "internal-error"; val: string };

// wasm-rquickjs convention for `result<T, E>`: return T directly for
// the ok arm; throw a value matching E for the err arm. There is no
// `{ tag: "ok" | "err", val: ... }` wrapping.
function configError(message: string): CheckError {
  return { tag: "config-error", val: message };
}

function internalError(message: string): CheckError {
  return { tag: "internal-error", val: message };
}

// -----------------------------------------------------------------------------
// Mappings
// -----------------------------------------------------------------------------

function severityOf(category: ts.DiagnosticCategory): Severity {
  switch (category) {
    case ts.DiagnosticCategory.Error:
      return "error";
    case ts.DiagnosticCategory.Warning:
      return "warning";
    // Suggestion and Message both map to info — neither blocks the build.
    case ts.DiagnosticCategory.Suggestion:
    case ts.DiagnosticCategory.Message:
      return "info";
    default:
      return "info";
  }
}

/// Map a string like "ES2022" / "esnext" / "es5" to the matching
/// `ts.ScriptTarget` enum value. Case-insensitive.
function scriptTargetOf(name: string): ts.ScriptTarget | undefined {
  const norm = name.toLowerCase();
  for (const key of Object.keys(ts.ScriptTarget)) {
    if (key.toLowerCase() === norm) {
      // ts.ScriptTarget is a numeric enum; the string keys map to numbers.
      const value = (ts.ScriptTarget as unknown as Record<string, number>)[key];
      if (typeof value === "number") return value;
    }
  }
  return undefined;
}

// -----------------------------------------------------------------------------
// check()
// -----------------------------------------------------------------------------

export const typecheck = {
  check(opts: CheckOptions): Diagnostic[] {
    const target = scriptTargetOf(opts.target);
    if (target === undefined) {
      throw configError(`unknown TypeScript target: ${opts.target}`);
    }

    const compilerOptions: ts.CompilerOptions = {
      strict: opts.strict,
      target,
      // Explicit lib. Particles run in QuickJS-on-WASM with the
      // wasm-rquickjs web-platform polyfills (fetch, Request,
      // Response, Headers, URL, URLSearchParams, Blob, FormData,
      // TextEncoder/Decoder, AbortController, ReadableStream,
      // crypto.subtle). lib.webworker.d.ts is the closest TS-native
      // description of that environment — no Window/Document
      // cruft, but every web-fetch type a server-side particle
      // actually uses.
      lib: [
        "lib.es2022.d.ts",
        "lib.webworker.d.ts",
        "lib.webworker.iterable.d.ts",
        "lib.webworker.asynciterable.d.ts",
      ],
      module: ts.ModuleKind.ESNext,
      moduleResolution: ts.ModuleResolutionKind.Bundler,
      noEmit: true,
      skipLibCheck: true,
      allowJs: true,
      esModuleInterop: true,
      isolatedModules: true,
    };

    let program: ts.Program;
    try {
      const host = libBundleHost(compilerOptions);
      program = ts.createProgram({
        // Inject @partite-ai/particle-* module declarations as a synthetic
        // root so users get type-checking on `import { credentials
        // } from "@partite-ai/particle-credentials"` without an external
        // types package. libBundleHost serves the file's contents.
        rootNames: [...opts.rootFiles, PARTICLE_GLOBALS_PATH],
        options: compilerOptions,
        host,
      });
    } catch (e) {
      throw internalError(`program creation failed: ${errMessage(e)}`);
    }

    let raw: readonly ts.Diagnostic[];
    try {
      raw = [
        ...program.getConfigFileParsingDiagnostics(),
        ...program.getOptionsDiagnostics(),
        ...program.getGlobalDiagnostics(),
        ...program.getSyntacticDiagnostics(),
        ...program.getSemanticDiagnostics(),
      ];
    } catch (e) {
      throw internalError(`diagnostic collection failed: ${errMessage(e)}`);
    }

    return raw
      .filter(suppressNpmImplicitAny)
      .map((d) => {
        let line = 0;
        let column = 0;
        if (d.file && typeof d.start === "number") {
          const pos = d.file.getLineAndCharacterOfPosition(d.start);
          line = pos.line + 1; // TS is 0-based; design doc reports 1-based.
          column = pos.character + 1;
        }
        return {
          file: d.file?.fileName ?? "",
          line,
          column,
          severity: severityOf(d.category),
          code: d.code,
          message: ts.flattenDiagnosticMessageText(d.messageText, "\n"),
        };
      });
  },
};

// suppressNpmImplicitAny drops TS7016 ("Could not find a declaration
// file for module X.  '<path>' implicitly has an 'any' type.") when X
// is an `npm:` specifier. Reasoning: npm packages routinely ship no
// `.d.ts` files; particle authors can't be expected to write declaration
// files for every untyped dep. We let those imports be `any` silently —
// TypeScript still flags implicit-any in the user's own code, just not
// at the boundary with untyped npm packages.
//
// Mirrors how Deno and modern bundlers handle the same case.
function suppressNpmImplicitAny(d: ts.Diagnostic): boolean {
  if (d.code !== 7016) return true;
  const msg = ts.flattenDiagnosticMessageText(d.messageText, "\n");
  // The message embeds the original specifier in single quotes.
  return !msg.includes("'npm:");
}

function errMessage(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

// libBundleHost wraps the default CompilerHost with two pieces of
// particle-specific behavior:
//
//  1. lib.*.d.ts files are served from TS_LIB_FILES (bundled into the
//     wasm at build time) instead of the wasi:filesystem preopens.
//     TypeScript's `getDefaultLibFilePath` returns paths like
//     `/typecheck.js/../lib/lib.es2022.d.ts` which would otherwise
//     need to live on the host's filesystem.
//
//  2. `npm:foo@range[/sub]` import specifiers are rewritten to bare
//     `foo[/sub]` before being handed to TypeScript's standard
//     resolver. The bundle phase rewrites the same way at JS-emit time
//     (see internal/bundle); without the type-side rewrite TypeScript
//     reports `Cannot find module 'npm:foo@range'` for every npm import.
//
// Everything else delegates to the default host.
function libBundleHost(opts: ts.CompilerOptions): ts.CompilerHost {
  const base = ts.createCompilerHost(opts);
  const lookup = (p: string): string | undefined => {
    if (p === PARTICLE_GLOBALS_PATH) return PARTICLE_GLOBALS_DTS;
    const slash = p.lastIndexOf("/");
    const basename = slash >= 0 ? p.slice(slash + 1) : p;
    return TS_LIB_FILES[basename];
  };
  return {
    ...base,
    fileExists(p: string): boolean {
      if (lookup(p) !== undefined) return true;
      return base.fileExists(p);
    },
    readFile(p: string): string | undefined {
      const bundled = lookup(p);
      if (bundled !== undefined) return bundled;
      return base.readFile(p);
    },
    getSourceFile(
      fileName: string,
      languageVersionOrOptions: ts.ScriptTarget | ts.CreateSourceFileOptions,
      onError?: (message: string) => void,
      shouldCreateNewSourceFile?: boolean,
    ): ts.SourceFile | undefined {
      const bundled = lookup(fileName);
      if (bundled !== undefined) {
        const target =
          typeof languageVersionOrOptions === "object"
            ? languageVersionOrOptions.languageVersion
            : languageVersionOrOptions;
        return ts.createSourceFile(fileName, bundled, target, /*setParentNodes*/ false);
      }
      return base.getSourceFile(
        fileName,
        languageVersionOrOptions,
        onError,
        shouldCreateNewSourceFile,
      );
    },
    resolveModuleNameLiterals(
      moduleLiterals: readonly ts.StringLiteralLike[],
      containingFile: string,
      _redirectedReference: ts.ResolvedProjectReference | undefined,
      options: ts.CompilerOptions,
      _containingSourceFile: ts.SourceFile,
      _reusedNames: readonly ts.StringLiteralLike[] | undefined,
    ): readonly ts.ResolvedModuleWithFailedLookupLocations[] {
      return moduleLiterals.map((lit) => {
        const cleaned = stripNpmPrefix(lit.text);
        return ts.resolveModuleName(cleaned, containingFile, options, base);
      });
    },
  };
}

// stripNpmPrefix maps `npm:foo@range[/sub]` → `foo[/sub]`, leaving any
// other specifier untouched. Mirrors the rewrite the bundle phase does
// in internal/bundle for runtime resolution.
//
//   "npm:lodash@^4.17.0"      → "lodash"
//   "npm:lodash@^4.17.0/get"  → "lodash/get"
//   "npm:@scope/pkg@1"        → "@scope/pkg"
//   "npm:@scope/pkg@1/sub"    → "@scope/pkg/sub"
//   "npm:foo"                 → "foo"   (missing version is rejected
//                                        earlier in the import-scan
//                                        phase; we only see well-formed
//                                        specifiers here)
function stripNpmPrefix(spec: string): string {
  if (!spec.startsWith("npm:")) return spec;
  const s = spec.slice(4);
  if (s.length === 0) return spec;

  // Find the end of the package name (handles @scope/name).
  let nameEnd: number;
  if (s.startsWith("@")) {
    const firstSlash = s.indexOf("/", 1);
    if (firstSlash < 0) return s; // malformed; let TS surface it
    const afterScope = firstSlash + 1;
    const next = nextDelim(s, afterScope);
    nameEnd = next < 0 ? s.length : next;
  } else {
    const next = nextDelim(s, 0);
    nameEnd = next < 0 ? s.length : next;
  }

  const name = s.slice(0, nameEnd);
  const rest = s.slice(nameEnd);

  // rest is either empty, "/<sub>", or "@<version>[/<sub>]".
  if (rest.startsWith("@")) {
    const slash = rest.indexOf("/");
    return slash < 0 ? name : name + rest.slice(slash);
  }
  return name + rest;
}

// nextDelim returns the index of the first '@' or '/' in s at/after
// `from`, or -1 if none.
function nextDelim(s: string, from: number): number {
  for (let i = from; i < s.length; i++) {
    const c = s[i];
    if (c === "@" || c === "/") return i;
  }
  return -1;
}
