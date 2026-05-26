//! libdl.so replacement — supplies `dlopen` / `dlsym` / `dlclose` /
//! `dlerror` implementations that route through the canonical WIT
//! `particle:host/dyld@0.1.0` interface, plus `__grow_table` (a
//! wasm-asm helper). Built as a `--shared` side module composed
//! alongside libc.so + the wasi-emulated- libs,
//! replacing wasi-sdk's stock libdl.so in the composition.
//!
//! `#![no_std]` to keep the .so small — we'd otherwise transitively
//! pull in panic infra + io::Display + format machinery + an
//! allocator the size of a small city. wasm is single-threaded, so
//! plain `static mut` (UnsafeCell underneath) suffices in place of
//! `thread_local!`. The `alloc` crate gives us `Vec` and `String`
//! without the std-only bits.

#![no_std]

extern crate alloc;

use alloc::vec::Vec;
use core::alloc::{GlobalAlloc, Layout};
use core::cell::UnsafeCell;
use core::ffi::{c_char, c_int, c_void, CStr};
use core::ptr;

// Forward to libc.so's allocator. Imports become env.{malloc,free,
// realloc} wasm imports; composition wires them to libc.so's exports.
extern "C" {
    fn malloc(size: usize) -> *mut u8;
    fn free(ptr: *mut u8);
    fn realloc(ptr: *mut u8, size: usize) -> *mut u8;
}

// cabi_realloc: wit-component requires every dl-openable module that
// exchanges list/string types with the host to export this. The
// canonical ABI uses it to allocate space in the caller's memory for
// returned strings and lists.
#[no_mangle]
pub unsafe extern "C" fn cabi_realloc(
    old_ptr: *mut u8,
    _old_len: usize,
    _align: usize,
    new_len: usize,
) -> *mut u8 {
    if new_len == 0 {
        if !old_ptr.is_null() {
            free(old_ptr);
        }
        return ptr::null_mut();
    }
    if old_ptr.is_null() {
        malloc(new_len)
    } else {
        realloc(old_ptr, new_len)
    }
}

struct LibcAllocator;
unsafe impl GlobalAlloc for LibcAllocator {
    unsafe fn alloc(&self, layout: Layout) -> *mut u8 {
        // wasi-libc's malloc returns 16-byte-aligned pointers — fine
        // for everything in this crate (Vec<Option<Library>> = ptr+
        // len+cap of usize, naturally aligned).
        let _ = layout.align();
        malloc(layout.size())
    }
    unsafe fn dealloc(&self, ptr: *mut u8, _layout: Layout) {
        free(ptr);
    }
}

#[global_allocator]
static ALLOCATOR: LibcAllocator = LibcAllocator;

wit_bindgen::generate!({
    world: "dyld-gen",
    path: "wit",
    generate_all,
});

// wit_bindgen generates the module path from the wit file's package
// declaration. The Makefile stages a wit/world.wit that imports
// particle:host/dyld@0.1.0 (see the python-runtime target), so the
// binding tree lives under `particle::host::dyld`.
use particle::host::dyld::{self, Library};

// Single-threaded wasm — UnsafeCell<...> in static is safe given no
// concurrency. Handles are 1-based indices into LIBS; 0 stays
// null/error.
struct SingleThread<T>(UnsafeCell<T>);
unsafe impl<T> Sync for SingleThread<T> {}
impl<T> SingleThread<T> {
    const fn new(t: T) -> Self {
        Self(UnsafeCell::new(t))
    }
    #[inline]
    unsafe fn get(&self) -> &mut T {
        &mut *self.0.get()
    }
}

static LIBS: SingleThread<Vec<Option<Library>>> = SingleThread::new(Vec::new());
static LAST_ERROR: SingleThread<Vec<u8>> = SingleThread::new(Vec::new());

// Write a NUL-terminated message into LAST_ERROR. Manual to avoid
// pulling in format!. The dlerror contract returns a `*const c_char`
// valid until the next dlerror; storing the bytes in a Vec we own
// satisfies that.
unsafe fn set_error(msg: &str) {
    let buf = LAST_ERROR.get();
    buf.clear();
    buf.extend_from_slice(msg.as_bytes());
    buf.push(0);
}

