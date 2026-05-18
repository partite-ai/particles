package runtime_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/partite-ai/wacogo"

	"github.com/partite-ai/particles/credentials"
	credmem "github.com/partite-ai/particles/credentials/memory"
	"github.com/partite-ai/particles/internal/build"
	"github.com/partite-ai/particles/kv"
	kvmem "github.com/partite-ai/particles/kv/memory"
	"github.com/partite-ai/particles/runtime"
)

// buildParticle compiles a tiny TypeScript source into a particle
// artifact via the real build pipeline. Returns the result; the
// runtime consumes res.Particle directly as an fs.FS.
func buildParticle(t *testing.T, source string) *build.Result {
	t.Helper()
	src := fstest.MapFS{"Particlefile.ts": &fstest.MapFile{Data: []byte(source)}}
	res, err := build.Build(context.Background(), build.Options{
		Source:      src,
		NoTypeCheck: true,
	})
	if err != nil {
		t.Fatalf("build.Build: %v", err)
	}
	return res
}

// newRuntime stands up a runtime backed by in-memory credential
// and kv stores. The returned stores are particle-scoped views
// callers pass straight into Runtime.NewParticle.
func newRuntime(t *testing.T, ctx context.Context) (*runtime.Runtime, credentials.Store, kv.Store, func()) {
	t.Helper()
	e := wacogo.NewEngine(ctx)
	credMgr, err := credentials.NewManager(ctx, credentials.ManagerConfig{Engine: e})
	if err != nil {
		t.Fatalf("credentials.NewManager: %v", err)
	}
	kvMgr, err := kv.NewManager(ctx, kv.ManagerConfig{Engine: e})
	if err != nil {
		t.Fatalf("kv.NewManager: %v", err)
	}
	rt, err := runtime.New(ctx, runtime.Config{
		Engine:      e,
		Credentials: credMgr,
		KV:          kvMgr,
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	cleanup := func() {
		_ = rt.Close(ctx)
		_ = credMgr.Close(ctx)
		_ = kvMgr.Close(ctx)
		_ = e.Close(ctx)
	}
	return rt, credmem.New().Scoped("test"), kvmem.New().Scoped("test"), cleanup
}

// -----------------------------------------------------------------------------
// Happy path: particle with no capabilities, simple echo tool.
// -----------------------------------------------------------------------------

func TestRuntime_EchoTool_EndToEnd(t *testing.T) {
	ctx := context.Background()

	src := `export default {
  name: "echo-tools",
  description: "Echo back the input.",
  version: "0.1.0",
  capabilities: {},
  tools: {
    echo: {
      description: "Echo the input upper-cased",
      inputSchema: { type: "object", properties: { input: { type: "string" } }, required: ["input"] },
      handler: async ({ input }: { input: string }) => ({ result: input.toUpperCase() }),
    },
    add: {
      description: "Add two numbers",
      inputSchema: { type: "object", properties: { a: { type: "number" }, b: { type: "number" } }, required: ["a", "b"] },
      handler: async ({ a, b }: { a: number; b: number }) => ({ sum: a + b }),
    },
  },
};`
	res := buildParticle(t, src)

	rt, credStore, kvStore, cleanup := newRuntime(t, ctx)
	defer cleanup()

	p, err := rt.NewParticle(ctx, res.Particle, credStore, kvStore)
	if err != nil {
		t.Fatalf("NewParticle: %v", err)
	}
	defer p.Close(ctx)

	t.Run("manifest", func(t *testing.T) {
		m := p.Manifest()
		if m.Name != "echo-tools" {
			t.Errorf("name = %q, want echo-tools", m.Name)
		}
		if m.Version != "0.1.0" {
			t.Errorf("version = %q", m.Version)
		}
	})

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
			if !strings.Contains(string(td.InputSchemaJSON), "object") {
				t.Errorf("tool %q schema missing 'object': %s", td.Name, td.InputSchemaJSON)
			}
		}
		if !seen["echo"] || !seen["add"] {
			t.Errorf("tools = %v, want {echo, add}", seen)
		}
	})

	t.Run("CallTool echo", func(t *testing.T) {
		got, err := p.CallTool(ctx, "echo", []byte(`{"input":"hello"}`))
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if !strings.Contains(string(got), `"result":"HELLO"`) {
			t.Errorf("result = %s", got)
		}
	})

	t.Run("CallTool add", func(t *testing.T) {
		got, err := p.CallTool(ctx, "add", []byte(`{"a":2,"b":3}`))
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if !strings.Contains(string(got), `"sum":5`) {
			t.Errorf("result = %s", got)
		}
	})
}

