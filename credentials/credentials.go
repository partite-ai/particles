// Package credentials defines the host-side credential storage
// contract for particle hosts.
//
// A credential is split into two pieces:
//
//   - **Metadata** — the typed, secret-free portion: URLs,
//     usernames, scopes, algorithms, locations, expiry. Cheap to
//     read; doesn't trigger a KMS unwrap.
//   - **Secrets** — one or more named blobs holding the actual
//     sensitive material, addressed by a [SecretRole]. For OAuth
//     that's [SecretRoleAccessToken], [SecretRoleRefreshToken],
//     and [SecretRoleClientSecret]; for an API key it's
//     [SecretRoleKey].
//
// The split lets frequently-rotated material — OAuth access tokens
// in particular — be rewritten without touching the rest of the
// entry, and lets readers fetch metadata (e.g., to render
// `particle credentials list`) without paying for a secret unwrap.
//
// [Store.Put] writes metadata and any number of secrets atomically
// in one call — useful for new entries that need both fields and
// secrets set in lockstep, and for refresh flows that update the
// expiry alongside a rotated access token. Standalone secret
// rotations (e.g., access-token-only refresh) use [Store.WriteSecrets].
//
// A host application implements [Store] to back per-particle
// credential storage with whatever policy makes sense (in-memory,
// on-disk, KMS, …). Each entry carries two identifiers: a stable,
// store-generated [Descriptor.ID] and a user-facing
// [Descriptor.Name] unique within the particle's namespace.
//
// To wire a Store into a particle runtime, build a host component
// instance with [NewHostInstance] and pass `inst.Core()` to
// `wacogo.WithInstanceImport(...)` when assembling the runtime.
// The instance is scoped to one particle so credentials never leak
// across particles.
//
// Concrete Store implementations live in subpackages —
// credentials/memory for an in-process map-backed Store. There are
// no implementations in this package.
//
// Spec: docs/initial-design.md §7.
package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Store is the host-side credential storage interface, scoped to
// a single particle: every method operates against the
// particle's own namespace, with no cross-particle visibility.
// Construct one via a backend-specific helper — e.g.,
// `credentials/sqlite.(*Backend).Scoped(particle)` returns a
// Store that wraps the multi-particle sqlite backing store and
// pre-binds it to `particle`.
//
// A "credential" is identified by a particle-scoped name (e.g.,
// "github" or "openai" — what the manifest's top-level
// `credentials` map keys are). Each credential has one currently-
// configured method, identified by its method name (e.g., "pat",
// "oauth"); the per-credential method choice is recorded alongside
// the metadata.
//
// IDs are store-generated and stable — survive renames, secret
// rotations, and other in-place modifications. The interface
// contract requires IDs to be ASCII with no whitespace or
// punctuation; concrete implementations are free to choose their
// own scheme.
//
// Implementations should be safe for concurrent use.
type Store interface {
	// -------- entry-level (metadata) operations --------

	// GetByID returns the descriptor (id, name, metadata) for
	// the entry under `id`. Returns ErrNotFound when no such
	// entry exists. Does NOT read secrets — cheap.
	GetByID(ctx context.Context, id string) (Descriptor, error)

	// GetByName returns the descriptor for the entry under
	// `name`. Same error contract as GetByID.
	//
	// The runtime adapter consults this on every
	// `getPlaceholder` / `getRaw` call to determine the
	// credential's id and apply-spec.
	GetByName(ctx context.Context, name string) (Descriptor, error)

	// List returns a lightweight summary of every entry in this
	// particle's namespace.
	List(ctx context.Context) ([]ListEntry, error)

	// Put configures the `name` credential with the given
	// method and metadata, replacing any prior configuration
	// for the same name.
	//
	// Each name holds at most one row — Put enforces the
	// invariant atomically:
	//
	//   - If no prior row exists, a fresh entry is inserted with a
	//     new ID.
	//   - If a prior row exists under the same `method`, the
	//     existing ID is preserved; metadata is replaced; secrets
	//     listed in `secrets` UPSERT over their prior values;
	//     secrets NOT listed are preserved untouched. (Effectively
	//     a Reconfigure-with-same-method.)
	//   - If a prior row exists under a DIFFERENT `method`, every
	//     secret previously stored for that row is wiped and the
	//     row is rewritten with the new method + metadata + the
	//     supplied secrets. (Method switching never leaves stray
	//     secrets from the old method behind.)
	//
	// Different names coexist freely — a particle declaring
	// both "github" and "openai" gets two rows.
	//
	// Returns the resulting descriptor.
	Put(ctx context.Context, name, method string, meta Metadata, secrets ...Secret) (Descriptor, error)

	// Delete removes the entire entry — metadata and every
	// secret. Idempotent: returns nil if no such entry existed.
	Delete(ctx context.Context, id string) error

	// ConfiguredMethod returns the method name currently stored
	// for `name`, or "" when no credential is configured under
	// that name.
	//
	// Used to drive the "which authentication method is
	// configured?" question — the answer the runtime surfaces to
	// particles via getConfiguredMethod(name) and the CLI shows
	// in `particle list`.
	ConfiguredMethod(ctx context.Context, name string) (string, error)

	// -------- secret-level operations --------
	//
	// Secrets are independently versioned; updating one secret
	// does not require re-writing any other secret or the
	// metadata. The well-known SecretRole values per credential
	// kind are listed below; implementations don't validate
	// roles — they're treated as opaque keys.

	// ReadSecret returns the raw bytes stored for (id, role).
	// Returns ErrSecretNotSet when the role has not been
	// written. ErrSecretNotSet wraps ErrNotFound, so callers
	// using `errors.Is(err, ErrNotFound)` to detect both
	// missing-entry and missing-secret get the right answer.
	ReadSecret(ctx context.Context, id string, role SecretRole) ([]byte, error)

	// WriteSecrets atomically writes one or more secrets to an
	// existing entry, replacing each named role. Does not
	// touch metadata or any role not in `secrets`.
	//
	// This is the path for OAuth refresh: rotate the access
	// token (and, when the provider supplies one, the refresh
	// token) atomically without re-touching metadata. The
	// caller wraps the access token's expiry into the
	// AccessToken bundle; metadata stays static post-setup.
	//
	// Returns ErrNotFound if the entry itself does not exist.
	WriteSecrets(ctx context.Context, id string, secrets ...Secret) error

	// DeleteSecret removes a secret. Idempotent: returns nil
	// when the role is already absent or the entry is missing.
	DeleteSecret(ctx context.Context, id string, role SecretRole) error
}

