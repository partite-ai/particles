package wacogo

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing/fstest"

	wc "github.com/partite-ai/wacogo"
	"github.com/partite-ai/wacogo/wasi"
	"github.com/partite-ai/wacogo/wasi/filesystem/preopens"
)

// introspectInterface is the canonical id of the exported instance the
// introspect component publishes. See components/introspect/wit/introspect.wit.
const introspectInterface = "particle:introspect/introspect@0.1.0"

// IntrospectResult is what Components.Introspect returns on success.
type IntrospectResult struct {
	// Manifest is the JSON-encoded particle manifest.
	Manifest []byte
	// Stderr is whatever the component wrote to wasi:cli/stderr during
	// the call (typically empty on success; populated when the JS
	// throws or panics so the caller can include it in diagnostics).
	Stderr []byte
}

// Introspect evaluates bundleJS inside particle-introspect.wasm and
// returns the manifest JSON.
//
// Per call:
//   - Build a wasi:cli/imports + wasi:http world with a single-file
//     fs.FS containing `particle/bundle.js` mounted as a preopen. The
//     introspect component's JS source reads `/particle/bundle.js` via
//     dynamic ESM import.
//   - Wire a no-op `wasi:logging/logging` host stub (the QuickJS
//     engine inside the component imports this for `console.*`).
//   - Call `wizer-initialize` to bring up the JS state machine.
//   - Resolve `particle:introspect/introspect@0.1.0`'s `manifest`
//     export and call it; lift the returned `result<string,
//     introspect-error>` into ([]byte, error).
//
// The introspect component imports no `particle:host/*` interfaces —
// the bundle's `particle:*` module imports must resolve via QuickJS-
// internal stubs. Bundles that import `particle:*` module specifiers
// will currently fail at module load; that's a known limitation of the
// current introspect.ts source.
func (c *Components) Introspect(ctx context.Context, bundleJS []byte) (*IntrospectResult, error) {
	if c.introspect == nil {
		return nil, fmt.Errorf("wacogo: introspect component not loaded")
	}

	bundleFS := fstest.MapFS{
		"particle/bundle.js": &fstest.MapFile{Data: bundleJS},
	}

	stderrBuf := &bytes.Buffer{}
	w, err := wasi.NewWorld(ctx, c.engine, &wasi.Config{
		Args:     []string{"particle-introspect"},
		Preopens: preopens.NewFSPreopens(bundleFS),
		Stdin:    strings.NewReader(""),
		Stdout:   io.Discard,
		Stderr:   stderrBuf,
	})
	if err != nil {
		return nil, fmt.Errorf("build wasi world: %w", err)
	}
	defer w.Close(ctx)

	logger, err := newLoggingStub(ctx, c.engine)
	if err != nil {
		return nil, fmt.Errorf("build wasi:logging stub: %w", err)
	}
	defer logger.Close(ctx)

	imports := append(w.Imports(), wc.WithInstanceImport(loggingInterfaceName, logger.Core()))

	inst, err := c.introspect.Instantiate(ctx, imports...)
	if err != nil {
		return nil, withStderr(err, stderrBuf, "instantiate introspect")
	}
	defer inst.Close(ctx)

	if fn := inst.ExportedFunc("wizer-initialize"); fn != nil {
		if _, err := fn.Call(ctx); err != nil {
			return nil, withStderr(err, stderrBuf, "wizer-initialize")
		}
	}

	iface := inst.ExportedInstance(introspectInterface)
	if iface == nil {
		return nil, fmt.Errorf("introspect component does not export instance %q", introspectInterface)
	}
	fn := iface.ExportedFunc("manifest")
	if fn == nil {
		return nil, fmt.Errorf("introspect.%s does not export manifest()", introspectInterface)
	}

	results, err := fn.Call(ctx)
	if err != nil {
		return &IntrospectResult{Stderr: stderrBuf.Bytes()},
			withStderr(err, stderrBuf, "call introspect.manifest")
	}
	if len(results) != 1 {
		return nil, fmt.Errorf("introspect.manifest returned %d results, want 1", len(results))
	}
	res, ok := results[0].(*wc.ValResult)
	if !ok {
		return nil, fmt.Errorf("introspect.manifest result is %T, want *wacogo.ValResult", results[0])
	}
	if !res.IsOk() {
		return &IntrospectResult{Stderr: stderrBuf.Bytes()}, decodeIntrospectError(res.Err())
	}
	s, ok := res.Ok().(wc.ValString)
	if !ok {
		return nil, fmt.Errorf("introspect.manifest ok payload is %T, want ValString", res.Ok())
	}
	return &IntrospectResult{
		Manifest: []byte(s),
		Stderr:   stderrBuf.Bytes(),
	}, nil
}

// decodeIntrospectError converts a `variant introspect-error` Val into
// a Go error.
//
//	variant introspect-error {
//	  bundle-load-error(string),
//	  invalid-manifest(string),
//	}
func decodeIntrospectError(v wc.Val) error {
	variant, ok := v.(*wc.ValVariant)
	if !ok {
		return fmt.Errorf("introspect error payload is %T, want *wacogo.ValVariant", v)
	}
	cases := []string{"bundle-load-error", "invalid-manifest"}
	d := int(variant.Discriminant())
	name := "unknown"
	if d >= 0 && d < len(cases) {
		name = cases[d]
	}
	msg := ""
	if s, ok := variant.Val().(wc.ValString); ok {
		msg = string(s)
	}
	return fmt.Errorf("introspect: %s: %s", name, msg)
}
