package importer

import (
	"encoding/json"
	"fmt"
	"sort"
)

// credentialsCapability mirrors the manifest's
// `capabilities.credentials` block: a top-level required flag plus
// a map of named alternative methods (the user picks one at setup).
type credentialsCapability struct {
	Required bool             `json:"required"`
	Methods  []credentialDecl `json:"-"` // populated from `methods` map for stable order
}

// credentialDecl is one entry in `capabilities.credentials.methods`.
// Type-specific fields (Flows, Algorithm, Location, URL overrides, …)
// are tolerated even when not applicable to a given Type — the build
// pipeline already validated the manifest's structure, so the
// importer trusts what's there.
type credentialDecl struct {
	Name        string `json:"-"` // populated from the map key
	Type        string `json:"type"`
	Description string `json:"description"`

	// oauth2-specific
	Flows            []string `json:"flows"`
	Scopes           []string `json:"scopes"`
	Provider         string   `json:"provider"`
	AuthorizationURL string   `json:"authorizationUrl"`
	TokenURL         string   `json:"tokenUrl"`
	DeviceAuthURL    string   `json:"deviceAuthUrl"`
	RevocationURL    string   `json:"revocationUrl"`

	// apikey-specific. nil → setup prompts for the location;
	// non-nil → use as-is, only the value is asked for.
	Location *applyLocation `json:"location"`

	// signing-key-specific
	Algorithm string `json:"algorithm"`
}

// applyLocation mirrors APIKeyApplyLocation in
// types/particle.d.ts: a discriminated union over `kind`. Name is
// for `header` / `query-param`; Scheme is for `auth-scheme`.
type applyLocation struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Scheme string `json:"scheme"`
}

// parseCredentialsCapability decodes the manifest's
// `capabilities.credentials` block. Returns (nil, nil) when the
// capability isn't declared.
func parseCredentialsCapability(caps map[string]json.RawMessage) (*credentialsCapability, error) {
	raw, ok := caps["credentials"]
	if !ok {
		return nil, nil
	}
	var shell struct {
		Required bool                       `json:"required"`
		Methods  map[string]json.RawMessage `json:"methods"`
	}
	if err := json.Unmarshal(raw, &shell); err != nil {
		return nil, fmt.Errorf("manifest.capabilities.credentials: %w", err)
	}
	out := &credentialsCapability{Required: shell.Required}
	for name, rawDecl := range shell.Methods {
		var d credentialDecl
		if err := json.Unmarshal(rawDecl, &d); err != nil {
			return nil, fmt.Errorf("manifest.capabilities.credentials.methods.%s: %w", name, err)
		}
		d.Name = name
		out.Methods = append(out.Methods, d)
	}
	// Sort so the prompt always presents methods in the same
	// order across runs.
	sort.Slice(out.Methods, func(i, j int) bool { return out.Methods[i].Name < out.Methods[j].Name })
	return out, nil
}
