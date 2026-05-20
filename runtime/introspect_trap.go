package runtime

import (
	"errors"
	"net/http"
)

// ErrIntrospectModeHTTP is the sentinel returned by the trap HTTP
// doer when a particle attempts an outbound request during a
// get-manifest call. Companion to credentials.ErrIntrospectMode and
// kv.ErrIntrospectMode — the three together cover every host
// capability surface so module-scope host calls during introspect
// produce uniformly recognizable errors.
var ErrIntrospectModeHTTP = errors.New("http: host capabilities are not allowed during get-manifest")

// introspectTrapHTTPDoer satisfies [HTTPDoer]. The runtime wires it
// in when the internal introspect-mode flag is set —
// [Runtime.IntrospectParticle] is the only caller that sets that
// flag. Bypasses the normal httpPolicy entirely so callers don't
// see policy's generic "destination prohibited" message; they see
// the introspect-specific signal.
type introspectTrapHTTPDoer struct{}

func (introspectTrapHTTPDoer) Do(_ *http.Request) (*http.Response, error) {
	return nil, ErrIntrospectModeHTTP
}
