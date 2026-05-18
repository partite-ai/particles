// Package importer registers a built particle into a [registry.Registry],
// prompting for any missing credentials along the way.
//
// "Import" is the boundary at which a particle is forced through
// configuration: by the time `Import` returns, every credential the
// manifest declares is provisioned in the credentials store, and the
// registry holds the particle's full FS.
//
// The build CLI (`particle build`) is the first call site, but the
// abstraction is intentionally separable so future commands —
// `particle import <tarball>`, `particle import <oci-ref>`, etc. —
// can reuse it.
package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"

	"golang.org/x/mod/semver"

	"github.com/partite-ai/particles/credentials"
	"github.com/partite-ai/particles/registry"
)

// Options configures one [Import] call.
type Options struct {
	// Registry the particle will be stored in. Required.
	Registry registry.Registry

	// Credentials store consulted when the manifest declares any
	// top-level `credentials`. May be nil when the manifest
	// declares none. Required otherwise.
	Credentials credentials.Store

	// Prompter drives the per-credential setup conversation and
	// the permission-confirmation prompt (see [PermissionMode]).
	// Required whenever Import will ask the user a question.
	Prompter Prompter

	// PermissionMode controls when the capability-confirmation
	// prompt fires. Default ([PermissionAuto]) prompts only when
	// the manifest's capabilities differ from the previously-
	// registered version (or on a fresh install).
	PermissionMode PermissionMode
}

// Prompter is the surface importer uses to ask the user for
// credential values. Production code uses [StdioPrompter];
// tests substitute a fake.
type Prompter interface {
	// String prompts for a free-form line. defaultValue is shown
	// in brackets and used when the user just hits enter.
	String(question, defaultValue string) (string, error)

	// Secret prompts for a value the prompter must NOT echo to
	// the screen.
	Secret(question string) (string, error)

	// Choice prompts the user to pick one of the given options.
	// Returns the option's Value.
	Choice(question string, options []ChoiceOption) (string, error)

	// Confirm asks a yes/no question. defaultYes is the value
	// when the user just hits enter.
	Confirm(question string, defaultYes bool) (bool, error)

	// Warn surfaces a high-emphasis advisory message ("⚠ This
	// credential will be visible to your handler in plaintext.")
	Warn(message string)

	// Info surfaces a low-emphasis informational message.
	Info(message string)
}

// ChoiceOption is one entry in a [Prompter.Choice] menu.
type ChoiceOption struct {
	Value       string // returned to the caller
	Label       string // shown next to the index
	Description string // optional second line
}

// Import the particle at particleFS into opts.Registry, prompting
// for credentials when the manifest declares them. On success
// returns the resulting registry entry.
//
// Authentication shape: the manifest's top-level `credentials`
// map declares one or more named credentials (e.g., "github",
// "openai"). Each declares one or more alternative methods —
// at setup the user picks one per credential. The runtime
// exposes the chosen method per credential via
// `credentials.getConfiguredMethod(name)`.
//
// Idempotent in the steady state: any credential that's already
// configured is skipped; only unconfigured ones prompt.
func Import(ctx context.Context, particleFS fs.FS, opts Options) (registry.Entry, error) {
	if opts.Registry == nil {
		return registry.Entry{}, errors.New("importer: Registry is required")
	}

	manifest, err := readManifest(particleFS)
	if err != nil {
		return registry.Entry{}, err
	}

	creds, err := parseCredentials(manifest.CredentialsRaw)
	if err != nil {
		return registry.Entry{}, err
	}

	// Confirm declared capabilities before any credential setup
	// — a declined permission shouldn't drag the user through an
	// OAuth flow first.
	if err := confirmPermissions(ctx, opts, manifest); err != nil {
		return registry.Entry{}, err
	}

	if len(creds) > 0 {
		if opts.Credentials == nil {
			return registry.Entry{}, errors.New("importer: particle declares credentials, but no credentials store was provided")
		}
		if opts.Prompter == nil {
			return registry.Entry{}, errors.New("importer: particle declares credentials, but no Prompter was provided")
		}
		for _, cred := range creds {
			if _, err := setupOneCredential(ctx, opts, cred); err != nil {
				return registry.Entry{}, err
			}
		}
	}

	if err := opts.Registry.Put(ctx, manifest.Name, manifest.Version, particleFS); err != nil {
		return registry.Entry{}, fmt.Errorf("register %s@%s: %w", manifest.Name, manifest.Version, err)
	}
	return registry.Entry{
		Name:     manifest.Name,
		Version:  manifest.Version,
		Particle: particleFS,
	}, nil
}

