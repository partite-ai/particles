// Package quotafs wraps a writable [io/fs.FS] with a hard byte cap on
// total regular-file content. It's the enforcement layer behind a
// particle's temp mount (capabilities.filesystem.temp.maxSize), but it
// decorates any writable FS — in particular one whose directory files
// implement the wasi:filesystem preopen capability interfaces
// (OpenAter, MkdirAter, UnlinkAter, …) and whose regular files
// implement the write interfaces (io.Writer/io.WriterAt/Truncater).
//
// # What is counted
//
// A single counter tracks the sum of the logical size (EOF offset) of
// every regular file reachable through the FS. Directories, symlinks,
// and hardlink overhead count as zero — the cap is purely file-content
// bytes, matching the "desired size" intent of a temp mount.
//
// # How it stays accurate
//
// Every size-changing op follows reserve-worst-case → do → reconcile:
// it computes an upper-bound delta, atomically checks it against the
// cap and reserves it BEFORE touching the backing FS (so an over-cap
// write is rejected with nothing written), performs the op, then
// refunds the difference between the reservation and the bytes actually
// moved. Shrinking ops (truncate-down, unlink, O_TRUNC over an existing
// file) credit their freed bytes back. One mutex-guarded counter makes
// reserve+commit atomic across concurrent handles.
//
// A rejected write returns [ErrQuotaExceeded]. Note the wasi:filesystem
// host currently maps an unrecognized error to a generic I/O error
// rather than a "no space"/"quota" code; see the package README / the
// runtime's mount wiring for the follow-up that surfaces a precise
// errno to the guest.
package quotafs

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"sync"

	"github.com/partite-ai/wacogo/wasi/filesystem/preopens"
)

// ErrQuotaExceeded is returned by a write/grow/create op that would
// push total usage past the cap. The backing FS is left untouched.
var ErrQuotaExceeded = errors.New("quotafs: quota exceeded")

// FS is a byte-capped view over an inner writable fs.FS. Construct it
// with [New]; pass it anywhere an fs.FS is accepted (e.g. a
// preopens.PreopenEntry.FS). Reads pass straight through; size-growing
// operations are metered and may fail with [ErrQuotaExceeded].
type FS struct {
	inner fs.FS
	c     *counter
}

// New returns an FS that caps total regular-file bytes under inner at
// max. It walks inner once to seed the current usage, so wrapping a
// non-empty directory starts from its real size (a freshly-created
// temp dir seeds at zero).
func New(inner fs.FS, max int64) (*FS, error) {
	used, err := walkSize(inner)
	if err != nil {
		return nil, err
	}
	return &FS{inner: inner, c: &counter{used: used, max: max}}, nil
}

// Used reports the currently-accounted byte total. Max reports the cap.
func (f *FS) Used() int64 { return f.c.load() }
func (f *FS) Max() int64  { return f.c.max }

// Open implements fs.FS, wrapping the returned file so its writes are
// metered (regular files) or its path-relative ops are metered
// (directories).
func (f *FS) Open(name string) (fs.File, error) {
	inner, err := f.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return wrap(inner, f.c), nil
}

// wrap returns a metered view of inner: a quotaDir for directories
// (so OpenAt/UnlinkAt/… stay accounted) or a quotaFile for everything
// else (so Write/WriteAt/Truncate stay accounted).
func wrap(inner fs.File, c *counter) fs.File {
	if info, err := inner.Stat(); err == nil && info.IsDir() {
		return &quotaDir{inner: inner, c: c}
	}
	return &quotaFile{inner: inner, c: c}
}

// counter is the shared usage tally. tryReserve admits a positive
// delta only if it fits under max; release returns bytes to the pool.
type counter struct {
	mu   sync.Mutex
	used int64
	max  int64
}

func (c *counter) load() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

