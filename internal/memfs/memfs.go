// Package memfs is a small in-memory fs.FS used to assemble files
// for code paths that consume the io/fs interface — wasi preopens,
// archive packers, the build pipeline's staging tree. Modeled after
// testing/fstest.MapFS (same shape, same semantics) but living
// outside the testing/ package so production code doesn't import
// from testing/.
//
// Use it like a regular map: add leaf-file entries, hand the FS to
// any fs.FS consumer. Intermediate directories are synthesized on
// demand from path prefixes — callers don't have to add explicit
// dir entries. The FS-as-map model means edits race with concurrent
// reads, so finish populating before handing it off.
//
// Adapted from Go's testing/fstest.MapFS (Copyright 2020 The Go
// Authors, BSD-style license). Symlink handling stripped — we have
// no callers that need it. If a use case shows up, the upstream
// resolveSymlinks / ReadLink / Lstat methods drop in cleanly.
package memfs

import (
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"
)

// FS is an in-memory fs.FS. Populate as a map then hand to any
// io/fs consumer.
type FS map[string]*File

// File is one entry in an FS — content + metadata.
type File struct {
	Data    []byte
	Mode    fs.FileMode
	ModTime time.Time
	Sys     any
}

var (
	_ fs.FS         = FS(nil)
	_ fs.StatFS     = FS(nil)
	_ fs.ReadFileFS = FS(nil)
	_ fs.ReadDirFS  = FS(nil)
	_ fs.GlobFS     = FS(nil)
	_ fs.SubFS      = FS(nil)
)

// Open implements fs.FS.
func (fsys FS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}

	file := fsys[name]
	if file != nil && file.Mode&fs.ModeDir == 0 {
		return &openFile{name, fileInfo{path.Base(name), file}, 0}, nil
	}

	// Directory — possibly synthesized. file is nil here if the
	// caller didn't add an explicit dir entry; we still serve it
	// as long as some leaf file lives under this prefix.
	var list []fileInfo
	need := make(map[string]bool)
	if name == "." {
		for fname, f := range fsys {
			i := strings.Index(fname, "/")
			if i < 0 {
				if fname != "." {
					list = append(list, fileInfo{fname, f})
				}
			} else {
				need[fname[:i]] = true
			}
		}
	} else {
		prefix := name + "/"
		for fname, f := range fsys {
			if strings.HasPrefix(fname, prefix) {
				felem := fname[len(prefix):]
				i := strings.Index(felem, "/")
				if i < 0 {
					list = append(list, fileInfo{felem, f})
				} else {
					need[fname[len(prefix):len(prefix)+i]] = true
				}
			}
		}
		// Neither named nor implied — treat as missing.
		if file == nil && list == nil && len(need) == 0 {
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
		}
	}
	for _, fi := range list {
		delete(need, fi.name)
	}
	for n := range need {
		list = append(list, fileInfo{n, &File{Mode: fs.ModeDir | 0o555}})
	}
	slices.SortFunc(list, func(a, b fileInfo) int {
		return strings.Compare(a.name, b.name)
	})

	if file == nil {
		file = &File{Mode: fs.ModeDir | 0o555}
	}
	var elem string
	if name == "." {
		elem = "."
	} else {
		elem = name[strings.LastIndex(name, "/")+1:]
	}
	return &openDir{name, fileInfo{elem, file}, list, 0}, nil
}

// fsOnly hides the auxiliary fs.FS methods on FS so the helpers
// below (ReadFile / Stat / ReadDir / Glob) can delegate to package
// `fs` without recursing into FS's own implementations.
type fsOnly struct{ fs.FS }

func (fsys FS) ReadFile(name string) ([]byte, error) {
	return fs.ReadFile(fsOnly{fsys}, name)
}

func (fsys FS) Stat(name string) (fs.FileInfo, error) {
	return fs.Stat(fsOnly{fsys}, name)
}

func (fsys FS) ReadDir(name string) ([]fs.DirEntry, error) {
	return fs.ReadDir(fsOnly{fsys}, name)
}

func (fsys FS) Glob(pattern string) ([]string, error) {
	return fs.Glob(fsOnly{fsys}, pattern)
}

// noSub hides FS's Sub method while delegating fs.FS, so fs.Sub
// can wrap us in its own SubFS without recursive shenanigans.
type noSub struct{ FS }

func (noSub) Sub() {} // intentionally wrong signature

func (fsys FS) Sub(dir string) (fs.FS, error) {
	return fs.Sub(noSub{fsys}, dir)
}

// fileInfo implements fs.FileInfo + fs.DirEntry for one FS entry.
type fileInfo struct {
	name string
	f    *File
}

func (i *fileInfo) Name() string               { return path.Base(i.name) }
func (i *fileInfo) Size() int64                { return int64(len(i.f.Data)) }
func (i *fileInfo) Mode() fs.FileMode          { return i.f.Mode }
func (i *fileInfo) Type() fs.FileMode          { return i.f.Mode.Type() }
func (i *fileInfo) ModTime() time.Time         { return i.f.ModTime }
func (i *fileInfo) IsDir() bool                { return i.f.Mode&fs.ModeDir != 0 }
func (i *fileInfo) Sys() any                   { return i.f.Sys }
func (i *fileInfo) Info() (fs.FileInfo, error) { return i, nil }

func (i *fileInfo) String() string { return fs.FormatFileInfo(i) }

// openFile is a regular (non-directory) fs.File open for reading.
type openFile struct {
	path string
	fileInfo
	offset int64
}

func (f *openFile) Stat() (fs.FileInfo, error) { return &f.fileInfo, nil }
func (f *openFile) Close() error               { return nil }

func (f *openFile) Read(b []byte) (int, error) {
	if f.offset >= int64(len(f.f.Data)) {
		return 0, io.EOF
	}
	if f.offset < 0 {
		return 0, &fs.PathError{Op: "read", Path: f.path, Err: fs.ErrInvalid}
	}
	n := copy(b, f.f.Data[f.offset:])
	f.offset += int64(n)
	return n, nil
}

func (f *openFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case 0:
		// offset += 0
	case 1:
		offset += f.offset
	case 2:
		offset += int64(len(f.f.Data))
	}
	if offset < 0 || offset > int64(len(f.f.Data)) {
		return 0, &fs.PathError{Op: "seek", Path: f.path, Err: fs.ErrInvalid}
	}
	f.offset = offset
	return offset, nil
}

func (f *openFile) ReadAt(b []byte, offset int64) (int, error) {
	if offset < 0 || offset > int64(len(f.f.Data)) {
		return 0, &fs.PathError{Op: "read", Path: f.path, Err: fs.ErrInvalid}
	}
	n := copy(b, f.f.Data[offset:])
	if n < len(b) {
		return n, io.EOF
	}
	return n, nil
}

// openDir is a directory fs.File (also fs.ReadDirFile).
type openDir struct {
	path string
	fileInfo
	entry  []fileInfo
	offset int
}

func (d *openDir) Stat() (fs.FileInfo, error) { return &d.fileInfo, nil }
func (d *openDir) Close() error               { return nil }
func (d *openDir) Read(b []byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.path, Err: fs.ErrInvalid}
}

func (d *openDir) ReadDir(count int) ([]fs.DirEntry, error) {
	n := len(d.entry) - d.offset
	if n == 0 && count > 0 {
		return nil, io.EOF
	}
	if count > 0 && n > count {
		n = count
	}
	list := make([]fs.DirEntry, n)
	for i := range list {
		list[i] = &d.entry[d.offset+i]
	}
	d.offset += n
	return list, nil
}
