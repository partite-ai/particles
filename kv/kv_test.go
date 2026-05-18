package kv

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/partite-ai/wacogo"

	gen "github.com/partite-ai/particles/internal/host/gen/particle/host/kv"
)

// fakeStore is the in-test kv.Store. Behavior is pluggable via
// closures so each test specifies exactly what the store does.
type fakeStore struct {
	getFn    func(ctx context.Context, key string) (string, bool, error)
	setFn    func(ctx context.Context, key, value string) error
	deleteFn func(ctx context.Context, key string) error
	listFn   func(ctx context.Context, prefix string) ([]string, error)
}

func (s *fakeStore) Get(ctx context.Context, key string) (string, bool, error) {
	if s.getFn != nil {
		return s.getFn(ctx, key)
	}
	return "", false, nil
}

func (s *fakeStore) Set(ctx context.Context, key, value string) error {
	if s.setFn != nil {
		return s.setFn(ctx, key, value)
	}
	return nil
}

func (s *fakeStore) Delete(ctx context.Context, key string) error {
	if s.deleteFn != nil {
		return s.deleteFn(ctx, key)
	}
	return nil
}

func (s *fakeStore) List(ctx context.Context, prefix string) ([]string, error) {
	if s.listFn != nil {
		return s.listFn(ctx, prefix)
	}
	return nil, nil
}

// -----------------------------------------------------------------------------
// Manager lifecycle
// -----------------------------------------------------------------------------

func TestManager_NewInstance(t *testing.T) {
	ctx := context.Background()
	e := wacogo.NewEngine(ctx)
	defer e.Close(ctx)

	mgr, err := NewManager(ctx, ManagerConfig{Engine: e})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close(ctx)

	inst, err := mgr.NewInstance(ctx, &fakeStore{})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	defer inst.Close(ctx)
	if inst.Core() == nil {
		t.Error("instance core is nil")
	}
}

func TestNewManager_RejectsMissingFields(t *testing.T) {
	if _, err := NewManager(context.Background(), ManagerConfig{}); err == nil {
		t.Error("expected error when Engine is nil")
	}
}

func TestManager_RejectsNilStore(t *testing.T) {
	ctx := context.Background()
	e := wacogo.NewEngine(ctx)
	defer e.Close(ctx)

	mgr, err := NewManager(ctx, ManagerConfig{Engine: e})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(ctx)

	if _, err := mgr.NewInstance(ctx, nil); err == nil {
		t.Error("expected error for nil store")
	}
}

// -----------------------------------------------------------------------------
// Adapter behavior (driven directly to keep tests fast)
// -----------------------------------------------------------------------------

func TestAdapter_Get_Found(t *testing.T) {
	store := &fakeStore{
		getFn: func(_ context.Context, key string) (string, bool, error) {
			if key != "k1" {
				t.Errorf("unexpected key=%q", key)
			}
			return "v1", true, nil
		},
	}
	a := newAdapter(store)
	res, err := a.Get(context.Background(), "k1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	ok, isOk := res.(gen.ResultOptionStringKvErrorOk)
	if !isOk {
		t.Fatalf("got %T, want Ok", res)
	}
	if !ok.Value.IsSome || ok.Value.Value != "v1" {
		t.Errorf("got %+v, want Some(v1)", ok.Value)
	}
}

// Missing key → Ok(None), NOT an error variant. The runtime
// surfaces option<string>::none to the particle as undefined.
func TestAdapter_Get_NotFound(t *testing.T) {
	a := newAdapter(&fakeStore{})
	res, _ := a.Get(context.Background(), "missing")
	ok, isOk := res.(gen.ResultOptionStringKvErrorOk)
	if !isOk {
		t.Fatalf("got %T, want Ok", res)
	}
	if ok.Value.IsSome {
		t.Errorf("expected None, got Some(%q)", ok.Value.Value)
	}
}

func TestAdapter_Get_StorageError(t *testing.T) {
	store := &fakeStore{
		getFn: func(_ context.Context, _ string) (string, bool, error) {
			return "", false, errors.New("disk full")
		},
	}
	a := newAdapter(store)
	res, _ := a.Get(context.Background(), "k")
	errRes := res.(gen.ResultOptionStringKvErrorErr)
	se, ok := errRes.Value.(gen.KvErrorStorageError)
	if !ok {
		t.Fatalf("got %T, want StorageError", errRes.Value)
	}
	if se.Value != "disk full" {
		t.Errorf("message = %q", se.Value)
	}
}

func TestAdapter_Set_OkAndQuota(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		store := &fakeStore{
			setFn: func(_ context.Context, key, value string) error {
				if key != "k" || value != "v" {
					t.Errorf("unexpected (key=%q, value=%q)", key, value)
				}
				return nil
			},
		}
		a := newAdapter(store)
		res, _ := a.Set(context.Background(), "k", "v")
		if _, ok := res.(gen.Result_KvErrorOk); !ok {
			t.Errorf("got %T, want Ok", res)
		}
	})

	t.Run("quota", func(t *testing.T) {
		store := &fakeStore{
			setFn: func(_ context.Context, _, _ string) error { return ErrQuotaExceeded },
		}
		a := newAdapter(store)
		res, _ := a.Set(context.Background(), "k", "v")
		errRes := res.(gen.Result_KvErrorErr)
		if _, ok := errRes.Value.(gen.KvErrorQuotaExceeded); !ok {
			t.Errorf("got %T, want QuotaExceeded", errRes.Value)
		}
	})

	t.Run("wrapped quota error", func(t *testing.T) {
		// errors.Is should walk wrappers — if the host adds
		// context, the variant still maps correctly.
		store := &fakeStore{
			setFn: func(_ context.Context, _, _ string) error {
				return errors.Join(ErrQuotaExceeded, errors.New("with context"))
			},
		}
		a := newAdapter(store)
		res, _ := a.Set(context.Background(), "k", "v")
		errRes := res.(gen.Result_KvErrorErr)
		if _, ok := errRes.Value.(gen.KvErrorQuotaExceeded); !ok {
			t.Errorf("got %T, want QuotaExceeded for wrapped sentinel", errRes.Value)
		}
	})

	t.Run("storage error", func(t *testing.T) {
		store := &fakeStore{
			setFn: func(_ context.Context, _, _ string) error { return errors.New("boom") },
		}
		a := newAdapter(store)
		res, _ := a.Set(context.Background(), "k", "v")
		errRes := res.(gen.Result_KvErrorErr)
		se, ok := errRes.Value.(gen.KvErrorStorageError)
		if !ok {
			t.Fatalf("got %T, want StorageError", errRes.Value)
		}
		if se.Value != "boom" {
			t.Errorf("message = %q", se.Value)
		}
	})
}

