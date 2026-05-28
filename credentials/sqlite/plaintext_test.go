package sqlite_test

import (
	"bytes"
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/partite-ai/particles/credentials"
	"github.com/partite-ai/particles/credentials/sqlite"
)

// PlaintextSealer round-trips: the sealed blob is not the bare
// plaintext (it carries the marker), but Open recovers it exactly.
func TestPlaintextSealer_RoundTrip(t *testing.T) {
	var s sqlite.PlaintextSealer
	plain := []byte("token-value")
	ct, err := s.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ct, plain) {
		t.Error("sealed blob equals plaintext; expected a marker prefix")
	}
	got, err := s.Open(ct)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("Open = %q, want %q", got, plain)
	}
}

// Open of an unmarked blob (e.g. a KeyringSealer ciphertext) errors
// rather than returning the bytes as if they were the secret.
func TestPlaintextSealer_OpenRejectsUnmarked(t *testing.T) {
	var s sqlite.PlaintextSealer
	if _, err := s.Open([]byte("arbitrary unmarked bytes long enough to look real")); err == nil {
		t.Error("Open of unmarked blob should error")
	}
}

// The two sealers must never silently decode each other's output:
// PlaintextSealer.Open rejects keyring ciphertext (no marker), and
// KeyringSealer.Open rejects a plaintext blob (fails secretbox auth).
// This is the guard for a DB written when the keyring was available
// and later read when it isn't (or vice versa) — a loud error beats a
// garbage token.
func TestSealers_DoNotCrossDecode(t *testing.T) {
	ks, err := sqlite.NewKeyringSealer("particle-test", t.Name())
	if err != nil {
		t.Fatal(err)
	}
	var ps sqlite.PlaintextSealer
	plain := []byte("secret")

	ksCT, err := ks.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ps.Open(ksCT); err == nil {
		t.Error("PlaintextSealer.Open accepted a KeyringSealer ciphertext")
	}

	psCT, err := ps.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ks.Open(psCT); err == nil {
		t.Error("KeyringSealer.Open accepted a PlaintextSealer blob")
	}
}

// End-to-end: a Store built with PlaintextSealer (the keyring-
// unavailable fallback) round-trips secrets, but the on-disk bytes
// contain the plaintext — confirming the fallback genuinely stores
// unencrypted, as the warning advertises.
func TestStore_PlaintextSealerStoresUnencrypted(t *testing.T) {
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	s, err := sqlite.New(ctx, db, sqlite.PlaintextSealer{})
	if err != nil {
		t.Fatal(err)
	}

	plain := []byte("super-secret-token-value")
	desc, err := s.Put(ctx, "yaml-tools", "gh", "gh", credentials.OAuth2Meta{},
		credentials.Secret{Role: credentials.SecretRoleAccessToken, Value: plain},
	)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.ReadSecret(ctx, "yaml-tools", desc.ID, credentials.SecretRoleAccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("ReadSecret = %q, want %q", got, plain)
	}

	var raw []byte
	if err := db.QueryRowContext(ctx,
		`SELECT value FROM particle_credential_secrets WHERE particle = ? AND entry_id = ? AND role = ?`,
		"yaml-tools", desc.ID, string(credentials.SecretRoleAccessToken)).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, plain) {
		t.Error("on-disk value does not contain the plaintext; PlaintextSealer should store unencrypted")
	}
}
