package main

import (
	"bytes"
	"strings"
	"testing"
)

// On a missing-required-flag failure, callers print the error
// text followed by writeToolUsage(...). This pins the combined
// output shape so we don't accidentally drop the schema-derived
// flag listing again.
func TestWriteToolUsage_PrintsDescriptionAndFlags(t *testing.T) {
	schema := []byte(`{
		"type":"object",
		"properties":{
			"input":{"type":"string","description":"YAML to parse"},
			"strict":{"type":"boolean","description":"reject unknown keys"}
		},
		"required":["input"]
	}`)
	fs, _, err := schemaToFlags("particle run yaml-tools@0.1.0 format", schema)
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	writeToolUsage(&out, fs, "particle run yaml-tools@0.1.0 format", "Format a YAML document")

	got := out.String()
	must := []string{
		"Format a YAML document",
		"Usage of particle run yaml-tools@0.1.0 format:",
		"-input string",
		"YAML to parse",
		"-strict",
		"reject unknown keys",
		"(required)",
	}
	for _, want := range must {
		if !strings.Contains(got, want) {
			t.Errorf("usage missing %q in:\n%s", want, got)
		}
	}
}

// Empty description shouldn't produce a blank line at the top —
// keeps the output crisp for tools that didn't bother with a
// description.
func TestWriteToolUsage_OmitsBlankDescriptionPrefix(t *testing.T) {
	fs, _, err := schemaToFlags("test", []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	writeToolUsage(&out, fs, "test", "")
	if strings.HasPrefix(out.String(), "\n") {
		t.Errorf("usage should not start with a blank line:\n%q", out.String())
	}
	if !strings.HasPrefix(out.String(), "Usage of test:") {
		t.Errorf("usage should start with the Usage line; got:\n%s", out.String())
	}
}
