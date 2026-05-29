package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strconv"
	"strings"
)

// Manifest is the parsed contents of a particle's manifest.json
// — the file the build pipeline emits at the root of the
// particle FS.
type Manifest struct {
	Name         string                `json:"name"`
	Description  string                `json:"description"`
	Version      string                `json:"version"`
	// Runtime selects which engine the host instantiates for this
	// particle: the QuickJS-based JS runtime (`"js"`) or the
	// CPython-based Python runtime (`"python"`). Omitted /
	// empty-string is treated as `"js"` so manifests authored
	// before the field existed keep working. Use ResolvedRuntime
	// to read the effective value with defaulting applied.
	Runtime      RuntimeKind           `json:"runtime,omitempty"`
	Capabilities Capabilities          `json:"capabilities"`
	Credentials  map[string]Credential `json:"credentials,omitempty"`
	Tools        []ManifestTool        `json:"tools"`
}

// RuntimeKind names a guest engine. The build pipeline picks the
// engine based on the source language (Particlefile.ts/.js → js,
// Particlefile.py → python, Particlefile.wasm → wasm); the host
// picks which embedded wasm to instantiate based on the same value
// at run time.
//
// `js` and `python` reference shared, preloaded runtime images
// (particle-js-runtime.wasm / particle-python-runtime.wasm). `wasm`
// has no shared image — the particle's own component IS the runtime,
// loaded from particle.wasm in the artifact at instantiation time.
type RuntimeKind string

const (
	RuntimeJS     RuntimeKind = "js"
	RuntimePython RuntimeKind = "python"
	RuntimeWasm   RuntimeKind = "wasm"
)

// ResolvedRuntime returns the effective RuntimeKind, treating an
// empty value as RuntimeJS. All callers that need to switch on the
// engine should go through this rather than reading Runtime
// directly, so a future default change is a single edit.
func (m Manifest) ResolvedRuntime() RuntimeKind {
	if m.Runtime == "" {
		return RuntimeJS
	}
	return m.Runtime
}

// Capabilities holds the runtime-policy-relevant declarations.
// The struct shape leaves room to add more without breaking
// callers.
type Capabilities struct {
	HTTP       HTTPCapability       `json:"http"`
	Filesystem FilesystemCapability `json:"filesystem"`
}

// HTTPCapability mirrors `capabilities.http`. An empty
// AllowedHosts means no outbound destinations are permitted —
// the policy denies every request.
type HTTPCapability struct {
	AllowedHosts []string `json:"allowedHosts"`
}

// FilesystemCapability mirrors `capabilities.filesystem`. Mounts
// are host directories the particle may read (and, when declared
// read-write, write); the host maps each to a real directory at
// install or run time. Temp mounts are scratch areas the host
// provisions fresh per run, size-caps, and clears on teardown.
//
// Both maps are keyed by mount name — the stable identifier the
// `particle mount` command and `--mount name=path` flag refer to.
// The guest opens files at the declared Path; the name never
// appears inside the sandbox.
type FilesystemCapability struct {
	Mounts map[string]MountDecl     `json:"mounts,omitempty"`
	Temp   map[string]TempMountDecl `json:"temp,omitempty"`
}

// MountDecl is one entry in `capabilities.filesystem.mounts`.
type MountDecl struct {
	// Description is the human-facing explanation shown in the
	// install prompt and `particle mount` listings. Required.
	Description string `json:"description"`

	// Path is the absolute location the mount appears at inside
	// the sandbox; the handler opens files here.
	Path string `json:"path"`

	// Access is "readonly" or "readwrite". The runtime enforces
	// it by wrapping a read-only mount's FS so writes are
	// rejected regardless of what the host supplied.
	Access MountAccess `json:"access"`

	// Required refuses to *run* the particle until a host
	// directory is mapped (via `particle mount` or `--mount`).
	// Unlike credentials, a missing required mount does not block
	// install — it's enforced at boot.
	Required bool `json:"required"`
}

// MountAccess is the read/write mode of a declared mount.
type MountAccess string

const (
	MountReadOnly  MountAccess = "readonly"
	MountReadWrite MountAccess = "readwrite"
)

