// TypeScript types for the host-provided `@partite-ai/particle-kv`
// module. The runtime resolves this import to a JS shim that wraps
// the WIT-imported `particle:host/kv@0.1.0` interface — see the
// Particle repository for the implementation. Types only.

/**
 * Per-particle key/value store. Strings only — base64-encode
 * binary if you must. Keys and values are scoped by particle name;
 * two particles using the same key see independent values.
 */
export const kv: {
  /** Returns the stored string, or null if no entry exists. */
  get(key: string): Promise<string | null>;
  /** Replaces or creates the entry. */
  set(key: string, value: string): Promise<void>;
  /** Idempotent — removing an absent key is fine. */
  delete(key: string): Promise<void>;
  /** Returns keys with the given prefix, in unspecified order. */
  list(prefix: string): Promise<string[]>;
};

export type KVError =
  | { tag: "storage-error"; val: string }
  | { tag: "quota-exceeded" };
