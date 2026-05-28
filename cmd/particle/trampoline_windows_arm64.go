//go:build windows && arm64

package main

import (
	"embed"
	"fmt"

	"github.com/partite-ai/particles/internal/embedzstd"
)

//go:embed all:winstub/arm64
var trampolineFS embed.FS

func trampolineStub() ([]byte, error) {
	b, err := embedzstd.Read(trampolineFS, "winstub/arm64/trampoline.exe.zst")
	if err != nil {
		return nil, fmt.Errorf("windows launcher stub unavailable (build it with `make win-trampoline`): %w", err)
	}
	return b, nil
}
