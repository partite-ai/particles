package wacogo_test

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
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

// Compiled-only package that the particle wheels index doesn't ship a
// cross-build for: resolver must surface no-pure-python-wheel.
//
// Picking a stable witness for this is awkward — every "PyPI publishes
// only compiled wheels" package is potentially also published in the
// particle wheels index, which would make the test fail. lxml is the
// long-standing choice: heavyweight C-extension package nobody has
// asked us to cross-build, and unlikely to appear in the particle
// index for the foreseeable future. If the particle index starts
// publishing lxml, swap the witness here.
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

	_, err = c.PipResolveAndFetch(ctx, []string{"lxml"}, "3.12")
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

// PARTICLES_PY_WHEEL_DIR with a package PyPI has never heard of: the
// resolver must find it in the local dir, read its deps from the
// wheel's own METADATA, and return the local bytes. This is the
// headline of the feature — private/local-only wheels resolve without
// being published anywhere. (Still gated under -short: the resolver
// makes a tolerated PyPI lookup for the name, which 404s, before
// falling back to local.)
func TestPipResolve_LocalWheelDir_LocalOnlyPackage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short (resolver makes a live PyPI lookup before local fallback)")
	}

	dir := t.TempDir()
	marker := []byte("PARTICLE-LOCAL-WHEEL-MARKER")
	// A name nothing on PyPI uses; dist part is underscore-escaped per
	// PEP 427, normalizing back to the hyphenated requirement name.
	writeTestWheel(t, dir, "particle_localtest_pkg-9.9.9-py3-none-any.whl",
		"Metadata-Version: 2.1\nName: particle-localtest-pkg\nVersion: 9.9.9\n", marker)
	t.Setenv("PARTICLES_PY_WHEEL_DIR", dir)

	ctx := context.Background()
	c, err := wacogo.New(ctx)
	if err != nil {
		t.Fatalf("wacogo.New: %v", err)
	}
	defer c.Close(ctx)

	res, err := c.PipResolveAndFetch(ctx, []string{"particle-localtest-pkg"}, "3.12")
	if err != nil {
		t.Fatalf("PipResolveAndFetch: %v\nstderr: %s", err, stderr(res))
	}
	if len(res.Wheels) != 1 {
		t.Fatalf("got %d wheels, want 1 (no deps): %v", len(res.Wheels), sortedNames(res.Wheels))
	}
	w := res.Wheels[0]
	if w.Name != "particle-localtest-pkg" {
		t.Errorf("name = %q, want particle-localtest-pkg", w.Name)
	}
	if w.Version != "9.9.9" {
		t.Errorf("version = %q, want 9.9.9", w.Version)
	}
	if !bytes.Contains(w.WheelBytes, marker) {
		t.Errorf("wheel bytes don't contain the local marker — served the wrong wheel")
	}
	if !strings.HasPrefix(w.Sha256, "sha256:") {
		t.Errorf("sha256 = %q, want sha256:... prefix (computed over local bytes)", w.Sha256)
	}
}

// A local wheel must win over PyPI even when PyPI ships a newer
// version: dropping idna 2.0 into the dir and requiring an unconstrained
// `idna` must resolve to the local 2.0 (not PyPI's latest), and the
// returned bytes must be ours, not PyPI's real idna 2.0.
func TestPipResolve_LocalWheelDir_PreferredOverPyPI(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short (live PyPI access for idna's release index)")
	}

	dir := t.TempDir()
	marker := []byte("PARTICLE-LOCAL-IDNA-MARKER")
	writeTestWheel(t, dir, "idna-2.0-py3-none-any.whl",
		"Metadata-Version: 2.1\nName: idna\nVersion: 2.0\n", marker)
	t.Setenv("PARTICLES_PY_WHEEL_DIR", dir)

	ctx := context.Background()
	c, err := wacogo.New(ctx)
	if err != nil {
		t.Fatalf("wacogo.New: %v", err)
	}
	defer c.Close(ctx)

	res, err := c.PipResolveAndFetch(ctx, []string{"idna"}, "3.12")
	if err != nil {
		t.Fatalf("PipResolveAndFetch: %v\nstderr: %s", err, stderr(res))
	}
	if len(res.Wheels) != 1 {
		t.Fatalf("got %d wheels, want 1: %v", len(res.Wheels), sortedNames(res.Wheels))
	}
	w := res.Wheels[0]
	if w.Version != "2.0" {
		t.Errorf("version = %q, want 2.0 (local should win over PyPI's newer)", w.Version)
	}
	if !bytes.Contains(w.WheelBytes, marker) {
		t.Errorf("served PyPI's idna 2.0, not the local wheel (marker missing)")
	}
}

// writeTestWheel writes a minimal valid wheel (a zip carrying a
// dist-info/METADATA plus a marker file so the bytes are identifiable)
// into dir. distInfo is derived from the filename's name-version stem.
func writeTestWheel(t *testing.T, dir, filename, metadata string, marker []byte) {
	t.Helper()
	stem := strings.TrimSuffix(filename, ".whl")
	parts := strings.Split(stem, "-")
	if len(parts) < 2 {
		t.Fatalf("bad test wheel filename %q", filename)
	}
	distInfo := parts[0] + "-" + parts[1] + ".dist-info"

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range map[string][]byte{
		distInfo + "/METADATA": []byte(metadata),
		distInfo + "/RECORD":   {}, // present-but-empty is fine for our reader
		"_marker":              marker,
	} {
		// Store (uncompressed) so the marker bytes appear verbatim in
		// the raw .whl — lets the test confirm the returned bytes are
		// this local wheel, not a same-name/version one from PyPI.
		f, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := f.Write(content); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, filename), buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write wheel: %v", err)
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
