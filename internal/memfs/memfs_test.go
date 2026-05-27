package memfs

import (
	"io"
	"io/fs"
	"slices"
	"testing"
	"testing/fstest"
)

// TestFS_Stdlib_Compliance runs the io/fs reference test suite
// (testing/fstest.TestFS) against a populated memfs.FS. The reference
// suite is the standard way to certify any io/fs implementation —
// it exercises every fs.FS extension interface, edge cases around
// path normalization, directory listing, etc.
func TestFS_Stdlib_Compliance(t *testing.T) {
	fsys := FS{
		"hello.txt":              &File{Data: []byte("hello")},
		"deps/site-packages/a.py": &File{Data: []byte("x = 1\n")},
		"deps/site-packages/sub/b.py": &File{Data: []byte("y = 2\n")},
		"deps/INFO":              &File{Data: []byte("info\n")},
	}
	if err := fstest.TestFS(fsys,
		"hello.txt",
		"deps/site-packages/a.py",
		"deps/site-packages/sub/b.py",
		"deps/INFO",
	); err != nil {
		t.Fatal(err)
	}
}

// TestFS_ReadFile reads a leaf file end-to-end and confirms bytes
// round-trip without modification.
func TestFS_ReadFile(t *testing.T) {
	want := []byte("the quick brown fox\n")
	fsys := FS{"a/b/c.txt": &File{Data: want}}

	got, err := fs.ReadFile(fsys, "a/b/c.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("ReadFile = %q, want %q", got, want)
	}
}

// TestFS_SynthesizedDirs ensures intermediate dirs are walkable
// even though we only added the leaf. The build pipeline + wasi
// preopen path both rely on this — they list parent directories
// without ever calling Add(dir) explicitly.
func TestFS_SynthesizedDirs(t *testing.T) {
	fsys := FS{
		"x/y/leaf1.txt": &File{Data: []byte("1")},
		"x/y/leaf2.txt": &File{Data: []byte("2")},
		"x/z/leaf3.txt": &File{Data: []byte("3")},
	}

	// Walk and collect every entry we see.
	var entries []string
	if err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		entries = append(entries, p)
		return nil
	}); err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	want := []string{".", "x", "x/y", "x/y/leaf1.txt", "x/y/leaf2.txt", "x/z", "x/z/leaf3.txt"}
	slices.Sort(entries)
	slices.Sort(want)
	if !slices.Equal(entries, want) {
		t.Errorf("walk = %q, want %q", entries, want)
	}
}

// TestFS_NotExist confirms missing paths give back a real
// fs.ErrNotExist (not a wrapped string error) so callers can
// errors.Is correctly.
func TestFS_NotExist(t *testing.T) {
	fsys := FS{"a.txt": &File{Data: []byte("a")}}

	_, err := fsys.Open("b.txt")
	if err == nil {
		t.Fatal("Open of missing file: want error, got nil")
	}
	pe, ok := err.(*fs.PathError)
	if !ok {
		t.Fatalf("err type = %T, want *fs.PathError", err)
	}
	if pe.Err != fs.ErrNotExist {
		t.Errorf("err.Err = %v, want fs.ErrNotExist", pe.Err)
	}
}

// TestFS_ReadAt covers the *openFile ReadAt implementation —
// archive writers (the .particle zip packer) sometimes use ReadAt
// instead of sequential Read.
func TestFS_ReadAt(t *testing.T) {
	fsys := FS{"a.txt": &File{Data: []byte("0123456789")}}
	f, err := fsys.Open("a.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	ra, ok := f.(io.ReaderAt)
	if !ok {
		t.Fatal("Open's file doesn't implement io.ReaderAt")
	}
	buf := make([]byte, 4)
	if _, err := ra.ReadAt(buf, 3); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(buf) != "3456" {
		t.Errorf("ReadAt(buf, 3) = %q, want %q", buf, "3456")
	}
}
