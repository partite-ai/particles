package dyld

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/partite-ai/wacogo"
	"github.com/partite-ai/wacogo/host"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	gen "github.com/partite-ai/particles/internal/host/gen/particle/host/dyld"
)

// errNotConfigured surfaces when adapter.Load can't reach the caller's
// core wasm module — happens when Initialize never ran (no
// inject-capture pass / the injected core module's start function
// didn't fire) so a.captured is still nil.
var errNotConfigured = errors.New("dyld adapter: caller core module unavailable")

// AdapterConfig parameterizes NewAdapter.
type AdapterConfig struct {
	// Engine is the wacogo engine — the wazero runtime under it is
	// what we instantiate the env module + .so on. Required.
	Engine *wacogo.Engine

	// FS is the file system the adapter reads .so files from. dyld.load
	// names map to fs.FS paths verbatim.
	FS fs.FS
}

// Adapter implements the gen.Dyld interface. Owns a registry of
// loaded libraries (both runtime-dlopen'd via Load AND build-time
// pre-loaded via SetLibraries). The registry powers cross-library GOT
// resolution and NEEDED processing: when a runtime-loaded .so requests
// env.GOT.func.X / env.GOT.mem.X / or names X as a NEEDED dependency,
// the loader looks up X in the registry before falling back to the
// captured main module.
//
// Concurrency: there is none. A wazero guest is single-threaded by
// construction (one host call at a time per wazero.Runtime, and the
// adapter is held by exactly one *Particle). All registry access
// happens inside host calls invoked from the guest — sequenced. No
// mutex is necessary or used.
type Adapter struct {
	cfg     AdapterConfig
	runtime wazero.Runtime

	// loaded indexes both runtime-dlopen'd libraries (soInst != nil)
	// and build-time pre-loaded ones (soInst == nil, registered via
	// SetLibraries). The two share the same map because a NEEDED
	// dependency may resolve to either kind, and the symbol lookup
	// path is identical.
	loaded map[string]*loadedLibrary

	// loading guards Load against NEEDED cycles. Plain map (no mutex)
	// because the guest can only call Load reentrantly via NEEDED, and
	// that all unwinds on one goroutine.
	loading map[string]bool

	// captured is the inject-capture-injected core module, pinned
	// during component init via Initialize. It re-exports the union
	// of $main's exports (memory, table, stack_pointer, every
	// composed-library symbol $main re-exports) and main.wasm's
	// exports (__grow_table, __wasi_init_tp, dlerror, etc.). Used as
	// the source of "main"-routed env-shim imports and for direct
	// loader-side function lookups (malloc, __grow_table).
	captured api.Module
}

// loadedLibrary records everything the loader needs to resolve future
// references against this library. Populated by Load (for runtime
// dlopen) and SetLibraries (for build-time composed libraries).
//
// Lifecycle fields (soInst, envInst, ...) are nil for pre-loaded
// libraries — those are part of the main component image and aren't
// managed by us. They live as long as the component instance.
type loadedLibrary struct {
	name string

	// exports maps a symbol name to its absolute address — kind-
	// agnostic per wasm-tools' libdl Libraries layout. For function
	// symbols the value is an absolute index into main's
	// __indirect_function_table; for data symbols it's an absolute
	// memory address in main's linear memory. The kind is inferred
	// by the lookup context: resolveFunc reads it as a table index,
	// resolveData as a memory address.
	exports map[string]uint32

	// refCount counts user dlopen()s + edges from NEEDED parents.
	// Each Load (whether triggered by a user dlopen or by another
	// library's NEEDED) increments by exactly 1. Drop decrements.
	// At 0 the lifecycle modules close and the entry is removed
	// from a.loaded; pre-loaded entries (soInst == nil) refuse to
	// refcount — they belong to the component image, not to us.
	refCount int

	// neededDeps lists libraries this one pulled in via its dylink.0
	// NEEDED entries. When this library's refCount drops to 0 we
	// release one reference on each — the matching `Load` performed
	// during this library's initial load.
	neededDeps []*loadedLibrary

	// Lifecycle handles (runtime loads only; preloaded entries have
	// these as nil). Closed when refCount reaches 0.
	soInst, envInst, gotFuncInst, gotMemInst, postShimInst api.Module
}

