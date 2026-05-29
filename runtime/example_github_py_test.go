package runtime_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/partite-ai/particles/internal/build"
	"github.com/partite-ai/particles/runtime"
)

// End-to-end check on examples/github-py/Particlefile.py.
//
// Builds it through the real pipeline (PEP 723 → no deps → introspect
// → assemble), instantiates the python runtime, and exercises the
// pieces that don't require live GitHub auth:
//
//   - ListTools — the three tools the example declares
//   - Manifest — credentials declaration matches the JS sibling
//   - Ping returns "unhealthy" with the "no credential configured"
//     message (we don't configure credentials in this test)
//
// What it deliberately does not test:
//   - Tool calls that hit api.github.com — those need a configured
//     credential (OAuth or PAT). Covered by the user's setup flow,
//     not by this regression test.
//   - The JS sibling's tool wiring — already covered by the
//     existing JS runtime tests.
func TestExample_GithubPy(t *testing.T) {
	srcPath := "../examples/github-py/Particlefile.py"
	if _, err := os.Stat(srcPath); err != nil {
		t.Skipf("example missing at %s: %v", srcPath, err)
	}

	// The build pipeline takes an fs.FS rooted at the source dir.
	// Use os.DirFS so the test reads the real file rather than
	// duplicating its contents inline.
	src := os.DirFS(filepath.Dir(srcPath))

	res, err := build.Build(context.Background(), build.Options{
		Source:      src,
		NoTypeCheck: true,
	})
	if err != nil {
		t.Fatalf("build.Build: %v", err)
	}

	t.Run("manifest", func(t *testing.T) {
		mfBytes, err := fs.ReadFile(res.Particle, "manifest.json")
		if err != nil {
			t.Fatalf("read manifest: %v", err)
		}
		var mf struct {
			Name        string `json:"name"`
			Runtime     string `json:"runtime"`
			Credentials map[string]struct {
				Required bool                   `json:"required"`
				Methods  map[string]interface{} `json:"methods"`
			} `json:"credentials"`
			Capabilities map[string]interface{} `json:"capabilities"`
		}
		if err := json.Unmarshal(mfBytes, &mf); err != nil {
			t.Fatalf("parse manifest: %v\n%s", err, mfBytes)
		}
		if mf.Name != "github-tools-py" {
			t.Errorf("name = %q", mf.Name)
		}
		if mf.Runtime != "python" {
			t.Errorf("runtime = %q, want python", mf.Runtime)
		}
		gh, ok := mf.Credentials["github"]
		if !ok {
			t.Fatalf("credentials.github missing")
		}
		if !gh.Required {
			t.Error("credentials.github.required = false, want true")
		}
		if _, hasOAuth := gh.Methods["oauth"]; !hasOAuth {
			t.Error("credentials.github.methods.oauth missing")
		}
		if _, hasPAT := gh.Methods["pat"]; !hasPAT {
			t.Error("credentials.github.methods.pat missing")
		}
		if _, hasHTTP := mf.Capabilities["http"]; !hasHTTP {
			t.Error("capabilities.http missing")
		}
	})

	t.Run("runtime", func(t *testing.T) {
		ctx := context.Background()
		rt, credStore, kvStore, cleanup := newRuntime(t, ctx)
		defer cleanup()

		p, err := rt.NewParticle(ctx, res.Particle, credStore, kvStore, nil)
		if err != nil {
			t.Fatalf("NewParticle: %v", err)
		}
		defer p.Close(ctx)

		tools, err := p.ListTools(ctx)
		if err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		seen := map[string]bool{}
		for _, td := range tools {
			seen[td.Name] = true
		}
		for _, want := range []string{"get_repo", "list_issues", "create_issue"} {
			if !seen[want] {
				t.Errorf("missing tool %q (got %v)", want, seen)
			}
		}

		// Ping without configured credentials → the handler's
		// own "unhealthy" branch fires (not a runtime error).
		pr, err := p.Ping(ctx)
		if err != nil {
			t.Fatalf("Ping: %v", err)
		}
		if pr.Status != runtime.PingStatusUnhealthy {
			t.Errorf("Ping status = %v, want Unhealthy (no creds)", pr.Status)
		}
		if !strings.Contains(pr.Message, "no credential configured") {
			t.Errorf("Ping message = %q, want it to mention no credential", pr.Message)
		}
	})
}
