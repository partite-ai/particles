package memory_test

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/partite-ai/particle/credentials"
	"github.com/partite-ai/particle/credentials/memory"
)

// IDs must be ASCII with no whitespace or punctuation. The memory
// store uses 26-char lowercase base32 (RFC 4648 alphabet, no
// padding).
var idShape = regexp.MustCompile(`^[a-z2-7]{26}$`)

// -----------------------------------------------------------------------------
// Metadata operations
// -----------------------------------------------------------------------------

func TestPut_GeneratesID(t *testing.T) {
	s := memory.New()
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

// Put with an existing name should preserve the ID AND the secrets
// — the design's whole reason to split metadata from secrets.
func TestPut_PreservesIDAndSecretsOnNameOverwrite(t *testing.T) {
	s := memory.New()
	ctx := context.Background()

	first, _ := s.Put(ctx, "yaml-tools", "gh", credentials.OAuth2Meta{ClientID: "old"})
	if err := s.WriteSecrets(ctx, "yaml-tools", first.ID,
		credentials.Secret{Role: credentials.SecretRoleAccessToken, Value: []byte("at")},
		credentials.Secret{Role: credentials.SecretRoleRefreshToken, Value: []byte("rt")},
	); err != nil {
		t.Fatalf("WriteSecrets: %v", err)
	}

	// Update metadata only — e.g., user re-ran setup with a
	// new client ID. Secrets should survive.
	second, _ := s.Put(ctx, "yaml-tools", "gh", credentials.OAuth2Meta{ClientID: "new"})
	if first.ID != second.ID {
		t.Errorf("ID changed on metadata-only update: %q → %q", first.ID, second.ID)
	}
	got := second.Meta.(credentials.OAuth2Meta)
	if got.ClientID != "new" {
		t.Errorf("ClientID = %q, want new", got.ClientID)
	}

	// Both secrets must still be readable.
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
	s := memory.New()
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
	s := memory.New()
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
	s := memory.New()
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

// Each particle has at most one credential, so List is per-
// particle "the one entry, if any." Seed multiple particles to
// verify scoping + correct surfacing of name/ID/kind.
func TestList(t *testing.T) {
	s := memory.New()
	ctx := context.Background()
	seed := []struct {
		particle string
		name     string
		meta     credentials.Metadata
		kind     credentials.Kind
	}{
		{"github", "pat", credentials.APIKeyMeta{Location: credentials.ApplySpec{Kind: credentials.ApplyHeader, Name: "X-K"}}, credentials.KindAPIKey},
		{"stripe", "oauth", credentials.OAuth2Meta{}, credentials.KindOAuth2},
		{"anthropic", "raw", credentials.RawMeta{}, credentials.KindRaw},
	}
	for _, p := range seed {
		if _, err := s.Put(ctx, p.particle, p.name, p.meta); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range seed {
		got, err := s.List(ctx, p.particle)
		if err != nil {
			t.Fatalf("List(%s): %v", p.particle, err)
		}
		if len(got) != 1 {
			t.Errorf("List(%s) len = %d, want 1", p.particle, len(got))
			continue
		}
		if got[0].Name != p.name {
			t.Errorf("List(%s)[0].Name = %q, want %q", p.particle, got[0].Name, p.name)
		}
		if !idShape.MatchString(got[0].ID) {
			t.Errorf("List(%s)[0].ID malformed: %q", p.particle, got[0].ID)
		}
		if got[0].Kind != p.kind {
			t.Errorf("List(%s)[0].Kind = %q, want %q", p.particle, got[0].Kind, p.kind)
		}
	}
}

func TestList_EmptyParticle(t *testing.T) {
	s := memory.New()
	got, err := s.List(context.Background(), "absent")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List = %v, want empty", got)
	}
}

func TestDelete_RemovesEverything(t *testing.T) {
	s := memory.New()
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
	// Secrets should be gone too.
	if _, err := s.ReadSecret(ctx, "tools", put.ID, credentials.SecretRoleValue); !errors.Is(err, credentials.ErrNotFound) {
		t.Errorf("after Delete, ReadSecret err = %v, want ErrNotFound", err)
	}

	// Idempotent.
	if err := s.Delete(ctx, "tools", put.ID); err != nil {
		t.Errorf("second Delete: %v", err)
	}
}

// Put with vararg secrets is the new-entry happy path: metadata
// + every secret set together. The OAuth-refresh path (subset of
// secrets to update, others preserved) is verified by
// TestPut_AtomicUpdatePreservesUnlistedSecrets below.
func TestPut_AtomicCreateWithSecrets(t *testing.T) {
	s := memory.New()
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

// On Put-by-name overwrite, secrets passed in `secrets` replace the
// listed roles; secrets NOT mentioned are preserved. Useful for any
// case where setup is genuinely re-run with a new metadata shape
// AND new secrets in lockstep — distinct from the refresh path,
// which uses WriteSecrets and never touches metadata.
func TestPut_AtomicUpdatePreservesUnlistedSecrets(t *testing.T) {
	s := memory.New()
	ctx := context.Background()

	first, _ := s.Put(ctx, "yaml-tools", "gh",
		credentials.OAuth2Meta{ClientID: "c-1"},
		credentials.Secret{Role: credentials.SecretRoleAccessToken, Value: []byte("at-1")},
		credentials.Secret{Role: credentials.SecretRoleRefreshToken, Value: []byte("rt-1")},
		credentials.Secret{Role: credentials.SecretRoleClientSecret, Value: []byte("cs-1")},
	)

	// Re-run setup with a new client id + new access token.
	// Refresh / client secret left alone.
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
	s := memory.New()
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
	s := memory.New()
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

// WriteSecrets is the OAuth-refresh primitive: rotate access (and
// optionally refresh) atomically without touching metadata or any
// other secret. This is the design's main motivator.
func TestWriteSecrets_AtomicRotateLeavesOthersAlone(t *testing.T) {
	s := memory.New()
	ctx := context.Background()
	put, _ := s.Put(ctx, "tools", "gh", credentials.OAuth2Meta{})

	_ = s.WriteSecrets(ctx, "tools", put.ID,
		credentials.Secret{Role: credentials.SecretRoleAccessToken, Value: []byte("at-1")},
		credentials.Secret{Role: credentials.SecretRoleRefreshToken, Value: []byte("rt-1")},
		credentials.Secret{Role: credentials.SecretRoleClientSecret, Value: []byte("cs-1")},
	)

	// Rotation: new access token, and provider also rotated
	// the refresh token — both written atomically. Client
	// secret untouched.
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
	s := memory.New()
	ctx := context.Background()
	put, _ := s.Put(ctx, "tools", "k", credentials.RawMeta{})

	_, err := s.ReadSecret(ctx, "tools", put.ID, credentials.SecretRoleValue)
	if !errors.Is(err, credentials.ErrSecretNotSet) {
		t.Errorf("err = %v, want ErrSecretNotSet", err)
	}
	// ErrSecretNotSet wraps ErrNotFound, so the conflated check
	// also passes.
	if !errors.Is(err, credentials.ErrNotFound) {
		t.Errorf("err = %v, should also satisfy errors.Is(_, ErrNotFound)", err)
	}
}

func TestReadSecret_EntryMissing(t *testing.T) {
	s := memory.New()
	_, err := s.ReadSecret(context.Background(), "tools", "no-such-id", credentials.SecretRoleValue)
	if !errors.Is(err, credentials.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestWriteSecrets_EntryMissing(t *testing.T) {
	s := memory.New()
	err := s.WriteSecrets(context.Background(), "tools", "no-such-id",
		credentials.Secret{Role: credentials.SecretRoleValue, Value: []byte("v")},
	)
	if !errors.Is(err, credentials.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestWriteSecrets_RejectsEmptyRole(t *testing.T) {
	s := memory.New()
	put, _ := s.Put(context.Background(), "tools", "k", credentials.RawMeta{})
	if err := s.WriteSecrets(context.Background(), "tools", put.ID,
		credentials.Secret{Role: "", Value: []byte("v")},
	); err == nil {
		t.Error("WriteSecrets with an empty role should error")
	}
}

func TestDeleteSecret_Idempotent(t *testing.T) {
	s := memory.New()
	ctx := context.Background()
	put, _ := s.Put(ctx, "tools", "k", credentials.RawMeta{})
	_ = s.WriteSecrets(ctx, "tools", put.ID,
		credentials.Secret{Role: credentials.SecretRoleValue, Value: []byte("v")},
	)

	if err := s.DeleteSecret(ctx, "tools", put.ID, credentials.SecretRoleValue); err != nil {
		t.Fatal(err)
	}
	// After delete, secret is missing.
	if _, err := s.ReadSecret(ctx, "tools", put.ID, credentials.SecretRoleValue); !errors.Is(err, credentials.ErrSecretNotSet) {
		t.Errorf("after DeleteSecret, ReadSecret err = %v, want ErrSecretNotSet", err)
	}
	// Re-deleting is fine.
	if err := s.DeleteSecret(ctx, "tools", put.ID, credentials.SecretRoleValue); err != nil {
		t.Errorf("second DeleteSecret: %v", err)
	}
	// Deleting from a missing entry is also fine — idempotent.
	if err := s.DeleteSecret(ctx, "tools", "no-such-id", credentials.SecretRoleValue); err != nil {
		t.Errorf("DeleteSecret on missing entry: %v", err)
	}
}

// Secret reads return a copy — callers can't mutate stored state by
// modifying the returned slice.
func TestReadSecret_ReturnsCopy(t *testing.T) {
	s := memory.New()
	ctx := context.Background()
	put, _ := s.Put(ctx, "tools", "k", credentials.RawMeta{})
	original := []byte("secret")
	_ = s.WriteSecrets(ctx, "tools", put.ID,
		credentials.Secret{Role: credentials.SecretRoleValue, Value: original},
	)

	got, _ := s.ReadSecret(ctx, "tools", put.ID, credentials.SecretRoleValue)
	got[0] = 'X' // mutate the returned slice

	again, _ := s.ReadSecret(ctx, "tools", put.ID, credentials.SecretRoleValue)
	if string(again) != "secret" {
		t.Errorf("stored value mutated through returned slice: %q", again)
	}
}

// -----------------------------------------------------------------------------
// Cross-cutting
// -----------------------------------------------------------------------------

func TestScopedByParticle(t *testing.T) {
	s := memory.New()
	ctx := context.Background()
	yaml, _ := s.Put(ctx, "yaml-tools", "shared", credentials.RawMeta{})
	json, _ := s.Put(ctx, "json-tools", "shared", credentials.RawMeta{})

	if yaml.ID == json.ID {
		t.Errorf("IDs collided across particles: %q", yaml.ID)
	}

	// Secrets are also particle-scoped — same id in another
	// particle's namespace doesn't exist.
	_ = s.WriteSecrets(ctx, "yaml-tools", yaml.ID,
		credentials.Secret{Role: credentials.SecretRoleValue, Value: []byte("yaml-secret")},
	)
	if _, err := s.ReadSecret(ctx, "json-tools", yaml.ID, credentials.SecretRoleValue); !errors.Is(err, credentials.ErrNotFound) {
		t.Errorf("ID lookup leaked across particle scopes: err = %v", err)
	}
}

// IDs must be ASCII / no special chars per the Store contract.
func TestIDShape_NoSpecialChars(t *testing.T) {
	s := memory.New()
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		desc, err := s.Put(ctx, "tools", "n", credentials.RawMeta{})
		if err != nil {
			t.Fatal(err)
		}
		// Re-Put preserves the ID, so to generate fresh IDs we
		// delete first.
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
