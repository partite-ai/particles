// Package bundle wraps esbuild's Go API to produce a single-file ESM
// bundle of a particle's source.
//
// Inputs are an `fs.FS` containing both the particle source and a
// node_modules tree (the orchestrator builds this — typically by extracting
// the deno-npm resolver's tarballs into a virtual or on-disk layout) and
// a path within that FS pointing at the entry source.
//
// The bundle:
//   - rewrites every `npm:name@range[/sub]` to a bare-specifier resolution
//     against the FS's node_modules tree
//   - resolves bare specifiers by walking node_modules / handling
//     package.json exports / main / module fields
//   - leaves `particle:*` (and any caller-supplied externals) untouched
//
// The whole resolution + load path runs against `fs.FS`. esbuild's default
// disk-touching resolver and loader are bypassed entirely — every file
// esbuild reads comes from `OnLoad`, every path it asks about comes from
// our `OnResolve`. No tempdir materialization.
//
// Spec: docs/initial-design.md §5 "Phase 4: bundle".
package bundle

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// Options describe a single bundle invocation.
type Options struct {
	// FS contains both the particle source and a node_modules tree at the
	// path given by NodeModulesPath (default: "node_modules").
	FS fs.FS

	// EntryPoint is the FS-relative path to the particle's entry source
	// (e.g. "src/Particlefile.ts").
	EntryPoint string

	// NodeModulesPath is the FS-relative path to the node_modules dir.
	// Defaults to "node_modules".
	NodeModulesPath string

	// Sourcemap controls whether a sourcemap is produced. When true, the
	// sourcemap is returned in Result.Sourcemap and a `//# sourceMappingURL`
	// comment is added to the JS output.
	Sourcemap bool

	// Minify shrinks the output (whitespace, identifiers, syntax).
	Minify bool

	// Externals are import paths to leave untouched. `particle:*` is always
	// added. Each entry can be an exact specifier ("foo") or a `prefix:*`
	// glob ("node:*").
	Externals []string
}

// Result is the bundle output. JS is always populated on success; Sourcemap
// is populated only when Options.Sourcemap is true.
type Result struct {
	JS        []byte
	Sourcemap []byte
	// Metafile is esbuild's metafile JSON.
	Metafile []byte
	// Warnings are non-fatal diagnostics.
	Warnings []Diagnostic
}

// Diagnostic is a structured location-aware message from esbuild.
type Diagnostic struct {
	File    string
	Line    int
	Column  int
	Message string
}

func (d Diagnostic) Error() string {
	if d.Line > 0 {
		return fmt.Sprintf("%s:%d:%d: %s", d.File, d.Line, d.Column, d.Message)
	}
	if d.File != "" {
		return fmt.Sprintf("%s: %s", d.File, d.Message)
	}
	return d.Message
}

// Error groups one or more bundle errors. It implements `error` so a single
// caller-visible value covers the multi-error case naturally.
type Error struct {
	Diagnostics []Diagnostic
}

func (e *Error) Error() string {
	if len(e.Diagnostics) == 0 {
		return "bundle failed"
	}
	if len(e.Diagnostics) == 1 {
		return "bundle failed: " + e.Diagnostics[0].Error()
	}
	parts := make([]string, len(e.Diagnostics))
	for i, d := range e.Diagnostics {
		parts[i] = d.Error()
	}
	return "bundle failed:\n  " + strings.Join(parts, "\n  ")
}

// Bundle runs esbuild against opts.FS using a custom resolver that walks
// the FS for both source and node_modules.
func Bundle(opts Options) (*Result, error) {
	if opts.FS == nil {
		return nil, fmt.Errorf("bundle: FS is required")
	}
	if opts.EntryPoint == "" {
		return nil, fmt.Errorf("bundle: EntryPoint is required")
	}
	nmPath := opts.NodeModulesPath
	if nmPath == "" {
		nmPath = "node_modules"
	}

	resolver := &fsResolver{
		fsys:         opts.FS,
		nodeModules:  path.Clean(nmPath),
		externals:    opts.Externals,
		pkgJsonCache: map[string]*packageJSON{},
	}

	sourcemap := api.SourceMapNone
	if opts.Sourcemap {
		sourcemap = api.SourceMapLinked
	}

	build := api.Build(api.BuildOptions{
		EntryPoints:       []string{path.Clean(opts.EntryPoint)},
		Bundle:            true,
		Write:             false,
		Format:            api.FormatESModule,
		Platform:          api.PlatformNeutral,
		Sourcemap:         sourcemap,
		Metafile:          true,
		LogLevel:          api.LogLevelSilent,
		Outfile:           "bundle.js",
		MinifyWhitespace:  opts.Minify,
		MinifyIdentifiers: opts.Minify,
		MinifySyntax:      opts.Minify,
		Plugins:           []api.Plugin{resolver.plugin()},
	})

	if len(build.Errors) > 0 {
		return nil, &Error{Diagnostics: toDiagnostics(build.Errors)}
	}

	r := &Result{
		Warnings: toDiagnostics(build.Warnings),
		Metafile: []byte(build.Metafile),
	}
	for _, f := range build.OutputFiles {
		switch {
		case strings.HasSuffix(f.Path, ".js"):
			r.JS = f.Contents
		case strings.HasSuffix(f.Path, ".js.map"):
			r.Sourcemap = f.Contents
		}
	}
	if r.JS == nil {
		return nil, &Error{Diagnostics: []Diagnostic{{
			Message: "esbuild produced no JS output",
		}}}
	}
	return r, nil
}

