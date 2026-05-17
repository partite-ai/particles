package importer

import (
	"encoding/json"
	"fmt"
	"sort"
)

// credentialDecl mirrors one entry in the manifest's top-level
// `credentials` map: a name, optional host binding, an optional
// required flag, and one-or-more alternative methods the user
// picks from at setup. The `Name` field is populated from the
// map key during parsing.
type credentialDecl struct {
	Name     string             `json:"-"`
	Hosts    []string           `json:"hosts"`
	Required bool               `json:"required"`
	Methods  []credentialMethod `json:"-"` // populated from `methods` map for stable order
}

// credentialMethod is one entry in `credentials.<name>.methods`.
// Type-specific fields (Flows, Algorithm, Location, URL overrides, …)
// are tolerated even when not applicable to a given Type — the build
// pipeline already validated the manifest's structure, so the
// importer trusts what's there.
type credentialMethod struct {
	Name        string `json:"-"` // populated from the map key
	Type        string `json:"type"`
	Description string `json:"description"`

	// oauth2-specific
	Flows            []string `json:"flows"`
	Scopes           []string `json:"scopes"`
	AuthorizationURL string   `json:"authorizationUrl"`
	TokenURL         string   `json:"tokenUrl"`
	DeviceAuthURL    string   `json:"deviceAuthUrl"`

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

// parseCredentials decodes the manifest's top-level `credentials`
// map into a sorted slice (sorted by credential name) of decls.
// Within each decl, methods are also sorted by name. Returns
// (nil, nil) when no credentials are declared.
func parseCredentials(raw map[string]json.RawMessage) ([]credentialDecl, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]credentialDecl, 0, len(raw))
	for name, rawDecl := range raw {
		var shell struct {
			Hosts    []string                   `json:"hosts"`
			Required bool                       `json:"required"`
			Methods  map[string]json.RawMessage `json:"methods"`
		}
		if err := json.Unmarshal(rawDecl, &shell); err != nil {
			return nil, fmt.Errorf("manifest.credentials.%s: %w", name, err)
		}
		decl := credentialDecl{
			Name:     name,
			Hosts:    shell.Hosts,
			Required: shell.Required,
		}
		for mname, rawMethod := range shell.Methods {
			var m credentialMethod
			if err := json.Unmarshal(rawMethod, &m); err != nil {
				return nil, fmt.Errorf("manifest.credentials.%s.methods.%s: %w", name, mname, err)
			}
			m.Name = mname
			decl.Methods = append(decl.Methods, m)
		}
		sort.Slice(decl.Methods, func(i, j int) bool { return decl.Methods[i].Name < decl.Methods[j].Name })
		out = append(out, decl)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
