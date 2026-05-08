package runtime

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"

	httptypes "github.com/partite-ai/wacogo/wasi/http/types"

	"github.com/partite-ai/particle/credentials"
)

// HostNotAllowedError is the error a denying [httpPolicy] returns
// when a particle attempts an outbound request to a host not in
// its manifest's `capabilities.http.allowedHosts`.
//
// It also carries a wasi:http/types ErrorCode (via the wrapped
// *CodedError, see codedDenialError) so the wasi:http impl
// surfaces a specific error to the guest —
// `destination-ip-prohibited` — instead of the generic
// `internal-error` mapping.
type HostNotAllowedError struct {
	Host string
}

func (e *HostNotAllowedError) Error() string {
	return fmt.Sprintf("http: host %q not declared in capabilities.http.allowedHosts", e.Host)
}

// IsHostNotAllowed reports whether err is (or wraps) a
// HostNotAllowedError.
func IsHostNotAllowed(err error) bool {
	var hae *HostNotAllowedError
	return errors.As(err, &hae)
}

// httpPolicy is the per-particle wasi.HTTPDoer. It directly
// implements wacogo's `Do(*http.Request) (*http.Response, error)`
// interface and does two jobs in order:
//
//  1. Host allow-list check — drop the request with
//     `destination-ip-prohibited` if the URL's host isn't in the
//     manifest's allowedHosts.
//
//  2. Spec-driven credential placeholder substitution — for each
//     credential the manifest declares, look it up in the Store
//     and check the apply-spec's expected location in the request
//     for the matching placeholder. If found, replace with the
//     real value. A placeholder anywhere else (including for an
//     undeclared credential) transmits literally.
//
// Host check runs first so a request to a disallowed host never
// causes the Store to be touched — denied destinations shouldn't
// see substituted secrets and shouldn't be probable for which
// credentials a particle has configured.
type httpPolicy struct {
	allowedHosts map[string]struct{}
	inner        httptypes.HTTPDoer

	store               credentials.Store
	particle            string
	declaredCredentials []string
}

// newHTTPPolicy builds the policy for one particle.
//
//   - declared==false → manifest doesn't declare `http` at all →
//     deny every outbound request.
//   - declaredCredentials lists the credential names the manifest
//     authorized; substitution only ever attempts these. nil/empty
//     → no substitution runs and any placeholder a particle
//     planted transmits literally.
func newHTTPPolicy(declared bool, allowedHosts []string, inner httptypes.HTTPDoer, store credentials.Store, particle string, declaredCredentials []string) *httpPolicy {
	p := &httpPolicy{
		inner:               inner,
		store:               store,
		particle:            particle,
		declaredCredentials: declaredCredentials,
	}
	if declared {
		p.allowedHosts = make(map[string]struct{}, len(allowedHosts))
		for _, h := range allowedHosts {
			if h != "" {
				p.allowedHosts[h] = struct{}{}
			}
		}
	}
	return p
}

// Do implements wasi/http/types.HTTPDoer.
func (p *httpPolicy) Do(req *http.Request) (*http.Response, error) {
	if _, ok := p.allowedHosts[req.URL.Hostname()]; !ok {
		hae := &HostNotAllowedError{Host: req.URL.Hostname()}
		return nil, &codedDenialError{
			HostNotAllowed: hae,
			coded: &httptypes.CodedError{
				Code: httptypes.ErrorCodeDestinationIPProhibited{},
				Msg:  hae.Error(),
			},
		}
	}
	if err := p.substituteCredentials(req); err != nil {
		return nil, err
	}
	return p.inner.Do(req)
}

// codedDenialError pairs a *CodedError (so wasi:http surfaces the
// right WIT error code to JS) with a *HostNotAllowedError (so
// IsHostNotAllowed and other Go-side helpers can recognize the
// denial when the error makes it back through the tool path).
type codedDenialError struct {
	HostNotAllowed *HostNotAllowedError
	coded          *httptypes.CodedError
}

func (e *codedDenialError) Error() string { return e.coded.Error() }

// Unwrap exposes both the *CodedError (so wasi:http picks up the
// ErrorCode) and the *HostNotAllowedError (so errors.As on
// *HostNotAllowedError keeps working).
func (e *codedDenialError) Unwrap() []error {
	return []error{e.coded, e.HostNotAllowed}
}

// -----------------------------------------------------------------------------
// Credential substitution — spec-driven.
//
// We iterate the manifest-declared credential names, look each up
// in the Store, and check the request at the apply-spec's expected
// location. The manifest is the source of truth: a placeholder for
// an undeclared credential — even a real ID forged by an attacker
// — never triggers a Store lookup.
// -----------------------------------------------------------------------------

