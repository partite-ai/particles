// Package memory is an in-process, map-backed [credentials.Store]
// implementation. Useful for tests, for `particle run` against a
// dev-time particle, and for any host that doesn't need persistence
// (CI ephemeral runs, sandboxes, …).
//
// All state lives in memory — restarting the host loses every
// credential. For persistent storage, use a different subpackage.
package memory

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/partite-ai/particle/credentials"
)

// Store is an in-process credentials.Store.
//
// Safe for concurrent use. Operations are O(1) for GetByID,
// GetByName, Put, Delete, ReadSecret, DeleteSecret; O(k) for
// WriteSecrets where k is the number of secrets passed; O(n) for
// List where n is the number of entries in the requested particle's
// namespace.
type Store struct {
	mu          sync.RWMutex
	byParticle  map[string]*particleSlot
	idGenerator func() string // overridable for tests; defaults to newID
}

// New returns an empty in-memory store.
func New() *Store {
	return &Store{
		byParticle:  map[string]*particleSlot{},
		idGenerator: newID,
	}
}

var _ credentials.Store = (*Store)(nil)

// particleSlot holds the per-particle indexes. The same *record
// pointer lives in both maps, so updates write once and stay
// consistent.
type particleSlot struct {
	byID   map[string]*record
	byName map[string]*record
}

type record struct {
	id      string
	name    string
	meta    credentials.Metadata
	secrets map[credentials.SecretRole][]byte
}

func (s *Store) slotFor(particle string) *particleSlot {
	slot, ok := s.byParticle[particle]
	if !ok {
		slot = &particleSlot{
			byID:   map[string]*record{},
			byName: map[string]*record{},
		}
		s.byParticle[particle] = slot
	}
	return slot
}

func (s *Store) recordByID(particle, id string) (*record, error) {
	slot, ok := s.byParticle[particle]
	if !ok {
		return nil, credentials.ErrNotFound
	}
	rec, ok := slot.byID[id]
	if !ok {
		return nil, credentials.ErrNotFound
	}
	return rec, nil
}

// -----------------------------------------------------------------------------
// Metadata operations
// -----------------------------------------------------------------------------

func (s *Store) GetByID(_ context.Context, particle, id string) (credentials.Descriptor, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, err := s.recordByID(particle, id)
	if err != nil {
		return credentials.Descriptor{}, err
	}
	return descriptorOf(rec), nil
}

func (s *Store) GetByName(_ context.Context, particle, name string) (credentials.Descriptor, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	slot, ok := s.byParticle[particle]
	if !ok {
		return credentials.Descriptor{}, credentials.ErrNotFound
	}
	rec, ok := slot.byName[name]
	if !ok {
		return credentials.Descriptor{}, credentials.ErrNotFound
	}
	return descriptorOf(rec), nil
}

func (s *Store) List(_ context.Context, particle string) ([]credentials.ListEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	slot, ok := s.byParticle[particle]
	if !ok {
		return nil, nil
	}
	out := make([]credentials.ListEntry, 0, len(slot.byID))
	for _, rec := range slot.byID {
		out = append(out, credentials.ListEntry{
			ID:   rec.id,
			Name: rec.name,
			Kind: rec.meta.Kind(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Put writes metadata + the listed secrets atomically. Roles not
// listed are preserved on overwrite (the OAuth-refresh path: write
// new ExpiresAt + new access token, don't touch refresh token or
// client secret).
func (s *Store) Put(_ context.Context, particle, name string, meta credentials.Metadata, secrets ...credentials.Secret) (credentials.Descriptor, error) {
	if meta == nil {
		return credentials.Descriptor{}, fmt.Errorf("memory: Put requires a non-nil Metadata")
	}
	if name == "" {
		return credentials.Descriptor{}, fmt.Errorf("memory: Put requires a non-empty name")
	}
	for i, sec := range secrets {
		if sec.Role == "" {
			return credentials.Descriptor{}, fmt.Errorf("memory: Put: secrets[%d] has empty Role", i)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	slot := s.slotFor(particle)
	rec, isUpdate := slot.byName[name]
	if !isUpdate {
		rec = &record{
			id:      s.idGenerator(),
			name:    name,
			meta:    meta,
			secrets: map[credentials.SecretRole][]byte{},
		}
		slot.byID[rec.id] = rec
		slot.byName[rec.name] = rec
	} else {
		rec.meta = meta
	}
	for _, sec := range secrets {
		stored := make([]byte, len(sec.Value))
		copy(stored, sec.Value)
		rec.secrets[sec.Role] = stored
	}
	return descriptorOf(rec), nil
}

// Delete removes the entire entry. Idempotent.
func (s *Store) Delete(_ context.Context, particle, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	slot, ok := s.byParticle[particle]
	if !ok {
		return nil
	}
	rec, ok := slot.byID[id]
	if !ok {
		return nil
	}
	delete(slot.byID, id)
	delete(slot.byName, rec.name)
	if len(slot.byID) == 0 {
		delete(s.byParticle, particle)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Secret operations
// -----------------------------------------------------------------------------

func (s *Store) ReadSecret(_ context.Context, particle, id string, role credentials.SecretRole) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rec, err := s.recordByID(particle, id)
	if err != nil {
		return nil, err
	}
	v, ok := rec.secrets[role]
	if !ok {
		return nil, credentials.ErrSecretNotSet
	}
	out := make([]byte, len(v))
	copy(out, v)
	return out, nil
}

func (s *Store) WriteSecrets(_ context.Context, particle, id string, secrets ...credentials.Secret) error {
	for i, sec := range secrets {
		if sec.Role == "" {
			return fmt.Errorf("memory: WriteSecrets: secrets[%d] has empty Role", i)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.recordByID(particle, id)
	if err != nil {
		return err
	}
	for _, sec := range secrets {
		stored := make([]byte, len(sec.Value))
		copy(stored, sec.Value)
		rec.secrets[sec.Role] = stored
	}
	return nil
}

func (s *Store) DeleteSecret(_ context.Context, particle, id string, role credentials.SecretRole) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.recordByID(particle, id)
	if err != nil {
		// Idempotent for missing entries — Delete is allowed
		// to be called against state that has already been
		// torn down.
		return nil
	}
	delete(rec.secrets, role)
	return nil
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func descriptorOf(rec *record) credentials.Descriptor {
	return credentials.Descriptor{
		ID:   rec.id,
		Name: rec.name,
		Meta: rec.meta,
	}
}

// newID returns a base32-encoded 128-bit random ID. Output is
// 26 lowercase ASCII characters from [a-z2-7] — no whitespace, no
// punctuation, safe to use in URLs, log messages, and file paths.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure on POSIX is essentially impossible.
		panic("credentials/memory: crypto/rand failed: " + err.Error())
	}
	return strings.ToLower(
		base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]),
	)
}
