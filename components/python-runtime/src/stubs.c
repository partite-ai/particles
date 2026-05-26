// Stubs for libc / libstd symbols our composition's PIC side modules
// don't define. Kept local so python-runtime is self-contained.
//
// All symbols are exported with default visibility so wasm-ld places
// them in the export table; composition then makes them visible to
// libpython3.14.so / libstd's wasi-pal references via the env shim.

#include <stddef.h>
#include <stdlib.h>

// wit-bindgen 0.57.1's libwit_bindgen_cabi.a references
// cabi_realloc_wit_bindgen_0_57_1 but only defines it on
// target_env != "p2". On wasip2 it expects libstd to provide the
// symbol — libstd's wasi-pal in turn imports it as undefined. Define
// it here to break the cycle and hand the caller a real allocator.
__attribute__((visibility("default")))
void *cabi_realloc_wit_bindgen_0_57_1(void *old_ptr, size_t old_len,
                                      size_t align, size_t new_len) {
    (void)align;
    (void)old_len;
    if (new_len == 0) {
        free(old_ptr);
        return NULL;
    }
    return realloc(old_ptr, new_len);
}

// Rust's libstd references _CLOCK_PROCESS_CPUTIME_ID and
// _CLOCK_THREAD_CPUTIME_ID via GOT.mem (e.g., in
// std::time::Instant::now() paths). wasi-libc-shared's libc.so
// exports _CLOCK_MONOTONIC + _CLOCK_REALTIME but not these per-
// thread/process variants — WASI doesn't have them. Define them as
// static i32s holding the POSIX numerical constants; clock_gettime on
// either would return ENOTSUP from wasi-libc anyway, so the value
// doesn't matter for correctness.
__attribute__((visibility("default")))
int _CLOCK_PROCESS_CPUTIME_ID = 2;

__attribute__((visibility("default")))
int _CLOCK_THREAD_CPUTIME_ID = 3;
