package runtime

import (
	"io"
	"net/http"
	"time"
)

// ParticleOption configures one [Runtime.NewParticle] call. The
// per-particle options cover the optional knobs — the HTTP doer
// the wasi:http policy delegates to, the wasi:logging sink. The
// required per-particle stores (credentials, kv) are positional
// parameters on [Runtime.NewParticle].
type ParticleOption func(*particleConfig)

// particleConfig holds the resolved per-particle settings the
// runtime needs at instantiation time. Populated by applying every
// [ParticleOption] in order.
type particleConfig struct {
	httpClientFactory HTTPClientFactory
	log               LogCallback
	introspectMode    bool

	traceLevel  TraceLevel
	traceWriter io.Writer
}

// HTTPClientFactory builds the [HTTPDoer] the per-particle
// wasi:http policy delegates to. The factory is invoked once per
// [Runtime.NewParticle] call, after the manifest's egress policy
// is known, with that policy passed in so the returned doer can
// install [HTTPPolicy.CheckRedirect] on its http.Client (or
// equivalent) and re-validate every redirect hop against the same
// allowed-hosts set the first-hop check uses.
type HTTPClientFactory func(policy *HTTPPolicy) HTTPDoer

// WithHTTPClient overrides the [HTTPClientFactory] the runtime
// uses to build each particle's inner [HTTPDoer]. nil factory
// (the default) builds an http.Client wired with
// [HTTPPolicy.CheckRedirect], which is the safe choice — every
// redirect hop is re-checked against allowedHosts.
//
// A custom factory that returns something other than an
// http.Client (a recording shim, a Transport, a hand-rolled
// implementation) is responsible for performing equivalent
// per-hop validation. The descriptor passed in carries the
// allowed-hosts set as both [HTTPPolicy.AllowsHost] and a
// drop-in [HTTPPolicy.CheckRedirect] callback — use whichever
// fits the underlying client.
//
// The policy's allowed-hosts gate and credential substitution
// always run first; this doer sees only the already-validated
// outbound request.
func WithHTTPClient(f HTTPClientFactory) ParticleOption {
	return func(c *particleConfig) { c.httpClientFactory = f }
}

// defaultHTTPClientFactory builds an http.Client wired with
// [HTTPPolicy.CheckRedirect] so the wasi:http policy re-runs the
// allowed-hosts check on every redirect hop. This is the factory
// used when no [WithHTTPClient] override is supplied.
func defaultHTTPClientFactory(policy *HTTPPolicy) HTTPDoer {
	return &http.Client{CheckRedirect: policy.CheckRedirect}
}

// WithHTTPTrace wraps the per-particle HTTP doer with a
// [TracingHTTPDoer] that writes a record of every request and
// response to w at the given level. [TraceOff] (or a nil writer)
// disables tracing.
//
// The wrap composes with [WithHTTPClient]: if both are supplied,
// the tracer sits outside the user-supplied doer, so the user
// doer sees the request as the wasi:http policy would issue it
// and the tracer captures the same bytes. Order of WithHTTPClient
// / WithHTTPTrace in the option list is irrelevant.
func WithHTTPTrace(level TraceLevel, w io.Writer) ParticleOption {
	return func(c *particleConfig) {
		c.traceLevel = level
		c.traceWriter = w
	}
}

// WithLog routes every wasi:logging/log call this particle makes
// (the destination of console.*) to cb. Passing nil installs a
// no-op sink that drops every entry for this particle.
//
// Callbacks run inline while the guest is paused; keep them
// cheap and non-blocking. The runtime defaults the callback to
// [DefaultLogCallback] (stdlib log) when no WithLog option is
// supplied; pass an explicit no-op to silence a single particle.
func WithLog(cb LogCallback) ParticleOption {
	return func(c *particleConfig) { c.log = cb }
}

// applyParticleOptions folds the variadic ParticleOption slice
// into a populated config. Defaults are filled in after option
// application so explicit nil-passes (WithLog(nil)) are honored.
func applyParticleOptions(opts []ParticleOption) particleConfig {
	var cfg particleConfig
	cfg.log = DefaultLogCallback
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.httpClientFactory == nil {
		cfg.httpClientFactory = defaultHTTPClientFactory
	}
	return cfg
}

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
