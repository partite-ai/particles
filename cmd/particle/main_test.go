package main_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildCLI compiles cmd/particle into the test's temp dir and returns
// the path to the binary. Cached per package via t.TempDir's behavior
// (one binary per test), but since each test creates its own working
// directory and runs the same compiled CLI, we accept the rebuild
// cost rather than introduce a sync.Once that would couple test
// ordering.
func buildCLI(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "particle")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/partite-ai/particle/cmd/particle")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build cmd/particle: %v\n%s", err, out)
	}
	return bin
}

// runIn executes bin with args inside cwd. Returns stdout, stderr, exit code.
func runIn(t *testing.T, bin, cwd string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = cwd
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return stdout.String(), stderr.String(), ee.ExitCode()
		}
		t.Fatalf("run %s: %v", bin, err)
	}
	return stdout.String(), stderr.String(), 0
}

const sourceNoCredentials = `export default {
  name: "yaml-tools",
  description: "Parse and format YAML.",
  version: "0.1.0",
  capabilities: {},
  tools: {
    echo: {
      description: "Echo back the input",
      inputSchema: { type: "object", properties: { input: { type: "string" } } },
      handler: async ({ input }: { input: string }) => ({ result: input.toUpperCase() }),
    },
  },
};
`

func writeSource(t *testing.T, dir, source string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "Particlefile.ts"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Default `particle build` registers the particle in --db. The
// happy path uses a particle without declared credentials so the
// keychain (which we can't mock from a CLI subprocess) never gets
// touched.
func TestParticleBuild_RegistersByDefault(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
	writeSource(t, dir, sourceNoCredentials)
	dbPath := filepath.Join(t.TempDir(), "state.db")

	stdout, stderr, code := runIn(t, bin, dir, "build", "--db", dbPath)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr:\n%s", code, stderr)
	}
	if got, want := stdout, "registered yaml-tools@0.1.0\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	// No tarball produced on register.
	if matches, _ := filepath.Glob(filepath.Join(dir, "*.particle")); len(matches) != 0 {
		t.Errorf("register path should not produce .particle file: %v", matches)
	}
	// Re-running registration is idempotent — same (name, version) replaces.
	if _, _, code := runIn(t, bin, dir, "build", "--db", dbPath); code != 0 {
		t.Errorf("second build exit = %d (re-register should succeed)", code)
	}
}

// `--pack` recovers the pre-registry behavior: write a deterministic
// tarball to CWD, no DB touched.
func TestParticleBuild_Pack_WritesTarball(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
	writeSource(t, dir, sourceNoCredentials)

	stdout, stderr, code := runIn(t, bin, dir, "build", "--pack")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr:\n%s", code, stderr)
	}
	want := "yaml-tools-0.1.0.particle\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}

	out := filepath.Join(dir, "yaml-tools-0.1.0.particle")
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}

	got := tarEntries(t, data)
	want4 := []string{"build-info.json", "bundle.js", "bundle.js.map", "manifest.json"}
	if !equal(got, want4) {
		t.Errorf("tarball entries = %v, want %v", got, want4)
	}
}

func TestParticleBuild_FailureGoesToStderr(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
	writeSource(t, dir, `import _ from "lodash";
export default { name: "x", description: "x", version: "0.1.0", capabilities: {}, tools: {} };
`)
	dbPath := filepath.Join(t.TempDir(), "state.db")

	stdout, stderr, code := runIn(t, bin, dir, "build", "--db", dbPath)
	if code == 0 {
		t.Fatal("expected non-zero exit on bare specifier import")
	}
	if stdout != "" {
		t.Errorf("stdout should be empty on failure, got: %q", stdout)
	}
	if !strings.Contains(stderr, "import-scan") {
		t.Errorf("stderr should mention import-scan: %s", stderr)
	}
	if !strings.Contains(stderr, "lodash") {
		t.Errorf("stderr should mention the offending specifier: %s", stderr)
	}
	// No tarball produced on failure.
	if matches, _ := filepath.Glob(filepath.Join(dir, "*.particle")); len(matches) != 0 {
		t.Errorf("unexpected .particle file produced on failure: %v", matches)
	}
}

func TestParticleBuild_RejectsArgs(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
	_, stderr, code := runIn(t, bin, dir, "build", "extra-arg")
	if code == 0 {
		t.Errorf("exit code = %d, want non-zero for usage error", code)
	}
	if !strings.Contains(stderr, "unknown command") && !strings.Contains(stderr, "accepts 0 arg") {
		t.Errorf("stderr should explain the arg error: %s", stderr)
	}
}

// -----------------------------------------------------------------------------

func tarEntries(t *testing.T, data []byte) []string {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(data))
	var names []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		names = append(names, hdr.Name)
		if _, err := io.Copy(io.Discard, tr); err != nil {
			t.Fatalf("tar copy: %v", err)
		}
	}
	return names
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
