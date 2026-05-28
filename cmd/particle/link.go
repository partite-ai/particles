package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	regsqlite "github.com/partite-ai/particles/registry/sqlite"
)

// linkSpec is the resolved description of a link to create, shared by
// the platform-specific writeLink implementations (sh script on
// Unix, trampoline .exe on Windows).
type linkSpec struct {
	// path is the file to create — the executable the user will run.
	path string
	// particleBin is the absolute path to the particle binary that
	// the link should invoke.
	particleBin string
	// target is the "<name>[@version]" the link runs, exactly as the
	// user passed it. A bare name keeps tracking the latest version
	// at run time; "name@version" pins.
	target string
	// dbPath, when non-empty, is forwarded as `--db <dbPath>` so the
	// link targets the same registry the user linked against. Empty
	// means "use default DB resolution at run time", matching plain
	// `particle run <target>`.
	dbPath string
}

func newLinkCmd() *cobra.Command {
	var (
		dbPath string
		force  bool
	)
	cmd := &cobra.Command{
		Use:   "link <name>[@version] <path>",
		Short: "Create an executable that runs a particle",
		Long: `Create a small executable at <path> that runs the moral
equivalent of "particle run <name>[@version]". Arguments passed to the
linked command are forwarded to the particle, so

    particle link github-tools ./gh
    ./gh list-issues --repo octocat/hello

behaves like "particle run github-tools list-issues --repo octocat/hello".

A bare <name> keeps resolving to the latest registered version each
time the link runs; "<name>@<version>" pins to that version.

On Unix the link is a tiny sh script; on Windows it is a self-contained
launcher .exe. Refuses to overwrite an existing file unless --force.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLink(cmd, args[0], args[1], dbPath, cmd.Flags().Changed("db"), force)
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "", dbFlagUsage())
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Overwrite <path> if it already exists")
	return cmd
}

func runLink(cmd *cobra.Command, target, path, dbPath string, dbSet, force bool) error {
	resolvedDB, err := resolveDBPath(dbPath)
	if err != nil {
		return err
	}

	// Validate the particle resolves now so a typo fails at link
	// time rather than the first time the link is run.
	ctx := cmd.Context()
	db, err := openStateDB(resolvedDB)
	if err != nil {
		return err
	}
	defer db.Close()
	reg, err := regsqlite.New(ctx, db)
	if err != nil {
		return fmt.Errorf("registry: %w", err)
	}
	if _, err := resolveParticle(ctx, reg, target); err != nil {
		return err
	}

	bin, err := resolveParticleBinary()
	if err != nil {
		return err
	}

	spec := linkSpec{
		path:        path,
		particleBin: bin,
		target:      target,
	}
	// Only pin the DB into the link when the user asked for a specific
	// one; absolutize it so the link works from any working directory.
	if dbSet {
		abs, err := filepath.Abs(resolvedDB)
		if err != nil {
			return fmt.Errorf("resolve --db path: %w", err)
		}
		spec.dbPath = abs
	}

	finalPath, err := writeLink(spec, force)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "linked %s -> particle run %s\n", finalPath, target)
	return nil
}

// resolveParticleBinary returns the absolute path of the running
// particle binary, to be baked into the link. We deliberately do NOT
// resolve symlinks: package managers (Homebrew, apt) expose particle
// through a stable symlink that points at a version-specific real
// path, and pinning the resolved path would break the link on the
// next upgrade. os.Executable already returns an absolute path.
func resolveParticleBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate particle binary: %w", err)
	}
	return exe, nil
}
