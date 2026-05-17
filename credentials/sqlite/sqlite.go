// Package sqlite is a SQLite-backed [credentials.Store] implementation.
//
// It uses database/sql with whatever driver the caller registered.
// modernc.org/sqlite is the natural pure-Go choice; mattn/go-sqlite3
// works too. The Store is given an already-open *sql.DB so the caller
// owns connection-pool tuning, DSN choices, and lifetime.
//
// Schema management runs on construction: [New] creates two tables
// the package owns — `particle_credentials` and
// `particle_credential_secrets` — if they don't already exist. The
// caller may share the database with their own tables; nothing else
// in this package touches names outside the `particle_credential*`
// prefix.
//
// Secret values are encrypted at rest via a [Sealer]. The default
// implementation, [KeyringSealer], wraps NaCl secretbox with a key
// stored in the OS keychain (zalando/go-keyring) — generated on
// first use so the host doesn't need any out-of-band setup. Hosts
// that prefer a KMS, HSM, or other key custody can plug in their
// own Sealer. Metadata is stored in cleartext: it's secret-free by
// design (URLs, scopes, usernames — see [credentials.Metadata]).
//
// All Put / Delete / WriteSecrets paths run inside a transaction so
// readers see all-or-nothing updates — preserving the atomicity
// promise [credentials.Store.Put] makes (metadata + N secrets in
// lockstep), required for OAuth refresh.
//
// All state is persistent — restarting the host preserves every
// credential. For the in-process equivalent, use credentials/memory.
package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"

	"github.com/partite-ai/particles/credentials"
)

// Store is a SQLite-backed credentials.Store.
//
// Safe for concurrent use — the underlying *sql.DB serializes
// access through its connection pool, and every multi-statement
// operation is wrapped in a transaction.
type Store struct {
	db          *sql.DB
	sealer      Sealer
	idGenerator func() string // overridable for tests; defaults to newID
}

// New constructs a Store against an already-open *sql.DB and
// applies the schema. `sealer` is the encryption layer applied to
// secret blobs; pass [NewKeyringSealer]'s result for the default
// "key in OS keychain, secretbox at rest" behavior.
//
// A nil sealer constructs a metadata-only Store: Put /
// WriteSecrets / ReadSecret all error with a clear message, but
// the no-crypto operations (List, GetByID, GetByName,
// ConfiguredMethod, Delete, DeleteSecret) work. This is how
// `particle list` avoids surfacing a keychain prompt when all it
// needs is the configured-method name per particle.
//
// The caller retains ownership of the DB — closing the Store does
// NOT close the DB; the caller decides when to.
func New(ctx context.Context, db *sql.DB, sealer Sealer) (*Store, error) {
	if db == nil {
		return nil, errors.New("credentials/sqlite: db is required")
	}
	s := &Store{db: db, sealer: sealer, idGenerator: newID}
	if err := s.migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// errNoSealer is the canonical error a metadata-only Store
// returns when a secret operation is attempted. Constants instead
// of fresh allocations so errors.Is comparisons work for callers
// that want to detect "this store doesn't have a sealer."
var errNoSealer = errors.New("credentials/sqlite: secret operation requires a Sealer (Store was constructed without one)")

var _ credentials.Store = (*Store)(nil)

// schema: one row per (particle, name) — `name` is the credential's
// user-facing name (e.g., "github"); `method` is the configured
// method name (e.g., "pat"). Multiple credentials per particle
// coexist under different `name` values.
//
// Pre-1.0 breaking change: the prior schema lacked the `method`
// column. Existing state DBs need to be deleted before use.
const schema = `
CREATE TABLE IF NOT EXISTS particle_credentials (
  particle  TEXT NOT NULL,
  id        TEXT NOT NULL,
  name      TEXT NOT NULL,
  method    TEXT NOT NULL,
  kind      TEXT NOT NULL,
  meta_json TEXT NOT NULL,
  PRIMARY KEY (particle, id)
);
CREATE UNIQUE INDEX IF NOT EXISTS particle_credentials_by_name
  ON particle_credentials (particle, name);

CREATE TABLE IF NOT EXISTS particle_credential_secrets (
  particle TEXT NOT NULL,
  entry_id TEXT NOT NULL,
  role     TEXT NOT NULL,
  value    BLOB NOT NULL,
  PRIMARY KEY (particle, entry_id, role)
);
`

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("credentials/sqlite: migrate: %w", err)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Metadata operations
// -----------------------------------------------------------------------------

func (s *Store) GetByID(ctx context.Context, particle, id string) (credentials.Descriptor, error) {
	return s.getOne(ctx,
		`SELECT id, name, method, kind, meta_json FROM particle_credentials WHERE particle = ? AND id = ?`,
		particle, id)
}

func (s *Store) GetByName(ctx context.Context, particle, name string) (credentials.Descriptor, error) {
	return s.getOne(ctx,
		`SELECT id, name, method, kind, meta_json FROM particle_credentials WHERE particle = ? AND name = ?`,
		particle, name)
}

func (s *Store) getOne(ctx context.Context, query string, args ...any) (credentials.Descriptor, error) {
	row := s.db.QueryRowContext(ctx, query, args...)
	var id, name, method, kind string
	var metaJSON []byte
	if err := row.Scan(&id, &name, &method, &kind, &metaJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return credentials.Descriptor{}, credentials.ErrNotFound
		}
		return credentials.Descriptor{}, fmt.Errorf("credentials/sqlite: scan: %w", err)
	}
	meta, err := unmarshalMeta(kind, metaJSON)
	if err != nil {
		return credentials.Descriptor{}, fmt.Errorf("credentials/sqlite: %w", err)
	}
	return credentials.Descriptor{ID: id, Name: name, Method: method, Meta: meta}, nil
}

