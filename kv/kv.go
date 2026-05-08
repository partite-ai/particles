// Package kv defines the host-side per-particle key/value store
// contract.
//
// Particles import `particle:kv` and call get / set / delete / list
// to persist small bits of state across tool invocations. The host
// supplies a [Store] implementation; this package wires it to the
// runtime via the `particle:host/kv@0.1.0` WIT interface.
//
// All operations are scoped by particle name — each particle gets
// its own namespace; the JS sees a flat keyspace. Two particles
// using the same key see independent values.
//
// Concrete Store implementations live in subpackages — kv/memory
// for an in-process map-backed Store. There are no implementations
// in this package.
//
// Spec: docs/initial-design.md §2 (`particle:kv`).
package kv

import (
	"context"
	"errors"
)

// Store is the host-side key/value store interface. All operations
// are scoped by particle name — each particle has its own
// namespace.
//
// Values are strings (the WIT contract is `string`, not `list<u8>`).
// Hosts that need to store binary data should base64-encode in the
// particle.
//
// Implementations should be safe for concurrent use.
type Store interface {
	// Get returns the value stored under (particle, key). The
	// boolean is false when no entry exists for that key — the
	// runtime maps this to `option<string>::none` for the
	// particle. error is reserved for storage failures.
	Get(ctx context.Context, particle, key string) (value string, found bool, err error)

	// Set creates or replaces the value. Returns ErrQuotaExceeded
	// if the host enforces a per-particle quota and writing
	// would exceed it; the runtime maps that to
	// `kv-error::quota-exceeded`.
	Set(ctx context.Context, particle, key, value string) error

	// Delete removes the entry. Idempotent: returns nil when
	// no such entry existed.
	Delete(ctx context.Context, particle, key string) error

	// List returns every key in the particle's namespace whose
	// name has `prefix` as a prefix, in unspecified order.
	// Empty `prefix` matches every key.
	//
	// Implementations should not include values — this is the
	// inventory path; the particle calls Get to fetch values
	// individually if needed.
	List(ctx context.Context, particle, prefix string) (keys []string, err error)
}

// ErrQuotaExceeded is the sentinel a Store returns from Set when
// the host enforces a per-particle quota and the write would
// exceed it. Maps to the WIT `kv-error::quota-exceeded` variant.
//
// Other errors from any Store method are surfaced as
// `kv-error::storage-error` carrying the error message.
var ErrQuotaExceeded = errors.New("kv: quota exceeded")
