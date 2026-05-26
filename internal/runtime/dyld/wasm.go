package dyld

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// Minimal wasm-binary encoder, scoped to what we need for the per-load
// env module: type/import/global/export sections. No code section,
// no data section, no elem section — the env module is pure plumbing.
//
// References:
//   - WebAssembly Core Specification 2.0, Section 5 (binary format)
//   - https://webassembly.github.io/spec/core/binary/modules.html

const (
	wasmMagic = "\x00asm"
	wasmVer   = 1
)

const (
	secType   byte = 1
	secImport byte = 2
	secGlobal byte = 6
	secExport byte = 7
	secElem   byte = 9
)

const (
	valI32     byte = 0x7F
	valI64     byte = 0x7E
	valF32     byte = 0x7D
	valF64     byte = 0x7C
	valFuncRef byte = 0x70
)

const (
	importKindFunc   byte = 0x00
	importKindTable  byte = 0x01
	importKindMemory byte = 0x02
	importKindGlobal byte = 0x03
)

const (
	exportKindFunc   byte = 0x00
	exportKindTable  byte = 0x01
	exportKindMemory byte = 0x02
	exportKindGlobal byte = 0x03
)

const (
	opEnd     byte = 0x0B
	opI32Const byte = 0x41
)

// funcType is one entry in the type section. params and results are
// value type bytes (valI32 etc.).
type funcType struct {
	params  []byte
	results []byte
}

// envImport describes one import the env module declares against the
// caller's main module. ModuleName is always "main" in our setup;
// kept here to keep the encoder generic.
type envImport struct {
	module string
	name   string
	kind   byte

	// kind=Func: typeIdx is set
	typeIdx uint32

	// kind=Memory: minPages set (no max — match main)
	minPages uint32

	// kind=Table: minSize set, refType = valFuncRef
	minTable uint32
	refType  byte

	// kind=Global: valType + mutable
	gValType byte
	gMut     bool
}

// envExport describes one export the env module provides to the .so
// being loaded. Each export points at an index in the appropriate
// index space — for our generated env module, that means an imported
// item (re-exports) or a locally-defined global.
type envExport struct {
	name string
	kind byte
	idx  uint32
}

// envGlobal describes one locally-defined global in the env module.
// We emit two of these per load: __memory_base and __table_base,
// both i32 constants populated from the runtime malloc/table.grow
// results.
type envGlobal struct {
	valType byte
	mutable bool
	initI32 int32 // we only emit i32-const-init globals
}

// envModuleSpec collects everything needed to encode one env module.
type envModuleSpec struct {
	types   []funcType
	imports []envImport
	globals []envGlobal
	exports []envExport
	elems   []envElem
}

// envElem describes one elem segment in active-i32-const-offset form:
// `(elem (table 0) (i32.const offset) funcref funcIndices...)`.
// The encoder emits the wasm 2.0 flag-byte form (0x02 — table idx +
// reftype + offset expr + indices).
type envElem struct {
	tableIdx   uint32
	offset     int32
	funcIndices []uint32
}

