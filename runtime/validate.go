package runtime

import (
	"encoding/json"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
)

// compileToolSchema parses raw JSON-Schema bytes and resolves it
// into a form ready for [jsonschema.Resolved.Validate].
//
// Empty input yields a nil resolved schema, which the caller
// treats as "no validation" — some particles legitimately have
// tools with no declared input shape, and the runtime shouldn't
// invent constraints we'd then have to enforce.
func compileToolSchema(raw []byte) (*jsonschema.Resolved, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	resolved, err := s.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("resolve schema: %w", err)
	}
	return resolved, nil
}

// validateToolInput checks argumentsJSON against the resolved
// schema. Empty argumentsJSON validates as the empty object so
// no-arg tools can be called without the host having to write
// "{}" explicitly.
//
// nil schema → nil error (the caller already decided no
// validation applies).
func validateToolInput(schema *jsonschema.Resolved, argumentsJSON []byte) error {
	if schema == nil {
		return nil
	}
	raw := argumentsJSON
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	var instance any
	if err := json.Unmarshal(raw, &instance); err != nil {
		return fmt.Errorf("parse arguments: %w", err)
	}
	return schema.Validate(instance)
}