// =============================================================================
// fs.FS-backed resolver + loader
// =============================================================================

const sourceNamespace = "particle-fs"

type fsResolver struct {
	fsys         fs.FS
	nodeModules  string   // FS-relative path
	externals    []string // exact or "prefix:*"
	pkgJsonCache map[string]*packageJSON
}

// packageJSON is the subset of fields we read for resolution.
type packageJSON struct {
	Type    string          `json:"type"`
	Main    string          `json:"main"`
	Module  string          `json:"module"`
	Exports json.RawMessage `json:"exports"` // string | object
}

func (r *fsResolver) plugin() api.Plugin {
	return api.Plugin{
		Name: "particle-fs-resolver",
		Setup: func(b api.PluginBuild) {
			b.OnResolve(api.OnResolveOptions{Filter: ".*"}, r.onResolve)
			b.OnLoad(api.OnLoadOptions{Filter: ".*", Namespace: sourceNamespace}, r.onLoad)
		},
	}
}

func (r *fsResolver) onResolve(args api.OnResolveArgs) (api.OnResolveResult, error) {
	spec := args.Path

	// Externals (caller-supplied + the always-on particle:*).
	if r.matchesExternal(spec) {
		return api.OnResolveResult{External: true}, nil
	}

	// `npm:name@range[/sub]` → strip prefix, treat as bare.
	if strings.HasPrefix(spec, "npm:") {
		name, _, subpath, ok := parseNpmSpec(spec)
		if !ok || name == "" {
			return api.OnResolveResult{}, fmt.Errorf("malformed npm specifier %q", spec)
		}
		spec = name + subpath
	}

	// Entry points come in as already-FS-relative paths from our caller.
	if args.Kind == api.ResolveEntryPoint {
		return api.OnResolveResult{
			Path:      path.Clean(spec),
			Namespace: sourceNamespace,
		}, nil
	}

	switch {
	case strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../"):
		base := path.Dir(args.Importer)
		resolved := r.resolveAsFileOrDir(path.Join(base, spec))
		if resolved == "" {
			return api.OnResolveResult{}, fmt.Errorf("cannot resolve %q from %q", args.Path, args.Importer)
		}
		return api.OnResolveResult{Path: resolved, Namespace: sourceNamespace}, nil

	case strings.HasPrefix(spec, "/"):
		// Treat absolute as FS-rooted (no leading slash in fs.FS land).
		resolved := r.resolveAsFileOrDir(strings.TrimPrefix(spec, "/"))
		if resolved == "" {
			return api.OnResolveResult{}, fmt.Errorf("cannot resolve %q from %q", args.Path, args.Importer)
		}
		return api.OnResolveResult{Path: resolved, Namespace: sourceNamespace}, nil

	default:
		// Bare specifier (e.g. "lodash", "@scope/pkg", "lodash/get").
		resolved := r.resolveBare(spec)
		if resolved == "" {
			return api.OnResolveResult{}, fmt.Errorf("cannot resolve %q from %q", args.Path, args.Importer)
		}
		return api.OnResolveResult{Path: resolved, Namespace: sourceNamespace}, nil
	}
}

func (r *fsResolver) onLoad(args api.OnLoadArgs) (api.OnLoadResult, error) {
	data, err := fs.ReadFile(r.fsys, args.Path)
	if err != nil {
		return api.OnLoadResult{}, fmt.Errorf("read %s: %w", args.Path, err)
	}
	contents := string(data)
	return api.OnLoadResult{
		Contents: &contents,
		Loader:   loaderForExt(path.Ext(args.Path)),
	}, nil
}

