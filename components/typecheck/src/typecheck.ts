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

import ts from "typescript";

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

type Result<T, E> = { tag: "ok"; val: T } | { tag: "err"; val: E };

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
  check(opts: CheckOptions): Result<Diagnostic[], CheckError> {
    const target = scriptTargetOf(opts.target);
    if (target === undefined) {
      return {
        tag: "err",
        val: {
          tag: "config-error",
          val: `unknown TypeScript target: ${opts.target}`,
        },
      };
    }

    const compilerOptions: ts.CompilerOptions = {
      strict: opts.strict,
      target,
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
      const host = ts.createCompilerHost(compilerOptions);
      program = ts.createProgram({
        rootNames: opts.rootFiles,
        options: compilerOptions,
        host,
      });
    } catch (e) {
      return {
        tag: "err",
        val: {
          tag: "internal-error",
          val: `program creation failed: ${errMessage(e)}`,
        },
      };
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
      return {
        tag: "err",
        val: {
          tag: "internal-error",
          val: `diagnostic collection failed: ${errMessage(e)}`,
        },
      };
    }

    const diagnostics: Diagnostic[] = raw.map((d) => {
      let line = 0;
      let column = 0;
      if (d.file && typeof d.start === "number") {
        const pos = d.file.getLineAndCharacterOfPosition(d.start);
        line = pos.line + 1;       // TS is 0-based; design doc reports 1-based.
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

    return { tag: "ok", val: diagnostics };
  },
};

function errMessage(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}
