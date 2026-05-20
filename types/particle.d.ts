/**
 * Type definition for the value a `Particlefile.{ts,js}` default-exports.
 *
 * Use it to get type-checking on your particle definition:
 *
 * ```ts
 * import type { Particle } from "particle";
 *
 * export default {
 *   name: "yaml-tools",
 *   description: "Parse and format YAML",
 *   version: "0.1.0",
 *   capabilities: {},
 *   tools: {
 *     parse: {
 *       description: "Parse YAML to JSON",
 *       inputSchema: {
 *         type: "object",
 *         properties: { input: { type: "string" } },
 *         required: ["input"],
 *       },
 *       handler: async ({ input }: { input: string }) => ({ result: input }),
 *     },
 *   },
 * } satisfies Particle;
 * ```
 *
 * Schema-derived argument typing is left to the user — JSON Schema doesn't
 * round-trip cleanly through TypeScript's type system. Annotate the handler's
 * parameter explicitly (as above), or cast inside the handler.
 *
 * Spec: docs/initial-design.md §4 (Particlefile DSL).
 */

// -----------------------------------------------------------------------------
// Top-level
// -----------------------------------------------------------------------------

/**
 * The default export of a Particlefile.
 */
export interface Particle {
  /** Particle identifier; kebab-case. Must match `[a-z0-9][a-z0-9-]*`. */
  name: string;

  /** One-line human-readable description. Surfaces in `particle ls`, MCP tool listings, etc. */
  description: string;

  /** Semver version string (e.g., "0.1.0"). The build pipeline checks this. */
  version: string;

  /**
   * Optional. Names the guest engine the runtime should use for this
   * particle. JS/TS particles default to `"js"` if omitted — the only
   * value this field needs in a `Particlefile.ts`. Python particles
   * (`Particlefile.py`) are tagged `"python"` automatically by the
   * build pipeline.
   */
  runtime?: "js" | "python";

  /**
   * Declares which host capabilities this particle uses. Importing a
   * `particle:*` namespace whose capability isn't listed here is a build
   * error — every host call is gated by an explicit declaration.
   */
  capabilities: Capabilities;

  /**
   * Named credentials the particle needs. Each entry declares one
   * or more alternative methods (the user picks at setup), and
   * optionally pins the credential to a set of HTTP destinations —
   * a credential's value is only substituted into requests bound
   * for one of those hosts.
   *
   * Credentials are NOT a capability: they don't gate runtime
   * behavior, they describe what secret material the particle's
   * declared HTTP allow-list needs. The runtime's
   * `particle:host/credentials` interface is wired unconditionally;
   * this map controls what's stored and how it's applied.
   */
  credentials?: Record<string, CredentialDecl>;

  /**
   * Tools the particle exposes. Keys are the tool names that will be
   * surfaced over MCP / `particle run`.
   */
  tools: Record<string, ToolDef>;

  /**
   * Optional health-check handler. Called by `particle ping`. Omit if
   * the particle has no meaningful liveness signal — `particle ping`
   * will report "not implemented" and exit 0.
   */
  ping?(): PingResult | Promise<PingResult>;

  /**
   * Optional inline test cases run by `particle test`.
   */
  tests?: TestCase[];
}

// -----------------------------------------------------------------------------
// Tools
// -----------------------------------------------------------------------------

export interface ToolDef {
  /** Human-readable description shown to LLM clients. */
  description: string;

  /**
   * JSON-Schema (Draft 2020-12) describing the tool's argument object.
   * Must have root `type: "object"`. Validated host-side before the
   * handler runs — invalid arguments never reach this function.
   */
  inputSchema: JSONSchema;

  /**
   * The tool body. Receives the validated argument object; returns
   * any JSON-serializable value (or a Promise of one). Throwing
   * surfaces to the caller as a handler error.
   *
   * Note: this is declared with method-shorthand syntax so user
   * handlers can type their argument explicitly
   * (e.g., `handler: ({ input }: { input: string }) => …`)
   * without TypeScript flagging a contravariance error against
   * `args: unknown`.
   */
  handler(args: unknown): unknown | Promise<unknown>;
}

/**
 * A subset of JSON Schema — just enough structure to constrain the
 * shape of `inputSchema` without trying to model every keyword.
 * Unrecognized keywords are allowed (the host validates against the
 * full draft).
 */
export interface JSONSchema {
  type?: "object" | "string" | "number" | "integer" | "boolean" | "array" | "null";
  properties?: Record<string, JSONSchema>;
  required?: string[];
  items?: JSONSchema | JSONSchema[];
  enum?: unknown[];
  description?: string;
  default?: unknown;
  // The 2020-12 draft has many more keywords (oneOf, anyOf, allOf,
  // const, format, …). They're permitted here via the index
  // signature so the type doesn't fight users who reach for them.
  [keyword: string]: unknown;
}

// -----------------------------------------------------------------------------
// Health
// -----------------------------------------------------------------------------

export interface PingResult {
  status: "ok" | "degraded" | "unhealthy";
  message?: string;
  details?: string;
}

// -----------------------------------------------------------------------------
// Tests
// -----------------------------------------------------------------------------

export interface TestCase {
  /** Test name shown in `particle test` output. */
  name: string;
  /** Tool to invoke. Must match a key in `tools`. */
  tool: string;
  /** Arguments passed to the handler. Validated against the tool's `inputSchema`. */
  args: unknown;
  /** Expected handler return value. Compared deep-equal. */
  expect?: unknown;
  /** Or, expect the handler to error. */
  expectError?: {
    kind: "handler-error" | "invalid-arguments" | "capability-denied" | "not-found";
    messageMatches?: string;
  };
}

