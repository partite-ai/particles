package runtime

import "time"

// ParticleOption configures a [Runtime.NewParticle] call.
//
// At present no options are exposed — the runtime derives every
// per-particle setting from the manifest. This type and its
// associated wiring is kept so callers and tests have a stable
// place to attach options as they're added.
type ParticleOption func(*particleConfig)

// particleConfig holds the resolved per-particle settings the
// runtime needs at instantiation time. Populated by applying every
// [ParticleOption] in order.
type particleConfig struct{}

// CallOption configures a single [Particle.CallTool],
// [Particle.ListTools], or [Particle.Ping] invocation. The
// pattern is variadic, so options are forward-compatible:
//
//	p.CallTool(ctx, "echo", args, runtime.WithCPUBudget(time.Second))
type CallOption func(*callConfig)

// callConfig is the resolved per-call settings.
type callConfig struct {
	// cpuBudget caps the wall-clock time wasm is allowed to
	// run during this call. Time spent inside host functions
	// (kv lookups, HTTP requests, OAuth refresh, etc.) is
	// excluded. Zero = no limit.
	cpuBudget time.Duration
}

// WithCPUBudget bounds the wasm-side wall-clock time of one
// call. When the budget is exceeded the call is interrupted by
// cancelling its context, and the call surfaces as a
// [*BudgetExceededError].
//
// Enforcement comes in two modes:
//
//   - Cooperative (default): the cancellation only takes effect
//     at the next host-call boundary. A particle stuck in
//     pure-wasm work — a tight loop that never yields to the
//     host — won't be interrupted. In practice particles call
//     out constantly (every fetch, every kv op), so this is
//     enough for the typical "runaway handler" case.
//   - Hard: wazero halts the wasm mid-instruction. Requires the
//     engine to be built with
//     [NewWasmConfig](WasmOptions{CloseOnContextDone: true}).
//     wazero applies per-instruction overhead to every running
//     wasm when this flag is set, so it's opt-in. Interrupting
//     mid-instruction also closes the underlying wasm
//     instance: the *Particle handle becomes terminal and the
//     caller must instantiate a fresh one to retry.
//
// Time spent inside host calls (the kv get, the HTTP fetch,
// the OAuth refresh, the OS keychain unseal) is NOT charged
// against the budget — only the guest's compute is metered.
//
// Zero or negative durations disable the limit entirely; the
// hot path then has no measurable overhead.
func WithCPUBudget(d time.Duration) CallOption {
	return func(c *callConfig) {
		if d > 0 {
			c.cpuBudget = d
		}
	}
}

// applyCallOptions folds the variadic options into a callConfig.
func applyCallOptions(opts []CallOption) callConfig {
	var cfg callConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}
