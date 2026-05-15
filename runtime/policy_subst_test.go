package runtime

// Substitution tests live in `package runtime` (not _test) so they
// can drive newHTTPPolicy + httpPolicy.Do directly, skipping the
// wasm round-trip. The full-runtime tests in policy_test.go cover
// the host allow-list path end-to-end; this file is focused on the
// substitution semantics.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/partite-ai/particle/credentials"
	"github.com/partite-ai/particle/credentials/memory"
)

// recordingDoer captures the request that finally goes "outbound"
// after substitution + allow-list, so substitution tests can
// assert on the wire-bound headers + URL.
type recordingDoer struct {
	got *http.Request
}

func (d *recordingDoer) Do(req *http.Request) (*http.Response, error) {
	// Snapshot the request state — the test inspects this.
	d.got = req
	rec := httptest.NewRecorder()
	rec.WriteString("ok")
	return rec.Result(), nil
}

// newPolicyWithStore builds an httpPolicy that allows a single
// host (so substitution can be exercised without tripping the
// allow-list), backed by a memory.Store. The variadic `declared`
// argument names the credentials the policy should treat as
// manifest-declared — substitution only ever attempts those.
//
// Returns the policy + store + recording doer.
func newPolicyWithStore(t *testing.T, particle string, declared ...string) (*httpPolicy, *memory.Store, *recordingDoer) {
	t.Helper()
	store := memory.New()
	rec := &recordingDoer{}
	pol := newHTTPPolicy(true, []string{"upstream.test"}, rec, store, particle, declared, nil, nil)
	return pol, store, rec
}

func mustReq(t *testing.T, method, url string, header http.Header) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return req
}

// -----------------------------------------------------------------------------
// Basic — "Authorization: Basic <placeholder>" → base64(user:pass)
// -----------------------------------------------------------------------------

func TestSubstitute_Basic_ReplacesAuthorization(t *testing.T) {
	pol, store, rec := newPolicyWithStore(t, "yaml-tools", "db")
	desc, _ := store.Put(context.Background(), "yaml-tools", "db", "db",
		credentials.BasicMeta{Username: "alice"},
		credentials.Secret{Role: credentials.SecretRolePassword, Value: []byte("hunter2")},
	)
	placeholder := credentials.PlaceholderPrefix + desc.ID

	req := mustReq(t, "GET", "https://upstream.test/", http.Header{
		"Authorization": {"Basic " + placeholder},
	})
	if _, err := pol.Do(req); err != nil {
		t.Fatalf("Do: %v", err)
	}
	got := rec.got.Header.Get("Authorization")
	// "alice:hunter2" → base64
	if got != "Basic YWxpY2U6aHVudGVyMg==" {
		t.Errorf("Authorization = %q", got)
	}
}

// A Basic placeholder that appears at the WRONG location (not in
// the Authorization header in the expected `Basic <ph>` shape)
// must NOT be substituted. This is the design's defense against
// exfiltration — see docs/initial-design.md §7
// "Targeted substitution".
func TestSubstitute_Basic_WrongLocation_NotReplaced(t *testing.T) {
	pol, store, rec := newPolicyWithStore(t, "p", "db", "gh", "k", "sig")
	desc, _ := store.Put(context.Background(), "p", "db", "db",
		credentials.BasicMeta{Username: "u"},
		credentials.Secret{Role: credentials.SecretRolePassword, Value: []byte("v")},
	)
	placeholder := credentials.PlaceholderPrefix + desc.ID

	req := mustReq(t, "GET", "https://upstream.test/", http.Header{
		// Attacker's exfil header — must transmit the literal
		// placeholder, never the real credential.
		"X-Steal-Token": {placeholder},
	})
	if _, err := pol.Do(req); err != nil {
		t.Fatal(err)
	}
	if got := rec.got.Header.Get("X-Steal-Token"); got != placeholder {
		t.Errorf("X-Steal-Token = %q, expected literal placeholder", got)
	}
	if got := rec.got.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, should be empty", got)
	}
}

