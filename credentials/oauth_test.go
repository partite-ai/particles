package credentials

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	gen "github.com/partite-ai/particles/internal/host/gen/particle/host/oauth"
)

// fakeRefresher is the in-test OAuthRefresher. The closure shape
// lets each test specify exactly what the upstream "returns".
type fakeRefresher struct {
	called int
	got    RefreshRequest
	fn     func(ctx context.Context, req RefreshRequest) (RefreshResponse, error)
}

func (f *fakeRefresher) Refresh(ctx context.Context, req RefreshRequest) (RefreshResponse, error) {
	f.called++
	f.got = req
	if f.fn != nil {
		return f.fn(ctx, req)
	}
	return RefreshResponse{}, errors.New("fakeRefresher: no fn configured")
}

// -----------------------------------------------------------------------------
// Adapter behavior
// -----------------------------------------------------------------------------

func TestOAuthAdapter_Refresh_HappyPath(t *testing.T) {
	store := &fakeStore{}
	originalMeta := OAuth2Meta{
		ClientID: "client-1",
		TokenURL: "https://example.test/token",
		Scopes:   []string{"repo"},
	}
	// Existing access token bundle (will be replaced).
	prev := AccessToken{Token: "at-old", Type: "Bearer", ExpiresAt: time.Unix(1_000, 0)}.Marshal()
	store.putWithSecrets("id-gh", "github_oauth", originalMeta,
		map[SecretRole][]byte{
			SecretRoleAccessToken:  prev,
			SecretRoleRefreshToken: []byte("rt-old"),
			SecretRoleClientSecret: []byte("cs"),
		})

	newExpiry := time.Unix(2_000, 0)
	rf := &fakeRefresher{
		fn: func(_ context.Context, req RefreshRequest) (RefreshResponse, error) {
			if string(req.RefreshToken) != "rt-old" {
				t.Errorf("refresher saw refresh token %q, want rt-old", req.RefreshToken)
			}
			if string(req.ClientSecret) != "cs" {
				t.Errorf("refresher saw client secret %q, want cs", req.ClientSecret)
			}
			return RefreshResponse{
				AccessToken: []byte("at-new"),
				ExpiresAt:   newExpiry,
				TokenType:   "Bearer",
			}, nil
		},
	}

	a := newOAuthAdapter(store, rf, nil)
	res, err := a.Refresh(context.Background(), "github_oauth")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if _, ok := res.(gen.Result_OauthErrorOk); !ok {
		t.Fatalf("got %T, want Ok", res)
	}

	// Access token rotated, refresh token preserved (provider
	// didn't return one), client secret untouched.
	atBytes, _ := store.ReadSecret(context.Background(), "id-gh", SecretRoleAccessToken)
	at, err := UnmarshalAccessToken(atBytes)
	if err != nil {
		t.Fatalf("UnmarshalAccessToken: %v", err)
	}
	if at.Token != "at-new" {
		t.Errorf("access bundle Token = %q, want at-new", at.Token)
	}
	if at.Type != "Bearer" {
		t.Errorf("access bundle Type = %q, want Bearer", at.Type)
	}
	if !at.ExpiresAt.Equal(newExpiry) {
		t.Errorf("access bundle ExpiresAt = %v, want %v", at.ExpiresAt, newExpiry)
	}

	rt, _ := store.ReadSecret(context.Background(), "id-gh", SecretRoleRefreshToken)
	if string(rt) != "rt-old" {
		t.Errorf("refresh secret = %q, want rt-old (preserved)", rt)
	}
	cs, _ := store.ReadSecret(context.Background(), "id-gh", SecretRoleClientSecret)
	if string(cs) != "cs" {
		t.Errorf("client secret = %q, want cs (preserved)", cs)
	}

	// Crucially: metadata is unchanged. Refresh writes only secrets.
	desc, _ := store.GetByName(context.Background(), "github_oauth")
	if !reflect.DeepEqual(desc.Meta, originalMeta) {
		t.Errorf("metadata mutated by refresh:\n got: %+v\nwant: %+v", desc.Meta, originalMeta)
	}
}

