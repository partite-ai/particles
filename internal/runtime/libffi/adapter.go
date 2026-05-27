// Package libffi implements particle:host/libffi@0.1.0 — the
// trampoline-generation interface that backs cffi/libffi-style FFI
// dispatch on wasm32-wasi.
//
// emscripten's libffi port generates marshal trampolines at runtime
// via JavaScript's WebAssembly.Function constructor; on wasi we have
// no JS host, so the runtime owns that codegen. Each unique
// target-function signature gets one tiny wasm module (a single
// `trampoline` function plus an elem segment placing it in the
// caller's __indirect_function_table at a specific slot). The
// caller then `call_indirect`s that slot via a fixed-signature
// dispatch helper (see components/python-runtime/src/libffi_dispatch.s).
//
// Code structure within this package:
//   sig.go            — sig-descriptor parser (ParsedSig, ParseSig)
//   tramp_call.go     — call-direction trampoline (libffi → C)
//   tramp_closure.go  — closure trampoline (C → Python via closure->fun)
//   encoder.go        — LEB128 + wasm-section helpers
//   adapter.go        — this file; runtime/cache/WIT-impl layer
//
// Caching: each unique sig descriptor maps to one call-direction
// trampoline + one table slot. Closures are NOT cached because each
// closure carries its own user-data + function pointer (baked into
// the trampoline as i32.const).
package libffi

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"

	"github.com/partite-ai/wacogo/host"

	gen "github.com/partite-ai/particles/internal/host/gen/particle/host/libffi"
)

// Adapter is the libffi WIT impl. One adapter per python-runtime
// particle, bound to the caller's main module via host.CallerCoreModule
// on the first dispatch.
type Adapter struct {
	runtime wazero.Runtime

	mu        sync.Mutex
	main      api.Module
	callCache map[string]uint32 // sig descriptor → table slot
}

// NewAdapter builds an empty Adapter bound to the given wazero
// runtime. The caller is expected to lazy-capture main via the
// first prep-trampoline call.
func NewAdapter(rt wazero.Runtime) *Adapter {
	return &Adapter{
		runtime:   rt,
		callCache: make(map[string]uint32),
	}
}

// Compile-time check.
var _ gen.Libffi = (*Adapter)(nil)

// PrepTrampoline implements the WIT method. Routes to prepCall.
func (a *Adapter) PrepTrampoline(ctx context.Context, sig []uint8) (gen.ResultU32LibffiError, error) {
	slot, err := a.prepCall(ctx, sig)
	if err != nil {
		return gen.ResultU32LibffiErrorErr{
			Value: gen.LibffiErrorUnsupportedSignature{Value: err.Error()},
		}, nil
	}
	return gen.ResultU32LibffiErrorOk{Value: slot}, nil
}

// prepCall caches per sig: same signature → same trampoline → same
// table slot.
func (a *Adapter) prepCall(ctx context.Context, sig []uint8) (uint32, error) {
	parsed, err := ParseSig(sig)
	if err != nil {
		return 0, fmt.Errorf("parse sig: %w", err)
	}

	key := string(sig)
	a.mu.Lock()
	if slot, ok := a.callCache[key]; ok {
		a.mu.Unlock()
		return slot, nil
	}
	a.mu.Unlock()

	main, err := a.ensureMain(ctx)
	if err != nil {
		return 0, err
	}
	slot, err := a.growAndInstantiate(ctx, main, func(slot uint32) ([]byte, error) {
		return buildCallTrampoline(parsed, slot)
	})
	if err != nil {
		return 0, fmt.Errorf("call trampoline: %w", err)
	}

	a.mu.Lock()
	a.callCache[key] = slot
	a.mu.Unlock()
	return slot, nil
}

// PrepClosure implements the WIT method. The closure pointer + slot
// are pre-supplied by the Rust libffi shim (which does __grow_table
// itself and writes the slot into closure->ftramp before calling).
// We emit the wasm trampoline module and instantiate it; the
// module's elem segment writes the trampoline funcref into the slot
// at instantiation time.
func (a *Adapter) PrepClosure(ctx context.Context, closurePtr, slot uint32, sig []uint8) (gen.Result_LibffiError, error) {
	if err := a.prepClosure(ctx, closurePtr, slot, sig); err != nil {
		return gen.Result_LibffiErrorErr{
			Value: gen.LibffiErrorUnsupportedSignature{Value: err.Error()},
		}, nil
	}
	return gen.Result_LibffiErrorOk{}, nil
}

func (a *Adapter) prepClosure(ctx context.Context, closurePtr, slot uint32, sig []byte) error {
	parsed, err := ParseSig(sig)
	if err != nil {
		return fmt.Errorf("parse closure sig: %w", err)
	}
	main, err := a.ensureMain(ctx)
	if err != nil {
		return err
	}
	bytes, err := buildClosureTrampoline(parsed, slot, closurePtr)
	if err != nil {
		return fmt.Errorf("build closure trampoline: %w", err)
	}
	return a.instantiate(ctx, main, bytes)
}

// ensureMain returns the cached main module, capturing it from the
// current host call's caller if it's not been seen yet.
func (a *Adapter) ensureMain(ctx context.Context) (api.Module, error) {
	a.mu.Lock()
	main := a.main
	a.mu.Unlock()
	if main != nil {
		return main, nil
	}
	caller := host.CallerCoreModule(ctx)
	if caller == nil {
		return nil, errors.New("libffi: no caller core module on ctx")
	}
	a.mu.Lock()
	if a.main == nil {
		a.main = caller
	}
	main = a.main
	a.mu.Unlock()
	return main, nil
}

// growAndInstantiate is the shared "allocate a slot, build module
// bytes for that slot, instantiate" sequence used by call and
// closure prep paths.
func (a *Adapter) growAndInstantiate(
	ctx context.Context,
	main api.Module,
	build func(slot uint32) ([]byte, error),
) (uint32, error) {
	growFn := main.ExportedFunction("__grow_table")
	if growFn == nil {
		return 0, errors.New("main does not export __grow_table")
	}
	results, err := growFn.Call(ctx, 1)
	if err != nil {
		return 0, fmt.Errorf("__grow_table(1): %w", err)
	}
	slot := uint32(results[0])

	bytes, err := build(slot)
	if err != nil {
		return 0, err
	}
	if err := a.instantiate(ctx, main, bytes); err != nil {
		return 0, err
	}
	return slot, nil
}

// instantiate compiles + instantiates a trampoline module. The
// module imports memory + table + __stack_pointer from main; the
// elem segment fires at instantiation and writes the trampoline
// into main's __indirect_function_table at the baked-in slot.
func (a *Adapter) instantiate(ctx context.Context, main api.Module, bytes []byte) error {
	mod, err := a.runtime.CompileModule(ctx, bytes)
	if err != nil {
		return fmt.Errorf("compile trampoline: %w", err)
	}
	resolveCtx := experimental.WithImportResolver(ctx, func(mn string) api.Module {
		if mn == "main" {
			return main
		}
		return nil
	})
	_, err = a.runtime.InstantiateModule(resolveCtx, mod,
		wazero.NewModuleConfig().WithName("").WithStartFunctions())
	if err != nil {
		return fmt.Errorf("instantiate trampoline: %w", err)
	}
	return nil
}
