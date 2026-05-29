package osmount_test

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/partite-ai/wacogo/wasi/filesystem/preopens"

	"github.com/partite-ai/particles/internal/osmount"
)

func newRoot(t *testing.T) (*osmount.FS, string) {
	t.Helper()
	dir := t.TempDir()
	fsys, err := osmount.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = fsys.Close() })
	return fsys, dir
}

func openRootDir(t *testing.T, fsys *osmount.FS) preopens.OpenAter {
	t.Helper()
	f, err := fsys.Open(".")
	if err != nil {
		t.Fatalf("Open .: %v", err)
	}
	oa, ok := f.(preopens.OpenAter)
	if !ok {
		t.Fatal("root dir is not OpenAter")
	}
	return oa
}

func TestWriteReadRoundtrip(t *testing.T) {
	fsys, dir := newRoot(t)
	oa := openRootDir(t, fsys)
	f, err := oa.OpenAt("hello.txt", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.(io.WriterAt).WriteAt([]byte("hi"), 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()
	got, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil || string(got) != "hi" {
		t.Fatalf("readback = %q, %v", got, err)
	}
}

func TestMkdirNestedUnlink(t *testing.T) {
	fsys, dir := newRoot(t)
	oa := openRootDir(t, fsys)
	if err := oa.(preopens.MkdirAter).MkdirAt("sub", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := oa.OpenAt("sub/x.txt", os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("nested create: %v", err)
	}
	f.Close()
	if _, err := os.Stat(filepath.Join(dir, "sub", "x.txt")); err != nil {
		t.Fatalf("stat nested: %v", err)
	}
	if err := oa.(preopens.UnlinkAter).UnlinkAt("sub/x.txt"); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "x.txt")); !os.IsNotExist(err) {
		t.Fatalf("file should be gone, stat err = %v", err)
	}
}

func TestEscapeRejected(t *testing.T) {
	fsys, dir := newRoot(t)
	oa := openRootDir(t, fsys)

	if _, err := oa.OpenAt("../escape.txt", os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		t.Error("OpenAt(../) should be rejected by os.Root")
	}

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := oa.OpenAt("link/secret", os.O_RDONLY, 0); err == nil {
		t.Error("OpenAt through an escaping symlink should be rejected")
	}
}
