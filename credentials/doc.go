// This file holds only the //go:generate directives for each host
// capability the package wires up. The package documentation lives
// in credentials.go (and oauth.go).
//
// Bindings layout:
//
//	internal/host/
//	├── wit/                                  // shared WIT for every host capability
//	│   ├── host.wit                          // package particle:host: all four interfaces
//	│   ├── credentials-gen.wit               // synthetic `credentials-gen` world
//	│   ├── oauth-gen.wit                     // synthetic `oauth-gen` world
//	│   ├── signing-gen.wit                   // synthetic `signing-gen` world
//	│   └── (kv lives in its own package outside credentials/)
//	└── gen/                                  // shared wacogo-witgen output (committed)
//	    └── particle/host/
//	        ├── credentials/                  // generated bindings
//	        ├── oauth/                        // generated bindings
//	        └── signing/                      // generated bindings
//
//	credentials/                              // this package
//	├── doc.go                                // this file + go:generate directives
//	├── credentials.go                        // Store interface + types
//	├── host.go                               // credentials adapter + factory
//	├── oauth.go                              // oauth adapter + factory + Refresher
//	└── *_test.go
//
// Each capability has its own `*-gen.wit` sibling in the same
// `particle:host` package, so the per-capability witgen run
// produces a focused binding subtree under
// gen/particle/host/<capability>/.
//
// To regenerate after a WIT change, run `go generate ./credentials/`.
// All directives below run in order; the gen output dir is shared
// so per-capability subtrees coexist.
//
//go:generate wacogo-witgen generate -w particle:host/credentials-gen -o ../internal/host/gen -p github.com/partite-ai/particle/internal/host/gen ../internal/host/wit/
//go:generate wacogo-witgen generate -w particle:host/oauth-gen       -o ../internal/host/gen -p github.com/partite-ai/particle/internal/host/gen ../internal/host/wit/
//go:generate wacogo-witgen generate -w particle:host/signing-gen     -o ../internal/host/gen -p github.com/partite-ai/particle/internal/host/gen ../internal/host/wit/

package credentials
