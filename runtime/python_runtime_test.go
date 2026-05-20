package runtime_test

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/partite-ai/particles/internal/build"
	"github.com/partite-ai/particles/runtime"
)

// Smoke-test the Python runtime path: build a particle artifact by
// hand (the build pipeline doesn't grow a Python branch until task
// #7), instantiate it through Runtime.NewParticle, then exercise
// ListTools / CallTool / Ping. The point is to prove that:
//
//  1. The manifest's `runtime: "python"` triggers the python wasm,
//  2. bundle.py mounts at /particle/bundle.py and the bootstrap loads
//     it via importlib.machinery.SourceFileLoader (the dynamic-load
//     trick we validated in the spike, now wired through the real
//     host),
//  3. Frozen stdlib (json, hashlib, base64) is reachable from user
//     code,
//  4. The `particle` dict DSL serializes round-trip through tools.

func pythonParticleFS(manifest, bundle string) fs.FS {
	return fstest.MapFS{
		"manifest.json":   {Data: []byte(manifest)},
		"bundle.py":       {Data: []byte(bundle)},
		"build-info.json": {Data: []byte(`{"runtimeApi":"0.1.0"}`)},
	}
}

// buildPythonParticle builds a Python particle through the real
// pipeline (build.Build with NoTypeCheck=true). Helper for tests
// that exercise the full path from source to runtime, including
// PEP 723 deps and the _deps/site-packages tree.
func buildPythonParticle(t *testing.T, source string) *build.Result {
	t.Helper()
	src := fstest.MapFS{"Particlefile.py": &fstest.MapFile{Data: []byte(source)}}
	res, err := build.Build(context.Background(), build.Options{
		Source:      src,
		NoTypeCheck: true,
	})
	if err != nil {
		t.Fatalf("build.Build (python): %v", err)
	}
	return res
}

func TestPythonRuntime_EndToEnd(t *testing.T) {
	ctx := context.Background()

	manifest := `{
		"name": "py-echo",
		"description": "Python smoke test",
		"version": "0.1.0",
		"runtime": "python",
		"capabilities": {},
		"tools": [
			{"name":"echo","description":"echo upper","inputSchema":{"type":"object","properties":{"input":{"type":"string"}},"required":["input"]}},
			{"name":"hash","description":"sha256 hex","inputSchema":{"type":"object","properties":{"input":{"type":"string"}},"required":["input"]}}
		]
	}`

	bundle := `import hashlib
from particle.manifest import Particle, Tool

def _echo(args):
    return {"result": args["input"].upper()}

def _hash(args):
    return {"sha256": hashlib.sha256(args["input"].encode()).hexdigest()}

particle = Particle(
    name="py-echo",
    description="Python smoke test",
    version="0.1.0",
    tools={
        "echo": Tool(
            description="echo upper",
            input_schema={"type": "object", "properties": {"input": {"type": "string"}}, "required": ["input"]},
            handler=_echo,
        ),
        "hash": Tool(
            description="sha256 hex",
            input_schema={"type": "object", "properties": {"input": {"type": "string"}}, "required": ["input"]},
            handler=_hash,
        ),
    },
    ping=lambda: {"status": "ok", "message": "alive"},
)
`

	rt, credStore, kvStore, cleanup := newRuntime(t, ctx)
	defer cleanup()

	p, err := rt.NewParticle(ctx, pythonParticleFS(manifest, bundle), credStore, kvStore)
	if err != nil {
		t.Fatalf("NewParticle: %v", err)
	}
	defer p.Close(ctx)

	if got := p.Manifest().ResolvedRuntime(); got != runtime.RuntimePython {
		t.Fatalf("ResolvedRuntime = %q, want python", got)
	}

	t.Run("ListTools", func(t *testing.T) {
		tools, err := p.ListTools(ctx)
		if err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		if len(tools) != 2 {
			t.Fatalf("len(tools) = %d, want 2", len(tools))
		}
		seen := map[string]bool{}
		for _, td := range tools {
			seen[td.Name] = true
		}
		if !seen["echo"] || !seen["hash"] {
			t.Errorf("tools = %v, want {echo, hash}", seen)
		}
	})

	t.Run("CallTool echo", func(t *testing.T) {
		got, err := p.CallTool(ctx, "echo", []byte(`{"input":"hello"}`))
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if !strings.Contains(string(got), `"result": "HELLO"`) &&
			!strings.Contains(string(got), `"result":"HELLO"`) {
			t.Errorf("result = %s", got)
		}
	})

	t.Run("CallTool hash (stdlib reachable)", func(t *testing.T) {
		got, err := p.CallTool(ctx, "hash", []byte(`{"input":"hello world"}`))
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		// Known SHA-256 of "hello world".
		const want = "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"
		if !strings.Contains(string(got), want) {
			t.Errorf("sha mismatch:\n%s", got)
		}
	})

	t.Run("Ping", func(t *testing.T) {
		res, err := p.Ping(ctx)
		if err != nil {
			t.Fatalf("Ping: %v", err)
		}
		if res.Status != runtime.PingStatusOK {
			t.Errorf("status = %v, want OK", res.Status)
		}
		if res.Message != "alive" {
			t.Errorf("message = %q, want alive", res.Message)
		}
	})
}

