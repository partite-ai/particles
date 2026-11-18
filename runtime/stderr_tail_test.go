package runtime

import (
	"fmt"
	"strings"
	"testing"
)

// A particle's stderr keeps everything up to maxStderrBytes, and past that the
// newest maxStderrBytes, after a line saying how many earlier bytes were
// dropped, however the writes split it.
func TestStderrKeepsTheNewestBytes(t *testing.T) {
	line := func(i int) string { return fmt.Sprintf("line %06d\n", i) } // 12 bytes
	for _, tc := range []struct {
		name   string
		writes []string
	}{
		{"nothing", nil},
		{"under the limit", []string{"one\n", "two\n"}},
		{"exactly the limit", []string{strings.Repeat("x", maxStderrBytes)}},
		{"many small writes past the limit", func() []string {
			var writes []string
			for i := range 20000 {
				writes = append(writes, line(i))
			}
			return writes
		}()},
		{"one write larger than the limit", []string{"start\n", strings.Repeat("y", 3*maxStderrBytes) + "end\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tail stderrTail
			all := strings.Join(tc.writes, "")
			for _, w := range tc.writes {
				if n, err := tail.Write([]byte(w)); n != len(w) || err != nil {
					t.Fatalf("Write = %d, %v; want %d", n, err, len(w))
				}
			}
			want := all
			if len(all) > maxStderrBytes {
				want = fmt.Sprintf("[… %d earlier bytes dropped]\n%s", len(all)-maxStderrBytes, all[len(all)-maxStderrBytes:])
			}
			if got := tail.String(); got != want {
				t.Errorf("kept %d bytes starting %.40q, want %d starting %.40q", len(got), got, len(want), want)
			}
		})
	}
}
