package main

import (
	"archive/tar"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"testing/fstest"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/spf13/cobra"

	"github.com/partite-ai/particles/credentials"
	credsqlite "github.com/partite-ai/particles/credentials/sqlite"
	"github.com/partite-ai/particles/importer"
	regsqlite "github.com/partite-ai/particles/registry/sqlite"
)

func newImportCmd() *cobra.Command {
	var (
		dbPath       string
		acceptPerms  bool
		confirmPerms bool
	)
	cmd := &cobra.Command{
		Use:   "import <file-or-url>",
		Short: "Import a .particle tarball into the local registry",
		Long: `Reads a .particle tarball (the output of ` + "`particle build --pack`" + `),
expands it in memory, then runs the same import flow as ` + "`particle build`" + ` —
confirming declared permissions, prompting for credentials when the
manifest declares them, and writing to the registry on success.

The source can be a local file path or an http(s):// URL. URLs are
downloaded over plain HTTP — no signature verification — so only
import from sources you trust.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runImport(cmd, args[0], dbPath, permissionModeFromFlags(acceptPerms, confirmPerms))
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "", dbFlagUsage())
	cmd.Flags().BoolVarP(&acceptPerms, "yes", "y", false, "Auto-accept the permission summary (does not skip credential prompts)")
	cmd.Flags().BoolVar(&confirmPerms, "confirm-permissions", false, "Force the permission prompt even when capabilities match the prior version")
	cmd.MarkFlagsMutuallyExclusive("yes", "confirm-permissions")
	return cmd
}

func runImport(cmd *cobra.Command, src, dbPath string, permMode importer.PermissionMode) error {
	particleFS, err := loadParticle(cmd.Context(), src)
	if err != nil {
		return fmt.Errorf("load %s: %w", src, err)
	}

	declared, err := declaredCredentialNames(particleFS)
	if err != nil {
		return err
	}

	dbPath, err = resolveDBPath(dbPath)
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

	// Build a prompter unconditionally — permission
	// confirmation needs one even for particles without
	// credentials. If we DO end up needing to prompt on a
	// non-TTY, the StdioPrompter's Read surfaces an EOF error.
	var prompter importer.Prompter = importer.NewStdioPrompter()

	// Scope the credential backend to this particle's name —
	// pulled off the manifest the loader produced.
	name, _, err := manifestNameVersion(particleFS)
	if err != nil {
		return err
	}
	var credStore credentials.Store
	if len(declared) > 0 {
		sealer, sErr := credsqlite.NewKeyringSealer(keyringService, keyringName)
		if sErr != nil {
			return fmt.Errorf("keyring: %w", sErr)
		}
		backend, bErr := credsqlite.New(ctx, db, sealer)
		if bErr != nil {
			return fmt.Errorf("credentials store: %w", bErr)
		}
		credStore = backend.Scoped(name)
	}

	entry, err := importer.Import(ctx, particleFS, importer.Options{
		Registry:       reg,
		Credentials:    credStore,
		Prompter:       prompter,
		PermissionMode: permMode,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "imported %s@%s\n", entry.Name, entry.Version)
	return nil
}

// loadParticle resolves src to an in-memory FS. It accepts either
// a local file path or an http(s):// URL — URLs are streamed
// through readTar, so the on-the-wire bytes are the same shape as
// what `particle build --pack` writes locally (zstd-compressed
// tar archive).
func loadParticle(ctx context.Context, src string) (fs.FS, error) {
	if u, ok := parseHTTPURL(src); ok {
		return loadParticleFromHTTP(ctx, u.String())
	}
	return readParticleTar(src)
}

// parseHTTPURL returns the parsed URL if s looks like an http or
// https URL, plus a bool flag for the caller. We deliberately
// don't accept file:// — users with a local file already have a
// path-based code path.
func parseHTTPURL(s string) (*url.URL, bool) {
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		return nil, false
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, false
	}
	return u, true
}

// readParticleTar opens the file at path, decompresses the zstd
// stream, walks the tar archive inside, and returns an in-memory
// FS keyed by file path. Mirrors the deterministic zstd-of-tar
// layout `writeParticleTar` produces — but is tolerant of any
// zstd-wrapped tarball with regular files.
func readParticleTar(path string) (fs.FS, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readTar(f)
}

// maxParticleDownloadBytes caps the size of a tarball fetched over
// HTTP — a hostile or wrong URL shouldn't be able to fill memory.
// 100 MiB is well above anything a sensible particle should reach.
// Declared as a var so tests can shrink it.
var maxParticleDownloadBytes int64 = 100 * 1024 * 1024

// loadParticleFromHTTP fetches a tarball over HTTP(S) and parses
// it through the same readTar that handles local files. The
// client has a hard timeout so a stalled remote can't hang the
// CLI indefinitely; the body is wrapped in MaxBytesReader so an
// oversized download fails with a typed error rather than
// silently truncating.
func loadParticleFromHTTP(ctx context.Context, rawURL string) (fs.FS, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s: HTTP %s", rawURL, resp.Status)
	}
	// HTML responses are almost certainly an error page, a
	// captive-portal interstitial, or a misconfigured server —
	// they are NEVER a particle tarball. Catch this up front so
	// the user sees a useful diagnostic instead of a confusing
	// "tar header: unexpected EOF" from readTar.
	if ct := resp.Header.Get("Content-Type"); isHTMLContentType(ct) {
		return nil, fmt.Errorf("GET %s: server returned %s — not a particle tarball (wrong URL, captive portal, or auth wall?)", rawURL, ct)
	}
	body := http.MaxBytesReader(nil, resp.Body, maxParticleDownloadBytes)
	fsys, err := readTar(body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, fmt.Errorf("particle exceeds %d bytes", maxParticleDownloadBytes)
		}
		return nil, err
	}
	return fsys, nil
}

// isHTMLContentType reports whether ct names an HTML payload.
// Handles the parameterized form (`text/html; charset=utf-8`) by
// trimming at the first `;`.
func isHTMLContentType(ct string) bool {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(strings.ToLower(ct))
	return ct == "text/html" || ct == "application/xhtml+xml"
}

func readTar(r io.Reader) (fs.FS, error) {
	zr, err := zstd.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("zstd reader: %w", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	out := fstest.MapFS{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar header: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			// Skip directories, symlinks, etc. — particle
			// tarballs only carry regular files; anything
			// else is suspect.
			continue
		}
		if err := validateTarName(hdr.Name); err != nil {
			return nil, fmt.Errorf("tar entry %q: %w", hdr.Name, err)
		}
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, tr); err != nil {
			return nil, fmt.Errorf("tar body for %s: %w", hdr.Name, err)
		}
		out[hdr.Name] = &fstest.MapFile{Data: buf.Bytes()}
	}
	return out, nil
}

// validateTarName rejects entry names that don't normalize to a
// clean, relative, in-tree path. Particle tarballs only carry
// regular files at well-known paths; an absolute name (`/etc/...`)
// or a traversal segment (`../`) is either a packing bug or a
// hostile probe. Today's downstream consumers read by exact path
// so a malicious name wouldn't escape, but rejecting at the
// parser keeps any future "extract to disk" code path safe by
// default.
func validateTarName(name string) error {
	if name == "" {
		return errors.New("empty name")
	}
	if path.IsAbs(name) || strings.HasPrefix(name, "/") {
		return fmt.Errorf("absolute path not allowed")
	}
	if cleaned := path.Clean(name); cleaned != name {
		return fmt.Errorf("non-canonical path (cleans to %q)", cleaned)
	}
	// path.Clean leaves a leading "../" intact; check explicitly.
	if name == ".." || strings.HasPrefix(name, "../") {
		return fmt.Errorf("traversal outside archive root")
	}
	return nil
}
