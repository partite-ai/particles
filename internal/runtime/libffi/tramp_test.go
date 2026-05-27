// Unit tests for the libffi trampoline builders.
//
// These spin up a real wazero runtime, instantiate the trampoline
// modules against a hand-crafted "main" that exports memory + table
// + __stack_pointer, and exercise each lowering rule end-to-end:
//
//   - primitives (i32, i64, f32, f64)
//   - longdouble (2× i64)
//   - struct-flat (multi-field small struct)
//   - varargs
//   - sret for aggregate returns
//   - closures (C-side calls into the trampoline, trampoline calls
//     a "user callback" registered as the closure's fun field)
//
// The fixture's strategy: emit a tiny "user function" wasm module
// (the would-be target of the call_indirect) that does something
// observable per signature (multiplies arg by 2, writes a struct,
// etc.). The test then drives the trampoline via the Go side and
// verifies the observable result.

package libffi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

// fixture spins up a wazero runtime with a "main" module that
// exports the imports trampolines need: memory, an indirect-function
// table sized for our slots, and __stack_pointer + __grow_table.
// Tests then build trampolines + user-callback modules against this
// fixture.
type fixture struct {
	rt   wazero.Runtime
	main api.Module
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	t.Cleanup(func() { rt.Close(ctx) })

	// "main" module exporting:
	//   memory       (1 page = 64 KB, with room for stack at the top)
	//   __indirect_function_table (initial 0, growable)
	//   __stack_pointer (mut i32 global, initialized to 64KB-16)
	//   __grow_table  (table.grow wrapper)
	//
	// Hand-encode the wasm bytes — same pattern as the trampoline
	// builders themselves use.
	mainWasm := buildFixtureMain()
	mainMod, err := rt.CompileModule(ctx, mainWasm)
	if err != nil {
		t.Fatalf("compile fixture main: %v", err)
	}
	main, err := rt.InstantiateModule(ctx, mainMod,
		wazero.NewModuleConfig().WithName("main").WithStartFunctions())
	if err != nil {
		t.Fatalf("instantiate fixture main: %v", err)
	}
	t.Cleanup(func() { main.Close(ctx) })

	return &fixture{rt: rt, main: main}
}

// buildFixtureMain emits the test-fixture "main" module. Has the
// surface area trampolines need to import + a couple of helper
// exports (memory accessors, __grow_table).
func buildFixtureMain() []byte {
	// (module
	//   (memory (export "memory") 2)
	//   (table (export "__indirect_function_table") 0 funcref)
	//   (global (export "__stack_pointer") (mut i32) (i32.const 131072))
	//   (func (export "__grow_table") (param i32) (result i32)
	//     ref.null func local.get 0 table.grow 0)
	// )
	var out []byte
	out = append(out, 0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00)

	// Types: 1 type — (i32) -> i32 for __grow_table.
	out = appendSection(out, 0x01, []byte{
		0x01,
		0x60, 0x01, TyI32, 0x01, TyI32,
	})
	// Functions: 1 of type 0.
	out = appendSection(out, 0x03, []byte{0x01, 0x00})
	// Tables: 1 table, funcref, min=0.
	out = appendSection(out, 0x04, []byte{0x01, 0x70, 0x00, 0x00})
	// Memories: 1 memory, min=2 pages.
	out = appendSection(out, 0x05, []byte{0x01, 0x00, 0x02})
	// Globals: __stack_pointer, mut i32, init = i32.const 131072.
	out = appendSection(out, 0x06, []byte{
		0x01, TyI32, 0x01,
		0x41, 0x80, 0x80, 0x08, // i32.const 131072 (uleb)
		0x0b,
	})
	// Exports: memory, __indirect_function_table, __stack_pointer, __grow_table.
	var exports []byte
	exports = append(exports, 0x04) // count
	exports = appendStr(exports, "memory")
	exports = append(exports, 0x02, 0x00)
	exports = appendStr(exports, "__indirect_function_table")
	exports = append(exports, 0x01, 0x00)
	exports = appendStr(exports, "__stack_pointer")
	exports = append(exports, 0x03, 0x00)
	exports = appendStr(exports, "__grow_table")
	exports = append(exports, 0x00, 0x00)
	out = appendSection(out, 0x07, exports)

	// Code: __grow_table body = ref.null func; local.get 0; table.grow 0.
	body := []byte{
		0x00,             // 0 locals
		0xd0, 0x70,       // ref.null func (heap_type funcref)
		0x20, 0x00,       // local.get 0
		0xfc, 0x0f, 0x00, // table.grow 0
		0x0b,
	}
	var codeSec []byte
	codeSec = append(codeSec, 0x01)
	codeSec = append(codeSec, uleb128(uint32(len(body)))...)
	codeSec = append(codeSec, body...)
	out = appendSection(out, 0x0a, codeSec)
	return out
}

