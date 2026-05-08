package credentials

import (
	"context"
	"errors"
	"fmt"

	"github.com/partite-ai/wacogo"
	"github.com/partite-ai/wacogo/host"

	gen_creds "github.com/partite-ai/particle/internal/host/gen/particle/host/credentials"
	gen_oauth "github.com/partite-ai/particle/internal/host/gen/particle/host/oauth"
	gen_sign "github.com/partite-ai/particle/internal/host/gen/particle/host/signing"
)

// Manager produces wacogo host instances for every host capability
// the credentials package implements — credentials, oauth, and
// signing. (kv lives in a sibling package: it doesn't need a Store.)
//
// One Manager per (engine, store, refresher) tuple, shared across
// every particle the host runs. Calling
// [Manager.NewCredentialsInstance] / [Manager.NewOAuthInstance]
// (etc.) per particle produces lightweight host.ComponentInstances
// scoped to that particle's name; the underlying host.Component
// templates are owned by the Manager and reused.
//
// Lifecycle: build with [NewManager], use across particles, [Close]
// when done. Closing the wacogo.Engine is also sufficient — Manager
// state lives within it.
type Manager struct {
	engine    *wacogo.Engine
	store     Store
	refresher OAuthRefresher

	credFac    *gen_creds.Factory
	oauthFac   *gen_oauth.Factory
	signingFac *gen_sign.Factory
}

// ManagerConfig is the input to [NewManager]. Adding new optional
// fields here (next: signing key provider, kv backend) is
// non-breaking; the only required fields are Engine and Store.
type ManagerConfig struct {
	// Engine is the wacogo runtime the host instances will be
	// built against. Required.
	Engine *wacogo.Engine

	// Store backs every capability — the credentials adapter
	// consults it for placeholders + raw lookups, the oauth
	// adapter for refresh-token reads + access-token writes.
	// Required.
	Store Store

	// Refresher performs the upstream OAuth 2.0 token refresh
	// exchange. nil → [HTTPRefresher] with the default
	// http.DefaultClient. Only consulted when the host calls
	// [Manager.NewOAuthInstance].
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
	if cfg.Store == nil {
		return nil, errors.New("credentials: NewManager: Store is required")
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
		store:      cfg.Store,
		refresher:  refresher,
		credFac:    credFac,
		oauthFac:   oauthFac,
		signingFac: signingFac,
	}, nil
}

// Store returns the Store the Manager was built around. Exposed
// for callers that need direct read access alongside the WIT-level
// host instances — e.g., the runtime's wasi:http policy reads
// secrets at substitution time without going through a host
// component.
func (m *Manager) Store() Store { return m.store }

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
// `import particle:host/credentials@0.1.0`, scoped to the named
// particle. Pass `inst.Core()` to `wacogo.WithInstanceImport(...)`.
func (m *Manager) NewCredentialsInstance(ctx context.Context, particle string) (*host.ComponentInstance, error) {
	if particle == "" {
		return nil, errors.New("credentials: NewCredentialsInstance: particle name is required")
	}
	inst, err := m.credFac.NewInstance(ctx, newAdapter(m.store, particle), nil)
	if err != nil {
		return nil, fmt.Errorf("credentials: instantiate credentials: %w", err)
	}
	return inst, nil
}

// NewOAuthInstance produces a host instance satisfying
// `import particle:host/oauth@0.1.0`, scoped to the named particle.
// Pass `inst.Core()` to `wacogo.WithInstanceImport(...)`.
func (m *Manager) NewOAuthInstance(ctx context.Context, particle string) (*host.ComponentInstance, error) {
	if particle == "" {
		return nil, errors.New("credentials: NewOAuthInstance: particle name is required")
	}
	inst, err := m.oauthFac.NewInstance(ctx, newOAuthAdapter(m.store, m.refresher, particle), nil)
	if err != nil {
		return nil, fmt.Errorf("credentials: instantiate oauth: %w", err)
	}
	return inst, nil
}

// NewSigningInstance produces a host instance satisfying
// `import particle:host/signing@0.1.0`, scoped to the named
// particle. Sign / verify operate on SigningKeyMeta entries — the
// adapter looks up the entry by name, fetches the key bytes from
// SecretRoleKey, and dispatches to crypto/hmac per the entry's
// Algorithm.
func (m *Manager) NewSigningInstance(ctx context.Context, particle string) (*host.ComponentInstance, error) {
	if particle == "" {
		return nil, errors.New("credentials: NewSigningInstance: particle name is required")
	}
	inst, err := m.signingFac.NewInstance(ctx, newSigningAdapter(m.store, particle), nil)
	if err != nil {
		return nil, fmt.Errorf("credentials: instantiate signing: %w", err)
	}
	return inst, nil
}
