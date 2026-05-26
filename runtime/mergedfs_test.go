package runtime

import (
	"io"
	"io/fs"
	"sort"
	"testing"
	"testing/fstest"
)

// TestMountedFS_Routes verifies the mount-prefix routing: a path inside
// a mount delegates to that mount's backing FS, with the prefix stripped.
func TestMountedFS_Routes(t *testing.T) {
	stdlib := fstest.MapFS{
		"encodings/__init__.py": {Data: []byte("stdlib-encodings")},
		"json/__init__.py":      {Data: []byte("stdlib-json")},
	}
	bundle := fstest.MapFS{
		"bundle.py": {Data: []byte("user-bundle")},
	}
	m := newMountedFS(map[string]fs.FS{
		"usr/local/lib/python3.14": stdlib,
		"particle":                 bundle,
	})

	cases := []struct {
		path string
		want string
	}{
		{"usr/local/lib/python3.14/encodings/__init__.py", "stdlib-encodings"},
		{"usr/local/lib/python3.14/json/__init__.py", "stdlib-json"},
		{"particle/bundle.py", "user-bundle"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			f, err := m.Open(tc.path)
			if err != nil {
				t.Fatalf("Open(%q): %v", tc.path, err)
			}
			b, err := io.ReadAll(f)
			f.Close()
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(b) != tc.want {
				t.Errorf("got %q, want %q", string(b), tc.want)
			}
		})
	}
}

// TestMountedFS_SyntheticDirs verifies that path components ABOVE a
// mount are listable as directories — Python's site-packages probe
// stat's `/usr`, `/usr/local`, etc. on its way down to the stdlib.
func TestMountedFS_SyntheticDirs(t *testing.T) {
	m := newMountedFS(map[string]fs.FS{
		"usr/local/lib/python3.14": fstest.MapFS{"encodings/__init__.py": {Data: []byte("x")}},
		"particle":                 fstest.MapFS{"bundle.py": {Data: []byte("y")}},
	})

	cases := []struct {
		path    string
		entries []string
	}{
		{".", []string{"particle", "usr"}},
		{"usr", []string{"local"}},
		{"usr/local", []string{"lib"}},
		{"usr/local/lib", []string{"python3.14"}},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			dir, err := m.Open(tc.path)
			if err != nil {
				t.Fatalf("Open(%q): %v", tc.path, err)
			}
			defer dir.Close()
			rdr, ok := dir.(fs.ReadDirFile)
			if !ok {
				t.Fatalf("%q: not a ReadDirFile (%T)", tc.path, dir)
			}
			entries, err := rdr.ReadDir(-1)
			if err != nil {
				t.Fatalf("ReadDir: %v", err)
			}
			got := make([]string, 0, len(entries))
			for _, e := range entries {
				got = append(got, e.Name())
			}
			sort.Strings(got)
			if !equalStrings(got, tc.entries) {
				t.Errorf("got %v, want %v", got, tc.entries)
			}
		})
	}
}

// TestMountedFS_NotFound verifies missing paths produce fs.ErrNotExist.
func TestMountedFS_NotFound(t *testing.T) {
	m := newMountedFS(map[string]fs.FS{
		"particle": fstest.MapFS{"bundle.py": {Data: []byte("y")}},
	})
	_, err := m.Open("particle/missing.py")
	if err == nil {
		t.Fatal("expected error for missing inner path")
	}
	_, err = m.Open("nowhere/at/all")
	if err == nil {
		t.Fatal("expected error for path outside any mount")
	}
}

func equalStrings(a, b []string) bool {
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
