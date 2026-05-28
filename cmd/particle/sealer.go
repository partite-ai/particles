package main

import (
	"fmt"
	"io"

	credsqlite "github.com/partite-ai/particles/credentials/sqlite"
)

// openCredentialSealer returns the Sealer used to encrypt credential
// secrets at rest. It prefers the OS keyring (KeyringSealer); when the
// keyring can't be reached it falls back to storing secrets
// UNENCRYPTED and writes a warning to warnW instead of failing the
// command.
//
// The common trigger is Linux without a running D-Bus / Secret Service
// (headless servers, minimal containers): go-keyring surfaces that as
// a raw connection error with no stable sentinel, so we treat any
// construction failure as "keyring unavailable" and degrade. That's a
// deliberate availability-over-confidentiality choice — a particle that
// can't open the keyring would otherwise be unusable — and the warning
// keeps the downgrade visible so operators who need encryption know to
// provide a Secret Service.
//
// warnW is typically cmd.ErrOrStderr(); pass nil to suppress the
// warning (the fallback still happens).
func openCredentialSealer(warnW io.Writer) credsqlite.Sealer {
	sealer, err := credsqlite.NewKeyringSealer(keyringService, keyringName)
	if err == nil {
		return sealer
	}
	if warnW != nil {
		fmt.Fprintf(warnW, "warning: OS keyring unavailable (%v);\n"+
			"         storing credential secrets UNENCRYPTED in the state DB\n", err)
	}
	return credsqlite.PlaintextSealer{}
}
