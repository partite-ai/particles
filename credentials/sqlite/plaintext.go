package sqlite

import (
	"bytes"
	"errors"
)

// plaintextMarker prefixes every PlaintextSealer blob. It exists so a
// secret stored without encryption is never silently confused with a
// KeyringSealer ciphertext: feeding a marked blob to KeyringSealer.Open
// fails secretbox authentication (loud), and feeding a real ciphertext
// to PlaintextSealer.Open fails this prefix check (loud) instead of
// handing back 24 bytes of nonce followed by garbage as if it were the
// secret. The trailing newline keeps it from being a valid UTF-8
// prefix of any realistic token by accident.
var plaintextMarker = []byte("particle:plaintext:v1\n")

// PlaintextSealer is a [Sealer] that does NOT encrypt: it stores the
// secret as-is, behind plaintextMarker. It's the fallback the CLI
// selects when the OS keyring is unavailable — most commonly Linux
// without a running D-Bus / Secret Service (headless servers, minimal
// containers) — so credential operations can proceed instead of
// failing outright.
//
// This is an availability-over-confidentiality tradeoff: secrets
// written through it sit in cleartext inside the SQLite file. Prefer
// [KeyringSealer] (or a host-provided Sealer backed by a KMS/HSM)
// whenever the environment can support one.
type PlaintextSealer struct{}

func (PlaintextSealer) Seal(plaintext []byte) ([]byte, error) {
	out := make([]byte, 0, len(plaintextMarker)+len(plaintext))
	out = append(out, plaintextMarker...)
	out = append(out, plaintext...)
	return out, nil
}

func (PlaintextSealer) Open(ciphertext []byte) ([]byte, error) {
	if !bytes.HasPrefix(ciphertext, plaintextMarker) {
		return nil, errors.New("credentials/sqlite: secret was not stored by PlaintextSealer — it is likely encrypted by the OS keyring, which is currently unavailable")
	}
	return ciphertext[len(plaintextMarker):], nil
}
