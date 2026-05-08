package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
	"github.com/zalando/go-keyring"

	"github.com/partite-ai/particle/credentials"
	"github.com/partite-ai/particle/credentials/sqlite"
)

// TestMain swaps zalando/go-keyring's backend for an in-process
// mock for the duration of the test binary. Tests don't touch the
// host's real keychain, and CI environments without a keychain
// daemon work fine.
func TestMain(m *testing.M) {
	keyring.MockInit()
	m.Run()
}

// newSealer returns a fresh KeyringSealer backed by a unique
// (service, name) per test, so test-to-test interference can't
// happen even when the mock keyring is shared.
func newSealer(t *testing.T) sqlite.Sealer {
	t.Helper()
	sealer, err := sqlite.NewKeyringSealer("particle-test", t.Name())
	if err != nil {
		t.Fatalf("NewKeyringSealer: %v", err)
	}
	return sealer
}

// IDs must be ASCII with no whitespace or punctuation. The store
// uses the same 26-char lowercase base32 scheme as credentials/memory.
var idShape = regexp.MustCompile(`^[a-z2-7]{26}$`)

// newStore opens an in-memory SQLite (one DB per test, fully
// isolated) and returns a fresh Store. Cache=shared lets the single
// in-memory database remain reachable across the connection pool's
// distinct connections.
func newStore(t *testing.T) *sqlite.Store {
	t.Helper()
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	s, err := sqlite.New(context.Background(), db, newSealer(t))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	return s
}

// -----------------------------------------------------------------------------
// Metadata operations
// -----------------------------------------------------------------------------