// placeCallable: instantiate a user-function module that exports a
// single function "fn" with the given (params, results, body). The
// elem segment places it in main's __indirect_function_table at a
// freshly-allocated slot; returns the slot index.
func (f *fixture) placeCallable(t *testing.T, params, results, body []byte) uint32 {
	t.Helper()
	ctx := context.Background()
	growFn := f.main.ExportedFunction("__grow_table")
	res, err := growFn.Call(ctx, 1)
	if err != nil {
		t.Fatalf("__grow_table: %v", err)
	}
	slot := uint32(res[0])

	var out []byte
	out = append(out, 0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00)
	// Type 0 = the function's signature.
	var typeSec []byte
	typeSec = append(typeSec, 0x01)
	typeSec = appendFuncType(typeSec, params, results)
	out = appendSection(out, 0x01, typeSec)
	// Imports: memory + table (so we can place into main's table).
	var importSec []byte
	importSec = append(importSec, 0x02)
	importSec = appendStr(importSec, "main")
	importSec = appendStr(importSec, "memory")
	importSec = append(importSec, 0x02, 0x00, 0x00)
	importSec = appendStr(importSec, "main")
	importSec = appendStr(importSec, "__indirect_function_table")
	importSec = append(importSec, 0x01, 0x70, 0x00, 0x00)
	out = appendSection(out, 0x02, importSec)
	// Function section: 1 func of type 0.
	out = appendSection(out, 0x03, []byte{0x01, 0x00})
	// Element: place at table[slot].
	var elemSec []byte
	elemSec = append(elemSec, 0x01)
	elemSec = append(elemSec, 0x00)
	elemSec = append(elemSec, 0x41)
	elemSec = append(elemSec, sleb128(int64(slot))...)
	elemSec = append(elemSec, 0x0b)
	elemSec = append(elemSec, 0x01, 0x00)
	out = appendSection(out, 0x09, elemSec)
	// Code.
	var codeSec []byte
	codeSec = append(codeSec, 0x01)
	codeSec = append(codeSec, uleb128(uint32(len(body)))...)
	codeSec = append(codeSec, body...)
	out = appendSection(out, 0x0a, codeSec)

	mod, err := f.rt.CompileModule(ctx, out)
	if err != nil {
		t.Fatalf("compile user fn: %v", err)
	}
	resolveCtx := experimental.WithImportResolver(ctx, func(mn string) api.Module {
		if mn == "main" {
			return f.main
		}
		return nil
	})
	if _, err := f.rt.InstantiateModule(resolveCtx, mod,
		wazero.NewModuleConfig().WithName("").WithStartFunctions()); err != nil {
		t.Fatalf("instantiate user fn: %v", err)
	}
	return slot
}

// instantiateTrampoline builds + instantiates a call-direction
// trampoline for the given sig at a freshly-allocated table slot.
func (f *fixture) instantiateTrampoline(t *testing.T, sig []byte) uint32 {
	t.Helper()
	ctx := context.Background()
	parsed, err := ParseSig(sig)
	if err != nil {
		t.Fatalf("ParseSig: %v", err)
	}
	growFn := f.main.ExportedFunction("__grow_table")
	res, err := growFn.Call(ctx, 1)
	if err != nil {
		t.Fatalf("__grow_table: %v", err)
	}
	slot := uint32(res[0])
	bytes, err := buildCallTrampoline(parsed, slot)
	if err != nil {
		t.Fatalf("buildCallTrampoline: %v", err)
	}
	mod, err := f.rt.CompileModule(ctx, bytes)
	if err != nil {
		t.Fatalf("compile trampoline: %v", err)
	}
	resolveCtx := experimental.WithImportResolver(ctx, func(mn string) api.Module {
		if mn == "main" {
			return f.main
		}
		return nil
	})
	if _, err := f.rt.InstantiateModule(resolveCtx, mod,
		wazero.NewModuleConfig().WithName("").WithStartFunctions()); err != nil {
		t.Fatalf("instantiate trampoline: %v", err)
	}
	return slot
}

