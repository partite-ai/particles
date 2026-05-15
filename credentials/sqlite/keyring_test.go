package sqlite_test

import (
	"bytes"
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
	"github.com/zalando/go-keyring"

	"github.com/partite-ai/particle/credentials"
	"github.com/partite-ai/particle/credentials/sqlite"
)

// First call generates and stores a key; second call for the same
// (service, name) loads the existing one. Two sealers built from
// the same keyring slot must agree on what they encrypt — that's
// what lets persisted ciphertexts survive process restarts.
func TestKeyringSealer_GeneratesOnFirstUseAndLoadsAfter(t *testing.T) {
	a, err := sqlite.NewKeyringSealer("particle-test", t.Name())
	if err != nil {
		t.Fatalf("first NewKeyringSealer: %v", err)
	}
	b, err := sqlite.NewKeyringSealer("particle-test", t.Name())
	if err != nil {
		t.Fatalf("second NewKeyringSealer: %v", err)
	}

	plain := []byte("the password")
	ct, err := a.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.Open(ct)
	if err != nil {
		t.Fatalf("second sealer can't decrypt first sealer's ciphertext: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("decrypted = %q, want %q", got, plain)
	}
}

// Each Seal call uses a fresh random nonce, so two encryptions of
// the same plaintext yield distinct ciphertexts. Pinning this
// property keeps a future change from accidentally breaking
// nonce-uniqueness (e.g., switching to a counter and forgetting to
// persist it).
func TestKeyringSealer_NonceFreshness(t *testing.T) {
	s, err := sqlite.NewKeyringSealer("particle-test", t.Name())
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.Seal([]byte("same"))
	b, _ := s.Seal([]byte("same"))
	if bytes.Equal(a, b) {
		t.Error("two seals of the same plaintext produced identical ciphertexts; nonce should be random per message")
	}
}

// Truncated ciphertext — anything shorter than nonce + auth tag —
// is rejected with an error, not a panic or silent zero-length
// plaintext.
func TestKeyringSealer_TruncatedCiphertextRejected(t *testing.T) {
	s, err := sqlite.NewKeyringSealer("particle-test", t.Name())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open([]byte("too-short")); err == nil {
		t.Error("Open of short input should error")
	}
}

// Tampered ciphertext fails authentication. NaCl secretbox
// guarantees this; the test pins the contract so a switch to a
// non-AEAD primitive would fail loudly.
func TestKeyringSealer_TamperedCiphertextRejected(t *testing.T) {
	s, err := sqlite.NewKeyringSealer("particle-test", t.Name())
	if err != nil {
		t.Fatal(err)
	}
	ct, err := s.Seal([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte in the encrypted region (after the nonce).
	ct[len(ct)-1] ^= 0xff
	if _, err := s.Open(ct); err == nil {
		t.Error("Open of tampered ciphertext should error")
	}
}

// Two sealers built from different keyring slots cannot decrypt
// each other's ciphertexts — i.e., the (service, name) pair
// actually scopes the key.
func TestKeyringSealer_DifferentSlotsCantDecryptEachOther(t *testing.T) {
	a, err := sqlite.NewKeyringSealer("particle-test", t.Name()+"-A")
	if err != nil {
		t.Fatal(err)
	}
	b, err := sqlite.NewKeyringSealer("particle-test", t.Name()+"-B")
	if err != nil {
		t.Fatal(err)
	}
	ct, _ := a.Seal([]byte("secret"))
	if _, err := b.Open(ct); err == nil {
		t.Error("sealer with a different keyring slot decrypted another's ciphertext")
	}
}

// The on-disk value stored in particle_credential_secrets must NOT
// equal the plaintext — that's the entire point. Verify by
// peeking at the raw column.
func TestStore_SecretsAreEncryptedAtRest(t *testing.T) {
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	sealer, err := sqlite.NewKeyringSealer("particle-test", t.Name())
	if err != nil {
		t.Fatal(err)
	}
	s, err := sqlite.New(ctx, db, sealer)
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

	// Round-trip: Store decrypts on read.
	got, err := s.ReadSecret(ctx, "yaml-tools", desc.ID, credentials.SecretRoleAccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("ReadSecret = %q, want %q", got, plain)
	}

	// And on disk, the row's bytes are NOT the plaintext.
	var raw []byte
	if err := db.QueryRowContext(ctx,
		`SELECT value FROM particle_credential_secrets WHERE particle = ? AND entry_id = ? AND role = ?`,
		"yaml-tools", desc.ID, string(credentials.SecretRoleAccessToken)).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(raw, plain) {
		t.Error("raw on-disk value equals plaintext; encryption isn't being applied")
	}
	if bytes.Contains(raw, plain) {
		t.Error("plaintext appears as a substring of the on-disk value")
	}
}

// Reads against a Store whose Sealer can't decrypt the on-disk
// data (e.g., the keyring key was rotated, or the wrong slot is
// configured) must surface an error rather than return garbage.
func TestStore_WrongSealerSurfacesDecryptError(t *testing.T) {
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	sealerA, err := sqlite.NewKeyringSealer("particle-test", t.Name()+"-A")
	if err != nil {
		t.Fatal(err)
	}
	s, err := sqlite.New(ctx, db, sealerA)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := s.Put(ctx, "p", "gh", "gh", credentials.OAuth2Meta{},
		credentials.Secret{Role: credentials.SecretRoleAccessToken, Value: []byte("token")},
	)
	if err != nil {
		t.Fatal(err)
	}

	// Open the same DB with a different sealer slot — its key is
	// independent, so it can't authenticate the existing row.
	sealerB, err := sqlite.NewKeyringSealer("particle-test", t.Name()+"-B")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := sqlite.New(ctx, db, sealerB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.ReadSecret(ctx, "p", desc.ID, credentials.SecretRoleAccessToken); err == nil {
		t.Error("ReadSecret with wrong sealer should error")
	}
}

// Empty service/name aren't a valid keyring slot — guard against
// the silent "store everything in the same slot" footgun.
func TestNewKeyringSealer_RejectsEmptyArgs(t *testing.T) {
	if _, err := sqlite.NewKeyringSealer("", "x"); err == nil {
		t.Error("empty service should error")
	}
	if _, err := sqlite.NewKeyringSealer("x", ""); err == nil {
		t.Error("empty name should error")
	}
}

// If the keyring slot's stored value isn't a valid 32-byte
// base64-encoded key (e.g., corrupted or written by a different
// program), construction must error rather than silently using a
// truncated/garbage key.
func TestNewKeyringSealer_RejectsCorruptStoredKey(t *testing.T) {
	if err := keyring.Set("particle-test", t.Name(), "not-base64!!!"); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewKeyringSealer("particle-test", t.Name()); err == nil {
		t.Error("expected error for non-base64 stored key")
	}
}
