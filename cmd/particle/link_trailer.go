package main

import (
	"bytes"
	"encoding/binary"
)

// The Windows link is the embedded trampoline .exe with a trailer
// appended that tells it what to run. The trampoline (see
// components/win-trampoline) reads its own file from the end:
//
//	[ trampoline.exe bytes ][ payload ][ u32 payloadLen ][ 8-byte magic ]
//
// payload is a little-endian length-prefixed UTF-8 argv that the
// trampoline execs, with the caller's runtime arguments appended:
//
//	u32 argc
//	argc × ( u32 byteLen, <byteLen> UTF-8 bytes )
//
// argv[0] is the particle binary to launch; the rest is the fixed
// `run [--db <db>] <target>` prefix. Keep this in lockstep with the
// parser in components/win-trampoline/src/main.rs.
//
// This lives in a non-build-tagged file (it touches no OS APIs) so the
// format stays unit-testable on any host, not just Windows.
const trampolineMagic = "PRTCLNK1"

// encodeTrailer renders the payload + length + magic appended to the
// stub. argv[0] is the particle binary so the trampoline knows what
// to CreateProcess; the rest is the run-line prefix the trampoline
// appends the caller's runtime arguments to. Argv order matches the
// Unix shim:
//
//	particle run [--db X] [--mount A=a]... [--trace-http=lvl] <target> [<tool>]
func encodeTrailer(spec linkSpec) []byte {
	args := []string{spec.particleBin, "run"}
	if spec.dbPath != "" {
		args = append(args, "--db", spec.dbPath)
	}
	for _, m := range spec.mounts {
		args = append(args, "--mount", m)
	}
	if spec.traceLevelSet {
		// --trace-http requires the `=` form (see shimScript for
		// the NoOptDefVal explanation).
		args = append(args, "--trace-http="+traceLevelName(spec.traceLevel))
	}
	args = append(args, spec.target)
	if spec.tool != "" {
		args = append(args, spec.tool)
	}

	var payload bytes.Buffer
	var u32 [4]byte
	binary.LittleEndian.PutUint32(u32[:], uint32(len(args)))
	payload.Write(u32[:])
	for _, a := range args {
		binary.LittleEndian.PutUint32(u32[:], uint32(len(a)))
		payload.Write(u32[:])
		payload.WriteString(a)
	}

	var out bytes.Buffer
	out.Write(payload.Bytes())
	binary.LittleEndian.PutUint32(u32[:], uint32(payload.Len()))
	out.Write(u32[:])
	out.WriteString(trampolineMagic)
	return out.Bytes()
}
