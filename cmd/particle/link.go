package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	regsqlite "github.com/partite-ai/particles/registry/sqlite"
	"github.com/partite-ai/particles/runtime"
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
	// target is the "<name>[@version]" the link runs, with the
	// optional /tool selector stripped — what `particle run` takes
	// as its first positional. A bare name keeps tracking the
	// latest version at run time; "name@version" pins.
	target string
	// tool, when non-empty, binds the link to a single tool on the
	// particle. The shim invokes `particle run <target> <tool>` and
	// the caller never names the tool. Empty means "whole-particle
	// link", same shape as before this field existed.
	tool string
	// mounts holds resolved "<name>=<abs-host-path>" entries for
	// each --mount the user supplied at link time. Each one is
	// emitted as `--mount <entry>` before the target in the shim,
	// so the linked command always runs with these mappings — the
	// caller can still add more --mount flags at run time and they
	// concatenate.
	mounts []string
	// traceLevel, when traceLevelSet, is baked as
	// `--trace-http=<name>` before the target so the linked command
	// always runs with the chosen tracing level.
	traceLevel    runtime.TraceLevel
	traceLevelSet bool
	// dbPath, when non-empty, is forwarded as `--db <dbPath>` so the
	// link targets the same registry the user linked against. Empty
	// means "use default DB resolution at run time", matching plain
	// `particle run <target>`.
	dbPath string
}

