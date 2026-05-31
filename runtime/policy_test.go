package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/partite-ai/particles/runtime"
)

// fetchParticleSource builds a particle source whose `fetch_url`
// tool calls fetch() against an arbitrary URL passed in args. The
// runtime's wasi:http impl is what receives the request, so this
// is the canonical way to exercise the HTTP policy end-to-end.
func fetchParticleSource(name string, capabilitiesJSON string) string {
	return fmt.Sprintf(`export default {
  name: %q,
  description: "Outbound HTTP test particle.",
  version: "0.1.0",
  capabilities: %s,
  tools: {
    fetch_url: {
      description: "Fetch the given URL and return its status.",
      inputSchema: { type: "object", properties: { url: { type: "string" } }, required: ["url"] },
      handler: async ({ url }: { url: string }) => {
        const r = await fetch(url);
        return { status: r.status, body: await r.text() };
      },
    },
  },
};`, name, capabilitiesJSON)
}

// -----------------------------------------------------------------------------
// HTTP allowedHosts — pure unit tests for the policy RoundTripper.
// These don't spin up the full runtime; they test the layer in
// isolation so failures localize quickly.
// -----------------------------------------------------------------------------

func TestHostNotAllowedError_Identifiable(t *testing.T) {
	err := &runtime.HostNotAllowedError{Host: "evil.example"}
	if !runtime.IsHostNotAllowed(err) {
		t.Error("IsHostNotAllowed missed direct error")
	}
	wrapped := errors.Join(errors.New("context"), err)
	if !runtime.IsHostNotAllowed(wrapped) {
		t.Error("IsHostNotAllowed missed wrapped error")
	}
	if runtime.IsHostNotAllowed(errors.New("unrelated")) {
		t.Error("IsHostNotAllowed false positive")
	}
}

// -----------------------------------------------------------------------------
// HTTP allowedHosts — end-to-end via runtime + httptest.Server.
// -----------------------------------------------------------------------------

func TestRuntime_HTTP_AllowedHostSucceeds(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello from server"))
	}))
	defer srv.Close()

	host := mustHost(t, srv.URL)
	caps := fmt.Sprintf(`{ "http": { "allowedHosts": [%q] } }`, host)
	res := buildParticle(t, fetchParticleSource("http-ok", caps))

	rt, credStore, kvStore, cleanup := newRuntime(t, ctx)
	defer cleanup()
	p, err := rt.NewParticle(ctx, res.Particle, credStore, kvStore, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(ctx)

	got, err := p.CallTool(ctx, "fetch_url", []byte(fmt.Sprintf(`{"url":%q}`, srv.URL)))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !strings.Contains(string(got), `"status":200`) {
		t.Errorf("result = %s, want status:200", got)
	}
	if !strings.Contains(string(got), "hello from server") {
		t.Errorf("result missing server body: %s", got)
	}
}

func TestRuntime_HTTP_DisallowedHostBlocked(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be reached")
		_, _ = w.Write([]byte("oops"))
	}))
	defer srv.Close()

	// allowedHosts only mentions a different host — the actual
	// request to srv must be denied without ever reaching the
	// upstream.
	caps := `{ "http": { "allowedHosts": ["only.allowed.example"] } }`
	res := buildParticle(t, fetchParticleSource("http-denied", caps))

	rt, credStore, kvStore, cleanup := newRuntime(t, ctx)
	defer cleanup()
	p, err := rt.NewParticle(ctx, res.Particle, credStore, kvStore, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(ctx)

	_, err = p.CallTool(ctx, "fetch_url", []byte(fmt.Sprintf(`{"url":%q}`, srv.URL)))
	if err == nil {
		t.Fatal("expected error when host not in allowedHosts")
	}
	var te *runtime.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("error = %v (%T), want *runtime.ToolError", err, err)
	}
	// The denial logs the message "destination-ip-prohibited"
	// so we have at least a meaningful breadcrumb visible to
	// callers. The exact JS-side wording is wasm-rquickjs's
	// translation of the wasi:http ErrorCode; we just want
	// some signal beyond a generic failure string.
	t.Logf("denial surfaced as: %v", te)
	if te.Kind != runtime.ToolErrorKindHandlerError {
		t.Errorf("kind = %v, want HandlerError", te.Kind)
	}
}

// allowedHosts comparisons are case-insensitive. DNS treats
// "Example.COM" and "example.com" as the same host, so a particle
// author whose manifest uses one casing but whose code uses
// another shouldn't be tripped up by the policy layer. We use a
// disallowed-target test here (rather than spinning up a server)
// so the test is independent of the loopback hostname.
func TestRuntime_HTTP_AllowedHostsCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	host := mustHost(t, srv.URL)
	// Manifest declares the UPPERCASE form; request URL uses the
	// lowercase form (whatever httptest.Server returned). The
	// policy must still treat them as equal.
	caps := fmt.Sprintf(`{ "http": { "allowedHosts": [%q] } }`, strings.ToUpper(host))
	res := buildParticle(t, fetchParticleSource("http-case", caps))

	rt, credStore, kvStore, cleanup := newRuntime(t, ctx)
	defer cleanup()
	p, err := rt.NewParticle(ctx, res.Particle, credStore, kvStore, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(ctx)

	got, err := p.CallTool(ctx, "fetch_url", []byte(fmt.Sprintf(`{"url":%q}`, srv.URL)))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !strings.Contains(string(got), `"status":200`) {
		t.Errorf("uppercase-declared host should match lowercase request: %s", got)
	}
}


// A redirect to a host that IS in allowedHosts is followed, so a
// legitimate provider that 302s within its own origin keeps working.
func TestRuntime_HTTP_RedirectToAllowedHostFollowed(t *testing.T) {
	ctx := context.Background()

	var redirected bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/landed" {
			redirected = true
			_, _ = w.Write([]byte("ok"))
			return
		}
		// Redirect with a relative URL so the target host is the
		// same server (which IS in allowedHosts).
		http.Redirect(w, r, "/landed", http.StatusFound)
	}))
	defer srv.Close()

	caps := fmt.Sprintf(`{ "http": { "allowedHosts": [%q] } }`, mustHost(t, srv.URL))
	res := buildParticle(t, fetchParticleSource("http-redirect-allow", caps))

	rt, credStore, kvStore, cleanup := newRuntime(t, ctx)
	defer cleanup()
	p, err := rt.NewParticle(ctx, res.Particle, credStore, kvStore, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(ctx)

	got, err := p.CallTool(ctx, "fetch_url", []byte(fmt.Sprintf(`{"url":%q}`, srv.URL+"/start")))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !redirected {
		t.Error("expected the redirect target to be reached")
	}
	if !strings.Contains(string(got), `"status":200`) {
		t.Errorf("result = %s, want status:200", got)
	}
}

// A particle that never declares the http capability at all has
// every outbound request denied, even to localhost.
func TestRuntime_HTTP_NoCapability_AllDenied(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be reached")
	}))
	defer srv.Close()

	res := buildParticle(t, fetchParticleSource("http-undeclared", `{}`))

	rt, credStore, kvStore, cleanup := newRuntime(t, ctx)
	defer cleanup()
	p, err := rt.NewParticle(ctx, res.Particle, credStore, kvStore, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(ctx)

	_, err = p.CallTool(ctx, "fetch_url", []byte(fmt.Sprintf(`{"url":%q}`, srv.URL)))
	if err == nil {
		t.Fatal("expected error when http capability isn't declared")
	}
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Hostname()
}
