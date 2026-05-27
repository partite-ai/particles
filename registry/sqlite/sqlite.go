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
// is never observable. Get returns a `dbFS` that snapshots the
// particle's path/size index up front (a single small SELECT) and
// answers Stat/ReadDir entirely in memory. Blob bytes still stream
// out of SQLite lazily — Open and ReadFile issue one SELECT per
// call — so a 100MB Python particle's bytes never leave SQLite
// until something inside the wasi sandbox actually reads them.
// The snapshot turns Python's import-system stat storms (probing
// `.py` / `__init__.py` / `.so` for every candidate path) into Go
// map lookups instead of SQL round-trips.
//
// All state is persistent — restarting the host preserves every
// entry.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/partite-ai/particles/internal/semver"
	"github.com/partite-ai/particles/registry"
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

// Get returns the registered entry for (name, version). The returned
// Particle is a dbFS that has snapshotted the particle's path/size
// index — a single small SELECT scans the row table, no blob bytes
// are fetched. Subsequent Stat / ReadDir / dir Open calls answer
// from the snapshot with zero SQL; only file Open / ReadFile issue
// further queries (to fetch the blob).
//
// The snapshot is per-Get: a re-Put against the same (name, version)
// is not observed by an existing dbFS. This matches how the runtime
// uses the registry (load once at instantiation, never re-read), and
// avoids the "stat storm" cost during Python interpreter startup.
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
	files, dirEntries, err := loadSnapshot(ctx, s.db, name, version)
	if err != nil {
		return registry.Entry{}, fmt.Errorf("registry/sqlite: Get: snapshot: %w", err)
	}
	return registry.Entry{
		Name:    name,
		Version: version,
		Particle: &dbFS{
			db: s.db, name: name, version: version,
			files: files, dirEntries: dirEntries,
		},
	}, nil
}

