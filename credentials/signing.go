package credentials

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"errors"
	"fmt"
	"hash"

	gen "github.com/partite-ai/particle/internal/host/gen/particle/host/signing"
)

// Algorithms supported by the signing adapter. The set matches the
// design doc §7's v1 list — RSA/ECDSA are deferred to phase 2.
const (
	AlgorithmHMACSHA256 = "hmac-sha256"
	AlgorithmHMACSHA512 = "hmac-sha512"
)

// hashFor returns a hash.Hash constructor for the given WIT
// algorithm string. The boolean is false for unsupported algorithms;
// the adapter surfaces those as `invalid-input` rather than panicking.
func hashFor(algorithm string) (func() hash.Hash, bool) {
	switch algorithm {
	case AlgorithmHMACSHA256:
		return sha256.New, true
	case AlgorithmHMACSHA512:
		return sha512.New, true
	}
	return nil, false
}

// -----------------------------------------------------------------------------
// adapter
// -----------------------------------------------------------------------------

type signingAdapter struct {
	store    Store
	particle string
}

var _ gen.Signing = (*signingAdapter)(nil)

func newSigningAdapter(store Store, particle string) *signingAdapter {
	return &signingAdapter{store: store, particle: particle}
}

// Sign computes a MAC over data using the named credential's key.
//
// Maps to the WIT `result<list<u8>, signing-error>`:
//
//	credential not found              → not-configured
//	credential exists but not signing → not-signing-key
//	algorithm unrecognized            → invalid-input(message)
//	key slot empty                    → not-configured
//	(other Store errors)              → invalid-input(err.Error())
func (a *signingAdapter) Sign(ctx context.Context, name string, data []uint8) (gen.ResultListU8SigningError, error) {
	mac, errResult, ok := a.macFor(ctx, name)
	if !ok {
		return gen.ResultListU8SigningErrorErr{Value: errResult}, nil
	}
	mac.Write(data)
	return gen.ResultListU8SigningErrorOk{Value: mac.Sum(nil)}, nil
}

// Verify recomputes the MAC over data and constant-time compares it
// against signature. Returns ok(true) on match, ok(false) on
// mismatch (including length mismatch — hmac.Equal handles that
// safely). Error variants mirror Sign.
func (a *signingAdapter) Verify(ctx context.Context, name string, data []uint8, signature []uint8) (gen.ResultBoolSigningError, error) {
	mac, errResult, ok := a.macFor(ctx, name)
	if !ok {
		return gen.ResultBoolSigningErrorErr{Value: errResult}, nil
	}
	mac.Write(data)
	expected := mac.Sum(nil)
	return gen.ResultBoolSigningErrorOk{Value: hmac.Equal(expected, signature)}, nil
}

// macFor resolves (particle, name) to a fresh hash.Hash keyed with
// the credential's key material, ready to .Write() data into. On
// failure it returns the SigningError variant the caller should
// surface. Splitting it out keeps Sign and Verify near-identical at
// the top level.
func (a *signingAdapter) macFor(ctx context.Context, name string) (hash.Hash, gen.SigningError, bool) {
	desc, err := a.store.GetByName(ctx, a.particle, name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, gen.SigningErrorNotConfigured{}, false
		}
		return nil, gen.SigningErrorInvalidInput{Value: err.Error()}, false
	}
	meta, ok := desc.Meta.(SigningKeyMeta)
	if !ok {
		return nil, gen.SigningErrorNotSigningKey{}, false
	}
	newHash, ok := hashFor(meta.Algorithm)
	if !ok {
		return nil, gen.SigningErrorInvalidInput{
			Value: fmt.Sprintf("unsupported signing algorithm %q (supported: %s, %s)",
				meta.Algorithm, AlgorithmHMACSHA256, AlgorithmHMACSHA512),
		}, false
	}
	key, err := a.store.ReadSecret(ctx, a.particle, desc.ID, SecretRoleKey)
	if err != nil {
		// Both ErrNotFound and ErrSecretNotSet collapse to
		// not-configured — without key material there's
		// nothing to sign with.
		if errors.Is(err, ErrNotFound) {
			return nil, gen.SigningErrorNotConfigured{}, false
		}
		return nil, gen.SigningErrorInvalidInput{Value: err.Error()}, false
	}
	return hmac.New(newHash, key), nil, true
}
