package importer

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/oauth2"

	"github.com/partite-ai/particles/credentials"
)

// flow constants — also surfaced in js-types/particle/index.d.ts as the
// values a manifest may list under `credentials.<name>.methods.<method>.flows`.
const (
	flowAuthCode     = "authorization-code"
	flowAuthCodePKCE = "authorization-code-pkce"
	flowDeviceCode   = "device-code"
)

// setupOAuth2 captures the OAuth client config, runs the chosen
// flow against the provider, and stores both metadata and the
// resulting tokens in the credentials store.
//
// The method's `flows` list constrains which flow runs. When
// multiple are offered, the user picks one at the prompt.
//
// When existing is non-nil (reconfigure with same method),
// visible fields default to the stored values and the client-
// secret prompt offers "press Enter to keep current". The
// OAuth flow itself always re-runs — token rotation is the
// whole point of running setup again, and `--reauth-only` is
// the path for the "I only want fresh tokens" case.
func setupOAuth2(ctx context.Context, opts Options, credName string, method credentialMethod, existing *credentials.OAuth2Meta) error {
	if len(method.Flows) == 0 {
		return fmt.Errorf("manifest declares oauth2 %s.%s without flows", credName, method.Name)
	}

	cfg, flow, clientSecret, clientSecretChoice, meta, err := promptOAuth2Config(opts.Prompter, method, existing)
	if err != nil {
		return err
	}

	// When the user kept the existing client secret, fold it
	// back into the Config so the chosen flow can authenticate
	// to the token endpoint. We re-resolve the descriptor to
	// get the credential's ID, then read the secret directly —
	// the prompter never surfaced its value. A Cleared choice
	// leaves cfg.ClientSecret empty so the flow runs as a
	// public client; the stored secret is removed below.
	if clientSecretChoice == SecretKept {
		desc, derr := opts.Credentials.GetByName(ctx, credName)
		if derr != nil {
			return fmt.Errorf("look up %s for kept client secret: %w", credName, derr)
		}
		stored, rerr := opts.Credentials.ReadSecret(ctx, desc.ID, credentials.SecretRoleClientSecret)
		if rerr == nil {
			cfg.ClientSecret = string(stored)
		} else if !errors.Is(rerr, credentials.ErrNotFound) {
			return fmt.Errorf("read stored client secret: %w", rerr)
		}
		// ErrNotFound: nothing stored under that role — leave
		// ClientSecret empty (matches the public-client case).
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
	// Write the client secret only when the user provided a
	// new one. SecretKept → Put with the same method preserves
	// the prior value untouched. SecretCleared → run Put, then
	// DeleteSecret below to remove the role entirely.
	if clientSecretChoice == SecretSet && clientSecret != "" {
		secrets = append(secrets, credentials.Secret{
			Role: credentials.SecretRoleClientSecret, Value: []byte(clientSecret),
		})
	}

	desc, err := opts.Credentials.Put(ctx, credName, method.Name, meta, secrets...)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if clientSecretChoice == SecretCleared {
		if err := opts.Credentials.DeleteSecret(ctx, desc.ID, credentials.SecretRoleClientSecret); err != nil {
			return fmt.Errorf("clear client secret: %w", err)
		}
	}
	opts.Prompter.Info(fmt.Sprintf("✓ %s.%s — OAuth complete (%s)", credName, method.Name, flow))
	return nil
}

// promptOAuth2Config walks the user through every static field
// the OAuth flows need: URLs, client ID, client secret (when
// applicable), flow choice.
//
// URL precedence: manifest override > prompt the user. A manifest
// that pre-sets `tokenUrl` silently uses it; one that omits it
// prompts on a blank slate.
//
// Scopes come from the manifest itself — re-prompting would
// invite drift between the schema and what's negotiated.
//
// When existing is non-nil (reconfigure with same oauth method),
// the Client ID prompt defaults to the stored value, and the
// Client secret prompt offers keep/set/clear via
// [Prompter.SecretWithKeep]. The returned clientSecretChoice
// reports which branch ran so the caller can either fold the
// stored secret back into the *oauth2.Config (Kept) or schedule
// a DeleteSecret (Cleared).
func promptOAuth2Config(p Prompter, method credentialMethod, existing *credentials.OAuth2Meta) (
	cfg *oauth2.Config, flow string, clientSecret string, clientSecretChoice SecretChoice,
	meta credentials.OAuth2Meta, err error,
) {
	authURL, err := resolveOAuthURL(p, "Authorization URL", method.AuthorizationURL)
	if err != nil {
		return nil, "", "", SecretKept, credentials.OAuth2Meta{}, err
	}
	tokenURL, err := resolveOAuthURL(p, "Token URL", method.TokenURL)
	if err != nil {
		return nil, "", "", SecretKept, credentials.OAuth2Meta{}, err
	}
	// Device-auth isn't prompted; if the manifest omits it and
	// the device-code flow runs, the device-flow runner errors
	// with a clear message — no path silently does the wrong
	// thing.
	deviceURL := method.DeviceAuthURL

	defaultClientID := ""
	if existing != nil {
		defaultClientID = existing.ClientID
	}
	clientID, err := p.String("Client ID", defaultClientID)
	if err != nil {
		return nil, "", "", SecretKept, credentials.OAuth2Meta{}, err
	}

	flow, err = chooseFlow(p, method.Flows)
	if err != nil {
		return nil, "", "", SecretKept, credentials.OAuth2Meta{}, err
	}

	// Auth-code (no PKCE) ALWAYS needs a client secret. PKCE may
	// or may not — public clients omit it; some providers
	// (GitHub, …) require it even with PKCE. Letting the user
	// leave it blank for PKCE handles both, and the keep/clear
	// affordance covers "I want this PKCE client to become
	// public" (clear) and "leave my existing secret alone"
	// (keep).
	switch flow {
	case flowAuthCode:
		clientSecret, clientSecretChoice, err = secretFor(p, "Client secret", existing != nil)
	case flowAuthCodePKCE:
		if existing != nil {
			clientSecret, clientSecretChoice, err = p.SecretWithKeep("Client secret (leave blank for public clients)")
		} else {
			// Fresh PKCE setup: blank is allowed, so use
			// String. Map to SecretSet so the caller writes
			// (or skips, if it's empty).
			clientSecret, err = p.String("Client secret (leave blank for public clients)", "")
			clientSecretChoice = SecretSet
		}
	}
	if err != nil {
		return nil, "", "", SecretKept, credentials.OAuth2Meta{}, err
	}

	if len(method.Scopes) > 0 {
		p.Info(fmt.Sprintf("Requesting scopes: %v", method.Scopes))
	}

	cfg = &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       method.Scopes,
		Endpoint: oauth2.Endpoint{
			AuthURL:       authURL,
			TokenURL:      tokenURL,
			DeviceAuthURL: deviceURL,
		},
	}
	meta = credentials.OAuth2Meta{
		AuthorizationURL: authURL,
		TokenURL:         tokenURL,
		ClientID:         clientID,
		Scopes:           method.Scopes,
		Flow:             flow,
	}
	return cfg, flow, clientSecret, clientSecretChoice, meta, nil
}

// resolveOAuthURL applies the manifest-override > prompt
// precedence for one OAuth URL field. When the manifest
// pre-sets the URL, it's logged at info-level and used as-is.
func resolveOAuthURL(p Prompter, label, manifestValue string) (string, error) {
	if manifestValue != "" {
		p.Info("  " + label + ": " + manifestValue)
		return manifestValue, nil
	}
	return p.String(label, "")
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
