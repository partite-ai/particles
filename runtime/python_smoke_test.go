package runtime

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/partite-ai/wacogo"
	"github.com/partite-ai/wacogo/wasi"
	"github.com/partite-ai/wacogo/wasi/filesystem/preopens"

	wccreds "github.com/partite-ai/particles/internal/host/gen/particle/host/credentials"
	wcdyld "github.com/partite-ai/particles/internal/host/gen/particle/host/dyld"
	wckv "github.com/partite-ai/particles/internal/host/gen/particle/host/kv"
	wclibffi "github.com/partite-ai/particles/internal/host/gen/particle/host/libffi"
	wcoauth "github.com/partite-ai/particles/internal/host/gen/particle/host/oauth"
	wcsigning "github.com/partite-ai/particles/internal/host/gen/particle/host/signing"
	"github.com/partite-ai/particles/internal/runtime/dyld"
	"github.com/partite-ai/particles/internal/runtime/libffi"
)

// nullHostAdapters bundles no-op adapter impls for the four
// particle:host capability interfaces the python-runtime imports.
// The smoke test doesn't exercise these — they only need to satisfy
// instantiation, since host_module.rs references the wit-bindgen-
// generated symbols (which makes the imports live regardless of
// whether Python code actually calls into them).
type nullCreds struct{}

func (nullCreds) GetConfiguredMethod(_ context.Context, _ string) (wccreds.ResultOptionStringCredentialError, error) {
	return wccreds.ResultOptionStringCredentialErrorErr{Value: wccreds.CredentialErrorNotConfigured{}}, nil
}
func (nullCreds) GetPlaceholder(_ context.Context, _ string) (wccreds.ResultPlaceholderInfoCredentialError, error) {
	return wccreds.ResultPlaceholderInfoCredentialErrorErr{Value: wccreds.CredentialErrorNotConfigured{}}, nil
}
func (nullCreds) GetRaw(_ context.Context, _ string) (wccreds.ResultStringCredentialError, error) {
	return wccreds.ResultStringCredentialErrorErr{Value: wccreds.CredentialErrorNotConfigured{}}, nil
}

type nullKV struct{}

func (nullKV) Get(_ context.Context, _ string) (wckv.ResultOptionStringKvError, error) {
	return wckv.ResultOptionStringKvErrorOk{Value: wckv.NoneString()}, nil
}
func (nullKV) Set(_ context.Context, _, _ string) (wckv.Result_KvError, error) {
	return wckv.Result_KvErrorOk{}, nil
}
func (nullKV) Delete(_ context.Context, _ string) (wckv.Result_KvError, error) {
	return wckv.Result_KvErrorOk{}, nil
}
func (nullKV) List(_ context.Context, _ string) (wckv.ResultListStringKvError, error) {
	return wckv.ResultListStringKvErrorOk{Value: nil}, nil
}

type nullOauth struct{}

func (nullOauth) Refresh(_ context.Context, _ string) (wcoauth.Result_OauthError, error) {
	return wcoauth.Result_OauthErrorErr{Value: wcoauth.OauthErrorNotConfigured{}}, nil
}

type nullSigning struct{}

func (nullSigning) Sign(_ context.Context, _ string, _ []uint8) (wcsigning.ResultListU8SigningError, error) {
	return wcsigning.ResultListU8SigningErrorErr{Value: wcsigning.SigningErrorNotConfigured{}}, nil
}
func (nullSigning) Verify(_ context.Context, _ string, _, _ []uint8) (wcsigning.ResultBoolSigningError, error) {
	return wcsigning.ResultBoolSigningErrorErr{Value: wcsigning.SigningErrorNotConfigured{}}, nil
}

// pythonSubTestHelpers consolidates the wacogo.Val unwrap boilerplate
// the sub-tests below share. Kept as plain functions (not methods) so
// each sub-test stays self-contained at the call site.
func recordField(rec *wacogo.ValRecord, name string) wacogo.Val {
	for _, f := range rec.Fields() {
		if f.Name == name {
			return f.Val
		}
	}
	return nil
}

