// sig.go — parser + formatter for the libffi sig descriptor.
//
// V2 layout (variable length):
//
//   byte 0:  header
//             bit 0 = varargs (function takes ... after nfixedargs)
//             bits 1-7 reserved (must be 0)
//   byte 1:  nfixedargs (0..255; equal to nparams when bit 0 clear)
//   byte 2:  return type encoding (0x00 = void, else a type byte)
//             return-by-struct/longdouble uses tag 0x10/0x12 here too —
//             the trampoline lowers those to sret form (return void,
//             prepend rvalue as the first wasm arg).
//   byte 3:  nparams (count of param-type encodings that follow)
//   byte 4..: param type encodings (variable length each)
//
// Type encodings (one per param, plus one for return):
//   0x7f i32   — wasm valtype byte for i32
//   0x7e i64   — wasm valtype byte for i64
//   0x7d f32   — wasm valtype byte for f32
//   0x7c f64   — wasm valtype byte for f64
//   0x10        — long double; expands to 2× i64 in the wasm sig
//   0x12 N <t1> <t2> ... <tN>
//                — flattened struct: N primitive-byte fields. Each
//                  field is a single byte from the i32/i64/f32/f64 set.
//                  Multi-byte expansions (longdouble or nested struct)
//                  are NOT supported inside a flattened struct — the
//                  Rust shim should unbox in those cases before encoding.
//
// The Rust shim (components/python-runtime/src/libffi_shim.rs) is
// responsible for the unbox_small_structs algorithm: looking at an
// ffi_type, deciding between struct-by-pointer (encode as i32) and
// struct-flat (encode as 0x12 + flat fields). The host side just
// processes the descriptor it's given.
package libffi

import (
	"errors"
	"fmt"
)

// Type-byte sentinels.
const (
	TyVoid       byte = 0x00
	TyI32        byte = 0x7f
	TyI64        byte = 0x7e
	TyF32        byte = 0x7d
	TyF64        byte = 0x7c
	TyLongDouble byte = 0x10
	TyStructFlat byte = 0x12
)

// HeaderVarargs flag bit.
const HeaderVarargs byte = 0x01

// ParsedSig is the structured form of the descriptor — what the
// trampoline-builder consumes. Construct via ParseSig.
type ParsedSig struct {
	Varargs     bool
	NFixedArgs  uint8
	Return      ParsedType   // zero (TyVoid) for void
	Params      []ParsedType // length == NParams in the descriptor
}

// ParsedType is one parsed type slot — either a primitive valtype or
// an aggregate (struct, longdouble) with sub-fields.
type ParsedType struct {
	Kind byte // one of TyVoid, TyI32, ..., TyLongDouble, TyStructFlat
	// FlatFields is set only when Kind == TyStructFlat. Each entry is
	// itself a ParsedType, but only primitive Kinds (TyI32..TyF64) are
	// allowed (no nesting). Encoder + parser enforce this.
	FlatFields []ParsedType
}

// LoweredValtypes returns the sequence of wasm valtype bytes a
// ParsedType expands to on the wasm stack. Primitives map 1:1; long
// double splits into 2× i64; struct-flat unfolds to its fields.
func (p ParsedType) LoweredValtypes() []byte {
	switch p.Kind {
	case TyVoid:
		return nil
	case TyI32, TyI64, TyF32, TyF64:
		return []byte{p.Kind}
	case TyLongDouble:
		return []byte{TyI64, TyI64}
	case TyStructFlat:
		out := make([]byte, 0, len(p.FlatFields))
		for _, f := range p.FlatFields {
			out = append(out, f.LoweredValtypes()...)
		}
		return out
	}
	return nil
}

