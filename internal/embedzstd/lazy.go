package embedzstd

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"sync"

	wc "github.com/partite-ai/wacogo"
)

// LazyComponent wraps `embed.FS` lookup → zstd decompression →
// wacogo Engine.LoadComponent in a [sync.Once] cell. Construction
// just records the path; the first Get call does the heavy work
// (open, decompress ~MB of wasm, parse + validate the component).
// Subsequent calls return the cached result with no work.
//
// Used by `runtime` and `internal/build/wacogo` to skip loading
// components a particular invocation never touches — e.g. a JS
// `particle build` doesn't need particle-python-runtime, a
// `particle run` doesn't need any of the build-time wasms.
//
// First-caller's `ctx` is the one used inside the Once; subsequent
// callers can pass any ctx and get the cached component. wacogo's
// LoadComponent finishes in ms even for the larger images, so the
// "first ctx wins" semantics are fine in practice.
type LazyComponent struct {
	path string
	once sync.Once
	comp *wc.Component
	err  error
}

// NewLazyComponent returns a [LazyComponent] that will load `path`
// (a zstd-compressed wasm) from `fs` on first Get. `fs` is the embed
// FS the caller passes to every Get — it's not stored here because
// embed.FS values are cheap and the calling package's go:embed
// directive owns its lifetime.
func NewLazyComponent(path string) *LazyComponent {
	return &LazyComponent{path: path}
}

// Get returns the loaded wacogo component. First call materializes
// it (read + decompress + parse); subsequent calls return the same
// pointer. Concurrent callers see consistent results via sync.Once.
//
// `fs` and `engine` must be the same on every call — they're
// arguments rather than fields so callers don't have to maintain a
// circular reference between Components and its lazy cells.
func (lc *LazyComponent) Get(ctx context.Context, fs embed.FS, engine *wc.Engine) (*wc.Component, error) {
	lc.once.Do(func() {
		data, err := Read(fs, lc.path)
		if err != nil {
			lc.err = err
			return
		}
		comp, err := engine.LoadComponent(ctx, bytes.NewReader(data))
		if err != nil {
			lc.err = fmt.Errorf("load %s: %w", lc.path, err)
			return
		}
		lc.comp = comp
	})
	return lc.comp, lc.err
}
