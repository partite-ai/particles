package dyld

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

// soInfo is everything the loader extracts from a .so binary before
// instantiating it: the dylink.0-declared memory/table requirements,
// list of env imports (so we can build the env module), list of own-
// function exports (so library.symbol can map names to table
// indices).
//
// References:
//   - tool-conventions/DynamicLinking.md ("The dylink.0 Section"):
//     subsection IDs + payload layout that we parse below.
type soInfo struct {
	// dylink.0 MEM_INFO. memorySize is the exact byte count to
	// reserve at env.__memory_base; memoryAlignLog2 is power-of-two
	// alignment (the spec encodes alignment as log2). Same for the
	// table.
	memorySize        uint32
	memoryAlignLog2   uint32
	tableSize         uint32
	tableAlignLog2    uint32

	// dylink.0 NEEDED — transitive shared-library deps the .so
	// expects the loader to load first. Surfaced for diagnostics;
	// the spike doesn't actually do nested loads.
	needed []string


	// Function imports the .so wants from "env". Each entry's typeIdx
	// is an index into types — the .so's own type-section indices,
	// which we re-export by interning the same shapes in the env
	// module's type section.
	funcImports []funcImport

	// Whether the .so imports memory / table / each known global.
	// All wasm-dl .so's import these, but we double-check.
	importsMemory      bool
	importsTable       bool
	importsStackPtr    bool
	importsMemoryBase  bool
	importsTableBase   bool

	// gotFuncImports / gotMemImports: per-symbol GOT entries
	// (mutable i32 globals) the .so requests. wasm-ld --shared emits
	// these when the .so takes the address of a symbol from main.
	gotFuncImports []string
	gotMemImports  []string

	// ownFuncCount is the number of locally-defined functions in the
	// .so. Used to size the table grow.
	ownFuncCount uint32

	// ownFuncTypeIdx[i] is the type-section index of the i-th own
	// function. Used by buildPostLoadShim to declare imports with
	// the right signature — wazero validates that import types match
	// the source module's export types.
	ownFuncTypeIdx []uint32

	// importedFuncs is the count of function-typed imports — needed
	// to convert .so absolute function indices to own-function
	// indices (export index - importedFuncs = own index).
	importedFuncs uint32

	// exports maps an exported function's name to its OWN-function
	// index inside the .so (0-based, excluding imports). After
	// instantiation, table[__table_base + ownIndex] holds the
	// function — that's what library.symbol returns.
	exports map[string]uint32

	// dataExportNames lists global-kind exports — the .so's
	// exposed data symbols, each carrying their memory address as
	// the global's i32 value (set by wasm-ld --shared at link time
	// relative to __memory_base). The loader looks these up via
	// soInst.ExportedGlobal(name).Get() post-instantiation to
	// populate the registry's dataExports map. Empty for most .so's
	// unless the build explicitly --export=<data-symbol>'d them.
	dataExportNames []string

	// types is the .so's type section in raw param/result bytes.
	// We re-intern the function-import shapes in the env module
	// by referencing back into here.
	types []funcType
}

// funcImport is one entry in soInfo.funcImports — keyed on name so
// the env module can re-export it under the same name.
type funcImport struct {
	name    string
	typeIdx uint32 // index into soInfo.types
}

// parseSO consumes a wasm .so binary and extracts everything the
// loader needs. We parse the binary ourselves rather than going
// through wazero's CompiledModule — wazero doesn't expose imported
// globals or imported tables, which we need to distinguish env
// memory / table / global imports from function imports.
//
// Sections we read: type (1), import (2), function (3), export (7),
// element (9). Other sections are skipped.
func parseSO(wasmBytes []byte) (*soInfo, error) {
	if len(wasmBytes) < 8 || string(wasmBytes[:4]) != wasmMagic {
		return nil, fmt.Errorf("parseSO: not a wasm binary")
	}
	r := bytes.NewReader(wasmBytes[8:]) // skip magic + version

	info := &soInfo{exports: make(map[string]uint32)}

	for r.Len() > 0 {
		id, err := r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("parseSO: section id: %w", err)
		}
		size, err := readULEB128(r)
		if err != nil {
			return nil, fmt.Errorf("parseSO: section size: %w", err)
		}
		sec := make([]byte, size)
		if _, err := io.ReadFull(r, sec); err != nil {
			return nil, fmt.Errorf("parseSO: section %d body: %w", id, err)
		}
		sr := bytes.NewReader(sec)
		switch id {
		case 0: // custom section — interesting iff named "dylink.0"
			if err := info.maybeParseDylink0(sr); err != nil {
				return nil, fmt.Errorf("parseSO: custom section: %w", err)
			}
		case secType:
			if err := info.parseTypeSection(sr); err != nil {
				return nil, fmt.Errorf("parseSO: type section: %w", err)
			}
		case secImport:
			if err := info.parseImportSection(sr); err != nil {
				return nil, fmt.Errorf("parseSO: import section: %w", err)
			}
		case 3: // function section — for own-function count
			if err := info.parseFunctionSection(sr); err != nil {
				return nil, fmt.Errorf("parseSO: function section: %w", err)
			}
		case secExport:
			if err := info.parseExportSection(sr); err != nil {
				return nil, fmt.Errorf("parseSO: export section: %w", err)
			}
		}
	}

	return info, nil
}

// dylink.0 subsection type codes (tool-conventions
// DynamicLinking.md).
const (
	dylinkMemInfo    byte = 1
	dylinkNeeded     byte = 2
	dylinkExportInfo byte = 3 // unused for the spike but parsed-and-skipped to keep going
	dylinkImportInfo byte = 4
	dylinkRunPath    byte = 5
)

