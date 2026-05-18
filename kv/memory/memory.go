// Package memory is an in-process, map-backed multi-particle
// kv backend. Useful for tests and for any host that doesn't
// need persistence.
//
// All state lives in memory — restarting the host loses every
// entry. For persistent storage, use a different subpackage.
package memory

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/partite-ai/particles/kv"
)

// Backend is the in-process multi-particle kv backing store.
// Per-particle [kv.Store] views are produced via
// [*Backend.Scoped]; the Backend itself isn't a Store (every
// method on Store is particle-scoped).
//
// Safe for concurrent use. Operations are O(1) for Get/Set/Delete
// and O(n) for List where n is the number of entries in the
// requested particle's namespace.
type Backend struct {
	mu sync.RWMutex
	// entries[particle][key] = value. Two-level map so
	// per-particle iteration / quota counting is cheap.
	entries map[string]map[string]string

	// QuotaBytes, if non-zero, caps the total number of bytes
	// (sum of `key + value` lengths) any one particle may store.
	// Set returns kv.ErrQuotaExceeded when a write would push
	// the namespace past the cap.
	QuotaBytes int
}

// New returns an empty Backend with no quota.
func New() *Backend {
	return &Backend{entries: map[string]map[string]string{}}
}

// Scoped returns a [kv.Store] view pre-bound to `particle`.
// Mirrors `sqlite.(*Backend).Scoped`.
func (b *Backend) Scoped(particle string) kv.Store {
	return &scopedStore{backend: b, particle: particle}
}

// scopedStore is the per-particle wrapper. Methods just thread
// `particle` into the matching Backend method.
type scopedStore struct {
	backend  *Backend
	particle string
}

var _ kv.Store = (*scopedStore)(nil)

func (s *scopedStore) Get(ctx context.Context, key string) (string, bool, error) {
	return s.backend.Get(ctx, s.particle, key)
}
func (s *scopedStore) Set(ctx context.Context, key, value string) error {
	return s.backend.Set(ctx, s.particle, key, value)
}
func (s *scopedStore) Delete(ctx context.Context, key string) error {
	return s.backend.Delete(ctx, s.particle, key)
}
func (s *scopedStore) List(ctx context.Context, prefix string) ([]string, error) {
	return s.backend.List(ctx, s.particle, prefix)
}

func (b *Backend) Get(_ context.Context, particle, key string) (string, bool, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	bucket, ok := b.entries[particle]
	if !ok {
		return "", false, nil
	}
	v, ok := bucket[key]
	return v, ok, nil
}

func (b *Backend) Set(_ context.Context, particle, key, value string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	bucket, ok := b.entries[particle]
	if !ok {
		bucket = map[string]string{}
		b.entries[particle] = bucket
	}
	if b.QuotaBytes > 0 {
		// Compute the prospective total: existing usage minus
		// the prior value at `key` plus the new write.
		prev := 0
		if existing, ok := bucket[key]; ok {
			prev = len(key) + len(existing)
		}
		total := b.usageLocked(bucket) - prev + len(key) + len(value)
		if total > b.QuotaBytes {
			return kv.ErrQuotaExceeded
		}
	}
	bucket[key] = value
	return nil
}

func (b *Backend) Delete(_ context.Context, particle, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	bucket, ok := b.entries[particle]
	if !ok {
		return nil
	}
	delete(bucket, key)
	if len(bucket) == 0 {
		delete(b.entries, particle)
	}
	return nil
}

func (b *Backend) List(_ context.Context, particle, prefix string) ([]string, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	bucket, ok := b.entries[particle]
	if !ok {
		return nil, nil
	}
	out := make([]string, 0, len(bucket))
	for k := range bucket {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

// usageLocked returns the current total bytes used by `bucket`.
// Caller must hold the lock.
func (b *Backend) usageLocked(bucket map[string]string) int {
	total := 0
	for k, v := range bucket {
		total += len(k) + len(v)
	}
	return total
}
