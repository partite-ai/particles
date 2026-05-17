package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	regsqlite "github.com/partite-ai/particles/registry/sqlite"
	"github.com/partite-ai/particles/runtime"
)

func newRunCmd() *cobra.Command {
	var (
		dbPath  string
		profile string
	)
	cmd := &cobra.Command{
		Use:   "run <name>[@version] [tool] [tool-flags]",
		Short: "Call a tool on a registered particle",
		Long: `Call a tool on the registered particle. Tool flags are derived
from the tool's input schema.

Without a tool name, lists the available tools. Run
"particle run <name>[@version] <tool> --help" for tool-specific flags.

The --db flag must precede the particle name; everything after
the name is forwarded to the tool.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if profile != "" {
				stop, err := startProfile(profile, cmd.ErrOrStderr())
				if err != nil {
					return err
				}
				defer stop()
			}
			return runRun(cmd, args, dbPath)
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "", dbFlagUsage())
	cmd.Flags().StringVar(&profile, "profile", "", "Write CPU + heap pprof profiles with this prefix")
	_ = cmd.Flags().MarkHidden("profile")
	// SetInterspersed(false) tells pflag (cobra's parser) to stop
	// processing flags at the first positional arg, so tool flags
	// like `--input` aren't intercepted as unknown.
	cmd.Flags().SetInterspersed(false)
	return cmd
}

func runRun(cmd *cobra.Command, args []string, dbPath string) error {
	target := args[0]
	rest := args[1:]

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
	entry, err := resolveParticle(ctx, reg, target)
	if err != nil {
		return err
	}

	p, teardown, err := bootParticle(ctx, db, entry)
	if err != nil {
		return err
	}
	defer teardown()

	tools, err := p.ListTools(ctx)
	if err != nil {
		return fmt.Errorf("list tools: %w", err)
	}

	// `particle run yaml-tools` (no tool) or `particle run yaml-tools --help`
	// → list tools. We treat a leading "-" in rest[0] as "no tool"
	// so --help works as a discovery aid at this level too.
	if len(rest) == 0 || (len(rest[0]) > 0 && rest[0][0] == '-') {
		printToolListing(cmd.ErrOrStderr(), entry.Name, entry.Version, tools)
		return nil
	}
	toolName := rest[0]
	toolArgs := rest[1:]

	td := findTool(tools, toolName)
	if td == nil {
		printToolListing(cmd.ErrOrStderr(), entry.Name, entry.Version, tools)
		return fmt.Errorf("tool %q not found in %s@%s", toolName, entry.Name, entry.Version)
	}

	progName := fmt.Sprintf("particle run %s@%s %s", entry.Name, entry.Version, td.Name)
	toolFS, encode, err := schemaToFlags(progName, td.InputSchemaJSON)
	if err != nil {
		return err
	}
	toolFS.SetOutput(cmd.ErrOrStderr())
	toolFS.Usage = func() { writeToolUsage(cmd.ErrOrStderr(), toolFS, progName, td.Description) }

	if err := toolFS.Parse(toolArgs); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			// pflag prints help on its own; treat as success.
			return nil
		}
		return err
	}

	argsJSON, err := encode()
	if err != nil {
		fmt.Fprintln(cmd.ErrOrStderr())
		toolFS.Usage()
		return err
	}

	if vErr := validateArgs(td.InputSchemaJSON, argsJSON); vErr != nil {
		fmt.Fprintln(cmd.ErrOrStderr())
		toolFS.Usage()
		return vErr
	}

	out, err := p.CallTool(ctx, td.Name, argsJSON)
	if err != nil {
		fmt.Fprintln(cmd.ErrOrStderr())
		toolFS.Usage()
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), string(out))
	return nil
}

// writeToolUsage prints the tool's description followed by the
// flag set's auto-generated usage. Matched output is the same
// regardless of whether we got here via --help, a parse error, an
// encode error, a validation error, or a tool error, so the user
// always sees the schema-derived flags as a quick reference.
func writeToolUsage(w io.Writer, fs *pflag.FlagSet, progName, description string) {
	if description != "" {
		fmt.Fprintf(w, "%s\n\n", description)
	}
	fmt.Fprintf(w, "Usage of %s:\n", progName)
	prev := fs.Output()
	fs.SetOutput(w)
	fs.PrintDefaults()
	fs.SetOutput(prev)
}

func findTool(tools []runtime.ToolDef, name string) *runtime.ToolDef {
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
		}
	}
	return nil
}

func printToolListing(w io.Writer, name, version string, tools []runtime.ToolDef) {
	fmt.Fprintf(w, "Tools in %s@%s:\n", name, version)
	if len(tools) == 0 {
		fmt.Fprintln(w, "  (none)")
		return
	}
	width := 0
	for _, td := range tools {
		if len(td.Name) > width {
			width = len(td.Name)
		}
	}
	for _, td := range tools {
		fmt.Fprintf(w, "  %-*s  %s\n", width, td.Name, td.Description)
	}
	fmt.Fprintf(w, "\nRun \"particle run %s <tool> --help\" for tool-specific flags.\n", name)
}

// Compile-time guard.
var _ context.Context = context.Background()
