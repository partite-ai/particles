// TypeScript types for the host-provided `@partite-ai/particle-credentials`
// module. The runtime resolves this import to a JS shim that wraps the
// WIT-imported `particle:host/credentials@0.1.0` interface — see the
// Particle repository for the implementation. This npm package ships
// types only; the actual implementation is provided by the Particle
// runtime at execution time.

/**
 * Per-particle credential access. The particle's
 * `capabilities.credentials.methods` declaration governs which names
 * are valid — calling `fetcher` / `getRaw` with an undeclared name
 * throws a `not-configured` error.
 */
export const credentials: {
  /**
   * Returns a fetch-shaped function bound to the named credential.
   * Each request through it receives the credential at the
   * configured location (Authorization header, custom header, or
   * query param) before the request leaves the host.
   *
   * Available for `basic`, `oauth2`, and `apikey` credentials. For
   * `signing-key` / `raw`, use `@partite-ai/particle-signing`'s
   * `signing.sign` or this module's `getRaw` instead — calling
   * `fetcher` on those throws `type-mismatch`.
   */
  fetcher(name: string): Promise<(input: string | URL, init?: RequestInit) => Promise<Response>>;

  /**
   * Returns the raw value of a `raw`-typed credential. Importing
   * this function requires at least one `type: "raw"` credential
   * in the manifest — making raw access auditable in code review.
   */
  getRaw(name: string): Promise<string>;

  /**
   * Returns the name of the credential method the user configured
   * at setup, or `null` when no method is configured. Particles
   * whose manifest declares multiple alternative auth methods
   * (e.g. oauth2 + apikey for the same provider) call this to
   * discover which one to pass to `fetcher` / `getRaw`.
   *
   * Synchronous — the result is fixed at setup time and resolves
   * via a single host call without any I/O on the hot path.
   */
  getConfiguredMethod(): string | null;
};

/**
 * Discriminated error thrown from the credentials API. Use a type
 * switch on `tag` to disambiguate.
 */
export type CredentialError =
  | { tag: "not-configured" }
  | { tag: "not-found" }
  | { tag: "type-mismatch"; val: string }
  | { tag: "storage-error"; val: string };
