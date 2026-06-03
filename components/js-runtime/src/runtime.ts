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

// Shared diagnostic payload — mirrors WIT `diagnostics.error-detail`.
// wasm-rquickjs lifts JS records into the canonical ABI via camelCased
// fields; `stack` is `undefined` on the JS side ↔ `none` on the WIT
// side.
type ErrorDetail = { message: string; stack: string | undefined };

function detailFromThrown(e: unknown): ErrorDetail {
  return { message: messageOf(e), stack: stackOf(e) };
}

type ToolError =
  | { tag: "not-found" }
  | { tag: "invalid-arguments"; val: ErrorDetail }
  | { tag: "handler-error"; val: ErrorDetail }
  | { tag: "capability-denied"; val: ErrorDetail };

function notFoundError(): ToolError {
  return { tag: "not-found" };
}

function handlerError(detail: ErrorDetail): ToolError {
  return { tag: "handler-error", val: detail };
}

type PingStatus = "ok" | "degraded" | "unhealthy";

type PingResult = {
  status: PingStatus;
  message?: string;
  details?: string;
};

type HealthError =
  | { tag: "not-implemented" }
  | { tag: "handler-error"; val: ErrorDetail };

function notImplementedError(): HealthError {
  return { tag: "not-implemented" };
}

function healthHandlerError(detail: ErrorDetail): HealthError {
  return { tag: "handler-error", val: detail };
}

// -----------------------------------------------------------------------------
// Particle module shape — what the user's bundle.mjs default-exports.
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
  credentials?: Record<string, unknown>;
  tools: Record<string, UserToolDef>;
  ping?: () => PingResult | Promise<PingResult>;
};

type ManifestError =
  | { tag: "bundle-load-error"; val: ErrorDetail }
  | { tag: "invalid-manifest"; val: ErrorDetail };

function bundleLoadError(detail: ErrorDetail): ManifestError {
  return { tag: "bundle-load-error", val: detail };
}

function invalidManifest(detail: ErrorDetail): ManifestError {
  return { tag: "invalid-manifest", val: detail };
}

// -----------------------------------------------------------------------------
// Bundle loader — reads /particle/bundle.mjs via dynamic import (which
// wasm-rquickjs routes through the wasi:filesystem preopens the host
// mounted before instantiation) and captures its default export.
// Cached for the instance lifetime: the bundle is immutable and a hot-
// path tool call shouldn't pay re-evaluation cost.
//
// The `.mjs` extension is load-bearing: wasm-rquickjs's
// `ImportMetaLoader` accepts it unconditionally as ESM, sidestepping
// `CjsCompatLoader`'s content-based CJS detection (which mis-fires on
// bundles that embed CJS modules verbatim inside ESM wrappers).
// -----------------------------------------------------------------------------

let cachedParticle: UserParticle | null = null;
let loadError: Error | null = null;

async function loadParticle(): Promise<UserParticle> {
  if (cachedParticle) return cachedParticle;
  if (loadError) throw loadError;

  try {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const mod = await (import("/particle/bundle.mjs") as Promise<any>);
    if (!mod || typeof mod.default === "undefined") {
      throw new Error("bundle has no default export");
    }
    cachedParticle = mod.default as UserParticle;
    return cachedParticle;
  } catch (e) {
    // Preserve the original Error so its stack survives all the way
    // to the host (via stackOf). For non-Error throws (strings,
    // primitives, WIT variants from particle:* host calls), wrap in
    // a fresh Error using the synthesized message — we don't have
    // a stack for those.
    loadError = e instanceof Error ? e : new Error(messageOf(e));
    throw loadError;
  }
}

