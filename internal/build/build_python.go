package build

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/partite-ai/particles/internal/build/wacogo"
	"github.com/partite-ai/particles/internal/memfs"
	"github.com/partite-ai/particles/internal/pyscan"
	"github.com/partite-ai/particles/runtime"
)

// buildPython runs the Python pipeline. Same six conceptual phases as
// buildJS, just with the Python-flavored components:
//
//	Phase 1: pyscan          (Go)                            — parse PEP 723 inline metadata
//	Phase 2: pip-resolve     (pip-resolve.wasm)              — resolve + fetch pure-Python wheels
//	Phase 3: typecheck       (skipped)                       — Python typecheck is v2
//	Phase 4: bundle          (Go, no-op)                     — copy Particlefile.py as bundle.py
//	Phase 5: manifest-extract(runtime.IntrospectParticle)    — read `particle` dict from bundle.py
//	Phase 6: assemble        (Go)                            — manifest + bundle.py + _deps/site-packages
//
// Artifact layout differs from JS: there's no JS-style bundling step, so we
// keep the Particlefile.py as bundle.py and unpack the resolved wheels
// alongside under `_deps/site-packages/`. The runtime adds that path to
// sys.path at instantiation so user code can `import httpx` etc.
func buildPython(ctx context.Context, opts Options, comps *wacogo.Components, entry string) (*Result, error) {
	// No warnings surface from the Python pipeline today (PEP 723
	// parse, pip-resolve, introspect all binary pass/fail). When the
	// resolver gets a "yanked-version warning" or similar, plumb
	// here.
	var logs []Log

	// ---- Phase 1: import-scan (PEP 723) -------------------------------
	reportPhaseStart(opts, PhaseImportScan)
	py, err := pyscan.Scan(opts.Source, entry)
	if err != nil {
		return nil, &Error{Phase: PhaseImportScan, Logs: logs, Cause: err}
	}
	reportPhaseDetail(opts, "%d dep%s declared",
		len(py.Dependencies), plural(len(py.Dependencies), "", "s"))

	// ---- Phase 2: resolve-and-fetch (only when deps declared) ---------
	//
	// pip-resolve checks PyPI for pure-Python wheels and the particle
	// wheels index at
	// https://partite-ai.github.io/particle-python-wheels/ for
	// wasm-cross-compiled native wheels — packages whose published
	// PyPI wheels are all platform-tagged (cryptography, cffi, …).
	// All the policy lives inside the wasm component; the host just
	// passes the declared dep list.
	var wheels []wacogo.PipResolvedWheel
	if py.HasBlock && len(py.Dependencies) > 0 {
		reportPhaseStart(opts, PhaseResolveAndFetch)
		pr, err := comps.PipResolveAndFetch(ctx, py.Dependencies, pythonRuntimeVersion)
		logs = appendLog(logs, PhaseResolveAndFetch, pr)
		if err != nil {
			return nil, &Error{Phase: PhaseResolveAndFetch, Logs: logs, Cause: err}
		}
		wheels = append(wheels, pr.Wheels...)
		// Headline pinned versions inline — useful for spotting an
		// unexpected resolution at a glance. Cap the list at a small
		// number; full set lands in build-info.json + Particle.lock.
		reportPhaseDetail(opts, "%d wheel%s: %s",
			len(wheels), plural(len(wheels), "", "s"),
			summarizeWheels(wheels, 6))
	}

	// ---- Phase 3: typecheck (skipped) ---------------------------------
	// Python typecheck (mypy/pyright) is Phase-2 work — out of scope for
	// v1. Note for callers: opts.NoTypeCheck has no effect here.

	// ---- Phase 4: bundle (read source as bytes; no JS-style bundling) -
	reportPhaseStart(opts, PhaseBundle)
	bundlePy, err := fs.ReadFile(opts.Source, entry)
	if err != nil {
		return nil, &Error{Phase: PhaseBundle, Logs: logs, Cause: fmt.Errorf("read %s: %w", entry, err)}
	}
	reportPhaseDetail(opts, "%s", humanBytes(len(bundlePy)))

	// Unpack wheels into the artifact layout *before* introspect.
	// The user's bundle.py may `import httpx` at module scope; for
	// introspect to load the bundle successfully, those imports
	// have to resolve. We mount the same _deps/site-packages tree
	// the artifact will ship, and the runtime's bootstrap appends
	// that path to sys.path on load.
	depsFiles, err := unpackWheels(wheels)
	if err != nil {
		return nil, &Error{Phase: PhaseAssemble, Logs: logs, Cause: fmt.Errorf("unpack wheels: %w", err)}
	}

	// Build the artifact-shaped FS once and share it: introspect
	// mounts it (under /particle/), the final Result returns it.
	// Avoids a double-pack and keeps "what we ship" and "what we
	// introspect" provably identical.
	stagingFS := memfs.FS{
		"bundle.py": &memfs.File{Data: bundlePy, Mode: 0o644},
	}
	for path, data := range depsFiles {
		stagingFS[path] = &memfs.File{Data: data, Mode: 0o644}
	}

	// ---- Phase 5: manifest-extract ------------------------------------
	// ExtractManifest invokes the runtime's
	// particle:runtime/manifest export — same call shape and typed
	// result as the JS path.
	reportPhaseStart(opts, PhaseManifestExtract)
	extracted, err := comps.ExtractManifest(ctx, runtime.RuntimePython, stagingFS)
	if err != nil {
		return nil, &Error{Phase: PhaseManifestExtract, Logs: logs, Cause: err}
	}
	// Builder is the source of truth for `runtime` — the WIT record
	// doesn't carry it. We dispatched into buildPython off the .py
	// extension, so the value is fixed here.
	extracted.Runtime = runtime.RuntimePython
	if err := validateExtractedManifest(extracted); err != nil {
		return nil, &Error{Phase: PhaseManifestExtract, Logs: logs, Cause: err}
	}
	manifestJSON, err := json.Marshal(extracted)
	if err != nil {
		return nil, &Error{Phase: PhaseManifestExtract, Logs: logs, Cause: fmt.Errorf("marshal manifest: %w", err)}
	}
	reportPhaseDetail(opts, "%s %s — %d tool%s, %d credential%s",
		extracted.Name, extracted.Version,
		len(extracted.Tools), plural(len(extracted.Tools), "", "s"),
		len(extracted.Credentials), plural(len(extracted.Credentials), "", "s"))

	// ---- Phase 6: assemble --------------------------------------------
	reportPhaseStart(opts, PhaseAssemble)
	buildInfo, err := encodePythonBuildInfo(py, wheels)
	if err != nil {
		return nil, &Error{Phase: PhaseAssemble, Logs: logs, Cause: fmt.Errorf("encode build-info: %w", err)}
	}

	lock, err := encodePythonLockfile(wheels)
	if err != nil {
		return nil, &Error{Phase: PhaseAssemble, Logs: logs, Cause: fmt.Errorf("encode lockfile: %w", err)}
	}

	// Add the build-pipeline-derived files (manifest, build-info,
	// optional lock) onto the already-populated staging FS — the
	// staging FS is then the artifact FS, no separate assemble.
	stagingFS["manifest.json"] = &memfs.File{Data: manifestJSON, Mode: 0o644}
	stagingFS["build-info.json"] = &memfs.File{Data: buildInfo, Mode: 0o644}
	if len(lock) > 0 {
		stagingFS["Particle.lock"] = &memfs.File{Data: lock, Mode: 0o644}
	}

	result := &Result{
		Particle: stagingFS,
		Logs:     logs,
	}
	reportPhaseDetail(opts, "%s", artifactSummary(result.Particle))
	return result, nil
}

