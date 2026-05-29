package runtime

import (
	"io"
	"io/fs"
	"os"
	"testing"

	"github.com/partite-ai/wacogo/wasi/filesystem/preopens"

	"github.com/partite-ai/particles/internal/osmount"
)

// TestTrackingFS_ReclaimsLeakedHandle verifies that Close reclaims a
// file the guest opened but never closed — the leak wacogo's teardown
// doesn't cover — while still forwarding the file's write capability.
func TestTrackingFS_ReclaimsLeakedHandle(t *testing.T) {
	dir := t.TempDir()
	backing, err := osmount.New(dir)
	if err != nil {
		t.Fatalf("osmount.New: %v", err)
	}
	defer backing.Close()

	tfs := newTrackingFS(backing)
	root, err := tfs.Open(".")
	if err != nil {
		t.Fatalf("Open .: %v", err)
	}
	oa := root.(preopens.OpenAter)

	f, err := oa.OpenAt("out.txt", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	// trackedFile doesn't restate write methods; the write capability is
	// reached through Unwrap (as wacogo's as[T] does). Resolve it that way.
	u, ok := f.(preopens.Unwrapper[fs.File])
	if !ok {
		t.Fatal("trackedFile should be an Unwrapper[fs.File]")
	}
	w, ok := u.Unwrap().(io.WriterAt)
	if !ok {
		t.Fatal("unwrapped file should be an io.WriterAt")
	}
	if _, err := w.WriteAt([]byte("x"), 0); err != nil {
		t.Fatalf("write through unwrapped handle: %v", err)
	}
	// Deliberately leave f (and root) open.

	if err := tfs.Close(); err != nil {
		t.Fatalf("trackingFS.Close: %v", err)
	}
	if _, err := w.WriteAt([]byte("y"), 0); err == nil {
		t.Error("expected write to fail after trackingFS.Close reclaimed the leaked handle")
	}
}

// TestTrackingFS_DeregistersOnClose verifies handles closed normally
// don't accumulate — important for a long-lived serve-mcp session.
func TestTrackingFS_DeregistersOnClose(t *testing.T) {
	dir := t.TempDir()
	backing, err := osmount.New(dir)
	if err != nil {
		t.Fatalf("osmount.New: %v", err)
	}
	defer backing.Close()

	tfs := newTrackingFS(backing)
	root, err := tfs.Open(".")
	if err != nil {
		t.Fatalf("Open .: %v", err)
	}
	f, err := root.(preopens.OpenAter).OpenAt("a.txt", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	if got := trackingLen(tfs); got != 2 {
		t.Fatalf("tracked handles = %d, want 2 (root + file)", got)
	}
	_ = f.Close()
	_ = root.Close()
	if got := trackingLen(tfs); got != 0 {
		t.Errorf("tracked handles after close = %d, want 0", got)
	}
}

func trackingLen(t *trackingFS) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.open)
}
