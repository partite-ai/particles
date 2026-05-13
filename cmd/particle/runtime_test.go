package main

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/partite-ai/particle/registry"
)

// fullStubRegistry adds Get/Put/Delete on top of the listing-only
// stubRegistry (defined in ping_test.go) so resolveParticle can
// be exercised end-to-end.
type fullStubRegistry struct {
	stubRegistry
	got map[string]registry.Entry // keyed by "name@version"
}

func (r *fullStubRegistry) Get(_ context.Context, name, version string) (registry.Entry, error) {
	e, ok := r.got[name+"@"+version]
	if !ok {
		return registry.Entry{}, registry.ErrNotFound
	}
	return e, nil
}

func TestResolveParticle_ExactVersion(t *testing.T) {
	reg := &fullStubRegistry{
		stubRegistry: stubRegistry{entries: []registry.ListEntry{
			{Name: "yaml-tools", Version: "0.1.0"},
		}},
		got: map[string]registry.Entry{
			"yaml-tools@0.1.0": {Name: "yaml-tools", Version: "0.1.0", Particle: fstest.MapFS{}},
		},
	}
	e, err := resolveParticle(context.Background(), reg, "yaml-tools@0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "yaml-tools" || e.Version != "0.1.0" {
		t.Errorf("entry = %+v", e)
	}
}

// Omitted version + multiple entries → resolver picks the highest
// semver and Get is called with that version.
func TestResolveParticle_OmittedVersion_PicksLatest(t *testing.T) {
	reg := &fullStubRegistry{
		stubRegistry: stubRegistry{entries: []registry.ListEntry{
			{Name: "yaml-tools", Version: "0.2.0"},
			{Name: "yaml-tools", Version: "0.10.0"},
		}},
		got: map[string]registry.Entry{
			"yaml-tools@0.10.0": {Name: "yaml-tools", Version: "0.10.0", Particle: fstest.MapFS{}},
		},
	}
	e, err := resolveParticle(context.Background(), reg, "yaml-tools")
	if err != nil {
		t.Fatal(err)
	}
	if e.Version != "0.10.0" {
		t.Errorf("version = %q, want 0.10.0", e.Version)
	}
}

func TestResolveParticle_NotRegistered(t *testing.T) {
	reg := &fullStubRegistry{got: map[string]registry.Entry{}}
	_, err := resolveParticle(context.Background(), reg, "missing")
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Errorf("err = %v, want 'not registered'", err)
	}
}

// Asking for a name+version pair that isn't in the registry
// surfaces a "<name>@<version> not registered" message rather
// than the bare ErrNotFound.
func TestResolveParticle_GetMiss(t *testing.T) {
	reg := &fullStubRegistry{
		stubRegistry: stubRegistry{entries: []registry.ListEntry{
			{Name: "yaml-tools", Version: "0.1.0"},
		}},
		got: map[string]registry.Entry{}, // Get returns ErrNotFound
	}
	_, err := resolveParticle(context.Background(), reg, "yaml-tools@9.9.9")
	if err == nil || !strings.Contains(err.Error(), "yaml-tools@9.9.9 not registered") {
		t.Errorf("err = %v, want 'yaml-tools@9.9.9 not registered'", err)
	}
}

func TestResolveParticle_RejectsEmptyName(t *testing.T) {
	reg := &fullStubRegistry{got: map[string]registry.Entry{}}
	_, err := resolveParticle(context.Background(), reg, "")
	if err == nil {
		t.Error("expected error for empty target")
	}
}

// Hard-fail (not ErrNotFound) from Get propagates with context.
func TestResolveParticle_PropagatesGetError(t *testing.T) {
	boom := errors.New("disk failed")
	reg := &errReg{err: boom}
	_, err := resolveParticle(context.Background(), reg, "x@1.0.0")
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want chain containing %v", err, boom)
	}
}

// errReg returns a non-NotFound error from Get for the storage-
// failure path. List returns nil so the resolver moves to Get.
type errReg struct{ err error }

func (r *errReg) List(context.Context) ([]registry.ListEntry, error) { return nil, nil }
func (r *errReg) Get(context.Context, string, string) (registry.Entry, error) {
	return registry.Entry{}, r.err
}
func (r *errReg) Put(context.Context, string, string, fs.FS) error { panic("unused") }
func (r *errReg) Delete(context.Context, string, string) error      { panic("unused") }
