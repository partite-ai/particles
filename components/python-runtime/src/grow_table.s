## __grow_table — wasm-assembly wrapper around `table.grow` on
## __indirect_function_table. Exported so the dyld host can reserve
## table slots for dlopen'd .so's; returns the previous table size
## (the new entries' base index). Written in wasm-asm because clang's
## funcref/externref C type system can't name our funcref table.

	.file	"grow_table.s"
	.functype	__grow_table (i32) -> (i32)
	.export_name	__grow_table, __grow_table
	.section	.text.__grow_table,"R",@
	.hidden	__grow_table
	.globl	__grow_table
	.type	__grow_table,@function
__grow_table:
	.functype	__grow_table (i32) -> (i32)
	ref.null_func
	local.get	0
	table.grow	__indirect_function_table
	end_function
	.no_dead_strip	__grow_table

	.tabletype	__indirect_function_table, funcref
	.no_dead_strip	__indirect_function_table