// Descriptor is the metadata-level record for an entry: stable ID,
// credential name, configured method name, and the typed metadata.
// Returned by GetByID, GetByName, and Put. Does NOT carry secret
// values — read those separately via Store.ReadSecret.
type Descriptor struct {
	ID     string
	Name   string // credential name (e.g., "github")
	Method string // configured method name (e.g., "pat", "oauth")
	Meta   Metadata
}

// ListEntry is the lightweight summary surfaced by Store.List —
// metadata-key fields only.
type ListEntry struct {
	ID     string
	Name   string
	Method string
	Kind   Kind
}

// Secret is a (role, value) pair used as the payload for
// Store.Put's atomic metadata+secrets write.
type Secret struct {
	Role  SecretRole
	Value []byte
}

// SecretRole names a particular secret slot within an entry.
// Implementations treat it as an opaque key; the constants below
// document the well-known names per credential kind.
type SecretRole string

const (
	// SecretRolePassword — password for a BasicMeta entry.
	SecretRolePassword SecretRole = "password"

	// SecretRoleAccessToken — current OAuth2 access token.
	// Rotated on refresh.
	SecretRoleAccessToken SecretRole = "access_token"

	// SecretRoleRefreshToken — OAuth2 refresh token. Rotated
	// when the provider issues a new one alongside the access
	// token.
	SecretRoleRefreshToken SecretRole = "refresh_token"

	// SecretRoleClientSecret — OAuth2 client secret. Set once
	// at setup; not normally rotated.
	SecretRoleClientSecret SecretRole = "client_secret"

	// SecretRoleKey — key material for both APIKeyMeta and
	// SigningKeyMeta. Only one Kind has any given entry, so
	// reusing the role name is fine.
	SecretRoleKey SecretRole = "key"

	// SecretRoleValue — opaque value of a RawMeta entry.
	SecretRoleValue SecretRole = "value"
)

