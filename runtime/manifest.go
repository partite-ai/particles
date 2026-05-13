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
type Manifest struct {
	Name         string                     `json:"name"`
	Description  string                     `json:"description"`
	Version      string                     `json:"version"`
	Capabilities map[string]json.RawMessage `json:"capabilities"`
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

// declaredCredentialNames returns every credential method name
// the manifest declares under
// `capabilities.credentials.methods`, sorted for deterministic
// iteration. nil when the capability is not declared.
//
// Spec-driven HTTP substitution iterates this list per outbound
// request, which is why we don't care about the per-method
// declaration fields here — only the names.
func (m Manifest) declaredCredentialNames() []string {
	raw, ok := m.Capabilities["credentials"]
	if !ok {
		return nil
	}
	var shell struct {
		Methods map[string]json.RawMessage `json:"methods"`
	}
	if err := json.Unmarshal(raw, &shell); err != nil {
		return nil
	}
	names := make([]string, 0, len(shell.Methods))
	for n := range shell.Methods {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
