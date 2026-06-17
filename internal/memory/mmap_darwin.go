package memory

import "golang.org/x/sys/unix"

// reserve maps max bytes of address space with no access. macOS has no
// meaningful MAP_NORESERVE, but a PROT_NONE anonymous mapping consumes neither
// physical memory nor swap until pages are committed by commit.
func reserve(max uintptr) ([]byte, error) {
	return unix.Mmap(-1, 0, int(max), unix.PROT_NONE,
		unix.MAP_ANON|unix.MAP_PRIVATE)
}

// commit makes region[from:to] readable and writable. The pages are
// demand-zeroed by the kernel on first access.
func commit(region []byte, from, to uintptr) error {
	return unix.Mprotect(region[from:to], unix.PROT_READ|unix.PROT_WRITE)
}

func release(region []byte) error {
	return unix.Munmap(region)
}
