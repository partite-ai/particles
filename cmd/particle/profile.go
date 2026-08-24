package main

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
)

// startProfile begins a CPU profile written to <prefix>.cpu and
// returns a stop function that:
//
//   - flushes and closes the CPU profile,
//   - takes a heap snapshot to <prefix>.heap (after a GC, so the
//     in-use bytes reflect what survived rather than the
//     allocator's working set).
//
// Wired to the hidden --profile global flag on the root command
// (see main.go). The flag covers every subcommand; the start runs
// in PersistentPreRunE and the stop runs from main() so error
// paths still flush the data. The flag isn't surfaced in --help —
// it's a debugging hatch, not a user-facing knob.
func startProfile(prefix string, log io.Writer) (stop func(), err error) {
	cpuPath := prefix + ".cpu"
	cpuFile, err := os.Create(cpuPath)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", cpuPath, err)
	}
	if err := pprof.StartCPUProfile(cpuFile); err != nil {
		_ = cpuFile.Close()
		return nil, fmt.Errorf("start CPU profile: %w", err)
	}

	// Create the output file for trace data
	tracePath := prefix + ".trace"
	traceFile, err := os.Create(tracePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create trace file: %v\n", err)
		os.Exit(1)
	}

	// Start tracing - all runtime events will be recorded from this point
	if err := trace.Start(traceFile); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start trace: %v\n", err)
		os.Exit(1)
	}

	return func() {
		trace.Stop()
		if err := traceFile.Close(); err != nil && log != nil {
			fmt.Fprintf(log, "profile: close trace: %v\n", err)
		}

		pprof.StopCPUProfile()
		if err := cpuFile.Close(); err != nil && log != nil {
			fmt.Fprintf(log, "profile: close cpu: %v\n", err)
		}

		heapPath := prefix + ".heap"
		heapFile, err := os.Create(heapPath)
		if err != nil {
			if log != nil {
				fmt.Fprintf(log, "profile: create %s: %v\n", heapPath, err)
			}
			return
		}
		// Force GC so the heap profile shows in-use bytes,
		// not whatever the allocator's working set happens to
		// look like at command exit.
		runtime.GC()
		if err := pprof.WriteHeapProfile(heapFile); err != nil && log != nil {
			fmt.Fprintf(log, "profile: write heap: %v\n", err)
		}
		if err := heapFile.Close(); err != nil && log != nil {
			fmt.Fprintf(log, "profile: close heap: %v\n", err)
		}

		if log != nil {
			fmt.Fprintf(log, "profile: wrote %s, %s and %s\n", cpuPath, heapPath, tracePath)
		}
	}, nil
}