func (c *counter) tryReserve(n int64) bool {
	if n <= 0 {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.used+n > c.max {
		return false
	}
	c.used += n
	return true
}

func (c *counter) release(n int64) {
	if n <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.used -= n
	if c.used < 0 {
		c.used = 0
	}
}

// growth is the non-negative number of bytes by which writing up to
// newEnd extends a file currently sized old.
func growth(old, newEnd int64) int64 {
	if newEnd > old {
		return newEnd - old
	}
	return 0
}

func walkSize(fsys fs.FS) (int64, error) {
	var total int64
	err := fs.WalkDir(fsys, ".", func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// -----------------------------------------------------------------------------
// quotaFile — metered regular file
// -----------------------------------------------------------------------------

type quotaFile struct {
	inner fs.File
	c     *counter
}

func (f *quotaFile) Stat() (fs.FileInfo, error) { return f.inner.Stat() }
func (f *quotaFile) Read(p []byte) (int, error) { return f.inner.Read(p) }
func (f *quotaFile) Close() error               { return f.inner.Close() }

// Unwrap exposes the inner file so wacogo's as[T] resolves the
// capabilities this wrapper does NOT meter (ReaderAt, Seeker, Syncer)
// against it directly; the metered ops (Write, WriteAt, Truncate) are
// implemented here and so take precedence over the unwrap.
func (f *quotaFile) Unwrap() fs.File { return f.inner }

// curSize reads the file's current logical size; 0 on stat error.
func (f *quotaFile) curSize() int64 {
	info, err := f.inner.Stat()
	if err != nil {
		return 0
	}
	return info.Size()
}

func (f *quotaFile) WriteAt(p []byte, off int64) (int, error) {
	wa, ok := f.inner.(io.WriterAt)
	if !ok {
		return 0, fs.ErrInvalid
	}
	old := f.curSize()
	predicted := growth(old, off+int64(len(p)))
	if !f.c.tryReserve(predicted) {
		return 0, ErrQuotaExceeded
	}
	n, err := wa.WriteAt(p, off)
	actual := growth(old, off+int64(n))
	f.c.release(predicted - actual)
	return n, err
}

func (f *quotaFile) Write(p []byte) (int, error) {
	w, ok := f.inner.(io.Writer)
	if !ok {
		return 0, fs.ErrInvalid
	}
	old := f.curSize()
	pos := old
	if s, ok := f.inner.(io.Seeker); ok {
		if cur, err := s.Seek(0, io.SeekCurrent); err == nil {
			pos = cur
		}
	}
	predicted := growth(old, pos+int64(len(p)))
	if !f.c.tryReserve(predicted) {
		return 0, ErrQuotaExceeded
	}
	n, err := w.Write(p)
	actual := growth(old, pos+int64(n))
	f.c.release(predicted - actual)
	return n, err
}

func (f *quotaFile) Truncate(size int64) error {
	t, ok := f.inner.(preopens.Truncater)
	if !ok {
		return fs.ErrInvalid
	}
	old := f.curSize()
	if size > old {
		if !f.c.tryReserve(size - old) {
			return ErrQuotaExceeded
		}
		if err := t.Truncate(size); err != nil {
			f.c.release(size - old)
			return err
		}
		return nil
	}
	if err := t.Truncate(size); err != nil {
		return err
	}
	f.c.release(old - size)
	return nil
}

// -----------------------------------------------------------------------------
// quotaDir — metered directory
// -----------------------------------------------------------------------------

type quotaDir struct {
	inner fs.File
	c     *counter
}

func (d *quotaDir) Stat() (fs.FileInfo, error) { return d.inner.Stat() }
func (d *quotaDir) Read(p []byte) (int, error) { return d.inner.Read(p) }
func (d *quotaDir) Close() error               { return d.inner.Close() }

// Unwrap exposes the inner directory so the un-metered ops (ReadDir,
// StatAt, MkdirAt, RmdirAt, ReadlinkAt, ChtimesAt, SymlinkAt — none of
// which change file-content byte totals) resolve against it. OpenAt and
// UnlinkAt are metered here and so take precedence over the unwrap.
func (d *quotaDir) Unwrap() fs.File { return d.inner }

func (d *quotaDir) OpenAt(name string, flag int, perm fs.FileMode) (fs.File, error) {
	oa, ok := d.inner.(preopens.OpenAter)
	if !ok {
		return nil, fs.ErrInvalid
	}
	// O_TRUNC over an existing regular file frees its old bytes; note
	// the amount to credit back after a successful open.
	var truncFreed int64
	if flag&os.O_TRUNC != 0 {
		if sa, ok := d.inner.(preopens.StatAter); ok {
			if info, err := sa.StatAt(name); err == nil && info.Mode().IsRegular() {
				truncFreed = info.Size()
			}
		}
	}
	child, err := oa.OpenAt(name, flag, perm)
	if err != nil {
		return nil, err
	}
	d.c.release(truncFreed)
	return wrap(child, d.c), nil
}

func (d *quotaDir) UnlinkAt(name string) error {
	u, ok := d.inner.(preopens.UnlinkAter)
	if !ok {
		return fs.ErrInvalid
	}
	var freed int64
	if sa, ok := d.inner.(preopens.StatAter); ok {
		if info, err := sa.StatAt(name); err == nil && info.Mode().IsRegular() {
			freed = info.Size()
		}
	}
	if err := u.UnlinkAt(name); err != nil {
		return err
	}
	d.c.release(freed)
	return nil
}

// Compile-time guarantees that the metered wrappers satisfy fs.File,
// expose the inner via Unwrap, and directly implement the metered
// capabilities (so as[T] finds them before unwrapping).
var (
	_ fs.FS                       = (*FS)(nil)
	_ fs.File                     = (*quotaDir)(nil)
	_ fs.File                     = (*quotaFile)(nil)
	_ preopens.Unwrapper[fs.File] = (*quotaDir)(nil)
	_ preopens.Unwrapper[fs.File] = (*quotaFile)(nil)
	_ preopens.OpenAter           = (*quotaDir)(nil)
	_ preopens.UnlinkAter         = (*quotaDir)(nil)
	_ io.WriterAt                 = (*quotaFile)(nil)
	_ io.Writer                   = (*quotaFile)(nil)
	_ preopens.Truncater          = (*quotaFile)(nil)
)
