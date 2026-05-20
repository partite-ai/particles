package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync"
	"testing/fstest"

	"github.com/google/jsonschema-go/jsonschema"
	wc "github.com/partite-ai/wacogo"
	"github.com/partite-ai/wacogo/host"
	"github.com/partite-ai/wacogo/wasi"
	"github.com/partite-ai/wacogo/wasi/filesystem/preopens"

	"github.com/partite-ai/particles/credentials"
	"github.com/partite-ai/particles/internal/hostmeter"
	"github.com/partite-ai/particles/kv"
)

// Canonical WIT export names the runtime publishes — see
// components/js-runtime/wit/runtime.wit (and the matching
// components/python-runtime/wit/runtime.wit for the Python engine).
const (
	toolsInterface    = "particle:runtime/tools@0.1.0"
	healthInterface   = "particle:runtime/health@0.1.0"
	manifestInterface = "particle:runtime/manifest@0.1.0"
)

// runtimeBundleLayout returns (artifact-filename, mount-path) for
// the bundle file the JS / Python runtimes load at startup. Two
// pieces because the same bytes are read from the particle FS at
// the artifact name and mounted at the wasi:filesystem path the in-
// runtime loader looks under.
//
// For RuntimeWasm there is no bundle file — the component IS the
// runtime — so both return values are empty and the caller skips
// the bundle-required check.
func runtimeBundleLayout(rt RuntimeKind) (artifact, mountPath string, err error) {
	switch rt {
	case RuntimeJS:
		return "bundle.js", "particle/bundle.js", nil
	case RuntimePython:
		return "bundle.py", "particle/bundle.py", nil
	case RuntimeWasm:
		return "", "", nil
	default:
		return "", "", fmt.Errorf("unknown runtime %q", rt)
	}
}

// ToolDef is the metadata for one tool the particle exposes.
// Mirrors the WIT `tool-def` record.
type ToolDef struct {
	Name        string
	Description string
	// InputSchemaJSON is the JSON-Schema bytes the tool's
	// `inputSchema` was serialized to at build time. Object-
	// rooted Draft 2020-12.
	InputSchemaJSON []byte
}

// ToolError is a typed error returned by [Particle.CallTool]. The
// concrete underlying value is one of [ToolErrorNotFound],
// [ToolErrorInvalidArguments], [ToolErrorHandlerError],
// [ToolErrorCapabilityDenied]. Use a type switch to dispatch.
type ToolError struct {
	// Kind classifies the error. Mirrors the WIT variant.
	Kind ToolErrorKind
	// Message is the human-readable description. Empty for
	// kinds that don't carry a payload (NotFound).
	Message string
}

// ToolErrorKind is the variant case discriminator.
type ToolErrorKind int

const (
	ToolErrorKindNotFound ToolErrorKind = iota + 1
	ToolErrorKindInvalidArguments
	ToolErrorKindHandlerError
	ToolErrorKindCapabilityDenied
)

func (e *ToolError) Error() string {
	switch e.Kind {
	case ToolErrorKindNotFound:
		return "tool not found"
	case ToolErrorKindInvalidArguments:
		return "invalid arguments: " + e.Message
	case ToolErrorKindHandlerError:
		return "handler error: " + e.Message
	case ToolErrorKindCapabilityDenied:
		return "capability denied: " + e.Message
	}
	return fmt.Sprintf("tool error (kind=%d): %s", e.Kind, e.Message)
}

// PingStatus mirrors the WIT enum.
type PingStatus int

const (
	PingStatusOK PingStatus = iota + 1
	PingStatusDegraded
	PingStatusUnhealthy
)

// PingResult mirrors the WIT record.
type PingResult struct {
	Status  PingStatus
	Message string
	Details string
}

// HealthError is returned when a particle's ping handler errors or
// the particle didn't declare one.
type HealthError struct {
	NotImplemented bool
	Message        string
}

func (e *HealthError) Error() string {
	if e.NotImplemented {
		return "ping: not implemented"
	}
	return "ping: handler error: " + e.Message
}

