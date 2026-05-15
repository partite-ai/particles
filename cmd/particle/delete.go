package main

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/partite-ai/particle/registry"
	regsqlite "github.com/partite-ai/particle/registry/sqlite"
)

func newDeleteCmd() *cobra.Command {
	var dbPath string
	cmd := &cobra.Command{
		Use:     "delete <name>[@version]",
		Aliases: []string{"rm"},
		Short:   "Remove a registered particle",
		Long: `Removes a registered particle. With <name>@<version>, only that
version is dropped; with just <name>, every version of the particle is
removed and the per-name auth-method selection is cleared. Idempotent.

Credentials in the credentials store are NOT touched — they live
on a separate axis (see ` + "`particle reconfigure`" + `).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDelete(cmd, args[0], dbPath)
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "", dbFlagUsage())
	return cmd
}

func runDelete(cmd *cobra.Command, target, dbPath string) error {
	name, version := parsePingTarget(target)
	if name == "" {
		return fmt.Errorf("name is required")
	}
	dbPath, err := resolveDBPath(dbPath)
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

	versions, err := versionsToDelete(ctx, reg, name, version)
	if err != nil {
		return err
	}
	for _, v := range versions {
		if err := reg.Delete(ctx, name, v); err != nil {
			return fmt.Errorf("delete %s@%s: %w", name, v, err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "deleted %s@%s\n", name, v)
	}
	return nil
}

// versionsToDelete returns the list of versions the delete should
// hit. With an explicit version, that's just it (errors when the
// (name, version) pair isn't registered, so a typo doesn't
// silently no-op). Without one, every registered version of name.
func versionsToDelete(ctx context.Context, reg registry.Registry, name, version string) ([]string, error) {
	entries, err := reg.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list registry: %w", err)
	}
	if version != "" {
		for _, e := range entries {
			if e.Name == name && e.Version == version {
				return []string{version}, nil
			}
		}
		return nil, fmt.Errorf("%s@%s not registered", name, version)
	}
	var out []string
	for _, e := range entries {
		if e.Name == name {
			out = append(out, e.Version)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s not registered", name)
	}
	return out, nil
}
