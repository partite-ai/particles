// Package runtime hosts the particle JS runtime — the QuickJS-based
// WASM component that executes a particle's tool handlers.
//
// A [Runtime] owns the shared host-side state: the wacogo engine,
// the loaded runtime.wasm, and the credentials and kv Managers
// (the reusable WIT-host factory templates). Per-particle state
// — which Store backs the credentials and kv host shims, who
// handles outbound HTTP, where log lines land — is passed at
// [Runtime.NewParticle] time, the Stores as positional parameters
// and the optional knobs as [ParticleOption]s.
//
// Capability checks are enforced per-particle: the build phase
// (internal/importscan) already rejects particles whose source
// imports a particle:* capability they don't declare in their
// manifest, and NewParticle additionally requires the host to
// pass non-nil Store views for both credentials and kv.
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
// [WithHTTPClient].
type HTTPDoer = wasihttp.HTTPDoer

//go:embed all:embed
var embeddedRuntime embed.FS

const embeddedRuntimePath = "embed/particle-runtime.wasm"

// Config configures a [Runtime] — the shared, process-wide host
// state. Per-particle wiring (Stores, HTTP client, log sink) is
// passed at [Runtime.NewParticle] time.
type Config struct {
	// Engine is the wacogo runtime particles are built against.
	// Required.
	Engine *wacogo.Engine

	// Credentials owns the host-side credential, oauth, and
	// signing WIT-host factory templates. Required — runtime.wasm
	// imports those interfaces unconditionally, and they must be
	// satisfied at instantiation. Per-particle Stores are passed
	// as the credStore argument to [Runtime.NewParticle].
	Credentials *credentials.Manager

	// KV owns the host-side kv WIT-host factory template.
	// Required for the same reason as Credentials. Per-particle
	// Stores are passed as the kvStore argument to
	// [Runtime.NewParticle].
	KV *kv.Manager
}

// Runtime is the entry point: hosts construct one per
// (engine, credentials-manager, kv-manager) tuple and spin
// Particles off of it.
//
// One Runtime owns one wacogo Component (the loaded runtime.wasm)
// plus references to the shared Managers. Per-particle state lives
// entirely in the [Particle] handle.
type Runtime struct {
	cfg  Config
	comp *wacogo.Component
}

// New constructs a Runtime, loading the embedded runtime.wasm.
// Validates that the required dependencies are present so a missing
// Manager produces an error here rather than at the first
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

	return &Runtime{cfg: cfg, comp: comp}, nil
}

// Close releases runtime-scoped resources. Existing Particles
// remain callable until each is individually closed; closing the
// wacogo.Engine cascades and closes everything.
func (r *Runtime) Close(_ context.Context) error { return nil }

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
