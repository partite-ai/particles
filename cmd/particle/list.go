package main

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/partite-ai/particles/credentials"
	credsqlite "github.com/partite-ai/particles/credentials/sqlite"
	regsqlite "github.com/partite-ai/particles/registry/sqlite"
)

func newListCmd() *cobra.Command {
	var dbPath string
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List registered particles",
		Long: `Lists every registered particle, one row per (name, version), with the
configured credentials in "credName=method" form (or "(unconfigured)"
when none are set). Credential configuration is per-particle-name, so
every version of the same particle reports the same set.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runList(cmd, dbPath)
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "", dbFlagUsage())
	return cmd
}

func runList(cmd *cobra.Command, dbPath string) error {
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
	entries, err := reg.List(ctx)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "no particles registered")
		return nil
	}

	// `particle list` only reads metadata; it never decrypts.
	// Construct a sealer-less Backend so the OS keychain isn't
	// prompted just to render the table.
	credBackend, err := credsqlite.New(ctx, db, nil)
	if err != nil {
		return fmt.Errorf("credentials store: %w", err)
	}
	// Cache by particle name — every version of the same particle
	// shares its credential set, so a List call per particle is
	// enough.
	summaries := map[string]string{}

	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tVERSION\tCREDENTIALS")
	for _, e := range entries {
		summary, ok := summaries[e.Name]
		if !ok {
			entries, err := credBackend.Scoped(e.Name).List(ctx)
			if err != nil {
				return fmt.Errorf("list credentials for %s: %w", e.Name, err)
			}
			summary = formatCredentialSummary(entries)
			summaries[e.Name] = summary
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", e.Name, e.Version, summary)
	}
	return tw.Flush()
}

// formatCredentialSummary renders a particle's configured
// credentials as "credName=method,…" — sorted for stable output.
// Empty input → "(unconfigured)".
func formatCredentialSummary(entries []credentials.ListEntry) string {
	if len(entries) == 0 {
		return "(unconfigured)"
	}
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		parts = append(parts, e.Name+"="+e.Method)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}