// TempMountDecl is one entry in `capabilities.filesystem.temp`. A
// temp mount is always read-write; the host gives it a fresh empty
// directory each run, enforces MaxSize bytes of file content, and
// removes it when the particle exits.
type TempMountDecl struct {
	// Description is the human-facing explanation shown at install.
	// Required.
	Description string `json:"description"`

	// Path is the absolute location the scratch dir appears at
	// inside the sandbox.
	Path string `json:"path"`

	// MaxSize caps total file-content bytes. A bare integer is
	// bytes; a unit suffix (KB/MB/GB) multiplies by 1024^n. See
	// [ParseByteSize]. Required.
	MaxSize string `json:"maxSize"`
}

// MaxSizeBytes parses MaxSize into a byte count via [ParseByteSize].
func (t TempMountDecl) MaxSizeBytes() (int64, error) {
	return ParseByteSize(t.MaxSize)
}

// MarshalJSON renders Capabilities with `http` always present (its
// historical shape) and `filesystem` emitted only when it declares at
// least one mount or temp mount. Omitting an empty filesystem block
// keeps manifest.json byte-identical for the many particles that
// declare no mounts. A struct `json:",omitempty"` can't express this —
// omitempty never elides a non-pointer struct — hence the hand-rolled
// marshaler.
func (c Capabilities) MarshalJSON() ([]byte, error) {
	out := map[string]any{"http": c.HTTP}
	if len(c.Filesystem.Mounts) > 0 || len(c.Filesystem.Temp) > 0 {
		out["filesystem"] = c.Filesystem
	}
	return json.Marshal(out)
}

// Credential is one entry in the manifest's top-level
// `credentials` map. The key is the credential's name (e.g.,
// "github"); this struct holds everything underneath.
type Credential struct {
	// Hosts pins the credential to a set of HTTP destinations.
	// Empty / nil → the credential is not HTTP-bound (e.g.,
	// signing-key or raw, consumed entirely through the
	// JS-side API).
	Hosts []string `json:"hosts"`

	// Required tells the importer to refuse to register the
	// particle without a configured method.
	Required bool `json:"required"`

	// Methods is the set of authentication methods the user
	// may pick from at setup. Exactly one is configured per
	// credential.
	Methods map[string]CredentialMethod `json:"methods"`
}

// CredentialMethodKind enumerates the supported authentication
// method variants. Matches the `type:` strings used in the
// manifest JSON; safe to compare against the constants below or
// the matching credentials.Kind values from the store.
type CredentialMethodKind string

const (
	MethodBasic      CredentialMethodKind = "basic"
	MethodOAuth2     CredentialMethodKind = "oauth2"
	MethodAPIKey     CredentialMethodKind = "apikey"
	MethodSigningKey CredentialMethodKind = "signing-key"
	MethodRaw        CredentialMethodKind = "raw"
)

// CredentialMethod is a discriminated union over Kind. Exactly
// one of the typed sub-pointers is non-nil per kind:
//
//	Kind == MethodOAuth2     → OAuth2     != nil
//	Kind == MethodAPIKey     → APIKey     != nil
//	Kind == MethodSigningKey → SigningKey != nil
//	Kind == MethodBasic / MethodRaw → all sub-pointers nil
//
// Callers switch on Kind and read the matching pointer.
type CredentialMethod struct {
	Kind        CredentialMethodKind
	Description string

	OAuth2     *OAuth2Method
	APIKey     *APIKeyMethod
	SigningKey *SigningKeyMethod
}

// OAuth2Method is the per-method shape for `type: "oauth2"`.
// Optional URLs are plain strings (empty = unset); a manifest
// either pins them or the importer prompts the user at setup.
type OAuth2Method struct {
	Flows            []OAuth2Flow `json:"flows"`
	Scopes           []string     `json:"scopes"`
	AuthorizationURL string       `json:"authorizationUrl"`
	TokenURL         string       `json:"tokenUrl"`
	DeviceAuthURL    string       `json:"deviceAuthUrl"`
}

// OAuth2Flow is the OAuth 2.0 flow the importer should run.
type OAuth2Flow string

const (
	OAuth2FlowAuthorizationCode     OAuth2Flow = "authorization-code"
	OAuth2FlowAuthorizationCodePKCE OAuth2Flow = "authorization-code-pkce"
	OAuth2FlowDeviceCode            OAuth2Flow = "device-code"
)

