// TypeScript types for the host-provided `@partite-ai/particle-credentials`
// module. The runtime resolves this import to a JS shim that wraps the
// WIT-imported `particle:host/credentials@0.1.0` interface — see the
// Particle repository for the implementation. This npm package ships
// types only; the actual implementation is provided by the Particle
// runtime at execution time.

/**
 * Per-particle credential access. The particle's top-level
 * `credentials` map declares which names are valid — calling
 * `fetcher` / `getRaw` with an undeclared name throws a
 * `not-configured` error.
 */
export const credentials: {
  /**
   * Returns a fetch-shaped function bound to the named credential.
   * Each request through it receives the credential at the
   * configured location (Authorization header, custom header, or
   * query param) before the request leaves the host.
   *
   * Host-side scope: when the manifest pins the credential to a
   * `hosts` set, substitution only fires on requests to one of
   * those hosts; a stray request to another host transmits the
   * placeholder literally and the upstream returns 401, surfacing
   * misuse to the particle.
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
   * Lower-level escape hatch: returns an opaque placeholder string
   * + apply-spec for the named credential. Use this when handing a
   * bearer token (or API key) to an SDK that owns its own HTTP
   * client — the SDK can plant the placeholder anywhere it would
   * normally plant the secret, and the host substitutes the real
   * value at the wasi:http boundary as long as the request goes
   * through the global `fetch`.
   *
   * Prefer `fetcher` for the common case (one fetch call, one
   * credential). Reach for `getPlaceholder` when integrating with
   * googleapis, axios, etc.
   *
   * Synchronous — the placeholder is fixed for the particle
   * instance's lifetime.
   */
  getPlaceholder(name: string): {
    placeholder: string;
    apply: { kind: string; name?: string; scheme?: string };
  };

  /**
   * Returns the method name the user configured for the named
   * credential at setup, or `null` when no method is set.
   * Particles whose manifest declares multiple alternative
   * methods for a credential (e.g. oauth2 + apikey for the same
   * provider) call this to discover which method backs the
   * credential — usually unneeded if all methods share an
   * apply-spec (the common case).
   *
   * Synchronous — the result is fixed at setup time and resolves
   * via a single host call without any I/O on the hot path.
   */
  getConfiguredMethod(name: string): string | null;
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
