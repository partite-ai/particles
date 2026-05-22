/**
 * Bridges the user-facing `particle:*` JS module names to the
 * WIT-imported `particle:host/*@0.1.0` modules.
 *
 * The auto-generated wasm-rquickjs bindings expose each WIT
 * interface under its full WIT name (`particle:host/credentials@0.1.0`),
 * but particles import the shorter `@partite-ai/particle-credentials` etc. (per
 * docs/initial-design.md §2). This shim:
 *
 *   - Imports the WIT-bound functions
 *   - Wraps them in the user-facing API objects (fetcher composition
 *     for credentials, sync→async normalization for kv/signing/oauth,
 *     option<string> → string|null for the few results that need it)
 *   - Registers each `particle:*` module via wasm-rquickjs's
 *     module-mock API so user `import { credentials } from
 *     "@partite-ai/particle-credentials"` resolves before the bundle is evaluated
 *
 * Importing this file at the top of runtime.ts is what arms the
 * registrations; subsequent `import("/particle/bundle.js")` inside
 * loadParticle then sees the bridges in place.
 */

import {
  getPlaceholder as witGetPlaceholder,
  getRaw as witGetRaw,
  getConfiguredMethod as witGetConfiguredMethod,
} from "particle:host/credentials@0.1.0";
import { refresh as witRefresh } from "particle:host/oauth@0.1.0";
import { sign as witSign, verify as witVerify } from "particle:host/signing@0.1.0";
import * as witKv from "particle:host/kv@0.1.0";

declare const __wasm_rquickjs_register_module_mock: (
  specifier: string,
  options: { namedExports?: Record<string, unknown>; defaultExport?: unknown },
) => unknown;

// -----------------------------------------------------------------------------
// credentials
// -----------------------------------------------------------------------------

type ApplyInfo = {
  placeholder: string;
  apply: { kind: string; name?: string; scheme?: string };
};

// applyPlaceholder rewrites (url, init) per the apply-spec the host
// gave us — same logic as docs/initial-design.md §7's runtime
// sketch. Keeps the user-side fetch wrapper one statement long.
function applyPlaceholder(url: string, init: RequestInit, info: ApplyInfo): { url: string; init: RequestInit } {
  const headers = new Headers(init.headers);
  switch (info.apply.kind) {
    case "basic":
      headers.set("Authorization", `Basic ${info.placeholder}`);
      break;
    case "bearer":
      headers.set("Authorization", `Bearer ${info.placeholder}`);
      break;
    case "header":
      if (info.apply.name) headers.set(info.apply.name, info.placeholder);
      break;
    case "auth-scheme":
      if (info.apply.scheme) headers.set("Authorization", `${info.apply.scheme} ${info.placeholder}`);
      break;
    case "query-param": {
      if (!info.apply.name) break;
      const u = new URL(url);
      u.searchParams.set(info.apply.name, info.placeholder);
      return { url: u.toString(), init: { ...init, headers } };
    }
  }
  return { url, init: { ...init, headers } };
}

const credentials = {
  /**
   * Returns a fetch-shaped function bound to the named credential.
   * The wrapper resolves the placeholder via the host once and
   * applies it on every call — the host-side wasi:http policy then
   * substitutes the real value at the wire boundary.
   */
  async fetcher(name: string) {
    const info = witGetPlaceholder(name) as ApplyInfo;
    return async (input: string | URL, init: RequestInit = {}) => {
      const decorated = applyPlaceholder(input.toString(), init, info);
      return fetch(decorated.url, decorated.init);
    };
  },

  /**
   * Lower-level: return the opaque placeholder string + apply-spec
   * for a named credential. Use this when an SDK (axios, googleapis,
   * etc.) manages its own request flow and you need to hand it a
   * bearer/header value directly — the placeholder is safe to log,
   * embed in URLs, or stash in SDK config, since the real secret is
   * substituted at the wasi:http boundary.
   *
   * Prefer `fetcher` for the common case where a single fetch call
   * is what you want; this exists for SDK integration.
   */
  getPlaceholder(name: string): { placeholder: string; apply: { kind: string; name?: string; scheme?: string } } {
    return witGetPlaceholder(name) as ApplyInfo;
  },

  /**
   * Returns the raw value of a `raw`-typed credential.
   */
  async getRaw(name: string): Promise<string> {
    return witGetRaw(name);
  },

  /**
   * Returns the method name the user configured for the named
   * credential at setup, or null when no method is set.
   * Particles that declare multiple alternative methods for a
   * credential (e.g. oauth2 + apikey for the same provider)
   * call this to find out which method backs the credential.
   *
   * Sync — the result is fixed at setup time, no I/O on the
   * hot path.
   */
  getConfiguredMethod(name: string): string | null {
    const v = witGetConfiguredMethod(name);
    return v == null ? null : v;
  },
};

// -----------------------------------------------------------------------------
// oauth
// -----------------------------------------------------------------------------

const oauth = {
  async refresh(name: string): Promise<void> {
    witRefresh(name);
  },
};

// -----------------------------------------------------------------------------
// signing
// -----------------------------------------------------------------------------

const signing = {
  async sign(name: string, data: Uint8Array): Promise<Uint8Array> {
    return witSign(name, data);
  },
  async verify(name: string, data: Uint8Array, signature: Uint8Array): Promise<boolean> {
    return witVerify(name, data, signature);
  },
};

// -----------------------------------------------------------------------------
// kv
// -----------------------------------------------------------------------------

const kv = {
  async get(key: string): Promise<string | null> {
    const v = witKv.get(key);
    return v == null ? null : v;
  },
  async set(key: string, value: string): Promise<void> {
    witKv.set(key, value);
  },
  async delete(key: string): Promise<void> {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    (witKv as any)["delete"](key);
  },
  async list(prefix: string): Promise<string[]> {
    return witKv.list(prefix);
  },
};

// -----------------------------------------------------------------------------
// Register the user-facing modules. wasm-rquickjs evaluates this
// file before any user-bundle module is loaded, so the
// registrations are armed by the time the dynamic import fires.
// -----------------------------------------------------------------------------

__wasm_rquickjs_register_module_mock("@partite-ai/particle-credentials", {
  namedExports: { credentials },
});
__wasm_rquickjs_register_module_mock("@partite-ai/particle-oauth", {
  namedExports: { oauth },
});
__wasm_rquickjs_register_module_mock("@partite-ai/particle-signing", {
  namedExports: { signing },
});
__wasm_rquickjs_register_module_mock("@partite-ai/particle-kv", {
  namedExports: { kv },
});
