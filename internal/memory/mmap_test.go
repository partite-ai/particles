//go:build linux || darwin || windows

package memory

import (
	"testing"
	"unsafe"

	"github.com/tetratelabs/wazero/experimental"
)

const (
	pageSize = 65536
	// 100+MB grown a page at a time, the scenario that dominates the profile.
	targetBytes = 100 * 1024 * 1024
	maxBytes    = 1 << 32 // unbounded max == the 4GiB run-time limit.
)

func TestMmappedMemory_GrowIsZeroCopyAndStable(t *testing.T) {
	lm := NewMemoryAllocator().Allocate(pageSize, maxBytes)
	defer lm.Free()

	if _, ok := lm.(*mmappedMemory); !ok {
		t.Fatalf("expected mmappedMemory, got %T (reservation failed)", lm)
	}

	buf := lm.Reallocate(pageSize)
	if len(buf) != pageSize {
		t.Fatalf("len = %d, want %d", len(buf), pageSize)
	}
	base := unsafe.Pointer(&buf[0])

	// Grow a page at a time, writing a marker at the end of each new page and
	// checking every previously written marker still reads back. This verifies
	// the base never moves (zero copy) and committed data survives growth.
	const pages = 64
	for p := 1; p < pages; p++ {
		buf = lm.Reallocate(uint64((p + 1) * pageSize))
		if buf == nil {
			t.Fatalf("Reallocate to %d pages failed", p+1)
		}
		if got := unsafe.Pointer(&buf[0]); got != base {
			t.Fatalf("base moved at page %d: %p -> %p", p, base, got)
		}
		buf[(p+1)*pageSize-1] = byte(p)
	}
	for p := 1; p < pages; p++ {
		if got := buf[(p+1)*pageSize-1]; got != byte(p) {
			t.Fatalf("page %d marker = %d, want %d", p, got, byte(p))
		}
	}
}

func TestMmappedMemory_GrowBeyondMaxFails(t *testing.T) {
	lm := NewMemoryAllocator().Allocate(pageSize, 2*pageSize)
	defer lm.Free()
	if got := lm.Reallocate(3 * pageSize); got != nil {
		t.Fatalf("Reallocate beyond reservation = %v, want nil", got)
	}
}

func TestSliceMemory_Fallback(t *testing.T) {
	var lm experimental.LinearMemory = &sliceMemory{}
	defer lm.Free()
	buf := lm.Reallocate(pageSize)
	buf[0] = 1
	buf = lm.Reallocate(2 * pageSize)
	if buf[0] != 1 {
		t.Fatal("data not preserved across grow")
	}
}

// growTo grows lm to targetBytes one page at a time, touching the first byte of
// each new page so the comparison includes the page faults both strategies pay.
func growTo(lm experimental.LinearMemory) {
	for size := pageSize; size <= targetBytes; size += pageSize {
		buf := lm.Reallocate(uint64(size))
		buf[size-pageSize] = 1
	}
}

func BenchmarkGrow_Mmap(b *testing.B) {
	b.SetBytes(targetBytes)
	for i := 0; i < b.N; i++ {
		lm := NewMemoryAllocator().Allocate(pageSize, maxBytes)
		growTo(lm)
		lm.Free()
	}
}

// BenchmarkGrow_Slice measures the copying baseline (wazero's default path).
func BenchmarkGrow_Slice(b *testing.B) {
	b.SetBytes(targetBytes)
	for i := 0; i < b.N; i++ {
		lm := &sliceMemory{}
		growTo(lm)
		lm.Free()
	}
}
