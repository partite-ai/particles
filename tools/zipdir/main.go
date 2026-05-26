// zipdir: walk a directory and write a zip containing it.
//
// Used to produce runtime/embed/python3.14-stdlib.zip from the
// CPython 3.14 install tree. The zip is later go:embed'd into the
// Go runtime and mounted as a wasi preopen so the Python runtime
// component sees the stdlib at instantiate-time without needing a
// host filesystem dependency.
//
// Usage:
//
//	zipdir <src-dir> <dst-zip> [-exclude PATTERN]...
//
// Patterns are matched against the path-relative-to-src using
// filepath.Match per path segment ("**" is NOT supported — match
// each segment individually). To exclude `test/`, use `-exclude
// test/*` (matches first-segment `test`). Trailing `/*` is implied
// for directory matches.
package main

import (
	"archive/zip"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type excludeList []string

func (e *excludeList) String() string     { return strings.Join(*e, ",") }
func (e *excludeList) Set(v string) error { *e = append(*e, v); return nil }

func main() {
	var excludes excludeList
	flag.Var(&excludes, "exclude", "glob pattern matched against path segments; segment-prefix matches exclude the subtree")
	flag.Parse()
	if flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: zipdir [-exclude GLOB]... <src-dir> <dst-zip>")
		os.Exit(2)
	}
	src, dst := flag.Arg(0), flag.Arg(1)
	if err := run(src, dst, excludes); err != nil {
		fmt.Fprintln(os.Stderr, "zipdir:", err)
		os.Exit(1)
	}
}

func run(src, dst string, excludes []string) error {
	src = filepath.Clean(src)
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	// Use the default deflate compression — best size/speed balance
	// for Python source. Random access is per-file so compression
	// level doesn't hurt module-load performance.

	files, dirs := 0, 0
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			return nil
		}
		if matchesExclude(rel, excludes) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// archive/zip uses forward slashes regardless of platform.
		zipPath := filepath.ToSlash(rel)
		if d.IsDir() {
			dirs++
			_, err := zw.Create(zipPath + "/")
			return err
		}
		// Symlinks: dereference (matches how Python's importlib reads
		// the file — it stat's, follows, opens). The cpython install
		// uses symlinks for a few config files.
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			info, err = os.Stat(p)
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil // skip dir symlinks; the walk visits the underlying dir directly
			}
		}
		fh, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		fh.Name = zipPath
		fh.Method = zip.Deflate
		w, err := zw.CreateHeader(fh)
		if err != nil {
			return err
		}
		r, err := os.Open(p)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(w, r)
		closeErr := r.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		files++
		return nil
	})
	if err != nil {
		_ = zw.Close()
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	st, _ := out.Stat()
	fmt.Printf("zipdir: %d files, %d dirs, %s -> %s (%d bytes)\n", files, dirs, src, dst, st.Size())
	return nil
}

// matchesExclude checks whether any path segment of rel matches any
// glob in patterns. Segments are checked left-to-right; a match at
// any prefix excludes the subtree.
func matchesExclude(rel string, patterns []string) bool {
	if len(patterns) == 0 {
		return false
	}
	segs := strings.Split(filepath.ToSlash(rel), "/")
	for _, pat := range patterns {
		// Trim a trailing /* — directory excludes like "test/*" should
		// match the directory itself too, so test/ stops the walk.
		dirPat := strings.TrimSuffix(pat, "/*")
		for _, seg := range segs {
			ok, err := filepath.Match(dirPat, seg)
			if err == nil && ok {
				return true
			}
		}
		// Also try the literal pattern against full rel for catch-all
		// excludes (e.g., "*.pyc").
		ok, err := filepath.Match(pat, rel)
		if err == nil && ok {
			return true
		}
	}
	return false
}