// -----------------------------------------------------------------------------
// Bearer / OAuth2 — "Authorization: Bearer <placeholder>" → access token
// -----------------------------------------------------------------------------

func TestSubstitute_OAuth2_ReplacesAccessToken(t *testing.T) {
	pol, store, rec := newPolicyWithStore(t, "p", "db", "gh", "k", "sig")
	bundle := credentials.AccessToken{Token: "real-access-token"}.Marshal()
	desc, _ := store.Put(context.Background(), "p", "gh", "gh",
		credentials.OAuth2Meta{ClientID: "c"},
		credentials.Secret{Role: credentials.SecretRoleAccessToken, Value: bundle},
	)
	placeholder := credentials.PlaceholderPrefix + desc.ID

	req := mustReq(t, "GET", "https://upstream.test/", http.Header{
		"Authorization": {"Bearer " + placeholder},
	})
	if _, err := pol.Do(req); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got := rec.got.Header.Get("Authorization"); got != "Bearer real-access-token" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestSubstitute_OAuth2_WrongScheme_NotReplaced(t *testing.T) {
	// `Basic <bearer-placeholder>` doesn't match the OAuth2
	// substitution rule (`Bearer <ph>`), so the placeholder
	// transmits literally.
	pol, store, rec := newPolicyWithStore(t, "p", "db", "gh", "k", "sig")
	bundle := credentials.AccessToken{Token: "tok"}.Marshal()
	desc, _ := store.Put(context.Background(), "p", "gh", "gh",
		credentials.OAuth2Meta{},
		credentials.Secret{Role: credentials.SecretRoleAccessToken, Value: bundle},
	)
	placeholder := credentials.PlaceholderPrefix + desc.ID

	req := mustReq(t, "GET", "https://upstream.test/", http.Header{
		"Authorization": {"Basic " + placeholder},
	})
	if _, err := pol.Do(req); err != nil {
		t.Fatal(err)
	}
	if got := rec.got.Header.Get("Authorization"); got != "Basic "+placeholder {
		t.Errorf("Authorization = %q, expected literal placeholder", got)
	}
}

// A token within tokenSkew of expiry gets refreshed in place
// before substitution. The fresh token's value reaches the wire.
func TestSubstitute_OAuth2_ExpiredToken_ProactivelyRefreshed(t *testing.T) {
	store := memory.New()
	rec := &recordingDoer{}

	stale := credentials.AccessToken{
		Token:     "stale-token",
		ExpiresAt: time.Now().Add(-1 * time.Minute), // already expired
	}.Marshal()
	desc, _ := store.Put(context.Background(), "p", "gh", "gh",
		credentials.OAuth2Meta{TokenURL: "https://provider.test/token"},
		credentials.Secret{Role: credentials.SecretRoleAccessToken, Value: stale},
	)
	placeholder := credentials.PlaceholderPrefix + desc.ID

	refreshCalls := 0
	refresh := func(_ context.Context, id string) (credentials.AccessToken, error) {
		refreshCalls++
		if id != desc.ID {
			t.Errorf("refresh called with id %q, want %q", id, desc.ID)
		}
		return credentials.AccessToken{
			Token:     "fresh-token",
			ExpiresAt: time.Now().Add(1 * time.Hour),
		}, nil
	}
	pol := newHTTPPolicy(true, []string{"upstream.test"}, rec, store, "p", []string{"gh"}, nil, refresh)

	req := mustReq(t, "GET", "https://upstream.test/", http.Header{
		"Authorization": {"Bearer " + placeholder},
	})
	if _, err := pol.Do(req); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if refreshCalls != 1 {
		t.Errorf("refresh called %d times, want 1", refreshCalls)
	}
	if got := rec.got.Header.Get("Authorization"); got != "Bearer fresh-token" {
		t.Errorf("Authorization = %q, want fresh-token", got)
	}
}

// A token with plenty of life left doesn't trigger a refresh —
// we don't waste valid lifetime on early rotation.
func TestSubstitute_OAuth2_FreshToken_NoRefresh(t *testing.T) {
	store := memory.New()
	rec := &recordingDoer{}

	bundle := credentials.AccessToken{
		Token:     "still-good",
		ExpiresAt: time.Now().Add(1 * time.Hour),
	}.Marshal()
	desc, _ := store.Put(context.Background(), "p", "gh", "gh",
		credentials.OAuth2Meta{},
		credentials.Secret{Role: credentials.SecretRoleAccessToken, Value: bundle},
	)
	placeholder := credentials.PlaceholderPrefix + desc.ID

	refresh := func(context.Context, string) (credentials.AccessToken, error) {
		t.Fatal("refresh must not be called for a token outside the skew window")
		return credentials.AccessToken{}, nil
	}
	pol := newHTTPPolicy(true, []string{"upstream.test"}, rec, store, "p", []string{"gh"}, nil, refresh)

	req := mustReq(t, "GET", "https://upstream.test/", http.Header{
		"Authorization": {"Bearer " + placeholder},
	})
	if _, err := pol.Do(req); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got := rec.got.Header.Get("Authorization"); got != "Bearer still-good" {
		t.Errorf("Authorization = %q", got)
	}
}

// When proactive refresh fails — refresher down, network error,
// store rejected the write — we fall through and substitute the
// stale token rather than failing the request. The upstream's
// 401 (if any) is the particle's signal to call oauth.refresh()
// and retry; failing here would block requests that might still
// have worked (provider clock skew, etc).
func TestSubstitute_OAuth2_RefreshFailure_FallsBackToStale(t *testing.T) {
	store := memory.New()
	rec := &recordingDoer{}

	stale := credentials.AccessToken{
		Token:     "stale-token",
		ExpiresAt: time.Now().Add(-1 * time.Minute),
	}.Marshal()
	desc, _ := store.Put(context.Background(), "p", "gh", "gh",
		credentials.OAuth2Meta{},
		credentials.Secret{Role: credentials.SecretRoleAccessToken, Value: stale},
	)
	placeholder := credentials.PlaceholderPrefix + desc.ID

	refresh := func(context.Context, string) (credentials.AccessToken, error) {
		return credentials.AccessToken{}, errors.New("provider unreachable")
	}
	pol := newHTTPPolicy(true, []string{"upstream.test"}, rec, store, "p", []string{"gh"}, nil, refresh)

	req := mustReq(t, "GET", "https://upstream.test/", http.Header{
		"Authorization": {"Bearer " + placeholder},
	})
	if _, err := pol.Do(req); err != nil {
		t.Fatalf("Do: %v (should have substituted stale token, not failed)", err)
	}
	if got := rec.got.Header.Get("Authorization"); got != "Bearer stale-token" {
		t.Errorf("Authorization = %q, want stale-token fallback", got)
	}
}

// A bundle with ExpiresAt zero (provider didn't supply one) is
// treated as "we have no signal" — no proactive refresh fires,
// and refresh-on-401 in the particle remains the only path.
func TestSubstitute_OAuth2_ZeroExpiresAt_NoRefresh(t *testing.T) {
	store := memory.New()
	rec := &recordingDoer{}

	bundle := credentials.AccessToken{Token: "no-expiry"}.Marshal() // ExpiresAt is zero
	desc, _ := store.Put(context.Background(), "p", "gh", "gh",
		credentials.OAuth2Meta{},
		credentials.Secret{Role: credentials.SecretRoleAccessToken, Value: bundle},
	)
	placeholder := credentials.PlaceholderPrefix + desc.ID

	refresh := func(context.Context, string) (credentials.AccessToken, error) {
		t.Fatal("refresh must not fire when ExpiresAt is zero")
		return credentials.AccessToken{}, nil
	}
	pol := newHTTPPolicy(true, []string{"upstream.test"}, rec, store, "p", []string{"gh"}, nil, refresh)

	req := mustReq(t, "GET", "https://upstream.test/", http.Header{
		"Authorization": {"Bearer " + placeholder},
	})
	if _, err := pol.Do(req); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got := rec.got.Header.Get("Authorization"); got != "Bearer no-expiry" {
		t.Errorf("Authorization = %q", got)
	}
}

// -----------------------------------------------------------------------------
// APIKey — depends on the location chosen at setup
// -----------------------------------------------------------------------------

func TestSubstitute_APIKey_Header(t *testing.T) {
	pol, store, rec := newPolicyWithStore(t, "p", "db", "gh", "k", "sig")
	desc, _ := store.Put(context.Background(), "p", "k", "k",
		credentials.APIKeyMeta{Location: credentials.ApplySpec{
			Kind: credentials.ApplyHeader, Name: "X-API-Key",
		}},
		credentials.Secret{Role: credentials.SecretRoleKey, Value: []byte("the-real-key")},
	)
	placeholder := credentials.PlaceholderPrefix + desc.ID

	req := mustReq(t, "GET", "https://upstream.test/", http.Header{
		"X-API-Key": {placeholder},
	})
	if _, err := pol.Do(req); err != nil {
		t.Fatal(err)
	}
	if got := rec.got.Header.Get("X-API-Key"); got != "the-real-key" {
		t.Errorf("X-API-Key = %q", got)
	}
}

func TestSubstitute_APIKey_Header_WrongName_NotReplaced(t *testing.T) {
	// APIKey with location {Header, "X-API-Key"} should not
	// substitute when the placeholder appears in any other
	// header.
	pol, store, rec := newPolicyWithStore(t, "p", "db", "gh", "k", "sig")
	desc, _ := store.Put(context.Background(), "p", "k", "k",
		credentials.APIKeyMeta{Location: credentials.ApplySpec{
			Kind: credentials.ApplyHeader, Name: "X-API-Key",
		}},
		credentials.Secret{Role: credentials.SecretRoleKey, Value: []byte("k")},
	)
	placeholder := credentials.PlaceholderPrefix + desc.ID

	req := mustReq(t, "GET", "https://upstream.test/", http.Header{
		"X-Other-Header": {placeholder},
	})
	if _, err := pol.Do(req); err != nil {
		t.Fatal(err)
	}
	if got := rec.got.Header.Get("X-Other-Header"); got != placeholder {
		t.Errorf("X-Other-Header = %q, expected literal", got)
	}
}

func TestSubstitute_APIKey_QueryParam(t *testing.T) {
	pol, store, rec := newPolicyWithStore(t, "p", "db", "gh", "k", "sig")
	desc, _ := store.Put(context.Background(), "p", "k", "k",
		credentials.APIKeyMeta{Location: credentials.ApplySpec{
			Kind: credentials.ApplyQueryParam, Name: "api_key",
		}},
		credentials.Secret{Role: credentials.SecretRoleKey, Value: []byte("realkey")},
	)
	placeholder := credentials.PlaceholderPrefix + desc.ID

	req := mustReq(t, "GET", "https://upstream.test/path?other=v&api_key="+placeholder, nil)
	if _, err := pol.Do(req); err != nil {
		t.Fatal(err)
	}
	if got := rec.got.URL.Query().Get("api_key"); got != "realkey" {
		t.Errorf("api_key = %q", got)
	}
	if got := rec.got.URL.Query().Get("other"); got != "v" {
		t.Errorf("other = %q (should be untouched)", got)
	}
}

func TestSubstitute_APIKey_AuthScheme(t *testing.T) {
	pol, store, rec := newPolicyWithStore(t, "p", "db", "gh", "k", "sig")
	desc, _ := store.Put(context.Background(), "p", "k", "k",
		credentials.APIKeyMeta{Location: credentials.ApplySpec{
			Kind: credentials.ApplyAuthScheme, Scheme: "Token",
		}},
		credentials.Secret{Role: credentials.SecretRoleKey, Value: []byte("realkey")},
	)
	placeholder := credentials.PlaceholderPrefix + desc.ID

	req := mustReq(t, "GET", "https://upstream.test/", http.Header{
		"Authorization": {"Token " + placeholder},
	})
	if _, err := pol.Do(req); err != nil {
		t.Fatal(err)
	}
	if got := rec.got.Header.Get("Authorization"); got != "Token realkey" {
		t.Errorf("Authorization = %q", got)
	}
}

// -----------------------------------------------------------------------------
// Negative paths
// -----------------------------------------------------------------------------

// SigningKey and Raw kinds don't participate in HTTP substitution.
// A placeholder for either kind transmits literally.
func TestSubstitute_SigningKey_LeavesPlaceholder(t *testing.T) {
	pol, store, rec := newPolicyWithStore(t, "p", "db", "gh", "k", "sig")
	desc, _ := store.Put(context.Background(), "p", "sig", "sig",
		credentials.SigningKeyMeta{Algorithm: credentials.AlgorithmHMACSHA256},
		credentials.Secret{Role: credentials.SecretRoleKey, Value: []byte("k")},
	)
	placeholder := credentials.PlaceholderPrefix + desc.ID

	req := mustReq(t, "GET", "https://upstream.test/", http.Header{
		"Authorization": {"Bearer " + placeholder},
	})
	if _, err := pol.Do(req); err != nil {
		t.Fatal(err)
	}
	if got := rec.got.Header.Get("Authorization"); got != "Bearer "+placeholder {
		t.Errorf("Authorization = %q, expected literal placeholder", got)
	}
}

// A placeholder whose ID isn't in the Store is not in the
// particle's namespace — leave it alone (transmitting it gives
// the upstream a useless string).
func TestSubstitute_UnknownID_LeavesPlaceholder(t *testing.T) {
	pol, _, rec := newPolicyWithStore(t, "p", "db", "gh", "k", "sig")
	placeholder := credentials.PlaceholderPrefix + "fabricated-id-not-in-store"

	req := mustReq(t, "GET", "https://upstream.test/", http.Header{
		"Authorization": {"Bearer " + placeholder},
	})
	if _, err := pol.Do(req); err != nil {
		t.Fatal(err)
	}
	if got := rec.got.Header.Get("Authorization"); got != "Bearer "+placeholder {
		t.Errorf("Authorization = %q", got)
	}
}

// Spec-driven enforcement: the Store has a real credential, but
// the manifest doesn't declare it. A placeholder for that
// credential — even with the right ID and at the right location —
// must NOT be substituted. The manifest is the source of truth.
func TestSubstitute_UndeclaredInManifest_LeavesPlaceholder(t *testing.T) {
	// declared = []string{"db"}; the credential we'll put under
	// "smuggled" is NOT in the declared list.
	pol, store, rec := newPolicyWithStore(t, "p", "db")

	// Put a real Bearer credential under a name the manifest
	// doesn't declare.
	bundle := credentials.AccessToken{Token: "secret-token"}.Marshal()
	desc, _ := store.Put(context.Background(), "p", "smuggled", "smuggled",
		credentials.OAuth2Meta{},
		credentials.Secret{Role: credentials.SecretRoleAccessToken, Value: bundle},
	)
	placeholder := credentials.PlaceholderPrefix + desc.ID

	req := mustReq(t, "GET", "https://upstream.test/", http.Header{
		"Authorization": {"Bearer " + placeholder},
	})
	if _, err := pol.Do(req); err != nil {
		t.Fatal(err)
	}
	if got := rec.got.Header.Get("Authorization"); got != "Bearer "+placeholder {
		t.Errorf("Authorization = %q, expected literal placeholder (credential not declared in manifest)", got)
	}
}

// When a manifest declares multiple credential methods but only
// one is configured (the steady-state invariant — Put enforces
// "one credential per particle"), a request carrying placeholders
// for both methods has the CONFIGURED one substituted and the
// declared-but-unconfigured placeholder passed through literally.
// Catches a class of "selected method got dropped from
// substitution because the policy iterated by declaration order"
// regressions.
func TestSubstitute_OneOfMultipleDeclared_Substituted(t *testing.T) {
	pol, store, rec := newPolicyWithStore(t, "p", "key", "gh")

	// Only "gh" is configured. The user picked OAuth.
	oauth, _ := store.Put(context.Background(), "p", "gh", "gh",
		credentials.OAuth2Meta{},
		credentials.Secret{Role: credentials.SecretRoleAccessToken, Value: credentials.AccessToken{Token: "oauth-token"}.Marshal()},
	)
	oauthPH := credentials.PlaceholderPrefix + oauth.ID

	// Plant placeholders for both. "key" wasn't configured, so
	// its descriptor doesn't exist — anything in X-API-Key looks
	// like an opaque value to the substituter and stays literal.
	const apikeyPH = credentials.PlaceholderPrefix + "no-such-id-aaaaaaaaaaaaaaaa"
	req := mustReq(t, "GET", "https://upstream.test/", http.Header{
		"X-API-Key":     {apikeyPH},
		"Authorization": {"Bearer " + oauthPH},
	})
	if _, err := pol.Do(req); err != nil {
		t.Fatal(err)
	}
	if got := rec.got.Header.Get("Authorization"); got != "Bearer oauth-token" {
		t.Errorf("Authorization = %q, want substituted", got)
	}
	if got := rec.got.Header.Get("X-API-Key"); got != apikeyPH {
		t.Errorf("X-API-Key = %q, want unchanged (no configured credential)", got)
	}
}

// -----------------------------------------------------------------------------
// Substitution error paths
// -----------------------------------------------------------------------------

// The metadata is set but the secret slot is empty — substitution
// can't proceed; the request fails with a clear error rather than
// going on the wire with an empty credential.
func TestSubstitute_SecretMissing_ReturnsError(t *testing.T) {
	pol, store, rec := newPolicyWithStore(t, "p", "db", "gh", "k", "sig")
	desc, _ := store.Put(context.Background(), "p", "db", "db",
		credentials.BasicMeta{Username: "alice"},
		// no SecretRolePassword written
	)
	placeholder := credentials.PlaceholderPrefix + desc.ID

	req := mustReq(t, "GET", "https://upstream.test/", http.Header{
		"Authorization": {"Basic " + placeholder},
	})
	_, err := pol.Do(req)
	if err == nil {
		t.Fatal("expected error when password secret is missing")
	}
	if !strings.Contains(err.Error(), "password") {
		t.Errorf("error %q should mention the missing secret role", err)
	}
	if rec.got != nil {
		t.Error("inner doer should not be called when substitution fails")
	}
}

// Host allow-list check runs BEFORE substitution, so a request to
// a denied host is rejected without the inner doer being called and
// — though we don't assert it directly here — without the Store
// being touched. The placeholder is still present in the request
// the caller built, but it never reaches the wire.
func TestSubstitute_DeniedHost_ShortCircuitsBeforeSubstitution(t *testing.T) {
	store := memory.New()
	rec := &recordingDoer{}
	pol := newHTTPPolicy(true, []string{"only.allowed.test"}, rec, store, "p", []string{"k"}, nil, nil)

	desc, _ := store.Put(context.Background(), "p", "k", "k",
		credentials.APIKeyMeta{Location: credentials.ApplySpec{
			Kind: credentials.ApplyHeader, Name: "X-API-Key",
		}},
		credentials.Secret{Role: credentials.SecretRoleKey, Value: []byte("real")},
	)
	placeholder := credentials.PlaceholderPrefix + desc.ID

	req := mustReq(t, "GET", "https://other-host.test/", http.Header{
		"X-API-Key": {placeholder},
	})
	_, err := pol.Do(req)
	if err == nil {
		t.Fatal("expected host-not-allowed error")
	}
	var hae *HostNotAllowedError
	if !errors.As(err, &hae) {
		t.Fatalf("error = %v, want *HostNotAllowedError", err)
	}
	if rec.got != nil {
		t.Error("inner doer should not be called when host is denied")
	}
}
