package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"

	"github.com/partite-ai/particles/registry"
	regsqlite "github.com/partite-ai/particles/registry/sqlite"
	"github.com/partite-ai/particles/runtime"
)

func newPingCmd() *cobra.Command {
	var dbPath string
	cmd := &cobra.Command{
		Use:   "ping <name>[@version]",
		Short: "Call a registered particle's ping handler",
		Long: `Call the registered particle's ping handler. Exits 0 with a
"ping: not implemented" message if the particle didn't define one,
0 with the result on success, non-zero on handler error.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPing(cmd, args[0], dbPath)
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "", dbFlagUsage())
	return cmd
}

func runPing(cmd *cobra.Command, target, dbPath string) error {
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
	entry, err := resolveParticle(ctx, reg, target)
	if err != nil {
		return err
	}

	p, teardown, err := bootParticle(ctx, db, entry, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	defer teardown()

	pr, err := p.Ping(ctx)
	if err != nil {
		var he *runtime.HealthError
		if errors.As(err, &he) && he.NotImplemented {
			// Spec: not-implemented is a successful exit
			// with a clear message.
			fmt.Fprintln(cmd.OutOrStdout(), "ping: not implemented")
			return nil
		}
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), formatPing(pr))
	return nil
}

// parsePingTarget splits "<name>[@version]" into its parts.
func parsePingTarget(s string) (name, version string) {
	if i := strings.Index(s, "@"); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

// resolveLatestVersion returns the highest semver version
// registered for name. golang.org/x/mod/semver expects a leading
// "v"; we prefix when the registered string lacks one. Versions
// that aren't valid semver compare as "lower" than any valid one
// (semver.Compare canonicalizes invalid inputs to ""), so the
// resolver still works when a particle uses an idiosyncratic
// version scheme — it just won't beat a valid sibling.
func resolveLatestVersion(ctx context.Context, reg registry.Registry, name string) (string, error) {
	entries, err := reg.List(ctx)
	if err != nil {
		return "", fmt.Errorf("list registry: %w", err)
	}
	var matches []string
	for _, e := range entries {
		if e.Name == name {
			matches = append(matches, e.Version)
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("%s not registered", name)
	}
	sort.Slice(matches, func(i, j int) bool {
		return semver.Compare(canonicalSemver(matches[i]), canonicalSemver(matches[j])) > 0
	})
	return matches[0], nil
}

// canonicalSemver returns v with a leading "v" so it round-trips
// through golang.org/x/mod/semver's API, which only recognizes
// "v"-prefixed strings.
func canonicalSemver(v string) string {
	if strings.HasPrefix(v, "v") {
		return v
	}
	return "v" + v
}

// formatPing turns a PingResult into a single-line status string.
// Examples:
//
//	"ok"
//	"ok: all good"
//	"degraded: cache cold (rebuilding 12%)"
func formatPing(pr *runtime.PingResult) string {
	out := pingStatusName(pr.Status)
	if pr.Message != "" {
		out += ": " + pr.Message
	}
	if pr.Details != "" {
		out += " (" + pr.Details + ")"
	}
	return out
}

func pingStatusName(s runtime.PingStatus) string {
	switch s {
	case runtime.PingStatusOK:
		return "ok"
	case runtime.PingStatusDegraded:
		return "degraded"
	case runtime.PingStatusUnhealthy:
		return "unhealthy"
	}
	return fmt.Sprintf("status(%d)", int(s))
}