// encode returns the wasm binary bytes for the spec. Section order
// matches the wasm spec's prescribed order (type, import, function,
// table, memory, global, export, start, elem, code, data, datacount);
// we only emit the four sections we use.
func (s *envModuleSpec) encode() ([]byte, error) {
	var w bytes.Buffer
	w.WriteString(wasmMagic)
	if err := binary.Write(&w, binary.LittleEndian, uint32(wasmVer)); err != nil {
		return nil, err
	}

	if len(s.types) > 0 {
		writeSection(&w, secType, func(b *bytes.Buffer) {
			writeULEB128(b, uint64(len(s.types)))
			for _, t := range s.types {
				b.WriteByte(0x60) // func type tag
				writeULEB128(b, uint64(len(t.params)))
				b.Write(t.params)
				writeULEB128(b, uint64(len(t.results)))
				b.Write(t.results)
			}
		})
	}

	if len(s.imports) > 0 {
		writeSection(&w, secImport, func(b *bytes.Buffer) {
			writeULEB128(b, uint64(len(s.imports)))
			for _, imp := range s.imports {
				writeName(b, imp.module)
				writeName(b, imp.name)
				b.WriteByte(imp.kind)
				switch imp.kind {
				case importKindFunc:
					writeULEB128(b, uint64(imp.typeIdx))
				case importKindMemory:
					// limits: flag=0 (no max) + min
					b.WriteByte(0x00)
					writeULEB128(b, uint64(imp.minPages))
				case importKindTable:
					b.WriteByte(imp.refType)
					b.WriteByte(0x00) // no max
					writeULEB128(b, uint64(imp.minTable))
				case importKindGlobal:
					b.WriteByte(imp.gValType)
					if imp.gMut {
						b.WriteByte(0x01)
					} else {
						b.WriteByte(0x00)
					}
				}
			}
		})
	}

	if len(s.globals) > 0 {
		writeSection(&w, secGlobal, func(b *bytes.Buffer) {
			writeULEB128(b, uint64(len(s.globals)))
			for _, g := range s.globals {
				b.WriteByte(g.valType)
				if g.mutable {
					b.WriteByte(0x01)
				} else {
					b.WriteByte(0x00)
				}
				// init expr: i32.const N + end
				b.WriteByte(opI32Const)
				writeSLEB128(b, int64(g.initI32))
				b.WriteByte(opEnd)
			}
		})
	}

	if len(s.exports) > 0 {
		writeSection(&w, secExport, func(b *bytes.Buffer) {
			writeULEB128(b, uint64(len(s.exports)))
			for _, e := range s.exports {
				writeName(b, e.name)
				b.WriteByte(e.kind)
				writeULEB128(b, uint64(e.idx))
			}
		})
	}

	if len(s.elems) > 0 {
		writeSection(&w, secElem, func(b *bytes.Buffer) {
			writeULEB128(b, uint64(len(s.elems)))
			for _, el := range s.elems {
				// Flag 0x02 = active + explicit table-idx + reftype +
				// vec(funcidx). We always write `funcref` since
				// __indirect_function_table is funcref.
				b.WriteByte(0x02)
				writeULEB128(b, uint64(el.tableIdx))
				// offset init expr: i32.const N + end
				b.WriteByte(opI32Const)
				writeSLEB128(b, int64(el.offset))
				b.WriteByte(opEnd)
				// reftype
				b.WriteByte(0x00) // 0 = func (elemkind for the func-idx vec form)
				writeULEB128(b, uint64(len(el.funcIndices)))
				for _, fi := range el.funcIndices {
					writeULEB128(b, uint64(fi))
				}
			}
		})
	}

	return w.Bytes(), nil
}

func writeSection(w *bytes.Buffer, id byte, body func(*bytes.Buffer)) {
	var content bytes.Buffer
	body(&content)
	w.WriteByte(id)
	writeULEB128(w, uint64(content.Len()))
	w.Write(content.Bytes())
}

func writeName(w *bytes.Buffer, s string) {
	writeULEB128(w, uint64(len(s)))
	w.WriteString(s)
}

// writeULEB128 emits an unsigned LEB128-encoded integer. Used for
// every "u32" field in the binary format (counts, indices, sizes).
func writeULEB128(w *bytes.Buffer, v uint64) {
	for {
		b := byte(v & 0x7F)
		v >>= 7
		if v != 0 {
			w.WriteByte(b | 0x80)
			continue
		}
		w.WriteByte(b)
		return
	}
}

// writeSLEB128 emits a signed LEB128-encoded integer. Used for the
// i32-const immediate in global init exprs.
func writeSLEB128(w *bytes.Buffer, v int64) {
	for {
		b := byte(v & 0x7F)
		// Sign bit of the byte (after shift).
		signBit := b & 0x40
		v >>= 7
		// "Done" when remaining value is all sign-extension of the
		// last byte's high bit.
		if (v == 0 && signBit == 0) || (v == -1 && signBit != 0) {
			w.WriteByte(b)
			return
		}
		w.WriteByte(b | 0x80)
	}
}

// internType deduplicates funcType entries against `s.types`,
// returning the index to use in import.typeIdx. New types are
// appended.
func (s *envModuleSpec) internType(ft funcType) uint32 {
	for i, existing := range s.types {
		if bytes.Equal(existing.params, ft.params) && bytes.Equal(existing.results, ft.results) {
			return uint32(i)
		}
	}
	s.types = append(s.types, ft)
	return uint32(len(s.types) - 1)
}

// valTypeBytes converts a sequence of wazero ValueType ints to the
// single-byte encoding the binary format uses. Only the value types
// we encounter on libpython/libc symbol signatures are handled —
// i32/i64/f32/f64. v128/funcref/externref aren't expected for the
// libpython surface we're re-exporting.
func valTypeBytes(types []byte) ([]byte, error) {
	out := make([]byte, 0, len(types))
	for _, t := range types {
		switch t {
		case valI32, valI64, valF32, valF64:
			out = append(out, t)
		default:
			return nil, fmt.Errorf("unsupported value type 0x%02x", t)
		}
	}
	return out, nil
}

// valueTypesToBytes converts a wazero api.ValueType slice (each is a
// byte under the hood, same encoding as the wasm spec — i32=0x7F
// etc.) into the byte slice the encoder expects.
func valueTypesToBytes(vts []byte) []byte {
	out := make([]byte, len(vts))
	copy(out, vts)
	return out
}
