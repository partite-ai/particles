// Package importscan walks a particle source tree and extracts every
// `import` and `import()` specifier from `.js` / `.ts` / `.jsx` / `.tsx`
// files using esbuild as a parser.
//
// Output: the npm: dep requests (name + version range + optional subpath),
// the particle:* capabilities used, and structured errors for the four
// disallowed forms (bare specifier, npm: without version, computed import,
// local import that escapes the source tree).
//
// The scan operates on a `fs.FS` rooted at the source tree — no host paths
// leak in, and tests can use `fstest.MapFS` instead of touching disk. We
// drive esbuild via a custom namespace + OnLoad so esbuild never reads from
// the underlying disk either; the metafile becomes the source of truth.
//
// Spec: docs/initial-design.md §5 "Phase 1: import-scan".
package importscan

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// NpmSpec is one resolved-and-validated npm: import found in the source.
type NpmSpec struct {
	Name         string // "lodash" or "@scope/name"
	VersionRange string // e.g. "^4.17.0"
	Subpath      string // "" or e.g. "/get"
	Importer     string // FS-relative path of the file containing the import
}

// LocalImport is a relative or absolute file import inside the source tree.
type LocalImport struct {
	Resolved string // FS-relative path of the imported file
	Importer string // FS-relative path of the file containing the import
}

// ErrorKind enumerates the disallowed import shapes.
type ErrorKind int

const (
	ErrBareSpecifier  ErrorKind = iota // `import x from "foo"` — no `npm:` prefix
	ErrMissingVersion                  // `import x from "npm:foo"` — no `@version`
	ErrComputedImport                  // `import(name + ".js")` — non-string-literal
	ErrOutsideTree                     // `import x from "../../etc/passwd"`
	ErrUnknownPrefix                   // `import x from "weird:thing"`
)

// Error is a per-import scan error. The scan does not abort on the first one;
// callers decide whether to fail by checking len(Result.Errors).
type Error struct {
	Kind    ErrorKind
	Message string
	File    string // FS-relative path
	Line    int    // 1-based; 0 if unknown
	Column  int    // 1-based; 0 if unknown
}

func (e Error) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("%s:%d:%d: %s", e.File, e.Line, e.Column, e.Message)
	}
	if e.File != "" {
		return fmt.Sprintf("%s: %s", e.File, e.Message)
	}
	return e.Message
}

// Result is what Scan produces.
type Result struct {
	NpmDeps      []NpmSpec
	Capabilities []string // sorted, unique (e.g. "credentials", "kv")
	Locals       []LocalImport
	Errors       []Error
}

// Scan walks fsys, parses every JS/TS file, and returns the import inventory.
// Paths in the result are relative to the FS root.
//
// The returned `error` is reserved for hard failures (esbuild internal
// failure); per-import problems live in Result.Errors.
func Scan(fsys fs.FS) (*Result, error) {
	entryPoints, err := findSourceFiles(fsys)
	if err != nil {
		return nil, err
	}
	if len(entryPoints) == 0 {
		return &Result{}, nil
	}

	scan := &scanState{fsys: fsys}

	build := api.Build(api.BuildOptions{
		EntryPoints: entryPoints,
		Bundle:      true,
		Write:       false,
		LogLevel:    api.LogLevelSilent,
		Metafile:    true,
		Platform:    api.PlatformNeutral,
		Plugins:     []api.Plugin{scan.plugin()},
		// Outdir/Outfile is required when Bundle is true; nothing is
		// actually written (Write: false).
		Outdir: "/dev/null/importscan-noop",
	})

	r := &Result{}
	seenCap := map[string]struct{}{}
	seenLocal := map[string]struct{}{}
	seenNpm := map[string]struct{}{}

	if build.Metafile == "" {
		var msgs []string
		for _, e := range build.Errors {
			msgs = append(msgs, e.Text)
		}
		return nil, fmt.Errorf("esbuild produced no metafile: %s", strings.Join(msgs, "; "))
	}

	var meta metafile
	if err := json.Unmarshal([]byte(build.Metafile), &meta); err != nil {
		return nil, fmt.Errorf("parse metafile: %w", err)
	}

	for inputPath, input := range meta.Inputs {
		// Strip our namespace prefix; what's left is FS-relative.
		importer := strings.TrimPrefix(inputPath, namespacePrefix)
		for _, imp := range input.Imports {
			classifyImport(importer, imp, r, seenCap, seenLocal, seenNpm)
		}
	}

	for _, w := range build.Warnings {
		if isComputedImportWarning(w.Text) {
			e := Error{
				Kind:    ErrComputedImport,
				Message: "dynamic import() must use a string literal — computed specifiers are not supported",
			}
			if w.Location != nil {
				e.File = strings.TrimPrefix(w.Location.File, namespacePrefix)
				e.Line = w.Location.Line
				e.Column = w.Location.Column
			}
			r.Errors = append(r.Errors, e)
		}
	}

	finalize(r, seenCap)
	return r, nil
}