// APIKeyMethod is the per-method shape for `type: "apikey"`.
// Location is optional in the manifest — when omitted, the
// importer prompts the user for it.
type APIKeyMethod struct {
	Location *APIKeyLocation `json:"location"`
}

// APIKeyLocation describes where a credential's value gets
// substituted in an outgoing HTTP request.
type APIKeyLocation struct {
	Kind   APIKeyLocationKind `json:"kind"`
	Name   string             `json:"name"`
	Scheme string             `json:"scheme"`
}

// APIKeyLocationKind enumerates the apikey substitution shapes.
type APIKeyLocationKind string

const (
	APIKeyLocationHeader     APIKeyLocationKind = "header"
	APIKeyLocationAuthScheme APIKeyLocationKind = "auth-scheme"
	APIKeyLocationQueryParam APIKeyLocationKind = "query-param"
)

// SigningKeyMethod is the per-method shape for
// `type: "signing-key"`.
type SigningKeyMethod struct {
	Algorithm SigningAlgorithm `json:"algorithm"`
}

// SigningAlgorithm names a HMAC variant. v1 supports SHA-256
// and SHA-512; RSA / ECDSA are phase 2.
type SigningAlgorithm string

const (
	SigningHMACSHA256 SigningAlgorithm = "hmac-sha256"
	SigningHMACSHA512 SigningAlgorithm = "hmac-sha512"
)

// ManifestTool is one entry in the manifest's `tools` array.
// InputSchema is kept as raw JSON: it's the author's JSON Schema
// and the runtime compiles it on the host side at the WIT
// boundary.
//
// Distinct from [ToolDef] (the live wasm-side `list-tools`
// result), which is what callers should use when they want what
// the bundle actually exposes — the manifest is a static
// declaration, the wasm export is the source of truth at run
// time.
type ManifestTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// -----------------------------------------------------------------------------
// Load / Parse
// -----------------------------------------------------------------------------

