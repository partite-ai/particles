package main

import (
	"context"
	"path/filepath"
	"testing"
)

// TestOpenStateDB_AppliesPragmas asserts that openStateDB hands back
// a DB with the hot-path pragmas we rely on. These tunings make
// Python's import-time stat storm cheap; a regression that quietly
// reverted any of them would re-introduce the per-query fcntl()
// dance.
func TestOpenStateDB_AppliesPragmas(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := openStateDB(dbPath)
	if err != nil {
		t.Fatalf("openStateDB: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	// PRAGMAs whose returned values we can validate. We deliberately
	// check journal_mode and synchronous as strings/ints because
	// SQLite returns them in mixed forms; the others come back as
	// integers.
	cases := []struct {
		pragma string
		want   string
	}{
		{"journal_mode", "wal"},
		{"synchronous", "1"}, // NORMAL=1, FULL=2
		{"busy_timeout", "5000"},
		// cache_size: stored as the value we passed (negative means KB).
		{"cache_size", "-64000"},
		{"mmap_size", "268435456"},
		{"temp_store", "2"}, // MEMORY=2, FILE=1, DEFAULT=0
		{"foreign_keys", "1"},
	}
	for _, tc := range cases {
		var got string
		if err := db.QueryRowContext(ctx, "PRAGMA "+tc.pragma).Scan(&got); err != nil {
			t.Errorf("PRAGMA %s: %v", tc.pragma, err)
			continue
		}
		if got != tc.want {
			t.Errorf("PRAGMA %s = %q, want %q", tc.pragma, got, tc.want)
		}
	}
}
