package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/partite-ai/particles/internal/build"
)

// newBuildReporter picks the right reporter for the build command
// based on the --verbose flag and whether the destination is a
// terminal. The destination is typically `cmd.ErrOrStderr()`.
//
//	verbose=true                   → verboseReporter (timestamped log lines)
//	verbose=false, isatty(w)=true  → prettyReporter (animated spinner + bar)
//	verbose=false, isatty(w)=false → prettyReporter in plain-line fallback mode
func newBuildReporter(w io.Writer, verbose bool) build.Reporter {
	if verbose {
		return newVerboseReporter(w)
	}
	return newPrettyReporter(w)
}

// -----------------------------------------------------------------------------
// pretty reporter
// -----------------------------------------------------------------------------

// prettyReporter renders a build as one line per phase: an animated
// spinner while the phase runs, replaced in place by a checkmark and
// summary on completion. Phases that emit PhaseProgress events get a
// progress bar appended to the same line.
//
// When the underlying writer is not a TTY, the reporter falls back to
// plain text — one line on PhaseStart, one line on PhaseDone — so
// piped output (CI logs, redirects) stays readable.
type prettyReporter struct {
	w     io.Writer
	isTTY bool

	mu sync.Mutex

	// active phase state. Zero-valued when no phase is in progress.
	active     bool
	phase      build.Phase
	phaseStart time.Time
	progress   *progressState
	frameIdx   int

	// goroutine plumbing — only used in TTY mode.
	stopTicker chan struct{}
	tickerDone chan struct{}
}

type progressState struct {
	current int
	total   int
	label   string
}

// spinnerFrames is the Braille spinner sequence used by most modern
// CLIs (cargo, npm, …). Eight frames at ~100ms gives a smooth spin.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠴", "⠦", "⠧", "⠇", "⠏"}

const (
	// tickInterval drives the spinner repaint rate. Faster than ~60ms
	// looks twitchy; slower than ~150ms looks laggy.
	tickInterval = 100 * time.Millisecond

	// barWidth is the number of cells in the progress bar. Held
	// fixed so the line layout is stable; with the spinner + phase
	// name + elapsed + counter the total stays well under 80
	// columns for the phases that emit progress.
	barWidth = 20
)

func newPrettyReporter(w io.Writer) *prettyReporter {
	return &prettyReporter{w: w, isTTY: writerIsTTY(w)}
}

func (r *prettyReporter) PhaseStart(phase build.Phase) {
	r.mu.Lock()
	r.active = true
	r.phase = phase
	r.phaseStart = time.Now()
	r.progress = nil
	r.frameIdx = 0
	r.mu.Unlock()

	if !r.isTTY {
		// Plain fallback: announce the phase start so the user has
		// some signal that work is happening. PhaseDone will print
		// the result on its own line.
		fmt.Fprintf(r.w, "  • %s starting\n", phase)
		return
	}

	// Initial paint and start the ticker goroutine.
	r.repaint()
	r.stopTicker = make(chan struct{})
	r.tickerDone = make(chan struct{})
	go r.tickLoop(r.stopTicker, r.tickerDone)
}

func (r *prettyReporter) PhaseProgress(phase build.Phase, current, total int, label string) {
	r.mu.Lock()
	if r.phase != phase {
		r.mu.Unlock()
		return
	}
	r.progress = &progressState{current: current, total: total, label: label}
	r.mu.Unlock()

	if !r.isTTY {
		return
	}
	r.repaint()
}

func (r *prettyReporter) PhaseDetail(build.Phase, string, ...any) {
	// Pretty mode keeps the surface single-line per phase. Detail
	// events are the verbose mode's job.
}

func (r *prettyReporter) PhaseDone(phase build.Phase, summary string) {
	r.mu.Lock()
	elapsed := time.Since(r.phaseStart)
	r.active = false
	r.progress = nil
	stop := r.stopTicker
	done := r.tickerDone
	r.stopTicker = nil
	r.tickerDone = nil
	r.mu.Unlock()

	if stop != nil {
		close(stop)
		<-done
	}

	if r.isTTY {
		// Erase the spinner line and emit the final summary.
		fmt.Fprint(r.w, clearLine)
	}
	fmt.Fprintf(r.w, "  ✓ %-18s %s  %s\n", phase, summary, formatDuration(elapsed))
}

