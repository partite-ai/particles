// Package build is the particle build orchestrator.
//
// Build turns a particle source tree into a particle artifact: an
// in-memory fs.FS holding `manifest.json`, `bundle.js`, `bundle.js.map`,
// and `build-info.json`. Callers can pack the FS into a tarball, upload
// it as files, or feed it directly into the runtime.
//
// The pipeline runs the six phases described in docs/initial-design.md
// §5 sequentially:
//
//	Phase 1: import-scan       (Go)
//	Phase 2: resolve-and-fetch (deno-npm.wasm,  via wacogo)
//	Phase 3: typecheck         (typecheck.wasm, via wacogo) — skippable
//	Phase 4: bundle            (esbuild Go lib)
//	Phase 5: manifest-extract  (introspect.wasm, via wacogo)
//	Phase 6: assemble          (Go)
//
// The wasm-backed phases are embedded into the binary at compile time
// — see internal/build/wacogo/embed and the `go generate` /
// `make embed` flow.
package build

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"testing/fstest"
	"time"

	"github.com/partite-ai/particle/internal/build/wacogo"
	"github.com/partite-ai/particle/internal/bundle"
	"github.com/partite-ai/particle/internal/importscan"
	"github.com/partite-ai/particle/internal/semver"
)

// Options configure a single build invocation.
type Options struct {
	// Source is the particle source tree (typically `os.DirFS(srcDir)`).
	Source fs.FS

	// EntryPoint is the FS-relative path to the particle's entry source.
	// Defaults to "Particlefile.ts" then "Particlefile.js".
	EntryPoint string

	// NoTypeCheck skips Phase 3 (default: type-check on).
	NoTypeCheck bool
}

// Result is what Build returns on success.
type Result struct {
	// Particle is the in-memory artifact: a virtual fs.FS containing
	//
	//   manifest.json       — output of Phase 5
	//   bundle.js           — output of Phase 4
	//   bundle.js.map       — sourcemap (always emitted)
	//   build-info.json     — runtime version, capabilities, npm deps
	//
	// Callers can range it directly (e.g., write to disk, stream to
	// a registry, pack into a tarball — `archive/tar` works fine
	// against `fs.WalkDir`).
	Particle fs.FS

	// Warnings collects non-fatal Diagnostics from any phase. Errors
	// surface via the returned `error`.
	Warnings []Diagnostic

	// Logs is everything the wasm components wrote to wasi:cli/stderr
	// during the build, keyed by phase. Useful for debugging when a
	// phase trapped or printed diagnostics — typically empty on a
	// clean build.
	Logs []Log
}

// Phase identifies which phase produced an error, log entry, or
// diagnostic.
type Phase int

const (
	PhaseImportScan Phase = iota + 1
	PhaseResolveAndFetch
	PhaseTypecheck
	PhaseBundle
	PhaseManifestExtract
	PhaseAssemble
)

func (p Phase) String() string {
	switch p {
	case PhaseImportScan:
		return "import-scan"
	case PhaseResolveAndFetch:
		return "resolve-and-fetch"
	case PhaseTypecheck:
		return "typecheck"
	case PhaseBundle:
		return "bundle"
	case PhaseManifestExtract:
		return "manifest-extract"
	case PhaseAssemble:
		return "assemble"
	default:
		return fmt.Sprintf("phase(%d)", int(p))
	}
}

// Diagnostic is a structured location-aware message produced by a phase.
type Diagnostic struct {
	Phase   Phase
	File    string
	Line    int
	Column  int
	Code    uint32 // optional; populated for typecheck diagnostics
	Message string
}

func (d Diagnostic) Error() string {
	loc := d.File
	if d.Line > 0 {
		loc = fmt.Sprintf("%s:%d:%d", d.File, d.Line, d.Column)
	}
	if loc != "" {
		return fmt.Sprintf("%s: %s: %s", d.Phase, loc, d.Message)
	}
	return fmt.Sprintf("%s: %s", d.Phase, d.Message)
}

