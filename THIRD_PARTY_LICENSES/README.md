# Third-Party Licenses

Particles is licensed under GPL-3.0-or-later (see top-level `LICENSE`).
The shipped binary additionally contains third-party software listed
below. This file inventories every redistributed component, its
license, and the copyright holders that need attribution.

The full text of each unique license appears under `licenses/`. Where
a single license file covers multiple components (the canonical MIT,
BSD-2-Clause, BSD-3-Clause and Apache-2.0 texts), the inventory below
lists per-component copyright holders that the license requires you
to preserve.

---

## 1. Layout: what binaries ship what

Files marked **shipped** are present in the distributed binary. Files
marked **build-time** are tools used to produce shipped artifacts but
are not themselves redistributed.

| Surface | Contents |
|---|---|
| `particle` Go binary (shipped) | Go stdlib + every module in `go.mod` (direct + indirect) |
| `runtime/embed/particle-js-runtime.wasm.zst` (shipped) | QuickJS, wasm-rquickjs runtime, Rust libstd, wit-bindgen runtime, our TS shim |
| `runtime/embed/particle-python-runtime.wasm.zst` (shipped) | CPython 3.14 (dicej fork) and its bundled dependencies, PyO3, Rust libstd, wit-bindgen runtime, our Rust shim |
| `runtime/embed/python3.14-stdlib.zip` (shipped) | CPython 3.14 standard library bytecode |
| `runtime/embed/python-runtime-bootstrap.zip` (shipped) | Our own bootstrap.py (GPL-3.0-or-later) |
| `internal/build/wacogo/embed/deno-npm.wasm.zst` (shipped) | deno_npm + transitive Rust crates |
| `internal/build/wacogo/embed/pip-resolve.wasm.zst` (shipped) | pubgrub, pep508_rs, pep440_rs + transitive Rust crates |
| `internal/build/wacogo/embed/particle-typecheck.wasm.zst` (shipped) | TypeScript compiler, QuickJS, wasm-rquickjs |
| `cmd/particle/winstub/*/trampoline.exe.zst` (shipped) | Rust no_std stub + windows-sys (adapted from posy + uv) |
| `internal/nodebuiltins/vendored/punycode/` (shipped) | Vendored npm `punycode` package |
| esbuild CLI, wasm-tools, wasi-sdk, wasm-rquickjs, componentize-py | build-time only — not redistributed |

---

## 2. Go module dependencies (in `particle` binary)

Generated against `go.mod` at the time of this file's most recent
update. To regenerate inventory after a `go.mod` change, run
`go-licenses report ./...` and reconcile with the table below.

