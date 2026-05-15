package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/partite-ai/particle/registry"
)

// PermissionMode controls when Import prompts the user to
// confirm the manifest's declared capabilities. Default is
// [PermissionAuto] — only prompt when the capabilities differ
// from the previously-registered version (or there is no prior
// version).
type PermissionMode int

const (
	// PermissionAuto prompts only when the capabilities or
	// credentials differ from the most recent registered
	// version. New installs always prompt. Particles declaring
	// neither never prompt.
	PermissionAuto PermissionMode = iota

	// PermissionSkip auto-accepts every permission declaration.
	// Use for CI / scripted installs where there's no human at
	// the keyboard to answer y/n.
	PermissionSkip

	// PermissionForce prompts on every install, even when the
	// permission set matches the prior version. Useful when the
	// user wants to re-confirm — or has explicitly asked to.
	PermissionForce
)

// confirmPermissions surfaces the manifest's capabilities +
// credentials to the user and asks them to confirm. Behavior is
// governed by opts.PermissionMode; see [PermissionMode] for the
// three modes.
//
// Returns nil when the install is approved (either by user
// confirmation or because we never prompted). Returns an error
// when the user declines.
func confirmPermissions(ctx context.Context, opts Options, mf manifest) error {
	nextCaps := mf.CapabilitiesRaw
	nextCreds := mf.CredentialsRaw

	prevCaps, prevCreds, prevVer, _ := loadPriorPermissions(ctx, opts.Registry, mf.Name)

	switch opts.PermissionMode {
	case PermissionSkip:
		return nil
	case PermissionForce:
		// fall through to prompt
	case PermissionAuto:
		// No capabilities AND no credentials → nothing to confirm.
		if len(nextCaps) == 0 && len(nextCreds) == 0 {
			return nil
		}
		// Prior version with identical permissions → silent reinstall.
		if prevVer != "" {
			capsSame, err := jsonEqual(prevCaps, nextCaps)
			if err != nil {
				return fmt.Errorf("compare capabilities: %w", err)
			}
			credsSame, err := jsonEqual(prevCreds, nextCreds)
			if err != nil {
				return fmt.Errorf("compare credentials: %w", err)
			}
			if capsSame && credsSame {
				return nil
			}
		}
	}

	if opts.Prompter == nil {
		return errors.New("importer: permission confirmation requires a Prompter")
	}
	summary := formatPermissions(mf, nextCaps, nextCreds, prevVer)
	opts.Prompter.Info(summary)
	ok, err := opts.Prompter.Confirm("Allow these permissions?", false)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("permissions declined")
	}
	return nil
}

// loadPriorPermissions returns the capabilities + credentials of
// the most recently registered version of name, plus that
// version. Returns ("", nil, nil, nil) when the particle has
// never been registered — that's the "fresh install" case the
// caller treats as "must prompt".
func loadPriorPermissions(ctx context.Context, reg registry.Registry, name string) (caps, creds map[string]json.RawMessage, version string, err error) {
	ver, err := highestRegisteredVersion(ctx, reg, name)
	if err != nil {
		// "not registered" isn't a real failure here — it just
		// means the install is fresh. Surface unexpected errors
		// (registry storage failure, etc.).
		if strings.Contains(err.Error(), "not registered") {
			return nil, nil, "", nil
		}
		return nil, nil, "", err
	}
	entry, err := reg.Get(ctx, name, ver)
	if err != nil {
		return nil, nil, "", err
	}
	mf, err := readManifest(entry.Particle)
	if err != nil {
		return nil, nil, ver, fmt.Errorf("read prior manifest: %w", err)
	}
	return mf.CapabilitiesRaw, mf.CredentialsRaw, ver, nil
}

// jsonEqual reports whether two manifest blocks describe the same
// JSON tree, regardless of key ordering or whitespace. Implemented
// by canonicalizing both sides through a generic decoder
// (map[string]any) — which has stable equality semantics — and
// comparing the canonical bytes.
func jsonEqual(a, b map[string]json.RawMessage) (bool, error) {
	ca, err := canonical(a)
	if err != nil {
		return false, err
	}
	cb, err := canonical(b)
	if err != nil {
		return false, err
	}
	return string(ca) == string(cb), nil
}