// Reconfigure re-runs credential setup for one named credential
// against an already-registered particle, letting the user pick a
// (possibly different) method and provide fresh values. Returns
// the registry entry plus the name of the method the user chose —
// callers don't need to round-trip back to
// [credentials.Store.ConfiguredMethod] to learn what was just
// configured.
//
// `credName` selects which entry in the manifest's top-level
// `credentials` map to reconfigure. When the manifest declares
// exactly one credential, callers may pass "" — the lone entry
// is used; that's the common case for single-credential particles.
//
// Switching method calls into [credentials.Store.Put], which
// atomically replaces the row's metadata and wipes any prior
// method's secrets in the same transaction — readers never see a
// half-rotated credential.
//
// To pick which manifest's method declarations to walk,
// Reconfigure resolves the highest semver-registered version of
// the particle and uses that one's manifest.
func Reconfigure(ctx context.Context, particleName, credName string, opts Options) (registry.Entry, string, error) {
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
		return registry.Entry{}, "", fmt.Errorf("%s@%s declares no credentials to configure", particleName, version)
	}

	cred, err := pickCredential(creds, credName)
	if err != nil {
		return registry.Entry{}, "", fmt.Errorf("%s@%s: %w", particleName, version, err)
	}

	previous, err := opts.Credentials.ConfiguredMethod(ctx, cred.Name)
	if err != nil {
		return registry.Entry{}, "", fmt.Errorf("look up current method for %s: %w", cred.Name, err)
	}
	if previous != "" {
		opts.Prompter.Info(fmt.Sprintf("Currently configured: %s.%s", cred.Name, previous))
	}

	chosen, err := chooseAuthMethod(opts.Prompter, cred)
	if err != nil {
		return registry.Entry{}, "", err
	}
	opts.Prompter.Info(fmt.Sprintf("→ %s.%s (%s) — %s", cred.Name, chosen.Name, chosen.Type, chosen.Description))
	if err := dispatchSetup(ctx, opts, cred.Name, chosen); err != nil {
		return registry.Entry{}, "", err
	}
	return entry, chosen.Name, nil
}

// pickCredential resolves credName to a credentialDecl from creds.
// Empty credName + exactly one credential → that credential.
// Empty credName + multiple credentials → ambiguous, error listing
// available names. Otherwise → exact match.
func pickCredential(creds []credentialDecl, credName string) (credentialDecl, error) {
	if credName == "" {
		if len(creds) == 1 {
			return creds[0], nil
		}
		names := make([]string, 0, len(creds))
		for _, c := range creds {
			names = append(names, c.Name)
		}
		return credentialDecl{}, fmt.Errorf("multiple credentials declared (%v); pass a credential name to disambiguate", names)
	}
	for _, c := range creds {
		if c.Name == credName {
			return c, nil
		}
	}
	return credentialDecl{}, fmt.Errorf("no credential named %q", credName)
}

// highestRegisteredVersion returns the highest semver-registered
// version of name, or an error when name isn't registered. Used
// by Reconfigure to pick a manifest. golang.org/x/mod/semver
// expects a leading "v"; we prefix to canonicalize, and
// non-semver strings sort below valid ones (semver.Compare
// canonicalizes invalid input to "").
func highestRegisteredVersion(ctx context.Context, reg registry.Registry, name string) (string, error) {
	entries, err := reg.List(ctx)
	if err != nil {
		return "", fmt.Errorf("list registry: %w", err)
	}
	best := ""
	for _, e := range entries {
		if e.Name != name {
			continue
		}
		if best == "" || semverCompare(e.Version, best) > 0 {
			best = e.Version
		}
	}
	if best == "" {
		return "", fmt.Errorf("%s not registered", name)
	}
	return best, nil
}