// -----------------------------------------------------------------------------
// scan state + esbuild plugin
// -----------------------------------------------------------------------------

const namespace = "particle-fs"

// esbuild's metafile renders namespaced inputs as "<namespace>:<path>". We
// strip this prefix before exposing paths to callers.
const namespacePrefix = namespace + ":"

type scanState struct {
	fsys fs.FS
}

func (s *scanState) plugin() api.Plugin {
	return api.Plugin{
		Name: "particle-importscan",
		Setup: func(b api.PluginBuild) {
			// Resolve every specifier through us. Entry points enter our
			// namespace so OnLoad serves them from fs.FS; everything else
			// is marked External so esbuild never tries to read it.
			b.OnResolve(api.OnResolveOptions{Filter: ".*"}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
				if args.Kind == api.ResolveEntryPoint {
					return api.OnResolveResult{
						Path:      args.Path,
						Namespace: namespace,
					}, nil
				}
				return api.OnResolveResult{External: true}, nil
			})

			// Read source from fsys for files in our namespace.
			b.OnLoad(api.OnLoadOptions{Filter: ".*", Namespace: namespace}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
				data, err := fs.ReadFile(s.fsys, args.Path)
				if err != nil {
					return api.OnLoadResult{}, fmt.Errorf("read %s from fs.FS: %w", args.Path, err)
				}
				contents := string(data)
				return api.OnLoadResult{
					Contents: &contents,
					Loader:   loaderForExt(path.Ext(args.Path)),
				}, nil
			})
		},
	}
}

func loaderForExt(ext string) api.Loader {
	switch ext {
	case ".ts":
		return api.LoaderTS
	case ".tsx":
		return api.LoaderTSX
	case ".jsx":
		return api.LoaderJSX
	default:
		return api.LoaderJS
	}
}

// -----------------------------------------------------------------------------
// classification
// -----------------------------------------------------------------------------