// Particle is one running instance of a particle, scoped to a
// single archive / build output. Hosts construct it via
// [Runtime.NewParticle], call ListTools / CallTool / Ping against
// it, and Close when done.
//
// Goroutine-safety: the underlying QuickJS engine serializes calls
// per instance — concurrent CallTool invocations against the same
// Particle queue inside the runtime. Hosts that want concurrency
// should spin multiple Particles for the same particle artifact.
type Particle struct {
	manifest Manifest
	inst     *wc.ComponentInstance
	wasi     *wasi.World

	// hostInsts owns every per-particle WIT-host instance the
	// runtime stood up — logging, credentials, oauth, signing,
	// kv. Close walks them in build order's reverse so each
	// layer's resources release cleanly.
	hostInsts []*host.ComponentInstance

	// Captured wasi:cli/stderr — surfaced when a wasm trap
	// hides the actual diagnostic. Reset by readStderr.
	stderr *bytes.Buffer

	// schemas holds one compiled validator per tool, populated
	// lazily on first CallTool / first ListTools cache. Per
	// design doc §6, all input validation runs in Go before
	// entering wasm; this map is what enforces it.
	schemasOnce sync.Once
	schemas     map[string]*jsonschema.Resolved
	schemasErr  error
}

// NewParticle loads a particle's artifact (the fs.FS produced by
// [build.Build] — typically an archive unpacked into memory or
// os.DirFS over a directory) and brings up an instance.
//
// credStore and kvStore are the per-particle Store views the
// host capabilities read and write through; obtain them from a
// backend's Scoped helper (e.g. credsqlite.Backend.Scoped). Both
// are required — every particle imports the credentials and kv
// host shims unconditionally.
//
// Failure modes:
//
//   - missing or invalid manifest.json
//   - missing bundle.js
//   - particle declares a capability the [Runtime] wasn't
//     configured with (the runtime requires every declared
//     capability to be backed by a real Manager, never a stub)
//   - wasm instantiation failure
func (r *Runtime) NewParticle(ctx context.Context, particleFS fs.FS, credStore credentials.Store, kvStore kv.Store, opts ...ParticleOption) (*Particle, error) {
	if particleFS == nil {
		return nil, errors.New("runtime: NewParticle: particleFS is required")
	}
	if credStore == nil {
		return nil, errors.New("runtime: NewParticle: credStore is required")
	}
	if kvStore == nil {
		return nil, errors.New("runtime: NewParticle: kvStore is required")
	}
	return r.newParticleInternal(ctx, particleFS, credStore, kvStore, applyParticleOptions(opts))
}

