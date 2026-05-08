package main

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/partite-ai/particle/registry"
	"github.com/partite-ai/particle/runtime"
)

func TestParsePingTarget(t *testing.T) {
	cases := []struct {
		in   string
		name string
		ver  string
	}{
		{"yaml-tools", "yaml-tools", ""},
		{"yaml-tools@0.1.0", "yaml-tools", "0.1.0"},
		// Versions with their own '@' (uncommon but legal in semver
		// build metadata) split on the first '@' only.
		{"a@1.0.0+build@2", "a", "1.0.0+build@2"},
		{"@no-name", "", "no-name"},
		{"", "", ""},
	}
	for _, c := range cases {
		gotName, gotVer := parsePingTarget(c.in)
		if gotName != c.name || gotVer != c.ver {
			t.Errorf("parsePingTarget(%q) = (%q, %q), want (%q, %q)",
				c.in, gotName, gotVer, c.name, c.ver)
		}
	}
}

// stubRegistry is a minimal registry.Registry — only List is
// consulted by resolveSingleVersion.
type stubRegistry struct {
	entries []registry.ListEntry
	err     error
}

func (r *stubRegistry) List(context.Context) ([]registry.ListEntry, error) {
	return r.entries, r.err
}
func (r *stubRegistry) Get(context.Context, string, string) (registry.Entry, error) {
	panic("unused")
}
func (r *stubRegistry) Put(context.Context, string, string, fs.FS) error { panic("unused") }
func (r *stubRegistry) Delete(context.Context, string, string) error      { panic("unused") }
func (r *stubRegistry) SetSelectedAuthenticationMethod(context.Context, string, string) error {
	panic("unused")
}

func TestResolveLatestVersion_Unique(t *testing.T) {
	reg := &stubRegistry{entries: []registry.ListEntry{
		{Name: "yaml-tools", Version: "0.1.0"},
		{Name: "json-tools", Version: "0.2.0"},
	}}
	v, err := resolveLatestVersion(context.Background(), reg, "yaml-tools")
	if err != nil {
		t.Fatal(err)
	}
	if v != "0.1.0" {
		t.Errorf("version = %q", v)
	}
}

func TestResolveLatestVersion_PicksHighestSemver(t *testing.T) {
	// 0.10.0 is HIGHER than 0.2.0 in semver but LOWER lexically —
	// pinning that we sort by semver, not lexical order.
	reg := &stubRegistry{entries: []registry.ListEntry{
		{Name: "yaml-tools", Version: "0.2.0"},
		{Name: "yaml-tools", Version: "0.10.0"},
		{Name: "yaml-tools", Version: "0.1.0"},
	}}
	v, err := resolveLatestVersion(context.Background(), reg, "yaml-tools")
	if err != nil {
		t.Fatal(err)
	}
	if v != "0.10.0" {
		t.Errorf("version = %q, want 0.10.0", v)
	}
}

// Prerelease tags rank below their base version per semver.
func TestResolveLatestVersion_PrereleaseRanksBelowRelease(t *testing.T) {
	reg := &stubRegistry{entries: []registry.ListEntry{
		{Name: "yaml-tools", Version: "1.0.0-rc.1"},
		{Name: "yaml-tools", Version: "1.0.0"},
	}}
	v, err := resolveLatestVersion(context.Background(), reg, "yaml-tools")
	if err != nil {
		t.Fatal(err)
	}
	if v != "1.0.0" {
		t.Errorf("version = %q, want 1.0.0", v)
	}
}

func TestResolveLatestVersion_Missing(t *testing.T) {
	reg := &stubRegistry{}
	_, err := resolveLatestVersion(context.Background(), reg, "yaml-tools")
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Errorf("err = %v, want 'not registered'", err)
	}
}

func TestResolveLatestVersion_PropagatesListError(t *testing.T) {
	boom := errors.New("disk failed")
	reg := &stubRegistry{err: boom}
	_, err := resolveLatestVersion(context.Background(), reg, "yaml-tools")
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want chain containing %v", err, boom)
	}
}

func TestFormatPing(t *testing.T) {
	cases := []struct {
		pr   *runtime.PingResult
		want string
	}{
		{&runtime.PingResult{Status: runtime.PingStatusOK}, "ok"},
		{&runtime.PingResult{Status: runtime.PingStatusOK, Message: "all good"}, "ok: all good"},
		{&runtime.PingResult{Status: runtime.PingStatusDegraded, Message: "cache cold", Details: "rebuilding 12%"},
			"degraded: cache cold (rebuilding 12%)"},
		{&runtime.PingResult{Status: runtime.PingStatusUnhealthy}, "unhealthy"},
		// Details without a message still renders.
		{&runtime.PingResult{Status: runtime.PingStatusOK, Details: "x"}, "ok (x)"},
	}
	for i, c := range cases {
		if got := formatPing(c.pr); got != c.want {
			t.Errorf("[%d] formatPing(%+v) = %q, want %q", i, c.pr, got, c.want)
		}
	}
}
