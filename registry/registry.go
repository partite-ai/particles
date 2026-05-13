// Package registry defines the host-side particle registry contract.
//
// A registry stores built particle artifacts indexed by (name, version)
// so the host can later look one up by name without needing to know
// where the build output landed. The build CLI registers a particle
// after a successful build (with a `--pack` flag for the alternate
// "write a .particle tarball" flow); the runtime can resolve a
// particle from the registry when starting a host instance.
//
// The Registry interface is pluggable: hosts that prefer a different
// backend (filesystem layout, OCI layer, KMS-tracked, …) can supply
// their own implementation. Concrete implementations live in
// subpackages — registry/sqlite for the persistent default. There
// are no implementations in this package.
//
// Registration is the boundary at which a particle is forced through
// configuration: the CLI validates that every credential the
// manifest declares has been provisioned in the credentials store
// before calling [Registry.Put]. The registry itself does not carry
// a "configured" bit — by the time a particle is in the registry,
// it's expected to be runnable.
package registry

import (
	"context"
	"errors"
	"io/fs"
)

// Registry stores particle artifacts indexed by (name, version).
//
// Implementations should be safe for concurrent use.
type Registry interface {
	// Put stores particle's full FS contents under (name, version),
	// replacing any prior entry for the same key. The caller is
	// expected to have validated that the particle is fully
	// configured before calling Put — the registry doesn't enforce
	// configuration policy.
	Put(ctx context.Context, name, version string, particle fs.FS) error

	// Get returns the entry for (name, version). Returns
	// ErrNotFound if no such entry exists.
	Get(ctx context.Context, name, version string) (Entry, error)

	// List returns a metadata-only summary of every entry, in
	// (name, version) order. The FS contents are not loaded —
	// callers needing those should call Get.
	List(ctx context.Context) ([]ListEntry, error)

	// Delete removes the entry. Idempotent: returns nil if no
	// such entry existed.
	Delete(ctx context.Context, name, version string) error
}

// Entry is a particle as the registry holds it: the identifying
// (name, version) pair plus the in-memory FS the runtime can
// instantiate against.
type Entry struct {
	Name    string
	Version string
	// Particle is the in-memory FS the runtime can instantiate
	// against.
	Particle fs.FS
}

// ListEntry is the metadata-only summary [Registry.List] returns.
type ListEntry struct {
	Name    string
	Version string
}

// ErrNotFound is the sentinel a Registry returns when no entry
// exists for the requested (name, version).
var ErrNotFound = errors.New("registry: not found")