// newParticleInternal is the un-validated, no-options-parsing core
// that NewParticle and IntrospectParticle both call into. Callers
// pass a fully-resolved [particleConfig] — the public NewParticle
// builds one from variadic options, IntrospectParticle constructs
// one directly with `introspectMode: true` so the trap-mode wiring
// is a private concern, not a public option that callers could
// pass alongside the wrong stores.
func (r *Runtime) newParticleInternal(ctx context.Context, particleFS fs.FS, credStore credentials.Store, kvStore kv.Store, cfg particleConfig) (*Particle, error) {
	manifest, err := LoadManifest(particleFS)
	if err != nil {
		return nil, fmt.Errorf("runtime: NewParticle: %w", err)
	}

	// Pick the wasm component to instantiate. JS / Python use the
	// shared preloaded images; Wasm particles ARE their component,
	// loaded fresh from particle.wasm in the artifact. All three
	// kinds export the same particle:runtime contract.
	selectedRuntime := manifest.ResolvedRuntime()
	comp, err := r.preloadedComponentFor(ctx, selectedRuntime)
	if err != nil {
		return nil, fmt.Errorf("runtime: NewParticle: %w", err)
	}
	if selectedRuntime == RuntimeWasm {
		comp, err = r.loadWasmComponent(ctx, particleFS)
		if err != nil {
			return nil, fmt.Errorf("runtime: NewParticle: %w", err)
		}
	}
	bundleFilename, _, err := runtimeBundleLayout(selectedRuntime)
	if err != nil {
		return nil, fmt.Errorf("runtime: NewParticle: %w", err)
	}

	// Build the in-memory FS the runtime sees through
	// wasi:filesystem. JS / Python read their bundle from
	// `/particle/bundle.{js,py}` (mounted from particleFS). Wasm
	// particles don't read any bundle from the FS — the component
	// is fully self-contained — but we still mount the artifact so
	// authors can ship per-particle data files alongside.
	//
	// For Python particles we mount the entire artifact FS (not
	// just bundle.py) so `_deps/site-packages` comes along for the
	// ride and `import httpx` finds the wheels resolved at build
	// time. For JS particles the FS only contains bundle.js + the
	// metadata files; the extra paths are harmless.
	bundleFS, err := mountParticleFS(particleFS, bundleFilename, selectedRuntime)
	if err != nil {
		return nil, fmt.Errorf("runtime: NewParticle: %w", err)
	}

	allowedHosts := manifest.Capabilities.HTTP.AllowedHosts

	listener := hostmeter.Listener{}

	// HTTP wiring branches on introspect mode:
	//   - Normal: per-particle httpPolicy that gates against
	//     allowedHosts and substitutes credential placeholders.
	//   - Introspect: trap doer that returns
	//     ErrIntrospectMode for every outbound — the synthesized
	//     get-manifest particle has no real HTTP policy to enforce,
	//     and we want module-scope fetch() to surface the explicit
	//     introspect error.
	var httpClient HTTPDoer
	if cfg.introspectMode {
		httpClient = introspectTrapHTTPDoer{}
	} else {
		httpClient = newHTTPPolicy(
			allowedHosts,
			cfg.httpClient,
			credStore,
			manifest.declaredCredentialNames(),
			credentialHostBindings(manifest),
			func(ctx context.Context, id string) (credentials.AccessToken, error) {
				return r.cfg.Credentials.RotateAccessToken(ctx, credStore, id)
			},
		)
	}

	stderr := &bytes.Buffer{}
	w, err := wasi.NewWorld(ctx, r.cfg.Engine, &wasi.Config{
		Args:         []string{"particle-runtime"},
		Preopens:     preopens.NewFSPreopens(preopens.ImmutableFS{FS: bundleFS}),
		Stdin:        strings.NewReader(""),
		Stdout:       io.Discard,
		Stderr:       stderr,
		CallListener: listener,
		HttpClient:   httpClient,
	})
	if err != nil {
		return nil, fmt.Errorf("runtime: build wasi world: %w", err)
	}

	// Track every per-particle host instance so Close can
	// release them. The slice doubles as the cleanup-on-error
	// path: failures partway through wire-up close everything
	// that was built so far.
	var hostInsts []*host.ComponentInstance
	closeAll := func() {
		for i := len(hostInsts) - 1; i >= 0; i-- {
			_ = hostInsts[i].Close(ctx)
		}
		_ = w.Close(ctx)
	}

	// wasi:logging is a JS-runtime-only import: the QuickJS engine
	// routes console.* through it. The Python runtime writes
	// directly to wasi:cli/stderr (captured by the per-instance
	// stderr buffer just below the wasi.Config above), so it doesn't
	// declare wasi:logging in its world and doesn't need an instance
	// wired up here. Going engine-agnostic on the rest keeps the
	// host wire-up flat — no big switch on selectedRuntime.
	var loggingInst *host.ComponentInstance
	if selectedRuntime == RuntimeJS {
		loggingInst, err = newLoggingHost(ctx, r.cfg.Engine, cfg.log, host.WithCallListener(listener))
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("runtime: build wasi:logging host: %w", err)
		}
		hostInsts = append(hostInsts, loggingInst)
	}

	// Wire the four particle:host capabilities. Every import
	// must be satisfied at instantiation (the runtime.wasm
	// imports them unconditionally); when the caller opted out
	// of a Store, we build the host against a do-nothing scope.
	credInst, err := r.cfg.Credentials.NewCredentialsInstance(ctx, credStore, host.WithCallListener(listener))
	if err != nil {
		closeAll()
		return nil, fmt.Errorf("runtime: build credentials host instance: %w", err)
	}
	hostInsts = append(hostInsts, credInst)

	oauthInst, err := r.cfg.Credentials.NewOAuthInstance(ctx, credStore, host.WithCallListener(listener))
	if err != nil {
		closeAll()
		return nil, fmt.Errorf("runtime: build oauth host instance: %w", err)
	}
	hostInsts = append(hostInsts, oauthInst)

	signingInst, err := r.cfg.Credentials.NewSigningInstance(ctx, credStore, host.WithCallListener(listener))
	if err != nil {
		closeAll()
		return nil, fmt.Errorf("runtime: build signing host instance: %w", err)
	}
	hostInsts = append(hostInsts, signingInst)

	kvInst, err := r.cfg.KV.NewInstance(ctx, kvStore, host.WithCallListener(listener))
	if err != nil {
		closeAll()
		return nil, fmt.Errorf("runtime: build kv host instance: %w", err)
	}
	hostInsts = append(hostInsts, kvInst)

	imports := append(
		w.Imports(),
		wc.WithInstanceImport("particle:host/credentials@0.1.0", credInst.Core()),
		wc.WithInstanceImport("particle:host/oauth@0.1.0", oauthInst.Core()),
		wc.WithInstanceImport("particle:host/signing@0.1.0", signingInst.Core()),
		wc.WithInstanceImport("particle:host/kv@0.1.0", kvInst.Core()),
	)
	if loggingInst != nil {
		imports = append(imports, wc.WithInstanceImport(loggingInterfaceName, loggingInst.Core()))
	}

	inst, err := comp.Instantiate(ctx, imports...)
	if err != nil {
		closeAll()
		return nil, fmt.Errorf("runtime: instantiate: %w\nstderr:\n%s", err, stderrSinkBytes(stderr))
	}

	// Note: we deliberately do NOT call `wizer-initialize` here.
	// The component is pre-initialized by `wasm-rquickjs optimize`
	// at build time (Wizer baked the QuickJS startup state into
	// the artifact), and the wrapper crate's get_js_state
	// transitions WizerPreInitialized → Initialized lazily on the
	// first export call. Calling wizer-initialize again post-snapshot
	// is unsafe.

	return &Particle{
		manifest:  manifest,
		inst:      inst,
		wasi:      w,
		hostInsts: hostInsts,
		stderr:    stderr,
	}, nil
}

