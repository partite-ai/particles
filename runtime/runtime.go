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
	"io/fs"
	"strings"
	"sync"

	"github.com/partite-ai/wacogo"
	wasihttp "github.com/partite-ai/wacogo/wasi/http/types"

	"github.com/partite-ai/particles/credentials"
	"github.com/partite-ai/particles/internal/embedzstd"
	wcdyld "github.com/partite-ai/particles/internal/host/gen/particle/host/dyld"
	wclibffi "github.com/partite-ai/particles/internal/host/gen/particle/host/libffi"
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

// Embedded paths point at zstd-compressed .wasm.zst artifacts —
// tools/embedcompress writes them, internal/embedzstd reads them.
// Compression cuts the binary's wasm footprint by ~60%; the
// decompression hit is a single allocation per file at New() time.
const (
	embeddedJSRuntimePath     = "embed/particle-js-runtime.wasm.zst"
	embeddedPythonRuntimePath = "embed/particle-python-runtime.wasm.zst"
)

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
// One Runtime carries lazy handles to the two preloaded engine
// images — the JS runtime (QuickJS via wasm-rquickjs) and the
// Python runtime (CPython via componentize-py) — plus on-demand
// loading for native-WASM particles. Each engine's wasm decompresses
// + parses only when the first particle of that kind is built or
// instantiated. Per-particle state lives entirely in the [Particle]
// handle.
type Runtime struct {
	cfg    Config
	jsComp *embedzstd.LazyComponent
	pyComp *embedzstd.LazyComponent

	// dyldFactory builds particle:host/dyld@0.1.0 instance handles —
	// one per python-runtime particle. Created lazily on first use so
	// JS-only / Wasm-only programs don't pay the binding-setup cost.
	dyldFactory     *wcdyld.Factory
	dyldFactoryOnce sync.Once
	dyldFactoryErr  error

	// libffiFactory backs particle:host/libffi@0.1.0 — the trampoline-
	// generation interface cffi-built .so files use for ffi_call
	// dispatch. Same lifecycle as dyldFactory.
	libffiFactory     *wclibffi.Factory
	libffiFactoryOnce sync.Once
	libffiFactoryErr  error
}

// dyldFactoryFor returns the shared dyld factory, initializing it on
// first call. Safe for concurrent use.
func (r *Runtime) dyldFactoryFor(ctx context.Context) (*wcdyld.Factory, error) {
	r.dyldFactoryOnce.Do(func() {
		r.dyldFactory, r.dyldFactoryErr = wcdyld.NewFactory(ctx, r.cfg.Engine)
	})
	return r.dyldFactory, r.dyldFactoryErr
}

// libffiFactoryFor mirrors dyldFactoryFor for the libffi interface.
func (r *Runtime) libffiFactoryFor(ctx context.Context) (*wclibffi.Factory, error) {
	r.libffiFactoryOnce.Do(func() {
		r.libffiFactory, r.libffiFactoryErr = wclibffi.NewFactory(ctx, r.cfg.Engine)
	})
	return r.libffiFactory, r.libffiFactoryErr
}

// preloadedComponentFor returns the shared engine image for the JS
// or Python runtimes. First call loads + decompresses + parses the
// embedded wasm; subsequent calls return the cached component.
//
// Returns nil for RuntimeWasm (which loads the component per-
// particle from the artifact FS — see [Runtime.loadWasmComponent]).
func (r *Runtime) preloadedComponentFor(ctx context.Context, rt RuntimeKind) (*wacogo.Component, error) {
	switch rt {
	case RuntimeJS:
		comp, err := r.jsComp.Get(ctx, embeddedRuntime, r.cfg.Engine)
		if err != nil {
			return nil, fmt.Errorf("runtime: load JS runtime wasm — run `go generate ./runtime/` (or `make runtime-embed`) to build it: %w", err)
		}
		return comp, nil
	case RuntimePython:
		comp, err := r.pyComp.Get(ctx, embeddedRuntime, r.cfg.Engine)
		if err != nil {
			return nil, fmt.Errorf("runtime: load Python runtime wasm — run `go generate ./runtime/` (or `make runtime-embed`) to build it: %w", err)
		}
		return comp, nil
	case RuntimeWasm:
		return nil, nil // not preloaded — loadWasmComponent handles this kind
	default:
		return nil, fmt.Errorf("runtime: unknown runtime kind %q", rt)
	}
}

// loadWasmComponent reads `particle.wasm` from the artifact FS and
// loads it as a wacogo component. Used for RuntimeWasm particles
// where the artifact IS the runtime — no shared image, the author's
// component implements particle:runtime directly.
func (r *Runtime) loadWasmComponent(ctx context.Context, particleFS fs.FS) (*wacogo.Component, error) {
	data, err := fs.ReadFile(particleFS, "particle.wasm")
	if err != nil {
		return nil, fmt.Errorf("read particle.wasm: %w", err)
	}
	comp, err := r.cfg.Engine.LoadComponent(ctx, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("load particle.wasm: %w", err)
	}
	return comp, nil
}

// New constructs a Runtime. Validates the required dependencies so
// a missing Manager produces an error here rather than at the first
// NewParticle call. No engine wasm is loaded at this point — the JS
// and Python images are lazy and decompress on first use.
func New(_ context.Context, cfg Config) (*Runtime, error) {
	if cfg.Engine == nil {
		return nil, errors.New("runtime: New: Engine is required")
	}
	if cfg.Credentials == nil {
		return nil, errors.New("runtime: New: Credentials manager is required")
	}
	if cfg.KV == nil {
		return nil, errors.New("runtime: New: KV manager is required")
	}
	return &Runtime{
		cfg:    cfg,
		jsComp: embedzstd.NewLazyComponent(embeddedJSRuntimePath),
		pyComp: embedzstd.NewLazyComponent(embeddedPythonRuntimePath),
	}, nil
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
