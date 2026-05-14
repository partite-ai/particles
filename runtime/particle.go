package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"testing/fstest"

	"github.com/google/jsonschema-go/jsonschema"
	wc "github.com/partite-ai/wacogo"
	"github.com/partite-ai/wacogo/host"
	"github.com/partite-ai/wacogo/wasi"
	"github.com/partite-ai/wacogo/wasi/filesystem/preopens"

	"github.com/partite-ai/particle/credentials"
)

// Canonical WIT export names the runtime publishes — see
// components/runtime/wit/runtime.wit.
const (
	toolsInterface  = "particle:runtime/tools@0.1.0"
	healthInterface = "particle:runtime/health@0.1.0"
)

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
// single tarball / build output. Hosts construct it via
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
// [build.Build] — typically a tarball unpacked into memory or
// `os.DirFS` over a directory) and brings up an instance.
//
// Failure modes:
//
//   - missing or invalid manifest.json
//   - missing bundle.js
//   - particle declares a capability the [Runtime] wasn't
//     configured with (the runtime requires every declared
//     capability to be backed by a real Manager, never a stub)
//   - wasm instantiation failure
func (r *Runtime) NewParticle(ctx context.Context, particleFS fs.FS, opts ...ParticleOption) (*Particle, error) {
	if particleFS == nil {
		return nil, errors.New("runtime: NewParticle: particleFS is required")
	}

	cfg := particleConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}

	manifest, err := readManifest(particleFS)
	if err != nil {
		return nil, fmt.Errorf("runtime: NewParticle: %w", err)
	}

	// Sockets are gated by manifest declaration; v1 doesn't yet
	// support socket-using particles (the wacogo wasi:sockets
	// impl is wide-open, so accepting the declaration without a
	// real policy would be a security regression). Reject at
	// instantiation time with a clear message so the limitation
	// shows up early.
	if manifest.declares("sockets") {
		return nil, fmt.Errorf("runtime: NewParticle: particle %q declares capabilities.sockets, which is not yet supported by this runtime", manifest.Name)
	}

	bundle, err := fs.ReadFile(particleFS, "bundle.js")
	if err != nil {
		return nil, fmt.Errorf("runtime: NewParticle: read bundle.js: %w", err)
	}

	// Build the in-memory FS the runtime sees through
	// wasi:filesystem. The runtime's JS does dynamic
	// `import("/particle/bundle.js")`, so the file lives at the
	// matching path.
	bundleFS := fstest.MapFS{
		"particle/bundle.js": &fstest.MapFile{Data: bundle},
	}

	httpDeclared, allowedHosts, err := manifest.httpAllowedHosts()
	if err != nil {
		return nil, fmt.Errorf("runtime: NewParticle: %w", err)
	}

	stderr := &bytes.Buffer{}
	w, err := wasi.NewWorld(ctx, r.cfg.Engine, &wasi.Config{
		Args:     []string{"particle-runtime"},
		Preopens: preopens.NewFSPreopens(bundleFS),
		Stdin:    strings.NewReader(""),
		Stdout:   io.Discard,
		Stderr:   stderr,
		// HTTP policy is per-particle: our httpPolicy
		// implements wacogo's wasi.HTTPDoer single-method
		// interface directly. It does two jobs on every
		// outbound request: (1) substitute credential
		// placeholders the JS bundle planted in headers /
		// query params with the real credential values, and
		// (2) reject the request if the URL host isn't in
		// the manifest's allowedHosts.
		//
		// Substitution is spec-driven: we walk the manifest's
		// declared credential names, look each up in the
		// Store, and check the apply-spec's expected location
		// in the request. The manifest is the source of
		// truth — a placeholder for an undeclared credential
		// never causes a Store read.
		HttpClient: newHTTPPolicy(
			httpDeclared, allowedHosts,
			http.DefaultClient,
			r.cfg.Credentials.Store(),
			manifest.Name,
			activeCredentialNames(cfg.selectedAuthenticationMethod),
			func(ctx context.Context, id string) (credentials.AccessToken, error) {
				return r.cfg.Credentials.RotateAccessToken(ctx, manifest.Name, id)
			},
		),
	})
	if err != nil {
		return nil, fmt.Errorf("runtime: build wasi world: %w", err)
	}

	cleanup := func() { _ = w.Close(ctx) }

	// Wire the four host capabilities the runtime imports. We
	// build instances for ALL four regardless of what the
	// manifest declares — the imports must be satisfied at
	// instantiation, period. The user-supplied managers are
	// the source of truth at runtime; the manifest filters
	// what's reachable from JS via build-time import checks.
	credInst, err := r.cfg.Credentials.NewCredentialsInstance(ctx, manifest.Name)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("runtime: build credentials host instance: %w", err)
	}
	oauthInst, err := r.cfg.Credentials.NewOAuthInstance(ctx, manifest.Name)
	if err != nil {
		_ = credInst.Close(ctx)
		cleanup()
		return nil, fmt.Errorf("runtime: build oauth host instance: %w", err)
	}
	signingInst, err := r.cfg.Credentials.NewSigningInstance(ctx, manifest.Name)
	if err != nil {
		_ = credInst.Close(ctx)
		_ = oauthInst.Close(ctx)
		cleanup()
		return nil, fmt.Errorf("runtime: build signing host instance: %w", err)
	}
	kvInst, err := r.cfg.KV.NewInstance(ctx, manifest.Name)
	if err != nil {
		_ = credInst.Close(ctx)
		_ = oauthInst.Close(ctx)
		_ = signingInst.Close(ctx)
		cleanup()
		return nil, fmt.Errorf("runtime: build kv host instance: %w", err)
	}

	imports := append(
		w.Imports(),
		wc.WithInstanceImport(loggingInterfaceName, r.logging.inst.(*host.ComponentInstance).Core()),
		wc.WithInstanceImport("particle:host/credentials@0.1.0", credInst.Core()),
		wc.WithInstanceImport("particle:host/oauth@0.1.0", oauthInst.Core()),
		wc.WithInstanceImport("particle:host/signing@0.1.0", signingInst.Core()),
		wc.WithInstanceImport("particle:host/kv@0.1.0", kvInst.Core()),
	)

	inst, err := r.comp.Instantiate(ctx, imports...)
	if err != nil {
		_ = credInst.Close(ctx)
		_ = oauthInst.Close(ctx)
		_ = signingInst.Close(ctx)
		_ = kvInst.Close(ctx)
		cleanup()
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
		manifest: manifest,
		inst:     inst,
		wasi:     w,
		stderr:   stderr,
	}, nil
}

