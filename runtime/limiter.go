package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/partite-ai/particle/internal/hostmeter"
)

// BudgetExceededError is returned from CallTool / ListTools /
// Ping when a CPU budget was set and the particle's accumulated
// wasm-side time exceeded it. The host-side context was cancelled
// to interrupt the call — any in-flight host call sees
// ctx.Done() and returns early.
type BudgetExceededError struct {
	// Budget is the limit that was configured.
	Budget time.Duration
	// Used is the accumulated wasm-side time at the moment the
	// watchdog tripped. Always >= Budget by construction.
	Used time.Duration
	// Op names the entry-point that was running (call-tool,
	// list-tools, ping). Useful diagnostic when several happened
	// in a row.
	Op string
}

func (e *BudgetExceededError) Error() string {
	return fmt.Sprintf("runtime: %s exceeded CPU budget (used %s, budget %s)", e.Op, e.Used, e.Budget)
}

// IsBudgetExceeded reports whether err is (or wraps) a
// *BudgetExceededError. Useful for callers that want to surface
// the limit specifically.
func IsBudgetExceeded(err error) bool {
	var b *BudgetExceededError
	return errors.As(err, &b)
}

// limiter is the per-call CPU clock. Built with [newLimiter];
// started on the wasm entry path and stopped when control
// returns. Pause/Resume are wired at every host-call boundary
// (see internal/hostmeter), so host-side latency (HTTP, KV
// reads, OAuth refresh, OS keychain unseal) never counts against
// the particle's budget.
//
// Zero budget short-circuits every method: no goroutine, no
// mutex contention, no allocations on the hot path. The runtime
// builds a limiter unconditionally when WithCPUBudget is passed
// but skips construction otherwise.
type limiter struct {
	budget time.Duration

	mu       sync.Mutex
	running  bool // true between Resume (or Start) and the next Pause
	runStart time.Time
	elapsed  time.Duration

	done    chan struct{}
	cancel  context.CancelFunc
	stopped bool
	tripped bool // set when the watchdog cancelled the context
}

// newLimiter builds a limiter with the given budget. budget=0
// means "no limit" — Pause/Resume are no-ops, Start returns the
// parent context unchanged, and no goroutine spins.
func newLimiter(budget time.Duration) *limiter {
	return &limiter{budget: budget}
}

// armLimit is the call-site one-liner: build a limiter from
// `opts`, start it against parent, attach it to the context as
// a [hostmeter.Meter] so host adapters can call Pause/Resume.
// Returns the child context, the limiter (nil when the budget
// is 0), and a cleanup func that Stops it. Pattern:
//
//	ctx, lim, stop := armLimit(ctx, opts)
//	defer stop()
//	results, err := fn.Call(ctx, ...)
//	if err != nil && lim.tripped() { return budgetExceeded(...) }
//
// Nil limiter is intentional: callers don't have to nil-check
// before using it in [BudgetExceededError] because they only
// enter that branch when err != nil AND the limiter exists.
func armLimit(parent context.Context, opts []CallOption) (context.Context, *limiter, func()) {
	cfg := applyCallOptions(opts)
	if cfg.cpuBudget == 0 {
		return parent, nil, func() {}
	}
	lim := newLimiter(cfg.cpuBudget)
	ctx := lim.Start(parent)
	ctx = hostmeter.WithMeter(ctx, lim)
	return ctx, lim, func() { lim.Stop() }
}

// Start arms the watchdog and begins tracking. Returns a child
// of parent that the watchdog will cancel when the budget is
// exceeded. With budget=0 the parent is returned unchanged.
func (l *limiter) Start(parent context.Context) context.Context {
	if l.budget == 0 {
		return parent
	}
	ctx, cancel := context.WithCancel(parent)
	l.mu.Lock()
	l.running = true
	l.runStart = time.Now()
	l.cancel = cancel
	l.done = make(chan struct{})
	l.mu.Unlock()
	go l.watchdog()
	return ctx
}

// Stop finalizes tracking and reports total elapsed running
// time. Idempotent — safe to call multiple times (e.g., from a
// `defer` plus an explicit call on the success path).
func (l *limiter) Stop() time.Duration {
	if l.budget == 0 {
		return 0
	}
	l.mu.Lock()
	if l.stopped {
		used := l.elapsed
		l.mu.Unlock()
		return used
	}
	l.stopped = true
	if l.running {
		l.elapsed += time.Since(l.runStart)
		l.running = false
	}
	used := l.elapsed
	close(l.done)
	l.mu.Unlock()
	if l.cancel != nil {
		l.cancel()
	}
	return used
}

// Pause stops the clock when wasm yields into a host call.
// No-op when the limiter is unarmed (budget=0) or already
// paused.
func (l *limiter) Pause() {
	if l.budget == 0 {
		return
	}
	l.mu.Lock()
	if l.running {
		l.elapsed += time.Since(l.runStart)
		l.running = false
	}
	l.mu.Unlock()
}

// Resume restarts the clock as control returns to wasm. No-op
// when unarmed or not currently paused. Skips re-arming if
// Stop has already finalized the limiter — late host-call
// returns after a budget trip don't put the clock back on.
func (l *limiter) Resume() {
	if l.budget == 0 {
		return
	}
	l.mu.Lock()
	if !l.stopped && !l.running {
		l.running = true
		l.runStart = time.Now()
	}
	l.mu.Unlock()
}

// Used reports the current accumulated wasm-side time without
// pausing. Used by [Particle.CallTool] etc. to decide whether
// the budget tripped after the wasm call returns with an error.
func (l *limiter) Used() time.Duration {
	if l.budget == 0 {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	used := l.elapsed
	if l.running {
		used += time.Since(l.runStart)
	}
	return used
}

// watchdogInterval is how often the watchdog polls accumulated
// time. 10ms gives a worst-case ~10ms overshoot past the budget;
// faster polling buys more precision at the cost of CPU churn
// for every metered call.
const watchdogInterval = 10 * time.Millisecond

// Tripped reports whether the watchdog cancelled the context
// because the budget was exhausted. Distinguishes "the parent
// context was cancelled by something else" from "we ran out of
// budget" — the call paths surface the budget-trip case as a
// *BudgetExceededError.
func (l *limiter) Tripped() bool {
	if l.budget == 0 {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.tripped
}

// watchdog runs in its own goroutine. Polls accumulated time
// every watchdogInterval; cancels the per-call context the
// moment the budget is hit. Exits cleanly when Stop closes
// `done`.
func (l *limiter) watchdog() {
	ticker := time.NewTicker(watchdogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-l.done:
			return
		case <-ticker.C:
			if l.Used() >= l.budget {
				l.mu.Lock()
				l.tripped = true
				cancel := l.cancel
				l.mu.Unlock()
				if cancel != nil {
					cancel()
				}
				return
			}
		}
	}
}