// callTrampoline invokes the trampoline by simulating
// __wasi_libffi_dispatch — call_indirect at the trampoline's slot
// with (fn_idx, rvalue, avalue). We compile a tiny adapter module
// inside the fixture for this rather than reach into wazero's
// table directly.
func (f *fixture) callTrampoline(t *testing.T, trampolineSlot, fnIdx, rvalue, avalue uint32) {
	t.Helper()
	ctx := context.Background()

	// A dispatch helper module: exports `dispatch(fn,rv,av,slot)`.
	// body: local.get 0; local.get 1; local.get 2; local.get 3;
	//       call_indirect type=0 table=0; end.
	var out []byte
	out = append(out, 0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00)
	// 2 types: marshaler (i32,i32,i32)->(), dispatch (i32,i32,i32,i32)->().
	out = appendSection(out, 0x01, []byte{
		0x02,
		0x60, 0x03, TyI32, TyI32, TyI32, 0x00,
		0x60, 0x04, TyI32, TyI32, TyI32, TyI32, 0x00,
	})
	var importSec []byte
	importSec = append(importSec, 0x01)
	importSec = appendStr(importSec, "main")
	importSec = appendStr(importSec, "__indirect_function_table")
	importSec = append(importSec, 0x01, 0x70, 0x00, 0x00)
	out = appendSection(out, 0x02, importSec)
	// 1 func of type 1.
	out = appendSection(out, 0x03, []byte{0x01, 0x01})
	// Export "dispatch".
	var exp []byte
	exp = append(exp, 0x01)
	exp = appendStr(exp, "dispatch")
	exp = append(exp, 0x00, 0x00)
	out = appendSection(out, 0x07, exp)
	dispatchBody := []byte{
		0x00,
		0x20, 0x00, // local.get 0
		0x20, 0x01, // local.get 1
		0x20, 0x02, // local.get 2
		0x20, 0x03, // local.get 3
		0x11, 0x00, 0x00, // call_indirect type=0 table=0
		0x0b,
	}
	var dispatchCodeSec []byte
	dispatchCodeSec = append(dispatchCodeSec, 0x01)
	dispatchCodeSec = append(dispatchCodeSec, uleb128(uint32(len(dispatchBody)))...)
	dispatchCodeSec = append(dispatchCodeSec, dispatchBody...)
	out = appendSection(out, 0x0a, dispatchCodeSec)

	mod, err := f.rt.CompileModule(ctx, out)
	if err != nil {
		t.Fatalf("compile dispatcher: %v", err)
	}
	resolveCtx := experimental.WithImportResolver(ctx, func(mn string) api.Module {
		if mn == "main" {
			return f.main
		}
		return nil
	})
	inst, err := f.rt.InstantiateModule(resolveCtx, mod,
		wazero.NewModuleConfig().WithName("").WithStartFunctions())
	if err != nil {
		t.Fatalf("instantiate dispatcher: %v", err)
	}
	defer inst.Close(ctx)
	dispatch := inst.ExportedFunction("dispatch")
	if _, err := dispatch.Call(ctx,
		uint64(fnIdx), uint64(rvalue), uint64(avalue), uint64(trampolineSlot),
	); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
}

// --- tests ---------------------------------------------------------------

