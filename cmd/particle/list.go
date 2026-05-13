package main

import (
	"database/sql"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	credsqlite "github.com/partite-ai/particle/credentials/sqlite"
	regsqlite "github.com/partite-ai/particle/registry/sqlite"
)

func newListCmd() *cobra.Command {
	var dbPath string
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List registered particles",
		Long: `Lists every registered particle, one row per (name, version), with the
configured authentication method (or "(unconfigured)" when none is set).
Auth selection is per-particle-name, so every version of the same
particle reports the same method.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runList(cmd, dbPath)
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "", "Path to the particle state DB (default: <user-config-dir>/particle/state.db)")
	return cmd
}

func runList(cmd *cobra.Command, dbPath string) error {
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
	entries, err := reg.List(ctx)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "no particles registered")
		return nil
	}

	// `particle list` only reads credential NAMES (the method
	// name IS the selection); it never decrypts. Construct a
	// sealer-less Store so the OS keychain isn't prompted just
	// to render the table.
	credStore, err := credsqlite.New(ctx, db, nil)
	if err != nil {
		return fmt.Errorf("credentials store: %w", err)
	}
	methods := map[string]string{}

	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tVERSION\tAUTH")
	for _, e := range entries {
		auth, ok := methods[e.Name]
		if !ok {
			auth, err = credStore.ConfiguredMethod(ctx, e.Name)
			if err != nil {
				return fmt.Errorf("lookup method for %s: %w", e.Name, err)
			}
			methods[e.Name] = auth
		}
		if auth == "" {
			auth = "(unconfigured)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", e.Name, e.Version, auth)
	}
	return tw.Flush()
}
