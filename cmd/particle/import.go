package main

import (
	"archive/zip"
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
	"time"

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
		Short: "Import a .particle archive into the local registry",
		Long: `Reads a .particle archive (the output of ` + "`particle build --pack`" + `),
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
// a local file path or an http(s):// URL.
func loadParticle(ctx context.Context, src string) (fs.FS, error) {
	if u, ok := parseHTTPURL(src); ok {
		return loadParticleFromHTTP(ctx, u.String())
	}
	return readParticleZipFile(src)
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

// readParticleZipFile reads the zip archive at path into memory
// and returns it as an fs.FS. Buffering the bytes (rather than
// keeping a file handle open) means the caller doesn't have to
// reason about archive lifetime.
func readParticleZipFile(path string) (fs.FS, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("zip reader: %w", err)
	}
	return readZip(zr)
}

// maxParticleDownloadBytes caps the size of an archive fetched
// over HTTP — a hostile or wrong URL shouldn't be able to fill
// memory. 100 MiB is well above anything a sensible particle
// should reach. Declared as a var so tests can shrink it.
var maxParticleDownloadBytes int64 = 100 * 1024 * 1024

// loadParticleFromHTTP fetches an archive over HTTP(S) and parses
// it through the same zip reader that handles local files. The
// client has a hard timeout so a stalled remote can't hang the
// CLI indefinitely; the body is wrapped in MaxBytesReader so an
// oversized download fails with a typed error rather than
// silently truncating. The zip central directory lives at the end
// of the file, so the entire body is buffered before parsing.
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
	// captive-portal interstitial, or a misconfigured server.
	// Catch this up front so the user sees a useful diagnostic
	// instead of a confusing zip-decode failure.
	if ct := resp.Header.Get("Content-Type"); isHTMLContentType(ct) {
		return nil, fmt.Errorf("GET %s: server returned %s — not a particle archive (wrong URL, captive portal, or auth wall?)", rawURL, ct)
	}
	body := http.MaxBytesReader(nil, resp.Body, maxParticleDownloadBytes)
	buf, err := io.ReadAll(body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, fmt.Errorf("particle exceeds %d bytes", maxParticleDownloadBytes)
		}
		return nil, err
	}
	zr, err := zip.NewReader(bytes.NewReader(buf), int64(len(buf)))
	if err != nil {
		return nil, fmt.Errorf("zip reader: %w", err)
	}
	return readZip(zr)
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

// readZip validates every entry name in the archive and returns
// it as an fs.FS. *zip.Reader already satisfies fs.FS and
// fs.ReadDirFS so no copy into a secondary FS is needed; the
// up-front name check ensures any hostile path fails before a
// downstream reader can act on it.
func readZip(zr *zip.Reader) (fs.FS, error) {
	for _, entry := range zr.File {
		if entry.FileInfo().IsDir() {
			continue
		}
		if err := validateEntryName(entry.Name); err != nil {
			return nil, fmt.Errorf("zip entry %q: %w", entry.Name, err)
		}
	}
	return zr, nil
}

// validateEntryName rejects entry names that don't normalize to a
// clean, relative, in-tree path. Particle archives only carry
// regular files at well-known paths; an absolute name (/etc/...)
// or a traversal segment (../) is either a packing bug or a
// hostile probe. Today's downstream consumers read by exact path
// so a malicious name wouldn't escape, but rejecting at the
// parser keeps any future "extract to disk" code path safe by
// default.
func validateEntryName(name string) error {
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
