package runtime_test

import (
	"context"
	"strings"
	"testing"
)

// TestPythonRuntime_HTTPClients locks in the auto-hook surface that
// makes the popular Python HTTP libraries — urllib3, requests, httpx
// — work transparently inside a particle. Each subtest issues a real
// request and pins status 200 + a decoded body, so a regression in
// any of the three (socket guard, urllib3 connection injection, or
// httpx transport injection) surfaces here.
//
// Live network — skipped under -short.
func TestPythonRuntime_HTTPClients(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short (live HTTP)")
	}
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "urllib_request_urlopen",
			src: `
def _probe(args):
    import urllib.request
    with urllib.request.urlopen("https://api.github.com/zen") as r:
        body = r.read()
    return {"status": r.getcode(), "bytes": len(body), "text": body.decode("utf-8", "replace")}
`,
		},
		{
			name: "urllib3_pool",
			src: `# /// script
# requires-python = ">=3.12"
# dependencies = ["urllib3"]
# ///
def _probe(args):
    import urllib3
    pool = urllib3.PoolManager()
    r = pool.request("GET", "https://api.github.com/zen")
    return {"status": r.status, "bytes": len(r.data), "text": r.data.decode("utf-8", "replace")}
`,
		},
		{
			name: "requests_get",
			src: `# /// script
# requires-python = ">=3.12"
# dependencies = ["requests"]
# ///
def _probe(args):
    import requests
    r = requests.get("https://api.github.com/zen")
    return {"status": r.status_code, "bytes": len(r.content), "text": r.text}
`,
		},
		{
			name: "httpx_get",
			src: `# /// script
# requires-python = ">=3.12"
# dependencies = ["httpx"]
# ///
def _probe(args):
    import httpx, traceback
    try:
        r = httpx.get("https://api.github.com/zen")
        return {"status": r.status_code, "bytes": len(r.content), "text": r.text}
    except BaseException as e:
        return {"err": repr(e), "tb": traceback.format_exc()[:2000]}
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bundle := tc.src + `
from particle.manifest import Particle, Tool, Http
particle = Particle(
    name="urllib3-probe",
    description="urllib3 compat probe",
    version="0.1.0",
    http=Http(allowed_hosts=["api.github.com"]),
    tools={"probe": Tool(
        description="probe",
        input_schema={"type":"object"},
        handler=_probe,
    )},
)
`
			res := buildPythonParticle(t, bundle)

			ctx := context.Background()
			rt, credStore, kvStore, cleanup := newRuntime(t, ctx)
			defer cleanup()

			p, err := rt.NewParticle(ctx, res.Particle, credStore, kvStore)
			if err != nil {
				t.Fatalf("NewParticle: %v", err)
			}
			defer p.Close(ctx)

			out, err := p.CallTool(ctx, "probe", []byte(`{}`))
			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			body := string(out)
			if !strings.Contains(body, `"status": 200`) {
				t.Errorf("status not 200 in response: %s", body)
			}
			t.Logf("%s: %s", tc.name, body)
		})
	}
}
