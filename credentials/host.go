package credentials

import (
	"context"
	"errors"
	"fmt"

	gen "github.com/partite-ai/particle/internal/host/gen/particle/host/credentials"
)

// PlaceholderPrefix is the fixed prefix the runtime/host stamps onto
// every placeholder. The wasi:http policy recognizes outgoing
// requests carrying a `<PlaceholderPrefix><id>` token by stripping
// the prefix and resolving against the Store.
//
// The prefix is unlikely to collide with anything in real HTTP
// traffic. Within a particle's scope an ID alone uniquely
// identifies a credential, so we don't need additional entropy in
// the placeholder.
const PlaceholderPrefix = "__particle_cred_"

// -----------------------------------------------------------------------------
// adapter: implements gen.Credentials on top of Store + particle.
// One adapter per particle instance. Stateless beyond the (store,
// particle) pair — placeholders are deterministic, so no
// per-instance bookkeeping is needed.
// -----------------------------------------------------------------------------

type adapter struct {
	store    Store
	particle string
}

var _ gen.Credentials = (*adapter)(nil)

func newAdapter(store Store, particle string) *adapter {
	return &adapter{store: store, particle: particle}
}

// IDFromPlaceholder reverses the placeholder format applied here:
// strips PlaceholderPrefix and returns (id, true) if the input was
// a particle-credential placeholder; (_, false) otherwise. Useful
// for the wasi:http policy to recognize outgoing requests carrying
// a placeholder.
func IDFromPlaceholder(s string) (string, bool) {
	if len(s) <= len(PlaceholderPrefix) || s[:len(PlaceholderPrefix)] != PlaceholderPrefix {
		return "", false
	}
	return s[len(PlaceholderPrefix):], true
}

func placeholderFor(id string) string { return PlaceholderPrefix + id }

// GetPlaceholder reads metadata only — no secrets cross the
// adapter at this stage. The wasi:http policy fetches secret slots
// at substitution time, so the placeholder path stays cheap.
func (a *adapter) GetPlaceholder(ctx context.Context, name string) (gen.ResultPlaceholderInfoCredentialError, error) {
	desc, err := a.store.GetByName(ctx, a.particle, name)
	if err != nil {
		return gen.ResultPlaceholderInfoCredentialErrorErr{Value: errToCredentialError(err)}, nil
	}

	apply, ok := applySpecForMeta(desc.Meta)
	if !ok {
		return gen.ResultPlaceholderInfoCredentialErrorErr{
			Value: gen.CredentialErrorTypeMismatch{Value: typeMismatchMessageForPlaceholder(name, desc.Meta)},
		}, nil
	}

	return gen.ResultPlaceholderInfoCredentialErrorOk{
		Value: gen.PlaceholderInfo{
			Placeholder: placeholderFor(desc.ID),
			Apply:       apply,
		},
	}, nil
}

// GetConfiguredMethod returns the name of the credential method
// the user configured at setup, or none when no credential is
// set for this particle. Particles that declare multiple
// alternative authentication methods (e.g. oauth2 + apikey) call
// this to discover which one the host is holding.
//
// "Configured" means at least one entry exists in the store under
// (particle, *). When more than one is set we return the first by
// store order — the importer enforces "exactly one method" at
// setup time, so multiple-set is only reachable when the host is
// driven directly through the API.
func (a *adapter) GetConfiguredMethod(ctx context.Context) (gen.ResultOptionStringCredentialError, error) {
	entries, err := a.store.List(ctx, a.particle)
	if err != nil {
		return gen.ResultOptionStringCredentialErrorErr{Value: errToCredentialError(err)}, nil
	}
	if len(entries) == 0 {
		return gen.ResultOptionStringCredentialErrorOk{Value: gen.NoneString()}, nil
	}
	return gen.ResultOptionStringCredentialErrorOk{Value: gen.SomeString(entries[0].Name)}, nil
}

// GetRaw is the only adapter method that reads a secret — the
// `value` of a RawMeta entry is what the particle wants. Two-step:
// resolve the entry by name, then read SecretRoleValue.
func (a *adapter) GetRaw(ctx context.Context, name string) (gen.ResultStringCredentialError, error) {
	desc, err := a.store.GetByName(ctx, a.particle, name)
	if err != nil {
		return gen.ResultStringCredentialErrorErr{Value: errToCredentialError(err)}, nil
	}
	if _, ok := desc.Meta.(RawMeta); !ok {
		return gen.ResultStringCredentialErrorErr{
			Value: gen.CredentialErrorTypeMismatch{
				Value: fmt.Sprintf("getRaw is only valid for raw credentials; %q is %s", name, desc.Meta.Kind()),
			},
		}, nil
	}
	value, err := a.store.ReadSecret(ctx, a.particle, desc.ID, SecretRoleValue)
	if err != nil {
		// Both ErrNotFound and ErrSecretNotSet surface as
		// not-configured here — the entry exists but no
		// usable value is available.
		return gen.ResultStringCredentialErrorErr{Value: errToCredentialError(err)}, nil
	}
	return gen.ResultStringCredentialErrorOk{Value: string(value)}, nil
}

// applySpecForMeta returns the WIT apply-spec for metadata variants
// that participate in placeholder substitution. SigningKey and Raw
// don't — callers should surface a type-mismatch error.
func applySpecForMeta(m Metadata) (gen.ApplySpec, bool) {
	switch v := m.(type) {
	case BasicMeta:
		return gen.ApplySpec{Kind: gen.ApplyKindBasic}, true
	case OAuth2Meta:
		return gen.ApplySpec{Kind: gen.ApplyKindBearer}, true
	case APIKeyMeta:
		return liftApplySpec(v.Location), true
	}
	return gen.ApplySpec{}, false
}

func liftApplySpec(s ApplySpec) gen.ApplySpec {
	out := gen.ApplySpec{Kind: liftApplyKind(s.Kind)}
	if s.Name != "" {
		out.Name = gen.SomeString(s.Name)
	}
	if s.Scheme != "" {
		out.Scheme = gen.SomeString(s.Scheme)
	}
	return out
}

func liftApplyKind(k ApplyKind) gen.ApplyKind {
	switch k {
	case ApplyBasic:
		return gen.ApplyKindBasic
	case ApplyBearer:
		return gen.ApplyKindBearer
	case ApplyHeader:
		return gen.ApplyKindHeader
	case ApplyAuthScheme:
		return gen.ApplyKindAuthScheme
	case ApplyQueryParam:
		return gen.ApplyKindQueryParam
	}
	panic(fmt.Sprintf("credentials: unknown ApplyKind: %d", k))
}

func typeMismatchMessageForPlaceholder(name string, m Metadata) string {
	switch m.(type) {
	case SigningKeyMeta:
		return fmt.Sprintf("credential %q is a signing-key; use the signing API (sign / verify) instead of getPlaceholder", name)
	case RawMeta:
		return fmt.Sprintf("credential %q is raw; use getRaw to access its value", name)
	}
	return fmt.Sprintf("credential %q (%s) does not support placeholder substitution", name, m.Kind())
}

// errToCredentialError maps a Store error onto a credential-error
// variant case.
//
//	ErrNotFound (or any error wrapping it, including
//	  ErrSecretNotSet)                     → not-configured
//	any other error                        → storage-error(err.Error())
func errToCredentialError(err error) gen.CredentialError {
	if errors.Is(err, ErrNotFound) {
		return gen.CredentialErrorNotConfigured{}
	}
	return gen.CredentialErrorStorageError{Value: err.Error()}
}
