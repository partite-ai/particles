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
   * Declares which host capabilities this particle uses. Importing a
   * `particle:*` namespace whose capability isn't listed here is a build
   * error — every host call is gated by an explicit declaration.
   */
  capabilities: Capabilities;

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
   * Outbound HTTP. Presence enables `wasi:http`; the host's policy
   * rejects requests to hosts not in `allowedHosts`.
   */
  http?: HTTPCapability;

  /**
   * Outbound TCP/UDP. Off by default; listening sockets are denied
   * unless `allowListen: true`. Phase 1 has limited support.
   */
  sockets?: SocketsCapability;

  /**
   * Per-particle persistent KV store. Presence enables the
   * `particle:kv` import — value type is intentionally empty
   * (no per-keyspace policy in v1).
   */
  kv?: Record<string, never>;

  /**
   * Credentials the particle needs.
   *
   * `methods` lists the alternative authentication methods the
   * particle accepts — at setup the user picks one, and only that
   * one is provisioned. Use [`credentials.getConfiguredMethod`]
   * inside a tool to find out which name to pass to
   * `fetcher` / `getRaw`.
   *
   * `required: true` makes setup refuse to register the particle
   * until one method is configured. `required: false` lets the
   * particle run without auth (handlers should fall back when
   * `getConfiguredMethod` returns `null`).
   */
  credentials?: CredentialsCapability;

  /**
   * Allowlist of environment variables exposed via `process.env`.
   * Vars not listed here are filtered out before reaching the
   * particle.
   */
  env?: Record<string, EnvDecl>;
}

export interface HTTPCapability {
  /**
   * Hosts the particle is permitted to reach. Wildcards aren't
   * supported in v1; list each host literally.
   */
  allowedHosts?: string[];
}

export interface SocketsCapability {
  /** Allowlist of (host, port) the particle may connect to. */
  allowedEndpoints: { host: string; port: number }[];
}

export interface EnvDecl {
  /** Human-readable description shown during `particle setup`. */
  description?: string;
  /** Whether the host should refuse to start without this var set. */
  required?: boolean;
}

// -----------------------------------------------------------------------------
// Credentials
//
// `CredentialsCapability` declares the credential block at the top
// level: a required flag and a map of named alternative methods.
// At setup the user picks one; only that one is provisioned, and
// the runtime exposes the chosen name via `credentials.getConfiguredMethod`.
//
// Each method declaration is one of the five typed variants,
// discriminated on `type` — TypeScript narrows correctly when the
// user reads `decl.type === "oauth2"` etc.
// -----------------------------------------------------------------------------

export interface CredentialsCapability {
  /**
   * Refuse to register the particle until one method is configured.
   * `false` means the particle can run unauthenticated; handlers
   * should treat `credentials.getConfiguredMethod()` returning null
   * as the unauthenticated state.
   */
  required: boolean;

  /**
   * Alternative authentication methods. The user picks one at
   * setup; the rest are never touched.
   */
  methods: Record<string, CredentialDecl>;
}

export type CredentialDecl =
  | BasicCredentialDecl
  | OAuth2CredentialDecl
  | APIKeyCredentialDecl
  | SigningKeyCredentialDecl
  | RawCredentialDecl;

interface CredentialDeclBase {
  /** Human-readable description shown during `particle setup`. */
  description?: string;
}

export interface BasicCredentialDecl extends CredentialDeclBase {
  type: "basic";
}

export interface OAuth2CredentialDecl extends CredentialDeclBase {
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
   * Optional well-known provider hint. Pre-fills the URL prompts
   * (authorization / token / device-auth / revocation) at setup
   * with that provider's well-known endpoints — the user can
   * override at the prompt for self-hosted variants
   * (GitHub Enterprise, etc.).
   *
   * Manifest-level URL overrides (the four fields below) take
   * precedence over the provider hint. Use them when you know
   * the exact endpoints and want setup to skip the prompts
   * entirely.
   */
  provider?: "github" | "google" | "slack";

  /**
   * Pre-set authorization endpoint. Set to skip the prompt.
   * Empty / unset → setup prompts (defaulting to the provider
   * hint's value if any).
   */
  authorizationUrl?: string;
  /** Pre-set token endpoint. Same prompt-vs-hardcoded behavior. */
  tokenUrl?: string;
  /** Pre-set device-authorization endpoint (used by `device-code` flow). */
  deviceAuthUrl?: string;
  /** Pre-set revocation endpoint (used at credential removal). */
  revocationUrl?: string;
}

export type OAuth2Flow =
  /** Browser-based; PKCE-only client (typically no client secret). */
  | "authorization-code-pkce"
  /** Browser-based; client-secret-bearing OAuth app. */
  | "authorization-code"
  /** Headless / SSH-friendly; user authorizes on a separate device. */
  | "device-code";

export interface APIKeyCredentialDecl extends CredentialDeclBase {
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

export interface SigningKeyCredentialDecl extends CredentialDeclBase {
  type: "signing-key";
  /** v1 supports HMAC-SHA-256 and HMAC-SHA-512. */
  algorithm: "hmac-sha256" | "hmac-sha512";
}

export interface RawCredentialDecl extends CredentialDeclBase {
  type: "raw";
  // Setup shows an explicit warning before storing — `raw`
  // credentials are returned in plaintext to the JS handler.
}
