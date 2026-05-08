// This file holds only the //go:generate directive for the runtime
// package. Package documentation lives in runtime.go.
//
// Building runtime.wasm requires the wasm-rquickjs toolchain and a
// Rust target for wasm32-wasip2; running this from a fresh checkout
// is what populates runtime/embed/particle-runtime.wasm so the
// //go:embed below picks it up.
//
//go:generate make -C ../ runtime-embed

package runtime
