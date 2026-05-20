package wacogo_test

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/partite-ai/particles/internal/build/wacogo"
	"github.com/partite-ai/particles/runtime"
)

// Trap-mode smoke test: a synthesized JS bundle that calls a
// particle:credentials API at module scope must surface the
// explicit ErrIntrospectMode-derived error when ExtractManifest
// instantiates the runtime. We're not building the bundle through
// esbuild here — we hand-craft a tiny JS string that imports the
// runtime-side host-shim API and calls it during top-level
// evaluation.
//
// The point isn't to exercise the build pipeline (other tests do
// that); the point is to lock in the contract that ExtractManifest
// goes through runtime.IntrospectParticle (which owns the trap
// stores + trap HTTPDoer wiring) so module-scope host
// calls fail in a recognizable way rather than silently working
// against an empty store.
func TestExtractManifest_TrapStores_BlockModuleScopeHostCalls(t *testing.T) {
	// Bundle that does a module-scope credentials.fetcher call.
	// The trap store should fire on getPlaceholder; the JS runtime
	// surfaces that as an exception during bundle evaluation;
	// loadParticle() catches it and GetManifest reports
	// bundle-load-error.
	//
	// The handler body is unreachable because module evaluation
	// fails first.
	bundleJS := []byte(`
import { credentials } from "@partite-ai/particle-credentials";
// Module-scope host call — illegal during get-manifest. The trap
// store must surface a recognizable error.
const _trap = await credentials.fetcher("x");
export default {
  name: "trap-test",
  description: "module-scope credential call should trip the introspect trap",
  version: "0.0.1",
  capabilities: { http: { allowedHosts: ["api.example.com"] } },
  tools: {},
};
`)

	ctx := context.Background()
	c, err := wacogo.New(ctx)
	if err != nil {
		t.Fatalf("wacogo.New: %v", err)
	}
	defer c.Close(ctx)

	src := fstest.MapFS{"bundle.js": &fstest.MapFile{Data: bundleJS}}
	_, err = c.ExtractManifest(ctx, runtime.RuntimeJS, src)
	if err == nil {
		t.Fatal("expected ExtractManifest to fail with introspect-mode trap")
	}
	// The error chain: wacogo wrap → runtime.ManifestError →
	// JS-runtime stringification of the trap. The substring we
	// expect is in the JS exception message, which originally
	// came from credentials.ErrIntrospectMode.Error(). The
	// runtime returned bundle-load-error since the JS module
	// failed at top-level evaluation.
	want := "not allowed during get-manifest"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v\n  want substring %q so we know the trap fired with its explicit message", err, want)
	}
	if !strings.Contains(err.Error(), "bundle-load-error") {
		t.Errorf("err = %v\n  want bundle-load-error variant (module evaluation failed)", err)
	}
}

// Same trap-mode check but for Python. A Python bundle that calls
// `particle.credentials.get_placeholder("x")` at module scope must
// surface the explicit introspect error, not "[object]"-style
// stringification.
func TestExtractManifest_TrapStores_Python_BlockModuleScopeHostCalls(t *testing.T) {
	bundlePy := []byte(`
from particle import credentials
# Module-scope host call — illegal during get-manifest.
_trap = credentials.get_placeholder("x")
particle = {
    "name": "trap-test-py",
    "description": "module-scope credential call should trip the introspect trap",
    "version": "0.0.1",
    "capabilities": {"http": {"allowedHosts": ["api.example.com"]}},
    "tools": {},
}
`)

	ctx := context.Background()
	c, err := wacogo.New(ctx)
	if err != nil {
		t.Fatalf("wacogo.New: %v", err)
	}
	defer c.Close(ctx)

	src := fstest.MapFS{"bundle.py": &fstest.MapFile{Data: bundlePy}}
	_, err = c.ExtractManifest(ctx, runtime.RuntimePython, src)
	if err == nil {
		t.Fatal("expected ExtractManifest to fail with introspect-mode trap")
	}
	want := "not allowed during get-manifest"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v\n  want substring %q so we know the trap fired with its explicit message", err, want)
	}
	if !strings.Contains(err.Error(), "bundle-load-error") {
		t.Errorf("err = %v\n  want bundle-load-error variant (module evaluation failed)", err)
	}
}

// Belt-and-suspenders: confirm a well-behaved particle (no
// module-scope host calls) still succeeds — i.e. the trap doesn't
// break normal builds.
func TestExtractManifest_WellBehavedParticle_Succeeds(t *testing.T) {
	bundleJS := []byte(`export default {
  name: "no-host-calls",
  description: "",
  version: "0.1.0",
  capabilities: { http: { allowedHosts: [] } },
  tools: {
    noop: {
      description: "noop",
      inputSchema: { type: "object" },
      handler: async () => ({ ok: true }),
    },
  },
};`)

	ctx := context.Background()
	c, err := wacogo.New(ctx)
	if err != nil {
		t.Fatalf("wacogo.New: %v", err)
	}
	defer c.Close(ctx)

	src := fstest.MapFS{"bundle.js": &fstest.MapFile{Data: bundleJS}}
	mf, err := c.ExtractManifest(ctx, runtime.RuntimeJS, src)
	if err != nil {
		t.Fatalf("ExtractManifest: %v", err)
	}
	if mf.Name != "no-host-calls" {
		t.Errorf("name = %q", mf.Name)
	}
}

var _ = errors.New // keep errors import for go vet on stripped-down builds
var _ fs.FS        = fstest.MapFS{}
