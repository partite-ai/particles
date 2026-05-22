// Package nodebuiltins is the canonical list of Node-shaped modules
// wasm-rquickjs (the JS runtime) provides at execution time, plus
// the small list of npm packages we transparently shadow-fetch so
// that transitive `require(...)` calls resolve to a real package
// rather than relying on a runtime builtin that may be missing or
// the wrong shape.
//
// The build pipeline consults `Names` / `Is` in two places:
//
//   - importscan (Phase 1): bare imports of these names are allowed
//     through without an `npm:` prefix and don't get reported as deps.
//   - bundle (Phase 4): imports of these names are marked external,
//     so esbuild leaves the import statement alone instead of walking
//     into node_modules/.
//
// `ShadowNpmDeps` is consulted by the build orchestrator: every JS
// build appends these to the resolver input so the corresponding
// real npm package lands in node_modules. See `whatwg-url` v5's
// `require("punycode")` (the canonical case): the package's own
// metadata doesn't declare punycode as a dep because it expects
// Node's built-in, but we don't ship a CJS-shape-correct punycode
// to the runtime, so the shadow makes the npm package the answer.
//
// Keep `Names` in sync with components/js-runtime/build/crate/src/builtin/*.rs
// — those are the modules the runtime knows how to satisfy. Adding
// one here without a runtime impl means user code will fail at the
// first import call, not at build time.
package nodebuiltins

import "strings"

// Names is the set of Node module names the runtime ships. The
// `node:` prefix is handled separately (treated as always-external)
// so this list contains only the bare names.
var Names = map[string]struct{}{
	"assert":              {},
	"async_hooks":         {},
	"buffer":              {},
	"child_process":       {},
	"cluster":             {},
	"console":             {},
	"constants":           {},
	"crypto":              {},
	"dgram":               {},
	"diagnostics_channel": {},
	"dns":                 {},
	"domain":              {},
	"events":              {},
	"fs":                  {},
	"http":                {},
	"http2":               {},
	"https":               {},
	"inspector":           {},
	"module":              {},
	"net":                 {},
	"os":                  {},
	"path":                {},
	"perf_hooks":          {},
	"process":             {},
	"querystring":         {},
	"readline":            {},
	"repl":                {},
	"stream":              {},
	"string_decoder":      {},
	"timers":              {},
	"tls":                 {},
	"trace_events":        {},
	"tty":                 {},
	"url":                 {},
	"util":                {},
	"v8":                  {},
	"vm":                  {},
	"worker_threads":      {},
	"zlib":                {},
}

// ShadowNpmDeps maps a (bare) module name to the npm version range
// the build orchestrator transparently adds to every JS build's
// resolver input. Each entry is a Node-shaped module that the
// runtime either doesn't satisfy correctly (CJS shape mismatch,
// missing from the CJS builtinModuleMap, ...) and that the npm
// ecosystem ships as a real package. Adding to the resolver input
// makes the npm package land in `node_modules` so transitive
// `require(...)` calls find a real, correctly-shaped implementation.
//
// Order-of-magnitude small: each entry costs the package's bytes in
// every bundle that pulls it in transitively. Reserve for cases
// where shadow-fetching is genuinely cheaper than fixing the
// runtime.
var ShadowNpmDeps = map[string]string{
	// whatwg-url v5 (transitive via node-fetch / googleapis) does
	// `require("punycode")` without declaring it as a dep; in
	// modern Node setups this is shadow-installed at the top
	// level for the same reason.
	"punycode": "^2.3.0",
}

// Is reports whether spec names a runtime-provided module. Accepts:
//
//   - the bare name              ("fs", "stream")
//   - a sub-path of a known name ("fs/promises", "stream/web")
//   - any `node:` specifier      ("node:fs", "node:stream/web")
//
// The `node:` namespace is permitted wholesale — the runtime owns
// resolution under that prefix and we don't second-guess it here.
func Is(spec string) bool {
	if strings.HasPrefix(spec, "node:") {
		return true
	}
	name := spec
	if i := strings.Index(spec, "/"); i >= 0 {
		name = spec[:i]
	}
	_, ok := Names[name]
	return ok
}
