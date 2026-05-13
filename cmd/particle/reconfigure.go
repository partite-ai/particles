package main

import (
	"database/sql"
	"fmt"

	"github.com/spf13/cobra"

	credsqlite "github.com/partite-ai/particle/credentials/sqlite"
	"github.com/partite-ai/particle/importer"
	regsqlite "github.com/partite-ai/particle/registry/sqlite"
)

func newReconfigureCmd() *cobra.Command {
	var dbPath string
	cmd := &cobra.Command{
		Use:   "reconfigure <name>",
		Short: "Re-run credential setup for a registered particle",
		Long: `Walk the credential-setup flow against a registered particle,
letting the user pick a (possibly different) authentication method and
provide its values. The previously-configured credential is removed
only after the new one is successfully written, so an aborted
reconfigure leaves the prior state intact.

The chosen method applies to every registered version of the particle
(credentials are per-name, not per-version). The latest registered
version's manifest drives which methods are offered as choices.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runReconfigure(cmd, args[0], dbPath)
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "", "Path to the particle state DB (default: <user-config-dir>/particle/state.db)")
	return cmd
}

func runReconfigure(cmd *cobra.Command, name, dbPath string) error {
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

	sealer, err := credsqlite.NewKeyringSealer(keyringService, keyringName)
	if err != nil {
		return fmt.Errorf("keyring: %w", err)
	}
	credStore, err := credsqlite.New(ctx, db, sealer)
	if err != nil {
		return fmt.Errorf("credentials store: %w", err)
	}

	stdio := importer.NewStdioPrompter()
	if !stdio.IsTerminal() {
		return fmt.Errorf("reconfigure needs an interactive terminal")
	}

	entry, method, err := importer.Reconfigure(ctx, name, importer.Options{
		Registry:    reg,
		Credentials: credStore,
		Prompter:    stdio,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "reconfigured %s — using %s\n", entry.Name, method)
	return nil
}
