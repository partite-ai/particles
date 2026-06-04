package dyld

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

// ehRuntimeConfig mirrors the production config (cmd/particle/runtime.go):
// CoreFeaturesV2 + extended-const + exception-handling.
func ehRuntimeConfig() wazero.RuntimeConfig {
	return wazero.NewRuntimeConfig().WithCoreFeatures(
		api.CoreFeaturesV2 |
			experimental.CoreFeaturesExtendedConst |
			experimental.CoreFeaturesExceptionHandling,
	)
}

// TestEnvModuleEncodesTag checks that an env module carrying a tag
// definition + tag export encodes to a binary wazero accepts (decode +
// validate + instantiate). This is the env side of satisfying a .so's
// `(import "env" "__cpp_exception" (tag (param i32)))`.
func TestEnvModuleEncodesTag(t *testing.T) {
	spec := &envModuleSpec{
		// type 0: (func (param i32)) — the C++ exception tag's shape.
		types: []funcType{{params: []byte{valI32}}},
		tags:  []envTag{{typeIdx: 0}},
		exports: []envExport{
			{name: "__cpp_exception", kind: exportKindTag, idx: 0},
		},
	}
	bin, err := spec.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, ehRuntimeConfig())
	defer rt.Close(ctx)

	// CompileModule runs wazero's decoder + validator: if the tag
	// section is mis-positioned or the export kind/index is wrong,
	// this fails.
	if _, err := rt.CompileModule(ctx, bin); err != nil {
		t.Fatalf("wazero rejected env module with tag: %v", err)
	}
	if _, err := rt.InstantiateWithConfig(ctx, bin, wazero.NewModuleConfig().WithName("env")); err != nil {
		t.Fatalf("instantiate env module with tag: %v", err)
	}
}

// TestEnvTagLinksIntoImporter is the end-to-end link check: an env
// module defines+exports a tag, and a second module imports it and
// actually uses it (throw inside a function). wazero must resolve the
// cross-module tag import (store.go ExternTypeTag) and validate the
// throw against the imported tag's type.
func TestEnvTagLinksIntoImporter(t *testing.T) {
	env := &envModuleSpec{
		types:   []funcType{{params: []byte{valI32}}},
		tags:    []envTag{{typeIdx: 0}},
		exports: []envExport{{name: "__cpp_exception", kind: exportKindTag, idx: 0}},
	}
	envBin, err := env.encode()
	if err != nil {
		t.Fatalf("encode env: %v", err)
	}

	// Importer module (hand-assembled): imports the env tag, then a
	// `raise` function that does `i32.const 7; throw 0`.
	importer := buildTagImporter(t)

	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, ehRuntimeConfig())
	defer rt.Close(ctx)

	if _, err := rt.InstantiateWithConfig(ctx, envBin, wazero.NewModuleConfig().WithName("env")); err != nil {
		t.Fatalf("instantiate env: %v", err)
	}
	mod, err := rt.InstantiateWithConfig(ctx, importer, wazero.NewModuleConfig().WithName("lib"))
	if err != nil {
		t.Fatalf("instantiate importer (tag link failed): %v", err)
	}

	// Calling raise() throws the imported tag; wazero surfaces it as an
	// error. The point is that compile+link+throw all accept the tag —
	// not the specific error value.
	_, callErr := mod.ExportedFunction("raise").Call(ctx)
	if callErr == nil {
		t.Fatalf("expected raise() to throw, got nil error")
	}
}

