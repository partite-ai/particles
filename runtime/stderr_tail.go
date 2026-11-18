package runtime

import (
	"fmt"
	"sync"
)

// maxStderrBytes is the most of a particle's stderr that is kept: the newest,
// which explains a failure better than the oldest.
const maxStderrBytes = 64 << 10

// stderrTail keeps the newest maxStderrBytes written to a particle's stderr,
// in a ring, and counts the bytes that came before them.
type stderrTail struct {
	mu sync.Mutex
	// ring holds the kept bytes, the oldest at next once it is full. It is
	// made on the first write.
	ring []byte
	next int
	// written is every byte ever written.
	written int64
}

func (t *stderrTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(p)
	t.written += int64(n)
	if t.ring == nil {
		t.ring = make([]byte, maxStderrBytes)
	}
	if len(p) > maxStderrBytes {
		p = p[len(p)-maxStderrBytes:]
	}
	for len(p) > 0 {
		copied := copy(t.ring[t.next:], p)
		t.next = (t.next + copied) % maxStderrBytes
		p = p[copied:]
	}
	return n, nil
}

// String is the kept bytes, oldest first, after a line saying how many
// earlier bytes were dropped, if any were.
func (t *stderrTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.written <= maxStderrBytes {
		return string(t.ring[:t.written])
	}
	return fmt.Sprintf("[… %d earlier bytes dropped]\n%s%s", t.written-maxStderrBytes, t.ring[t.next:], t.ring[:t.next])
}
