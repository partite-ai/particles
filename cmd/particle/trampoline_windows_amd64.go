//go:build windows && amd64

package main

import (
	"embed"
	"fmt"

	"github.com/partite-ai/particles/internal/embedzstd"
)

// all: lets the build succeed even before `make win-trampoline` has
// produced the .exe.zst — trampolineStub reports a clear error at run
// time in that case (mirrors internal/build/wacogo's embed pattern).
//
//go:embed all:winstub/amd64
var trampolineFS embed.FS

func trampolineStub() ([]byte, error) {
	b, err := embedzstd.Read(trampolineFS, "winstub/amd64/trampoline.exe.zst")
	if err != nil {
		return nil, fmt.Errorf("windows launcher stub unavailable (build it with `make win-trampoline`): %w", err)
	}
	return b, nil
}
