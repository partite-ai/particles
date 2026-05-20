package build

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"testing/fstest"
	"time"

	"github.com/partite-ai/particles/internal/build/wacogo"
	"github.com/partite-ai/particles/runtime"
)

// buildWasm packages a prebuilt wasi:p2 component as a particle.
// Triggered by Options.Component != "". Steps:
//
//   1. Read the component bytes from Options.Source at the path
//      Options.Component names.
//   2. Stage an artifact-shaped FS with the component at
//      "particle.wasm".
//   3. Call ExtractManifest, which instantiates the component and
//      invokes its particle:runtime/manifest.get-manifest export.
//   4. Validate + serialize the manifest, write build-info.json,
//      assemble the final artifact (manifest.json + particle.wasm +
//      build-info.json).
//
// Native build chains pick how they name their wasm; the build
// just takes whatever path the caller hands over.
func buildWasm(ctx context.Context, opts Options, comps *wacogo.Components) (*Result, error) {
	componentBytes, err := fs.ReadFile(opts.Source, opts.Component)
	if err != nil {
		return nil, &Error{Phase: PhaseBundle, Cause: fmt.Errorf("read component %s: %w", opts.Component, err)}
	}

	stagingFS := fstest.MapFS{
		"particle.wasm": &fstest.MapFile{Data: componentBytes, Mode: 0o644},
	}

	extracted, err := comps.ExtractManifest(ctx, runtime.RuntimeWasm, stagingFS)
	if err != nil {
		return nil, &Error{Phase: PhaseManifestExtract, Cause: err}
	}
	if err := validateExtractedManifest(extracted); err != nil {
		return nil, &Error{Phase: PhaseManifestExtract, Cause: err}
	}
	manifestJSON, err := json.Marshal(extracted)
	if err != nil {
		return nil, &Error{Phase: PhaseManifestExtract, Cause: fmt.Errorf("marshal manifest: %w", err)}
	}

	buildInfo, err := encodeWasmBuildInfo(opts.Component, len(componentBytes))
	if err != nil {
		return nil, &Error{Phase: PhaseAssemble, Cause: fmt.Errorf("encode build-info: %w", err)}
	}

	stagingFS["manifest.json"] = &fstest.MapFile{Data: manifestJSON, Mode: 0o644}
	stagingFS["build-info.json"] = &fstest.MapFile{Data: buildInfo, Mode: 0o644}

	return &Result{Particle: stagingFS}, nil
}

// wasmBuildInfo is the build-info shape for wasm particles. Carries
// just enough provenance to identify the artifact: when it was
// built, which `runtimeApi` it targets, where the component came
// from, and its size. No dep list (the component is fully self-
// contained).
type wasmBuildInfo struct {
	BuildTime     string `json:"buildTime"`
	RuntimeAPI    string `json:"runtimeApi"`
	Runtime       string `json:"runtime"`
	ComponentPath string `json:"componentPath,omitempty"`
	ComponentSize int    `json:"componentSize"`
}

func encodeWasmBuildInfo(componentPath string, size int) ([]byte, error) {
	info := wasmBuildInfo{
		BuildTime:     time.Now().UTC().Format(time.RFC3339),
		RuntimeAPI:    runtimeAPIVersion,
		Runtime:       "wasm",
		ComponentPath: componentPath,
		ComponentSize: size,
	}
	return json.MarshalIndent(info, "", "  ")
}
