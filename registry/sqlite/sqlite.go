// Package sqlite is a SQLite-backed [registry.Registry]
// implementation.
//
// It uses database/sql with whatever driver the caller registered.
// modernc.org/sqlite is the natural pure-Go choice; mattn/go-sqlite3
// works too. The Store is given an already-open *sql.DB so the
// caller owns connection-pool tuning, DSN choices, and lifetime.
//
// Schema management runs on construction: [New] creates two tables
// the package owns — `particle_registry` for the (name, version)
// index and `particle_registry_files` for one row per file in each
// particle's FS — if they don't already exist. The caller may
// share the database with their own tables.
//
// Put walks the FS inside a transaction so a partial replacement
// is never observable. Get rebuilds an fs.FS from rows via
// testing/fstest.MapFS, which is fine for the registry's payload
// shape (small handful of files: bundle.js, manifest.json,
// build-info.json, sourcemap).
//
// All state is persistent — restarting the host preserves every
// entry.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"testing/fstest"

	"github.com/partite-ai/particle/internal/semver"
	"github.com/partite-ai/particle/registry"
)

// Store is a SQLite-backed registry.Registry.
//
// Safe for concurrent use — the underlying *sql.DB serializes
// access through its connection pool, and Put wraps its
// truncate-then-rewrite in a transaction.
type Store struct {
	db *sql.DB
}

// New constructs a Store against an already-open *sql.DB and
// applies the schema. The caller retains ownership of the DB —
// closing the Store does NOT close the DB; the caller decides when
// to.
func New(ctx context.Context, db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("registry/sqlite: db is required")
	}
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

var _ registry.Registry = (*Store)(nil)

const schema = `
CREATE TABLE IF NOT EXISTS particle_registry (
  name    TEXT NOT NULL,
  version TEXT NOT NULL,
  PRIMARY KEY (name, version)
);
CREATE TABLE IF NOT EXISTS particle_registry_files (
  name    TEXT NOT NULL,
  version TEXT NOT NULL,
  path    TEXT NOT NULL,
  data    BLOB NOT NULL,
  PRIMARY KEY (name, version, path)
);
`

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("registry/sqlite: migrate: %w", err)
	}
	// Drop the legacy per-name selection table. Its contents
	// are already mirrored in the credentials store — the
	// selected method is now derived from which credential row
	// exists per particle.
	if _, err := s.db.ExecContext(ctx,
		`DROP TABLE IF EXISTS particle_settings`); err != nil {
		return fmt.Errorf("registry/sqlite: drop particle_settings: %w", err)
	}
	// An even-older schema kept selected_method on
	// particle_registry (per-(name, version)). The column has
	// been unused since the selection moved out of the registry
	// entirely — drop it if it still lingers.
	hasOld, err := s.columnExists(ctx, "particle_registry", "selected_method")
	if err != nil {
		return fmt.Errorf("registry/sqlite: probe legacy column: %w", err)
	}
	if hasOld {
		if _, err := s.db.ExecContext(ctx,
			`ALTER TABLE particle_registry DROP COLUMN selected_method`); err != nil {
			return fmt.Errorf("registry/sqlite: drop legacy column: %w", err)
		}
	}
	return nil
}

func (s *Store) columnExists(ctx context.Context, table, column string) (bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT 1 FROM pragma_table_info(?) WHERE name = ?`, table, column)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	return rows.Next(), nil
}

// Put stores particle's full FS contents under (name, version),
// replacing any prior entry. Atomic: a partial walk failure rolls
// back, so readers never see a torn entry.
func (s *Store) Put(ctx context.Context, name, version string, particle fs.FS) error {
	if name == "" || version == "" {
		return fmt.Errorf("registry/sqlite: Put requires non-empty name and version")
	}
	if !semver.IsValid(version) {
		return fmt.Errorf("registry/sqlite: Put: version %q is not valid semver (e.g. \"1.2.3\", \"0.1.0-rc.1\")", version)
	}
	if particle == nil {
		return fmt.Errorf("registry/sqlite: Put requires a non-nil FS")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("registry/sqlite: Put: begin: %w", err)
	}
	defer tx.Rollback()

	// Drop any prior entry's files; the entry row itself is
	// upserted below. Keeping the drop first means a re-Put with
	// fewer files than the prior version doesn't leave orphans.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM particle_registry_files WHERE name = ? AND version = ?`,
		name, version); err != nil {
		return fmt.Errorf("registry/sqlite: Put: clear files: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO particle_registry (name, version) VALUES (?, ?)
		 ON CONFLICT(name, version) DO NOTHING`,
		name, version); err != nil {
		return fmt.Errorf("registry/sqlite: Put: index: %w", err)
	}

	insertFile, err := tx.PrepareContext(ctx,
		`INSERT INTO particle_registry_files (name, version, path, data) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("registry/sqlite: Put: prepare: %w", err)
	}
	defer insertFile.Close()

	walkErr := fs.WalkDir(particle, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(particle, path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		if _, err := insertFile.ExecContext(ctx, name, version, path, data); err != nil {
			return fmt.Errorf("insert %s: %w", path, err)
		}
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("registry/sqlite: Put: walk: %w", walkErr)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("registry/sqlite: Put: commit: %w", err)
	}
	return nil
}

// Get returns the registered entry for (name, version), with
// the FS reconstructed from rows.
func (s *Store) Get(ctx context.Context, name, version string) (registry.Entry, error) {
	var exists int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM particle_registry WHERE name = ? AND version = ?`,
		name, version).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return registry.Entry{}, registry.ErrNotFound
	}
	if err != nil {
		return registry.Entry{}, fmt.Errorf("registry/sqlite: Get: %w", err)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT path, data FROM particle_registry_files WHERE name = ? AND version = ?`,
		name, version)
	if err != nil {
		return registry.Entry{}, fmt.Errorf("registry/sqlite: Get: files: %w", err)
	}
	defer rows.Close()

	mapFS := fstest.MapFS{}
	for rows.Next() {
		var path string
		var data []byte
		if err := rows.Scan(&path, &data); err != nil {
			return registry.Entry{}, fmt.Errorf("registry/sqlite: Get: scan: %w", err)
		}
		mapFS[path] = &fstest.MapFile{Data: data}
	}
	if err := rows.Err(); err != nil {
		return registry.Entry{}, fmt.Errorf("registry/sqlite: Get: rows: %w", err)
	}
	return registry.Entry{
		Name:     name,
		Version:  version,
		Particle: mapFS,
	}, nil
}

// List returns every (name, version) pair, sorted.
func (s *Store) List(ctx context.Context) ([]registry.ListEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, version FROM particle_registry ORDER BY name, version`)
	if err != nil {
		return nil, fmt.Errorf("registry/sqlite: List: %w", err)
	}
	defer rows.Close()

	var out []registry.ListEntry
	for rows.Next() {
		var le registry.ListEntry
		if err := rows.Scan(&le.Name, &le.Version); err != nil {
			return nil, fmt.Errorf("registry/sqlite: List scan: %w", err)
		}
		out = append(out, le)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("registry/sqlite: List rows: %w", err)
	}
	return out, nil
}

// Delete removes the entry — index row and every file row.
// Idempotent.
func (s *Store) Delete(ctx context.Context, name, version string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("registry/sqlite: Delete: begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM particle_registry_files WHERE name = ? AND version = ?`,
		name, version); err != nil {
		return fmt.Errorf("registry/sqlite: Delete: files: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM particle_registry WHERE name = ? AND version = ?`,
		name, version); err != nil {
		return fmt.Errorf("registry/sqlite: Delete: index: %w", err)
	}
	return tx.Commit()
}