// buildTagImporter hand-assembles a wasm module that imports
// `env.__cpp_exception` (tag, type (param i32)) and exports a `raise`
// function whose body is `i32.const 7; throw 0`.
func buildTagImporter(t *testing.T) []byte {
	t.Helper()
	var w bytes.Buffer
	w.WriteString(wasmMagic)
	w.Write([]byte{0x01, 0x00, 0x00, 0x00}) // version 1

	// Type section: type 0 = (func (param i32)) for the tag;
	// type 1 = (func) for raise.
	writeSection(&w, secType, func(b *bytes.Buffer) {
		writeULEB128(b, 2)
		// type 0: (param i32) -> ()
		b.WriteByte(0x60)
		writeULEB128(b, 1)
		b.WriteByte(valI32)
		writeULEB128(b, 0)
		// type 1: () -> ()
		b.WriteByte(0x60)
		writeULEB128(b, 0)
		writeULEB128(b, 0)
	})

	// Import section: env.__cpp_exception tag of type 0.
	writeSection(&w, secImport, func(b *bytes.Buffer) {
		writeULEB128(b, 1)
		writeName(b, "env")
		writeName(b, "__cpp_exception")
		b.WriteByte(importKindTag)
		b.WriteByte(0x00) // attribute
		writeULEB128(b, 0) // type idx
	})

	// Function section: one function (raise) of type 1.
	writeSection(&w, 3, func(b *bytes.Buffer) {
		writeULEB128(b, 1)
		writeULEB128(b, 1) // type index 1
	})

	// Export section: export raise (func index 0).
	writeSection(&w, secExport, func(b *bytes.Buffer) {
		writeULEB128(b, 1)
		writeName(b, "raise")
		b.WriteByte(exportKindFunc)
		writeULEB128(b, 0)
	})

	// Code section: raise body = i32.const 7; throw 0; end.
	writeSection(&w, 10, func(b *bytes.Buffer) {
		writeULEB128(b, 1) // one function body
		var body bytes.Buffer
		writeULEB128(&body, 0) // no locals
		body.WriteByte(0x41)   // i32.const
		writeSLEB128(&body, 7)
		body.WriteByte(0x08)   // throw
		writeULEB128(&body, 0) // tag index 0
		body.WriteByte(0x0B)   // end
		writeULEB128(b, uint64(body.Len()))
		b.Write(body.Bytes())
	})

	return w.Bytes()
}

// TestParseSOTagImport checks parseSO records a tag import (kind 0x04)
// from a hand-built .so, rather than failing with "unsupported kind".
func TestParseSOTagImport(t *testing.T) {
	var w bytes.Buffer
	w.WriteString(wasmMagic)
	w.Write([]byte{0x01, 0x00, 0x00, 0x00})

	writeSection(&w, secType, func(b *bytes.Buffer) {
		writeULEB128(b, 1)
		b.WriteByte(0x60)
		writeULEB128(b, 1)
		b.WriteByte(valI32)
		writeULEB128(b, 0)
	})
	writeSection(&w, secImport, func(b *bytes.Buffer) {
		writeULEB128(b, 1)
		writeName(b, "env")
		writeName(b, "__cpp_exception")
		b.WriteByte(importKindTag)
		b.WriteByte(0x00)
		writeULEB128(b, 0)
	})

	info, err := parseSO(w.Bytes())
	if err != nil {
		t.Fatalf("parseSO: %v", err)
	}
	if len(info.tagImports) != 1 {
		t.Fatalf("tagImports = %d, want 1", len(info.tagImports))
	}
	if got := info.tagImports[0]; got.name != "__cpp_exception" || got.typeIdx != 0 {
		t.Fatalf("tagImport = %+v, want {__cpp_exception 0}", got)
	}
}

// TestParseRealDuckDBSO parses the actual duckdb extension .so when its
// path is supplied via DUCKDB_SO, asserting the __cpp_exception tag
// import is recovered. Skipped in normal CI (the 44 MB .so isn't
// vendored).
func TestParseRealDuckDBSO(t *testing.T) {
	path := os.Getenv("DUCKDB_SO")
	if path == "" {
		t.Skip("set DUCKDB_SO to the extracted _duckdb…wasm32-wasi.so to run")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	info, err := parseSO(raw)
	if err != nil {
		t.Fatalf("parseSO(duckdb): %v", err)
	}
	var found bool
	for _, ti := range info.tagImports {
		if ti.name == "__cpp_exception" {
			found = true
		}
	}
	if !found {
		t.Fatalf("did not find __cpp_exception tag import; got %+v", info.tagImports)
	}
}
