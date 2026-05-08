# Particle — top-level build orchestrator.
#
#   make            build every component to dist/   (default)
#   make deno-npm    build dist/deno-npm.wasm
#   make runtime     build dist/particle-runtime.wasm
#   make introspect  build dist/particle-introspect.wasm
#   make typecheck   build dist/particle-typecheck.wasm
#   make embed       build the build-pipeline wasms and copy into
#                    internal/build/wacogo/embed/ for go:embed
#   make clean       wipe dist/ and intermediate build dirs
#
# Targets are .PHONY because cargo/esbuild/wasm-rquickjs each handle their
# own incremental rebuilds — re-running them when nothing changed is fast,
# and tracking individual source files in make would be brittle.

DIST_DIR := dist

# Cargo target dirs for the QuickJS-hosted components are forced off
# /workspace. rquickjs-sys's build script unpacks the WASI SDK tarball with
# utime() calls, which virtiofs (the workspace mount on this dev container)
# blocks.
RUNTIME_CARGO_TARGET    := $(HOME)/cargo-target/runtime
INTROSPECT_CARGO_TARGET := $(HOME)/cargo-target/introspect
TYPECHECK_CARGO_TARGET  := $(HOME)/cargo-target/typecheck

# `npm install --no-bin-links` skips the .bin/ symlink dir creation; that's
# the only npm step virtiofs blocks (chmod on the symlink targets fails).
# The package source itself extracts fine and is what we actually bundle.
NPM_INSTALL := npm install --no-bin-links --no-audit --no-fund

BUILD_EMBED_DIR   := internal/build/wacogo/embed
RUNTIME_EMBED_DIR := runtime/embed

.PHONY: all deno-npm runtime introspect typecheck embed runtime-embed go-test test clean

all: deno-npm runtime introspect typecheck

# `go generate ./internal/build/wacogo/` runs this. Builds the three
# build-pipeline wasms and copies them into internal/build/wacogo/embed/.
embed: deno-npm introspect typecheck
	@mkdir -p $(BUILD_EMBED_DIR)
	cp $(DIST_DIR)/deno-npm.wasm           $(BUILD_EMBED_DIR)/
	cp $(DIST_DIR)/particle-introspect.wasm $(BUILD_EMBED_DIR)/
	cp $(DIST_DIR)/particle-typecheck.wasm  $(BUILD_EMBED_DIR)/
	@printf '✓  embedded:\n'; ls -lh $(BUILD_EMBED_DIR)/*.wasm | awk '{print "    "$$5"  "$$NF}'

# `go generate ./runtime/` runs this. Builds the runtime wasm and
# copies it into runtime/embed/ for go:embed pickup.
runtime-embed: runtime
	@mkdir -p $(RUNTIME_EMBED_DIR)
	cp $(DIST_DIR)/particle-runtime.wasm $(RUNTIME_EMBED_DIR)/
	@printf '✓  embedded:\n'; ls -lh $(RUNTIME_EMBED_DIR)/*.wasm | awk '{print "    "$$5"  "$$NF}'

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

# ---- particle-runtime.wasm ---------------------------------------------
# Three steps: TS → JS bundle → wasm-rquickjs wrapper crate → cargo build.

runtime:
	@mkdir -p $(DIST_DIR) $(RUNTIME_CARGO_TARGET) components/runtime/build
	@echo '[1/4] esbuild  src/runtime.ts  →  build/runtime.js'
	cd components/runtime && esbuild src/runtime.ts \
	  --bundle --format=esm --target=es2022 --platform=neutral \
	  '--external:wasi:*' '--external:particle:*' '--external:node:*' \
	  --outfile=build/runtime.js
	@echo '[2/4] wasm-rquickjs generate-wrapper-crate'
	rm -rf components/runtime/build/crate
	cd components/runtime && wasm-rquickjs generate-wrapper-crate \
	  --js build/runtime.js --wit wit --output build/crate
	@echo '[3/4] cargo build --target wasm32-wasip2 --release  (target dir: $(RUNTIME_CARGO_TARGET))'
	CARGO_TARGET_DIR=$(RUNTIME_CARGO_TARGET) cargo build \
	  --manifest-path components/runtime/build/crate/Cargo.toml \
	  --target wasm32-wasip2 --release -j 1
	@echo '[4/4] wasm-rquickjs optimize  (Wizer pre-init: bake QuickJS startup into the artifact)'
	wasm-rquickjs optimize \
	  --input  $(RUNTIME_CARGO_TARGET)/wasm32-wasip2/release/runtime.wasm \
	  --output $(DIST_DIR)/particle-runtime.wasm
	@printf '✓  '; ls -lh $(DIST_DIR)/particle-runtime.wasm | awk '{print $$5"  "$$NF}'

# ---- particle-introspect.wasm ------------------------------------------
# Build-time component (Phase 5 of the build pipeline). Same toolchain as
# the runtime — TS → JS bundle → wasm-rquickjs wrapper crate → cargo build
# — but a separate artifact so the runtime image stays free of build-time
# logic and the two evolve independently.

introspect:
	@mkdir -p $(DIST_DIR) $(INTROSPECT_CARGO_TARGET) components/introspect/build
	@echo '[1/3] esbuild  src/introspect.ts  →  build/introspect.js'
	cd components/introspect && esbuild src/introspect.ts \
	  --bundle --format=esm --target=es2022 --platform=neutral \
	  '--external:wasi:*' '--external:particle:*' '--external:node:*' \
	  --outfile=build/introspect.js
	@echo '[2/3] wasm-rquickjs generate-wrapper-crate'
	rm -rf components/introspect/build/crate
	cd components/introspect && wasm-rquickjs generate-wrapper-crate \
	  --js build/introspect.js --wit wit --output build/crate
	@echo '[3/3] cargo build --target wasm32-wasip2 --release  (target dir: $(INTROSPECT_CARGO_TARGET))'
	CARGO_TARGET_DIR=$(INTROSPECT_CARGO_TARGET) cargo build \
	  --manifest-path components/introspect/build/crate/Cargo.toml \
	  --target wasm32-wasip2 --release -j 1
	# Skip `wasm-rquickjs optimize` here: introspect runs once per
	# particle build, so saving the ~100 ms QuickJS warmup isn't
	# worth the ~6 MB pre-init bloat. The wrapper crate's
	# get_js_state lazy-inits on first export call.
	cp $(INTROSPECT_CARGO_TARGET)/wasm32-wasip2/release/component.wasm \
	   $(DIST_DIR)/particle-introspect.wasm
	@printf '✓  '; ls -lh $(DIST_DIR)/particle-introspect.wasm | awk '{print $$5"  "$$NF}'

# ---- particle-typecheck.wasm -------------------------------------------
# Same shape as runtime, but bundles the `typescript` package (~10MB) into
# the JS so the compiler runs inside QuickJS. esbuild --platform=node lets
# us pull in typescript's CommonJS deps and node:* requires (those are
# resolved at runtime by wasm-rquickjs's node-compat shims).

typecheck:
	@mkdir -p $(DIST_DIR) $(TYPECHECK_CARGO_TARGET) components/typecheck/build
	@test -d components/typecheck/node_modules/typescript || \
	  (cd components/typecheck && $(NPM_INSTALL))
	@echo '[1/4] generate src/lib-bundle.ts (lib.*.d.ts → ESM map)'
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
	rm -rf components/runtime/build
	rm -rf components/introspect/build
	rm -rf components/typecheck/build components/typecheck/node_modules
	rm -rf $(RUNTIME_CARGO_TARGET) $(INTROSPECT_CARGO_TARGET) $(TYPECHECK_CARGO_TARGET)
