package runtime

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
)

// Manifest is the parsed shape of a particle's manifest.json — the
// file the build pipeline emits at the root of the particle FS.
//
// Capabilities is preserved as a map of raw JSON values so the
// runtime can detect which capability categories are declared
// (presence-only check) without committing to per-capability
// schemas it'd then have to evolve in lockstep with the build.
//
// Credentials sits at the top level rather than under capabilities:
// credentials describe what secret material the particle needs at
// substitution time, not a permission the runtime gates. The map
// is keyed by credential name (e.g., "github") and is left as raw
// JSON for the same reason as Capabilities — the runtime only
// needs presence + the `hosts` and `methods` shape it parses
// lazily via helpers below.
type Manifest struct {
	Name         string                     `json:"name"`
	Description  string                     `json:"description"`
	Version      string                     `json:"version"`
	Capabilities map[string]json.RawMessage `json:"capabilities"`
	Credentials  map[string]json.RawMessage `json:"credentials"`
	Tools        []ManifestTool             `json:"tools"`
}

// ManifestTool mirrors one entry in the manifest's `tools` array.
// InputSchema is left as raw JSON; the runtime compiles it on the
// host side at the WIT boundary.
type ManifestTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// readManifest parses manifest.json from the particle FS.
func readManifest(particleFS fs.FS) (Manifest, error) {
	data, err := fs.ReadFile(particleFS, "manifest.json")
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest.json: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse manifest.json: %w", err)
	}
	if m.Name == "" {
		return Manifest{}, fmt.Errorf("manifest.json: name is empty")
	}
	return m, nil
}

// declares reports whether the manifest declares the given capability
// category (one of "credentials", "oauth", "signing", "http",
// "sockets"). Presence is enough — values vary per category.
func (m Manifest) declares(capability string) bool {
	_, ok := m.Capabilities[capability]
	return ok
}

// httpCapability is the parsed shape of capabilities.http.
type httpCapability struct {
	AllowedHosts []string `json:"allowedHosts"`
}

// httpAllowedHosts returns the manifest's declared HTTP allow list,
// or nil if the http capability is not declared. Empty list (declared
// but empty) is distinguished from absence so the runtime treats
// "declared without allowedHosts" as "explicit deny-all".
func (m Manifest) httpAllowedHosts() (declared bool, hosts []string, err error) {
	raw, ok := m.Capabilities["http"]
	if !ok {
		return false, nil, nil
	}
	var hc httpCapability
	if err := json.Unmarshal(raw, &hc); err != nil {
		return false, nil, fmt.Errorf("manifest: capabilities.http: %w", err)
	}
	return true, hc.AllowedHosts, nil
}

// declaredCredentialNames returns every credential name declared
// at the manifest's top-level `credentials` map, sorted for
// deterministic iteration. nil when no credentials are declared.
//
// Spec-driven HTTP substitution iterates this list per outbound
// request, looking each name up in the Store to find the active
// method's apply-spec.
func (m Manifest) declaredCredentialNames() []string {
	names := make([]string, 0, len(m.Credentials))
	for n := range m.Credentials {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// credentialHosts returns the `hosts` list declared for the named
// credential, or nil when none is declared (or the credential
// isn't HTTP-bound). The HTTP policy uses this to refuse
// substituting credential X into a request for a host not in X's
// hosts list.
func (m Manifest) credentialHosts(name string) []string {
	raw, ok := m.Credentials[name]
	if !ok {
		return nil
	}
	var shell struct {
		Hosts []string `json:"hosts"`
	}
	if err := json.Unmarshal(raw, &shell); err != nil {
		return nil
	}
	return shell.Hosts
}

// credentialHostBindings returns a fresh map from credential name
// to the credential's `hosts` list, for every credential declared
// in the manifest. Empty / nil host list → the credential isn't
// host-bound (e.g., signing-key, raw) and the HTTP policy treats
// it as "substitute anywhere a placeholder shows up".
func credentialHostBindings(m Manifest) map[string][]string {
	if len(m.Credentials) == 0 {
		return nil
	}
	out := make(map[string][]string, len(m.Credentials))
	for name := range m.Credentials {
		hosts := m.credentialHosts(name)
		if len(hosts) > 0 {
			out[name] = hosts
		}
	}
	return out
}
