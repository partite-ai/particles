# Embedded runtime wasms

This directory holds the two engines the host can instantiate per
particle, picked by the manifest's `runtime` field:

  - `particle-js-runtime.wasm`     — QuickJS via wasm-rquickjs
  - `particle-python-runtime.wasm` — CPython via componentize-py

Both also implement `particle:runtime/manifest.get-manifest` (the
build pipeline's Phase 5 — no separate introspect component).

The artifacts are committed so a fresh `go build` / `go install`
produces a working binary without first provisioning the
wasm-rquickjs / componentize-py toolchains.

To rebuild them from source (after a runtime source change), run
`go generate ./runtime/` — or `make runtime-embed` — from the repo
root. Commit the regenerated files alongside the source change.