// Manifest returns the particle's parsed manifest (read-only).
func (p *Particle) Manifest() Manifest { return p.manifest }

// Close releases the per-particle wasm instance, the per-particle
// host instances (logging, credentials, oauth, signing, kv), and
// the wasi world. Safe to call once.
func (p *Particle) Close(ctx context.Context) error {
	var first error
	if p.inst != nil {
		if err := p.inst.Close(ctx); err != nil {
			first = err
		}
		p.inst = nil
	}
	// Host instances close in reverse build order so each
	// layer's resources release cleanly.
	for i := len(p.hostInsts) - 1; i >= 0; i-- {
		if err := p.hostInsts[i].Close(ctx); err != nil && first == nil {
			first = err
		}
	}
	p.hostInsts = nil
	if p.wasi != nil {
		if err := p.wasi.Close(ctx); err != nil && first == nil {
			first = err
		}
		p.wasi = nil
	}
	return first
}

// -----------------------------------------------------------------------------
// ListTools / CallTool / Ping
// -----------------------------------------------------------------------------

// ensureSchemas lazily compiles each tool's input schema on first
// CallTool. Compilation runs through ListTools, so it pays the
// cost of one round-trip into wasm — but only once per Particle.
// Subsequent calls reuse the cached map.
//
// A failure here is sticky (cached in schemasErr) — if the wasm
// can't even produce the tool list, every CallTool will surface
// the same error rather than retrying.
func (p *Particle) ensureSchemas(ctx context.Context) (map[string]*jsonschema.Resolved, error) {
	p.schemasOnce.Do(func() {
		tools, err := p.ListTools(ctx)
		if err != nil {
			p.schemasErr = fmt.Errorf("list tools: %w", err)
			return
		}
		m := make(map[string]*jsonschema.Resolved, len(tools))
		for _, td := range tools {
			s, err := compileToolSchema(td.InputSchemaJSON)
			if err != nil {
				p.schemasErr = fmt.Errorf("tool %q: %w", td.Name, err)
				return
			}
			if s != nil {
				m[td.Name] = s
			}
		}
		p.schemas = m
	})
	return p.schemas, p.schemasErr
}