// ErrNotFound is the sentinel a Store returns when no entry exists
// for the requested (particle, id) or (particle, name). Also wraps
// ErrSecretNotSet, so `errors.Is(err, ErrNotFound)` catches both
// cases when a caller doesn't care about the distinction.
var ErrNotFound = errors.New("credentials: not found")

// ErrSecretNotSet is returned from ReadSecret when the entry exists
// but the requested role has not been written. Wraps ErrNotFound
// for the conflated-error pattern; use `errors.Is(err,
// ErrSecretNotSet)` for the specific case.
var ErrSecretNotSet = fmt.Errorf("%w: secret not set", ErrNotFound)

// -----------------------------------------------------------------------------
// Metadata types and Kind
// -----------------------------------------------------------------------------

// Kind names a credential variant. Matches the `type:` strings used
// in particle manifest `credentials.<name>.methods.<method>.type`
// declarations, so a manifest reader can compare against this type
// directly.
type Kind string

const (
	KindBasic      Kind = "basic"
	KindOAuth2     Kind = "oauth2"
	KindAPIKey     Kind = "apikey"
	KindSigningKey Kind = "signing-key"
	KindRaw        Kind = "raw"
)

// Metadata is the typed, secret-free portion of one credential.
// Sealed: only the five concrete structs in this package satisfy
// it. Each Kind has a corresponding Metadata struct.
type Metadata interface {
	// Kind reports the credential variant.
	Kind() Kind

	// metadata is an unexported marker that prevents foreign
	// types from satisfying the interface accidentally.
	metadata()
}

// BasicMeta is the metadata for an HTTP Basic credential. The
// password lives under SecretRolePassword.
type BasicMeta struct {
	Username string
}

func (BasicMeta) Kind() Kind { return KindBasic }
func (BasicMeta) metadata()  {}

// OAuth2Meta is the metadata for an OAuth 2.0 credential. Only
// the static, setup-time fields live here — provider URLs, client
// id, scopes, flow choice. The dynamic fields that change on every
// refresh — current access token, its type, its expiry — are
// bundled together inside the SecretRoleAccessToken value (see
// [AccessToken]). The refresh token lives in SecretRoleRefreshToken,
// the client secret (when applicable) in SecretRoleClientSecret.
//
// This split lets a refresh rotate the access token (and possibly
// the refresh token) via WriteSecrets without touching the
// metadata — the access path is entirely secret-side.
type OAuth2Meta struct {
	// Provider configuration captured at setup.
	AuthorizationURL string
	TokenURL         string
	ClientID         string
	Scopes           []string

	// Flow names the OAuth flow that produced this token bundle.
	// One of "authorization-code-pkce" or "device-code" in v1.
	Flow string
}

