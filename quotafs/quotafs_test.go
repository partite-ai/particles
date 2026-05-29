package quotafs_test

import (
	"errors"
	"io"
	"os"
	"testing"

	"github.com/partite-ai/wacogo/wasi/filesystem/preopens"

	"github.com/partite-ai/particles/internal/osmount"
	"github.com/partite-ai/particles/quotafs"
)

func newQuota(t *testing.T, max int64) *quotafs.FS {
	t.Helper()
	inner, err := osmount.New(t.TempDir())
	if err != nil {
		t.Fatalf("osmount.New: %v", err)
	}
	t.Cleanup(func() { _ = inner.Close() })
	q, err := quotafs.New(inner, max)
	if err != nil {
		t.Fatalf("quotafs.New: %v", err)
	}
	return q
}

func openRoot(t *testing.T, q *quotafs.FS) preopens.OpenAter {
	t.Helper()
	root, err := q.Open(".")
	if err != nil {
		t.Fatalf("Open .: %v", err)
	}
	oa, ok := root.(preopens.OpenAter)
	if !ok {
		t.Fatal("root is not OpenAter")
	}
	return oa
}

func TestQuotaReserveAndReject(t *testing.T) {
	q := newQuota(t, 100)
	oa := openRoot(t, q)
	f, err := oa.OpenAt("a.txt", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	wa := f.(io.WriterAt)

	if _, err := wa.WriteAt(make([]byte, 50), 0); err != nil {
		t.Fatalf("write 50: %v", err)
	}
	if q.Used() != 50 {
		t.Fatalf("used = %d, want 50", q.Used())
	}

	// Over-cap write is rejected and commits nothing.
	if _, err := wa.WriteAt(make([]byte, 60), 50); !errors.Is(err, quotafs.ErrQuotaExceeded) {
		t.Fatalf("over-cap write err = %v, want ErrQuotaExceeded", err)
	}
	if q.Used() != 50 {
		t.Fatalf("used after reject = %d, want 50", q.Used())
	}

	// Writing exactly to the cap succeeds.
	if _, err := wa.WriteAt(make([]byte, 50), 50); err != nil {
		t.Fatalf("write to cap: %v", err)
	}
	if q.Used() != 100 {
		t.Fatalf("used = %d, want 100", q.Used())
	}
}

func TestQuotaCreditBack(t *testing.T) {
	q := newQuota(t, 100)
	oa := openRoot(t, q)
	f, err := oa.OpenAt("a.txt", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.(io.WriterAt).WriteAt(make([]byte, 80), 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	if q.Used() != 80 {
		t.Fatalf("used = %d, want 80", q.Used())
	}

	if err := f.(preopens.Truncater).Truncate(30); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if q.Used() != 30 {
		t.Fatalf("used after truncate-down = %d, want 30", q.Used())
	}
	f.Close()

	if err := oa.(preopens.UnlinkAter).UnlinkAt("a.txt"); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if q.Used() != 0 {
		t.Fatalf("used after unlink = %d, want 0", q.Used())
	}
}

func TestQuotaOTruncCredits(t *testing.T) {
	q := newQuota(t, 100)
	oa := openRoot(t, q)
	f, err := oa.OpenAt("a.txt", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.(io.WriterAt).WriteAt(make([]byte, 40), 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()
	if q.Used() != 40 {
		t.Fatalf("used = %d, want 40", q.Used())
	}

	f2, err := oa.OpenAt("a.txt", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("reopen O_TRUNC: %v", err)
	}
	defer f2.Close()
	if q.Used() != 0 {
		t.Fatalf("used after O_TRUNC = %d, want 0", q.Used())
	}
}

func TestQuotaSeed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/seed.bin", make([]byte, 30), 0o644); err != nil {
		t.Fatal(err)
	}
	inner, err := osmount.New(dir)
	if err != nil {
		t.Fatalf("osmount.New: %v", err)
	}
	defer inner.Close()
	q, err := quotafs.New(inner, 100)
	if err != nil {
		t.Fatalf("quotafs.New: %v", err)
	}
	if q.Used() != 30 {
		t.Fatalf("seeded used = %d, want 30", q.Used())
	}
}