// ListTools returns the metadata for every tool the particle's
// default export declares. Enters wasm to call the runtime's
// list-tools export — the live JS is the source of truth for
// the schema, so a stale manifest.json never silently overrides
// what the bundle actually exposes.
func (p *Particle) ListTools(ctx context.Context, opts ...CallOption) ([]ToolDef, error) {
	iface := p.inst.ExportedInstance(toolsInterface)
	if iface == nil {
		return nil, fmt.Errorf("runtime: missing exported instance %q", toolsInterface)
	}
	fn := iface.ExportedFunc("list-tools")
	if fn == nil {
		return nil, fmt.Errorf("runtime: %s does not export list-tools", toolsInterface)
	}
	ctx, lim, stop := armLimit(ctx, opts)
	defer stop()
	results, err := fn.Call(ctx)
	if lim != nil && lim.Tripped() {
		return nil, &BudgetExceededError{Op: "list-tools", Budget: lim.budget, Used: lim.Used()}
	}
	if err != nil {
		return nil, p.wrapTrap(err, "list-tools")
	}
	if len(results) != 1 {
		return nil, fmt.Errorf("list-tools returned %d results, want 1", len(results))
	}
	list, ok := results[0].(*wc.ValList)
	if !ok {
		return nil, fmt.Errorf("list-tools returned %T, want *wacogo.ValList", results[0])
	}
	out := make([]ToolDef, 0, list.Len())
	for i := 0; i < list.Len(); i++ {
		rec, ok := list.Get(i).(*wc.ValRecord)
		if !ok {
			return nil, fmt.Errorf("list-tools[%d] is %T, want *wacogo.ValRecord", i, list.Get(i))
		}
		td := ToolDef{}
		if v, _ := rec.Field("name").(wc.ValString); v != "" {
			td.Name = string(v)
		}
		if v, _ := rec.Field("description").(wc.ValString); v != "" {
			td.Description = string(v)
		}
		if v, _ := rec.Field("input-schema-json").(wc.ValString); v != "" {
			td.InputSchemaJSON = []byte(v)
		}
		out = append(out, td)
	}
	return out, nil
}

