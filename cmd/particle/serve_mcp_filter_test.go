package main

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/partite-ai/particles/runtime"
)

func toolsNamed(names ...string) []runtime.ToolDef {
	out := make([]runtime.ToolDef, len(names))
	for i, n := range names {
		out[i] = runtime.ToolDef{Name: n}
	}
	return out
}

func nameSet(tools []runtime.ToolDef) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Name
	}
	sort.Strings(out)
	return out
}

func TestFilterTools_NoFlags_PassesThrough(t *testing.T) {
	tools := toolsNamed("a", "b", "c")
	got, err := filterTools(tools, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, tools) {
		t.Errorf("got %v, want %v", got, tools)
	}
}

func TestFilterTools_Allowlist(t *testing.T) {
	tools := toolsNamed("a", "b", "c")
	got, err := filterTools(tools, []string{"a", "c"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "c"}
	if !reflect.DeepEqual(nameSet(got), want) {
		t.Errorf("got %v, want %v", nameSet(got), want)
	}
}

func TestFilterTools_Denylist(t *testing.T) {
	tools := toolsNamed("a", "b", "c")
	got, err := filterTools(tools, nil, []string{"b"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "c"}
	if !reflect.DeepEqual(nameSet(got), want) {
		t.Errorf("got %v, want %v", nameSet(got), want)
	}
}

// Filter preserves the source ordering — important because the
// runtime's ListTools is what defines a stable, manifest-derived
// order, and any reordering at the filter layer would surprise
// MCP clients that index by position.
func TestFilterTools_PreservesOrdering(t *testing.T) {
	tools := toolsNamed("z", "a", "m")
	got, err := filterTools(tools, nil, []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Name != "z" || got[1].Name != "m" {
		t.Errorf("got %v, expected source order minus 'a'", nameSet(got))
	}
}

// Both flags non-empty is rejected with a message naming both —
// even though cobra also enforces this at the CLI, the helper is
// callable on its own and shouldn't trust its caller blindly.
func TestFilterTools_BothFlagsRejected(t *testing.T) {
	tools := toolsNamed("a")
	_, err := filterTools(tools, []string{"a"}, []string{"a"})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("err = %v, want one mentioning 'mutually exclusive'", err)
	}
}

// Allowlist with an unknown name errors so a typo doesn't
// silently expose nothing.
func TestFilterTools_UnknownInAllowlist(t *testing.T) {
	tools := toolsNamed("a", "b")
	_, err := filterTools(tools, []string{"typo"}, nil)
	if err == nil || !strings.Contains(err.Error(), `"typo"`) {
		t.Errorf("err = %v, want one mentioning typo", err)
	}
	if err == nil || !strings.Contains(err.Error(), "have:") {
		t.Errorf("err should list known tools; got %v", err)
	}
}

// Same for denylist.
func TestFilterTools_UnknownInDenylist(t *testing.T) {
	tools := toolsNamed("a", "b")
	_, err := filterTools(tools, nil, []string{"typo"})
	if err == nil || !strings.Contains(err.Error(), `"typo"`) {
		t.Errorf("err = %v, want one mentioning typo", err)
	}
}

// Allowlist that matches every tool → identity. Denylist that
// matches every tool → empty (degenerate but valid).
func TestFilterTools_DegenerateButValid(t *testing.T) {
	tools := toolsNamed("a", "b")

	got, err := filterTools(tools, []string{"a", "b"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("identity allowlist: got %d tools, want 2", len(got))
	}

	got, err = filterTools(tools, nil, []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("full denylist: got %d tools, want 0", len(got))
	}
}
