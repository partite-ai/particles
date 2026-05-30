package importer

import (
	"context"
	"fmt"

	"github.com/partite-ai/particles/credentials"
)

// setupBasic captures username + password for an HTTP Basic
// credential. The password is masked at the terminal.
//
// When existing is non-nil (reconfigure, same method), the
// username prompt defaults to the stored value and the password
// prompt offers keep/set/clear via [Prompter.SecretWithKeep] —
// Put preserves any secret role we don't include, so a kept
// password stays; an explicit clear goes through DeleteSecret
// after Put.
func setupBasic(ctx context.Context, opts Options, credName string, method credentialMethod, existing *credentials.BasicMeta) error {
	defaultUser := ""
	if existing != nil {
		defaultUser = existing.Username
	}
	username, err := opts.Prompter.String("Username", defaultUser)
	if err != nil {
		return err
	}
	password, choice, err := secretFor(opts.Prompter, "Password", existing != nil)
	if err != nil {
		return err
	}
	var secrets []credentials.Secret
	if choice == SecretSet {
		secrets = append(secrets, credentials.Secret{Role: credentials.SecretRolePassword, Value: []byte(password)})
	}
	desc, err := opts.Credentials.Put(ctx, credName, method.Name,
		credentials.BasicMeta{Username: username},
		secrets...,
	)
	if err != nil {
		return err
	}
	if choice == SecretCleared {
		return opts.Credentials.DeleteSecret(ctx, desc.ID, credentials.SecretRolePassword)
	}
	return nil
}

// secretFor centralizes the "fresh setup vs reconfigure" branch
// every helper repeats: in fresh setup the value is required
// (Secret), in reconfigure the user can keep, set, or clear
// (SecretWithKeep). Returns the value and the choice; the
// "fresh setup" return is always SecretSet so callers can route
// uniformly on the SecretChoice.
func secretFor(p Prompter, question string, reconfiguring bool) (string, SecretChoice, error) {
	if reconfiguring {
		return p.SecretWithKeep(question)
	}
	v, err := p.Secret(question)
	if err != nil {
		return "", SecretKept, err
	}
	return v, SecretSet, nil
}

// setupAPIKey provisions an apikey credential. The
// substitution location can come from one of two places:
//
//   - the manifest, via [APIKeyCredentialMethod.location] — when
//     present, setup skips the location prompt and only asks for
//     the key value
//   - the user, via the location prompt — when the manifest
//     leaves `location` unset
//
// Pre-setting matters when the API has a single canonical
// placement (GitHub PATs always go in `Authorization: Bearer
// <pat>`) — re-asking the user is just noise.
func setupAPIKey(ctx context.Context, opts Options, credName string, method credentialMethod, existing *credentials.APIKeyMeta) error {
	var loc credentials.ApplySpec
	var err error
	if method.Location != nil {
		loc, err = applyLocationFromManifest(method.Location)
		if err != nil {
			return err
		}
		opts.Prompter.Info("  Location: " + describeApplySpec(loc))
	} else if existing != nil {
		// Reconfigure with no manifest-pinned location: keep
		// what's stored. The user can change it by first
		// switching the method (which forces a fresh
		// location prompt). Re-asking here would force a
		// password-style typing exercise for something that
		// almost never changes.
		loc = existing.Location
		opts.Prompter.Info("  Location: " + describeApplySpec(loc) + " (unchanged)")
	} else {
		loc, err = promptAPIKeyLocation(opts.Prompter)
		if err != nil {
			return err
		}
	}
	key, choice, err := secretFor(opts.Prompter, "Key value", existing != nil)
	if err != nil {
		return err
	}
	var secrets []credentials.Secret
	if choice == SecretSet {
		secrets = append(secrets, credentials.Secret{Role: credentials.SecretRoleKey, Value: []byte(key)})
	}
	desc, err := opts.Credentials.Put(ctx, credName, method.Name,
		credentials.APIKeyMeta{Location: loc},
		secrets...,
	)
	if err != nil {
		return err
	}
	if choice == SecretCleared {
		return opts.Credentials.DeleteSecret(ctx, desc.ID, credentials.SecretRoleKey)
	}
	return nil
}

// applyLocationFromManifest converts the manifest JSON shape
// (kind + name/scheme) into the typed [credentials.ApplySpec],
// rejecting kinds and missing fields up front so the setup error
// points at the manifest rather than at a downstream type
// mismatch.
func applyLocationFromManifest(loc *applyLocation) (credentials.ApplySpec, error) {
	switch loc.Kind {
	case "header":
		if loc.Name == "" {
			return credentials.ApplySpec{}, fmt.Errorf("apikey location.kind=header requires `name`")
		}
		return credentials.ApplySpec{Kind: credentials.ApplyHeader, Name: loc.Name}, nil
	case "auth-scheme":
		if loc.Scheme == "" {
			return credentials.ApplySpec{}, fmt.Errorf("apikey location.kind=auth-scheme requires `scheme`")
		}
		return credentials.ApplySpec{Kind: credentials.ApplyAuthScheme, Scheme: loc.Scheme}, nil
	case "query-param":
		if loc.Name == "" {
			return credentials.ApplySpec{}, fmt.Errorf("apikey location.kind=query-param requires `name`")
		}
		return credentials.ApplySpec{Kind: credentials.ApplyQueryParam, Name: loc.Name}, nil
	}
	return credentials.ApplySpec{}, fmt.Errorf("apikey location: unknown kind %q (want header, auth-scheme, or query-param)", loc.Kind)
}

