package credentials

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/partite-ai/wacogo"

	gen "github.com/partite-ai/particles/internal/host/gen/particle/host/credentials"
)

// fakeStore is a minimal in-test [Store] used to drive the
// adapter. It models the (metadata, secrets) split the real
// interface exposes.
type fakeStore struct {
	byName map[string]*fakeRecord
	byID   map[string]*fakeRecord

	// optional override returned from any Get/ReadSecret/Write
	// call, regardless of arguments.
	getErr error
}

type fakeRecord struct {
	id      string
	name    string
	method  string
	meta    Metadata
	secrets map[SecretRole][]byte
}

var _ Store = (*fakeStore)(nil)

func (s *fakeStore) GetByID(_ context.Context, id string) (Descriptor, error) {
	if s.getErr != nil {
		return Descriptor{}, s.getErr
	}
	rec, ok := s.byID[id]
	if !ok {
		return Descriptor{}, ErrNotFound
	}
	return Descriptor{ID: rec.id, Name: rec.name, Method: rec.method, Meta: rec.meta}, nil
}

func (s *fakeStore) GetByName(_ context.Context, name string) (Descriptor, error) {
	if s.getErr != nil {
		return Descriptor{}, s.getErr
	}
	rec, ok := s.byName[name]
	if !ok {
		return Descriptor{}, ErrNotFound
	}
	return Descriptor{ID: rec.id, Name: rec.name, Method: rec.method, Meta: rec.meta}, nil
}

func (s *fakeStore) List(_ context.Context) ([]ListEntry, error) {
	var out []ListEntry
	for _, rec := range s.byName {
		out = append(out, ListEntry{ID: rec.id, Name: rec.name, Method: rec.method, Kind: rec.meta.Kind()})
	}
	return out, nil
}

func (s *fakeStore) Put(_ context.Context, name, method string, meta Metadata, secrets ...Secret) (Descriptor, error) {
	if s.byName == nil {
		s.byName = map[string]*fakeRecord{}
		s.byID = map[string]*fakeRecord{}
	}
	rec, ok := s.byName[name]
	if !ok {
		rec = &fakeRecord{id: "id-" + name, name: name, method: method, meta: meta, secrets: map[SecretRole][]byte{}}
		s.byName[name] = rec
		s.byID[rec.id] = rec
	} else {
		if rec.method != method {
			// Mirror the production "method switch wipes
			// secrets" behavior so adapter tests see the
			// real failure mode.
			rec.secrets = map[SecretRole][]byte{}
		}
		rec.method = method
		rec.meta = meta
	}
	for _, sec := range secrets {
		rec.secrets[sec.Role] = append([]byte(nil), sec.Value...)
	}
	return Descriptor{ID: rec.id, Name: rec.name, Method: rec.method, Meta: rec.meta}, nil
}

func (s *fakeStore) ConfiguredMethod(_ context.Context, name string) (string, error) {
	rec, ok := s.byName[name]
	if !ok {
		return "", nil
	}
	return rec.method, nil
}

func (s *fakeStore) Delete(_ context.Context, id string) error {
	rec, ok := s.byID[id]
	if !ok {
		return nil
	}
	delete(s.byID, id)
	delete(s.byName, rec.name)
	return nil
}

func (s *fakeStore) ReadSecret(_ context.Context, id string, role SecretRole) ([]byte, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	rec, ok := s.byID[id]
	if !ok {
		return nil, ErrNotFound
	}
	v, ok := rec.secrets[role]
	if !ok {
		return nil, ErrSecretNotSet
	}
	return append([]byte(nil), v...), nil
}

func (s *fakeStore) WriteSecrets(_ context.Context, id string, secrets ...Secret) error {
	rec, ok := s.byID[id]
	if !ok {
		return ErrNotFound
	}
	for _, sec := range secrets {
		rec.secrets[sec.Role] = append([]byte(nil), sec.Value...)
	}
	return nil
}

