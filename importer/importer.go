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

	"github.com/partite-ai/particle/credentials"
	"github.com/partite-ai/particle/registry"
)

// Options configures one [Import] call.
type Options struct {
	// Registry the particle will be stored in. Required.
	Registry registry.Registry

	// Credentials store consulted when the manifest declares any
	// `capabilities.credentials`. May be nil when the manifest
	// declares none. Required otherwise.
	Credentials credentials.Store

	// Prompter drives the per-credential setup conversation.
	// Required when the manifest declares any credentials. Use
	// [NewStdioPrompter] for the standard terminal experience.
	Prompter Prompter
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
// Authentication shape: the manifest declares
// `capabilities.credentials.methods` as a map of named
// alternative methods (e.g. oauth2 + apikey for the same
// provider). The user picks one at setup; only that one is
// provisioned. The runtime exposes the chosen name via
// `credentials.getConfiguredMethod()`.
//
// Idempotent in the steady state: if any method is already
// configured, re-importing skips prompting and just re-registers.
func Import(ctx context.Context, particleFS fs.FS, opts Options) (registry.Entry, error) {
	if opts.Registry == nil {
		return registry.Entry{}, errors.New("importer: Registry is required")
	}

	manifest, err := readManifest(particleFS)
	if err != nil {
		return registry.Entry{}, err
	}

	creds, err := parseCredentialsCapability(manifest.CapabilitiesRaw)
	if err != nil {
		return registry.Entry{}, err
	}

	var selectedMethod string
	if creds != nil && len(creds.Methods) > 0 {
		if opts.Credentials == nil {
			return registry.Entry{}, errors.New("importer: particle declares credentials, but no credentials store was provided")
		}
		if opts.Prompter == nil {
			return registry.Entry{}, errors.New("importer: particle declares credentials, but no Prompter was provided")
		}
		method, err := setupCredentials(ctx, opts, manifest.Name, creds)
		if err != nil {
			return registry.Entry{}, err
		}
		selectedMethod = method
	}

	if err := opts.Registry.Put(ctx, manifest.Name, manifest.Version, particleFS); err != nil {
		return registry.Entry{}, fmt.Errorf("register %s@%s: %w", manifest.Name, manifest.Version, err)
	}
	if selectedMethod != "" {
		if err := opts.Registry.SetSelectedAuthenticationMethod(ctx, manifest.Name, selectedMethod); err != nil {
			return registry.Entry{}, fmt.Errorf("record selected method: %w", err)
		}
	}
	return registry.Entry{
		Name:           manifest.Name,
		Version:        manifest.Version,
		Particle:       particleFS,
		SelectedAuthenticationMethod: selectedMethod,
	}, nil
}

// Reconfigure re-runs credential setup against an already-registered
// particle, letting the user pick a (possibly different)
// authentication method and provide fresh values.
//
// Identity is per-particle-name — credentials live in
// [credentials.Store] keyed by name only, and the
// authentication-method selection lives on the per-name
// [registry.Registry] settings. The version parameter that was
// here previously didn't match this model (it implied credentials
// were per-(name, version), which they aren't).
//
// To pick which manifest's method declarations to walk,
// Reconfigure resolves the highest semver-registered version of
// name and uses that one's manifest. Particles whose declared
// methods have changed across versions get the most recent
// view, which is the one the user is most likely thinking about.
//
// Behavior:
//   - Always prompts for the method (no idempotent skip).
//   - On a method change, the previously-configured credential is
//     removed only AFTER the new one writes successfully — a setup
//     error mid-flow leaves the prior state intact.
//   - The registry's SelectedAuthenticationMethod is updated last,
//     so a crash after credential setup but before that write
//     leaves the system in a self-correcting state (re-run picks
//     up the new method on the next reconfigure).
//
// Errors when the particle isn't registered, declares no
// credentials, or the chosen method's setup fails.
func Reconfigure(ctx context.Context, name string, opts Options) (registry.Entry, error) {
	if opts.Registry == nil {
		return registry.Entry{}, errors.New("importer: Registry is required")
	}
	if opts.Credentials == nil {
		return registry.Entry{}, errors.New("importer: Credentials store is required")
	}
	if opts.Prompter == nil {
		return registry.Entry{}, errors.New("importer: Prompter is required")
	}

	version, err := highestRegisteredVersion(ctx, opts.Registry, name)
	if err != nil {
		return registry.Entry{}, err
	}
	entry, err := opts.Registry.Get(ctx, name, version)
	if err != nil {
		return registry.Entry{}, fmt.Errorf("registry.Get %s@%s: %w", name, version, err)
	}

	manifest, err := readManifest(entry.Particle)
	if err != nil {
		return registry.Entry{}, err
	}
	creds, err := parseCredentialsCapability(manifest.CapabilitiesRaw)
	if err != nil {
		return registry.Entry{}, err
	}
	if creds == nil || len(creds.Methods) == 0 {
		return registry.Entry{}, fmt.Errorf("%s@%s declares no credentials to configure", name, version)
	}

	previous := entry.SelectedAuthenticationMethod
	if previous != "" {
		opts.Prompter.Info(fmt.Sprintf("Currently configured: %s", previous))
	}

	chosen, err := chooseAuthMethod(opts.Prompter, creds.Methods)
	if err != nil {
		return registry.Entry{}, err
	}
	opts.Prompter.Info(fmt.Sprintf("→ %s (%s) — %s", chosen.Name, chosen.Type, chosen.Description))
	if err := dispatchSetup(ctx, opts, name, chosen); err != nil {
		return registry.Entry{}, err
	}

	// Drop the previous credential only on success of the new
	// one, and only if it's a different method — otherwise the
	// new setup just overwrote it in place.
	if previous != "" && previous != chosen.Name {
		oldDesc, err := opts.Credentials.GetByName(ctx, name, previous)
		switch {
		case err == nil:
			if err := opts.Credentials.Delete(ctx, name, oldDesc.ID); err != nil {
				return registry.Entry{}, fmt.Errorf("delete previous credential %s: %w", previous, err)
			}
		case errors.Is(err, credentials.ErrNotFound):
			// Already gone; nothing to clean up.
		default:
			return registry.Entry{}, fmt.Errorf("look up previous credential %s: %w", previous, err)
		}
	}

	if err := opts.Registry.SetSelectedAuthenticationMethod(ctx, name, chosen.Name); err != nil {
		return registry.Entry{}, fmt.Errorf("record selected method: %w", err)
	}
	entry.SelectedAuthenticationMethod = chosen.Name
	return entry, nil
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

// setupCredentials enforces the "exactly one method configured"
// invariant: if any method is already set it returns that name;
// otherwise (when required) it prompts the user to pick one,
// runs that method's setup, and returns the method's name. Empty
// return + nil error means optional auth was declined / skipped.
func setupCredentials(ctx context.Context, opts Options, particle string, creds *credentialsCapability) (string, error) {
	existing, err := findConfiguredMethod(ctx, opts.Credentials, particle, creds.Methods)
	if err != nil {
		return "", err
	}
	if existing != "" {
		opts.Prompter.Info(fmt.Sprintf("✓ %s — authentication already configured", existing))
		return existing, nil
	}
	if !creds.Required {
		opts.Prompter.Info("Authentication is optional for this particle; skipping credential setup.")
		return "", nil
	}

	chosen, err := chooseAuthMethod(opts.Prompter, creds.Methods)
	if err != nil {
		return "", err
	}
	opts.Prompter.Info(fmt.Sprintf("→ %s (%s) — %s", chosen.Name, chosen.Type, chosen.Description))
	if err := dispatchSetup(ctx, opts, particle, chosen); err != nil {
		return "", err
	}
	return chosen.Name, nil
}

// findConfiguredMethod returns the name of any declared method
// already present in the store, or "" when none is. Lookup errors
// other than ErrNotFound propagate.
func findConfiguredMethod(ctx context.Context, store credentials.Store, particle string, methods []credentialDecl) (string, error) {
	for _, m := range methods {
		_, err := store.GetByName(ctx, particle, m.Name)
		if err == nil {
			return m.Name, nil
		}
		if !errors.Is(err, credentials.ErrNotFound) {
			return "", fmt.Errorf("lookup %s: %w", m.Name, err)
		}
	}
	return "", nil
}

// chooseAuthMethod short-circuits when the manifest declared
// exactly one method; otherwise prompts the user to pick.
func chooseAuthMethod(p Prompter, methods []credentialDecl) (credentialDecl, error) {
	if len(methods) == 1 {
		return methods[0], nil
	}
	options := make([]ChoiceOption, 0, len(methods))
	for _, m := range methods {
		opt := ChoiceOption{Value: m.Name, Label: m.Name + " (" + m.Type + ")"}
		if m.Description != "" {
			opt.Description = m.Description
		}
		options = append(options, opt)
	}
	chosen, err := p.Choice("Pick an authentication method:", options)
	if err != nil {
		return credentialDecl{}, err
	}
	for _, m := range methods {
		if m.Name == chosen {
			return m, nil
		}
	}
	return credentialDecl{}, fmt.Errorf("internal: chosen method %q not found", chosen)
}

func dispatchSetup(ctx context.Context, opts Options, particle string, decl credentialDecl) error {
	switch decl.Type {
	case "basic":
		return setupBasic(ctx, opts, particle, decl)
	case "apikey":
		return setupAPIKey(ctx, opts, particle, decl)
	case "signing-key":
		return setupSigningKey(ctx, opts, particle, decl)
	case "raw":
		return setupRaw(ctx, opts, particle, decl)
	case "oauth2":
		return setupOAuth2(ctx, opts, particle, decl)
	}
	return fmt.Errorf("unknown credential type %q", decl.Type)
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
}
