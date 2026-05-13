// TypeScript types for the host-provided `@partite-ai/particle-oauth`
// module. The runtime resolves this import to a JS shim that wraps
// the WIT-imported `particle:host/oauth@0.1.0` interface — see the
// Particle repository for the implementation. This npm package
// ships types only.

/**
 * OAuth-specific operations on top of `@partite-ai/particle-credentials`.
 * Importing this requires at least one `type: "oauth2"` credential
 * in the manifest.
 */
export const oauth: {
  /**
   * Force a refresh regardless of cached expiry. Use this when an
   * upstream returns 401/403 on a token you thought was still
   * valid; the next `credentials.fetcher` call will pick up the
   * rotated token.
   */
  refresh(name: string): Promise<void>;
};

export type OAuthError =
  | { tag: "not-configured" }
  | { tag: "not-oauth" }
  | { tag: "refresh-failed"; val: string };