// maybeParseDylink0 reads a custom section. If the section's name is
// "dylink.0", it parses every subsection into `info`; otherwise it
// silently ignores the section (custom sections can carry anything —
// names, producer, target-features, debug-info, etc.).
func (info *soInfo) maybeParseDylink0(r *bytes.Reader) error {
	name, err := readString(r)
	if err != nil {
		return fmt.Errorf("custom section name: %w", err)
	}
	if name != "dylink.0" {
		return nil
	}
	for r.Len() > 0 {
		subType, err := r.ReadByte()
		if err != nil {
			return err
		}
		subLen, err := readULEB128(r)
		if err != nil {
			return err
		}
		buf := make([]byte, subLen)
		if _, err := io.ReadFull(r, buf); err != nil {
			return fmt.Errorf("dylink.0 subsection type %d: %w", subType, err)
		}
		sub := bytes.NewReader(buf)
		switch subType {
		case dylinkMemInfo:
			if info.memorySize, err = uleb32(sub); err != nil {
				return err
			}
			if info.memoryAlignLog2, err = uleb32(sub); err != nil {
				return err
			}
			if info.tableSize, err = uleb32(sub); err != nil {
				return err
			}
			if info.tableAlignLog2, err = uleb32(sub); err != nil {
				return err
			}
		case dylinkNeeded:
			n, err := uleb32(sub)
			if err != nil {
				return err
			}
			for i := uint32(0); i < n; i++ {
				s, err := readString(sub)
				if err != nil {
					return err
				}
				info.needed = append(info.needed, s)
			}
		default:
			// Spec defines export-info / import-info / runtime-path
			// subsections; spike doesn't act on them yet. Drop on the
			// floor — we've already consumed payload_len bytes.
		}
	}
	return nil
}

// uleb32 is a small wrapper that returns uint32 (vs the uint64 from
// readULEB128) for the many varuint32 fields in the dylink spec.
func uleb32(r *bytes.Reader) (uint32, error) {
	v, err := readULEB128(r)
	if err != nil {
		return 0, err
	}
	return uint32(v), nil
}

func (info *soInfo) parseTypeSection(r *bytes.Reader) error {
	n, err := readULEB128(r)
	if err != nil {
		return err
	}
	for i := uint64(0); i < n; i++ {
		tag, err := r.ReadByte()
		if err != nil {
			return err
		}
		if tag != 0x60 {
			return fmt.Errorf("type[%d]: unexpected tag 0x%02x", i, tag)
		}
		params, err := readValTypes(r)
		if err != nil {
			return err
		}
		results, err := readValTypes(r)
		if err != nil {
			return err
		}
		info.types = append(info.types, funcType{params: params, results: results})
	}
	return nil
}

func (info *soInfo) parseImportSection(r *bytes.Reader) error {
	n, err := readULEB128(r)
	if err != nil {
		return err
	}
	var importedFuncs uint32
	for i := uint64(0); i < n; i++ {
		modName, err := readString(r)
		if err != nil {
			return err
		}
		name, err := readString(r)
		if err != nil {
			return err
		}
		kind, err := r.ReadByte()
		if err != nil {
			return err
		}
		switch kind {
		case importKindFunc:
			typeIdx, err := readULEB128(r)
			if err != nil {
				return err
			}
			if modName == "env" {
				info.funcImports = append(info.funcImports, funcImport{
					name:    name,
					typeIdx: uint32(typeIdx),
				})
			}
			importedFuncs++
		case importKindTable:
			refType, err := r.ReadByte()
			if err != nil {
				return err
			}
			_ = refType
			if err := skipLimits(r); err != nil {
				return err
			}
			if modName == "env" && name == "__indirect_function_table" {
				info.importsTable = true
			}
		case importKindMemory:
			if err := skipLimits(r); err != nil {
				return err
			}
			if modName == "env" && name == "memory" {
				info.importsMemory = true
			}
		case importKindGlobal:
			valType, err := r.ReadByte()
			if err != nil {
				return err
			}
			mutByte, err := r.ReadByte()
			if err != nil {
				return err
			}
			_ = valType
			_ = mutByte
			switch modName {
			case "env":
				switch name {
				case "__stack_pointer":
					info.importsStackPtr = true
				case "__memory_base":
					info.importsMemoryBase = true
				case "__table_base":
					info.importsTableBase = true
				}
			case "GOT.func":
				// wasm-ld emits address-taken functions as
				// imports of mutable i32 globals from module
				// "GOT.func" with name = the symbol. The
				// loader fills the global with the function's
				// table index.
				info.gotFuncImports = append(info.gotFuncImports, name)
			case "GOT.mem":
				// Similarly, address-taken data symbols
				// become mutable i32 global imports from
				// module "GOT.mem", filled with the symbol's
				// memory address.
				info.gotMemImports = append(info.gotMemImports, name)
			}
		default:
			return fmt.Errorf("import[%d]: unsupported kind 0x%02x", i, kind)
		}
	}
	// importedFuncs is used later for resolving export-section
	// function indices: an "own" function's index = absolute index -
	// importedFuncs.
	info.importedFuncs = importedFuncs
	return nil
}

func (info *soInfo) parseFunctionSection(r *bytes.Reader) error {
	n, err := readULEB128(r)
	if err != nil {
		return err
	}
	info.ownFuncCount = uint32(n)
	info.ownFuncTypeIdx = make([]uint32, n)
	for i := uint64(0); i < n; i++ {
		typeIdx, err := readULEB128(r)
		if err != nil {
			return err
		}
		info.ownFuncTypeIdx[i] = uint32(typeIdx)
	}
	return nil
}

