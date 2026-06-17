//! inject-capture: post-process a wasm component to wire a synthesized
//! `host.initialize` call into a start function of an injected core module.
//!
//! Input:  a composed component produced by `wasm-tools component link`
//!         that contains an embedded `$main` core module (the wit-component
//!         synthesized container that owns memory + __indirect_function_table),
//!         a `$main.wasm` side-module, an `init_dyld.so` re-export sibling,
//!         and several `--dl-openable` side libraries (libc.so,
//!         libpython3.14.so, libdl.so, etc.), plus a component-level import
//!         `particle:host/dyld@0.1.0` whose instance type declares
//!         a function named `initialize` (no params, no results).
//!
//! Output: the same component with an extra core module appended whose
//!         start function invokes the lowered `initialize` import. The
//!         new core module imports memory / __indirect_function_table /
//!         __stack_pointer / __heap_base / __heap_end from $main (under
//!         "main_synth") and every remaining export from each known
//!         dl-openable library (libc.so,
//!         libpython3.14.so, libdl.so, libwasi-emulated-signal.so,
//!         libwasi-emulated-getpid.so, libwasi-emulated-process-clocks.so),
//!         minus per-library lifecycle symbols `__wasm_apply_data_relocs`
//!         and `_initialize`. When the host
//!         runs the start function it can (via the host-side handler)
//!         capture stable references to the caller's memory, table, etc.
//!
//! This is a build-time step in the python-runtime pipeline; see
//! /workspace/Makefile target `dist/particle-python-runtime.wasm`.

use std::collections::{HashMap, HashSet};
use std::fs;
use std::path::PathBuf;
use std::process::ExitCode;

use anyhow::{Context, Result, anyhow, bail};
use wasm_encoder::reencode::{Reencode, RoundtripReencoder};
use wasm_encoder::{
    Alias, CodeSection, Component, ComponentAliasSection, EntityType, ExportKind, ExportSection,
    FuncType as EncFuncType, Function, FunctionSection, GlobalType, ImportSection,
    InstanceSection as CoreInstanceSection, MemoryType, Module, ModuleArg, RawSection,
    StartSection, TableType, TypeSection, ValType,
};
use wasmparser::{
    ComponentTypeRef, Encoding, ExportSectionReader, ExternalKind, FuncType,
    GlobalType as PGlobalType, ImportSectionReader, Instance, KnownCustom,
    MemoryType as PMemoryType, Name, Parser, Payload, TableSectionReader,
    TableType as PTableType, TypeRef, TypeSectionReader, ValType as PValType,
};

fn main() -> ExitCode {
    let args: Vec<String> = std::env::args().collect();
    if args.len() != 3 {
        eprintln!("usage: {} <input.wasm> <output.wasm>", args[0]);
        return ExitCode::from(2);
    }
    let input = PathBuf::from(&args[1]);
    let output = PathBuf::from(&args[2]);

    match run(&input, &output) {
        Ok(()) => ExitCode::SUCCESS,
        Err(e) => {
            eprintln!("inject-capture: {e:#}");
            ExitCode::from(1)
        }
    }
}

fn run(input: &PathBuf, output: &PathBuf) -> Result<()> {
    let bytes = fs::read(input).with_context(|| format!("reading {}", input.display()))?;

    let analysis = analyze(&bytes)?;
    let new_bytes = rewrite(&bytes, &analysis)?;

    fs::write(output, &new_bytes).with_context(|| format!("writing {}", output.display()))?;
    Ok(())
}

// ---------------------------------------------------------------------------
// Analysis pass: walk the input component, locate the two embedded core
// modules of interest and the dyld import's `initialize` function index.
// ---------------------------------------------------------------------------

/// Describes a single export of an embedded core module — name, kind, plus
/// the resolved type info needed to import it back into our injected module.
#[derive(Debug, Clone)]
struct ExportItem {
    name: String,
    desc: EntityDesc,
}

#[derive(Debug, Clone)]
enum EntityDesc {
    /// A function described by its parameter/result types.
    Func(FuncType),
    Table(PTableType),
    Memory(PMemoryType),
    Global(PGlobalType),
}

#[derive(Debug)]
struct Analysis {
    /// Index, in the component's core-INSTANCE index space, of the instance
    /// produced by instantiating `$main`. NOT the same as `main_module_idx`:
    /// the wasm-tools linker emits intervening shim instances, so the two
    /// index spaces diverge.
    main_core_instance_idx: u32,
    /// Exports of $main, in original section order. We only consume three
    /// of these (memory / __indirect_function_table / __stack_pointer);
    /// the 1100+ flattened library-symbol aliases on $main are ignored
    /// because the libraries themselves are also sources now.
    main_exports: Vec<ExportItem>,
    /// Index, in the component's core-INSTANCE index space, of the instance
    /// produced by instantiating the `init_dyld.so` sibling module (a tiny
    /// wat-authored re-exporter — see `components/python-runtime/src/init_dyld.wat`).
    /// Its `__init_dyld` core export resolves to the same lowered
    /// `dyld.initialize` core function wit-bindgen + wasm-tools already
    /// produced for main.wasm; aliasing it gives us a free path to the
    /// initialize import without minting our own component alias / canon.lower.
    init_dyld_core_instance_idx: u32,
    /// Composed dl-openable side libraries, discovered by name (libc.so,
    /// libpython3.14.so, libdl.so, libwasi-emulated-signal.so,
    /// libwasi-emulated-getpid.so, libwasi-emulated-process-clocks.so).
    /// The inject module imports + re-exports the union of every library's
    /// exports (minus the well-known per-library lifecycle symbols
    /// `__wasm_apply_data_relocs` and `_initialize`, which each library
    /// owns its own copy of). Order matches pre-component declaration
    /// order; first-occurrence-wins on duplicate symbol names.
    libraries: Vec<Library>,
    /// Component INSTANCE index (separate space from core instance) of
    /// each `wasi:*` instance import in the pre-component, keyed on the
    /// versioned import name (e.g. "wasi:io/streams@0.2.4").
    wasi_instance_indices: HashMap<String, u32>,
    /// The wasi methods to pre-lower, enumerated from the pre-component's
    /// WIT by [`enumerate_wasi_lowerings`] (every function + resource-drop
    /// of every `wasi:*` interface the host exposes). `wasi_signatures`
    /// is aligned 1:1 with this.
    lowered: Vec<WasiLowering>,
    /// Lowered core wasm signature for each entry in [`Analysis::lowered`],
    /// aligned 1:1 by index. Computed from the pre-component's embedded WIT
    /// via [`compute_wasi_signatures`].
    wasi_signatures: Vec<Option<WasiSig>>,
}

/// A composed dl-openable side library captured by [`analyze`] for inclusion
/// in the inject module's re-export union.
#[derive(Debug)]
struct Library {
    /// Stable name used to address this library in BOTH the inject module's
    /// import-module strings AND the matching component-level "with" arg.
    /// Derived from the wasm name section (`libc.so`, etc.), attached by
    /// `wasm-tools component link --dl-openable NAME=PATH`. We match
    /// against [`DL_OPENABLE_LIBRARY_NAMES`] to identify which embedded
    /// core modules are real side libraries vs. synthetic linker helpers.
    name: String,
    /// Index, in the component's core-INSTANCE index space, of the instance
    /// produced by instantiating this library's core module.
    core_instance_idx: u32,
    /// Full export list of the library, in original section order.
    exports: Vec<ExportItem>,
}

/// $main's module index is toolchain-defined and stable (0).  `init_dyld.so`
/// is identified by its shape (single import, single export named
/// `__init_dyld`) rather than a hard-coded index, because the wasm-tools
/// linker may renumber side modules as the side-library set evolves.
const MAIN_MODULE_IDX: u32 = 0;
const INIT_DYLD_EXPORT_NAME: &str = "__init_dyld";

/// The handful of exports we consume from $main. $main has 1100+ exports
/// (flattened aliases of every library's exports), but only a few items are
/// genuinely unique to $main — every other symbol the dyld env shim wants
/// reaches us via its native library directly:
///
///   memory, __indirect_function_table, __stack_pointer — the shared
///     allocation primitives wasm-tools sets up at composition time.
///   __heap_base, __heap_end — wasm-ld synthetic linker globals
///     (`@since DynamicLinking.md spec; auto-emitted by wasm-ld for
///     position-dependent main binaries`). Runtime-dlopen'd .so files
///     built with `-Wl,--shared` import these via GOT.mem when their
///     Rust allocator or PyO3 internals need a heap-bounds reference.
///     None of the dl-openable libraries below define them — $main does,
///     so this set has to include them or the dyld env shim has no
///     source to route to.
const MAIN_KEPT_EXPORTS: &[&str] = &[
    "memory",
    "__indirect_function_table",
    "__stack_pointer",
    "__heap_base",
    "__heap_end",
    // cabi_realloc is needed by canon.lower's Realloc option for the
    // wasi-lowering pass — it's how lowered wasi methods that produce
    // list/string return values allocate the destination buffer in the
    // .so's memory. main.wasm exports it via wit-bindgen's realloc
    // feature; we surface it here so the rewrite step has a core func
    // idx to feed into the canon.lower options.
    "cabi_realloc",
];