// describeApplySpec produces the human-readable string the
// prompter shows when a location is taken from the manifest —
// matches the wording of the prompt's choice descriptions.
func describeApplySpec(s credentials.ApplySpec) string {
	switch s.Kind {
	case credentials.ApplyHeader:
		return s.Name + ": <value>"
	case credentials.ApplyAuthScheme:
		return "Authorization: " + s.Scheme + " <value>"
	case credentials.ApplyQueryParam:
		return "?" + s.Name + "=<value>"
	}
	return s.Kind.String()
}

func promptAPIKeyLocation(p Prompter) (credentials.ApplySpec, error) {
	kind, err := p.Choice("Where does this key appear in requests?", []ChoiceOption{
		{Value: "header", Label: "Custom header", Description: "e.g., X-API-Key: <value>"},
		{Value: "auth-scheme", Label: "Authorization header with scheme", Description: "e.g., Authorization: Token <value>"},
		{Value: "query-param", Label: "Query parameter", Description: "e.g., ?api_key=<value>"},
	})
	if err != nil {
		return credentials.ApplySpec{}, err
	}
	switch kind {
	case "header":
		name, err := p.String("Header name", "X-API-Key")
		if err != nil {
			return credentials.ApplySpec{}, err
		}
		return credentials.ApplySpec{Kind: credentials.ApplyHeader, Name: name}, nil
	case "auth-scheme":
		scheme, err := p.String("Auth scheme prefix", "Bearer")
		if err != nil {
			return credentials.ApplySpec{}, err
		}
		return credentials.ApplySpec{Kind: credentials.ApplyAuthScheme, Scheme: scheme}, nil
	case "query-param":
		name, err := p.String("Query parameter name", "api_key")
		if err != nil {
			return credentials.ApplySpec{}, err
		}
		return credentials.ApplySpec{Kind: credentials.ApplyQueryParam, Name: name}, nil
	}
	return credentials.ApplySpec{}, fmt.Errorf("unrecognized location %q", kind)
}

// setupSigningKey captures the raw key material. The algorithm
// is taken from the method declaration (the manifest author
// committed to one at design time) — re-prompting would invite
// drift between the schema and what's stored.
func setupSigningKey(ctx context.Context, opts Options, credName string, method credentialMethod, existing *credentials.SigningKeyMeta) error {
	if method.Algorithm == "" {
		return fmt.Errorf("manifest declares signing-key %s.%s without algorithm", credName, method.Name)
	}
	key, choice, err := secretFor(opts.Prompter, fmt.Sprintf("Signing key (%s)", method.Algorithm), existing != nil)
	if err != nil {
		return err
	}
	var secrets []credentials.Secret
	if choice == SecretSet {
		secrets = append(secrets, credentials.Secret{Role: credentials.SecretRoleKey, Value: []byte(key)})
	}
	desc, err := opts.Credentials.Put(ctx, credName, method.Name,
		credentials.SigningKeyMeta{Algorithm: method.Algorithm},
		secrets...,
	)
	if err != nil {
		return err
	}
	if choice == SecretCleared {
		return opts.Credentials.DeleteSecret(ctx, desc.ID, credentials.SecretRoleKey)
	}
	return nil
}

// setupRaw captures an opaque value, after warning the user that
// it'll be visible to the JS handler (and any transitive npm
// dep) in plaintext. Per design doc §7.
func setupRaw(ctx context.Context, opts Options, credName string, method credentialMethod, existing *credentials.RawMeta) error {
	opts.Prompter.Warn("'raw' credentials are returned to your particle's JavaScript in their actual value.")
	opts.Prompter.Warn("They will be visible to all code in the particle, including transitive npm dependencies.")
	opts.Prompter.Warn("Use a more specific type (basic, oauth2, apikey, signing-key) where possible.")
	ok, err := opts.Prompter.Confirm("Continue?", false)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("aborted by user")
	}
	value, choice, err := secretFor(opts.Prompter, "Value", existing != nil)
	if err != nil {
		return err
	}
	var secrets []credentials.Secret
	if choice == SecretSet {
		secrets = append(secrets, credentials.Secret{Role: credentials.SecretRoleValue, Value: []byte(value)})
	}
	desc, err := opts.Credentials.Put(ctx, credName, method.Name,
		credentials.RawMeta{},
		secrets...,
	)
	if err != nil {
		return err
	}
	if choice == SecretCleared {
		return opts.Credentials.DeleteSecret(ctx, desc.ID, credentials.SecretRoleValue)
	}
	return nil
}
