package importer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// filesystemCap is the importer's view of `capabilities.filesystem` —
// enough to render the permission summary and drive mount setup. It
// mirrors the runtime's typed shape but is decoded from raw JSON here,
// matching how the importer handles the rest of the manifest.
type filesystemCap struct {
	Mounts map[string]mountDecl `json:"mounts"`
	Temp   map[string]tempDecl  `json:"temp"`
}

type mountDecl struct {
	Description string `json:"description"`
	Path        string `json:"path"`
	Access      string `json:"access"`
	Required    bool   `json:"required"`
}

type tempDecl struct {
	Description string `json:"description"`
	Path        string `json:"path"`
	MaxSize     string `json:"maxSize"`
}

// parseFilesystemCap decodes the filesystem block from a manifest's raw
// capabilities map. A missing block yields the zero value (no mounts).
func parseFilesystemCap(caps map[string]json.RawMessage) (filesystemCap, error) {
	raw, ok := caps["filesystem"]
	if !ok {
		return filesystemCap{}, nil
	}
	var fc filesystemCap
	if err := json.Unmarshal(raw, &fc); err != nil {
		return filesystemCap{}, fmt.Errorf("parse capabilities.filesystem: %w", err)
	}
	return fc, nil
}

// setupMounts offers to map each declared (non-temp) mount to a host
// directory and records accepted mappings in opts.Mounts. Temp mounts
// are skipped — the host provisions them fresh each run. A required
// mount left unmapped does NOT fail install; it's enforced when the
// particle runs. No-op when no mounts are declared, or when the store
// or prompter is absent.
func setupMounts(ctx context.Context, opts Options, fc filesystemCap) error {
	if len(fc.Mounts) == 0 || opts.Mounts == nil || opts.Prompter == nil {
		return nil
	}

	names := make([]string, 0, len(fc.Mounts))
	for n := range fc.Mounts {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		m := fc.Mounts[name]

		existing, found, err := opts.Mounts.Get(ctx, name)
		if err != nil {
			return fmt.Errorf("lookup mount mapping for %s: %w", name, err)
		}
		if found && existing != "" {
			opts.Prompter.Info(fmt.Sprintf("✓ %s — already mapped to %s", name, existing))
			continue
		}

		access := "read-write"
		if m.Access == "readonly" {
			access = "read-only"
		}
		req := "optional"
		if m.Required {
			req = "required"
		}
		opts.Prompter.Info(fmt.Sprintf("%s — %s", name, m.Description))
		ok, err := opts.Prompter.Confirm(
			fmt.Sprintf("Map a host directory for mount %q (%s, %s) now?", name, access, req),
			m.Required,
		)
		if err != nil {
			return err
		}
		if !ok {
			if m.Required {
				opts.Prompter.Info(fmt.Sprintf("%s is required at run time — map it later with `particle mount`.", name))
			}
			continue
		}

		dir, err := opts.Prompter.String(fmt.Sprintf("Host directory for %q:", name), "")
		if err != nil {
			return err
		}
		if dir == "" {
			continue
		}
		info, statErr := os.Stat(dir)
		if statErr != nil {
			opts.Prompter.Warn(fmt.Sprintf("%s: %v — skipped; map it later with `particle mount`.", dir, statErr))
			continue
		}
		if !info.IsDir() {
			opts.Prompter.Warn(fmt.Sprintf("%s is not a directory — skipped.", dir))
			continue
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", dir, err)
		}
		if err := opts.Mounts.Set(ctx, name, abs); err != nil {
			return fmt.Errorf("save mount mapping for %s: %w", name, err)
		}
		opts.Prompter.Info(fmt.Sprintf("→ %s mapped to %s", name, abs))
	}
	return nil
}
