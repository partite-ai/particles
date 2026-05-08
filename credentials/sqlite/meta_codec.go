package sqlite

import (
	"encoding/json"
	"fmt"

	"github.com/partite-ai/particle/credentials"
)

// marshalMeta encodes a typed Metadata value into the (kind, json)
// pair the schema stores.
//
// The kind column drives unmarshal dispatch — see [unmarshalMeta].
// The JSON itself uses the public field names of the concrete
// struct via stdlib encoding rules; no discriminator inside the
// JSON, since the schema column already carries it.
func marshalMeta(m credentials.Metadata) (kind string, data []byte, err error) {
	data, err = json.Marshal(m)
	if err != nil {
		return "", nil, fmt.Errorf("marshal %s metadata: %w", m.Kind(), err)
	}
	return string(m.Kind()), data, nil
}

// unmarshalMeta decodes a row's (kind, meta_json) back into the
// typed Metadata. Unknown kinds are an error: a future schema
// migration might add new kinds, and silently returning a zero
// value would let downstream code make decisions on the wrong
// type.
func unmarshalMeta(kind string, data []byte) (credentials.Metadata, error) {
	switch credentials.Kind(kind) {
	case credentials.KindBasic:
		var v credentials.BasicMeta
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("unmarshal basic metadata: %w", err)
		}
		return v, nil
	case credentials.KindOAuth2:
		var v credentials.OAuth2Meta
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("unmarshal oauth2 metadata: %w", err)
		}
		return v, nil
	case credentials.KindAPIKey:
		var v credentials.APIKeyMeta
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("unmarshal apikey metadata: %w", err)
		}
		return v, nil
	case credentials.KindSigningKey:
		var v credentials.SigningKeyMeta
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("unmarshal signing-key metadata: %w", err)
		}
		return v, nil
	case credentials.KindRaw:
		return credentials.RawMeta{}, nil
	}
	return nil, fmt.Errorf("unknown credential kind %q", kind)
}