/// The set of dl-openable side libraries we recognize as symbol sources.
/// Match is by exact module-name (from the wasm name section, attached by
/// `wasm-tools component link --dl-openable NAME=PATH`). Embedded core
/// modules NOT in this set are skipped ($main, main.wasm, $__init,
/// $wit-component:stubs, $wit-component-shim-module, $wit-component-fixup,
/// $init_dyld.so).
const DL_OPENABLE_LIBRARY_NAMES: &[&str] = &[
    "libpython3.14.so",
    "libc.so",
    "libdl.so",
    "libffi.so",
    "libwasi-emulated-signal.so",
    "libwasi-emulated-getpid.so",
    "libwasi-emulated-process-clocks.so",
];

/// Per-library lifecycle symbols we MUST skip from every library: each
/// library has its own copy targeting its own data segments, and they're
/// invoked by `$__init`'s start function during instantiation — so they're
/// both ambiguous (multiple definitions) and useless (already called) for
/// downstream resolution. (init_dyld.so is filtered upstream by name.)
const SKIPPED_LIBRARY_EXPORTS: &[&str] = &["__wasm_apply_data_relocs", "_initialize"];

/// Per-method metadata for the wasi-lowering pass.
///
/// `component_import_name` is the literal string used in the
/// pre-component's `(import "..." (instance ...))` for the parent
/// wasi interface — the version is whatever libc.so was built against
/// (0.2.4 at time of writing).
///
/// `module_short_name` is the same string minus the @version suffix
/// and becomes the prefix in the inject module's exports
/// (`wasi-lowered:<module_short_name>#<method>`). dyld looks names up
/// under this prefix at runtime — version-agnostic matching is
/// intentional because a wasm32-wasip2 .so built with stock libstd
/// today still emits `wasi:*@0.2.0` core imports.
///
/// `method` is the canonical wit interface method name as wasm-ld
/// emits in the core imports of a wasm32-wasip2 cdylib
/// (`[method]pollable.block`, `[resource-drop]pollable`, `get-stdin`,
/// etc.).
///
/// `kind` distinguishes a canon.lower lowering (regular methods,
/// top-level functions) from a canon.resource.drop one
/// ([resource-drop]X). The two go through different
/// canonical-function-section ops.
///
/// No hardcoded signature here. For Method entries we resolve the
/// canonical core wasm sig at build time via
/// `wit_parser::Resolve::wasm_signature` against the wit types the
/// pre-component carries in its custom sections. For ResourceDrop
/// entries the signature is always `(i32) → ()`.
///
/// Entries are enumerated from the pre-component's WIT by
/// [`enumerate_wasi_lowerings`], not hand-listed — hence owned
/// `String`s rather than `&'static str`.
#[derive(Debug)]
struct WasiLowering {
    component_import_name: String,
    module_short_name: String,
    method: String,
    kind: WasiLoweringKind,
}

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
enum WasiLoweringKind {
    /// canon.lower against an aliased instance-method func. Sig
    /// computed via `Resolve::wasm_signature(GuestImport, func)`.
    Method,
    /// canon.resource.drop against an aliased instance-type. Sig is
    /// fixed: `(i32) → ()` (the handle to drop).
    ResourceDrop,
}

// Shorthand for clarity.
use WasiLoweringKind::{Method, ResourceDrop};

/// Build the set of wasi methods to pre-lower from the pre-component's
/// own WIT: for every `wasi:*` instance the component imports — i.e.
/// everything the host exposes to a dlopen'd .so, which is the wasi:cli
/// world plus the outgoing http handler — lower every function and a
/// resource-drop for every resource the interface owns.
///
/// Enumerated rather than hand-listed so the lowering set can't drift
/// out of sync with what the host actually provides; a method the .so
/// imports but we didn't lower used to surface as a wasi-shim
/// instantiate failure. Sockets are included: they currently trap at
/// first use (the intended "not supported yet" behavior) and need no
/// change here when real socket support lands.
///
/// `wasi_import_names` is in component-import order (deterministic), so
/// the returned Vec — and thus the whole rewrite — is reproducible.
fn enumerate_wasi_lowerings(
    resolve: &wit_parser::Resolve,
    wasi_import_names: &[String],
) -> Vec<WasiLowering> {
    // Canonical interface id (e.g. "wasi:io/streams@0.2.4") → InterfaceId.
    let mut by_id: HashMap<String, wit_parser::InterfaceId> = HashMap::new();
    for (id, _iface) in resolve.interfaces.iter() {
        if let Some(canonical) = resolve.id_of(id) {
            by_id.insert(canonical, id);
        }
    }

    let mut out = Vec::new();
    for import_name in wasi_import_names {
        let Some(&iface_id) = by_id.get(import_name) else {
            // Imported wasi instance with no matching interface in the
            // embedded WIT — nothing to enumerate. (Shouldn't happen for
            // our composition.)
            continue;
        };
        let iface = &resolve.interfaces[iface_id];
        let module_short = import_name
            .rsplit_once('@')
            .map(|(base, _ver)| base)
            .unwrap_or(import_name)
            .to_string();

        // A resource-drop for every resource this interface OWNS
        // (defines). `use`d-in resources belong to — and are lowered
        // under — their defining interface.
        for (type_name, &type_id) in &iface.types {
            let ty = &resolve.types[type_id];
            if matches!(ty.kind, wit_parser::TypeDefKind::Resource)
                && ty.owner == wit_parser::TypeOwner::Interface(iface_id)
            {
                out.push(WasiLowering {
                    component_import_name: import_name.clone(),
                    module_short_name: module_short.clone(),
                    method: format!("[resource-drop]{type_name}"),
                    kind: ResourceDrop,
                });
            }
        }

        // Every function: methods, constructors, statics, and
        // freestanding interface functions all lower via canon.lower.
        // The map key is exactly the name wasm-ld emits as the core
        // import (`[method]x.y`, `[constructor]x`, `[static]x.y`,
        // `get-stdin`, …).
        for func_name in iface.functions.keys() {
            out.push(WasiLowering {
                component_import_name: import_name.clone(),
                module_short_name: module_short.clone(),
                method: func_name.clone(),
                kind: Method,
            });
        }
    }
    out
}

/// Strips the `[resource-drop]` prefix from a method name and returns
/// the underlying resource type name. Returns None for non-drop methods.
fn resource_drop_type_name(method: &str) -> Option<&str> {
    method.strip_prefix("[resource-drop]")
}

/// Per-WASI_LOWERED_METHODS-entry canonical-lowered core wasm signature.
/// Computed by [`compute_wasi_signatures`] from the pre-component's
/// embedded WIT and threaded through `build_inject_module` so the
/// inject module declares each import with the exact shape canon.lower
/// will produce at the component layer.
#[derive(Clone, Debug)]
struct WasiSig {
    params: Vec<ValType>,
    results: Vec<ValType>,
}

/// For each entry in `lowered`, runs the canonical ABI lowering
/// (`Resolve::wasm_signature(GuestImport, func)`) to get the core wasm
/// signature canon.lower will produce, so the inject module declares
/// each import with the exact shape it'll get at the component layer.
///
/// `[resource-drop]X` entries don't go through wit lookup — the
/// canonical ABI fixes their signature at `(i32) → ()`.
///
/// Returns a Vec aligned 1:1 with `lowered`. `None` only if an entry's
/// interface/method somehow isn't in the resolve; since `lowered` is
/// itself enumerated from this same resolve, that shouldn't happen.
fn compute_wasi_signatures(
    resolve: &wit_parser::Resolve,
    lowered: &[WasiLowering],
) -> Vec<Option<WasiSig>> {
    // Build an InterfaceId-by-canonical-id index once.
    let mut by_id: HashMap<String, wit_parser::InterfaceId> = HashMap::new();
    for (id, _iface) in resolve.interfaces.iter() {
        if let Some(canonical) = resolve.id_of(id) {
            by_id.insert(canonical, id);
        }
    }

    let mut out: Vec<Option<WasiSig>> = Vec::with_capacity(lowered.len());
    for m in lowered {
        match m.kind {
            WasiLoweringKind::ResourceDrop => {
                out.push(Some(WasiSig {
                    params: vec![ValType::I32],
                    results: vec![],
                }));
            }
            WasiLoweringKind::Method => {
                let Some(&iface_id) = by_id.get(m.component_import_name.as_str()) else {
                    out.push(None);
                    continue;
                };
                let iface = &resolve.interfaces[iface_id];
                let Some(func) = iface.functions.get(&m.method) else {
                    out.push(None);
                    continue;
                };
                let sig = resolve
                    .wasm_signature(wit_parser::abi::AbiVariant::GuestImport, func);
                out.push(Some(WasiSig {
                    params: sig.params.iter().map(wasm_type_to_val_type).collect(),
                    results: sig.results.iter().map(wasm_type_to_val_type).collect(),
                }));
            }
        }
    }
    out
}

