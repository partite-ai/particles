package main

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/partite-ai/particle/registry"
)

// listingRegistry is a tiny test stand-in: List returns a fixed
// slice; Delete is unused (versionsToDelete only consults List).
type listingRegistry struct {
	entries []registry.ListEntry
	listErr error
}

func (r *listingRegistry) List(context.Context) ([]registry.ListEntry, error) {
	return r.entries, r.listErr
}
func (r *listingRegistry) Get(context.Context, string, string) (registry.Entry, error) {
	panic("unused")
}
func (r *listingRegistry) Put(context.Context, string, string, fs.FS) error { panic("unused") }
func (r *listingRegistry) Delete(context.Context, string, string) error      { panic("unused") }
func (r *listingRegistry) SetSelectedAuthenticationMethod(context.Context, string, string) error {
	panic("unused")
}

func TestVersionsToDelete_ExplicitVersion(t *testing.T) {
	reg := &listingRegistry{entries: []registry.ListEntry{
		{Name: "yaml-tools", Version: "0.1.0"},
		{Name: "yaml-tools", Version: "0.2.0"},
	}}
	got, err := versionsToDelete(context.Background(), reg, "yaml-tools", "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "0.1.0" {
		t.Errorf("got %v, want [0.1.0]", got)
	}
}

func TestVersionsToDelete_AllVersions(t *testing.T) {
	reg := &listingRegistry{entries: []registry.ListEntry{
		{Name: "yaml-tools", Version: "0.1.0"},
		{Name: "yaml-tools", Version: "0.2.0"},
		{Name: "json-tools", Version: "0.1.0"},
	}}
	got, err := versionsToDelete(context.Background(), reg, "yaml-tools", "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"0.1.0", "0.2.0"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Specific (name, version) that isn't registered → error so a
// typo doesn't silently no-op.
func TestVersionsToDelete_ExplicitMissing(t *testing.T) {
	reg := &listingRegistry{entries: []registry.ListEntry{
		{Name: "yaml-tools", Version: "0.1.0"},
	}}
	_, err := versionsToDelete(context.Background(), reg, "yaml-tools", "9.9.9")
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Errorf("err = %v, want one mentioning 'not registered'", err)
	}
}

// Empty-name lookup with no matching entries also errors.
func TestVersionsToDelete_NameMissing(t *testing.T) {
	reg := &listingRegistry{entries: []registry.ListEntry{}}
	_, err := versionsToDelete(context.Background(), reg, "absent", "")
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Errorf("err = %v, want 'not registered'", err)
	}
}

func TestVersionsToDelete_ListError(t *testing.T) {
	boom := errors.New("disk failed")
	reg := &listingRegistry{listErr: boom}
	_, err := versionsToDelete(context.Background(), reg, "p", "")
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want chain containing %v", err, boom)
	}
}
