// Package build is the particle build orchestrator.
//
// Build turns a particle source tree into a particle artifact: an
// in-memory fs.FS holding `manifest.json`, `bundle.mjs`, `bundle.mjs.map`,
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
//	Phase 5: manifest-extract  (runtime.IntrospectParticle, via wacogo)
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
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/tetratelabs/wazero"

	"github.com/partite-ai/particles/internal/build/wacogo"
	"github.com/partite-ai/particles/internal/bundle"
	"github.com/partite-ai/particles/internal/importscan"
	"github.com/partite-ai/particles/internal/memfs"
	"github.com/partite-ai/particles/internal/nodebuiltins"
	"github.com/partite-ai/particles/internal/semver"
	"github.com/partite-ai/particles/runtime"
)

// reportPhaseStart writes a "phase starting" line to the configured
// progress writer. No-op when Options.Progress is nil — library
// callers expect silent builds. The line is intentionally short:
// just the phase name + a colon, so the more interesting details
// (number of deps fetched, etc.) can land on a subsequent indented
// `reportPhaseDetail` line once known.
func reportPhaseStart(opts Options, phase Phase) {
	if opts.Progress == nil {
		return
	}
	fmt.Fprintf(opts.Progress, "%s:\n", phase)
}

// reportPhaseDetail writes one indented detail line under the
// already-printed phase header. Use after the phase completes
// (or to report intermediate progress within a long phase) — the
// phase header announces "we're working on X", the detail explains
// "and the result is Y".
func reportPhaseDetail(opts Options, format string, args ...any) {
	if opts.Progress == nil {
		return
	}
	fmt.Fprintf(opts.Progress, "  "+format+"\n", args...)
}

// withShadowDeps appends every entry in `nodebuiltins.ShadowNpmDeps`
// to a copy of `deps`. If a user already declared one explicitly
// (via `npm:<name>@<range>`), their declaration wins — the shadow
// gets skipped. The returned slice is fresh; the input is not
// mutated.
func withShadowDeps(deps []importscan.NpmSpec) []importscan.NpmSpec {
	declared := make(map[string]bool, len(deps))
	for _, d := range deps {
		declared[d.Name] = true
	}
	out := append([]importscan.NpmSpec(nil), deps...)
	for name, version := range nodebuiltins.ShadowNpmDeps {
		if declared[name] {
			continue
		}
		out = append(out, importscan.NpmSpec{
			Name:         name,
			VersionRange: version,
			Importer:     "<shadow>",
		})
	}
	return out
}

// Options configure a single build invocation.
type Options struct {
	// Source is the particle source tree (typically `os.DirFS(srcDir)`).
	Source fs.FS

	// EntryPoint is the FS-relative path to the particle's entry source.
	// Defaults to "Particlefile.ts" then "Particlefile.js" then
	// "Particlefile.py". Ignored when Component is set.
	EntryPoint string

	// NoTypeCheck skips Phase 3 (default: type-check on). Ignored
	// when Component is set (the wasm build has no typecheck phase).
	NoTypeCheck bool

	// Component, when non-empty, names an FS-relative path to a
	// prebuilt wasi:p2 component the user wants to package as a
	// particle. The component must implement `particle:runtime`
	// (tools + health + manifest). Setting this triggers the wasm
	// build path: the orchestrator instantiates the component, calls
	// get-manifest, and writes manifest.json + particle.wasm into
	// the artifact — no source compilation involved. Native
	// toolchains (cargo, TinyGo, ...) name their outputs how they
	// want; the build doesn't enforce a convention.
	Component string

	// Progress receives one line per pipeline phase entry and one
	// indented summary line per phase exit, in real time as the
	// build runs. Build is otherwise silent, so the CLI wires
	// `os.Stderr` (or `cmd.ErrOrStderr()`) here to make builds
	// visibly progress; library callers leave this nil to keep
	// builds silent, or pass any `io.Writer` (an `io.MultiWriter`,
	// a buffer, a structured logger's writer adapter, …) to capture.
	// Lines are pre-formatted plain text; we don't expose a
	// structured event API yet — keep this dumb until a user has a
	// concrete need.
	Progress io.Writer

	// CompilationCache, when non-nil, persists compiled wasm
	// modules across Build invocations. Pluggable on purpose: the
	// CLI builds a disk-backed cache in the user cache dir;
	// library callers either leave this nil (no caching — same
	// behavior the build has always had) or supply their own
	// `wazero.CompilationCache` (in-memory across long-lived host
	// processes, custom storage, …).
	CompilationCache wazero.CompilationCache
}

