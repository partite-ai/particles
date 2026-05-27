//! libffi-wasi-bridge — side module (libffi.so in the composed
//! python-runtime) supplying `__wasi_libffi_*` C entry points.
//!
//! The libffi.a shell (built as part of the cffi wheel in the
//! particle wheels repo, https://github.com/partite-ai/particle-python-wheels)
//! declares `extern int __wasi_libffi_*` and leaves the definitions for this
//! crate to provide. cffi-built .so files import the same names
//! from `env`; dyld's env shim resolves them against this library
//! at .so-load time the same way it resolves libc.so symbols.
//!
//! Why a separate side library instead of folding into main.wasm:
//! inject-capture's MAIN_KEPT_EXPORTS is curated on purpose — main
//! has thousands of internal Rust + wit-bindgen symbols we don't
//! want to surface to every dlopened .so. libffi.so participates in
//! the standard dl-openable composition flow, gets its own entry
//! in DL_OPENABLE_LIBRARY_NAMES, and only its public surface is
//! visible to the env shim.
//!
//! Two host calls back the runtime (defined by particle:host/libffi):
//!
//!   prep-trampoline(sig) -> u32
//!       Allocates a CALL trampoline (libffi → C) at a table slot;
//!       cached per sig descriptor.
//!
//!   prep-closure(closure_ptr, slot, sig) -> result<_, error>
//!       Builds a CLOSURE trampoline (C → user callback) at the
//!       pre-allocated slot. closure_ptr is the closure struct
//!       address; the trampoline reads cif/fun/user_data out of
//!       offsets 4/8/12 at call time.
//!
//! Closure layout (upstream libffi convention for wasm32):
//!
//!   offset 0..4    ftramp     (the table slot — we store it here)
//!   offset 4..8    cif*
//!   offset 8..12   fun*       (user callback's table index)
//!   offset 12..16  user_data
//!
//! Sig descriptor v2 (matches internal/runtime/libffi/sig.go):
//!
//!   byte 0    flags (bit 0 = varargs)
//!   byte 1    nfixedargs
//!   byte 2    return-type byte (0 = void, else valtype/aggregate tag)
//!   byte 3    nparams
//!   ...       param-type encodings, variable length each
//!
//! Type tags:
//!   0x7f i32, 0x7e i64, 0x7d f32, 0x7c f64   wasm valtypes
//!   0x10  longdouble (expands to 2× i64 at the wasm level)
//!   0x12  struct-flat:  followed by N + N field bytes (each a
//!         primitive valtype, no nesting)
//!
//! ABI lowering rules baked into encode_type():
//!   - FFI_TYPE_VOID/{U,S}INT8/16/32/INT/POINTER → i32
//!   - FFI_TYPE_{U,S}INT64 → i64
//!   - FFI_TYPE_FLOAT → f32
//!   - FFI_TYPE_DOUBLE → f64
//!   - FFI_TYPE_LONGDOUBLE → 0x10 (16-byte; lowered to 2× i64)
//!   - FFI_TYPE_STRUCT:
//!       Total size > 16:                    encode as i32 (by-pointer)
//!       Single field ≤ 16:                  unbox to that field's tag
//!       Multi-field ≤ 16, all primitive:    encode as struct-flat
//!       Anything else (nested aggregates):  by-pointer fallback

#![no_std]

extern crate alloc;

use alloc::vec::Vec;
use core::ffi::{c_int, c_uint, c_void};

// wit-bindgen generates the host stubs for particle:host/libffi.
// `generate_all` rolls every interface in the imported world into
// scope. Only one is referenced (libffi); the rest are pruned at
// link time.
wit_bindgen::generate!({
    world: "libffi-bridge",
    path: "wit",
    generate_all,
});

// Forward-declare libc allocator symbols for the closure struct's
// malloc / free. The composed component wires these to libc.so.
extern "C" {
    fn malloc(size: usize) -> *mut c_void;
    fn free(ptr: *mut c_void);
}

// cabi_realloc — wit-component requires every dl-openable module
// that exchanges list/string types with the host to export this.
// We exchange `list<u8>` via prep-trampoline / prep-closure, so the
// host allocates space via this function when copying our sig
// descriptors into our memory region.
#[no_mangle]
pub unsafe extern "C" fn cabi_realloc(
    old_ptr: *mut u8,
    _old_len: usize,
    _align: usize,
    new_len: usize,
) -> *mut u8 {
    if new_len == 0 {
        if !old_ptr.is_null() {
            free(old_ptr as *mut c_void);
        }
        return core::ptr::null_mut();
    }
    extern "C" {
        fn realloc(ptr: *mut u8, size: usize) -> *mut u8;
    }
    realloc(old_ptr, new_len)
}

