package sqlite

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"

	"github.com/zalando/go-keyring"
	"golang.org/x/crypto/nacl/secretbox"
)

// keySize is the secretbox key length in bytes.
const keySize = 32

// nonceSize is the secretbox nonce length in bytes — random per
// message; the birthday bound at 192 bits is 2^96 messages, so
// random nonces are safe in practice.
const nonceSize = 24

// KeyringSealer is a [Sealer] backed by NaCl secretbox with the
// key persisted in the operating system's keychain via
// zalando/go-keyring.
//
// The key never leaves the host process at rest in plaintext —
// the keychain stores it as a base64-encoded string under a
// caller-chosen (service, name) pair. On first use, [NewKeyringSealer]
// generates a fresh random key; subsequent constructions for the
// same (service, name) reuse the existing key, which is the
// property that lets persisted ciphertexts decrypt across host
// restarts.
//
// On-disk ciphertext format: 24-byte nonce || secretbox output
// (which is plaintext + 16-byte authentication tag). No version
// byte — when we need a format change, the schema migration
// re-encrypts in place.
type KeyringSealer struct {
	key [keySize]byte
}

// NewKeyringSealer loads the secretbox key for (service, name)
// from the OS keychain. If no key exists yet, it generates one
// from crypto/rand and stores it. Returned errors come from
// keychain I/O — common causes are an absent or locked keychain
// daemon, missing dbus on Linux without a graphical session, or
// permission denial.
func NewKeyringSealer(service, name string) (*KeyringSealer, error) {
	if service == "" || name == "" {
		return nil, errors.New("credentials/sqlite: keyring service and name are required")
	}
	raw, err := keyring.Get(service, name)
	switch {
	case err == nil:
		decoded, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("credentials/sqlite: decode keyring key: %w", err)
		}
		if len(decoded) != keySize {
			return nil, fmt.Errorf("credentials/sqlite: keyring key has %d bytes, want %d", len(decoded), keySize)
		}
		s := &KeyringSealer{}
		copy(s.key[:], decoded)
		return s, nil

	case errors.Is(err, keyring.ErrNotFound):
		s := &KeyringSealer{}
		if _, err := io.ReadFull(rand.Reader, s.key[:]); err != nil {
			return nil, fmt.Errorf("credentials/sqlite: generate key: %w", err)
		}
		encoded := base64.StdEncoding.EncodeToString(s.key[:])
		if err := keyring.Set(service, name, encoded); err != nil {
			return nil, fmt.Errorf("credentials/sqlite: store key in keyring: %w", err)
		}
		return s, nil

	default:
		return nil, fmt.Errorf("credentials/sqlite: read keyring: %w", err)
	}
}

func (s *KeyringSealer) Seal(plaintext []byte) ([]byte, error) {
	var nonce [nonceSize]byte
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		return nil, fmt.Errorf("credentials/sqlite: nonce: %w", err)
	}
	// Allocate enough room for the nonce + sealed output up front
	// so secretbox.Seal can append in place.
	out := make([]byte, nonceSize, nonceSize+len(plaintext)+secretbox.Overhead)
	copy(out, nonce[:])
	return secretbox.Seal(out, plaintext, &nonce, &s.key), nil
}

func (s *KeyringSealer) Open(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < nonceSize+secretbox.Overhead {
		return nil, errors.New("credentials/sqlite: ciphertext too short")
	}
	var nonce [nonceSize]byte
	copy(nonce[:], ciphertext[:nonceSize])
	plain, ok := secretbox.Open(nil, ciphertext[nonceSize:], &nonce, &s.key)
	if !ok {
		return nil, errors.New("credentials/sqlite: decrypt failed (wrong key or tampered ciphertext)")
	}
	return plain, nil
}