func (info *soInfo) parseExportSection(r *bytes.Reader) error {
	n, err := readULEB128(r)
	if err != nil {
		return err
	}
	for i := uint64(0); i < n; i++ {
		name, err := readString(r)
		if err != nil {
			return err
		}
		kind, err := r.ReadByte()
		if err != nil {
			return err
		}
		idx, err := readULEB128(r)
		if err != nil {
			return err
		}
		if kind == exportKindGlobal {
			info.dataExportNames = append(info.dataExportNames, name)
			continue
		}
		if kind != exportKindFunc {
			continue
		}
		absIdx := uint32(idx)
		// .so's function index space: [0, importedFuncs) are imports,
		// [importedFuncs, importedFuncs+ownFuncCount) are own. We map
		// exported own functions to table position via the linear-
		// layout assumption (wasm-ld --shared standard).
		if absIdx < info.importedFuncs {
			// Re-export of an imported function — rare; skip.
			continue
		}
		info.exports[name] = absIdx - info.importedFuncs
	}
	return nil
}

func readULEB128(r *bytes.Reader) (uint64, error) {
	var result uint64
	var shift uint
	for {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		result |= uint64(b&0x7F) << shift
		if b&0x80 == 0 {
			return result, nil
		}
		shift += 7
		if shift >= 64 {
			return 0, fmt.Errorf("ULEB128 too long")
		}
	}
}

