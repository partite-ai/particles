package runtime_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/partite-ai/particles/internal/osmount"
)

// TestMounts_EndToEnd drives a real Python guest against two host-backed
// mounts — one read-write, one read-only — to validate the whole path:
// preopen wiring → trackingFS (Unwrap-resolved capabilities) → readOnlyFS
// enforcement → osmount → host disk. It proves a guest write lands on the
// host through a read-write mount, a guest read works through a read-only
// mount, and a guest write is refused on the read-only mount.
func TestMounts_EndToEnd(t *testing.T) {
	ctx := context.Background()

	const manifest = `{
		"name": "py-mounts",
		"description": "mount e2e",
		"version": "0.1.0",
		"runtime": "python",
		"capabilities": {
			"filesystem": {
				"mounts": {
					"data": {"description": "rw data", "path": "/mnt/data", "access": "readwrite", "required": true},
					"conf": {"description": "ro conf", "path": "/mnt/conf", "access": "readonly", "required": true}
				}
			}
		},
		"tools": [
			{"name": "run", "description": "exercise mounts", "inputSchema": {"type": "object", "properties": {}}}
		]
	}`

	// The runtime reads capabilities from manifest.json (above); the
	// bundle only needs the handler.
	const bundle = `from particle.manifest import Particle, Tool

def _run(args):
    with open("/mnt/data/out.txt", "w") as f:
        f.write("hello")
    with open("/mnt/data/out.txt") as f:
        data = f.read()
    with open("/mnt/conf/cfg.txt") as f:
        conf = f.read()
    ro_write_failed = False
    try:
        with open("/mnt/conf/blocked.txt", "w") as f:
            f.write("x")
    except OSError:
        ro_write_failed = True
    return {"data": data, "conf": conf, "ro_write_failed": ro_write_failed}

particle = Particle(
    name="py-mounts",
    description="mount e2e",
    version="0.1.0",
    tools={
        "run": Tool(
            description="exercise mounts",
            input_schema={"type": "object", "properties": {}},
            handler=_run,
        ),
    },
)
`

	dataDir := t.TempDir()
	confDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(confDir, "cfg.txt"), []byte("config!"), 0o644); err != nil {
		t.Fatal(err)
	}

	dataFS, err := osmount.New(dataDir)
	if err != nil {
		t.Fatalf("osmount.New(data): %v", err)
	}
	defer dataFS.Close()
	confFS, err := osmount.New(confDir)
	if err != nil {
		t.Fatalf("osmount.New(conf): %v", err)
	}
	defer confFS.Close()

	rt, credStore, kvStore, cleanup := newRuntime(t, ctx)
	defer cleanup()

	p, err := rt.NewParticle(ctx, pythonParticleFS(manifest, bundle), credStore, kvStore,
		map[string]fs.FS{"data": dataFS, "conf": confFS})
	if err != nil {
		t.Fatalf("NewParticle: %v", err)
	}
	defer p.Close(ctx)

	out, err := p.CallTool(ctx, "run", []byte(`{}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	var got struct {
		Data          string `json:"data"`
		Conf          string `json:"conf"`
		ROWriteFailed bool   `json:"ro_write_failed"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode result %s: %v", out, err)
	}

	if got.Data != "hello" {
		t.Errorf("read-back of rw mount = %q, want hello", got.Data)
	}
	if got.Conf != "config!" {
		t.Errorf("read of ro mount = %q, want config!", got.Conf)
	}
	if !got.ROWriteFailed {
		t.Error("write to read-only mount should have failed in the guest")
	}

	// The rw write reached the host directory.
	if b, err := os.ReadFile(filepath.Join(dataDir, "out.txt")); err != nil || string(b) != "hello" {
		t.Errorf("host rw file = %q, %v; want hello", b, err)
	}
	// The ro write never created a host file.
	if _, err := os.Stat(filepath.Join(confDir, "blocked.txt")); !os.IsNotExist(err) {
		t.Errorf("read-only mount let a file through: stat err = %v", err)
	}
}
