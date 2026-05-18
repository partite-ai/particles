package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/partite-ai/particles/kv"
	"github.com/partite-ai/particles/kv/sqlite"
)

// newStore opens an in-memory SQLite (one DB per test, fully
// isolated) and returns a fresh Store. cache=shared keeps the
// in-memory database reachable across the connection pool's
// distinct connections.
func newStore(t *testing.T) *sqlite.Backend {
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

func TestSet_Get(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if err := s.Set(ctx, "p", "k", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, found, err := s.Get(ctx, "p", "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	if v != "v" {
		t.Errorf("v = %q, want v", v)
	}
}

func TestGet_NotFound(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, found, err := s.Get(ctx, "p", "missing")
	if err != nil {
		t.Errorf("err = %v, want nil for missing key", err)
	}
	if found {
		t.Error("found = true for missing key")
	}

	_, found, err = s.Get(ctx, "absent", "any")
	if err != nil {
		t.Errorf("err = %v, want nil for absent particle", err)
	}
	if found {
		t.Error("found = true for absent particle")
	}
}

func TestSet_Replaces(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_ = s.Set(ctx, "p", "k", "v1")
	_ = s.Set(ctx, "p", "k", "v2")
	v, _, _ := s.Get(ctx, "p", "k")
	if v != "v2" {
		t.Errorf("v = %q, want v2 after replace", v)
	}
}

func TestDelete_Idempotent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	if err := s.Delete(ctx, "p", "missing"); err != nil {
		t.Errorf("Delete on missing: %v", err)
	}

	_ = s.Set(ctx, "p", "k", "v")
	if err := s.Delete(ctx, "p", "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, found, _ := s.Get(ctx, "p", "k"); found {
		t.Error("entry still present after Delete")
	}
	if err := s.Delete(ctx, "p", "k"); err != nil {
		t.Errorf("second Delete: %v", err)
	}
}

func TestList_PrefixFilter(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for k, v := range map[string]string{
		"users.alice": "1",
		"users.bob":   "2",
		"posts.foo":   "x",
	} {
		_ = s.Set(ctx, "p", k, v)
	}

	got, err := s.List(ctx, "p", "users.")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"users.alice", "users.bob"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List = %v, want %v", got, want)
	}
}

func TestList_EmptyPrefixReturnsAll(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_ = s.Set(ctx, "p", "a", "1")
	_ = s.Set(ctx, "p", "b", "2")

	got, _ := s.List(ctx, "p", "")
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("List empty-prefix = %v", got)
	}
}

func TestList_AbsentParticle(t *testing.T) {
	s := newStore(t)
	got, err := s.List(context.Background(), "absent", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List = %v, want empty", got)
	}
}

// LIKE meta-characters (`%`, `_`) in a prefix must NOT be
// reinterpreted as wildcards — they're escaped in the query.
func TestList_PrefixWithLikeMetacharacters(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_ = s.Set(ctx, "p", "100%off", "yes")
	_ = s.Set(ctx, "p", "10ABoff", "no")
	_ = s.Set(ctx, "p", "underscore_here", "yes")
	_ = s.Set(ctx, "p", "underscoreXhere", "no")

	got, _ := s.List(ctx, "p", "100%")
	if !reflect.DeepEqual(got, []string{"100%off"}) {
		t.Errorf("`100%%` prefix matched %v, want only [100%%off]", got)
	}

	got, _ = s.List(ctx, "p", "underscore_")
	if !reflect.DeepEqual(got, []string{"underscore_here"}) {
		t.Errorf("`underscore_` prefix matched %v, want only [underscore_here]", got)
	}
}

func TestScopedByParticle(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_ = s.Set(ctx, "yaml-tools", "shared", "yaml-value")
	_ = s.Set(ctx, "json-tools", "shared", "json-value")

	v1, _, _ := s.Get(ctx, "yaml-tools", "shared")
	v2, _, _ := s.Get(ctx, "json-tools", "shared")
	if v1 != "yaml-value" {
		t.Errorf("yaml-tools/shared = %q", v1)
	}
	if v2 != "json-value" {
		t.Errorf("json-tools/shared = %q", v2)
	}

	keys, _ := s.List(ctx, "yaml-tools", "")
	if len(keys) != 1 || keys[0] != "shared" {
		t.Errorf("yaml-tools list = %v, want [shared]", keys)
	}
}

// -----------------------------------------------------------------------------
// Quota
// -----------------------------------------------------------------------------