fn wasm_type_to_val_type(t: &wit_parser::abi::WasmType) -> ValType {
    use wit_parser::abi::WasmType::*;
    match t {
        I32 | Pointer | Length => ValType::I32,
        I64 | PointerOrI64 => ValType::I64,
        F32 => ValType::F32,
        F64 => ValType::F64,
    }
}


fn analyze(bytes: &[u8]) -> Result<Analysis> {
    // Walk the component once to capture the byte ranges of every top-level
    // embedded core module and track module-to-instance bindings. Track
    // depth so we only count top-level ModuleSection payloads as core
    // modules of this component (nested modules inside nested components
    // belong to those components, not us).
    let mut depth: u32 = 0;
    let mut core_module_idx: u32 = 0;
    // (module_idx, byte_range) for every top-level core module, in order.
    let mut module_ranges: Vec<(u32, std::ops::Range<usize>)> = Vec::new();
    // Identify init_dyld.so by shape (single import, single export named
    // `__init_dyld`). This is more robust than a hard-coded index against
    // a moving target — the linker may renumber side modules as the
    // --dl-openable set evolves.
    let mut init_dyld_module_idx: Option<u32> = None;

    // Core-instance index space tracking. The core-instance and core-module
    // index spaces are DISTINCT: a component may instantiate a shim module
    // first (consuming an instance index but not a "main" module index of
    // ours), and FromExports instances also consume instance indices without
    // referencing any module. We must therefore look up the actual instance
    // index for each module of interest by walking InstanceSection entries.
    let mut next_core_instance_idx: u32 = 0;
    let mut module_idx_to_instance_idx: HashMap<u32, u32> = HashMap::new();

    // Component INSTANCE index space — distinct from the core one. Each
    // top-level component import of instance kind consumes one slot; each
    // ComponentInstanceSection entry consumes one. We need the indices
    // assigned to the wasi:* instance imports to alias methods out of
    // them in the rewrite step.
    let mut next_component_instance_idx: u32 = 0;
    let mut wasi_instance_indices: HashMap<String, u32> = HashMap::new();
    // `wasi:*` instance import names in component-import order — drives
    // deterministic enumeration of the lowering set below.
    let mut wasi_import_names: Vec<String> = Vec::new();


    for payload in Parser::new(0).parse_all(bytes) {
        let payload = payload.context("parsing input component")?;
        match payload {
            Payload::Version { encoding, .. } => {
                if depth == 0 && encoding != Encoding::Component {
                    bail!("input must be a wasm component, got {:?}", encoding);
                }
            }
            Payload::ModuleSection {
                unchecked_range, ..
            } => {
                if depth == 0 {
                    if is_init_dyld_module(&bytes[unchecked_range.clone()])? {
                        if init_dyld_module_idx.is_some() {
                            bail!(
                                "found multiple candidate `init_dyld.so` modules \
                                 (one import + single export `{INIT_DYLD_EXPORT_NAME}`); \
                                 cannot disambiguate",
                            );
                        }
                        init_dyld_module_idx = Some(core_module_idx);
                    }
                    module_ranges.push((core_module_idx, unchecked_range.clone()));
                    core_module_idx += 1;
                }
                depth += 1;
            }
            Payload::ComponentSection { .. } => {
                depth += 1;
            }
            Payload::End(_) => {
                if depth > 0 {
                    depth -= 1;
                }
            }
            Payload::InstanceSection(s) if depth == 0 => {
                // Core instance section. Each entry consumes one slot in the
                // core-instance index space. `Instance::Instantiate` records
                // which module is being instantiated; `Instance::FromExports`
                // doesn't reference a module (it bundles existing core items
                // into an instance shape) but still consumes an instance idx.
                for inst in s {
                    let inst = inst?;
                    if let Instance::Instantiate { module_index, .. } = inst {
                        // Record only the FIRST instance for each module idx,
                        // in case a module is instantiated more than once.
                        module_idx_to_instance_idx
                            .entry(module_index)
                            .or_insert(next_core_instance_idx);
                    }
                    next_core_instance_idx += 1;
                }
            }
            Payload::ComponentImportSection(s) if depth == 0 => {
                // Top-level component imports. Only Instance-kind entries
                // consume the component instance index space.
                for imp in s {
                    let imp = imp?;
                    if let ComponentTypeRef::Instance(_) = imp.ty {
                        let idx = next_component_instance_idx;
                        next_component_instance_idx += 1;
                        // Record every `wasi:*` instance import: these are
                        // exactly the interfaces the host exposes to a
                        // dlopen'd .so, and the full set we lower.
                        if imp.name.name.starts_with("wasi:") {
                            let name = imp.name.name.to_string();
                            if !wasi_instance_indices.contains_key(&name) {
                                wasi_instance_indices.insert(name.clone(), idx);
                                wasi_import_names.push(name);
                            }
                        }
                    }
                }
            }
            Payload::ComponentInstanceSection(s) if depth == 0 => {
                for inst in s {
                    let _ = inst?;
                    next_component_instance_idx += 1;
                }
            }
            _ => {}
        }
    }

    let init_dyld_module_idx = init_dyld_module_idx.ok_or_else(|| {
        anyhow!(
            "could not locate the `init_dyld.so` sibling module \
             (looking for a core module with one import and a single \
             export named `{INIT_DYLD_EXPORT_NAME}`)",
        )
    })?;

    // Index module ranges for direct lookup of $main / $main.wasm.
    let find_range = |idx: u32| -> Option<std::ops::Range<usize>> {
        module_ranges
            .iter()
            .find_map(|(i, r)| if *i == idx { Some(r.clone()) } else { None })
    };
    let main_range = find_range(MAIN_MODULE_IDX).ok_or_else(|| {
        anyhow!(
            "could not locate embedded core module #{MAIN_MODULE_IDX} ($main) in input component"
        )
    })?;

    let main_exports =
        scan_core_module_exports(&bytes[main_range]).context("scanning $main exports")?;

    // Discover dl-openable side libraries by NAME (read from the wasm name
    // section, attached by `wasm-tools component link --dl-openable
    // NAME=PATH`). Synthetic linker-generated modules (`$__init`,
    // `$wit-component:stubs`, `$wit-component-shim-module`,
    // `$wit-component-fixup`), the $main container, the tiny `main.wasm`
    // side module, and the `init_dyld.so` re-export sibling are skipped
    // implicitly because their names don't appear in
    // [`DL_OPENABLE_LIBRARY_NAMES`].
    let mut libraries: Vec<Library> = Vec::new();
    for (idx, range) in &module_ranges {
        let module_bytes = &bytes[range.clone()];
        let Some(name) = read_module_name(module_bytes)? else {
            continue;
        };
        if !DL_OPENABLE_LIBRARY_NAMES.iter().any(|n| *n == name.as_str()) {
            continue;
        }
        let core_instance_idx = match module_idx_to_instance_idx.get(idx) {
            Some(i) => *i,
            // A library module that wasn't instantiated can't contribute
            // anything; skip silently rather than failing the whole pass.
            None => continue,
        };
        let exports = scan_core_module_exports(module_bytes)
            .with_context(|| format!("scanning exports of library `{name}` (module #{idx})"))?;
        libraries.push(Library {
            name,
            core_instance_idx,
            exports,
        });
    }

    // Resolve module → instance indices. The wasm-tools linker may instantiate
    // a wit-component-shim module before $main, so the instance index is not
    // necessarily equal to the module index.
    let main_core_instance_idx =
        *module_idx_to_instance_idx
            .get(&MAIN_MODULE_IDX)
            .ok_or_else(|| {
                anyhow!(
                    "no core instance found that instantiates module #{MAIN_MODULE_IDX} ($main)"
                )
            })?;
    let init_dyld_core_instance_idx = *module_idx_to_instance_idx
        .get(&init_dyld_module_idx)
        .ok_or_else(|| {
            anyhow!(
                "found `init_dyld.so` at module #{init_dyld_module_idx} \
                 but it was never instantiated",
            )
        })?;

    // Decode the embedded WIT once: enumerate the lowering set (every
    // function + resource-drop of every wasi interface the host exposes)
    // and compute each entry's lowered core signature from the same
    // resolve.
    let decoded = wit_parser::decoding::decode(bytes)
        .context("decoding pre-component WIT for wasi-lowering")?;
    let resolve = decoded.resolve();
    let lowered = enumerate_wasi_lowerings(resolve, &wasi_import_names);
    let wasi_signatures = compute_wasi_signatures(resolve, &lowered);

    Ok(Analysis {
        main_core_instance_idx,
        main_exports,
        init_dyld_core_instance_idx,
        libraries,
        wasi_instance_indices,
        lowered,
        wasi_signatures,
    })
}

