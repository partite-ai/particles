//go:build !(linux || darwin || windows)

package memory

import "github.com/tetratelabs/wazero/experimental"

// NewMemoryAllocator returns nil on platforms without a virtual-memory
// reservation implementation. experimental.WithMemoryAllocator treats a nil
// allocator as a no-op, so the caller transparently falls back to wazero's
// default (copying) growth path.
func NewMemoryAllocator() experimental.MemoryAllocator {
	return nil
}