| Module | Version | License | License file |
|---|---|---|---|
| github.com/BurntSushi/toml | v1.6.0 | MIT | `licenses/MIT.txt` — © 2013 TOML authors |
| github.com/andybalholm/brotli | v1.2.1 | MIT | `licenses/MIT.txt` — © 2016 The Brotli Authors |
| github.com/evanw/esbuild | v0.28.0 | MIT | `licenses/MIT.txt` — © 2020 Evan Wallace |
| github.com/google/jsonschema-go | v0.4.3 | Apache-2.0 | `licenses/Apache-2.0.txt` — © Google LLC |
| github.com/klauspost/compress | v1.18.6 | Apache-2.0 + BSD-3-Clause + MIT (per-subpackage) | `licenses/Apache-2.0.txt`, `licenses/BSD-3-Clause.txt`, `licenses/MIT.txt` — © Klaus Post, the Go Authors, et al. |
| github.com/modelcontextprotocol/go-sdk | v1.6.0 | MIT | `licenses/MIT.txt` — © Anthropic, PBC and contributors |
| github.com/partite-ai/wacogo | v0.0.0-20260601004924-5e687b021449 | GPL-3.0-or-later | (this project's owner; covered by repo LICENSE) |
| github.com/spf13/cobra | v1.10.2 | Apache-2.0 | `licenses/Apache-2.0.txt` — © 2013 Steve Francia |
| github.com/spf13/pflag | v1.0.9 | BSD-3-Clause | `licenses/BSD-3-Clause.txt` — © 2012 Alex Ogier, © 2012 The Go Authors |
| github.com/tetratelabs/wazero | v1.11.1-0.20260511190115-a3374cf27a3a | Apache-2.0 | `licenses/Apache-2.0.txt` — © wazero authors |
| github.com/zalando/go-keyring | v0.2.8 | MIT | `licenses/MIT.txt` — © Zalando SE |
| github.com/danieljoos/wincred | v1.2.3 | MIT | `licenses/MIT.txt` — © 2016 Daniel Joos |
| github.com/dustin/go-humanize | v1.0.1 | MIT | `licenses/MIT.txt` — © 2005-2008 Dustin Sallings |
| github.com/godbus/dbus/v5 | v5.2.2 | BSD-2-Clause | `licenses/BSD-2-Clause.txt` — © 2013, Georg Reinke et al. |
| github.com/google/uuid | v1.6.0 | BSD-3-Clause | `licenses/BSD-3-Clause.txt` — © 2009,2014 Google Inc. |
| github.com/inconshreveable/mousetrap | v1.1.0 | Apache-2.0 | `licenses/Apache-2.0.txt` — © 2014 Alan Shreve |
| github.com/mattn/go-isatty | v0.0.20 | MIT | `licenses/MIT.txt` — © Yasuhiro MATSUMOTO |
| github.com/ncruces/go-strftime | v1.0.0 | MIT | `licenses/MIT.txt` — © Nuno Cruces |
| github.com/remyoudompheng/bigfft | v0.0.0-20230129092748 | BSD-3-Clause | `licenses/BSD-3-Clause.txt` — © 2012 The Go Authors / Rémy Oudompheng |
| github.com/segmentio/asm | v1.1.3 | MIT | `licenses/MIT.txt` — © 2021 Segment |
| github.com/segmentio/encoding | v0.5.4 | MIT | `licenses/MIT.txt` — © 2019 Segment |
| github.com/yosida95/uritemplate/v3 | v3.0.2 | BSD-3-Clause | `licenses/BSD-3-Clause.txt` — © 2016, Kohei YOSHIDA |
| golang.org/x/crypto | v0.50.0 | BSD-3-Clause | `licenses/BSD-3-Clause.txt` — © 2009 The Go Authors |
| golang.org/x/mod | v0.35.0 | BSD-3-Clause | `licenses/BSD-3-Clause.txt` — © 2009 The Go Authors |
| golang.org/x/oauth2 | v0.36.0 | BSD-3-Clause | `licenses/BSD-3-Clause.txt` — © 2009 The Go Authors |
| golang.org/x/sys | v0.45.0 | BSD-3-Clause | `licenses/BSD-3-Clause.txt` — © 2009 The Go Authors |
| golang.org/x/term | v0.42.0 | BSD-3-Clause | `licenses/BSD-3-Clause.txt` — © 2009 The Go Authors |
| modernc.org/sqlite | v1.50.0 | BSD-3-Clause | `licenses/BSD-3-Clause.txt` — © 2017 The Sqlite Authors |
| modernc.org/libc | v1.72.0 | BSD-3-Clause | `licenses/BSD-3-Clause.txt` — © 2020 The libc Authors |
| modernc.org/mathutil | v1.7.1 | BSD-3-Clause | `licenses/BSD-3-Clause.txt` — © 2014 The mathutil Authors |
| modernc.org/memory | v1.11.0 | BSD-3-Clause | `licenses/BSD-3-Clause.txt` — © 2017 The Memory Authors |

The Go standard library itself is BSD-3-Clause (© 2009 The Go Authors)
and is statically linked into the `particle` binary; the BSD-3-Clause
text in `licenses/BSD-3-Clause.txt` covers it.

---

## 3. CPython 3.14 and its bundled dependencies (in `particle-python-runtime.wasm` and `python3.14-stdlib.zip`)

CPython 3.14 is redistributed as a fork (`github.com/dicej/cpython` at
commit `0e13686da8bb881b059d35e23c32bcd2e6440099`) compiled to
`wasm32-wasip2`. The full text of the PSF License Agreement v2 and
the "Licenses and Acknowledgements for Incorporated Software" section
(covering HACL\*, mpdecimal, expat, zlib, SQLite, and others) is in
`licenses/CPython-PSF-and-incorporated.txt`. That single file
satisfies the attribution requirements for every C component listed
below.

Note for downstream: this is a **modified** CPython distribution
(the dicej fork carries patches for wasm-component-model support).
PSF License Agreement section 3 requires we mention this.

| Component | Version / commit | License | Where to look |
|---|---|---|---|
| CPython 3.14 (dicej fork) | `0e13686da8bb881b059d35e23c32bcd2e6440099` | PSF License Agreement v2 | `licenses/CPython-PSF-and-incorporated.txt` (section B) |
| HACL\* hash primitives | bundled with CPython | Apache-2.0 | `licenses/CPython-PSF-and-incorporated.txt` (section C) + `licenses/Apache-2.0.txt` |
| libmpdec | bundled with CPython | BSD-2-Clause | `licenses/CPython-PSF-and-incorporated.txt` (section C) + `licenses/BSD-2-Clause.txt` |
| Expat XML parser | bundled with CPython | MIT | `licenses/CPython-PSF-and-incorporated.txt` (section C) + `licenses/MIT.txt` |
| zlib | 1.3.1 (also linked separately at build time) | zlib license | `licenses/zlib.txt` |
| SQLite | 3.51.x | public domain ("Blessing") | `licenses/CPython-PSF-and-incorporated.txt` (section C) |
| wasi-libc | from wasi-sdk | MIT + Apache-2.0 with LLVM exception | `licenses/MIT.txt` + `licenses/Apache-2.0-with-LLVM-exception.txt` — © WebAssembly contributors |
| wasi-emulated-{signal,getpid,process-clocks} | from wasi-sdk | MIT + Apache-2.0 with LLVM exception | as above |

---

## 4. Rust crates baked into the shipped WebAssembly artifacts

All Rust components are compiled to wasm32-wasip2 and linked into one
of the embedded wasm artifacts. Source crates are MIT, Apache-2.0,
MIT-OR-Apache-2.0 (dual-licensed), or MPL-2.0 as noted.

### 4a. In `particle-python-runtime.wasm`

| Crate | License | Notes |
|---|---|---|
| pyo3 0.27 | MIT OR Apache-2.0 | © 2017-present PyO3 Project and contributors |
| pyo3-ffi 0.27 | MIT OR Apache-2.0 | © PyO3 Project |
| wit-bindgen 0.57 (runtime + macros) | Apache-2.0 WITH LLVM-exception | © Bytecode Alliance |
| Rust standard library | MIT OR Apache-2.0 | © The Rust Project Developers |
| Our `python-runtime`, `dyld-libdl`, `libffi-wasi-bridge` crates | GPL-3.0-or-later | (this project) |

### 4b. In `particle-js-runtime.wasm` and `particle-typecheck.wasm`

| Crate / component | License | Notes |
|---|---|---|
| QuickJS (the JS engine, embedded via wasm-rquickjs) | MIT | © 2017-2024 Fabrice Bellard, Charlie Gordon |
| wasm-rquickjs runtime + Wizer pre-init | Apache-2.0 WITH LLVM-exception | © Bytecode Alliance |
| TypeScript compiler (in `particle-typecheck.wasm` only) | Apache-2.0 | © Microsoft Corporation. Microsoft's NOTICE applies. |
| wit-bindgen 0.57 | Apache-2.0 WITH LLVM-exception | © Bytecode Alliance |
| Rust standard library | MIT OR Apache-2.0 | © The Rust Project Developers |
| Our `js-runtime` TypeScript shim | GPL-3.0-or-later | (this project) |

### 4c. In `deno-npm.wasm`

| Crate | License | Notes |
|---|---|---|
| deno_npm 0.59 | MIT | © the Deno authors |
| deno_semver 0.9.1 | MIT | © the Deno authors |
| wstd 0.5 | Apache-2.0 WITH LLVM-exception | © Bytecode Alliance contributors |
| wit-bindgen 0.39 | Apache-2.0 WITH LLVM-exception | © Bytecode Alliance |
| serde, serde_json | MIT OR Apache-2.0 | © Erick Tryzelaar, David Tolnay, et al. |
| async-trait 0.1 | MIT OR Apache-2.0 | © David Tolnay |
| flate2 1 | MIT OR Apache-2.0 | © Alex Crichton |
| tar 0.4 | MIT OR Apache-2.0 | © Alex Crichton |
| sha2, sha1 0.10 | MIT OR Apache-2.0 | © RustCrypto Developers |
| base64 0.22 | MIT OR Apache-2.0 | © Alice Maz, Marshall Pierce |
| hex 0.4 | MIT OR Apache-2.0 | © KokaKiwi |
| Rust standard library | MIT OR Apache-2.0 | © The Rust Project Developers |
| Our `deno-npm-component` crate | GPL-3.0-or-later | (this project) |

### 4d. In `pip-resolve.wasm`

| Crate | License | Notes |
|---|---|---|
| pep508_rs 0.9 | Apache-2.0 | © Konstin |
| pep440_rs 0.7 | Apache-2.0 | © Konstin |
| **pubgrub 0.3** | **MPL-2.0** | © Matthieu Pizenberg, Jacob Finkelman et al. See "MPL-2.0 obligations" below. |
| wstd 0.5 | Apache-2.0 WITH LLVM-exception | © Bytecode Alliance contributors |
| wit-bindgen 0.39 | Apache-2.0 WITH LLVM-exception | © Bytecode Alliance |
| serde, serde_json | MIT OR Apache-2.0 | © serde authors |
| sha2 0.10, hex 0.4 | MIT OR Apache-2.0 | © RustCrypto / KokaKiwi |
| Rust standard library | MIT OR Apache-2.0 | © The Rust Project Developers |
| Our `pip-resolve-component` crate | GPL-3.0-or-later | (this project) |

### 4e. In `cmd/particle/winstub/*/trampoline.exe`

| Crate / source | License | Notes |
|---|---|---|
| windows-sys 0.59 | MIT OR Apache-2.0 | © Microsoft Corporation |
| Rust standard library | MIT OR Apache-2.0 | © The Rust Project Developers |
| Original code adapted from `njsmith/posy` | MIT OR Apache-2.0 | © Nathaniel J. Smith and contributors |
| Original code adapted from `astral-sh/uv` | MIT OR Apache-2.0 | © Astral Software Inc. |
| Our `particle-win-trampoline` crate | GPL-3.0-or-later | (this project) |

Adapted-from credit lines should also appear in the source file
headers under `components/win-trampoline/src/`.

---

## 5. Vendored npm packages

| Package | Version | License | Source |
|---|---|---|---|
| punycode | as committed | MIT — © Mathias Bynens | `internal/nodebuiltins/vendored/punycode/LICENSE-MIT.txt` |

The original `LICENSE-MIT.txt` is checked in next to the package and
already satisfies the MIT attribution requirement.

---

## 6. MPL-2.0 obligations (for pubgrub)

`pip-resolve.wasm` statically links `pubgrub` (MPL-2.0). MPL-2.0 is
per-file copyleft, not whole-work copyleft, so it does **not**
infect this project's GPL-3.0-or-later license. The downstream
obligations we take on by redistributing pubgrub are:

1. Include the full MPL-2.0 text — see `licenses/MPL-2.0.txt`.
2. Make pubgrub's source code (including any modifications we make
   to pubgrub source files, if any) available to recipients of
   `pip-resolve.wasm`. We currently link an unmodified upstream
   release; pointing recipients at the upstream repository
   (`https://github.com/pubgrub-rs/pubgrub` at the version we
   depend on, see `components/pip-resolve/Cargo.toml`) satisfies
   this. If we ever patch pubgrub locally, the patched source must
   be made available under MPL-2.0.
