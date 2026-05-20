package runtime

import (
	"encoding/json"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

// Round-trip of a fully-populated manifest covering every
// supported credential method type — pins both the unmarshal
// (typed dispatch on `type`) and the marshal (flatten the
// type-specific sub-struct back into the top-level object).
func TestParseManifest_AllMethodKinds(t *testing.T) {
	src := `{
		"name": "kitchen-sink",
		"description": "Exercises every supported manifest field.",
		"version": "1.2.3",
		"capabilities": {
			"http": { "allowedHosts": ["api.example.com", "api.other.com"] }
		},
		"credentials": {
			"github": {
				"hosts": ["api.example.com"],
				"required": true,
				"methods": {
					"oauth": {
						"type": "oauth2",
						"description": "OAuth flow",
						"flows": ["authorization-code", "device-code"],
						"scopes": ["repo"],
						"authorizationUrl": "https://example.com/auth",
						"tokenUrl": "https://example.com/token"
					},
					"pat": {
						"type": "apikey",
						"location": { "kind": "auth-scheme", "scheme": "Bearer" }
					},
					"basic": { "type": "basic" },
					"sig":   { "type": "signing-key", "algorithm": "hmac-sha256" },
					"raw":   { "type": "raw" }
				}
			}
		},
		"tools": [{ "name": "echo", "description": "echo", "inputSchema": { "type": "object" } }]
	}`

	m, err := ParseManifest(strings.NewReader(src))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}

	if m.Name != "kitchen-sink" || m.Version != "1.2.3" {
		t.Errorf("name/version = %q/%q", m.Name, m.Version)
	}
	if len(m.Capabilities.HTTP.AllowedHosts) != 2 {
		t.Errorf("http allowedHosts = %+v", m.Capabilities.HTTP)
	}

	gh := m.Credentials["github"]
	if len(gh.Hosts) != 1 || gh.Hosts[0] != "api.example.com" {
		t.Errorf("github.hosts = %v", gh.Hosts)
	}
	if !gh.Required {
		t.Error("github.required = false, want true")
	}

	// Discriminated-union dispatch: each method routes to the
	// matching typed sub-struct, the others stay nil.
	oauth := gh.Methods["oauth"]
	if oauth.Kind != MethodOAuth2 || oauth.OAuth2 == nil ||
		oauth.OAuth2.AuthorizationURL != "https://example.com/auth" ||
		len(oauth.OAuth2.Flows) != 2 || oauth.OAuth2.Flows[0] != OAuth2FlowAuthorizationCode {
		t.Errorf("oauth = %+v / %+v", oauth, oauth.OAuth2)
	}
	if oauth.APIKey != nil || oauth.SigningKey != nil {
		t.Error("oauth shouldn't populate APIKey/SigningKey")
	}

	pat := gh.Methods["pat"]
	if pat.Kind != MethodAPIKey || pat.APIKey == nil || pat.APIKey.Location == nil ||
		pat.APIKey.Location.Kind != APIKeyLocationAuthScheme || pat.APIKey.Location.Scheme != "Bearer" {
		t.Errorf("pat = %+v / %+v", pat, pat.APIKey)
	}

	basic := gh.Methods["basic"]
	if basic.Kind != MethodBasic || basic.OAuth2 != nil || basic.APIKey != nil || basic.SigningKey != nil {
		t.Errorf("basic = %+v", basic)
	}

	sig := gh.Methods["sig"]
	if sig.Kind != MethodSigningKey || sig.SigningKey == nil || sig.SigningKey.Algorithm != SigningHMACSHA256 {
		t.Errorf("sig = %+v / %+v", sig, sig.SigningKey)
	}

	raw := gh.Methods["raw"]
	if raw.Kind != MethodRaw || raw.OAuth2 != nil || raw.APIKey != nil || raw.SigningKey != nil {
		t.Errorf("raw = %+v", raw)
	}

	// Round-trip: marshal back, then re-decode, then compare.
	// MarshalJSON has to flatten the sub-struct fields so the
	// output matches the input shape.
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m2 Manifest
	if err := json.Unmarshal(out, &m2); err != nil {
		t.Fatalf("re-decode: %v\n%s", err, out)
	}
	if m2.Credentials["github"].Methods["oauth"].OAuth2.AuthorizationURL != "https://example.com/auth" {
		t.Errorf("round-trip lost OAuth2 fields:\n%s", out)
	}
	if m2.Credentials["github"].Methods["sig"].SigningKey.Algorithm != SigningHMACSHA256 {
		t.Errorf("round-trip lost SigningKey fields:\n%s", out)
	}
}

// A missing or empty `http` capability yields an empty
// AllowedHosts slice — either way, the policy denies every
// outbound request. The two manifest shapes are
// indistinguishable to the runtime by design.
func TestParseManifest_NoHTTP(t *testing.T) {
	cases := []string{
		`{"name":"p","version":"0.1.0","capabilities":{},"tools":[]}`,
		`{"name":"p","version":"0.1.0","capabilities":{"http":{}},"tools":[]}`,
	}
	for _, src := range cases {
		m, err := ParseManifest(strings.NewReader(src))
		if err != nil {
			t.Fatalf("ParseManifest: %v\nsrc: %s", err, src)
		}
		if len(m.Capabilities.HTTP.AllowedHosts) != 0 {
			t.Errorf("AllowedHosts = %v, want empty (deny-all)", m.Capabilities.HTTP.AllowedHosts)
		}
		if len(m.Credentials) != 0 {
			t.Errorf("Credentials = %v, want empty", m.Credentials)
		}
	}
}

