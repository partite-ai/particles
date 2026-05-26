package main_test

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
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
	cmd := exec.Command("go", "build", "-o", bin, "github.com/partite-ai/particles/cmd/particle")
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

	stdout, stderr, code := runIn(t, bin, dir, "build", "--yes", "--db", dbPath)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr:\n%s", code, stderr)
	}
	if got, want := stdout, "registered yaml-tools@0.1.0\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	// No archive produced on register.
	if matches, _ := filepath.Glob(filepath.Join(dir, "*.particle")); len(matches) != 0 {
		t.Errorf("register path should not produce .particle file: %v", matches)
	}
	// Re-running registration is idempotent — same (name, version) replaces.
	if _, _, code := runIn(t, bin, dir, "build", "--yes", "--db", dbPath); code != 0 {
		t.Errorf("second build exit = %d (re-register should succeed)", code)
	}
}

// `--pack` writes a deterministic zip archive to CWD, no DB
// touched.
func TestParticleBuild_Pack_WritesArchive(t *testing.T) {
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

	got := zipEntries(t, data)
	want4 := []string{"build-info.json", "bundle.mjs", "bundle.mjs.map", "manifest.json"}
	if !equal(got, want4) {
		t.Errorf("archive entries = %v, want %v", got, want4)
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
	// No archive produced on failure.
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

// SIGINT during an in-flight import must cancel the HTTP fetch
// and let the subprocess exit promptly — proving the
// signal.NotifyContext → ExecuteContext wiring in main reaches
// loadParticleFromHTTP. The test server signals the moment it
// receives the GET, blocks on its request context (so it doesn't
// leak if the test ends abnormally), and the test sends SIGINT
// to the child. A wired-up handler returns within seconds; an
// unwired one would only die when Go's default SIGINT handler
// killed the process — also fast, but we'd see a different exit
// signature.
func TestParticleImport_SIGINT_CancelsFetch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Interrupt to a child process is not supported on Windows")
	}
	bin := buildCLI(t)
	dbPath := filepath.Join(t.TempDir(), "state.db")

	var once sync.Once
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		// Block until the client cancels (or the test server
		// shuts down). Either way the response goes nowhere —
		// the point is to keep the subprocess waiting on the
		// body so SIGINT has something to interrupt.
		<-r.Context().Done()
	}))
	defer srv.Close()

	cmd := exec.Command(bin, "import", "--db", dbPath, srv.URL+"/p.particle")
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start subprocess: %v", err)
	}
	// Kill if anything below times out — avoids leaking the
	// process across test runs.
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}()

	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatalf("subprocess never reached the HTTP GET; stderr:\n%s", stderr.String())
	}

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("send SIGINT: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		// The wired-up path: NotifyContext cancels the context,
		// the HTTP client returns context.Canceled, cobra
		// surfaces it, main exits via os.Exit(1) — i.e. a
		// normal exit with code 1. The unwired path would be
		// Go's default SIGINT handler killing the process, in
		// which case ProcessState reports a signal-killed
		// status. We assert "exited normally" specifically so a
		// regression that drops ExecuteContext gets caught even
		// though both paths produce non-zero. We don't pin the
		// error string — it can surface as "context canceled",
		// "operation was canceled", or a wrapped variant
		// depending on which read was suspended.
		if err == nil {
			t.Fatalf("subprocess exited 0; SIGINT should produce a non-zero exit. stderr:\n%s", stderr.String())
		}
		ps := cmd.ProcessState
		if !ps.Exited() {
			t.Fatalf("subprocess was killed by signal rather than unwinding via context (signal wiring missing?). stderr:\n%s", stderr.String())
		}
		if code := ps.ExitCode(); code != 1 {
			t.Errorf("exit code = %d, want 1", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("subprocess did not exit within 10s of SIGINT — signal handling likely not wired. stderr:\n%s", stderr.String())
	}
}

// resolveDBPath must leave the state DB file at 0600 — anything
// looser exposes (sealed) credential ciphertexts and cleartext
// provider metadata to co-tenant users. We run the full CLI
// (instead of poking resolveDBPath internally) because that's the
// real entry point users hit.
func TestParticleBuild_StateDBIs0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes not enforced on Windows")
	}
	bin := buildCLI(t)
	dir := t.TempDir()
	writeSource(t, dir, sourceNoCredentials)
	dbPath := filepath.Join(t.TempDir(), "state.db")

	if _, _, code := runIn(t, bin, dir, "build", "--yes", "--db", dbPath); code != 0 {
		t.Fatalf("build exited %d", code)
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat state.db: %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Errorf("state.db mode = %o, want %o (no group/world bits)", got, want)
	}
}

// A pre-existing state DB created with loose perms (a prior run
// under a permissive umask, or an admin who hand-rolled the file)
// must be tightened on the next CLI invocation. Otherwise a
// single sloppy create leaves the file world-readable forever.
func TestParticleBuild_StateDB_TightensExistingLoosePerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes not enforced on Windows")
	}
	bin := buildCLI(t)
	dir := t.TempDir()
	writeSource(t, dir, sourceNoCredentials)
	dbPath := filepath.Join(t.TempDir(), "state.db")

	// Plant a world-readable empty file at dbPath.
	if err := os.WriteFile(dbPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, code := runIn(t, bin, dir, "build", "--yes", "--db", dbPath); code != 0 {
		t.Fatalf("build exited %d", code)
	}
	info, _ := os.Stat(dbPath)
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Errorf("after build, state.db mode = %o, want %o", got, want)
	}
}

// -----------------------------------------------------------------------------

func zipEntries(t *testing.T, data []byte) []string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("zip open: %v", err)
	}
	var names []string
	for _, entry := range zr.File {
		names = append(names, entry.Name)
		rc, err := entry.Open()
		if err != nil {
			t.Fatalf("open %s: %v", entry.Name, err)
		}
		if _, err := io.Copy(io.Discard, rc); err != nil {
			t.Fatalf("read %s: %v", entry.Name, err)
		}
		rc.Close()
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