func TestPut_GeneratesID(t *testing.T) {
	s := newStore(t)
	desc, err := s.Put(context.Background(), "yaml-tools", "main", credentials.BasicMeta{Username: "u"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !idShape.MatchString(desc.ID) {
		t.Errorf("ID %q doesn't match expected base32 shape %s", desc.ID, idShape)
	}
	if desc.Name != "main" {
		t.Errorf("Name = %q", desc.Name)
	}
	if desc.Meta.Kind() != credentials.KindBasic {
		t.Errorf("Kind = %q, want %q", desc.Meta.Kind(), credentials.KindBasic)
	}
}

func TestPut_PreservesIDAndSecretsOnNameOverwrite(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	first, _ := s.Put(ctx, "yaml-tools", "gh", credentials.OAuth2Meta{ClientID: "old"})
	if err := s.WriteSecrets(ctx, "yaml-tools", first.ID,
		credentials.Secret{Role: credentials.SecretRoleAccessToken, Value: []byte("at")},
		credentials.Secret{Role: credentials.SecretRoleRefreshToken, Value: []byte("rt")},
	); err != nil {
		t.Fatalf("WriteSecrets: %v", err)
	}

	second, _ := s.Put(ctx, "yaml-tools", "gh", credentials.OAuth2Meta{ClientID: "new"})
	if first.ID != second.ID {
		t.Errorf("ID changed on metadata-only update: %q → %q", first.ID, second.ID)
	}
	got := second.Meta.(credentials.OAuth2Meta)
	if got.ClientID != "new" {
		t.Errorf("ClientID = %q, want new", got.ClientID)
	}

	at, err := s.ReadSecret(ctx, "yaml-tools", second.ID, credentials.SecretRoleAccessToken)
	if err != nil {
		t.Fatalf("ReadSecret access after Put: %v", err)
	}
	if string(at) != "at" {
		t.Errorf("access secret = %q after Put", at)
	}
	rt, err := s.ReadSecret(ctx, "yaml-tools", second.ID, credentials.SecretRoleRefreshToken)
	if err != nil {
		t.Fatalf("ReadSecret refresh after Put: %v", err)
	}
	if string(rt) != "rt" {
		t.Errorf("refresh secret = %q after Put", rt)
	}
}

func TestGetByID(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	put, _ := s.Put(ctx, "yaml-tools", "main", credentials.RawMeta{})
	got, err := s.GetByID(ctx, "yaml-tools", put.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name != "main" || got.ID != put.ID {
		t.Errorf("got %+v, want id=%q name=main", got, put.ID)
	}
}

func TestGetByName(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	put, _ := s.Put(ctx, "yaml-tools", "main", credentials.RawMeta{})
	got, err := s.GetByName(ctx, "yaml-tools", "main")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if got.ID != put.ID {
		t.Errorf("ID = %q, want %q", got.ID, put.ID)
	}
}

func TestGet_Missing(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.GetByID(ctx, "yaml-tools", "missing"); !errors.Is(err, credentials.ErrNotFound) {
		t.Errorf("GetByID missing: %v, want ErrNotFound", err)
	}
	if _, err := s.GetByName(ctx, "yaml-tools", "missing"); !errors.Is(err, credentials.ErrNotFound) {
		t.Errorf("GetByName missing: %v, want ErrNotFound", err)
	}
	if _, err := s.GetByID(ctx, "absent-particle", "any"); !errors.Is(err, credentials.ErrNotFound) {
		t.Errorf("GetByID absent particle: %v, want ErrNotFound", err)
	}
}

func TestList(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for _, p := range []struct {
		name string
		meta credentials.Metadata
	}{
		{"github", credentials.OAuth2Meta{}},
		{"stripe", credentials.APIKeyMeta{Location: credentials.ApplySpec{Kind: credentials.ApplyHeader, Name: "X-Stripe"}}},
		{"anthropic", credentials.RawMeta{}},
	} {
		if _, err := s.Put(ctx, "tools", p.name, p.meta); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.List(ctx, "tools")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(List) = %d, want 3", len(got))
	}
	wantOrder := []string{"anthropic", "github", "stripe"}
	for i, le := range got {
		if le.Name != wantOrder[i] {
			t.Errorf("List[%d].Name = %q, want %q", i, le.Name, wantOrder[i])
		}
		if !idShape.MatchString(le.ID) {
			t.Errorf("List[%d].ID malformed: %q", i, le.ID)
		}
	}
	wantKind := map[string]credentials.Kind{
		"anthropic": credentials.KindRaw,
		"github":    credentials.KindOAuth2,
		"stripe":    credentials.KindAPIKey,
	}
	for _, le := range got {
		if le.Kind != wantKind[le.Name] {
			t.Errorf("List entry %q kind = %q, want %q", le.Name, le.Kind, wantKind[le.Name])
		}
	}
}

func TestList_EmptyParticle(t *testing.T) {
	s := newStore(t)
	got, err := s.List(context.Background(), "absent")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List = %v, want empty", got)
	}
}

func TestDelete_RemovesEverything(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	put, _ := s.Put(ctx, "tools", "x", credentials.RawMeta{})
	if err := s.WriteSecrets(ctx, "tools", put.ID,
		credentials.Secret{Role: credentials.SecretRoleValue, Value: []byte("v")},
	); err != nil {
		t.Fatal(err)
	}

	if err := s.Delete(ctx, "tools", put.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := s.GetByID(ctx, "tools", put.ID); !errors.Is(err, credentials.ErrNotFound) {
		t.Errorf("after Delete, GetByID err = %v, want ErrNotFound", err)
	}
	if _, err := s.GetByName(ctx, "tools", "x"); !errors.Is(err, credentials.ErrNotFound) {
		t.Errorf("after Delete, GetByName err = %v, want ErrNotFound", err)
	}
	if _, err := s.ReadSecret(ctx, "tools", put.ID, credentials.SecretRoleValue); !errors.Is(err, credentials.ErrNotFound) {
		t.Errorf("after Delete, ReadSecret err = %v, want ErrNotFound", err)
	}

	if err := s.Delete(ctx, "tools", put.ID); err != nil {
		t.Errorf("second Delete: %v", err)
	}
}

func TestPut_AtomicCreateWithSecrets(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	desc, err := s.Put(ctx, "yaml-tools", "gh", credentials.OAuth2Meta{ClientID: "c"},
		credentials.Secret{Role: credentials.SecretRoleAccessToken, Value: []byte("at")},
		credentials.Secret{Role: credentials.SecretRoleRefreshToken, Value: []byte("rt")},
		credentials.Secret{Role: credentials.SecretRoleClientSecret, Value: []byte("cs")},
	)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	for _, want := range []struct {
		role  credentials.SecretRole
		value string
	}{
		{credentials.SecretRoleAccessToken, "at"},
		{credentials.SecretRoleRefreshToken, "rt"},
		{credentials.SecretRoleClientSecret, "cs"},
	} {
		got, err := s.ReadSecret(ctx, "yaml-tools", desc.ID, want.role)
		if err != nil {
			t.Errorf("ReadSecret %s: %v", want.role, err)
			continue
		}
		if string(got) != want.value {
			t.Errorf("%s = %q, want %q", want.role, got, want.value)
		}
	}
}

func TestPut_AtomicUpdatePreservesUnlistedSecrets(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	first, _ := s.Put(ctx, "yaml-tools", "gh",
		credentials.OAuth2Meta{ClientID: "c-1"},
		credentials.Secret{Role: credentials.SecretRoleAccessToken, Value: []byte("at-1")},
		credentials.Secret{Role: credentials.SecretRoleRefreshToken, Value: []byte("rt-1")},
		credentials.Secret{Role: credentials.SecretRoleClientSecret, Value: []byte("cs-1")},
	)

	second, _ := s.Put(ctx, "yaml-tools", "gh",
		credentials.OAuth2Meta{ClientID: "c-2"},
		credentials.Secret{Role: credentials.SecretRoleAccessToken, Value: []byte("at-2")},
	)

	if first.ID != second.ID {
		t.Errorf("ID changed across atomic refresh: %q → %q", first.ID, second.ID)
	}
	got := second.Meta.(credentials.OAuth2Meta)
	if got.ClientID != "c-2" {
		t.Errorf("ClientID = %q, want c-2", got.ClientID)
	}

	at, _ := s.ReadSecret(ctx, "yaml-tools", second.ID, credentials.SecretRoleAccessToken)
	if string(at) != "at-2" {
		t.Errorf("access = %q, want at-2", at)
	}
	rt, _ := s.ReadSecret(ctx, "yaml-tools", second.ID, credentials.SecretRoleRefreshToken)
	if string(rt) != "rt-1" {
		t.Errorf("refresh = %q, want rt-1 (preserved)", rt)
	}
	cs, _ := s.ReadSecret(ctx, "yaml-tools", second.ID, credentials.SecretRoleClientSecret)
	if string(cs) != "cs-1" {
		t.Errorf("client secret = %q, want cs-1 (preserved)", cs)
	}
}

func TestPut_RejectsEmptyArgs(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, "yaml-tools", "", credentials.RawMeta{}); err == nil {
		t.Error("Put with empty name should error")
	}
	if _, err := s.Put(ctx, "yaml-tools", "name", nil); err == nil {
		t.Error("Put with nil Metadata should error")
	}
}

// -----------------------------------------------------------------------------
// Secret operations
// -----------------------------------------------------------------------------

func TestWriteSecrets_RoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	put, _ := s.Put(ctx, "tools", "k", credentials.OAuth2Meta{})

	if err := s.WriteSecrets(ctx, "tools", put.ID,
		credentials.Secret{Role: credentials.SecretRoleAccessToken, Value: []byte("abc")},
	); err != nil {
		t.Fatalf("WriteSecrets: %v", err)
	}
	got, err := s.ReadSecret(ctx, "tools", put.ID, credentials.SecretRoleAccessToken)
	if err != nil {
		t.Fatalf("ReadSecret: %v", err)
	}
	if string(got) != "abc" {
		t.Errorf("ReadSecret = %q, want abc", got)
	}
}

func TestWriteSecrets_AtomicRotateLeavesOthersAlone(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	put, _ := s.Put(ctx, "tools", "gh", credentials.OAuth2Meta{})

	_ = s.WriteSecrets(ctx, "tools", put.ID,
		credentials.Secret{Role: credentials.SecretRoleAccessToken, Value: []byte("at-1")},
		credentials.Secret{Role: credentials.SecretRoleRefreshToken, Value: []byte("rt-1")},
		credentials.Secret{Role: credentials.SecretRoleClientSecret, Value: []byte("cs-1")},
	)

	_ = s.WriteSecrets(ctx, "tools", put.ID,
		credentials.Secret{Role: credentials.SecretRoleAccessToken, Value: []byte("at-2")},
		credentials.Secret{Role: credentials.SecretRoleRefreshToken, Value: []byte("rt-2")},
	)

	at, _ := s.ReadSecret(ctx, "tools", put.ID, credentials.SecretRoleAccessToken)
	rt, _ := s.ReadSecret(ctx, "tools", put.ID, credentials.SecretRoleRefreshToken)
	cs, _ := s.ReadSecret(ctx, "tools", put.ID, credentials.SecretRoleClientSecret)
	if string(at) != "at-2" {
		t.Errorf("access = %q, want at-2", at)
	}
	if string(rt) != "rt-2" {
		t.Errorf("refresh = %q, want rt-2", rt)
	}
	if string(cs) != "cs-1" {
		t.Errorf("client secret = %q, want cs-1 (untouched)", cs)
	}
}

func TestReadSecret_NotSet(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	put, _ := s.Put(ctx, "tools", "k", credentials.RawMeta{})

	_, err := s.ReadSecret(ctx, "tools", put.ID, credentials.SecretRoleValue)
	if !errors.Is(err, credentials.ErrSecretNotSet) {
		t.Errorf("err = %v, want ErrSecretNotSet", err)
	}
	if !errors.Is(err, credentials.ErrNotFound) {
		t.Errorf("err = %v, should also satisfy errors.Is(_, ErrNotFound)", err)
	}
}

func TestReadSecret_EntryMissing(t *testing.T) {
	s := newStore(t)
	_, err := s.ReadSecret(context.Background(), "tools", "no-such-id", credentials.SecretRoleValue)
	if !errors.Is(err, credentials.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestWriteSecrets_EntryMissing(t *testing.T) {
	s := newStore(t)
	err := s.WriteSecrets(context.Background(), "tools", "no-such-id",
		credentials.Secret{Role: credentials.SecretRoleValue, Value: []byte("v")},
	)
	if !errors.Is(err, credentials.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestWriteSecrets_RejectsEmptyRole(t *testing.T) {
	s := newStore(t)
	put, _ := s.Put(context.Background(), "tools", "k", credentials.RawMeta{})
	if err := s.WriteSecrets(context.Background(), "tools", put.ID,
		credentials.Secret{Role: "", Value: []byte("v")},
	); err == nil {
		t.Error("WriteSecrets with an empty role should error")
	}
}

func TestDeleteSecret_Idempotent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	put, _ := s.Put(ctx, "tools", "k", credentials.RawMeta{})
	_ = s.WriteSecrets(ctx, "tools", put.ID,
		credentials.Secret{Role: credentials.SecretRoleValue, Value: []byte("v")},
	)

	if err := s.DeleteSecret(ctx, "tools", put.ID, credentials.SecretRoleValue); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadSecret(ctx, "tools", put.ID, credentials.SecretRoleValue); !errors.Is(err, credentials.ErrSecretNotSet) {
		t.Errorf("after DeleteSecret, ReadSecret err = %v, want ErrSecretNotSet", err)
	}
	if err := s.DeleteSecret(ctx, "tools", put.ID, credentials.SecretRoleValue); err != nil {
		t.Errorf("second DeleteSecret: %v", err)
	}
	if err := s.DeleteSecret(ctx, "tools", "no-such-id", credentials.SecretRoleValue); err != nil {
		t.Errorf("DeleteSecret on missing entry: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Cross-cutting
// -----------------------------------------------------------------------------

func TestScopedByParticle(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	yaml, _ := s.Put(ctx, "yaml-tools", "shared", credentials.RawMeta{})
	json, _ := s.Put(ctx, "json-tools", "shared", credentials.RawMeta{})

	if yaml.ID == json.ID {
		t.Errorf("IDs collided across particles: %q", yaml.ID)
	}

	_ = s.WriteSecrets(ctx, "yaml-tools", yaml.ID,
		credentials.Secret{Role: credentials.SecretRoleValue, Value: []byte("yaml-secret")},
	)
	if _, err := s.ReadSecret(ctx, "json-tools", yaml.ID, credentials.SecretRoleValue); !errors.Is(err, credentials.ErrNotFound) {
		t.Errorf("ID lookup leaked across particle scopes: err = %v", err)
	}
}

// All five Metadata kinds must round-trip through the JSON codec
// untouched — this is the SQLite store's only concession to
// serialization, and getting it wrong corrupts every read.
func TestMetadataRoundTrip_AllKinds(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	cases := []struct {
		name string
		meta credentials.Metadata
	}{
		{"basic", credentials.BasicMeta{Username: "alice"}},
		{"oauth2", credentials.OAuth2Meta{
			AuthorizationURL: "https://example/authorize",
			TokenURL:         "https://example/token",
			RevocationURL:    "https://example/revoke",
			ClientID:         "client-123",
			Scopes:           []string{"read", "write"},
			Flow:             "authorization-code-pkce",
		}},
		{"apikey-header", credentials.APIKeyMeta{Location: credentials.ApplySpec{Kind: credentials.ApplyHeader, Name: "X-API-Key"}}},
		{"apikey-query", credentials.APIKeyMeta{Location: credentials.ApplySpec{Kind: credentials.ApplyQueryParam, Name: "api_key"}}},
		{"apikey-scheme", credentials.APIKeyMeta{Location: credentials.ApplySpec{Kind: credentials.ApplyAuthScheme, Scheme: "Token"}}},
		{"signing-key", credentials.SigningKeyMeta{Algorithm: "hmac-sha256"}},
		{"raw", credentials.RawMeta{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			desc, err := s.Put(ctx, "p", c.name, c.meta)
			if err != nil {
				t.Fatalf("Put: %v", err)
			}
			got, err := s.GetByID(ctx, "p", desc.ID)
			if err != nil {
				t.Fatalf("GetByID: %v", err)
			}
			if !reflect.DeepEqual(got.Meta, c.meta) {
				t.Errorf("round-trip mismatch:\n got: %#v\nwant: %#v", got.Meta, c.meta)
			}
		})
	}
}

// IDs must be ASCII / no special chars per the Store contract.
func TestIDShape_NoSpecialChars(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		desc, err := s.Put(ctx, "tools", "n", credentials.RawMeta{})
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Delete(ctx, "tools", desc.ID)
		for _, r := range desc.ID {
			if !((r >= 'a' && r <= 'z') || (r >= '2' && r <= '7')) {
				t.Errorf("ID %q contains non-base32 rune %q", desc.ID, r)
				break
			}
		}
		if strings.ContainsAny(desc.ID, " \t\n=+/_-") {
			t.Errorf("ID %q contains forbidden character", desc.ID)
		}
	}
}

// -----------------------------------------------------------------------------
// Persistence — the whole point of the SQLite backend.
// -----------------------------------------------------------------------------

// Open the DB twice against the same file: state from the first
// Store is visible to a fresh Store opened against the same path.
// This is the property credentials/memory cannot offer.
func TestPersistsAcrossOpens(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "creds.db")
	dsn := "file:" + dbPath

	ctx := context.Background()

	// Both opens must use the same Sealer; otherwise the second
	// store can't decrypt what the first wrote. Using
	// NewKeyringSealer with the same (service, name) for both
	// is the production scenario.
	sealerOne, err := sqlite.NewKeyringSealer("particle-test", t.Name())
	if err != nil {
		t.Fatal(err)
	}

	db1, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	s1, err := sqlite.New(ctx, db1, sealerOne)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := s1.Put(ctx, "yaml-tools", "gh",
		credentials.OAuth2Meta{ClientID: "c-1", Scopes: []string{"repo"}},
		credentials.Secret{Role: credentials.SecretRoleAccessToken, Value: []byte("token")},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db1.Close(); err != nil {
		t.Fatal(err)
	}

	// Fresh sealer construction — the keyring already has the
	// key from the first call, so this loads (not generates) it.
	sealerTwo, err := sqlite.NewKeyringSealer("particle-test", t.Name())
	if err != nil {
		t.Fatal(err)
	}

	db2, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	s2, err := sqlite.New(ctx, db2, sealerTwo)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s2.GetByID(ctx, "yaml-tools", desc.ID)
	if err != nil {
		t.Fatalf("GetByID after reopen: %v", err)
	}
	want := credentials.OAuth2Meta{ClientID: "c-1", Scopes: []string{"repo"}}
	if !reflect.DeepEqual(got.Meta, want) {
		t.Errorf("after reopen, meta = %#v, want %#v", got.Meta, want)
	}
	v, err := s2.ReadSecret(ctx, "yaml-tools", desc.ID, credentials.SecretRoleAccessToken)
	if err != nil {
		t.Fatalf("ReadSecret after reopen: %v", err)
	}
	if string(v) != "token" {
		t.Errorf("secret = %q, want token", v)
	}
}

// Calling New twice against the same DB is fine — `CREATE TABLE IF
// NOT EXISTS` is idempotent. Important because hosts may
// reconstruct a Store on reload without dropping the underlying DB.
func TestNew_IdempotentMigration(t *testing.T) {
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	if _, err := sqlite.New(ctx, db, newSealer(t)); err != nil {
		t.Fatalf("first New: %v", err)
	}
	if _, err := sqlite.New(ctx, db, newSealer(t)); err != nil {
		t.Fatalf("second New: %v", err)
	}
}

func TestNew_RejectsNilDB(t *testing.T) {
	if _, err := sqlite.New(context.Background(), nil, newSealer(t)); err == nil {
		t.Error("New(nil) should error")
	}
}

func TestNew_RejectsNilSealer(t *testing.T) {
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := sqlite.New(context.Background(), db, nil); err == nil {
		t.Error("New(_, _, nil) should error")
	}
}