// LoadManifest reads `manifest.json` from fsys and parses it.
// Convenience wrapper around [ParseManifest] for the most
// common entry point — the particle FS produced by the build
// pipeline.
func LoadManifest(fsys fs.FS) (Manifest, error) {
	data, err := fs.ReadFile(fsys, "manifest.json")
	if err != nil {
		return Manifest{}, fmt.Errorf("manifest: read manifest.json: %w", err)
	}
	m, err := ParseManifest(bytes.NewReader(data))
	if err != nil {
		return Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	return m, nil
}

// ParseManifest decodes a manifest from r. Errors on a missing
// name or version (both are required for the registry's
// `(name, version)` key) and on unknown credential method types
// (the build pipeline already gates against these, but
// re-validating here means a hand-crafted manifest can't
// silently lose a method declaration).
func ParseManifest(r io.Reader) (Manifest, error) {
	var m Manifest
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("decode: %w", err)
	}
	if m.Name == "" {
		return Manifest{}, errors.New("manifest is missing name")
	}
	if m.Version == "" {
		return Manifest{}, errors.New("manifest is missing version")
	}
	switch m.Runtime {
	case "", RuntimeJS, RuntimePython, RuntimeWasm:
		// ok — empty defaults to JS via ResolvedRuntime
	default:
		return Manifest{}, fmt.Errorf("manifest: unknown runtime %q (want one of: %q, %q, %q)", m.Runtime, RuntimeJS, RuntimePython, RuntimeWasm)
	}
	if err := validateFilesystem(m.Capabilities.Filesystem); err != nil {
		return Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	return m, nil
}

// validateFilesystem checks the declared mounts and temp mounts:
// names are simple identifiers, descriptions are present, paths are
// absolute / clean / unique, mount access is a known mode, and every
// temp mount declares a parseable positive MaxSize. A name may not be
// used for both a mount and a temp mount (the runtime's name→FS map
// would collide).
func validateFilesystem(fsCap FilesystemCapability) error {
	// path → owner label, for duplicate-path diagnostics across both maps.
	seenPaths := make(map[string]string)

	for name, m := range fsCap.Mounts {
		if err := validateMountName(name); err != nil {
			return fmt.Errorf("filesystem mount %q: %w", name, err)
		}
		if m.Description == "" {
			return fmt.Errorf("filesystem mount %q: missing description", name)
		}
		if err := validateMountPath(m.Path); err != nil {
			return fmt.Errorf("filesystem mount %q: %w", name, err)
		}
		switch m.Access {
		case MountReadOnly, MountReadWrite:
		case "":
			return fmt.Errorf("filesystem mount %q: missing access (want %q or %q)", name, MountReadOnly, MountReadWrite)
		default:
			return fmt.Errorf("filesystem mount %q: unknown access %q (want %q or %q)", name, m.Access, MountReadOnly, MountReadWrite)
		}
		if prev, ok := seenPaths[m.Path]; ok {
			return fmt.Errorf("filesystem mount %q: path %q already used by %s", name, m.Path, prev)
		}
		seenPaths[m.Path] = fmt.Sprintf("mount %q", name)
	}

	for name, t := range fsCap.Temp {
		if err := validateMountName(name); err != nil {
			return fmt.Errorf("filesystem temp %q: %w", name, err)
		}
		if _, ok := fsCap.Mounts[name]; ok {
			return fmt.Errorf("filesystem temp %q: name is also declared as a mount", name)
		}
		if t.Description == "" {
			return fmt.Errorf("filesystem temp %q: missing description", name)
		}
		if err := validateMountPath(t.Path); err != nil {
			return fmt.Errorf("filesystem temp %q: %w", name, err)
		}
		if t.MaxSize == "" {
			return fmt.Errorf("filesystem temp %q: missing maxSize", name)
		}
		if _, err := ParseByteSize(t.MaxSize); err != nil {
			return fmt.Errorf("filesystem temp %q: %w", name, err)
		}
		if prev, ok := seenPaths[t.Path]; ok {
			return fmt.Errorf("filesystem temp %q: path %q already used by %s", name, t.Path, prev)
		}
		seenPaths[t.Path] = fmt.Sprintf("temp %q", name)
	}
	return nil
}

// validateMountName accepts non-empty names of letters, digits, '-',
// and '_'. The restriction keeps a name usable as the left-hand side
// of `--mount name=path` (no '=') and as a CLI argument (no spaces or
// slashes).
func validateMountName(name string) error {
	if name == "" {
		return errors.New("empty name")
	}
	for _, r := range name {
		ok := r == '-' || r == '_' ||
			(r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9')
		if !ok {
			return fmt.Errorf("invalid character %q in name (allowed: letters, digits, '-', '_')", r)
		}
	}
	return nil
}

// validateMountPath requires an absolute, lexically-clean path that
// isn't the root. Cleanliness rejects "..", "." segments, and trailing
// slashes so the path the guest sees matches the preopen exactly.
func validateMountPath(p string) error {
	if p == "" {
		return errors.New("missing path")
	}
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("path %q must be absolute", p)
	}
	if path.Clean(p) != p {
		return fmt.Errorf("path %q must be clean (no '.', '..', or trailing slash)", p)
	}
	if p == "/" {
		return errors.New(`path must not be the root "/"`)
	}
	return nil
}

// ParseByteSize parses a byte-size string. A bare decimal integer is a
// count of bytes; an optional case-insensitive unit suffix multiplies
// by a binary factor: B=1, KB=1024, MB=1024², GB=1024³. So "10000" is
// 10000 bytes and "10KB" is 10240. The value must be positive.
func ParseByteSize(s string) (int64, error) {
	str := strings.TrimSpace(s)
	if str == "" {
		return 0, errors.New("empty size")
	}
	// Split the trailing unit letters from the leading number.
	cut := len(str)
	for cut > 0 && (str[cut-1] < '0' || str[cut-1] > '9') {
		cut--
	}
	numPart := strings.TrimSpace(str[:cut])
	unitPart := strings.TrimSpace(str[cut:])
	if numPart == "" {
		return 0, fmt.Errorf("invalid size %q: missing number", s)
	}
	n, err := strconv.ParseInt(numPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}
	var mult int64
	switch strings.ToUpper(unitPart) {
	case "", "B":
		mult = 1
	case "KB":
		mult = 1 << 10
	case "MB":
		mult = 1 << 20
	case "GB":
		mult = 1 << 30
	default:
		return 0, fmt.Errorf("invalid size %q: unknown unit %q (want B, KB, MB, or GB)", s, unitPart)
	}
	if n <= 0 {
		return 0, fmt.Errorf("invalid size %q: must be positive", s)
	}
	if n > (1<<63-1)/mult {
		return 0, fmt.Errorf("invalid size %q: too large", s)
	}
	return n * mult, nil
}

// -----------------------------------------------------------------------------
// Custom unmarshaling for the discriminated union.
// -----------------------------------------------------------------------------

// UnmarshalJSON routes a credential method payload to the
// matching typed sub-struct based on the `type` discriminator.
// Unknown types are rejected so a typo (`apike` vs `apikey`) or
// a newer schema we don't support yet fails loud rather than
// degrading into a credential the runtime can't substitute.
func (m *CredentialMethod) UnmarshalJSON(data []byte) error {
	var peek struct {
		Type        string `json:"type"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(data, &peek); err != nil {
		return fmt.Errorf("credential method: %w", err)
	}
	m.Kind = CredentialMethodKind(peek.Type)
	m.Description = peek.Description
	switch m.Kind {
	case "":
		return errors.New("credential method: missing type")
	case MethodBasic, MethodRaw:
		// no extra fields
	case MethodOAuth2:
		var v OAuth2Method
		if err := json.Unmarshal(data, &v); err != nil {
			return fmt.Errorf("credential method (oauth2): %w", err)
		}
		m.OAuth2 = &v
	case MethodAPIKey:
		var v APIKeyMethod
		if err := json.Unmarshal(data, &v); err != nil {
			return fmt.Errorf("credential method (apikey): %w", err)
		}
		m.APIKey = &v
	case MethodSigningKey:
		var v SigningKeyMethod
		if err := json.Unmarshal(data, &v); err != nil {
			return fmt.Errorf("credential method (signing-key): %w", err)
		}
		m.SigningKey = &v
	default:
		return fmt.Errorf("credential method: unknown type %q", peek.Type)
	}
	return nil
}

// MarshalJSON re-emits a CredentialMethod with its sub-struct
// fields flattened into the top-level object, matching the
// input shape. Round-trips for snapshot tests + any caller that
// wants to re-serialize a Manifest.
func (m CredentialMethod) MarshalJSON() ([]byte, error) {
	out := map[string]any{"type": string(m.Kind)}
	if m.Description != "" {
		out["description"] = m.Description
	}
	var subFields []byte
	var err error
	switch {
	case m.OAuth2 != nil:
		subFields, err = json.Marshal(m.OAuth2)
	case m.APIKey != nil:
		subFields, err = json.Marshal(m.APIKey)
	case m.SigningKey != nil:
		subFields, err = json.Marshal(m.SigningKey)
	}
	if err != nil {
		return nil, err
	}
	if len(subFields) > 0 {
		var extra map[string]json.RawMessage
		if err := json.Unmarshal(subFields, &extra); err != nil {
			return nil, err
		}
		for k, v := range extra {
			out[k] = v
		}
	}
	return json.Marshal(out)
}

// -----------------------------------------------------------------------------
// Internal helpers used by [Particle] construction.
// -----------------------------------------------------------------------------

// credentialHostBindings returns a fresh map from credential
// name to the credential's `Hosts` list, for every declared
// credential. Empty / nil host list → the credential isn't
// host-bound (signing-key, raw) and the HTTP policy treats it
// as "substitute anywhere a placeholder shows up".
func credentialHostBindings(m Manifest) map[string][]string {
	if len(m.Credentials) == 0 {
		return nil
	}
	out := make(map[string][]string, len(m.Credentials))
	for name, cred := range m.Credentials {
		if len(cred.Hosts) > 0 {
			out[name] = cred.Hosts
		}
	}
	return out
}

// declaredCredentialNames returns every credential name in
// sorted order. The substitution loop iterates this so each
// outbound request walks credentials in deterministic order
// regardless of map iteration randomness.
func (m Manifest) declaredCredentialNames() []string {
	if len(m.Credentials) == 0 {
		return nil
	}
	names := make([]string, 0, len(m.Credentials))
	for n := range m.Credentials {
		names = append(names, n)
	}
	sortStrings(names)
	return names
}

// sortStrings is the indirection that lets the file avoid an
// "sort" import — we already lean on the standard lib enough.
// Implemented inline because the typical particle has a
// handful of credentials and insertion sort is plenty.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