func TestOAuthAdapter_Refresh_RotatesRefreshToken(t *testing.T) {
	// When the provider returns a new refresh token (Okta,
	// Auth0 with rotation, etc.), the adapter writes it.
	store := &fakeStore{}
	store.putWithSecrets("id-1", "x",
		OAuth2Meta{TokenURL: "https://example.test/token"},
		map[SecretRole][]byte{
			SecretRoleAccessToken:  []byte("at-old"),
			SecretRoleRefreshToken: []byte("rt-old"),
		},
	)

	rf := &fakeRefresher{
		fn: func(_ context.Context, _ RefreshRequest) (RefreshResponse, error) {
			return RefreshResponse{
				AccessToken:  []byte("at-new"),
				RefreshToken: []byte("rt-new"),
				ExpiresAt:    time.Now().Add(time.Hour),
			}, nil
		},
	}
	a := newOAuthAdapter(store, rf, nil)
	if _, err := a.Refresh(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	rt, _ := store.ReadSecret(context.Background(), "id-1", SecretRoleRefreshToken)
	if string(rt) != "rt-new" {
		t.Errorf("refresh secret = %q, want rt-new", rt)
	}
}

func TestOAuthAdapter_Refresh_NotConfigured_NoEntry(t *testing.T) {
	a := newOAuthAdapter(&fakeStore{}, &fakeRefresher{}, nil)
	res, _ := a.Refresh(context.Background(), "missing")
	errRes := res.(gen.Result_OauthErrorErr)
	if _, ok := errRes.Value.(gen.OauthErrorNotConfigured); !ok {
		t.Errorf("got %T, want NotConfigured", errRes.Value)
	}
}

func TestOAuthAdapter_Refresh_NotOAuth(t *testing.T) {
	// Entry exists but isn't OAuth2 → not-oauth.
	store := &fakeStore{}
	store.putWithSecrets("i", "x", APIKeyMeta{Location: ApplySpec{Kind: ApplyHeader, Name: "X-API"}}, nil)
	a := newOAuthAdapter(store, &fakeRefresher{}, nil)
	res, _ := a.Refresh(context.Background(), "x")
	errRes := res.(gen.Result_OauthErrorErr)
	if _, ok := errRes.Value.(gen.OauthErrorNotOauth); !ok {
		t.Errorf("got %T, want NotOauth", errRes.Value)
	}
}

func TestOAuthAdapter_Refresh_NotConfigured_RefreshTokenMissing(t *testing.T) {
	store := &fakeStore{}
	store.putWithSecrets("i", "x", OAuth2Meta{TokenURL: "https://x"}, nil) // no refresh token slot
	a := newOAuthAdapter(store, &fakeRefresher{}, nil)
	res, _ := a.Refresh(context.Background(), "x")
	errRes := res.(gen.Result_OauthErrorErr)
	if _, ok := errRes.Value.(gen.OauthErrorNotConfigured); !ok {
		t.Errorf("got %T, want NotConfigured", errRes.Value)
	}
}

func TestOAuthAdapter_Refresh_RefreshFailed_UpstreamError(t *testing.T) {
	store := &fakeStore{}
	store.putWithSecrets("i", "x",
		OAuth2Meta{TokenURL: "https://x"},
		map[SecretRole][]byte{SecretRoleRefreshToken: []byte("rt")},
	)
	rf := &fakeRefresher{
		fn: func(_ context.Context, _ RefreshRequest) (RefreshResponse, error) {
			return RefreshResponse{}, errors.New("server returned 500")
		},
	}
	a := newOAuthAdapter(store, rf, nil)
	res, _ := a.Refresh(context.Background(), "x")
	errRes := res.(gen.Result_OauthErrorErr)
	rf2, ok := errRes.Value.(gen.OauthErrorRefreshFailed)
	if !ok {
		t.Fatalf("got %T, want RefreshFailed", errRes.Value)
	}
	if !strings.Contains(rf2.Value, "500") {
		t.Errorf("message = %q, should mention 500", rf2.Value)
	}
}

func TestOAuthAdapter_Refresh_RefreshFailed_EmptyAccessToken(t *testing.T) {
	store := &fakeStore{}
	store.putWithSecrets("i", "x",
		OAuth2Meta{TokenURL: "https://x"},
		map[SecretRole][]byte{SecretRoleRefreshToken: []byte("rt")},
	)
	rf := &fakeRefresher{
		fn: func(_ context.Context, _ RefreshRequest) (RefreshResponse, error) {
			return RefreshResponse{}, nil // empty AccessToken
		},
	}
	a := newOAuthAdapter(store, rf, nil)
	res, _ := a.Refresh(context.Background(), "x")
	errRes := res.(gen.Result_OauthErrorErr)
	if _, ok := errRes.Value.(gen.OauthErrorRefreshFailed); !ok {
		t.Errorf("got %T, want RefreshFailed", errRes.Value)
	}
}

// PKCE flow: no client secret stored. Refresher should see empty
// ClientSecret without an error.
func TestOAuthAdapter_Refresh_PKCE_NoClientSecret(t *testing.T) {
	store := &fakeStore{}
	store.putWithSecrets("i", "x",
		OAuth2Meta{TokenURL: "https://x", Flow: "authorization-code-pkce"},
		map[SecretRole][]byte{SecretRoleRefreshToken: []byte("rt")},
	)
	var sawSecret []byte
	rf := &fakeRefresher{
		fn: func(_ context.Context, req RefreshRequest) (RefreshResponse, error) {
			sawSecret = req.ClientSecret
			return RefreshResponse{
				AccessToken: []byte("at"),
				ExpiresAt:   time.Now().Add(time.Hour),
			}, nil
		},
	}
	a := newOAuthAdapter(store, rf, nil)
	if _, err := a.Refresh(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if len(sawSecret) != 0 {
		t.Errorf("PKCE refresher received client secret %q, want empty", sawSecret)
	}
}

// -----------------------------------------------------------------------------
// HTTPRefresher
// -----------------------------------------------------------------------------

func TestHTTPRefresher_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q", got)
		}
		_ = r.ParseForm()
		if got := r.Form.Get("grant_type"); got != "refresh_token" {
			t.Errorf("grant_type = %q", got)
		}
		if got := r.Form.Get("refresh_token"); got != "rt" {
			t.Errorf("refresh_token = %q", got)
		}
		if got := r.Form.Get("client_id"); got != "cid" {
			t.Errorf("client_id = %q", got)
		}
		if got := r.Form.Get("client_secret"); got != "cs" {
			t.Errorf("client_secret = %q", got)
		}
		if got := r.Form.Get("scope"); got != "a b" {
			t.Errorf("scope = %q", got)
		}
		fmt.Fprintln(w, `{"access_token":"new-at","refresh_token":"new-rt","token_type":"Bearer","expires_in":3600}`)
	}))
	defer srv.Close()

	r := &HTTPRefresher{}
	resp, err := r.Refresh(context.Background(), RefreshRequest{
		Meta:         OAuth2Meta{TokenURL: srv.URL, ClientID: "cid", Scopes: []string{"a", "b"}},
		ClientSecret: []byte("cs"),
		RefreshToken: []byte("rt"),
	})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if string(resp.AccessToken) != "new-at" {
		t.Errorf("AccessToken = %q", resp.AccessToken)
	}
	if string(resp.RefreshToken) != "new-rt" {
		t.Errorf("RefreshToken = %q", resp.RefreshToken)
	}
	if resp.TokenType != "Bearer" {
		t.Errorf("TokenType = %q", resp.TokenType)
	}
	if resp.ExpiresAt.IsZero() {
		t.Error("ExpiresAt is zero, want bumped")
	}
}

