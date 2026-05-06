package wacogo

import (
	"bytes"
	"fmt"

	wc "github.com/partite-ai/wacogo"
)

// field constructs a wacogo record field without dragging the full
// wacogo.Field type into call sites.
func field(name string, v wc.Val) wc.Field {
	return wc.Field{Name: name, Val: v}
}

// withStderr wraps a wasm-trap error with whatever the component wrote
// to stderr — the trap message itself is just "wasm error: unreachable",
// so the actual diagnostic ends up on stderr.
func withStderr(err error, buf *bytes.Buffer, label string) error {
	if msg := buf.String(); msg != "" {
		return fmt.Errorf("%s: %w\nstderr:\n%s", label, err, msg)
	}
	return fmt.Errorf("%s: %w", label, err)
}
