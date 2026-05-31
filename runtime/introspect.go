package runtime

import (
	"context"
	"fmt"
	"io/fs"

	"github.com/partite-ai/particles/credentials"
	"github.com/partite-ai/particles/internal/memfs"
	"github.com/partite-ai/particles/kv"
)

// IntrospectParticle instantiates the runtime against `sourceFS`
// purely to call `particle:runtime/manifest.get-manifest`, then tears
// the instance down. This is the canonical entry point for the build
// pipeline's Phase 5 — and the contract any future "fully-WASM
// particle" host would call to learn what a binary exposes before
// committing real stores to it.
//
// The whole introspect setup — trap credential / kv stores, trap
// outbound HTTP, the runtime instantiation, the get-manifest call,
// cleanup — is owned by this method. Callers don't supply stores or
// option flags; the contract is "give me the FS, get back a typed
// Manifest." A particle that does a module-scope `credentials.fetcher`
// or `fetch` surfaces a `bundle-load-error` whose message contains
// "not allowed during get-manifest" (see credentials.ErrIntrospectMode,
// kv.ErrIntrospectMode, ErrIntrospectModeHTTP).
//
// `kind` selects which embedded runtime to instantiate; the build
// pipeline knows this from the entry-point extension (.ts/.js → JS,
// .py → Python). `sourceFS` carries the in-flight artifact: bundle.{js,py}
// at the root, optional `_deps/site-packages/` for Python.
func (r *Runtime) IntrospectParticle(ctx context.Context, kind RuntimeKind, sourceFS fs.FS) (*Manifest, error) {
	if sourceFS == nil {
		return nil, fmt.Errorf("runtime: IntrospectParticle: sourceFS is required")
	}

	// Overlay a synthesized manifest.json so the runtime's
	// NewParticle dispatch can pick the right wasm. The real
	// manifest is what we're about to compute — it doesn't exist
	// yet, so we don't read one off the user's source.
	particleFS, err := synthesizeIntrospectFS(sourceFS, kind)
	if err != nil {
		return nil, fmt.Errorf("runtime: IntrospectParticle: prep FS: %w", err)
	}

	// Trap stores + introspect-mode flag. No public surface; the
	// only caller that's allowed to set this is this method.
	credStore := credentials.NewIntrospectTrapStore()
	kvStore := kv.NewIntrospectTrapStore()
	cfg := particleConfig{
		// httpClientFactory is unused under introspectMode
		// (newParticleInternal wires the trap doer regardless), but
		// we fill it in so the resolved config matches what
		// applyParticleOptions would produce.
		httpClientFactory: defaultHTTPClientFactory,
		log:               DefaultLogCallback,
		introspectMode:    true,
	}

	p, err := r.newParticleInternal(ctx, particleFS, credStore, kvStore, nil, cfg)
	if err != nil {
		return nil, fmt.Errorf("runtime: IntrospectParticle: instantiate: %w", err)
	}
	defer p.Close(ctx)

	return p.GetManifest(ctx)
}

// synthesizeIntrospectFS walks `sourceFS` and returns a copy with a
// minimal manifest.json overlaid at the root. The synthesized
// manifest carries only the runtime kind — that's all NewParticle
// reads at instantiate time for dispatch and host wiring.
//
// Materializing into a MapFS keeps the overlay cheap; the build
// pipeline's source trees are bounded (KBs of source + at most a
// few MB of pure-Python wheels).
func synthesizeIntrospectFS(sourceFS fs.FS, kind RuntimeKind) (fs.FS, error) {
	stub := fmt.Sprintf(
		`{"name":"__introspect","version":"0.0.0","runtime":%q,"capabilities":{},"tools":[]}`,
		kind,
	)
	out := memfs.FS{
		"manifest.json": &memfs.File{Data: []byte(stub)},
	}
	err := fs.WalkDir(sourceFS, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || path == "manifest.json" {
			return nil
		}
		data, err := fs.ReadFile(sourceFS, path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		out[path] = &memfs.File{Data: data}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