func (s *fakeStore) DeleteSecret(_ context.Context, id string, role SecretRole) error {
	rec, ok := s.byID[id]
	if !ok {
		return nil
	}
	delete(rec.secrets, role)
	return nil
}

// putWithSecrets pre-populates an entry with its metadata and
// (optionally) some secret values for adapter tests. The
// credential's method is set to its name; adapter tests don't
// exercise the method dimension.
func (s *fakeStore) putWithSecrets(id, name string, meta Metadata, secrets map[SecretRole][]byte) {
	if s.byName == nil {
		s.byName = map[string]*fakeRecord{}
		s.byID = map[string]*fakeRecord{}
	}
	rec := &fakeRecord{id: id, name: name, method: name, meta: meta, secrets: map[SecretRole][]byte{}}
	for k, v := range secrets {
		rec.secrets[k] = append([]byte(nil), v...)
	}
	s.byName[name] = rec
	s.byID[id] = rec
}

// -----------------------------------------------------------------------------
// Manager lifecycle
// -----------------------------------------------------------------------------

func TestManager_NewCredentialsInstance(t *testing.T) {
	ctx := context.Background()
	e := wacogo.NewEngine(ctx)
	defer e.Close(ctx)

	mgr, err := NewManager(ctx, ManagerConfig{Engine: e})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close(ctx)

	inst, err := mgr.NewCredentialsInstance(ctx, &fakeStore{})
	if err != nil {
		t.Fatalf("NewCredentialsInstance: %v", err)
	}
	defer inst.Close(ctx)
	if inst.Core() == nil {
		t.Error("instance core is nil")
	}
}

func TestManager_NewOAuthInstance(t *testing.T) {
	ctx := context.Background()
	e := wacogo.NewEngine(ctx)
	defer e.Close(ctx)

	mgr, err := NewManager(ctx, ManagerConfig{Engine: e})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close(ctx)

	inst, err := mgr.NewOAuthInstance(ctx, &fakeStore{}, nil)
	if err != nil {
		t.Fatalf("NewOAuthInstance: %v", err)
	}
	defer inst.Close(ctx)
	if inst.Core() == nil {
		t.Error("instance core is nil")
	}
}

func TestManager_NewSigningInstance(t *testing.T) {
	ctx := context.Background()
	e := wacogo.NewEngine(ctx)
	defer e.Close(ctx)

	mgr, err := NewManager(ctx, ManagerConfig{Engine: e})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close(ctx)

	inst, err := mgr.NewSigningInstance(ctx, &fakeStore{})
	if err != nil {
		t.Fatalf("NewSigningInstance: %v", err)
	}
	defer inst.Close(ctx)
	if inst.Core() == nil {
		t.Error("instance core is nil")
	}
}

