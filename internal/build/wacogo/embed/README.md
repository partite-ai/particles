# Embedded build-pipeline wasms

This directory holds the three WASM components the build pipeline embeds:

- `deno-npm.wasm` — Phase 2 npm dep resolver
- `particle-typecheck.wasm` — Phase 3 TypeScript checker
- `particle-introspect.wasm` — Phase 5 manifest extractor

The artifacts are committed so a fresh `go build` / `go install`
produces a working binary without first provisioning the Rust + npm +
wasm-rquickjs toolchain.

To rebuild them from source (after a component source change), run
`go generate ./internal/build/wacogo/` — or `make embed` — from the
repo root. Commit the regenerated files alongside the source change.
