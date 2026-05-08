package main

import (
	"archive/tar"
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"testing/fstest"

	"github.com/spf13/cobra"

	"github.com/partite-ai/particle/credentials"
	credsqlite "github.com/partite-ai/particle/credentials/sqlite"
	"github.com/partite-ai/particle/importer"
	regsqlite "github.com/partite-ai/particle/registry/sqlite"
)

func newImportCmd() *cobra.Command {
	var dbPath string
	cmd := &cobra.Command{
		Use:   "import <file.particle>",
		Short: "Import a .particle tarball into the local registry",
		Long: `Reads a .particle tarball (the output of ` + "`particle build --pack`" + `),
expands it in memory, then runs the same import flow as ` + "`particle build`" + ` —
prompting for credentials when the manifest declares them, and writing
to the registry on success.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runImport(cmd, args[0], dbPath)
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "", "Path to the particle state DB (default: <user-config-dir>/particle/state.db)")
	return cmd
}

func runImport(cmd *cobra.Command, path, dbPath string) error {
	particleFS, err := readParticleTar(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	declared, err := declaredCredentialNames(particleFS)
	if err != nil {
		return err
	}

	dbPath, err = resolveDBPath(dbPath)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	reg, err := regsqlite.New(ctx, db)
	if err != nil {
		return fmt.Errorf("registry: %w", err)
	}

	// Mirror the build CLI's policy: only construct the
	// credentials store when the manifest declares anything to
	// configure. A no-creds particle imports without ever
	// touching the keychain.
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
			return errors.New("particle import: particle declares credentials but stdin is not a terminal")
		}
		prompter = stdio
	}

	entry, err := importer.Import(ctx, particleFS, importer.Options{
		Registry:    reg,
		Credentials: credStore,
		Prompter:    prompter,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "imported %s@%s\n", entry.Name, entry.Version)
	return nil
}

// readParticleTar opens the file at path, walks the USTAR
// archive, and returns an in-memory FS keyed by file path.
// Mirrors the deterministic-USTAR layout `writeParticleTar`
// produces — but is tolerant of any tarball with regular files.
func readParticleTar(path string) (fs.FS, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readTar(f)
}

func readTar(r io.Reader) (fs.FS, error) {
	tr := tar.NewReader(r)
	out := fstest.MapFS{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar header: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			// Skip directories, symlinks, etc. — particle
			// tarballs only carry regular files; anything
			// else is suspect.
			continue
		}
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, tr); err != nil {
			return nil, fmt.Errorf("tar body for %s: %w", hdr.Name, err)
		}
		out[hdr.Name] = &fstest.MapFile{Data: buf.Bytes()}
	}
	return out, nil
}
