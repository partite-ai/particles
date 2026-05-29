package runtime

import (
	"io"
	"os"
	"testing"

	"github.com/partite-ai/wacogo/wasi/filesystem/preopens"

	"github.com/partite-ai/particles/internal/osmount"
)

// TestReadOnlyFS_BlocksWrites verifies the wrapper enforces read-only
// over a fully-writable backing FS (os.Root) — the case preopens.
// ImmutableFS does NOT cover.
func TestReadOnlyFS_BlocksWrites(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/data.txt", []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	writable, err := osmount.New(dir) // backing FS is writable
	if err != nil {
		t.Fatalf("osmount.New: %v", err)
	}
	defer writable.Close()

	// Sanity: the backing dir really is writable, so it's the wrapper —
	// not the backing store — that enforces read-only below.
	if wroot, _ := writable.Open("."); wroot != nil {
		if _, ok := wroot.(preopens.MkdirAter); !ok {
			t.Fatal("expected osmount dir to be writable (MkdirAter)")
		}
		_ = wroot.Close()
	}

	root, err := readOnlyFS{fsys: writable}.Open(".")
	if err != nil {
		t.Fatalf("Open .: %v", err)
	}
	oa, ok := root.(preopens.OpenAter)
	if !ok {
		t.Fatal("root is not OpenAter")
	}

	// No mutating capability is exposed on the directory.
	for name, exposed := range map[string]bool{
		"MkdirAter":   asserts[preopens.MkdirAter](root),
		"UnlinkAter":  asserts[preopens.UnlinkAter](root),
		"RmdirAter":   asserts[preopens.RmdirAter](root),
		"SymlinkAter": asserts[preopens.SymlinkAter](root),
		"ChtimesAter": asserts[preopens.ChtimesAter](root),
	} {
		if exposed {
			t.Errorf("read-only dir must not implement %s", name)
		}
	}

	// Opening with a write flag is rejected.
	if _, err := oa.OpenAt("new.txt", os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		t.Error("OpenAt with write flags should be rejected")
	}

	// A read open succeeds; the file exposes no write interface and
	// reads back correctly.
	f, err := oa.OpenAt("data.txt", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("read open: %v", err)
	}
	defer f.Close()
	if _, ok := f.(io.Writer); ok {
		t.Error("read-only file must not implement io.Writer")
	}
	if _, ok := f.(io.WriterAt); ok {
		t.Error("read-only file must not implement io.WriterAt")
	}
	got, err := io.ReadAll(f)
	if err != nil || string(got) != "hello" {
		t.Fatalf("read = %q, %v; want hello", got, err)
	}
}

// asserts reports whether v implements interface T.
func asserts[T any](v any) bool {
	_, ok := v.(T)
	return ok
}
