package importer

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/oauth2"

	"github.com/partite-ai/particles/credentials"
	"github.com/partite-ai/particles/registry"
)

// ReauthOAuth re-runs only the OAuth authorization flow for an
// already-configured oauth2 credential, rotating the access and
// refresh tokens without touching client ID, client secret, or
// any other stored config.
//
// `credName` selects the credential by name; pass "" when the
// particle declares exactly one credential. The credential must
// already be configured as oauth2 — switching method requires
// the full reconfigure path.
//
// The flow used is whichever the user picked at original setup
// (recorded as OAuth2Meta.Flow). If that flow is no longer
// listed in the manifest's `flows` for this method, reauth
// errors with a message pointing the user at the full
// reconfigure path.
//
// All other state — client ID, scopes, URLs, client secret —
// is preserved as-is by going through Store.WriteSecrets, which
// rewrites only the named secret roles.
func ReauthOAuth(ctx context.Context, particleName, credName string, opts Options) (registry.Entry, string, error) {
	if opts.Registry == nil {
		return registry.Entry{}, "", errors.New("importer: Registry is required")
	}
	if opts.Credentials == nil {
		return registry.Entry{}, "", errors.New("importer: Credentials store is required")
	}
	if opts.Prompter == nil {
		return registry.Entry{}, "", errors.New("importer: Prompter is required")
	}

	version, err := highestRegisteredVersion(ctx, opts.Registry, particleName)
	if err != nil {
		return registry.Entry{}, "", err
	}
	entry, err := opts.Registry.Get(ctx, particleName, version)
	if err != nil {
		return registry.Entry{}, "", fmt.Errorf("registry.Get %s@%s: %w", particleName, version, err)
	}
	manifest, err := readManifest(entry.Particle)
	if err != nil {
		return registry.Entry{}, "", err
	}
	creds, err := parseCredentials(manifest.CredentialsRaw)
	if err != nil {
		return registry.Entry{}, "", err
	}
	if len(creds) == 0 {
		return registry.Entry{}, "", fmt.Errorf("%s@%s declares no credentials", particleName, version)
	}
	cred, err := pickCredential(creds, credName)
	if err != nil {
		return registry.Entry{}, "", fmt.Errorf("%s@%s: %w", particleName, version, err)
	}

	desc, err := opts.Credentials.GetByName(ctx, cred.Name)
	if err != nil {
		if errors.Is(err, credentials.ErrNotFound) {
			return registry.Entry{}, "", fmt.Errorf("%s.%s is not configured yet — run `particle reconfigure %s` first", particleName, cred.Name, particleName)
		}
		return registry.Entry{}, "", fmt.Errorf("look up %s: %w", cred.Name, err)
	}
	meta, ok := desc.Meta.(credentials.OAuth2Meta)
	if !ok {
		return registry.Entry{}, "", fmt.Errorf("%s.%s is %s, not oauth2 — --reauth-only only applies to OAuth credentials", particleName, cred.Name, desc.Meta.Kind())
	}
	method := findMethod(cred, desc.Method)
	if method == nil {
		return registry.Entry{}, "", fmt.Errorf("manifest no longer declares method %q for %s — run `particle reconfigure %s` to pick a new one", desc.Method, cred.Name, particleName)
	}
	if meta.Flow == "" {
		return registry.Entry{}, "", fmt.Errorf("no OAuth flow recorded for %s — run `particle reconfigure %s` to re-establish it", cred.Name, particleName)
	}
	if !flowDeclared(method.Flows, meta.Flow) {
		return registry.Entry{}, "", fmt.Errorf("stored flow %q no longer listed in manifest for %s.%s — run `particle reconfigure %s`", meta.Flow, cred.Name, method.Name, particleName)
	}

	// Reconstruct the *oauth2.Config from stored metadata + the
	// stored client secret. Auth/token/device URLs come from the
	// manifest (it's the source of truth post-setup); falling
	// back to the stored copy lets reauth work when the manifest
	// was written without these fields.
	cfg := &oauth2.Config{
		ClientID: meta.ClientID,
		Scopes:   meta.Scopes,
		Endpoint: oauth2.Endpoint{
			AuthURL:       firstNonEmpty(method.AuthorizationURL, meta.AuthorizationURL),
			TokenURL:      firstNonEmpty(method.TokenURL, meta.TokenURL),
			DeviceAuthURL: method.DeviceAuthURL,
		},
	}
	if storedSecret, rerr := opts.Credentials.ReadSecret(ctx, desc.ID, credentials.SecretRoleClientSecret); rerr == nil {
		cfg.ClientSecret = string(storedSecret)
	} else if !errors.Is(rerr, credentials.ErrNotFound) {
		return registry.Entry{}, "", fmt.Errorf("read stored client secret: %w", rerr)
	}

	opts.Prompter.Info(fmt.Sprintf("Re-running OAuth for %s.%s (%s)", cred.Name, method.Name, meta.Flow))
	if len(meta.Scopes) > 0 {
		opts.Prompter.Info(fmt.Sprintf("Requesting scopes: %v", meta.Scopes))
	}

	var token *oauth2.Token
	switch meta.Flow {
	case flowAuthCodePKCE:
		token, err = runAuthCodeFlow(ctx, opts.Prompter, cfg, true)
	case flowAuthCode:
		token, err = runAuthCodeFlow(ctx, opts.Prompter, cfg, false)
	case flowDeviceCode:
		token, err = runDeviceCodeFlow(ctx, opts.Prompter, cfg)
	default:
		return registry.Entry{}, "", fmt.Errorf("unsupported OAuth flow %q", meta.Flow)
	}
	if err != nil {
		return registry.Entry{}, "", fmt.Errorf("%s flow: %w", meta.Flow, err)
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
	// WriteSecrets touches only the listed roles; client_secret,
	// metadata, and any other state stay exactly as they were.
	if err := opts.Credentials.WriteSecrets(ctx, desc.ID, secrets...); err != nil {
		return registry.Entry{}, "", fmt.Errorf("store: %w", err)
	}
	opts.Prompter.Info(fmt.Sprintf("✓ %s.%s — tokens rotated (%s)", cred.Name, method.Name, meta.Flow))
	return entry, method.Name, nil
}

// findMethod returns a pointer to the method in cred with the
// given name, or nil if not declared.
func findMethod(cred credentialDecl, name string) *credentialMethod {
	for i := range cred.Methods {
		if cred.Methods[i].Name == name {
			return &cred.Methods[i]
		}
	}
	return nil
}

// flowDeclared reports whether flow is in the manifest's flow
// list for this method.
func flowDeclared(flows []string, flow string) bool {
	for _, f := range flows {
		if f == flow {
			return true
		}
	}
	return false
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