// ByteSize returns the in-memory byte size of the type, for sizing
// the avalue[] backing pointers. For struct-flat, this is the sum
// of the field sizes (no padding — the C ABI is the caller's
// responsibility, and the field types already include the layout
// the Rust shim derived from FFI_TYPE_STRUCT's element list).
func (p ParsedType) ByteSize() int {
	switch p.Kind {
	case TyVoid:
		return 0
	case TyI32, TyF32:
		return 4
	case TyI64, TyF64, TyLongDouble:
		// long double is 16 bytes on wasm32 IEEE-quad — but we lower
		// it to 2× i64 on the wasm stack. The avalue ptr still has
		// the full 16-byte payload.
		if p.Kind == TyLongDouble {
			return 16
		}
		return 8
	case TyStructFlat:
		n := 0
		for _, f := range p.FlatFields {
			n += f.ByteSize()
		}
		return n
	}
	return 0
}

// IsAggregate reports whether the type is something other than a
// single primitive — used by callers to decide if sret marshaling
// is needed for return values.
func (p ParsedType) IsAggregate() bool {
	switch p.Kind {
	case TyLongDouble, TyStructFlat:
		return true
	}
	return false
}

// ParseSig decodes the descriptor into a ParsedSig or returns an
// error explaining the format violation. Used by the trampoline
// builder and by the WIT-level validation.
func ParseSig(sig []byte) (*ParsedSig, error) {
	r := sigReader{buf: sig}
	hdr, err := r.byte_()
	if err != nil {
		return nil, fmt.Errorf("header: %w", err)
	}
	if hdr & ^HeaderVarargs != 0 {
		return nil, fmt.Errorf("header: reserved bits set (0x%02x)", hdr)
	}
	nfixed, err := r.byte_()
	if err != nil {
		return nil, fmt.Errorf("nfixedargs: %w", err)
	}
	rt, err := parseType(&r)
	if err != nil {
		return nil, fmt.Errorf("return type: %w", err)
	}
	nparams, err := r.byte_()
	if err != nil {
		return nil, fmt.Errorf("nparams: %w", err)
	}
	params := make([]ParsedType, 0, nparams)
	for i := uint8(0); i < nparams; i++ {
		pt, err := parseType(&r)
		if err != nil {
			return nil, fmt.Errorf("param %d: %w", i, err)
		}
		params = append(params, pt)
	}
	if r.remaining() != 0 {
		return nil, fmt.Errorf("%d trailing bytes after descriptor", r.remaining())
	}
	out := &ParsedSig{
		Varargs:    hdr&HeaderVarargs != 0,
		NFixedArgs: nfixed,
		Return:     rt,
		Params:     params,
	}
	if !out.Varargs && int(out.NFixedArgs) != len(params) {
		// Tolerate mismatch — many callers set nfixedargs == nparams
		// regardless of varargs flag. Just normalize.
		out.NFixedArgs = uint8(len(params))
	}
	return out, nil
}

// parseType consumes one type encoding from r.
func parseType(r *sigReader) (ParsedType, error) {
	b, err := r.byte_()
	if err != nil {
		return ParsedType{}, err
	}
	switch b {
	case TyVoid, TyI32, TyI64, TyF32, TyF64, TyLongDouble:
		return ParsedType{Kind: b}, nil
	case TyStructFlat:
		nfields, err := r.byte_()
		if err != nil {
			return ParsedType{}, fmt.Errorf("struct-flat nfields: %w", err)
		}
		fields := make([]ParsedType, 0, nfields)
		for i := uint8(0); i < nfields; i++ {
			fb, err := r.byte_()
			if err != nil {
				return ParsedType{}, fmt.Errorf("struct-flat field %d: %w", i, err)
			}
			switch fb {
			case TyI32, TyI64, TyF32, TyF64:
				fields = append(fields, ParsedType{Kind: fb})
			default:
				return ParsedType{}, fmt.Errorf("struct-flat field %d: nested aggregates not allowed (0x%02x)", i, fb)
			}
		}
		return ParsedType{Kind: TyStructFlat, FlatFields: fields}, nil
	default:
		return ParsedType{}, fmt.Errorf("unknown type byte 0x%02x", b)
	}
}

type sigReader struct {
	buf []byte
	pos int
}

func (r *sigReader) byte_() (byte, error) {
	if r.pos >= len(r.buf) {
		return 0, errors.New("unexpected end of descriptor")
	}
	b := r.buf[r.pos]
	r.pos++
	return b, nil
}

func (r *sigReader) remaining() int { return len(r.buf) - r.pos }
