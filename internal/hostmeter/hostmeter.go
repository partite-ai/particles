// Package hostmeter is the tiny bridge between the runtime's CPU
// limiter and the host-side adapter methods that need to pause it
// while running. Lives outside the runtime / credentials / kv
// packages to avoid an import cycle: host adapters consult a
// [Meter] via context.Value and don't depend on the concrete
// limiter type.
//
// Wiring: the runtime attaches a [Meter] to the per-call context
// in armLimit, and a single stateless [Listener] is installed on
// every host-component instance at Instantiate time (and on the
// wasi world). wacogo's host.CallListener fires around every host
// function and resource destructor invocation; the listener
// consults the per-call ctx for a Meter and Pause/Resume's it.
//
// The upshot: host adapter methods don't have to call anything to
// participate in metering. There's no `defer hostmeter.EnterHost(ctx)()`
// to forget on a new method; destructors are covered automatically
// (the old EnterHost pattern couldn't reach them).
package hostmeter

import (
	"context"

	"github.com/partite-ai/wacogo/host"
)

// Meter is the slice of the runtime's CPU limiter that host
// adapters need to call. Concrete implementations are
// `runtime.limiter` (the real one) and a test fake. The
// interface is intentionally narrow; the listener MUST NOT
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
// is present.
func MeterFromContext(ctx context.Context) Meter {
	m, _ := ctx.Value(ctxKey{}).(Meter)
	return m
}

// Listener is the [host.CallListener] adapter. Stateless: install
// one instance (the zero value is ready to use) on every host
// component and on the wasi world, and it'll Pause/Resume any
// Meter attached to the per-call ctx around each host invocation.
// With no Meter on ctx, each event costs one ctx.Value lookup.
type Listener struct{}

var _ host.CallListener = Listener{}

// BeforeCall pauses the per-call meter (if any) as control
// crosses from wasm into a host function or resource destructor.
func (Listener) BeforeCall(ctx context.Context, _ *host.ComponentInstance, _ host.CallKind, _ string, _ []uint64) {
	if m := MeterFromContext(ctx); m != nil {
		m.Pause()
	}
}

// AfterCall resumes the per-call meter (if any) as control
// returns to wasm. wacogo guarantees AfterCall fires for every
// BeforeCall, including on panic — so Pause/Resume always pair up.
func (Listener) AfterCall(ctx context.Context, _ *host.ComponentInstance, _ host.CallKind, _ string, _ []uint64, _ error) {
	if m := MeterFromContext(ctx); m != nil {
		m.Resume()
	}
}
