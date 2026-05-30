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
// `particle import <archive>`, `particle import <oci-ref>`, etc. —
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
	"github.com/partite-ai/particles/mounts"
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

	// Mounts is the per-particle store that records persistent
	// filesystem mount mappings. When set (along with Prompter),
	// Import offers to map each declared `capabilities.filesystem`
	// mount to a host directory. May be nil when the manifest
	// declares no mounts or the caller doesn't want to set them up
	// at install time.
	Mounts mounts.Store

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

	// SecretWithKeep is like Secret, but exposes three outcomes
	// for a reconfigure-style flow:
	//
	//   SecretKept    — user pressed Enter; preserve the stored
	//                   value. The caller should omit that role
	//                   from Store.Put / WriteSecrets.
	//   SecretSet     — user typed a new value; rotate.
	//   SecretCleared — user asked to remove the stored value
	//                   (e.g. dropping the client_secret from a
	//                   PKCE public client). The caller should
	//                   call Store.DeleteSecret for that role.
	//
	// Use only in reconfigure-style flows where an existing
	// secret is known to be present; in fresh-setup flows where
	// the user MUST supply a value, call Secret instead.
	SecretWithKeep(question string) (value string, choice SecretChoice, err error)

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

// SecretChoice is the outcome of a [Prompter.SecretWithKeep]
// call — see its docstring for the meaning of each value.
type SecretChoice int

const (
	// SecretKept means the user pressed Enter to keep the
	// existing stored secret.
	SecretKept SecretChoice = iota
	// SecretSet means the user typed a new value to rotate
	// the stored secret.
	SecretSet
	// SecretCleared means the user explicitly asked to
	// remove the stored secret (e.g. via a sentinel + confirm
	// in the prompter's UI).
	SecretCleared
)

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

	// Offer to map declared filesystem mounts to host directories.
	// Optional: a declined or absent mapping just defers to run time
	// (--mount / `particle mount`); a missing required mount never
	// blocks registration.
	fsCap, err := parseFilesystemCap(manifest.CapabilitiesRaw)
	if err != nil {
		return registry.Entry{}, err
	}
	if err := setupMounts(ctx, opts, fsCap); err != nil {
		return registry.Entry{}, err
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

	// Load the prior descriptor so dispatchSetup can default
	// matching-method prompts to the existing values (and let
	// the user press Enter to keep stored secrets). When the
	// user switches method, the prior descriptor's metadata
	// shape won't match the new helper's typed `existing`
	// parameter — each helper guards by checking
	// prev.Method == method.Name itself.
	var prev *credentials.Descriptor
	if previous != "" {
		desc, derr := opts.Credentials.GetByName(ctx, cred.Name)
		if derr == nil {
			prev = &desc
		} else if !errors.Is(derr, credentials.ErrNotFound) {
			return registry.Entry{}, "", fmt.Errorf("look up current credential for %s: %w", cred.Name, derr)
		}
	}

	chosen, err := chooseAuthMethod(opts.Prompter, cred)
	if err != nil {
		return registry.Entry{}, "", err
	}
	opts.Prompter.Info(fmt.Sprintf("→ %s.%s (%s) — %s", cred.Name, chosen.Name, chosen.Type, chosen.Description))
	if err := dispatchSetup(ctx, opts, cred.Name, chosen, prev); err != nil {
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
	if err := dispatchSetup(ctx, opts, cred.Name, chosen, nil); err != nil {
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
//
// prev, when non-nil, is the credential's prior descriptor —
// used by helpers to default visible-field prompts and to enable
// "press Enter to keep current" on secret prompts. When the
// chosen method differs from prev.Method the helper sees no
// usable prior state (typed metadata won't match) and behaves
// like a fresh setup.
func dispatchSetup(ctx context.Context, opts Options, credName string, method credentialMethod, prev *credentials.Descriptor) error {
	switch method.Type {
	case "basic":
		return setupBasic(ctx, opts, credName, method, sameMethodMeta[credentials.BasicMeta](prev, method))
	case "apikey":
		return setupAPIKey(ctx, opts, credName, method, sameMethodMeta[credentials.APIKeyMeta](prev, method))
	case "signing-key":
		return setupSigningKey(ctx, opts, credName, method, sameMethodMeta[credentials.SigningKeyMeta](prev, method))
	case "raw":
		return setupRaw(ctx, opts, credName, method, sameMethodMeta[credentials.RawMeta](prev, method))
	case "oauth2":
		return setupOAuth2(ctx, opts, credName, method, sameMethodMeta[credentials.OAuth2Meta](prev, method))
	}
	return fmt.Errorf("unknown credential type %q", method.Type)
}

// sameMethodMeta returns prev's typed metadata when the chosen
// method matches the stored one, otherwise nil. The generic
// parameter pins the metadata type per dispatch arm, so each
// helper's signature stays strongly typed and a mismatched
// method type is a nil — i.e., "behave like a fresh setup".
func sameMethodMeta[M credentials.Metadata](prev *credentials.Descriptor, method credentialMethod) *M {
	if prev == nil || prev.Method != method.Name {
		return nil
	}
	typed, ok := prev.Meta.(M)
	if !ok {
		return nil
	}
	return &typed
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
