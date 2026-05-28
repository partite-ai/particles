package nodebuiltins

import (
	"embed"
	"io/fs"
)

//go:embed vendored
var vendoredFS embed.FS

// VendoredPackages returns an fs.FS whose top-level entries are npm
// package directories (e.g. `punycode/`) that the build overlays into
// the resolved node_modules tree before bundling.
//
// These are real npm packages a transitive dependency `require(...)`s
// but that the JS runtime can't satisfy through CJS require() —
// `require("punycode")` reaches tr46 (via whatwg-url / @googleapis) is
// the canonical case: the runtime ships punycode as an ESM builtin but
// never registers it in the CJS builtinModuleMap, so the require traps
// at load. Vendoring the package and overlaying it into node_modules
// lets esbuild bundle the implementation at build time instead of
// fetching it from npm on every build.
//
// esbuild tree-shakes anything unreferenced, so a vendored package no
// particle imports costs nothing in the output bundle — and a build
// with no npm deps never mounts node_modules at all, so these never
// leak into dependency-free particles.
func VendoredPackages() fs.FS {
	sub, err := fs.Sub(vendoredFS, "vendored")
	if err != nil {
		// vendoredFS is a compile-time embedded constant; a failure
		// here means the //go:embed directive and this Sub path
		// disagree, which is a build-time programming error.
		panic("nodebuiltins: vendored subtree missing: " + err.Error())
	}
	return sub
}