// summarizeWheels formats a short "<name> <ver>, <name> <ver>, …"
// preview of the resolved wheel set, capping at `limit` entries so a
// 30-wheel transitive closure doesn't blow out the line.
func summarizeWheels(wheels []wacogo.PipResolvedWheel, limit int) string {
	if len(wheels) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(wheels))
	for i, w := range wheels {
		if i >= limit {
			parts = append(parts, fmt.Sprintf("and %d more", len(wheels)-limit))
			break
		}
		parts = append(parts, fmt.Sprintf("%s %s", w.Name, w.Version))
	}
	return strings.Join(parts, ", ")
}

// pythonRuntimeVersion is the CPython version baked into
// particle-python-runtime.wasm via componentize-py. Threaded into the
// resolver only as a hint — the resolver doesn't yet evaluate
// environment markers based on it (see components/pip-resolve), so
// it's informational for now. Keep in sync with the
// `componentize-py python install 3.12` line in the Dockerfile.
const pythonRuntimeVersion = "3.12"

// -----------------------------------------------------------------------------
// Artifact assembly helpers
// -----------------------------------------------------------------------------

// unpackWheels extracts every resolved wheel into a flat
// `_deps/site-packages/...` tree. Each wheel is a standard zip; we
// unpack regular files only and skip directory entries (MapFS doesn't
// need explicit dir entries). All wheels share the same
// site-packages root — a wheel for `httpx` contributes both `httpx/`
// (the package) and `httpx-0.27.0.dist-info/` (the metadata) at the
// same level, matching what pip itself produces.
func unpackWheels(wheels []wacogo.PipResolvedWheel) (map[string][]byte, error) {
	const prefix = "_deps/site-packages/"
	out := make(map[string][]byte)
	for _, w := range wheels {
		zr, err := zip.NewReader(bytes.NewReader(w.WheelBytes), int64(len(w.WheelBytes)))
		if err != nil {
			return nil, fmt.Errorf("%s: open zip: %w", w.Filename, err)
		}
		for _, f := range zr.File {
			if f.FileInfo().IsDir() {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("%s/%s: open: %w", w.Filename, f.Name, err)
			}
			data, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return nil, fmt.Errorf("%s/%s: read: %w", w.Filename, f.Name, err)
			}
			// PEP 427: wheel entries can be either at the root
			// (the common case for pure-Python wheels — package
			// folder + dist-info folder) or under `<name>.data/`
			// (data files installed to specific scheme paths).
			// For v1 we only support the root case; we skip
			// anything under `.data/` so a wheel that uses it
			// doesn't silently land in the wrong place.
			if strings.Contains(f.Name, ".data/") {
				continue
			}
			out[prefix+f.Name] = data
		}
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// build-info and Particle.lock for Python particles
// -----------------------------------------------------------------------------

// pythonBuildInfo is the Python-flavored buildInfo. Shares the
// `buildTime` + `runtimeApi` + `capabilities` lead with the JS
// version (so a consumer can read those without branching on
// runtime) and adds Python-specific blocks for declared and resolved
// deps.
type pythonBuildInfo struct {
	BuildTime  string                   `json:"buildTime"`
	RuntimeAPI string                   `json:"runtimeApi"`
	Runtime    string                   `json:"runtime"`
	PyDeps     []string                 `json:"pyDeps,omitempty"`
	Resolved   []pythonBuildInfoResolve `json:"resolved,omitempty"`
}

type pythonBuildInfoResolve struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Sha256   string `json:"sha256"`
	Filename string `json:"filename"`
}