3. We do not need to GPL pip-resolve.wasm because of pubgrub;
   pip-resolve.wasm is our own code, which we GPL by choice.

---

## 7. Apache-2.0 NOTICE files

Apache-2.0 section 4(d) requires that we preserve any `NOTICE` file
present in an Apache-2.0-licensed dependency. The Apache-2.0 deps
in this tree that carry a non-trivial NOTICE are:

- **TypeScript** (`particle-typecheck.wasm`): the Microsoft TypeScript
  project ships a NOTICE crediting third-party components it
  incorporates. The current upstream NOTICE is mirrored at
  `https://github.com/microsoft/TypeScript/blob/main/CopyrightNotice.txt`
  and should be reproduced verbatim in a future
  `notices/TypeScript-NOTICE.txt` if you intend to redistribute
  pre-built typecheck.wasm to third parties. For internal use only,
  the canonical Apache-2.0 text plus this acknowledgement suffices.
- **wazero**, **klauspost/compress**, **cobra**, **mousetrap**,
  **jsonschema-go**, **pep508_rs/pep440_rs**: no project-level
  NOTICE file at the version pinned in `go.mod` / `Cargo.toml` at
  the time of writing. Verify on `go.mod`/`Cargo.toml` bumps.
- **HACL\*** (in CPython): NOTICE text included in
  `CPython-PSF-and-incorporated.txt`.
