package main

import (
	"archive/zip"
	"bytes"
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// readZip should round-trip whatever writeParticleZip produces:
// every regular file in, same paths and bytes out.
func TestReadZip_RoundTripsRegularFiles(t *testing.T) {
	files := map[string][]byte{
		"manifest.json": []byte(`{"name":"p","version":"0.1.0"}`),
		"bundle.mjs":     []byte(`export default {};`),
	}

	got, err := readZipBytes(t, zipBytes(t, files))
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

// readZip rejects entries with absolute paths, traversal segments,
// or names that don't survive path.Clean unchanged. These are
// either packing bugs or hostile probes; the parser is the
// chokepoint so any future "extract to disk" feature inherits the
// guard for free.
func TestReadZip_RejectsHostileNames(t *testing.T) {
	cases := []struct {
		name string
		want string // substring expected in the error
	}{
		{"/etc/passwd", "absolute path"},
		{"../escape", "traversal"},
		{"foo/../bar", "non-canonical"},
		{"./manifest.json", "non-canonical"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			zw := zip.NewWriter(&buf)
			w, err := zw.Create(c.name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write([]byte("payload")); err != nil {
				t.Fatal(err)
			}
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}

			_, err = readZipBytes(t, buf.Bytes())
			if err == nil {
				t.Fatalf("readZip accepted hostile name %q", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want substring %q", err, c.want)
			}
		})
	}
}

// Directory entries (paths ending in "/") are skipped — particle
// archives carry only regular files, and the readZip walk only
// surfaces leaf entries to the FS.
func TestReadZip_SkipsDirectoryEntries(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	// A directory entry — must NOT show up in the FS.
	if _, err := zw.Create("subdir/"); err != nil {
		t.Fatal(err)
	}
	// A regular file.
	w, err := zw.Create("good.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := readZipBytes(t, buf.Bytes())
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
	if _, err := fs.ReadFile(got, "subdir"); err == nil {
		t.Error("directory entry should NOT have been imported")
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

// zipBytes packs a synthetic zip archive of name→body pairs.
func zipBytes(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// readZipBytes is a tiny adapter that runs readZip against an
// in-memory archive — saves every test from re-doing the
// bytes.NewReader + zip.NewReader dance.
func readZipBytes(t *testing.T, raw []byte) (fs.FS, error) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, err
	}
	return readZip(zr)
}

// loadParticleFromHTTP fetches a served archive and returns an FS
// whose contents match the bytes the server sent.
func TestLoadParticleFromHTTP_Success(t *testing.T) {
	files := map[string][]byte{
		"manifest.json": []byte(`{"name":"p","version":"0.1.0"}`),
		"bundle.mjs":     []byte(`export default {};`),
	}
	body := zipBytes(t, files)
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

	// Build a real (but slightly oversized) archive so the
	// failure is the size limit, not a parse error before we hit
	// it. The zip header + body is well past 32 bytes.
	body := zipBytes(t, map[string][]byte{
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
// surface a clear diagnostic — never a raw zip-decode error.
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
	if !strings.Contains(err.Error(), "not a particle archive") {
		t.Errorf("err = %v, want clear non-archive message", err)
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