// AccessToken is the decoded form of the SecretRoleAccessToken
// secret bytes for an OAuth2 entry. Token, type, and expiry travel
// together so a refresh can update them in one secret-only write
// without changing metadata.
//
// The wasi:http policy reads the access secret, [UnmarshalAccessToken]s
// it, checks ExpiresAt, and decides whether to substitute the
// token directly or trigger a refresh first.
type AccessToken struct {
	// Token is the access-token string as issued by the
	// provider. Stored verbatim — no decoding, no validation.
	Token string `json:"token"`

	// Type is the token-type string the provider returned
	// (typically "Bearer"). Empty when the provider didn't
	// send one — callers should treat empty as Bearer.
	Type string `json:"type,omitempty"`

	// ExpiresAt is the absolute time the token expires. Zero
	// when the provider didn't supply an `expires_in`.
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// Marshal encodes the AccessToken as the byte payload stored under
// SecretRoleAccessToken. The format is JSON; future format changes
// will keep round-trip compatibility with existing data.
func (t AccessToken) Marshal() []byte {
	b, err := json.Marshal(t)
	if err != nil {
		// Marshal of this struct cannot fail in practice
		// (no channels, funcs, complex numbers). Surface as a
		// panic so a contract regression is loud.
		panic("credentials: AccessToken.Marshal: " + err.Error())
	}
	return b
}

// UnmarshalAccessToken decodes bytes returned from
// Store.ReadSecret(SecretRoleAccessToken).
func UnmarshalAccessToken(data []byte) (AccessToken, error) {
	var t AccessToken
	if err := json.Unmarshal(data, &t); err != nil {
		return AccessToken{}, fmt.Errorf("credentials: AccessToken: %w", err)
	}
	return t, nil
}

func (OAuth2Meta) Kind() Kind { return KindOAuth2 }
func (OAuth2Meta) metadata()  {}

// APIKeyMeta is the metadata for an API-key credential. The key
// itself lives under SecretRoleKey.
type APIKeyMeta struct {
	// Location records the substitution descriptor chosen at
	// setup time — header, query param, or auth-scheme prefix.
	Location ApplySpec
}

func (APIKeyMeta) Kind() Kind { return KindAPIKey }
func (APIKeyMeta) metadata()  {}

// SigningKeyMeta is the metadata for a signing-key credential. The
// raw key material lives under SecretRoleKey.
type SigningKeyMeta struct {
	// Algorithm identifies the signing algorithm — v1 supports
	// "hmac-sha256" and "hmac-sha512".
	Algorithm string
}

func (SigningKeyMeta) Kind() Kind { return KindSigningKey }
func (SigningKeyMeta) metadata()  {}

// RawMeta is the metadata for an opaque value the host returns to
// JS verbatim. The actual value lives under SecretRoleValue.
type RawMeta struct{}

func (RawMeta) Kind() Kind { return KindRaw }
func (RawMeta) metadata()  {}

// -----------------------------------------------------------------------------
// Apply spec — needed by APIKeyMeta.Location.
// -----------------------------------------------------------------------------

// ApplySpec describes where a credential's real value gets
// substituted in an outgoing HTTP request.
type ApplySpec struct {
	Kind ApplyKind

	// Name is the header name (Kind == ApplyHeader) or query
	// parameter name (Kind == ApplyQueryParam). Empty for the
	// other kinds.
	Name string

	// Scheme is the auth scheme prefix (Kind == ApplyAuthScheme).
	// Empty for the other kinds.
	Scheme string
}

// ApplyKind enumerates the substitution shapes. The zero value is
// intentionally invalid so a forgotten initialization surfaces
// immediately when the adapter consults the spec.
type ApplyKind int

const (
	// ApplyBasic — `Authorization: Basic <placeholder>`.
	ApplyBasic ApplyKind = iota + 1

	// ApplyBearer — `Authorization: Bearer <placeholder>`.
	ApplyBearer

	// ApplyHeader — `<Name>: <placeholder>`.
	ApplyHeader

	// ApplyAuthScheme — `Authorization: <Scheme> <placeholder>`.
	ApplyAuthScheme

	// ApplyQueryParam — `?<Name>=<placeholder>` in the URL.
	ApplyQueryParam
)

func (k ApplyKind) String() string {
	switch k {
	case ApplyBasic:
		return "basic"
	case ApplyBearer:
		return "bearer"
	case ApplyHeader:
		return "header"
	case ApplyAuthScheme:
		return "auth-scheme"
	case ApplyQueryParam:
		return "query-param"
	}
	return "ApplyKind(invalid)"
}
