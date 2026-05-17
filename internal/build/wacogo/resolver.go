package wacogo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"testing/fstest"
	"time"

	wc "github.com/partite-ai/wacogo"
	"github.com/partite-ai/wacogo/wasi"

	"github.com/partite-ai/particles/internal/importscan"
)

// installerInterface is the canonical id of the exported instance the
// deno-npm component publishes — see components/deno-npm/wit/world.wit.
const installerInterface = "particle:build/installer@0.1.0"

// ResolvedPackage is one entry in a ResolveResult.
type ResolvedPackage struct {
	Name       string
	Version    string
	Integrity  string
	Transitive []int
}

// ResolveResult is what Components.ResolveAndFetch returns on success.
type ResolveResult struct {
	Packages []ResolvedPackage
	// NodeModules is a virtual node_modules tree the bundle phase
	// resolves `npm:foo` against. Each top-level entry is a package
	// directory (`<pkgName>/...`), populated from the unpacked
	// registry tarballs.
	NodeModules fs.FS
	Stderr      []byte
}

// ResolveAndFetch resolves the transitive dep tree for `deps`, fetches
// each tarball over wasi:http, and returns the resolved tree plus a
// virtual node_modules fs.FS.
//
// The deno-npm component is a Rust component with no QuickJS engine —
// no wasm-rquickjs convention quirks — and imports wasi:http directly.
func (c *Components) ResolveAndFetch(ctx context.Context, deps []importscan.NpmSpec) (*ResolveResult, error) {
	if c.denoNpm == nil {
		return nil, fmt.Errorf("wacogo: deno-npm component not loaded")
	}

	stderrBuf := &bytes.Buffer{}
	w, err := wasi.NewWorld(ctx, c.engine, &wasi.Config{
		Args: []string{"deno-npm"},
		// No Preopens: the component reads no host filesystem; it
		// talks to the npm registry over wasi:http.
		Stdin:  strings.NewReader(""),
		Stdout: io.Discard,
		Stderr: stderrBuf,
	})
	if err != nil {
		return nil, fmt.Errorf("build wasi world: %w", err)
	}
	defer w.Close(ctx)

	inst, err := c.denoNpm.Instantiate(ctx, w.Imports()...)
	if err != nil {
		return nil, withStderr(err, stderrBuf, "instantiate deno-npm")
	}
	defer inst.Close(ctx)

	iface := inst.ExportedInstance(installerInterface)
	if iface == nil {
		return nil, fmt.Errorf("deno-npm component does not export instance %q", installerInterface)
	}
	fn := iface.ExportedFunc("resolve-and-fetch")
	if fn == nil {
		return nil, fmt.Errorf("deno-npm.%s does not export resolve-and-fetch()", installerInterface)
	}

	depRequests := dedupSpecs(deps)
	args := make([]wc.Val, len(depRequests))
	for i, d := range depRequests {
		args[i] = wc.NewValRecord(
			field("name", wc.ValString(d.name)),
			field("version-range", wc.ValString(d.versionRange)),
		)
	}
	depList := wc.NewValListOf[wc.Val](args...)

	results, err := fn.Call(ctx, depList)
	if err != nil {
		return &ResolveResult{Stderr: stderrBuf.Bytes()},
			withStderr(err, stderrBuf, "call installer.resolve-and-fetch")
	}
	if len(results) != 1 {
		return nil, fmt.Errorf("resolve-and-fetch returned %d results, want 1", len(results))
	}
	res, ok := results[0].(*wc.ValResult)
	if !ok {
		return nil, fmt.Errorf("resolve-and-fetch result is %T, want *wacogo.ValResult", results[0])
	}
	if !res.IsOk() {
		return &ResolveResult{Stderr: stderrBuf.Bytes()}, decodeInstallerError(res.Err())
	}

	list, ok := res.Ok().(*wc.ValList)
	if !ok {
		return nil, fmt.Errorf("resolve-and-fetch ok payload is %T, want *wacogo.ValList", res.Ok())
	}
	pkgs, nm, err := assembleResolved(list)
	if err != nil {
		return nil, err
	}
	return &ResolveResult{
		Packages:    pkgs,
		NodeModules: nm,
		Stderr:      stderrBuf.Bytes(),
	}, nil
}

