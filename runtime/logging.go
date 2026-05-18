package runtime

import (
	"context"
	"log"

	"github.com/partite-ai/wacogo"
	"github.com/partite-ai/wacogo/host"

	gen "github.com/partite-ai/particles/internal/host/gen/wasi/logging/logging"
)

// loggingInterfaceName is the WIT id wasm-rquickjs imports its
// console.* sink from. Re-exported from the generated bindings
// so the runtime wiring uses a single source of truth — change
// the WIT and the constant moves with it.
const loggingInterfaceName = gen.InterfaceName

// LogLevel mirrors the wasi:logging/logging `level` enum. Used
// by [LogCallback] to discriminate the severity of a log
// invocation from inside a particle.
type LogLevel string

const (
	LogLevelTrace    LogLevel = "trace"
	LogLevelDebug    LogLevel = "debug"
	LogLevelInfo     LogLevel = "info"
	LogLevelWarn     LogLevel = "warn"
	LogLevelError    LogLevel = "error"
	LogLevelCritical LogLevel = "critical"
)

// LogCallback receives every wasi:logging/log call a particle
// makes. `scope` is the WIT `context` parameter (the string the
// guest passes to identify a sub-system, e.g. "fetch"); we
// rename to avoid the obvious clash with [context.Context].
//
// Callbacks should be cheap and non-blocking — the wasm guest
// is paused for the duration. Errors aren't reported back to the
// guest (wasi:logging/log has no result type); a callback that
// fails should record the failure host-side.
type LogCallback func(ctx context.Context, level LogLevel, scope, message string)

// DefaultLogCallback writes one line per call to the standard
// library's package-level logger ([log.Default]). Used as the
// fallback when [Config.Log] is left unset; also exported so a
// host can compose it (e.g., to filter low-severity calls
// before delegating).
//
// Format: `[<level>] <scope>: <message>` — or `[<level>]
// <message>` when scope is empty, which is the common case
// when wasm-rquickjs routes console.* through.
func DefaultLogCallback(_ context.Context, level LogLevel, scope, message string) {
	if scope == "" {
		log.Printf("[%s] %s", level, message)
		return
	}
	log.Printf("[%s] %s: %s", level, scope, message)
}

// newLoggingHost returns a host instance satisfying
// wasi:logging/logging by forwarding every `log` call to cb.
// When cb is nil the host wires a no-op — wasm-rquickjs's
// bundled console.* still needs the import to be satisfied, but
// the host has no interest in the output.
//
// Goes through the generated [gen.Factory] / [loggingImpl]
// pattern so the wiring matches every other host capability;
// no hand-rolled stack decoding or memory reads on this path.
func newLoggingHost(ctx context.Context, e *wacogo.Engine, cb LogCallback) (*host.ComponentInstance, error) {
	fac, err := gen.NewFactory(ctx, e)
	if err != nil {
		return nil, err
	}
	return fac.NewInstance(ctx, &loggingImpl{cb: cb}, nil)
}

// loggingImpl implements the generated gen.Logging interface.
// Mostly a thin shim: convert the generated [gen.Level] enum to
// our public [LogLevel] string and call cb. A nil cb is a
// no-op so the import can be satisfied without forcing every
// caller to pass a handler.
type loggingImpl struct {
	cb LogCallback
}

func (l *loggingImpl) Log(ctx context.Context, level gen.Level, scope string, message string) error {
	if l.cb == nil {
		return nil
	}
	l.cb(ctx, levelFromGen(level), scope, message)
	return nil
}

// levelFromGen maps the witgen-emitted enum back to the public
// [LogLevel] string. Returns the generated [Level.String()]
// fallback for the (impossible) unknown case rather than
// dropping the call — the callback at least sees the raw level
// name and can decide what to do.
func levelFromGen(v gen.Level) LogLevel {
	switch v {
	case gen.LevelTrace:
		return LogLevelTrace
	case gen.LevelDebug:
		return LogLevelDebug
	case gen.LevelInfo:
		return LogLevelInfo
	case gen.LevelWarn:
		return LogLevelWarn
	case gen.LevelError:
		return LogLevelError
	case gen.LevelCritical:
		return LogLevelCritical
	}
	return LogLevel(v.String())
}
