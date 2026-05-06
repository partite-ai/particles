package bundle

import (
	"strings"
	"testing"
	"testing/fstest"
)

func mapfs(files map[string]string) fstest.MapFS {
	out := fstest.MapFS{}
	for p, contents := range files {
		out[p] = &fstest.MapFile{Data: []byte(contents)}
	}
	return out
}

// -----------------------------------------------------------------------------
// parseNpmSpec — quick sanity (full coverage lives in importscan tests).
// -----------------------------------------------------------------------------

func TestParseNpmSpec(t *testing.T) {
	n, v, s, ok := parseNpmSpec("npm:lodash@^4.17.0/get")
	if !ok || n != "lodash" || v != "^4.17.0" || s != "/get" {
		t.Fatalf("got (%q, %q, %q, %v)", n, v, s, ok)
	}
}

// -----------------------------------------------------------------------------
// Bundle: end-to-end against in-memory FSes.
// -----------------------------------------------------------------------------

func TestBundleHappyPath(t *testing.T) {
	fsys := mapfs(map[string]string{
		"src/Particlefile.ts": `
import yaml from "npm:yaml@^2.3.0";
import { credentials } from "particle:credentials";
import { greet } from "./greet.ts";

export default {
  name: "demo",
  version: "0.1.0",
  description: "test",
  capabilities: {},
  tools: {
    parse: {
      description: "parse yaml",
      inputSchema: { type: "object" },
      handler: async ({ input }) => ({ result: yaml.parse(input), greeting: greet(), creds: !!credentials }),
    },
  },
};
`,
		"src/greet.ts":                   `export const greet = () => "hello";`,
		"node_modules/yaml/package.json": `{"name":"yaml","version":"2.3.0","main":"./index.js","type":"module"}`,
		"node_modules/yaml/index.js":     `export default { parse: (s) => "PARSED:" + s };`,
	})

	r, err := Bundle(Options{
		FS:         fsys,
		EntryPoint: "src/Particlefile.ts",
	})
	if err != nil {
		t.Fatalf("Bundle: %v", err)
	}
	js := string(r.JS)

	if !strings.Contains(js, "PARSED:") {
		t.Errorf("expected bundled yaml content (PARSED: literal); got:\n%s", js)
	}
	if !strings.Contains(js, `"particle:credentials"`) {
		t.Errorf("expected `particle:credentials` to remain externalized; got:\n%s", js)
	}
	if !strings.Contains(js, `"hello"`) {
		t.Errorf("expected local greet() string to be inlined; got:\n%s", js)
	}
}

func TestBundleWithSubpath(t *testing.T) {
	fsys := mapfs(map[string]string{
		"src/Particlefile.ts": `
import get from "npm:lodash@^4.17.0/get";
console.log(get({ a: { b: 1 } }, "a.b"));
`,
		"node_modules/lodash/package.json": `{"name":"lodash","version":"4.17.21","main":"./index.js"}`,
		"node_modules/lodash/index.js":     `export default {};`,
		"node_modules/lodash/get.js":       `export default (obj, path) => "GOT:" + path;`,
	})

	r, err := Bundle(Options{
		FS:         fsys,
		EntryPoint: "src/Particlefile.ts",
	})
	if err != nil {
		t.Fatalf("Bundle: %v", err)
	}
	if !strings.Contains(string(r.JS), "GOT:") {
		t.Errorf("subpath import didn't resolve to lodash/get; got:\n%s", string(r.JS))
	}
}

func TestBundleScopedPackage(t *testing.T) {
	fsys := mapfs(map[string]string{
		"src/Particlefile.ts": `
import x from "npm:@scope/utils@^1.0.0";
console.log(x);
`,
		"node_modules/@scope/utils/package.json": `{"name":"@scope/utils","version":"1.0.0","main":"./index.js","type":"module"}`,
		"node_modules/@scope/utils/index.js":     `export default "SCOPED";`,
	})

	r, err := Bundle(Options{
		FS:         fsys,
		EntryPoint: "src/Particlefile.ts",
	})
	if err != nil {
		t.Fatalf("Bundle: %v", err)
	}
	if !strings.Contains(string(r.JS), `"SCOPED"`) {
		t.Errorf("expected scoped pkg content inlined; got:\n%s", string(r.JS))
	}
}

func TestBundleParticleExternalized(t *testing.T) {
	fsys := mapfs(map[string]string{
		"src/Particlefile.ts": `
import { credentials } from "particle:credentials";
import { kv } from "particle:kv";
console.log(credentials, kv);
`,
		// MapFS needs at least one node_modules entry or fs.WalkDir won't
		// see the dir at all (and bundle wouldn't materialize one).
		"node_modules/.placeholder": "",
	})

	r, err := Bundle(Options{
		FS:         fsys,
		EntryPoint: "src/Particlefile.ts",
	})
	if err != nil {
		t.Fatalf("Bundle: %v", err)
	}
	js := string(r.JS)
	if !strings.Contains(js, `"particle:credentials"`) ||
		!strings.Contains(js, `"particle:kv"`) {
		t.Errorf("particle:* should pass through as imports; got:\n%s", js)
	}
}

