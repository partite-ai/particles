# Embedded runtime wasm

This directory holds the QuickJS-hosted particle runtime
(`particle-runtime.wasm`). The artifact is committed so a fresh
`go build` / `go install` produces a working binary without first
provisioning the Rust + wasm-rquickjs toolchain.

To rebuild it from source (after a runtime source change), run
`go generate ./runtime/` — or `make runtime-embed` — from the repo
root. Commit the regenerated file alongside the source change.
