// Command particle is the CLI front-end for the particle build /
// runtime libraries. Phase 1 carries a single subcommand:
//
//	particle build
//	    Build a particle from the current directory. CWD is used as
//	    the source FS, Particlefile.{ts,js} as the entry point. On
//	    success writes <name>-<version>.particle (a tarball) to CWD
//	    and prints its path. On failure prints diagnostics to stderr
//	    and exits non-zero.
//
// More commands (`run`, `test`, `setup`, `mcp`, `ping`, …) land
// alongside this one as we wire each runtime piece in.
package main

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/partite-ai/particle/internal/build"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "build":
		os.Exit(cmdBuild(os.Args[2:]))
	case "-h", "--help", "help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "particle: unknown command %q\n\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}
}

func usage(w *os.File) {
	fmt.Fprintln(w, `particle — build and run particles

Usage:
    particle <command> [arguments]

Commands:
    build    Build a particle from the current directory.
    help     Show this help.`)
}

// cmdBuild is the entry point for `particle build`. Returns the
// process exit code so main can wire it through os.Exit.
func cmdBuild(args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "particle build: takes no arguments")
		return 2
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "particle build: getwd: %v\n", err)
		return 1
	}

	res, err := build.Build(context.Background(), build.Options{
		Source: os.DirFS(cwd),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		printLogs(os.Stderr, errLogs(err))
		return 1
	}

	for _, w := range res.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w.Error())
	}

	name, version, err := manifestNameVersion(res.Particle)
	if err != nil {
		fmt.Fprintf(os.Stderr, "particle build: %v\n", err)
		return 1
	}

	outPath := fmt.Sprintf("%s-%s.particle", name, version)
	if err := writeParticleTar(res.Particle, outPath); err != nil {
		fmt.Fprintf(os.Stderr, "particle build: write %s: %v\n", outPath, err)
		return 1
	}

	fmt.Println(outPath)
	return 0
}

// manifestNameVersion reads the manifest.json from the result FS and
// extracts the kebab-case name + semver version that determine the
// output filename.
func manifestNameVersion(fsys fs.FS) (string, string, error) {
	data, err := fs.ReadFile(fsys, "manifest.json")
	if err != nil {
		return "", "", fmt.Errorf("read manifest.json: %w", err)
	}
	var m struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return "", "", fmt.Errorf("parse manifest.json: %w", err)
	}
	if m.Name == "" || m.Version == "" {
		return "", "", fmt.Errorf("manifest.json missing name or version")
	}
	return m.Name, m.Version, nil
}

// writeParticleTar packs every file in fsys into a deterministic USTAR
// tarball at outPath. "Deterministic" matters for content-addressing:
// two builds of the same source should produce byte-identical output.
// We rely on fs.WalkDir's lexical traversal order plus a zero ModTime.
func writeParticleTar(fsys fs.FS, outPath string) error {
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()

	tw := tar.NewWriter(f)
	defer tw.Close()

	return fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		hdr := &tar.Header{
			Name:    path,
			Mode:    0o644,
			Size:    int64(len(data)),
			ModTime: time.Time{},
			Format:  tar.FormatUSTAR,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("write header for %s: %w", path, err)
		}
		if _, err := tw.Write(data); err != nil {
			return fmt.Errorf("write body for %s: %w", path, err)
		}
		return nil
	})
}

// errLogs extracts captured wasm logs from a *build.Error, if the
// error is one. Returns nil for any other error type.
func errLogs(err error) []build.Log {
	type unwrappable interface {
		Unwrap() error
	}
	for cur := err; cur != nil; {
		if be, ok := cur.(*build.Error); ok {
			return be.Logs
		}
		if u, ok := cur.(unwrappable); ok {
			cur = u.Unwrap()
		} else {
			break
		}
	}
	return nil
}

// printLogs writes captured per-phase wasm output to w, separated by a
// header so a reader can tell which phase produced which bytes. Empty
// logs are suppressed (no header printed) to keep clean failures
// terse.
func printLogs(w *os.File, logs []build.Log) {
	for _, l := range logs {
		if len(l.Bytes) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n--- %s ---\n", l.Phase)
		_, _ = w.Write(l.Bytes)
		if len(l.Bytes) > 0 && l.Bytes[len(l.Bytes)-1] != '\n' {
			fmt.Fprintln(w)
		}
	}
}