// -----------------------------------------------------------------------------
// resolution primitives
// -----------------------------------------------------------------------------

// resolveAsFileOrDir tries `candidate` as a file (with extension hunting),
// then as a directory (package.json's exports/module/main, then `index.*`).
func (r *fsResolver) resolveAsFileOrDir(candidate string) string {
	if resolved := r.resolveAsFile(candidate); resolved != "" {
		return resolved
	}
	if pkg, ok := r.readPackageJSON(candidate); ok {
		if entry := r.entryFromPackageJSON(pkg, candidate, "."); entry != "" {
			return entry
		}
	}
	return r.resolveAsFile(path.Join(candidate, "index"))
}

// resolveAsFile returns the FS-relative path to `candidate` if it exists as
// a file, otherwise tries each known extension in order. Returns "" on miss.
func (r *fsResolver) resolveAsFile(candidate string) string {
	candidate = path.Clean(candidate)
	if r.isFile(candidate) {
		return candidate
	}
	for _, ext := range []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".json"} {
		if r.isFile(candidate + ext) {
			return candidate + ext
		}
	}
	return ""
}

// resolveBare maps a bare specifier ("lodash", "@scope/pkg/sub") to a file
// inside r.nodeModules. There is no parent-directory walk: our node_modules
// is flat and rooted at r.nodeModules.
func (r *fsResolver) resolveBare(spec string) string {
	pkgName, subpath := splitBareSpecifier(spec)
	pkgDir := path.Join(r.nodeModules, pkgName)

	if subpath != "" {
		// `lodash/get` → look for the subpath under pkgDir; consult
		// package.json exports first if it specifies the subpath.
		if pkg, ok := r.readPackageJSON(pkgDir); ok && len(pkg.Exports) > 0 {
			if entry := r.entryFromPackageJSON(pkg, pkgDir, "."+subpath); entry != "" {
				return entry
			}
		}
		return r.resolveAsFileOrDir(path.Join(pkgDir, subpath))
	}

	pkg, ok := r.readPackageJSON(pkgDir)
	if !ok {
		// No package.json — fall through to index file lookup.
		return r.resolveAsFile(path.Join(pkgDir, "index"))
	}
	if entry := r.entryFromPackageJSON(pkg, pkgDir, "."); entry != "" {
		return entry
	}
	return r.resolveAsFile(path.Join(pkgDir, "index"))
}

// entryFromPackageJSON consults exports → module → main, in that order, and
// resolves the picked path against pkgDir.
func (r *fsResolver) entryFromPackageJSON(pkg *packageJSON, pkgDir, subpath string) string {
	if len(pkg.Exports) > 0 {
		if entry := resolveExports(pkg.Exports, subpath); entry != "" {
			return r.resolveAsFile(path.Join(pkgDir, entry))
		}
	}
	if subpath != "." {
		return ""
	}
	if pkg.Module != "" {
		if e := r.resolveAsFile(path.Join(pkgDir, pkg.Module)); e != "" {
			return e
		}
	}
	if pkg.Main != "" {
		if e := r.resolveAsFile(path.Join(pkgDir, pkg.Main)); e != "" {
			return e
		}
	}
	return ""
}

// resolveExports handles the `exports` field of package.json. Supports:
//   - string form  "exports": "./index.js"
//   - subpath form "exports": { ".": "./index.js", "./sub": "./sub.js" }
//   - conditional  "exports": { "import": "./esm.js", "default": "./index.js" }
//   - subpath + conditional: "exports": { ".": { "import": "./esm.js" } }
//
// Only the `import` and `default` conditions are honored (we always emit
// ESM; `require`/`browser`/`node` are skipped).
func resolveExports(raw json.RawMessage, subpath string) string {
	// String form.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if subpath == "." {
			return s
		}
		return ""
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}

	hasSubpath := false
	hasCondition := false
	for k := range obj {
		if strings.HasPrefix(k, ".") {
			hasSubpath = true
		} else {
			hasCondition = true
		}
	}

	if hasSubpath {
		if entry, ok := obj[subpath]; ok {
			return resolveConditionalExport(entry)
		}
		return ""
	}
	if hasCondition && subpath == "." {
		return resolveConditionalExport(raw)
	}
	return ""
}

// resolveConditionalExport unwraps nested conditional objects, picking the
// first matching condition (`import` then `default`).
func resolveConditionalExport(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	for _, cond := range []string{"import", "default"} {
		if v, ok := obj[cond]; ok {
			if e := resolveConditionalExport(v); e != "" {
				return e
			}
		}
	}
	return ""
}

