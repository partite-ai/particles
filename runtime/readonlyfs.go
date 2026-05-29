package runtime

import (
	"io"
	"io/fs"
	"os"

	"github.com/partite-ai/wacogo/wasi/filesystem/preopens"
)

// readOnlyFS wraps an fs.FS and presents it to wasi:filesystem as a
// strictly read-only preopen. It is the enforcement point behind a
// mount declared `access: readonly`: the host may hand the runtime a
// fully writable fs.FS (e.g. an os.Root over a real directory) and this
// wrapper still guarantees no write reaches it.
//
// The guarantee is structural. The wrappers do NOT implement
// preopens.Unwrapper — so wacogo's as[T] can't reach the writable inner
// through them — and expose read capabilities selectively via As:
//   - roFile answers only io.ReaderAt / io.Seeker (plus Read via
//     fs.File); it refuses io.Writer / io.WriterAt / Truncater, so the
//     descriptor reports read-only and writes map to Unsupported.
//   - roDir answers only the read directory ops (ReadDirFile, StatAter,
//     ReadlinkAter) and overrides OpenAt to reject write flags and
//     return read-only children; it never answers the mutating *Aters
//     (mkdir / unlink / rmdir / symlink / set-times), so those are
//     Unsupported.
//
// Escape-safety comes from the wrapped FS (an os.Root-backed mount
// confines every lookup to its root); read opens are forced O_RDONLY
// through the inner directory's own OpenAter.
type readOnlyFS struct{ fsys fs.FS }

func (r readOnlyFS) Open(name string) (fs.File, error) {
	f, err := r.fsys.Open(name)
	if err != nil {
		return nil, err
	}
	return roWrap(f)
}

func roWrap(f fs.File) (fs.File, error) {
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if info.IsDir() {
		return &roDir{inner: f}, nil
	}
	return roFile{inner: f}, nil
}

// roFile exposes reads only. It implements fs.File directly and answers
// io.ReaderAt / io.Seeker through As by delegating to the inner file;
// it deliberately answers nothing else, so no write interface is ever
// discoverable.
type roFile struct{ inner fs.File }

func (f roFile) Stat() (fs.FileInfo, error) { return f.inner.Stat() }
func (f roFile) Read(p []byte) (int, error) { return f.inner.Read(p) }
func (f roFile) Close() error               { return f.inner.Close() }

func (f roFile) As(target any) bool {
	switch p := target.(type) {
	case *io.ReaderAt:
		if v, ok := f.inner.(io.ReaderAt); ok {
			*p = v
			return true
		}
	case *io.Seeker:
		if v, ok := f.inner.(io.Seeker); ok {
			*p = v
			return true
		}
	}
	return false
}

// roDir exposes read directory ops only. OpenAt is overridden (it must
// reject write flags and wrap children read-only — it can't be answered
// through As, which would hand back the writable inner's OpenAter); the
// read-side *Aters are delegated to the inner via As.
type roDir struct{ inner fs.File }

func (d *roDir) Stat() (fs.FileInfo, error) { return d.inner.Stat() }
func (d *roDir) Read(p []byte) (int, error) { return d.inner.Read(p) }
func (d *roDir) Close() error               { return d.inner.Close() }

func (d *roDir) OpenAt(name string, flag int, _ fs.FileMode) (fs.File, error) {
	if flag != os.O_RDONLY {
		return nil, fs.ErrPermission
	}
	oa, ok := d.inner.(preopens.OpenAter)
	if !ok {
		return nil, fs.ErrInvalid
	}
	child, err := oa.OpenAt(name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	return roWrap(child)
}

func (d *roDir) As(target any) bool {
	switch p := target.(type) {
	case *fs.ReadDirFile:
		if v, ok := d.inner.(fs.ReadDirFile); ok {
			*p = v
			return true
		}
	case *preopens.StatAter:
		if v, ok := d.inner.(preopens.StatAter); ok {
			*p = v
			return true
		}
	case *preopens.ReadlinkAter:
		if v, ok := d.inner.(preopens.ReadlinkAter); ok {
			*p = v
			return true
		}
	}
	return false
}

var (
	_ fs.FS             = readOnlyFS{}
	_ fs.File           = roFile{}
	_ fs.File           = (*roDir)(nil)
	_ preopens.Aser     = roFile{}
	_ preopens.Aser     = (*roDir)(nil)
	_ preopens.OpenAter = (*roDir)(nil)
)
