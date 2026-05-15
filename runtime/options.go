package runtime

// ParticleOption configures a [Runtime.NewParticle] call.
//
// At present no options are exposed — the runtime derives every
// per-particle setting from the manifest. This type and its
// associated wiring is kept so callers and tests have a stable
// place to attach options as they're added.
type ParticleOption func(*particleConfig)

// particleConfig holds the resolved per-particle settings the
// runtime needs at instantiation time. Populated by applying every
// [ParticleOption] in order.
type particleConfig struct{}