func TestBundleMissingNpmPackage(t *testing.T) {
	fsys := mapfs(map[string]string{
		"src/Particlefile.ts":       `import x from "npm:nonexistent@^1.0.0"; console.log(x);`,
		"node_modules/.placeholder": "",
	})

	_, err := Bundle(Options{
		FS:         fsys,
		EntryPoint: "src/Particlefile.ts",
	})
	if err == nil {
		t.Fatal("expected error for missing npm package, got nil")
	}
	bErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *bundle.Error, got %T: %v", err, err)
	}
	if len(bErr.Diagnostics) == 0 {
		t.Fatal("expected at least one diagnostic")
	}
	if !strings.Contains(bErr.Error(), "nonexistent") {
		t.Errorf("error should mention package name; got: %v", bErr)
	}
}

func TestBundleSourcemap(t *testing.T) {
	fsys := mapfs(map[string]string{
		"src/Particlefile.ts":       `console.log("hi");`,
		"node_modules/.placeholder": "",
	})

	r, err := Bundle(Options{
		FS:         fsys,
		EntryPoint: "src/Particlefile.ts",
		Sourcemap:  true,
	})
	if err != nil {
		t.Fatalf("Bundle: %v", err)
	}
	if len(r.Sourcemap) == 0 {
		t.Errorf("expected sourcemap bytes, got none")
	}
	if !strings.Contains(string(r.JS), "sourceMappingURL") {
		t.Errorf("JS should reference the sourcemap; got:\n%s", string(r.JS))
	}
}

func TestBundleCustomNodeModulesPath(t *testing.T) {
	// Some callers may want node_modules at a non-default location (e.g.
	// a content-addressed cache dir mounted into the FS).
	fsys := mapfs(map[string]string{
		"src/Particlefile.ts":             `import x from "npm:foo@^1.0.0"; console.log(x);`,
		"vendor/foo/package.json":         `{"name":"foo","version":"1.0.0","main":"./index.js","type":"module"}`,
		"vendor/foo/index.js":             `export default "VENDORED";`,
	})

	r, err := Bundle(Options{
		FS:              fsys,
		EntryPoint:      "src/Particlefile.ts",
		NodeModulesPath: "vendor",
	})
	if err != nil {
		t.Fatalf("Bundle: %v", err)
	}
	if !strings.Contains(string(r.JS), `"VENDORED"`) {
		t.Errorf("expected vendored content inlined; got:\n%s", string(r.JS))
	}
}

// TestBundleExportsConditional verifies that the custom resolver picks the
// `import` condition out of a package.json exports object — the typical
// modern-package shape that ships both ESM and CJS entries.
func TestBundleExportsConditional(t *testing.T) {
	fsys := mapfs(map[string]string{
		"src/Particlefile.ts": `import x from "npm:dual@^1.0.0"; console.log(x);`,
		"node_modules/dual/package.json": `{
			"name": "dual",
			"version": "1.0.0",
			"exports": {
				".": {
					"import": "./esm/index.js",
					"require": "./cjs/index.js",
					"default": "./fallback.js"
				}
			}
		}`,
		"node_modules/dual/esm/index.js":  `export default "FROM_ESM";`,
		"node_modules/dual/cjs/index.js":  `module.exports = "FROM_CJS";`,
		"node_modules/dual/fallback.js":   `export default "FROM_FALLBACK";`,
	})

	r, err := Bundle(Options{FS: fsys, EntryPoint: "src/Particlefile.ts"})
	if err != nil {
		t.Fatalf("Bundle: %v", err)
	}
	if !strings.Contains(string(r.JS), `"FROM_ESM"`) {
		t.Errorf("expected exports.import to win; got:\n%s", string(r.JS))
	}
}

// TestBundleTransitiveBareImport verifies that resolution chases bare
// specifiers from inside a node_modules package back into the same flat
// node_modules tree (no parent-walk needed in our layout).
func TestBundleTransitiveBareImport(t *testing.T) {
	fsys := mapfs(map[string]string{
		"src/Particlefile.ts": `import a from "npm:a@^1.0.0"; console.log(a);`,
		"node_modules/a/package.json": `{"name":"a","version":"1.0.0","main":"./index.js","type":"module"}`,
		"node_modules/a/index.js":     `import b from "b"; export default "A:" + b;`,
		"node_modules/b/package.json": `{"name":"b","version":"1.0.0","main":"./index.js","type":"module"}`,
		"node_modules/b/index.js":     `export default "B_DEP";`,
	})

	r, err := Bundle(Options{FS: fsys, EntryPoint: "src/Particlefile.ts"})
	if err != nil {
		t.Fatalf("Bundle: %v", err)
	}
	if !strings.Contains(string(r.JS), `"B_DEP"`) {
		t.Errorf("expected transitive dep `b` to be inlined; got:\n%s", string(r.JS))
	}
}

func TestBundleMissingArgs(t *testing.T) {
	if _, err := Bundle(Options{EntryPoint: "x"}); err == nil {
		t.Error("expected error when FS missing")
	}
	if _, err := Bundle(Options{FS: mapfs(nil)}); err == nil {
		t.Error("expected error when EntryPoint missing")
	}
}
