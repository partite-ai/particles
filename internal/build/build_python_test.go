package build_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/partite-ai/particles/internal/build"
)

// End-to-end Python build: a Particlefile.py with no third-party
// deps. Skips the pip-resolve phase entirely; the resulting artifact
// is just manifest.json + bundle.py + build-info.json.
//
// Confirms the dispatch picks the Python pipeline, the introspect
// phase produces a `runtime: python` manifest, and bundle.py
// round-trips byte-for-byte (no JS-style minification on the Python
// path).
func TestBuild_Python_NoDeps(t *testing.T) {
	source := `# A minimal Particlefile.py — no deps, no PEP 723 block.
from particle.manifest import Particle, Tool

def _echo(args):
    return {"result": args["input"]}

particle = Particle(
    name="py-no-deps",
    description="minimal Python particle",
    version="0.1.0",
    tools={
        "echo": Tool(
            description="echo back the input",
            input_schema={"type": "object", "properties": {"input": {"type": "string"}}, "required": ["input"]},
            handler=_echo,
        ),
    },
)
`
	src := fstest.MapFS{
		"Particlefile.py": &fstest.MapFile{Data: []byte(source)},
	}

	res, err := build.Build(context.Background(), build.Options{Source: src, NoTypeCheck: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Artifact has bundle.py (not bundle.js).
	if _, err := fs.Stat(res.Particle, "bundle.py"); err != nil {
		t.Errorf("bundle.py missing: %v", err)
	}
	if _, err := fs.Stat(res.Particle, "bundle.js"); err == nil {
		t.Error("bundle.js present on a Python build — should be Python-only")
	}

	// Particlefile.py round-trips byte-for-byte (no minification).
	gotPy, err := fs.ReadFile(res.Particle, "bundle.py")
	if err != nil {
		t.Fatalf("read bundle.py: %v", err)
	}
	if string(gotPy) != source {
		t.Errorf("bundle.py changed from source:\ngot:\n%s\nwant:\n%s", gotPy, source)
	}

	// Manifest has runtime=python.
	mfBytes, err := fs.ReadFile(res.Particle, "manifest.json")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var mf struct {
		Name    string `json:"name"`
		Runtime string `json:"runtime"`
	}
	if err := json.Unmarshal(mfBytes, &mf); err != nil {
		t.Fatalf("parse manifest: %v\n%s", err, mfBytes)
	}
	if mf.Name != "py-no-deps" {
		t.Errorf("manifest.name = %q", mf.Name)
	}
	if mf.Runtime != "python" {
		t.Errorf("manifest.runtime = %q, want python", mf.Runtime)
	}

	// No deps → no Particle.lock and no _deps tree.
	if _, err := fs.Stat(res.Particle, "Particle.lock"); err == nil {
		t.Error("Particle.lock present despite zero deps")
	}
}

// Python build with PEP 723 deps. Hits live PyPI for the resolve;
// skipped under -short. Confirms the full chain end-to-end:
// PEP 723 → pip-resolve.wasm → wheel unpack → artifact layout.
func TestBuild_Python_WithDeps(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short (live PyPI access)")
	}

	source := `# /// script
# requires-python = ">=3.12"
# dependencies = [
#   "idna>=3",
# ]
# ///

import idna  # noqa: F401  (resolved at runtime via _deps/site-packages)
from particle.manifest import Particle, Tool

def _check(args):
    return {"ok": True}

particle = Particle(
    name="py-with-deps",
    description="Python particle with one pure-Python dep",
    version="0.1.0",
    tools={
        "check": Tool(
            description="smoke check",
            input_schema={"type": "object"},
            handler=_check,
        ),
    },
)
`
	src := fstest.MapFS{
		"Particlefile.py": &fstest.MapFile{Data: []byte(source)},
	}

	res, err := build.Build(context.Background(), build.Options{Source: src, NoTypeCheck: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// idna got unpacked under _deps/site-packages.
	idnaInit := "_deps/site-packages/idna/__init__.py"
	if _, err := fs.Stat(res.Particle, idnaInit); err != nil {
		t.Errorf("expected %s in artifact: %v", idnaInit, err)
	}

	// Particle.lock has the resolved wheel.
	lockBytes, err := fs.ReadFile(res.Particle, "Particle.lock")
	if err != nil {
		t.Fatalf("read Particle.lock: %v", err)
	}
	if !strings.Contains(string(lockBytes), `"name": "idna"`) {
		t.Errorf("Particle.lock missing idna entry:\n%s", lockBytes)
	}
	if !strings.Contains(string(lockBytes), `"sha256": "sha256:`) {
		t.Errorf("Particle.lock missing sha256 prefix:\n%s", lockBytes)
	}

	// build-info.json records the declared PEP 723 dep AND the
	// resolved wheel.
	biBytes, err := fs.ReadFile(res.Particle, "build-info.json")
	if err != nil {
		t.Fatalf("read build-info: %v", err)
	}
	// json.MarshalIndent escapes `>` to `>`, so check the
	// unambiguous prefix instead of the full PEP 508 string.
	if !strings.Contains(string(biBytes), `"idna`) {
		t.Errorf("build-info missing declared dep:\n%s", biBytes)
	}
	if !strings.Contains(string(biBytes), `"runtime": "python"`) {
		t.Errorf("build-info missing runtime field:\n%s", biBytes)
	}
}

// Python build with an invalid PEP 723 block — error surfaces at the
// import-scan phase, before any wasm is spun up.
func TestBuild_Python_InvalidPEP723(t *testing.T) {
	source := `# /// script
# dependencies = [
"http"
# ]
# ///

from particle.manifest import Particle
particle = Particle(name="x", description="", version="0.1.0")
`
	src := fstest.MapFS{"Particlefile.py": &fstest.MapFile{Data: []byte(source)}}
	_, err := build.Build(context.Background(), build.Options{Source: src, NoTypeCheck: true})
	if err == nil {
		t.Fatal("expected error for malformed PEP 723 block")
	}
}