// no_std builds need a global allocator that proxies to libc.
struct LibcAlloc;

unsafe impl core::alloc::GlobalAlloc for LibcAlloc {
    unsafe fn alloc(&self, layout: core::alloc::Layout) -> *mut u8 {
        if layout.align() <= core::mem::size_of::<usize>() * 2 {
            malloc(layout.size()) as *mut u8
        } else {
            extern "C" {
                fn aligned_alloc(alignment: usize, size: usize) -> *mut c_void;
            }
            let size = (layout.size() + layout.align() - 1) & !(layout.align() - 1);
            aligned_alloc(layout.align(), size) as *mut u8
        }
    }
    unsafe fn dealloc(&self, ptr: *mut u8, _layout: core::alloc::Layout) {
        free(ptr as *mut c_void);
    }
}

#[global_allocator]
static ALLOCATOR: LibcAlloc = LibcAlloc;

// no_std requires a panic handler. Panic = abort: same policy as
// dyld-libdl, keeps the .so small.
#[panic_handler]
fn panic(_info: &core::panic::PanicInfo) -> ! {
    core::arch::wasm32::unreachable()
}

#[repr(C)]
pub struct FfiType {
    pub size: usize,                  // 0
    pub alignment: u16,               // 4
    pub _pad: u16,                    // 6
    pub type_: u16,                   // 8
    pub _pad2: u16,                   // 10
    pub elements: *mut *mut FfiType,  // 12  (null-terminated for FFI_TYPE_STRUCT)
}

#[repr(C)]
pub struct FfiCif {
    pub abi: c_uint,
    pub nargs: c_uint,
    pub arg_types: *mut *mut FfiType,
    pub rtype: *mut FfiType,
    pub bytes: c_uint,
    pub flags: c_uint,
    pub nfixedargs: c_uint,
}

// FFI_TYPE_* — keep in lock-step with libffi's include/ffi.h in the
// particle wheels repo (https://github.com/partite-ai/particle-python-wheels).
const FFI_TYPE_VOID: u16 = 0;
const FFI_TYPE_INT: u16 = 1;
const FFI_TYPE_FLOAT: u16 = 2;
const FFI_TYPE_DOUBLE: u16 = 3;
const FFI_TYPE_LONGDOUBLE: u16 = 4;
const FFI_TYPE_UINT8: u16 = 5;
const FFI_TYPE_SINT8: u16 = 6;
const FFI_TYPE_UINT16: u16 = 7;
const FFI_TYPE_SINT16: u16 = 8;
const FFI_TYPE_UINT32: u16 = 9;
const FFI_TYPE_SINT32: u16 = 10;
const FFI_TYPE_UINT64: u16 = 11;
const FFI_TYPE_SINT64: u16 = 12;
const FFI_TYPE_STRUCT: u16 = 13;
const FFI_TYPE_POINTER: u16 = 14;

const FFI_OK: c_int = 0;
const FFI_BAD_TYPEDEF: c_int = 1;

// Tag bytes — must match internal/runtime/libffi/sig.go.
const TY_VOID: u8 = 0x00;
const TY_I32: u8 = 0x7f;
const TY_I64: u8 = 0x7e;
const TY_F32: u8 = 0x7d;
const TY_F64: u8 = 0x7c;
const TY_LONG_DOUBLE: u8 = 0x10;
const TY_STRUCT_FLAT: u8 = 0x12;

const HEADER_VARARGS: u8 = 0x01;

// Closure struct field offsets (upstream libffi wasm32 layout).
const CLOSURE_OFF_FTRAMP: usize = 0;
const CLOSURE_OFF_CIF: usize = 4;
const CLOSURE_OFF_FUN: usize = 8;
const CLOSURE_OFF_USER_DATA: usize = 12;

extern "C" {
    fn __wasi_libffi_dispatch(fn_idx: u32, rvalue: u32, avalue: u32, trampoline_slot: u32);

    // wasm-asm helper (libffi_dispatch.s) that performs the table-grow
    // step for closure_alloc. Returns the new table size before
    // growing (i.e. the index of the first new slot). We use it the
    // same way the dyld loader does for shared-table allocation.
    fn __grow_table(delta: u32) -> u32;
}

// Generated WIT bindings.
use crate::particle::host::libffi as host_libffi;

// Drop the duplicate libc declarations that the standalone form
// elsewhere in the file had; the top-level externs above cover
// malloc/free for the closure path.

