# Embedded build-pipeline wasms

This directory holds the three WASM components the build pipeline embeds:

- `deno-npm.wasm` — Phase 2 npm dep resolver
- `particle-typecheck.wasm` — Phase 3 TypeScript checker
- `particle-introspect.wasm` — Phase 5 manifest extractor

The `.wasm` files are gitignored. Run `go generate ./internal/build/wacogo/`
(or `make embed`) from the repo root to build them and populate this dir.