func TestAdapter_Delete(t *testing.T) {
	store := &fakeStore{
		deleteFn: func(_ context.Context, key string) error {
			if key != "k" {
				t.Errorf("unexpected key=%q", key)
			}
			return nil
		},
	}
	a := newAdapter(store)
	res, _ := a.Delete(context.Background(), "k")
	if _, ok := res.(gen.Result_KvErrorOk); !ok {
		t.Errorf("got %T, want Ok", res)
	}
}

func TestAdapter_List_Ok(t *testing.T) {
	want := []string{"a", "b", "c"}
	store := &fakeStore{
		listFn: func(_ context.Context, prefix string) ([]string, error) {
			if prefix != "pre" {
				t.Errorf("unexpected prefix=%q", prefix)
			}
			return want, nil
		},
	}
	a := newAdapter(store)
	res, _ := a.List(context.Background(), "pre")
	ok, isOk := res.(gen.ResultListStringKvErrorOk)
	if !isOk {
		t.Fatalf("got %T, want Ok", res)
	}
	if !reflect.DeepEqual(ok.Value, want) {
		t.Errorf("List = %v, want %v", ok.Value, want)
	}
}

// nil result from the Store gets normalized to an empty slice —
// particles always see [], never the JS quirk of seeing
// `undefined` when WIT-marshaling a nil through.
func TestAdapter_List_NilNormalizedToEmpty(t *testing.T) {
	store := &fakeStore{
		listFn: func(_ context.Context, _ string) ([]string, error) { return nil, nil },
	}
	a := newAdapter(store)
	res, _ := a.List(context.Background(), "")
	ok := res.(gen.ResultListStringKvErrorOk)
	if ok.Value == nil {
		t.Error("expected non-nil empty slice")
	}
	if len(ok.Value) != 0 {
		t.Errorf("length = %d, want 0", len(ok.Value))
	}
}

func TestAdapter_List_StorageError(t *testing.T) {
	store := &fakeStore{
		listFn: func(_ context.Context, _ string) ([]string, error) { return nil, errors.New("boom") },
	}
	a := newAdapter(store)
	res, _ := a.List(context.Background(), "")
	if _, ok := res.(gen.ResultListStringKvErrorErr); !ok {
		t.Errorf("got %T, want Err", res)
	}
}

// Each adapter is bound to its own Store; reads on one adapter
// see only that Store's contents.
func TestAdapter_ScopedByStore(t *testing.T) {
	storeA := &fakeStore{
		getFn: func(_ context.Context, _ string) (string, bool, error) { return "from-a", true, nil },
	}
	storeB := &fakeStore{}

	a := newAdapter(storeA)
	b := newAdapter(storeB)

	resA, _ := a.Get(context.Background(), "k")
	okA := resA.(gen.ResultOptionStringKvErrorOk)
	if !okA.Value.IsSome || okA.Value.Value != "from-a" {
		t.Errorf("a got %+v, want Some(from-a)", okA.Value)
	}

	resB, _ := b.Get(context.Background(), "k")
	okB := resB.(gen.ResultOptionStringKvErrorOk)
	if okB.Value.IsSome {
		t.Errorf("b got %+v, want None", okB.Value)
	}
}
