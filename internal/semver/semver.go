// Package semver carries the single regex + validator that both
// the build pipeline and the registry use to gate manifest
// versions.
//
// Two gates need this check:
//
//   - At build time: `internal/build` validates the manifest
//     produced by the introspect WASM, so a typo like
//     `version: "latest"` fails before assembly.
//   - At registration time: `registry/sqlite.Put` validates again,
//     so a tarball produced by a tool that DOESN'T run the build
//     pipeline still can't slip a bad version in.
//
// Keeping the rule in one place avoids the previous setup, where
// the same regex was duplicated in TypeScript (introspect.ts) and
// Go (registry/sqlite) and drifted on the next edit.
package semver

import "regexp"

// Re is the strict SemVer 2.0.0 regex suggested by semver.org —
// requires MAJOR.MINOR.PATCH, rejects leading zeros in numeric
// identifiers, supports optional `-PRERELEASE` and `+BUILD` parts,
// and forbids a leading `v` (manifest convention is bare numbers
// — `golang.org/x/mod/semver`'s leading-v dialect is added by
// callers when needed).
var Re = regexp.MustCompile(
	`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)` +
		`(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?` +
		`(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`,
)

// IsValid reports whether v is a valid SemVer 2.0.0 string. The
// package convention is bare numeric versions: `"1.2.3"` passes,
// `"v1.2.3"` does not.
func IsValid(v string) bool {
	return Re.MatchString(v)
}
