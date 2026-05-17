package main

import (
	"errors"
	"fmt"
	"sort"

	"github.com/partite-ai/particles/runtime"
)

// filterTools applies the serve-mcp `--only-tools` allowlist or
// `--exclude-tools` denylist to the particle's full tool set.
//
// Both flags reference tool names, which are stable identifiers
// — typos shouldn't silently no-op. Names that don't match a
// real tool error out with the available set in the message so
// the user can fix the command in place.
//
// Empty include and exclude → return tools as-is. Both
// non-empty isn't a code path we expect (cobra
// MarkFlagsMutuallyExclusive blocks it at the CLI), but we
// reject it here too so the helper can be unit-tested
// independently and trusted by other callers.
func filterTools(tools []runtime.ToolDef, include, exclude []string) ([]runtime.ToolDef, error) {
	if len(include) > 0 && len(exclude) > 0 {
		return nil, errors.New("--only-tools and --exclude-tools are mutually exclusive")
	}
	if len(include) == 0 && len(exclude) == 0 {
		return tools, nil
	}

	known := make(map[string]bool, len(tools))
	for _, t := range tools {
		known[t.Name] = true
	}
	for _, n := range append(append([]string{}, include...), exclude...) {
		if !known[n] {
			return nil, fmt.Errorf("unknown tool %q (have: %v)", n, sortedKeys(known))
		}
	}

	var keep map[string]bool
	if len(include) > 0 {
		keep = setOf(include)
	}
	var drop map[string]bool
	if len(exclude) > 0 {
		drop = setOf(exclude)
	}

	out := make([]runtime.ToolDef, 0, len(tools))
	for _, t := range tools {
		if keep != nil && !keep[t.Name] {
			continue
		}
		if drop != nil && drop[t.Name] {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

func setOf(ss []string) map[string]bool {
	out := make(map[string]bool, len(ss))
	for _, s := range ss {
		out[s] = true
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
