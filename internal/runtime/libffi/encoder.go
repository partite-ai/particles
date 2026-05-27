// encoder.go — tiny wasm-binary helpers (LEB128, sections, names).
// Used by both trampoline builders.
package libffi

// uleb128 encodes a uint32 as unsigned LEB128. All values used in
// this package fit comfortably in a few bytes, but we use the full
// variable-length form for safety.
func uleb128(v uint32) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			out = append(out, b|0x80)
			continue
		}
		out = append(out, b)
		return out
	}
}

// sleb128 encodes an int64 as signed LEB128 — used for active elem
// segment offsets (i32.const N) and address offsets in i32.add.
func sleb128(v int64) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		s := v >> 7
		signBit := b & 0x40
		if (s == 0 && signBit == 0) || (s == -1 && signBit != 0) {
			out = append(out, b)
			return out
		}
		out = append(out, b|0x80)
		v = s
	}
}

// appendSection writes a wasm section header (id + length-prefixed
// payload) into out and returns the extended slice.
func appendSection(out []byte, id byte, payload []byte) []byte {
	out = append(out, id)
	out = append(out, uleb128(uint32(len(payload)))...)
	return append(out, payload...)
}

// appendStr writes a length-prefixed wasm name (LEB128 length + UTF-8
// bytes).
func appendStr(out []byte, s string) []byte {
	out = append(out, uleb128(uint32(len(s)))...)
	return append(out, s...)
}
