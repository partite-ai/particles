package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/partite-ai/wacogo"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"

	"github.com/partite-ai/particles/credentials"
	credsqlite "github.com/partite-ai/particles/credentials/sqlite"
	"github.com/partite-ai/particles/kv"
	kvsqlite "github.com/partite-ai/particles/kv/sqlite"
	mountsqlite "github.com/partite-ai/particles/mounts/sqlite"
	"github.com/partite-ai/particles/registry"
	"github.com/partite-ai/particles/runtime"
)

// addHTTPTraceFlag registers `--trace-http[=basic|headers|full]`
// on cmd, writing the parsed level into *level. The flag is
// declared with NoOptDefVal="basic" so a bare `--trace-http` is
// equivalent to `--trace-http=basic`. Passing the flag with an
// invalid value defers the error to PreRunE, which runs after
// cobra parsing.
//
// All three particle-running commands (run / serve-mcp / ping)
// share this so the spelling, value set, and default behavior
// stay identical across them.
func addHTTPTraceFlag(cmd *cobra.Command, level *runtime.TraceLevel) {
	var raw string
	f := cmd.Flags().VarPF(&traceLevelValue{level: level, raw: &raw}, "trace-http", "",
		"Trace outbound HTTP to stderr at level basic|headers|full (default basic; off when unset)")
	// Bare `--trace-http` (no value) → basic.
	f.NoOptDefVal = "basic"
}

// traceLevelValue is a pflag.Value that parses --trace-http's
// argument into a runtime.TraceLevel.
type traceLevelValue struct {
	level *runtime.TraceLevel
	raw   *string
}

func (v *traceLevelValue) String() string {
	if v.raw == nil {
		return ""
	}
	return *v.raw
}
func (v *traceLevelValue) Set(s string) error {
	lvl, err := runtime.ParseTraceLevel(s)
	if err != nil {
		return err
	}
	*v.level = lvl
	*v.raw = s
	return nil
}
func (v *traceLevelValue) Type() string { return "level" }

// bootOptions bundles the per-invocation knobs the CLI passes
// down to bootParticle on top of the required db + entry +
// mounts. Each command builds one of these from its own flags;
// the zero value is a valid "no extras" configuration.
type bootOptions struct {
	// HTTPTraceLevel, when > TraceOff, wraps the per-particle
	// HTTP doer in a [runtime.TracingHTTPDoer] writing to
	// HTTPTraceWriter. The writer is required when the level
	// is non-zero — callers default it to stderr.
	HTTPTraceLevel  runtime.TraceLevel
	HTTPTraceWriter io.Writer
}