// One Manager builds host instances for multiple capabilities
// for the same particle in parallel — this is the wiring shape
// a real runtime composition uses.
func TestManager_BothCapabilitiesForOneParticle(t *testing.T) {
	ctx := context.Background()
	e := wacogo.NewEngine(ctx)
	defer e.Close(ctx)

	mgr, err := NewManager(ctx, ManagerConfig{Engine: e})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(ctx)

	store := &fakeStore{}
	credInst, err := mgr.NewCredentialsInstance(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	defer credInst.Close(ctx)

	oauthInst, err := mgr.NewOAuthInstance(ctx, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer oauthInst.Close(ctx)

	if credInst.Core() == oauthInst.Core() {
		t.Error("credentials and oauth instances should be distinct host components")
	}
}

func TestNewManager_RejectsMissingEngine(t *testing.T) {
	if _, err := NewManager(context.Background(), ManagerConfig{}); err == nil {
		t.Error("expected error when Engine is nil")
	}
}

func TestManager_RejectsNilStore(t *testing.T) {
	ctx := context.Background()
	e := wacogo.NewEngine(ctx)
	defer e.Close(ctx)

	mgr, err := NewManager(ctx, ManagerConfig{Engine: e})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(ctx)

	if _, err := mgr.NewCredentialsInstance(ctx, nil); err == nil {
		t.Error("expected error for nil store")
	}
	if _, err := mgr.NewOAuthInstance(ctx, nil, nil); err == nil {
		t.Error("expected error for nil store")
	}
	if _, err := mgr.NewSigningInstance(ctx, nil); err == nil {
		t.Error("expected error for nil store")
	}
}

// nil Refresher → HTTPRefresher default. Only relevant when oauth
// instances are actually used.
func TestNewManager_NilRefresher_DefaultsToHTTPRefresher(t *testing.T) {
	ctx := context.Background()
	e := wacogo.NewEngine(ctx)
	defer e.Close(ctx)

	mgr, err := NewManager(ctx, ManagerConfig{Engine: e})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(ctx)
	if _, ok := mgr.refresher.(*HTTPRefresher); !ok {
		t.Errorf("default refresher = %T, want *HTTPRefresher", mgr.refresher)
	}
}

// -----------------------------------------------------------------------------
// Adapter — GetPlaceholder
// -----------------------------------------------------------------------------

func TestAdapter_GetPlaceholder_Basic(t *testing.T) {
	store := &fakeStore{}
	store.putWithSecrets("id-1", "db_creds", BasicMeta{Username: "alice"}, nil)

	a := newAdapter(store)
	res, err := a.GetPlaceholder(context.Background(), "db_creds")
	if err != nil {
		t.Fatalf("GetPlaceholder: %v", err)
	}
	ok := res.(gen.ResultPlaceholderInfoCredentialErrorOk)
	if ok.Value.Placeholder != PlaceholderPrefix+"id-1" {
		t.Errorf("placeholder = %q, want %q", ok.Value.Placeholder, PlaceholderPrefix+"id-1")
	}
	if ok.Value.Apply.Kind != gen.ApplyKindBasic {
		t.Errorf("Apply.Kind = %v, want basic", ok.Value.Apply.Kind)
	}
}

func TestAdapter_GetPlaceholder_OAuth2(t *testing.T) {
	store := &fakeStore{}
	store.putWithSecrets("id-2", "gh", OAuth2Meta{ClientID: "client"}, nil)

	a := newAdapter(store)
	res, _ := a.GetPlaceholder(context.Background(), "gh")
	ok := res.(gen.ResultPlaceholderInfoCredentialErrorOk)
	if ok.Value.Apply.Kind != gen.ApplyKindBearer {
		t.Errorf("Kind = %v, want bearer", ok.Value.Apply.Kind)
	}
}

func TestAdapter_GetPlaceholder_APIKey_PopulatesLocation(t *testing.T) {
	store := &fakeStore{}
	store.putWithSecrets("i1", "k_header",
		APIKeyMeta{Location: ApplySpec{Kind: ApplyHeader, Name: "X-API-Key"}}, nil)
	store.putWithSecrets("i2", "k_query",
		APIKeyMeta{Location: ApplySpec{Kind: ApplyQueryParam, Name: "api_key"}}, nil)
	store.putWithSecrets("i3", "k_scheme",
		APIKeyMeta{Location: ApplySpec{Kind: ApplyAuthScheme, Scheme: "Token"}}, nil)
	a := newAdapter(store)

	t.Run("header", func(t *testing.T) {
		res, _ := a.GetPlaceholder(context.Background(), "k_header")
		ok := res.(gen.ResultPlaceholderInfoCredentialErrorOk)
		if ok.Value.Apply.Kind != gen.ApplyKindHeader {
			t.Fatalf("Kind = %v", ok.Value.Apply.Kind)
		}
		if !ok.Value.Apply.Name.IsSome || ok.Value.Apply.Name.Value != "X-API-Key" {
			t.Errorf("Name = %+v", ok.Value.Apply.Name)
		}
	})
	t.Run("query", func(t *testing.T) {
		res, _ := a.GetPlaceholder(context.Background(), "k_query")
		ok := res.(gen.ResultPlaceholderInfoCredentialErrorOk)
		if !ok.Value.Apply.Name.IsSome || ok.Value.Apply.Name.Value != "api_key" {
			t.Errorf("Name = %+v", ok.Value.Apply.Name)
		}
	})
	t.Run("auth-scheme", func(t *testing.T) {
		res, _ := a.GetPlaceholder(context.Background(), "k_scheme")
		ok := res.(gen.ResultPlaceholderInfoCredentialErrorOk)
		if !ok.Value.Apply.Scheme.IsSome || ok.Value.Apply.Scheme.Value != "Token" {
			t.Errorf("Scheme = %+v", ok.Value.Apply.Scheme)
		}
	})
}

func TestAdapter_GetPlaceholder_NotConfigured(t *testing.T) {
	a := newAdapter(&fakeStore{})
	res, _ := a.GetPlaceholder(context.Background(), "missing")
	errRes := res.(gen.ResultPlaceholderInfoCredentialErrorErr)
	if _, ok := errRes.Value.(gen.CredentialErrorNotConfigured); !ok {
		t.Errorf("got %T, want NotConfigured", errRes.Value)
	}
}

func TestAdapter_GetPlaceholder_StorageError(t *testing.T) {
	store := &fakeStore{getErr: errors.New("disk full")}
	a := newAdapter(store)
	res, _ := a.GetPlaceholder(context.Background(), "x")
	errRes := res.(gen.ResultPlaceholderInfoCredentialErrorErr)
	storage, ok := errRes.Value.(gen.CredentialErrorStorageError)
	if !ok {
		t.Fatalf("got %T, want StorageError", errRes.Value)
	}
	if storage.Value != "disk full" {
		t.Errorf("message = %q", storage.Value)
	}
}

func TestAdapter_GetPlaceholder_TypeMismatch(t *testing.T) {
	cases := []struct {
		label   string
		meta    Metadata
		wantSub string
	}{
		{"signing-key", SigningKeyMeta{Algorithm: "hmac-sha256"}, "signing API"},
		{"raw", RawMeta{}, "getRaw"},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			store := &fakeStore{}
			store.putWithSecrets("i", "x", c.meta, nil)
			a := newAdapter(store)
			res, _ := a.GetPlaceholder(context.Background(), "x")
			errRes := res.(gen.ResultPlaceholderInfoCredentialErrorErr)
			tm, ok := errRes.Value.(gen.CredentialErrorTypeMismatch)
			if !ok {
				t.Fatalf("got %T, want TypeMismatch", errRes.Value)
			}
			if !strings.Contains(tm.Value, c.wantSub) {
				t.Errorf("message = %q, want substring %q", tm.Value, c.wantSub)
			}
		})
	}
}

