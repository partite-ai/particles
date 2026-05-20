package build_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/partite-ai/particles/internal/build"
)

// wasmExamplePath points at the prebuilt fixture component
// (components/wasm-example). Tests skip when it's missing — that
// avoids a hard dep on the Rust toolchain for normal `go test`
// runs, while keeping the build-fixture path exercised when the
// component has been built at least once.
const wasmExamplePath = "/home/node/cargo-target/wasm-example/wasm32-wasip2/release/wasm_example_component.wasm"

// End-to-end: feed Build a Rust-built native wasm component via
// Options.Component, expect the artifact to carry the typed manifest
// extracted from the component's get-manifest export plus the
// component bytes themselves.
func TestBuild_Wasm_Component(t *testing.T) {
	wasmBytes, err := os.ReadFile(wasmExamplePath)
	if err != nil {
		t.Skipf("wasm-example fixture missing (%v) — run `cargo build --manifest-path components/wasm-example/Cargo.toml --target wasm32-wasip2 --release`", err)
	}

	src := fstest.MapFS{
		"component.wasm": &fstest.MapFile{Data: wasmBytes},
	}

	res, err := build.Build(context.Background(), build.Options{
		Source:    src,
		Component: "component.wasm",
	})
	if err != nil {
		t.Fatalf("build.Build: %v", err)
	}

	// Artifact carries particle.wasm + manifest.json + build-info.json.
	if _, err := fs.Stat(res.Particle, "particle.wasm"); err != nil {
		t.Errorf("particle.wasm missing: %v", err)
	}
	if _, err := fs.Stat(res.Particle, "manifest.json"); err != nil {
		t.Errorf("manifest.json missing: %v", err)
	}

	// Manifest content matches what the component declares.
	mfBytes, err := fs.ReadFile(res.Particle, "manifest.json")
	if err != nil {
		t.Fatalf("read manifest.json: %v", err)
	}
	var mf struct {
		Name    string `json:"name"`
		Runtime string `json:"runtime"`
		Tools   []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(mfBytes, &mf); err != nil {
		t.Fatalf("parse manifest: %v\n%s", err, mfBytes)
	}
	if mf.Name != "wasm-example" {
		t.Errorf("name = %q, want wasm-example", mf.Name)
	}
	if mf.Runtime != "wasm" {
		t.Errorf("runtime = %q, want wasm", mf.Runtime)
	}
	toolNames := make([]string, 0, len(mf.Tools))
	for _, t := range mf.Tools {
		toolNames = append(toolNames, t.Name)
	}
	if !strings.Contains(strings.Join(toolNames, ","), "echo") ||
		!strings.Contains(strings.Join(toolNames, ","), "add") {
		t.Errorf("tools = %v, want both echo and add", toolNames)
	}

	// particle.wasm round-trips byte-for-byte (we just repackage).
	gotWasm, err := fs.ReadFile(res.Particle, "particle.wasm")
	if err != nil {
		t.Fatalf("read particle.wasm: %v", err)
	}
	if len(gotWasm) != len(wasmBytes) {
		t.Errorf("particle.wasm len = %d, want %d", len(gotWasm), len(wasmBytes))
	}

	// build-info has runtime=wasm + the component size.
	biBytes, err := fs.ReadFile(res.Particle, "build-info.json")
	if err != nil {
		t.Fatalf("read build-info: %v", err)
	}
	if !strings.Contains(string(biBytes), `"runtime": "wasm"`) {
		t.Errorf("build-info missing runtime=wasm:\n%s", biBytes)
	}
}