// canonical produces a deterministic JSON encoding of v: any
// map[string]X is emitted with sorted keys (json.Marshal's stable
// behavior), so two semantically-equal but textually-different
// inputs serialize identically.
func canonical(v any) ([]byte, error) {
	// Round-trip through the generic decoder to flatten
	// json.RawMessage / typed structs into map[string]any /
	// []any / primitives — those marshal with sorted keys.
	bytes, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal(bytes, &decoded); err != nil {
		return nil, err
	}
	return json.Marshal(decoded)
}

// formatPermissions builds the human-readable summary shown
// before the y/n prompt. Aims for "scannable in 2 seconds" —
// indentation conveys the manifest's structure, one line per
// notable capability / credential.
func formatPermissions(mf manifest, caps, creds map[string]json.RawMessage, prevVer string) string {
	var b strings.Builder
	header := fmt.Sprintf("%s@%s requests:", mf.Name, mf.Version)
	if prevVer != "" {
		header = fmt.Sprintf("%s@%s requests these permissions (changed from %s):", mf.Name, mf.Version, prevVer)
	}
	b.WriteString(header)
	b.WriteString("\n")

	if len(caps) == 0 && len(creds) == 0 {
		b.WriteString("\n  (no permissions)\n")
		return b.String()
	}

	capKeys := make([]string, 0, len(caps))
	for k := range caps {
		capKeys = append(capKeys, k)
	}
	sort.Strings(capKeys)
	for _, k := range capKeys {
		fmt.Fprintln(&b)
		writeCapability(&b, k, caps[k])
	}

	if len(creds) > 0 {
		fmt.Fprintln(&b)
		writeCredentials(&b, creds)
	}
	return b.String()
}

func writeCapability(b *strings.Builder, name string, raw json.RawMessage) {
	switch name {
	case "http":
		var v struct {
			AllowedHosts []string `json:"allowedHosts"`
		}
		_ = json.Unmarshal(raw, &v)
		fmt.Fprintf(b, "  HTTP — outbound to:\n")
		if len(v.AllowedHosts) == 0 {
			fmt.Fprintf(b, "    (no hosts declared)\n")
			return
		}
		for _, h := range v.AllowedHosts {
			fmt.Fprintf(b, "    %s\n", h)
		}
	case "sockets":
		var v struct {
			AllowedEndpoints []struct {
				Host string `json:"host"`
				Port int    `json:"port"`
			} `json:"allowedEndpoints"`
		}
		_ = json.Unmarshal(raw, &v)
		fmt.Fprintf(b, "  Sockets — outbound TCP/UDP:\n")
		if len(v.AllowedEndpoints) == 0 {
			fmt.Fprintf(b, "    (no endpoints declared)\n")
			return
		}
		for _, e := range v.AllowedEndpoints {
			fmt.Fprintf(b, "    %s:%d\n", e.Host, e.Port)
		}
	default:
		// Unknown capability category — print as raw JSON so
		// the user can still see what they're agreeing to.
		fmt.Fprintf(b, "  %s — %s\n", name, string(raw))
	}
}

