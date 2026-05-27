// tramp_closure.go — emits the wasm bytes for a closure trampoline.
//
// A libffi closure is the inverse of ffi_call: C code dispatches
// through `call_indirect <closure->ftramp>` with the target's
// signature; the trampoline marshals those wasm args back into a
// libffi avalue[] format, calls `closure->fun(cif, rvalue, avalue,
// user_data)`, and returns the result.
//
// Layout of the closure struct (matching upstream libffi for wasm32):
//   offset 0..4   ftramp (we store the table slot here for sanity)
//   offset 4..8   cif    (*ffi_cif)
//   offset 8..12  fun    (void (*)(ffi_cif*, void*, void**, void*))
//   offset 12..16 user_data
//
// At trampoline generation time we already know the closure pointer
// (the libffi consumer just allocated it). We bake that pointer in
// as an i32.const so the trampoline can read its fields directly
// without needing to be told who it is at call time.
//
// At runtime the trampoline:
//   1. Allocates a shadow-stack frame holding:
//        - one slot per wasm-stack arg (sized per ParsedType)
//        - the avalue[] array of N pointers, one per LIBFFI arg
//        - a return-value slot
//   2. Stores each incoming wasm arg into its scratch slot.
//   3. Builds avalue[] by pointing each entry at the appropriate
//      scratch slot.
//   4. Calls closure->fun(closure->cif, rvalue_ptr, avalue_ptr,
//                          closure->user_data) via call_indirect
//      against closure->fun.
//   5. Loads the return value from the rvalue slot, restores the
//      shadow stack, and returns.
//
// Aggregate-return closures use the sret convention: the wasm
// signature has no result and the first wasm arg is the rvalue
// pointer. In that case step 1 doesn't allocate a return slot and
// step 5 returns nothing.
package libffi

// Local-index conventions for the closure trampoline.
const (
	cFirstLocal byte = 0 // first declared local — sp_save
	// Following the declared locals, we use the params (variable count)
	// directly via local.get N where N starts at <number_of_locals>.
	// We compute these at runtime in the builder.
)

// buildClosureTrampoline assembles a wasm module whose elem segment
// places a closure trampoline at the given table slot. The
// trampoline reads cif/fun/user_data from the baked-in closure
// pointer at runtime and dispatches.
//
// The wasm-level signature of the trampoline IS the target sig
// (what the C caller sees), not the marshaler sig — different from
// the call-direction trampoline.
func buildClosureTrampoline(parsed *ParsedSig, slot uint32, closurePtr uint32) ([]byte, error) {
	// The trampoline's wasm sig: same lowering as a normal call
	// would have, since the C caller is going to call_indirect us
	// with the target sig.
	params, results := loweredTargetSig(parsed)

	// The closure->fun callback has fixed C signature:
	//   void fun(ffi_cif *cif, void *rvalue, void **avalue, void *user_data)
	// which lowers to wasm `(i32, i32, i32, i32) -> ()`. We need a
	// type for this in the module's type section to call_indirect
	// closure->fun.
	var out []byte
	out = append(out, 0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00)

	var typeSec []byte
	typeSec = append(typeSec, 0x02) // 2 types
	// type 0: trampoline's own sig — what the C caller sees.
	typeSec = appendFuncType(typeSec, params, results)
	// type 1: closure->fun sig.
	typeSec = appendFuncType(typeSec, []byte{TyI32, TyI32, TyI32, TyI32}, nil)
	out = appendSection(out, 0x01, typeSec)

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

	out = appendSection(out, 0x03, []byte{0x01, 0x00})

	var elemSec []byte
	elemSec = append(elemSec, 0x01)
	elemSec = append(elemSec, 0x00)
	elemSec = append(elemSec, 0x41)
	elemSec = append(elemSec, sleb128(int64(slot))...)
	elemSec = append(elemSec, 0x0b)
	elemSec = append(elemSec, 0x01, 0x00)
	out = appendSection(out, 0x09, elemSec)

	body := buildClosureTrampBody(parsed, closurePtr)
	var codeSec []byte
	codeSec = append(codeSec, 0x01)
	codeSec = append(codeSec, uleb128(uint32(len(body)))...)
	codeSec = append(codeSec, body...)
	out = appendSection(out, 0x0a, codeSec)
	return out, nil
}