// --- sig encoding --------------------------------------------------------

/// unbox_small_structs: for a struct type ≤ 16 bytes with a single
/// non-empty field, return that field's ffi_type instead. Mirrors
/// the algorithm in libffi-emscripten's ffi.c::unbox_small_structs
/// — single-element structs get flattened at the wasm32 ABI level.
unsafe fn unbox_single_struct(t: *mut FfiType) -> *mut FfiType {
    let mut cur = t;
    while !cur.is_null() && (*cur).type_ == FFI_TYPE_STRUCT {
        if (*cur).size > 16 {
            return cur;
        }
        let elements = (*cur).elements;
        if elements.is_null() {
            return cur;
        }
        let first = *elements;
        if first.is_null() {
            // Empty struct — treat as void.
            return core::ptr::null_mut();
        }
        let second_ptr = elements.add(1);
        let second = *second_ptr;
        if !second.is_null() {
            return cur; // multi-field struct — leave for struct-flat handling
        }
        cur = first;
    }
    cur
}

/// Encode one ffi_type into the sig descriptor's variable-length
/// type encoding. Returns OK on success; pushes one or more bytes to
/// `out`. Returns Err if the type is unrecognized.
unsafe fn encode_type(t: *mut FfiType, out: &mut Vec<u8>) -> Result<(), &'static str> {
    if t.is_null() {
        out.push(TY_VOID);
        return Ok(());
    }
    // Step 1: unbox single-field small structs.
    let t = unbox_single_struct(t);
    if t.is_null() {
        out.push(TY_VOID);
        return Ok(());
    }
    let ty = (*t).type_;

    // Step 2: primitive cases.
    match ty {
        FFI_TYPE_VOID => {
            out.push(TY_VOID);
            return Ok(());
        }
        FFI_TYPE_INT | FFI_TYPE_UINT8 | FFI_TYPE_SINT8 | FFI_TYPE_UINT16 | FFI_TYPE_SINT16
        | FFI_TYPE_UINT32 | FFI_TYPE_SINT32 | FFI_TYPE_POINTER => {
            out.push(TY_I32);
            return Ok(());
        }
        FFI_TYPE_UINT64 | FFI_TYPE_SINT64 => {
            out.push(TY_I64);
            return Ok(());
        }
        FFI_TYPE_FLOAT => {
            out.push(TY_F32);
            return Ok(());
        }
        FFI_TYPE_DOUBLE => {
            out.push(TY_F64);
            return Ok(());
        }
        FFI_TYPE_LONGDOUBLE => {
            out.push(TY_LONG_DOUBLE);
            return Ok(());
        }
        FFI_TYPE_STRUCT => {} // fall through
        _ => return Err("unrecognized ffi_type tag"),
    }

    // Step 3: struct. Size > 16 ⇒ by pointer. Otherwise flat with
    // each primitive field.
    if (*t).size > 16 {
        out.push(TY_I32);
        return Ok(());
    }

    // Walk the elements list to discover the flat fields.
    let mut fields: Vec<u8> = Vec::new();
    let mut elem_ptr = (*t).elements;
    if elem_ptr.is_null() {
        // Empty struct — treat as 0-field flat (the trampoline will
        // load nothing for it).
        out.push(TY_STRUCT_FLAT);
        out.push(0);
        return Ok(());
    }
    loop {
        let field = *elem_ptr;
        if field.is_null() {
            break;
        }
        // Flat structs can't contain aggregates — encode each field
        // as a primitive. Nested aggregates fall through to TY_I32
        // (treat as pointer-sized opaque); the user code is
        // responsible for passing pointers in that case.
        let field_ty = (*field).type_;
        let tag = match field_ty {
            FFI_TYPE_INT | FFI_TYPE_UINT8 | FFI_TYPE_SINT8 | FFI_TYPE_UINT16 | FFI_TYPE_SINT16
            | FFI_TYPE_UINT32 | FFI_TYPE_SINT32 | FFI_TYPE_POINTER => TY_I32,
            FFI_TYPE_UINT64 | FFI_TYPE_SINT64 => TY_I64,
            FFI_TYPE_FLOAT => TY_F32,
            FFI_TYPE_DOUBLE => TY_F64,
            _ => return Err("struct-flat: nested aggregate or unknown field type"),
        };
        fields.push(tag);
        elem_ptr = elem_ptr.add(1);
    }
    out.push(TY_STRUCT_FLAT);
    out.push(fields.len() as u8);
    out.extend_from_slice(&fields);
    Ok(())
}

