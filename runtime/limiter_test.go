package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/partite-ai/wacogo/host"

	"github.com/partite-ai/particles/internal/hostmeter"
)

// Zero budget → every operation is a no-op fast path. No
// goroutine spawns, no context wrap, Used/Tripped report zero
// values. Pins the cheap-when-disabled contract.
func TestLimiter_ZeroBudget_NoOp(t *testing.T) {
	l := newLimiter(0)
	parent := context.Background()
	ctx := l.Start(parent)
	if ctx != parent {
		t.Errorf("Start returned a derived ctx for budget=0; want parent unchanged")
	}
	l.Pause()
	l.Resume()
	if got := l.Used(); got != 0 {
		t.Errorf("Used = %v, want 0", got)
	}
	if l.Tripped() {
		t.Error("Tripped = true for budget=0")
	}
	if got := l.Stop(); got != 0 {
		t.Errorf("Stop = %v, want 0", got)
	}
}

// Pause/Resume cleanly excludes intermediate time from the
// accumulated total. The exact numbers depend on the OS
// scheduler — we assert ranges rather than exact values so a
// loaded CI machine doesn't flake the test.
func TestLimiter_PauseExcludesHostTime(t *testing.T) {
	l := newLimiter(time.Hour) // budget high enough not to trip
	ctx := l.Start(context.Background())
	defer l.Stop()
	_ = ctx

	// Run a tiny segment, pause for a longer one (the "host
	// call"), then run another tiny segment. Total Used should
	// be ~2× the running segments, not 3× including the pause.
	const runSegment = 20 * time.Millisecond
	const hostSegment = 100 * time.Millisecond
	time.Sleep(runSegment)
	l.Pause()
	time.Sleep(hostSegment)
	l.Resume()
	time.Sleep(runSegment)

	used := l.Used()
	lower := time.Duration(float64(2*runSegment) * 0.7) // -30% slop
	upper := 2*runSegment + hostSegment/4               // generous high bound
	if used < lower || used > upper {
		t.Errorf("Used = %v, want roughly %v (run-segments only, host-segment excluded)", used, 2*runSegment)
	}
}

// The watchdog cancels the per-call context the moment the
// budget is hit, and Tripped() reports that the cancellation
// came from us (not from the parent context).
func TestLimiter_Watchdog_TripsAndCancels(t *testing.T) {
	const budget = 25 * time.Millisecond
	l := newLimiter(budget)
	ctx := l.Start(context.Background())
	defer l.Stop()

	// Block on ctx until the watchdog cancels.
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not cancel within 2s; budget was 25ms")
	}
	if !l.Tripped() {
		t.Error("Tripped = false after watchdog cancel; want true")
	}
	if got := l.Used(); got < budget {
		t.Errorf("Used = %v, want >= budget %v", got, budget)
	}
}

// A watchdog cancel that happens while the limiter is paused
// (host call in flight) does NOT trip — the budget hasn't been
// exhausted by guest compute, the watchdog should keep waiting.
// Pins the "host-time is free" contract.
func TestLimiter_PausedTimeDoesNotTrip(t *testing.T) {
	const budget = 30 * time.Millisecond
	l := newLimiter(budget)
	ctx := l.Start(context.Background())
	defer l.Stop()

	// Use ~5ms of guest time, then pause for 200ms (well past
	// the budget), then resume for ~5ms more. Total real time
	// = 210ms, but Used should be ~10ms — under budget.
	time.Sleep(5 * time.Millisecond)
	l.Pause()
	time.Sleep(200 * time.Millisecond)
	l.Resume()
	time.Sleep(5 * time.Millisecond)

	if l.Tripped() {
		t.Errorf("Tripped = true after 210ms wall but only ~10ms of running-time; ctx.Err = %v", ctx.Err())
	}
}

// Stop after the watchdog already fired must be safe; the
// channel-close in Stop is the synchronization point. Without
// the idempotency guard, a second Stop call would close `done`
// twice and panic.
func TestLimiter_Stop_AfterTrip_Idempotent(t *testing.T) {
	l := newLimiter(15 * time.Millisecond)
	l.Start(context.Background())
	time.Sleep(60 * time.Millisecond) // give the watchdog time to fire
	if !l.Tripped() {
		t.Fatal("watchdog did not trip within 60ms")
	}
	_ = l.Stop()
	_ = l.Stop() // must not panic
}

// hostmeter.Listener looks up the limiter via context and runs
// Pause/Resume around the host-call body. This test wires a
// real limiter into a ctx and asserts the meter contract: time
// spent inside a faked "host call" doesn't count.
func TestHostmeterListener_PausesAndResumes(t *testing.T) {
	l := newLimiter(time.Hour)
	ctx := l.Start(context.Background())
	ctx = hostmeter.WithMeter(ctx, l)
	defer l.Stop()

	listener := hostmeter.Listener{}

	time.Sleep(10 * time.Millisecond)
	func() {
		listener.BeforeCall(ctx, nil, host.CallKindFunction, "fake", nil)
		defer listener.AfterCall(ctx, nil, host.CallKindFunction, "fake", nil, nil)
		time.Sleep(50 * time.Millisecond)
	}()
	time.Sleep(10 * time.Millisecond)

	used := l.Used()
	if used < 15*time.Millisecond || used > 40*time.Millisecond {
		t.Errorf("Used = %v, want ~20ms (the two 10ms run-segments only)", used)
	}
}

// hostmeter.Listener with no meter attached is a cheap no-op:
// a single ctx.Value lookup that returns nil. We verify it
// doesn't crash; the no-overhead claim is covered by inspection.
func TestHostmeterListener_NoMeter_NoOp(t *testing.T) {
	listener := hostmeter.Listener{}
	listener.BeforeCall(context.Background(), nil, host.CallKindFunction, "fake", nil)
	listener.AfterCall(context.Background(), nil, host.CallKindFunction, "fake", nil, nil)
}

// Pause/Resume from multiple goroutines must not race. Builds a
// limiter with a wide budget and hammers Pause/Resume from N
// goroutines — run with `-race` to catch a missing mutex.
func TestLimiter_Concurrent_PauseResume_NoRace(t *testing.T) {
	l := newLimiter(time.Hour)
	l.Start(context.Background())
	defer l.Stop()

	const goroutines = 16
	const iters = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				l.Pause()
				l.Resume()
				_ = l.Used()
			}
		}()
	}
	wg.Wait()
}

// IsBudgetExceeded matches the concrete type even through
// wrapping (errors.As semantics). The CLI / MCP server use this
// to surface a specific exit message.
func TestIsBudgetExceeded(t *testing.T) {
	err := &BudgetExceededError{Op: "call-tool", Budget: 10 * time.Millisecond, Used: 12 * time.Millisecond}
	if !IsBudgetExceeded(err) {
		t.Error("IsBudgetExceeded(*BudgetExceededError) = false")
	}
	wrapped := errWrap{inner: err}
	if !IsBudgetExceeded(wrapped) {
		t.Error("IsBudgetExceeded through wrap = false")
	}
	if IsBudgetExceeded(errors.New("something else")) {
		t.Error("IsBudgetExceeded(random) = true")
	}
}

type errWrap struct{ inner error }

func (e errWrap) Error() string { return "wrapped: " + e.inner.Error() }
func (e errWrap) Unwrap() error { return e.inner }
