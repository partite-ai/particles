package credentials

import (
	"context"
	"errors"
	"fmt"

	gen "github.com/partite-ai/particles/internal/host/gen/particle/host/credentials"
	"github.com/partite-ai/particles/internal/hostmeter"
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
// adapter: implements gen.Credentials on top of a particle-scoped
// Store. One adapter per particle instance — the store is already
// pre-bound to a particle, so the adapter is purely the WIT/Go
// translation layer.
// -----------------------------------------------------------------------------

type adapter struct {
	store Store
}

var _ gen.Credentials = (*adapter)(nil)

func newAdapter(store Store) *adapter {
	return &adapter{store: store}
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
	defer hostmeter.EnterHost(ctx)()

	desc, err := a.store.GetByName(ctx, name)
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

// GetConfiguredMethod returns the method name the user configured
// for the named credential at setup, or none when no method is
// set for (particle, name). Particles that declare multiple
// alternative methods (e.g. oauth2 + apikey) call this to find
// out which method backs the named credential.
func (a *adapter) GetConfiguredMethod(ctx context.Context, name string) (gen.ResultOptionStringCredentialError, error) {
	defer hostmeter.EnterHost(ctx)()

	method, err := a.store.ConfiguredMethod(ctx, name)
	if err != nil {
		return gen.ResultOptionStringCredentialErrorErr{Value: errToCredentialError(err)}, nil
	}
	if method == "" {
		return gen.ResultOptionStringCredentialErrorOk{Value: gen.NoneString()}, nil
	}
	return gen.ResultOptionStringCredentialErrorOk{Value: gen.SomeString(method)}, nil
}

// GetRaw is the only adapter method that reads a secret — the
// `value` of a RawMeta entry is what the particle wants. Two-step:
// resolve the entry by name, then read SecretRoleValue.
func (a *adapter) GetRaw(ctx context.Context, name string) (gen.ResultStringCredentialError, error) {
	defer hostmeter.EnterHost(ctx)()

	desc, err := a.store.GetByName(ctx, name)
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
	value, err := a.store.ReadSecret(ctx, desc.ID, SecretRoleValue)
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
