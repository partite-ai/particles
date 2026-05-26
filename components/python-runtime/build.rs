// build.rs is intentionally a no-op for the staticlib path: cargo
// only bundles Rust object code into the .a, ignoring
// `cargo:rustc-link-arg` for crate_type = "staticlib". The non-Rust
// pieces (stubs.c, init_dyld.wat) live in the Makefile alongside the
// manual wasm-ld link step.
//
// We keep build.rs around to emit rerun-if-changed directives so
// cargo notices wit/ updates.

fn main() {
    println!("cargo:rerun-if-changed=build.rs");
    println!("cargo:rerun-if-changed=wit/world.wit");
}