// buildClosureTrampBody builds the body of a closure trampoline.
//
// Stack frame layout (in shadow-stack memory, all sizes byte-counts):
//   [args_storage]   each fixed arg gets a slot, sized to its byte width
//   [avalue_array]   N pointers (one per fixed arg), N*4 bytes
//   [retval_slot]    sized to the return type (or 0 if sret/void)
//
// Total frame size is rounded up to 16-byte alignment.
//
// Layout offsets are pre-computed at emit time (closure trampolines
// are signature-static), embedded as i32.const literals in the body.
//
// We declare ONE local: lSPSave_closure (saved __stack_pointer).
// The trampoline's wasm params are at local indices 0, 1, 2, ...
// with their count = len(params). The local declaration we add
// occupies the slot AFTER all params (Wasm rule: param locals first,
// then declared locals).
func buildClosureTrampBody(parsed *ParsedSig, closurePtr uint32) []byte {
	// Lowered wasm params, in the same order the C caller pushes them.
	loweredParams, _ := loweredTargetSig(parsed)
	sret := parsed.Return.IsAggregate()

	// Compute storage offsets within the shadow-stack frame.
	// We need:
	//   - one scratch slot per FIXED arg (not per wasm param —
	//     longdouble/struct-flat occupy one scratch slot of byte_size each)
	//   - the avalue[] array (nfixed_args entries × 4 bytes)
	//   - retval slot for non-sret/non-void
	nfixed := int(parsed.NFixedArgs)
	if parsed.Varargs {
		// Closure-side variadic support requires the trampoline to
		// know how to decode the vararg-buffer pointer back into
		// individual avalue entries. We don't currently support it
		// — fail at codegen time rather than emit broken code.
		return emitTrap("libffi-wasi closure: varargs not supported")
	}
	args := parsed.Params[:nfixed]

	argOffsets := make([]int, len(args))
	off := 0
	for i, p := range args {
		off = alignUp(off, naturalAlign(p))
		argOffsets[i] = off
		off += p.ByteSize()
	}
	off = alignUp(off, 4)
	avalueArrayOff := off
	off += 4 * len(args)
	retvalOff := -1
	if !sret && parsed.Return.Kind != TyVoid {
		off = alignUp(off, naturalAlign(parsed.Return))
		retvalOff = off
		off += parsed.Return.ByteSize()
	}
	frameSize := alignUp(off, 16)

	// The first local index AFTER the wasm params is what we use
	// for our shadow-stack save. Wasm locals come right after
	// params in the local index space.
	nparams := len(loweredParams)
	lSPSave := byte(nparams)

	// Map each fixed-arg to its starting wasm-param index. Longdouble
	// and struct-flat each consume multiple wasm params.
	paramStartIdx := make([]int, len(args))
	wasmParamCursor := 0
	if sret {
		wasmParamCursor = 1 // sret rvalue is wasm param 0
	}
	for i, p := range args {
		paramStartIdx[i] = wasmParamCursor
		wasmParamCursor += len(p.LoweredValtypes())
	}

	var body []byte
	// Local decls: 1 i32 (sp_save).
	body = append(body, 0x01)
	body = append(body, 0x01, TyI32)

	// Allocate shadow frame: sp_save = sp; sp -= frameSize.
	body = append(body, 0x23, 0x00) // global.get __stack_pointer
	body = append(body, 0x22, lSPSave)
	body = append(body, 0x41)
	body = append(body, sleb128(int64(frameSize))...)
	body = append(body, 0x6b)
	body = append(body, 0x24, 0x00) // global.set __stack_pointer

	// frame_base = sp_save - frameSize. We don't store frame_base in
	// a local; we recompute as `sp_save - frameSize` each time we
	// need an absolute pointer into the frame. Cheap and avoids
	// adding another local.
	pushFrameBase := func(b []byte) []byte {
		b = append(b, 0x20, lSPSave)
		b = append(b, 0x41)
		b = append(b, sleb128(int64(frameSize))...)
		b = append(b, 0x6b)
		return b
	}

	// Step 1: stash each wasm-param into the frame's arg-slot
	// region. Each ParsedType pulls len(LoweredValtypes()) wasm
	// params in order.
	for i, p := range args {
		switch p.Kind {
		case TyI32, TyI64, TyF32, TyF64:
			// dest = frame_base + argOffsets[i]
			body = pushFrameBase(body)
			body = append(body, 0x41)
			body = append(body, sleb128(int64(argOffsets[i]))...)
			body = append(body, 0x6a)
			// value = local.get paramStartIdx[i]
			body = append(body, 0x20, byte(paramStartIdx[i]))
			body = append(body, storeOp(p.Kind), alignFor(p.Kind), 0x00)
		case TyLongDouble:
			// Two i64 slots, each 8 bytes apart.
			for w := 0; w < 2; w++ {
				body = pushFrameBase(body)
				body = append(body, 0x41)
				body = append(body, sleb128(int64(argOffsets[i]+w*8))...)
				body = append(body, 0x6a)
				body = append(body, 0x20, byte(paramStartIdx[i]+w))
				body = append(body, 0x37, 0x03, 0x00) // i64.store
			}
		case TyStructFlat:
			fieldOff := 0
			wasmIdx := paramStartIdx[i]
			for _, f := range p.FlatFields {
				fieldOff = alignUp(fieldOff, naturalAlign(f))
				body = pushFrameBase(body)
				body = append(body, 0x41)
				body = append(body, sleb128(int64(argOffsets[i]+fieldOff))...)
				body = append(body, 0x6a)
				body = append(body, 0x20, byte(wasmIdx))
				body = append(body, storeOp(f.Kind), alignFor(f.Kind), 0x00)
				fieldOff += f.ByteSize()
				wasmIdx++
			}
		}
	}

	// Step 2: build avalue[]: avalue[i] = frame_base + argOffsets[i].
	for i := range args {
		body = pushFrameBase(body)
		body = append(body, 0x41)
		body = append(body, sleb128(int64(avalueArrayOff+i*4))...)
		body = append(body, 0x6a)
		body = pushFrameBase(body)
		body = append(body, 0x41)
		body = append(body, sleb128(int64(argOffsets[i]))...)
		body = append(body, 0x6a)
		body = append(body, 0x36, 0x02, 0x00) // i32.store
	}

	// Step 3: call closure->fun(cif, rvalue, avalue, user_data).
	// Read cif from closure+4, fun from +8, user_data from +12.
	// rvalue is the sret param if aggregate-return, else frame_base+retvalOff.
	//
	// Args order for call_indirect:
	//   cif, rvalue, avalue, user_data, then fun_idx for the indirect call.

	// cif = *(closurePtr + 4)
	body = append(body, 0x41)
	body = append(body, sleb128(int64(closurePtr+4))...)
	body = append(body, 0x28, 0x02, 0x00) // i32.load

	// rvalue
	if sret {
		// First wasm param is the sret pointer.
		body = append(body, 0x20, 0x00)
	} else if parsed.Return.Kind == TyVoid {
		// void return — pass NULL.
		body = append(body, 0x41, 0x00)
	} else {
		body = pushFrameBase(body)
		body = append(body, 0x41)
		body = append(body, sleb128(int64(retvalOff))...)
		body = append(body, 0x6a)
	}

	// avalue = frame_base + avalueArrayOff
	body = pushFrameBase(body)
	body = append(body, 0x41)
	body = append(body, sleb128(int64(avalueArrayOff))...)
	body = append(body, 0x6a)

	// user_data = *(closurePtr + 12)
	body = append(body, 0x41)
	body = append(body, sleb128(int64(closurePtr+12))...)
	body = append(body, 0x28, 0x02, 0x00)

	// fun table index = *(closurePtr + 8)
	body = append(body, 0x41)
	body = append(body, sleb128(int64(closurePtr+8))...)
	body = append(body, 0x28, 0x02, 0x00)

	// call_indirect type=1 (closure->fun's sig) table=0
	body = append(body, 0x11, 0x01, 0x00)

	// Step 4: load return value (non-sret, non-void).
	if !sret && parsed.Return.Kind != TyVoid {
		body = pushFrameBase(body)
		body = append(body, 0x41)
		body = append(body, sleb128(int64(retvalOff))...)
		body = append(body, 0x6a)
		body = append(body, loadOp(parsed.Return.Kind), alignFor(parsed.Return.Kind), 0x00)
	}

	// Step 5: restore __stack_pointer.
	body = append(body, 0x20, lSPSave)
	body = append(body, 0x24, 0x00)

	body = append(body, 0x0b)
	return body
}

// emitTrap returns a wasm function body that immediately executes
// `unreachable` — surfaces a clear runtime error if a code path we
// don't support gets invoked. Used as a fallback for combos we
// haven't implemented yet (e.g. closure varargs).
func emitTrap(_ string) []byte {
	// 0 locals; unreachable; end.
	// (We can't easily emit the message string from within wasm;
	// the trap goes through wazero's stack trace machinery.)
	return []byte{0x00, 0x00, 0x0b}
}
