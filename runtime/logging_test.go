package runtime_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/partite-ai/wacogo"

	"github.com/partite-ai/particles/credentials"
	credmem "github.com/partite-ai/particles/credentials/memory"
	"github.com/partite-ai/particles/kv"
	kvmem "github.com/partite-ai/particles/kv/memory"
	"github.com/partite-ai/particles/runtime"
)

// A particle that calls `console.error` from a handler routes
// through the runtime's wasi:logging/log import. With a
// [runtime.LogCallback] wired in, the message should land in
// our recorder at the `error` level — confirms the stack-
// decoding path (level enum + two strings) round-trips
// correctly and that the runtime threads the callback through.
func TestRuntime_Logging_RoutesConsoleError(t *testing.T) {
	ctx := context.Background()
	src := `export default {
  name: "console-test",
  description: "logs from inside a handler",
  version: "0.1.0",
  capabilities: {},
  tools: {
    say: {
      description: "console.log a message",
      inputSchema: { type: "object" },
      handler: async () => { console.error("hello from wasm"); return { ok: true }; },
    },
  },
};`
	res := buildParticle(t, src)

	var (
		mu   sync.Mutex
		seen []seenLog
	)
	cb := func(_ context.Context, level runtime.LogLevel, scope, message string) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, seenLog{level: level, scope: scope, message: message})
	}

	rt, cleanup := newRuntimeWithLog(t, ctx, cb)
	defer cleanup()

	p, err := rt.NewParticle(ctx, res.Particle)
	if err != nil {
		t.Fatalf("NewParticle: %v", err)
	}
	defer p.Close(ctx)

	if _, err := p.CallTool(ctx, "say", []byte(`{}`)); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("no log calls recorded; console.log inside handler should have surfaced one")
	}
	// Find the matching record — wasm-rquickjs may also emit
	// internal frames; we only care that ours showed up.
	var hit *seenLog
	for i := range seen {
		if seen[i].message == "hello from wasm" {
			hit = &seen[i]
			break
		}
	}
	if hit == nil {
		t.Fatalf("expected to see 'hello from wasm' in logs; got %+v", seen)
	}
	// console.error routes to the "error" level per the
	// wasm-rquickjs / wasi:logging convention.
	if hit.level != runtime.LogLevelError {
		t.Errorf("level = %q, want %q", hit.level, runtime.LogLevelError)
	}
}

// Nil LogCallback keeps the host instance no-op (the import
// still has to be satisfied for the runtime wasm to instantiate;
// the runtime just drops anything the guest sends).
func TestRuntime_Logging_NilCallback_DoesNotPanic(t *testing.T) {
	ctx := context.Background()
	rt, cleanup := newRuntimeWithLog(t, ctx, nil)
	defer cleanup()
	if rt == nil {
		t.Fatal("expected a runtime, got nil")
	}
}

// HTTPClient wiring: a custom doer should receive every outbound
// request the particle initiates. Uses an httptest server so we
// can assert the URL passed through the policy, and a recording
// doer so we know our doer was the one that ran.
func TestRuntime_HTTPClient_CustomDoer(t *testing.T) {
	ctx := context.Background()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	src := `export default {
  name: "http-test",
  description: "fetches a thing",
  version: "0.1.0",
  capabilities: { http: { allowedHosts: ["` + upstreamHost(t, upstream.URL) + `"] } },
  tools: {
    go: {
      description: "fetch once",
      inputSchema: { type: "object" },
      handler: async () => {
        const r = await fetch("` + upstream.URL + `/probe");
        return { status: r.status };
      },
    },
  },
};`
	res := buildParticle(t, src)

	var (
		mu      sync.Mutex
		gotURLs []string
	)
	doer := doerFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		gotURLs = append(gotURLs, req.URL.String())
		mu.Unlock()
		return http.DefaultClient.Do(req)
	})

	rt, cleanup := newRuntimeWithHTTPClient(t, ctx, doer)
	defer cleanup()

	p, err := rt.NewParticle(ctx, res.Particle)
	if err != nil {
		t.Fatalf("NewParticle: %v", err)
	}
	defer p.Close(ctx)

	if _, err := p.CallTool(ctx, "go", []byte(`{}`)); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(gotURLs) != 1 {
		t.Fatalf("custom doer saw %d requests, want 1; gotURLs=%v", len(gotURLs), gotURLs)
	}
	if gotURLs[0] != upstream.URL+"/probe" {
		t.Errorf("doer received %q, want %q", gotURLs[0], upstream.URL+"/probe")
	}
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

type seenLog struct {
	level   runtime.LogLevel
	scope   string
	message string
}

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func newRuntimeWithLog(t *testing.T, ctx context.Context, cb runtime.LogCallback) (*runtime.Runtime, func()) {
	t.Helper()
	e := wacogo.NewEngine(ctx)
	credMgr, err := credentials.NewManager(ctx, credentials.ManagerConfig{Engine: e, Store: credmem.New()})
	if err != nil {
		t.Fatalf("credentials.NewManager: %v", err)
	}
	kvMgr, err := kv.NewManager(ctx, kv.ManagerConfig{Engine: e, Store: kvmem.New()})
	if err != nil {
		t.Fatalf("kv.NewManager: %v", err)
	}
	rt, err := runtime.New(ctx, runtime.Config{
		Engine: e, Credentials: credMgr, KV: kvMgr,
		Log: cb,
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	return rt, func() {
		_ = rt.Close(ctx)
		_ = credMgr.Close(ctx)
		_ = kvMgr.Close(ctx)
		_ = e.Close(ctx)
	}
}

func newRuntimeWithHTTPClient(t *testing.T, ctx context.Context, c runtime.HTTPDoer) (*runtime.Runtime, func()) {
	t.Helper()
	e := wacogo.NewEngine(ctx)
	credMgr, err := credentials.NewManager(ctx, credentials.ManagerConfig{Engine: e, Store: credmem.New()})
	if err != nil {
		t.Fatalf("credentials.NewManager: %v", err)
	}
	kvMgr, err := kv.NewManager(ctx, kv.ManagerConfig{Engine: e, Store: kvmem.New()})
	if err != nil {
		t.Fatalf("kv.NewManager: %v", err)
	}
	rt, err := runtime.New(ctx, runtime.Config{
		Engine: e, Credentials: credMgr, KV: kvMgr,
		HTTPClient: c,
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	return rt, func() {
		_ = rt.Close(ctx)
		_ = credMgr.Close(ctx)
		_ = kvMgr.Close(ctx)
		_ = e.Close(ctx)
	}
}

// upstreamHost strips the scheme + port from an httptest URL so
// the manifest's allowedHosts compares against just the host
// (the policy does case-insensitive host-only matching).
func upstreamHost(t *testing.T, raw string) string {
	t.Helper()
	// httptest URLs look like http://127.0.0.1:42345 — split on
	// "://" then take everything up to the next slash. The port
	// is part of the URL but the policy matches on the bare
	// hostname; including the port here would silently miss.
	rest := raw
	if i := indexOf(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if i := indexOf(rest, ":"); i >= 0 {
		rest = rest[:i]
	}
	if i := indexOf(rest, "/"); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
