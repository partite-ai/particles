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
	//
	// Put doesn't touch authentication-method selection: that
	// state lives at the per-particle-name level (credentials
	// are per-name in [credentials.Store], so the chosen method
	// is shared by every version). Re-Putting a different
	// version of the same particle leaves the prior selection
	// in place.
	Put(ctx context.Context, name, version string, particle fs.FS) error

	// Get returns the entry for (name, version). Returns
	// ErrNotFound if no such entry exists. The returned
	// [Entry.SelectedAuthenticationMethod] reflects the
	// per-particle-name selection — same value across every
	// registered version of the same particle.
	Get(ctx context.Context, name, version string) (Entry, error)

	// List returns a metadata-only summary of every entry, in
	// (name, version) order. The FS contents are not loaded —
	// callers needing those should call Get.
	List(ctx context.Context) ([]ListEntry, error)

	// SetSelectedAuthenticationMethod records the credential
	// method the user chose at setup. Per-particle-name (no
	// version): every version of the same particle shares the
	// same auth-method choice because credentials are
	// per-particle in [credentials.Store]. Empty `method`
	// clears the selection.
	//
	// The runtime reads this when wiring the wasi:http policy,
	// so substitution checks only that one method's placeholder.
	SetSelectedAuthenticationMethod(ctx context.Context, name, method string) error

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
	// SelectedAuthenticationMethod is the name of the credential
	// method the user picked at setup time, taken from the
	// per-particle-name selection table (so it's the same value
	// for every version of the same particle). Empty when the
	// particle either declares no credentials or hasn't been
	// set up yet.
	SelectedAuthenticationMethod string
}

// ListEntry is the metadata-only summary [Registry.List] returns.
type ListEntry struct {
	Name    string
	Version string
	// SelectedAuthenticationMethod is the per-particle-name
	// selection (so identical for every row of the same name).
	// Empty when no method is configured.
	SelectedAuthenticationMethod string
}

// ErrNotFound is the sentinel a Registry returns when no entry
// exists for the requested (name, version).
var ErrNotFound = errors.New("registry: not found")
