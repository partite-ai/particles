// TypeScript types for the host-provided `@partite-ai/particle-signing`
// module. The runtime resolves this import to a JS shim that wraps
// the WIT-imported `particle:host/signing@0.1.0` interface — see
// the Particle repository for the implementation. Types only.

/**
 * Cryptographic operations against a host-stored key. The key
 * material never enters JS; sign / verify happen entirely on the
 * host side. Importing this requires at least one
 * `type: "signing-key"` credential in the manifest.
 */
export const signing: {
  /** Returns the signature of `data` under the named key. */
  sign(name: string, data: Uint8Array): Promise<Uint8Array>;
  /** Returns whether `signature` matches `data` under the named key. */
  verify(name: string, data: Uint8Array, signature: Uint8Array): Promise<boolean>;
};

export type SigningError =
  | { tag: "not-configured" }
  | { tag: "not-signing-key" }
  | { tag: "invalid-input"; val: string };
