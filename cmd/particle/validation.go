package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// validateArgs runs jsonschema-go validation on the encoded args
// against the tool's input schema and returns a flag-aware error
// message (or nil if everything checked out).
//
// The runtime ALSO validates before entering wasm — that
// validation is the source of truth. We do it again here so the
// CLI can produce friendlier error messages that name CLI flags
// rather than JSON-Pointer paths.
//
// schemaJSON empty → no validation; matches the runtime's behavior
// for tools without an input schema.
func validateArgs(schemaJSON, argsJSON []byte) error {
	if len(schemaJSON) == 0 {
		return nil
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(schemaJSON, &s); err != nil {
		return fmt.Errorf("compile schema: %w", err)
	}
	resolved, err := s.Resolve(nil)
	if err != nil {
		return fmt.Errorf("compile schema: %w", err)
	}

	raw := argsJSON
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	var instance any
	if err := json.Unmarshal(raw, &instance); err != nil {
		return fmt.Errorf("parse arguments: %w", err)
	}
	if err := resolved.Validate(instance); err != nil {
		return fmt.Errorf("%s", flagAwareValidationError(err))
	}
	return nil
}

// flagAwareValidationError turns jsonschema-go's verbose "validating
// /properties/<prop>: ..." messages into something CLI-shaped:
// "--<prop>: ...". Also collapses "missing properties: [\"a\",\"b\"]"
// into "missing required flag --a" / "missing required flags:
// --a, --b". Anything we don't recognize falls through unchanged
// so we never accidentally drop information.
func flagAwareValidationError(err error) string {
	msg := err.Error()

	if matches := missingPropertiesRe.FindStringSubmatch(msg); matches != nil {
		names := parseJSONStringList(matches[1])
		flags := make([]string, 0, len(names))
		for _, n := range names {
			if n != "" {
				flags = append(flags, "--"+n)
			}
		}
		switch len(flags) {
		case 0:
			// fall through — couldn't parse
		case 1:
			return "missing required flag " + flags[0]
		default:
			return "missing required flags: " + strings.Join(flags, ", ")
		}
	}

	if matches := propPathRe.FindStringSubmatch(msg); matches != nil {
		prop := matches[1]
		rest := strings.TrimSpace(matches[2])
		return "--" + prop + ": " + rest
	}

	return msg
}

// missingPropertiesRe matches the library's "required: missing
// properties: [\"a\",\"b\"]" framing.
var missingPropertiesRe = regexp.MustCompile(`required: missing properties:\s*\[([^\]]+)\]`)

// propPathRe matches "validating <whatever>/properties/<name>: <rest>"
// — the "<name>" capture is the offending property and "<rest>" is
// the human-readable detail.
var propPathRe = regexp.MustCompile(`validating [^:]*?/properties/([A-Za-z_][A-Za-z0-9_-]*):\s*(.*)`)

// parseJSONStringList extracts every JSON string literal from s.
// jsonschema-go formats the missing-properties list as Go's
// fmt %v of a string slice — `"a" "b"` (space-separated, no
// commas) — so a JSON-array decode wouldn't work. Pulling out
// the quoted segments handles both the space-separated and the
// comma-separated framings without needing to know which one we
// got.
func parseJSONStringList(s string) []string {
	quoted := stringLitRe.FindAllString(s, -1)
	out := make([]string, 0, len(quoted))
	for _, q := range quoted {
		var v string
		if err := json.Unmarshal([]byte(q), &v); err == nil {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// stringLitRe matches one JSON string literal, including escaped
// quotes inside.
var stringLitRe = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
