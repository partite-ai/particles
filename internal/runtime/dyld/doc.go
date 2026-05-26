// Package dyld implements the particle:host/dyld interface — a
// runtime dynamic-loader for wasm32-wasip2 shared libraries
// (.so files following the wasm-tools dynamic-linking convention).
//
// The adapter reads .so files from a host-provided fs.FS, allocates
// memory in the caller's heap, grows the caller's
// __indirect_function_table, builds a per-load env shim module that
// satisfies the .so's `env.*` imports against the runtime's
// composed-library symbols, and instantiates the .so via wazero's
// experimental ImportResolver.
//
// Used by:
//   - components/python-runtime (Rust component that links libpython
//     dynamically and dlopen's Python C extensions at runtime)
//
// The Initialize hook is invoked at component-instantiate time by
// the inject-capture-injected core module's start function; the
// host stashes the calling module as the "captured" module that
// all subsequent env-shim imports route through.

package dyld

//go:generate wacogo-witgen generate -w particle:host/dyld-gen -o ../../host/gen -p github.com/partite-ai/particles/internal/host/gen ../../host/wit/
