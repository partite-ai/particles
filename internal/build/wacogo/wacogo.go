// Package wacogo wires the build pipeline's WASM-backed phases to the
// wacogo runtime. It owns the lifecycle of the build-time wasm
// components — deno-npm.wasm, pip-resolve.wasm, particle-typecheck.wasm
// — and exposes a narrow Go API the build orchestrator drives directly:
// TypeCheck, ResolveAndFetch, PipResolveAndFetch, ExtractManifest.
//
// Component loads are lazy: each per-phase wasm goes through embed-
// FS read → zstd decompress → wacogo.Engine.LoadComponent only on
// first use. A `particle build` for JS never pays the price of
// pip-resolve / python-runtime; a `particle build --component foo`
// pays for neither typecheck nor deno-npm. The host-capability
// managers stay eager because their setup is cheap (binding
// generation, no wasm load).
//
// Manifest extraction goes through the runtime's
// particle:runtime/manifest export (see ExtractManifest, which
// delegates to runtime.Runtime.IntrospectParticle); the build wasms
// embedded here cover only the npm/pip/typecheck phases.
//
// The wasm artifacts are baked into the binary via go:embed (see
// embed/). Run `go generate ./internal/build/wacogo/` (or `make embed`
// from the repo root) to populate that directory before the first
// build.
package wacogo

//go:generate make -C ../../.. embed

import (
	"context"
	"embed"
	"fmt"

	wc "github.com/partite-ai/wacogo"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"

	"github.com/partite-ai/particles/credentials"
	"github.com/partite-ai/particles/internal/embedzstd"
	"github.com/partite-ai/particles/kv"
	"github.com/partite-ai/particles/runtime"
)

// Options configure a Components value at construction time.
type Options struct {
	// CompilationCache, when non-nil, persists compiled wasm modules
	// across Engine lifetimes. wazero hashes each module's bytes;
	// re-loading the same module finds the prior compile in the
	// cache and skips translation.
	//
	// Pluggable on purpose. The CLI builds a disk-backed cache in
	// the user cache dir; library callers either leave this nil
	// (no caching — the current default) or supply their own
	// implementation (e.g. `wazero.NewCompilationCache()` for an
	// in-memory cache shared across multiple Build calls in a long-
	// lived host, or a custom storage backend).
	CompilationCache wazero.CompilationCache
}

// embeddedWasm holds the three build-pipeline wasms baked into the
// binary at compile time. The wasms are committed under embed/ so a
// fresh `go build` produces a working binary without the Rust + npm +
// wasm-rquickjs toolchain on hand; `make embed` (driven by the
// //go:generate directive above) regenerates them after a component
// source change.
//
// `all:embed` is used so the directive succeeds even if the .wasm
// files are absent (e.g., someone wiped them locally) — readEmbedded
// reports a clear error at runtime in that case.
//
//go:embed all:embed
var embeddedWasm embed.FS

// Embedded paths point at zstd-compressed .wasm.zst artifacts —
// tools/embedcompress writes them, internal/embedzstd reads them.
const (
	embeddedDenoNpm    = "embed/deno-npm.wasm.zst"
	embeddedPipResolve = "embed/pip-resolve.wasm.zst"
	embeddedTypecheck  = "embed/particle-typecheck.wasm.zst"
)

// Components owns the lifecycle of the wasm engine + lazy-loaded
// per-phase components + the host-capability managers. Construct
// with New, drive via the TypeCheck / ResolveAndFetch /
// PipResolveAndFetch / ExtractManifest methods, Close when done.
type Components struct {
	engine *wc.Engine

	// Lazy per-phase wasm components. Each LazyComponent loads its
	// .wasm.zst on first Get and caches the parsed result; phases
	// that aren't reached during a given invocation never pay the
	// decompress + parse cost.
	denoNpm    *embedzstd.LazyComponent
	pipResolve *embedzstd.LazyComponent
	typecheck  *embedzstd.LazyComponent

	// Capability managers + runtime. Built eagerly in New —
	// manager setup is cheap (binding generation), and constructing
	// runtime.Runtime is cheap because its wasms are lazy too.
	// What's deferred is the actual wasm load, not the Go-side
	// scaffolding.
	credentials *credentials.Manager
	kv          *kv.Manager
	runtime     *runtime.Runtime
}