// Log is one chunk of wasm-component output captured during a build.
type Log struct {
	Phase Phase
	Bytes []byte
}

// Error is the failure shape returned by Build. It groups one or more
// diagnostics under a single phase, plus any wasm-component output
// captured up to the failure point.
type Error struct {
	Phase       Phase
	Diagnostics []Diagnostic
	Logs        []Log
	Cause       error
}

func (e *Error) Error() string {
	if len(e.Diagnostics) == 0 && e.Cause != nil {
		return fmt.Sprintf("particle build failed: %s: %v", e.Phase, e.Cause)
	}
	if len(e.Diagnostics) == 1 {
		return fmt.Sprintf("particle build failed: %s\n  %s", e.Phase, e.Diagnostics[0].Error())
	}
	out := fmt.Sprintf("particle build failed: %s", e.Phase)
	for _, d := range e.Diagnostics {
		out += "\n  " + d.Error()
	}
	return out
}

func (e *Error) Unwrap() error { return e.Cause }

// Build runs the full pipeline. Returns *Result and nil on success;
// *Error and nil-Result on failure.
func Build(ctx context.Context, opts Options) (*Result, error) {
	if opts.Source == nil {
		return nil, &Error{Cause: errors.New("Options.Source is required")}
	}

	entry, err := resolveEntryPoint(opts.Source, opts.EntryPoint)
	if err != nil {
		return nil, &Error{Phase: PhaseImportScan, Cause: err}
	}

	comps, err := wacogo.New(ctx)
	if err != nil {
		return nil, &Error{Cause: err}
	}
	defer comps.Close(ctx)

	var (
		logs     []Log
		warnings []Diagnostic
	)

	// ---- Phase 1: import-scan -----------------------------------------
	scan, err := importscan.Scan(opts.Source)
	if err != nil {
		return nil, &Error{Phase: PhaseImportScan, Logs: logs, Cause: err}
	}
	if len(scan.Errors) > 0 {
		return nil, scanErrorsAsBuildError(scan.Errors, logs)
	}

	// ---- Phase 2: resolve-and-fetch (only when needed) ----------------
	var nodeModules fs.FS
	var resolvedPkgs []wacogo.ResolvedPackage
	if len(scan.NpmDeps) > 0 {
		rr, err := comps.ResolveAndFetch(ctx, scan.NpmDeps)
		logs = appendLog(logs, PhaseResolveAndFetch, rr)
		if err != nil {
			return nil, &Error{Phase: PhaseResolveAndFetch, Logs: logs, Cause: err}
		}
		nodeModules = rr.NodeModules
		resolvedPkgs = rr.Packages
	}

	// ---- Phase 3: typecheck (optional) --------------------------------
	if !opts.NoTypeCheck {
		cr, err := comps.TypeCheck(ctx, opts.Source, nodeModules)
		logs = appendLog(logs, PhaseTypecheck, cr)
		if err != nil {
			return nil, &Error{Phase: PhaseTypecheck, Logs: logs, Cause: err}
		}
		var fatal []Diagnostic
		for _, d := range cr.Diagnostics {
			liftedAsError := d.Severity == wacogo.SeverityError
			lifted := liftDiagnostic(PhaseTypecheck, d)
			if liftedAsError {
				fatal = append(fatal, lifted)
			} else {
				warnings = append(warnings, lifted)
			}
		}
		if len(fatal) > 0 {
			return nil, &Error{Phase: PhaseTypecheck, Diagnostics: fatal, Logs: logs}
		}
	}

	// ---- Phase 4: bundle ----------------------------------------------
	bundleFS := opts.Source
	if nodeModules != nil {
		bundleFS = mountedSourceFS{base: opts.Source, overlayName: "node_modules", overlay: nodeModules}
	}
	bundleResult, err := bundle.Bundle(bundle.Options{
		FS:         bundleFS,
		EntryPoint: entry,
		Sourcemap:  true,
		Minify:     true,
	})
	if err != nil {
		return nil, wrapBundleError(err, logs)
	}
	for _, w := range bundleResult.Warnings {
		warnings = append(warnings, Diagnostic{
			Phase: PhaseBundle, File: w.File, Line: w.Line, Column: w.Column, Message: w.Message,
		})
	}

	// ---- Phase 5: manifest-extract ------------------------------------
	ir, err := comps.Introspect(ctx, bundleResult.JS)
	logs = appendLog(logs, PhaseManifestExtract, ir)
	if err != nil {
		return nil, &Error{Phase: PhaseManifestExtract, Logs: logs, Cause: err}
	}
	if err := validateExtractedManifest(ir.Manifest); err != nil {
		return nil, &Error{Phase: PhaseManifestExtract, Logs: logs, Cause: err}
	}

	// ---- Phase 6: assemble --------------------------------------------
	buildInfo, err := encodeBuildInfo(scan, resolvedPkgs)
	if err != nil {
		return nil, &Error{Phase: PhaseAssemble, Logs: logs, Cause: fmt.Errorf("encode build-info: %w", err)}
	}

	particle := assembleParticleFS(particleFiles{
		Manifest:  ir.Manifest,
		Bundle:    bundleResult.JS,
		Sourcemap: bundleResult.Sourcemap,
		BuildInfo: buildInfo,
	})

	return &Result{
		Particle: particle,
		Warnings: warnings,
		Logs:     logs,
	}, nil
}