// writeCredentials renders the top-level credentials block as a
// sorted list of named credentials with their host scope and
// methods. Host scope matters most for review — "this credential
// will be sent to api.openai.com" is the part the user has to
// actually trust. Multiple methods are explicitly framed as
// alternatives ("pick one of") so the reader doesn't think
// they're additive.
func writeCredentials(b *strings.Builder, creds map[string]json.RawMessage) {
	names := make([]string, 0, len(creds))
	for n := range creds {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Fprintf(b, "  Credentials:\n")
	for _, n := range names {
		var c struct {
			Hosts    []string                   `json:"hosts"`
			Required bool                       `json:"required"`
			Methods  map[string]json.RawMessage `json:"methods"`
		}
		_ = json.Unmarshal(creds[n], &c)
		req := "optional"
		if c.Required {
			req = "required"
		}
		fmt.Fprintf(b, "    %s — %s", n, req)
		if len(c.Hosts) > 0 {
			fmt.Fprintf(b, ", on %s", strings.Join(c.Hosts, ", "))
		}
		fmt.Fprintln(b)

		methodNames := make([]string, 0, len(c.Methods))
		for mn := range c.Methods {
			methodNames = append(methodNames, mn)
		}
		sort.Strings(methodNames)
		// Headline the alternative semantics when there's a choice
		// — without this, "oauth" + "pat" reads as both being
		// required, instead of "user picks one".
		if len(methodNames) > 1 {
			fmt.Fprintf(b, "      authenticate with one of:\n")
		}
		for _, mn := range methodNames {
			writeCredentialMethod(b, mn, c.Methods[mn], len(methodNames) > 1)
		}
	}
}

// writeCredentialMethod renders one method declaration inside a
// credential. For oauth2 it surfaces the authorizationUrl /
// tokenUrl (so a hostile manifest can't slip a phishing URL past
// a user who only read the install prompt); for apikey it
// surfaces the apply-spec location (so the user can tell whether
// their key will be sent in a header that CDNs see vs. in a URL
// parameter that gets logged everywhere).
//
// `nested` indents one level deeper when the credential offers
// multiple alternative methods (sits under an "authenticate with
// one of:" subhead); the single-method case skips that frame and
// renders the method flush with the credential header.
func writeCredentialMethod(b *strings.Builder, name string, raw json.RawMessage, nested bool) {
	var m struct {
		Type        string `json:"type"`
		Description string `json:"description"`

		// OAuth2 fields. Provider-supplied defaults aren't
		// surfaced — only what the manifest itself locks in,
		// which is the attack surface.
		AuthorizationURL string   `json:"authorizationUrl"`
		TokenURL         string   `json:"tokenUrl"`
		DeviceAuthURL    string   `json:"deviceAuthUrl"`
		Provider         string   `json:"provider"`
		Scopes           []string `json:"scopes"`

		// API-key location.
		Location *struct {
			Kind   string `json:"kind"`
			Name   string `json:"name"`
			Scheme string `json:"scheme"`
		} `json:"location"`
	}
	_ = json.Unmarshal(raw, &m)

	indent, detailIndent := "      ", "        "
	if nested {
		indent, detailIndent = "        ", "          "
	}

	label := fmt.Sprintf("%s (%s)", name, m.Type)
	if m.Description != "" {
		label += " — " + m.Description
	}
	fmt.Fprintf(b, "%s%s\n", indent, label)

	switch m.Type {
	case "oauth2":
		if m.Provider != "" {
			fmt.Fprintf(b, "%sprovider: %s\n", detailIndent, m.Provider)
		}
		if m.AuthorizationURL != "" {
			fmt.Fprintf(b, "%sauthorize: %s\n", detailIndent, m.AuthorizationURL)
		}
		if m.TokenURL != "" {
			fmt.Fprintf(b, "%stoken:     %s\n", detailIndent, m.TokenURL)
		}
		if m.DeviceAuthURL != "" {
			fmt.Fprintf(b, "%sdevice:    %s\n", detailIndent, m.DeviceAuthURL)
		}
		if len(m.Scopes) > 0 {
			fmt.Fprintf(b, "%sscopes:    %s\n", detailIndent, strings.Join(m.Scopes, ", "))
		}
	case "apikey":
		if m.Location != nil {
			fmt.Fprintf(b, "%ssent via:  %s\n", detailIndent, describeAPIKeyLocation(m.Location.Kind, m.Location.Name, m.Location.Scheme))
		}
	}
}

// describeAPIKeyLocation produces the same wording the credential
// setup prompt uses, so the install-time summary and the runtime
// setup describe the substitution identically.
func describeAPIKeyLocation(kind, name, scheme string) string {
	switch kind {
	case "header":
		return name + ": <value>"
	case "auth-scheme":
		return "Authorization: " + scheme + " <value>"
	case "query-param":
		return "?" + name + "=<value>"
	}
	return "(unrecognized location)"
}
