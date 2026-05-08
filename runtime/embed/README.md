# Embedded runtime wasm

This directory holds the QuickJS-hosted particle runtime
(`particle-runtime.wasm`). The `.wasm` file is gitignored — run
`go generate ./runtime/` (or `make runtime-embed`) from the repo
root to build it.
