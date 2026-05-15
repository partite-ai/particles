// Package hostmeter is the tiny bridge between the runtime's CPU
// limiter and the host-side adapter methods that need to pause it
// while running. Lives outside the runtime / credentials / kv
// packages to avoid an import cycle: host adapters consult a
// [Meter] via context.Value and don't depend on the concrete
// limiter type.
//
// Usage in a host adapter:
//
//	func (a *adapter) DoThing(ctx context.Context, ...) (..., error) {
//	    defer hostmeter.EnterHost(ctx)()
//	    ...
//	}
//
// The deferred close runs Resume() right before the function
// returns to wasm. When no Meter is attached to ctx — the common
// case while a budget isn't set — EnterHost returns a no-op so
// the cost is one ctx.Value lookup.
package hostmeter

import "context"

// Meter is the slice of the runtime's CPU limiter that host
// adapters need to call. Concrete implementations are
// `runtime.limiter` (the real one) and a test fake. The
// interface is intentionally narrow; host adapters MUST NOT
// reach for anything else on the value.
type Meter interface {
	// Pause records that wasm has yielded into a host call —
	// the clock should stop counting against the particle's
	// budget until Resume is called.
	Pause()

	// Resume is called immediately before control returns to
	// wasm. Restarts the clock.
	Resume()
}

type ctxKey struct{}

// WithMeter attaches m to ctx. Returns ctx unchanged when m is
// nil — there's no reason to wrap a context just to pass a nil
// the lookups have to skip anyway.
func WithMeter(ctx context.Context, m Meter) context.Context {
	if m == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, m)
}

// MeterFromContext returns the attached Meter, or nil when none
// is present. Exported for tests; production adapters should
// prefer [EnterHost], which already nil-handles.
func MeterFromContext(ctx context.Context) Meter {
	m, _ := ctx.Value(ctxKey{}).(Meter)
	return m
}

// EnterHost signals that wasm has yielded into a host call,
// pausing the meter (if any), and returns a function that
// Resume()s the meter. The pattern is:
//
//	defer hostmeter.EnterHost(ctx)()
//
// No-op when ctx carries no meter. The returned func is always
// safe to invoke; the nil-meter case is a closure that does
// nothing.
func EnterHost(ctx context.Context) func() {
	m := MeterFromContext(ctx)
	if m == nil {
		return noopResume
	}
	m.Pause()
	return m.Resume
}

func noopResume() {}
