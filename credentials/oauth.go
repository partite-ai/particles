package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/partite-ai/particle/internal/hostmeter"
	gen "github.com/partite-ai/particle/internal/host/gen/particle/host/oauth"
)

// OAuthRefresher performs the actual OAuth 2.0 token refresh
// exchange against an upstream provider.
//
// The credentials package's OAuth host adapter consults a Refresher
// every time a particle calls `oauth.refresh(name)`. The Refresher
// is responsible for the network round-trip; the adapter handles
// the Store side (reading the existing entry, packaging the new
// access token bundle, writing it via Store.WriteSecrets without
// touching metadata).
//
// HTTPRefresher is a default RFC-6749-conformant implementation;
// hosts can plug in something else for providers that deviate or
// for tests that want to mock the network.
type OAuthRefresher interface {
	Refresh(ctx context.Context, req RefreshRequest) (RefreshResponse, error)
}

// RefreshRequest is everything the refresher needs to perform one
// token refresh: the credential's metadata (provider URLs, client
// id/secret, scopes) and the current refresh token.
type RefreshRequest struct {
	Meta         OAuth2Meta
	ClientSecret []byte // empty when the credential has no client secret (PKCE)
	RefreshToken []byte
}

// RefreshResponse is what the refresher produces on success. The
// adapter packages AccessToken + ExpiresAt + TokenType into an
// [AccessToken] bundle and writes it to SecretRoleAccessToken;
// when RefreshToken is non-empty, it's written to
// SecretRoleRefreshToken too. Metadata is never updated.
type RefreshResponse struct {
	AccessToken []byte

	// RefreshToken is the new refresh token if the provider
	// rotated it; empty otherwise (the existing refresh token
	// stays valid). Many providers — Google, GitHub — do NOT
	// rotate; some — Okta, Auth0 with rotation enabled — do.
	RefreshToken []byte

	// ExpiresAt is the absolute time the new access token
	// expires. The default HTTPRefresher converts the standard
	// `expires_in` (seconds-from-now) field to absolute time
	// before populating this.
	ExpiresAt time.Time

	// TokenType is the access-token type string returned by the
	// provider (typically "Bearer"). Empty leaves the existing
	// metadata's TokenType untouched.
	TokenType string
}

// -----------------------------------------------------------------------------
// HTTP refresher (default implementation)
// -----------------------------------------------------------------------------

// HTTPRefresher is the default OAuthRefresher: a stock RFC 6749
// section 6 token-refresh exchange over HTTP. It posts a
// `grant_type=refresh_token` request to the credential's TokenURL
// and parses a JSON response.
//
// Hosts that need provider-specific behavior (header overrides,
// non-JSON responses, bespoke error parsing) should provide their
// own OAuthRefresher.
type HTTPRefresher struct {
	// Client is the http.Client used to perform the request.
	// nil means http.DefaultClient.
	Client *http.Client
}

var _ OAuthRefresher = (*HTTPRefresher)(nil)