// splitBareSpecifier splits a bare spec into (packageName, subpath).
//
//	"lodash"        → ("lodash", "")
//	"lodash/get"    → ("lodash", "/get")
//	"@scope/pkg"    → ("@scope/pkg", "")
//	"@scope/pkg/x"  → ("@scope/pkg", "/x")
func splitBareSpecifier(spec string) (name, subpath string) {
	if strings.HasPrefix(spec, "@") {
		slash := strings.IndexByte(spec[1:], '/')
		if slash < 0 {
			return spec, ""
		}
		afterScope := 1 + slash + 1
		nextSlash := strings.IndexByte(spec[afterScope:], '/')
		if nextSlash < 0 {
			return spec, ""
		}
		return spec[:afterScope+nextSlash], spec[afterScope+nextSlash:]
	}
	slash := strings.IndexByte(spec, '/')
	if slash < 0 {
		return spec, ""
	}
	return spec[:slash], spec[slash:]
}

// -----------------------------------------------------------------------------
// FS access helpers
// -----------------------------------------------------------------------------

func (r *fsResolver) isFile(p string) bool {
	info, err := fs.Stat(r.fsys, p)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

func (r *fsResolver) readPackageJSON(pkgDir string) (*packageJSON, bool) {
	pkgDir = path.Clean(pkgDir)
	if pkg, hit := r.pkgJsonCache[pkgDir]; hit {
		return pkg, pkg != nil
	}
	data, err := fs.ReadFile(r.fsys, path.Join(pkgDir, "package.json"))
	if err != nil {
		r.pkgJsonCache[pkgDir] = nil
		return nil, false
	}
	var pkg packageJSON
	if err := json.Unmarshal(data, &pkg); err != nil {
		r.pkgJsonCache[pkgDir] = nil
		return nil, false
	}
	r.pkgJsonCache[pkgDir] = &pkg
	return &pkg, true
}

func (r *fsResolver) matchesExternal(spec string) bool {
	if strings.HasPrefix(spec, "particle:") {
		return true
	}
	for _, ext := range r.externals {
		if strings.HasSuffix(ext, ":*") {
			prefix := strings.TrimSuffix(ext, ":*")
			if strings.HasPrefix(spec, prefix+":") {
				return true
			}
		} else if ext == spec {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------------
// extension → loader
// -----------------------------------------------------------------------------

func loaderForExt(ext string) api.Loader {
	switch ext {
	case ".ts":
		return api.LoaderTS
	case ".tsx":
		return api.LoaderTSX
	case ".jsx":
		return api.LoaderJSX
	case ".json":
		return api.LoaderJSON
	default:
		return api.LoaderJS
	}
}

// -----------------------------------------------------------------------------
// npm: spec parsing (duplicated from importscan to keep this pkg standalone)
// -----------------------------------------------------------------------------

func parseNpmSpec(spec string) (name, version, subpath string, ok bool) {
	if !strings.HasPrefix(spec, "npm:") {
		return "", "", "", false
	}
	s := spec[len("npm:"):]
	if s == "" {
		return "", "", "", false
	}
	var nameEnd int
	if strings.HasPrefix(s, "@") {
		slash := strings.IndexByte(s[1:], '/')
		if slash < 0 {
			return "", "", "", false
		}
		afterScope := 1 + slash + 1
		next := strings.IndexAny(s[afterScope:], "@/")
		if next < 0 {
			return s, "", "", true
		}
		nameEnd = afterScope + next
	} else {
		next := strings.IndexAny(s, "@/")
		if next < 0 {
			return s, "", "", true
		}
		nameEnd = next
	}
	name = s[:nameEnd]
	rest := s[nameEnd:]
	if strings.HasPrefix(rest, "@") {
		afterAt := rest[1:]
		if slash := strings.IndexByte(afterAt, '/'); slash >= 0 {
			version = afterAt[:slash]
			subpath = afterAt[slash:]
		} else {
			version = afterAt
		}
	} else {
		subpath = rest
	}
	return name, version, subpath, true
}

func toDiagnostics(msgs []api.Message) []Diagnostic {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]Diagnostic, len(msgs))
	for i, m := range msgs {
		d := Diagnostic{Message: m.Text}
		if m.Location != nil {
			d.File = m.Location.File
			d.Line = m.Location.Line
			d.Column = m.Location.Column
		}
		out[i] = d
	}
	return out
}