func TestParseSig_Roundtrip(t *testing.T) {
	cases := []struct {
		name   string
		sig    []byte
		want   *ParsedSig
		errMsg string
	}{
		{
			name: "void/void",
			sig:  []byte{0x00, 0x00, TyVoid, 0x00},
			want: &ParsedSig{NFixedArgs: 0, Return: ParsedType{Kind: TyVoid}},
		},
		{
			name: "i32->i32",
			sig:  []byte{0x00, 0x01, TyI32, 0x01, TyI32},
			want: &ParsedSig{NFixedArgs: 1, Return: ParsedType{Kind: TyI32}, Params: []ParsedType{{Kind: TyI32}}},
		},
		{
			name: "longdouble param",
			sig:  []byte{0x00, 0x01, TyVoid, 0x01, TyLongDouble},
			want: &ParsedSig{NFixedArgs: 1, Return: ParsedType{Kind: TyVoid}, Params: []ParsedType{{Kind: TyLongDouble}}},
		},
		{
			name: "struct-flat 2 fields",
			sig:  []byte{0x00, 0x01, TyVoid, 0x01, TyStructFlat, 0x02, TyI32, TyI64},
			want: &ParsedSig{
				NFixedArgs: 1,
				Return:     ParsedType{Kind: TyVoid},
				Params: []ParsedType{
					{Kind: TyStructFlat, FlatFields: []ParsedType{{Kind: TyI32}, {Kind: TyI64}}},
				},
			},
		},
		{
			name: "varargs 1 fixed",
			sig:  []byte{HeaderVarargs, 0x01, TyI32, 0x02, TyI32, TyI32},
			want: &ParsedSig{
				Varargs:    true,
				NFixedArgs: 1,
				Return:     ParsedType{Kind: TyI32},
				Params:     []ParsedType{{Kind: TyI32}, {Kind: TyI32}},
			},
		},
		{
			name:   "trailing garbage",
			sig:    []byte{0x00, 0x00, TyVoid, 0x00, 0x42},
			errMsg: "trailing",
		},
		{
			name:   "nested struct rejected",
			sig:    []byte{0x00, 0x01, TyVoid, 0x01, TyStructFlat, 0x01, TyStructFlat},
			errMsg: "nested",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSig(tc.sig)
			if tc.errMsg != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil", tc.errMsg)
				}
				if !contains(err.Error(), tc.errMsg) {
					t.Fatalf("err = %q, want substring %q", err, tc.errMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSig: %v", err)
			}
			if got.Varargs != tc.want.Varargs ||
				got.NFixedArgs != tc.want.NFixedArgs ||
				got.Return.Kind != tc.want.Return.Kind ||
				len(got.Params) != len(tc.want.Params) {
				t.Fatalf("parsed: %+v\nwant:   %+v", got, tc.want)
			}
		})
	}
}

func contains(s, sub string) bool { return bytes.Contains([]byte(s), []byte(sub)) }

// TestCallTrampoline_I32xI32 exercises the simplest case: a function
// that takes (i32, i32) and returns i32. The user fn returns a + b;
// we pass (10, 20) via libffi's avalue and assert rvalue = 30.
func TestCallTrampoline_I32xI32(t *testing.T) {
	f := newFixture(t)

	// User fn: (i32, i32) -> i32, body returns a + b.
	userBody := []byte{
		0x00,
		0x20, 0x00,
		0x20, 0x01,
		0x6a, // i32.add
		0x0b,
	}
	fnSlot := f.placeCallable(t, []byte{TyI32, TyI32}, []byte{TyI32}, userBody)

	// sig: (i32, i32) -> i32
	sig := []byte{0x00, 0x02, TyI32, 0x02, TyI32, TyI32}
	trampSlot := f.instantiateTrampoline(t, sig)

	// Lay out memory: avalue[0] -> &arg0, avalue[1] -> &arg1, rvalue.
	mem := f.main.Memory()
	const (
		arg0Off  = 1024
		arg1Off  = 1028
		avalOff  = 1032
		rvalOff  = 1040
	)
	mem.WriteUint32Le(arg0Off, 10)
	mem.WriteUint32Le(arg1Off, 20)
	mem.WriteUint32Le(avalOff, arg0Off)
	mem.WriteUint32Le(avalOff+4, arg1Off)

	f.callTrampoline(t, trampSlot, fnSlot, rvalOff, avalOff)

	got, _ := mem.ReadUint32Le(rvalOff)
	if got != 30 {
		t.Fatalf("rvalue = %d, want 30", got)
	}
}

// TestCallTrampoline_F64Result exercises floating-point lowering.
// User fn: f64.add(a, b). Inputs 1.5 + 2.5 should give 4.0.
func TestCallTrampoline_F64Result(t *testing.T) {
	f := newFixture(t)
	// (f64, f64) -> f64 ; body: f64.add
	userBody := []byte{
		0x00,
		0x20, 0x00,
		0x20, 0x01,
		0xa0, // f64.add
		0x0b,
	}
	fnSlot := f.placeCallable(t, []byte{TyF64, TyF64}, []byte{TyF64}, userBody)
	sig := []byte{0x00, 0x02, TyF64, 0x02, TyF64, TyF64}
	trampSlot := f.instantiateTrampoline(t, sig)

	mem := f.main.Memory()
	const (
		arg0Off = 1024
		arg1Off = 1032
		avalOff = 1040
		rvalOff = 1048
	)
	mem.WriteUint64Le(arg0Off, math.Float64bits(1.5))
	mem.WriteUint64Le(arg1Off, math.Float64bits(2.5))
	mem.WriteUint32Le(avalOff, arg0Off)
	mem.WriteUint32Le(avalOff+4, arg1Off)

	f.callTrampoline(t, trampSlot, fnSlot, rvalOff, avalOff)

	bits, _ := mem.ReadUint64Le(rvalOff)
	got := math.Float64frombits(bits)
	if got != 4.0 {
		t.Fatalf("rvalue = %v, want 4.0", got)
	}
}

