package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/partite-ai/particles/internal/osmount"
	"github.com/partite-ai/particles/mounts"
	"github.com/partite-ai/particles/quotafs"
	"github.com/partite-ai/particles/runtime"
)

// resolveMounts turns a particle's declared filesystem mounts into the
// name→fs.FS map runtime.NewParticle expects, plus a cleanup that
// releases os.Root handles and deletes temp dirs. cliOverrides
// (name→host path, from --mount) win over persistent mappings in the
// store.
//
// Regular mounts: a mapped host dir becomes an os.Root-backed writable
// FS (the runtime downgrades it to read-only when the manifest says
// so); a required mount with no mapping is an error; an optional one is
// skipped. Temp mounts: a fresh os.MkdirTemp dir wrapped in a quota
// cap, deleted on cleanup.
func resolveMounts(ctx context.Context, particleName string, particleFS fs.FS, store mounts.Store, cliOverrides map[string]string) (map[string]fs.FS, func(), error) {
	manifest, err := runtime.LoadManifest(particleFS)
	if err != nil {
		return nil, nil, fmt.Errorf("load manifest: %w", err)
	}
	fsCap := manifest.Capabilities.Filesystem

	// A --mount can only target a declared regular mount.
	for name := range cliOverrides {
		if _, ok := fsCap.Mounts[name]; ok {
			continue
		}
		if _, isTemp := fsCap.Temp[name]; isTemp {
			return nil, nil, fmt.Errorf("--mount %s: temp mounts are provisioned automatically and can't be mapped", name)
		}
		return nil, nil, fmt.Errorf("--mount %s: %q is not a declared mount", name, name)
	}

	result := make(map[string]fs.FS)
	var cleanups []func()
	cleanup := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	fail := func(err error) (map[string]fs.FS, func(), error) {
		cleanup()
		return nil, nil, err
	}

	for name, decl := range fsCap.Mounts {
		hostPath, ok := cliOverrides[name]
		if !ok {
			stored, found, gerr := store.Get(ctx, name)
			if gerr != nil {
				return fail(fmt.Errorf("mount %q: lookup mapping: %w", name, gerr))
			}
			hostPath, ok = stored, found
		}
		if !ok || hostPath == "" {
			if decl.Required {
				return fail(fmt.Errorf("mount %q is required but not configured; run `particle mount %s %s <host-path>` or pass --mount %s=<host-path>", name, particleName, name, name))
			}
			continue
		}
		osfs, oerr := osmount.New(hostPath)
		if oerr != nil {
			return fail(fmt.Errorf("mount %q at %s: %w", name, hostPath, oerr))
		}
		cleanups = append(cleanups, func() { _ = osfs.Close() })
		result[name] = osfs
	}

	for name, decl := range fsCap.Temp {
		maxBytes, merr := decl.MaxSizeBytes()
		if merr != nil {
			return fail(fmt.Errorf("temp mount %q: %w", name, merr))
		}
		dir, derr := os.MkdirTemp("", "particle-"+particleName+"-"+name+"-*")
		if derr != nil {
			return fail(fmt.Errorf("temp mount %q: create dir: %w", name, derr))
		}
		cleanups = append(cleanups, func() { _ = os.RemoveAll(dir) })
		osfs, oerr := osmount.New(dir)
		if oerr != nil {
			return fail(fmt.Errorf("temp mount %q: %w", name, oerr))
		}
		cleanups = append(cleanups, func() { _ = osfs.Close() })
		qfs, qerr := quotafs.New(osfs, maxBytes)
		if qerr != nil {
			return fail(fmt.Errorf("temp mount %q: quota: %w", name, qerr))
		}
		result[name] = qfs
	}

	return result, cleanup, nil
}

// parseMountFlags parses repeated "name=host/path" --mount values into
// a name→path map, rejecting duplicates and malformed entries.
func parseMountFlags(flags []string) (map[string]string, error) {
	if len(flags) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(flags))
	for _, f := range flags {
		i := strings.IndexByte(f, '=')
		if i <= 0 {
			return nil, fmt.Errorf("invalid --mount %q: want name=path", f)
		}
		name, hostPath := f[:i], f[i+1:]
		if hostPath == "" {
			return nil, fmt.Errorf("invalid --mount %q: empty path", f)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("--mount %s specified more than once", name)
		}
		out[name] = hostPath
	}
	return out, nil
}