// -----------------------------------------------------------------------------
// Error variants
// -----------------------------------------------------------------------------

func TestRuntime_CallTool_NotFound(t *testing.T) {
	ctx := context.Background()
	src := `export default {
  name: "p", description: "p", version: "0.1.0", capabilities: {},
  tools: { echo: { description: "e", inputSchema: {type:"object"}, handler: async () => ({ok:true}) } },
};`
	res := buildParticle(t, src)
	rt, credStore, kvStore, cleanup := newRuntime(t, ctx)
	defer cleanup()
	p, err := rt.NewParticle(ctx, res.Particle, credStore, kvStore)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(ctx)

	_, err = p.CallTool(ctx, "nonexistent", []byte(`{}`))
	if err == nil {
		t.Fatal("expected error for missing tool")
	}
	var te *runtime.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("error = %v (%T), want *runtime.ToolError", err, err)
	}
	if te.Kind != runtime.ToolErrorKindNotFound {
		t.Errorf("kind = %v, want NotFound", te.Kind)
	}
}

func TestRuntime_CallTool_HandlerError(t *testing.T) {
	ctx := context.Background()
	src := `export default {
  name: "p", description: "p", version: "0.1.0", capabilities: {},
  tools: {
    boom: {
      description: "throw",
      inputSchema: { type: "object" },
      handler: async () => { throw new Error("kaboom"); },
    },
  },
};`
	res := buildParticle(t, src)
	rt, credStore, kvStore, cleanup := newRuntime(t, ctx)
	defer cleanup()
	p, _ := rt.NewParticle(ctx, res.Particle, credStore, kvStore)
	defer p.Close(ctx)

	_, err := p.CallTool(ctx, "boom", []byte(`{}`))
	var te *runtime.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("error = %v, want *runtime.ToolError", err)
	}
	if te.Kind != runtime.ToolErrorKindHandlerError {
		t.Errorf("kind = %v, want HandlerError", te.Kind)
	}
	if !strings.Contains(te.Message, "kaboom") {
		t.Errorf("message = %q, should contain handler's error text", te.Message)
	}
}

// Args that don't match the tool's input schema must be rejected
// host-side, before the JS handler runs. Per design doc §6, this
// is what makes "argument validation: host-side only" possible.
func TestRuntime_CallTool_ValidatesAgainstSchema(t *testing.T) {
	ctx := context.Background()
	src := `export default {
  name: "p", description: "p", version: "0.1.0", capabilities: {},
  tools: {
    add: {
      description: "Add two numbers",
      inputSchema: {
        type: "object",
        properties: { a: { type: "number" }, b: { type: "number" } },
        required: ["a", "b"],
      },
      handler: async ({ a, b }: { a: number; b: number }) => ({ sum: a + b }),
    },
  },
};`
	res := buildParticle(t, src)
	rt, credStore, kvStore, cleanup := newRuntime(t, ctx)
	defer cleanup()
	p, _ := rt.NewParticle(ctx, res.Particle, credStore, kvStore)
	defer p.Close(ctx)

	t.Run("missing required", func(t *testing.T) {
		_, err := p.CallTool(ctx, "add", []byte(`{"a":1}`))
		var te *runtime.ToolError
		if !errors.As(err, &te) || te.Kind != runtime.ToolErrorKindInvalidArguments {
			t.Fatalf("err = %v (%T), want InvalidArguments ToolError", err, err)
		}
	})

	t.Run("wrong type", func(t *testing.T) {
		_, err := p.CallTool(ctx, "add", []byte(`{"a":1,"b":"two"}`))
		var te *runtime.ToolError
		if !errors.As(err, &te) || te.Kind != runtime.ToolErrorKindInvalidArguments {
			t.Fatalf("err = %v (%T), want InvalidArguments ToolError", err, err)
		}
	})

	t.Run("valid payload still works", func(t *testing.T) {
		got, err := p.CallTool(ctx, "add", []byte(`{"a":2,"b":3}`))
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !strings.Contains(string(got), `"sum":5`) {
			t.Errorf("result = %s", got)
		}
	})
}

