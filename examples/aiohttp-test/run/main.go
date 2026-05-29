// run/main.go — drives the async wasi:http smoke test particle
// without going through the keyring-using CLI. Builds the
// Particlefile.py in the parent dir, instantiates it under the
// python runtime with ephemeral in-memory credential + kv stores
// (no keyring / dbus required), and calls the requested tool.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/partite-ai/wacogo"

	"github.com/partite-ai/particles/credentials"
	credmem "github.com/partite-ai/particles/credentials/memory"
	"github.com/partite-ai/particles/internal/build"
	"github.com/partite-ai/particles/kv"
	kvmem "github.com/partite-ai/particles/kv/memory"
	"github.com/partite-ai/particles/runtime"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("FAIL: %v", err)
	}
}

func run() error {
	ctx := context.Background()

	srcDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}
	if _, err := os.Stat(filepath.Join(srcDir, "Particlefile.py")); err != nil {
		return fmt.Errorf("no Particlefile.py in %s", srcDir)
	}

	res, err := build.Build(ctx, build.Options{
		Source:      os.DirFS(srcDir),
		NoTypeCheck: true,
	})
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}
	fmt.Println("build: ok")

	engine := wacogo.NewEngine(ctx)
	defer engine.Close(ctx)
	credMgr, err := credentials.NewManager(ctx, credentials.ManagerConfig{Engine: engine})
	if err != nil {
		return fmt.Errorf("credentials manager: %w", err)
	}
	defer credMgr.Close(ctx)
	kvMgr, err := kv.NewManager(ctx, kv.ManagerConfig{Engine: engine})
	if err != nil {
		return fmt.Errorf("kv manager: %w", err)
	}
	defer kvMgr.Close(ctx)

	rt, err := runtime.New(ctx, runtime.Config{
		Engine: engine, Credentials: credMgr, KV: kvMgr,
	})
	if err != nil {
		return fmt.Errorf("runtime.New: %w", err)
	}
	defer rt.Close(ctx)

	credStore := credmem.New().Scoped("aiohttp-test")
	kvStore := kvmem.New().Scoped("aiohttp-test")
	p, err := rt.NewParticle(ctx, res.Particle, credStore, kvStore, nil)
	if err != nil {
		return fmt.Errorf("NewParticle: %w", err)
	}
	defer p.Close(ctx)

	tool := "one"
	args := `{}`
	if len(os.Args) >= 2 {
		tool = os.Args[1]
	}
	if len(os.Args) >= 3 {
		args = os.Args[2]
	}
	fmt.Printf("calling: %s %s\n", tool, args)
	out, err := p.CallTool(ctx, tool, []byte(args))
	if err != nil {
		return fmt.Errorf("CallTool %s: %w", tool, err)
	}
	fmt.Printf("%s: %s\n", tool, out)
	return nil
}
