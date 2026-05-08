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
// SelectedAuthenticationMethod
// -----------------------------------------------------------------------------

func TestSetSelectedAuthenticationMethod_RoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if err := s.Put(ctx, "p", "1.0.0", sampleParticle()); err != nil {
		t.Fatal(err)
	}
	// Default: no method selected.
	got, err := s.Get(ctx, "p", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if got.SelectedAuthenticationMethod != "" {
		t.Errorf("fresh entry SelectedAuthenticationMethod = %q, want empty", got.SelectedAuthenticationMethod)
	}

	// Set, read back.
	if err := s.SetSelectedAuthenticationMethod(ctx, "p", "oauth"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, "p", "1.0.0")
	if got.SelectedAuthenticationMethod != "oauth" {
		t.Errorf("after Set, SelectedAuthenticationMethod = %q", got.SelectedAuthenticationMethod)
	}

	// Update to a different method.
	if err := s.SetSelectedAuthenticationMethod(ctx, "p", "pat"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, "p", "1.0.0")
	if got.SelectedAuthenticationMethod != "pat" {
		t.Errorf("after second Set, SelectedAuthenticationMethod = %q", got.SelectedAuthenticationMethod)
	}

	// Clear.
	if err := s.SetSelectedAuthenticationMethod(ctx, "p", ""); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, "p", "1.0.0")
	if got.SelectedAuthenticationMethod != "" {
		t.Errorf("after clear, SelectedAuthenticationMethod = %q, want empty", got.SelectedAuthenticationMethod)
	}
}

func TestSetSelectedAuthenticationMethod_MissingEntry(t *testing.T) {
	s := newStore(t)
	if err := s.SetSelectedAuthenticationMethod(context.Background(), "absent", "oauth"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// Re-Put doesn't touch SelectedAuthenticationMethod (it lives in
// particle_settings, not particle_registry). Re-builds for the
// same (name, version) leave the auth-method choice in place.
func TestPut_PreservesSelectedAuthenticationMethod(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_ = s.Put(ctx, "p", "1.0.0", sampleParticle())
	_ = s.SetSelectedAuthenticationMethod(ctx, "p", "oauth")

	// Re-Put with new bytes.
	updated := fstest.MapFS{}
	for k, v := range sampleParticle().(fstest.MapFS) {
		updated[k] = v
	}
	updated["new-file.txt"] = &fstest.MapFile{Data: []byte("hi")}
	if err := s.Put(ctx, "p", "1.0.0", updated); err != nil {
		t.Fatal(err)
	}

	got, _ := s.Get(ctx, "p", "1.0.0")
	if got.SelectedAuthenticationMethod != "oauth" {
		t.Errorf("after re-Put, SelectedAuthenticationMethod = %q, want oauth (preserved)", got.SelectedAuthenticationMethod)
	}
}

// When the last version of a particle is deleted, the per-name
// settings row (which had no other version to belong to) is
// cleared. A re-import then doesn't carry stale state from the
// previous incarnation.
func TestDelete_LastVersion_ClearsSelectedAuthenticationMethod(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_ = s.Put(ctx, "p", "0.1.0", sampleParticle())
	_ = s.SetSelectedAuthenticationMethod(ctx, "p", "oauth")

	if err := s.Delete(ctx, "p", "0.1.0"); err != nil {
		t.Fatal(err)
	}

	// Re-register and read back: the selection must be empty.
	if err := s.Put(ctx, "p", "0.1.0", sampleParticle()); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, "p", "0.1.0")
	if got.SelectedAuthenticationMethod != "" {
		t.Errorf("after delete-and-reimport, SelectedAuthenticationMethod = %q, want empty",
			got.SelectedAuthenticationMethod)
	}
}

// Deleting one version with siblings still in place leaves the
// selection alone — every remaining version still benefits from
// the saved auth choice.
func TestDelete_OneOfManyVersions_PreservesSelection(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_ = s.Put(ctx, "p", "0.1.0", sampleParticle())
	_ = s.Put(ctx, "p", "0.2.0", sampleParticle())
	_ = s.SetSelectedAuthenticationMethod(ctx, "p", "oauth")

	if err := s.Delete(ctx, "p", "0.1.0"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, "p", "0.2.0")
	if got.SelectedAuthenticationMethod != "oauth" {
		t.Errorf("after deleting one version, surviving version's SelectedAuthenticationMethod = %q",
			got.SelectedAuthenticationMethod)
	}
}

// List reports the per-name selection on every row of the same
// name (since selection is per-name).
func TestList_IncludesSelectedAuthenticationMethod(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_ = s.Put(ctx, "p", "0.1.0", sampleParticle())
	_ = s.Put(ctx, "p", "0.2.0", sampleParticle())
	_ = s.Put(ctx, "q", "0.1.0", sampleParticle())
	_ = s.SetSelectedAuthenticationMethod(ctx, "p", "oauth")

	got, _ := s.List(ctx)
	want := map[string]string{
		"p@0.1.0": "oauth",
		"p@0.2.0": "oauth",
		"q@0.1.0": "",
	}
	for _, e := range got {
		key := e.Name + "@" + e.Version
		if w, ok := want[key]; ok && w != e.SelectedAuthenticationMethod {
			t.Errorf("%s SelectedAuthenticationMethod = %q, want %q", key, e.SelectedAuthenticationMethod, w)
		}
	}
}

// Selection is per-particle-name: every registered version of the
// same particle reads back the same value. (The credentials store
// is keyed by name only, so a per-version selection wouldn't be
// meaningful.)
func TestSelectedAuthenticationMethod_SharedAcrossVersions(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_ = s.Put(ctx, "p", "0.1.0", sampleParticle())
	_ = s.Put(ctx, "p", "0.2.0", sampleParticle())
	if err := s.SetSelectedAuthenticationMethod(ctx, "p", "oauth"); err != nil {
		t.Fatal(err)
	}

	for _, v := range []string{"0.1.0", "0.2.0"} {
		got, err := s.Get(ctx, "p", v)
		if err != nil {
			t.Fatalf("Get %s: %v", v, err)
		}
		if got.SelectedAuthenticationMethod != "oauth" {
			t.Errorf("%s SelectedAuthenticationMethod = %q, want oauth", v, got.SelectedAuthenticationMethod)
		}
	}

	// Updating once propagates everywhere.
	_ = s.SetSelectedAuthenticationMethod(ctx, "p", "pat")
	for _, v := range []string{"0.1.0", "0.2.0"} {
		got, _ := s.Get(ctx, "p", v)
		if got.SelectedAuthenticationMethod != "pat" {
			t.Errorf("%s after update SelectedAuthenticationMethod = %q, want pat", v, got.SelectedAuthenticationMethod)
		}
	}
}