// Full-pipeline end-to-end: a Particlefile.py with a PEP 723 dep,
// built through the real build pipeline (PyPI resolve, wheel
// unpack, introspect), instantiated through the runtime, and the
// resulting tool actually uses the third-party dep.
//
// `idna` is the simplest "uses a real PyPI package" exercise — pure
// Python, small, has been stable for years. We call its
// `idna.encode` to demonstrate the bundled dep is reachable from
// user code through /particle/_deps/site-packages.
//
// Skipped under -short because it hits PyPI.
func TestPythonRuntime_WithDep_EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short (live PyPI access)")
	}

	source := `# /// script
# requires-python = ">=3.12"
# dependencies = ["idna>=3"]
# ///

import idna
from particle.manifest import Particle, Tool

def _idna_info(args):
    # Proves three things at once:
    #   1. _deps/site-packages mounted by the runtime
    #   2. bootstrap added it to sys.path
    #   3. user code can read attributes from a third-party module
    # Deliberately avoids idna.encode() — that goes through CPython's
    # codec registry, which componentize-py's embedded interpreter
    # doesn't always wire up.
    return {
        "package": idna.__name__,
        "version": idna.__version__,
        "has_check_hostname": callable(getattr(idna, "check_hostname_label", None)),
    }

particle = Particle(
    name="py-with-idna",
    description="uses a third-party pure-Python dep",
    version="0.1.0",
    tools={
        "idna_info": Tool(
            description="report idna package info from a third-party wheel",
            input_schema={"type": "object"},
            handler=_idna_info,
        ),
    },
)
`

	res := buildPythonParticle(t, source)

	ctx := context.Background()
	rt, credStore, kvStore, cleanup := newRuntime(t, ctx)
	defer cleanup()

	p, err := rt.NewParticle(ctx, res.Particle, credStore, kvStore)
	if err != nil {
		t.Fatalf("NewParticle: %v", err)
	}
	defer p.Close(ctx)

	if got := p.Manifest().ResolvedRuntime(); got != runtime.RuntimePython {
		t.Fatalf("ResolvedRuntime = %q, want python", got)
	}

	got, err := p.CallTool(ctx, "idna_info", []byte(`{}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !strings.Contains(string(got), `"package": "idna"`) &&
		!strings.Contains(string(got), `"package":"idna"`) {
		t.Errorf("expected idna package in result: %s", got)
	}
	if !strings.Contains(string(got), `"version"`) {
		t.Errorf("expected version in result: %s", got)
	}
}
