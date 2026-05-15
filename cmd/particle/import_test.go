package main

import (
	"archive/tar"
	"bytes"
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// readTar should round-trip whatever writeParticleTar produces:
// every regular file in, same paths and bytes out. We synthesize
// an in-memory archive mirroring the build CLI's deterministic
// zstd-of-tar shape.
func TestReadTar_RoundTripsRegularFiles(t *testing.T) {
	files := map[string][]byte{
		"manifest.json": []byte(`{"name":"p","version":"0.1.0"}`),
		"bundle.js":     []byte(`export default {};`),
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), Format: tar.FormatUSTAR,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := readTar(bytes.NewReader(zstdCompress(t, buf.Bytes())))
	if err != nil {
		t.Fatal(err)
	}
	gotMap := map[string][]byte{}
	if err := fs.WalkDir(got, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(got, p)
		if err != nil {
			return err
		}
		gotMap[p] = data
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotMap, files) {
		t.Errorf("contents mismatch:\nwant keys: %v\ngot keys: %v", keysOf(files), keysOf(gotMap))
	}
}

// readTar rejects entries with absolute paths, traversal segments,
// or names that don't survive path.Clean unchanged. These are
// either packing bugs or hostile probes; the parser is the
// chokepoint so any future "extract to disk" feature inherits the
// guard for free.
func TestReadTar_RejectsHostileNames(t *testing.T) {
	cases := []struct {
		name    string
		want    string // substring expected in the error
	}{
		{"/etc/passwd", "absolute path"},
		{"../escape", "traversal"},
		{"foo/../bar", "non-canonical"},
		{"./manifest.json", "non-canonical"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			body := []byte("payload")
			if err := tw.WriteHeader(&tar.Header{
				Name: c.name, Mode: 0o644, Size: int64(len(body)), Format: tar.FormatPAX,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
			tw.Close()

			_, err := readTar(bytes.NewReader(zstdCompress(t, buf.Bytes())))
			if err == nil {
				t.Fatalf("readTar accepted hostile name %q", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want substring %q", err, c.want)
			}
		})
	}
}

// Symlinks / directories / hardlinks in the archive are skipped —
// particle tarballs only carry regular files, anything else is
// either a packing bug or a security probe.
func TestReadTar_SkipsNonRegularEntries(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// A symlink — must NOT show up in the FS.
	if err := tw.WriteHeader(&tar.Header{
		Name: "evil-link", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink, Mode: 0o644, Format: tar.FormatUSTAR,
	}); err != nil {
		t.Fatal(err)
	}
	// A regular file.
	body := []byte("ok")
	if err := tw.WriteHeader(&tar.Header{
		Name: "good.txt", Mode: 0o644, Size: int64(len(body)), Format: tar.FormatUSTAR,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	tw.Close()

	got, err := readTar(bytes.NewReader(zstdCompress(t, buf.Bytes())))
	if err != nil {
		t.Fatal(err)
	}
	data, err := fs.ReadFile(got, "good.txt")
	if err != nil {
		t.Errorf("good.txt missing: %v", err)
	}
	if string(data) != "ok" {
		t.Errorf("good.txt = %q", data)
	}
	if _, err := fs.ReadFile(got, "evil-link"); err == nil {
		t.Error("symlink entry should NOT have been imported")
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// tarBytes packs a synthetic tar archive of name→body pairs and
// returns the zstd-compressed result. Mirrors `writeParticleTar`'s
// layout closely enough for readTar to round-trip through it.
func tarBytes(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), Format: tar.FormatUSTAR,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return zstdCompress(t, buf.Bytes())
}

// zstdCompress wraps raw bytes in a zstd stream with the same level
// + concurrency settings writeParticleTar uses, so test fixtures
// match the on-disk format readTar expects.
func zstdCompress(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf,
		zstd.WithEncoderLevel(zstd.SpeedBetterCompression),
		zstd.WithEncoderConcurrency(1),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// loadParticleFromHTTP fetches a served tarball and returns an FS
// whose contents match the bytes the server sent.
func TestLoadParticleFromHTTP_Success(t *testing.T) {
	files := map[string][]byte{
		"manifest.json": []byte(`{"name":"p","version":"0.1.0"}`),
		"bundle.js":     []byte(`export default {};`),
	}
	body := tarBytes(t, files)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	fsys, err := loadParticleFromHTTP(context.Background(), srv.URL+"/foo.particle")
	if err != nil {
		t.Fatalf("loadParticleFromHTTP: %v", err)
	}
	for name, want := range files {
		got, err := fs.ReadFile(fsys, name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: got %q want %q", name, got, want)
		}
	}
}

// Non-2xx responses surface as a wrapped error mentioning the
// status — the user shouldn't have to grep the request log to
// figure out why the import failed.
func TestLoadParticleFromHTTP_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such particle", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := loadParticleFromHTTP(context.Background(), srv.URL+"/missing.particle")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want status code in message", err)
	}
}

// A body that exceeds maxParticleDownloadBytes fails with a clear
// "exceeds N bytes" message — never silently truncates. We shrink
// the cap for this test so we don't have to serve 100 MiB of
// padding.
func TestLoadParticleFromHTTP_OversizedDownload(t *testing.T) {
	prev := maxParticleDownloadBytes
	maxParticleDownloadBytes = 32
	defer func() { maxParticleDownloadBytes = prev }()

	// Build a real (but slightly oversized) tarball so the
	// failure is the size limit, not a tar parse error before we
	// hit it. The header + body is well past 32 bytes.
	body := tarBytes(t, map[string][]byte{
		"manifest.json": []byte(`{"name":"p","version":"0.1.0"}`),
	})
	if int64(len(body)) <= maxParticleDownloadBytes {
		t.Fatalf("test setup: body must exceed cap; got %d <= %d", len(body), maxParticleDownloadBytes)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	_, err := loadParticleFromHTTP(context.Background(), srv.URL+"/big.particle")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("err = %v, want one mentioning size cap", err)
	}
}

// An HTML response (captive portal, error page, auth wall) must
// surface a clear diagnostic — never the raw "tar header:
// unexpected EOF" we'd otherwise get from readTar.
func TestLoadParticleFromHTTP_HTMLResponse_ClearError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><body>Captive portal — please log in.</body>"))
	}))
	defer srv.Close()

	_, err := loadParticleFromHTTP(context.Background(), srv.URL+"/p.particle")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not a particle tarball") {
		t.Errorf("err = %v, want clear non-tar message", err)
	}
}

// parseHTTPURL recognises http and https schemes, rejects
// everything else. The detection is what flips the dispatch in
// loadParticle, so it has to be conservative — a typo in a file
// path shouldn't accidentally trigger an HTTP fetch.
func TestParseHTTPURL(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"https://example.com/foo.particle", true},
		{"http://example.com/foo.particle", true},
		{"HTTP://example.com/foo.particle", false}, // case-sensitive on purpose
		{"file:///tmp/foo.particle", false},
		{"./foo.particle", false},
		{"/abs/path/foo.particle", false},
		{"foo.particle", false},
		{"", false},
		{"https://", true}, // url.Parse accepts it; the GET will fail later
	}
	for _, c := range cases {
		_, got := parseHTTPURL(c.in)
		if got != c.want {
			t.Errorf("parseHTTPURL(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
