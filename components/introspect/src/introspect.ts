/**
 * Particle introspect component — TypeScript source compiled to a single
 * ESM file consumed by `wasm-rquickjs generate-wrapper-crate`.
 *
 * Phase 5 of the particle build pipeline. The Go orchestrator:
 *   1. bundles the user's source into bundle.js (Phase 4),
 *   2. mounts /particle/bundle.js into a fresh introspect instance,
 *   3. calls `manifest()` and gets the manifest JSON back.
 *
 * The component never invokes a handler — it just imports the bundle,
 * reads the default export's metadata, validates the shape, and
 * stringifies it.
 *
 * The user's bundle.js still contains `import { credentials } from
 * "@partite-ai/particle-credentials"` and similar statements (esbuild marks
 * `particle:*` external during Phase 4 so the real runtime can wire
 * them). Because handlers are never invoked here, those imported
 * modules are never *used* — only loaded. We register local no-op
 * stubs for every `particle:*` module specifier before evaluating the
 * bundle, so resolution succeeds entirely inside QuickJS without
 * crossing any WIT boundary. The host therefore wires no
 * `particle:host/*` imports — see wit/introspect.wit.
 *
 * wasm-rquickjs WIT-to-JS convention:
 *   - WIT interface names → JS exports in camelCase, methods in camelCase
 *   - `result<T, E>` is NOT serialized as `{ tag, val }` from JS. The
 *     ok arm is the function's return value directly; the err arm is
 *     produced by *throwing* a value that matches E's shape (for a
 *     variant E, that's `{ tag: "case-name", val: payload }`).
 */

// -----------------------------------------------------------------------------
// Types mirroring the WIT records/variants. These match what wasm-rquickjs
// produces from the WIT bindings.
//
// `result<T, E>` is implicit: returning T = ok arm; throwing E = err arm.
// Variants are `{ tag: "case-name", val: payload }`.
// -----------------------------------------------------------------------------

type IntrospectError =
  | { tag: "bundle-load-error"; val: string }
  | { tag: "invalid-manifest"; val: string };

function bundleLoadError(message: string): IntrospectError {
  return { tag: "bundle-load-error", val: message };
}

function invalidManifest(message: string): IntrospectError {
  return { tag: "invalid-manifest", val: message };
}

// -----------------------------------------------------------------------------
// Particle module shape. Mirrors docs/initial-design.md §4. Only the
// metadata side matters here — handlers are never called, just verified to
// be functions.
// -----------------------------------------------------------------------------

type UserToolDef = {
  description: string;
  inputSchema: object;
  handler: (args: unknown) => unknown | Promise<unknown>;
};

type UserParticle = {
  name: string;
  description: string;
  version: string;
  capabilities: Record<string, unknown>;
  credentials?: Record<string, unknown>;
  tools: Record<string, UserToolDef>;
};

// -----------------------------------------------------------------------------
// Bundle loader. Reads /particle/bundle.js (mounted by the host as a
// wasi:filesystem preopen) and evaluates it as ESM via dynamic import.
//
// Cached: introspect is a one-shot in practice, but the contract is
// idempotent in case the host calls manifest() more than once.
//
// Before the bundle is evaluated, we register no-op JS modules for
// every `particle:*` namespace through wasm-rquickjs's module-mock
// API. The bundle's top-level `import { credentials } from
// "@partite-ai/particle-credentials"` (etc.) resolves to one of these stubs, so
// the user's source loads even though no host instance is wired —
// per design doc §3, introspect never calls a handler, so the
// stubbed methods are never invoked.
// -----------------------------------------------------------------------------

declare const __wasm_rquickjs_register_module_mock: (
  specifier: string,
  options: { namedExports?: Record<string, unknown>; defaultExport?: unknown },
) => unknown;

const PARTICLE_NAMESPACE_STUBS: Record<string, string[]> = {
  "@partite-ai/particle-credentials": ["fetcher", "getRaw"],
  "@partite-ai/particle-oauth":       ["refresh"],
  "@partite-ai/particle-signing":     ["sign", "verify"],
  "@partite-ai/particle-kv":          ["get", "set", "delete", "list"],
};

let stubsRegistered = false;

function registerParticleStubs(): void {
  if (stubsRegistered) return;
  stubsRegistered = true;
  // The mock methods exist solely to satisfy module resolution.
  // If a handler ever ran during introspect (it shouldn't), it'd
  // hit one of these and the thrown error makes the contract
  // violation loud — quieter than letting handlers see no-op
  // values that look "successful".
  const trap = () => {
    throw new Error("particle:* APIs are not callable during introspect");
  };
  for (const [specifier, methods] of Object.entries(PARTICLE_NAMESPACE_STUBS)) {
    const namespaceObject: Record<string, unknown> = {};
    for (const m of methods) {
      namespaceObject[m] = trap;
    }
    // The named export key is the bit after the host prefix —
    // e.g., `@partite-ai/particle-credentials` exports
    // `credentials`.
    const exportName = specifier.slice("@partite-ai/particle-".length);
    __wasm_rquickjs_register_module_mock(specifier, {
      namedExports: { [exportName]: namespaceObject },
    });
  }
}

let cachedParticle: UserParticle | null = null;
let loadError: Error | null = null;

async function loadParticle(): Promise<UserParticle> {
  if (cachedParticle) return cachedParticle;
  if (loadError) throw loadError;

  try {
    registerParticleStubs();
    // wasm-rquickjs resolves dynamic imports against wasi:filesystem
    // preopens — the host mounts the bundle at /particle/bundle.js.
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const mod = await (import("/particle/bundle.js") as Promise<any>);
    if (!mod || typeof mod.default === "undefined") {
      throw new Error("bundle has no default export");
    }
    cachedParticle = mod.default as UserParticle;
    return cachedParticle;
  } catch (e) {
    loadError = e instanceof Error ? e : new Error(String(e));
    throw loadError;
  }
}

// -----------------------------------------------------------------------------
// Manifest validation. Build errors out of any structural mismatch — these
// surface to the user with a clean "build failed: manifest-extract" wrapper.
// -----------------------------------------------------------------------------

// Note: strict SemVer validation lives Go-side (internal/semver,
// shared by the build pipeline and the registry) so the rule
// can't drift between JS and Go versions of the same regex. We
// still require version to be a non-empty string here — that's
// the in-bundle "did the author put SOMETHING here" check.

function validateAndSerialize(p: UserParticle): string {
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

  const tools: Array<{ name: string; description: string; inputSchema: object }> = [];
  for (const [name, def] of Object.entries(p.tools)) {
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
    tools.push({
      name,
      description: def.description,
      inputSchema: def.inputSchema,
    });
  }

  return JSON.stringify({
    name: p.name,
    description: p.description,
    version: p.version,
    capabilities: p.capabilities ?? {},
    credentials: p.credentials ?? {},
    tools,
  });
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

function errMessage(e: unknown): string {
  if (e instanceof Error) return e.message;
  return String(e);
}

// -----------------------------------------------------------------------------
// Exports
// -----------------------------------------------------------------------------

export const introspect = {
  async manifest(): Promise<string> {
    let particle: UserParticle;
    try {
      particle = await loadParticle();
    } catch (e) {
      throw bundleLoadError(errMessage(e));
    }
    try {
      return validateAndSerialize(particle);
    } catch (e) {
      throw invalidManifest(errMessage(e));
    }
  },
};