// CallTool invokes the named tool with `argumentsJSON`. The
// arguments are validated host-side against the tool's input
// schema (design doc §6 "Argument validation: host-side only")
// before the call enters wasm; if validation fails, the returned
// error is *ToolError with Kind == ToolErrorKindInvalidArguments.
// Returns the tool's JSON-encoded result on success.
func (p *Particle) CallTool(ctx context.Context, name string, argumentsJSON []byte, opts ...CallOption) ([]byte, error) {
	schemas, err := p.ensureSchemas(ctx)
	if err != nil {
		return nil, fmt.Errorf("runtime: prepare tool schemas: %w", err)
	}
	if s, ok := schemas[name]; ok {
		if vErr := validateToolInput(s, argumentsJSON); vErr != nil {
			return nil, &ToolError{
				Kind:    ToolErrorKindInvalidArguments,
				Message: vErr.Error(),
			}
		}
	}
	// Tools not in the schemas map (e.g., a name the JS bundle
	// doesn't recognize) fall through to wasm so the runtime's
	// own NotFound error path produces the canonical message.

	iface := p.inst.ExportedInstance(toolsInterface)
	if iface == nil {
		return nil, fmt.Errorf("runtime: missing exported instance %q", toolsInterface)
	}
	fn := iface.ExportedFunc("call-tool")
	if fn == nil {
		return nil, fmt.Errorf("runtime: %s does not export call-tool", toolsInterface)
	}
	ctx, lim, stop := armLimit(ctx, opts)
	defer stop()
	results, err := fn.Call(ctx, wc.ValString(name), wc.ValString(string(argumentsJSON)))
	if lim != nil && lim.Tripped() {
		return nil, &BudgetExceededError{Op: "call-tool", Budget: lim.budget, Used: lim.Used()}
	}
	if err != nil {
		return nil, p.wrapTrap(err, "call-tool")
	}
	if len(results) != 1 {
		return nil, fmt.Errorf("call-tool returned %d results, want 1", len(results))
	}
	res, ok := results[0].(*wc.ValResult)
	if !ok {
		return nil, fmt.Errorf("call-tool returned %T, want *wacogo.ValResult", results[0])
	}
	if res.IsOk() {
		s, ok := res.Ok().(wc.ValString)
		if !ok {
			return nil, fmt.Errorf("call-tool ok payload is %T, want ValString", res.Ok())
		}
		return []byte(s), nil
	}
	return nil, decodeToolError(res.Err())
}

// ManifestError is the typed error variant returned by
// [Particle.GetManifest]. Kind discriminates between
// `bundle-load-error` (the runtime couldn't load the user's
// handler module) and `invalid-manifest` (the bundle loaded but
// its declared shape didn't match the contract).
type ManifestError struct {
	Kind    ManifestErrorKind
	Message string
}

// ManifestErrorKind names the variant case.
type ManifestErrorKind int

const (
	ManifestErrorKindBundleLoadError ManifestErrorKind = iota + 1
	ManifestErrorKindInvalidManifest
)

func (e *ManifestError) Error() string {
	// Hyphenated to match the WIT variant case names — keeps the
	// error substring stable for callers grep-ing for
	// "bundle-load-error" / "invalid-manifest".
	switch e.Kind {
	case ManifestErrorKindBundleLoadError:
		return "bundle-load-error: " + e.Message
	case ManifestErrorKindInvalidManifest:
		return "invalid-manifest: " + e.Message
	}
	return fmt.Sprintf("manifest error (kind=%d): %s", e.Kind, e.Message)
}

// GetManifest invokes the particle's `manifest.get-manifest` export
// and returns the typed manifest record. Every particle (current or
// future fully-WASM) describes itself through this export — a
// uniform contract the host queries at build time and at setup time.
//
// For build-time use, prefer [Runtime.IntrospectParticle] (which
// owns the trap-store + introspect-mode wiring). GetManifest on its
// own is the lower-level call that assumes the particle was already
// instantiated under the right configuration.
func (p *Particle) GetManifest(ctx context.Context, opts ...CallOption) (*Manifest, error) {
	iface := p.inst.ExportedInstance(manifestInterface)
	if iface == nil {
		return nil, fmt.Errorf("runtime: missing exported instance %q", manifestInterface)
	}
	fn := iface.ExportedFunc("get-manifest")
	if fn == nil {
		return nil, fmt.Errorf("runtime: %s does not export get-manifest", manifestInterface)
	}
	ctx, lim, stop := armLimit(ctx, opts)
	defer stop()
	results, err := fn.Call(ctx)
	if lim != nil && lim.Tripped() {
		return nil, &BudgetExceededError{Op: "get-manifest", Budget: lim.budget, Used: lim.Used()}
	}
	if err != nil {
		return nil, p.wrapTrap(err, "get-manifest")
	}
	if len(results) != 1 {
		return nil, fmt.Errorf("get-manifest returned %d results, want 1", len(results))
	}
	res, ok := results[0].(*wc.ValResult)
	if !ok {
		return nil, fmt.Errorf("get-manifest returned %T, want *wacogo.ValResult", results[0])
	}
	if res.IsOk() {
		rec, ok := res.Ok().(*wc.ValRecord)
		if !ok {
			return nil, fmt.Errorf("get-manifest ok payload is %T, want *wacogo.ValRecord", res.Ok())
		}
		return decodeManifestRecord(rec)
	}
	return nil, decodeManifestError(res.Err())
}

