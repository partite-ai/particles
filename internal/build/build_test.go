package build_test

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/partite-ai/particles/internal/build"
)

// All build tests run against the real wacogo backend (the wasm
// artifacts are go:embed'd; `make embed` populates the dir). Tests
// that don't need npm fetch-resolution run offline; the resolver path
// is exercised through the dedicated resolver test below and is
// gated on -short / PARTICLE_OFFLINE_TESTS.

func TestBuild_HappyPath(t *testing.T) {
	src := fstest.MapFS{
		"Particlefile.ts": &fstest.MapFile{
			Data: []byte(`export default {
  name: "echo-tools",
  description: "Echo a string back.",
  version: "0.1.0",
  capabilities: {},
  tools: {
    echo: {
      description: "Echo the input",
      inputSchema: { type: "object", properties: { input: { type: "string" } } },
      handler: async ({ input }: { input: string }) => ({ result: input }),
    },
  },
};
`),
		},
	}

	res, err := build.Build(context.Background(), build.Options{
		Source:      src,
		NoTypeCheck: true,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	manifest := readFile(t, res.Particle, "manifest.json")
	for _, want := range []string{
		`"name":"echo-tools"`,
		`"version":"0.1.0"`,
		`"name":"echo"`,
	} {
		if !strings.Contains(string(manifest), want) {
			t.Errorf("manifest missing %q; full payload:\n%s", want, manifest)
		}
	}

	bundle := readFile(t, res.Particle, "bundle.js")
	if len(bundle) == 0 {
		t.Error("bundle.js is empty")
	}
	if _, err := fs.Stat(res.Particle, "bundle.js.map"); err != nil {
		t.Errorf("bundle.js.map missing: %v", err)
	}

	buildInfo := readFile(t, res.Particle, "build-info.json")
	if !strings.Contains(string(buildInfo), `"runtimeApi": "0.1.0"`) {
		t.Errorf("build-info.json missing runtimeApi: %s", buildInfo)
	}
}

func TestBuild_RejectsBareSpecifier(t *testing.T) {
	src := fstest.MapFS{
		"Particlefile.ts": &fstest.MapFile{
			Data: []byte(`import _ from "lodash";
export default { name: "x", description: "x", version: "0.1.0", capabilities: {}, tools: {} };
`),
		},
	}
	_, err := build.Build(context.Background(), build.Options{
		Source:      src,
		NoTypeCheck: true,
	})
	if err == nil {
		t.Fatal("expected error for bare specifier import")
	}
	var be *build.Error
	if !errors.As(err, &be) {
		t.Fatalf("error type: got %T, want *build.Error", err)
	}
	if be.Phase != build.PhaseImportScan {
		t.Errorf("phase = %v, want PhaseImportScan", be.Phase)
	}
	if !strings.Contains(err.Error(), "lodash") {
		t.Errorf("error should mention the offending specifier: %v", err)
	}
}

func TestBuild_MissingEntryPoint(t *testing.T) {
	src := fstest.MapFS{
		"otherfile.ts": &fstest.MapFile{Data: []byte(`export const x = 1;`)},
	}
	_, err := build.Build(context.Background(), build.Options{
		Source:      src,
		NoTypeCheck: true,
	})
	if err == nil {
		t.Fatal("expected error when no Particlefile is present")
	}
}

func TestBuild_RejectsMalformedManifest(t *testing.T) {
	src := fstest.MapFS{
		// Missing tools field — caught by introspect's validateAndSerialize.
		"Particlefile.ts": &fstest.MapFile{
			Data: []byte(`export default { name: "x", description: "x", version: "0.1.0" };`),
		},
	}
	_, err := build.Build(context.Background(), build.Options{
		Source:      src,
		NoTypeCheck: true,
	})
	if err == nil {
		t.Fatal("expected validation error for missing tools field")
	}
	var be *build.Error
	if !errors.As(err, &be) {
		t.Fatalf("error type: got %T, want *build.Error", err)
	}
	if be.Phase != build.PhaseManifestExtract {
		t.Errorf("phase = %v, want PhaseManifestExtract", be.Phase)
	}
	if !strings.Contains(err.Error(), "invalid-manifest") {
		t.Errorf("error should carry invalid-manifest variant case: %v", err)
	}
}

// A non-semver version string fails the manifest-extract phase
// at the build level — same plumbing as other invalid-manifest
// errors. Catches typos like `"latest"` or shortened forms like
// `"1.2"` before they reach the registry.
func TestBuild_RejectsNonSemverVersion(t *testing.T) {
	src := fstest.MapFS{
		"Particlefile.ts": &fstest.MapFile{
			Data: []byte(`export default { name: "x", description: "x", version: "latest", capabilities: {}, tools: {} };`),
		},
	}
	_, err := build.Build(context.Background(), build.Options{
		Source:      src,
		NoTypeCheck: true,
	})
	if err == nil {
		t.Fatal("expected validation error for non-semver version")
	}
	var be *build.Error
	if !errors.As(err, &be) {
		t.Fatalf("error type: got %T, want *build.Error", err)
	}
	if be.Phase != build.PhaseManifestExtract {
		t.Errorf("phase = %v, want PhaseManifestExtract", be.Phase)
	}
	if !strings.Contains(err.Error(), "semver") {
		t.Errorf("error should mention semver: %v", err)
	}
}

// An unknown credential method type fails build-time validation
// — same plumbing as the unknown-capability check. Without this,
// a manifest typo like `type: "apike"` would slip past introspect
// (which only enforces shape) and surface much later as a runtime
// substitution error far from the offending field.
func TestBuild_RejectsUnknownCredentialMethodType(t *testing.T) {
	src := fstest.MapFS{
		"Particlefile.ts": &fstest.MapFile{
			Data: []byte(`export default {
				name: "x",
				description: "x",
				version: "0.1.0",
				capabilities: {},
				credentials: { svc: { methods: { x: { type: "telepathy" } } } },
				tools: {},
			};`),
		},
	}
	_, err := build.Build(context.Background(), build.Options{
		Source:      src,
		NoTypeCheck: true,
	})
	if err == nil {
		t.Fatal("expected validation error for unknown method type")
	}
	if !strings.Contains(err.Error(), "telepathy") {
		t.Errorf("error should name the offending type: %v", err)
	}
	if !strings.Contains(err.Error(), "credentials.svc.methods.x") {
		t.Errorf("error should point at the offending path: %v", err)
	}
}

// readFile reads `name` from the result FS, t.Fatal on failure.
func readFile(t *testing.T, fsys fs.FS, name string) []byte {
	t.Helper()
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		t.Fatalf("read %s from result FS: %v", name, err)
	}
	return data
}
