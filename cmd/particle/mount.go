package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"

	"github.com/partite-ai/particles/mounts"
	mountsqlite "github.com/partite-ai/particles/mounts/sqlite"
	regsqlite "github.com/partite-ai/particles/registry/sqlite"
	"github.com/partite-ai/particles/runtime"
)

func newMountCmd() *cobra.Command {
	var dbPath string
	cmd := &cobra.Command{
		Use:   "mount <name>[@version] [<mount-name> <host-path>]",
		Short: "Configure or list a particle's filesystem mounts",
		Long: `Map a declared filesystem mount to a host directory, or list a
particle's mounts and their current mappings.

With just a particle name, lists every declared mount (and temp mount)
with its description and current host mapping.

With a mount name and a host path, saves a persistent mapping so the
particle can read/write that directory on every run without --mount.
The host path must be an existing directory.`,
		Args: cobra.RangeArgs(1, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMount(cmd, args, dbPath)
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "", dbFlagUsage())
	return cmd
}

func newUnmountCmd() *cobra.Command {
	var dbPath string
	cmd := &cobra.Command{
		Use:   "unmount <name>[@version] <mount-name>",
		Short: "Remove a saved filesystem mount mapping",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUnmount(cmd, args, dbPath)
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "", dbFlagUsage())
	return cmd
}

func runMount(cmd *cobra.Command, args []string, dbPath string) error {
	if len(args) == 2 {
		return fmt.Errorf("provide a host path: particle mount %s %s <host-path>", args[0], args[1])
	}

	dbPath, err := resolveDBPath(dbPath)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	db, err := openStateDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	reg, err := regsqlite.New(ctx, db)
	if err != nil {
		return fmt.Errorf("registry: %w", err)
	}
	entry, err := resolveParticle(ctx, reg, args[0])
	if err != nil {
		return err
	}
	manifest, err := runtime.LoadManifest(entry.Particle)
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}
	mountBackend, err := mountsqlite.New(ctx, db)
	if err != nil {
		return fmt.Errorf("mounts store: %w", err)
	}
	store := mountBackend.Scoped(entry.Name)

	if len(args) == 1 {
		return listMounts(cmd, entry.Name, entry.Version, manifest, store)
	}

	// Set a mapping: args[1] = mount name, args[2] = host path.
	mountName, hostPath := args[1], args[2]
	decl, ok := manifest.Capabilities.Filesystem.Mounts[mountName]
	if !ok {
		if _, isTemp := manifest.Capabilities.Filesystem.Temp[mountName]; isTemp {
			return fmt.Errorf("%q is a temp mount — it's provisioned automatically and can't be mapped", mountName)
		}
		return fmt.Errorf("%s@%s declares no mount named %q", entry.Name, entry.Version, mountName)
	}

	info, err := os.Stat(hostPath)
	if err != nil {
		return fmt.Errorf("host path %s: %w", hostPath, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("host path %s is not a directory", hostPath)
	}
	abs, err := filepath.Abs(hostPath)
	if err != nil {
		return fmt.Errorf("resolve host path: %w", err)
	}

	if err := store.Set(ctx, mountName, abs); err != nil {
		return fmt.Errorf("save mapping: %w", err)
	}
	access := "read-write"
	if decl.Access == runtime.MountReadOnly {
		access = "read-only"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "mapped %s mount %q → %s (%s)\n", entry.Name, mountName, abs, access)
	return nil
}

func runUnmount(cmd *cobra.Command, args []string, dbPath string) error {
	dbPath, err := resolveDBPath(dbPath)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	db, err := openStateDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	reg, err := regsqlite.New(ctx, db)
	if err != nil {
		return fmt.Errorf("registry: %w", err)
	}
	entry, err := resolveParticle(ctx, reg, args[0])
	if err != nil {
		return err
	}
	mountBackend, err := mountsqlite.New(ctx, db)
	if err != nil {
		return fmt.Errorf("mounts store: %w", err)
	}
	if err := mountBackend.Scoped(entry.Name).Delete(ctx, args[1]); err != nil {
		return fmt.Errorf("remove mapping: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "unmapped %s mount %q\n", entry.Name, args[1])
	return nil
}

// listMounts prints a particle's declared mounts and temp mounts,
// leading with name + description and annotating each regular mount
// with its saved host mapping (or "(unmapped)").
func listMounts(cmd *cobra.Command, name, version string, manifest runtime.Manifest, store mounts.Store) error {
	mappings, err := store.List(cmd.Context())
	if err != nil {
		return fmt.Errorf("list mappings: %w", err)
	}
	mapped := make(map[string]string, len(mappings))
	for _, m := range mappings {
		mapped[m.Name] = m.HostPath
	}

	fsCap := manifest.Capabilities.Filesystem
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Mounts for %s@%s:\n", name, version)

	if len(fsCap.Mounts) == 0 && len(fsCap.Temp) == 0 {
		fmt.Fprintln(out, "  (none declared)")
		return nil
	}

	names := make([]string, 0, len(fsCap.Mounts))
	for n := range fsCap.Mounts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		decl := fsCap.Mounts[n]
		access := "read-write"
		if decl.Access == runtime.MountReadOnly {
			access = "read-only"
		}
		req := "optional"
		if decl.Required {
			req = "required"
		}
		mapping := "(unmapped)"
		if hp, ok := mapped[n]; ok {
			mapping = hp
		}
		fmt.Fprintf(out, "\n  %s — %s\n", n, decl.Description)
		fmt.Fprintf(out, "    %s, %s, at %s\n", access, req, decl.Path)
		fmt.Fprintf(out, "    mapped: %s\n", mapping)
	}

	if len(fsCap.Temp) > 0 {
		tnames := make([]string, 0, len(fsCap.Temp))
		for n := range fsCap.Temp {
			tnames = append(tnames, n)
		}
		sort.Strings(tnames)
		fmt.Fprintln(out, "\n  Temp (provisioned automatically, cleared each run):")
		for _, n := range tnames {
			decl := fsCap.Temp[n]
			fmt.Fprintf(out, "  %s — %s\n", n, decl.Description)
			fmt.Fprintf(out, "    max size: %s, at %s\n", decl.MaxSize, decl.Path)
		}
	}
	return nil
}
