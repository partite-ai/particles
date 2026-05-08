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
-- Per-particle-name configuration. Credentials are stored
-- per-name in credentials.Store (not per-version), so the
-- chosen authentication method is shared across every
-- registered version of the same particle.
CREATE TABLE IF NOT EXISTS particle_settings (
  name                            TEXT PRIMARY KEY,
  selected_authentication_method  TEXT
);
`

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("registry/sqlite: migrate: %w", err)
	}
	// Backfill: an earlier schema kept selected_method on
	// particle_registry (per-(name,version)). The column has
	// since moved to particle_settings (per-name). Hoist any
	// stale values across, then drop the old column to keep the
	// schema honest.
	hasOld, err := s.columnExists(ctx, "particle_registry", "selected_method")
	if err != nil {
		return fmt.Errorf("registry/sqlite: probe legacy column: %w", err)
	}
	if hasOld {
		// Pick any non-null per-name value (`MAX` over text picks
		// the lexicographically-greatest, which is good enough
		// — the old per-version selection is already losing
		// information by collapsing). Skip NULL with WHERE.
		if _, err := s.db.ExecContext(ctx, `
			INSERT OR IGNORE INTO particle_settings (name, selected_authentication_method)
			SELECT name, MAX(selected_method)
			FROM particle_registry
			WHERE selected_method IS NOT NULL
			GROUP BY name
		`); err != nil {
			return fmt.Errorf("registry/sqlite: migrate selected_method: %w", err)
		}
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
	// Authentication-method selection is per-name (see
	// particle_settings) so Put doesn't have to do anything
	// special to preserve it across re-Puts. The plain INSERT
	// is fine — particle_registry only carries identity here.
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
// the FS reconstructed from rows. The SelectedAuthenticationMethod
// comes from the per-name particle_settings table; the same value
// applies to every version of the same particle.
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

	var selected sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT selected_authentication_method FROM particle_settings WHERE name = ?`,
		name).Scan(&selected); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return registry.Entry{}, fmt.Errorf("registry/sqlite: Get: settings: %w", err)
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
		Name:                         name,
		Version:                      version,
		Particle:                     mapFS,
		SelectedAuthenticationMethod: selected.String,
	}, nil
}

// SetSelectedAuthenticationMethod records (or clears, on empty
// input) the credential method the user picked at setup. The
// selection is per-particle-name — every version of the same
// particle reads the same value back via Get.
//
// Returns ErrNotFound when the particle name has no registered
// version. (The selection is per-name, but it's not meaningful
// without at least one registered artifact to apply to — and
// rejecting catches typos at the API boundary.)
func (s *Store) SetSelectedAuthenticationMethod(ctx context.Context, name, method string) error {
	var exists int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM particle_registry WHERE name = ? LIMIT 1`, name).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return registry.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("registry/sqlite: SetSelectedAuthenticationMethod: lookup: %w", err)
	}

	if method == "" {
		// Clearing: collapse to a delete so an unset
		// particle reads back the same way as a never-set one.
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM particle_settings WHERE name = ?`, name); err != nil {
			return fmt.Errorf("registry/sqlite: SetSelectedAuthenticationMethod: clear: %w", err)
		}
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO particle_settings (name, selected_authentication_method)
		 VALUES (?, ?)
		 ON CONFLICT(name) DO UPDATE SET selected_authentication_method = excluded.selected_authentication_method`,
		name, method); err != nil {
		return fmt.Errorf("registry/sqlite: SetSelectedAuthenticationMethod: %w", err)
	}
	return nil
}

// List returns every (name, version) pair plus the per-name
// selected authentication method, sorted by (name, version).
//
// LEFT JOIN against particle_settings so a particle without a
// configured method just gets an empty string — saves a per-row
// follow-up query in the CLI's list command.
func (s *Store) List(ctx context.Context) ([]registry.ListEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT pr.name, pr.version, ps.selected_authentication_method
		FROM particle_registry pr
		LEFT JOIN particle_settings ps ON ps.name = pr.name
		ORDER BY pr.name, pr.version
	`)
	if err != nil {
		return nil, fmt.Errorf("registry/sqlite: List: %w", err)
	}
	defer rows.Close()

	var out []registry.ListEntry
	for rows.Next() {
		var le registry.ListEntry
		var sel sql.NullString
		if err := rows.Scan(&le.Name, &le.Version, &sel); err != nil {
			return nil, fmt.Errorf("registry/sqlite: List scan: %w", err)
		}
		le.SelectedAuthenticationMethod = sel.String
		out = append(out, le)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("registry/sqlite: List rows: %w", err)
	}
	return out, nil
}

// Delete removes the entry — index row and every file row.
// Idempotent.
//
// When this removes the last version of a particle, the per-name
// selection in particle_settings is cleared too: an orphan setting
// would either confuse a re-import (with a now-stale method
// choice) or be invisible deadwood. Deletes that leave other
// versions in place leave the selection alone — every version
// shares it.
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

	// Cleanup orphan settings only when no version of this name
	// remains. This is a same-tx check so a concurrent Put on
	// another version can't slip in between count and delete.
	var remaining int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM particle_registry WHERE name = ?`,
		name).Scan(&remaining); err != nil {
		return fmt.Errorf("registry/sqlite: Delete: count: %w", err)
	}
	if remaining == 0 {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM particle_settings WHERE name = ?`, name); err != nil {
			return fmt.Errorf("registry/sqlite: Delete: settings: %w", err)
		}
	}
	return tx.Commit()
}
