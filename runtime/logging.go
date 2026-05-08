package runtime

import (
	"context"

	"github.com/partite-ai/wacogo"
	"github.com/partite-ai/wacogo/host"
)

// loggingInterfaceName is the WIT id wasm-rquickjs imports its
// console.* sink from. Predates the versioned wasi:io scheme, so
// it ships unversioned.
const loggingInterfaceName = "wasi:logging/logging"

// newLoggingStub returns a host instance satisfying
// wasi:logging/logging with a no-op `log` function. The runtime's
// `console.error` calls land here; we discard everything for
// Phase 1 (a future iteration routes to a host-supplied
// `io.Writer`).
func newLoggingStub(ctx context.Context, e *wacogo.Engine) (*host.ComponentInstance, error) {
	b := e.NewHostBuilder(loggingInterfaceName)
	levelType := b.AddType("level", host.Enum{Cases: []string{
		"trace", "debug", "info", "warn", "error", "critical",
	}})
	b.AddFunction("log", &host.FuncType{
		Params: []host.Param{
			{Name: "level", Type: levelType},
			{Name: "context", Type: host.String},
			{Name: "message", Type: host.String},
		},
	}, func(_ context.Context, _ *host.CallContext, _ *host.ComponentInstance, _ []uint64) error {
		return nil
	})
	comp, err := b.Build(ctx)
	if err != nil {
		return nil, err
	}
	return comp.Instantiate(ctx)
}