// bootParticle brings up everything needed to talk to a registered
// particle: keyring sealer, credentials store, kv store, wacogo
// engine, managers, runtime, and the particle instance itself.
//
// `warnW`, when non-nil, receives one-line warnings for non-fatal
// setup issues (e.g. wasm-cache open failed → operation continues
// without a cache). CLI commands pass `cmd.ErrOrStderr()`.
//
// `opts` carries optional per-invocation knobs (e.g. HTTP
// tracing); the zero value is "no extras".
//
// Returns the live *runtime.Particle and a teardown function that
// closes everything in reverse order. The teardown is a single
// closure (not a chain of `defer`s) so the two callers — `ping` and
// `serve-mcp` — can share the bring-up without each carrying eight
// `defer` lines.
func bootParticle(ctx context.Context, db *sql.DB, entry registry.Entry, cliMounts map[string]string, warnW io.Writer, opts bootOptions) (*runtime.Particle, func(), error) {
	credBackend, err := credsqlite.New(ctx, db, openCredentialSealer(warnW))
	if err != nil {
		return nil, nil, fmt.Errorf("credentials store: %w", err)
	}
	kvBackend, err := kvsqlite.New(ctx, db)
	if err != nil {
		return nil, nil, fmt.Errorf("kv store: %w", err)
	}
	mountBackend, err := mountsqlite.New(ctx, db)
	if err != nil {
		return nil, nil, fmt.Errorf("mounts store: %w", err)
	}

	// Persistent wasm compilation cache. Failure to open is non-
	// fatal — a particle run that can't open the cache just pays
	// the per-invocation compile cost. The cache is shared with
	// `particle build` (same dir), so a runtime.wasm compiled
	// during build is reused at run-time.
	cache, cacheErr := loadWasmCompilationCache(ctx)
	if cacheErr != nil && warnW != nil {
		fmt.Fprintln(warnW, "warning:", cacheErr)
	}

	var engineOpts []wacogo.EngineOption
	if cache != nil {
		// Mirror wacogo's default core-features set (see
		// internal/build/wacogo's NewWithOptions for context) so
		// turning the cache on doesn't subtly diverge behavior.
		cfg := wazero.NewRuntimeConfig().
			WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesExtendedConst).
			WithCompilationCache(cache)
		engineOpts = append(engineOpts, wacogo.WithRuntimeConfig(cfg))
	}
	engine := wacogo.NewEngine(ctx, engineOpts...)
	credMgr, err := credentials.NewManager(ctx, credentials.ManagerConfig{Engine: engine})
	if err != nil {
		_ = engine.Close(ctx)
		return nil, nil, fmt.Errorf("credentials manager: %w", err)
	}
	kvMgr, err := kv.NewManager(ctx, kv.ManagerConfig{Engine: engine})
	if err != nil {
		_ = credMgr.Close(ctx)
		_ = engine.Close(ctx)
		return nil, nil, fmt.Errorf("kv manager: %w", err)
	}

	rt, err := runtime.New(ctx, runtime.Config{
		Engine: engine, Credentials: credMgr, KV: kvMgr,
	})
	if err != nil {
		_ = kvMgr.Close(ctx)
		_ = credMgr.Close(ctx)
		_ = engine.Close(ctx)
		return nil, nil, fmt.Errorf("runtime: %w", err)
	}

	// Resolve the particle's declared filesystem mounts into the
	// name→FS map NewParticle wants: persistent mappings from the
	// mounts store, --mount overrides on top, and temp scratch dirs
	// provisioned fresh. mountCleanup releases os.Root handles and
	// deletes the temp dirs.
	mountMap, mountCleanup, err := resolveMounts(ctx, entry.Name, entry.Particle, mountBackend.Scoped(entry.Name), cliMounts)
	if err != nil {
		_ = rt.Close(ctx)
		_ = kvMgr.Close(ctx)
		_ = credMgr.Close(ctx)
		_ = engine.Close(ctx)
		if cache != nil {
			_ = cache.Close(ctx)
		}
		return nil, nil, err
	}

	var particleOpts []runtime.ParticleOption
	if opts.HTTPTraceLevel > runtime.TraceOff && opts.HTTPTraceWriter != nil {
		particleOpts = append(particleOpts, runtime.WithHTTPTrace(opts.HTTPTraceLevel, opts.HTTPTraceWriter))
	}

	// Scope the multi-particle backends to this particle's
	// name. The resulting per-particle Stores are what
	// NewParticle reads through; the Managers themselves stay
	// particle-agnostic.
	p, err := rt.NewParticle(ctx, entry.Particle,
		credBackend.Scoped(entry.Name),
		kvBackend.Scoped(entry.Name),
		mountMap,
		particleOpts...,
	)
	if err != nil {
		mountCleanup()
		_ = rt.Close(ctx)
		_ = kvMgr.Close(ctx)
		_ = credMgr.Close(ctx)
		_ = engine.Close(ctx)
		if cache != nil {
			_ = cache.Close(ctx)
		}
		return nil, nil, fmt.Errorf("instantiate: %w", err)
	}

	teardown := func() {
		_ = p.Close(ctx)
		mountCleanup()
		_ = rt.Close(ctx)
		_ = kvMgr.Close(ctx)
		_ = credMgr.Close(ctx)
		_ = engine.Close(ctx)
		if cache != nil {
			_ = cache.Close(ctx)
		}
	}
	return p, teardown, nil
}

// resolveParticle parses a "<name>[@version]" target and returns
// the registered entry, resolving an omitted version to the
// highest semver.
func resolveParticle(ctx context.Context, reg registry.Registry, target string) (registry.Entry, error) {
	name, version := parsePingTarget(target)
	if name == "" {
		return registry.Entry{}, errors.New("name is required")
	}
	if version == "" {
		v, err := resolveLatestVersion(ctx, reg, name)
		if err != nil {
			return registry.Entry{}, err
		}
		version = v
	}
	entry, err := reg.Get(ctx, name, version)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return registry.Entry{}, fmt.Errorf("%s@%s not registered", name, version)
		}
		return registry.Entry{}, fmt.Errorf("lookup: %w", err)
	}
	return entry, nil
}
