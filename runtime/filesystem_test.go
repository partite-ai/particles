package runtime

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/partite-ai/wacogo/wasi/filesystem/preopens"
)

func TestParseByteSize(t *testing.T) {
	ok := map[string]int64{
		"10000":  10000,
		"10KB":   10 * 1024,
		"1MB":    1024 * 1024,
		"2GB":    2 * 1024 * 1024 * 1024,
		"5B":     5,
		"5b":     5,
		"10kb":   10 * 1024,
		" 16KB ": 16 * 1024,
	}
	for in, want := range ok {
		got, err := ParseByteSize(in)
		if err != nil {
			t.Errorf("ParseByteSize(%q): unexpected error %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseByteSize(%q) = %d, want %d", in, got, want)
		}
	}

	bad := []string{"", "abc", "10TB", "0", "-5", "KB", "1.5MB"}
	for _, in := range bad {
		if _, err := ParseByteSize(in); err == nil {
			t.Errorf("ParseByteSize(%q): want error, got nil", in)
		}
	}
}

const validFSManifest = `{
  "name": "fs-test", "version": "0.1.0",
  "capabilities": {
    "filesystem": {
      "mounts": {
        "data":   {"description": "data dir", "path": "/data", "access": "readwrite", "required": true},
        "config": {"description": "config dir", "path": "/etc/app", "access": "readonly"}
      },
      "temp": {
        "scratch": {"description": "scratch", "path": "/tmp/work", "maxSize": "1MB"}
      }
    }
  },
  "tools": []
}`

func TestParseManifestFilesystem(t *testing.T) {
	m, err := ParseManifest(strings.NewReader(validFSManifest))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	fsCap := m.Capabilities.Filesystem
	if len(fsCap.Mounts) != 2 || len(fsCap.Temp) != 1 {
		t.Fatalf("got %d mounts, %d temp; want 2, 1", len(fsCap.Mounts), len(fsCap.Temp))
	}
	if got := fsCap.Mounts["data"]; got.Path != "/data" || got.Access != MountReadWrite || !got.Required {
		t.Errorf("data mount = %+v", got)
	}
	if got := fsCap.Mounts["config"]; got.Access != MountReadOnly || got.Required {
		t.Errorf("config mount = %+v", got)
	}
	if n, err := fsCap.Temp["scratch"].MaxSizeBytes(); err != nil || n != 1024*1024 {
		t.Errorf("scratch maxSize = %d, %v", n, err)
	}
}

func TestParseManifestFilesystemInvalid(t *testing.T) {
	cases := map[string]string{
		"missing description": `{"description":"","path":"/d","access":"readwrite"}`,
		"missing path":        `{"description":"d","access":"readwrite"}`,
		"relative path":       `{"description":"d","path":"d","access":"readwrite"}`,
		"root path":           `{"description":"d","path":"/","access":"readwrite"}`,
		"unclean path":        `{"description":"d","path":"/a/../b","access":"readwrite"}`,
		"bad access":          `{"description":"d","path":"/d","access":"rw"}`,
		"missing access":      `{"description":"d","path":"/d"}`,
	}
	for name, mountJSON := range cases {
		mf := `{"name":"x","version":"0.1.0","capabilities":{"filesystem":{"mounts":{"m":` + mountJSON + `}}},"tools":[]}`
		if _, err := ParseManifest(strings.NewReader(mf)); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}

	// Temp-specific and cross-cutting cases.
	other := map[string]string{
		"temp missing maxSize": `{"name":"x","version":"0.1.0","capabilities":{"filesystem":{"temp":{"t":{"description":"d","path":"/t"}}}},"tools":[]}`,
		"temp bad maxSize":     `{"name":"x","version":"0.1.0","capabilities":{"filesystem":{"temp":{"t":{"description":"d","path":"/t","maxSize":"big"}}}},"tools":[]}`,
		"duplicate path":       `{"name":"x","version":"0.1.0","capabilities":{"filesystem":{"mounts":{"a":{"description":"d","path":"/x","access":"readonly"},"b":{"description":"d","path":"/x","access":"readonly"}}}},"tools":[]}`,
		"name in both":         `{"name":"x","version":"0.1.0","capabilities":{"filesystem":{"mounts":{"m":{"description":"d","path":"/m","access":"readonly"}},"temp":{"m":{"description":"d","path":"/t","maxSize":"1MB"}}}},"tools":[]}`,
		"bad mount name":       `{"name":"x","version":"0.1.0","capabilities":{"filesystem":{"mounts":{"a=b":{"description":"d","path":"/m","access":"readonly"}}}},"tools":[]}`,
	}
	for name, mf := range other {
		if _, err := ParseManifest(strings.NewReader(mf)); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}
}

func TestBuildMountPreopens(t *testing.T) {
	m, err := ParseManifest(strings.NewReader(validFSManifest))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	// Pointer FS values so identity comparison (== ) is well-defined
	// — fstest.MapFS is a map and panics under ==.
	dataFS := &fstest.MapFS{}
	configFS := &fstest.MapFS{}
	tempFS := &fstest.MapFS{}

	t.Run("happy path", func(t *testing.T) {
		entries, err := buildMountPreopens(m, map[string]fs.FS{
			"data":    dataFS,
			"config":  configFS,
			"scratch": tempFS,
		})
		if err != nil {
			t.Fatalf("buildMountPreopens: %v", err)
		}
		byPath := map[string]*preopens.PreopenEntry{}
		for _, e := range entries {
			byPath[e.Path] = e
		}
		// A read-write mount exposes the provided FS directly.
		if got := byPath["/data"]; got == nil {
			t.Fatalf("/data missing")
		} else if got.FS != fs.FS(dataFS) {
			t.Errorf("/data should be the provided FS, got %T", got.FS)
		}
		// A read-only mount is a readOnlyFS over the provided FS.
		if got := byPath["/etc/app"]; got == nil {
			t.Fatalf("/etc/app missing")
		} else if ro, ok := got.FS.(readOnlyFS); !ok || ro.fsys != fs.FS(configFS) {
			t.Errorf("/etc/app should be a readOnlyFS over the provided FS, got %T", got.FS)
		}
		// temp exposes the provided FS directly.
		if got := byPath["/tmp/work"]; got == nil {
			t.Fatalf("/tmp/work missing")
		} else if got.FS != fs.FS(tempFS) {
			t.Errorf("/tmp/work should be the provided FS, got %T", got.FS)
		}
	})

	t.Run("undeclared rejected", func(t *testing.T) {
		_, err := buildMountPreopens(m, map[string]fs.FS{"data": dataFS, "scratch": tempFS, "bogus": dataFS})
		if err == nil {
			t.Fatal("want error for undeclared mount")
		}
	})

	t.Run("required missing", func(t *testing.T) {
		_, err := buildMountPreopens(m, map[string]fs.FS{"scratch": tempFS})
		if err == nil {
			t.Fatal("want error for missing required mount")
		}
	})

	t.Run("temp missing", func(t *testing.T) {
		_, err := buildMountPreopens(m, map[string]fs.FS{"data": dataFS})
		if err == nil {
			t.Fatal("want error for missing temp mount")
		}
	})

	t.Run("optional omitted is skipped", func(t *testing.T) {
		entries, err := buildMountPreopens(m, map[string]fs.FS{"data": dataFS, "scratch": tempFS})
		if err != nil {
			t.Fatalf("buildMountPreopens: %v", err)
		}
		for _, e := range entries {
			if e.Path == "/etc/app" {
				t.Error("optional /etc/app should be omitted when not provided")
			}
		}
	})
}
