// Package osmount adapts a host directory into a writable [io/fs.FS]
// suitable for a particle mount. It is backed by an [os.Root], so every
// path the wasm guest resolves is confined to the mounted directory —
// symlink and ".." escapes are rejected by the kernel-checked Root, not
// by string munging.
//
// The directory files it hands out implement the wasi:filesystem
// preopen capability interfaces (OpenAter, MkdirAter, UnlinkAter,
// StatAter, …) so the guest can create, write, and delete within the
// mount; regular files are returned as *os.File, which already satisfy
// the write/seek/truncate interfaces the host probes for. Wrap the
// result in quotafs to add a byte cap (temp mounts), or in the runtime's
// read-only wrapper to enforce a read-only mount.
//
// [FS.Close] releases the os.Root. Closing per-file handles the guest
// left open is the runtime's concern, not this package's: the runtime
// wraps every mount FS (osmount or otherwise) in a tracking wrapper that
// reclaims leaked handles at instance teardown.
//
// This package is host/OS-specific by design and lives under internal/
// so the SDK-agnostic runtime never imports os.Root directly.
package osmount

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/partite-ai/wacogo/wasi/filesystem/preopens"
)

// FS is a writable fs.FS rooted at a host directory. Construct it with
// [New]; close it with [FS.Close] to release the underlying os.Root.
type FS struct {
	root *os.Root
}

// New opens dir as an os.Root and returns an FS rooted there. The
// caller owns the returned FS and must Close it to release the root's
// file descriptor.
func New(dir string) (*FS, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &FS{root: root}, nil
}

// Close releases the underlying os.Root.
func (f *FS) Close() error { return f.root.Close() }

// Open implements fs.FS. Names are clean forward-slash paths per the
// io/fs contract; they're translated to the OS separator before hitting
// os.Root. Directories come back as a *dir exposing path-relative write
// ops; regular files come back as *os.File.
func (f *FS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	file, err := f.root.Open(filepath.FromSlash(name))
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if info.IsDir() {
		return &dir{root: f.root, rel: name, file: file}, nil
	}
	return file, nil
}

// dir is an open directory inside the root. It keeps the os.Root plus
// its own forward-slash path relative to the root ("." for the root
// dir) so each path-relative op resolves the full child path through
// os.Root — descending into a subdirectory reuses the same Root rather
// than reopening, and every op stays escape-checked.
type dir struct {
	root *os.Root
	rel  string
	file *os.File
}

func (d *dir) Stat() (fs.FileInfo, error)           { return d.file.Stat() }
func (d *dir) Read(p []byte) (int, error)           { return d.file.Read(p) }
func (d *dir) Close() error                         { return d.file.Close() }
func (d *dir) ReadDir(n int) ([]fs.DirEntry, error) { return d.file.ReadDir(n) }

// child resolves name (a single component or relative subpath) against
// this directory's position, returning the OS-separator path os.Root
// expects.
func (d *dir) child(name string) string {
	return filepath.FromSlash(path.Join(d.rel, name))
}

func (d *dir) OpenAt(name string, flag int, perm fs.FileMode) (fs.File, error) {
	rel := path.Join(d.rel, name)
	file, err := d.root.OpenFile(filepath.FromSlash(rel), flag, perm)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if info.IsDir() {
		return &dir{root: d.root, rel: rel, file: file}, nil
	}
	return file, nil
}

func (d *dir) StatAt(name string) (fs.FileInfo, error)     { return d.root.Stat(d.child(name)) }
func (d *dir) MkdirAt(name string, perm fs.FileMode) error { return d.root.Mkdir(d.child(name), perm) }
func (d *dir) RmdirAt(name string) error                   { return d.root.Remove(d.child(name)) }
func (d *dir) UnlinkAt(name string) error                  { return d.root.Remove(d.child(name)) }
func (d *dir) ReadlinkAt(name string) (string, error)      { return d.root.Readlink(d.child(name)) }

func (d *dir) ChtimesAt(name string, atime, mtime time.Time) error {
	return d.root.Chtimes(d.child(name), atime, mtime)
}

// SymlinkAt creates a symlink at linkName whose target is the literal
// string target. target is the link's contents (not resolved through
// the root); linkName is created inside the root.
func (d *dir) SymlinkAt(target, linkName string) error {
	return d.root.Symlink(target, d.child(linkName))
}

// Compile-time guarantees the directory handle satisfies the capability
// interfaces the wasi:filesystem host probes for.
var (
	_ fs.FS                 = (*FS)(nil)
	_ fs.ReadDirFile        = (*dir)(nil)
	_ preopens.OpenAter     = (*dir)(nil)
	_ preopens.StatAter     = (*dir)(nil)
	_ preopens.MkdirAter    = (*dir)(nil)
	_ preopens.RmdirAter    = (*dir)(nil)
	_ preopens.UnlinkAter   = (*dir)(nil)
	_ preopens.ReadlinkAter = (*dir)(nil)
	_ preopens.ChtimesAter  = (*dir)(nil)
	_ preopens.SymlinkAter  = (*dir)(nil)
)