// isPreloaded distinguishes build-time-composed entries from
// runtime-loaded ones. Pre-loaded libraries have no soInst because
// their symbols already live in the composed main component — we
// only mirror their exports for resolution.
func (l *loadedLibrary) isPreloaded() bool { return l.soInst == nil }

// mainSatisfiedNeeded is the set of NEEDED library names we treat as
// "already provided by main" without needing a SetLibraries entry or
// runtime load. These are the wasi-libc family that every guest .so
// references and that wasm-tools' composition step already wired
// into main statically.
var mainSatisfiedNeeded = map[string]bool{
	"libc.so":                            true,
	"libdl.so":                           true,
	"libpthread.so":                      true,
	"libm.so":                            true,
	"libc++.so":                          true,
	"libc++abi.so":                       true,
	"libwasi-emulated-signal.so":         true,
	"libwasi-emulated-getpid.so":         true,
	"libwasi-emulated-process-clocks.so": true,
}

// NewAdapter builds an Adapter. The returned value must be passed to
// dyld.Factory.NewInstance.
func NewAdapter(cfg AdapterConfig) (*Adapter, error) {
	if cfg.Engine == nil {
		return nil, errors.New("dyld adapter: Engine is required")
	}
	if cfg.FS == nil {
		return nil, errors.New("dyld adapter: FS is required")
	}
	return &Adapter{
		cfg:     cfg,
		runtime: cfg.Engine.WazeroRuntime(),
		loaded:  make(map[string]*loadedLibrary),
		loading: make(map[string]bool),
	}, nil
}

// resolveFunc looks up name as a function symbol across (1) loaded
// libraries (both runtime and pre-loaded — pre-loaded entries carry
// resolved table indices in their exports map) and (2) the captured
// main module's wasm exports. Returns the absolute table index and
// whether the registry already has it placed.
//
// foundInLib=true means the registry's tableIdx is authoritative; the
// caller should NOT re-register. foundInLib=false + mainHas=true
// means main exports the symbol directly — the GOT.func builder will
// import it from main and elem-place at a fresh slot.
func (a *Adapter) resolveFunc(name string, main api.Module) (tableIdx uint32, foundInLib bool, mainHas bool) {
	for _, lib := range a.loaded {
		if idx, ok := lib.exports[name]; ok {
			return idx, true, false
		}
	}
	if main.ExportedFunction(name) != nil {
		return 0, false, true
	}
	return 0, false, false
}

// resolveData looks up name as a data symbol. Data-symbol globals
// exported by wasm-ld --export-dynamic carry the i32 address as
// their value; the caller interprets the returned u32 as a memory
// address.
func (a *Adapter) resolveData(name string, main api.Module) (addr uint32, found bool) {
	for _, lib := range a.loaded {
		if address, ok := lib.exports[name]; ok {
			return address, true
		}
	}
	if g := main.ExportedGlobal(name); g != nil {
		return uint32(g.Get()), true
	}
	return 0, false
}

// FS exposes the file system the adapter was constructed with — the
// tests use this to assemble an embedded FS containing test .so's.
func (a *Adapter) FS() fs.FS { return a.cfg.FS }

// -----------------------------------------------------------------------------
// gen.Dyld implementation
// -----------------------------------------------------------------------------

var _ gen.Dyld = (*Adapter)(nil)

// Load is the wasm-side `dyld.load(name)` entry point. Each successful
// Load returns a fresh handle whose Drop decrements the library's
// refcount; the lifecycle modules tear down when the refcount reaches
// 0 (and the underlying library's own NEEDED edges then cascade).
func (a *Adapter) Load(ctx context.Context, name string) (gen.ResultLibraryDyldError, error) {
	if a.captured == nil {
		return gen.ResultLibraryDyldErrorErr{Value: gen.DyldErrorNotConfigured{}}, nil
	}

	lib, err := a.load(ctx, a.captured, name)
	if err != nil {
		if errors.Is(err, errNotConfigured) {
			return gen.ResultLibraryDyldErrorErr{Value: gen.DyldErrorNotConfigured{}}, nil
		}
		return gen.ResultLibraryDyldErrorErr{
			Value: gen.DyldErrorLoadFailed{Value: err.Error()},
		}, nil
	}
	return gen.ResultLibraryDyldErrorOk{Value: gen.NewLibraryHandle(lib)}, nil
}

