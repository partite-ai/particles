// Package sqlite is a SQLite-backed [kv.Store] implementation.
//
// It uses database/sql with whatever driver the caller registered.
// modernc.org/sqlite is the natural pure-Go choice; mattn/go-sqlite3
// works too. The Store is given an already-open *sql.DB so the
// caller owns connection-pool tuning, DSN choices, and lifetime.
//
// Schema management runs on construction: [New] creates two tables
// the package owns — `particle_kv` for entries and
// `particle_kv_usage` for a per-particle byte counter — if they
// don't already exist. The caller may share the database with
// their own tables.
//
// Quotas are enforced inside the Set transaction by checking the
// usage counter (O(1)). The counter is maintained transactionally
// alongside every Set / Delete, so the quota check never has to
// scan the namespace. Set the cap with [Store.QuotaBytes] before
// taking the Store into use.
//
// All state is persistent — restarting the host preserves every
// entry. For the in-process equivalent, use kv/memory.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/partite-ai/particles/kv"
)

// Backend is the SQLite-backed multi-particle kv backing store
// with optional per-particle quota. Per-particle [kv.Store] views
// come from [*Backend.Scoped].
//
// Safe for concurrent use — the underlying *sql.DB serializes
// access through its connection pool, and Set wraps its
// read-then-write quota check in a transaction so two concurrent
// writes can't both squeak past the cap.
type Backend struct {
	db *sql.DB

	// QuotaBytes, if non-zero, caps the total number of bytes
	// (sum of `key + value` lengths) any one particle may
	// store. Set returns kv.ErrQuotaExceeded when a write would
	// push the namespace past the cap.
	QuotaBytes int
}

// New constructs a Backend against an already-open *sql.DB and
// applies the schema. The caller retains ownership of the DB —
// closing the Backend does NOT close the DB; the caller decides
// when to.
func New(ctx context.Context, db *sql.DB) (*Backend, error) {
	if db == nil {
		return nil, errors.New("kv/sqlite: db is required")
	}
	b := &Backend{db: db}
	if err := b.migrate(ctx); err != nil {
		return nil, err
	}
	return b, nil
}

// Scoped returns a [kv.Store] view of the Backend pre-bound to
// `particle`. Mirrors `credentials/sqlite.(*Backend).Scoped`.
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

const schema = `
CREATE TABLE IF NOT EXISTS particle_kv (
  particle TEXT NOT NULL,
  key      TEXT NOT NULL,
  value    TEXT NOT NULL,
  PRIMARY KEY (particle, key)
);
CREATE TABLE IF NOT EXISTS particle_kv_usage (
  particle TEXT PRIMARY KEY,
  bytes    INTEGER NOT NULL
);
`

func (s *Backend) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("kv/sqlite: migrate: %w", err)
	}
	return nil
}

func (s *Backend) Get(ctx context.Context, particle, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM particle_kv WHERE particle = ? AND key = ?`,
		particle, key).Scan(&v)
	if err == nil {
		return v, true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return "", false, fmt.Errorf("kv/sqlite: Get: %w", err)
}

// Set creates or replaces (particle, key) → value, enforcing
// QuotaBytes if non-zero. The per-particle usage counter in
// particle_kv_usage is updated transactionally with the data row,
// so the quota check is O(1) regardless of namespace size.
func (s *Backend) Set(ctx context.Context, particle, key, value string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("kv/sqlite: Set: begin: %w", err)
	}
	defer tx.Rollback()

	// Bytes the existing entry (if any) currently contributes —
	// subtract it from the post-write delta so a same-size
	// replacement is a no-op against the quota.
	var prev int
	err = tx.QueryRowContext(ctx,
		`SELECT LENGTH(key) + LENGTH(value)
		 FROM particle_kv WHERE particle = ? AND key = ?`,
		particle, key).Scan(&prev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("kv/sqlite: Set: prev: %w", err)
	}
	delta := len(key) + len(value) - prev

	if s.QuotaBytes > 0 {
		var current int
		err := tx.QueryRowContext(ctx,
			`SELECT bytes FROM particle_kv_usage WHERE particle = ?`,
			particle).Scan(&current)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("kv/sqlite: Set: usage: %w", err)
		}
		if current+delta > s.QuotaBytes {
			return kv.ErrQuotaExceeded
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO particle_kv (particle, key, value) VALUES (?, ?, ?)
		 ON CONFLICT(particle, key) DO UPDATE SET value = excluded.value`,
		particle, key, value); err != nil {
		return fmt.Errorf("kv/sqlite: Set: write: %w", err)
	}

	// Insert-with-fresh-total OR update-by-delta. The two
	// parameters paper over the gap between the INSERT branch
	// (needs the absolute byte total) and the UPDATE branch
	// (needs the signed delta).
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO particle_kv_usage (particle, bytes) VALUES (?, ?)
		 ON CONFLICT(particle) DO UPDATE SET bytes = bytes + ?`,
		particle, len(key)+len(value), delta); err != nil {
		return fmt.Errorf("kv/sqlite: Set: usage write: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("kv/sqlite: Set: commit: %w", err)
	}
	return nil
}

// Delete removes (particle, key) and decrements the usage counter
// transactionally. Idempotent — a missing key is a no-op.
func (s *Backend) Delete(ctx context.Context, particle, key string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("kv/sqlite: Delete: begin: %w", err)
	}
	defer tx.Rollback()

	var existing int
	err = tx.QueryRowContext(ctx,
		`SELECT LENGTH(key) + LENGTH(value)
		 FROM particle_kv WHERE particle = ? AND key = ?`,
		particle, key).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		// Nothing to delete and no usage to decrement.
		return tx.Commit()
	}
	if err != nil {
		return fmt.Errorf("kv/sqlite: Delete: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM particle_kv WHERE particle = ? AND key = ?`,
		particle, key); err != nil {
		return fmt.Errorf("kv/sqlite: Delete: row: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE particle_kv_usage SET bytes = bytes - ? WHERE particle = ?`,
		existing, particle); err != nil {
		return fmt.Errorf("kv/sqlite: Delete: usage: %w", err)
	}
	// Drop the usage row when its namespace empties out, so the
	// table doesn't grow in proportion to particles ever used.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM particle_kv_usage WHERE particle = ? AND bytes <= 0`,
		particle); err != nil {
		return fmt.Errorf("kv/sqlite: Delete: usage prune: %w", err)
	}
	return tx.Commit()
}

// List returns keys in `particle` whose name has `prefix` as a
// prefix, sorted ascending. SQLite's `LIKE` with an escape clause
// keeps `%` and `_` in the prefix from matching as wildcards.
func (s *Backend) List(ctx context.Context, particle, prefix string) ([]string, error) {
	pattern := likeEscape(prefix) + "%"
	rows, err := s.db.QueryContext(ctx,
		`SELECT key FROM particle_kv
		 WHERE particle = ? AND key LIKE ? ESCAPE '\'
		 ORDER BY key`,
		particle, pattern)
	if err != nil {
		return nil, fmt.Errorf("kv/sqlite: List: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, fmt.Errorf("kv/sqlite: List scan: %w", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("kv/sqlite: List rows: %w", err)
	}
	return out, nil
}

// likeEscape escapes the LIKE wildcards `%`, `_`, and the escape
// character `\` itself so a literal prefix isn't reinterpreted as
// a pattern. Pairs with `ESCAPE '\'` in the query.
func likeEscape(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' || c == '%' || c == '_' {
			out = append(out, '\\')
		}
		out = append(out, c)
	}
	return string(out)
}