func decodeInstallerError(v wc.Val) error {
	variant, ok := v.(*wc.ValVariant)
	if !ok {
		return fmt.Errorf("installer error payload is %T, want *wacogo.ValVariant", v)
	}
	cases := []string{"network-error", "resolution-error", "integrity-mismatch"}
	d := int(variant.Discriminant())
	name := "unknown"
	if d >= 0 && d < len(cases) {
		name = cases[d]
	}
	msg := ""
	if s, ok := variant.Val().(wc.ValString); ok {
		msg = string(s)
	}
	return fmt.Errorf("deno-npm: %s: %s", name, msg)
}

func assembleResolved(list *wc.ValList) ([]ResolvedPackage, fs.FS, error) {
	pkgs := make([]ResolvedPackage, 0, list.Len())
	mfs := fstest.MapFS{}

	for i := 0; i < list.Len(); i++ {
		rec, ok := list.Get(i).(*wc.ValRecord)
		if !ok {
			return nil, nil, fmt.Errorf("resolved-dep[%d] is %T, want *wacogo.ValRecord", i, list.Get(i))
		}
		name := stringField(rec, "name")
		version := stringField(rec, "version")
		integrity := stringField(rec, "integrity")
		tarballBytes, err := bytesField(rec, "tarball-bytes")
		if err != nil {
			return nil, nil, fmt.Errorf("resolved-dep[%d].tarball-bytes: %w", i, err)
		}
		_ = stringField(rec, "package-json")
		transitive, err := uintList(rec, "transitive")
		if err != nil {
			return nil, nil, fmt.Errorf("resolved-dep[%d].transitive: %w", i, err)
		}

		mountPrefix := name + "/"
		if err := unpackNpmTarball(mountPrefix, tarballBytes, mfs); err != nil {
			return nil, nil, fmt.Errorf("unpack %s@%s: %w", name, version, err)
		}

		pkgs = append(pkgs, ResolvedPackage{
			Name:       name,
			Version:    version,
			Integrity:  integrity,
			Transitive: transitive,
		})
	}
	return pkgs, mfs, nil
}

func stringField(rec *wc.ValRecord, name string) string {
	v, _ := rec.Field(name).(wc.ValString)
	return string(v)
}

func bytesField(rec *wc.ValRecord, name string) ([]byte, error) {
	list, ok := rec.Field(name).(*wc.ValList)
	if !ok {
		return nil, fmt.Errorf("field %q is %T, want *wacogo.ValList", name, rec.Field(name))
	}
	out := make([]byte, list.Len())
	for i := 0; i < list.Len(); i++ {
		b, ok := list.Get(i).(wc.ValU8)
		if !ok {
			return nil, fmt.Errorf("field %q[%d] is %T, want ValU8", name, i, list.Get(i))
		}
		out[i] = byte(b)
	}
	return out, nil
}

func uintList(rec *wc.ValRecord, name string) ([]int, error) {
	v := rec.Field(name)
	if v == nil {
		return nil, nil
	}
	list, ok := v.(*wc.ValList)
	if !ok {
		return nil, fmt.Errorf("field %q is %T, want *wacogo.ValList", name, v)
	}
	out := make([]int, list.Len())
	for i := 0; i < list.Len(); i++ {
		u, ok := list.Get(i).(wc.ValU32)
		if !ok {
			return nil, fmt.Errorf("field %q[%d] is %T, want ValU32", name, i, list.Get(i))
		}
		out[i] = int(u)
	}
	return out, nil
}

// unpackNpmTarball decompresses the gzipped tarball bytes and writes
// regular files under mountPrefix in mfs. npm tarballs always wrap
// their contents in a "package/" top-level dir which we strip.
func unpackNpmTarball(mountPrefix string, gzbytes []byte, mfs fstest.MapFS) error {
	gz, err := gzip.NewReader(bytes.NewReader(gzbytes))
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			continue
		}
		name := strings.TrimPrefix(hdr.Name, "package/")
		if name == hdr.Name {
			name = hdr.Name
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return fmt.Errorf("read %q: %w", hdr.Name, err)
		}
		mfs[mountPrefix+name] = &fstest.MapFile{
			Data:    data,
			Mode:    0o644,
			ModTime: time.Time{},
		}
	}
	return nil
}

// dedupSpecs collapses NpmSpecs that point at the same (name, range).
type depKey struct {
	name         string
	versionRange string
}

func dedupSpecs(specs []importscan.NpmSpec) []depKey {
	seen := map[depKey]struct{}{}
	var out []depKey
	for _, s := range specs {
		k := depKey{name: s.Name, versionRange: s.VersionRange}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}