// TestCallTrampoline_LongDouble — user fn takes a longdouble and
// returns void after writing the low/high 64-bit halves to a known
// memory location. Exercises the longdouble = 2× i64 lowering on
// the param side.
func TestCallTrampoline_LongDouble(t *testing.T) {
	f := newFixture(t)
	// user fn (i64 lo, i64 hi) -> void, body:
	//   i32.const PROBE_LO; local.get 0; i64.store
	//   i32.const PROBE_HI; local.get 1; i64.store
	// where PROBE_LO/HI are baked into the body — we use offsets
	// 2048 / 2056.
	const probeLo = 2048
	const probeHi = 2056
	userBody := []byte{
		0x00,
		0x41, 0x80, 0x10, // i32.const probeLo (uleb 2048)
		0x20, 0x00,
		0x37, 0x03, 0x00, // i64.store align=3
		0x41, 0x88, 0x10, // i32.const probeHi (2056)
		0x20, 0x01,
		0x37, 0x03, 0x00,
		0x0b,
	}
	fnSlot := f.placeCallable(t, []byte{TyI64, TyI64}, nil, userBody)

	sig := []byte{0x00, 0x01, TyVoid, 0x01, TyLongDouble}
	trampSlot := f.instantiateTrampoline(t, sig)

	mem := f.main.Memory()
	const (
		argOff  = 1024 // 16 bytes for longdouble
		avalOff = 1040
		rvalOff = 1048 // unused for void return
	)
	// Write a recognizable bit-pattern as two i64s.
	mem.WriteUint64Le(argOff, 0xdeadbeefcafebabe)
	mem.WriteUint64Le(argOff+8, 0xfeedfacefeedfeed)
	mem.WriteUint32Le(avalOff, argOff)

	f.callTrampoline(t, trampSlot, fnSlot, rvalOff, avalOff)

	gotLo, _ := mem.ReadUint64Le(probeLo)
	gotHi, _ := mem.ReadUint64Le(probeHi)
	if gotLo != 0xdeadbeefcafebabe || gotHi != 0xfeedfacefeedfeed {
		t.Fatalf("longdouble halves wrong: lo=0x%x hi=0x%x", gotLo, gotHi)
	}
}

// TestCallTrampoline_StructFlat — user fn takes a struct (i32, i64)
// flattened to 2 wasm args. Sums them as i64 and returns.
func TestCallTrampoline_StructFlat(t *testing.T) {
	f := newFixture(t)
	// (i32, i64) -> i64, body: i64.extend_i32_s(local.get 0) +
	// local.get 1 (i64.add). Simpler: just return arg1 + arg0
	// extended.
	userBody := []byte{
		0x00,
		0x20, 0x01,
		0x20, 0x00,
		0xac, // i64.extend_i32_s
		0x7c, // i64.add
		0x0b,
	}
	fnSlot := f.placeCallable(t, []byte{TyI32, TyI64}, []byte{TyI64}, userBody)

	// sig: (struct{i32, i64}) -> i64
	sig := []byte{0x00, 0x01, TyI64, 0x01, TyStructFlat, 0x02, TyI32, TyI64}
	trampSlot := f.instantiateTrampoline(t, sig)

	mem := f.main.Memory()
	const (
		argOff  = 1024 // struct: i32 at 0, i64 at 8 (natural align)
		avalOff = 1040
		rvalOff = 1048
	)
	mem.WriteUint32Le(argOff, 7)
	mem.WriteUint64Le(argOff+8, 100)
	mem.WriteUint32Le(avalOff, argOff)

	f.callTrampoline(t, trampSlot, fnSlot, rvalOff, avalOff)

	got, _ := mem.ReadUint64Le(rvalOff)
	if got != 107 {
		t.Fatalf("struct-flat result = %d, want 107", got)
	}
}