// loadSnapshot fetches every file path + blob length for (name,
// version) and materializes a Go-side path/dir index. Synthetic
// intermediate directories are walked once here so that subsequent
// Stat / ReadDir on a path like "a/b" hit a precomputed map even
// though only leaf paths ("a/b/c.py") live in the table.
//
// Only path strings and int64 sizes are loaded — blob bytes stay
// in SQLite. For a particle with 10k files at ~64-byte average path
// length, the snapshot is well under a megabyte.
func loadSnapshot(ctx context.Context, db *sql.DB, name, version string) (
	map[string]int64, map[string][]fs.DirEntry, error,
) {
	rows, err := db.QueryContext(ctx,
		`SELECT path, length(data) FROM particle_registry_files
		 WHERE name = ? AND version = ?`, name, version)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	files := map[string]int64{}
	// children[parent][childName] = pointer into the eventual sorted
	// slice. We mutate in place so a sub-directory marker can be
	// "promoted" if we discover both `a/b` (file) and `a/b/c` (file
	// implying b is a dir) — though the schema's primary key
	// prevents that pathological case in practice.
	children := map[string]map[string]*dirEntry{}
	addChild := func(parent, leaf string, isDir bool, size int64) {
		if children[parent] == nil {
			children[parent] = map[string]*dirEntry{}
		}
		existing, ok := children[parent][leaf]
		if !ok {
			children[parent][leaf] = &dirEntry{
				info: fileInfo{name: leaf, isDir: isDir, size: size},
			}
			return
		}
		if isDir && !existing.info.isDir {
			existing.info.isDir = true
			existing.info.size = 0
		}
	}

	for rows.Next() {
		var p string
		var size int64
		if err := rows.Scan(&p, &size); err != nil {
			return nil, nil, err
		}
		files[p] = size

		// Walk leaf → root, registering this row in its dir, then
		// each ancestor as a sub-directory of the next ancestor up.
		cur := p
		curIsDir := false
		curSize := size
		for {
			parent, leaf := path.Split(cur)
			parent = strings.TrimSuffix(parent, "/")
			if parent == "" {
				parent = "."
			}
			addChild(parent, leaf, curIsDir, curSize)
			if parent == "." {
				break
			}
			cur = parent
			curIsDir = true
			curSize = 0
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	dirEntries := map[string][]fs.DirEntry{}
	for parent, kids := range children {
		entries := make([]fs.DirEntry, 0, len(kids))
		for _, e := range kids {
			entries = append(entries, e)
		}
		slices.SortFunc(entries, func(a, b fs.DirEntry) int {
			return strings.Compare(a.Name(), b.Name())
		})
		dirEntries[parent] = entries
	}
	// Empty particle still has a walkable root.
	if _, ok := dirEntries["."]; !ok {
		dirEntries["."] = nil
	}
	return files, dirEntries, nil
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

// -----------------------------------------------------------------------------
// dbFS — lazy fs.FS over particle_registry_files
// -----------------------------------------------------------------------------

// dbFS is an fs.FS that serves files for a single (name, version)
// out of particle_registry_files. Its path/size index is snapshotted
// in memory at construction time (see [loadSnapshot]); blob bytes
// stay in SQLite and stream out lazily on Open / ReadFile.
//
// fs.FS doesn't carry a context.Context (the interface predates ctx
// in stdlib I/O), so internal queries use context.Background(). The
// trade-off: a caller's cancellation won't propagate into a wasi
// guest's mid-read SQL query. SQLite queries against a local file
// finish in microseconds, so this is rarely visible — and the
// alternative (stashing ctx on the struct) draws a sharper objection
// from the Go community than the lost cancellation does in practice.
type dbFS struct {
	db      *sql.DB
	name    string
	version string

	files      map[string]int64        // path → blob size
	dirEntries map[string][]fs.DirEntry // dir path → sorted children
}

var (
	_ fs.FS         = (*dbFS)(nil)
	_ fs.StatFS     = (*dbFS)(nil)
	_ fs.ReadFileFS = (*dbFS)(nil)
	_ fs.ReadDirFS  = (*dbFS)(nil)
)

// Open implements fs.FS. Stat (snapshot lookup) is free; only file
// opens hit the DB — one SELECT for the blob bytes.
func (f *dbFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if size, ok := f.files[name]; ok {
		data, err := f.queryBlob(name)
		if err != nil {
			return nil, &fs.PathError{Op: "open", Path: name, Err: err}
		}
		return &fileHandle{name: name, size: size, reader: bytesReader{data: data}}, nil
	}
	if entries, ok := f.dirEntries[name]; ok {
		return &dirHandle{name: name, entries: entries}, nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// Stat implements fs.StatFS — pure in-memory snapshot lookup. The
// hottest path in the runtime: Python's import system stats every
// candidate it probes (most of which don't exist).
func (f *dbFS) Stat(name string) (fs.FileInfo, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrInvalid}
	}
	if size, ok := f.files[name]; ok {
		return &fileInfo{name: path.Base(name), size: size}, nil
	}
	if _, ok := f.dirEntries[name]; ok {
		return synthDirInfo(path.Base(name)), nil
	}
	return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
}

// ReadFile implements fs.ReadFileFS. Snapshot lookup for the
// existence check, then one SELECT for the blob bytes.
func (f *dbFS) ReadFile(name string) ([]byte, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if _, ok := f.files[name]; !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	data, err := f.queryBlob(name)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	return data, nil
}

// ReadDir implements fs.ReadDirFS — pure in-memory snapshot lookup.
func (f *dbFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	entries, ok := f.dirEntries[name]
	if !ok {
		if _, isFile := f.files[name]; isFile {
			return nil, &fs.PathError{Op: "open", Path: name, Err: errNotDir}
		}
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return entries, nil
}

// queryBlob fetches a single row's data blob. Caller should have
// already verified the path exists via the snapshot; a SQL miss
// here would indicate the row was deleted out from under us
// (treated as ErrNotExist).
func (f *dbFS) queryBlob(name string) ([]byte, error) {
	var data []byte
	err := f.db.QueryRowContext(context.Background(),
		`SELECT data FROM particle_registry_files
		 WHERE name = ? AND version = ? AND path = ?`,
		f.name, f.version, name).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fs.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

// errNotDir is the sentinel returned when something asks for a
// directory listing of a file path.
var errNotDir = errors.New("not a directory")

// fileInfo / dirEntry / dirHandle / fileHandle / bytesReader / synthDirInfo
// are the io/fs adapter glue around the lazy-query primitives above.

type fileInfo struct {
	name  string
	size  int64
	isDir bool
}

func (i *fileInfo) Name() string { return i.name }
func (i *fileInfo) Size() int64  { return i.size }
func (i *fileInfo) Mode() fs.FileMode {
	if i.isDir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}
func (i *fileInfo) ModTime() time.Time { return time.Time{} }
func (i *fileInfo) IsDir() bool        { return i.isDir }
func (i *fileInfo) Sys() any           { return nil }

func synthDirInfo(name string) *fileInfo {
	return &fileInfo{name: name, isDir: true}
}

type dirEntry struct {
	info fileInfo
}

func (e *dirEntry) Name() string               { return e.info.name }
func (e *dirEntry) IsDir() bool                { return e.info.isDir }
func (e *dirEntry) Type() fs.FileMode          { return e.info.Mode().Type() }
func (e *dirEntry) Info() (fs.FileInfo, error) { return &e.info, nil }

type fileHandle struct {
	name   string
	size   int64
	reader bytesReader
}

func (f *fileHandle) Stat() (fs.FileInfo, error) {
	return &fileInfo{name: path.Base(f.name), size: f.size}, nil
}

func (f *fileHandle) Read(p []byte) (int, error) {
	return f.reader.Read(p)
}

func (f *fileHandle) Close() error { return nil }

// bytesReader is a tiny io.Reader over a byte slice. We don't use
// bytes.Reader to keep the Particle FS allocation footprint down —
// one extra allocation per Open isn't free when called thousands of
// times during a Python interpreter init.
type bytesReader struct {
	data []byte
	pos  int
}

func (r *bytesReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

type dirHandle struct {
	name    string
	entries []fs.DirEntry
	offset  int
}

func (d *dirHandle) Stat() (fs.FileInfo, error) {
	return synthDirInfo(path.Base(d.name)), nil
}

func (d *dirHandle) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.name, Err: fs.ErrInvalid}
}

func (d *dirHandle) Close() error { return nil }

func (d *dirHandle) ReadDir(n int) ([]fs.DirEntry, error) {
	remaining := d.entries[d.offset:]
	// fs.ReadDir contract: at EOF, ReadDir(n>0) returns io.EOF;
	// ReadDir(n<=0) returns the (possibly empty) remainder with
	// a nil error.
	if n > 0 && len(remaining) == 0 {
		return nil, io.EOF
	}
	if n <= 0 || n > len(remaining) {
		n = len(remaining)
	}
	out := append([]fs.DirEntry(nil), remaining[:n]...)
	d.offset += n
	return out, nil
}
