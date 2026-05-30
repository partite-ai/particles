// Package examples embeds the Particlefile sources under each
// example directory so the `particle builder-mcp` server can serve
// them to LLM clients as reference code. Only Particlefile.{ts,py}
// entries are embedded — auxiliary files in an example dir
// (shell wrappers, fixtures) stay on disk.
package examples

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
)

//go:embed */Particlefile.ts */Particlefile.py
var files embed.FS

// Names returns the available example directory names, sorted.
// Computed from the embedded FS at first call — adding or removing
// an example directory and rebuilding flows through with no other
// code changes.
func Names() []string {
	entries, err := files.ReadDir(".")
	if err != nil {
		// embed.ReadDir on `.` cannot fail for a non-empty embed.
		// If we get here, the embed pattern matched nothing —
		// that's a build-time bug worth screaming about.
		panic(fmt.Sprintf("examples: embed.ReadDir: %v", err))
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// Get returns the (filename, content) of the named example's
// Particlefile. `name` is the directory name under examples/; the
// returned filename is "Particlefile.ts" or "Particlefile.py" —
// whichever the directory contains. .ts wins when both exist (no
// example currently does).
func Get(name string) (filename, content string, err error) {
	for _, ext := range []string{"ts", "py"} {
		fname := "Particlefile." + ext
		data, rErr := files.ReadFile(path.Join(name, fname))
		if rErr == nil {
			return fname, string(data), nil
		}
		if !errors.Is(rErr, fs.ErrNotExist) {
			return "", "", rErr
		}
	}
	return "", "", fmt.Errorf("unknown example %q (available: %v)", name, Names())
}
