package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tetratelabs/wazero"
)

// defaultWasmCacheDir returns the OS-standard user cache dir with a
// `particle/wasm` suffix. The platform-specific path layout follows
// [os.UserCacheDir]:
//
//	Linux:   ~/.cache/particle/wasm
//	macOS:   ~/Library/Caches/particle/wasm
//	Windows: %LocalAppData%\particle\wasm
//
// We use the cache dir (not the config dir where state.db lives)
// because wasm-cache entries are derived artifacts — safe to wipe,
// rebuilt on next use. Mixing them with state.db would invite
// "rm -rf ~/.config/particle" from a user trying to free space and
// accidentally nuking their credential / registry state.
func defaultWasmCacheDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate user cache dir: %w", err)
	}
	return filepath.Join(dir, "particle", "wasm"), nil
}

// loadWasmCompilationCache constructs a wazero compilation cache
// rooted at [defaultWasmCacheDir]. wazero hashes each compiled module
// by source-bytes + config; the same .wasm being instantiated twice
// across two CLI invocations finds the prior compile and skips
// translation.
//
// All failure modes downgrade to "no cache" via a returned nil — a
// build or run shouldn't fail because a cache dir couldn't be
// created. Caller may log the returned error as a warning.
func loadWasmCompilationCache(_ context.Context) (wazero.CompilationCache, error) {
	path, err := defaultWasmCacheDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("create wasm cache dir %s: %w", path, err)
	}
	cache, err := wazero.NewCompilationCacheWithDir(path)
	if err != nil {
		return nil, fmt.Errorf("open wasm cache at %s: %w", path, err)
	}
	return cache, nil
}
