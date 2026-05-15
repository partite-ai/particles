package runtime

import "testing"

// NewWasmConfig is a thin wrapper around wazero's builder; the
// test pins that the function constructs without panicking
// across the obvious inputs (no limit, small limit). The
// wazero-side behavior (CoreFeatures, CloseOnContextDone) is
// already tested upstream; this is a guard against API drift —
// if a wazero option is renamed, the build breaks here too.
func TestNewWasmConfig_Builds(t *testing.T) {
	if got := NewWasmConfig(WasmOptions{}); got == nil {
		t.Fatal("NewWasmConfig({}) returned nil")
	}
	if got := NewWasmConfig(WasmOptions{MemoryLimitPages: 16}); got == nil {
		t.Fatal("NewWasmConfig({MemoryLimitPages: 16}) returned nil")
	}
}
