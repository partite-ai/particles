// Package nodebuiltins is the canonical list of Node-shaped modules
// wasm-rquickjs (the JS runtime) provides at execution time.
//
// The build pipeline consults `Names` / `Is` in two places:
//
//   - importscan (Phase 1): bare imports of these names are allowed
//     through without an `npm:` prefix and don't get reported as deps.
//   - bundle (Phase 4): imports of these names are marked external,
//     so esbuild leaves the import statement alone instead of walking
//     into node_modules/.
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
