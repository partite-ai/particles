# windows/arm64 launcher stub

`trampoline.exe.zst` (zstd-compressed) is embedded here and appended,
with a trailer, to the file `particle link` creates on Windows. It is
the `components/win-trampoline` crate built for
`aarch64-pc-windows-gnullvm`.

The artifact is committed so a fresh `go build` / `go install`
produces a working `particle link` on windows/arm64 without first
provisioning the Rust + llvm-mingw cross-toolchain.

To rebuild it from source (after a `components/win-trampoline` change),
run `make win-trampoline LLVM_MINGW_BIN=<llvm-mingw>/bin` from the repo
root and commit the regenerated file alongside the source change.