// messageOf turns whatever a handler threw into a one-line summary
// — what the WIT error variant's `message` field carries. JS's
// default `String(x)` on a plain object yields the notorious
// "[object Object]"; the typical particle-side throw is one of:
//   - an Error (use .message)
//   - a WIT variant like `{ tag: "not-configured" }` thrown by a
//     particle:* host call when the runtime maps the error back
//     into JS
//   - a string, number, boolean, null
//   - some other plain object
// Each branch produces something a human reading the log can act
// on. JSON.stringify is the catch-all so even unusual payloads
// stay debuggable.
function messageOf(e: unknown): string {
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

// stackOf returns the operator-visible stack for the WIT error
// variant's `stack` field, or undefined when no stack is available
// (the throw was a string / WIT variant / primitive with no
// .stack property). Hosts surface this separately from the
// summary — typically dropped in a log line, not the user-visible
// error.
function stackOf(e: unknown): string | undefined {
  if (e instanceof Error && typeof e.stack === "string") return e.stack;
  return undefined;
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

function logStack(e: unknown): void {
  // Operator-visible diagnostic on wasi:cli/stderr. The host captures
  // this buffer and surfaces it on the Stderr field of the Go error
  // struct; the WIT error variant's `stack` carries the same info
  // structured, so callers have it either way. We still write it here
  // because some throw sites (module-evaluation traps) never reach a
  // WIT return — only stderr makes it across.
  const stack = stackOf(e);
  if (stack !== undefined) {
    console.error(stack);
    return;
  }
  console.error(messageOf(e));
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
      throw handlerError(detailFromThrown(e));
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
      throw handlerError({ message: `argument JSON parse: ${messageOf(e)}`, stack: stackOf(e) });
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
      throw handlerError(detailFromThrown(e));
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
      throw bundleLoadError(detailFromThrown(e));
    }
    try {
      return buildManifestRecord(particle);
    } catch (e) {
      throw invalidManifest(detailFromThrown(e));
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

type HttpCapability = { allowedHosts: string[] };
type MountAccess = "readonly" | "readwrite";
type MountDecl = {
  name: string;
  description: string;
  path: string;
  access: MountAccess;
  required: boolean;
};
type TempMountDecl = {
  name: string;
  description: string;
  path: string;
  maxSize: string;
};
type FilesystemCapability = { mounts: MountDecl[]; temp: TempMountDecl[] };
type KvCapability = { enabled: boolean };
type CapabilitySet = {
  http: HttpCapability | undefined;
  filesystem: FilesystemCapability | undefined;
  kv: KvCapability | undefined;
};

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
    capabilities: buildCapabilitySet(p.capabilities ?? {}),
    credentials: buildCredentials((p.credentials ?? {}) as Record<string, RawObject>),
    tools: buildTools(p.tools),
  };
}

function buildCapabilitySet(raw: Record<string, unknown>): CapabilitySet {
  return {
    http: buildHttpCapability(raw.http as RawObject | undefined),
    filesystem: buildFilesystemCapability(raw.filesystem as RawObject | undefined),
    kv: buildKvCapability(raw.kv as RawObject | undefined),
  };
}

function buildKvCapability(kvRaw: RawObject | undefined): KvCapability | undefined {
  if (!kvRaw) return undefined;
  if (typeof kvRaw.enabled !== "boolean") {
    throw new Error("capabilities.kv.enabled must be a boolean");
  }
  return { enabled: kvRaw.enabled };
}

function buildHttpCapability(http: RawObject | undefined): HttpCapability | undefined {
  if (!http) return undefined;
  const allowed = (http.allowedHosts as unknown[] | undefined) ?? [];
  if (!Array.isArray(allowed) || !allowed.every((h) => typeof h === "string")) {
    throw new Error("capabilities.http.allowedHosts must be a list of strings");
  }
  return { allowedHosts: allowed as string[] };
}

// Lifts capabilities.filesystem (mounts/temp keyed by name) into the
// WIT list-shape, with the map key inlined as `name`. Mirrors the
// validation the Go runtime enforces at load time so an invalid
// declaration fails at build, not first run.
function buildFilesystemCapability(fsRaw: RawObject | undefined): FilesystemCapability | undefined {
  if (!fsRaw) return undefined;

  const mounts: MountDecl[] = [];
  const mountsRaw = (fsRaw.mounts as Record<string, RawObject> | undefined) ?? {};
  for (const [name, m] of Object.entries(mountsRaw)) {
    if (typeof m.description !== "string" || !m.description) {
      throw new Error(`capabilities.filesystem.mounts.${name}.description is required`);
    }
    if (typeof m.path !== "string" || !m.path) {
      throw new Error(`capabilities.filesystem.mounts.${name}.path must be a non-empty string`);
    }
    if (m.access !== "readonly" && m.access !== "readwrite") {
      throw new Error(`capabilities.filesystem.mounts.${name}.access must be "readonly" or "readwrite"`);
    }
    mounts.push({
      name,
      description: m.description,
      path: m.path,
      access: m.access,
      required: m.required === true,
    });
  }

  const temp: TempMountDecl[] = [];
  const tempRaw = (fsRaw.temp as Record<string, RawObject> | undefined) ?? {};
  for (const [name, t] of Object.entries(tempRaw)) {
    if (typeof t.description !== "string" || !t.description) {
      throw new Error(`capabilities.filesystem.temp.${name}.description is required`);
    }
    if (typeof t.path !== "string" || !t.path) {
      throw new Error(`capabilities.filesystem.temp.${name}.path must be a non-empty string`);
    }
    if (typeof t.maxSize !== "string" || !t.maxSize) {
      throw new Error(`capabilities.filesystem.temp.${name}.maxSize must be a non-empty string`);
    }
    temp.push({ name, description: t.description, path: t.path, maxSize: t.maxSize });
  }

  return { mounts, temp };
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
      throw healthHandlerError(detailFromThrown(e));
    }

    if (typeof particle.ping !== "function") {
      throw notImplementedError();
    }

    try {
      return await particle.ping();
    } catch (e) {
      logStack(e);
      throw healthHandlerError(detailFromThrown(e));
    }
  },
};