func classifyImport(
	importer string,
	imp metaImport,
	r *Result,
	seenCap, seenLocal, seenNpm map[string]struct{},
) {
	p := imp.Path
	switch {
	case strings.HasPrefix(p, "npm:"):
		key := importer + "\x00" + p
		if _, ok := seenNpm[key]; ok {
			return
		}
		seenNpm[key] = struct{}{}
		name, version, subpath, ok := parseNpmSpec(p)
		if !ok {
			r.Errors = append(r.Errors, Error{
				Kind:    ErrBareSpecifier,
				Message: fmt.Sprintf("malformed npm specifier %q", p),
				File:    importer,
			})
			return
		}
		if version == "" {
			r.Errors = append(r.Errors, Error{
				Kind:    ErrMissingVersion,
				Message: fmt.Sprintf("npm specifier %q is missing a version range (e.g. \"npm:%s@^1.0.0\")", p, name),
				File:    importer,
			})
			return
		}
		r.NpmDeps = append(r.NpmDeps, NpmSpec{
			Name:         name,
			VersionRange: version,
			Subpath:      subpath,
			Importer:     importer,
		})

	case strings.HasPrefix(p, "particle:"):
		seenCap[strings.TrimPrefix(p, "particle:")] = struct{}{}

	case strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/"):
		// Resolve against the importer's directory using forward-slash
		// path arithmetic (FS paths are always /-separated regardless of OS).
		var resolved string
		if strings.HasPrefix(p, "/") {
			resolved = path.Clean(strings.TrimPrefix(p, "/"))
		} else {
			resolved = path.Clean(path.Join(path.Dir(importer), p))
		}
		// fs.FS roots don't have a leading "/"; anything that resolves to
		// "." or starts with ".." escapes the source tree.
		if resolved == "" || resolved == "." || strings.HasPrefix(resolved, "..") {
			r.Errors = append(r.Errors, Error{
				Kind:    ErrOutsideTree,
				Message: fmt.Sprintf("local import %q resolves outside the source tree", p),
				File:    importer,
			})
			return
		}
		key := resolved + "\x00" + importer
		if _, ok := seenLocal[key]; ok {
			return
		}
		seenLocal[key] = struct{}{}
		r.Locals = append(r.Locals, LocalImport{Resolved: resolved, Importer: importer})

	case strings.Contains(p, ":"):
		r.Errors = append(r.Errors, Error{
			Kind:    ErrUnknownPrefix,
			Message: fmt.Sprintf("unsupported import scheme: %q (only npm:, particle:, ./, /)", p),
			File:    importer,
		})

	default:
		r.Errors = append(r.Errors, Error{
			Kind:    ErrBareSpecifier,
			Message: fmt.Sprintf("bare specifier %q — npm packages must use the `npm:` prefix (e.g. \"npm:%s@^1.0.0\")", p, p),
			File:    importer,
		})
	}
}

// -----------------------------------------------------------------------------
// metafile JSON shape
// -----------------------------------------------------------------------------

type metafile struct {
	Inputs map[string]metaInput `json:"inputs"`
}

type metaInput struct {
	Imports []metaImport `json:"imports"`
}

type metaImport struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

// findSourceFiles returns FS-relative paths of every .js/.ts/.jsx/.tsx
// file under the FS root, excluding node_modules and dot-directories.
func findSourceFiles(fsys fs.FS) ([]string, error) {
	var files []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "node_modules" || (strings.HasPrefix(name, ".") && p != ".") {
				return fs.SkipDir
			}
			return nil
		}
		switch path.Ext(d.Name()) {
		case ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs":
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk source tree: %w", err)
	}
	return files, nil
}

// parseNpmSpec splits an "npm:..." import into (name, version, subpath).
// Returns ok=false only for syntactically malformed specs; an empty version
// is a valid parse outcome (the caller raises ErrMissingVersion).
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

func isComputedImportWarning(text string) bool {
	t := strings.ToLower(text)
	return strings.Contains(t, "argument is not a string literal") ||
		strings.Contains(t, "could not be statically analyzed")
}

func finalize(r *Result, seenCap map[string]struct{}) {
	caps := make([]string, 0, len(seenCap))
	for c := range seenCap {
		caps = append(caps, c)
	}
	sort.Strings(caps)
	r.Capabilities = caps

	sort.Slice(r.NpmDeps, func(i, j int) bool {
		a, b := r.NpmDeps[i], r.NpmDeps[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.VersionRange < b.VersionRange
	})

	sort.Slice(r.Locals, func(i, j int) bool {
		return r.Locals[i].Resolved < r.Locals[j].Resolved
	})

	sort.Slice(r.Errors, func(i, j int) bool {
		a, b := r.Errors[i], r.Errors[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Message < b.Message
	})
}