/// Returns true iff `module_bytes` is the `init_dyld.so` sibling: a core
/// module with exactly one import and a single export whose name is
/// `__init_dyld`. The export references the import directly (no wrapper
/// body), but we don't need to inspect the export kind/index — the
/// name + shape are unique enough across the side-library set the
/// linker composes.
fn is_init_dyld_module(module_bytes: &[u8]) -> Result<bool> {
    let mut import_count: u32 = 0;
    let mut exports: Vec<String> = Vec::new();
    for payload in Parser::new(0).parse_all(module_bytes) {
        let payload = payload.context("parsing embedded core module shape")?;
        match payload {
            Payload::ImportSection(s) => {
                for group in s {
                    let group = group?;
                    match group {
                        wasmparser::Imports::Single(_, _) => import_count += 1,
                        wasmparser::Imports::Compact1 { items, .. } => {
                            for item in items {
                                let _ = item?;
                                import_count += 1;
                            }
                        }
                        wasmparser::Imports::Compact2 { names, .. } => {
                            for name in names {
                                let _ = name?;
                                import_count += 1;
                            }
                        }
                    }
                }
            }
            Payload::ExportSection(s) => {
                for e in s {
                    let e = e?;
                    exports.push(e.name.to_string());
                }
            }
            _ => {}
        }
    }
    Ok(import_count == 1 && exports.len() == 1 && exports[0] == INIT_DYLD_EXPORT_NAME)
}

/// Read the module name from the wasm "name" custom section, if present.
/// `wasm-tools component link --dl-openable NAME=PATH` attaches the
/// --dl-openable NAME here (`libc.so`, `libpython3.14.so`, etc.). Returns
/// None for modules without a name subsection.
fn read_module_name(module_bytes: &[u8]) -> Result<Option<String>> {
    for payload in Parser::new(0).parse_all(module_bytes) {
        let payload = payload.context("parsing core module for name section")?;
        if let Payload::CustomSection(reader) = payload {
            if let KnownCustom::Name(name_section) = reader.as_known() {
                for sub in name_section {
                    let sub = sub?;
                    if let Name::Module { name, .. } = sub {
                        return Ok(Some(name.to_string()));
                    }
                }
            }
        }
    }
    Ok(None)
}

/// Parse a core module's bytes and return its export list with each export's
/// resolved entity type (func signature / memory type / table type / global
/// type). Tags are skipped with a warning.
fn scan_core_module_exports(module_bytes: &[u8]) -> Result<Vec<ExportItem>> {
    // First pass: types section.
    let mut func_types: Vec<FuncType> = Vec::new();
    // func index space: type-index per function.
    let mut func_type_indices: Vec<u32> = Vec::new();
    let mut memories: Vec<PMemoryType> = Vec::new();
    let mut tables: Vec<PTableType> = Vec::new();
    let mut globals: Vec<PGlobalType> = Vec::new();
    let mut exports_raw: Vec<(String, ExternalKind, u32)> = Vec::new();

    for payload in Parser::new(0).parse_all(module_bytes) {
        let payload = payload.context("parsing embedded core module")?;
        match payload {
            Payload::TypeSection(s) => collect_type_section(s, &mut func_types)?,
            Payload::ImportSection(s) => collect_import_section(
                s,
                &mut func_type_indices,
                &mut tables,
                &mut memories,
                &mut globals,
            )?,
            Payload::FunctionSection(s) => {
                for ty in s {
                    func_type_indices.push(ty?);
                }
            }
            Payload::TableSection(s) => collect_table_section(s, &mut tables)?,
            Payload::MemorySection(s) => {
                for m in s {
                    memories.push(m?);
                }
            }
            Payload::GlobalSection(s) => {
                for g in s {
                    let g = g?;
                    globals.push(g.ty);
                }
            }
            Payload::ExportSection(s) => collect_export_section(s, &mut exports_raw)?,
            _ => {}
        }
    }

    let mut out = Vec::with_capacity(exports_raw.len());
    for (name, kind, idx) in exports_raw {
        let desc = match kind {
            ExternalKind::Func => {
                let ty_idx = *func_type_indices.get(idx as usize).ok_or_else(|| {
                    anyhow!("export {name}: func index {idx} out of range")
                })?;
                let ty = func_types.get(ty_idx as usize).cloned().ok_or_else(|| {
                    anyhow!("export {name}: type index {ty_idx} out of range")
                })?;
                EntityDesc::Func(ty)
            }
            ExternalKind::Table => {
                let t = tables.get(idx as usize).copied().ok_or_else(|| {
                    anyhow!("export {name}: table index {idx} out of range")
                })?;
                EntityDesc::Table(t)
            }
            ExternalKind::Memory => {
                let m = memories.get(idx as usize).copied().ok_or_else(|| {
                    anyhow!("export {name}: memory index {idx} out of range")
                })?;
                EntityDesc::Memory(m)
            }
            ExternalKind::Global => {
                let g = globals.get(idx as usize).copied().ok_or_else(|| {
                    anyhow!("export {name}: global index {idx} out of range")
                })?;
                EntityDesc::Global(g)
            }
            ExternalKind::Tag | ExternalKind::FuncExact => {
                eprintln!(
                    "inject-capture: skipping unsupported export `{name}` of kind {kind:?}"
                );
                continue;
            }
        };
        out.push(ExportItem { name, desc });
    }
    Ok(out)
}

fn collect_type_section(
    section: TypeSectionReader<'_>,
    out: &mut Vec<FuncType>,
) -> Result<()> {
    for rec_group in section {
        let group = rec_group?;
        // Modules produced by the wasm-tools linker don't use GC subtyping;
        // we treat each rec group as one func-type entry. If a rec group ends
        // up with multiple types or a non-func composite type, we keep a
        // placeholder so type indices stay aligned.
        for sub in group.into_types() {
            match sub.composite_type.inner {
                wasmparser::CompositeInnerType::Func(f) => out.push(f),
                _ => {
                    // Push an arbitrary placeholder to keep the index space
                    // aligned. We won't reference it for any export we
                    // actually re-emit (those are validated against func
                    // index space anyway).
                    out.push(FuncType::new([], []));
                }
            }
        }
    }
    Ok(())
}

fn collect_import_section(
    section: ImportSectionReader<'_>,
    func_type_indices: &mut Vec<u32>,
    tables: &mut Vec<PTableType>,
    memories: &mut Vec<PMemoryType>,
    globals: &mut Vec<PGlobalType>,
) -> Result<()> {
    for group in section {
        let group = group?;
        // Normalize all three import-section grouping forms into individual
        // (TypeRef) entries; we don't care about module/name here, just the
        // typed entity-index increments.
        match group {
            wasmparser::Imports::Single(_, imp) => add_import_type(
                imp.ty,
                func_type_indices,
                tables,
                memories,
                globals,
            ),
            wasmparser::Imports::Compact1 { items, .. } => {
                for item in items {
                    let item = item?;
                    add_import_type(
                        item.ty,
                        func_type_indices,
                        tables,
                        memories,
                        globals,
                    );
                }
            }
            wasmparser::Imports::Compact2 { ty, names, .. } => {
                for _name in names {
                    let _name = _name?;
                    add_import_type(ty, func_type_indices, tables, memories, globals);
                }
            }
        }
    }
    Ok(())
}

fn add_import_type(
    ty: TypeRef,
    func_type_indices: &mut Vec<u32>,
    tables: &mut Vec<PTableType>,
    memories: &mut Vec<PMemoryType>,
    globals: &mut Vec<PGlobalType>,
) {
    match ty {
        TypeRef::Func(t) | TypeRef::FuncExact(t) => func_type_indices.push(t),
        TypeRef::Table(t) => tables.push(t),
        TypeRef::Memory(m) => memories.push(m),
        TypeRef::Global(g) => globals.push(g),
        TypeRef::Tag(_) => { /* ignored */ }
    }
}

fn collect_table_section(
    section: TableSectionReader<'_>,
    tables: &mut Vec<PTableType>,
) -> Result<()> {
    for t in section {
        let t = t?;
        tables.push(t.ty);
    }
    Ok(())
}