unsafe fn set_error_2(a: &str, b: &str) {
    let buf = LAST_ERROR.get();
    buf.clear();
    buf.extend_from_slice(a.as_bytes());
    buf.extend_from_slice(b.as_bytes());
    buf.push(0);
}

unsafe fn set_error_3(a: &str, b: &str, c: &str) {
    let buf = LAST_ERROR.get();
    buf.clear();
    buf.extend_from_slice(a.as_bytes());
    buf.extend_from_slice(b.as_bytes());
    buf.extend_from_slice(c.as_bytes());
    buf.push(0);
}

#[no_mangle]
pub unsafe extern "C" fn dlopen(name: *const c_char, _flags: c_int) -> *mut c_void {
    if name.is_null() {
        set_error("dlopen: null name");
        return ptr::null_mut();
    }
    let name = match CStr::from_ptr(name).to_str() {
        Ok(s) => s,
        Err(_) => {
            set_error("dlopen: name not utf-8");
            return ptr::null_mut();
        }
    };
    match dyld::load(name) {
        Ok(lib) => {
            let libs = LIBS.get();
            libs.push(Some(lib));
            libs.len() as *mut c_void
        }
        Err(dyld::DyldError::NotConfigured) => {
            set_error("dyld host not configured");
            ptr::null_mut()
        }
        Err(dyld::DyldError::LoadFailed(msg)) => {
            set_error_3("dlopen(", name, "): ");
            // Append msg without a fresh clear
            let buf = LAST_ERROR.get();
            buf.pop(); // remove the trailing NUL we just placed
            buf.extend_from_slice(msg.as_bytes());
            buf.push(0);
            ptr::null_mut()
        }
    }
}

#[no_mangle]
pub unsafe extern "C" fn dlsym(handle: *mut c_void, sym: *const c_char) -> *mut c_void {
    if handle.is_null() || sym.is_null() {
        set_error("dlsym: null arg");
        return ptr::null_mut();
    }
    let idx = (handle as usize) - 1;
    let sym = match CStr::from_ptr(sym).to_str() {
        Ok(s) => s,
        Err(_) => {
            set_error("dlsym: sym not utf-8");
            return ptr::null_mut();
        }
    };
    let libs = LIBS.get();
    let lib = match libs.get(idx).and_then(|o| o.as_ref()) {
        Some(l) => l,
        None => {
            set_error_2("dlsym: stale handle: ", sym);
            return ptr::null_mut();
        }
    };
    match lib.symbol(sym) {
        Ok(table_idx) => table_idx as usize as *mut c_void,
        Err(e) => {
            set_error_3("dlsym(", sym, "): ");
            let buf = LAST_ERROR.get();
            buf.pop();
            buf.extend_from_slice(e.as_bytes());
            buf.push(0);
            ptr::null_mut()
        }
    }
}

#[no_mangle]
pub unsafe extern "C" fn dlclose(handle: *mut c_void) -> c_int {
    if handle.is_null() {
        return -1;
    }
    let idx = (handle as usize) - 1;
    let libs = LIBS.get();
    match libs.get_mut(idx) {
        Some(slot) => {
            *slot = None;
            0
        }
        None => -1,
    }
}

#[no_mangle]
pub unsafe extern "C" fn dlerror() -> *const c_char {
    let buf = LAST_ERROR.get();
    if buf.is_empty() {
        ptr::null()
    } else {
        buf.as_ptr() as *const c_char
    }
}

// no_std panic handler — abort. wit-bindgen 0.57's macros generate
// code that asserts on out-of-range enum values; without a handler,
// the linker errors. abort is fine — these paths shouldn't fire at
// runtime for well-formed inputs.
#[panic_handler]
fn panic(_: &core::panic::PanicInfo) -> ! {
    core::arch::wasm32::unreachable()
}