func encodePythonBuildInfo(py *pyscan.Result, wheels []wacogo.PipResolvedWheel) ([]byte, error) {
	info := pythonBuildInfo{
		BuildTime:  time.Now().UTC().Format(time.RFC3339),
		RuntimeAPI: runtimeAPIVersion,
		Runtime:    "python",
	}
	if py != nil {
		info.PyDeps = append(info.PyDeps, py.Dependencies...)
	}
	for _, w := range wheels {
		info.Resolved = append(info.Resolved, pythonBuildInfoResolve{
			Name:     w.Name,
			Version:  w.Version,
			Sha256:   w.Sha256,
			Filename: w.Filename,
		})
	}
	sort.Slice(info.Resolved, func(i, j int) bool {
		return info.Resolved[i].Name < info.Resolved[j].Name
	})
	return json.MarshalIndent(info, "", "  ")
}

// Particle.lock for Python: a flat list of pinned (name, version,
// sha256). One per line as JSON would be more typical, but a single
// JSON document keeps it diff-friendly and matches the JS
// Particle.lock plan in docs/initial-design.md.
type pythonLockfile struct {
	Wheels []pythonLockfileEntry `json:"wheels"`
}

type pythonLockfileEntry struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Sha256   string `json:"sha256"`
	Filename string `json:"filename"`
}

func encodePythonLockfile(wheels []wacogo.PipResolvedWheel) ([]byte, error) {
	if len(wheels) == 0 {
		return nil, nil
	}
	lf := pythonLockfile{}
	for _, w := range wheels {
		lf.Wheels = append(lf.Wheels, pythonLockfileEntry{
			Name:     w.Name,
			Version:  w.Version,
			Sha256:   w.Sha256,
			Filename: w.Filename,
		})
	}
	sort.Slice(lf.Wheels, func(i, j int) bool { return lf.Wheels[i].Name < lf.Wheels[j].Name })
	return json.MarshalIndent(lf, "", "  ")
}
