# Particle — top-level build orchestrator.
#
#   make                   default — builds every dist/ artifact
#   make deno-npm          dist/deno-npm.wasm
#   make pip-resolve       dist/pip-resolve.wasm
#   make js-runtime        dist/particle-js-runtime.wasm
#   make python-runtime    dist/particle-python-runtime.wasm
#   make typecheck         dist/particle-typecheck.wasm
#   make wasm-example      native-WASM particle test fixture
#   make python-lib        CPython 3.14 as wasm32-wasip2 libpython3.14.{a,so}
#   make python-stdlib-zip      runtime/embed/python3.14-stdlib.zip
#   make python-bootstrap-zip   runtime/embed/python-runtime-bootstrap.zip
#   make embed             build-pipeline wasms → internal/build/wacogo/embed/
#   make runtime-embed     runtime wasms + zips → runtime/embed/
#   make win-trampoline    windows launcher stubs → cmd/particle/winstub/{amd64,arm64}/
#   make test              go test ./...
#   make clean             wipe dist/ and intermediate build dirs
#
# Most targets are .PHONY: cargo/esbuild/wasm-rquickjs each handle their
# own incremental rebuilds, and reproducing make's dependency tracking
# for those would be brittle.

# -----------------------------------------------------------------------------
# Paths + tools
# -----------------------------------------------------------------------------

DIST_DIR          := dist
BUILD_EMBED_DIR   := internal/build/wacogo/embed
RUNTIME_EMBED_DIR := runtime/embed

WASI_SDK_PATH      ?= /opt/wasi-sdk
WASI_SYSROOT       := $(WASI_SDK_PATH)/share/wasi-sysroot
WASI_SYSROOT_SHARED := $(WASI_SYSROOT)/lib/wasm32-wasip2

# Cargo target dirs live under $(HOME)/cargo-target/ rather than the
# workspace. rquickjs-sys's build script unpacks the WASI SDK tarball
# with utime() calls, which virtiofs (this dev container's workspace
# mount) blocks. Same reason applies to the CPython autotools build
# below — symlinks and freeze steps don't survive that mount either.
CARGO_TARGET_BASE := $(HOME)/cargo-target

# `npm install --no-bin-links` skips .bin/ symlink dir creation; that's
# the only npm step virtiofs blocks (chmod on the symlink targets
# fails). The package contents we actually bundle extract fine.
NPM_INSTALL := npm install --no-bin-links --no-audit --no-fund

# -----------------------------------------------------------------------------
# .PHONY
# -----------------------------------------------------------------------------

.PHONY: all clean test go-test embed runtime-embed win-trampoline \
        deno-npm pip-resolve js-runtime typecheck wasm-example \
        python-lib python-runtime python-stdlib-zip python-bootstrap-zip

all: deno-npm pip-resolve js-runtime python-runtime typecheck

# -----------------------------------------------------------------------------
# embed targets — populate go:embed pickup dirs for the build pipeline
# (internal/build/wacogo/) and the runtime (runtime/). Compression is
# klauspost zstd at max level via tools/embedcompress; shrinks the
# embedded footprint by ~60% and decompresses once per Runtime.New.
# -----------------------------------------------------------------------------

# `go generate ./internal/build/wacogo/` runs this.
embed: deno-npm pip-resolve typecheck
	@mkdir -p $(BUILD_EMBED_DIR)
	@echo 'compress (zstd max via tools/embedcompress) → $(BUILD_EMBED_DIR)/'
	go run ./tools/embedcompress $(DIST_DIR)/deno-npm.wasm           $(BUILD_EMBED_DIR)/deno-npm.wasm.zst
	go run ./tools/embedcompress $(DIST_DIR)/pip-resolve.wasm        $(BUILD_EMBED_DIR)/pip-resolve.wasm.zst
	go run ./tools/embedcompress $(DIST_DIR)/particle-typecheck.wasm $(BUILD_EMBED_DIR)/particle-typecheck.wasm.zst
	@printf '✓  embedded:\n'; ls -lh $(BUILD_EMBED_DIR)/*.wasm.zst | awk '{print "    "$$5"  "$$NF}'

# `go generate ./runtime/` runs this.
runtime-embed: js-runtime python-runtime python-stdlib-zip python-bootstrap-zip
	@mkdir -p $(RUNTIME_EMBED_DIR)
	@echo 'compress (zstd max via tools/embedcompress) → $(RUNTIME_EMBED_DIR)/'
	go run ./tools/embedcompress $(DIST_DIR)/particle-js-runtime.wasm     $(RUNTIME_EMBED_DIR)/particle-js-runtime.wasm.zst
	go run ./tools/embedcompress $(DIST_DIR)/particle-python-runtime.wasm $(RUNTIME_EMBED_DIR)/particle-python-runtime.wasm.zst
	@printf '✓  embedded:\n'; ls -lh $(RUNTIME_EMBED_DIR)/*.wasm.zst $(RUNTIME_EMBED_DIR)/*.zip 2>/dev/null | awk '{print "    "$$5"  "$$NF}'

# -----------------------------------------------------------------------------
# win-trampoline — native Windows launcher stub (no_std Rust) embedded
# into the particle CLI and appended-to by `particle link`. Built for
# both Windows arches and zstd-compressed into the cmd/particle
# go:embed dirs. Each stub is ~19 KB (≈9 KB compressed).
#
# Not part of `all`: it needs a Windows cross-toolchain the default dev
# container lacks. We cross-compile the *-pc-windows-gnullvm targets
# from Linux/macOS with llvm-mingw (self-contained clang + lld + CRT,
# no sudo): download the release matching your HOST arch from
# https://github.com/mstorsjo/llvm-mingw/releases, extract it, and pass
# its bin/ dir:
#
#   make win-trampoline LLVM_MINGW_BIN=$HOME/llvm-mingw/bin
#
# On a native Windows runner, override the triples to the msvc ones and
# leave LLVM_MINGW_BIN empty to use the default linker:
#
#   make win-trampoline \
#     WIN_TRAMPOLINE_TARGET_AMD64=x86_64-pc-windows-msvc \
#     WIN_TRAMPOLINE_TARGET_ARM64=aarch64-pc-windows-msvc
#
# The matching rustup targets must be installed:
#   rustup target add x86_64-pc-windows-gnullvm aarch64-pc-windows-gnullvm
#
# Like the wasm embeds, the outputs (cmd/particle/winstub/*/*.zst) are
# committed so a plain `go build` works toolchain-free; commit the
# regenerated stubs alongside any components/win-trampoline change.
# -----------------------------------------------------------------------------

