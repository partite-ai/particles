package importer

import (
	"context"
	"fmt"

	"golang.org/x/oauth2"

	"github.com/partite-ai/particle/credentials"
)

// flow constants — also surfaced in types/particle.d.ts as the
// values a manifest may list under `capabilities.credentials.*.flows`.
const (
	flowAuthCode     = "authorization-code"
	flowAuthCodePKCE = "authorization-code-pkce"
	flowDeviceCode   = "device-code"
)

// setupOAuth2 captures the OAuth client config, runs the chosen
// flow against the provider, and stores both metadata and the
// resulting tokens in the credentials store.
//
// The manifest's `flows` list constrains which flow runs. When
// multiple are offered, the user picks one at the prompt.
func setupOAuth2(ctx context.Context, opts Options, particle string, decl credentialDecl) error {
	if len(decl.Flows) == 0 {
		return fmt.Errorf("manifest declares oauth2 %q without flows", decl.Name)
	}

	cfg, flow, clientSecret, meta, err := promptOAuth2Config(opts.Prompter, decl)
	if err != nil {
		return err
	}

	var token *oauth2.Token
	switch flow {
	case flowAuthCodePKCE:
		token, err = runAuthCodeFlow(ctx, opts.Prompter, cfg, true)
	case flowAuthCode:
		token, err = runAuthCodeFlow(ctx, opts.Prompter, cfg, false)
	case flowDeviceCode:
		token, err = runDeviceCodeFlow(ctx, opts.Prompter, cfg)
	default:
		return fmt.Errorf("unsupported OAuth flow %q", flow)
	}
	if err != nil {
		return fmt.Errorf("%s flow: %w", flow, err)
	}

	bundle := credentials.AccessToken{
		Token:     token.AccessToken,
		Type:      token.TokenType,
		ExpiresAt: token.Expiry,
	}
	secrets := []credentials.Secret{
		{Role: credentials.SecretRoleAccessToken, Value: bundle.Marshal()},
	}
	if token.RefreshToken != "" {
		secrets = append(secrets, credentials.Secret{
			Role: credentials.SecretRoleRefreshToken, Value: []byte(token.RefreshToken),
		})
	}
	if clientSecret != "" {
		secrets = append(secrets, credentials.Secret{
			Role: credentials.SecretRoleClientSecret, Value: []byte(clientSecret),
		})
	}

	if _, err := opts.Credentials.Put(ctx, particle, decl.Name, meta, secrets...); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	opts.Prompter.Info(fmt.Sprintf("✓ %s — OAuth complete (%s)", decl.Name, flow))
	return nil
}

// promptOAuth2Config walks the user through every static field
// the OAuth flows need: URLs, client ID, client secret (when
// applicable), flow choice.
//
// URL precedence: manifest override > provider-hint default >
// prompt the user. So a manifest that pre-sets `tokenUrl`
// silently uses it; one that just hints `provider: "github"`
// pre-fills the prompt with GitHub's token URL but lets the user
// override; one with neither prompts on a blank slate.
//
// Scopes come from the manifest itself — re-prompting would
// invite drift between the schema and what's negotiated.
func promptOAuth2Config(p Prompter, decl credentialDecl) (*oauth2.Config, string, string, credentials.OAuth2Meta, error) {
	presets := providerPresets(decl.Provider)

	authURL, err := resolveOAuthURL(p, "Authorization URL", decl.AuthorizationURL, presets.Auth)
	if err != nil {
		return nil, "", "", credentials.OAuth2Meta{}, err
	}
	tokenURL, err := resolveOAuthURL(p, "Token URL", decl.TokenURL, presets.Token)
	if err != nil {
		return nil, "", "", credentials.OAuth2Meta{}, err
	}
	// Device-auth and revocation aren't prompted; we use the
	// override if set, fall through to the provider preset
	// otherwise. If the resulting deviceURL is empty and the
	// device-code flow is chosen, the device-flow runner errors
	// with a clear message — there's no path that silently does
	// the wrong thing.
	deviceURL := decl.DeviceAuthURL
	if deviceURL == "" {
		deviceURL = presets.Device
	}
	revokeURL := decl.RevocationURL
	if revokeURL == "" {
		revokeURL = presets.Revoke
	}

	clientID, err := p.String("Client ID", "")
	if err != nil {
		return nil, "", "", credentials.OAuth2Meta{}, err
	}

	flow, err := chooseFlow(p, decl.Flows)
	if err != nil {
		return nil, "", "", credentials.OAuth2Meta{}, err
	}

	// Auth-code (no PKCE) ALWAYS needs a client secret. PKCE may
	// or may not — public clients omit it; some providers
	// (GitHub, …) require it even with PKCE. Letting the user
	// leave it blank for PKCE handles both.
	var clientSecret string
	switch flow {
	case flowAuthCode:
		clientSecret, err = p.Secret("Client secret")
	case flowAuthCodePKCE:
		clientSecret, err = p.String("Client secret (leave blank for public clients)", "")
	}
	if err != nil {
		return nil, "", "", credentials.OAuth2Meta{}, err
	}

	if len(decl.Scopes) > 0 {
		p.Info(fmt.Sprintf("Requesting scopes: %v", decl.Scopes))
	}

	cfg := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       decl.Scopes,
		Endpoint: oauth2.Endpoint{
			AuthURL:       authURL,
			TokenURL:      tokenURL,
			DeviceAuthURL: deviceURL,
		},
	}
	meta := credentials.OAuth2Meta{
		AuthorizationURL: authURL,
		TokenURL:         tokenURL,
		RevocationURL:    revokeURL,
		ClientID:         clientID,
		Scopes:           decl.Scopes,
		Flow:             flow,
	}
	return cfg, flow, clientSecret, meta, nil
}

// resolveOAuthURL applies the manifest-override > provider-hint
// > prompt precedence for one OAuth URL field. When the manifest
// pre-sets the URL, it's logged at info-level and used as-is.
func resolveOAuthURL(p Prompter, label, manifestValue, providerDefault string) (string, error) {
	if manifestValue != "" {
		p.Info("  " + label + ": " + manifestValue)
		return manifestValue, nil
	}
	return p.String(label, providerDefault)
}

// chooseFlow short-circuits when the manifest declared exactly
// one flow; otherwise prompts the user with a labeled menu.
func chooseFlow(p Prompter, declared []string) (string, error) {
	if len(declared) == 1 {
		return declared[0], nil
	}
	options := make([]ChoiceOption, 0, len(declared))
	for _, f := range declared {
		opt := ChoiceOption{Value: f}
		switch f {
		case flowAuthCodePKCE:
			opt.Label = "Authorization Code + PKCE"
			opt.Description = "browser flow; preferred for public clients"
		case flowAuthCode:
			opt.Label = "Authorization Code"
			opt.Description = "browser flow with client secret"
		case flowDeviceCode:
			opt.Label = "Device Code"
			opt.Description = "headless / SSH; no browser on this host"
		default:
			opt.Label = f
		}
		options = append(options, opt)
	}
	return p.Choice("Which OAuth flow?", options)
}
