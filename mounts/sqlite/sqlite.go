// Package sqlite is a SQLite-backed [mounts.Store] implementation.
//
// It uses database/sql with whatever driver the caller registered
// (modernc.org/sqlite is the natural pure-Go choice). The Backend is
// given an already-open *sql.DB so the caller owns connection-pool
// tuning, DSN choices, and lifetime — it's the same DB the registry,
// credentials, and kv stores share.
//
// Schema management runs on construction: [New] creates the
// `particle_mounts` table — one row per (particle, mount_name) →
// host_path mapping — if it doesn't already exist.
//
// All state is persistent and keyed by particle NAME, so a mapping
// configured for one version is reused after an upgrade.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/partite-ai/particles/mounts"
)

// Backend is the SQLite-backed multi-particle mount-mapping store.
// Per-particle [mounts.Store] views come from [*Backend.Scoped].
//
// Safe for concurrent use — the underlying *sql.DB serializes access
// through its connection pool.
type Backend struct {
	db *sql.DB
}

// New constructs a Backend against an already-open *sql.DB and applies
// the schema. The caller retains ownership of the DB — closing the
// Backend does NOT close the DB.
func New(ctx context.Context, db *sql.DB) (*Backend, error) {
	if db == nil {
		return nil, errors.New("mounts/sqlite: db is required")
	}
	b := &Backend{db: db}
	if err := b.migrate(ctx); err != nil {
		return nil, err
	}
	return b, nil
}

// Scoped returns a [mounts.Store] view pre-bound to `particle`. Mirrors
// `credentials/sqlite.(*Backend).Scoped` and `kv/sqlite`.
func (b *Backend) Scoped(particle string) mounts.Store {
	return &scopedStore{backend: b, particle: particle}
}

type scopedStore struct {
	backend  *Backend
	particle string
}

var _ mounts.Store = (*scopedStore)(nil)

func (s *scopedStore) Get(ctx context.Context, name string) (string, bool, error) {
	return s.backend.Get(ctx, s.particle, name)
}
func (s *scopedStore) Set(ctx context.Context, name, hostPath string) error {
	return s.backend.Set(ctx, s.particle, name, hostPath)
}
func (s *scopedStore) Delete(ctx context.Context, name string) error {
	return s.backend.Delete(ctx, s.particle, name)
}
func (s *scopedStore) List(ctx context.Context) ([]mounts.Mapping, error) {
	return s.backend.List(ctx, s.particle)
}

const schema = `
CREATE TABLE IF NOT EXISTS particle_mounts (
  particle   TEXT NOT NULL,
  mount_name TEXT NOT NULL,
  host_path  TEXT NOT NULL,
  PRIMARY KEY (particle, mount_name)
);
`

func (b *Backend) migrate(ctx context.Context) error {
	if _, err := b.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("mounts/sqlite: migrate: %w", err)
	}
	return nil
}

func (b *Backend) Get(ctx context.Context, particle, name string) (string, bool, error) {
	var hostPath string
	err := b.db.QueryRowContext(ctx,
		`SELECT host_path FROM particle_mounts WHERE particle = ? AND mount_name = ?`,
		particle, name).Scan(&hostPath)
	if err == nil {
		return hostPath, true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return "", false, fmt.Errorf("mounts/sqlite: Get: %w", err)
}

func (b *Backend) Set(ctx context.Context, particle, name, hostPath string) error {
	if _, err := b.db.ExecContext(ctx,
		`INSERT INTO particle_mounts (particle, mount_name, host_path) VALUES (?, ?, ?)
		 ON CONFLICT(particle, mount_name) DO UPDATE SET host_path = excluded.host_path`,
		particle, name, hostPath); err != nil {
		return fmt.Errorf("mounts/sqlite: Set: %w", err)
	}
	return nil
}

func (b *Backend) Delete(ctx context.Context, particle, name string) error {
	if _, err := b.db.ExecContext(ctx,
		`DELETE FROM particle_mounts WHERE particle = ? AND mount_name = ?`,
		particle, name); err != nil {
		return fmt.Errorf("mounts/sqlite: Delete: %w", err)
	}
	return nil
}

func (b *Backend) List(ctx context.Context, particle string) ([]mounts.Mapping, error) {
	rows, err := b.db.QueryContext(ctx,
		`SELECT mount_name, host_path FROM particle_mounts WHERE particle = ? ORDER BY mount_name`,
		particle)
	if err != nil {
		return nil, fmt.Errorf("mounts/sqlite: List: %w", err)
	}
	defer rows.Close()

	var out []mounts.Mapping
	for rows.Next() {
		var m mounts.Mapping
		if err := rows.Scan(&m.Name, &m.HostPath); err != nil {
			return nil, fmt.Errorf("mounts/sqlite: List scan: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mounts/sqlite: List rows: %w", err)
	}
	return out, nil
}
