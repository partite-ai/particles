package memory

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// reserve reserves max bytes of address space with no access and no commit
// charge. Pages are committed later by commit.
func reserve(max uintptr) ([]byte, error) {
	base, err := windows.VirtualAlloc(0, max, windows.MEM_RESERVE, windows.PAGE_NOACCESS)
	if err != nil {
		return nil, err
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(base)), max), nil
}

// commit makes region[from:to] readable and writable. Committing pages that are
// already committed is idempotent, and the pages are demand-zeroed on first
// access.
func commit(region []byte, from, to uintptr) error {
	base := uintptr(unsafe.Pointer(&region[0]))
	_, err := windows.VirtualAlloc(base+from, to-from, windows.MEM_COMMIT, windows.PAGE_READWRITE)
	return err
}

func release(region []byte) error {
	// size must be 0 with MEM_RELEASE; the whole reservation is freed.
	return windows.VirtualFree(uintptr(unsafe.Pointer(&region[0])), 0, windows.MEM_RELEASE)
}
