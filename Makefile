# Particle — top-level build orchestrator.
#
#   make            build every component to dist/   (default)
#   make deno-npm   build dist/deno-npm.wasm
#   make runtime    build dist/particle-runtime.wasm
#   make typecheck  build dist/particle-typecheck.wasm
#   make clean      wipe dist/ and intermediate build dirs
#
# Targets are .PHONY because cargo/esbuild/wasm-rquickjs each handle their
# own incremental rebuilds — re-running them when nothing changed is fast,
# and tracking individual source files in make would be brittle.

DIST_DIR := dist

# Cargo target dirs for the QuickJS-hosted components are forced off
# /workspace. rquickjs-sys's build script unpacks the WASI SDK tarball with
# utime() calls, which virtiofs (the workspace mount on this dev container)
# blocks.
RUNTIME_CARGO_TARGET   := $(HOME)/cargo-target/runtime
TYPECHECK_CARGO_TARGET := $(HOME)/cargo-target/typecheck

# `npm install --no-bin-links` skips the .bin/ symlink dir creation; that's
# the only npm step virtiofs blocks (chmod on the symlink targets fails).
# The package source itself extracts fine and is what we actually bundle.
NPM_INSTALL := npm install --no-bin-links --no-audit --no-fund

.PHONY: all deno-npm runtime typecheck go-test test clean

all: deno-npm runtime typecheck

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
	@echo '[1/3] esbuild  src/runtime.ts  →  build/runtime.js'
	cd components/runtime && esbuild src/runtime.ts \
	  --bundle --format=esm --target=es2022 --platform=neutral \
	  '--external:wasi:*' '--external:particle:*' '--external:node:*' \
	  --outfile=build/runtime.js
	@echo '[2/3] wasm-rquickjs generate-wrapper-crate'
	rm -rf components/runtime/build/crate
	cd components/runtime && wasm-rquickjs generate-wrapper-crate \
	  --js build/runtime.js --wit wit --output build/crate
	@echo '[3/3] cargo build --target wasm32-wasip2 --release  (target dir: $(RUNTIME_CARGO_TARGET))'
	CARGO_TARGET_DIR=$(RUNTIME_CARGO_TARGET) cargo build \
	  --manifest-path components/runtime/build/crate/Cargo.toml \
	  --target wasm32-wasip2 --release -j 1
	cp $(RUNTIME_CARGO_TARGET)/wasm32-wasip2/release/runtime.wasm \
	   $(DIST_DIR)/particle-runtime.wasm
	@printf '✓  '; ls -lh $(DIST_DIR)/particle-runtime.wasm | awk '{print $$5"  "$$NF}'

# ---- particle-typecheck.wasm -------------------------------------------
# Same shape as runtime, but bundles the `typescript` package (~10MB) into
# the JS so the compiler runs inside QuickJS. esbuild --platform=node lets
# us pull in typescript's CommonJS deps and node:* requires (those are
# resolved at runtime by wasm-rquickjs's node-compat shims).

typecheck:
	@mkdir -p $(DIST_DIR) $(TYPECHECK_CARGO_TARGET) components/typecheck/build
	@test -d components/typecheck/node_modules/typescript || \
	  (cd components/typecheck && $(NPM_INSTALL))
	@echo '[1/3] esbuild  src/typecheck.ts  →  build/typecheck.js  (bundles typescript)'
	cd components/typecheck && esbuild src/typecheck.ts \
	  --bundle --format=esm --platform=node --target=es2022 \
	  '--external:wasi:*' '--external:particle:*' \
	  --outfile=build/typecheck.js
	@echo '[2/3] wasm-rquickjs generate-wrapper-crate'
	rm -rf components/typecheck/build/crate
	cd components/typecheck && wasm-rquickjs generate-wrapper-crate \
	  --js build/typecheck.js --wit wit --output build/crate
	@echo '[3/3] cargo build --target wasm32-wasip2 --release  (target dir: $(TYPECHECK_CARGO_TARGET))'
	CARGO_TARGET_DIR=$(TYPECHECK_CARGO_TARGET) cargo build \
	  --manifest-path components/typecheck/build/crate/Cargo.toml \
	  --target wasm32-wasip2 --release -j 1
	cp $(TYPECHECK_CARGO_TARGET)/wasm32-wasip2/release/component.wasm \
	   $(DIST_DIR)/particle-typecheck.wasm
	@printf '✓  '; ls -lh $(DIST_DIR)/particle-typecheck.wasm | awk '{print $$5"  "$$NF}'

clean:
	rm -rf $(DIST_DIR)
	rm -rf components/deno-npm/target
	rm -rf components/runtime/build
	rm -rf components/typecheck/build components/typecheck/node_modules
	rm -rf $(RUNTIME_CARGO_TARGET) $(TYPECHECK_CARGO_TARGET)
