/**
 * Particle runtime — TypeScript source compiled to a single ESM file
 * consumed by `wasm-rquickjs generate-wrapper-crate`.
 *
 * The generated component exports three interfaces (see wit/runtime.wit):
 *   - particle:host/tools       — list-tools, call-tool   (host calls per request)
 *   - particle:host/health      — ping                    (operator liveness check)
 *   - particle:host/introspect  — manifest                (build-time, Phase 5)
 *
 * Naming convention (from wasm-rquickjs README): WIT interface names → JS
 * exports in camelCase, methods in camelCase. Async methods are awaited.
 *
 * This file is a SKELETON. The bundle-loading and handler-dispatch logic is
 * stubbed; what's locked down is the export shape, the WIT-bridged tool/error
 * types, and the boot lifecycle (lazy-load on first call, cache result).
 */

// -----------------------------------------------------------------------------
// Types mirroring the WIT records/variants. These match what wasm-rquickjs
// produces from the WIT bindings — see the type-mapping table in its README.
// -----------------------------------------------------------------------------

type ToolDef = {
  name: string;
  description: string;
  inputSchemaJson: string;
};

type ToolError =
  | { tag: "not-found" }
  | { tag: "invalid-arguments"; val: string }
  | { tag: "handler-error"; val: string }
  | { tag: "capability-denied"; val: string };

type Result<T, E> = { tag: "ok"; val: T } | { tag: "err"; val: E };

type PingStatus = "ok" | "degraded" | "unhealthy";

type PingResult = {
  status: PingStatus;
  message?: string;
  details?: string;
};

type HealthError =
  | { tag: "not-implemented" }
  | { tag: "handler-error"; val: string };

type IntrospectError =
  | { tag: "bundle-load-error"; val: string }
  | { tag: "invalid-manifest"; val: string };

// -----------------------------------------------------------------------------
// Particle module shape — what the user's bundle.js default-exports.
// Mirrors the type definitions in docs/initial-design.md §4.
// -----------------------------------------------------------------------------

type Handler = (args: unknown) => unknown | Promise<unknown>;

type UserToolDef = {
  description: string;
  inputSchema: object;
  handler: Handler;
};

type UserParticle = {
  name: string;
  description: string;
  version: string;
  capabilities: Record<string, unknown>;
  tools: Record<string, UserToolDef>;
  ping?: () => PingResult | Promise<PingResult>;
};

// -----------------------------------------------------------------------------
// Bundle loader — reads /particle/bundle.js via wasi:filesystem and evaluates
// it. Cached per-instance: the bundle is immutable for the instance lifetime.
//
// STUB: the wasi:filesystem dance is non-trivial (descriptor walking,
// stream reads). For now, throw with a clear message. The shape — async,
// idempotent, returns the parsed default export — is the contract the rest
// of this file builds on.
// -----------------------------------------------------------------------------

let cachedParticle: UserParticle | null = null;
let loadError: Error | null = null;

async function loadParticle(): Promise<UserParticle> {
  if (cachedParticle) return cachedParticle;
  if (loadError) throw loadError;

  try {
    // TODO: read /particle/bundle.js (the host mounts the particle tarball
    // before instantiating us) via wasm-rquickjs's `node:fs` shim, evaluate
    // the module, and capture its default export. Sketch:
    //   const { readFile } = await import("node:fs/promises");
    //   const code = await readFile("/particle/bundle.js", "utf8");
    //   // esbuild produces ESM; need a way to evaluate it as a module and
    //   // grab its default export. QuickJS-side approach to confirm —
    //   // likely `eval` won't suffice for ESM and we'll want a synthetic
    //   // module URL or a small CJS-flavoured bundle output instead.
    //   cachedParticle = bundle.default;
    throw new Error(
      "particle bundle loader not yet implemented — see runtime.ts loadParticle()",
    );
  } catch (e) {
    loadError = e instanceof Error ? e : new Error(String(e));
    throw loadError;
  }
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

function errMessage(e: unknown): string {
  if (e instanceof Error) return e.message;
  return String(e);
}

function logStack(e: unknown): void {
  // Stack traces go to stderr (wasi:logging at error level), never out via WIT.
  if (e instanceof Error && e.stack) {
    console.error(e.stack);
  } else {
    console.error(String(e));
  }
}

// -----------------------------------------------------------------------------
// Exports
// -----------------------------------------------------------------------------

/// `particle:host/tools` — what the host calls for every tool invocation.
export const tools = {
  async listTools(): Promise<ToolDef[]> {
    const particle = await loadParticle();
    return Object.entries(particle.tools).map(([name, def]) => ({
      name,
      description: def.description,
      inputSchemaJson: JSON.stringify(def.inputSchema),
    }));
  },

  async callTool(
    name: string,
    argumentsJson: string,
  ): Promise<Result<string, ToolError>> {
    let particle: UserParticle;
    try {
      particle = await loadParticle();
    } catch (e) {
      return { tag: "err", val: { tag: "handler-error", val: errMessage(e) } };
    }

    const tool = particle.tools[name];
    if (!tool) return { tag: "err", val: { tag: "not-found" } };

    // Args are pre-validated host-side against the tool's JSON Schema
    // (design doc §6 "Argument validation: host-side only"). Trust them.
    let args: unknown;
    try {
      args = JSON.parse(argumentsJson);
    } catch (e) {
      // Should not happen — host serialized it. Treat as handler error
      // rather than invalid-arguments (which is the host's domain).
      return {
        tag: "err",
        val: { tag: "handler-error", val: `argument JSON parse: ${errMessage(e)}` },
      };
    }

    try {
      const result = await tool.handler(args);
      return { tag: "ok", val: JSON.stringify(result ?? null) };
    } catch (e) {
      logStack(e);
      // TODO: the host adapter remaps host-emitted denial signals (HTTP
      // policy, missing credential) to capability-denied; bare throws
      // surface here as handler-error.
      return {
        tag: "err",
        val: { tag: "handler-error", val: errMessage(e) },
      };
    }
  },
};

/// `particle:host/health` — operator-facing liveness check, optional.
export const health = {
  async ping(): Promise<Result<PingResult, HealthError>> {
    let particle: UserParticle;
    try {
      particle = await loadParticle();
    } catch (e) {
      return {
        tag: "err",
        val: { tag: "handler-error", val: errMessage(e) },
      };
    }

    if (typeof particle.ping !== "function") {
      return { tag: "err", val: { tag: "not-implemented" } };
    }

    try {
      const result = await particle.ping();
      return { tag: "ok", val: result };
    } catch (e) {
      logStack(e);
      return {
        tag: "err",
        val: { tag: "handler-error", val: errMessage(e) },
      };
    }
  },
};

/// `particle:host/introspect` — build-time manifest extraction (Phase 5).
/// Run with no-op stubs wired to every capability; only reads the default
/// export's metadata, never invokes a handler.
export const introspect = {
  async manifest(): Promise<Result<string, IntrospectError>> {
    let particle: UserParticle;
    try {
      particle = await loadParticle();
    } catch (e) {
      return {
        tag: "err",
        val: { tag: "bundle-load-error", val: errMessage(e) },
      };
    }

    try {
      const manifest = {
        name: particle.name,
        description: particle.description,
        version: particle.version,
        capabilities: particle.capabilities,
        tools: Object.entries(particle.tools).map(([name, def]) => ({
          name,
          description: def.description,
          inputSchema: def.inputSchema,
        })),
      };
      return { tag: "ok", val: JSON.stringify(manifest) };
    } catch (e) {
      return {
        tag: "err",
        val: { tag: "invalid-manifest", val: errMessage(e) },
      };
    }
  },
};