func (s *Store) List(ctx context.Context, particle string) ([]credentials.ListEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, method, kind FROM particle_credentials WHERE particle = ? ORDER BY name`,
		particle)
	if err != nil {
		return nil, fmt.Errorf("credentials/sqlite: List: %w", err)
	}
	defer rows.Close()

	var out []credentials.ListEntry
	for rows.Next() {
		var id, name, method, kind string
		if err := rows.Scan(&id, &name, &method, &kind); err != nil {
			return nil, fmt.Errorf("credentials/sqlite: List scan: %w", err)
		}
		out = append(out, credentials.ListEntry{ID: id, Name: name, Method: method, Kind: credentials.Kind(kind)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("credentials/sqlite: List rows: %w", err)
	}
	return out, nil
}

// Put configures the (particle, name) credential — see the
// [credentials.Store] interface for the full contract. Atomic via a
// single transaction.
func (s *Store) Put(ctx context.Context, particle, name, method string, meta credentials.Metadata, secrets ...credentials.Secret) (credentials.Descriptor, error) {
	if s.sealer == nil {
		return credentials.Descriptor{}, errNoSealer
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return credentials.Descriptor{}, fmt.Errorf("credentials/sqlite: Put: begin: %w", err)
	}
	defer tx.Rollback()

	desc, err := s.putInTx(ctx, tx, particle, name, method, meta, secrets)
	if err != nil {
		return credentials.Descriptor{}, err
	}
	if err := tx.Commit(); err != nil {
		return credentials.Descriptor{}, fmt.Errorf("credentials/sqlite: Put: commit: %w", err)
	}
	return desc, nil
}

// ConfiguredMethod returns the method name stored for
// (particle, name), or "" when no credential is configured under
// that name.
func (s *Store) ConfiguredMethod(ctx context.Context, particle, name string) (string, error) {
	var method string
	err := s.db.QueryRowContext(ctx,
		`SELECT method FROM particle_credentials WHERE particle = ? AND name = ?`,
		particle, name).Scan(&method)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("credentials/sqlite: ConfiguredMethod: %w", err)
	}
	return method, nil
}

// putInTx is the validation + insert-or-update body of Put. Runs
// inside the caller's transaction. Same-(name, method) re-Put
// preserves the existing ID and unmentioned secrets; switching
// method wipes every prior secret for the row before writing new
// ones.
func (s *Store) putInTx(ctx context.Context, tx *sql.Tx, particle, name, method string, meta credentials.Metadata, secrets []credentials.Secret) (credentials.Descriptor, error) {
	if meta == nil {
		return credentials.Descriptor{}, fmt.Errorf("credentials/sqlite: requires a non-nil Metadata")
	}
	if name == "" {
		return credentials.Descriptor{}, fmt.Errorf("credentials/sqlite: requires a non-empty name")
	}
	if method == "" {
		return credentials.Descriptor{}, fmt.Errorf("credentials/sqlite: requires a non-empty method")
	}
	for i, sec := range secrets {
		if sec.Role == "" {
			return credentials.Descriptor{}, fmt.Errorf("credentials/sqlite: secrets[%d] has empty Role", i)
		}
	}

	kind, metaJSON, err := marshalMeta(meta)
	if err != nil {
		return credentials.Descriptor{}, fmt.Errorf("credentials/sqlite: %w", err)
	}

	// Look up existing entry by (particle, name) to decide
	// create-vs-update and to detect a method switch.
	var (
		id        string
		oldMethod string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT id, method FROM particle_credentials WHERE particle = ? AND name = ?`,
		particle, name).Scan(&id, &oldMethod)
	switch {
	case err == nil:
		if oldMethod != method {
			// Method changed — wipe the row's existing
			// secrets before writing the new ones so a
			// "pat" → "oauth" switch doesn't leave the old
			// api-key bytes lying around.
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM particle_credential_secrets WHERE particle = ? AND entry_id = ?`,
				particle, id); err != nil {
				return credentials.Descriptor{}, fmt.Errorf("credentials/sqlite: wipe secrets on method switch: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE particle_credentials SET method = ?, kind = ?, meta_json = ? WHERE particle = ? AND id = ?`,
			method, kind, metaJSON, particle, id); err != nil {
			return credentials.Descriptor{}, fmt.Errorf("credentials/sqlite: update: %w", err)
		}
	case errors.Is(err, sql.ErrNoRows):
		id = s.idGenerator()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO particle_credentials (particle, id, name, method, kind, meta_json) VALUES (?, ?, ?, ?, ?, ?)`,
			particle, id, name, method, kind, metaJSON); err != nil {
			return credentials.Descriptor{}, fmt.Errorf("credentials/sqlite: insert: %w", err)
		}
	default:
		return credentials.Descriptor{}, fmt.Errorf("credentials/sqlite: lookup: %w", err)
	}

	for _, sec := range secrets {
		sealed, err := s.sealer.Seal(sec.Value)
		if err != nil {
			return credentials.Descriptor{}, fmt.Errorf("credentials/sqlite: seal %s: %w", sec.Role, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO particle_credential_secrets (particle, entry_id, role, value)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT(particle, entry_id, role) DO UPDATE SET value = excluded.value`,
			particle, id, string(sec.Role), sealed); err != nil {
			return credentials.Descriptor{}, fmt.Errorf("credentials/sqlite: write secret %s: %w", sec.Role, err)
		}
	}
	return credentials.Descriptor{ID: id, Name: name, Method: method, Meta: meta}, nil
}

