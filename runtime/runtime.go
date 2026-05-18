// Package runtime hosts the particle JS runtime — the QuickJS-based
// WASM component that executes a particle's tool handlers.
//
// A [Runtime] owns the engine, the loaded runtime.wasm, and host
// capability managers (credentials + kv). Hosts spin up
// [Particle]s from the runtime — one per particle they want to
// serve — and invoke ListTools / CallTool / Ping on each.
//
// Capability checks are enforced per-particle:
//
//   - The build phase (internal/importscan) already rejects
//     particles whose source imports a `particle:*` capability they
//     don't declare in their manifest.
//
//   - The runtime additionally requires the host to provide the
//     matching Manager for any declared capability. Constructing a
//     Particle whose manifest declares `credentials` against a
//     [Config] without a Credentials manager fails at NewParticle
//     time, before any tool call lands.
//
//   - For undeclared capabilities, the runtime still wires the
//     manager (the WIT imports must be satisfied) but the Store is
//     the host's normal Store, so unknown credential names surface
//     as `not-configured` and unknown kv keys as `not-found` —
//     defense in depth on top of the build's import check.
//
// The runtime.wasm artifact is embedded into the binary at compile
// time; see runtime/embed and the //go:generate directive in
// doc.go.
package runtime

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/partite-ai/wacogo"
	wasihttp "github.com/partite-ai/wacogo/wasi/http/types"

	"github.com/partite-ai/particles/credentials"
	"github.com/partite-ai/particles/kv"
)

// HTTPDoer is the interface the runtime accepts for handling a
// particle's outbound HTTP requests. *http.Client satisfies it
// (its `Do` method matches), as does any custom transport the
// host wants to inject — useful for stubbing in tests,
// inspecting / mutating requests, or routing through a proxy.
//
// Type-aliased from wacogo's wasi.HTTPDoer so callers don't have
// to pull wacogo into their import set just to satisfy
// [Config.HTTPClient].
type HTTPDoer = wasihttp.HTTPDoer

//go:embed all:embed
var embeddedRuntime embed.FS

const embeddedRuntimePath = "embed/particle-runtime.wasm"

// Config configures a [Runtime].
type Config struct {
	// Engine is the wacogo runtime particles are built against.
	// Required.
	Engine *wacogo.Engine

	// Credentials supplies host-side credential storage (the
	// particle:host/{credentials,oauth,signing} interfaces).
	// Required: the runtime.wasm imports those interfaces
	// unconditionally, and they must be satisfied at
	// instantiation.
	Credentials *credentials.Manager

	// KV supplies the host-side key/value store
	// (particle:host/kv). Required for the same reason.
	KV *kv.Manager

	// HTTPClient handles a particle's outbound HTTP requests
	// after the per-particle policy (allowed-hosts gate +
	// credential substitution) has done its work. nil →
	// [http.DefaultClient].
	//
	// Use the override to plug in retry middleware, an HTTP/2
	// transport tuned for long-lived connections, or a recording
	// proxy for tests.
	HTTPClient HTTPDoer

	// Log, if non-nil, receives every wasi:logging/log call a
	// particle makes (i.e., the destination of `console.*`).
	// nil → drop. Useful when embedding the runtime in a host
	// that already has its own structured-log sink: log into
	// your own logger here and per-particle output threads
	// through without per-particle wiring.
	//
	// Callbacks run inline while the wasm guest is paused; keep
	// them cheap and non-blocking. Errors aren't reported back
	// to the guest (wasi:logging/log has no result type).
	Log LogCallback
}

// Runtime is the entry point: hosts construct one per
// (engine, credentials, kv) tuple and spin Particles off of it.
//
// One Runtime owns one wacogo Component (the loaded runtime.wasm)
// and the four host-capability factories the runtime imports. Per-
// particle state lives entirely in the [Particle] handle.
type Runtime struct {
	cfg     Config
	comp    *wacogo.Component
	logging *wacogoInstanceCloser
}

// New constructs a Runtime, loading the embedded runtime.wasm.
// Validates that the required dependencies are present so a missing
// manager produces an error here rather than at the first
// NewParticle call.
func New(ctx context.Context, cfg Config) (*Runtime, error) {
	if cfg.Engine == nil {
		return nil, errors.New("runtime: New: Engine is required")
	}
	if cfg.Credentials == nil {
		return nil, errors.New("runtime: New: Credentials manager is required")
	}
	if cfg.KV == nil {
		return nil, errors.New("runtime: New: KV manager is required")
	}

	data, err := embeddedRuntime.ReadFile(embeddedRuntimePath)
	if err != nil {
		return nil, fmt.Errorf("runtime: embedded runtime wasm missing — run `go generate ./runtime/` (or `make runtime-embed`) to build it: %w", err)
	}
	comp, err := cfg.Engine.LoadComponent(ctx, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("runtime: load runtime wasm: %w", err)
	}

	logging, err := newLoggingHost(ctx, cfg.Engine, cfg.Log)
	if err != nil {
		return nil, fmt.Errorf("runtime: build wasi:logging host: %w", err)
	}
	// Default the HTTP client here so [Particle] construction
	// doesn't carry a per-particle "is it nil?" dance.
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}

	return &Runtime{
		cfg:     cfg,
		comp:    comp,
		logging: &wacogoInstanceCloser{inst: logging},
	}, nil
}

// Close releases runtime-scoped resources. Existing Particles
// remain callable until each is individually closed; closing the
// wacogo.Engine cascades and closes everything.
func (r *Runtime) Close(ctx context.Context) error {
	if r.logging != nil {
		return r.logging.Close(ctx)
	}
	return nil
}

// wacogoInstanceCloser is a tiny interface over *host.ComponentInstance
// just so the Runtime fields stay readable (the *host.* type lives
// in a deeper import path).
type wacogoInstanceCloser struct {
	inst interface {
		Close(context.Context) error
	}
}

func (c *wacogoInstanceCloser) Close(ctx context.Context) error {
	if c == nil || c.inst == nil {
		return nil
	}
	return c.inst.Close(ctx)
}

// stderrSinkBytes returns whatever the runtime's wasi-cli/stderr
// captured, formatted for inclusion in error messages. Used by the
// adapter wrappers when a wasm trap surfaces — without the captured
// stderr the user sees only "wasm error: unreachable".
func stderrSinkBytes(buf *bytes.Buffer) string {
	if buf == nil {
		return ""
	}
	s := buf.String()
	return strings.TrimRight(s, "\n")
}