WIN_TRAMPOLINE_DIR          := components/win-trampoline
WIN_TRAMPOLINE_CARGO_TARGET := $(CARGO_TARGET_BASE)/win-trampoline
WIN_TRAMPOLINE_TARGET_AMD64 ?= x86_64-pc-windows-gnullvm
WIN_TRAMPOLINE_TARGET_ARM64 ?= aarch64-pc-windows-gnullvm
LLVM_MINGW_BIN              ?=

# When LLVM_MINGW_BIN is set, point each gnullvm target's Rust linker at
# the matching llvm-mingw clang driver (it knows its own CRT sysroot)
# and put the toolchain on PATH so the driver finds ld.lld.
ifneq ($(LLVM_MINGW_BIN),)
WIN_TRAMPOLINE_ENV_AMD64 := PATH="$(LLVM_MINGW_BIN):$$PATH" CARGO_TARGET_X86_64_PC_WINDOWS_GNULLVM_LINKER="$(LLVM_MINGW_BIN)/x86_64-w64-mingw32-clang"
WIN_TRAMPOLINE_ENV_ARM64 := PATH="$(LLVM_MINGW_BIN):$$PATH" CARGO_TARGET_AARCH64_PC_WINDOWS_GNULLVM_LINKER="$(LLVM_MINGW_BIN)/aarch64-w64-mingw32-clang"
endif

