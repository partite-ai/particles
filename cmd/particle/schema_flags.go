package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/pflag"
)

// schemaToFlags builds a [pflag.FlagSet] from a tool's JSON-Schema
// input definition and returns the set plus an `encode` closure
// that, after Parse, produces the JSON arguments object the
// runtime expects.
//
// Type mapping (JSON Schema 2020-12 with the subset particles
// actually emit — see internal/build introspect):
//
//   - "string"  → string flag
//   - "integer" → int64 flag
//   - "number"  → float64 flag
//   - "boolean" → bool flag
//   - "array" with primitive item type → repeated flag (--foo=a --foo=b)
//   - everything else (object, oneOf, untyped, …) → string flag
//     accepting the value as raw JSON so the caller still has an
//     escape hatch for non-primitive shapes
//
// Required properties get a "(required)" prefix in the help text
// and the encode closure errors when one wasn't supplied.
//
// progName is what pflag.Usage prints as the command name; pass
// something like "particle run yaml-tools format".
func schemaToFlags(progName string, schemaJSON []byte) (*pflag.FlagSet, func() (json.RawMessage, error), error) {
	var s argSchema
	if len(schemaJSON) > 0 {
		if err := json.Unmarshal(schemaJSON, &s); err != nil {
			return nil, nil, fmt.Errorf("parse input schema: %w", err)
		}
	}

	fs := pflag.NewFlagSet(progName, pflag.ContinueOnError)

	required := make(map[string]bool, len(s.Required))
	for _, r := range s.Required {
		required[r] = true
	}

	// Sort property names so --help output is deterministic and
	// the encoded JSON object's key order is stable across runs.
	names := make([]string, 0, len(s.Properties))
	for n := range s.Properties {
		names = append(names, n)
	}
	sort.Strings(names)

	type binding struct {
		name string
		get  func() (any, error)
	}
	bindings := make([]binding, 0, len(names))

	for _, name := range names {
		var prop propSchema
		_ = json.Unmarshal(s.Properties[name], &prop) // best-effort; falls through to JSON-string flag

		desc := prop.Description
		if required[name] {
			desc = "(required) " + desc
		}

		b, err := bindFlagForProp(fs, name, desc, prop)
		if err != nil {
			return nil, nil, err
		}
		bindings = append(bindings, binding{name: name, get: b})
	}

	encode := func() (json.RawMessage, error) {
		setNames := map[string]bool{}
		fs.Visit(func(f *pflag.Flag) { setNames[f.Name] = true })

		// Required-flag check happens before binding extraction
		// so the user sees "missing required" before any
		// JSON-parse error from a stray optional.
		for _, name := range s.Required {
			if !setNames[name] {
				return nil, fmt.Errorf("missing required flag --%s", name)
			}
		}

		out := map[string]any{}
		for _, b := range bindings {
			if !setNames[b.name] {
				continue
			}
			v, err := b.get()
			if err != nil {
				return nil, err
			}
			out[b.name] = v
		}
		return json.Marshal(out)
	}
	return fs, encode, nil
}

// bindFlagForProp registers one schema property on fs and returns
// a getter that reads the parsed value back as the correct Go
// type. Splitting this out keeps schemaToFlags readable.
func bindFlagForProp(fs *pflag.FlagSet, name, desc string, prop propSchema) (func() (any, error), error) {
	switch prop.Type {
	case "string":
		v := fs.String(name, "", desc)
		return func() (any, error) { return *v, nil }, nil
	case "integer":
		v := fs.Int64(name, 0, desc)
		return func() (any, error) { return *v, nil }, nil
	case "number":
		v := fs.Float64(name, 0, desc)
		return func() (any, error) { return *v, nil }, nil
	case "boolean":
		v := fs.Bool(name, false, desc)
		return func() (any, error) { return *v, nil }, nil
	case "array":
		return bindArray(fs, name, desc, prop)
	default:
		// Object, untyped, oneOf/anyOf, etc. — accept raw JSON.
		v := fs.String(name, "", desc+" (JSON)")
		return jsonGetter(name, v), nil
	}
}

