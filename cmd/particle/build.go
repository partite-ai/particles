package main

import (
	"archive/tar"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/partite-ai/particle/credentials"
	credsqlite "github.com/partite-ai/particle/credentials/sqlite"
	"github.com/partite-ai/particle/importer"
	"github.com/partite-ai/particle/internal/build"
	regsqlite "github.com/partite-ai/particle/registry/sqlite"
)

func newBuildCmd() *cobra.Command {
	var (
		pack   bool
		dbPath string
	)
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build a particle and register it in the local state DB",
		Long: `Build the particle in the current directory.

By default the result is registered in the local state DB. Pass
--pack to write a <name>-<version>.particle tarball to CWD instead.

Registration fails if any credential the manifest declares hasn't
been provisioned in the credentials store.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBuild(cmd, pack, dbPath)
		},
	}
	cmd.Flags().BoolVar(&pack, "pack", false, "Write <name>-<version>.particle to CWD instead of registering")
	cmd.Flags().StringVar(&dbPath, "db", "", "Path to the particle state DB (default: <user-config-dir>/particle/state.db)")
	return cmd
}

func runBuild(cmd *cobra.Command, pack bool, dbPath string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}

	res, err := build.Build(cmd.Context(), build.Options{
		Source: os.DirFS(cwd),
	})
	if err != nil {
		printLogs(cmd.ErrOrStderr(), errLogs(err))
		return err
	}

	for _, w := range res.Warnings {
		fmt.Fprintln(cmd.ErrOrStderr(), "warning:", w.Error())
	}

	if pack {
		return runPack(cmd, res)
	}
	return runRegister(cmd, res, dbPath)
}

// runPack writes the particle FS as <name>-<version>.particle to CWD.
func runPack(cmd *cobra.Command, res *build.Result) error {
	name, version, err := manifestNameVersion(res.Particle)
	if err != nil {
		return err
	}
	outPath := fmt.Sprintf("%s-%s.particle", name, version)
	if err := writeParticleTar(res.Particle, outPath); err != nil {
		return fmt.Errorf("write %s: %w", outPath, err)
	}
	fmt.Fprintln(cmd.OutOrStdout(), outPath)
	return nil
}

// runRegister opens the state DB, walks the importer to set up
// any unconfigured credentials, then stores the particle in the
// registry.
func runRegister(cmd *cobra.Command, res *build.Result, dbPath string) error {
	dbPath, err := resolveDBPath(dbPath)
	if err != nil {
		return err
	}

	ctx := cmd.Context()

	declared, err := declaredCredentialNames(res.Particle)
	if err != nil {
		return err
	}

	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	// Only construct the credentials store when the manifest
	// declares anything — particles with no credentials don't
	// need the keychain to be available.
	var credStore credentials.Store
	var prompter importer.Prompter
	if len(declared) > 0 {
		sealer, err := credsqlite.NewKeyringSealer(keyringService, keyringName)
		if err != nil {
			return fmt.Errorf("keyring: %w", err)
		}
		credStore, err = credsqlite.New(ctx, db, sealer)
		if err != nil {
			return fmt.Errorf("credentials store: %w", err)
		}
		stdio := importer.NewStdioPrompter()
		if !stdio.IsTerminal() {
			// We need to prompt the user for any unset
			// credential, but stdin isn't a TTY — fail
			// fast with actionable advice instead of
			// blocking on a `bufio.Read` that'll never
			// come.
			return fmt.Errorf("particle declares credentials but stdin is not a terminal; configure credentials before piping into `particle build`, or use `--pack` to skip registration")
		}
		prompter = stdio
	}

	reg, err := regsqlite.New(ctx, db)
	if err != nil {
		return fmt.Errorf("registry: %w", err)
	}

	entry, err := importer.Import(ctx, res.Particle, importer.Options{
		Registry:    reg,
		Credentials: credStore,
		Prompter:    prompter,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "registered %s@%s\n", entry.Name, entry.Version)
	return nil
}

// declaredCredentialNames parses manifest.json and returns the
// names of every method declared under
// `capabilities.credentials.methods`, sorted. Empty slice when
// the capability isn't declared.
//
// The CLI uses this only as a "do we need to build the
// credentials store at all" gate; the per-method semantics live
// in the importer.
func declaredCredentialNames(fsys fs.FS) ([]string, error) {
	data, err := fs.ReadFile(fsys, "manifest.json")
	if err != nil {
		return nil, fmt.Errorf("read manifest.json: %w", err)
	}
	var m struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest.json: %w", err)
	}
	raw, ok := m.Capabilities["credentials"]
	if !ok {
		return nil, nil
	}
	var shell struct {
		Methods map[string]json.RawMessage `json:"methods"`
	}
	if err := json.Unmarshal(raw, &shell); err != nil {
		return nil, fmt.Errorf("parse manifest.json capabilities.credentials: %w", err)
	}
	out := make([]string, 0, len(shell.Methods))
	for n := range shell.Methods {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// manifestNameVersion reads the manifest.json from the result FS and
// extracts the kebab-case name + semver version that determine the
// output filename.
func manifestNameVersion(fsys fs.FS) (string, string, error) {
	data, err := fs.ReadFile(fsys, "manifest.json")
	if err != nil {
		return "", "", fmt.Errorf("read manifest.json: %w", err)
	}
	var m struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return "", "", fmt.Errorf("parse manifest.json: %w", err)
	}
	if m.Name == "" || m.Version == "" {
		return "", "", fmt.Errorf("manifest.json missing name or version")
	}
	return m.Name, m.Version, nil
}

// writeParticleTar packs every file in fsys into a deterministic USTAR
// tarball at outPath. "Deterministic" matters for content-addressing:
// two builds of the same source should produce byte-identical output.
// We rely on fs.WalkDir's lexical traversal order plus a zero ModTime.
func writeParticleTar(fsys fs.FS, outPath string) error {
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()

	tw := tar.NewWriter(f)
	defer tw.Close()

	return fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		hdr := &tar.Header{
			Name:    path,
			Mode:    0o644,
			Size:    int64(len(data)),
			ModTime: time.Time{},
			Format:  tar.FormatUSTAR,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("write header for %s: %w", path, err)
		}
		if _, err := tw.Write(data); err != nil {
			return fmt.Errorf("write body for %s: %w", path, err)
		}
		return nil
	})
}

// errLogs extracts captured wasm logs from a *build.Error, if the
// error is one. Returns nil for any other error type.
func errLogs(err error) []build.Log {
	type unwrappable interface {
		Unwrap() error
	}
	for cur := err; cur != nil; {
		if be, ok := cur.(*build.Error); ok {
			return be.Logs
		}
		if u, ok := cur.(unwrappable); ok {
			cur = u.Unwrap()
		} else {
			break
		}
	}
	return nil
}

// printLogs writes captured per-phase wasm output to w, separated by a
// header so a reader can tell which phase produced which bytes. Empty
// logs are suppressed (no header printed) to keep clean failures
// terse.
func printLogs(w interface{ Write(p []byte) (int, error) }, logs []build.Log) {
	for _, l := range logs {
		if len(l.Bytes) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n--- %s ---\n", l.Phase)
		_, _ = w.Write(l.Bytes)
		if len(l.Bytes) > 0 && l.Bytes[len(l.Bytes)-1] != '\n' {
			fmt.Fprintln(w)
		}
	}
}