// decodeManifestError lifts the WIT `variant manifest-error` into
// *ManifestError.
func decodeManifestError(v wc.Val) *ManifestError {
	variant, ok := v.(*wc.ValVariant)
	if !ok {
		return &ManifestError{
			Kind:    ManifestErrorKindInvalidManifest,
			Message: fmt.Sprintf("non-variant error payload %T", v),
		}
	}
	me := &ManifestError{}
	switch variant.Discriminant() {
	case 0:
		me.Kind = ManifestErrorKindBundleLoadError
		if s, ok := variant.Val().(wc.ValString); ok {
			me.Message = string(s)
		}
	case 1:
		me.Kind = ManifestErrorKindInvalidManifest
		if s, ok := variant.Val().(wc.ValString); ok {
			me.Message = string(s)
		}
	default:
		me.Kind = ManifestErrorKindInvalidManifest
		me.Message = fmt.Sprintf("unknown manifest-error discriminant %d", variant.Discriminant())
	}
	return me
}

// Ping invokes the particle's optional `ping` health-check. Returns
// *HealthError when the particle didn't declare ping or when the
// handler errored.
func (p *Particle) Ping(ctx context.Context, opts ...CallOption) (*PingResult, error) {
	iface := p.inst.ExportedInstance(healthInterface)
	if iface == nil {
		return nil, fmt.Errorf("runtime: missing exported instance %q", healthInterface)
	}
	fn := iface.ExportedFunc("ping")
	if fn == nil {
		return nil, fmt.Errorf("runtime: %s does not export ping", healthInterface)
	}
	ctx, lim, stop := armLimit(ctx, opts)
	defer stop()
	results, err := fn.Call(ctx)
	if lim != nil && lim.Tripped() {
		return nil, &BudgetExceededError{Op: "ping", Budget: lim.budget, Used: lim.Used()}
	}
	if err != nil {
		return nil, p.wrapTrap(err, "ping")
	}
	if len(results) != 1 {
		return nil, fmt.Errorf("ping returned %d results, want 1", len(results))
	}
	res, ok := results[0].(*wc.ValResult)
	if !ok {
		return nil, fmt.Errorf("ping returned %T, want *wacogo.ValResult", results[0])
	}
	if res.IsOk() {
		rec, ok := res.Ok().(*wc.ValRecord)
		if !ok {
			return nil, fmt.Errorf("ping ok payload is %T, want *wacogo.ValRecord", res.Ok())
		}
		return decodePingResult(rec), nil
	}
	return nil, decodeHealthError(res.Err())
}

// -----------------------------------------------------------------------------
// Decoders (variant / record → Go-idiomatic types)
// -----------------------------------------------------------------------------

// decodeToolError lifts the WIT `variant tool-error` into *ToolError.
func decodeToolError(v wc.Val) *ToolError {
	variant, ok := v.(*wc.ValVariant)
	if !ok {
		return &ToolError{Kind: ToolErrorKindHandlerError, Message: fmt.Sprintf("non-variant error payload %T", v)}
	}
	te := &ToolError{}
	switch variant.Discriminant() {
	case 0:
		te.Kind = ToolErrorKindNotFound
	case 1:
		te.Kind = ToolErrorKindInvalidArguments
		if s, ok := variant.Val().(wc.ValString); ok {
			te.Message = string(s)
		}
	case 2:
		te.Kind = ToolErrorKindHandlerError
		if s, ok := variant.Val().(wc.ValString); ok {
			te.Message = string(s)
		}
	case 3:
		te.Kind = ToolErrorKindCapabilityDenied
		if s, ok := variant.Val().(wc.ValString); ok {
			te.Message = string(s)
		}
	default:
		te.Kind = ToolErrorKindHandlerError
		te.Message = fmt.Sprintf("unknown tool-error discriminant %d", variant.Discriminant())
	}
	return te
}

