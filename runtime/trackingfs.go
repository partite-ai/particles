package runtime

import (
	"io"
	"io/fs"
	"sync"

	"github.com/partite-ai/wacogo/wasi/filesystem/preopens"
)

// trackingFS wraps any fs.FS and records every file it hands out — from
// Open and from OpenAt on its directories — so they can all be closed
// at once in Close. wacogo's component teardown does NOT drop
// outstanding wasi resource handles (ComponentInstance.Close only
// checks for and reports them), so a guest that exits without closing
// its open files would otherwise leak whatever those fs.File handles
// hold (host fds, etc.) until the process exits. The runtime wraps every
// mount FS in one of these and closes it from [Particle.Close], which
// runs on normal return, a trapped tool call, and SIGINT/SIGTERM.
//
// It forwards the full set of optional capability interfaces the
// wasi:filesystem host probes for, so the wrapped FS's behavior is
// preserved. It is always applied INNERMOST — beneath any read-only or
// quota wrapper — so a wrapped file advertising write capability is
// never observed directly for a read-only mount (the outer read-only
// wrapper hides it).
type trackingFS struct {
	fsys fs.FS
	mu   sync.Mutex
	open map[io.Closer]struct{}
}

func newTrackingFS(fsys fs.FS) *trackingFS {
	return &trackingFS{fsys: fsys, open: make(map[io.Closer]struct{})}
}

// Close closes every file handed out by this FS that's still open.
// Handles the guest closed normally have already deregistered; a double
// Close on a file is harmless.
func (t *trackingFS) Close() error {
	t.mu.Lock()
	live := make([]io.Closer, 0, len(t.open))
	for c := range t.open {
		live = append(live, c)
	}
	t.open = make(map[io.Closer]struct{})
	t.mu.Unlock()
	for _, c := range live {
		_ = c.Close()
	}
	return nil
}

func (t *trackingFS) track(c io.Closer)  { t.mu.Lock(); t.open[c] = struct{}{}; t.mu.Unlock() }
func (t *trackingFS) forget(c io.Closer) { t.mu.Lock(); delete(t.open, c); t.mu.Unlock() }

func (t *trackingFS) Open(name string) (fs.File, error) {
	f, err := t.fsys.Open(name)
	if err != nil {
		return nil, err
	}
	return t.wrap(f), nil
}

// wrap returns a tracked view of f — a trackedDir for directories (so
// files opened beneath it are tracked too) or a trackedFile otherwise.
// Each deregisters itself on Close.
func (t *trackingFS) wrap(f fs.File) fs.File {
	if info, err := f.Stat(); err == nil && info.IsDir() {
		d := &trackedDir{owner: t, inner: f}
		t.track(d)
		return d
	}
	tf := &trackedFile{owner: t, inner: f}
	t.track(tf)
	return tf
}

// trackedFile adds nothing but Close-time deregistration. Every other
// capability (Write, WriteAt, Seek, ReaderAt, Truncater, Syncer, …) is
// reached by wacogo's as[T] helper through Unwrap, so the wrapped
// file's behavior is preserved without restating each interface.
type trackedFile struct {
	owner *trackingFS
	inner fs.File
}

func (f *trackedFile) Stat() (fs.FileInfo, error) { return f.inner.Stat() }
func (f *trackedFile) Read(p []byte) (int, error) { return f.inner.Read(p) }
func (f *trackedFile) Unwrap() fs.File            { return f.inner }

func (f *trackedFile) Close() error {
	f.owner.forget(f)
	return f.inner.Close()
}

// trackedDir likewise only overrides Close and OpenAt — OpenAt MUST be
// overridden (not delegated through Unwrap) so files opened beneath
// this directory are wrapped and tracked too. Stat-at, mkdir, unlink,
// readlink, chtimes, symlink, and read-dir reach the inner via Unwrap.
type trackedDir struct {
	owner *trackingFS
	inner fs.File
}

func (d *trackedDir) Stat() (fs.FileInfo, error) { return d.inner.Stat() }
func (d *trackedDir) Read(p []byte) (int, error) { return d.inner.Read(p) }
func (d *trackedDir) Unwrap() fs.File            { return d.inner }

func (d *trackedDir) Close() error {
	d.owner.forget(d)
	return d.inner.Close()
}

func (d *trackedDir) OpenAt(name string, flag int, perm fs.FileMode) (fs.File, error) {
	oa, ok := d.inner.(preopens.OpenAter)
	if !ok {
		return nil, fs.ErrInvalid
	}
	child, err := oa.OpenAt(name, flag, perm)
	if err != nil {
		return nil, err
	}
	return d.owner.wrap(child), nil
}

var (
	_ io.Closer                   = (*trackingFS)(nil)
	_ fs.FS                       = (*trackingFS)(nil)
	_ fs.File                     = (*trackedFile)(nil)
	_ fs.File                     = (*trackedDir)(nil)
	_ preopens.OpenAter           = (*trackedDir)(nil)
	_ preopens.Unwrapper[fs.File] = (*trackedFile)(nil)
	_ preopens.Unwrapper[fs.File] = (*trackedDir)(nil)
)