// Initialize runs once during component instantiation, fired by the
// inject-capture-injected core module's start function. That module
// imports the union of $main's and main.wasm's exports (memory,
// __indirect_function_table, __stack_pointer, every libc/libpython
// function $main re-exports, plus main.wasm-only stubs like
// __grow_table / __wasi_init_tp / dlerror / cabi_realloc), re-exports
// them, and calls this WIT function from its start. The wasm caller
// of the cross-component call IS the injected module — host.CallerCoreModule
// hands us a single Module that satisfies everything the env shim needs.
func (a *Adapter) Initialize(ctx context.Context) error {
	captured := host.CallerCoreModule(ctx)
	if captured == nil {
		return errors.New("dyld.initialize: no caller core module on ctx")
	}
	a.captured = captured
	return nil
}

// SetLibraries replaces the adapter's registry of pre-loaded
// libraries with the provided list. Driven by main's
// `__wasm_set_libraries(ptr)` Rust shim, which reads wasm-tools'
// LIBRARIES struct from memory and marshals it into the canon-ABI
// records this method receives.
//
// Runtime-loaded entries (soInst != nil) are preserved; only the
// pre-loaded set is replaced.
func (a *Adapter) SetLibraries(ctx context.Context, libraries []gen.PreloadedLibrary) (gen.Result_String, error) {
	for name, lib := range a.loaded {
		if lib.isPreloaded() {
			delete(a.loaded, name)
		}
	}
	for _, lib := range libraries {
		entry := &loadedLibrary{
			name:    lib.Name,
			exports: make(map[string]uint32, len(lib.Symbols)),
		}
		for _, sym := range lib.Symbols {
			entry.exports[sym.Name] = sym.Address
		}
		a.loaded[lib.Name] = entry
	}
	return gen.Result_StringOk{}, nil
}

// -----------------------------------------------------------------------------
// gen.Library implementation
// -----------------------------------------------------------------------------

// libraryImpl backs each `library` resource handed back to dlopen's
// caller. One libraryImpl per outstanding handle; the underlying
// *loadedLibrary in a.loaded is shared and refcounted across them.
type libraryImpl struct {
	a   *Adapter
	lib *loadedLibrary
}

var _ gen.Library = (*libraryImpl)(nil)

func (l *libraryImpl) Symbol(ctx context.Context, name string) (gen.ResultU32String, error) {
	if addr, ok := l.lib.exports[name]; ok {
		// For dlsym-style use, callers expect a table index they can
		// invoke as a function pointer. Data symbols would also be
		// returned here — POSIX dlsym doesn't distinguish; the caller
		// knows the symbol's kind.
		return gen.ResultU32StringOk{Value: addr}, nil
	}
	return gen.ResultU32StringErr{
		Value: fmt.Sprintf("symbol %q not exported by %s", name, l.lib.name),
	}, nil
}

// Drop is invoked by the resource destructor when the wasm side drops
// the library handle. We decrement the library's refcount; if it
// reaches zero we tear down the lifecycle modules and cascade-drop
// NEEDED dependencies. Matches Unix dlopen/dlclose semantics:
// dropping a library releases one ref per `parent → child` NEEDED
// edge, transitively closing children whose refcount drops to 0.
//
// Pre-loaded entries are not refcounted — they belong to the
// component image, so Drop on them is a no-op.
func (l *libraryImpl) Drop() {
	if l.lib.isPreloaded() {
		return
	}
	l.a.unref(l.lib)
}

// unref is the refcount decrement path shared by libraryImpl.Drop
// and the NEEDED cascade. Closes lifecycle modules and removes the
// registry entry on hitting zero, then unrefs every NEEDED dep so
// closures propagate.
func (a *Adapter) unref(lib *loadedLibrary) {
	lib.refCount--
	if lib.refCount > 0 {
		return
	}
	ctx := context.Background()
	for _, m := range []api.Module{lib.postShimInst, lib.soInst, lib.gotFuncInst, lib.gotMemInst, lib.envInst} {
		if m != nil {
			_ = m.Close(ctx)
		}
	}
	delete(a.loaded, lib.name)
	// Release the NEEDED references this library held. Captured in
	// load() as one ref per child; releasing them here is what makes
	// `dlclose(parent)` transitively close children whose only
	// refcount came from the parent edge.
	for _, dep := range lib.neededDeps {
		a.unref(dep)
	}
}