func readString(r *bytes.Reader) (string, error) {
	n, err := readULEB128(r)
	if err != nil {
		return "", err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func readValTypes(r *bytes.Reader) ([]byte, error) {
	n, err := readULEB128(r)
	if err != nil {
		return nil, err
	}
	out := make([]byte, n)
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, err
	}
	return out, nil
}

// sampledExportNames returns up to `max` of the module's exported
// function names — diagnostic only.
func sampledExportNames(m api.Module, max int) []string {
	var out []string
	for n := range m.ExportedFunctionDefinitions() {
		out = append(out, n)
		if len(out) >= max {
			break
		}
	}
	return out
}

func skipLimits(r *bytes.Reader) error {
	flag, err := r.ReadByte()
	if err != nil {
		return err
	}
	if _, err := readULEB128(r); err != nil {
		return err
	}
	if flag&0x01 != 0 {
		if _, err := readULEB128(r); err != nil {
			return err
		}
	}
	return nil
}

// buildEnvModule synthesizes the per-load env module that satisfies
// the .so's "env" imports. Layout:
//   - Imports from main and runtime-loaded libraries — for each
//     function the .so wants, an import from whichever already-loaded
//     module actually exports it. Runtime-loaded libraries are
//     addressed by their dyld.load name (e.g. "libfoo.so"); symbols
//     not exported by any of those fall back to "main" (the captured
//     module, which carries the union of $main + every pre-loaded
//     library's exports).
//   - Imports from "main": memory, __indirect_function_table,
//     __stack_pointer.
//   - Own globals: __memory_base, __table_base (i32 const).
//   - Exports: every imported func re-exported under the same name,
//     plus memory / table / stack-pointer / __memory_base /
//     __table_base for the .so to import as "env".
//
// memoryBase / tableBase are runtime values from the caller's malloc
// and __grow_table; gotFuncBase is reserved for the GOT.func builder
// (env shim itself doesn't allocate from it).
//
// envFuncSources[i] is the wasm import module name for funcImports[i]
// — either "main" or a runtime-loaded library's dyld name. The
// caller's ImportResolver must map each distinct module name to its
// backing api.Module.
func (a *Adapter) buildEnvModule(info *soInfo, envFuncSources []string, memoryBase, tableBase, gotFuncBase int32) ([]byte, error) {
	spec := &envModuleSpec{}

	if len(envFuncSources) != len(info.funcImports) {
		return nil, fmt.Errorf("buildEnvModule: envFuncSources len=%d want %d",
			len(envFuncSources), len(info.funcImports))
	}

	// Intern every funcType the .so's env funcImports reference. The
	// env module's type section ends up being a subset of the .so's,
	// keyed by content so we don't duplicate.
	envFuncTypeIdx := make([]uint32, len(info.funcImports))
	for i, fi := range info.funcImports {
		if int(fi.typeIdx) >= len(info.types) {
			return nil, fmt.Errorf("buildEnvModule: typeIdx %d out of range", fi.typeIdx)
		}
		t := info.types[fi.typeIdx]
		envFuncTypeIdx[i] = spec.internType(t)
	}

	// Indices we'll track for the export section.
	var (
		tableImportIdx  uint32
		memoryImportIdx uint32
		stackPtrIdx     uint32 // global index space; runs in parallel with own globals
		memoryBaseIdx   uint32
		tableBaseIdx    uint32
	)

	for i, fi := range info.funcImports {
		spec.imports = append(spec.imports, envImport{
			module:  envFuncSources[i],
			name:    fi.name,
			kind:    importKindFunc,
			typeIdx: envFuncTypeIdx[i],
		})
	}

	if info.importsMemory {
		memoryImportIdx = 0
		spec.imports = append(spec.imports, envImport{
			module:   "main",
			name:     "memory",
			kind:     importKindMemory,
			minPages: 0,
		})
	}

	if info.importsTable {
		tableImportIdx = 0
		spec.imports = append(spec.imports, envImport{
			module:   "main",
			name:     "__indirect_function_table",
			kind:     importKindTable,
			refType:  valFuncRef,
			minTable: 0,
		})
	}

	// __stack_pointer import (mutable i32 global). Same source.
	var globalIdx uint32
	if info.importsStackPtr {
		stackPtrIdx = globalIdx
		globalIdx++
		spec.imports = append(spec.imports, envImport{
			module:   "main",
			name:     "__stack_pointer",
			kind:     importKindGlobal,
			gValType: valI32,
			gMut:     true,
		})
	}

	// (GOT.func / GOT.mem are handled in separate shim modules — see
	// buildGotFuncShim, buildGotMemShim. The env shim sticks to env.*
	// re-exports.)
	_ = gotFuncBase

	// Own globals: __memory_base, __table_base (i32 const).
	if info.importsMemoryBase {
		memoryBaseIdx = globalIdx
		globalIdx++
		spec.globals = append(spec.globals, envGlobal{
			valType: valI32,
			mutable: false,
			initI32: memoryBase,
		})
	}
	if info.importsTableBase {
		tableBaseIdx = globalIdx
		globalIdx++
		spec.globals = append(spec.globals, envGlobal{
			valType: valI32,
			mutable: false,
			initI32: tableBase,
		})
	}

	// Exports: every imported function (re-export under same name),
	// memory, table, __stack_pointer, __memory_base, __table_base.
	for i, fi := range info.funcImports {
		spec.exports = append(spec.exports, envExport{
			name: fi.name,
			kind: exportKindFunc,
			idx:  uint32(i),
		})
	}
	if info.importsMemory {
		spec.exports = append(spec.exports, envExport{
			name: "memory", kind: exportKindMemory, idx: memoryImportIdx,
		})
	}
	if info.importsTable {
		spec.exports = append(spec.exports, envExport{
			name: "__indirect_function_table", kind: exportKindTable, idx: tableImportIdx,
		})
	}
	if info.importsStackPtr {
		spec.exports = append(spec.exports, envExport{
			name: "__stack_pointer", kind: exportKindGlobal, idx: stackPtrIdx,
		})
	}
	if info.importsMemoryBase {
		spec.exports = append(spec.exports, envExport{
			name: "__memory_base", kind: exportKindGlobal, idx: memoryBaseIdx,
		})
	}
	if info.importsTableBase {
		spec.exports = append(spec.exports, envExport{
			name: "__table_base", kind: exportKindGlobal, idx: tableBaseIdx,
		})
	}

	return spec.encode()
}

// buildGotFuncShim synthesizes the module that satisfies the .so's
// GOT.func.X imports. Each X is resolved against (a) the loader's
// registry of already-loaded libraries and (b) main's exports.
//
// Symbols resolved against the registry already have a table slot
// (placed there by the source library's post-load shim) — we just
// emit an i32-const global with that slot's index, no fresh import
// or elem placement required.
//
// Symbols resolved against main need (1) a func import (signature
// pulled from main.ExportedFunction(sym)) and (2) an active elem
// segment placing the imported func at table[gotFuncBase + i].
//
// `a` is the adapter — locked by the caller, so resolveFunc can
// read the registry safely.
func (a *Adapter) buildGotFuncShim(info *soInfo, main api.Module, gotFuncBase int32) ([]byte, error) {
	spec := &envModuleSpec{}

	// Decide per-target whether the symbol comes from main (needs
	// elem placement here) or a loaded library (just record the
	// index). Allocate fresh slots in [gotFuncBase, gotFuncBase+G)
	// only for main-sourced symbols.
	type gotResolution struct {
		fromMain     bool
		tableIdx     uint32 // populated when fromMain=false
		mainFuncIdx  uint32 // populated when fromMain=true; index into func index space of this shim
	}
	resolutions := make([]gotResolution, len(info.gotFuncImports))

	// First pass: import main's table (only if we'll do any elem
	// placement) and the per-target funcs that come from main.
	var needTable bool
	for _, sym := range info.gotFuncImports {
		_, foundInLib, mainHas := a.resolveFunc(sym, main)
		if !foundInLib && mainHas {
			needTable = true
			break
		}
	}
	if needTable {
		spec.imports = append(spec.imports, envImport{
			module:   "main",
			name:     "__indirect_function_table",
			kind:     importKindTable,
			refType:  valFuncRef,
			minTable: 0,
		})
	}

	mainFuncImportCount := uint32(0)
	for i, sym := range info.gotFuncImports {
		idx, foundInLib, mainHas := a.resolveFunc(sym, main)
		switch {
		case foundInLib:
			resolutions[i] = gotResolution{fromMain: false, tableIdx: idx}
		case mainHas:
			mainFn := main.ExportedFunction(sym)
			def := mainFn.Definition()
			ft := funcType{
				params:  valueTypesToBytes(def.ParamTypes()),
				results: valueTypesToBytes(def.ResultTypes()),
			}
			resolutions[i] = gotResolution{
				fromMain:    true,
				mainFuncIdx: mainFuncImportCount,
			}
			mainFuncImportCount++
			spec.imports = append(spec.imports, envImport{
				module:  "main",
				name:    sym,
				kind:    importKindFunc,
				typeIdx: spec.internType(ft),
			})
		default:
			return nil, fmt.Errorf("GOT.func.%s: not resolved by main or any loaded library", sym)
		}
	}

	// Reserve fresh table slots only for main-sourced symbols. Each
	// such slot lives at gotFuncBase+0, gotFuncBase+1, ... in
	// declaration order (matches our adapter's table accounting).
	mainSlotOffset := int32(0)
	for i := range info.gotFuncImports {
		if resolutions[i].fromMain {
			resolutions[i].tableIdx = uint32(gotFuncBase + mainSlotOffset)
			mainSlotOffset++
		}
	}

	// Active elem segment placing main-sourced funcs into main's
	// table at the fresh slots. funcIndices lists each main-sourced
	// shim-local func index in the order they go into the table.
	if mainFuncImportCount > 0 {
		funcIndices := make([]uint32, 0, mainFuncImportCount)
		for _, r := range resolutions {
			if r.fromMain {
				funcIndices = append(funcIndices, r.mainFuncIdx)
			}
		}
		spec.elems = append(spec.elems, envElem{
			tableIdx:    0,
			offset:      gotFuncBase,
			funcIndices: funcIndices,
		})
	}

	// Own mutable i32 globals — one per GOT.func.X target. The
	// global's value is the absolute table index (either freshly
	// allocated above for main-sourced targets, or recorded by the
	// registry for loaded-lib-sourced targets).
	//
	// The global is mutable because wasm-ld emits the .so's import
	// as mutable (its loader can update it at any time); wazero
	// validates kind+mutability match between import and exporting
	// source.
	for i, sym := range info.gotFuncImports {
		globalIdx := uint32(i)
		spec.globals = append(spec.globals, envGlobal{
			valType: valI32,
			mutable: true,
			initI32: int32(resolutions[i].tableIdx),
		})
		spec.exports = append(spec.exports, envExport{
			name: sym,
			kind: exportKindGlobal,
			idx:  globalIdx,
		})
	}

	return spec.encode()
}

// buildGotMemShim synthesizes the module that satisfies the .so's
// GOT.mem.X imports. addresses[i] is the resolved memory address for
// info.gotMemImports[i] — picked up by the caller from the loader's
// registry first, then main's exported data-symbol globals.
//
// Source-tracking is simpler for GOT.mem than GOT.func: data addresses
// are intrinsic memory values, no table-slot allocation needed
// regardless of where the symbol originated.
func buildGotMemShim(info *soInfo, addresses []int32) ([]byte, error) {
	if len(addresses) != len(info.gotMemImports) {
		return nil, fmt.Errorf("buildGotMemShim: have %d addresses, need %d", len(addresses), len(info.gotMemImports))
	}
	spec := &envModuleSpec{}
	for i, sym := range info.gotMemImports {
		globalIdx := uint32(i)
		spec.globals = append(spec.globals, envGlobal{
			valType: valI32,
			mutable: true,
			initI32: addresses[i],
		})
		spec.exports = append(spec.exports, envExport{
			name: sym,
			kind: exportKindGlobal,
			idx:  globalIdx,
		})
	}
	return spec.encode()
}

// buildPostLoadShim synthesizes a tiny wasm module whose only job is
// to register the .so's by-name function exports into main's
// __indirect_function_table. The .so's own elem section only places
// functions whose addresses are taken inside the .so — exports like
// `PyInit__hello` (called via dlsym from outside) are NOT in the
// table after the .so instantiates, so call_indirect against them
// would fail.
//
// The shim imports each export from the .so as a func, imports main's
// table, and runs an active elem segment placing them at
// table[shimBase], table[shimBase+1], ... in declaration order. After
// the shim instantiates, library.symbol(name) returns
// shimBase + ordering.
func buildPostLoadShim(info *soInfo, exportOrder []string, shimBase int32) ([]byte, error) {
	spec := &envModuleSpec{}

	// Import the shared __indirect_function_table from the
	// wit-component-synthesized $main module so we can write the
	// .so's exports into it.
	spec.imports = append(spec.imports, envImport{
		module:   "main",
		name:     "__indirect_function_table",
		kind:     importKindTable,
		refType:  valFuncRef,
		minTable: 0,
	})

	// Import each export the .so provides (in declaration order).
	// Each import's type MUST match the .so's actual export signature
	// — wazero validates this at instantiate time. We look up each
	// own function's type from info.ownFuncTypeIdx and intern it in
	// the shim's type section.
	funcIndices := make([]uint32, 0, len(exportOrder))
	for i, name := range exportOrder {
		ownIdx, ok := info.exports[name]
		if !ok {
			return nil, fmt.Errorf("buildPostLoadShim: export %q not in info.exports", name)
		}
		if int(ownIdx) >= len(info.ownFuncTypeIdx) {
			return nil, fmt.Errorf("buildPostLoadShim: ownIdx %d out of range (have %d)", ownIdx, len(info.ownFuncTypeIdx))
		}
		typeIdxInSo := info.ownFuncTypeIdx[ownIdx]
		if int(typeIdxInSo) >= len(info.types) {
			return nil, fmt.Errorf("buildPostLoadShim: typeIdx %d out of range", typeIdxInSo)
		}
		ft := info.types[typeIdxInSo]
		spec.imports = append(spec.imports, envImport{
			module:  "so",
			name:    name,
			kind:    importKindFunc,
			typeIdx: spec.internType(ft),
		})
		// Table import doesn't contribute to func index space; each
		// imported func takes the next func index starting at 0.
		funcIndices = append(funcIndices, uint32(i))
	}

	spec.elems = append(spec.elems, envElem{
		tableIdx:    0,
		offset:      shimBase,
		funcIndices: funcIndices,
	})

	return spec.encode()
}

// load is the body of dyld.load. Walks the registry first (re-dlopen
// returns a fresh handle on the same library, bumping refcount), then
// NEEDED dependencies (each adds one refcount edge so cascading
// dlclose works correctly), then reads `name` from the adapter's
// fs.FS, parses it, allocates memory + table room from main, builds
// the env + GOT + post-load shim modules, instantiates everything,
// runs the init functions, records the new library in the registry,
// and returns a libraryImpl.
//
// `main` is the captured inject-capture core module, supplied by the
// adapter's Load entry point from Initialize-time state.
func (a *Adapter) load(ctx context.Context, main api.Module, name string) (*libraryImpl, error) {
	if main == nil {
		return nil, errNotConfigured
	}

	// Re-dlopen of an already-loaded library: bump refcount and
	// return a new libraryImpl pointing at the existing record.
	// Matches POSIX dlopen — each call returns a handle that Drop
	// will decrement one slot of. Pre-loaded entries (soInst==nil)
	// stay at refCount=0; they aren't owned by us.
	if existing, ok := a.loaded[name]; ok {
		if !existing.isPreloaded() {
			existing.refCount++
		}
		return &libraryImpl{a: a, lib: existing}, nil
	}
	// Cycle detection — recursive NEEDED processing might call
	// load(A → B → A). Bail clearly instead of looping.
	if a.loading[name] {
		return nil, fmt.Errorf("dyld: cycle detected loading %q", name)
	}
	a.loading[name] = true
	defer delete(a.loading, name)

	soBytes, err := fs.ReadFile(a.cfg.FS, name)
	if err != nil {
		return nil, fmt.Errorf("read .so %q: %w", name, err)
	}

	info, err := parseSO(soBytes)
	if err != nil {
		return nil, fmt.Errorf("parse .so %q: %w", name, err)
	}

	// Honor the dylink.0 MEM_INFO. memorysize is the loader's
	// responsibility per spec ("Size of the memory area the loader
	// should reserve for the module, which will begin at
	// env.__memory_base"); memoryalignment is encoded as a
	// power-of-2 (so log2=4 means 16-byte alignment).
	//
	// We over-allocate by (1 << align) - 1 bytes so we can round
	// the malloc result up to the required alignment. wasi-libc's
	// malloc returns 16-byte-aligned pointers in practice, so for
	// log2 <= 4 the over-allocation is paid but never consumed.
	mallocFn := main.ExportedFunction("malloc")
	if mallocFn == nil {
		return nil, fmt.Errorf("captured module %q does not export malloc", main.Name())
	}
	alignBytes := uint32(1) << info.memoryAlignLog2
	allocBytes := info.memorySize + alignBytes - 1
	if allocBytes == 0 {
		// memorysize=0 is legal (no data segments); skip the malloc.
		allocBytes = 0
	}
	var memoryBase int32
	if allocBytes > 0 {
		results, err := mallocFn.Call(ctx, uint64(allocBytes))
		if err != nil {
			return nil, fmt.Errorf("malloc(%d): %w", allocBytes, err)
		}
		raw := uint32(results[0])
		if raw == 0 {
			return nil, fmt.Errorf("malloc(%d) returned NULL", allocBytes)
		}
		// Round up to alignment boundary.
		aligned := (raw + alignBytes - 1) &^ (alignBytes - 1)
		memoryBase = int32(aligned)

		// Zero-init the reserved region — spec says the loader
		// "will reserve room in memory of that size and initialize
		// it to zero". The .so's data segments overwrite parts of
		// this on instantiate; uninitialized bytes (BSS-style) must
		// be zero.
		mem := main.Memory()
		if mem == nil {
			return nil, fmt.Errorf("main module has no memory")
		}
		if !mem.Write(uint32(memoryBase), make([]byte, info.memorySize)) {
			return nil, fmt.Errorf("zero-init memory[%d..%d] out of bounds", memoryBase, uint32(memoryBase)+info.memorySize)
		}
	}

	// Reserve table room. The total table claim is:
	//
	//   [oldSize, oldSize+G)              — env shim's GOT.func slots
	//   [oldSize+G, oldSize+G+T)          — .so's own elem (.so's __table_base
	//                                      points here)
	//   [oldSize+G+T, oldSize+G+T+E)      — post-load shim's by-name exports
	//
	// where G = len(gotFuncImports), T = dylink.0 tablesize, E = len(exports).
	//
	// tableAlignLog2 is the .so's required alignment for its slice; if
	// nonzero we'd need to pad before the .so's region. wasm-ld --shared
	// emits 0 (no alignment) for typical .so output — we sanity-check.
	growFn := main.ExportedFunction("__grow_table")
	if growFn == nil {
		return nil, fmt.Errorf("main module does not export `__grow_table`")
	}
	if info.tableAlignLog2 > 0 {
		return nil, fmt.Errorf("tableAlignLog2=%d (need padding logic not yet implemented)", info.tableAlignLog2)
	}
	exportOrder := orderedExportNames(info)
	gotFuncCount := uint32(len(info.gotFuncImports))
	totalSlots := gotFuncCount + info.tableSize + uint32(len(exportOrder))
	results, err := growFn.Call(ctx, uint64(totalSlots))
	if err != nil {
		return nil, fmt.Errorf("__grow_table(%d): %w", totalSlots, err)
	}
	oldTableSize := int32(results[0])
	if oldTableSize < 0 {
		return nil, fmt.Errorf("__grow_table returned %d", oldTableSize)
	}
	gotFuncBase := oldTableSize
	tableBase := gotFuncBase + int32(gotFuncCount)
	shimBase := tableBase + int32(info.tableSize)

	// Resolve NEEDED libraries before building any of this .so's
	// shims — wasm-tools (and ELF) semantics: a library's
	// dependencies must be initialized first. We skip well-known
	// names that main statically satisfies (libc, libdl, etc.);
	// anything else is recursively loaded via the same path. Each
	// successful load increments the dep's refcount by one — that
	// edge represents this library's NEEDED reference and will be
	// released when this library is dropped.
	var neededDeps []*loadedLibrary
	for _, dep := range info.needed {
		if mainSatisfiedNeeded[dep] {
			continue
		}
		// If the dep is already in our registry (preloaded OR
		// previously dlopen'd) bump its refcount directly — we don't
		// need to walk the full load() path again.
		if existing, ok := a.loaded[dep]; ok {
			if !existing.isPreloaded() {
				existing.refCount++
				neededDeps = append(neededDeps, existing)
			}
			continue
		}
		depImpl, err := a.load(ctx, main, dep)
		if err != nil {
			// Roll back NEEDED edges we already established so a
			// partial failure here doesn't leak refs on previously
			// loaded deps.
			for _, d := range neededDeps {
				a.unref(d)
			}
			return nil, fmt.Errorf("NEEDED %q (required by %q): %w", dep, name, err)
		}
		neededDeps = append(neededDeps, depImpl.lib)
	}

	// Now that NEEDED is loaded, decide where each env funcImport
	// comes from: a runtime-loaded library whose soInst exports the
	// symbol, or "main" (captured) for everything else.
	envFuncSources := a.resolveEnvFuncSources(info, main)

	// Build + instantiate env shim.
	envBytes, err := a.buildEnvModule(info, envFuncSources, memoryBase, tableBase, gotFuncBase)
	if err != nil {
		return nil, fmt.Errorf("build env module: %w", err)
	}
	envMod, err := a.runtime.CompileModule(ctx, envBytes)
	if err != nil {
		return nil, fmt.Errorf("compile env module: %w", err)
	}
	envCtx := experimental.WithImportResolver(ctx, a.makeResolver(main))
	envInst, err := a.runtime.InstantiateModule(envCtx, envMod, wazero.NewModuleConfig().
		WithName("").WithStartFunctions())
	if err != nil {
		return nil, fmt.Errorf("instantiate env module: %w", err)
	}

	// GOT shims — only when the .so requests GOT entries. Each is a
	// separate module since the .so imports from distinct module
	// names (`GOT.func`, `GOT.mem`).
	var gotFuncInst api.Module
	if len(info.gotFuncImports) > 0 {
		gotFuncBytes, err := a.buildGotFuncShim(info, main, gotFuncBase)
		if err != nil {
			return nil, fmt.Errorf("build GOT.func shim: %w", err)
		}
		gotFuncMod, err := a.runtime.CompileModule(ctx, gotFuncBytes)
		if err != nil {
			return nil, fmt.Errorf("compile GOT.func shim: %w", err)
		}
		gotFuncCtx := experimental.WithImportResolver(ctx, a.makeResolver(main))
		gotFuncInst, err = a.runtime.InstantiateModule(gotFuncCtx, gotFuncMod, wazero.NewModuleConfig().
			WithName("").WithStartFunctions())
		if err != nil {
			return nil, fmt.Errorf("instantiate GOT.func shim: %w", err)
		}
	}

	var gotMemInst api.Module
	if len(info.gotMemImports) > 0 {
		// Resolve each GOT.mem.X by consulting the loader's registry
		// first, then main's exported data-symbol globals. wasm-ld
		// with --export-dynamic emits the latter for data symbols;
		// a missing entry surfaces explicitly so the user knows
		// whether to add --export=X to main or to register a
		// pre-loaded library that defines X.
		addresses := make([]int32, len(info.gotMemImports))
		for i, sym := range info.gotMemImports {
			addr, ok := a.resolveData(sym, main)
			if !ok {
				return nil, fmt.Errorf("GOT.mem.%s: not resolved by main or any loaded library", sym)
			}
			addresses[i] = int32(addr)
		}
		gotMemBytes, err := buildGotMemShim(info, addresses)
		if err != nil {
			return nil, fmt.Errorf("build GOT.mem shim: %w", err)
		}
		gotMemMod, err := a.runtime.CompileModule(ctx, gotMemBytes)
		if err != nil {
			return nil, fmt.Errorf("compile GOT.mem shim: %w", err)
		}
		gotMemInst, err = a.runtime.InstantiateModule(ctx, gotMemMod, wazero.NewModuleConfig().
			WithName("").WithStartFunctions())
		if err != nil {
			return nil, fmt.Errorf("instantiate GOT.mem shim: %w", err)
		}
	}

	// Compile + instantiate the .so. Its "env" import resolves to
	// envInst; "GOT.func" → gotFuncInst; "GOT.mem" → gotMemInst.
	soMod, err := a.runtime.CompileModule(ctx, soBytes)
	if err != nil {
		return nil, fmt.Errorf("compile .so: %w", err)
	}
	soCtx := experimental.WithImportResolver(ctx, func(mn string) api.Module {
		switch mn {
		case "env":
			return envInst
		case "GOT.func":
			return gotFuncInst
		case "GOT.mem":
			return gotMemInst
		}
		return nil
	})
	soInst, err := a.runtime.InstantiateModule(soCtx, soMod, wazero.NewModuleConfig().
		WithName("").WithStartFunctions())
	if err != nil {
		return nil, fmt.Errorf("instantiate .so: %w", err)
	}

	// Run wasm-ld's synthesized init functions in the order the spec
	// + wasm-ld source dictate (lld/wasm/Writer.cpp):
	//
	//   1. __wasm_init_memory                — start function, ran
	//                                          automatically at instantiate
	//   2. __wasm_apply_data_relocs          — passive-segment data init
	//   3. __wasm_apply_global_relocs        — global-value relocations
	//   4. __wasm_call_ctors                 — C++ ctors / static inits
	//   5. _initialize                       — WASI reactor entry, if exported
	//
	// We skip the TLS-related synthetics (__wasm_init_tls,
	// __wasm_apply_tls_relocs, __wasm_apply_global_tls_relocs) —
	// single-threaded WASI doesn't need them, and they take a tls_base
	// arg we don't supply.
	for _, fname := range []string{
		"__wasm_apply_data_relocs",
		"__wasm_apply_global_relocs",
		"__wasm_call_ctors",
		"_initialize",
	} {
		fn := soInst.ExportedFunction(fname)
		if fn == nil {
			continue
		}
		if _, err := fn.Call(ctx); err != nil {
			return nil, fmt.Errorf("%s: %w", fname, err)
		}
	}

	// Post-load shim: places the .so's by-name exports into main's
	// table at indices shimBase .. shimBase+len(exports)-1. After
	// instantiation, dlsym/Symbol and resolveFunc consult the
	// library's exports map.
	exports := make(map[string]uint32, len(exportOrder)+len(info.dataExportNames))
	for i, n := range exportOrder {
		exports[n] = uint32(int32(shimBase) + int32(i))
	}
	var shimInst api.Module
	if len(exportOrder) > 0 {
		shimBytes, err := buildPostLoadShim(info, exportOrder, shimBase)
		if err != nil {
			return nil, fmt.Errorf("build post-load shim: %w", err)
		}
		shimMod, err := a.runtime.CompileModule(ctx, shimBytes)
		if err != nil {
			return nil, fmt.Errorf("compile post-load shim: %w", err)
		}
		shimCtx := experimental.WithImportResolver(ctx, func(mn string) api.Module {
			switch mn {
			case "main":
				return main
			case "so":
				return soInst
			}
			return nil
		})
		shimInst, err = a.runtime.InstantiateModule(shimCtx, shimMod, wazero.NewModuleConfig().
			WithName("").WithStartFunctions())
		if err != nil {
			return nil, fmt.Errorf("instantiate post-load shim: %w", err)
		}
	}

	// Data exports: for each global the .so exports, read its
	// value (= memory address per wasm-ld --shared conventions) so
	// future loads can resolve GOT.mem.X against this library.
	// Populated from info.dataExportNames (parsed from the .so's
	// export section) via wazero ExportedGlobal lookup post-
	// instantiation. Folded into the same map as function exports
	// since wasm-tools' libdl convention doesn't distinguish kind
	// at the symbol level.
	for _, gname := range info.dataExportNames {
		if g := soInst.ExportedGlobal(gname); g != nil {
			exports[gname] = uint32(g.Get())
		}
	}

	lib := &loadedLibrary{
		name:         name,
		exports:      exports,
		refCount:     1, // caller's reference; NEEDED parents bump on Load
		neededDeps:   neededDeps,
		soInst:       soInst,
		envInst:      envInst,
		gotFuncInst:  gotFuncInst,
		gotMemInst:   gotMemInst,
		postShimInst: shimInst,
	}
	a.loaded[name] = lib

	return &libraryImpl{a: a, lib: lib}, nil
}

// resolveEnvFuncSources picks the wasm import-module name for each of
// the .so's env funcImports. Order of preference:
//
//  1. A runtime-loaded library (soInst != nil) whose soInst exports
//     the symbol by name — the env-shim imports directly from that
//     library and the ImportResolver routes lib.name → lib.soInst.
//  2. "main" — the captured inject-capture module, which re-exports
//     the union of $main + every pre-loaded library's surface.
//
// Pre-loaded libraries (soInst == nil) live inside the captured
// module, so they fall under case 2 — there is no separate api.Module
// to address them by. A symbol no one provides ends up routed to
// "main" anyway, and wazero will surface the missing-import error at
// env-module instantiate time with a clear name.
func (a *Adapter) resolveEnvFuncSources(info *soInfo, main api.Module) []string {
	out := make([]string, len(info.funcImports))
	for i, fi := range info.funcImports {
		out[i] = "main"
		for _, lib := range a.loaded {
			if lib.isPreloaded() {
				continue
			}
			if lib.soInst.ExportedFunction(fi.name) != nil {
				out[i] = lib.name
				break
			}
		}
	}
	return out
}

// makeResolver returns an ImportResolver suitable for env/GOT shim
// instantiation: "main" routes to the captured module; every
// runtime-loaded library name routes to that library's soInst.
// Pre-loaded entries don't participate — they're inside `main`.
func (a *Adapter) makeResolver(main api.Module) func(string) api.Module {
	return func(mn string) api.Module {
		if mn == "main" {
			return main
		}
		if lib, ok := a.loaded[mn]; ok && !lib.isPreloaded() {
			return lib.soInst
		}
		return nil
	}
}

// orderedExportNames returns the .so's exported function names in a
// deterministic order — sorted, so re-instantiating the same .so
// produces the same name→index mapping.
func orderedExportNames(info *soInfo) []string {
	out := make([]string, 0, len(info.exports))
	for n := range info.exports {
		out = append(out, n)
	}
	// Stable order for determinism + reproducibility of bug reports.
	// Use a stdlib sort.
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	// Simple insertion sort — list is tiny (single-digit elements
	// typical for a C extension).
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

