// Package memory is an in-process, map-backed [kv.Store]
// implementation. Useful for tests and for any host that doesn't
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

// Store is an in-process kv.Store with optional per-particle
// quota.
//
// Safe for concurrent use. Operations are O(1) for Get/Set/Delete
// and O(n) for List where n is the number of entries in the
// requested particle's namespace.
type Store struct {
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

// New returns an empty Store with no quota.
func New() *Store {
	return &Store{entries: map[string]map[string]string{}}
}

var _ kv.Store = (*Store)(nil)

func (s *Store) Get(_ context.Context, particle, key string) (string, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	bucket, ok := s.entries[particle]
	if !ok {
		return "", false, nil
	}
	v, ok := bucket[key]
	return v, ok, nil
}

func (s *Store) Set(_ context.Context, particle, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, ok := s.entries[particle]
	if !ok {
		bucket = map[string]string{}
		s.entries[particle] = bucket
	}
	if s.QuotaBytes > 0 {
		// Compute the prospective total: existing usage minus
		// the prior value at `key` plus the new write.
		prev := 0
		if existing, ok := bucket[key]; ok {
			prev = len(key) + len(existing)
		}
		total := s.usageLocked(bucket) - prev + len(key) + len(value)
		if total > s.QuotaBytes {
			return kv.ErrQuotaExceeded
		}
	}
	bucket[key] = value
	return nil
}

func (s *Store) Delete(_ context.Context, particle, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, ok := s.entries[particle]
	if !ok {
		return nil
	}
	delete(bucket, key)
	if len(bucket) == 0 {
		delete(s.entries, particle)
	}
	return nil
}

func (s *Store) List(_ context.Context, particle, prefix string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	bucket, ok := s.entries[particle]
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
func (s *Store) usageLocked(bucket map[string]string) int {
	total := 0
	for k, v := range bucket {
		total += len(k) + len(v)
	}
	return total
}
