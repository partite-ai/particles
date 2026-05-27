// tramp_call.go — emits the wasm bytes for a call-direction
// trampoline. This is the function libffi's `ffi_call` uses: the
// trampoline takes the marshaler args (fn_idx, rvalue, avalue),
// unpacks libffi's avalue[] pointer array, dispatches into the
// target via `call_indirect`, and stores the result at *rvalue.
//
// Trampoline wasm signature is always `(i32, i32, i32) -> ()`
// (fn_idx, rvalue, avalue) so the main.wasm-side
// `__wasi_libffi_dispatch` helper can call_indirect any trampoline
// uniformly. The TARGET signature (what we actually call_indirect)
// varies — long double splits into 2× i64, struct-flat fields
// expand into multiple wasm slots, structs > 16 bytes pass by
// pointer (i32). Aggregate returns use the sret convention: rvalue
// becomes the first wasm arg and the target returns void.
package libffi

// Local-index conventions for the trampoline function.
const (
	lFn      byte = 0 // fn_idx param
	lRvalue  byte = 1 // rvalue param
	lAvalue  byte = 2 // avalue param
	lSPSave  byte = 3 // saved __stack_pointer (varargs)
	lBufBase byte = 4 // base of vararg buffer
)

// buildCallTrampoline assembles a wasm module whose elem segment
// drops a call-direction trampoline at the given table slot.
func buildCallTrampoline(parsed *ParsedSig, slot uint32) ([]byte, error) {
	targetParams, targetResults := loweredTargetSig(parsed)

	var out []byte
	out = append(out, 0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00)

	// Type section.
	var typeSec []byte
	typeSec = append(typeSec, 0x02)
	typeSec = appendFuncType(typeSec, []byte{TyI32, TyI32, TyI32}, nil) // marshaler
	typeSec = appendFuncType(typeSec, targetParams, targetResults)      // target
	out = appendSection(out, 0x01, typeSec)

	// Import section: memory + table + stack-pointer global.
	var importSec []byte
	importSec = append(importSec, 0x03)
	importSec = appendStr(importSec, "main")
	importSec = appendStr(importSec, "memory")
	importSec = append(importSec, 0x02, 0x00, 0x00)
	importSec = appendStr(importSec, "main")
	importSec = appendStr(importSec, "__indirect_function_table")
	importSec = append(importSec, 0x01, 0x70, 0x00, 0x00)
	importSec = appendStr(importSec, "main")
	importSec = appendStr(importSec, "__stack_pointer")
	importSec = append(importSec, 0x03, TyI32, 0x01)
	out = appendSection(out, 0x02, importSec)

	// Function section.
	out = appendSection(out, 0x03, []byte{0x01, 0x00})

	// Element section: place at table[slot].
	var elemSec []byte
	elemSec = append(elemSec, 0x01)
	elemSec = append(elemSec, 0x00)
	elemSec = append(elemSec, 0x41)
	elemSec = append(elemSec, sleb128(int64(slot))...)
	elemSec = append(elemSec, 0x0b)
	elemSec = append(elemSec, 0x01, 0x00)
	out = appendSection(out, 0x09, elemSec)

	body := buildCallTrampBody(parsed)
	var codeSec []byte
	codeSec = append(codeSec, 0x01)
	codeSec = append(codeSec, uleb128(uint32(len(body)))...)
	codeSec = append(codeSec, body...)
	out = appendSection(out, 0x0a, codeSec)
	return out, nil
}

// loweredTargetSig computes the wasm-level params + results for
// call_indirect's target type.
//
// Lowering rules:
//   - Aggregate return (struct-flat or longdouble): sret — return
//     void; rvalue is prepended as the first param.
//   - longdouble param: expands to 2× i64.
//   - struct-flat param: expands to each field's primitive.
//   - varargs: extra trailing i32 (pointer to vararg buffer).
func loweredTargetSig(parsed *ParsedSig) (params, results []byte) {
	if parsed.Return.IsAggregate() {
		params = append(params, TyI32)
	} else {
		results = parsed.Return.LoweredValtypes()
	}
	nfixed := int(parsed.NFixedArgs)
	for i, p := range parsed.Params {
		if parsed.Varargs && i >= nfixed {
			break
		}
		params = append(params, p.LoweredValtypes()...)
	}
	if parsed.Varargs {
		params = append(params, TyI32)
	}
	return params, results
}