// Result is what Build returns on success.
type Result struct {
	// Particle is the in-memory artifact: a virtual fs.FS containing
	//
	//   manifest.json       — output of Phase 5
	//   bundle.mjs          — output of Phase 4
	//   bundle.mjs.map      — sourcemap (always emitted)
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
//
// Dispatches to a language-specific inner builder. When
// Options.Component is set the wasm path runs (no source
// compilation; the component IS the runtime). Otherwise the
// orchestrator picks JS or Python based on the entry-point file
// extension at the source root.
func Build(ctx context.Context, opts Options) (*Result, error) {
	if opts.Source == nil {
		return nil, &Error{Cause: errors.New("Options.Source is required")}
	}

	comps, err := wacogo.NewWithOptions(ctx, wacogo.Options{
		CompilationCache: opts.CompilationCache,
	})
	if err != nil {
		return nil, &Error{Cause: err}
	}
	defer comps.Close(ctx)

	if opts.Component != "" {
		return buildWasm(ctx, opts, comps)
	}

	entry, kind, err := resolveEntryPoint(opts.Source, opts.EntryPoint)
	if err != nil {
		return nil, &Error{Phase: PhaseImportScan, Cause: err}
	}

	switch kind {
	case entryKindJS:
		return buildJS(ctx, opts, comps, entry)
	case entryKindPython:
		return buildPython(ctx, opts, comps, entry)
	default:
		return nil, &Error{Cause: fmt.Errorf("unsupported entry kind for %q", entry)}
	}
}

// buildJS runs the JS/TS pipeline — the original six phases. Split
// out from Build so the Python path can sit alongside without
// language-conditional plumbing in every phase.
func buildJS(ctx context.Context, opts Options, comps *wacogo.Components, entry string) (*Result, error) {
	var (
		logs     []Log
		warnings []Diagnostic
	)

	// ---- Phase 1: import-scan -----------------------------------------
	reportPhaseStart(opts, PhaseImportScan)
	scan, err := importscan.Scan(opts.Source)
	if err != nil {
		return nil, &Error{Phase: PhaseImportScan, Logs: logs, Cause: err}
	}
	if len(scan.Errors) > 0 {
		return nil, scanErrorsAsBuildError(scan.Errors, logs)
	}
	reportPhaseDetail(opts, "%d npm dep%s declared, %d capabilit%s",
		len(scan.NpmDeps), plural(len(scan.NpmDeps), "", "s"),
		len(scan.Capabilities), plural(len(scan.Capabilities), "y", "ies"))

	// ---- Phase 2: resolve-and-fetch (only when needed) ----------------
	//
	// `withShadowDeps` injects any `nodebuiltins.ShadowNpmDeps` (e.g.
	// `punycode`) into the resolver input so transitive
	// `require(...)` calls in bundled packages find a real npm
	// package in node_modules rather than failing against a
	// runtime-builtin gap. The shadow deps go through the regular
	// fetch path; this isn't a fast-path or a stub.
	npmDeps := withShadowDeps(scan.NpmDeps)
	var nodeModules fs.FS
	var resolvedPkgs []wacogo.ResolvedPackage
	if len(npmDeps) > 0 {
		reportPhaseStart(opts, PhaseResolveAndFetch)
		rr, err := comps.ResolveAndFetch(ctx, npmDeps)
		logs = appendLog(logs, PhaseResolveAndFetch, rr)
		if err != nil {
			return nil, &Error{Phase: PhaseResolveAndFetch, Logs: logs, Cause: err}
		}
		nodeModules = rr.NodeModules
		resolvedPkgs = rr.Packages
		reportPhaseDetail(opts, "%d package%s resolved",
			len(resolvedPkgs), plural(len(resolvedPkgs), "", "s"))
	}

	// ---- Phase 3: typecheck (optional) --------------------------------
	if !opts.NoTypeCheck {
		reportPhaseStart(opts, PhaseTypecheck)
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
		reportPhaseDetail(opts, "%d diagnostic%s",
			len(cr.Diagnostics), plural(len(cr.Diagnostics), "", "s"))
	}

	// ---- Phase 4: bundle ----------------------------------------------
	reportPhaseStart(opts, PhaseBundle)
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
	reportPhaseDetail(opts, "%s", humanBytes(len(bundleResult.JS)))

	// ---- Phase 5: manifest-extract ------------------------------------
	// The runtime's particle:runtime/manifest export is the uniform
	// way every particle answers "describe yourself" — current
	// bundle-loading runtimes and future fully-WASM particles alike.
	reportPhaseStart(opts, PhaseManifestExtract)
	sourceFS := memfs.FS{"bundle.mjs": &memfs.File{Data: bundleResult.JS}}
	extracted, err := comps.ExtractManifest(ctx, runtime.RuntimeJS, sourceFS)
	if err != nil {
		return nil, &Error{Phase: PhaseManifestExtract, Logs: logs, Cause: err}
	}
	// Builder is the source of truth for `runtime` — the WIT record
	// returned by get-manifest doesn't carry it. Entry-point extension
	// drove the dispatch into buildJS, so the value is fixed here.
	extracted.Runtime = runtime.RuntimeJS
	if err := validateExtractedManifest(extracted); err != nil {
		return nil, &Error{Phase: PhaseManifestExtract, Logs: logs, Cause: err}
	}
	// Compact JSON: keeps manifest.json byte-diff-stable across runs
	// and easy to substring-match in tests.
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
	buildInfo, err := encodeBuildInfo(scan, resolvedPkgs)
	if err != nil {
		return nil, &Error{Phase: PhaseAssemble, Logs: logs, Cause: fmt.Errorf("encode build-info: %w", err)}
	}

	particle := assembleParticleFS(particleFiles{
		Manifest:  manifestJSON,
		Bundle:    bundleResult.JS,
		Sourcemap: bundleResult.Sourcemap,
		BuildInfo: buildInfo,
	})
	reportPhaseDetail(opts, "%s", artifactSummary(particle))

	return &Result{
		Particle: particle,
		Warnings: warnings,
		Logs:     logs,
	}, nil
}

// validateExtractedManifest runs the Go-side cross-field gates on
// the typed manifest returned by the runtime. Two checks:
//
//  1. SemVer 2.0.0 on `version` — shared with the registry via
//     `internal/semver`, so a tarball that bypasses the build can't
//     slip a bad version past `registry.Put` either.
//  2. Every host listed under `credentials.<name>.hosts` must also
//     appear in `capabilities.http.allowedHosts` — a credential
//     bound to a host the particle can't reach is a layering bug.
//
// Enumeration shape (recognized runtimes / capability categories /
// credential-method types) is enforced by the WIT contract itself:
// the runtime's get-manifest can only return records that match the
// typed shape, so anything out of band fails inside the runtime with
// `invalid-manifest` before reaching this layer.
func validateExtractedManifest(m *runtime.Manifest) error {
	if !semver.IsValid(m.Version) {
		return fmt.Errorf("particle.version %q is not a valid semver string (e.g. \"1.2.3\", \"0.1.0-rc.1\", \"1.0.0+build.7\")", m.Version)
	}
	allowed := make(map[string]struct{}, len(m.Capabilities.HTTP.AllowedHosts))
	for _, h := range m.Capabilities.HTTP.AllowedHosts {
		allowed[strings.ToLower(h)] = struct{}{}
	}
	for credName, cred := range m.Credentials {
		for _, h := range cred.Hosts {
			if _, ok := allowed[strings.ToLower(h)]; !ok {
				return fmt.Errorf("credentials.%s.hosts: %q is not in capabilities.http.allowedHosts — add it there or remove it from this credential", credName, h)
			}
		}
	}
	return nil
}

// entryKind names the source language of a particle's entry point.
// Determines which set of pipeline phases the build runs (JS vs
// Python). RuntimeKind on the emitted manifest is the same value
// rendered as a string.
type entryKind int

const (
	entryKindJS entryKind = iota + 1
	entryKindPython
)

func (k entryKind) String() string {
	switch k {
	case entryKindJS:
		return "js"
	case entryKindPython:
		return "python"
	default:
		return "unknown"
	}
}

// resolveEntryPoint picks the conventional entry point and classifies
// the source language. Spec §4: conventional entry is
// `Particlefile.{js,ts}`; the Python addition extends that with
// `Particlefile.py`. The user can override via opts.EntryPoint.
func resolveEntryPoint(fsys fs.FS, override string) (string, entryKind, error) {
	if override != "" {
		if _, err := fs.Stat(fsys, override); err != nil {
			return "", 0, fmt.Errorf("entry point %q: %w", override, err)
		}
		return override, kindFromExt(override), nil
	}
	// Try in priority order. TS first (richer DX), then JS, then PY.
	// Listing JS before PY keeps existing JS particles unchanged.
	for _, candidate := range []string{"Particlefile.ts", "Particlefile.js", "Particlefile.py"} {
		if _, err := fs.Stat(fsys, candidate); err == nil {
			return candidate, kindFromExt(candidate), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", 0, err
		}
	}
	return "", 0, errors.New("no Particlefile.{ts,js,py} found at the source root")
}

// kindFromExt classifies an entry path by extension. The default
// (.ts/.js/anything else) is JS so existing particles with unusual
// extensions don't fail unexpectedly.
func kindFromExt(path string) entryKind {
	if strings.HasSuffix(path, ".py") {
		return entryKindPython
	}
	return entryKindJS
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
// Progress / formatting helpers
// -----------------------------------------------------------------------------

// plural picks the right English suffix for `n` — `singular` when
// n==1, otherwise `plural`. Used inline in Sprintf with the count.
// We pass both forms because some words pluralize as `s` (`wheel`/
// `wheels`) and others as `ies` (`dependency`/`dependencies`).
func plural(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

// humanBytes formats a byte count as a short human-readable string.
// Two significant digits, KB / MB cutoffs at 1024. Used by progress
// summaries — we deliberately avoid an external dep for ~10 lines
// of formatting.
func humanBytes(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}

// artifactSummary returns a one-line description of the artifact FS
// — file count + total uncompressed size. Used in the Phase-6
// progress line so the user sees what was built.
func artifactSummary(particle fs.FS) string {
	var files, bytes int
	_ = fs.WalkDir(particle, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		files++
		info, err := d.Info()
		if err == nil {
			bytes += int(info.Size())
		}
		return nil
	})
	return fmt.Sprintf("%d file%s, %s",
		files, plural(files, "", "s"), humanBytes(bytes))
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
	mfs := memfs.FS{
		"manifest.json":   &memfs.File{Data: in.Manifest, Mode: 0o644},
		"bundle.mjs":      &memfs.File{Data: in.Bundle, Mode: 0o644},
		"build-info.json": &memfs.File{Data: in.BuildInfo, Mode: 0o644},
	}
	if len(in.Sourcemap) > 0 {
		mfs["bundle.mjs.map"] = &memfs.File{Data: in.Sourcemap, Mode: 0o644}
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
