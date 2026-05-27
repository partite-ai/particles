package runtime

import (
	"io"
	"io/fs"
	"testing"
)

// TestPythonStdlibFS_Opens spot-checks the embedded CPython stdlib
// zip: file count is in the right ballpark, a known module
// (`os.pyc`) is readable, and a known package dir (`encodings`) is
// walkable. The zip ships bytecode-only (no .py) — see the
// `python-stdlib-zip` Makefile target — so the checks look for
// `.pyc` files, not source.
func TestPythonStdlibFS_Opens(t *testing.T) {
	zfs, err := pythonStdlibFS()
	if err != nil {
		t.Fatalf("pythonStdlibFS: %v", err)
	}

	var count int
	var sawOs, sawEncodings, sawAnyPy bool
	err = fs.WalkDir(zfs, ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		count++
		switch p {
		case "os.pyc":
			sawOs = true
		case "encodings":
			sawEncodings = true
		}
		if !d.IsDir() && len(p) > 3 && p[len(p)-3:] == ".py" {
			sawAnyPy = true
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
		t.Error("os.pyc missing from zip")
	}
	if !sawEncodings {
		t.Error("encodings/ dir missing from zip")
	}
	if sawAnyPy {
		t.Error("zip contains *.py — bytecode-only build was expected (see Makefile python-stdlib-zip)")
	}

	// Read a known bytecode file end-to-end. We don't try to
	// interpret it — just verify it survived go:embed + the zip
	// pack with a plausible CPython magic-number header.
	f, err := zfs.Open("encodings/__init__.pyc")
	if err != nil {
		t.Fatalf("open encodings/__init__.pyc: %v", err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read encodings/__init__.pyc: %v", err)
	}
	if len(b) < 16 {
		t.Fatalf("encodings/__init__.pyc too small: %d bytes", len(b))
	}
	// Bytecode header is 16 bytes: 4-byte magic, 4-byte flags,
	// 8-byte source-info (mtime + size, or hash). The magic's
	// trailing two bytes are always 0x0d 0x0a for any CPython
	// version — an old "\r\n" sentinel.
	if b[2] != 0x0d || b[3] != 0x0a {
		t.Errorf("encodings/__init__.pyc: missing \\r\\n magic-number sentinel at offset 2-3, got 0x%02x 0x%02x", b[2], b[3])
	}
}