// New constructs a Components value with default options (no
// compilation cache). Equivalent to NewWithOptions(ctx, Options{}).
func New(ctx context.Context) (*Components, error) {
	return NewWithOptions(ctx, Options{})
}

// NewWithOptions constructs a Components value with the given options.
// Spins up the wasm engine (configured with opts.CompilationCache if
// set), records each per-phase wasm's lazy cell, and builds the
// host-capability managers + runtime. No wasm load happens here —
// the first call to TypeCheck / ResolveAndFetch / PipResolveAndFetch
// / ExtractManifest pays the load cost for the wasms it touches
// (and writes their compiled forms to the cache, if configured).
func NewWithOptions(ctx context.Context, opts Options) (*Components, error) {
	var engineOpts []wc.EngineOption
	if opts.CompilationCache != nil {
		// Mirror wacogo's default core-features set; if we omit
		// it, the cache-enabled config doesn't get the extended-
		// const proposal and ends up subtly behavior-divergent
		// from the no-cache path.
		cfg := wazero.NewRuntimeConfig().
			WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesExtendedConst | experimental.CoreFeaturesExceptionHandling).
			WithCompilationCache(opts.CompilationCache)
		engineOpts = append(engineOpts, wc.WithRuntimeConfig(cfg))
	}
	e := wc.NewEngine(ctx, engineOpts...)
	c := &Components{
		engine:     e,
		denoNpm:    embedzstd.NewLazyComponent(embeddedDenoNpm),
		pipResolve: embedzstd.NewLazyComponent(embeddedPipResolve),
		typecheck:  embedzstd.NewLazyComponent(embeddedTypecheck),
	}

	// Stand up the host-capability managers and the runtime. The
	// managers register the `particle:host/*` WIT bindings against
	// the engine — needed before any particle instance is built.
	// runtime.New itself is cheap; its wasms (js-runtime, python-
	// runtime) are lazy and load on first IntrospectParticle /
	// NewParticle for the relevant kind.
	credMgr, err := credentials.NewManager(ctx, credentials.ManagerConfig{Engine: e})
	if err != nil {
		_ = c.Close(ctx)
		return nil, fmt.Errorf("wacogo: build credentials manager: %w", err)
	}
	c.credentials = credMgr

	kvMgr, err := kv.NewManager(ctx, kv.ManagerConfig{Engine: e})
	if err != nil {
		_ = c.Close(ctx)
		return nil, fmt.Errorf("wacogo: build kv manager: %w", err)
	}
	c.kv = kvMgr

	rt, err := runtime.New(ctx, runtime.Config{
		Engine:      e,
		Credentials: credMgr,
		KV:          kvMgr,
	})
	if err != nil {
		_ = c.Close(ctx)
		return nil, fmt.Errorf("wacogo: build runtime: %w", err)
	}
	c.runtime = rt

	return c, nil
}

// Close releases the underlying engine, which in turn frees every
// loaded component. Safe to call multiple times.
func (c *Components) Close(ctx context.Context) error {
	if c.engine == nil {
		return nil
	}
	if c.runtime != nil {
		_ = c.runtime.Close(ctx)
		c.runtime = nil
	}
	if c.credentials != nil {
		_ = c.credentials.Close(ctx)
		c.credentials = nil
	}
	if c.kv != nil {
		_ = c.kv.Close(ctx)
		c.kv = nil
	}
	err := c.engine.Close(ctx)
	c.engine = nil
	return err
}

// loadEmbedded resolves a lazy component. The first call materializes
// (read + decompress + parse); subsequent calls return the cached
// result. The wrapper exists only to surface the "run go generate"
// hint on a missing-embed error.
func (c *Components) loadEmbedded(ctx context.Context, lc *embedzstd.LazyComponent) (*wc.Component, error) {
	comp, err := lc.Get(ctx, embeddedWasm, c.engine)
	if err != nil {
		return nil, fmt.Errorf("wacogo: embedded component missing — run `go generate ./internal/build/wacogo/` (or `make embed`) to build it: %w", err)
	}
	return comp, nil
}
