package runtime

import (
	"io/fs"
	"path"
	"strings"
	"time"
)

// mountedFS is an fs.FS that overlays a set of named sub-filesystems
// at fixed mount points under a common root. Open requests are
// routed to the longest-prefix-matching mount, with the mount's
// prefix stripped before delegating.
//
// Used by the python runtime to expose the embedded CPython stdlib
// at /usr/local/lib/python3.14 and the user's bundle at
// /particle/bundle.py simultaneously, served from a single fs.FS
// handed to wacogo's preopens.NewFSPreopens.
type mountedFS struct {
	mounts map[string]fs.FS // mount path (forward-slash, no leading "/") → backing fs
}

func newMountedFS(mounts map[string]fs.FS) *mountedFS {
	return &mountedFS{mounts: mounts}
}

// Open implements fs.FS. For paths that ARE a mount or are ABOVE a
// mount we synthesize a directory entry. For paths inside a mount
// we delegate with the mount's prefix stripped.
func (m *mountedFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	name = path.Clean(name)

	// Find the longest mount prefix that contains `name`.
	var bestPrefix string
	var bestFS fs.FS
	found := false
	for prefix, sub := range m.mounts {
		if prefix == "" {
			if !found {
				bestPrefix, bestFS = "", sub
				found = true
			}
			continue
		}
		if name == prefix || strings.HasPrefix(name, prefix+"/") {
			if !found || len(prefix) > len(bestPrefix) {
				bestPrefix, bestFS = prefix, sub
				found = true
			}
		}
	}
	if found {
		sub := strings.TrimPrefix(strings.TrimPrefix(name, bestPrefix), "/")
		if sub == "" {
			sub = "."
		}
		return bestFS.Open(sub)
	}

	// `name` is at or above a mount but not inside it → synthesize.
	// Collect every mount prefix segment that lives directly under
	// `name`.
	var children []string
	seen := map[string]bool{}
	for prefix := range m.mounts {
		if prefix == "" {
			continue
		}
		rest := ""
		switch {
		case name == ".":
			rest = prefix
		case strings.HasPrefix(prefix, name+"/"):
			rest = strings.TrimPrefix(prefix, name+"/")
		default:
			continue
		}
		first, _, _ := strings.Cut(rest, "/")
		if first != "" && !seen[first] {
			seen[first] = true
			children = append(children, first)
		}
	}
	if len(children) == 0 {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return newSyntheticDir(path.Base(name), children), nil
}

// syntheticDir is a stand-in fs.File for paths above any actual
// mount in a mountedFS (e.g. ".", "usr", "usr/local"). The entries
// it lists are the unique first segments of every deeper mount.
type syntheticDir struct {
	name     string
	children []string
	pos      int
}

func newSyntheticDir(name string, children []string) *syntheticDir {
	return &syntheticDir{name: name, children: children}
}

func (d *syntheticDir) Stat() (fs.FileInfo, error) {
	return syntheticFileInfo{name: d.name, dir: true}, nil
}

func (d *syntheticDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.name, Err: fs.ErrInvalid}
}

func (d *syntheticDir) Close() error { return nil }

func (d *syntheticDir) ReadDir(n int) ([]fs.DirEntry, error) {
	remaining := d.children[d.pos:]
	if n <= 0 || n > len(remaining) {
		n = len(remaining)
	}
	out := make([]fs.DirEntry, 0, n)
	for _, name := range remaining[:n] {
		out = append(out, syntheticDirEntry{name: name})
	}
	d.pos += n
	return out, nil
}

type syntheticDirEntry struct{ name string }

func (e syntheticDirEntry) Name() string { return e.name }
func (e syntheticDirEntry) IsDir() bool  { return true }
func (e syntheticDirEntry) Type() fs.FileMode {
	return fs.ModeDir
}
func (e syntheticDirEntry) Info() (fs.FileInfo, error) {
	return syntheticFileInfo{name: e.name, dir: true}, nil
}

type syntheticFileInfo struct {
	name string
	dir  bool
}

func (i syntheticFileInfo) Name() string { return i.name }
func (i syntheticFileInfo) Size() int64  { return 0 }
func (i syntheticFileInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}
func (i syntheticFileInfo) ModTime() time.Time { return time.Time{} }
func (i syntheticFileInfo) IsDir() bool         { return i.dir }
func (i syntheticFileInfo) Sys() any            { return nil }