// Placeholder embeds the credential's stable ID — the wasi:http
// policy can recover it deterministically by stripping the prefix.
func TestAdapter_GetPlaceholder_EmbedsID(t *testing.T) {
	store := &fakeStore{}
	store.putWithSecrets("stable-id-7", "db", BasicMeta{}, nil)
	a := newAdapter(store)
	res, _ := a.GetPlaceholder(context.Background(), "db")
	ok := res.(gen.ResultPlaceholderInfoCredentialErrorOk)
	if want := PlaceholderPrefix + "stable-id-7"; ok.Value.Placeholder != want {
		t.Errorf("placeholder = %q, want %q", ok.Value.Placeholder, want)
	}
}

func TestAdapter_GetPlaceholder_StablePerCall(t *testing.T) {
	store := &fakeStore{}
	store.putWithSecrets("i", "db", BasicMeta{}, nil)
	a := newAdapter(store)
	res1, _ := a.GetPlaceholder(context.Background(), "db")
	res2, _ := a.GetPlaceholder(context.Background(), "db")
	p1 := res1.(gen.ResultPlaceholderInfoCredentialErrorOk).Value.Placeholder
	p2 := res2.(gen.ResultPlaceholderInfoCredentialErrorOk).Value.Placeholder
	if p1 != p2 {
		t.Errorf("placeholders should be stable for repeated calls; got %q vs %q", p1, p2)
	}
}