// Manifest returns the particle's parsed manifest (read-only).
func (p *Particle) Manifest() Manifest { return p.manifest }

// Close releases the per-particle wasm instance and wasi world.
// Safe to call once.
func (p *Particle) Close(ctx context.Context) error {
	var first error
	if p.inst != nil {
		if err := p.inst.Close(ctx); err != nil {
			first = err
		}
		p.inst = nil
	}
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
// default export declares.
func (p *Particle) ListTools(ctx context.Context) ([]ToolDef, error) {
	iface := p.inst.ExportedInstance(toolsInterface)
	if iface == nil {
		return nil, fmt.Errorf("runtime: missing exported instance %q", toolsInterface)
	}
	fn := iface.ExportedFunc("list-tools")
	if fn == nil {
		return nil, fmt.Errorf("runtime: %s does not export list-tools", toolsInterface)
	}
	results, err := fn.Call(ctx)
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
func (p *Particle) CallTool(ctx context.Context, name string, argumentsJSON []byte) ([]byte, error) {
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
	results, err := fn.Call(ctx, wc.ValString(name), wc.ValString(string(argumentsJSON)))
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

// Ping invokes the particle's optional `ping` health-check. Returns
// *HealthError when the particle didn't declare ping or when the
// handler errored.
func (p *Particle) Ping(ctx context.Context) (*PingResult, error) {
	iface := p.inst.ExportedInstance(healthInterface)
	if iface == nil {
		return nil, fmt.Errorf("runtime: missing exported instance %q", healthInterface)
	}
	fn := iface.ExportedFunc("ping")
	if fn == nil {
		return nil, fmt.Errorf("runtime: %s does not export ping", healthInterface)
	}
	results, err := fn.Call(ctx)
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
