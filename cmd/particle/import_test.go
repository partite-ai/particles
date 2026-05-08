package main

import (
	"archive/tar"
	"bytes"
	"io/fs"
	"reflect"
	"sort"
	"testing"
)

// readTar should round-trip whatever writeParticleTar produces:
// every regular file in, same paths and bytes out. We synthesize
// an in-memory tarball mirroring the build CLI's deterministic
// USTAR shape.
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

	got, err := readTar(&buf)
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

	got, err := readTar(&buf)
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
