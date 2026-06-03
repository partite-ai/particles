package kv

import (
	"context"
	"errors"
)

// ErrIntrospectMode is the sentinel returned by the trap Store when
// any kv operation is attempted during a `get-manifest` call. See
// credentials.ErrIntrospectMode for the rationale; the kv module
// follows the same pattern.
var ErrIntrospectMode = errors.New("kv: host capabilities are not allowed during get-manifest")

// NewIntrospectTrapStore returns a [Store] whose every method
// returns [ErrIntrospectMode]. Wired by the build pipeline when
// instantiating the runtime for get-manifest only.
func NewIntrospectTrapStore() Store { return introspectTrap{} }

type introspectTrap struct{}

func (introspectTrap) Get(ctx context.Context, key string) (string, bool, error) {
	return "", false, ErrIntrospectMode
}
func (introspectTrap) Set(ctx context.Context, key, value string) error { return ErrIntrospectMode }
func (introspectTrap) Delete(ctx context.Context, key string) error     { return ErrIntrospectMode }
func (introspectTrap) List(ctx context.Context, prefix string) ([]string, error) {
	return nil, ErrIntrospectMode
}

// NewDeniedTrapStore returns a [Store] whose every method returns
// [ErrNotDeclared]. The runtime wires this in place of a real
// [Store] when the particle's manifest does not declare the kv
// capability — the WIT-level `particle:host/kv@0.1.0` import is
// always satisfied at instantiation, so undeclared particles see a
// typed `kv-error::not-declared` from every call rather than
// quietly using a Store the manifest never asked for.
func NewDeniedTrapStore() Store { return deniedTrap{} }

type deniedTrap struct{}

func (deniedTrap) Get(ctx context.Context, key string) (string, bool, error) {
	return "", false, ErrNotDeclared
}
func (deniedTrap) Set(ctx context.Context, key, value string) error { return ErrNotDeclared }
func (deniedTrap) Delete(ctx context.Context, key string) error     { return ErrNotDeclared }
func (deniedTrap) List(ctx context.Context, prefix string) ([]string, error) {
	return nil, ErrNotDeclared
}
