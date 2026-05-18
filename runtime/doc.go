// This file holds the runtime package's //go:generate
// directives. Package documentation lives in runtime.go.
//
// Two things get generated:
//
//   1. The runtime wasm itself (`make -C ../ runtime-embed`).
//      Requires the wasm-rquickjs toolchain and a Rust target
//      for wasm32-wasip2; running this from a fresh checkout
//      populates runtime/embed/particle-runtime.wasm so the
//      //go:embed in runtime.go picks it up.
//
//   2. The wasi:logging host bindings consumed by
//      runtime/logging.go. Mirrors the per-capability witgen
//      directives in credentials/doc.go and kv/doc.go;
//      we run our own here because the runtime package is the
//      only consumer of the wasi:logging adapter.
//
//go:generate make -C ../ runtime-embed
//go:generate wacogo-witgen generate -w particle:host/wasi-logging-gen -o ../internal/host/gen -p github.com/partite-ai/particles/internal/host/gen ../internal/host/wit/

package runtime
