package main

import (
	"fmt"

	"github.com/spf13/cobra"

	credsqlite "github.com/partite-ai/particles/credentials/sqlite"
	"github.com/partite-ai/particles/importer"
	regsqlite "github.com/partite-ai/particles/registry/sqlite"
)

func newReconfigureCmd() *cobra.Command {
	var (
		dbPath     string
		reauthOnly bool
	)
	cmd := &cobra.Command{
		Use:   "reconfigure <particle> [credential]",
		Short: "Re-run credential setup for a registered particle",
		Long: `Walk the credential-setup flow against a registered particle,
letting the user pick a (possibly different) authentication method and
provide its values. The previously-configured credential's secrets are
wiped only after the new ones are successfully written, so an aborted
reconfigure leaves the prior state intact.

When the particle declares multiple credentials in its manifest, pass
the credential name as the second argument to pick which one to
reconfigure. With a single declared credential the argument may be
omitted.

When the chosen method matches the one already configured, every
prompt defaults to the stored value — press Enter to keep it. Secret
prompts show "(press Enter to keep current)" for the same reason.
Switching to a different method always asks for every value fresh.

With --reauth-only, skip every config prompt and only re-run the
OAuth authorization flow, rotating the stored access (and refresh)
token. The client ID, client secret, scopes, and URLs are preserved
as-is. Only applies to oauth2 credentials.

The chosen method applies to every registered version of the particle
(credentials are per-name, not per-version). The latest registered
version's manifest drives which methods are offered as choices.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			particleName := args[0]
			credName := ""
			if len(args) > 1 {
				credName = args[1]
			}
			return runReconfigure(cmd, particleName, credName, dbPath, reauthOnly)
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "", dbFlagUsage())
	cmd.Flags().BoolVar(&reauthOnly, "reauth-only", false,
		"Re-run only the OAuth flow; preserve client ID, client secret, and other config. oauth2 credentials only.")
	return cmd
}

func runReconfigure(cmd *cobra.Command, particleName, credName, dbPath string, reauthOnly bool) error {
	if particleName == "" {
		return fmt.Errorf("particle name is required")
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

	credBackend, err := credsqlite.New(ctx, db, openCredentialSealer(cmd.ErrOrStderr()))
	if err != nil {
		return fmt.Errorf("credentials store: %w", err)
	}

	stdio := importer.NewStdioPrompter()
	if !stdio.IsTerminal() {
		return fmt.Errorf("reconfigure needs an interactive terminal")
	}

	importerOpts := importer.Options{
		Registry:    reg,
		Credentials: credBackend.Scoped(particleName),
		Prompter:    stdio,
	}

	if reauthOnly {
		entry, method, err := importer.ReauthOAuth(ctx, particleName, credName, importerOpts)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "re-authenticated %s.%s\n", entry.Name, method)
		return nil
	}

	entry, method, err := importer.Reconfigure(ctx, particleName, credName, importerOpts)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "reconfigured %s — using %s\n", entry.Name, method)
	return nil
}
