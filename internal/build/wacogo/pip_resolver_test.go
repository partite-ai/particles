package wacogo_test

import (
	"context"
	"strings"
	"testing"

	"github.com/partite-ai/particles/internal/build/wacogo"
)

// Live smoke test: hits PyPI's real JSON API to resolve a small,
// stable pure-Python package. Skipped under `go test -short` so a
// network-air-gapped CI doesn't hang.
//
// `idna` is a textbook pure-Python package — six versions back are
// all published as `py3-none-any.whl`, no deps, ~50KB.
func TestPipResolve_Live_Idna(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short (live PyPI access)")
	}

	ctx := context.Background()
	c, err := wacogo.New(ctx)
	if err != nil {
		t.Fatalf("wacogo.New: %v", err)
	}
	defer c.Close(ctx)

	res, err := c.PipResolveAndFetch(ctx, []string{"idna>=3"}, "3.12")
	if err != nil {
		t.Fatalf("PipResolveAndFetch: %v\nstderr: %s", err, stderr(res))
	}
	if len(res.Wheels) != 1 {
		t.Fatalf("got %d wheels, want 1 (idna has no runtime deps)", len(res.Wheels))
	}
	w := res.Wheels[0]
	if w.Name != "idna" {
		t.Errorf("name = %q, want idna", w.Name)
	}
	if !strings.HasPrefix(w.Sha256, "sha256:") {
		t.Errorf("sha256 = %q, want sha256:... prefix", w.Sha256)
	}
	if !strings.HasSuffix(w.Filename, "-py3-none-any.whl") {
		t.Errorf("filename = %q, want a py3-none-any wheel", w.Filename)
	}
	if len(w.WheelBytes) < 1024 {
		t.Errorf("wheel bytes = %d, suspiciously small", len(w.WheelBytes))
	}
	// Wheels are zip files — sanity check the magic bytes so we
	// know we didn't accidentally cache an HTML error page.
	if string(w.WheelBytes[:2]) != "PK" {
		t.Errorf("wheel bytes don't start with zip magic 'PK': % x", w.WheelBytes[:8])
	}
}

// Transitive deps: `httpx` pulls in `httpcore`, `anyio`, `idna`,
// `certifi`, `sniffio`. All pure-Python. Confirms the BFS walks
// transitives correctly and the result is more than one wheel.
func TestPipResolve_Live_Httpx_Transitives(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short (live PyPI access)")
	}

	ctx := context.Background()
	c, err := wacogo.New(ctx)
	if err != nil {
		t.Fatalf("wacogo.New: %v", err)
	}
	defer c.Close(ctx)

	res, err := c.PipResolveAndFetch(ctx, []string{"httpx>=0.27"}, "3.12")
	if err != nil {
		t.Fatalf("PipResolveAndFetch: %v\nstderr: %s", err, stderr(res))
	}
	// httpx 0.27+ direct + transitive: httpx, httpcore, anyio,
	// idna, certifi, sniffio, h11. Loose-bound check — exact set
	// can shift between releases.
	if len(res.Wheels) < 5 {
		t.Errorf("got %d wheels, want >= 5 (httpx + transitives)", len(res.Wheels))
	}
	names := map[string]bool{}
	for _, w := range res.Wheels {
		names[w.Name] = true
	}
	for _, want := range []string{"httpx", "httpcore", "idna", "certifi"} {
		if !names[want] {
			t.Errorf("transitive %q missing from result: got %v", want, sortedNames(res.Wheels))
		}
	}
}

// Compiled-only package: pyyaml has never published a pure-Python
// wheel. The resolver must surface no-pure-python-wheel rather than
// silently picking a platform wheel that the runtime can't load.
func TestPipResolve_Live_RejectsCompiledOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short (live PyPI access)")
	}

	ctx := context.Background()
	c, err := wacogo.New(ctx)
	if err != nil {
		t.Fatalf("wacogo.New: %v", err)
	}
	defer c.Close(ctx)

	_, err = c.PipResolveAndFetch(ctx, []string{"pyyaml"}, "3.12")
	if err == nil {
		t.Fatal("expected no-pure-python-wheel error, got nil")
	}
	if !strings.Contains(err.Error(), "no-pure-python-wheel") &&
		!strings.Contains(err.Error(), "pure") {
		t.Errorf("err = %v, want a no-pure-python-wheel-ish error", err)
	}
}

// Pubgrub backtracking exercise. `requests` and `httpx` both depend
// on `idna`, with overlapping but distinct version ranges. Resolution
// must pick a single `idna` version that satisfies both — a case the
// previous greedy BFS couldn't reliably handle, but pubgrub solves
// natively. We don't assert the exact idna version (it shifts across
// PyPI releases); we assert that one *is* picked and the resolver
// returns a coherent closure.
func TestPipResolve_Live_PubgrubBacktrack(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short (live PyPI access)")
	}

	ctx := context.Background()
	c, err := wacogo.New(ctx)
	if err != nil {
		t.Fatalf("wacogo.New: %v", err)
	}
	defer c.Close(ctx)

	// Two top-level packages whose transitive closures intersect
	// at idna (and also at anyio + sniffio). If pubgrub picks
	// per-package without unifying, we'd see idna appear twice or
	// the resolution would fail.
	res, err := c.PipResolveAndFetch(ctx, []string{"httpx>=0.27", "requests>=2.31"}, "3.12")
	if err != nil {
		t.Fatalf("PipResolveAndFetch: %v\nstderr: %s", err, stderr(res))
	}

	count := map[string]int{}
	for _, w := range res.Wheels {
		count[w.Name]++
	}
	// Each package — including the shared idna — appears exactly
	// once.
	for _, want := range []string{"httpx", "requests", "idna", "certifi"} {
		if count[want] != 1 {
			t.Errorf("expected %q exactly once, got %d (wheels: %v)",
				want, count[want], sortedNames(res.Wheels))
		}
	}
}

func stderr(res *wacogo.PipResolveResult) string {
	if res == nil {
		return ""
	}
	return string(res.Stderr)
}

func sortedNames(wheels []wacogo.PipResolvedWheel) []string {
	out := make([]string, len(wheels))
	for i, w := range wheels {
		out[i] = w.Name
	}
	return out
}
