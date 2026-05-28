package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	credsqlite "github.com/partite-ai/particles/credentials/sqlite"
)

// When the keyring is reachable, openCredentialSealer returns the
// encrypting KeyringSealer and writes nothing to warnW.
func TestOpenCredentialSealer_KeyringAvailable(t *testing.T) {
	keyring.MockInit()

	var warn bytes.Buffer
	s := openCredentialSealer(&warn)
	if _, ok := s.(*credsqlite.KeyringSealer); !ok {
		t.Fatalf("got %T, want *credsqlite.KeyringSealer", s)
	}
	if warn.Len() != 0 {
		t.Errorf("expected no warning, got %q", warn.String())
	}
}

// When the keyring can't be reached (the Linux-without-D-Bus case),
// openCredentialSealer degrades to the non-encrypting PlaintextSealer
// and warns instead of failing.
func TestOpenCredentialSealer_KeyringUnavailableFallsBack(t *testing.T) {
	keyring.MockInitWithError(errors.New("dbus: couldn't determine address of session bus"))
	t.Cleanup(keyring.MockInit) // restore the working mock for other tests

	var warn bytes.Buffer
	s := openCredentialSealer(&warn)
	if _, ok := s.(credsqlite.PlaintextSealer); !ok {
		t.Fatalf("got %T, want credsqlite.PlaintextSealer", s)
	}
	if !strings.Contains(warn.String(), "UNENCRYPTED") {
		t.Errorf("warning should flag the unencrypted downgrade, got %q", warn.String())
	}
}
