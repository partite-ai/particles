/**
 * Particle runtime — TypeScript source compiled to a single ESM file
 * consumed by `wasm-rquickjs generate-wrapper-crate`.
 *
 * The generated component exports two interfaces (see wit/runtime.wit):
 *   - particle:runtime/tools    — list-tools, call-tool   (host calls per request)
 *   - particle:runtime/health   — ping                    (operator liveness check)
 *
 * Build-time manifest extraction lives in a separate component
 * (`particle-introspect.wasm` — see components/introspect) so the two
 * artifacts can evolve independently.
 *
 * wasm-rquickjs WIT-to-JS convention:
 *   - WIT interface names → JS exports in camelCase, methods in
 *     camelCase. Async methods are awaited.
 *   - `result<T, E>` is implicit: the ok arm is the function's
 *     return value; the err arm is produced by *throwing* a value
 *     matching E (for variant E, that's `{ tag: "case-name", val: payload }`).
 */

// host-shim MUST come before the dynamic import of the user
// bundle. It registers the user-facing `particle:*` JS modules
// (credentials, oauth, signing, kv) by wrapping the WIT-imported
// `particle:host/*@0.1.0` modules — without this, a bundle that
// `import`s `@partite-ai/particle-credentials` errors out at load time.
import "./host-shim";

// -----------------------------------------------------------------------------
// Types mirroring the WIT records / variants. These only document what the
// JS side returns / throws — wasm-rquickjs handles the canonical-ABI lift.
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

function notFoundError(): ToolError {
  return { tag: "not-found" };
}

function handlerError(message: string): ToolError {
  return { tag: "handler-error", val: message };
}

type PingStatus = "ok" | "degraded" | "unhealthy";

type PingResult = {
  status: PingStatus;
  message?: string;
  details?: string;
};

type HealthError =
  | { tag: "not-implemented" }
  | { tag: "handler-error"; val: string };

function notImplementedError(): HealthError {
  return { tag: "not-implemented" };
}

function healthHandlerError(message: string): HealthError {
  return { tag: "handler-error", val: message };
}

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
// Bundle loader — reads /particle/bundle.js via dynamic import (which
// wasm-rquickjs routes through the wasi:filesystem preopens the host
// mounted before instantiation) and captures its default export.
// Cached for the instance lifetime: the bundle is immutable and a hot-
// path tool call shouldn't pay re-evaluation cost.
// -----------------------------------------------------------------------------

let cachedParticle: UserParticle | null = null;
let loadError: Error | null = null;

async function loadParticle(): Promise<UserParticle> {
  if (cachedParticle) return cachedParticle;
  if (loadError) throw loadError;

  try {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const mod = await (import("/particle/bundle.js") as Promise<any>);
    if (!mod || typeof mod.default === "undefined") {
      throw new Error("bundle has no default export");
    }
    cachedParticle = mod.default as UserParticle;
    return cachedParticle;
  } catch (e) {
    loadError = e instanceof Error ? e : new Error(String(e));
    throw loadError;
  }
}

// describeThrown turns whatever a handler threw into a useful
// string. JS's default `String(x)` on a plain object yields the
// notorious "[object Object]" — fine for `new Error("...")` but
// the typical particle-side throw is one of:
//   - an Error (use .message)
//   - a WIT variant like `{ tag: "not-configured" }` thrown by a
//     particle:* host call when the runtime maps the error back
//     into JS
//   - a string, number, boolean, null
//   - some other plain object
// Each branch produces something a human reading the log can
// act on. JSON.stringify is the catch-all so even unusual
// payloads stay debuggable.
function describeThrown(e: unknown): string {
  if (e instanceof Error) return e.message;
  if (e === null || e === undefined) return String(e);
  const t = typeof e;
  if (t === "string" || t === "number" || t === "boolean" || t === "bigint") {
    return String(e);
  }
  if (t === "object") {
    const obj = e as Record<string, unknown>;
    if (typeof obj.tag === "string") {
      // WIT variant payload: render as `tag` or `tag: val` so
      // the tag (which carries the semantic) leads.
      if (obj.val === undefined || obj.val === null) return obj.tag;
      const val = typeof obj.val === "string" ? obj.val : safeStringify(obj.val);
      return `${obj.tag}: ${val}`;
    }
    return safeStringify(obj);
  }
  return String(e);
}

function safeStringify(v: unknown): string {
  try {
    return JSON.stringify(v);
  } catch {
    // Circular / unserializable payload — fall back to
    // Object.prototype.toString.call so the log carries SOME
    // type info instead of a thrown error.
    return Object.prototype.toString.call(v);
  }
}

function errMessage(e: unknown): string {
  return describeThrown(e);
}

function logStack(e: unknown): void {
  // Stack traces go to stderr (wasi:logging at error level), never out via WIT.
  if (e instanceof Error && e.stack) {
    console.error(e.stack);
    return;
  }
  console.error(describeThrown(e));
}

// -----------------------------------------------------------------------------
// Exports
// -----------------------------------------------------------------------------

/// `particle:runtime/tools` — what the host calls for every tool invocation.
export const tools = {
  async listTools(): Promise<ToolDef[]> {
    const particle = await loadParticle();
    return Object.entries(particle.tools).map(([name, def]) => ({
      name,
      description: def.description,
      inputSchemaJson: JSON.stringify(def.inputSchema),
    }));
  },

  async callTool(name: string, argumentsJson: string): Promise<string> {
    let particle: UserParticle;
    try {
      particle = await loadParticle();
    } catch (e) {
      throw handlerError(errMessage(e));
    }

    const tool = particle.tools[name];
    if (!tool) throw notFoundError();

    // Args are pre-validated host-side against the tool's JSON Schema
    // (design doc §6 "Argument validation: host-side only"). Trust them.
    let args: unknown;
    try {
      args = JSON.parse(argumentsJson);
    } catch (e) {
      // Should not happen — host serialized it. Treat as handler
      // error rather than invalid-arguments (which is the host's
      // domain).
      throw handlerError(`argument JSON parse: ${errMessage(e)}`);
    }

    try {
      const result = await tool.handler(args);
      return JSON.stringify(result ?? null);
    } catch (e) {
      logStack(e);
      // TODO: a future host adapter can recognize denial signals
      // (HTTP policy, missing credential) and remap to
      // capability-denied. For now bare throws surface as
      // handler-error.
      throw handlerError(errMessage(e));
    }
  },
};

/// `particle:runtime/health` — operator-facing liveness check, optional.
export const health = {
  async ping(): Promise<PingResult> {
    let particle: UserParticle;
    try {
      particle = await loadParticle();
    } catch (e) {
      throw healthHandlerError(errMessage(e));
    }

    if (typeof particle.ping !== "function") {
      throw notImplementedError();
    }

    try {
      return await particle.ping();
    } catch (e) {
      logStack(e);
      throw healthHandlerError(errMessage(e));
    }
  },
};
