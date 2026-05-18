package kv

import (
	"context"
	"errors"
	"fmt"

	"github.com/partite-ai/wacogo"
	"github.com/partite-ai/wacogo/host"

	gen "github.com/partite-ai/particles/internal/host/gen/particle/host/kv"
)

// Manager produces wacogo host instances satisfying
// `import particle:host/kv@0.1.0`. The Manager owns the
// host.Component template; per-particle [Store] views are passed
// in to [Manager.NewInstance] so the same template can serve
// every particle the host hosts.
//
// Lifecycle: build with [NewManager], use across particles, [Close]
// when done. Closing the wacogo.Engine is also sufficient.
type Manager struct {
	fac *gen.Factory
}

// ManagerConfig is the input to [NewManager].
type ManagerConfig struct {
	// Engine is the wacogo runtime the host instance is built
	// against. Required.
	Engine *wacogo.Engine
}

// NewManager creates a Manager and pre-builds the underlying
// host.Component template.
func NewManager(ctx context.Context, cfg ManagerConfig) (*Manager, error) {
	if cfg.Engine == nil {
		return nil, errors.New("kv: NewManager: Engine is required")
	}
	fac, err := gen.NewFactory(ctx, cfg.Engine)
	if err != nil {
		return nil, fmt.Errorf("kv: build factory: %w", err)
	}
	return &Manager{fac: fac}, nil
}

// Close releases the host.Component template. Existing instances
// remain callable until each is individually closed; closing the
// wacogo.Engine cascades and closes everything.
func (m *Manager) Close(ctx context.Context) error { return m.fac.Close(ctx) }

// NewInstance produces a host instance backed by the supplied
// (particle-scoped) Store. Pass `inst.Core()` to
// `wacogo.WithInstanceImport(...)`. Additional `opts` are
// forwarded to wacogo's Instantiate — the runtime uses this to
// attach a host.CallListener for CPU metering.
func (m *Manager) NewInstance(ctx context.Context, store Store, opts ...host.InstantiateOption) (*host.ComponentInstance, error) {
	if store == nil {
		return nil, errors.New("kv: NewInstance: store is required")
	}
	inst, err := m.fac.NewInstance(ctx, newAdapter(store), nil, opts...)
	if err != nil {
		return nil, fmt.Errorf("kv: instantiate: %w", err)
	}
	return inst, nil
}

// -----------------------------------------------------------------------------
// adapter
// -----------------------------------------------------------------------------

type adapter struct {
	store Store
}

var _ gen.Kv = (*adapter)(nil)

func newAdapter(store Store) *adapter {
	return &adapter{store: store}
}

func (a *adapter) Get(ctx context.Context, key string) (gen.ResultOptionStringKvError, error) {
	value, found, err := a.store.Get(ctx, key)
	if err != nil {
		return gen.ResultOptionStringKvErrorErr{Value: liftError(err)}, nil
	}
	if !found {
		return gen.ResultOptionStringKvErrorOk{Value: gen.NoneString()}, nil
	}
	return gen.ResultOptionStringKvErrorOk{Value: gen.SomeString(value)}, nil
}

func (a *adapter) Set(ctx context.Context, key, value string) (gen.Result_KvError, error) {
	if err := a.store.Set(ctx, key, value); err != nil {
		return gen.Result_KvErrorErr{Value: liftError(err)}, nil
	}
	return gen.Result_KvErrorOk{}, nil
}

func (a *adapter) Delete(ctx context.Context, key string) (gen.Result_KvError, error) {
	if err := a.store.Delete(ctx, key); err != nil {
		return gen.Result_KvErrorErr{Value: liftError(err)}, nil
	}
	return gen.Result_KvErrorOk{}, nil
}

func (a *adapter) List(ctx context.Context, prefix string) (gen.ResultListStringKvError, error) {
	keys, err := a.store.List(ctx, prefix)
	if err != nil {
		return gen.ResultListStringKvErrorErr{Value: liftError(err)}, nil
	}
	if keys == nil {
		// gen marshalling tolerates nil slices, but emit an
		// empty slice for predictability — particles always
		// see `[]`, never the JS quirk of `undefined`.
		keys = []string{}
	}
	return gen.ResultListStringKvErrorOk{Value: keys}, nil
}

// liftError maps a Store error to a kv-error variant case.
//
//	ErrQuotaExceeded (or wrappers) → quota-exceeded
//	any other error                → storage-error(err.Error())
func liftError(err error) gen.KvError {
	if errors.Is(err, ErrQuotaExceeded) {
		return gen.KvErrorQuotaExceeded{}
	}
	return gen.KvErrorStorageError{Value: err.Error()}
}
