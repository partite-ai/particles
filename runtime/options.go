package runtime

// ParticleOption configures a [Runtime.NewParticle] call.
type ParticleOption func(*particleConfig)

// particleConfig holds the resolved per-particle settings the
// runtime needs at instantiation time. Populated by applying every
// [ParticleOption] in order.
type particleConfig struct {
	// selectedAuthenticationMethod is the credential method the user picked at
	// setup. The wasi:http policy substitutes only this method's
	// placeholder, even when the manifest declares multiple
	// alternatives. Empty means no auth method is active and no
	// substitution runs (the request still goes through the
	// allow-list check).
	selectedAuthenticationMethod string
}

// WithSelectedAuthenticationMethod tells the runtime which credential method
// the host has provisioned for this particle. The CLI reads it
// from [registry.Entry.SelectedAuthenticationMethod] and passes it through;
// other call sites (tests, future host integrations) supply it
// directly.
//
// Empty string is valid and means "no method active" — useful
// for particles whose manifest declares optional auth that the
// user didn't configure.
func WithSelectedAuthenticationMethod(method string) ParticleOption {
	return func(c *particleConfig) { c.selectedAuthenticationMethod = method }
}

// activeCredentialNames returns the slice the wasi:http policy
// uses to drive substitution. With a selected method, it's a
// single-element list containing that method's name; without
// one, it's nil and no substitution runs.
//
// Per the importer's "exactly one method" invariant, this list
// has at most one entry — the wasi:http policy iterates it like
// any other credential set, but in practice only one
// substitution location is ever checked.
func activeCredentialNames(selectedAuthenticationMethod string) []string {
	if selectedAuthenticationMethod == "" {
		return nil
	}
	return []string{selectedAuthenticationMethod}
}
