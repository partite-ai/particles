// Command particle is the CLI front-end for the particle build /
// runtime libraries.
//
//	particle build [--pack]
//	    Build a particle from the current directory and either
//	    register it in the local state DB (default) or write a
//	    .particle archive to CWD (--pack).
//
//	particle ping <name>[@version]
//	    Call a registered particle's ping handler. Exits 0 with
//	    "ping: not implemented" when the particle didn't define
//	    one, 0 with the result on success, non-zero on handler
//	    error. Version omitted resolves to the highest registered
//	    semver.
//
//	particle run <name>[@version] [tool] [tool-flags]
//	    Call a tool on the registered particle. Without a tool
//	    name, lists tools. Tool flags are derived from the tool's
//	    JSON-Schema input definition; `--help` after the tool
//	    prints them.
//
//	particle serve-mcp <name>[@version]
//	    Run a stdio MCP server backed by the registered particle.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	_ "modernc.org/sqlite"

	"github.com/spf13/cobra"
)

// keyringService / keyringName name the keychain slot that holds
// the secretbox key encrypting credential secrets at rest.
const (
	keyringService = "particle"
	keyringName    = "credentials-key"
)

func main() {
	// Catch SIGINT / SIGTERM and cancel the root context so
	// long-running operations (HTTP fetches, wasm runs, DB
	// queries) unwind cleanly rather than dying mid-flight. A
	// second signal falls back to the Go default — instant kill —
	// so the user can still escape a stuck handler with a
	// repeated Ctrl+C.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root, profileStop := newRootCmd()
	err := root.ExecuteContext(ctx)
	// Flush the pprof profile even when the command errored —
	// cobra's PersistentPostRunE only fires on success, so a
	// half-broken command otherwise leaves an empty .cpu file.
	if fn := profileStop(); fn != nil {
		fn()
	}
	if err != nil {
		// Cobra has already printed the error; just exit non-zero.
		os.Exit(1)
	}
}

// newRootCmd returns the root cobra.Command with every subcommand
// attached. Constructing it as a function (rather than a global)
// keeps tests able to stand up a fresh command tree without state
// from a previous run leaking through cobra's package globals.
//
// The returned `profileStop` closure is called after the command
// returns (success or error). It yields the profile-flush function
// if --profile was set on this invocation, or nil otherwise.
func newRootCmd() (*cobra.Command, func() func()) {
	root := &cobra.Command{
		Use:           "particle",
		Short:         "Build and run particles",
		SilenceUsage:  true, // we already print usage ourselves where it helps
		SilenceErrors: false,
	}

	// --profile is a global, hidden flag that writes CPU + heap
	// pprof profiles with the given prefix for the lifetime of the
	// command. Started in PersistentPreRunE so every subcommand
	// gets profiled by the same code path; stopped by the caller
	// (main) so error paths still flush their data.
	var (
		profile     string
		profileFlush func()
	)
	root.PersistentFlags().StringVar(&profile, "profile", "", "Write CPU + heap pprof profiles with this prefix")
	_ = root.PersistentFlags().MarkHidden("profile")
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		if profile == "" {
			return nil
		}
		stop, err := startProfile(profile, cmd.ErrOrStderr())
		if err != nil {
			return err
		}
		profileFlush = stop
		return nil
	}

	root.AddCommand(
		newBuildCmd(),
		newDeleteCmd(),
		newImportCmd(),
		newListCmd(),
		newPingCmd(),
		newReconfigureCmd(),
		newRunCmd(),
		newServeMCPCmd(),
	)
	return root, func() func() { return profileFlush }
}

// defaultDBPath returns the platform's user config directory with
// a particle/state.db suffix. The actual path is OS-dependent;
// see [os.UserConfigDir].
func defaultDBPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config dir: %w", err)
	}
	return filepath.Join(dir, "particle", "state.db"), nil
}

// dbFlagUsage is the --db flag's `help` string. Resolves the
// default-path placeholder to the actual platform-specific path
// at flag-registration time (e.g.,
// "/home/alice/.config/particle/state.db" on Linux,
// "/Users/alice/Library/Application Support/particle/state.db"
// on macOS). Falls back to a generic <user-config-dir> token
// only if UserConfigDir is unavailable — that's the case on a
// host without HOME, where every CLI command needs --db anyway.
func dbFlagUsage() string {
	def, err := defaultDBPath()
	if err != nil {
		def = "<user-config-dir>/particle/state.db"
	}
	return "Path to the particle state DB (default: " + def + ")"
}

// resolveDBPath returns dbPath if non-empty, else the default. It
// also creates the parent directory and pre-creates the DB file
// with restrictive (0600) permissions, so the SQLite driver opens
// an existing file rather than creating one under the process
// umask (which on typical systems is world-readable). The DB
// holds sealed credential ciphertexts AND cleartext provider
// metadata; the latter is what we're hardening against co-tenant
// reads.
func resolveDBPath(dbPath string) (string, error) {
	if dbPath == "" {
		p, err := defaultDBPath()
		if err != nil {
			return "", err
		}
		dbPath = p
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return "", fmt.Errorf("create state dir: %w", err)
	}
	// Pre-create the file (if absent) with 0600, then unconditionally
	// chmod — handles the case where a prior run left it world-
	// readable. Chmod is best-effort on platforms that don't model
	// POSIX modes (Windows): Chmod returns nil on a no-op there.
	f, err := os.OpenFile(dbPath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("create state db: %w", err)
	}
	_ = f.Close()
	if err := os.Chmod(dbPath, 0o600); err != nil {
		return "", fmt.Errorf("chmod state db: %w", err)
	}
	return dbPath, nil
}