fn collect_export_section(
    section: ExportSectionReader<'_>,
    out: &mut Vec<(String, ExternalKind, u32)>,
) -> Result<()> {
    for e in section {
        let e = e?;
        out.push((e.name.to_string(), e.kind, e.index));
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// Rewrite pass: copy the input component's sections through verbatim, then
// append our injected module + a single alias section + core instance section.
// ---------------------------------------------------------------------------

fn rewrite(bytes: &[u8], analysis: &Analysis) -> Result<Vec<u8>> {
    // Build the injected core module first; we'll need its bytes inside
    // ModuleSection. We'll also need to know exactly which exports of
    // $main vs $main.wasm we wired (deduped) so the synthetic core
    // instance "with" args match the import list.
    let (inject_module_bytes, wiring) = build_inject_module(analysis)?;

    let mut out = Component::new();

    // First pass: copy top-level non-custom sections in their original order.
    // We just stream raw section bytes via RawSection; nested module/component
    // sections come through as a single ModuleSection / ComponentSection raw
    // chunk by way of `as_section()`, which encompasses the whole nested
    // module's contents in the parent's byte stream.
    let mut depth: u32 = 0;
    for payload in Parser::new(0).parse_all(bytes) {
        let payload = payload.context("re-parsing input component")?;
        match &payload {
            Payload::ModuleSection { .. } | Payload::ComponentSection { .. } => {
                if depth == 0 {
                    if let Some((id, range)) = payload.as_section() {
                        out.section(&RawSection {
                            id,
                            data: &bytes[range],
                        });
                    }
                }
                depth += 1;
                continue;
            }
            Payload::End(_) => {
                if depth > 0 {
                    depth -= 1;
                }
                continue;
            }
            Payload::Version { .. } => continue,
            _ => {}
        }

        if depth != 0 {
            // Inside a nested module / nested component — its bytes are
            // already part of the parent's ModuleSection/ComponentSection
            // payload we passed through above.
            continue;
        }
        if let Some((id, range)) = payload.as_section() {
            out.section(&RawSection {
                id,
                data: &bytes[range],
            });
        }
    }

    // Second pass: append our injected sections.
    //
    // Ordering required by the binary format:
    //   1. Append the injected core module.
    //   2. Emit one component alias section that:
    //        - aliases `__init_dyld` out of the `init_dyld.so` core instance
    //          into the core-function index space (this is the host's
    //          dyld.initialize, surfaced under a plain name by the
    //          wat-authored sibling — wasm-tools component link wired its
    //          import to the SAME lowered canonical adapter wit-bindgen
    //          already minted for main.wasm's own dyld::initialize reference,
    //          so we don't need to mint another alias + canon.lower);
    //        - aliases every needed export of $main / $main.wasm into the
    //          appropriate core-* index spaces.
    //   3. Add a core-instance section that:
    //        a. exports the aliased `__init_dyld` core func under the name
    //           `initialize` via a synthetic `host` instance (the injected
    //           module imports `host.initialize` — only the export name
    //           need match, the underlying func is reused);
    //        b. exports every export of $main as the `main_synth` core
    //           instance,
    //        c. exports every needed export of $main.wasm as `main_side`,
    //        d. instantiates the injected core module wiring those three.

    // 1. Inject the new core module.
    let pre_existing_core_modules = count_pre_existing_core_modules(bytes)?;
    let new_core_module_idx: u32 = pre_existing_core_modules;

    // Splice the injected module's full bytes into a CoreModule component
    // section. We can't easily use `ModuleSection(&Module)` here because
    // `build_inject_module` returns finished bytes rather than a `Module`
    // value (we needed to assemble sections directly); wrapping as a
    // RawSection with id = CoreModule yields the same on-wire encoding.
    out.section(&RawSection {
        id: wasm_encoder::ComponentSectionId::CoreModule.into(),
        data: &inject_module_bytes,
    });

    // 2. One component alias section that produces every core item we need
    //    into the appropriate core-* index space: first __init_dyld, then
    //    each wired export (from $main, $main.wasm, and every composed
    //    side library).
    //
    //    Core-instance indices are resolved during analysis by walking the
    //    input's InstanceSection entries (see `analyze`). They are NOT the
    //    same as the module indices — the wasm-tools linker emits a
    //    wit-component shim instance ahead of $main, so e.g. module-0
    //    ($main) corresponds to a later core instance.
    let init_dyld_core_instance_idx: u32 = analysis.init_dyld_core_instance_idx;

    let mut wiring_aliases = ComponentAliasSection::new();

    // Per-kind index-space positions assigned to each alias. The alias
    // section produces items into the core function / table / memory /
    // global index spaces of the component, each in its own counter. We
    // need to know the starting counter values.
    let core_func_base = count_pre_existing_core_funcs(bytes)?;
    let core_table_base = count_pre_existing_core_tables(bytes)?;
    let core_memory_base = count_pre_existing_core_memories(bytes)?;
    let core_global_base = count_pre_existing_core_globals(bytes)?;

    let mut next_core_func = core_func_base;
    let mut next_core_table = core_table_base;
    let mut next_core_memory = core_memory_base;
    let mut next_core_global = core_global_base;

    // First alias: __init_dyld from init_dyld.so's core instance. This
    // becomes the host's `initialize` import (renamed via export_items
    // below).
    wiring_aliases.alias(Alias::CoreInstanceExport {
        instance: init_dyld_core_instance_idx,
        kind: ExportKind::Func,
        name: INIT_DYLD_EXPORT_NAME,
    });
    let init_dyld_core_func_idx = next_core_func;
    next_core_func += 1;

    // Per-source resolved (name, kind, core-item-index) lists, in the same
    // order as the source's wire items. Drives the synthetic "with"
    // instances emitted in the core-instance section below.
    let mut per_source_items: Vec<Vec<(String, ExportKind, u32)>> =
        Vec::with_capacity(wiring.sources.len());

    for src in &wiring.sources {
        let mut src_items: Vec<(String, ExportKind, u32)> = Vec::new();
        for w in &src.items {
            let kind = w.kind;
            let idx = match kind {
                ExportKind::Func => {
                    let i = next_core_func;
                    next_core_func += 1;
                    i
                }
                ExportKind::Table => {
                    let i = next_core_table;
                    next_core_table += 1;
                    i
                }
                ExportKind::Memory => {
                    let i = next_core_memory;
                    next_core_memory += 1;
                    i
                }
                ExportKind::Global => {
                    let i = next_core_global;
                    next_core_global += 1;
                    i
                }
                ExportKind::Tag => continue,
            };
            wiring_aliases.alias(Alias::CoreInstanceExport {
                instance: src.src_core_instance_idx,
                kind,
                name: &w.name,
            });
            src_items.push((w.name.clone(), kind, idx));
        }
        per_source_items.push(src_items);
    }

    // wasi-lowering pass — add per-method component aliases that the
    // canon.lower / canon.resource.drop ops below will operate on.
    //
    // Outputs:
    //   * `wasi_method_comp_func_idx[i]` — for entries with kind Method,
    //     the component function index produced by Alias::InstanceExport.
    //   * `wasi_drop_comp_type_idx[i]` — for entries with kind ResourceDrop,
    //     the component type index produced by Alias::InstanceExport.
    //
    // Indices into `analysis.lowered` whose interface didn't match a
    // pre-component import (or whose signature lookup failed) are left
    // None so the canon-section pass skips them. Both are defensive —
    // the lowering set is enumerated from the same imports/WIT, so every
    // entry should resolve.
    let mut next_comp_func_idx = count_pre_existing_component_funcs(bytes)?;
    let mut next_comp_type_idx = count_pre_existing_component_types(bytes)?;
    let mut wasi_method_comp_func_idx: Vec<Option<u32>> =
        vec![None; analysis.lowered.len()];
    let mut wasi_drop_comp_type_idx: Vec<Option<u32>> =
        vec![None; analysis.lowered.len()];

    for (i, m) in analysis.lowered.iter().enumerate() {
        let Some(&inst_idx) = analysis
            .wasi_instance_indices
            .get(m.component_import_name.as_str())
        else {
            eprintln!(
                "inject-capture: wasi-lowering: import {:?} not present in component, skipping {}",
                m.component_import_name, m.method
            );
            continue;
        };
        // Also skip when the wit-derived signature lookup failed —
        // the inject module won't import this entry anyway, so emitting
        // a canon.lower + instance export here would just be dead weight.
        if analysis.wasi_signatures[i].is_none() {
            eprintln!(
                "inject-capture: wasi-lowering: signature for {}#{} not found in WIT, skipping",
                m.module_short_name, m.method
            );
            continue;
        }
        match m.kind {
            WasiLoweringKind::Method => {
                wiring_aliases.alias(Alias::InstanceExport {
                    instance: inst_idx,
                    kind: wasm_encoder::ComponentExportKind::Func,
                    name: &m.method,
                });
                wasi_method_comp_func_idx[i] = Some(next_comp_func_idx);
                next_comp_func_idx += 1;
            }
            WasiLoweringKind::ResourceDrop => {
                let ty_name = resource_drop_type_name(&m.method).unwrap_or(&m.method);
                wiring_aliases.alias(Alias::InstanceExport {
                    instance: inst_idx,
                    kind: wasm_encoder::ComponentExportKind::Type,
                    name: ty_name,
                });
                wasi_drop_comp_type_idx[i] = Some(next_comp_type_idx);
                next_comp_type_idx += 1;
            }
        }
    }

    out.section(&wiring_aliases);

    // Find main_synth's "memory" + "cabi_realloc" core indices — required
    // as canon.lower options for any wasi method that param/returns
    // list/string. main_synth is wiring.sources[0] by construction.
    let main_synth_items = &per_source_items[0];
    let main_memory_core_idx = main_synth_items
        .iter()
        .find(|(n, k, _)| n == "memory" && *k == ExportKind::Memory)
        .map(|(_, _, i)| *i)
        .ok_or_else(|| anyhow!("main_synth missing `memory` export — needed for canon.lower"))?;
    let main_cabi_realloc_core_idx = main_synth_items
        .iter()
        .find(|(n, k, _)| n == "cabi_realloc" && *k == ExportKind::Func)
        .map(|(_, _, i)| *i)
        .ok_or_else(|| {
            anyhow!("main_synth missing `cabi_realloc` export — needed for canon.lower options")
        })?;

    // Emit canon.lower for each Method, canon.resource.drop for each
    // ResourceDrop. Output is a CanonicalFunctionSection whose entries
    // consume core function index slots starting at next_core_func.
    let mut canon = wasm_encoder::CanonicalFunctionSection::new();
    let mut wasi_lowered_core_func_idx: Vec<Option<u32>> =
        vec![None; analysis.lowered.len()];
    for (i, m) in analysis.lowered.iter().enumerate() {
        match m.kind {
            WasiLoweringKind::Method => {
                let Some(comp_idx) = wasi_method_comp_func_idx[i] else {
                    continue;
                };
                canon.lower(
                    comp_idx,
                    [
                        wasm_encoder::CanonicalOption::Memory(main_memory_core_idx),
                        wasm_encoder::CanonicalOption::Realloc(main_cabi_realloc_core_idx),
                    ],
                );
                wasi_lowered_core_func_idx[i] = Some(next_core_func);
                next_core_func += 1;
            }
            WasiLoweringKind::ResourceDrop => {
                let Some(ty_idx) = wasi_drop_comp_type_idx[i] else {
                    continue;
                };
                canon.resource_drop(ty_idx);
                wasi_lowered_core_func_idx[i] = Some(next_core_func);
                next_core_func += 1;
            }
        }
    }
    if !canon.is_empty() {
        out.section(&canon);
    }

    // 3. Core instance section: build one synthetic "with" instance per
    //    source ($main → main_synth, $main.wasm → main_side, each library
    //    → its name), plus the synthetic `host` instance carrying
    //    `initialize`, then the actual instantiation of our injected
    //    module.
    let pre_existing_core_instances = count_pre_existing_core_instances(bytes)?;
    let mut next_instance_idx = pre_existing_core_instances;

    // Per-source synthetic-instance indices, in source order.
    let mut source_instance_indices: Vec<u32> = Vec::with_capacity(wiring.sources.len());
    for _ in &wiring.sources {
        source_instance_indices.push(next_instance_idx);
        next_instance_idx += 1;
    }
    let host_instance_idx: u32 = next_instance_idx;
    next_instance_idx += 1;
    // The wasi-lowered instance bundles the canon-lowered + resource-drop
    // core funcs as exports named `wasi-lowered:<module_short_name>#<method>`.
    // The inject module imports from this instance under module name
    // `wasi-lowered` and re-exports under the same prefixed names; dyld
    // looks them up under the prefix at .so-load time.
    let wasi_lowered_items_present = wasi_lowered_core_func_idx.iter().any(|i| i.is_some());
    let wasi_lowered_instance_idx: Option<u32> = if wasi_lowered_items_present {
        let idx = next_instance_idx;
        next_instance_idx += 1;
        Some(idx)
    } else {
        None
    };
    // The instantiation we add below will produce one more core instance
    // (the injected module's), but we don't reference it elsewhere.
    let _injected_instance_idx: u32 = next_instance_idx;

    let mut core_instances = CoreInstanceSection::new();

    // One synthetic instance per wire source, in source order. Each
    // instance exposes the source's aliased exports under their original
    // names; the inject module's import section addresses it by its
    // matching import_module string (`main_synth`, `main_side`, `libc.so`,
    // ...).
    for items in &per_source_items {
        core_instances.export_items(items.iter().map(|(n, k, i)| (n.as_str(), *k, *i)));
    }

    // "host" — exposes the `__init_dyld` core func renamed to `initialize`,
    // since the injected module imports `host.initialize`. Only the
    // exported NAME need match the importer; the underlying core function
    // is the one wasm-tools already lowered for dyld.initialize.
    core_instances.export_items([(
        "initialize",
        ExportKind::Func,
        init_dyld_core_func_idx,
    )]);

    // "wasi-lowered" — exposes each canon-lowered core func under
    // wasi-lowered:<module>#<method>. Build the names here (owned) so
    // export_items can borrow the string slices.
    let wasi_lowered_names: Vec<String> = analysis
        .lowered
        .iter()
        .enumerate()
        .filter_map(|(i, m)| wasi_lowered_core_func_idx[i].map(|idx| (i, m, idx)))
        .map(|(_i, m, _idx)| format!("wasi-lowered:{}#{}", m.module_short_name, m.method))
        .collect();
    if wasi_lowered_instance_idx.is_some() {
        let mut wasi_lowered_items: Vec<(&str, ExportKind, u32)> = Vec::new();
        let mut name_cursor = 0usize;
        for (i, _m) in analysis.lowered.iter().enumerate() {
            if let Some(idx) = wasi_lowered_core_func_idx[i] {
                wasi_lowered_items.push((
                    wasi_lowered_names[name_cursor].as_str(),
                    ExportKind::Func,
                    idx,
                ));
                name_cursor += 1;
            }
        }
        core_instances.export_items(wasi_lowered_items);
    }

    // Instantiate the injected module wiring "host" + one entry per
    // source + (when populated) "wasi-lowered".
    let mut with_args: Vec<(&str, ModuleArg)> = Vec::with_capacity(wiring.sources.len() + 2);
    with_args.push(("host", ModuleArg::Instance(host_instance_idx)));
    for (src, &inst_idx) in wiring.sources.iter().zip(source_instance_indices.iter()) {
        with_args.push((src.import_module.as_str(), ModuleArg::Instance(inst_idx)));
    }
    if let Some(idx) = wasi_lowered_instance_idx {
        with_args.push(("wasi-lowered", ModuleArg::Instance(idx)));
    }
    core_instances.instantiate(new_core_module_idx, with_args);

    out.section(&core_instances);

    Ok(out.finish())
}

// ---------------------------------------------------------------------------
// Inject module builder.
// ---------------------------------------------------------------------------

#[derive(Debug, Clone)]
struct WireItem {
    name: String,
    kind: ExportKind,
    /// For func imports, the original wasmparser FuncType (used to
    /// reproduce the signature in the new module's type section).
    func_ty: Option<FuncType>,
    /// For non-func imports, the original entity descriptor.
    desc: EntityDesc,
}

/// One source of wire items the inject module imports + re-exports. The
/// inject module imports each `items` entry under module string `import_module`,
/// and the matching component-level core instance section emits a "with" arg
/// using the SAME string so the two sides line up.
#[derive(Debug, Clone)]
struct WireSource {
    /// Module-string used in the inject module's import section AND the
    /// matching "with" arg of the inject module's instantiation. Currently
    /// `main_synth` / `main_side` for $main / $main.wasm and the bare
    /// library name (e.g. `libc.so`) for each composed side library.
    import_module: String,
    /// Core-instance index in the input component that this source's items
    /// will be aliased from. Used during the rewrite pass to emit the
    /// CoreInstanceExport aliases.
    src_core_instance_idx: u32,
    /// The actual wire items (one per export of the source). Func items
    /// carry their original FuncType so we can reproduce the signature in
    /// the inject module's type section.
    items: Vec<WireItem>,
}

#[derive(Debug)]
struct Wiring {
    /// All sources, in import-emission order. Index 0 is `main_synth`
    /// ($main) contributing only `memory` / `__indirect_function_table` /
    /// `__stack_pointer`; subsequent entries are the composed dl-openable
    /// side libraries (libc.so, libpython3.14.so, libdl.so, ...) each
    /// contributing every export except the per-library lifecycle symbols
    /// `__wasm_apply_data_relocs` / `_initialize`.
    sources: Vec<WireSource>,
}

fn build_inject_module(analysis: &Analysis) -> Result<(Vec<u8>, Wiring)> {
    // Names already claimed by an earlier source. `$main`'s three exports
    // win; then each library in the order it appears in the pre-component.
    // Later sources contribute only their new symbols. We also pre-claim
    // the per-library lifecycle symbols so they're filtered out of every
    // library (each library has its own and they'd conflict otherwise).
    let mut claimed: HashSet<String> = HashSet::new();
    for skip in SKIPPED_LIBRARY_EXPORTS {
        claimed.insert((*skip).to_string());
    }

    let mut sources: Vec<WireSource> = Vec::new();

    let push_source = |module_str: &str,
                       src_idx: u32,
                       exports: &[ExportItem],
                       filter: Option<&[&str]>,
                       claimed: &mut HashSet<String>|
     -> WireSource {
        let mut items = Vec::with_capacity(exports.len());
        for e in exports {
            if let Some(allow) = filter {
                if !allow.iter().any(|a| *a == e.name.as_str()) {
                    continue;
                }
            }
            if claimed.contains(&e.name) {
                continue;
            }
            if let Some(w) = to_wire_item(e) {
                claimed.insert(w.name.clone());
                items.push(w);
            }
        }
        WireSource {
            import_module: module_str.to_string(),
            src_core_instance_idx: src_idx,
            items,
        }
    };

    // $main contributes ONLY the three primary items (memory, the indirect
    // table, the stack pointer). Every other apparent $main export is a
    // flattened alias of some library's export — we get those from the
    // libraries directly, where their original definition lives.
    sources.push(push_source(
        "main_synth",
        analysis.main_core_instance_idx,
        &analysis.main_exports,
        Some(MAIN_KEPT_EXPORTS),
        &mut claimed,
    ));
    for lib in &analysis.libraries {
        // Use the library name directly as the import-module string so the
        // import side (in the inject module) and the "with" side (in the
        // core instance section of the parent component) trivially agree.
        sources.push(push_source(
            &lib.name,
            lib.core_instance_idx,
            &lib.exports,
            None,
            &mut claimed,
        ));
    }

    // Report per-source import counts to stderr (visible in build logs).
    // Confirms the source list change took effect and helps spot drops in
    // upstream toolchain output.
    eprintln!("inject-capture: source contribution counts:");
    for src in &sources {
        eprintln!(
            "  {:>40} -> {} import(s)",
            src.import_module,
            src.items.len()
        );
    }

    let wiring = Wiring {
        sources: sources.clone(),
    };

    // Build types: we need one type entry per unique FuncType used by func
    // imports, plus one for `host.initialize` (() -> ()).
    let mut reencoder = RoundtripReencoder;
    let mut types = TypeSection::new();
    let mut type_pool: Vec<EncFuncType> = Vec::new();

    let type_index_for = |ty: &EncFuncType, pool: &mut Vec<EncFuncType>| -> u32 {
        if let Some(i) = pool.iter().position(|t| t == ty) {
            i as u32
        } else {
            let i = pool.len() as u32;
            pool.push(ty.clone());
            i
        }
    };

    // Per-source list of type indices for each func wire item. Walks each
    // source in lockstep with the import-section emission below.
    let mut per_source_func_types: Vec<Vec<u32>> = Vec::with_capacity(sources.len());
    for src in &sources {
        let mut v: Vec<u32> = Vec::new();
        for w in &src.items {
            if let ExportKind::Func = w.kind {
                let ft = w.func_ty.as_ref().expect("func wire item must have func_ty");
                let enc = reencode_func_type(&mut reencoder, ft)?;
                let idx = type_index_for(&enc, &mut type_pool);
                v.push(idx);
            }
        }
        per_source_func_types.push(v);
    }
    // host.initialize type: () -> ()
    let void_void = EncFuncType::new([], []);
    let host_init_type_idx = type_index_for(&void_void, &mut type_pool);

    // wasi-lowered imports' types. Signatures come from the pre-component's
    // embedded WIT (canonical-ABI-lowered via wit_parser); entries whose
    // interface/method isn't present in the pre-component get `None` and
    // are skipped here AND by the matching loops below + rewrite()'s
    // canon section. Aligned 1:1 with `analysis.lowered`.
    let mut wasi_type_indices: Vec<Option<u32>> =
        Vec::with_capacity(analysis.lowered.len());
    for sig in &analysis.wasi_signatures {
        match sig {
            Some(s) => {
                let ft = EncFuncType::new(s.params.iter().copied(), s.results.iter().copied());
                wasi_type_indices.push(Some(type_index_for(&ft, &mut type_pool)));
            }
            None => wasi_type_indices.push(None),
        }
    }

    // Emit type section.
    for ft in &type_pool {
        types.ty().func_type(ft);
    }

    // Imports.
    let mut imports = ImportSection::new();

    // host.initialize func index in the new module's func index space; we
    // place it last among imports.
    // Track the new module's *core* index spaces as we go.
    let mut new_func_idx: u32 = 0;
    let mut new_table_idx: u32 = 0;
    let mut new_mem_idx: u32 = 0;
    let mut new_global_idx: u32 = 0;
    // Per-export final index in the new module, in the same order as we
    // emit them. Used to drive the export section.
    let mut emitted: Vec<(String, ExportKind, u32)> = Vec::new();

    for (src_i, src) in sources.iter().enumerate() {
        let mut func_cursor = 0usize;
        for w in &src.items {
            let kind = w.kind;
            match kind {
                ExportKind::Func => {
                    let ty_idx = per_source_func_types[src_i][func_cursor];
                    func_cursor += 1;
                    imports.import(&src.import_module, &w.name, EntityType::Function(ty_idx));
                    emitted.push((w.name.clone(), ExportKind::Func, new_func_idx));
                    new_func_idx += 1;
                }
                ExportKind::Table => {
                    let t = match &w.desc {
                        EntityDesc::Table(t) => *t,
                        _ => unreachable!(),
                    };
                    let enc = reencode_table_type(&mut reencoder, t)?;
                    imports.import(&src.import_module, &w.name, EntityType::Table(enc));
                    emitted.push((w.name.clone(), ExportKind::Table, new_table_idx));
                    new_table_idx += 1;
                }
                ExportKind::Memory => {
                    let m = match &w.desc {
                        EntityDesc::Memory(m) => *m,
                        _ => unreachable!(),
                    };
                    let enc = reencode_memory_type(&mut reencoder, m);
                    imports.import(&src.import_module, &w.name, EntityType::Memory(enc));
                    emitted.push((w.name.clone(), ExportKind::Memory, new_mem_idx));
                    new_mem_idx += 1;
                }
                ExportKind::Global => {
                    let g = match &w.desc {
                        EntityDesc::Global(g) => *g,
                        _ => unreachable!(),
                    };
                    let enc = reencode_global_type(&mut reencoder, g)?;
                    imports.import(&src.import_module, &w.name, EntityType::Global(enc));
                    emitted.push((w.name.clone(), ExportKind::Global, new_global_idx));
                    new_global_idx += 1;
                }
                ExportKind::Tag => { /* skipped at conversion time */ }
            }
        }
    }

    // host.initialize last.
    imports.import("host", "initialize", EntityType::Function(host_init_type_idx));
    let host_init_func_idx = new_func_idx;
    new_func_idx += 1;
    // Don't re-export host.initialize.

    // wasi-lowering imports: one entry per `analysis.lowered` row whose
    // signature was resolvable from the pre-component's embedded WIT,
    // imported under module `wasi-lowered` with the canon.lower'd core
    // signature. dyld looks the export up under the same
    // `wasi-lowered:<module>#<method>` name on the captured module at
    // .so-load time. The actual lowered core funcs are supplied by
    // rewrite()'s wasi-lowered synth instance via this module's
    // instantiation `with` args.
    let wasi_export_names: Vec<String> = analysis
        .lowered
        .iter()
        .map(|m| format!("wasi-lowered:{}#{}", m.module_short_name, m.method))
        .collect();
    for (i, _m) in analysis.lowered.iter().enumerate() {
        let Some(ty_idx) = wasi_type_indices[i] else {
            continue;
        };
        imports.import(
            "wasi-lowered",
            wasi_export_names[i].as_str(),
            EntityType::Function(ty_idx),
        );
        emitted.push((
            wasi_export_names[i].clone(),
            ExportKind::Func,
            new_func_idx,
        ));
        new_func_idx += 1;
    }

    // Define a tiny local wrapper function `__entry` whose body is just
    // `call host.initialize`. The start section points to this wrapper
    // (NOT directly to the imported function): we need the cross-component
    // `call` instruction to live in *this* module's body so that, from the
    // host side, `host.CallerCoreModule(ctx)` resolves to the inject module
    // — which is the whole point of this rewrite. If we pointed start at
    // the imported func directly, wazero would dispatch into the underlying
    // canonical adapter with no inject-module wasm frame on the stack, and
    // the captured module would be wacogo's lowered host adapter instead.
    let mut functions = FunctionSection::new();
    functions.function(host_init_type_idx);
    let wrapper_func_idx = new_func_idx;
    new_func_idx += 1;

    let mut code = CodeSection::new();
    let mut wrapper_body = Function::new([]);
    wrapper_body.instructions().call(host_init_func_idx).end();
    code.function(&wrapper_body);

    // Exports — re-export everything we imported under main_synth/main_side
    // by the same name. Skip the host import and the wrapper.
    let mut exports = ExportSection::new();
    for (name, kind, idx) in &emitted {
        exports.export(name, *kind, *idx);
    }

    // Start section: call our wrapper, which in turn calls host.initialize.
    let start = StartSection {
        function_index: wrapper_func_idx,
    };

    // Suppress unused warnings for the FuncType conversion when there are no
    // funcs to import (degenerate case).
    let _ = (
        &per_source_func_types,
        new_global_idx,
        new_func_idx,
        new_table_idx,
        new_mem_idx,
    );

    // Assemble the module. Sections must be in the canonical core wasm
    // order: type, import, function, table, memory, global, export, start,
    // element, datacount, code, data.
    let mut module = Module::new();
    if !type_pool.is_empty() {
        module.section(&types);
    }
    if imports.len() > 0 {
        module.section(&imports);
    }
    module.section(&functions);
    if !emitted.is_empty() {
        module.section(&exports);
    }
    module.section(&start);
    module.section(&code);

    Ok((module.finish(), wiring))
}

fn to_wire_item(e: &ExportItem) -> Option<WireItem> {
    let (kind, func_ty) = match &e.desc {
        EntityDesc::Func(f) => (ExportKind::Func, Some(f.clone())),
        EntityDesc::Table(_) => (ExportKind::Table, None),
        EntityDesc::Memory(_) => (ExportKind::Memory, None),
        EntityDesc::Global(_) => (ExportKind::Global, None),
    };
    Some(WireItem {
        name: e.name.clone(),
        kind,
        func_ty,
        desc: e.desc.clone(),
    })
}

fn reencode_func_type(
    r: &mut RoundtripReencoder,
    f: &FuncType,
) -> Result<EncFuncType> {
    let mut params = Vec::with_capacity(f.params().len());
    for p in f.params() {
        params.push(reencode_val_type(r, *p)?);
    }
    let mut results = Vec::with_capacity(f.results().len());
    for p in f.results() {
        results.push(reencode_val_type(r, *p)?);
    }
    Ok(EncFuncType::new(params, results))
}

fn reencode_val_type(r: &mut RoundtripReencoder, v: PValType) -> Result<ValType> {
    r.val_type(v)
        .map_err(|_| anyhow!("unable to reencode val_type {:?}", v))
}

fn reencode_table_type(
    r: &mut RoundtripReencoder,
    t: PTableType,
) -> Result<TableType> {
    r.table_type(t)
        .map_err(|_| anyhow!("unable to reencode table type"))
}

fn reencode_memory_type(_r: &mut RoundtripReencoder, m: PMemoryType) -> MemoryType {
    MemoryType {
        minimum: m.initial,
        maximum: m.maximum,
        memory64: m.memory64,
        shared: m.shared,
        page_size_log2: m.page_size_log2,
    }
}

fn reencode_global_type(
    r: &mut RoundtripReencoder,
    g: PGlobalType,
) -> Result<GlobalType> {
    r.global_type(g)
        .map_err(|_| anyhow!("unable to reencode global type"))
}

// ---------------------------------------------------------------------------
// Counting helpers: figure out the next available index in each component
// index space so we can correctly reference the items we're about to append.
// ---------------------------------------------------------------------------
//
// These walk the input component once each. Could be combined with `analyze`
// for efficiency, but at this binary's size the cost is negligible and the
// separation keeps each function single-purpose.

/// Counts component-level function index slots already consumed by the
/// input — component imports of Func kind + ComponentAliasSection
/// alias entries that produce component funcs (InstanceExport with
/// kind=Func) + CanonicalFunctionSection lower/lift entries.
fn count_pre_existing_component_funcs(bytes: &[u8]) -> Result<u32> {
    let mut count: u32 = 0;
    let mut depth: u32 = 0;
    for payload in Parser::new(0).parse_all(bytes) {
        let payload = payload.context("re-parsing for component func count")?;
        match payload {
            Payload::ModuleSection { .. } | Payload::ComponentSection { .. } => depth += 1,
            Payload::End(_) => {
                if depth > 0 {
                    depth -= 1;
                }
            }
            Payload::ComponentImportSection(s) if depth == 0 => {
                for imp in s {
                    let imp = imp?;
                    if let ComponentTypeRef::Func(_) = imp.ty {
                        count += 1;
                    }
                }
            }
            Payload::ComponentAliasSection(s) if depth == 0 => {
                for alias in s {
                    let alias = alias?;
                    if let wasmparser::ComponentAlias::InstanceExport { kind, .. } = alias {
                        if kind == wasmparser::ComponentExternalKind::Func {
                            count += 1;
                        }
                    }
                }
            }
            Payload::ComponentCanonicalSection(s) if depth == 0 => {
                // Every canon entry that's a Lift produces a component
                // func. canon.lower / canon.resource_drop produce CORE
                // funcs (different space).
                for f in s {
                    let f = f?;
                    if let wasmparser::CanonicalFunction::Lift { .. } = f {
                        count += 1;
                    }
                }
            }
            _ => {}
        }
    }
    Ok(count)
}

/// Counts component-level type index slots already consumed — every
/// component import of Type kind, every ComponentTypeSection entry,
/// every ComponentAliasSection alias that produces a component type,
/// every CanonicalFunctionSection resource constructor.
fn count_pre_existing_component_types(bytes: &[u8]) -> Result<u32> {
    let mut count: u32 = 0;
    let mut depth: u32 = 0;
    for payload in Parser::new(0).parse_all(bytes) {
        let payload = payload.context("re-parsing for component type count")?;
        match payload {
            Payload::ModuleSection { .. } | Payload::ComponentSection { .. } => depth += 1,
            Payload::End(_) => {
                if depth > 0 {
                    depth -= 1;
                }
            }
            Payload::ComponentImportSection(s) if depth == 0 => {
                for imp in s {
                    let imp = imp?;
                    if let ComponentTypeRef::Type(_) = imp.ty {
                        count += 1;
                    }
                }
            }
            Payload::ComponentTypeSection(s) if depth == 0 => {
                for _ in s {
                    count += 1;
                }
            }
            Payload::ComponentAliasSection(s) if depth == 0 => {
                for alias in s {
                    let alias = alias?;
                    if let wasmparser::ComponentAlias::InstanceExport { kind, .. } = alias {
                        if kind == wasmparser::ComponentExternalKind::Type {
                            count += 1;
                        }
                    }
                }
            }
            _ => {}
        }
    }
    Ok(count)
}

fn count_pre_existing_core_funcs(bytes: &[u8]) -> Result<u32> {
    // Core-function index space at component level is populated by core
    // aliases of kind Func and by canon lower.
    let mut depth = 0u32;
    let mut count = 0u32;
    for p in Parser::new(0).parse_all(bytes) {
        let p = p?;
        match p {
            Payload::ModuleSection { .. } | Payload::ComponentSection { .. } => depth += 1,
            Payload::End(_) if depth > 0 => depth -= 1,
            Payload::ComponentAliasSection(s) if depth == 0 => {
                for a in s {
                    let a = a?;
                    if let wasmparser::ComponentAlias::CoreInstanceExport { kind, .. } = a {
                        if matches!(kind, ExternalKind::Func) {
                            count += 1;
                        }
                    }
                }
            }
            Payload::ComponentCanonicalSection(s) if depth == 0 => {
                for c in s {
                    let c = c?;
                    if matches!(c, wasmparser::CanonicalFunction::Lower { .. })
                        || matches!(c, wasmparser::CanonicalFunction::ResourceNew { .. })
                        || matches!(c, wasmparser::CanonicalFunction::ResourceDrop { .. })
                        || matches!(c, wasmparser::CanonicalFunction::ResourceRep { .. })
                    {
                        count += 1;
                    }
                }
            }
            _ => {}
        }
    }
    Ok(count)
}

fn count_pre_existing_core_modules(bytes: &[u8]) -> Result<u32> {
    let mut depth = 0u32;
    let mut count = 0u32;
    for p in Parser::new(0).parse_all(bytes) {
        let p = p?;
        match p {
            Payload::ModuleSection { .. } => {
                if depth == 0 {
                    count += 1;
                }
                depth += 1;
            }
            Payload::ComponentSection { .. } => depth += 1,
            Payload::End(_) if depth > 0 => depth -= 1,
            _ => {}
        }
    }
    Ok(count)
}

fn count_pre_existing_core_instances(bytes: &[u8]) -> Result<u32> {
    let mut depth = 0u32;
    let mut count = 0u32;
    for p in Parser::new(0).parse_all(bytes) {
        let p = p?;
        match p {
            Payload::ModuleSection { .. } | Payload::ComponentSection { .. } => depth += 1,
            Payload::End(_) if depth > 0 => depth -= 1,
            Payload::InstanceSection(s) if depth == 0 => {
                count += s.count();
            }
            _ => {}
        }
    }
    Ok(count)
}

fn count_pre_existing_core_tables(bytes: &[u8]) -> Result<u32> {
    count_core_aliased_kind(bytes, ExternalKind::Table)
}

fn count_pre_existing_core_memories(bytes: &[u8]) -> Result<u32> {
    count_core_aliased_kind(bytes, ExternalKind::Memory)
}

fn count_pre_existing_core_globals(bytes: &[u8]) -> Result<u32> {
    count_core_aliased_kind(bytes, ExternalKind::Global)
}

fn count_core_aliased_kind(bytes: &[u8], want: ExternalKind) -> Result<u32> {
    let mut depth = 0u32;
    let mut count = 0u32;
    for p in Parser::new(0).parse_all(bytes) {
        let p = p?;
        match p {
            Payload::ModuleSection { .. } | Payload::ComponentSection { .. } => depth += 1,
            Payload::End(_) if depth > 0 => depth -= 1,
            Payload::ComponentAliasSection(s) if depth == 0 => {
                for a in s {
                    let a = a?;
                    if let wasmparser::ComponentAlias::CoreInstanceExport { kind, .. } = a {
                        if kind == want {
                            count += 1;
                        }
                    }
                }
            }
            _ => {}
        }
    }
    Ok(count)
}
