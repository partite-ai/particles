package importscan

import (
	"reflect"
	"testing"
	"testing/fstest"
)

// mapfs is a tiny convenience for spelling out an in-memory FS in tests.
func mapfs(files map[string]string) fstest.MapFS {
	out := fstest.MapFS{}
	for p, contents := range files {
		out[p] = &fstest.MapFile{Data: []byte(contents)}
	}
	return out
}

// -----------------------------------------------------------------------------
// parseNpmSpec
// -----------------------------------------------------------------------------

func TestParseNpmSpec(t *testing.T) {
	cases := []struct {
		spec    string
		name    string
		version string
		subpath string
		ok      bool
	}{
		{"npm:lodash@^4.17.0", "lodash", "^4.17.0", "", true},
		{"npm:lodash@4/get", "lodash", "4", "/get", true},
		{"npm:@scope/pkg@^1.0.0", "@scope/pkg", "^1.0.0", "", true},
		{"npm:@scope/pkg@^1.0.0/sub", "@scope/pkg", "^1.0.0", "/sub", true},
		{"npm:lodash", "lodash", "", "", true},
		{"npm:@scope/pkg", "@scope/pkg", "", "", true},
		{"npm:lodash/get", "lodash", "", "/get", true},
		{"lodash@^4.0.0", "", "", "", false},
		{"npm:", "", "", "", false},
		{"npm:@scope", "", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.spec, func(t *testing.T) {
			n, v, s, ok := parseNpmSpec(c.spec)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if !ok {
				return
			}
			if n != c.name || v != c.version || s != c.subpath {
				t.Fatalf("got (%q, %q, %q), want (%q, %q, %q)",
					n, v, s, c.name, c.version, c.subpath)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Scan: end-to-end against in-memory FSes
// -----------------------------------------------------------------------------

func TestScanHappyPath(t *testing.T) {
	fsys := mapfs(map[string]string{
		"Particlefile.ts": `
			import yaml from "npm:yaml@^2.3.0";
			import { credentials } from "particle:credentials";
			import parse from "./tools/parse.ts";
			import _ from "npm:@scope/utils@1.0.0/sub";
			console.log(yaml, credentials, parse, _);
		`,
		"tools/parse.ts": `
			import { kv } from "particle:kv";
			export default async ({ input }) => kv.get(input);
		`,
	})

	r, err := Scan(fsys)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(r.Errors) != 0 {
		t.Fatalf("unexpected errors: %#v", r.Errors)
	}

	gotNpm := stripImporter(r.NpmDeps)
	wantNpm := []NpmSpec{
		{Name: "@scope/utils", VersionRange: "1.0.0", Subpath: "/sub"},
		{Name: "yaml", VersionRange: "^2.3.0"},
	}
	if !reflect.DeepEqual(gotNpm, wantNpm) {
		t.Fatalf("npm deps mismatch:\n got: %#v\nwant: %#v", gotNpm, wantNpm)
	}

	wantCaps := []string{"credentials", "kv"}
	if !reflect.DeepEqual(r.Capabilities, wantCaps) {
		t.Fatalf("capabilities = %v, want %v", r.Capabilities, wantCaps)
	}

	if len(r.Locals) != 1 || r.Locals[0].Resolved != "tools/parse.ts" {
		t.Fatalf("locals = %#v, want one entry pointing at tools/parse.ts", r.Locals)
	}
}

func TestScanMissingVersion(t *testing.T) {
	fsys := mapfs(map[string]string{
		"Particlefile.ts": `import yaml from "npm:yaml";`,
	})
	r, err := Scan(fsys)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(r.Errors) != 1 || r.Errors[0].Kind != ErrMissingVersion {
		t.Fatalf("want one ErrMissingVersion, got %#v", r.Errors)
	}
}

func TestScanBareSpecifier(t *testing.T) {
	fsys := mapfs(map[string]string{
		"Particlefile.ts": `import yaml from "yaml";`,
	})
	r, err := Scan(fsys)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(r.Errors) != 1 || r.Errors[0].Kind != ErrBareSpecifier {
		t.Fatalf("want one ErrBareSpecifier, got %#v", r.Errors)
	}
}

func TestScanOutsideTree(t *testing.T) {
	fsys := mapfs(map[string]string{
		"Particlefile.ts": `import x from "../escape.ts";`,
	})
	r, err := Scan(fsys)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(r.Errors) != 1 || r.Errors[0].Kind != ErrOutsideTree {
		t.Fatalf("want one ErrOutsideTree, got %#v", r.Errors)
	}
}

// TODO: detection for computed dynamic imports (`import(name + ".js")`).
// Current esbuild doesn't emit a warning for this case via the public API,
// so the metafile-driven scan can't see it. A small source-text scan for
// `import(` followed by non-string-literal arguments would catch it; defer
// until we hit a real particle that wants to do this.

func TestScanUnknownPrefix(t *testing.T) {
	fsys := mapfs(map[string]string{
		"Particlefile.ts": `import fs from "node:fs";`,
	})
	r, err := Scan(fsys)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(r.Errors) != 1 || r.Errors[0].Kind != ErrUnknownPrefix {
		t.Fatalf("want one ErrUnknownPrefix, got %#v", r.Errors)
	}
}

func TestScanSkipsNodeModules(t *testing.T) {
	fsys := mapfs(map[string]string{
		"Particlefile.ts":                `import yaml from "npm:yaml@^2.3.0";`,
		"node_modules/yaml/index.js":     `import bare from "wherever";`,
		"node_modules/yaml/package.json": `{"name":"yaml"}`,
	})
	r, err := Scan(fsys)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, e := range r.Errors {
		t.Fatalf("unexpected error from inside node_modules: %v", e)
	}
}

func TestScanCapabilityDedup(t *testing.T) {
	fsys := mapfs(map[string]string{
		"a.ts": `import { credentials } from "particle:credentials";`,
		"b.ts": `import { credentials } from "particle:credentials";`,
		"c.ts": `import { kv } from "particle:kv";`,
	})
	r, err := Scan(fsys)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	want := []string{"credentials", "kv"}
	if !reflect.DeepEqual(r.Capabilities, want) {
		t.Fatalf("capabilities = %v, want %v", r.Capabilities, want)
	}
}

func TestScanEmptyFS(t *testing.T) {
	r, err := Scan(mapfs(map[string]string{}))
	if err != nil {
		t.Fatalf("Scan empty: %v", err)
	}
	if len(r.NpmDeps) != 0 || len(r.Locals) != 0 || len(r.Capabilities) != 0 || len(r.Errors) != 0 {
		t.Fatalf("expected fully-empty result, got %#v", r)
	}
}

// stripImporter zeroes the Importer field so expectations stay readable.
func stripImporter(in []NpmSpec) []NpmSpec {
	out := make([]NpmSpec, len(in))
	for i, s := range in {
		s.Importer = ""
		out[i] = s
	}
	return out
}