// -----------------------------------------------------------------------------
// Capabilities
// -----------------------------------------------------------------------------

export interface Capabilities {
  /**
   * Outbound HTTP. The host's policy denies every request whose
   * URL hostname isn't in `allowedHosts`. Omitting `http` (or
   * leaving `allowedHosts` empty) denies everything.
   */
  http?: HTTPCapability;
}

export interface HTTPCapability {
  /**
   * Hosts the particle is permitted to reach. Wildcards aren't
   * supported in v1; list each host literally.
   */
  allowedHosts?: string[];
}

// -----------------------------------------------------------------------------
// Credentials
//
// One entry per named credential the particle uses (e.g., "github",
// "openai"). Each declares one or more alternative authentication
// methods — at setup the user picks one and only that one is
// provisioned. The runtime exposes the chosen method's name via
// `credentials.getConfiguredMethod(name)`.
//
// `hosts` is the HTTP-side binding: when set, the host-side
// substitution policy only swaps the credential's placeholder for
// the real value on requests whose hostname is in `hosts`. Omit
// for non-HTTP credentials (e.g., signing keys), where the
// credential is accessed by name through the relevant host
// interface and HTTP scoping is irrelevant.
//
// Each method declaration is one of the five typed variants,
// discriminated on `type` — TypeScript narrows correctly when the
// user reads `m.type === "oauth2"` etc.
// -----------------------------------------------------------------------------

export interface CredentialDecl {
  /**
   * Hosts on which the host-side wasi:http policy is allowed to
   * substitute this credential. Exact match against the request
   * URL's hostname; wildcards aren't supported. Every entry must
   * also appear in `capabilities.http.allowedHosts` — the build
   * pipeline rejects out-of-scope hosts.
   *
   * Absent → not bound to any HTTP destination. Use for
   * credentials consumed entirely via the JS-side API (signing
   * keys, raw values) where HTTP substitution isn't relevant.
   */
  hosts?: string[];

  /**
   * Refuse to register the particle until one method for this
   * credential is configured. Defaults to `false` — handlers
   * should treat `credentials.getConfiguredMethod(name)`
   * returning `null` as the unauthenticated state.
   */
  required?: boolean;

  /**
   * Alternative authentication methods. The user picks one at
   * setup; the rest are never touched. A single entry skips the
   * prompt.
   */
  methods: Record<string, CredentialMethod>;
}

export type CredentialMethod =
  | BasicCredentialMethod
  | OAuth2CredentialMethod
  | APIKeyCredentialMethod
  | SigningKeyCredentialMethod
  | RawCredentialMethod;

interface CredentialMethodBase {
  /** Human-readable description shown during `particle setup`. */
  description?: string;
}

export interface BasicCredentialMethod extends CredentialMethodBase {
  type: "basic";
}

export interface OAuth2CredentialMethod extends CredentialMethodBase {
  type: "oauth2";
  /**
   * OAuth flows the particle supports. `particle setup` lets the
   * user pick when more than one is offered. Listing a single
   * value fixes the flow without prompting.
   */
  flows: OAuth2Flow[];
  /** Application-level scope set this credential is granted. */
  scopes: string[];

  /**
   * Pre-set authorization endpoint. Set to skip the prompt;
   * empty / unset → setup prompts with no default.
   */
  authorizationUrl?: string;
  /** Pre-set token endpoint. Same prompt-vs-hardcoded behavior. */
  tokenUrl?: string;
  /** Pre-set device-authorization endpoint (used by `device-code` flow). */
  deviceAuthUrl?: string;
}

export type OAuth2Flow =
  /** Browser-based; PKCE-only client (typically no client secret). */
  | "authorization-code-pkce"
  /** Browser-based; client-secret-bearing OAuth app. */
  | "authorization-code"
  /** Headless / SSH-friendly; user authorizes on a separate device. */
  | "device-code";

export interface APIKeyCredentialMethod extends CredentialMethodBase {
  type: "apikey";
  /**
   * Optional pre-set location. When provided, setup skips the
   * "where does this key appear?" prompt and only asks for the
   * key value. Useful when the API has a single canonical place
   * for its key — e.g., GitHub PATs always go in
   * `Authorization: Bearer <pat>`.
   */
  location?: APIKeyApplyLocation;
}

/**
 * Where an apikey credential's value gets substituted in an
 * outgoing HTTP request. The kind discriminator keys the rest of
 * the shape — TypeScript narrows correctly when you switch on it.
 */
export type APIKeyApplyLocation =
  /** `<name>: <key>` — a custom request header. */
  | { kind: "header"; name: string }
  /** `Authorization: <scheme> <key>` — e.g. "Bearer", "Token". */
  | { kind: "auth-scheme"; scheme: string }
  /** `?<name>=<key>` — appended to the URL query string. */
  | { kind: "query-param"; name: string };

export interface SigningKeyCredentialMethod extends CredentialMethodBase {
  type: "signing-key";
  /** v1 supports HMAC-SHA-256 and HMAC-SHA-512. */
  algorithm: "hmac-sha256" | "hmac-sha512";
}

export interface RawCredentialMethod extends CredentialMethodBase {
  type: "raw";
  // Setup shows an explicit warning before storing — `raw`
  // credentials are returned in plaintext to the JS handler.
}
