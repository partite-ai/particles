package runtime_test

import (
	"context"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/partite-ai/particles/internal/build"
	"github.com/partite-ai/particles/runtime"
)

// Full source-to-tool-call exercise on a native-WASM particle:
//
//   1. Read the wasm-example fixture from cargo's release dir.
//   2. Pipe it through build.Build with --component → artifact FS.
//   3. Instantiate via runtime.NewParticle (which dispatches to the
//      RuntimeWasm path: load particle.wasm from the artifact, wire
//      the same particle:host/* bindings the JS/Python runtimes get).
//   4. Call list-tools, then a real call-tool.
//
// Skipped when the fixture isn't built.
func TestWasmParticle_EndToEnd(t *testing.T) {
	const wasmFixture = "/home/node/cargo-target/wasm-example/wasm32-wasip2/release/wasm_example_component.wasm"

	wasmBytes, err := os.ReadFile(wasmFixture)
	if err != nil {
		t.Skipf("wasm-example fixture missing (%v) — build it under components/wasm-example first", err)
	}

	src := fstest.MapFS{"component.wasm": &fstest.MapFile{Data: wasmBytes}}

	res, err := build.Build(context.Background(), build.Options{
		Source:    src,
		Component: "component.wasm",
	})
	if err != nil {
		t.Fatalf("build.Build: %v", err)
	}

	ctx := context.Background()
	rt, credStore, kvStore, cleanup := newRuntime(t, ctx)
	defer cleanup()

	p, err := rt.NewParticle(ctx, res.Particle, credStore, kvStore, nil)
	if err != nil {
		t.Fatalf("NewParticle: %v", err)
	}
	defer p.Close(ctx)

	if got := p.Manifest().ResolvedRuntime(); got != runtime.RuntimeWasm {
		t.Fatalf("ResolvedRuntime = %q, want wasm", got)
	}

	t.Run("ListTools", func(t *testing.T) {
		tools, err := p.ListTools(ctx)
		if err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		seen := map[string]bool{}
		for _, td := range tools {
			seen[td.Name] = true
		}
		if !seen["echo"] || !seen["add"] {
			t.Errorf("tools = %v, want {echo, add}", seen)
		}
	})

	t.Run("CallTool_echo", func(t *testing.T) {
		got, err := p.CallTool(ctx, "echo", []byte(`{"input":"hello"}`))
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if !strings.Contains(string(got), `"result":"hello"`) {
			t.Errorf("result = %s", got)
		}
	})

	t.Run("CallTool_add", func(t *testing.T) {
		got, err := p.CallTool(ctx, "add", []byte(`{"a":2,"b":3}`))
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if !strings.Contains(string(got), `"sum":5`) {
			t.Errorf("result = %s", got)
		}
	})

	t.Run("Ping", func(t *testing.T) {
		pr, err := p.Ping(ctx)
		if err != nil {
			t.Fatalf("Ping: %v", err)
		}
		if pr.Status != runtime.PingStatusOK {
			t.Errorf("status = %v, want OK", pr.Status)
		}
	})

	// Component bytes round-trip identically into the artifact.
	gotWasm, err := fs.ReadFile(res.Particle, "particle.wasm")
	if err != nil {
		t.Fatalf("read particle.wasm: %v", err)
	}
	if len(gotWasm) != len(wasmBytes) {
		t.Errorf("particle.wasm len = %d, source = %d", len(gotWasm), len(wasmBytes))
	}
}
