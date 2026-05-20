# Particle — top-level build orchestrator.
#
#   make            build every component to dist/   (default)
#   make deno-npm        build dist/deno-npm.wasm
#   make pip-resolve     build dist/pip-resolve.wasm
#   make js-runtime      build dist/particle-js-runtime.wasm
#   make python-runtime  build dist/particle-python-runtime.wasm
#   make typecheck       build dist/particle-typecheck.wasm
#   make wasm-example    build the native-WASM particle test fixture
#   make embed           build the build-pipeline wasms and copy into
#                        internal/build/wacogo/embed/ for go:embed
#   make clean           wipe dist/ and intermediate build dirs
#
# Targets are .PHONY because cargo/esbuild/wasm-rquickjs each handle their
# own incremental rebuilds — re-running them when nothing changed is fast,
# and tracking individual source files in make would be brittle.

DIST_DIR := dist

# Cargo target dirs for the QuickJS-hosted components are forced off
# /workspace. rquickjs-sys's build script unpacks the WASI SDK tarball with
# utime() calls, which virtiofs (the workspace mount on this dev container)
# blocks.
JS_RUNTIME_CARGO_TARGET := $(HOME)/cargo-target/js-runtime
TYPECHECK_CARGO_TARGET  := $(HOME)/cargo-target/typecheck

# `npm install --no-bin-links` skips the .bin/ symlink dir creation; that's
# the only npm step virtiofs blocks (chmod on the symlink targets fails).
# The package source itself extracts fine and is what we actually bundle.
NPM_INSTALL := npm install --no-bin-links --no-audit --no-fund

BUILD_EMBED_DIR   := internal/build/wacogo/embed
RUNTIME_EMBED_DIR := runtime/embed

.PHONY: all deno-npm pip-resolve js-runtime python-runtime typecheck wasm-example embed runtime-embed go-test test clean

all: deno-npm pip-resolve js-runtime python-runtime typecheck