func decodeHealthError(v wc.Val) *HealthError {
	variant, ok := v.(*wc.ValVariant)
	if !ok {
		return &HealthError{Message: fmt.Sprintf("non-variant error payload %T", v)}
	}
	he := &HealthError{}
	switch variant.Discriminant() {
	case 0:
		he.NotImplemented = true
	case 1:
		if s, ok := variant.Val().(wc.ValString); ok {
			he.Message = string(s)
		}
	default:
		he.Message = fmt.Sprintf("unknown health-error discriminant %d", variant.Discriminant())
	}
	return he
}

func decodePingResult(rec *wc.ValRecord) *PingResult {
	pr := &PingResult{}
	if v, ok := rec.Field("status").(*wc.ValEnum); ok {
		switch v.Discriminant() {
		case 0:
			pr.Status = PingStatusOK
		case 1:
			pr.Status = PingStatusDegraded
		case 2:
			pr.Status = PingStatusUnhealthy
		}
	}
	if v, ok := rec.Field("message").(*wc.ValOption); ok && !v.IsNone() {
		if s, ok := v.Val().(wc.ValString); ok {
			pr.Message = string(s)
		}
	}
	if v, ok := rec.Field("details").(*wc.ValOption); ok && !v.IsNone() {
		if s, ok := v.Val().(wc.ValString); ok {
			pr.Details = string(s)
		}
	}
	return pr
}

// wrapTrap surfaces the captured stderr alongside a wasm trap so
// the caller sees the actual JS-level diagnostic instead of just
// "wasm error: unreachable".
func (p *Particle) wrapTrap(err error, op string) error {
	msg := stderrSinkBytes(p.stderr)
	if msg == "" {
		return fmt.Errorf("runtime: %s: %w", op, err)
	}
	return fmt.Errorf("runtime: %s: %w\nstderr:\n%s", op, err, msg)
}

// mountParticleFS walks the particle artifact FS and re-rooots every
// file under `particle/`. For JS / Python particles, requires that
// `requiredBundle` (bundle.js / bundle.py) exists at the root —
// that's the entry point the runtime's bootstrap loads. For Wasm
// particles requiredBundle is empty and the check is skipped.
//
// Python particles ship a `_deps/site-packages/` tree alongside
// bundle.py; mounting the whole artifact means `import httpx` etc.
// just works at runtime via the bootstrap's sys.path setup. JS
// particles ship only bundle.js + a couple metadata files; the extra
// mounted paths are harmless (the JS runtime doesn't read them).
// Wasm particles may ship arbitrary data files alongside
// particle.wasm.
func mountParticleFS(particleFS fs.FS, requiredBundle string, rt RuntimeKind) (fs.FS, error) {
	if requiredBundle != "" {
		if _, err := fs.Stat(particleFS, requiredBundle); err != nil {
			return nil, fmt.Errorf("read %s: %w", requiredBundle, err)
		}
	}
	out := fstest.MapFS{}
	err := fs.WalkDir(particleFS, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		// manifest.json / build-info.json / Particle.lock don't
		// need to be in the runtime's view (the host already read
		// the manifest before instantiating; the others are
		// build-pipeline bookkeeping). For wasm particles also
		// filter particle.wasm itself — the component is already
		// loaded as the runtime image and re-mounting it would be
		// pointless. bundle.{js,py} and the _deps tree get through.
		switch path {
		case "manifest.json", "build-info.json", "bundle.js.map", "Particle.lock":
			return nil
		}
		if rt == RuntimeWasm && path == "particle.wasm" {
			return nil
		}
		data, err := fs.ReadFile(particleFS, path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		out["particle/"+path] = &fstest.MapFile{Data: data}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