func newLinkCmd() *cobra.Command {
	var (
		dbPath     string
		force      bool
		mountFlags []string
		traceLevel runtime.TraceLevel
	)
	cmd := &cobra.Command{
		Use:   "link [--mount name=path] [--trace-http=level] <name>[@version][/tool] <path>",
		Short: "Create an executable that runs a particle (optionally one tool, with baked-in run flags)",
		Long: `Create a small executable at <path> that runs the moral
equivalent of "particle run <name>[@version]". Arguments passed to the
linked command are forwarded to the particle, so

    particle link github-tools ./gh
    ./gh list-issues --repo octocat/hello

behaves like "particle run github-tools list-issues --repo octocat/hello".

A bare <name> keeps resolving to the latest registered version each
time the link runs; "<name>@<version>" pins to that version.

Append "/<tool>" to the target to bind the link to a single tool. The
shim then names the tool itself, so the caller invokes the link with
just the tool's own flags:

    particle link github-tools/list_issues ./list-issues
    ./list-issues --repo octocat/hello   # tool name is implicit

Pre-tool "particle run" flags supplied at link time are baked into
the shim so every invocation runs with them:

  --mount name=path   Map a declared filesystem mount; repeat for
                      multiple. Validated at link time against the
                      particle's manifest and the host path's
                      existence.
  --trace-http[=lvl]  Bake HTTP tracing at level basic|headers|full
                      (default basic) into the link.

On Unix the link is a tiny sh script; on Windows it is a self-contained
launcher .exe. Refuses to overwrite an existing file unless --force.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLink(cmd, args[0], args[1], mountFlags, traceLevel, cmd.Flags().Changed("trace-http"), dbPath, cmd.Flags().Changed("db"), force)
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "", dbFlagUsage())
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Overwrite <path> if it already exists")
	cmd.Flags().StringArrayVar(&mountFlags, "mount", nil, "Bake a --mount name=host/path into the link (repeatable; validated against the manifest)")
	addHTTPTraceFlag(cmd, &traceLevel)
	return cmd
}

// splitLinkTarget separates the optional "/tool" suffix from the
// "<name>[@version]" prefix. The split is on the first '/', which is
// unambiguous because neither particle names nor version strings
// contain '/'. Returns (target-without-tool, tool-or-empty).
func splitLinkTarget(s string) (string, string) {
	i := strings.IndexByte(s, '/')
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i+1:]
}

func runLink(cmd *cobra.Command, rawTarget, path string, mountFlags []string, traceLevel runtime.TraceLevel, traceSet bool, dbPath string, dbSet, force bool) error {
	target, tool := splitLinkTarget(rawTarget)
	if target == "" {
		return errors.New("name is required")
	}

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
	entry, err := resolveParticle(ctx, reg, target)
	if err != nil {
		return err
	}

	// Load the manifest only when something needs validating: a
	// bound tool or baked --mount flags. Read off disk rather than
	// boot the wasm — same fail-at-link-time posture as the
	// particle resolve above, without paying for an engine
	// bring-up. (The trace level is just an enum; nothing to check
	// against the manifest.)
	var resolvedMounts []string
	if tool != "" || len(mountFlags) > 0 {
		manifest, err := runtime.LoadManifest(entry.Particle)
		if err != nil {
			return fmt.Errorf("read manifest: %w", err)
		}
		if tool != "" {
			if findManifestTool(manifest.Tools, tool) == nil {
				return fmt.Errorf("tool %q not found in %s@%s (available: %s)", tool, entry.Name, entry.Version, listToolNames(manifest.Tools))
			}
		}
		resolvedMounts, err = resolveLinkMounts(mountFlags, manifest.Capabilities.Filesystem.Mounts)
		if err != nil {
			return err
		}
	}

	bin, err := resolveParticleBinary()
	if err != nil {
		return err
	}

	spec := linkSpec{
		path:          path,
		particleBin:   bin,
		target:        target,
		tool:          tool,
		mounts:        resolvedMounts,
		traceLevel:    traceLevel,
		traceLevelSet: traceSet,
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
	fmt.Fprintf(cmd.OutOrStdout(), "linked %s -> %s\n", finalPath, describeLink(spec))
	return nil
}

// findManifestTool returns the named tool from a manifest's tools
// list, or nil if absent. Tools are matched by exact name.
func findManifestTool(tools []runtime.ManifestTool, name string) *runtime.ManifestTool {
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
		}
	}
	return nil
}

// listToolNames renders a comma-separated list of tool names for the
// "tool not found" error so the user sees what *is* available.
func listToolNames(tools []runtime.ManifestTool) string {
	if len(tools) == 0 {
		return "(none)"
	}
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	return strings.Join(names, ", ")
}

// resolveLinkMounts validates each "--mount name=path" supplied at
// link time and returns them as "name=<abs-host-path>" strings in
// the user's input order, ready to be emitted as --mount entries by
// the shim/trailer. Each mount name must be declared in the
// particle's manifest, and each host path must exist as a
// directory. Duplicate mount names are rejected so the shim doesn't
// silently lose the first value to the second at run time.
func resolveLinkMounts(raw []string, declared map[string]runtime.MountDecl) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, f := range raw {
		i := strings.IndexByte(f, '=')
		if i <= 0 {
			return nil, fmt.Errorf("invalid --mount %q: want name=path", f)
		}
		name, hostPath := f[:i], f[i+1:]
		if hostPath == "" {
			return nil, fmt.Errorf("invalid --mount %q: empty path", f)
		}
		if seen[name] {
			return nil, fmt.Errorf("--mount %s specified more than once", name)
		}
		seen[name] = true
		if _, ok := declared[name]; !ok {
			return nil, fmt.Errorf("--mount %s: not declared in the particle's capabilities.filesystem.mounts (available: %s)", name, listMountNames(declared))
		}
		abs, err := filepath.Abs(hostPath)
		if err != nil {
			return nil, fmt.Errorf("--mount %s: resolve %q: %w", name, hostPath, err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("--mount %s: %w", name, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("--mount %s: %s is not a directory", name, abs)
		}
		out = append(out, name+"="+abs)
	}
	return out, nil
}

// listMountNames is the manifest-mount equivalent of listToolNames —
// used in the "mount not declared" error so the user can see what
// names the manifest actually exposes.
func listMountNames(declared map[string]runtime.MountDecl) string {
	if len(declared) == 0 {
		return "(none)"
	}
	names := make([]string, 0, len(declared))
	for n := range declared {
		names = append(names, n)
	}
	// Stable order for predictable error messages.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j-1] > names[j]; j-- {
			names[j-1], names[j] = names[j], names[j-1]
		}
	}
	return strings.Join(names, ", ")
}

// traceLevelName is the inverse of runtime.ParseTraceLevel for the
// three on-states. Only used when spec.traceLevelSet is true (the
// link command rejects --trace-http=off at parse time), so a zero
// return is unreachable in normal flow but kept defensive for any
// future caller.
func traceLevelName(l runtime.TraceLevel) string {
	switch l {
	case runtime.TraceBasic:
		return "basic"
	case runtime.TraceHeaders:
		return "headers"
	case runtime.TraceFull:
		return "full"
	}
	return ""
}

// describeLink renders the trailing half of the "linked X -> Y"
// success message: a human-readable form of the run line the link
// invokes. Mirrors the shim's own argv order — mounts and trace
// flag before the target so the reader can spot what got baked in.
func describeLink(spec linkSpec) string {
	var b strings.Builder
	b.WriteString("particle run")
	for _, m := range spec.mounts {
		b.WriteString(" --mount ")
		b.WriteString(m)
	}
	if spec.traceLevelSet {
		b.WriteString(" --trace-http=")
		b.WriteString(traceLevelName(spec.traceLevel))
	}
	b.WriteByte(' ')
	b.WriteString(spec.target)
	if spec.tool != "" {
		b.WriteByte(' ')
		b.WriteString(spec.tool)
	}
	return b.String()
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
