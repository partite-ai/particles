package credentials

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"strings"
	"testing"

	gen "github.com/partite-ai/particles/internal/host/gen/particle/host/signing"
)

// -----------------------------------------------------------------------------
// Sign — happy paths
// -----------------------------------------------------------------------------

func TestSigningAdapter_Sign_HMACSHA256(t *testing.T) {
	store := &fakeStore{}
	key := []byte("super-secret-key-1234")
	store.putWithSecrets("yaml-tools", "id-1", "webhook",
		SigningKeyMeta{Algorithm: AlgorithmHMACSHA256},
		map[SecretRole][]byte{SecretRoleKey: key},
	)
	a := newSigningAdapter(store, "yaml-tools")

	data := []byte("hello world")
	res, err := a.Sign(context.Background(), "webhook", data)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	ok, isOk := res.(gen.ResultListU8SigningErrorOk)
	if !isOk {
		t.Fatalf("got %T, want Ok", res)
	}

	// Independently compute the expected MAC and assert byte
	// equality — locks in the algorithm choice.
	expected := hmac.New(sha256.New, key)
	expected.Write(data)
	if !hmac.Equal(ok.Value, expected.Sum(nil)) {
		t.Errorf("signature does not match independent HMAC-SHA256 computation")
	}
}

func TestSigningAdapter_Sign_HMACSHA512(t *testing.T) {
	store := &fakeStore{}
	key := []byte("a-different-key")
	store.putWithSecrets("yaml-tools", "id-2", "longer",
		SigningKeyMeta{Algorithm: AlgorithmHMACSHA512},
		map[SecretRole][]byte{SecretRoleKey: key},
	)
	a := newSigningAdapter(store, "yaml-tools")

	data := []byte("payload")
	res, _ := a.Sign(context.Background(), "longer", data)
	ok := res.(gen.ResultListU8SigningErrorOk)

	expected := hmac.New(sha512.New, key)
	expected.Write(data)
	if !hmac.Equal(ok.Value, expected.Sum(nil)) {
		t.Errorf("signature does not match independent HMAC-SHA512 computation")
	}
	// SHA-512 outputs are 64 bytes; sanity check.
	if len(ok.Value) != 64 {
		t.Errorf("signature length = %d, want 64", len(ok.Value))
	}
}

// -----------------------------------------------------------------------------
// Verify — happy paths
// -----------------------------------------------------------------------------

func TestSigningAdapter_Verify_RoundTrip(t *testing.T) {
	store := &fakeStore{}
	key := []byte("k")
	store.putWithSecrets("p", "i", "x",
		SigningKeyMeta{Algorithm: AlgorithmHMACSHA256},
		map[SecretRole][]byte{SecretRoleKey: key},
	)
	a := newSigningAdapter(store, "p")

	data := []byte("contents")
	signRes, _ := a.Sign(context.Background(), "x", data)
	signature := signRes.(gen.ResultListU8SigningErrorOk).Value

	verifyRes, err := a.Verify(context.Background(), "x", data, signature)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	ok := verifyRes.(gen.ResultBoolSigningErrorOk)
	if !ok.Value {
		t.Error("Verify returned false on a freshly-signed message")
	}
}

func TestSigningAdapter_Verify_MismatchReturnsFalse(t *testing.T) {
	store := &fakeStore{}
	store.putWithSecrets("p", "i", "x",
		SigningKeyMeta{Algorithm: AlgorithmHMACSHA256},
		map[SecretRole][]byte{SecretRoleKey: []byte("k")},
	)
	a := newSigningAdapter(store, "p")

	verifyRes, _ := a.Verify(context.Background(), "x", []byte("contents"), []byte("not the right signature"))
	ok := verifyRes.(gen.ResultBoolSigningErrorOk)
	if ok.Value {
		t.Error("Verify returned true for a wrong signature")
	}
}

// hmac.Equal is constant-time and tolerates length mismatches
// safely — we shouldn't surface them as invalid-input.
func TestSigningAdapter_Verify_LengthMismatchReturnsFalse(t *testing.T) {
	store := &fakeStore{}
	store.putWithSecrets("p", "i", "x",
		SigningKeyMeta{Algorithm: AlgorithmHMACSHA256},
		map[SecretRole][]byte{SecretRoleKey: []byte("k")},
	)
	a := newSigningAdapter(store, "p")

	verifyRes, err := a.Verify(context.Background(), "x", []byte("data"), []byte{0x00, 0x01})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	ok, isOk := verifyRes.(gen.ResultBoolSigningErrorOk)
	if !isOk {
		t.Fatalf("got %T, want Ok(false)", verifyRes)
	}
	if ok.Value {
		t.Error("Verify with too-short signature returned true")
	}
}

// -----------------------------------------------------------------------------
// Error variants
// -----------------------------------------------------------------------------

func TestSigningAdapter_NotConfigured_NoEntry(t *testing.T) {
	a := newSigningAdapter(&fakeStore{}, "p")
	res, _ := a.Sign(context.Background(), "missing", []byte("d"))
	errRes := res.(gen.ResultListU8SigningErrorErr)
	if _, ok := errRes.Value.(gen.SigningErrorNotConfigured); !ok {
		t.Errorf("got %T, want NotConfigured", errRes.Value)
	}
}

