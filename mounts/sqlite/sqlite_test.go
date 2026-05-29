package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	mountsqlite "github.com/partite-ai/particles/mounts/sqlite"
)

func TestMountsStore(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	b, err := mountsqlite.New(ctx, db)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := b.Scoped("p1")

	if _, found, err := s.Get(ctx, "data"); err != nil || found {
		t.Fatalf("unexpected mapping: found=%v err=%v", found, err)
	}
	if err := s.Set(ctx, "data", "/host/data"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if v, found, _ := s.Get(ctx, "data"); !found || v != "/host/data" {
		t.Fatalf("Get = %q, %v", v, found)
	}

	// Overwrite.
	if err := s.Set(ctx, "data", "/host/data2"); err != nil {
		t.Fatalf("overwrite Set: %v", err)
	}
	if v, _, _ := s.Get(ctx, "data"); v != "/host/data2" {
		t.Fatalf("overwrite Get = %q, want /host/data2", v)
	}

	// Scope isolation.
	if _, found, _ := b.Scoped("p2").Get(ctx, "data"); found {
		t.Fatal("p2 must not see p1's mapping")
	}

	// List is ordered by name.
	if err := s.Set(ctx, "config", "/host/cfg"); err != nil {
		t.Fatalf("Set config: %v", err)
	}
	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].Name != "config" || list[1].Name != "data" {
		t.Fatalf("List = %+v, want [config, data]", list)
	}

	// Delete is idempotent.
	if err := s.Delete(ctx, "data"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, found, _ := s.Get(ctx, "data"); found {
		t.Fatal("Delete left mapping behind")
	}
	if err := s.Delete(ctx, "data"); err != nil {
		t.Fatalf("idempotent Delete: %v", err)
	}
}