// knownCapabilities is the set of capability category names the
// runtime recognizes. Any other key under `capabilities`
// indicates a typo or a stale schema and fails the build —
// silently ignoring would let a manifest declare permissions the
// runtime never actually enforces. Update this list when a new
// capability lands in types/particle.d.ts.
var knownCapabilities = map[string]struct{}{
	"http": {},
}

// validateExtractedManifest runs the Go-side gates on the JSON
// the introspect WASM produced:
//
//  1. Strict SemVer 2.0.0 on `version` (shared with the registry
//     via `internal/semver`, so a tarball that bypasses the build
//     path still can't slip a bad version past `registry.Put`).
//
//  2. Every key under `capabilities` must be a recognized
//     category. `credentials` was a capability in pre-1.0 and
//     this is the migration-aware error users hit when they
//     update particle but not their manifest.
//
//  3. Every host listed under `credentials.<name>.hosts` must
//     also appear in `capabilities.http.allowedHosts`. A
//     credential bound to a host the particle can't actually
//     reach is either a typo or a layering bug — fail loud at
//     build time rather than at substitution time.
//
// Other shape checks — name non-empty, tools well-formed —
// already happen inside introspect.ts; this layer is the
// host-side gate that doesn't depend on guest-language code
// being correct.
func validateExtractedManifest(manifestJSON []byte) error {
	var mf struct {
		Version      string                     `json:"version"`
		Capabilities map[string]json.RawMessage `json:"capabilities"`
		Credentials  map[string]struct {
			Hosts []string `json:"hosts"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal(manifestJSON, &mf); err != nil {
		return fmt.Errorf("parse extracted manifest: %w", err)
	}
	if !semver.IsValid(mf.Version) {
		return fmt.Errorf("particle.version %q is not a valid semver string (e.g. \"1.2.3\", \"0.1.0-rc.1\", \"1.0.0+build.7\")", mf.Version)
	}
	for cap := range mf.Capabilities {
		if _, ok := knownCapabilities[cap]; !ok {
			return fmt.Errorf("capabilities.%s is not a recognized capability (known: %s)", cap, knownCapabilitiesList())
		}
	}
	var allowed map[string]struct{}
	if rawHTTP, ok := mf.Capabilities["http"]; ok {
		var v struct {
			AllowedHosts []string `json:"allowedHosts"`
		}
		if err := json.Unmarshal(rawHTTP, &v); err != nil {
			return fmt.Errorf("parse capabilities.http: %w", err)
		}
		allowed = make(map[string]struct{}, len(v.AllowedHosts))
		for _, h := range v.AllowedHosts {
			allowed[strings.ToLower(h)] = struct{}{}
		}
	}
	for credName, cred := range mf.Credentials {
		for _, h := range cred.Hosts {
			if _, ok := allowed[strings.ToLower(h)]; !ok {
				return fmt.Errorf("credentials.%s.hosts: %q is not in capabilities.http.allowedHosts — add it there or remove it from this credential", credName, h)
			}
		}
	}
	return nil
}

// knownCapabilitiesList returns the recognized capability names
// in sorted order, comma-joined — used to build error messages
// that show the author what's actually accepted.
func knownCapabilitiesList() string {
	names := make([]string, 0, len(knownCapabilities))
	for n := range knownCapabilities {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// resolveEntryPoint picks the conventional entry point. Spec §4: the
// conventional entry is `Particlefile.{js,ts}`; the user can override.
func resolveEntryPoint(fsys fs.FS, override string) (string, error) {
	if override != "" {
		if _, err := fs.Stat(fsys, override); err != nil {
			return "", fmt.Errorf("entry point %q: %w", override, err)
		}
		return override, nil
	}
	for _, candidate := range []string{"Particlefile.ts", "Particlefile.js"} {
		if _, err := fs.Stat(fsys, candidate); err == nil {
			return candidate, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return "", errors.New("no Particlefile.{ts,js} found at the source root")
}

// scanErrorsAsBuildError wraps importscan errors as a Phase 1 *Error.
func scanErrorsAsBuildError(errs []importscan.Error, logs []Log) *Error {
	diags := make([]Diagnostic, len(errs))
	for i, e := range errs {
		diags[i] = Diagnostic{
			Phase: PhaseImportScan, File: e.File, Line: e.Line, Column: e.Column, Message: e.Message,
		}
	}
	return &Error{Phase: PhaseImportScan, Diagnostics: diags, Logs: logs}
}

func wrapBundleError(err error, logs []Log) *Error {
	var be *bundle.Error
	if errors.As(err, &be) {
		diags := make([]Diagnostic, len(be.Diagnostics))
		for i, d := range be.Diagnostics {
			diags[i] = Diagnostic{
				Phase: PhaseBundle, File: d.File, Line: d.Line, Column: d.Column, Message: d.Message,
			}
		}
		return &Error{Phase: PhaseBundle, Diagnostics: diags, Logs: logs, Cause: err}
	}
	return &Error{Phase: PhaseBundle, Logs: logs, Cause: err}
}

func liftDiagnostic(phase Phase, d wacogo.Diagnostic) Diagnostic {
	return Diagnostic{
		Phase:   phase,
		File:    d.File,
		Line:    d.Line,
		Column:  d.Column,
		Code:    d.Code,
		Message: d.Message,
	}
}

// stderrCarrier is the small interface satisfied by every wacogo phase
// result that captures stderr. Lets appendLog stay phase-agnostic.
type stderrCarrier interface {
	stderrBytes() []byte
}

// appendLog records a phase's stderr (if any) into logs. Tolerates a
// nil result so it can sit in the failure path.
func appendLog(logs []Log, phase Phase, carrier any) []Log {
	if carrier == nil {
		return logs
	}
	var b []byte
	switch v := carrier.(type) {
	case *wacogo.IntrospectResult:
		if v != nil {
			b = v.Stderr
		}
	case *wacogo.CheckResult:
		if v != nil {
			b = v.Stderr
		}
	case *wacogo.ResolveResult:
		if v != nil {
			b = v.Stderr
		}
	case stderrCarrier:
		if v != nil {
			b = v.stderrBytes()
		}
	}
	if len(b) == 0 {
		return logs
	}
	return append(logs, Log{Phase: phase, Bytes: b})
}

// -----------------------------------------------------------------------------
// Phase 6 helpers
// -----------------------------------------------------------------------------

type particleFiles struct {
	Manifest  []byte
	Bundle    []byte
	Sourcemap []byte
	BuildInfo []byte
}

// assembleParticleFS produces the in-memory fs.FS that callers receive
// in Result.Particle. Files at the root, no nested layout — matches
// the design doc tarball convention.
func assembleParticleFS(in particleFiles) fs.FS {
	mfs := fstest.MapFS{
		"manifest.json":   &fstest.MapFile{Data: in.Manifest, Mode: 0o644},
		"bundle.js":       &fstest.MapFile{Data: in.Bundle, Mode: 0o644},
		"build-info.json": &fstest.MapFile{Data: in.BuildInfo, Mode: 0o644},
	}
	if len(in.Sourcemap) > 0 {
		mfs["bundle.js.map"] = &fstest.MapFile{Data: in.Sourcemap, Mode: 0o644}
	}
	return mfs
}

type buildInfo struct {
	BuildTime    string             `json:"buildTime"`
	RuntimeAPI   string             `json:"runtimeApi"`
	Capabilities []string           `json:"capabilities,omitempty"`
	NpmDeps      []buildInfoDep     `json:"npmDeps,omitempty"`
	Resolved     []buildInfoResolve `json:"resolved,omitempty"`
}

type buildInfoDep struct {
	Name         string `json:"name"`
	VersionRange string `json:"versionRange"`
	Importer     string `json:"importer,omitempty"`
}

type buildInfoResolve struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Integrity string `json:"integrity"`
}

// runtimeAPIVersion is the WIT-package version the build targets.
// Bumped when particle:host's WIT contract gets a major-version bump.
const runtimeAPIVersion = "0.1.0"

func encodeBuildInfo(scan *importscan.Result, resolved []wacogo.ResolvedPackage) ([]byte, error) {
	info := buildInfo{
		BuildTime:    time.Now().UTC().Format(time.RFC3339),
		RuntimeAPI:   runtimeAPIVersion,
		Capabilities: scan.Capabilities,
	}
	for _, d := range scan.NpmDeps {
		info.NpmDeps = append(info.NpmDeps, buildInfoDep{
			Name: d.Name, VersionRange: d.VersionRange, Importer: d.Importer,
		})
	}
	for _, p := range resolved {
		info.Resolved = append(info.Resolved, buildInfoResolve{
			Name: p.Name, Version: p.Version, Integrity: p.Integrity,
		})
	}
	return json.MarshalIndent(info, "", "  ")
}

// -----------------------------------------------------------------------------
// Source / node_modules overlay for esbuild
// -----------------------------------------------------------------------------

// mountedSourceFS overlays the source tree with a single fs.FS at one
// directory name (e.g., "node_modules"). esbuild's resolver consults
// the bundler's FS via fs.ReadFile / fs.Stat / fs.ReadDir, all of
// which we forward.
type mountedSourceFS struct {
	base        fs.FS
	overlayName string
	overlay     fs.FS
}

var (
	_ fs.FS         = mountedSourceFS{}
	_ fs.StatFS     = mountedSourceFS{}
	_ fs.ReadFileFS = mountedSourceFS{}
	_ fs.ReadDirFS  = mountedSourceFS{}
)

func (m mountedSourceFS) pickOverlay(name string) (fs.FS, string, bool) {
	if name == m.overlayName {
		return m.overlay, ".", true
	}
	if strings.HasPrefix(name, m.overlayName+"/") {
		return m.overlay, strings.TrimPrefix(name, m.overlayName+"/"), true
	}
	return nil, "", false
}

func (m mountedSourceFS) Open(name string) (fs.File, error) {
	if sub, p, ok := m.pickOverlay(name); ok {
		return sub.Open(p)
	}
	return m.base.Open(name)
}

func (m mountedSourceFS) Stat(name string) (fs.FileInfo, error) {
	if sub, p, ok := m.pickOverlay(name); ok {
		return fs.Stat(sub, p)
	}
	return fs.Stat(m.base, name)
}

func (m mountedSourceFS) ReadFile(name string) ([]byte, error) {
	if sub, p, ok := m.pickOverlay(name); ok {
		return fs.ReadFile(sub, p)
	}
	return fs.ReadFile(m.base, name)
}

func (m mountedSourceFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if sub, p, ok := m.pickOverlay(name); ok {
		return fs.ReadDir(sub, p)
	}
	return fs.ReadDir(m.base, name)
}