// PKCE clients omit client_secret from the body.
func TestHTTPRefresher_PKCE_OmitsClientSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Has("client_secret") {
			t.Errorf("client_secret should not be sent in PKCE flow; form = %v", r.Form)
		}
		fmt.Fprintln(w, `{"access_token":"at","expires_in":600}`)
	}))
	defer srv.Close()

	r := &HTTPRefresher{}
	if _, err := r.Refresh(context.Background(), RefreshRequest{
		Meta:         OAuth2Meta{TokenURL: srv.URL, ClientID: "cid"},
		RefreshToken: []byte("rt"),
		// ClientSecret intentionally empty
	}); err != nil {
		t.Fatal(err)
	}
}

// `expires_in` may be a JSON number or a string-encoded integer.
func TestHTTPRefresher_ExpiresInString(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, `{"access_token":"at","expires_in":"3600"}`)
	}))
	defer srv.Close()
	r := &HTTPRefresher{}
	resp, err := r.Refresh(context.Background(), RefreshRequest{
		Meta:         OAuth2Meta{TokenURL: srv.URL},
		RefreshToken: []byte("rt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ExpiresAt.IsZero() {
		t.Error("ExpiresAt should be set when expires_in is a string-encoded number")
	}
}

func TestHTTPRefresher_ErrorBody_RFCShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintln(w, `{"error":"invalid_grant","error_description":"refresh token is invalid"}`)
	}))
	defer srv.Close()
	r := &HTTPRefresher{}
	_, err := r.Refresh(context.Background(), RefreshRequest{
		Meta:         OAuth2Meta{TokenURL: srv.URL},
		RefreshToken: []byte("rt"),
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "invalid_grant") || !strings.Contains(err.Error(), "refresh token is invalid") {
		t.Errorf("error %q should embed both error and error_description", err)
	}
}