- **wit-bindgen / wstd / wasm-rquickjs** (Bytecode Alliance): no
  separate NOTICE file required — the LLVM-exception variant of
  Apache-2.0 already covers the relevant cases.

If you redistribute the binary to third parties, populate a
`notices/` subdirectory by fetching each dep's `NOTICE` from
upstream at the pinned version and dropping it in. The Apache-2.0
NOTICE obligation only fires if a dep actually ships one.

---

## 8. License-text index

Available under `licenses/`:

- `Apache-2.0.txt` — canonical Apache License 2.0 text
- `Apache-2.0-with-LLVM-exception.txt` — the LLVM Exception text (used in addition to `Apache-2.0.txt`)
- `BSD-2-Clause.txt` — canonical BSD 2-Clause text
- `BSD-3-Clause.txt` — canonical BSD 3-Clause text
- `CPython-PSF-and-incorporated.txt` — verbatim copy of CPython 3.14's LICENSE file (PSF + incorporated-software acknowledgements)
- `MIT.txt` — canonical MIT permission notice
- `MPL-2.0.txt` — Mozilla Public License 2.0
- `zlib.txt` — zlib license

The project's own license (GPL-3.0-or-later) is at the repo root as
`LICENSE`.

---

## 9. Regenerating this inventory

If `go.mod`, the Rust component manifests, or the embedded artifacts
change in a way that changes the dep tree, this file should be
updated. Useful commands:

```sh
# Go side:
go install github.com/google/go-licenses@latest
go-licenses report ./... --template '{{.Name}},{{.LicenseName}},{{.LicenseURL}}\n'

# Rust side, per component:
cargo install cargo-license
for c in components/deno-npm components/pip-resolve \
         components/python-runtime components/dyld-libdl \
         components/libffi-wasi-bridge components/win-trampoline; do
  echo "== $c =="
  cargo license --manifest-path "$c/Cargo.toml" --avoid-build-deps
done
```

When a new license type appears, add its canonical text to
`licenses/` and reference it from the relevant table above.
