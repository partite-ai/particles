// Package wacogo wires the build pipeline's WASM-backed phases to the
// wacogo runtime. It owns the lifecycle of the three build-time wasm
// components — deno-npm.wasm, particle-typecheck.wasm,
// particle-introspect.wasm — and exposes a narrow Go API the build
// orchestrator drives directly: Introspect, TypeCheck, ResolveAndFetch.
//
// The wasm artifacts are baked into the binary via go:embed (see
// embed/). Run `go generate ./internal/build/wacogo/` (or `make embed`
// from the repo root) to populate that directory before the first
// build.
package wacogo

//go:generate make -C ../../.. embed

import (
	"bytes"
	"context"
	"embed"
	"fmt"

	wc "github.com/partite-ai/wacogo"
)

// embeddedWasm holds the three build-pipeline wasms baked into the
// binary at compile time. The wasms are committed under embed/ so a
// fresh `go build` produces a working binary without the Rust + npm +
// wasm-rquickjs toolchain on hand; `make embed` (driven by the
// //go:generate directive above) regenerates them after a component
// source change.
//
// `all:embed` is used so the directive succeeds even if the .wasm
// files are absent (e.g., someone wiped them locally) — readEmbedded
// reports a clear error at runtime in that case.
//
//go:embed all:embed
var embeddedWasm embed.FS

// embedded names — must match the file names the Makefile copies into
// internal/build/wacogo/embed/.
const (
	embeddedDenoNpm     = "embed/deno-npm.wasm"
	embeddedTypecheck   = "embed/particle-typecheck.wasm"
	embeddedIntrospect  = "embed/particle-introspect.wasm"
)

// Components holds the loaded wasm artifacts plus the engine that owns
// them. Construct one with New, drive it via the Introspect /
// TypeCheck / ResolveAndFetch methods, and Close it when done.
type Components struct {
	engine *wc.Engine

	denoNpm    *wc.Component
	typecheck  *wc.Component
	introspect *wc.Component
}

// New constructs a Components value by spinning up an Engine and
// loading every embedded artifact. Failure to find an embedded file
// surfaces with a clear "run go generate" message.
func New(ctx context.Context) (*Components, error) {
	e := wc.NewEngine(ctx)
	c := &Components{engine: e}

	type item struct {
		name string
		set  func(*wc.Component)
	}
	for _, it := range []item{
		{embeddedDenoNpm, func(comp *wc.Component) { c.denoNpm = comp }},
		{embeddedTypecheck, func(comp *wc.Component) { c.typecheck = comp }},
		{embeddedIntrospect, func(comp *wc.Component) { c.introspect = comp }},
	} {
		comp, err := loadEmbedded(ctx, e, it.name)
		if err != nil {
			_ = c.Close(ctx)
			return nil, err
		}
		it.set(comp)
	}
	return c, nil
}

// Close releases the underlying engine, which in turn frees every
// loaded component. Safe to call multiple times.
func (c *Components) Close(ctx context.Context) error {
	if c.engine == nil {
		return nil
	}
	err := c.engine.Close(ctx)
	c.engine = nil
	return err
}

func loadEmbedded(ctx context.Context, e *wc.Engine, name string) (*wc.Component, error) {
	data, err := embeddedWasm.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("wacogo: embedded %s missing — run `go generate ./internal/build/wacogo/` (or `make embed`) to build it: %w", name, err)
	}
	comp, err := e.LoadComponent(ctx, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("wacogo: load embedded %s: %w", name, err)
	}
	return comp, nil
}
