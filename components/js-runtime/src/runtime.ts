/**
 * Particle JS runtime — TypeScript source compiled to a single ESM file
 * consumed by `wasm-rquickjs generate-wrapper-crate`.
 *
 * The generated component exports three interfaces (see wit/runtime.wit):
 *   - particle:runtime/tools     — list-tools, call-tool   (host calls per request)
 *   - particle:runtime/health    — ping                    (operator liveness check)
 *   - particle:runtime/manifest  — get-manifest            (typed self-description)
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
  runtime?: "js" | "python";
  capabilities: Record<string, unknown>;
  credentials?: Record<string, unknown>;
  tools: Record<string, UserToolDef>;
  ping?: () => PingResult | Promise<PingResult>;
};

type ManifestError =
  | { tag: "bundle-load-error"; val: string }
  | { tag: "invalid-manifest"; val: string };

function bundleLoadError(message: string): ManifestError {
  return { tag: "bundle-load-error", val: message };
}

function invalidManifest(message: string): ManifestError {
  return { tag: "invalid-manifest", val: message };
}

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
    // describeThrown handles the common JS surface forms — Error,
    // WIT-variant `{ tag, val }`, primitives — so the original
    // diagnostic isn't reduced to "[object Object]" when a
    // particle:host call throws during module evaluation (which
    // is exactly what the introspect-mode trap stores produce).
    loadError = e instanceof Error ? e : new Error(describeThrown(e));
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

/// `particle:runtime/manifest` — returns the particle's self-description
/// as a typed WIT record. Every particle exposes its metadata through
/// this export: the build pipeline calls it in Phase 5, a runtime host
/// calls it at instantiate-time to learn which credentials to provision,
/// and any future fully-WASM particle answers the same way.
///
/// Reuses the same loadParticle() cache as tools/health — a typical
/// session calls get-manifest exactly once (at build time) and then
/// list-tools + call-tool for the lifetime of the instance.
export const manifest = {
  async getManifest(): Promise<ParticleManifestRecord> {
    let particle: UserParticle;
    try {
      particle = await loadParticle();
    } catch (e) {
      throw bundleLoadError(errMessage(e));
    }
    try {
      return buildManifestRecord(particle);
    } catch (e) {
      throw invalidManifest(errMessage(e));
    }
  },
};

// -----------------------------------------------------------------------------
// Typed manifest record shape (mirrors the WIT records in
// components/js-runtime/wit/runtime.wit). wasm-rquickjs lifts these JS
// shapes into the WIT canonical ABI:
//   - records → plain objects with camelCased fields (kebab-case WIT
//     names become camelCase here)
//   - variants → `{ tag: "case-name", val: payload }`
//     (payload-less cases: just `{ tag: "case-name" }`)
//   - enums → the kebab-case string value of the case name
//   - option<T> → undefined for none, T for some
// Keeping the type aliases here gives the type checker a chance to
// catch shape drift if the WIT changes underneath us.
// -----------------------------------------------------------------------------

type RuntimeKind = "js" | "python";

type HttpCapability = { allowedHosts: string[] };
type CapabilitySet = { http: HttpCapability | undefined };

type Oauth2Flow = "authorization-code" | "authorization-code-pkce" | "device-code";
type Oauth2Method = {
  flows: Oauth2Flow[];
  scopes: string[];
  authorizationUrl: string;
  tokenUrl: string;
  deviceAuthUrl: string;
};

type ApikeyLocationKind = "header" | "auth-scheme" | "query-param";
type ApikeyLocation = {
  kind: ApikeyLocationKind;
  name: string | undefined;
  scheme: string | undefined;
};
type ApikeyMethod = { location: ApikeyLocation | undefined };

type SigningAlgorithm = "hmac-sha256" | "hmac-sha512";
type SigningKeyMethod = { algorithm: SigningAlgorithm };

type CredentialMethodVariant =
  | { tag: "basic" }
  | { tag: "oauth2"; val: Oauth2Method }
  | { tag: "apikey"; val: ApikeyMethod }
  | { tag: "signing-key"; val: SigningKeyMethod }
  | { tag: "raw" };

type CredentialMethodEntry = {
  name: string;
  description: string;
  method: CredentialMethodVariant;
};
type CredentialEntry = {
  name: string;
  hosts: string[];
  required: boolean;
  methods: CredentialMethodEntry[];
};

type ToolEntry = {
  name: string;
  description: string;
  inputSchemaJson: string;
};

type ParticleManifestRecord = {
  name: string;
  description: string;
  version: string;
  runtime: RuntimeKind | undefined;
  capabilities: CapabilitySet;
  credentials: CredentialEntry[];
  tools: ToolEntry[];
};

// Shape-cast for the per-method input. Source uses unknown so callers
// can pass user-shaped dicts; we narrow inside.
type RawObject = Record<string, unknown>;

function buildManifestRecord(p: UserParticle): ParticleManifestRecord {
  if (typeof p.name !== "string" || !p.name) {
    throw new Error("particle.name must be a non-empty string");
  }
  if (typeof p.description !== "string") {
    throw new Error("particle.description must be a string");
  }
  if (typeof p.version !== "string" || !p.version) {
    throw new Error("particle.version must be a non-empty string");
  }
  if (p.runtime !== undefined && p.runtime !== "js" && p.runtime !== "python") {
    throw new Error(`particle.runtime must be "js" or "python", got ${JSON.stringify(p.runtime)}`);
  }
  if (p.capabilities && typeof p.capabilities !== "object") {
    throw new Error("particle.capabilities must be an object");
  }
  if (p.credentials != null && typeof p.credentials !== "object") {
    throw new Error("particle.credentials must be an object");
  }
  if (!p.tools || typeof p.tools !== "object") {
    throw new Error("particle.tools must be an object");
  }

  return {
    name: p.name,
    description: p.description,
    version: p.version,
    runtime: p.runtime,
    capabilities: buildCapabilitySet(p.capabilities ?? {}),
    credentials: buildCredentials((p.credentials ?? {}) as Record<string, RawObject>),
    tools: buildTools(p.tools),
  };
}

function buildCapabilitySet(raw: Record<string, unknown>): CapabilitySet {
  const http = raw.http as RawObject | undefined;
  if (!http) return { http: undefined };
  const allowed = (http.allowedHosts as unknown[] | undefined) ?? [];
  if (!Array.isArray(allowed) || !allowed.every((h) => typeof h === "string")) {
    throw new Error("capabilities.http.allowedHosts must be a list of strings");
  }
  return { http: { allowedHosts: allowed as string[] } };
}

function buildCredentials(raw: Record<string, RawObject>): CredentialEntry[] {
  const out: CredentialEntry[] = [];
  for (const [name, cred] of Object.entries(raw)) {
    if (!cred || typeof cred !== "object") {
      throw new Error(`credentials.${name} must be an object`);
    }
    const hosts = (cred.hosts as unknown[] | undefined) ?? [];
    if (!Array.isArray(hosts) || !hosts.every((h) => typeof h === "string")) {
      throw new Error(`credentials.${name}.hosts must be a list of strings`);
    }
    const methodsRaw = (cred.methods as Record<string, RawObject> | undefined) ?? {};
    const methods: CredentialMethodEntry[] = [];
    for (const [mname, method] of Object.entries(methodsRaw)) {
      methods.push({
        name: mname,
        description: typeof method.description === "string" ? method.description : "",
        method: buildCredentialMethod(name, mname, method),
      });
    }
    out.push({
      name,
      hosts: hosts as string[],
      required: Boolean(cred.required),
      methods,
    });
  }
  return out;
}

function buildCredentialMethod(cname: string, mname: string, method: RawObject): CredentialMethodVariant {
  const kind = method.type;
  switch (kind) {
    case "basic": return { tag: "basic" };
    case "raw":   return { tag: "raw" };
    case "oauth2": {
      const flowsRaw = (method.flows as unknown[] | undefined) ?? [];
      const allowedFlows = new Set<Oauth2Flow>(["authorization-code", "authorization-code-pkce", "device-code"]);
      const flows: Oauth2Flow[] = [];
      for (const f of flowsRaw) {
        if (typeof f !== "string" || !allowedFlows.has(f as Oauth2Flow)) {
          throw new Error(`credentials.${cname}.methods.${mname}: unknown OAuth2 flow ${JSON.stringify(f)}`);
        }
        flows.push(f as Oauth2Flow);
      }
      return {
        tag: "oauth2",
        val: {
          flows,
          scopes: ((method.scopes as unknown[] | undefined) ?? []) as string[],
          authorizationUrl: (method.authorizationUrl as string | undefined) ?? "",
          tokenUrl:         (method.tokenUrl         as string | undefined) ?? "",
          deviceAuthUrl:    (method.deviceAuthUrl    as string | undefined) ?? "",
        },
      };
    }
    case "apikey": {
      const loc = method.location as RawObject | undefined;
      if (!loc) return { tag: "apikey", val: { location: undefined } };
      const allowedKinds = new Set<ApikeyLocationKind>(["header", "auth-scheme", "query-param"]);
      if (typeof loc.kind !== "string" || !allowedKinds.has(loc.kind as ApikeyLocationKind)) {
        throw new Error(`credentials.${cname}.methods.${mname}.location.kind ${JSON.stringify(loc.kind)} not recognized`);
      }
      return {
        tag: "apikey",
        val: {
          location: {
            kind:   loc.kind as ApikeyLocationKind,
            name:   typeof loc.name   === "string" ? loc.name   : undefined,
            scheme: typeof loc.scheme === "string" ? loc.scheme : undefined,
          },
        },
      };
    }
    case "signing-key": {
      const allowed = new Set<SigningAlgorithm>(["hmac-sha256", "hmac-sha512"]);
      if (typeof method.algorithm !== "string" || !allowed.has(method.algorithm as SigningAlgorithm)) {
        throw new Error(`credentials.${cname}.methods.${mname}.algorithm ${JSON.stringify(method.algorithm)} not recognized`);
      }
      return { tag: "signing-key", val: { algorithm: method.algorithm as SigningAlgorithm } };
    }
    default:
      throw new Error(`credentials.${cname}.methods.${mname}.type ${JSON.stringify(kind)} not recognized`);
  }
}

function buildTools(raw: Record<string, UserToolDef>): ToolEntry[] {
  const out: ToolEntry[] = [];
  for (const [name, def] of Object.entries(raw)) {
    if (!def || typeof def !== "object") {
      throw new Error(`tools.${name} must be an object`);
    }
    if (typeof def.description !== "string") {
      throw new Error(`tools.${name}.description must be a string`);
    }
    if (!def.inputSchema || typeof def.inputSchema !== "object") {
      throw new Error(`tools.${name}.inputSchema must be an object`);
    }
    if (typeof def.handler !== "function") {
      throw new Error(`tools.${name}.handler must be a function`);
    }
    out.push({
      name,
      description: def.description,
      inputSchemaJson: JSON.stringify(def.inputSchema),
    });
  }
  return out;
}

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
