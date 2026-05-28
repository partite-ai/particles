package build

import (
	"fmt"
)

// Reporter receives per-phase progress events while a build runs.
//
// Build is otherwise silent; library callers leave Options.Reporter nil
// to keep it that way. The CLI wires one of two implementations: a
// TTY-aware pretty reporter (spinner + progress bar) or a verbose
// log-line reporter.
//
// The protocol is sequential — only one phase is active at a time, and
// every PhaseStart is followed by exactly one PhaseDone (on the success
// path) before the next PhaseStart. PhaseProgress and PhaseDetail can
// fire any number of times in between.
type Reporter interface {
	// PhaseStart fires once when a phase begins.
	PhaseStart(phase Phase)

	// PhaseProgress fires during a phase that iterates a known number
	// of items (e.g., unpacking N wheels). `current` ranges from 0 to
	// `total`; `label` names the unit or the current item. Reporters
	// may rate-limit or coalesce.
	PhaseProgress(phase Phase, current, total int, label string)

	// PhaseDetail fires for arbitrary in-phase events that don't map
	// to a counter — single resolutions, individual wheels, lines
	// captured from a wasm component's stderr. Pretty reporters
	// typically discard these; verbose reporters log them.
	PhaseDetail(phase Phase, format string, args ...any)

	// PhaseDone fires when the phase finishes successfully. `summary`
	// is a pre-formatted one-line description ("21 wheels, 18.3 MB").
	PhaseDone(phase Phase, summary string)
}

// nopReporter discards every event. Used when Options.Reporter is nil.
type nopReporter struct{}

func (nopReporter) PhaseStart(Phase)                      {}
func (nopReporter) PhaseProgress(Phase, int, int, string) {}
func (nopReporter) PhaseDetail(Phase, string, ...any)     {}
func (nopReporter) PhaseDone(Phase, string)               {}

// reporterFor returns Options.Reporter if set, else a nopReporter so
// build phases can fire events unconditionally.
func reporterFor(opts Options) Reporter {
	if opts.Reporter == nil {
		return nopReporter{}
	}
	return opts.Reporter
}

// phaseStart / phaseDone / phaseProgress / phaseDetail are terse
// wrappers used at phase call sites. They format the summary/detail
// strings once, here, so reporters receive a ready-to-render string
// instead of repeating fmt.Sprintf themselves.
func phaseStart(opts Options, p Phase) {
	reporterFor(opts).PhaseStart(p)
}

func phaseDone(opts Options, p Phase, format string, args ...any) {
	reporterFor(opts).PhaseDone(p, fmt.Sprintf(format, args...))
}

func phaseProgress(opts Options, p Phase, current, total int, label string) {
	reporterFor(opts).PhaseProgress(p, current, total, label)
}

func phaseDetail(opts Options, p Phase, format string, args ...any) {
	reporterFor(opts).PhaseDetail(p, format, args...)
}