func TestSigningAdapter_NotSigningKey(t *testing.T) {
	// Entry exists, but it's a different credential kind.
	store := &fakeStore{}
	store.putWithSecrets("p", "i", "x",
		BasicMeta{Username: "u"},
		map[SecretRole][]byte{SecretRolePassword: []byte("hunter2")},
	)
	a := newSigningAdapter(store, "p")

	res, _ := a.Sign(context.Background(), "x", []byte("d"))
	errRes := res.(gen.ResultListU8SigningErrorErr)
	if _, ok := errRes.Value.(gen.SigningErrorNotSigningKey); !ok {
		t.Errorf("got %T, want NotSigningKey", errRes.Value)
	}

	// Verify path same.
	vRes, _ := a.Verify(context.Background(), "x", []byte("d"), []byte("s"))
	vErr := vRes.(gen.ResultBoolSigningErrorErr)
	if _, ok := vErr.Value.(gen.SigningErrorNotSigningKey); !ok {
		t.Errorf("Verify: got %T, want NotSigningKey", vErr.Value)
	}
}

func TestSigningAdapter_InvalidInput_UnsupportedAlgorithm(t *testing.T) {
	store := &fakeStore{}
	store.putWithSecrets("p", "i", "x",
		SigningKeyMeta{Algorithm: "rsa-sha256"}, // not in v1
		map[SecretRole][]byte{SecretRoleKey: []byte("k")},
	)
	a := newSigningAdapter(store, "p")

	res, _ := a.Sign(context.Background(), "x", []byte("d"))
	errRes := res.(gen.ResultListU8SigningErrorErr)
	tm, ok := errRes.Value.(gen.SigningErrorInvalidInput)
	if !ok {
		t.Fatalf("got %T, want InvalidInput", errRes.Value)
	}
	if !strings.Contains(tm.Value, "rsa-sha256") {
		t.Errorf("error message = %q, should mention the offending algorithm", tm.Value)
	}
}

func TestSigningAdapter_NotConfigured_KeyMissing(t *testing.T) {
	// Metadata exists, but the key bytes were never written.
	store := &fakeStore{}
	store.putWithSecrets("p", "i", "x",
		SigningKeyMeta{Algorithm: AlgorithmHMACSHA256},
		nil, // no SecretRoleKey
	)
	a := newSigningAdapter(store, "p")

	res, _ := a.Sign(context.Background(), "x", []byte("d"))
	errRes := res.(gen.ResultListU8SigningErrorErr)
	if _, ok := errRes.Value.(gen.SigningErrorNotConfigured); !ok {
		t.Errorf("got %T, want NotConfigured", errRes.Value)
	}
}

// -----------------------------------------------------------------------------
// Determinism + scoping
// -----------------------------------------------------------------------------

// HMAC is deterministic — signing the same message twice with the
// same key returns the same MAC. Locks in that we don't accidentally
// inject any salt or nonce.
func TestSigningAdapter_Sign_Deterministic(t *testing.T) {
	store := &fakeStore{}
	store.putWithSecrets("p", "i", "x",
		SigningKeyMeta{Algorithm: AlgorithmHMACSHA256},
		map[SecretRole][]byte{SecretRoleKey: []byte("k")},
	)
	a := newSigningAdapter(store, "p")

	data := []byte("identical bytes")
	first, _ := a.Sign(context.Background(), "x", data)
	second, _ := a.Sign(context.Background(), "x", data)
	a1 := first.(gen.ResultListU8SigningErrorOk).Value
	a2 := second.(gen.ResultListU8SigningErrorOk).Value
	if !hmac.Equal(a1, a2) {
		t.Error("two signs of identical input produced different MACs")
	}
}

func TestSigningAdapter_ScopedByParticle(t *testing.T) {
	// Same name in different particles must produce different
	// signatures (because each has its own key, with different IDs).
	store := &fakeStore{}
	store.putWithSecrets("yaml-tools", "y", "k",
		SigningKeyMeta{Algorithm: AlgorithmHMACSHA256},
		map[SecretRole][]byte{SecretRoleKey: []byte("yaml-key")},
	)
	store.putWithSecrets("json-tools", "j", "k",
		SigningKeyMeta{Algorithm: AlgorithmHMACSHA256},
		map[SecretRole][]byte{SecretRoleKey: []byte("json-key")},
	)

	yaml := newSigningAdapter(store, "yaml-tools")
	json := newSigningAdapter(store, "json-tools")

	data := []byte("hello")
	resY, _ := yaml.Sign(context.Background(), "k", data)
	resJ, _ := json.Sign(context.Background(), "k", data)
	sigY := resY.(gen.ResultListU8SigningErrorOk).Value
	sigJ := resJ.(gen.ResultListU8SigningErrorOk).Value
	if hmac.Equal(sigY, sigJ) {
		t.Error("signatures collided across particle scopes — keys leaked")
	}
}
