package pyscan

import (
	"strings"
	"testing"
	"testing/fstest"
)

// Happy path: a Particlefile with a well-formed script block.
// Pins all three observable fields — HasBlock, Dependencies in source
// order, RequiresPython — so a regression in any one surfaces here.
func TestScan_HappyPath(t *testing.T) {
	src := `# /// script
# requires-python = ">=3.12"
# dependencies = [
#   "httpx>=0.27",
#   "pyjwt>=2",
# ]
# ///

particle = {
    "name": "demo",
}
`
	fsys := fstest.MapFS{"Particlefile.py": {Data: []byte(src)}}
	r, err := Scan(fsys, "Particlefile.py")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !r.HasBlock {
		t.Error("HasBlock = false, want true")
	}
	if r.RequiresPython != ">=3.12" {
		t.Errorf("RequiresPython = %q, want >=3.12", r.RequiresPython)
	}
	got := strings.Join(r.Dependencies, ",")
	if got != "httpx>=0.27,pyjwt>=2" {
		t.Errorf("Dependencies = %q, want httpx>=0.27,pyjwt>=2", got)
	}
}

// No block: completely valid — particles with no third-party deps
// don't need to declare anything. HasBlock=false + no error is the
// signal the build pipeline reads as "skip the resolve phase".
func TestScan_NoBlock(t *testing.T) {
	src := `particle = {"name": "demo", "tools": {}}` + "\n"
	r, err := Scan(fstest.MapFS{"Particlefile.py": {Data: []byte(src)}}, "Particlefile.py")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if r.HasBlock {
		t.Error("HasBlock = true, want false (no script block in source)")
	}
	if len(r.Dependencies) != 0 {
		t.Errorf("Dependencies = %v, want empty", r.Dependencies)
	}
}

// Block exists but declares no deps: valid edge case (author wants
// to pin requires-python without external deps). HasBlock=true,
// Dependencies empty.
func TestScan_BlockNoDeps(t *testing.T) {
	src := `# /// script
# requires-python = ">=3.12"
# ///
`
	r, err := Scan(fstest.MapFS{"Particlefile.py": {Data: []byte(src)}}, "Particlefile.py")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !r.HasBlock {
		t.Error("HasBlock = false, want true")
	}
	if len(r.Dependencies) != 0 {
		t.Errorf("Dependencies = %v, want empty", r.Dependencies)
	}
	if r.RequiresPython != ">=3.12" {
		t.Errorf("RequiresPython = %q", r.RequiresPython)
	}
}

// Bare-`#` lines inside the block translate to blank TOML lines.
// PEP 723 explicitly allows them — many real-world scripts use them
// for visual grouping.
func TestScan_BareHashLinesAllowed(t *testing.T) {
	src := `# /// script
# dependencies = [
#   "httpx>=0.27",
#
#   "pyjwt>=2",
# ]
# ///
`
	r, err := Scan(fstest.MapFS{"Particlefile.py": {Data: []byte(src)}}, "Particlefile.py")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if strings.Join(r.Dependencies, ",") != "httpx>=0.27,pyjwt>=2" {
		t.Errorf("Dependencies = %v", r.Dependencies)
	}
}

// Multiple `script` blocks → error per PEP 723. The error message must
// point at both locations so the author can find the duplicates.
func TestScan_MultipleBlocks_Errors(t *testing.T) {
	src := `# /// script
# dependencies = ["a"]
# ///

# something in between

# /// script
# dependencies = ["b"]
# ///
`
	_, err := Scan(fstest.MapFS{"Particlefile.py": {Data: []byte(src)}}, "Particlefile.py")
	if err == nil {
		t.Fatal("expected error for two script blocks")
	}
	if !strings.Contains(err.Error(), "multiple") {
		t.Errorf("err = %v, want it to mention 'multiple'", err)
	}
}

// Unterminated block (EOF before any closing `# ///`) → error. The
// author may have deleted the closer by mistake; the error points at
// the open line so they can locate it. A blank (non-comment) line
// inside the block hits a different code path (non-comment-in-block),
// so this case keeps every line inside as a valid comment to
// isolate the EOF behavior.
func TestScan_UnterminatedBlock_Errors(t *testing.T) {
	src := `# /// script
# dependencies = ["httpx"]
# more comment lines
# but never closed
`
	_, err := Scan(fstest.MapFS{"Particlefile.py": {Data: []byte(src)}}, "Particlefile.py")
	if err == nil {
		t.Fatal("expected error for unterminated block")
	}
	if !strings.Contains(err.Error(), "unterminated") {
		t.Errorf("err = %v, want it to mention 'unterminated'", err)
	}
}

// Non-comment line inside the block (author forgot the `# ` on a
// continuation line) → error pointing at the offending line.
func TestScan_NonCommentInBlock_Errors(t *testing.T) {
	src := `# /// script
# dependencies = [
"httpx>=0.27",
# ]
# ///
`
	_, err := Scan(fstest.MapFS{"Particlefile.py": {Data: []byte(src)}}, "Particlefile.py")
	if err == nil {
		t.Fatal("expected error for non-comment line inside block")
	}
	if !strings.Contains(err.Error(), "non-comment") {
		t.Errorf("err = %v, want it to mention 'non-comment'", err)
	}
}

// Malformed TOML body (e.g. unterminated string) surfaces with the
// block start line so the author isn't told to look at line 1.
func TestScan_InvalidTOML_Errors(t *testing.T) {
	src := `# /// script
# dependencies = [ "httpx
# ///
`
	_, err := Scan(fstest.MapFS{"Particlefile.py": {Data: []byte(src)}}, "Particlefile.py")
	if err == nil {
		t.Fatal("expected error for invalid TOML")
	}
	if !strings.Contains(err.Error(), "TOML") && !strings.Contains(err.Error(), "toml") {
		t.Errorf("err = %v, want TOML mention", err)
	}
}

// CRLF line endings (Windows authoring) must parse identically. The
// scanner strips trailing \r so the "# ///" recognition still fires.
func TestScan_CRLFLineEndings(t *testing.T) {
	src := "# /// script\r\n# dependencies = [\"httpx\"]\r\n# ///\r\n"
	r, err := Scan(fstest.MapFS{"Particlefile.py": {Data: []byte(src)}}, "Particlefile.py")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !r.HasBlock {
		t.Error("HasBlock = false on CRLF source")
	}
	if len(r.Dependencies) != 1 || r.Dependencies[0] != "httpx" {
		t.Errorf("Dependencies = %v", r.Dependencies)
	}
}

// The block doesn't have to be at the top of the file. PEP 723
// matches the opening against any line — authors who prefer to keep
// the particle definition at the top can park the block lower.
func TestScan_BlockNotAtTop(t *testing.T) {
	src := `"""Module docstring."""

import json

# /// script
# dependencies = ["httpx"]
# ///

particle = {}
`
	r, err := Scan(fstest.MapFS{"Particlefile.py": {Data: []byte(src)}}, "Particlefile.py")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !r.HasBlock || len(r.Dependencies) != 1 {
		t.Errorf("expected one dep, got %+v", r)
	}
}

// I/O failure (missing file) surfaces as a wrapped error so callers
// can errors.Is(... fs.ErrNotExist).
func TestScan_MissingFile(t *testing.T) {
	_, err := Scan(fstest.MapFS{}, "Particlefile.py")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}
