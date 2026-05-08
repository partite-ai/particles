package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func parsedJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("encoded args isn't valid JSON: %v\nraw: %s", err, b)
	}
	return out
}

// All five primitive types round-trip through their flag bindings.
func TestSchemaToFlags_Primitives(t *testing.T) {
	schema := []byte(`{
		"type":"object",
		"properties":{
			"name":{"type":"string","description":"the name"},
			"count":{"type":"integer"},
			"ratio":{"type":"number"},
			"verbose":{"type":"boolean"}
		}
	}`)
	fs, encode, err := schemaToFlags("test", schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Parse([]string{
		"--name=alice", "--count=7", "--ratio=2.5", "--verbose",
	}); err != nil {
		t.Fatal(err)
	}
	args, err := encode()
	if err != nil {
		t.Fatal(err)
	}
	got := parsedJSON(t, args)
	want := map[string]any{
		"name":    "alice",
		"count":   float64(7), // JSON numbers come back as float64
		"ratio":   float64(2.5),
		"verbose": true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("encoded = %v, want %v", got, want)
	}
}

// Unset optionals are omitted from the encoded JSON — the runtime
// distinguishes "not provided" from "zero value", so we have to
// preserve absence rather than emit zeroes for every property.
func TestSchemaToFlags_OmitsUnsetOptionals(t *testing.T) {
	schema := []byte(`{
		"type":"object",
		"properties":{
			"name":{"type":"string"},
			"count":{"type":"integer"}
		}
	}`)
	fs, encode, err := schemaToFlags("test", schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Parse([]string{"--name=alice"}); err != nil {
		t.Fatal(err)
	}
	args, _ := encode()
	got := parsedJSON(t, args)
	if _, present := got["count"]; present {
		t.Errorf("encoded should omit unset 'count': %v", got)
	}
	if got["name"] != "alice" {
		t.Errorf("name = %v", got["name"])
	}
}

// Required flags missing from the parse error out of encode with
// a clear message.
func TestSchemaToFlags_RequiredFlagMissing(t *testing.T) {
	schema := []byte(`{
		"type":"object",
		"properties":{"name":{"type":"string"}},
		"required":["name"]
	}`)
	fs, encode, err := schemaToFlags("test", schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Parse([]string{}); err != nil {
		t.Fatal(err)
	}
	if _, err := encode(); err == nil {
		t.Error("encode should error for missing required flag")
	}
}

// Repeated `--tag=v` values bind into a string slice.
func TestSchemaToFlags_StringArray_Repeated(t *testing.T) {
	schema := []byte(`{
		"type":"object",
		"properties":{
			"tags":{"type":"array","items":{"type":"string"}}
		}
	}`)
	fs, encode, err := schemaToFlags("test", schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Parse([]string{"--tags=alpha", "--tags=beta", "--tags=gamma"}); err != nil {
		t.Fatal(err)
	}
	args, _ := encode()
	got := parsedJSON(t, args)
	want := []any{"alpha", "beta", "gamma"}
	if !reflect.DeepEqual(got["tags"], want) {
		t.Errorf("tags = %v, want %v", got["tags"], want)
	}
}

// Integer arrays parse each value as int64.
func TestSchemaToFlags_IntArray_Repeated(t *testing.T) {
	schema := []byte(`{
		"type":"object",
		"properties":{
			"ids":{"type":"array","items":{"type":"integer"}}
		}
	}`)
	fs, encode, err := schemaToFlags("test", schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Parse([]string{"--ids=10", "--ids=20"}); err != nil {
		t.Fatal(err)
	}
	args, _ := encode()
	got := parsedJSON(t, args)
	want := []any{float64(10), float64(20)}
	if !reflect.DeepEqual(got["ids"], want) {
		t.Errorf("ids = %v, want %v", got["ids"], want)
	}
}

// Object/untyped properties accept raw JSON via a string flag, so
// callers who really need a nested shape have an escape hatch.
func TestSchemaToFlags_ObjectAcceptsJSON(t *testing.T) {
	schema := []byte(`{
		"type":"object",
		"properties":{
			"opts":{"type":"object","description":"options"}
		}
	}`)
	fs, encode, err := schemaToFlags("test", schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Parse([]string{`--opts={"a":1,"b":"x"}`}); err != nil {
		t.Fatal(err)
	}
	args, _ := encode()
	got := parsedJSON(t, args)
	wantOpts := map[string]any{"a": float64(1), "b": "x"}
	if !reflect.DeepEqual(got["opts"], wantOpts) {
		t.Errorf("opts = %v, want %v", got["opts"], wantOpts)
	}
}

// Invalid JSON in a JSON-style flag surfaces a per-flag error
// rather than a generic encode failure — gives the user a clear
// pointer to which flag was wrong.
func TestSchemaToFlags_ObjectRejectsBadJSON(t *testing.T) {
	schema := []byte(`{
		"type":"object",
		"properties":{"opts":{"type":"object"}}
	}`)
	fs, encode, _ := schemaToFlags("test", schema)
	_ = fs.Parse([]string{`--opts=not-json`})
	_, err := encode()
	if err == nil || !strings.Contains(err.Error(), "--opts") {
		t.Errorf("err = %v, want error mentioning --opts", err)
	}
}

// --help triggers flag.ErrHelp and prints the per-property
// documentation produced from the schema. We capture flag output
// to verify each property and its description shows up.
func TestSchemaToFlags_HelpListsAllProperties(t *testing.T) {
	schema := []byte(`{
		"type":"object",
		"properties":{
			"input":{"type":"string","description":"YAML to parse"},
			"strict":{"type":"boolean","description":"reject unknown keys"}
		},
		"required":["input"]
	}`)
	fs, _, err := schemaToFlags("test", schema)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	fs.SetOutput(&out)

	err = fs.Parse([]string{"--help"})
	if err == nil {
		t.Fatal("expected ErrHelp")
	}

	got := out.String()
	if !strings.Contains(got, "-input") || !strings.Contains(got, "YAML to parse") {
		t.Errorf("help missing 'input': %s", got)
	}
	if !strings.Contains(got, "-strict") || !strings.Contains(got, "reject unknown keys") {
		t.Errorf("help missing 'strict': %s", got)
	}
	if !strings.Contains(got, "(required)") {
		t.Error("help should mark required flags")
	}
}

// An empty schema (no properties) still produces a usable
// FlagSet — encode() returns "{}" so a no-arg tool can be called.
func TestSchemaToFlags_EmptySchema(t *testing.T) {
	fs, encode, err := schemaToFlags("test", []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	args, err := encode()
	if err != nil {
		t.Fatal(err)
	}
	if string(args) != "{}" {
		t.Errorf("encoded = %q, want {}", args)
	}
}

// Malformed schema JSON surfaces from the constructor, not at
// Parse time. (Particles built through our pipeline never produce
// these, but third-party tooling could.)
func TestSchemaToFlags_MalformedSchema(t *testing.T) {
	if _, _, err := schemaToFlags("test", []byte(`{not json`)); err == nil {
		t.Error("expected error for malformed schema")
	}
}
