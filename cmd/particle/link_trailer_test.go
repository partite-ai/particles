package main

import (
	"encoding/binary"
	"testing"
)

// decodeTrailer mirrors the parser in
// components/win-trampoline/src/main.rs. Keeping an independent
// decoder here pins the wire format so a change on one side that
// breaks the other fails this test.
func decodeTrailer(t *testing.T, data []byte) []string {
	t.Helper()
	magic := []byte(trampolineMagic)
	if len(data) < len(magic)+4 {
		t.Fatalf("trailer too short: %d bytes", len(data))
	}
	if got := string(data[len(data)-len(magic):]); got != trampolineMagic {
		t.Fatalf("magic = %q, want %q", got, trampolineMagic)
	}
	head := data[:len(data)-len(magic)]
	payloadLen := int(binary.LittleEndian.Uint32(head[len(head)-4:]))
	head = head[:len(head)-4]
	if payloadLen > len(head) {
		t.Fatalf("payloadLen %d exceeds available %d", payloadLen, len(head))
	}
	p := head[len(head)-payloadLen:]

	argc := int(binary.LittleEndian.Uint32(p))
	p = p[4:]
	out := make([]string, 0, argc)
	for i := 0; i < argc; i++ {
		n := int(binary.LittleEndian.Uint32(p))
		p = p[4:]
		out = append(out, string(p[:n]))
		p = p[n:]
	}
	return out
}

func TestEncodeTrailer_RoundTrip(t *testing.T) {
	got := decodeTrailer(t, encodeTrailer(linkSpec{
		particleBin: `C:\Program Files\particle\particle.exe`,
		target:      "github-tools@1.2.0",
	}))
	want := []string{`C:\Program Files\particle\particle.exe`, "run", "github-tools@1.2.0"}
	assertArgs(t, got, want)
}

func TestEncodeTrailer_WithDB(t *testing.T) {
	got := decodeTrailer(t, encodeTrailer(linkSpec{
		particleBin: `C:\particle.exe`,
		target:      "demo",
		dbPath:      `D:\state\particle.db`,
	}))
	want := []string{`C:\particle.exe`, "run", "--db", `D:\state\particle.db`, "demo"}
	assertArgs(t, got, want)
}

func assertArgs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