// Validation must precede the JS handler — i.e., a particle whose
// handler would throw when called with bogus args should never
// run, because we should reject the args first.
func TestRuntime_CallTool_ValidationPrecedesHandler(t *testing.T) {
	ctx := context.Background()
	// The handler fingerprints the call by writing to a global.
	// If the handler runs, the print shows up; we then verify it
	// did NOT run on an invalid call.
	src := `export default {
  name: "p", description: "p", version: "0.1.0", capabilities: {},
  tools: {
    setX: {
      description: "set",
      inputSchema: {
        type: "object",
        properties: { x: { type: "string" } },
        required: ["x"],
      },
      handler: async ({ x }: { x: string }) => {
        console.error("HANDLER_RAN:" + x);
        return { ok: true };
      },
    },
  },
};`
	res := buildParticle(t, src)
	rt, credStore, kvStore, cleanup := newRuntime(t, ctx)
	defer cleanup()
	p, _ := rt.NewParticle(ctx, res.Particle, credStore, kvStore)
	defer p.Close(ctx)

	_, err := p.CallTool(ctx, "setX", []byte(`{}`))
	if err == nil {
		t.Fatal("expected InvalidArguments")
	}
	// We can't easily inspect particle stderr from the test, but
	// the contract is: validation rejects before wasm sees the
	// call. Any future regression here would surface as a
	// HandlerError instead of InvalidArguments.
	var te *runtime.ToolError
	if !errors.As(err, &te) || te.Kind != runtime.ToolErrorKindInvalidArguments {
		t.Errorf("err = %v, want InvalidArguments (validation must precede handler)", err)
	}
}