# `go generate ./internal/build/wacogo/` runs this. Builds the
# build-pipeline wasms and copies them into internal/build/wacogo/embed/.
embed: deno-npm pip-resolve typecheck
	@mkdir -p $(BUILD_EMBED_DIR)
	@echo 'compress (zstd max via tools/embedcompress) → $(BUILD_EMBED_DIR)/'
	go run ./tools/embedcompress $(DIST_DIR)/deno-npm.wasm           $(BUILD_EMBED_DIR)/deno-npm.wasm.zst
	go run ./tools/embedcompress $(DIST_DIR)/pip-resolve.wasm        $(BUILD_EMBED_DIR)/pip-resolve.wasm.zst
	go run ./tools/embedcompress $(DIST_DIR)/particle-typecheck.wasm $(BUILD_EMBED_DIR)/particle-typecheck.wasm.zst
	@printf '✓  embedded:\n'; ls -lh $(BUILD_EMBED_DIR)/*.wasm.zst | awk '{print "    "$$5"  "$$NF}'

# `go generate ./runtime/` runs this. Builds both runtime wasms and
# copies them into runtime/embed/ for go:embed pickup. The
# compression step (tools/embedcompress, klauspost zstd max level)
# shrinks the embedded footprint by ~60%; decompression happens once
# per Runtime.New call.
runtime-embed: js-runtime python-runtime
	@mkdir -p $(RUNTIME_EMBED_DIR)
	@echo 'compress (zstd max via tools/embedcompress) → $(RUNTIME_EMBED_DIR)/'
	go run ./tools/embedcompress $(DIST_DIR)/particle-js-runtime.wasm     $(RUNTIME_EMBED_DIR)/particle-js-runtime.wasm.zst
	go run ./tools/embedcompress $(DIST_DIR)/particle-python-runtime.wasm $(RUNTIME_EMBED_DIR)/particle-python-runtime.wasm.zst
	@printf '✓  embedded:\n'; ls -lh $(RUNTIME_EMBED_DIR)/*.wasm.zst | awk '{print "    "$$5"  "$$NF}'

# Run Go tests for the build-time libraries (internal/importscan, internal/bundle).
test: go-test

go-test:
	go test ./...

# ---- deno-npm.wasm ------------------------------------------------------
# Pure Rust crate using wit-bindgen; cargo's local target/ on virtiofs is fine.

deno-npm:
	@mkdir -p $(DIST_DIR)
	cd components/deno-npm && cargo build --target wasm32-wasip2 --release
	cp components/deno-npm/target/wasm32-wasip2/release/deno_npm_component.wasm \
	   $(DIST_DIR)/deno-npm.wasm
	@printf '✓  '; ls -lh $(DIST_DIR)/deno-npm.wasm | awk '{print $$5"  "$$NF}'

# ---- pip-resolve.wasm --------------------------------------------------
# Python analog of deno-npm: resolves PEP 508 requirements from a
# Particlefile.py's PEP 723 inline metadata block, fetches the
# transitive closure of pure-Python wheels from PyPI. Off-workspace
# cargo target dir mirrors the runtime/typecheck pattern — keeps
# virtiofs out of the build path.

PIP_RESOLVE_CARGO_TARGET := $(HOME)/cargo-target/pip-resolve

pip-resolve:
	@mkdir -p $(DIST_DIR) $(PIP_RESOLVE_CARGO_TARGET)
	CARGO_TARGET_DIR=$(PIP_RESOLVE_CARGO_TARGET) cargo build \
	  --manifest-path components/pip-resolve/Cargo.toml \
	  --target wasm32-wasip2 --release
	cp $(PIP_RESOLVE_CARGO_TARGET)/wasm32-wasip2/release/pip_resolve_component.wasm \
	   $(DIST_DIR)/pip-resolve.wasm
	@printf '✓  '; ls -lh $(DIST_DIR)/pip-resolve.wasm | awk '{print $$5"  "$$NF}'

# ---- particle-js-runtime.wasm ------------------------------------------
# Three steps: TS → JS bundle → wasm-rquickjs wrapper crate → cargo build.
# Target name kept symmetric with `python-runtime` — there's no
# single "runtime" anymore, the host instantiates one of two.

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
	@echo '[5/5] wasm-rquickjs optimize  (Wizer pre-init: bake QuickJS startup into the artifact)'
	wasm-rquickjs optimize \
	  --input  $(JS_RUNTIME_CARGO_TARGET)/wasm32-wasip2/release/runtime.wasm \
	  --output $(DIST_DIR)/particle-js-runtime.wasm
	@printf '✓  '; ls -lh $(DIST_DIR)/particle-js-runtime.wasm | awk '{print $$5"  "$$NF}'

# ---- particle-python-runtime.wasm --------------------------------------
# componentize-py freezes CPython 3.12 + our bootstrap module + the
# WIT bindings into a single component. The user's Particlefile.py is
# NOT seen at this build step — it's loaded at runtime via
# importlib.machinery.SourceFileLoader from /particle/bundle.py (a
# wasi:filesystem preopen the host mounts). See
# components/python-runtime/app/bootstrap.py for the entry module.

python-runtime:
	@mkdir -p $(DIST_DIR) components/python-runtime/build/wit/deps
	@echo '[1/3] assemble wit/  (runtime.wit + python-runtime.wit, plus shared deps/)'
	cat wit/runtime.wit wit/python-runtime.wit > components/python-runtime/build/wit/world.wit
	cp -R wit/deps/* components/python-runtime/build/wit/deps/
	@echo '[2/3] componentize-py bindings (regenerate from build/wit/)'
	rm -rf components/python-runtime/app/wit_world \
	       components/python-runtime/app/componentize_py_types.py \
	       components/python-runtime/app/componentize_py_runtime.pyi \
	       components/python-runtime/app/componentize_py_async_support \
	       components/python-runtime/app/poll_loop.py
	cd components/python-runtime && componentize-py \
	  --wit-path build/wit --world python-runtime bindings app
	@echo '[3/3] componentize-py componentize (freezes CPython + bootstrap into the component)'
	cd components/python-runtime && componentize-py \
	  --wit-path build/wit --world python-runtime componentize \
	  -p app bootstrap -o $(abspath $(DIST_DIR))/particle-python-runtime.wasm
	@printf '✓  '; ls -lh $(DIST_DIR)/particle-python-runtime.wasm | awk '{print $$5"  "$$NF}'

# ---- wasm-example fixture ----------------------------------------------
# Native-WASM particle test fixture used by the build and runtime test
# suites to exercise the `particle build --component` path. Tiny Rust
# component (~100KB) that implements particle:runtime directly — no
# JS/Python engine in the picture.

WASM_EXAMPLE_CARGO_TARGET := $(HOME)/cargo-target/wasm-example

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

# ---- particle-typecheck.wasm -------------------------------------------
# Same shape as js-runtime, but bundles the `typescript` package (~10MB) into
# the JS so the compiler runs inside QuickJS. esbuild --platform=node lets
# us pull in typescript's CommonJS deps and node:* requires (those are
# resolved at runtime by wasm-rquickjs's node-compat shims).

typecheck:
	@mkdir -p $(DIST_DIR) $(TYPECHECK_CARGO_TARGET) components/typecheck/build
	@test -d components/typecheck/node_modules/typescript || \
	  (cd components/typecheck && $(NPM_INSTALL))
	@echo '[1/4] generate src/lib-bundle.ts and src/particle-globals.d.ts'
	cd components/typecheck && node build-lib-bundle.mjs
	cd components/typecheck && node build-particle-globals.mjs
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
	# Skip `wasm-rquickjs optimize` here: pre-initializing snapshots
	# the entire TypeScript compiler heap (~75 MB on top of the
	# unoptimized 15 MB), which we'd then ship inside every binary
	# that runs typechecks. Lazy init on first call costs a small
	# fraction of the actual type-check work, so it isn't worth it.
	cp $(TYPECHECK_CARGO_TARGET)/wasm32-wasip2/release/component.wasm \
	   $(DIST_DIR)/particle-typecheck.wasm
	@printf '✓  '; ls -lh $(DIST_DIR)/particle-typecheck.wasm | awk '{print $$5"  "$$NF}'

clean:
	rm -rf $(DIST_DIR)
	rm -rf components/deno-npm/target
	rm -rf components/js-runtime/build           # includes assembled build/wit/
	rm -rf components/typecheck/build components/typecheck/node_modules
	rm -rf components/python-runtime/build       # includes assembled build/wit/
	rm -rf components/wasm-example/wit           # assembled wit/ tree
	rm -rf components/python-runtime/app/wit_world
	rm -rf components/python-runtime/app/componentize_py_async_support
	rm -f  components/python-runtime/app/componentize_py_types.py
	rm -f  components/python-runtime/app/componentize_py_runtime.pyi
	rm -f  components/python-runtime/app/poll_loop.py
	rm -rf $(JS_RUNTIME_CARGO_TARGET) $(TYPECHECK_CARGO_TARGET) $(PIP_RESOLVE_CARGO_TARGET) $(WASM_EXAMPLE_CARGO_TARGET)