// repaint writes the current spinner/progress line over the previous
// paint. Caller must NOT hold r.mu.
func (r *prettyReporter) repaint() {
	r.mu.Lock()
	if !r.active {
		r.mu.Unlock()
		return
	}
	frame := spinnerFrames[r.frameIdx%len(spinnerFrames)]
	r.frameIdx++
	phase := r.phase
	elapsed := time.Since(r.phaseStart)
	progress := r.progress
	r.mu.Unlock()

	var b strings.Builder
	b.WriteString(clearLine)
	fmt.Fprintf(&b, "  %s %-18s %s", frame, phase, formatDuration(elapsed))
	if progress != nil && progress.total > 0 {
		fmt.Fprintf(&b, "  %s %d/%d %s",
			renderBar(progress.current, progress.total),
			progress.current, progress.total, progress.label)
	}
	_, _ = io.WriteString(r.w, b.String())
}

func (r *prettyReporter) tickLoop(stop, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			r.repaint()
		}
	}
}

// -----------------------------------------------------------------------------
// verbose reporter
// -----------------------------------------------------------------------------

// verboseReporter prints every phase event as a timestamped log line.
// No animation, no escape codes — safe to redirect to a file or CI log.
//
// Per-item progress counters (PhaseProgress) are dropped: the build's
// counter-bound phases also emit a PhaseDetail per item, which already
// gives the verbose log a line per unit of work.
type verboseReporter struct {
	w          io.Writer
	buildStart time.Time

	mu         sync.Mutex
	phaseStart time.Time
}

func newVerboseReporter(w io.Writer) *verboseReporter {
	return &verboseReporter{w: w, buildStart: time.Now()}
}

func (r *verboseReporter) PhaseStart(phase build.Phase) {
	r.mu.Lock()
	r.phaseStart = time.Now()
	r.mu.Unlock()
	fmt.Fprintf(r.w, "[%s] %s: starting\n", r.elapsed(), phase)
}

func (r *verboseReporter) PhaseProgress(build.Phase, int, int, string) {
	// PhaseDetail events from the same loop already provide a
	// per-unit log line; the counter is redundant here.
}

func (r *verboseReporter) PhaseDetail(phase build.Phase, format string, args ...any) {
	fmt.Fprintf(r.w, "[%s] %s: %s\n", r.elapsed(), phase, fmt.Sprintf(format, args...))
}

func (r *verboseReporter) PhaseDone(phase build.Phase, summary string) {
	r.mu.Lock()
	phaseElapsed := time.Since(r.phaseStart)
	r.mu.Unlock()
	fmt.Fprintf(r.w, "[%s] %s: done — %s (%s)\n", r.elapsed(), phase, summary, formatDuration(phaseElapsed))
}

func (r *verboseReporter) elapsed() string {
	d := time.Since(r.buildStart)
	return fmt.Sprintf("%6.2fs", d.Seconds())
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

// clearLine is the ANSI sequence to move to column 0 and clear the
// line. Used by the pretty reporter to redraw the spinner in place.
const clearLine = "\r\033[2K"

// writerIsTTY returns true when w is an *os.File pointing at a
// terminal. Anything else (a buffer, an io.MultiWriter, a pipe,
// /dev/null) returns false so the pretty reporter falls back to
// plain-line output.
func writerIsTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// renderBar draws a fixed-width filled/empty progress bar. Uses block
// characters that line up to a uniform width in any monospace font.
func renderBar(current, total int) string {
	if total <= 0 {
		return strings.Repeat("░", barWidth)
	}
	filled := current * barWidth / total
	if filled > barWidth {
		filled = barWidth
	}
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled) + "]"
}

// formatDuration prints elapsed time as a short, fixed-shape string.
// Sub-minute durations render as "X.Ys"; longer ones promote to
// "MmSSs" to stay terse.
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	m := int(d / time.Minute)
	s := int((d % time.Minute) / time.Second)
	return fmt.Sprintf("%dm%02ds", m, s)
}
