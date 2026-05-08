package memory_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/partite-ai/particle/kv"
	"github.com/partite-ai/particle/kv/memory"
)

func TestSet_Get(t *testing.T) {
	s := memory.New()
	ctx := context.Background()
	if err := s.Set(ctx, "p", "k", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, found, err := s.Get(ctx, "p", "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	if v != "v" {
		t.Errorf("v = %q, want v", v)
	}
}

func TestGet_NotFound(t *testing.T) {
	s := memory.New()
	ctx := context.Background()

	_, found, err := s.Get(ctx, "p", "missing")
	if err != nil {
		t.Errorf("err = %v, want nil for missing key", err)
	}
	if found {
		t.Error("found = true for missing key")
	}

	// Same for absent particle.
	_, found, err = s.Get(ctx, "absent", "any")
	if err != nil {
		t.Errorf("err = %v, want nil for absent particle", err)
	}
	if found {
		t.Error("found = true for absent particle")
	}
}

func TestSet_Replaces(t *testing.T) {
	s := memory.New()
	ctx := context.Background()
	_ = s.Set(ctx, "p", "k", "v1")
	_ = s.Set(ctx, "p", "k", "v2")
	v, _, _ := s.Get(ctx, "p", "k")
	if v != "v2" {
		t.Errorf("v = %q, want v2 after replace", v)
	}
}

func TestDelete_Idempotent(t *testing.T) {
	s := memory.New()
	ctx := context.Background()

	if err := s.Delete(ctx, "p", "missing"); err != nil {
		t.Errorf("Delete on missing: %v", err)
	}

	_ = s.Set(ctx, "p", "k", "v")
	if err := s.Delete(ctx, "p", "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, found, _ := s.Get(ctx, "p", "k"); found {
		t.Error("entry still present after Delete")
	}
	// Re-delete is fine.
	if err := s.Delete(ctx, "p", "k"); err != nil {
		t.Errorf("second Delete: %v", err)
	}
}

func TestList_PrefixFilter(t *testing.T) {
	s := memory.New()
	ctx := context.Background()
	for k, v := range map[string]string{
		"users.alice": "1",
		"users.bob":   "2",
		"posts.foo":   "x",
	} {
		_ = s.Set(ctx, "p", k, v)
	}

	got, err := s.List(ctx, "p", "users.")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"users.alice", "users.bob"} // sorted
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List = %v, want %v", got, want)
	}
}

func TestList_EmptyPrefixReturnsAll(t *testing.T) {
	s := memory.New()
	ctx := context.Background()
	_ = s.Set(ctx, "p", "a", "1")
	_ = s.Set(ctx, "p", "b", "2")

	got, _ := s.List(ctx, "p", "")
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("List empty-prefix = %v", got)
	}
}

func TestList_AbsentParticle(t *testing.T) {
	s := memory.New()
	got, err := s.List(context.Background(), "absent", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List = %v, want empty", got)
	}
}

func TestScopedByParticle(t *testing.T) {
	s := memory.New()
	ctx := context.Background()

	_ = s.Set(ctx, "yaml-tools", "shared", "yaml-value")
	_ = s.Set(ctx, "json-tools", "shared", "json-value")

	v1, _, _ := s.Get(ctx, "yaml-tools", "shared")
	v2, _, _ := s.Get(ctx, "json-tools", "shared")
	if v1 != "yaml-value" {
		t.Errorf("yaml-tools/shared = %q", v1)
	}
	if v2 != "json-value" {
		t.Errorf("json-tools/shared = %q", v2)
	}

	// Listing one particle's keys must not include the other's.
	keys, _ := s.List(ctx, "yaml-tools", "")
	if len(keys) != 1 || keys[0] != "shared" {
		t.Errorf("yaml-tools list = %v, want [shared]", keys)
	}
}

// -----------------------------------------------------------------------------
// Quota
// -----------------------------------------------------------------------------

func TestQuota_DefaultUnlimited(t *testing.T) {
	s := memory.New() // QuotaBytes = 0 means no quota
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		if err := s.Set(ctx, "p", "k", "v"); err != nil {
			t.Fatalf("set #%d: %v", i, err)
		}
	}
}

func TestQuota_Enforced(t *testing.T) {
	s := memory.New()
	s.QuotaBytes = 20 // small enough that two writes overflow

	ctx := context.Background()
	if err := s.Set(ctx, "p", "k1", "value-one"); err != nil { // 2 + 9 = 11 bytes
		t.Fatalf("first set: %v", err)
	}
	// Next write would push us over: 11 (existing) + 2 + 9 = 22 > 20.
	err := s.Set(ctx, "p", "k2", "value-two")
	if !errors.Is(err, kv.ErrQuotaExceeded) {
		t.Errorf("err = %v, want kv.ErrQuotaExceeded", err)
	}

	// Replacing the existing key with the same-size value is OK.
	if err := s.Set(ctx, "p", "k1", "value-mod"); err != nil {
		t.Errorf("same-size replace: %v", err)
	}
	// Replacing with a smaller value is OK and frees room.
	if err := s.Set(ctx, "p", "k1", "v"); err != nil {
		t.Errorf("smaller replace: %v", err)
	}
	// Now there's room for k2.
	if err := s.Set(ctx, "p", "k2", "v"); err != nil {
		t.Errorf("after freeing room: %v", err)
	}
}

func TestQuota_PerParticle(t *testing.T) {
	s := memory.New()
	s.QuotaBytes = 10
	ctx := context.Background()

	// Filling up yaml-tools' namespace shouldn't affect
	// json-tools — quota is per-particle.
	if err := s.Set(ctx, "yaml-tools", "k", "1234567"); err != nil { // 1 + 7 = 8 bytes
		t.Fatal(err)
	}
	if err := s.Set(ctx, "json-tools", "k", "1234567"); err != nil {
		t.Errorf("json-tools blocked by yaml-tools' quota usage: %v", err)
	}
}
