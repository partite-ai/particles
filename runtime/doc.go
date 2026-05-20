// This file holds the runtime package's //go:generate
// directives. Package documentation lives in runtime.go.
//
// Two things get generated:
//
//   1. The runtime wasms themselves (`make -C ../ runtime-embed`).
//      Builds particle-js-runtime.wasm (wasm-rquickjs) and
//      particle-python-runtime.wasm (componentize-py); a fresh
//      checkout running this populates runtime/embed/ so the
//      //go:embed in runtime.go picks them up.
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
