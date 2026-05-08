// This file holds only the //go:generate directive for the kv
// capability. Package documentation lives in kv.go.
//
// Bindings layout (shared with credentials/oauth/signing):
//
//	internal/host/
//	├── wit/
//	│   ├── host.wit                          // package particle:host (all four interfaces)
//	│   └── kv-gen.wit                        // synthetic `kv-gen` world
//	└── gen/particle/host/kv/                 // generated bindings (committed)
//
//	kv/                                       // this package
//	├── doc.go                                // this file + go:generate
//	├── kv.go                                 // Store interface + types
//	├── host.go                               // adapter + manager
//	├── *_test.go
//	└── memory/                               // in-memory Store impl
//
// To regenerate after a WIT change, run `go generate ./kv/`.
//
//go:generate wacogo-witgen generate -w particle:host/kv-gen -o ../internal/host/gen -p github.com/partite-ai/particle/internal/host/gen ../internal/host/wit/

package kv
