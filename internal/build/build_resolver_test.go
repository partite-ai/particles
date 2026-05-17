package build_test

import (
	"context"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/partite-ai/particles/internal/build"
)

// The resolver phase makes real npm registry calls. Skip in -short
// mode and when PARTICLE_OFFLINE_TESTS is set.
func skipIfOffline(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping resolver-backed test in -short mode (talks to registry.npmjs.org)")
	}
	if os.Getenv("PARTICLE_OFFLINE_TESTS") != "" {
		t.Skip("skipping resolver-backed test (PARTICLE_OFFLINE_TESTS set)")
	}
}

func TestBuild_WithNpmDep(t *testing.T) {
	skipIfOffline(t)

	// Type-check enabled to confirm `npm:` import specifiers are
	// recognized by the typecheck phase as well as the bundler.
	src := fstest.MapFS{
		"Particlefile.ts": &fstest.MapFile{
			Data: []byte(`import isBuffer from "npm:is-buffer@^2.0.0";

export default {
  name: "buffer-tools",
  description: "Tiny buffer-detection tool.",
  version: "0.1.0",
  capabilities: {},
  tools: {
    is_buffer: {
      description: "Check whether the input is a Node Buffer",
      inputSchema: { type: "object", properties: { value: {} } },
      handler: async ({ value }: { value: unknown }) => ({ result: isBuffer(value) }),
    },
  },
};
`),
		},
	}

	res, err := build.Build(context.Background(), build.Options{
		Source: src,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	bundle, err := fs.ReadFile(res.Particle, "bundle.js")
	if err != nil {
		t.Fatalf("read bundle.js: %v", err)
	}
	// `is-buffer`'s impl is short and stable — the bundled output
	// should mention Buffer detection somewhere. We're only sanity-
	// checking that the dep tarball was unpacked and bundled, not
	// auditing exact contents.
	if !strings.Contains(string(bundle), "Buffer") {
		t.Errorf("bundle does not mention 'Buffer'; first 400 bytes:\n%s", truncate(bundle, 400))
	}

	buildInfo, err := fs.ReadFile(res.Particle, "build-info.json")
	if err != nil {
		t.Fatalf("read build-info.json: %v", err)
	}
	if !strings.Contains(string(buildInfo), `"name": "is-buffer"`) {
		t.Errorf("build-info.json should record the resolved is-buffer entry: %s", buildInfo)
	}
}

// TestBuild_NpmDepWithoutTypes — `is-odd` ships no .d.ts files. With
// strict mode on, TypeScript would normally raise TS7016 ("could not
// find a declaration file"). The typecheck phase suppresses that
// specific diagnostic for `npm:` specifiers so untyped deps just
// surface as `any`, matching how Deno / modern bundlers behave.
func TestBuild_NpmDepWithoutTypes(t *testing.T) {
	skipIfOffline(t)

	src := fstest.MapFS{
		"Particlefile.ts": &fstest.MapFile{
			Data: []byte(`import isOdd from "npm:is-odd@3.0.1";

export default {
  name: "odd-tools",
  description: "Check parity",
  version: "0.1.0",
  capabilities: {},
  tools: {
    is_odd: {
      description: "Check whether the input is odd",
      inputSchema: { type: "object", properties: { value: { type: "number" } } },
      handler: async ({ value }: { value: number }) => ({ result: isOdd(value) }),
    },
  },
};
`),
		},
	}
	_, err := build.Build(context.Background(), build.Options{Source: src})
	if err != nil {
		t.Fatalf("Build with untyped npm dep: %v", err)
	}
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