func semverCompare(a, b string) int {
	return semver.Compare(canonicalSemver(a), canonicalSemver(b))
}

func canonicalSemver(v string) string {
	if len(v) > 0 && v[0] == 'v' {
		return v
	}
	return "v" + v
}

// setupOneCredential walks one declared credential through the
// "is it already configured? if not, prompt + setup" flow. Empty
// return + nil error means optional auth was declined / skipped.
func setupOneCredential(ctx context.Context, opts Options, cred credentialDecl) (string, error) {
	existing, err := opts.Credentials.ConfiguredMethod(ctx, cred.Name)
	if err != nil {
		return "", fmt.Errorf("lookup configured method for %s: %w", cred.Name, err)
	}
	if existing != "" {
		opts.Prompter.Info(fmt.Sprintf("✓ %s.%s — authentication already configured", cred.Name, existing))
		return existing, nil
	}
	if !cred.Required {
		opts.Prompter.Info(fmt.Sprintf("%s: authentication is optional; skipping credential setup.", cred.Name))
		return "", nil
	}

	chosen, err := chooseAuthMethod(opts.Prompter, cred)
	if err != nil {
		return "", err
	}
	opts.Prompter.Info(fmt.Sprintf("→ %s.%s (%s) — %s", cred.Name, chosen.Name, chosen.Type, chosen.Description))
	if err := dispatchSetup(ctx, opts, cred.Name, chosen); err != nil {
		return "", err
	}
	return chosen.Name, nil
}

// chooseAuthMethod short-circuits when the credential declared
// exactly one method; otherwise prompts the user to pick.
func chooseAuthMethod(p Prompter, cred credentialDecl) (credentialMethod, error) {
	if len(cred.Methods) == 1 {
		return cred.Methods[0], nil
	}
	options := make([]ChoiceOption, 0, len(cred.Methods))
	for _, m := range cred.Methods {
		opt := ChoiceOption{Value: m.Name, Label: m.Name + " (" + m.Type + ")"}
		if m.Description != "" {
			opt.Description = m.Description
		}
		options = append(options, opt)
	}
	chosen, err := p.Choice(fmt.Sprintf("Pick an authentication method for %s:", cred.Name), options)
	if err != nil {
		return credentialMethod{}, err
	}
	for _, m := range cred.Methods {
		if m.Name == chosen {
			return m, nil
		}
	}
	return credentialMethod{}, fmt.Errorf("internal: chosen method %q not found", chosen)
}

// dispatchSetup routes one method's setup to the per-type helper.
// Each helper writes via Store.Put with credName as the row name
// and method.Name as the method discriminator. The Store is
// already pre-bound to the right particle (opts.Credentials), so
// the particle name doesn't appear here.
func dispatchSetup(ctx context.Context, opts Options, credName string, method credentialMethod) error {
	switch method.Type {
	case "basic":
		return setupBasic(ctx, opts, credName, method)
	case "apikey":
		return setupAPIKey(ctx, opts, credName, method)
	case "signing-key":
		return setupSigningKey(ctx, opts, credName, method)
	case "raw":
		return setupRaw(ctx, opts, credName, method)
	case "oauth2":
		return setupOAuth2(ctx, opts, credName, method)
	}
	return fmt.Errorf("unknown credential type %q", method.Type)
}

// readManifest pulls manifest.json off the particle FS and decodes
// the fields the importer needs.
func readManifest(particleFS fs.FS) (manifest, error) {
	data, err := fs.ReadFile(particleFS, "manifest.json")
	if err != nil {
		return manifest{}, fmt.Errorf("read manifest.json: %w", err)
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return manifest{}, fmt.Errorf("parse manifest.json: %w", err)
	}
	if m.Name == "" || m.Version == "" {
		return manifest{}, errors.New("manifest.json is missing name or version")
	}
	return m, nil
}

type manifest struct {
	Name            string                     `json:"name"`
	Description     string                     `json:"description"`
	Version         string                     `json:"version"`
	CapabilitiesRaw map[string]json.RawMessage `json:"capabilities"`
	CredentialsRaw  map[string]json.RawMessage `json:"credentials"`
}