// Missing required fields fail with actionable errors so a
// hand-crafted manifest doesn't silently slip past the registry
// (which keys on name+version).
func TestParseManifest_RequiresNameAndVersion(t *testing.T) {
	cases := []struct {
		name string
		json string
		want string
	}{
		{"missing name", `{"version":"0.1.0"}`, "missing name"},
		{"empty name", `{"name":"","version":"0.1.0"}`, "missing name"},
		{"missing version", `{"name":"p"}`, "missing version"},
		{"empty version", `{"name":"p","version":""}`, "missing version"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseManifest(strings.NewReader(c.json))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want substring %q", err, c.want)
			}
		})
	}
}

// Unknown credential method types fail loud — silently dropping
// a method would mean the runtime treats it as not-declared and
// the user's intent (give me PAT support!) goes nowhere. The
// build pipeline already rejects unknown caps; the runtime
// pickup matches.
func TestParseManifest_UnknownMethodType_Errors(t *testing.T) {
	src := `{
		"name": "p", "version": "0.1.0", "capabilities": {}, "tools": [],
		"credentials": { "github": { "methods": { "x": { "type": "telepathy" } } } }
	}`
	_, err := ParseManifest(strings.NewReader(src))
	if err == nil {
		t.Fatal("expected error for unknown method type")
	}
	if !strings.Contains(err.Error(), "telepathy") {
		t.Errorf("err = %v, want it to name the offending type", err)
	}
}

// Missing-type also fails — the discriminator is what the rest
// of the parser switches on, so an empty `type` is unrecoverable.
func TestParseManifest_MissingMethodType_Errors(t *testing.T) {
	src := `{
		"name": "p", "version": "0.1.0", "capabilities": {}, "tools": [],
		"credentials": { "github": { "methods": { "x": { "description": "no type" } } } }
	}`
	_, err := ParseManifest(strings.NewReader(src))
	if err == nil || !strings.Contains(err.Error(), "missing type") {
		t.Errorf("err = %v, want one mentioning missing type", err)
	}
}

// LoadManifest is the FS-rooted convenience wrapper: it reads
// manifest.json out of the FS and threads through ParseManifest.
// A missing file surfaces a wrapped error; the underlying
// fs.ErrNotExist remains comparable.
func TestLoadManifest(t *testing.T) {
	fsys := fstest.MapFS{
		"manifest.json": &fstest.MapFile{Data: []byte(`{
			"name": "p", "version": "0.1.0",
			"capabilities": { "http": { "allowedHosts": ["api.example.com"] } },
			"tools": []
		}`)},
	}
	m, err := LoadManifest(fsys)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m.Name != "p" || len(m.Capabilities.HTTP.AllowedHosts) == 0 || m.Capabilities.HTTP.AllowedHosts[0] != "api.example.com" {
		t.Errorf("loaded = %+v", m)
	}
}

func TestLoadManifest_MissingFile(t *testing.T) {
	_, err := LoadManifest(fstest.MapFS{})
	if err == nil {
		t.Fatal("expected error for missing manifest.json")
	}
	// The fs.ErrNotExist sentinel propagates through wrapping
	// so callers can errors.Is against it.
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, expected to wrap fs.ErrNotExist", err)
	}
}

// Runtime parsing: empty / unset defaults to JS via
// ResolvedRuntime, explicit "js" and "python" pass through, anything
// else is rejected at parse time so a typo can't silently land in
// the registry and confuse the host's dispatch.
func TestParseManifest_Runtime(t *testing.T) {
	base := `{"name":"p","version":"0.1.0","capabilities":{},"tools":[]`

	t.Run("default (omitted) → js", func(t *testing.T) {
		m, err := ParseManifest(strings.NewReader(base + "}"))
		if err != nil {
			t.Fatalf("ParseManifest: %v", err)
		}
		if m.Runtime != "" {
			t.Errorf("Runtime = %q, want empty", m.Runtime)
		}
		if got := m.ResolvedRuntime(); got != RuntimeJS {
			t.Errorf("ResolvedRuntime() = %q, want %q", got, RuntimeJS)
		}
	})

	t.Run("explicit js", func(t *testing.T) {
		m, err := ParseManifest(strings.NewReader(base + `,"runtime":"js"}`))
		if err != nil {
			t.Fatalf("ParseManifest: %v", err)
		}
		if got := m.ResolvedRuntime(); got != RuntimeJS {
			t.Errorf("ResolvedRuntime() = %q, want %q", got, RuntimeJS)
		}
	})

	t.Run("python", func(t *testing.T) {
		m, err := ParseManifest(strings.NewReader(base + `,"runtime":"python"}`))
		if err != nil {
			t.Fatalf("ParseManifest: %v", err)
		}
		if got := m.ResolvedRuntime(); got != RuntimePython {
			t.Errorf("ResolvedRuntime() = %q, want %q", got, RuntimePython)
		}
	})

	t.Run("unknown rejected", func(t *testing.T) {
		_, err := ParseManifest(strings.NewReader(base + `,"runtime":"ruby"}`))
		if err == nil || !strings.Contains(err.Error(), "unknown runtime") {
			t.Errorf("err = %v, want unknown-runtime", err)
		}
	})
}

// credentialHostBindings returns only credentials that have
// hosts set — non-HTTP credentials (signing-key, raw) sit at
// the top-level credentials map without `hosts` and shouldn't
// appear in the policy's host-binding map.
func TestCredentialHostBindings_SkipsHostless(t *testing.T) {
	m := Manifest{Credentials: map[string]Credential{
		"github":  {Hosts: []string{"api.github.com"}},
		"signing": {}, // no Hosts → not HTTP-bound
	}}
	got := credentialHostBindings(m)
	if _, ok := got["github"]; !ok {
		t.Error("github missing from bindings")
	}
	if _, ok := got["signing"]; ok {
		t.Error("hostless signing credential should not appear in bindings")
	}
}
