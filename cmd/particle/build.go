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

	"github.com/klauspost/compress/zstd"
	"github.com/spf13/cobra"

	"github.com/partite-ai/particles/credentials"
	credsqlite "github.com/partite-ai/particles/credentials/sqlite"
	"github.com/partite-ai/particles/importer"
	"github.com/partite-ai/particles/internal/build"
	regsqlite "github.com/partite-ai/particles/registry/sqlite"
)

func newBuildCmd() *cobra.Command {
	var (
		pack         bool
		dbPath       string
		profile      string
		acceptPerms  bool
		confirmPerms bool
	)
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build a particle and register it in the local state DB",
		Long: `Build the particle in the current directory.

By default the result is registered in the local state DB. Pass
--pack to write a <name>-<version>.particle tarball to CWD instead.

Registration prompts to confirm the particle's declared capabilities
when they differ from the previously-registered version (or on a
fresh install) and walks credential setup for any unconfigured
authentication method.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if profile != "" {
				stop, err := startProfile(profile, cmd.ErrOrStderr())
				if err != nil {
					return err
				}
				defer stop()
			}
			return runBuild(cmd, pack, dbPath, permissionModeFromFlags(acceptPerms, confirmPerms))
		},
	}
	cmd.Flags().BoolVar(&pack, "pack", false, "Write <name>-<version>.particle to CWD instead of registering")
	cmd.Flags().StringVar(&dbPath, "db", "", dbFlagUsage())
	cmd.Flags().BoolVarP(&acceptPerms, "yes", "y", false, "Auto-accept the permission summary (does not skip credential prompts)")
	cmd.Flags().BoolVar(&confirmPerms, "confirm-permissions", false, "Force the permission prompt even when capabilities match the prior version")
	cmd.Flags().StringVar(&profile, "profile", "", "Write CPU + heap pprof profiles with this prefix")
	_ = cmd.Flags().MarkHidden("profile")
	cmd.MarkFlagsMutuallyExclusive("yes", "confirm-permissions")
	return cmd
}

func runBuild(cmd *cobra.Command, pack bool, dbPath string, permMode importer.PermissionMode) error {
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
	return runRegister(cmd, res, dbPath, permMode)
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
func runRegister(cmd *cobra.Command, res *build.Result, dbPath string, permMode importer.PermissionMode) error {
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

	// The prompter is built unconditionally: permission
	// confirmation needs it even for particles without
	// credentials. We don't gate on IsTerminal up front — a
	// no-caps particle never prompts, so requiring a TTY would
	// pessimistically reject valid pipelines. If we DO end up
	// needing to prompt on a non-TTY, the StdioPrompter's
	// Read surfaces a clear EOF error.
	var prompter importer.Prompter = importer.NewStdioPrompter()

	var credStore credentials.Store
	if len(declared) > 0 {
		sealer, err := credsqlite.NewKeyringSealer(keyringService, keyringName)
		if err != nil {
			return fmt.Errorf("keyring: %w", err)
		}
		credStore, err = credsqlite.New(ctx, db, sealer)
		if err != nil {
			return fmt.Errorf("credentials store: %w", err)
		}
	}

	reg, err := regsqlite.New(ctx, db)
	if err != nil {
		return fmt.Errorf("registry: %w", err)
	}

	entry, err := importer.Import(ctx, res.Particle, importer.Options{
		Registry:       reg,
		Credentials:    credStore,
		Prompter:       prompter,
		PermissionMode: permMode,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "registered %s@%s\n", entry.Name, entry.Version)
	return nil
}

// declaredCredentialNames parses manifest.json and returns the
// names of every credential declared at the top-level
// `credentials` map, sorted. Empty slice when none are declared.
//
// The CLI uses this only as a "do we need to build the
// credentials store at all" gate; the per-credential / per-method
// semantics live in the importer.
func declaredCredentialNames(fsys fs.FS) ([]string, error) {
	data, err := fs.ReadFile(fsys, "manifest.json")
	if err != nil {
		return nil, fmt.Errorf("read manifest.json: %w", err)
	}
	var m struct {
		Credentials map[string]json.RawMessage `json:"credentials"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest.json: %w", err)
	}
	out := make([]string, 0, len(m.Credentials))
	for n := range m.Credentials {
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

// writeParticleTar packs every file in fsys into a deterministic
// zstd-compressed tar archive at outPath. "Deterministic" matters
// for content-addressing: two builds of the same source should
// produce byte-identical output. We rely on fs.WalkDir's lexical
// traversal order, a zero ModTime, the fixed POSIX.1-1988 tar
// header dialect (Format: tar.FormatUSTAR below — picked over PAX
// because PAX writes extended headers carrying metadata that's
// harder to keep byte-stable), a fixed zstd level, and
// concurrency=1 (klauspost's zstd encoder can produce different
// frame splits under multi-threading).
func writeParticleTar(fsys fs.FS, outPath string) error {
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()

	zw, err := zstd.NewWriter(f,
		zstd.WithEncoderLevel(zstd.SpeedBetterCompression),
		zstd.WithEncoderConcurrency(1),
	)
	if err != nil {
		return fmt.Errorf("zstd writer: %w", err)
	}
	tw := tar.NewWriter(zw)

	walkErr := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
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
	if walkErr != nil {
		_ = tw.Close()
		_ = zw.Close()
		return walkErr
	}
	// Close in LIFO: tar (trailer into zw) → zstd (final frame into
	// f). Failures here turn a corrupt .particle into a clear error
	// rather than a confusing import failure downstream.
	if err := tw.Close(); err != nil {
		_ = zw.Close()
		return fmt.Errorf("close tar: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("close zstd: %w", err)
	}
	return nil
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
