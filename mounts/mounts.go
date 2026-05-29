// Package mounts defines the host-side per-particle store of persistent
// filesystem mount mappings.
//
// A particle declares named mounts in its manifest
// (`capabilities.filesystem.mounts`); this store records the host
// directory the user mapped each one to, so the mapping is reused on
// later runs without re-passing `--mount`. Mappings are scoped by
// particle NAME, not version — like credentials and kv — so they
// survive version upgrades.
//
// Temp mounts (`capabilities.filesystem.temp`) are never stored here:
// the host provisions them fresh each run.
//
// Concrete Store implementations live in subpackages — mounts/sqlite
// for the persistent default. There are no implementations in this
// package.
package mounts

import "context"

// Store is the host-side mount-mapping store, scoped to a single
// particle. Construct one via a backend-specific helper — e.g.
// `mounts/sqlite.(*Backend).Scoped(particle)` returns a Store that
// pre-binds the multi-particle backing store to the named particle.
//
// Implementations should be safe for concurrent use.
type Store interface {
	// Get returns the host path mapped to mount `name`. found is
	// false when the mount has no persistent mapping (the caller
	// then falls back to a --mount flag or, for a required mount,
	// errors at run time). error is reserved for storage failures.
	Get(ctx context.Context, name string) (hostPath string, found bool, err error)

	// Set creates or replaces the mapping mount `name` → hostPath.
	Set(ctx context.Context, name, hostPath string) error

	// Delete removes the mapping. Idempotent: returns nil when no
	// mapping existed.
	Delete(ctx context.Context, name string) error

	// List returns every persistent mapping for this particle,
	// ordered by mount name.
	List(ctx context.Context) ([]Mapping, error)
}

// Mapping is one persistent mount mapping: the declared mount name and
// the host directory it resolves to.
type Mapping struct {
	Name     string
	HostPath string
}
