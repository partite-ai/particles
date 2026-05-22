package runtime_test

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/partite-ai/particles/runtime"
)

// TestToolError_StackAndStderr_Captured verifies the structured
// diagnostic payload introduced for the error-detail WIT record:
// when a JS handler throws, the returned ToolError carries the
// guest stack (.Stack) and the wasi:cli/stderr buffer (.Stderr)
// alongside the user-visible message.
func TestToolError_StackAndStderr_Captured(t *testing.T) {
	manifest := `{
		"name": "err-probe",
		"description": "diagnostic probe",
		"version": "0.1.0",
		"runtime": "js",
		"capabilities": {},
		"tools": [{"name":"boom","description":"throw","inputSchema":{"type":"object"}}]
	}`
	bundle := `export default {
		name: "err-probe", description: "x", version: "0.1.0", capabilities: {},
		tools: {
			boom: {
				description: "throw",
				inputSchema: { type: "object" },
				handler: async () => {
					console.error("breadcrumb on stderr before the throw");
					function inner() { throw new Error("synthetic failure"); }
					function outer() { inner(); }
					outer();
				},
			},
		},
	};`
	fs := fstest.MapFS{
		"manifest.json": &fstest.MapFile{Data: []byte(manifest)},
		"bundle.mjs":    &fstest.MapFile{Data: []byte(bundle)},
	}

	ctx := context.Background()
	rt, credStore, kvStore, cleanup := newRuntime(t, ctx)
	defer cleanup()

	p, err := rt.NewParticle(ctx, fs, credStore, kvStore)
	if err != nil {
		t.Fatalf("NewParticle: %v", err)
	}
	defer p.Close(ctx)

	_, err = p.CallTool(ctx, "boom", []byte(`{}`))
	if err == nil {
		t.Fatal("CallTool: expected error from handler throw")
	}
	te, ok := err.(*runtime.ToolError)
	if !ok {
		t.Fatalf("err = %T, want *runtime.ToolError: %v", err, err)
	}
	if te.Kind != runtime.ToolErrorKindHandlerError {
		t.Errorf("Kind = %v, want HandlerError", te.Kind)
	}
	if !strings.Contains(te.Message, "synthetic failure") {
		t.Errorf("Message should carry the throw text: %q", te.Message)
	}
	if te.Stack == "" {
		t.Error("Stack is empty; expected guest-side stack from the caught Error")
	} else {
		// QuickJS stacks include the function names.
		if !strings.Contains(te.Stack, "inner") || !strings.Contains(te.Stack, "outer") {
			t.Errorf("Stack missing frame names — got:\n%s", te.Stack)
		}
	}
	if !strings.Contains(te.Stderr, "breadcrumb on stderr before the throw") {
		t.Errorf("Stderr missing the console.error breadcrumb — got:\n%s", te.Stderr)
	}
	// Error() string should compose them into one readable blob.
	s := te.Error()
	if !strings.Contains(s, "handler error: ") || !strings.Contains(s, "\nstack:\n") || !strings.Contains(s, "\nstderr:\n") {
		t.Errorf("Error() should include head + stack + stderr sections — got:\n%s", s)
	}
}
