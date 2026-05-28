//go:build windows

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

func writeLink(spec linkSpec, force bool) (string, error) {
	stub, err := trampolineStub()
	if err != nil {
		return "", err
	}

	// A Windows executable needs the .exe extension to run from a bare
	// name, so add it when missing (the user explicitly opted into
	// this convenience for `particle link`).
	if !strings.HasSuffix(strings.ToLower(spec.path), ".exe") {
		spec.path += ".exe"
	}

	flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if !force {
		flag |= os.O_EXCL
	}
	f, err := os.OpenFile(spec.path, flag, 0o755)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("%s already exists (use --force to overwrite)", spec.path)
		}
		return "", fmt.Errorf("create link: %w", err)
	}
	if _, err := f.Write(stub); err != nil {
		f.Close()
		return "", fmt.Errorf("write link: %w", err)
	}
	if _, err := f.Write(encodeTrailer(spec)); err != nil {
		f.Close()
		return "", fmt.Errorf("write link: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("write link: %w", err)
	}
	return spec.path, nil
}
