// Package embedzstd reads zstd-compressed wasm artifacts baked into the
// binary via go:embed. Single decompression call lives here so the
// `runtime` and `internal/build/wacogo` packages don't each need to
// know the artifact format — they just hand over the embed.FS plus
// the path of a `.wasm.zst` and get the raw wasm bytes back.
//
// Pair this with tools/embedcompress, which writes the .wasm.zst
// files at build time using the same klauspost/compress library.
package embedzstd

import (
	"bytes"
	"embed"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

// Read returns the decompressed contents of `path` inside `fs`. The
// file is expected to be a klauspost/zstd stream — tools/embedcompress
// is the only intended producer.
func Read(fs embed.FS, path string) ([]byte, error) {
	compressed, err := fs.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read embedded %s: %w", path, err)
	}
	dec, err := zstd.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("zstd reader for %s: %w", path, err)
	}
	defer dec.Close()
	out, err := io.ReadAll(dec)
	if err != nil {
		return nil, fmt.Errorf("decompress %s: %w", path, err)
	}
	return out, nil
}