// A particle that imports `@partite-ai/particle-credentials` must resolve the
// module at bundle-load time. Without the host-shim that registers
// `@partite-ai/particle-credentials` on top of the WIT-imported
// `particle:host/credentials@0.1.0`, ListTools panics with
// "Cannot find module '@partite-ai/particle-credentials'". This test pins the
// shim wiring.
//
// `getConfiguredMethod()` is invoked from a handler to verify the
// sync return shape (string | null) round-trips through the
// runtime — no credentials configured, so it should be null.
func TestRuntime_ParticleCredentialsModule_LoadsAndReturnsNull(t *testing.T) {
	ctx := context.Background()
	src := `import { credentials } from "@partite-ai/particle-credentials";
export default {
  name: "shim-test",
  description: "exercises the @partite-ai/particle-credentials shim",
  version: "0.1.0",
  capabilities: {},
  credentials: {
    svc: {
      required: false,
      methods: { tok: { type: "apikey", location: { kind: "header", name: "X-K" } } },
    },
  },
  tools: {
    method: {
      description: "Return the configured method name (or null).",
      inputSchema: { type: "object" },
      handler: async () => ({ method: credentials.getConfiguredMethod("svc") }),
    },
  },
};`
	res := buildParticle(t, src)
	rt, credStore, kvStore, cleanup := newRuntime(t, ctx)
	defer cleanup()
	p, err := rt.NewParticle(ctx, res.Particle, credStore, kvStore)
	if err != nil {
		t.Fatalf("NewParticle: %v", err)
	}
	defer p.Close(ctx)

	// ListTools triggers bundle evaluation, which would fail with
	// "Cannot find module '@partite-ai/particle-credentials'" if the shim
	// weren't registered.
	tools, err := p.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "method" {
		t.Fatalf("unexpected tools: %+v", tools)
	}

	// CallTool the handler. With no credential configured for
	// the runtime's empty memory store, getConfiguredMethod()
	// must return null — that's the JSON we expect on the wire.
	out, err := p.CallTool(ctx, "method", []byte(`{}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !strings.Contains(string(out), `"method":null`) {
		t.Errorf("result = %s, want method=null", out)
	}
}

// -----------------------------------------------------------------------------
// Ping
// -----------------------------------------------------------------------------

func TestRuntime_Ping_NotImplemented(t *testing.T) {
	ctx := context.Background()
	src := `export default {
  name: "p", description: "p", version: "0.1.0", capabilities: {},
  tools: {},
};`
	res := buildParticle(t, src)
	rt, credStore, kvStore, cleanup := newRuntime(t, ctx)
	defer cleanup()
	p, _ := rt.NewParticle(ctx, res.Particle, credStore, kvStore)
	defer p.Close(ctx)

	_, err := p.Ping(ctx)
	var he *runtime.HealthError
	if !errors.As(err, &he) {
		t.Fatalf("error = %v, want *runtime.HealthError", err)
	}
	if !he.NotImplemented {
		t.Errorf("expected NotImplemented; got %+v", he)
	}
}

func TestRuntime_Ping_Implemented(t *testing.T) {
	ctx := context.Background()
	src := `export default {
  name: "p", description: "p", version: "0.1.0", capabilities: {},
  tools: {},
  ping: async () => ({ status: "ok", message: "all good" }),
};`
	res := buildParticle(t, src)
	rt, credStore, kvStore, cleanup := newRuntime(t, ctx)
	defer cleanup()
	p, _ := rt.NewParticle(ctx, res.Particle, credStore, kvStore)
	defer p.Close(ctx)

	pr, err := p.Ping(ctx)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if pr.Status != runtime.PingStatusOK {
		t.Errorf("status = %v, want OK", pr.Status)
	}
	if pr.Message != "all good" {
		t.Errorf("message = %q", pr.Message)
	}
	// `details` was omitted; expect the option<string>::None
	// path (empty Go zero-value).
	if pr.Details != "" {
		t.Errorf("details = %q, want empty (None)", pr.Details)
	}
}

// -----------------------------------------------------------------------------
// Manifest / config validation
// -----------------------------------------------------------------------------

func TestRuntime_New_RejectsMissingDeps(t *testing.T) {
	ctx := context.Background()
	e := wacogo.NewEngine(ctx)
	defer e.Close(ctx)

	if _, err := runtime.New(ctx, runtime.Config{}); err == nil {
		t.Error("expected error for empty Config")
	}
	if _, err := runtime.New(ctx, runtime.Config{Engine: e}); err == nil {
		t.Error("expected error for missing managers")
	}
}

func TestRuntime_NewParticle_RejectsBadFS(t *testing.T) {
	ctx := context.Background()
	rt, credStore, kvStore, cleanup := newRuntime(t, ctx)
	defer cleanup()

	if _, err := rt.NewParticle(ctx, nil, credStore, kvStore); err == nil {
		t.Error("expected error for nil FS")
	}
	// FS without manifest.json
	if _, err := rt.NewParticle(ctx, fstest.MapFS{"bundle.js": &fstest.MapFile{Data: []byte("export default {};")}}, credStore, kvStore); err == nil {
		t.Error("expected error for FS without manifest.json")
	}
}
