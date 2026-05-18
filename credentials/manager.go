package credentials

import (
	"context"
	"errors"
	"fmt"

	"github.com/partite-ai/wacogo"
	"github.com/partite-ai/wacogo/host"

	gen_creds "github.com/partite-ai/particles/internal/host/gen/particle/host/credentials"
	gen_oauth "github.com/partite-ai/particles/internal/host/gen/particle/host/oauth"
	gen_sign "github.com/partite-ai/particles/internal/host/gen/particle/host/signing"
)

// Manager produces wacogo host instances for every host capability
// the credentials package implements — credentials, oauth, and
// signing. (kv lives in a sibling package.)
//
// The Manager owns the host.Component templates (the "wasm wiring"
// part) and the OAuth refresher; it does NOT own a Store. Per-
// particle [Store] views are passed in to each
// `NewXxxInstance` call so the same set of factories can serve
// every particle the host hosts.
//
// Lifecycle: build with [NewManager], use across particles, [Close]
// when done. Closing the wacogo.Engine is also sufficient — Manager
// state lives within it.
type Manager struct {
	engine    *wacogo.Engine
	refresher OAuthRefresher

	credFac    *gen_creds.Factory
	oauthFac   *gen_oauth.Factory
	signingFac *gen_sign.Factory
}

// ManagerConfig is the input to [NewManager].
type ManagerConfig struct {
	// Engine is the wacogo runtime the host instances will be
	// built against. Required.
	Engine *wacogo.Engine

	// Refresher performs the upstream OAuth 2.0 token refresh
	// exchange. nil → [HTTPRefresher] with the default
	// http.DefaultClient. Only consulted when the host calls
	// [Manager.NewOAuthInstance] or [Manager.RotateAccessToken].
	Refresher OAuthRefresher
}

// NewManager creates a Manager and pre-builds the underlying
// per-capability host.Component templates. Each template
// build is independent; failure to build any capability fails
// construction and releases what was built.
func NewManager(ctx context.Context, cfg ManagerConfig) (*Manager, error) {
	if cfg.Engine == nil {
		return nil, errors.New("credentials: NewManager: Engine is required")
	}

	credFac, err := gen_creds.NewFactory(ctx, cfg.Engine)
	if err != nil {
		return nil, fmt.Errorf("credentials: build credentials factory: %w", err)
	}
	oauthFac, err := gen_oauth.NewFactory(ctx, cfg.Engine)
	if err != nil {
		_ = credFac.Close(ctx)
		return nil, fmt.Errorf("credentials: build oauth factory: %w", err)
	}
	signingFac, err := gen_sign.NewFactory(ctx, cfg.Engine)
	if err != nil {
		_ = credFac.Close(ctx)
		_ = oauthFac.Close(ctx)
		return nil, fmt.Errorf("credentials: build signing factory: %w", err)
	}

	refresher := cfg.Refresher
	if refresher == nil {
		refresher = &HTTPRefresher{}
	}

	return &Manager{
		engine:     cfg.Engine,
		refresher:  refresher,
		credFac:    credFac,
		oauthFac:   oauthFac,
		signingFac: signingFac,
	}, nil
}

// RotateAccessToken performs an OAuth 2.0 token refresh for the
// credential identified by `id` in `store` and writes the rotated
// secrets back. Returns the new access-token bundle (Token +
// Type + ExpiresAt) so the caller doesn't need a follow-up
// ReadSecret.
//
// Exposed so the wasi:http policy can proactively refresh tokens
// approaching their ExpiresAt before substituting them into
// outbound requests — saving each particle's tool code from
// having to implement a refresh-on-401 retry. Errors come from
// missing/wrong-type metadata, an empty refresh token, refresher
// failure, or store write failure; callers decide whether to fail
// the operation, fall through to the stale token, or surface
// somewhere else.
func (m *Manager) RotateAccessToken(ctx context.Context, store Store, id string) (AccessToken, error) {
	return rotateAccessToken(ctx, store, m.refresher, id)
}

// Close releases the host.Component templates for every
// capability. Existing instances built from the Manager remain
// callable until each is individually closed; closing the
// wacogo.Engine cascades and closes everything.
//
// Errors from each capability are joined with errors.Join.
func (m *Manager) Close(ctx context.Context) error {
	var errs []error
	if err := m.credFac.Close(ctx); err != nil {
		errs = append(errs, fmt.Errorf("close credentials factory: %w", err))
	}
	if err := m.oauthFac.Close(ctx); err != nil {
		errs = append(errs, fmt.Errorf("close oauth factory: %w", err))
	}
	if err := m.signingFac.Close(ctx); err != nil {
		errs = append(errs, fmt.Errorf("close signing factory: %w", err))
	}
	return errors.Join(errs...)
}

// NewCredentialsInstance produces a host instance satisfying
// `import particle:host/credentials@0.1.0`. The instance reads
// and writes through the supplied (particle-scoped) Store. Pass
// `inst.Core()` to `wacogo.WithInstanceImport(...)`.
func (m *Manager) NewCredentialsInstance(ctx context.Context, store Store) (*host.ComponentInstance, error) {
	if store == nil {
		return nil, errors.New("credentials: NewCredentialsInstance: store is required")
	}
	inst, err := m.credFac.NewInstance(ctx, newAdapter(store), nil)
	if err != nil {
		return nil, fmt.Errorf("credentials: instantiate credentials: %w", err)
	}
	return inst, nil
}

// NewOAuthInstance produces a host instance satisfying
// `import particle:host/oauth@0.1.0`. The instance reads and
// writes through the supplied (particle-scoped) Store. Pass
// `inst.Core()` to `wacogo.WithInstanceImport(...)`.
func (m *Manager) NewOAuthInstance(ctx context.Context, store Store) (*host.ComponentInstance, error) {
	if store == nil {
		return nil, errors.New("credentials: NewOAuthInstance: store is required")
	}
	inst, err := m.oauthFac.NewInstance(ctx, newOAuthAdapter(store, m.refresher), nil)
	if err != nil {
		return nil, fmt.Errorf("credentials: instantiate oauth: %w", err)
	}
	return inst, nil
}

// NewSigningInstance produces a host instance satisfying
// `import particle:host/signing@0.1.0`. The instance reads
// and writes through the supplied (particle-scoped) Store.
// Sign / verify operate on SigningKeyMeta entries — the adapter
// looks up the entry by name, fetches the key bytes from
// SecretRoleKey, and dispatches to crypto/hmac per the entry's
// Algorithm.
func (m *Manager) NewSigningInstance(ctx context.Context, store Store) (*host.ComponentInstance, error) {
	if store == nil {
		return nil, errors.New("credentials: NewSigningInstance: store is required")
	}
	inst, err := m.signingFac.NewInstance(ctx, newSigningAdapter(store), nil)
	if err != nil {
		return nil, fmt.Errorf("credentials: instantiate signing: %w", err)
	}
	return inst, nil
}