// buildCallTrampBody emits the trampoline function body.
//
// Stack discipline:
//
//   - We declare 2 extra locals beyond the 3 params: lSPSave +
//     lBufBase (only used when varargs is set; cheap otherwise).
//   - For non-sret, non-void returns we push rvalue at the very
//     start so it sits beneath the call_indirect's return value —
//     a single typed store at the end consumes both.
//   - For sret returns, rvalue IS the first call_indirect arg.
//   - Varargs allocate a buffer on the shadow stack, pack arg
//     bytes there, pass the buffer pointer as the extra wasm arg.
//     We save the pre-allocation __stack_pointer in lSPSave and
//     restore at the end. Both lBufBase and lSPSave hold i32
//     values; bufBase is sp_save - bufSize (post-allocation).
func buildCallTrampBody(parsed *ParsedSig) []byte {
	var body []byte
	// Local decls: 2 locals of type i32 — sp_save, buf_base.
	body = append(body, 0x01)            // 1 local-group
	body = append(body, 0x02, TyI32)     // count=2, type=i32

	sret := parsed.Return.IsAggregate()
	hasRet := parsed.Return.Kind != TyVoid && !sret

	// Pre-push the destination address for the final result store.
	if sret {
		body = append(body, 0x20, lRvalue) // sret: rvalue is first call_indirect arg
	} else if hasRet {
		body = append(body, 0x20, lRvalue) // typed store dest (lives beneath call's result)
	}

	// Push fixed args.
	nfixed := int(parsed.NFixedArgs)
	for i := 0; i < nfixed; i++ {
		body = appendLoadFixedArg(body, parsed.Params[i], i*4)
	}

	// Handle varargs: pack into buffer, push buffer pointer.
	if parsed.Varargs {
		body = appendVarargsPack(body, parsed)
	}

	// Push fn_idx + call_indirect type=1 table=0.
	body = append(body, 0x20, lFn)
	body = append(body, 0x11, 0x01, 0x00)

	// Store result for non-sret returns.
	if hasRet {
		body = appendStoreReturn(body, parsed.Return)
	}

	// Restore shadow stack if we allocated.
	if parsed.Varargs {
		body = append(body, 0x20, lSPSave)
		body = append(body, 0x24, 0x00) // global.set __stack_pointer
	}

	body = append(body, 0x0b)
	return body
}

// appendLoadFixedArg pushes one fixed arg onto the operand stack,
// expanded to its lowered valtypes.
//
// avalue is at lAvalue; the i-th arg's pointer-slot is at
// `avalue + i*4`, which holds a pointer-to-value of the typed
// arg. Lowering:
//
//   - primitive: one typed load
//   - longdouble: two i64 loads (offsets 0 and 8)
//   - struct-flat: one typed load per field
func appendLoadFixedArg(body []byte, p ParsedType, avalueByteOff int) []byte {
	// Helper: emit `local.get avalue; [optionally add offset]; i32.load align=2 offset=0`
	// — leaves the value-pointer on the operand stack.
	pushArgPointer := func(b []byte) []byte {
		b = append(b, 0x20, lAvalue)
		if avalueByteOff != 0 {
			b = append(b, 0x41)
			b = append(b, sleb128(int64(avalueByteOff))...)
			b = append(b, 0x6a)
		}
		b = append(b, 0x28, 0x02, 0x00)
		return b
	}

	switch p.Kind {
	case TyI32, TyI64, TyF32, TyF64:
		body = pushArgPointer(body)
		body = append(body, loadOp(p.Kind), alignFor(p.Kind), 0x00)
	case TyLongDouble:
		body = pushArgPointer(body)
		body = append(body, 0x29, 0x03, 0x00) // i64.load offset=0
		body = pushArgPointer(body)
		body = append(body, 0x29, 0x03, 0x08) // i64.load offset=8
	case TyStructFlat:
		off := 0
		for _, f := range p.FlatFields {
			off = alignUp(off, naturalAlign(f))
			body = pushArgPointer(body)
			body = append(body, loadOp(f.Kind), alignFor(f.Kind), byte(off))
			off += f.ByteSize()
		}
	}
	return body
}

// appendVarargsPack:
//  1. Compute buf_size (sum of variadic arg sizes, 16-byte aligned).
//  2. Save __stack_pointer → lSPSave.
//  3. Subtract buf_size → lBufBase, write back to __stack_pointer.
//  4. For each variadic arg i: store its value at lBufBase + offset.
//  5. Push lBufBase as the extra wasm arg.
//
// The caller pushed rvalue + fixed args already; this only adds the
// extra trailing buffer-pointer arg.
func appendVarargsPack(body []byte, parsed *ParsedSig) []byte {
	nfixed := int(parsed.NFixedArgs)
	variadic := parsed.Params[nfixed:]

	// Compute total buffer size + per-arg offsets up-front so we
	// can emit constants directly.
	type slot struct {
		off  int
		kind byte
		// long-double / struct-flat get expanded; treat each variadic
		// arg as a single "memory blob" we copy into the buffer.
		size int
	}
	slots := make([]slot, len(variadic))
	off := 0
	for i, v := range variadic {
		off = alignUp(off, naturalAlign(v))
		slots[i] = slot{off: off, kind: v.Kind, size: v.ByteSize()}
		off += v.ByteSize()
	}
	bufSize := alignUp(off, 16)

	// Save __stack_pointer.
	body = append(body, 0x23, 0x00)        // global.get __stack_pointer
	body = append(body, 0x22, lSPSave)     // local.tee lSPSave

	// __stack_pointer -= bufSize; store buf base in lBufBase.
	body = append(body, 0x41)
	body = append(body, sleb128(int64(bufSize))...)
	body = append(body, 0x6b)              // i32.sub
	body = append(body, 0x22, lBufBase)    // local.tee lBufBase
	body = append(body, 0x24, 0x00)        // global.set __stack_pointer

	// Pack each variadic arg.
	for i, v := range variadic {
		body = appendPackVararg(body, v, slots[i].off, nfixed+i)
	}

	// Push buffer pointer.
	body = append(body, 0x20, lBufBase)
	return body
}