// Refresh performs an RFC 6749 §6 refresh token exchange.
func (r *HTTPRefresher) Refresh(ctx context.Context, req RefreshRequest) (RefreshResponse, error) {
	if req.Meta.TokenURL == "" {
		return RefreshResponse{}, errors.New("oauth: TokenURL is required")
	}
	if len(req.RefreshToken) == 0 {
		return RefreshResponse{}, errors.New("oauth: refresh token is empty")
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", string(req.RefreshToken))
	if req.Meta.ClientID != "" {
		form.Set("client_id", req.Meta.ClientID)
	}
	// Per RFC 6749 §6, scope is optional and must be a subset of
	// the original. Only include it if the entry recorded one.
	if len(req.Meta.Scopes) > 0 {
		form.Set("scope", strings.Join(req.Meta.Scopes, " "))
	}
	// Confidential clients send the secret in the body; PKCE
	// clients omit it. The form-param style is the more widely
	// supported variant.
	if len(req.ClientSecret) > 0 {
		form.Set("client_secret", string(req.ClientSecret))
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.Meta.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return RefreshResponse{}, fmt.Errorf("oauth: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Accept", "application/json")

	client := r.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return RefreshResponse{}, fmt.Errorf("oauth: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		return RefreshResponse{}, fmt.Errorf("oauth: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// RFC 6749 §5.2 spells out a JSON error body shape;
		// we surface whatever the provider said in a single
		// human-readable string.
		var errResp struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &errResp)
		switch {
		case errResp.Error != "" && errResp.ErrorDescription != "":
			return RefreshResponse{}, fmt.Errorf("oauth: %d %s: %s", resp.StatusCode, errResp.Error, errResp.ErrorDescription)
		case errResp.Error != "":
			return RefreshResponse{}, fmt.Errorf("oauth: %d %s", resp.StatusCode, errResp.Error)
		default:
			return RefreshResponse{}, fmt.Errorf("oauth: %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
	}

	// `expires_in` is documented as seconds-from-now; some
	// providers send it as a JSON number, others as a string.
	// Decode permissively.
	var raw struct {
		AccessToken  string          `json:"access_token"`
		RefreshToken string          `json:"refresh_token"`
		TokenType    string          `json:"token_type"`
		ExpiresIn    json.RawMessage `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return RefreshResponse{}, fmt.Errorf("oauth: parse response: %w", err)
	}
	if raw.AccessToken == "" {
		return RefreshResponse{}, errors.New("oauth: response missing access_token")
	}

	now := time.Now().UTC()
	out := RefreshResponse{
		AccessToken:  []byte(raw.AccessToken),
		RefreshToken: []byte(raw.RefreshToken),
		TokenType:    raw.TokenType,
	}
	if expiresIn, ok := parseExpiresIn(raw.ExpiresIn); ok {
		out.ExpiresAt = now.Add(time.Duration(expiresIn) * time.Second)
	}
	return out, nil
}

// parseExpiresIn handles both `"expires_in": 3600` and
// `"expires_in": "3600"` shapes — both are observed in the wild.
func parseExpiresIn(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	// Try as JSON number first.
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			return v, true
		}
	}
	return 0, false
}

// -----------------------------------------------------------------------------
// adapter
// -----------------------------------------------------------------------------

type oauthAdapter struct {
	store     Store
	refresher OAuthRefresher
	particle  string
}

var _ gen.Oauth = (*oauthAdapter)(nil)

func newOAuthAdapter(store Store, refresher OAuthRefresher, particle string) *oauthAdapter {
	return &oauthAdapter{store: store, refresher: refresher, particle: particle}
}

// Refresh resolves the credential by name, verifies it's an OAuth2
// entry, and rotates the access token via [rotateAccessToken].
//
// Maps the rotation mechanics' errors to the WIT
// `result<_, oauth-error>`:
//
//	credential not found            → not-configured
//	credential exists but not OAuth2 → not-oauth
//	refresh token slot empty        → not-configured
//	upstream refresh fails or store
//	  rejects the write             → refresh-failed(message)
func (a *oauthAdapter) Refresh(ctx context.Context, name string) (gen.Result_OauthError, error) {
	defer hostmeter.EnterHost(ctx)()

	desc, err := a.store.GetByName(ctx, a.particle, name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return gen.Result_OauthErrorErr{Value: gen.OauthErrorNotConfigured{}}, nil
		}
		return gen.Result_OauthErrorErr{Value: gen.OauthErrorRefreshFailed{Value: err.Error()}}, nil
	}
	if _, ok := desc.Meta.(OAuth2Meta); !ok {
		return gen.Result_OauthErrorErr{Value: gen.OauthErrorNotOauth{}}, nil
	}
	if _, err := rotateAccessToken(ctx, a.store, a.refresher, a.particle, desc.ID); err != nil {
		// ErrNotFound (and the ErrSecretNotSet that wraps it)
		// at this layer means the refresh token slot was empty —
		// the credential isn't usable for refresh.
		if errors.Is(err, ErrNotFound) {
			return gen.Result_OauthErrorErr{Value: gen.OauthErrorNotConfigured{}}, nil
		}
		return gen.Result_OauthErrorErr{Value: gen.OauthErrorRefreshFailed{Value: err.Error()}}, nil
	}
	return gen.Result_OauthErrorOk{}, nil
}

// rotateAccessToken performs the OAuth 2.0 refresh mechanics for
// the credential identified by (particle, id):
//
//  1. Read the current refresh token + (optionally) client secret.
//  2. Hand them to the [OAuthRefresher] along with the entry's
//     metadata.
//  3. Atomically write the rotated access-token bundle (and the
//     refresh token, if the provider returned a new one) back via
//     [Store.WriteSecrets].
//
// Returns the new [AccessToken] bundle (Token + Type + ExpiresAt)
// so callers don't need a follow-up ReadSecret. Plain Go errors —
// no WIT mapping — so callers can compose this in whichever
// context they want (the WIT adapter, the wasi:http policy's
// proactive refresh path, future host integrations).
//
// The entry's metadata is read inside (no metadata lookup is
// required from the caller). The caller is responsible for
// verifying that the entry IS an OAuth2 credential before
// calling — if it isn't, rotate returns a clear error pointing
// at the type mismatch.
func rotateAccessToken(ctx context.Context, store Store, refresher OAuthRefresher, particle, id string) (AccessToken, error) {
	desc, err := store.GetByID(ctx, particle, id)
	if err != nil {
		return AccessToken{}, fmt.Errorf("rotate: lookup %s: %w", id, err)
	}
	meta, ok := desc.Meta.(OAuth2Meta)
	if !ok {
		return AccessToken{}, fmt.Errorf("rotate %s: not an oauth2 credential", id)
	}

	refreshTok, err := store.ReadSecret(ctx, particle, id, SecretRoleRefreshToken)
	if err != nil {
		return AccessToken{}, fmt.Errorf("rotate %s: read refresh token: %w", id, err)
	}

	// Client secret is optional (PKCE flows omit it). Tolerate
	// ErrSecretNotSet and pass an empty value through.
	clientSecret, err := store.ReadSecret(ctx, particle, id, SecretRoleClientSecret)
	if err != nil && !errors.Is(err, ErrSecretNotSet) {
		return AccessToken{}, fmt.Errorf("rotate %s: read client secret: %w", id, err)
	}

	resp, err := refresher.Refresh(ctx, RefreshRequest{
		Meta:         meta,
		ClientSecret: clientSecret,
		RefreshToken: refreshTok,
	})
	if err != nil {
		return AccessToken{}, fmt.Errorf("rotate %s: %w", id, err)
	}
	if len(resp.AccessToken) == 0 {
		return AccessToken{}, fmt.Errorf("rotate %s: refresher returned empty access token", id)
	}

	bundle := AccessToken{
		Token:     string(resp.AccessToken),
		Type:      resp.TokenType,
		ExpiresAt: resp.ExpiresAt,
	}

	// Atomic secret-only write: rotate the access token bundle,
	// and — only when the provider rotated — the refresh token.
	// Client secret and any other secrets stay untouched.
	secrets := []Secret{{Role: SecretRoleAccessToken, Value: bundle.Marshal()}}
	if len(resp.RefreshToken) > 0 {
		secrets = append(secrets, Secret{Role: SecretRoleRefreshToken, Value: resp.RefreshToken})
	}
	if err := store.WriteSecrets(ctx, particle, id, secrets...); err != nil {
		return AccessToken{}, fmt.Errorf("rotate %s: store: %w", id, err)
	}
	return bundle, nil
}
