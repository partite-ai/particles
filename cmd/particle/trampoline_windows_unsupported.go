//go:build windows && !amd64 && !arm64

package main

import "errors"

// particle link ships a prebuilt launcher only for windows/amd64 and
// windows/arm64. Other Windows architectures compile but report this
// at link time rather than failing the build.
func trampolineStub() ([]byte, error) {
	return nil, errors.New("particle link is only supported on windows/amd64 and windows/arm64")
}