func TestIDFromPlaceholder(t *testing.T) {
	cases := []struct {
		in     string
		wantID string
		wantOk bool
	}{
		{PlaceholderPrefix + "abc123", "abc123", true},
		{PlaceholderPrefix + "stable-id-7", "stable-id-7", true},
		{"some-random-token", "", false},
		{"", "", false},
		{PlaceholderPrefix, "", false},
	}
	for _, c := range cases {
		gotID, gotOk := IDFromPlaceholder(c.in)
		if gotOk != c.wantOk {
			t.Errorf("IDFromPlaceholder(%q) ok = %v, want %v", c.in, gotOk, c.wantOk)
		}
		if gotID != c.wantID {
			t.Errorf("IDFromPlaceholder(%q) id = %q, want %q", c.in, gotID, c.wantID)
		}
	}
}

// -----------------------------------------------------------------------------
// Adapter — GetRaw (two-step: meta lookup + secret read)
// -----------------------------------------------------------------------------

func TestAdapter_GetRaw_Ok(t *testing.T) {
	store := &fakeStore{}
	store.putWithSecrets("i", "sec", RawMeta{},
		map[SecretRole][]byte{SecretRoleValue: []byte("the-actual-value")})
	a := newAdapter(store)

	res, _ := a.GetRaw(context.Background(), "sec")
	ok := res.(gen.ResultStringCredentialErrorOk)
	if ok.Value != "the-actual-value" {
		t.Errorf("value = %q", ok.Value)
	}
}

func TestAdapter_GetRaw_TypeMismatch(t *testing.T) {
	// Meta exists but isn't RawMeta — adapter rejects before
	// reading any slot.
	store := &fakeStore{}
	store.putWithSecrets("i", "oauth", OAuth2Meta{}, nil)
	a := newAdapter(store)

	res, _ := a.GetRaw(context.Background(), "oauth")
	errRes := res.(gen.ResultStringCredentialErrorErr)
	tm, ok := errRes.Value.(gen.CredentialErrorTypeMismatch)
	if !ok {
		t.Fatalf("got %T, want TypeMismatch", errRes.Value)
	}
	if !strings.Contains(tm.Value, "oauth2") {
		t.Errorf("message %q should mention the actual kind", tm.Value)
	}
}

func TestAdapter_GetRaw_NotConfigured_NoEntry(t *testing.T) {
	a := newAdapter(&fakeStore{})
	res, _ := a.GetRaw(context.Background(), "missing")
	errRes := res.(gen.ResultStringCredentialErrorErr)
	if _, ok := errRes.Value.(gen.CredentialErrorNotConfigured); !ok {
		t.Errorf("got %T, want NotConfigured", errRes.Value)
	}
}

func TestAdapter_GetRaw_NotConfigured_EntryButSecretEmpty(t *testing.T) {
	// Entry exists, but the SecretRoleValue hasn't been written
	// — the half-configured state. The adapter folds both
	// ErrNotFound (entry missing) and ErrSecretNotSet (secret
	// empty) into not-configured because the particle's view is
	// the same: no usable secret.
	store := &fakeStore{}
	store.putWithSecrets("i", "sec", RawMeta{}, nil) // no secrets
	a := newAdapter(store)

	res, _ := a.GetRaw(context.Background(), "sec")
	errRes := res.(gen.ResultStringCredentialErrorErr)
	if _, ok := errRes.Value.(gen.CredentialErrorNotConfigured); !ok {
		t.Errorf("got %T, want NotConfigured", errRes.Value)
	}
}

// -----------------------------------------------------------------------------
// Store scoping
// -----------------------------------------------------------------------------