// A token endpoint whose host isn't in the per-particle policy
// must be refused before the refresh_token goes on the wire.
func TestHTTPRefresher_PolicyRejectsTokenURL(t *testing.T) {
	r := &HTTPRefresher{}
	_, err := r.Refresh(context.Background(), RefreshRequest{
		Meta:         OAuth2Meta{TokenURL: "https://evil.example/token"},
		RefreshToken: []byte("rt"),
		Policy:       allowHostsPolicy{"good.example": {}},
	})
	if err == nil {
		t.Fatal("expected error when TokenURL host is not in allowedHosts")
	}
	if !strings.Contains(err.Error(), "not in capabilities.http.allowedHosts") {
		t.Errorf("err = %v, want the allowedHosts denial message", err)
	}
}

// A redirect from the token endpoint to a host the policy denies
// must not be followed — the refresh_token (and client_secret,
// for confidential clients) must not be re-POSTed to whatever
// Location says. httptest binds 127.0.0.1 so both servers share
// a hostname; the test uses a CheckRedirect that denies every hop
// to prove the policy callback is wired into the http.Client.
func TestHTTPRefresher_PolicyRejectsRedirectHop(t *testing.T) {
	var evilHits int
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		evilHits++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"access_token":"hijacked"}`)
	}))
	defer evil.Close()

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL, http.StatusFound)
	}))
	defer tokenSrv.Close()

	_, err := (&HTTPRefresher{}).Refresh(context.Background(), RefreshRequest{
		Meta:         OAuth2Meta{TokenURL: tokenSrv.URL},
		RefreshToken: []byte("rt"),
		Policy:       alwaysDenyRedirectPolicy{},
	})
	if err == nil {
		t.Fatal("expected error when redirect hop is denied by policy")
	}
	if evilHits != 0 {
		t.Fatalf("redirect target was reached %d time(s) — refresh_token may have been exfiltrated", evilHits)
	}
}

// allowHostsPolicy is a minimal EgressPolicy keyed by hostname.
// Used by tests so we don't depend on the runtime package's
// HTTPPolicy.
type allowHostsPolicy map[string]struct{}

func (a allowHostsPolicy) AllowsHost(host string) bool {
	_, ok := a[strings.ToLower(host)]
	return ok
}
func (a allowHostsPolicy) CheckRedirect(req *http.Request, _ []*http.Request) error {
	if !a.AllowsHost(req.URL.Hostname()) {
		return fmt.Errorf("disallowed redirect to %q", req.URL.Hostname())
	}
	return nil
}

// alwaysDenyRedirectPolicy approves the first-hop host check but
// refuses every redirect. Lets a test prove the CheckRedirect is
// actually wired into the http.Client used by the refresher.
type alwaysDenyRedirectPolicy struct{}

func (alwaysDenyRedirectPolicy) AllowsHost(string) bool { return true }
func (alwaysDenyRedirectPolicy) CheckRedirect(req *http.Request, _ []*http.Request) error {
	return fmt.Errorf("redirect to %q denied by test policy", req.URL.Hostname())
}

func TestHTTPRefresher_RejectsEmptyTokenURL(t *testing.T) {
	r := &HTTPRefresher{}
	_, err := r.Refresh(context.Background(), RefreshRequest{
		Meta:         OAuth2Meta{TokenURL: ""},
		RefreshToken: []byte("rt"),
	})
	if err == nil {
		t.Fatal("expected error for empty TokenURL")
	}
}

func TestHTTPRefresher_RejectsEmptyRefreshToken(t *testing.T) {
	r := &HTTPRefresher{}
	_, err := r.Refresh(context.Background(), RefreshRequest{
		Meta: OAuth2Meta{TokenURL: "https://x"},
	})
	if err == nil {
		t.Fatal("expected error for empty refresh token")
	}
}

// Sanity: the URL the request is sent to is the metadata's
// TokenURL exactly. (Catches accidental mangling.)
func TestHTTPRefresher_UsesTokenURL(t *testing.T) {
	var got *url.URL
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL
		fmt.Fprintln(w, `{"access_token":"at","expires_in":1}`)
	}))
	defer srv.Close()

	r := &HTTPRefresher{}
	if _, err := r.Refresh(context.Background(), RefreshRequest{
		Meta:         OAuth2Meta{TokenURL: srv.URL + "/token-here"},
		RefreshToken: []byte("rt"),
	}); err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Path != "/token-here" {
		t.Errorf("got URL %v, want /token-here", got)
	}
}
