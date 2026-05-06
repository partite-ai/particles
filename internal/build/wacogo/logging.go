package wacogo

import (
	"context"

	wc "github.com/partite-ai/wacogo"
	"github.com/partite-ai/wacogo/host"
)

// loggingInterfaceName is the WIT name used by the wasi-logging
// proposal as wasm-rquickjs imports it. The proposal predates the
// versioned wasi:io scheme, so the interface ships unversioned.
const loggingInterfaceName = "wasi:logging/logging"

// newLoggingStub returns a host instance that satisfies
// wasi:logging/logging with a no-op `log` function. The QuickJS engine
// inside particle-runtime.wasm and particle-introspect.wasm imports
// this interface for `console.*`-style diagnostics; for build-time
// usage we discard everything.
//
// A future iteration can route messages to an io.Writer (e.g., the
// build CLI's stderr) by reading the strings off the canonical-ABI
// stack and writing through cc.Memory().
func newLoggingStub(ctx context.Context, e *wc.Engine) (*host.ComponentInstance, error) {
	b := e.NewHostBuilder(loggingInterfaceName)

	// enum level { trace, debug, info, warn, error, critical }
	levelType := b.AddType("level", host.Enum{Cases: []string{
		"trace", "debug", "info", "warn", "error", "critical",
	}})

	// log: func(level: level, context: string, message: string)
	b.AddFunction("log", &host.FuncType{
		Params: []host.Param{
			{Name: "level", Type: levelType},
			{Name: "context", Type: host.String},
			{Name: "message", Type: host.String},
		},
	}, func(_ context.Context, _ *host.CallContext, _ *host.ComponentInstance, _ []uint64) error {
		// Silent drop: build phases must not pollute stdout/stderr with
		// QuickJS engine diagnostics — they belong in a real logger
		// once we add one.
		return nil
	})

	comp, err := b.Build(ctx)
	if err != nil {
		return nil, err
	}
	return comp.Instantiate(ctx)
}
