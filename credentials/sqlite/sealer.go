package sqlite

// Sealer is the encryption layer the [Store] applies to secret
// blobs before writing them to disk and inverts on read.
//
// Implementations must be safe for concurrent use — the Store may
// call Seal / Open from any goroutine.
type Sealer interface {
	// Seal returns the encrypted form of plaintext. The result
	// embeds whatever framing (nonce, version byte, …) the
	// implementation needs to invert with Open.
	Seal(plaintext []byte) ([]byte, error)

	// Open returns the plaintext for ciphertext, or an error if
	// the ciphertext is malformed, truncated, or fails
	// authentication.
	Open(ciphertext []byte) ([]byte, error)
}