// TestCallTrampoline_Varargs — function with 1 fixed i32 arg and
// 2 variadic i32s. The body reads each from the vararg buffer and
// returns the sum.
func TestCallTrampoline_Varargs(t *testing.T) {
	f := newFixture(t)
	// (i32 fixed, i32 varargs_buf_ptr) -> i32:
	//   sum = fixed + *(buf+0) + *(buf+4)
	userBody := []byte{
		0x00,
		0x20, 0x00, // fixed
		0x20, 0x01, // buf ptr
		0x28, 0x02, 0x00, // i32.load offset=0
		0x6a, // add
		0x20, 0x01, // buf ptr
		0x28, 0x02, 0x04, // i32.load offset=4
		0x6a,
		0x0b,
	}
	fnSlot := f.placeCallable(t, []byte{TyI32, TyI32}, []byte{TyI32}, userBody)

	sig := []byte{HeaderVarargs, 0x01, TyI32, 0x03, TyI32, TyI32, TyI32}
	trampSlot := f.instantiateTrampoline(t, sig)

	mem := f.main.Memory()
	const (
		a0Off   = 1024
		a1Off   = 1028
		a2Off   = 1032
		avalOff = 1040
		rvalOff = 1056
	)
	mem.WriteUint32Le(a0Off, 100)
	mem.WriteUint32Le(a1Off, 22)
	mem.WriteUint32Le(a2Off, 3)
	mem.WriteUint32Le(avalOff, a0Off)
	mem.WriteUint32Le(avalOff+4, a1Off)
	mem.WriteUint32Le(avalOff+8, a2Off)

	f.callTrampoline(t, trampSlot, fnSlot, rvalOff, avalOff)

	got, _ := mem.ReadUint32Le(rvalOff)
	if got != 125 {
		t.Fatalf("varargs sum = %d, want 125", got)
	}
}

// --- closure tests -------------------------------------------------------

// TestClosureTrampoline_I32 — a closure with sig `(i32) -> i32` and
// a tiny "user callback" that doubles its input. We invoke the
// closure via call_indirect at its slot with the natural calling
// convention; the trampoline marshals into libffi's
// (cif, rvalue, avalue, user_data) shape, calls the user callback
// via the closure's fun field, and the result lands in rvalue. The
// closure trampoline reads the result back and returns it as the
// wasm-level result.
func TestClosureTrampoline_I32(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Allocate the closure struct in main's memory.
	// Layout: ftramp(4) | cif(4) | fun(4) | user_data(4) = 16 bytes.
	const closureOff = 4096

	// User callback ("closure->fun"). Signature:
	//   void cb(ffi_cif *cif, void *rvalue, void **avalue, void *user_data)
	// Body: read avalue[0] (pointer), load i32 from it, double it,
	// store to rvalue. Ignore cif + user_data.
	cbBody := []byte{
		0x00,
		0x20, 0x01, // rvalue
		0x20, 0x02, // avalue
		0x28, 0x02, 0x00, // i32.load (avalue[0])
		0x28, 0x02, 0x00, // i32.load (the value)
		0x41, 0x02, 0x6c, // i32.const 2, i32.mul
		0x36, 0x02, 0x00, // i32.store (rvalue, doubled)
		0x0b,
	}
	cbSlot := f.placeCallable(t,
		[]byte{TyI32, TyI32, TyI32, TyI32}, nil, cbBody,
	)

	// Allocate a closure slot, write fun=cbSlot into the closure
	// struct, build the closure trampoline.
	mem := f.main.Memory()
	mem.WriteUint32Le(closureOff+CLOSURE_OFF_CIF, 0)        // cif (unused by cb)
	mem.WriteUint32Le(closureOff+CLOSURE_OFF_FUN, cbSlot)
	mem.WriteUint32Le(closureOff+CLOSURE_OFF_USER_DATA, 0)

	growFn := f.main.ExportedFunction("__grow_table")
	res, err := growFn.Call(ctx, 1)
	if err != nil {
		t.Fatalf("__grow_table: %v", err)
	}
	tramSlot := uint32(res[0])
	mem.WriteUint32Le(closureOff+CLOSURE_OFF_FTRAMP, tramSlot)

	sig := []byte{0x00, 0x01, TyI32, 0x01, TyI32}
	parsed, _ := ParseSig(sig)
	tramBytes, err := buildClosureTrampoline(parsed, tramSlot, closureOff)
	if err != nil {
		t.Fatalf("buildClosureTrampoline: %v", err)
	}
	mod, err := f.rt.CompileModule(ctx, tramBytes)
	if err != nil {
		t.Fatalf("compile closure: %v", err)
	}
	resolveCtx := experimental.WithImportResolver(ctx, func(mn string) api.Module {
		if mn == "main" {
			return f.main
		}
		return nil
	})
	if _, err := f.rt.InstantiateModule(resolveCtx, mod,
		wazero.NewModuleConfig().WithName("").WithStartFunctions()); err != nil {
		t.Fatalf("instantiate closure: %v", err)
	}

	// Invoke the closure: a one-off dispatcher module that does
	// call_indirect at tramSlot with the natural sig (i32) -> i32.
	got, err := callViaTable(ctx, f, tramSlot, []byte{TyI32}, []byte{TyI32}, []uint64{21})
	if err != nil {
		t.Fatalf("callViaTable: %v", err)
	}
	if got[0] != 42 {
		t.Fatalf("closure result = %d, want 42", got[0])
	}
}