func (p *httpPolicy) substituteCredentials(req *http.Request) error {
	for _, name := range p.declaredCredentials {
		if err := p.substituteOne(req, name); err != nil {
			return err
		}
	}
	return nil
}

// substituteOne resolves the named credential to its descriptor,
// figures out the apply-spec's expected location, and substitutes
// if a matching placeholder appears there. Errors propagate to Do
// (the request fails before any partial substitution lands on
// the wire).
func (p *httpPolicy) substituteOne(req *http.Request, name string) error {
	desc, err := p.store.GetByName(req.Context(), p.particle, name)
	if err != nil {
		// Declared but not configured → nothing to substitute.
		// The JS-side getPlaceholder for this name would have
		// already returned not-configured to the particle, so
		// the request shouldn't carry the placeholder anyway.
		if errors.Is(err, credentials.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("substitute %s: %w", name, err)
	}
	placeholder := credentials.PlaceholderPrefix + desc.ID
	switch m := desc.Meta.(type) {
	case credentials.BasicMeta:
		return p.substituteBasic(req, desc.ID, placeholder, m)
	case credentials.OAuth2Meta:
		return p.substituteBearer(req, desc.ID, placeholder)
	case credentials.APIKeyMeta:
		return p.substituteAPIKey(req, desc.ID, placeholder, m.Location)
	}
	// SigningKey / Raw don't participate in HTTP substitution.
	return nil
}

func (p *httpPolicy) substituteBasic(req *http.Request, id, placeholder string, meta credentials.BasicMeta) error {
	want := "Basic " + placeholder
	if req.Header.Get("Authorization") != want {
		return nil
	}
	password, err := p.readSecret(req.Context(), id, credentials.SecretRolePassword)
	if err != nil {
		return err
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(meta.Username + ":" + string(password)))
	req.Header.Set("Authorization", "Basic "+encoded)
	return nil
}

func (p *httpPolicy) substituteBearer(req *http.Request, id, placeholder string) error {
	want := "Bearer " + placeholder
	if req.Header.Get("Authorization") != want {
		return nil
	}
	bundleBytes, err := p.readSecret(req.Context(), id, credentials.SecretRoleAccessToken)
	if err != nil {
		return err
	}
	bundle, err := credentials.UnmarshalAccessToken(bundleBytes)
	if err != nil {
		return fmt.Errorf("substitute %s: decode access token: %w", id, err)
	}
	req.Header.Set("Authorization", "Bearer "+bundle.Token)
	return nil
}

func (p *httpPolicy) substituteAPIKey(req *http.Request, id, placeholder string, loc credentials.ApplySpec) error {
	switch loc.Kind {
	case credentials.ApplyHeader:
		if loc.Name == "" || req.Header.Get(loc.Name) != placeholder {
			return nil
		}
		key, err := p.readSecret(req.Context(), id, credentials.SecretRoleKey)
		if err != nil {
			return err
		}
		req.Header.Set(loc.Name, string(key))
	case credentials.ApplyAuthScheme:
		want := loc.Scheme + " " + placeholder
		if loc.Scheme == "" || req.Header.Get("Authorization") != want {
			return nil
		}
		key, err := p.readSecret(req.Context(), id, credentials.SecretRoleKey)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", loc.Scheme+" "+string(key))
	case credentials.ApplyQueryParam:
		if loc.Name == "" {
			return nil
		}
		q := req.URL.Query()
		if q.Get(loc.Name) != placeholder {
			return nil
		}
		key, err := p.readSecret(req.Context(), id, credentials.SecretRoleKey)
		if err != nil {
			return err
		}
		q.Set(loc.Name, string(key))
		req.URL.RawQuery = q.Encode()
	}
	return nil
}

// readSecret wraps the not-found / not-set cases into a richer
// error so a substitution failure on the wire reads "credential X
// is missing the password secret" rather than a bare
// "credentials: not found".
func (p *httpPolicy) readSecret(ctx context.Context, id string, role credentials.SecretRole) ([]byte, error) {
	v, err := p.store.ReadSecret(ctx, p.particle, id, role)
	if err != nil {
		if errors.Is(err, credentials.ErrNotFound) {
			return nil, fmt.Errorf("substitute %s: %s secret not set", id, role)
		}
		return nil, fmt.Errorf("substitute %s: read %s: %w", id, role, err)
	}
	return v, nil
}
