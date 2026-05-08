package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/partite-ai/wacogo"

	"github.com/partite-ai/particle/credentials"
	credsqlite "github.com/partite-ai/particle/credentials/sqlite"
	"github.com/partite-ai/particle/kv"
	kvsqlite "github.com/partite-ai/particle/kv/sqlite"
	"github.com/partite-ai/particle/registry"
	"github.com/partite-ai/particle/runtime"
)

// bootParticle brings up everything needed to talk to a registered
// particle: keyring sealer, credentials store, kv store, wacogo
// engine, managers, runtime, and the particle instance itself.
//
// Returns the live *runtime.Particle and a teardown function that
// closes everything in reverse order. The teardown is a single
// closure (not a chain of `defer`s) so the two callers — `ping` and
// `serve-mcp` — can share the bring-up without each carrying eight
// `defer` lines.
func bootParticle(ctx context.Context, db *sql.DB, entry registry.Entry) (*runtime.Particle, func(), error) {
	sealer, err := credsqlite.NewKeyringSealer(keyringService, keyringName)
	if err != nil {
		return nil, nil, fmt.Errorf("keyring: %w", err)
	}
	credStore, err := credsqlite.New(ctx, db, sealer)
	if err != nil {
		return nil, nil, fmt.Errorf("credentials store: %w", err)
	}
	kvStore, err := kvsqlite.New(ctx, db)
	if err != nil {
		return nil, nil, fmt.Errorf("kv store: %w", err)
	}

	engine := wacogo.NewEngine(ctx)
	credMgr, err := credentials.NewManager(ctx, credentials.ManagerConfig{
		Engine: engine, Store: credStore,
	})
	if err != nil {
		_ = engine.Close(ctx)
		return nil, nil, fmt.Errorf("credentials manager: %w", err)
	}
	kvMgr, err := kv.NewManager(ctx, kv.ManagerConfig{
		Engine: engine, Store: kvStore,
	})
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

	p, err := rt.NewParticle(ctx, entry.Particle, runtime.WithSelectedAuthenticationMethod(entry.SelectedAuthenticationMethod))
	if err != nil {
		_ = rt.Close(ctx)
		_ = kvMgr.Close(ctx)
		_ = credMgr.Close(ctx)
		_ = engine.Close(ctx)
		return nil, nil, fmt.Errorf("instantiate: %w", err)
	}

	teardown := func() {
		_ = p.Close(ctx)
		_ = rt.Close(ctx)
		_ = kvMgr.Close(ctx)
		_ = credMgr.Close(ctx)
		_ = engine.Close(ctx)
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
