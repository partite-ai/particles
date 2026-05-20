// embedcompress takes an input file and writes a zstd-compressed copy
// to the output path. Used by the Makefile to shrink wasm artifacts
// before they get baked in via go:embed.
//
// Single tool used by both runtime/embed and internal/build/wacogo/embed
// so the compression parameters (klauspost/zstd, level
// SpeedBestCompression) match what the matching decompressor in the
// `runtime` package expects.
//
//	go run ./tools/embedcompress  <in.wasm>  <out.wasm.zst>
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: embedcompress <input> <output>")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "embedcompress:", err)
		os.Exit(1)
	}
}

func run(inPath, outPath string) error {
	in, err := os.Open(inPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", inPath, err)
	}
	defer in.Close()

	out, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", outPath, err)
	}
	defer out.Close()

	// SpeedBestCompression is klauspost's max-effort level. Compression
	// is slow (seconds for a 22MB wasm) but the artifacts are
	// embedded once per build and decompressed once per process
	// startup — the runtime cost is bounded and well worth the
	// binary-size win.
	enc, err := zstd.NewWriter(out, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
	if err != nil {
		return fmt.Errorf("zstd writer: %w", err)
	}
	if _, err := io.Copy(enc, in); err != nil {
		_ = enc.Close()
		return fmt.Errorf("compress %s: %w", inPath, err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("flush %s: %w", outPath, err)
	}
	return nil
}
