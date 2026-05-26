;; init-dyld sibling module composed alongside main.wasm via
;; `wasm-tools component link --dl-openable`. Its sole purpose is
;; to re-export the WIT-imported `dyld.initialize` function under a
;; plain name (`__init_dyld`) that the inject-capture-injected core
;; module can resolve directly. The re-export is DIRECT (no wrapper
;; function): the export entry points at the import's function
;; index, with no intervening body.
;;
;; Why this matters: when the inject module's start function calls
;; `__init_dyld`, the `call` instruction is in the inject module's
;; body — wazero/wacogo's `host.CallerCoreModule(ctx)` for the
;; resulting cross-component call returns the inject module, which
;; is exactly what the host adapter wants to capture (it re-exports
;; the union of $main + main.wasm).
;;
;; A Rust shim like `fn __init_dyld() { dyld::initialize() }` would
;; NOT work — its body would contain the cross-component `call`, so
;; CallerCoreModule would resolve to main.wasm. wasm-ld can't emit a
;; direct re-export of an import without a wrapper, llvm's wasm-asm
;; rejects quoted import names with colons/slashes/`@`, and clang's
;; `--export=NAME` flag only force-exports defined symbols. wat is
;; the cleanest path: hand-write the exact module we want and let
;; `wasm-tools component link` compose it.
;;
;; Imports from the canonical particle:host/dyld package — the
;; interface served by internal/runtime/dyld.
(module
  (import "particle:host/dyld@0.1.0" "initialize" (func $init))
  (export "__init_dyld" (func $init))
)