win-trampoline:
	@mkdir -p cmd/particle/winstub/amd64 cmd/particle/winstub/arm64 $(WIN_TRAMPOLINE_CARGO_TARGET)
	@echo '[amd64] cargo build --target $(WIN_TRAMPOLINE_TARGET_AMD64) --release'
	$(WIN_TRAMPOLINE_ENV_AMD64) CARGO_TARGET_DIR=$(WIN_TRAMPOLINE_CARGO_TARGET) cargo build \
	  --manifest-path $(WIN_TRAMPOLINE_DIR)/Cargo.toml \
	  --target $(WIN_TRAMPOLINE_TARGET_AMD64) --release
	go run ./tools/embedcompress \
	  $(WIN_TRAMPOLINE_CARGO_TARGET)/$(WIN_TRAMPOLINE_TARGET_AMD64)/release/trampoline.exe \
	  cmd/particle/winstub/amd64/trampoline.exe.zst
	@echo '[arm64] cargo build --target $(WIN_TRAMPOLINE_TARGET_ARM64) --release'
	$(WIN_TRAMPOLINE_ENV_ARM64) CARGO_TARGET_DIR=$(WIN_TRAMPOLINE_CARGO_TARGET) cargo build \
	  --manifest-path $(WIN_TRAMPOLINE_DIR)/Cargo.toml \
	  --target $(WIN_TRAMPOLINE_TARGET_ARM64) --release
	go run ./tools/embedcompress \
	  $(WIN_TRAMPOLINE_CARGO_TARGET)/$(WIN_TRAMPOLINE_TARGET_ARM64)/release/trampoline.exe \
	  cmd/particle/winstub/arm64/trampoline.exe.zst
	@printf '✓  win-trampoline embedded:\n'; ls -lh cmd/particle/winstub/*/trampoline.exe.zst | awk '{print "    "$$5"  "$$NF}'

# -----------------------------------------------------------------------------
# Tests
# -----------------------------------------------------------------------------

test: go-test

go-test:
	go test ./...

# -----------------------------------------------------------------------------
# deno-npm.wasm — pure Rust crate, on-workspace target dir is fine.
# -----------------------------------------------------------------------------

deno-npm:
	@mkdir -p $(DIST_DIR)
	cd components/deno-npm && cargo build --target wasm32-wasip2 --release
	cp components/deno-npm/target/wasm32-wasip2/release/deno_npm_component.wasm \
	   $(DIST_DIR)/deno-npm.wasm
	@printf '✓  '; ls -lh $(DIST_DIR)/deno-npm.wasm | awk '{print $$5"  "$$NF}'

# -----------------------------------------------------------------------------
# pip-resolve.wasm — Python analog of deno-npm: resolves PEP 508 reqs
# from a Particlefile.py's PEP 723 inline metadata block, fetches the
# transitive closure of pure-Python wheels from PyPI.
# -----------------------------------------------------------------------------

PIP_RESOLVE_CARGO_TARGET := $(CARGO_TARGET_BASE)/pip-resolve

pip-resolve:
	@mkdir -p $(DIST_DIR) $(PIP_RESOLVE_CARGO_TARGET)
	CARGO_TARGET_DIR=$(PIP_RESOLVE_CARGO_TARGET) cargo build \
	  --manifest-path components/pip-resolve/Cargo.toml \
	  --target wasm32-wasip2 --release
	cp $(PIP_RESOLVE_CARGO_TARGET)/wasm32-wasip2/release/pip_resolve_component.wasm \
	   $(DIST_DIR)/pip-resolve.wasm
	@printf '✓  '; ls -lh $(DIST_DIR)/pip-resolve.wasm | awk '{print $$5"  "$$NF}'

# -----------------------------------------------------------------------------
# particle-js-runtime.wasm — TS → esbuild bundle → wasm-rquickjs
# wrapper crate → cargo build → wasm-rquickjs optimize (Wizer pre-init
# bakes QuickJS startup into the artifact).
# -----------------------------------------------------------------------------

JS_RUNTIME_CARGO_TARGET := $(CARGO_TARGET_BASE)/js-runtime

js-runtime:
	@mkdir -p $(DIST_DIR) $(JS_RUNTIME_CARGO_TARGET) components/js-runtime/build/wit/deps
	@echo '[1/5] assemble wit/  (runtime.wit + js-runtime.wit, plus shared deps/)'
	cat wit/runtime.wit wit/js-runtime.wit > components/js-runtime/build/wit/world.wit
	cp -R wit/deps/* components/js-runtime/build/wit/deps/
	@echo '[2/5] esbuild  src/runtime.ts  →  build/runtime.js'
	cd components/js-runtime && esbuild src/runtime.ts \
	  --bundle --format=esm --target=es2022 --platform=neutral \
	  '--external:wasi:*' '--external:particle:*' '--external:node:*' \
	  --outfile=build/runtime.js
	@echo '[3/5] wasm-rquickjs generate-wrapper-crate'
	rm -rf components/js-runtime/build/crate
	cd components/js-runtime && wasm-rquickjs generate-wrapper-crate \
	  --js build/runtime.js --wit build/wit --world runtime --output build/crate
	@echo '[4/5] cargo build --target wasm32-wasip2 --release  (target dir: $(JS_RUNTIME_CARGO_TARGET))'
	CARGO_TARGET_DIR=$(JS_RUNTIME_CARGO_TARGET) cargo build \
	  --manifest-path components/js-runtime/build/crate/Cargo.toml \
	  --target wasm32-wasip2 --release -j 1
	@echo '[5/5] wasm-rquickjs optimize  (Wizer pre-init)'
	wasm-rquickjs optimize \
	  --input  $(JS_RUNTIME_CARGO_TARGET)/wasm32-wasip2/release/runtime.wasm \
	  --output $(DIST_DIR)/particle-js-runtime.wasm
	@printf '✓  '; ls -lh $(DIST_DIR)/particle-js-runtime.wasm | awk '{print $$5"  "$$NF}'

# -----------------------------------------------------------------------------
# particle-typecheck.wasm — same shape as js-runtime, but bundles the
# `typescript` package (~10MB) into the JS so the compiler runs inside
# QuickJS. esbuild --platform=node lets us pull in typescript's
# CommonJS deps + node:* requires (resolved at runtime by
# wasm-rquickjs's node-compat shims).
# -----------------------------------------------------------------------------

TYPECHECK_CARGO_TARGET := $(CARGO_TARGET_BASE)/typecheck

typecheck:
	@mkdir -p $(DIST_DIR) $(TYPECHECK_CARGO_TARGET) components/typecheck/build
	@test -d components/typecheck/node_modules/typescript || \
	  (cd components/typecheck && $(NPM_INSTALL))
	@echo '[1/4] generate src/lib-bundle.ts and src/particle-globals.d.ts'
	cd components/typecheck && node build-particle-globals.mjs
	cd components/typecheck && node build-lib-bundle.mjs
	@echo '[2/4] esbuild  src/typecheck.ts  →  build/typecheck.js  (bundles typescript)'
	cd components/typecheck && esbuild src/typecheck.ts \
	  --bundle --format=esm --platform=node --target=es2022 \
	  '--external:wasi:*' '--external:particle:*' \
	  --outfile=build/typecheck.js
	@echo '[3/4] wasm-rquickjs generate-wrapper-crate'
	rm -rf components/typecheck/build/crate
	cd components/typecheck && wasm-rquickjs generate-wrapper-crate \
	  --js build/typecheck.js --wit wit --output build/crate
	@echo '[4/4] cargo build --target wasm32-wasip2 --release  (target dir: $(TYPECHECK_CARGO_TARGET))'
	CARGO_TARGET_DIR=$(TYPECHECK_CARGO_TARGET) cargo build \
	  --manifest-path components/typecheck/build/crate/Cargo.toml \
	  --target wasm32-wasip2 --release -j 1
	# Skip `wasm-rquickjs optimize` here: pre-initializing snapshots the
	# entire TypeScript compiler heap (~75 MB on top of the unoptimized
	# 15 MB), which we'd then ship inside every binary that runs
	# typechecks. Lazy init on first call costs a small fraction of the
	# actual type-check work, so it isn't worth it.
	cp $(TYPECHECK_CARGO_TARGET)/wasm32-wasip2/release/component.wasm \
	   $(DIST_DIR)/particle-typecheck.wasm
	@printf '✓  '; ls -lh $(DIST_DIR)/particle-typecheck.wasm | awk '{print $$5"  "$$NF}'

# -----------------------------------------------------------------------------
# wasm-example — native-WASM particle test fixture. Tiny Rust component
# (~100KB) that implements particle:runtime directly, no JS/Python
# engine in the picture. The build and runtime test suites use it to
# exercise the `particle build --component` path.
# -----------------------------------------------------------------------------

WASM_EXAMPLE_CARGO_TARGET := $(CARGO_TARGET_BASE)/wasm-example

wasm-example:
	@mkdir -p components/wasm-example/wit/deps $(WASM_EXAMPLE_CARGO_TARGET)
	@echo '[1/2] assemble wit/  (runtime.wit + wasm-particle.wit, plus shared deps/)'
	cat wit/runtime.wit wit/wasm-particle.wit > components/wasm-example/wit/world.wit
	cp -R wit/deps/* components/wasm-example/wit/deps/
	@echo '[2/2] cargo build --target wasm32-wasip2 --release  (target dir: $(WASM_EXAMPLE_CARGO_TARGET))'
	CARGO_TARGET_DIR=$(WASM_EXAMPLE_CARGO_TARGET) cargo build \
	  --manifest-path components/wasm-example/Cargo.toml \
	  --target wasm32-wasip2 --release
	@printf '✓  '; ls -lh $(WASM_EXAMPLE_CARGO_TARGET)/wasm32-wasip2/release/wasm_example_component.wasm | awk '{print $$5"  "$$NF}'

# =============================================================================
# python-lib — CPython 3.14 as wasm32-wasip2 library
# =============================================================================
# Builds dicej's CPython fork into libpython3.14.{a,so}. Mirrors
# componentize-py's build.rs (bytecodealliance/componentize-py@main,
# build.rs:292-389):
#
#   1. download dicej/cpython @ pinned commit + zlib + sqlite (cached
#      under $(CACHE_DIR) — these survive `make clean`)
#   2. build a native host Python (--with-build-python for step 4)
#   3. build zlib + sqlite as static libs into cpython/builddir/wasi/deps
#   4. configure CPython via Tools/wasm/wasi-env, write Modules/Setup.local
#      to force-enable _sqlite3 (configure can't auto-detect it
#      cross-compiling), then `make build_all install`
#   5. hand-link libpython3.14.so via --whole-archive + the HACL/mpdec/
#      expat/zlib/sqlite static libs + wasi-emulated-* shims
#
# CFLAGS/LDFLAGS asymmetry (CC targets wasm32-wasi with wasip1 includes,
# LD targets wasm32-wasip2) is deliberate and matches build.rs:566-581 —
# wasi-sdk-30's clang resolves both to the same sysroot artifacts.

CPYTHON_REV       := 0e13686da8bb881b059d35e23c32bcd2e6440099
ZLIB_VERSION      := 1.3.1
SQLITE_VERSION    := 3510200
SQLITE_YEAR       := 2026

# Source archive cache — survives `make clean`. Cache key = upstream
# version, so `make clean && make python-lib` reuses the tarballs.
CACHE_DIR         := $(HOME)/.cache/particle/cpython-build
CPYTHON_TARBALL   := $(CACHE_DIR)/cpython-$(CPYTHON_REV).tar.gz
ZLIB_TARBALL      := $(CACHE_DIR)/zlib-$(ZLIB_VERSION).tar.gz
SQLITE_TARBALL    := $(CACHE_DIR)/sqlite-autoconf-$(SQLITE_VERSION).tar.gz

# Build tree off virtiofs — autotools symlinks and CPython's freeze
# step don't survive the workspace mount.
PYBUILD_DIR        := $(CARGO_TARGET_BASE)/cpython-$(CPYTHON_REV)
CPYTHON_SRC        := $(PYBUILD_DIR)/cpython
ZLIB_SRC           := $(PYBUILD_DIR)/zlib-$(ZLIB_VERSION)
SQLITE_SRC         := $(PYBUILD_DIR)/sqlite-autoconf-$(SQLITE_VERSION)
CPYTHON_NATIVE_DIR := $(CPYTHON_SRC)/builddir/build
CPYTHON_WASI_DIR   := $(CPYTHON_SRC)/builddir/wasi
CPYTHON_DEPS_DIR   := $(CPYTHON_WASI_DIR)/deps
PYTHON_LIB_SO      := $(CPYTHON_WASI_DIR)/libpython3.14.so

# CFLAGS/LDFLAGS for the static C deps (zlib, sqlite). Matches build.rs
# add_compile_envs() — CC targets wasm32-wasi + wasip1 includes, LD
# targets wasm32-wasip2.
DEPS_CFLAGS    := --target=wasm32-wasi --sysroot=$(WASI_SYSROOT) -I$(WASI_SYSROOT)/include/wasm32-wasip1 -D_WASI_EMULATED_SIGNAL -fPIC
DEPS_LDFLAGS   := --target=wasm32-wasip2 --sysroot=$(WASI_SYSROOT) -L$(WASI_SYSROOT)/lib -lwasi-emulated-signal
SQLITE_CFLAGS  := --target=wasm32-wasi --sysroot=$(WASI_SYSROOT) -I$(WASI_SYSROOT)/include/wasm32-wasip1 -D_WASI_EMULATED_SIGNAL -D_WASI_EMULATED_PROCESS_CLOCKS -fPIC -O2 -DSQLITE_OMIT_WAL -DSQLITE_OMIT_LOAD_EXTENSION -DSQLITE_OMIT_LOCALTIME -DSQLITE_OMIT_RANDOMNESS -DSQLITE_OMIT_SHARED_CACHE
SQLITE_LDFLAGS := --target=wasm32-wasip2 --sysroot=$(WASI_SYSROOT) -L$(WASI_SYSROOT)/lib

python-lib: $(PYTHON_LIB_SO)
	@printf '✓  '; ls -lh $(PYTHON_LIB_SO) | awk '{print $$5"  "$$NF}'

# ---- source downloads (cached) ----

$(CPYTHON_TARBALL):
	@mkdir -p $(CACHE_DIR)
	curl -fsSL -o $@.tmp \
	  https://github.com/dicej/cpython/archive/$(CPYTHON_REV).tar.gz
	mv $@.tmp $@

$(ZLIB_TARBALL):
	@mkdir -p $(CACHE_DIR)
	curl -fsSL -o $@.tmp \
	  https://github.com/madler/zlib/releases/download/v$(ZLIB_VERSION)/zlib-$(ZLIB_VERSION).tar.gz
	mv $@.tmp $@

$(SQLITE_TARBALL):
	@mkdir -p $(CACHE_DIR)
	curl -fsSL -o $@.tmp \
	  https://sqlite.org/$(SQLITE_YEAR)/sqlite-autoconf-$(SQLITE_VERSION).tar.gz
	mv $@.tmp $@

# ---- extract ----

$(CPYTHON_SRC)/configure: $(CPYTHON_TARBALL)
	@mkdir -p $(PYBUILD_DIR)
	rm -rf $(CPYTHON_SRC) $(PYBUILD_DIR)/cpython-$(CPYTHON_REV)
	tar -xzf $(CPYTHON_TARBALL) -C $(PYBUILD_DIR)
	mv $(PYBUILD_DIR)/cpython-$(CPYTHON_REV) $(CPYTHON_SRC)
	touch $@

$(ZLIB_SRC)/configure: $(ZLIB_TARBALL)
	@mkdir -p $(PYBUILD_DIR)
	rm -rf $(ZLIB_SRC)
	tar -xzf $(ZLIB_TARBALL) -C $(PYBUILD_DIR)
	touch $@

$(SQLITE_SRC)/configure: $(SQLITE_TARBALL)
	@mkdir -p $(PYBUILD_DIR)
	rm -rf $(SQLITE_SRC)
	tar -xzf $(SQLITE_TARBALL) -C $(PYBUILD_DIR)
	touch $@

# ---- native host Python (build interpreter for the cross-build) ----

$(CPYTHON_NATIVE_DIR)/python: $(CPYTHON_SRC)/configure
	@mkdir -p $(CPYTHON_NATIVE_DIR)
	cd $(CPYTHON_NATIVE_DIR) && ../../configure --prefix=$(CPYTHON_NATIVE_DIR)/install
	$(MAKE) -C $(CPYTHON_NATIVE_DIR)

# ---- zlib (static, into wasi deps dir) ----

$(CPYTHON_DEPS_DIR)/lib/libz.a: $(ZLIB_SRC)/configure $(CPYTHON_SRC)/configure
	@mkdir -p $(CPYTHON_DEPS_DIR)
	cd $(ZLIB_SRC) && \
	  AR=$(WASI_SDK_PATH)/bin/ar \
	  CC=$(WASI_SDK_PATH)/bin/clang \
	  RANLIB=$(WASI_SDK_PATH)/bin/ranlib \
	  CFLAGS='$(DEPS_CFLAGS)' \
	  LDFLAGS='$(DEPS_LDFLAGS)' \
	  ./configure --static --prefix=$(CPYTHON_DEPS_DIR)
	$(MAKE) -C $(ZLIB_SRC) \
	  AR=$(WASI_SDK_PATH)/bin/ar \
	  ARFLAGS=rcs \
	  CC=$(WASI_SDK_PATH)/bin/clang \
	  RANLIB=$(WASI_SDK_PATH)/bin/ranlib \
	  CFLAGS='$(DEPS_CFLAGS)' \
	  LDFLAGS='$(DEPS_LDFLAGS)' \
	  static install

# ---- sqlite (static, into wasi deps dir) ----

$(CPYTHON_DEPS_DIR)/lib/libsqlite3.a: $(SQLITE_SRC)/configure $(CPYTHON_SRC)/configure
	@mkdir -p $(CPYTHON_DEPS_DIR)/lib $(CPYTHON_DEPS_DIR)/include
	cd $(SQLITE_SRC) && \
	  AR=$(WASI_SDK_PATH)/bin/ar \
	  CC=$(WASI_SDK_PATH)/bin/clang \
	  RANLIB=$(WASI_SDK_PATH)/bin/ranlib \
	  CFLAGS='$(SQLITE_CFLAGS)' \
	  LDFLAGS='$(SQLITE_LDFLAGS)' \
	  ./configure --host=wasm32-wasi --prefix=$(CPYTHON_DEPS_DIR) \
	    --disable-shared --enable-static \
	    --disable-readline --disable-threadsafe --disable-load-extension
	$(MAKE) -C $(SQLITE_SRC) \
	  AR=$(WASI_SDK_PATH)/bin/ar \
	  ARFLAGS=rcs \
	  CC=$(WASI_SDK_PATH)/bin/clang \
	  RANLIB=$(WASI_SDK_PATH)/bin/ranlib \
	  CFLAGS='$(SQLITE_CFLAGS)' \
	  libsqlite3.a
	cp $(SQLITE_SRC)/libsqlite3.a $@
	cp $(SQLITE_SRC)/sqlite3.h $(CPYTHON_DEPS_DIR)/include/sqlite3.h
	@if [ -f $(SQLITE_SRC)/sqlite3ext.h ]; then \
	   cp $(SQLITE_SRC)/sqlite3ext.h $(CPYTHON_DEPS_DIR)/include/sqlite3ext.h; \
	 fi

# ---- CPython wasi cross-build ----
#
# Setup.local is written AFTER configure but BEFORE make, matching
# build.rs ordering. The `_sqlite3` line lists the extension's source
# files because configure won't auto-wire them when cross-compiling.

$(CPYTHON_WASI_DIR)/libpython3.14.a: \
    $(CPYTHON_SRC)/configure \
    $(CPYTHON_NATIVE_DIR)/python \
    $(CPYTHON_DEPS_DIR)/lib/libz.a \
    $(CPYTHON_DEPS_DIR)/lib/libsqlite3.a
	@mkdir -p $(CPYTHON_WASI_DIR)
	cd $(CPYTHON_WASI_DIR) && \
	  CONFIG_SITE=../../Tools/wasm/wasi/config.site-wasm32-wasi \
	  WASI_SDK_PATH=$(WASI_SDK_PATH) \
	  CFLAGS='--target=wasm32-wasip2 -fPIC -I$(CPYTHON_DEPS_DIR)/include' \
	  LDFLAGS='--target=wasm32-wasip2 -L$(CPYTHON_DEPS_DIR)/lib' \
	  ../../Tools/wasm/wasi-env ../../configure -C \
	    --host=wasm32-unknown-wasip2 \
	    --build=$$(../../config.guess) \
	    --with-build-python=$(CPYTHON_NATIVE_DIR)/python \
	    --prefix=$(CPYTHON_WASI_DIR)/install \
	    --disable-test-modules \
	    --enable-ipv6
	@mkdir -p $(CPYTHON_WASI_DIR)/Modules
	@printf '%s\n' '_sqlite3 _sqlite/blob.c _sqlite/connection.c _sqlite/cursor.c _sqlite/microprotocols.c _sqlite/module.c _sqlite/prepare_protocol.c _sqlite/row.c _sqlite/statement.c _sqlite/util.c -I$(CPYTHON_DEPS_DIR)/include -L$(CPYTHON_DEPS_DIR)/lib -lsqlite3' \
	  > $(CPYTHON_WASI_DIR)/Modules/Setup.local
	$(MAKE) -C $(CPYTHON_WASI_DIR) build_all install

# ---- libpython3.14.so (hand-linked, --whole-archive) ----

$(PYTHON_LIB_SO): $(CPYTHON_WASI_DIR)/libpython3.14.a
	$(WASI_SDK_PATH)/bin/clang \
	  --target=wasm32-wasip2 -shared -o $@ \
	  -Wl,--whole-archive $(CPYTHON_WASI_DIR)/libpython3.14.a -Wl,--no-whole-archive \
	  $(CPYTHON_WASI_DIR)/Modules/_hacl/libHacl_HMAC.a \
	  $(CPYTHON_WASI_DIR)/Modules/_hacl/libHacl_Hash_BLAKE2.a \
	  $(CPYTHON_WASI_DIR)/Modules/_hacl/libHacl_Hash_MD5.a \
	  $(CPYTHON_WASI_DIR)/Modules/_hacl/libHacl_Hash_SHA1.a \
	  $(CPYTHON_WASI_DIR)/Modules/_hacl/libHacl_Hash_SHA2.a \
	  $(CPYTHON_WASI_DIR)/Modules/_hacl/libHacl_Hash_SHA3.a \
	  $(CPYTHON_WASI_DIR)/Modules/_decimal/libmpdec/libmpdec.a \
	  $(CPYTHON_WASI_DIR)/Modules/expat/libexpat.a \
	  $(CPYTHON_DEPS_DIR)/lib/libz.a \
	  $(CPYTHON_DEPS_DIR)/lib/libsqlite3.a \
	  -lwasi-emulated-signal -lwasi-emulated-getpid -lwasi-emulated-process-clocks -ldl

# =============================================================================
# python-runtime — PIC main.wasm + dyld-libdl libdl.so + libpython3.14.so
# + libc.so + wasi-emulated-* + init_dyld sibling, composed via
# `wasm-tools component link`, post-processed by inject-capture, then
# DWARF-stripped. main.wasm is a wit-bindgen-driven Rust crate that
# exports particle:runtime/{tools,health,manifest} and dispatches into a
# Python bootstrap module via the CPython C API (through PyO3).
# =============================================================================

PYTHON_RUNTIME_CARGO_TARGET     := $(CARGO_TARGET_BASE)/python-runtime
PYTHON_RUNTIME_STATICLIB        := $(PYTHON_RUNTIME_CARGO_TARGET)/wasm32-wasip2/release/libpython_runtime.a
PYTHON_RUNTIME_MAIN_WASM        := $(PYTHON_RUNTIME_CARGO_TARGET)/main.wasm
PYTHON_RUNTIME_STUBS_O          := $(PYTHON_RUNTIME_CARGO_TARGET)/stubs.o
PYTHON_RUNTIME_GROW_TABLE_O     := $(PYTHON_RUNTIME_CARGO_TARGET)/grow_table.o
PYTHON_RUNTIME_INIT_DYLD_WASM   := $(PYTHON_RUNTIME_CARGO_TARGET)/init_dyld.wasm
PYTHON_RUNTIME_DYLD_LIBDL_SO    := $(PYTHON_RUNTIME_CARGO_TARGET)/libdl.so
PYTHON_RUNTIME_LIBFFI_SO        := $(PYTHON_RUNTIME_CARGO_TARGET)/libffi.so
PYTHON_RUNTIME_INJECT_CAPTURE   := $(PYTHON_RUNTIME_CARGO_TARGET)/inject-capture/release/inject-capture
PYTHON_RUNTIME_PRE_COMPONENT    := $(PYTHON_RUNTIME_CARGO_TARGET)/python-runtime-pre.wasm

# dyld-libdl is built with --features canonical-dyld so its dlopen
# /dlsym/dlclose/dlerror route through particle:host/dyld@0.1.0 (the
# canonical interface internal/runtime/dyld serves). Separate target
# dir keeps it isolated from any other dyld-libdl build.
PYTHON_RUNTIME_DYLD_LIBDL_CARGO_TARGET := $(CARGO_TARGET_BASE)/python-runtime-dyld-libdl
PYTHON_RUNTIME_DYLD_LIBDL_STATICLIB    := $(PYTHON_RUNTIME_DYLD_LIBDL_CARGO_TARGET)/wasm32-wasip2/release/libdyld_libdl.a

# libffi-wasi-bridge — side module supplying __wasi_libffi_* C entry
# points + the wasm-asm dispatch helper. Composed in the
# python-runtime alongside libdl.so / libc.so / libpython3.14.so so
# its exports show up in the dyld env shim's regular library-source
# list, NOT pulled in via the MAIN_KEPT_EXPORTS curation.
PYTHON_RUNTIME_LIBFFI_CARGO_TARGET := $(CARGO_TARGET_BASE)/python-runtime-libffi-wasi-bridge
PYTHON_RUNTIME_LIBFFI_STATICLIB    := $(PYTHON_RUNTIME_LIBFFI_CARGO_TARGET)/wasm32-wasip2/release/liblibffi_wasi_bridge.a
PYTHON_RUNTIME_LIBFFI_DISPATCH_O   := $(PYTHON_RUNTIME_LIBFFI_CARGO_TARGET)/libffi_dispatch.o

# PyO3 cross-compile config — see the file header for what each field
# means and why we need it (no host Python to introspect against).
PYO3_CONFIG_FILE_ABS := $(abspath components/python-runtime/pyo3-wasm-cpython314.txt)

python-runtime: $(DIST_DIR)/particle-python-runtime.wasm

# ---- Step 1: staticlib (Rust crate → libpython_runtime.a) ----
# Stages wit/  (runtime.wit + python-runtime.wit + shared deps/) so
# wit_bindgen::generate!() in lib.rs can read it. cargo +stable with
# RUSTFLAGS=PIC: stable's prebuilt wasm32-wasip2 rlibs are
# PIC-compatible and don't transitively pull in wasip3's async_support
# imports.

$(PYTHON_RUNTIME_STATICLIB): $(PYTHON_LIB_SO) \
    components/python-runtime/Cargo.toml \
    components/python-runtime/src/lib.rs \
    components/python-runtime/src/host_module.rs \
    components/python-runtime/build.rs \
    components/python-runtime/pyo3-wasm-cpython314.txt \
    wit/runtime.wit wit/python-runtime.wit
	@mkdir -p $(DIST_DIR) $(PYTHON_RUNTIME_CARGO_TARGET) components/python-runtime/wit/deps
	cat wit/runtime.wit wit/python-runtime.wit > components/python-runtime/wit/world.wit
	cp -R wit/deps/* components/python-runtime/wit/deps/ 2>/dev/null || true
	RUSTFLAGS='-C relocation-model=pic' \
	CARGO_TARGET_DIR=$(PYTHON_RUNTIME_CARGO_TARGET) \
	PYO3_CONFIG_FILE=$(PYO3_CONFIG_FILE_ABS) \
	WASI_SDK_PATH=$(WASI_SDK_PATH) \
	  cargo +stable build \
	    --manifest-path components/python-runtime/Cargo.toml \
	    --target wasm32-wasip2 --release -j 1
	@printf '✓  staticlib: '; ls -lh $@ | awk '{print $$5"  "$$NF}'

# ---- Step 2: dyld-libdl staticlib ----
# The dyld-libdl crate has a checked-in wit/world.wit but its wit/deps/
# tree is copied from the shared root at build time (same shape as
# every other crate here).

$(PYTHON_RUNTIME_DYLD_LIBDL_STATICLIB): \
    components/dyld-libdl/Cargo.toml \
    components/dyld-libdl/src/lib.rs \
    components/dyld-libdl/wit/world.wit \
    wit/deps/particle-host/host.wit
	@mkdir -p $(PYTHON_RUNTIME_DYLD_LIBDL_CARGO_TARGET) components/dyld-libdl/wit/deps
	cp -R wit/deps/* components/dyld-libdl/wit/deps/ 2>/dev/null || true
	RUSTFLAGS='-C relocation-model=pic' \
	CARGO_TARGET_DIR=$(PYTHON_RUNTIME_DYLD_LIBDL_CARGO_TARGET) \
	  cargo +stable build \
	    --manifest-path components/dyld-libdl/Cargo.toml \
	    --features canonical-dyld \
	    --target wasm32-wasip2 --release -j 1

# ---- Step 3: link libdl.so ----
# Exports dlopen/dlsym/dlclose/dlerror + __grow_table + cabi_realloc;
# pulls libc via -nostdlib + -lc; -Wl,--shared for side-module layout.

$(PYTHON_RUNTIME_DYLD_LIBDL_SO): $(PYTHON_RUNTIME_DYLD_LIBDL_STATICLIB) $(PYTHON_RUNTIME_GROW_TABLE_O)
	$(WASI_SDK_PATH)/bin/clang \
	  --target=wasm32-wasip2 -fPIC \
	  -nostdlib -nodefaultlibs \
	  -Wl,--shared \
	  -Wl,--no-entry \
	  -Wl,--no-export-dynamic \
	  -Wl,--export=dlopen \
	  -Wl,--export=dlsym \
	  -Wl,--export=dlclose \
	  -Wl,--export=dlerror \
	  -Wl,--export=__grow_table \
	  -Wl,--export=cabi_realloc \
	  -Wl,--unresolved-symbols=import-dynamic \
	  -Wl,--allow-undefined \
	  -Wl,--gc-sections \
	  $(PYTHON_RUNTIME_DYLD_LIBDL_STATICLIB) \
	  $(PYTHON_RUNTIME_GROW_TABLE_O) \
	  -L$(WASI_SYSROOT_SHARED) -lc \
	  -o $@
	@printf '✓  libdl.so: '; ls -lh $@ | awk '{print $$5"  "$$NF}'

# ---- Step 3b: libffi-wasi-bridge staticlib + libffi.so ----
# Side module supplying __wasi_libffi_* C entry points. Composed
# alongside libdl.so so its symbols flow through the normal env-shim
# library-source path — not the curated MAIN_KEPT_EXPORTS list.

$(PYTHON_RUNTIME_LIBFFI_STATICLIB): \
    components/libffi-wasi-bridge/Cargo.toml \
    components/libffi-wasi-bridge/src/lib.rs \
    components/libffi-wasi-bridge/wit/world.wit \
    wit/deps/particle-host/host.wit
	@mkdir -p $(PYTHON_RUNTIME_LIBFFI_CARGO_TARGET) components/libffi-wasi-bridge/wit/deps
	cp -R wit/deps/* components/libffi-wasi-bridge/wit/deps/ 2>/dev/null || true
	RUSTFLAGS='-C relocation-model=pic' \
	CARGO_TARGET_DIR=$(PYTHON_RUNTIME_LIBFFI_CARGO_TARGET) \
	  cargo +stable build \
	    --manifest-path components/libffi-wasi-bridge/Cargo.toml \
	    --target wasm32-wasip2 --release -j 1

# libffi_dispatch.s — wasm-asm `call_indirect` helper. Hand-written
# for the same reason as grow_table.s (clang's C type system can't
# express variable-table-index + fixed-signature call_indirect).
$(PYTHON_RUNTIME_LIBFFI_DISPATCH_O): components/libffi-wasi-bridge/src/libffi_dispatch.s
	@mkdir -p $(@D)
	$(WASI_SDK_PATH)/bin/clang --target=wasm32-wasip2 -fPIC -c $< -o $@

$(PYTHON_RUNTIME_LIBFFI_SO): $(PYTHON_RUNTIME_LIBFFI_STATICLIB) $(PYTHON_RUNTIME_LIBFFI_DISPATCH_O)
	$(WASI_SDK_PATH)/bin/clang \
	  --target=wasm32-wasip2 -fPIC \
	  -nostdlib -nodefaultlibs \
	  -Wl,--shared \
	  -Wl,--no-entry \
	  -Wl,--no-export-dynamic \
	  -Wl,--export=__wasi_libffi_call \
	  -Wl,--export=__wasi_libffi_closure_alloc \
	  -Wl,--export=__wasi_libffi_closure_free \
	  -Wl,--export=__wasi_libffi_prep_closure_loc \
	  -Wl,--export=__wasi_libffi_dispatch \
	  -Wl,--export=cabi_realloc \
	  -Wl,--unresolved-symbols=import-dynamic \
	  -Wl,--allow-undefined \
	  -Wl,--gc-sections \
	  $(PYTHON_RUNTIME_LIBFFI_STATICLIB) \
	  $(PYTHON_RUNTIME_LIBFFI_DISPATCH_O) \
	  -L$(WASI_SYSROOT_SHARED) -lc \
	  -o $@
	@printf '✓  libffi.so: '; ls -lh $@ | awk '{print $$5"  "$$NF}'

# ---- Step 4: non-Rust objects ----
# grow_table.s wraps table.grow on __indirect_function_table (wasm-asm;
# clang's C type system can't name funcref tables). stubs.c plugs the
# cabi_realloc_wit_bindgen_0_57_1 + _CLOCK_*_CPUTIME_ID gaps that
# wit-bindgen 0.57.1 + libstd's wasi-pal leave open on wasip2.

$(PYTHON_RUNTIME_GROW_TABLE_O): components/python-runtime/src/grow_table.s
	@mkdir -p $(@D)
	$(WASI_SDK_PATH)/bin/clang --target=wasm32-wasip2 -fPIC -c $< -o $@

$(PYTHON_RUNTIME_STUBS_O): components/python-runtime/src/stubs.c
	@mkdir -p $(@D)
	$(WASI_SDK_PATH)/bin/clang --target=wasm32-wasip2 -fPIC -O2 -c $< -o $@

# ---- Step 5: init_dyld sibling ----
# wasm-tools parse turns the .wat into the binary core module that
# `wasm-tools component link --dl-openable` expects. The .wat imports
# particle:host/dyld@0.1.0/initialize and re-exports it as __init_dyld
# (see the .wat file's header).

$(PYTHON_RUNTIME_INIT_DYLD_WASM): components/python-runtime/src/init_dyld.wat
	@mkdir -p $(@D)
	wasm-tools parse $< -o $@

# ---- Step 6: link main.wasm ----
# Rust staticlib (with libstd's bundled wasi-libc) + the stubs object,
# dynamic deps on libpython3.14.so and libc.so via -lpython3.14 / -lc
# (composition adds the dylink.0 NEEDED entries from the .so files we
# resolve). --whole-archive on the staticlib so wasm-ld doesn't strip
# our WIT export shims as "unused" — they're called through the
# canonical ABI machinery, not from any Rust function in the .a.

$(PYTHON_RUNTIME_MAIN_WASM): $(PYTHON_RUNTIME_STATICLIB) $(PYTHON_RUNTIME_STUBS_O) $(PYTHON_LIB_SO)
	$(WASI_SDK_PATH)/bin/clang \
	  --target=wasm32-wasip2 -fPIC \
	  -nostdlib -nodefaultlibs \
	  -Wl,--shared \
	  -Wl,--no-entry \
	  -Wl,--export-dynamic \
	  -Wl,--export=__wasm_set_libraries \
	  -Wl,--export=cabi_realloc \
	  -Wl,--unresolved-symbols=import-dynamic \
	  -Wl,--allow-undefined \
	  -Wl,--gc-sections \
	  -Wl,--whole-archive $(PYTHON_RUNTIME_STATICLIB) -Wl,--no-whole-archive \
	  $(PYTHON_RUNTIME_STUBS_O) \
	  -L$(CPYTHON_WASI_DIR) -lpython3.14 \
	  -L$(WASI_SYSROOT_SHARED) -lc \
	  -o $@
	@printf '✓  main.wasm: '; ls -lh $@ | awk '{print $$5"  "$$NF}'

# ---- Step 7: inject-capture (Rust tool) ----
# Post-link processor that wires the host's dyld.initialize into a
# start function of an injected core module. See tools/inject-capture/
# for details.

$(PYTHON_RUNTIME_INJECT_CAPTURE): tools/inject-capture/Cargo.toml tools/inject-capture/src/main.rs
	CARGO_TARGET_DIR=$(PYTHON_RUNTIME_CARGO_TARGET)/inject-capture cargo build \
	  --manifest-path tools/inject-capture/Cargo.toml --release

# ---- Step 8: compose the pre-component ----
# `wasm-tools component link` composes main + libpython3.14.so + libc.so
# + libdl.so + wasi-emulated-* + init_dyld sibling into a single
# pre-component. --dl-openable flags every composed library so the
# toolchain-generated init module enumerates them in
# __wasm_set_libraries — which our Rust shim forwards to
# dyld.set-libraries.

$(PYTHON_RUNTIME_PRE_COMPONENT): $(PYTHON_RUNTIME_MAIN_WASM) $(PYTHON_RUNTIME_INIT_DYLD_WASM) $(PYTHON_RUNTIME_DYLD_LIBDL_SO) $(PYTHON_RUNTIME_LIBFFI_SO)
	wasm-tools component link \
	  main.wasm=$(PYTHON_RUNTIME_MAIN_WASM) \
	  --dl-openable libpython3.14.so=$(PYTHON_LIB_SO) \
	  --dl-openable libc.so=$(WASI_SYSROOT_SHARED)/libc.so \
	  --dl-openable libdl.so=$(PYTHON_RUNTIME_DYLD_LIBDL_SO) \
	  --dl-openable libffi.so=$(PYTHON_RUNTIME_LIBFFI_SO) \
	  --dl-openable libwasi-emulated-signal.so=$(WASI_SYSROOT_SHARED)/libwasi-emulated-signal.so \
	  --dl-openable libwasi-emulated-getpid.so=$(WASI_SYSROOT_SHARED)/libwasi-emulated-getpid.so \
	  --dl-openable libwasi-emulated-process-clocks.so=$(WASI_SYSROOT_SHARED)/libwasi-emulated-process-clocks.so \
	  --dl-openable init_dyld.so=$(PYTHON_RUNTIME_INIT_DYLD_WASM) \
	  -o $@
	@printf '✓  pre-inject: '; ls -lh $@ | awk '{print $$5"  "$$NF}'

# ---- Step 9: inject-capture + strip ----
# Inject the capture core module, then wasm-tools strip removes DWARF
# (libpython alone is ~21MB of debug info).
#
# Pass `KEEP_DEBUG=1` to skip the strip step — useful for debugging
# trap stack traces with full DWARF symbols available. The resulting
# .wasm is much larger (~35M vs 13M); re-running `make runtime-embed`
# afterward also re-embeds the debug-enabled wasm into the runtime
# binary, so the debug info reaches wazero at instantiation time.

$(DIST_DIR)/particle-python-runtime.wasm: $(PYTHON_RUNTIME_PRE_COMPONENT) $(PYTHON_RUNTIME_INJECT_CAPTURE)
	$(PYTHON_RUNTIME_INJECT_CAPTURE) $< $@.injected
ifeq ($(KEEP_DEBUG),1)
	@echo "KEEP_DEBUG=1: skipping wasm-tools strip (DWARF preserved)"
	mv $@.injected $@
else
	wasm-tools strip $@.injected -o $@
	rm -f $@.injected
endif
	@printf '✓  '; ls -lh $@ | awk '{print $$5"  "$$NF}'

# =============================================================================
# python-{stdlib,bootstrap} zips — go:embed payloads for the runtime
# =============================================================================
# python-stdlib-zip:  CPython 3.14's install/lib/python3.14 tree, mounted
#                     as a wasi preopen at /usr/local/lib/python3.14 so
#                     the embedded interpreter finds encodings/, etc.
# python-bootstrap-zip: bootstrap.py + particle/* helper modules, mounted
#                     at /runtime; ensure_python_initialized inserts
#                     /runtime onto sys.path before `import bootstrap`.

PYTHON_STDLIB_ZIP    := $(RUNTIME_EMBED_DIR)/python3.14-stdlib.zip
PYTHON_STDLIB_SRC    := $(CPYTHON_WASI_DIR)/install/lib/python3.14
PYTHON_BOOTSTRAP_ZIP := $(RUNTIME_EMBED_DIR)/python-runtime-bootstrap.zip
PYTHON_BOOTSTRAP_SRC := components/python-runtime/python

python-stdlib-zip: $(PYTHON_STDLIB_ZIP)

# Excluded subtrees aren't usable from a particle (no GUI, no test
# harness, no pip bootstrap) — dropping them shrinks the embed from
# ~95MB raw to ~21MB compressed.
#
# We ship bytecode-only (.pyc, no .py) to skip the parse/compile step
# CPython would otherwise run for every stdlib module loaded at
# startup. compileall -b writes legacy-format .pyc at <source>.pyc
# (next to the .py) instead of the PEP 3147 __pycache__/ location;
# combined with the *.py exclusion below, the resulting zip contains
# only bytecode and CPython's FileFinder serves it as a "sourceless"
# module load.
#
# Why the native build's python (not the host's system python): the
# .pyc magic number is interpreter-version-specific, and using the
# CPython we just built against this exact source tree guarantees
# the magic byte the wasi cross-build expects. Bytecode format
# itself is platform-independent — host-built bytecode runs on the
# wasi interpreter verbatim.
$(PYTHON_STDLIB_ZIP): $(PYTHON_LIB_SO) tools/zipdir/main.go
	@mkdir -p $(@D)
	$(CPYTHON_NATIVE_DIR)/python -m compileall -b -q -j 0 $(PYTHON_STDLIB_SRC)
	go run ./tools/zipdir \
	  -exclude 'test' -exclude 'idlelib' -exclude 'tkinter' \
	  -exclude 'ensurepip' -exclude '__pycache__' -exclude 'lib2to3' \
	  -exclude 'distutils' -exclude 'turtledemo' -exclude 'turtle.py' \
	  -exclude 'wsgiref' -exclude '*.py' \
	  $(PYTHON_STDLIB_SRC) $@
	@printf '✓  stdlib zip: '; ls -lh $@ | awk '{print $$5"  "$$NF}'

python-bootstrap-zip: $(PYTHON_BOOTSTRAP_ZIP)

$(PYTHON_BOOTSTRAP_ZIP): $(PYTHON_BOOTSTRAP_SRC)/bootstrap.py $(wildcard $(PYTHON_BOOTSTRAP_SRC)/particle/*.py) tools/zipdir/main.go
	@mkdir -p $(@D)
	go run ./tools/zipdir -exclude '__pycache__' -exclude '*.pyc' -exclude '*.disabled' \
	  $(PYTHON_BOOTSTRAP_SRC) $@
	@printf '✓  bootstrap zip: '; ls -lh $@ | awk '{print $$5"  "$$NF}'

# =============================================================================
# clean
# =============================================================================
# Wipes dist/, every off-workspace cargo target dir, the assembled wit/
# staging trees, and the CPython build tree. The source-tarball cache
# at $(CACHE_DIR) survives — it's keyed on upstream version, so
# `make clean && make python-lib` reuses what's already downloaded.

clean:
	rm -rf $(DIST_DIR)
	rm -rf components/deno-npm/target
	rm -rf components/js-runtime/build
	rm -rf components/typecheck/build components/typecheck/node_modules
	rm -rf components/wasm-example/wit
	rm -rf components/python-runtime/wit
	rm -rf components/dyld-libdl/wit/deps
	rm -rf components/libffi-wasi-bridge/wit/deps
	rm -rf $(JS_RUNTIME_CARGO_TARGET) $(TYPECHECK_CARGO_TARGET) \
	       $(PIP_RESOLVE_CARGO_TARGET) $(WASM_EXAMPLE_CARGO_TARGET) \
	       $(PYTHON_RUNTIME_CARGO_TARGET) $(PYTHON_RUNTIME_DYLD_LIBDL_CARGO_TARGET) \
	       $(PYTHON_RUNTIME_LIBFFI_CARGO_TARGET)
	rm -rf $(PYBUILD_DIR)