// bindArray handles the "array" branch, supporting repeated
// `--foo=v` for arrays whose items are primitives. Non-primitive
// item types fall through to the JSON-string escape hatch.
func bindArray(fs *pflag.FlagSet, name, desc string, prop propSchema) (func() (any, error), error) {
	var items propSchema
	if len(prop.Items) > 0 {
		_ = json.Unmarshal(prop.Items, &items)
	}
	switch items.Type {
	case "", "string":
		v := &stringSliceVar{}
		fs.Var(v, name, desc+" (repeatable)")
		return func() (any, error) { return v.vals, nil }, nil
	case "integer":
		v := &int64SliceVar{}
		fs.Var(v, name, desc+" (repeatable)")
		return func() (any, error) { return v.vals, nil }, nil
	case "number":
		v := &float64SliceVar{}
		fs.Var(v, name, desc+" (repeatable)")
		return func() (any, error) { return v.vals, nil }, nil
	case "boolean":
		v := &boolSliceVar{}
		fs.Var(v, name, desc+" (repeatable)")
		return func() (any, error) { return v.vals, nil }, nil
	default:
		v := fs.String(name, "", desc+" (JSON array)")
		return jsonGetter(name, v), nil
	}
}

func jsonGetter(name string, v *string) func() (any, error) {
	return func() (any, error) {
		var raw json.RawMessage
		if err := json.Unmarshal([]byte(*v), &raw); err != nil {
			return nil, fmt.Errorf("flag --%s: parse JSON: %w", name, err)
		}
		return raw, nil
	}
}

// argSchema is the subset of JSON Schema schemaToFlags consumes.
// Properties are kept as raw bytes so we can inspect each
// property's "type" lazily without needing a discriminated union.
type argSchema struct {
	Type       string                     `json:"type"`
	Properties map[string]json.RawMessage `json:"properties"`
	Required   []string                   `json:"required"`
}

type propSchema struct {
	Type        string          `json:"type"`
	Description string          `json:"description"`
	Items       json.RawMessage `json:"items"` // examined when Type == "array"
}

// -----------------------------------------------------------------------------
// Repeated-value flag types for primitive arrays.
//
// pflag.Value adds a Type() string method on top of flag.Value;
// each slice type implements it so pflag can render a useful
// "(string)"/"(int)"/"(float)"/"(bool)" hint in --help output.
// -----------------------------------------------------------------------------

type stringSliceVar struct{ vals []string }

func (s *stringSliceVar) String() string {
	if s == nil {
		return ""
	}
	return strings.Join(s.vals, ",")
}
func (s *stringSliceVar) Set(v string) error  { s.vals = append(s.vals, v); return nil }
func (s *stringSliceVar) Type() string        { return "stringSlice" }

type int64SliceVar struct{ vals []int64 }

func (s *int64SliceVar) String() string {
	if s == nil {
		return ""
	}
	parts := make([]string, len(s.vals))
	for i, v := range s.vals {
		parts[i] = fmt.Sprint(v)
	}
	return strings.Join(parts, ",")
}
func (s *int64SliceVar) Set(v string) error {
	var n int64
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return fmt.Errorf("not an integer: %q", v)
	}
	s.vals = append(s.vals, n)
	return nil
}
func (s *int64SliceVar) Type() string { return "intSlice" }

type float64SliceVar struct{ vals []float64 }

func (s *float64SliceVar) String() string {
	if s == nil {
		return ""
	}
	parts := make([]string, len(s.vals))
	for i, v := range s.vals {
		parts[i] = fmt.Sprint(v)
	}
	return strings.Join(parts, ",")
}
func (s *float64SliceVar) Set(v string) error {
	var n float64
	if _, err := fmt.Sscanf(v, "%g", &n); err != nil {
		return fmt.Errorf("not a number: %q", v)
	}
	s.vals = append(s.vals, n)
	return nil
}
func (s *float64SliceVar) Type() string { return "floatSlice" }

type boolSliceVar struct{ vals []bool }

func (s *boolSliceVar) String() string {
	if s == nil {
		return ""
	}
	parts := make([]string, len(s.vals))
	for i, v := range s.vals {
		parts[i] = fmt.Sprint(v)
	}
	return strings.Join(parts, ",")
}
func (s *boolSliceVar) Set(v string) error {
	switch v {
	case "true", "1":
		s.vals = append(s.vals, true)
	case "false", "0":
		s.vals = append(s.vals, false)
	default:
		return fmt.Errorf("not a bool: %q", v)
	}
	return nil
}
func (s *boolSliceVar) Type() string { return "boolSlice" }