func TestQuota_DefaultUnlimited(t *testing.T) {
	s := newStore(t) // QuotaBytes = 0 means no quota
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		if err := s.Set(ctx, "p", "k", "v"); err != nil {
			t.Fatalf("set #%d: %v", i, err)
		}
	}
}

func TestQuota_Enforced(t *testing.T) {
	s := newStore(t)
	s.QuotaBytes = 20

	ctx := context.Background()
	if err := s.Set(ctx, "p", "k1", "value-one"); err != nil { // 2 + 9 = 11 bytes
		t.Fatalf("first set: %v", err)
	}
	// Next write would push us over: 11 (existing) + 2 + 9 = 22 > 20.
	err := s.Set(ctx, "p", "k2", "value-two")
	if !errors.Is(err, kv.ErrQuotaExceeded) {
		t.Errorf("err = %v, want kv.ErrQuotaExceeded", err)
	}

	if err := s.Set(ctx, "p", "k1", "value-mod"); err != nil {
		t.Errorf("same-size replace: %v", err)
	}
	if err := s.Set(ctx, "p", "k1", "v"); err != nil {
		t.Errorf("smaller replace: %v", err)
	}
	if err := s.Set(ctx, "p", "k2", "v"); err != nil {
		t.Errorf("after freeing room: %v", err)
	}
}

func TestQuota_PerParticle(t *testing.T) {
	s := newStore(t)
	s.QuotaBytes = 10
	ctx := context.Background()

	if err := s.Set(ctx, "yaml-tools", "k", "1234567"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "json-tools", "k", "1234567"); err != nil {
		t.Errorf("json-tools blocked by yaml-tools' quota usage: %v", err)
	}
}

// The usage counter must stay in lockstep with the data table
// across mixed Set / Set-replace / Delete sequences. We sneak in
// via the underlying *sql.DB to verify the counter matches a
// recomputed SUM — quota correctness depends on the invariant.
func TestQuota_UsageCounterStaysInSync(t *testing.T) {
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	s, err := sqlite.New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	checkInSync := func(particle string, where string) {
		t.Helper()
		var counted, summed sql.NullInt64
		_ = db.QueryRowContext(ctx,
			`SELECT bytes FROM particle_kv_usage WHERE particle = ?`, particle).Scan(&counted)
		_ = db.QueryRowContext(ctx,
			`SELECT SUM(LENGTH(key) + LENGTH(value)) FROM particle_kv WHERE particle = ?`, particle).Scan(&summed)
		// SUM over an empty namespace is NULL — the usage row
		// should have been pruned, so both should be NULL.
		if counted.Valid != summed.Valid || counted.Int64 != summed.Int64 {
			t.Errorf("%s: usage=%v sum=%v (out of sync)", where, counted, summed)
		}
	}

	_ = s.Set(ctx, "p", "alpha", "first-value")
	checkInSync("p", "after first Set")

	_ = s.Set(ctx, "p", "beta", "second")
	checkInSync("p", "after second Set")

	// Same-key replace: delta path.
	_ = s.Set(ctx, "p", "alpha", "longer-replacement-value")
	checkInSync("p", "after replace-larger")

	_ = s.Set(ctx, "p", "alpha", "x")
	checkInSync("p", "after replace-smaller")

	_ = s.Delete(ctx, "p", "beta")
	checkInSync("p", "after Delete")

	// Last key gone — usage row should have been pruned.
	_ = s.Delete(ctx, "p", "alpha")
	checkInSync("p", "after final Delete")

	var rows int
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM particle_kv_usage WHERE particle = ?`, "p").Scan(&rows)
	if rows != 0 {
		t.Errorf("after final Delete, expected usage row pruned; got %d row(s)", rows)
	}

	// Idempotent Delete on missing key shouldn't resurrect a usage row.
	_ = s.Delete(ctx, "p", "nothing")
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM particle_kv_usage WHERE particle = ?`, "p").Scan(&rows)
	if rows != 0 {
		t.Errorf("after no-op Delete, usage row count = %d, want 0", rows)
	}
}

// -----------------------------------------------------------------------------
// Persistence — the whole point of the SQLite backend.
// -----------------------------------------------------------------------------

func TestPersistsAcrossOpens(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "kv.db")
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
	if err := s1.Set(ctx, "yaml-tools", "k", "v"); err != nil {
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

	v, found, err := s2.Get(ctx, "yaml-tools", "k")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if !found || v != "v" {
		t.Errorf("after reopen: v=%q found=%v, want v=%q found=true", v, found, "v")
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
