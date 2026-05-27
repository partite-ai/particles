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
	if sub, subPath, found := m.routeToMount(name); found {
		return sub.Open(subPath)
	}
	return m.synthesizeDir(name, "open")
}

// Stat implements fs.StatFS, delegating to the matching mount's
// own `fs.Stat` so optimized backends (e.g. registry/sqlite's
// dbFS — `SELECT length(data)`, no blob fetch) can short-circuit
// the default Open+Stat fallback in fs.Stat. Without this method
// here, fs.Stat against mountedFS would Open() the file, which on
// dbFS triggers a full blob load even when the caller only wants
// the size.
func (m *mountedFS) Stat(name string) (fs.FileInfo, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrInvalid}
	}
	name = path.Clean(name)
	if sub, subPath, found := m.routeToMount(name); found {
		return fs.Stat(sub, subPath)
	}
	// Synthetic dirs above a mount — return a synthesized FileInfo
	// rather than going through Open just to call .Stat() on it.
	if m.hasMountUnder(name) {
		return syntheticFileInfo{name: path.Base(name), dir: true}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
}

// ReadFile implements fs.ReadFileFS. Same delegation reason as
// Stat — keep optimized inner-FS reads (e.g. dbFS's single SELECT
// data query) from being defeated by the default Open+ReadAll
// fallback. fs.ReadFile against the synthetic-dir region returns
// IsDir error.
func (m *mountedFS) ReadFile(name string) ([]byte, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	name = path.Clean(name)
	if sub, subPath, found := m.routeToMount(name); found {
		return fs.ReadFile(sub, subPath)
	}
	if m.hasMountUnder(name) {
		// fs.ReadFile of a directory has no canonical error in
		// io/fs (the doc says "open or read"); ErrInvalid matches
		// what os.ReadFile returns and what wasi maps to.
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// ReadDir implements fs.ReadDirFS. Delegates to the inner FS for
// in-mount paths; for synthetic regions we list the immediate mount
// children directly without going through Open+ReadDirFile.
func (m *mountedFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	name = path.Clean(name)
	if sub, subPath, found := m.routeToMount(name); found {
		return fs.ReadDir(sub, subPath)
	}
	children := m.directChildren(name)
	if len(children) == 0 {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	entries := make([]fs.DirEntry, 0, len(children))
	for _, c := range children {
		entries = append(entries, syntheticDirEntry{name: c})
	}
	return entries, nil
}

// routeToMount finds the longest mount prefix containing `name`
// and returns the matching sub-FS plus the in-mount path. The bool
// is false when `name` lies above any mount (synthetic region) or
// is otherwise unrouted.
func (m *mountedFS) routeToMount(name string) (fs.FS, string, bool) {
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
	if !found {
		return nil, "", false
	}
	sub := strings.TrimPrefix(strings.TrimPrefix(name, bestPrefix), "/")
	if sub == "" {
		sub = "."
	}
	return bestFS, sub, true
}

// hasMountUnder reports whether `name` is an ancestor of any
// configured mount — i.e. a synthetic dir region. Cheap walk over
// mounts; same loop shape as Open's synthesize branch.
func (m *mountedFS) hasMountUnder(name string) bool {
	for prefix := range m.mounts {
		if prefix == "" {
			continue
		}
		if name == "." || strings.HasPrefix(prefix, name+"/") {
			return true
		}
	}
	return false
}

// directChildren returns the unique first-segment names of mount
// prefixes that live directly under `name`. Used by ReadDir on the
// synthetic-dir region.
func (m *mountedFS) directChildren(name string) []string {
	seen := map[string]bool{}
	var out []string
	for prefix := range m.mounts {
		if prefix == "" {
			continue
		}
		var rest string
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
			out = append(out, first)
		}
	}
	return out
}

func (m *mountedFS) synthesizeDir(name, op string) (fs.File, error) {
	children := m.directChildren(name)
	if len(children) == 0 {
		return nil, &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
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
