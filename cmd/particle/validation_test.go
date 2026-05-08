package main

import (
	"strings"
	"testing"
)

// validateArgs returns a flag-aware string for the common
// validation failure shapes; we verify each translation here so
// regressions in jsonschema-go's wording are caught immediately.

func TestValidateArgs_OK(t *testing.T) {
	schema := []byte(`{
		"type":"object",
		"properties":{"a":{"type":"number"}},
		"required":["a"]
	}`)
	if err := validateArgs(schema, []byte(`{"a":1}`)); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateArgs_NoSchema(t *testing.T) {
	if err := validateArgs(nil, []byte(`{"anything":"goes"}`)); err != nil {
		t.Errorf("unexpected error for nil schema: %v", err)
	}
}

func TestValidateArgs_MissingSingleRequired(t *testing.T) {
	schema := []byte(`{
		"type":"object",
		"properties":{"a":{"type":"number"},"b":{"type":"number"}},
		"required":["a","b"]
	}`)
	err := validateArgs(schema, []byte(`{"a":1}`))
	if err == nil {
		t.Fatal("expected validation error")
	}
	if got := err.Error(); got != "missing required flag --b" {
		t.Errorf("err = %q, want %q", got, "missing required flag --b")
	}
}

func TestValidateArgs_MissingMultipleRequired(t *testing.T) {
	schema := []byte(`{
		"type":"object",
		"properties":{"a":{"type":"number"},"b":{"type":"number"}},
		"required":["a","b"]
	}`)
	err := validateArgs(schema, []byte(`{}`))
	if err == nil {
		t.Fatal("expected validation error")
	}
	got := err.Error()
	// We don't assume order from jsonschema-go; check both flags
	// appear and the prefix is plural.
	if !strings.HasPrefix(got, "missing required flags: ") {
		t.Errorf("err = %q, want plural prefix", got)
	}
	if !strings.Contains(got, "--a") || !strings.Contains(got, "--b") {
		t.Errorf("err = %q, should mention both --a and --b", got)
	}
}

func TestValidateArgs_WrongType_NamesFlag(t *testing.T) {
	schema := []byte(`{
		"type":"object",
		"properties":{"count":{"type":"integer"}}
	}`)
	err := validateArgs(schema, []byte(`{"count":"abc"}`))
	if err == nil {
		t.Fatal("expected validation error")
	}
	got := err.Error()
	if !strings.HasPrefix(got, "--count: ") {
		t.Errorf("err = %q, should start with '--count: '", got)
	}
}

// Empty argsJSON validates as `{}` — required-fields trigger,
// non-required schemas pass.
func TestValidateArgs_EmptyArgsTreatedAsObject(t *testing.T) {
	schema := []byte(`{
		"type":"object",
		"properties":{"opt":{"type":"string"}}
	}`)
	if err := validateArgs(schema, nil); err != nil {
		t.Errorf("nil args with all-optional schema: %v", err)
	}

	schema = []byte(`{
		"type":"object",
		"properties":{"req":{"type":"string"}},
		"required":["req"]
	}`)
	if err := validateArgs(schema, nil); err == nil {
		t.Error("nil args with required field: expected error")
	}
}

func TestValidateArgs_MalformedSchema(t *testing.T) {
	if err := validateArgs([]byte(`{not json`), []byte(`{}`)); err == nil {
		t.Error("expected error for malformed schema")
	}
}

func TestValidateArgs_MalformedArgs(t *testing.T) {
	schema := []byte(`{"type":"object"}`)
	if err := validateArgs(schema, []byte(`{not json`)); err == nil {
		t.Error("expected error for malformed args")
	}
}

func TestParseJSONStringList(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`"a"`, []string{"a"}},
		// Space-separated (Go's fmt %v of a slice — what
		// jsonschema-go actually emits).
		{`"a" "b"`, []string{"a", "b"}},
		// Comma-separated (defensive support — a future
		// library version might switch).
		{`"a","b"`, []string{"a", "b"}},
		{`"a", "b", "c"`, []string{"a", "b", "c"}},
		{`broken`, nil},
		{``, nil},
	}
	for _, c := range cases {
		got := parseJSONStringList(c.in)
		if len(got) != len(c.want) {
			t.Errorf("parseJSONStringList(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("parseJSONStringList(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}