// callViaTable: instantiate an ephemeral module that exports
// `call(args...) -> results...` where the body just call_indirects
// at slot with the given target sig. Used by the closure tests to
// invoke the closure trampoline with its natural calling convention.
func callViaTable(ctx context.Context, f *fixture, slot uint32, params, results []byte, args []uint64) ([]uint64, error) {
	var out []byte
	out = append(out, 0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00)
	// Two types: type 0 = target (params -> results), type 1 = caller
	// (params -> results), same shape; we have type 0 stand in.
	var typeSec []byte
	typeSec = append(typeSec, 0x01)
	typeSec = appendFuncType(typeSec, params, results)
	out = appendSection(out, 0x01, typeSec)
	// Imports.
	var importSec []byte
	importSec = append(importSec, 0x01)
	importSec = appendStr(importSec, "main")
	importSec = appendStr(importSec, "__indirect_function_table")
	importSec = append(importSec, 0x01, 0x70, 0x00, 0x00)
	out = appendSection(out, 0x02, importSec)
	out = appendSection(out, 0x03, []byte{0x01, 0x00})
	var exp []byte
	exp = append(exp, 0x01)
	exp = appendStr(exp, "call")
	exp = append(exp, 0x00, 0x00)
	out = appendSection(out, 0x07, exp)
	// body: push each param local, push slot constant, call_indirect.
	var body []byte
	body = append(body, 0x00) // 0 locals
	for i := range params {
		body = append(body, 0x20, byte(i))
	}
	body = append(body, 0x41)
	body = append(body, sleb128(int64(slot))...)
	body = append(body, 0x11, 0x00, 0x00)
	body = append(body, 0x0b)
	var cvtCodeSec []byte
	cvtCodeSec = append(cvtCodeSec, 0x01)
	cvtCodeSec = append(cvtCodeSec, uleb128(uint32(len(body)))...)
	cvtCodeSec = append(cvtCodeSec, body...)
	out = appendSection(out, 0x0a, cvtCodeSec)

	mod, err := f.rt.CompileModule(ctx, out)
	if err != nil {
		return nil, err
	}
	resolveCtx := experimental.WithImportResolver(ctx, func(mn string) api.Module {
		if mn == "main" {
			return f.main
		}
		return nil
	})
	inst, err := f.rt.InstantiateModule(resolveCtx, mod,
		wazero.NewModuleConfig().WithName("").WithStartFunctions())
	if err != nil {
		return nil, err
	}
	defer inst.Close(ctx)
	res, err := inst.ExportedFunction("call").Call(ctx, args...)
	if err != nil {
		return nil, err
	}
	if len(res) != len(results) {
		return nil, errors.New("result count mismatch")
	}
	return res, nil
}

// Match offsets in closure struct (mirrors libffi_shim.rs).
const (
	CLOSURE_OFF_FTRAMP    = 0
	CLOSURE_OFF_CIF       = 4
	CLOSURE_OFF_FUN       = 8
	CLOSURE_OFF_USER_DATA = 12
)

// Stub binary import — silences unused.
var _ = binary.LittleEndian
