// Package pyscan extracts PEP 723 inline script metadata from a
// Particlefile.py source. It's the Python analog of internal/importscan
// — Phase 1 of the build pipeline for Python particles — but the shape
// is much smaller: Python has no `npm:`-style import-time dep
// declarations, so dependency information lives in a single TOML block
// at the top of the file (PEP 723) rather than spread across imports.
//
// Spec: https://peps.python.org/pep-0723/
//
// Format:
//
//	# /// script
//	# requires-python = ">=3.12"
//	# dependencies = [
//	#   "httpx>=0.27",
//	#   "pydantic>=2",
//	# ]
//	# ///
//
// The block can appear anywhere in the file. Comment lines inside the
// block are stripped of the leading "# " (or just "#" for blank lines)
// and the remaining text is parsed as TOML. PEP 723 mandates a single
// `script` block per file; multiple is an error.
package pyscan

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/BurntSushi/toml"
)

// Result is what Scan returns on success.
type Result struct {
	// HasBlock is true when a `# /// script` block was found.
	// Particles without external deps may omit the block entirely;
	// HasBlock=false + no error means "no deps declared".
	HasBlock bool

	// Dependencies is the raw PEP 508 strings from the block's
	// `dependencies` array, in source order. Empty when the block
	// is present but declares no deps.
	Dependencies []string

	// RequiresPython is the value of `requires-python`, or empty
	// when the block doesn't pin a Python version. Informational
	// for v1: componentize-py bakes 3.12 into the runtime image,
	// so we only validate that the declared range is compatible
	// when set.
	RequiresPython string
}

// Scan reads `entryPoint` from `fsys` and returns the PEP 723 metadata.
// A file without a block returns Result{HasBlock: false} and a nil
// error — pyscan doesn't require the block to exist, only that it be
// well-formed if present.
//
// Errors surface for:
//   - I/O failures reading the entry file
//   - malformed block (unterminated, non-comment line inside, invalid
//     TOML in the body)
//   - multiple `script` blocks (PEP 723: must error)
//   - `dependencies` present but not a list-of-strings
func Scan(fsys fs.FS, entryPoint string) (*Result, error) {
	if fsys == nil {
		return nil, errors.New("pyscan: fsys is nil")
	}
	if entryPoint == "" {
		return nil, errors.New("pyscan: entryPoint is empty")
	}
	data, err := fs.ReadFile(fsys, entryPoint)
	if err != nil {
		return nil, fmt.Errorf("pyscan: read %s: %w", entryPoint, err)
	}
	return scanBytes(data)
}

// scanBytes is the testable core — pure function over source bytes,
// no FS plumbing. Public Scan wraps it.
func scanBytes(src []byte) (*Result, error) {
	blocks, err := extractScriptBlocks(src)
	if err != nil {
		return nil, fmt.Errorf("pyscan: %w", err)
	}
	if len(blocks) == 0 {
		return &Result{HasBlock: false}, nil
	}
	if len(blocks) > 1 {
		// PEP 723: "Tools MUST raise an error if multiple
		// pyproject blocks of the same type are found." (Same
		// rule applies to the `script` block by extension.)
		return nil, fmt.Errorf("pyscan: multiple `# /// script` blocks (line %d and line %d)",
			blocks[0].startLine, blocks[1].startLine)
	}

	var doc struct {
		Dependencies   []string `toml:"dependencies"`
		RequiresPython string   `toml:"requires-python"`
	}
	if _, err := toml.Decode(blocks[0].body, &doc); err != nil {
		return nil, fmt.Errorf("pyscan: invalid TOML in script block (starting line %d): %w",
			blocks[0].startLine, err)
	}

	return &Result{
		HasBlock:       true,
		Dependencies:   doc.Dependencies,
		RequiresPython: doc.RequiresPython,
	}, nil
}

// scriptBlock holds one extracted block — the un-prefixed TOML body
// plus the 1-based line number the opener was on (for error messages).
type scriptBlock struct {
	body      string
	startLine int
}

// extractScriptBlocks walks the source line-by-line collecting every
// `# /// script` ... `# ///` block. Returns all of them so the caller
// can enforce "exactly one" itself — keeps this function strict to
// parsing.
func extractScriptBlocks(src []byte) ([]scriptBlock, error) {
	var (
		blocks      []scriptBlock
		inBlock     bool
		blockStart  int
		blockBody   strings.Builder
		lineNum     = 0
	)

	scanner := bufio.NewScanner(bytes.NewReader(src))
	// Particlefiles are small, but lines could be reasonably long
	// (a single dep specifier with markers can run ~200 chars).
	// 1MB is comfortably more than any sane source.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		// Trim trailing CR so CRLF files behave the same as LF.
		line = strings.TrimRight(line, "\r")

		if !inBlock {
			if line == "# /// script" {
				inBlock = true
				blockStart = lineNum
				blockBody.Reset()
			}
			continue
		}

		// Inside a block.
		if line == "# ///" {
			blocks = append(blocks, scriptBlock{
				body:      blockBody.String(),
				startLine: blockStart,
			})
			inBlock = false
			continue
		}

		// PEP 723: every line inside the block MUST begin with
		// "# " or be exactly "#". Anything else is malformed —
		// usually the author forgot the comment prefix on a
		// continuation line.
		switch {
		case line == "#":
			blockBody.WriteByte('\n')
		case strings.HasPrefix(line, "# "):
			blockBody.WriteString(line[2:])
			blockBody.WriteByte('\n')
		default:
			return nil, fmt.Errorf(
				"non-comment line inside script block (line %d): %q "+
					"— every line inside `# /// script` ... `# ///` "+
					"must start with `# ` or be exactly `#`",
				lineNum, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read source: %w", err)
	}
	if inBlock {
		return nil, fmt.Errorf(
			"unterminated script block (started at line %d, "+
				"missing closing `# ///`)", blockStart)
	}
	return blocks, nil
}