// Delete removes the entire entry — metadata and every secret.
// Idempotent.
func (s *Store) Delete(ctx context.Context, particle, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("credentials/sqlite: Delete: begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM particle_credential_secrets WHERE particle = ? AND entry_id = ?`,
		particle, id); err != nil {
		return fmt.Errorf("credentials/sqlite: Delete: secrets: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM particle_credentials WHERE particle = ? AND id = ?`,
		particle, id); err != nil {
		return fmt.Errorf("credentials/sqlite: Delete: entry: %w", err)
	}
	return tx.Commit()
}

// -----------------------------------------------------------------------------
// Secret operations
// -----------------------------------------------------------------------------

func (s *Store) ReadSecret(ctx context.Context, particle, id string, role credentials.SecretRole) ([]byte, error) {
	if s.sealer == nil {
		return nil, errNoSealer
	}
	var sealed []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM particle_credential_secrets WHERE particle = ? AND entry_id = ? AND role = ?`,
		particle, id, string(role)).Scan(&sealed)
	if err == nil {
		plain, err := s.sealer.Open(sealed)
		if err != nil {
			return nil, fmt.Errorf("credentials/sqlite: ReadSecret %s: %w", role, err)
		}
		return plain, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("credentials/sqlite: ReadSecret: %w", err)
	}
	// Row is missing — disambiguate "no entry" vs "no secret on entry".
	var exists int
	err = s.db.QueryRowContext(ctx,
		`SELECT 1 FROM particle_credentials WHERE particle = ? AND id = ?`,
		particle, id).Scan(&exists)
	if err == nil {
		return nil, credentials.ErrSecretNotSet
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, credentials.ErrNotFound
	}
	return nil, fmt.Errorf("credentials/sqlite: ReadSecret: %w", err)
}

// WriteSecrets atomically writes the given secrets, returning
// ErrNotFound if the entry doesn't exist.
func (s *Store) WriteSecrets(ctx context.Context, particle, id string, secrets ...credentials.Secret) error {
	if s.sealer == nil {
		return errNoSealer
	}
	for i, sec := range secrets {
		if sec.Role == "" {
			return fmt.Errorf("credentials/sqlite: WriteSecrets: secrets[%d] has empty Role", i)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("credentials/sqlite: WriteSecrets: begin: %w", err)
	}
	defer tx.Rollback()

	var exists int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM particle_credentials WHERE particle = ? AND id = ?`,
		particle, id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return credentials.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("credentials/sqlite: WriteSecrets: lookup: %w", err)
	}

	for _, sec := range secrets {
		sealed, err := s.sealer.Seal(sec.Value)
		if err != nil {
			return fmt.Errorf("credentials/sqlite: WriteSecrets: seal %s: %w", sec.Role, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO particle_credential_secrets (particle, entry_id, role, value)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT(particle, entry_id, role) DO UPDATE SET value = excluded.value`,
			particle, id, string(sec.Role), sealed); err != nil {
			return fmt.Errorf("credentials/sqlite: WriteSecrets: %s: %w", sec.Role, err)
		}
	}
	return tx.Commit()
}

// DeleteSecret removes a secret. Idempotent — silent on missing
// entry or already-absent role.
func (s *Store) DeleteSecret(ctx context.Context, particle, id string, role credentials.SecretRole) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM particle_credential_secrets WHERE particle = ? AND entry_id = ? AND role = ?`,
		particle, id, string(role)); err != nil {
		return fmt.Errorf("credentials/sqlite: DeleteSecret: %w", err)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// newID returns a base32-encoded 128-bit random ID. Output is
// 26 lowercase ASCII characters from [a-z2-7] — no whitespace, no
// punctuation, safe to use in URLs, log messages, and file paths.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("credentials/sqlite: crypto/rand failed: " + err.Error())
	}
	return strings.ToLower(
		base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]),
	)
}
