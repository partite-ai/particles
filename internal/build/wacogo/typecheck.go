package wacogo

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync"
	"testing/fstest"
	"time"

	wc "github.com/partite-ai/wacogo"
	"github.com/partite-ai/wacogo/wasi"
	"github.com/partite-ai/wacogo/wasi/filesystem/preopens"
)

// typecheckInterface is the canonical id of the exported instance the
// typecheck component publishes — see components/typecheck/wit/typecheck.wit.
const typecheckInterface = "particle:build/typecheck@0.1.0"

// Severity mirrors the WIT `severity` enum.
type Severity int

const (
	SeverityError Severity = iota + 1
	SeverityWarning
	SeverityInfo
)

// Diagnostic is one diagnostic returned by the type-check phase.
type Diagnostic struct {
	File     string
	Line     int
	Column   int
	Severity Severity
	Code     uint32
	Message  string
}

// CheckResult is what Components.TypeCheck returns on success.
type CheckResult struct {
	Diagnostics []Diagnostic
	Stderr      []byte
}

// TypeCheck runs the bundled TypeScript compiler against `source` and
// (optionally) `nodeModules`, mounting both under a wasi:filesystem
// preopen at `src/` and `node_modules/` respectively. Returns the
// flattened diagnostic list.
//
// nodeModules may be nil when the particle has no npm: imports.
func (c *Components) TypeCheck(ctx context.Context, source, nodeModules fs.FS) (*CheckResult, error) {
	if source == nil {
		return nil, fmt.Errorf("typecheck: source FS is required")
	}
	typecheck, err := c.loadEmbedded(ctx, c.typecheck)
	if err != nil {
		return nil, err
	}

	mounted := mountFS{source: source, nodeModules: nodeModules}
	rootFiles, err := findTSRoots(source)
	if err != nil {
		return nil, fmt.Errorf("walk source for type-check roots: %w", err)
	}

	stderrBuf := &bytes.Buffer{}
	w, err := wasi.NewWorld(ctx, c.engine, &wasi.Config{
		Args:     []string{"particle-typecheck"},
		Preopens: preopens.NewFSPreopens(preopens.ImmutableFS{FS: mounted}),
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

	inst, err := typecheck.Instantiate(ctx, imports...)
	if err != nil {
		return nil, withStderr(err, stderrBuf, "instantiate typecheck")
	}
	defer inst.Close(ctx)

	iface := inst.ExportedInstance(typecheckInterface)
	if iface == nil {
		return nil, fmt.Errorf("typecheck component does not export instance %q", typecheckInterface)
	}
	fn := iface.ExportedFunc("check")
	if fn == nil {
		return nil, fmt.Errorf("typecheck.%s does not export check()", typecheckInterface)
	}

	rootList := make([]wc.Val, len(rootFiles))
	for i, p := range rootFiles {
		rootList[i] = wc.ValString(p)
	}
	opts := wc.NewValRecord(
		field("root-files", wc.NewValListOf[wc.Val](rootList...)),
		field("strict", wc.ValBool(true)),
		field("target", wc.ValString("ES2022")),
	)

	results, err := fn.Call(ctx, opts)
	if err != nil {
		return &CheckResult{Stderr: stderrBuf.Bytes()},
			withStderr(err, stderrBuf, "call typecheck.check")
	}
	if len(results) != 1 {
		return nil, fmt.Errorf("typecheck.check returned %d results, want 1", len(results))
	}
	res, ok := results[0].(*wc.ValResult)
	if !ok {
		return nil, fmt.Errorf("typecheck.check result is %T, want *wacogo.ValResult", results[0])
	}
	if !res.IsOk() {
		return &CheckResult{Stderr: stderrBuf.Bytes()}, decodeCheckError(res.Err())
	}

	list, ok := res.Ok().(*wc.ValList)
	if !ok {
		return nil, fmt.Errorf("typecheck.check ok payload is %T, want *wacogo.ValList", res.Ok())
	}
	diags, err := decodeDiagnostics(list)
	if err != nil {
		return nil, err
	}
	return &CheckResult{Diagnostics: diags, Stderr: stderrBuf.Bytes()}, nil
}

func decodeCheckError(v wc.Val) error {
	variant, ok := v.(*wc.ValVariant)
	if !ok {
		return fmt.Errorf("typecheck error payload is %T, want *wacogo.ValVariant", v)
	}
	cases := []string{"config-error", "internal-error"}
	d := int(variant.Discriminant())
	name := "unknown"
	if d >= 0 && d < len(cases) {
		name = cases[d]
	}
	msg := ""
	if s, ok := variant.Val().(wc.ValString); ok {
		msg = string(s)
	}
	return fmt.Errorf("typecheck: %s: %s", name, msg)
}

func decodeDiagnostics(list *wc.ValList) ([]Diagnostic, error) {
	out := make([]Diagnostic, 0, list.Len())
	for i := 0; i < list.Len(); i++ {
		rec, ok := list.Get(i).(*wc.ValRecord)
		if !ok {
			return nil, fmt.Errorf("diagnostic[%d] is %T, want *wacogo.ValRecord", i, list.Get(i))
		}
		d := Diagnostic{}
		if v, _ := rec.Field("file").(wc.ValString); v != "" {
			d.File = string(v)
		}
		if v, ok := rec.Field("line").(wc.ValU32); ok {
			d.Line = int(v)
		}
		if v, ok := rec.Field("column").(wc.ValU32); ok {
			d.Column = int(v)
		}
		if v, ok := rec.Field("severity").(*wc.ValEnum); ok {
			switch v.Discriminant() {
			case 0:
				d.Severity = SeverityError
			case 1:
				d.Severity = SeverityWarning
			default:
				d.Severity = SeverityInfo
			}
		}
		if v, ok := rec.Field("code").(wc.ValU32); ok {
			d.Code = uint32(v)
		}
		if v, ok := rec.Field("message").(wc.ValString); ok {
			d.Message = string(v)
		}
		out = append(out, d)
	}
	return out, nil
}

// findTSRoots returns the .ts/.tsx files at the source FS root suitable
// as TypeScript program rootNames. They appear under "src/" because we
// mount the source FS at that prefix.
func findTSRoots(fsys fs.FS) ([]string, error) {
	var out []string
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
		switch ext := strings.ToLower(extOf(p)); ext {
		case ".ts", ".tsx", ".mts", ".cts":
			out = append(out, "src/"+p)
		}
		return nil
	})
	return out, err
}

func extOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '.' {
			return p[i:]
		}
		if p[i] == '/' {
			return ""
		}
	}
	return ""
}

// mountFS exposes the source tree at "src/" and the resolved
// node_modules at "node_modules/".
type mountFS struct {
	source      fs.FS
	nodeModules fs.FS
}

var (
	_ fs.FS         = mountFS{}
	_ fs.StatFS     = mountFS{}
	_ fs.ReadDirFS  = mountFS{}
	_ fs.ReadFileFS = mountFS{}
)

func (m mountFS) split(name string) (fs.FS, string, bool) {
	switch {
	case name == "src":
		return m.source, ".", true
	case strings.HasPrefix(name, "src/"):
		return m.source, strings.TrimPrefix(name, "src/"), true
	case name == "node_modules":
		if m.nodeModules == nil {
			return emptyFS{}, ".", true
		}
		return m.nodeModules, ".", true
	case strings.HasPrefix(name, "node_modules/"):
		if m.nodeModules == nil {
			return emptyFS{}, ".", true
		}
		return m.nodeModules, strings.TrimPrefix(name, "node_modules/"), true
	}
	return nil, "", false
}

func (m mountFS) Open(name string) (fs.File, error) {
	if name == "." {
		return openRoot(m), nil
	}
	if sub, p, ok := m.split(name); ok {
		return sub.Open(p)
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

func (m mountFS) Stat(name string) (fs.FileInfo, error) {
	if name == "." {
		return rootDirInfo{}, nil
	}
	if sub, p, ok := m.split(name); ok {
		return fs.Stat(sub, p)
	}
	return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
}

func (m mountFS) ReadFile(name string) ([]byte, error) {
	if sub, p, ok := m.split(name); ok {
		return fs.ReadFile(sub, p)
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

func (m mountFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name == "." {
		entries := []fs.DirEntry{dirEntry("src")}
		if m.nodeModules != nil {
			entries = append(entries, dirEntry("node_modules"))
		}
		return entries, nil
	}
	if sub, p, ok := m.split(name); ok {
		return fs.ReadDir(sub, p)
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

type emptyFS struct{}

func (emptyFS) Open(name string) (fs.File, error) {
	if name == "." {
		return openRoot(fstest.MapFS{}), nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

func openRoot(fsys fs.FS) fs.File {
	if rd, ok := fsys.(fs.ReadDirFS); ok {
		entries, _ := rd.ReadDir(".")
		return &rootFile{entries: entries}
	}
	return &rootFile{}
}

type rootFile struct {
	entries []fs.DirEntry
	pos     int
	mu      sync.Mutex
}

func (r *rootFile) Stat() (fs.FileInfo, error) { return rootDirInfo{}, nil }
func (r *rootFile) Read([]byte) (int, error)   { return 0, io.EOF }
func (r *rootFile) Close() error               { return nil }

func (r *rootFile) ReadDir(n int) ([]fs.DirEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 {
		out := r.entries[r.pos:]
		r.pos = len(r.entries)
		return out, nil
	}
	end := r.pos + n
	if end > len(r.entries) {
		end = len(r.entries)
	}
	out := r.entries[r.pos:end]
	r.pos = end
	if len(out) == 0 {
		return nil, io.EOF
	}
	return out, nil
}

type rootDirInfo struct{}

func (rootDirInfo) Name() string       { return "." }
func (rootDirInfo) Size() int64        { return 0 }
func (rootDirInfo) Mode() fs.FileMode  { return fs.ModeDir | 0o555 }
func (rootDirInfo) ModTime() time.Time { return time.Time{} }
func (rootDirInfo) IsDir() bool        { return true }
func (rootDirInfo) Sys() any           { return nil }

func dirEntry(name string) fs.DirEntry {
	return staticDirEntry{name: name}
}

type staticDirEntry struct{ name string }

func (e staticDirEntry) Name() string               { return e.name }
func (e staticDirEntry) IsDir() bool                { return true }
func (e staticDirEntry) Type() fs.FileMode          { return fs.ModeDir | 0o555 }
func (e staticDirEntry) Info() (fs.FileInfo, error) { return rootDirInfo{}, nil }
