// Package skills embeds the particle-authoring guides so the
// `particle builder-mcp` server can serve them to LLM clients. The
// SKILL.md files in this directory's subdirectories double as
// human-readable documentation and as Claude Code "skills" — they
// stay on disk and in sync with the embedded copies.
package skills

import (
	"embed"
	"fmt"
)

//go:embed write-particle-typescript/SKILL.md write-particle-python/SKILL.md
var files embed.FS

// Languages enumerates the supported language identifiers, sorted.
// The slice is freshly allocated, so callers may mutate it.
func Languages() []string {
	return []string{"python", "typescript"}
}

// Get returns the skill markdown for `language`. Returns an error
// listing the supported set when `language` is unrecognized — that
// shape lets MCP tool callers surface the valid options to an LLM
// in one round-trip.
func Get(language string) (string, error) {
	var path string
	switch language {
	case "typescript":
		path = "write-particle-typescript/SKILL.md"
	case "python":
		path = "write-particle-python/SKILL.md"
	default:
		return "", fmt.Errorf("unknown language %q (supported: %v)", language, Languages())
	}
	data, err := files.ReadFile(path)
	if err != nil {
		// embed.FS.ReadFile only fails when the pattern didn't
		// match at build time — a static bug, not a runtime
		// condition. Surface it explicitly rather than swallowing.
		return "", fmt.Errorf("read embedded skill %s: %w", path, err)
	}
	return string(data), nil
}
