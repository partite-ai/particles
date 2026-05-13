package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// startProfile should write both <prefix>.cpu and <prefix>.heap;
// stopping mid-test is enough to flush real (if tiny) profile data
// to each file. We don't try to parse the pprof format here —
// just verifying both files exist and are non-empty catches the
// "we forgot to call StopCPUProfile / WriteHeapProfile" class of
// regression.
func TestStartProfile_WritesCPUAndHeap(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "p")

	var log bytes.Buffer
	stop, err := startProfile(prefix, &log)
	if err != nil {
		t.Fatal(err)
	}

	// Burn a little CPU so the profile has at least one sample.
	x := 0
	for i := 0; i < 1_000_000; i++ {
		x += i
	}
	_ = x

	stop()

	for _, suffix := range []string{".cpu", ".heap"} {
		fi, err := os.Stat(prefix + suffix)
		if err != nil {
			t.Errorf("missing profile output %s: %v", suffix, err)
			continue
		}
		if fi.Size() == 0 {
			t.Errorf("profile output %s is empty", suffix)
		}
	}
}

// A bad prefix (unwritable directory) errors at startup rather
// than silently producing no output.
func TestStartProfile_RejectsUnwritablePath(t *testing.T) {
	if _, err := startProfile("/no/such/dir/p", nil); err == nil {
		t.Error("expected error for unwritable path")
	}
}