// appendPackVararg emits: copy *avalue[idx] into buf[off],
// preserving the type's natural alignment. For primitive types,
// it's a single load + store pair. For aggregates, it's a memcpy-
// shape sequence of typed loads/stores.
func appendPackVararg(body []byte, v ParsedType, off, idx int) []byte {
	avalueOff := idx * 4

	// Helper: push (lBufBase + off) — the destination address.
	pushDst := func(b []byte, fieldOff int) []byte {
		b = append(b, 0x20, lBufBase)
		if off+fieldOff != 0 {
			b = append(b, 0x41)
			b = append(b, sleb128(int64(off+fieldOff))...)
			b = append(b, 0x6a)
		}
		return b
	}
	// Helper: push the value pointer (avalue + avalueOff deref).
	pushSrcPtr := func(b []byte) []byte {
		b = append(b, 0x20, lAvalue)
		if avalueOff != 0 {
			b = append(b, 0x41)
			b = append(b, sleb128(int64(avalueOff))...)
			b = append(b, 0x6a)
		}
		b = append(b, 0x28, 0x02, 0x00) // i32.load
		return b
	}

	switch v.Kind {
	case TyI32, TyI64, TyF32, TyF64:
		body = pushDst(body, 0)
		body = pushSrcPtr(body)
		body = append(body, loadOp(v.Kind), alignFor(v.Kind), 0x00) // load src
		body = append(body, storeOp(v.Kind), alignFor(v.Kind), 0x00) // store dst
	case TyLongDouble:
		// Two i64s.
		for hi := 0; hi < 2; hi++ {
			body = pushDst(body, hi*8)
			body = pushSrcPtr(body)
			body = append(body, 0x29, 0x03, byte(hi*8)) // i64.load offset
			body = append(body, 0x37, 0x03, 0x00)       // i64.store
		}
	case TyStructFlat:
		fieldOff := 0
		for _, f := range v.FlatFields {
			fieldOff = alignUp(fieldOff, naturalAlign(f))
			body = pushDst(body, fieldOff)
			body = pushSrcPtr(body)
			body = append(body, loadOp(f.Kind), alignFor(f.Kind), byte(fieldOff))
			body = append(body, storeOp(f.Kind), alignFor(f.Kind), 0x00)
			fieldOff += f.ByteSize()
		}
	}
	return body
}

// appendStoreReturn stores the call_indirect's result value at
// rvalue (which is sitting beneath the result on the operand stack
// because we pushed it pre-call). Only used for non-sret, non-void
// returns. Long-double + struct-flat returns are sret, so they
// never hit this path.
func appendStoreReturn(body []byte, rt ParsedType) []byte {
	switch rt.Kind {
	case TyI32, TyI64, TyF32, TyF64:
		return append(body, storeOp(rt.Kind), alignFor(rt.Kind), 0x00)
	}
	// Should never happen: aggregate returns go through sret.
	return body
}

// loadOp returns the wasm load opcode for a valtype.
func loadOp(t byte) byte {
	switch t {
	case TyI32:
		return 0x28
	case TyI64:
		return 0x29
	case TyF32:
		return 0x2a
	case TyF64:
		return 0x2b
	}
	return 0
}

// storeOp returns the wasm store opcode for a valtype.
func storeOp(t byte) byte {
	switch t {
	case TyI32:
		return 0x36
	case TyI64:
		return 0x37
	case TyF32:
		return 0x38
	case TyF64:
		return 0x39
	}
	return 0
}

// alignFor returns the alignment-log2 immediate.
func alignFor(t byte) byte {
	switch t {
	case TyI32, TyF32:
		return 0x02
	case TyI64, TyF64:
		return 0x03
	}
	return 0
}

// naturalAlign returns the C natural alignment in bytes.
func naturalAlign(p ParsedType) int {
	switch p.Kind {
	case TyI32, TyF32:
		return 4
	case TyI64, TyF64, TyLongDouble:
		return 8
	case TyStructFlat:
		max := 1
		for _, f := range p.FlatFields {
			if a := naturalAlign(f); a > max {
				max = a
			}
		}
		return max
	}
	return 1
}

// alignUp rounds n up to the next multiple of align.
func alignUp(n, align int) int {
	return (n + align - 1) &^ (align - 1)
}

// appendFuncType writes a wasm functype (params + results).
func appendFuncType(out []byte, params, results []byte) []byte {
	out = append(out, 0x60)
	out = append(out, uleb128(uint32(len(params)))...)
	out = append(out, params...)
	out = append(out, uleb128(uint32(len(results)))...)
	out = append(out, results...)
	return out
}
