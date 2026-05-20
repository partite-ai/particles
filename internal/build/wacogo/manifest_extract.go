package wacogo

import (
	"context"
	"fmt"
	"io/fs"

	"github.com/partite-ai/particles/runtime"
)

// ExtractManifest is Phase 5 of the build pipeline. Delegates to
// [runtime.Runtime.IntrospectParticle], which owns the introspect-
// mode wiring (trap credential / kv stores, trap HTTP doer, the
// synthesized manifest overlay, the get-manifest call, cleanup).
// The build pipeline hands over `sourceFS` and gets the typed
// manifest back.
//
// The same call shape works for current bundle-loading runtimes and
// for any future fully-WASM particle whose only self-description
// surface is the get-manifest export.
func (c *Components) ExtractManifest(ctx context.Context, kind runtime.RuntimeKind, sourceFS fs.FS) (*runtime.Manifest, error) {
	if c.runtime == nil {
		return nil, fmt.Errorf("wacogo: runtime not loaded")
	}
	return c.runtime.IntrospectParticle(ctx, kind, sourceFS)
}