// Build the v2 sig descriptor from a cif. Walks rtype + arg_types.
unsafe fn build_sig(cif: &FfiCif) -> Result<Vec<u8>, &'static str> {
    let nargs = cif.nargs as usize;
    let nfixed = cif.nfixedargs as usize;
    let varargs_flag = if cif.flags & 1 != 0 { HEADER_VARARGS } else { 0 };

    let mut out: Vec<u8> = Vec::new();
    out.push(varargs_flag);
    out.push(nfixed as u8);

    encode_type(cif.rtype, &mut out)?;
    out.push(nargs as u8);

    for i in 0..nargs {
        let arg_ty = *cif.arg_types.add(i);
        encode_type(arg_ty, &mut out)?;
    }
    Ok(out)
}

// --- call path -----------------------------------------------------------

#[no_mangle]
pub unsafe extern "C" fn __wasi_libffi_call(
    cif: *mut FfiCif,
    fn_ptr: *mut c_void,
    rvalue: *mut c_void,
    avalue: *mut *mut c_void,
) -> c_int {
    if cif.is_null() {
        return FFI_BAD_TYPEDEF;
    }
    let sig = match build_sig(&*cif) {
        Ok(s) => s,
        Err(_) => return FFI_BAD_TYPEDEF,
    };
    let slot = match host_libffi::prep_trampoline(&sig) {
        Ok(s) => s,
        Err(_) => return FFI_BAD_TYPEDEF,
    };
    __wasi_libffi_dispatch(fn_ptr as u32, rvalue as u32, avalue as u32, slot);
    FFI_OK
}

// --- closure path --------------------------------------------------------

/// ffi_closure_alloc(size, code): allocate the closure struct + a
/// table slot. The slot is also written into closure->ftramp so
/// ffi_closure_free can reuse it (today: we just leak the slot, as
/// closures live for the lifetime of the process — same behavior
/// as our trampoline cache).
#[no_mangle]
pub unsafe extern "C" fn __wasi_libffi_closure_alloc(
    size: usize,
    code: *mut *mut c_void,
) -> *mut c_void {
    if size < 16 || code.is_null() {
        return core::ptr::null_mut();
    }
    let closure = malloc(size);
    if closure.is_null() {
        return core::ptr::null_mut();
    }
    // Zero the closure (libffi convention).
    core::ptr::write_bytes(closure as *mut u8, 0, size);

    // Allocate one table slot. We don't materialize the trampoline
    // here — that happens in ffi_prep_closure_loc when the cif is
    // available.
    let slot = __grow_table(1);
    // Stash the slot in closure->ftramp; ffi_prep_closure_loc reads
    // it back as codeloc.
    *(closure.add(CLOSURE_OFF_FTRAMP) as *mut u32) = slot;
    *code = slot as usize as *mut c_void;
    closure
}

#[no_mangle]
pub unsafe extern "C" fn __wasi_libffi_closure_free(closure: *mut c_void) {
    // We don't recycle slots in this build — table.grow only grows,
    // and freeing a closure's slot would require a free-list we
    // haven't built. The underlying memory does get freed.
    if closure.is_null() {
        return;
    }
    free(closure);
}

/// ffi_prep_closure_loc(closure, cif, fun, user_data, codeloc):
/// fill in the closure struct and materialize the wasm trampoline.
#[no_mangle]
pub unsafe extern "C" fn __wasi_libffi_prep_closure_loc(
    closure: *mut c_void,
    cif: *mut FfiCif,
    fun: *mut c_void,
    user_data: *mut c_void,
    codeloc: *mut c_void,
) -> c_int {
    if closure.is_null() || cif.is_null() || fun.is_null() {
        return FFI_BAD_TYPEDEF;
    }
    let slot = codeloc as usize as u32;
    if slot == 0 {
        return FFI_BAD_TYPEDEF;
    }
    // Write the user-supplied fields. ftramp was already populated
    // by closure_alloc; we re-write it for paranoia.
    *(closure.add(CLOSURE_OFF_FTRAMP) as *mut u32) = slot;
    *(closure.add(CLOSURE_OFF_CIF) as *mut u32) = cif as u32;
    *(closure.add(CLOSURE_OFF_FUN) as *mut u32) = fun as u32;
    *(closure.add(CLOSURE_OFF_USER_DATA) as *mut u32) = user_data as u32;

    let sig = match build_sig(&*cif) {
        Ok(s) => s,
        Err(_) => return FFI_BAD_TYPEDEF,
    };
    match host_libffi::prep_closure(closure as u32, slot, &sig) {
        Ok(()) => FFI_OK,
        Err(_) => FFI_BAD_TYPEDEF,
    }
}
