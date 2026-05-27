## __wasi_libffi_dispatch — wasm-assembly helper that does
## `call_indirect __indirect_function_table, $marshaler` against a
## runtime-supplied trampoline slot. Hand-written wasm because
## clang's funcref type system can't express the (variable-index,
## fixed-signature) shape — but a .s file with the literal opcodes
## can.
##
## Signature: (fn_idx: i32, rvalue: i32, avalue: i32,
##             trampoline_idx: i32) -> ()
##
## fn_idx       — the target function's table index (what
##                 the user's ffi_cif → fn parameter encodes)
## rvalue       — pointer to caller-allocated return-value slot
## avalue       — pointer to array of arg-value pointers
## trampoline_idx — slot returned by libffi.prep-trampoline; holds
##                  a wasm function whose body unpacks avalue[],
##                  call_indirects fn_idx with the right target
##                  type, and stores result at *rvalue.

	.file	"libffi_dispatch.s"
	.functype	__wasi_libffi_dispatch (i32, i32, i32, i32) -> ()
	.export_name	__wasi_libffi_dispatch, __wasi_libffi_dispatch
	.section	.text.__wasi_libffi_dispatch,"R",@
	.hidden	__wasi_libffi_dispatch
	.globl	__wasi_libffi_dispatch
	.type	__wasi_libffi_dispatch,@function
__wasi_libffi_dispatch:
	.functype	__wasi_libffi_dispatch (i32, i32, i32, i32) -> ()
	# Push the three args the trampoline's signature expects, then
	# call_indirect at the trampoline's table slot. The marshaler
	# signature is `(i32, i32, i32) -> ()` — fixed across all
	# generated trampolines; only the *body* of the trampoline
	# varies per target signature.
	# Push the marshaler's three args, then the trampoline's
	# table index. call_indirect type is fixed `(i32, i32, i32) -> ()`
	# across all generated trampolines.
	local.get	0   # fn_idx       (1st marshaler arg)
	local.get	1   # rvalue       (2nd)
	local.get	2   # avalue       (3rd)
	local.get	3   # trampoline_idx (selector for call_indirect)
	call_indirect	__indirect_function_table, (i32, i32, i32) -> ()
	end_function
	.no_dead_strip	__wasi_libffi_dispatch

	.tabletype	__indirect_function_table, funcref
	.no_dead_strip	__indirect_function_table