func recordStringField(t *testing.T, rec *wacogo.ValRecord, name string) string {
	t.Helper()
	v := recordField(rec, name)
	if v == nil {
		t.Fatalf("record missing field %q (fields: %v)", name, fieldNames(rec))
	}
	s, ok := v.(wacogo.ValString)
	if !ok {
		t.Fatalf("record field %q is %T, want ValString", name, v)
	}
	return string(s)
}

func fieldNames(rec *wacogo.ValRecord) []string {
	out := make([]string, 0, len(rec.Fields()))
	for _, f := range rec.Fields() {
		out = append(out, f.Name)
	}
	return out
}

// TestPythonRuntime_SmokeBoot validates the end-to-end wire-up of
// the dyld-based python runtime component: it loads
// dist/particle-python-runtime.wasm, stands up the dyld host
// adapter at particle:host/dyld@0.1.0, mounts the embedded CPython
// stdlib (via the python_stdlib zip) AND a tiny bundle.py through a
// merged wasi preopen, and instantiates the component. If
// instantiation succeeds, the inject-capture/init-dyld/Initialize
// dance worked, libpython3.14.so composed in cleanly, and the
// runtime's bootstrap had a Python interpreter to set up.
func TestPythonRuntime_SmokeBoot(t *testing.T) {
	const componentPath = "../dist/particle-python-runtime.wasm"
	wasmBytes, err := os.ReadFile(componentPath)
	if err != nil {
		t.Skipf("python runtime not built (run `make python-runtime`): %v", err)
	}

	ctx := context.Background()
	engine := wacogo.NewEngine(ctx)
	defer engine.Close(ctx)

	// dyld adapter: serves the runtime's particle:host/dyld import.
	// The .so files Python C extensions would resolve through dlopen
	// come from `soFS` — empty here because the smoke test bundle
	// has no extension imports.
	soFS := fstest.MapFS{}
	dyldAdapter, err := dyld.NewAdapter(dyld.AdapterConfig{
		Engine: engine,
		FS:     soFS,
	})
	if err != nil {
		t.Fatalf("dyld.NewAdapter: %v", err)
	}

	dyldFac, err := wcdyld.NewFactory(ctx, engine)
	if err != nil {
		t.Fatalf("dyld NewFactory: %v", err)
	}
	defer dyldFac.Close(ctx)
	dyldInst, err := dyldFac.NewInstance(ctx, dyldAdapter, nil)
	if err != nil {
		t.Fatalf("dyld NewInstance: %v", err)
	}
	defer dyldInst.Close(ctx)

	// Build the wasi preopen FS: stdlib + runtime bootstrap + bundle.
	stdlibFS, err := pythonStdlibFS()
	if err != nil {
		t.Fatalf("load python stdlib FS: %v", err)
	}
	bootstrapFS, err := pythonBootstrapFS()
	if err != nil {
		t.Fatalf("load python bootstrap FS: %v", err)
	}
	// A minimal real particle: declares one tool that echoes its
	// input. Exercises the Particle DSL the bootstrap expects.
	const bundleSource = `
from particle.manifest import Particle, Tool

def _echo(message: str) -> dict:
    return {"echoed": message}

particle = Particle(
    name="smoke-test",
    description="v2 runtime smoke test particle",
    version="0.0.1",
    tools={
        "echo": Tool(
            description="Echoes the input message back.",
            input_schema={
                "type": "object",
                "properties": {"message": {"type": "string"}},
                "required": ["message"],
            },
            handler=_echo,
        ),
    },
)
`
	bundleFS := fstest.MapFS{
		"bundle.py": &fstest.MapFile{Data: []byte(bundleSource)},
	}
	stderr := &bytes.Buffer{}
	w, err := wasi.NewWorld(ctx, engine, &wasi.Config{
		Args: []string{"particle-python-runtime"},
		Env: [][2]string{
			// Tell CPython where to find encodings/* etc.
			{"PYTHONHOME", "/usr/local"},
			// Mute Python's "no site-packages" warning at startup;
			// the bootstrap controls sys.path explicitly.
			{"PYTHONNOUSERSITE", "1"},
		},
		Stdin:  strings.NewReader(""),
		Stdout: io.Discard,
		Stderr: stderr,
		Preopens: preopens.NewMultiFSPreopens([]*preopens.PreopenEntry{
			{Path: "/" + pythonStdlibMountPath, Root: ".", FS: preopens.ImmutableFS{FS: stdlibFS}},
			{Path: "/runtime", Root: ".", FS: preopens.ImmutableFS{FS: bootstrapFS}},
			{Path: "/particle", Root: ".", FS: preopens.ImmutableFS{FS: bundleFS}},
		}),
	})
	if err != nil {
		t.Fatalf("wasi.NewWorld: %v", err)
	}
	defer w.Close(ctx)

	comp, err := engine.LoadComponent(ctx, bytes.NewReader(wasmBytes))
	if err != nil {
		t.Fatalf("LoadComponent: %v", err)
	}

	// Stub host instances for credentials/kv/oauth/signing. These
	// satisfy the python-runtime's WIT imports (now live because
	// host_module.rs references the wit-bindgen-generated functions)
	// without needing the full credentials.Manager / kv.Service
	// machinery — the smoke test isn't exercising those APIs.
	credsFac, err := wccreds.NewFactory(ctx, engine)
	if err != nil {
		t.Fatalf("credentials NewFactory: %v", err)
	}
	defer credsFac.Close(ctx)
	credsInst, err := credsFac.NewInstance(ctx, nullCreds{}, nil)
	if err != nil {
		t.Fatalf("credentials NewInstance: %v", err)
	}
	defer credsInst.Close(ctx)

	kvFac, err := wckv.NewFactory(ctx, engine)
	if err != nil {
		t.Fatalf("kv NewFactory: %v", err)
	}
	defer kvFac.Close(ctx)
	kvInst, err := kvFac.NewInstance(ctx, nullKV{}, nil)
	if err != nil {
		t.Fatalf("kv NewInstance: %v", err)
	}
	defer kvInst.Close(ctx)

	oauthFac, err := wcoauth.NewFactory(ctx, engine)
	if err != nil {
		t.Fatalf("oauth NewFactory: %v", err)
	}
	defer oauthFac.Close(ctx)
	oauthInst, err := oauthFac.NewInstance(ctx, nullOauth{}, nil)
	if err != nil {
		t.Fatalf("oauth NewInstance: %v", err)
	}
	defer oauthInst.Close(ctx)

	signingFac, err := wcsigning.NewFactory(ctx, engine)
	if err != nil {
		t.Fatalf("signing NewFactory: %v", err)
	}
	defer signingFac.Close(ctx)
	signingInst, err := signingFac.NewInstance(ctx, nullSigning{}, nil)
	if err != nil {
		t.Fatalf("signing NewInstance: %v", err)
	}
	defer signingInst.Close(ctx)

	// libffi adapter — wired the same way components/python-runtime's
	// runtime.go does for production particles. Smoke test doesn't
	// exercise cffi, but the import must be live or instantiation
	// fails (libffi-wasi-bridge is composed into the python-runtime
	// wasm and unconditionally imports the host interface).
	libffiAdapter := libffi.NewAdapter(engine.WazeroRuntime())
	libffiFac, err := wclibffi.NewFactory(ctx, engine)
	if err != nil {
		t.Fatalf("libffi NewFactory: %v", err)
	}
	defer libffiFac.Close(ctx)
	libffiInst, err := libffiFac.NewInstance(ctx, libffiAdapter, nil)
	if err != nil {
		t.Fatalf("libffi NewInstance: %v", err)
	}
	defer libffiInst.Close(ctx)

	imports := append(
		w.Imports(),
		wacogo.WithInstanceImport(wcdyld.InterfaceName, dyldInst.Core()),
		wacogo.WithInstanceImport(wccreds.InterfaceName, credsInst.Core()),
		wacogo.WithInstanceImport(wckv.InterfaceName, kvInst.Core()),
		wacogo.WithInstanceImport(wcoauth.InterfaceName, oauthInst.Core()),
		wacogo.WithInstanceImport(wcsigning.InterfaceName, signingInst.Core()),
		wacogo.WithInstanceImport(wclibffi.InterfaceName, libffiInst.Core()),
	)
	inst, err := comp.Instantiate(ctx, imports...)
	if err != nil {
		t.Fatalf("Instantiate: %v\nstderr:\n%s", err, stderr.String())
	}
	defer inst.Close(ctx)

	t.Logf("v2 runtime instantiated successfully (%d bytes captured stderr)", stderr.Len())

	// Now exercise list_tools to see how far the Python init path
	// goes. Empty list / typed error both count as "Python booted";
	// a wasm trap or missing-export error means a wire-up gap.
	t.Run("list-tools", func(t *testing.T) {
		iface := inst.ExportedInstance("particle:runtime/tools@0.1.0")
		if iface == nil {
			t.Fatal("component does not export particle:runtime/tools@0.1.0")
		}
		fn := iface.ExportedFunc("list-tools")
		if fn == nil {
			t.Fatal("tools interface does not export list-tools")
		}
		results, err := fn.Call(ctx)
		if err != nil {
			t.Logf("list-tools call err: %v", err)
			t.Logf("stderr:\n%s", stderr.String())
			t.Fatalf("list-tools failed (next step: more Python init work)")
		}
		if len(results) != 1 {
			t.Fatalf("expected 1 result wrapper, got %d", len(results))
		}
		list, ok := results[0].(*wacogo.ValList)
		if !ok {
			t.Fatalf("result[0] is %T, want *wacogo.ValList", results[0])
		}
		t.Logf("list-tools returned %d tools", list.Len())
		for i := 0; i < list.Len(); i++ {
			rec, ok := list.Get(i).(*wacogo.ValRecord)
			if !ok {
				t.Errorf("tool[%d] is %T, want *wacogo.ValRecord", i, list.Get(i))
				continue
			}
			var name, desc string
			for _, f := range rec.Fields() {
				if s, ok := f.Val.(wacogo.ValString); ok {
					switch f.Name {
					case "name":
						name = string(s)
					case "description":
						desc = string(s)
					}
				}
			}
			t.Logf("  tool[%d]: name=%q description=%q", i, name, desc)
		}
		if stderr.Len() > 0 {
			t.Logf("stderr:\n%s", stderr.String())
		}
	})

	// call-tool: invoke `echo` with the canonical message and verify
	// the JSON-encoded handler result round-trips back. Exercises the
	// PyTuple_New + PyObject_CallObject + str-unwrap path on the Rust
	// side, and the json.loads/dumps + handler dispatch path in
	// bootstrap.py.
	t.Run("call-tool/echo", func(t *testing.T) {
		iface := inst.ExportedInstance("particle:runtime/tools@0.1.0")
		if iface == nil {
			t.Fatal("component does not export particle:runtime/tools@0.1.0")
		}
		fn := iface.ExportedFunc("call-tool")
		if fn == nil {
			t.Fatal("tools interface does not export call-tool")
		}
		results, err := fn.Call(ctx, wacogo.ValString("echo"),
			wacogo.ValString(`{"message":"hello"}`))
		if err != nil {
			t.Logf("stderr:\n%s", stderr.String())
			t.Fatalf("call-tool: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("expected 1 result, got %d", len(results))
		}
		res, ok := results[0].(*wacogo.ValResult)
		if !ok {
			t.Fatalf("result[0] is %T, want *wacogo.ValResult", results[0])
		}
		if !res.IsOk() {
			// Best-effort dump of the err-variant payload so a
			// regression here doesn't leave the operator guessing
			// which case the runtime returned.
			if v, ok := res.Err().(*wacogo.ValVariant); ok {
				if rec, ok := v.Val().(*wacogo.ValRecord); ok {
					for _, f := range rec.Fields() {
						t.Logf("err.%s = %#v", f.Name, f.Val)
					}
				}
				t.Fatalf("call-tool returned err disc=%d\nstderr:\n%s",
					v.Discriminant(), stderr.String())
			}
			t.Fatalf("call-tool returned non-variant err: %#v", res.Err())
		}
		got, ok := res.Ok().(wacogo.ValString)
		if !ok {
			t.Fatalf("ok payload is %T, want ValString", res.Ok())
		}
		const want = `{"echoed": {"message": "hello"}}`
		if string(got) != want {
			t.Fatalf("echo handler returned %q, want %q", string(got), want)
		}
	})

	// call-tool/not-found: dispatch to a tool name the bundle didn't
	// register and verify we get the `not-found` variant (discriminant
	// 0 in the WIT, no payload). Exercises the bootstrap's NotFound
	// exception → Rust ToolError routing.
	t.Run("call-tool/not-found", func(t *testing.T) {
		iface := inst.ExportedInstance("particle:runtime/tools@0.1.0")
		fn := iface.ExportedFunc("call-tool")
		results, err := fn.Call(ctx, wacogo.ValString("nope"), wacogo.ValString(`{}`))
		if err != nil {
			t.Fatalf("call-tool(nope): %v\nstderr:\n%s", err, stderr.String())
		}
		res := results[0].(*wacogo.ValResult)
		if res.IsOk() {
			t.Fatalf("expected err variant, got ok: %+v", res.Ok())
		}
		v, ok := res.Err().(*wacogo.ValVariant)
		if !ok {
			t.Fatalf("err payload is %T, want *wacogo.ValVariant", res.Err())
		}
		if v.Discriminant() != 0 {
			t.Fatalf("variant discriminant = %d, want 0 (not-found)", v.Discriminant())
		}
	})

	// call-tool/invalid-arguments: send malformed JSON to exercise the
	// `invalid-arguments` path through json.loads inside bootstrap.
	// Discriminant 1 in the WIT; payload is an error-detail record.
	t.Run("call-tool/invalid-arguments", func(t *testing.T) {
		iface := inst.ExportedInstance("particle:runtime/tools@0.1.0")
		fn := iface.ExportedFunc("call-tool")
		results, err := fn.Call(ctx, wacogo.ValString("echo"),
			wacogo.ValString("not-json"))
		if err != nil {
			t.Fatalf("call-tool(bad json): %v\nstderr:\n%s", err, stderr.String())
		}
		res := results[0].(*wacogo.ValResult)
		if res.IsOk() {
			t.Fatalf("expected err variant, got ok")
		}
		v, ok := res.Err().(*wacogo.ValVariant)
		if !ok {
			t.Fatalf("err payload is %T, want *wacogo.ValVariant", res.Err())
		}
		if v.Discriminant() != 1 {
			t.Fatalf("variant discriminant = %d, want 1 (invalid-arguments)", v.Discriminant())
		}
		rec, ok := v.Val().(*wacogo.ValRecord)
		if !ok {
			t.Fatalf("invalid-arguments payload is %T, want *wacogo.ValRecord", v.Val())
		}
		if msg := recordStringField(t, rec, "message"); msg == "" {
			t.Errorf("invalid-arguments.message is empty")
		}
	})

	// ping: the smoke bundle does NOT declare a `ping` handler on its
	// Particle. The bootstrap should detect that and raise the
	// NotImplementedHealth marker class, which the Rust shim maps to
	// HealthError::NotImplemented (no payload).
	t.Run("ping/not-implemented", func(t *testing.T) {
		iface := inst.ExportedInstance("particle:runtime/health@0.1.0")
		if iface == nil {
			t.Fatal("component does not export particle:runtime/health@0.1.0")
		}
		fn := iface.ExportedFunc("ping")
		if fn == nil {
			t.Fatal("health interface does not export ping")
		}
		results, err := fn.Call(ctx)
		if err != nil {
			t.Fatalf("ping: %v\nstderr:\n%s", err, stderr.String())
		}
		res, ok := results[0].(*wacogo.ValResult)
		if !ok {
			t.Fatalf("result[0] is %T, want *wacogo.ValResult", results[0])
		}
		if res.IsOk() {
			t.Fatalf("expected err variant (not-implemented), got ok: %+v", res.Ok())
		}
		v, ok := res.Err().(*wacogo.ValVariant)
		if !ok {
			t.Fatalf("err payload is %T, want *wacogo.ValVariant", res.Err())
		}
		// health-error: not-implemented (0), handler-error (1).
		if v.Discriminant() != 0 {
			t.Fatalf("variant discriminant = %d, want 0 (not-implemented)", v.Discriminant())
		}
	})

	// get-manifest: round-trip the bundle's Particle declaration
	// through the manifest export. Exercises the deepest record path
	// — capabilities (no http), credentials (empty list), tools
	// (one entry).
	t.Run("get-manifest", func(t *testing.T) {
		iface := inst.ExportedInstance("particle:runtime/manifest@0.1.0")
		if iface == nil {
			t.Fatal("component does not export particle:runtime/manifest@0.1.0")
		}
		fn := iface.ExportedFunc("get-manifest")
		if fn == nil {
			t.Fatal("manifest interface does not export get-manifest")
		}
		results, err := fn.Call(ctx)
		if err != nil {
			t.Fatalf("get-manifest: %v\nstderr:\n%s", err, stderr.String())
		}
		res, ok := results[0].(*wacogo.ValResult)
		if !ok {
			t.Fatalf("result[0] is %T, want *wacogo.ValResult", results[0])
		}
		if !res.IsOk() {
			t.Fatalf("get-manifest err variant: %+v\nstderr:\n%s", res.Err(), stderr.String())
		}
		rec, ok := res.Ok().(*wacogo.ValRecord)
		if !ok {
			t.Fatalf("ok payload is %T, want *wacogo.ValRecord", res.Ok())
		}

		if got := recordStringField(t, rec, "name"); got != "smoke-test" {
			t.Errorf(".name = %q, want %q", got, "smoke-test")
		}
		if got := recordStringField(t, rec, "description"); got != "v2 runtime smoke test particle" {
			t.Errorf(".description = %q, want %q", got, "v2 runtime smoke test particle")
		}
		if got := recordStringField(t, rec, "version"); got != "0.0.1" {
			t.Errorf(".version = %q, want %q", got, "0.0.1")
		}

		// tools: list with one entry, name="echo".
		toolsField := recordField(rec, "tools")
		tools, ok := toolsField.(*wacogo.ValList)
		if !ok {
			t.Fatalf(".tools is %T, want *wacogo.ValList", toolsField)
		}
		if tools.Len() != 1 {
			t.Fatalf(".tools has %d entries, want 1", tools.Len())
		}
		tool, ok := tools.Get(0).(*wacogo.ValRecord)
		if !ok {
			t.Fatalf(".tools[0] is %T, want *wacogo.ValRecord", tools.Get(0))
		}
		if got := recordStringField(t, tool, "name"); got != "echo" {
			t.Errorf(".tools[0].name = %q, want %q", got, "echo")
		}

		// credentials: empty list.
		credsField := recordField(rec, "credentials")
		creds, ok := credsField.(*wacogo.ValList)
		if !ok {
			t.Fatalf(".credentials is %T, want *wacogo.ValList", credsField)
		}
		if creds.Len() != 0 {
			t.Errorf(".credentials has %d entries, want 0", creds.Len())
		}

		// capabilities.http: None (smoke bundle didn't declare http=Http(...)).
		capsField := recordField(rec, "capabilities")
		caps, ok := capsField.(*wacogo.ValRecord)
		if !ok {
			t.Fatalf(".capabilities is %T, want *wacogo.ValRecord", capsField)
		}
		http := recordField(caps, "http")
		opt, ok := http.(*wacogo.ValOption)
		if !ok {
			t.Fatalf(".capabilities.http is %T, want *wacogo.ValOption", http)
		}
		if !opt.IsNone() {
			t.Errorf(".capabilities.http = Some(%v), want None", opt.Val())
		}
	})
}

