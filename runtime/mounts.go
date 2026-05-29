package runtime

import (
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/partite-ai/wacogo/wasi/filesystem/preopens"
)

// builtinGuestPaths returns the absolute guest paths the runtime serves
// itself through the catch-all "/" preopen. A user mount may not
// overlap any of them. The particle artifact is always present at
// /particle; Python adds the CPython stdlib and the runtime bootstrap.
func builtinGuestPaths(rt RuntimeKind) []string {
	paths := []string{"/particle"}
	if rt == RuntimePython {
		paths = append(paths, "/"+pythonStdlibMountPath, "/runtime")
	}
	return paths
}

// pathsOverlap reports whether two absolute paths name the same node or
// one is an ancestor of the other — i.e. preopening one would shadow
// part of the other's subtree.
func pathsOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// buildMountPreopens validates the host-supplied mounts against the
// manifest's filesystem declarations and returns one preopen entry per
// mount to expose. The map is keyed by mount name (the key under
// `capabilities.filesystem.mounts` / `.temp`); the returned entries are
// keyed by the declared guest path.
//
// Rules:
//   - A supplied name the manifest didn't declare is rejected — a host
//     can't expose filesystem access the user never reviewed.
//   - A required regular mount that's absent is an error; an optional
//     one is simply skipped (no preopen, so the guest sees ENOENT).
//   - Every declared temp mount must be supplied (the host always
//     provisions temp scratch).
//   - A read-only mount is wrapped in readOnlyFS (refusing writes
//     regardless of what the host handed in); a read-write mount and
//     every temp mount are exposed writable.
//   - That is then wrapped in a trackingFS (outermost) so handles the
//     guest leaves open are reclaimed at teardown; the returned closers
//     are those wrappers, for [Particle.Close] to close. trackingFS is
//     outermost so readOnlyFS sits directly over the real mount and its
//     As-based capability delegation reaches the actual fs.File.
func buildMountPreopens(m Manifest, mounts map[string]fs.FS) ([]*preopens.PreopenEntry, []io.Closer, error) {
	fsCap := m.Capabilities.Filesystem

	for name := range mounts {
		_, isMount := fsCap.Mounts[name]
		_, isTemp := fsCap.Temp[name]
		if !isMount && !isTemp {
			return nil, nil, fmt.Errorf("mount %q is not declared in capabilities.filesystem", name)
		}
	}

	var (
		entries []*preopens.PreopenEntry
		closers []io.Closer
	)

	for name, decl := range fsCap.Mounts {
		sub, ok := mounts[name]
		if !ok {
			if decl.Required {
				return nil, nil, fmt.Errorf("mount %q is required but was not provided", name)
			}
			continue
		}
		if sub == nil {
			return nil, nil, fmt.Errorf("mount %q: provided fs.FS is nil", name)
		}
		var inner fs.FS = sub
		if decl.Access == MountReadOnly {
			inner = readOnlyFS{fsys: sub}
		}
		tracked := newTrackingFS(inner)
		closers = append(closers, tracked)
		entries = append(entries, &preopens.PreopenEntry{Path: decl.Path, Root: ".", FS: tracked})
	}

	for name, decl := range fsCap.Temp {
		sub, ok := mounts[name]
		if !ok {
			return nil, nil, fmt.Errorf("temp mount %q was not provided", name)
		}
		if sub == nil {
			return nil, nil, fmt.Errorf("temp mount %q: provided fs.FS is nil", name)
		}
		tracked := newTrackingFS(sub)
		closers = append(closers, tracked)
		entries = append(entries, &preopens.PreopenEntry{Path: decl.Path, Root: ".", FS: tracked})
	}

	return entries, closers, nil
}