// Two adapters built against two independent Stores see disjoint
// state.
func TestAdapter_ScopedByStore(t *testing.T) {
	yamlStore := &fakeStore{}
	yamlStore.putWithSecrets("y", "k", BasicMeta{Username: "yaml-user"}, nil)
	jsonStore := &fakeStore{}
	jsonStore.putWithSecrets("j", "k", BasicMeta{Username: "json-user"}, nil)

	yaml := newAdapter(yamlStore)
	json := newAdapter(jsonStore)

	res1, _ := yaml.GetPlaceholder(context.Background(), "k")
	res2, _ := json.GetPlaceholder(context.Background(), "k")

	ok1 := res1.(gen.ResultPlaceholderInfoCredentialErrorOk)
	ok2 := res2.(gen.ResultPlaceholderInfoCredentialErrorOk)
	if ok1.Value.Placeholder == ok2.Value.Placeholder {
		t.Errorf("placeholders shouldn't match across stores: %q", ok1.Value.Placeholder)
	}
}

// -----------------------------------------------------------------------------
// Metadata kinds + ApplyKind
// -----------------------------------------------------------------------------

func TestMetadataKind(t *testing.T) {
	cases := []struct {
		meta Metadata
		want Kind
	}{
		{BasicMeta{}, KindBasic},
		{OAuth2Meta{}, KindOAuth2},
		{APIKeyMeta{}, KindAPIKey},
		{SigningKeyMeta{}, KindSigningKey},
		{RawMeta{}, KindRaw},
	}
	for _, c := range cases {
		if got := c.meta.Kind(); got != c.want {
			t.Errorf("(%T).Kind() = %q, want %q", c.meta, got, c.want)
		}
	}
}

// Put with varargs is exercised against the fake store here as a
// sanity check of the test fake's behavior; the memory-store
// tests are the authoritative coverage of Put semantics.
func TestFakeStore_PutWithSecretsAtomic(t *testing.T) {
	s := &fakeStore{}
	desc, err := s.Put(context.Background(), "gh", "gh", OAuth2Meta{ClientID: "c"},
		Secret{Role: SecretRoleAccessToken, Value: []byte("at")},
		Secret{Role: SecretRoleRefreshToken, Value: []byte("rt")},
	)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	at, err := s.ReadSecret(context.Background(), desc.ID, SecretRoleAccessToken)
	if err != nil || string(at) != "at" {
		t.Errorf("access secret = %q (err=%v)", at, err)
	}
	rt, err := s.ReadSecret(context.Background(), desc.ID, SecretRoleRefreshToken)
	if err != nil || string(rt) != "rt" {
		t.Errorf("refresh secret = %q (err=%v)", rt, err)
	}
}

func TestErrSecretNotSet_WrapsErrNotFound(t *testing.T) {
	if !errors.Is(ErrSecretNotSet, ErrNotFound) {
		t.Error("ErrSecretNotSet should wrap ErrNotFound so callers using errors.Is(err, ErrNotFound) catch both cases")
	}
	// And the more specific check should still work:
	if !errors.Is(ErrSecretNotSet, ErrSecretNotSet) {
		t.Error("ErrSecretNotSet should be detectable via errors.Is")
	}
}

func TestApplyKind_String(t *testing.T) {
	cases := []struct {
		kind ApplyKind
		want string
	}{
		{ApplyBasic, "basic"},
		{ApplyBearer, "bearer"},
		{ApplyHeader, "header"},
		{ApplyAuthScheme, "auth-scheme"},
		{ApplyQueryParam, "query-param"},
		{ApplyKind(0), "ApplyKind(invalid)"},
		{ApplyKind(99), "ApplyKind(invalid)"},
	}
	for _, c := range cases {
		if got := c.kind.String(); got != c.want {
			t.Errorf("ApplyKind(%d) = %q, want %q", c.kind, got, c.want)
		}
	}
}

func TestLiftApplyKind_PanicsOnUnknown(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for unknown ApplyKind")
		}
	}()
	liftApplyKind(ApplyKind(99))
}
