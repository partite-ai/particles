package credentials

import (
	"context"
	"errors"
)

// ErrIntrospectMode is the sentinel returned by the trap Store when
// any credentials operation is attempted during a `get-manifest`
// call. The build pipeline wires this store into the runtime so
// module-scope host calls inside a user's bundle surface a clear,
// recognizable signal.
var ErrIntrospectMode = errors.New("credentials: host capabilities are not allowed during get-manifest")

// NewIntrospectTrapStore returns a [Store] whose every method
// returns [ErrIntrospectMode]. The build pipeline uses this when
// instantiating the runtime just to call particle:runtime/manifest.
// A particle that calls `credentials.fetcher("github")` at module
// scope sees the explicit "not allowed during get-manifest" error.
func NewIntrospectTrapStore() Store { return introspectTrap{} }

type introspectTrap struct{}

func (introspectTrap) GetByID(ctx context.Context, id string) (Descriptor, error) {
	return Descriptor{}, ErrIntrospectMode
}
func (introspectTrap) GetByName(ctx context.Context, name string) (Descriptor, error) {
	return Descriptor{}, ErrIntrospectMode
}
func (introspectTrap) List(ctx context.Context) ([]ListEntry, error) {
	return nil, ErrIntrospectMode
}
func (introspectTrap) Put(ctx context.Context, name, method string, meta Metadata, secrets ...Secret) (Descriptor, error) {
	return Descriptor{}, ErrIntrospectMode
}
func (introspectTrap) Delete(ctx context.Context, id string) error { return ErrIntrospectMode }
func (introspectTrap) ConfiguredMethod(ctx context.Context, name string) (string, error) {
	return "", ErrIntrospectMode
}
func (introspectTrap) ReadSecret(ctx context.Context, id string, role SecretRole) ([]byte, error) {
	return nil, ErrIntrospectMode
}
func (introspectTrap) WriteSecrets(ctx context.Context, id string, secrets ...Secret) error {
	return ErrIntrospectMode
}
func (introspectTrap) DeleteSecret(ctx context.Context, id string, role SecretRole) error {
	return ErrIntrospectMode
}
