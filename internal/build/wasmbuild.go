package build

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"time"

	"github.com/partite-ai/particles/internal/build/wacogo"
	"github.com/partite-ai/particles/internal/memfs"
	"github.com/partite-ai/particles/runtime"
)

// buildWasm packages a prebuilt wasi:p2 component as a particle.
// Triggered by Options.Component != "". Steps:
//
//  1. Read the component bytes from Options.Source at the path
//     Options.Component names.
//  2. Stage an artifact-shaped FS with the component at
//     "particle.wasm".
//  3. Call ExtractManifest, which instantiates the component and
//     invokes its particle:runtime/manifest.get-manifest export.
//  4. Validate + serialize the manifest, write build-info.json,
//     assemble the final artifact (manifest.json + particle.wasm +
//     build-info.json).
//
// Native build chains pick how they name their wasm; the build
// just takes whatever path the caller hands over.
func buildWasm(ctx context.Context, opts Options, comps *wacogo.Components) (*Result, error) {
	reportPhaseStart(opts, PhaseBundle)
	componentBytes, err := fs.ReadFile(opts.Source, opts.Component)
	if err != nil {
		return nil, &Error{Phase: PhaseBundle, Cause: fmt.Errorf("read component %s: %w", opts.Component, err)}
	}
	reportPhaseDetail(opts, "%s (%s)", opts.Component, humanBytes(len(componentBytes)))

	stagingFS := memfs.FS{
		"particle.wasm": &memfs.File{Data: componentBytes, Mode: 0o644},
	}

	reportPhaseStart(opts, PhaseManifestExtract)
	extracted, err := comps.ExtractManifest(ctx, runtime.RuntimeWasm, stagingFS)
	if err != nil {
		return nil, &Error{Phase: PhaseManifestExtract, Cause: err}
	}
	// Builder is the source of truth for `runtime` — the WIT record
	// doesn't carry it. The caller asked for a wasm build via
	// Options.Component, so the value is fixed here.
	extracted.Runtime = runtime.RuntimeWasm
	if err := validateExtractedManifest(extracted); err != nil {
		return nil, &Error{Phase: PhaseManifestExtract, Cause: err}
	}
	manifestJSON, err := json.Marshal(extracted)
	if err != nil {
		return nil, &Error{Phase: PhaseManifestExtract, Cause: fmt.Errorf("marshal manifest: %w", err)}
	}
	reportPhaseDetail(opts, "%s %s — %d tool%s, %d credential%s",
		extracted.Name, extracted.Version,
		len(extracted.Tools), plural(len(extracted.Tools), "", "s"),
		len(extracted.Credentials), plural(len(extracted.Credentials), "", "s"))

	reportPhaseStart(opts, PhaseAssemble)
	buildInfo, err := encodeWasmBuildInfo(opts.Component, len(componentBytes))
	if err != nil {
		return nil, &Error{Phase: PhaseAssemble, Cause: fmt.Errorf("encode build-info: %w", err)}
	}

	stagingFS["manifest.json"] = &memfs.File{Data: manifestJSON, Mode: 0o644}
	stagingFS["build-info.json"] = &memfs.File{Data: buildInfo, Mode: 0o644}

	result := &Result{Particle: stagingFS}
	reportPhaseDetail(opts, "%s", artifactSummary(result.Particle))
	return result, nil
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
