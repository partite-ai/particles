package runtime

import (
	"io"
	"io/fs"
	"testing"
)

// TestPythonStdlibFS_Opens spot-checks the embedded CPython stdlib
// zip: file count is in the right ballpark, a known module (`os.py`)
// is readable, and a known dir (`encodings`) is walkable. Confirms
// the go:embed + zip.NewReader plumbing works end-to-end.
func TestPythonStdlibFS_Opens(t *testing.T) {
	zfs, err := pythonStdlibFS()
	if err != nil {
		t.Fatalf("pythonStdlibFS: %v", err)
	}

	var count int
	var sawOs, sawEncodings bool
	err = fs.WalkDir(zfs, ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		count++
		switch p {
		case "os.py":
			sawOs = true
		case "encodings":
			sawEncodings = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	t.Logf("stdlib zip exposes %d entries", count)
	if count < 100 {
		t.Errorf("stdlib zip looks too small: %d entries", count)
	}
	if !sawOs {
		t.Error("os.py missing from zip")
	}
	if !sawEncodings {
		t.Error("encodings/ dir missing from zip")
	}

	// Read a known file end-to-end.
	f, err := zfs.Open("encodings/__init__.py")
	if err != nil {
		t.Fatalf("open encodings/__init__.py: %v", err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read encodings/__init__.py: %v", err)
	}
	if len(b) < 100 {
		t.Errorf("encodings/__init__.py too small: %d bytes", len(b))
	}
	if want := []byte("codecs"); !contains(b, want) {
		t.Errorf("encodings/__init__.py: expected to mention `codecs`")
	}
}

func contains(haystack, needle []byte) bool {
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i <= len(haystack)-len(needle); i++ {
		if eq(haystack[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}

func eq(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
