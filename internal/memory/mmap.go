//go:build linux || darwin || windows

// Package memory provides a zero-copy MemoryAllocator for linear memory.
//
// The default growth strategy reallocates and copies the backing slice each
// time a module outgrows its capacity, which for large memories grown a page
// at a time copies several times the final size in aggregate. The allocator
// returned by NewMemoryAllocator instead reserves the maximum size of each
// linear memory as virtual address space up front and commits pages lazily as
// the guest grows, so memory.Grow only ever changes the buffer length: no
// reallocation and no copying.
//
// Reserved-but-uncommitted address space costs no physical memory; the OS
// faults in (zeroed) pages on first write, so resident memory still tracks the
// pages the guest actually touches, not the reservation. Because the base
// address never moves, the allocator also satisfies the requirement for
// backing shared memories.
package memory

import "github.com/tetratelabs/wazero/experimental"

// NewMemoryAllocator returns an experimental.MemoryAllocator that backs each
// linear memory with a lazily-committed virtual reservation, making
// memory.Grow a zero-copy operation. See the package documentation for
// details.
//
// It is supported on Linux, macOS and Windows. On other platforms
// NewMemoryAllocator returns nil, which experimental.WithMemoryAllocator
// treats as a no-op, so those platforms transparently fall back to the default
// (copying) growth path.
func NewMemoryAllocator() experimental.MemoryAllocator {
	return allocator{}
}

type allocator struct{}

// Allocate implements experimental.MemoryAllocator.
func (allocator) Allocate(_, maxBytes uint64) experimental.LinearMemory {
	region, err := reserve(uintptr(maxBytes))
	if err != nil {
		// Reservation failed (e.g. out of address space): degrade gracefully
		// to the copying path rather than failing instantiation.
		return &sliceMemory{}
	}
	return &mmappedMemory{region: region}
}

// mmappedMemory is a LinearMemory backed by a fixed virtual reservation whose
// pages are committed on demand. Wasm linear memory only ever grows, so the
// committed prefix is monotonic and we commit only the newly-grown delta.
type mmappedMemory struct {
	region    []byte  // full reservation; len == cap == max, base never moves
	committed uintptr // bytes made readable/writable so far
}

// Reallocate implements experimental.LinearMemory.
func (m *mmappedMemory) Reallocate(size uint64) []byte {
	s := uintptr(size)
	if s > uintptr(len(m.region)) {
		return nil // beyond the reserved maximum: signal grow failure.
	}
	if s > m.committed {
		// size is always a multiple of the 64KiB Wasm page, hence OS-page
		// aligned, so [committed, s) is a valid range to commit.
		if err := commit(m.region, m.committed, s); err != nil {
			return nil
		}
		m.committed = s
	}
	return m.region[:size]
}

// Free implements experimental.LinearMemory.
func (m *mmappedMemory) Free() {
	if m.region != nil {
		_ = release(m.region)
		m.region = nil
	}
}

// sliceMemory is the copying fallback used when a reservation cannot be made.
// It mirrors wazero's default growth behaviour.
type sliceMemory struct{ buf []byte }

// Reallocate implements experimental.LinearMemory. It grows geometrically,
// mirroring wazero's default append-based path, so the fallback is never worse
// than not using an allocator at all.
func (m *sliceMemory) Reallocate(size uint64) []byte {
	if uint64(cap(m.buf)) < size {
		b := append(m.buf[:cap(m.buf)], make([]byte, size-uint64(cap(m.buf)))...)
		m.buf = b[:size]
	} else {
		m.buf = m.buf[:size]
	}
	return m.buf
}

// Free implements experimental.LinearMemory.
func (m *sliceMemory) Free() { m.buf = nil }
