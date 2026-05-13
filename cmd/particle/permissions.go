package main

import "github.com/partite-ai/particle/importer"

// permissionModeFromFlags converts the two mutually-exclusive CLI
// flags ("--yes" / "--confirm-permissions") into the importer's
// three-way enum. cobra's MarkFlagsMutuallyExclusive guarantees we
// won't see both set; the default (neither) is auto.
func permissionModeFromFlags(accept, confirm bool) importer.PermissionMode {
	switch {
	case accept:
		return importer.PermissionSkip
	case confirm:
		return importer.PermissionForce
	default:
		return importer.PermissionAuto
	}
}
