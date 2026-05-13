package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"path/filepath"
	"reflect"
	"testing"
	"testing/fstest"

	_ "modernc.org/sqlite"

	"github.com/partite-ai/particle/registry"
	"github.com/partite-ai/particle/registry/sqlite"
)

// newStore opens an in-memory SQLite (one DB per test, fully
// isolated) and returns a fresh Store.
func newStore(t *testing.T) *sqlite.Store {
	t.Helper()
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	s, err := sqlite.New(context.Background(), db)
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	return s
}

func sampleParticle() fs.FS {
	return fstest.MapFS{
		"manifest.json":   {Data: []byte(`{"name":"yaml-tools","version":"0.1.0"}`)},
		"bundle.js":       {Data: []byte(`export default {};`)},
		"bundle.js.map":   {Data: []byte(`{"version":3}`)},
		"build-info.json": {Data: []byte(`{"runtime":"0.1.0"}`)},
	}
}

func readAll(t *testing.T, fsys fs.FS) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	if err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		out[path] = data
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// -----------------------------------------------------------------------------
// Put / Get
// -----------------------------------------------------------------------------

func TestPut_Get_RoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	if err := s.Put(ctx, "yaml-tools", "0.1.0", sampleParticle()); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(ctx, "yaml-tools", "0.1.0")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "yaml-tools" || got.Version != "0.1.0" {
		t.Errorf("entry header = %+v", got)
	}
	want := readAll(t, sampleParticle())
	have := readAll(t, got.Particle)
	if !reflect.DeepEqual(want, have) {
		t.Errorf("contents mismatch:\nwant keys: %v\nhave keys: %v", keysOf(want), keysOf(have))
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestGet_Missing(t *testing.T) {
	s := newStore(t)
	if _, err := s.Get(context.Background(), "absent", "0.0.0"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// Re-Put for the same (name, version) replaces the bytes — and a
// shrunk FS doesn't leave orphan files from the prior version.
func TestPut_ReplacesAndDoesNotLeaveOrphans(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	first := fstest.MapFS{
		"manifest.json": {Data: []byte(`{"v":1}`)},
		"bundle.js":     {Data: []byte("v1")},
		"extra.json":    {Data: []byte(`{}`)},
	}
	second := fstest.MapFS{
		"manifest.json": {Data: []byte(`{"v":2}`)},
		"bundle.js":     {Data: []byte("v2")},
	}

	if err := s.Put(ctx, "p", "1.0.0", first); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "p", "1.0.0", second); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "p", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	have := readAll(t, got.Particle)
	if string(have["manifest.json"]) != `{"v":2}` {
		t.Errorf("manifest = %q, want v:2", have["manifest.json"])
	}
	if string(have["bundle.js"]) != "v2" {
		t.Errorf("bundle = %q, want v2", have["bundle.js"])
	}
	if _, present := have["extra.json"]; present {
		t.Error("extra.json from the prior Put was not cleared")
	}
}

// Put accepts every shape of valid SemVer 2.0.0 and rejects the
// common invalid forms. The semver gate runs at registration so
// the registry's "highest version" lookups and on-disk format
// can rely on every stored version being parseable.
func TestPut_VersionMustBeValidSemver(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for _, v := range []string{
		"0.1.0",
		"1.2.3",
		"10.0.0",
		"1.0.0-rc.1",
		"1.0.0-alpha+build.7",
		"0.0.0",
	} {
		t.Run("ok/"+v, func(t *testing.T) {
			if err := s.Put(ctx, "p-"+v, v, sampleParticle()); err != nil {
				t.Errorf("Put rejected valid semver %q: %v", v, err)
			}
		})
	}
	for _, v := range []string{
		"",            // empty
		"v1.2.3",      // leading 'v' isn't manifest convention
		"1.2",         // missing patch
		"1.2.3.4",     // too many segments
		"01.0.0",      // leading-zero in numeric identifier
		"latest",      // non-numeric
		"1.2.3-",      // dangling prerelease
		"1.2.3+",      // dangling build
	} {
		t.Run("bad/"+v, func(t *testing.T) {
			if err := s.Put(ctx, "p-bad", v, sampleParticle()); err == nil {
				t.Errorf("Put accepted invalid semver %q", v)
			}
		})
	}
}

func TestPut_RejectsEmptyArgs(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if err := s.Put(ctx, "", "0.0.0", sampleParticle()); err == nil {
		t.Error("Put with empty name should error")
	}
	if err := s.Put(ctx, "p", "", sampleParticle()); err == nil {
		t.Error("Put with empty version should error")
	}
	if err := s.Put(ctx, "p", "0.0.0", nil); err == nil {
		t.Error("Put with nil FS should error")
	}
}

// -----------------------------------------------------------------------------
// List
// -----------------------------------------------------------------------------

func TestList_Sorted(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for _, e := range []registry.ListEntry{
		{Name: "yaml-tools", Version: "0.2.0"},
		{Name: "yaml-tools", Version: "0.1.0"},
		{Name: "json-tools", Version: "0.1.0"},
	} {
		if err := s.Put(ctx, e.Name, e.Version, sampleParticle()); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []registry.ListEntry{
		{Name: "json-tools", Version: "0.1.0"},
		{Name: "yaml-tools", Version: "0.1.0"},
		{Name: "yaml-tools", Version: "0.2.0"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List = %+v, want %+v", got, want)
	}
}

func TestList_Empty(t *testing.T) {
	s := newStore(t)
	got, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List = %v, want empty", got)
	}
}

// -----------------------------------------------------------------------------
// Delete
// -----------------------------------------------------------------------------

func TestDelete_RemovesIndexAndFiles(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_ = s.Put(ctx, "p", "1.0.0", sampleParticle())

	if err := s.Delete(ctx, "p", "1.0.0"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, "p", "1.0.0"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("after Delete, Get err = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "p", "1.0.0"); err != nil {
		t.Errorf("second Delete: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Persistence
// -----------------------------------------------------------------------------

func TestPersistsAcrossOpens(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "registry.db")
	dsn := "file:" + dbPath
	ctx := context.Background()

	db1, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	s1, err := sqlite.New(ctx, db1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.Put(ctx, "yaml-tools", "0.1.0", sampleParticle()); err != nil {
		t.Fatal(err)
	}
	if err := db1.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	s2, err := sqlite.New(ctx, db2)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s2.Get(ctx, "yaml-tools", "0.1.0")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	want := readAll(t, sampleParticle())
	have := readAll(t, got.Particle)
	if !reflect.DeepEqual(want, have) {
		t.Errorf("contents mismatch after reopen")
	}
}

func TestNew_IdempotentMigration(t *testing.T) {
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	if _, err := sqlite.New(ctx, db); err != nil {
		t.Fatalf("first New: %v", err)
	}
	if _, err := sqlite.New(ctx, db); err != nil {
		t.Fatalf("second New: %v", err)
	}
}

func TestNew_RejectsNilDB(t *testing.T) {
	if _, err := sqlite.New(context.Background(), nil); err == nil {
		t.Error("New(nil) should error")
	}
}

// -----------------------------------------------------------------------------
// Schema-migration: the legacy particle_settings table is dropped
// on open; pre-existing rows aren't migrated anywhere — they're
// already mirrored in the credentials store (the new home for
// "which method is configured"). This test confirms the
// destructive part of the migration runs without complaint.
// -----------------------------------------------------------------------------

func TestMigrate_DropsLegacyParticleSettings(t *testing.T) {
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Seed the legacy table BEFORE Store.New runs migrations.
	if _, err := db.Exec(`CREATE TABLE particle_settings (
		name                            TEXT PRIMARY KEY,
		selected_authentication_method  TEXT
	)`); err != nil {
		t.Fatalf("seed legacy table: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO particle_settings VALUES ('p', 'oauth')`); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	// New should drop the table silently.
	if _, err := sqlite.New(context.Background(), db); err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}

	var count int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='particle_settings'`).Scan(&count); err != nil {
		t.Fatalf("probe sqlite_master: %v", err)
	}
	if count != 0 {
		t.Errorf("particle_settings still present after migration; want dropped")
	}
}
