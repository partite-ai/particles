package runtime

import (
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

// WasmOptions configures the wazero RuntimeConfig the particle
// host engine is built against. Pass the result of
// [NewWasmConfig] to `wacogo.WithRuntimeConfig` when constructing
// the engine.
//
// Default zero-value is "off": no memory cap, no
// CloseOnContextDone — same behavior as a stock wacogo engine.
type WasmOptions struct {
	// MemoryLimitPages caps each particle's linear memory in
	// 64KiB pages. wazero's stock default is 65536 pages
	// (≈4GiB); setting a tighter cap here is how a host
	// protects itself from a particle that tries to allocate
	// without bound. Zero leaves wazero's default in place.
	//
	// A particle that tries to grow past the cap traps with a
	// memory-growth failure, which the runtime surfaces as a
	// handler error to the caller.
	MemoryLimitPages uint32

	// CloseOnContextDone turns the CPU limiter from cooperative
	// (interruption only at the next host-call boundary) into
	// hard (wazero halts the wasm mid-instruction the moment
	// the per-call context is cancelled). Required to bound a
	// pure-wasm tight loop that never yields.
	//
	// Off by default because wazero applies non-trivial
	// per-instruction overhead — check-context-cancelled on the
	// hot path — to every running wasm whenever the runtime is
	// built with this flag, even when no budget is actually set
	// on a given call. Turn it on when the threat model
	// includes runaway guest code; leave it off when your
	// particles are trusted enough that cooperative
	// interruption is sufficient.
	CloseOnContextDone bool
}

// PageSize is the size of one wasm memory page, 64 KiB. Helper
// for callers translating user-facing byte counts to wazero's
// page-granular limit.
const PageSize = 64 * 1024

// NewWasmConfig returns a wazero RuntimeConfig configured per
// opts. Always sets the feature set the runtime's bundled wasms
// rely on (CoreFeaturesV2 + the extended-const proposal + the
// exception-handling proposal, which native CPython extension .so's
// built from C++ with -fwasm-exceptions import a tag for); everything
// else is gated on the corresponding [WasmOptions] field.
//
// Usage:
//
//	cfg := runtime.NewWasmConfig(runtime.WasmOptions{
//	    MemoryLimitPages:   1024, // 64 MiB
//	    CloseOnContextDone: true, // hard CPU-budget enforcement
//	})
//	engine := wacogo.NewEngine(ctx, wacogo.WithRuntimeConfig(cfg))
func NewWasmConfig(opts WasmOptions) wazero.RuntimeConfig {
	cfg := wazero.NewRuntimeConfig().
		WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesExtendedConst | experimental.CoreFeaturesExceptionHandling).
		WithCloseOnContextDone(opts.CloseOnContextDone)
	if opts.MemoryLimitPages > 0 {
		cfg = cfg.WithMemoryLimitPages(opts.MemoryLimitPages)
	}
	return cfg
}
