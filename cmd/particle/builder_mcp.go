package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"github.com/tetratelabs/wazero"

	"github.com/partite-ai/particles/credentials"
	credsqlite "github.com/partite-ai/particles/credentials/sqlite"
	"github.com/partite-ai/particles/examples"
	"github.com/partite-ai/particles/internal/build"
	"github.com/partite-ai/particles/internal/memfs"
	"github.com/partite-ai/particles/mounts"
	mountsqlite "github.com/partite-ai/particles/mounts/sqlite"
	regsqlite "github.com/partite-ai/particles/registry/sqlite"
	"github.com/partite-ai/particles/skills"
)

// Names the builder-mcp server registers under. Snake-case so they
// read naturally as identifiers an LLM picks from a list. The skill
// resource is registered as BOTH a prompt and a tool — the prompt
// is the canonical user-driven session-bootstrap path; the tool
// covers clients that don't surface prompts (the MCP spec has no
// client-capability flag for prompt rendering, so we always offer
// both — see the long form in [builderMCPBuildToolDescription]).
const (
	builderMCPBuildToolName   = "build_particle"
	builderMCPSkillToolName   = "particle_skill"
	builderMCPExampleToolName = "particle_example"
	builderMCPSkillPromptName = "particle_skill"
)

func newBuilderMCPCmd() *cobra.Command {
	var dbPath string
	cmd := &cobra.Command{
		Use:   "builder-mcp",
		Short: "Run a stdio MCP server that exposes particle-building affordances to an LLM client",
		Long: `Run a stdio MCP server that lets an LLM client build particles.

Exposes:
  • tool ` + "`" + builderMCPBuildToolName + "`" + `   — bundle source and register the particle
  • tool ` + "`" + builderMCPSkillToolName + "`" + `  — fetch the authoring guide for a language
  • tool ` + "`" + builderMCPExampleToolName + "`" + ` — fetch one example Particlefile by name
  • prompt ` + "`" + builderMCPSkillPromptName + "`" + ` — same content as the skill tool, surfaced as a
    user-driven prompt the operator picks at session start (preferred where
    the client renders prompts; the tool is the fallback)

Capabilities are auto-accepted (no permission prompt). Unconfigured
credentials are gathered via MCP elicitation when the client supports
it; otherwise the response asks the caller to run ` + "`particle reconfigure`" + `.
Mount mappings are taken from the tool input — required mounts left
unmapped are reported in the response, not enforced.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBuilderMCP(cmd, dbPath)
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "", dbFlagUsage())
	return cmd
}

func runBuilderMCP(cmd *cobra.Command, dbPath string) error {
	dbPath, err := resolveDBPath(dbPath)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	db, err := openStateDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	cache, cacheErr := loadWasmCompilationCache(ctx)
	if cacheErr != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "warning:", cacheErr)
	}
	if cache != nil {
		defer cache.Close(ctx)
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "particle-builder-mcp",
		Version: "0",
	}, nil)

	server.AddTool(&mcp.Tool{
		Name:        builderMCPBuildToolName,
		Description: builderMCPBuildToolDescription,
		InputSchema: json.RawMessage(builderMCPBuildInputSchema),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return handleBuilderMCPBuildTool(ctx, db, cache, cmd.ErrOrStderr(), req)
	})

	server.AddTool(&mcp.Tool{
		Name:        builderMCPSkillToolName,
		Description: builderMCPSkillToolDescription,
		InputSchema: json.RawMessage(builderMCPSkillInputSchema),
	}, handleBuilderMCPSkillTool)

	server.AddTool(&mcp.Tool{
		Name:        builderMCPExampleToolName,
		Description: builderMCPExampleToolDescription,
		InputSchema: json.RawMessage(builderMCPExampleInputSchema),
	}, handleBuilderMCPExampleTool)

	server.AddPrompt(&mcp.Prompt{
		Name:        builderMCPSkillPromptName,
		Description: builderMCPSkillPromptDescription,
		Arguments: []*mcp.PromptArgument{{
			Name:        "language",
			Description: "Source language: \"typescript\" or \"python\".",
			Required:    true,
		}},
	}, handleBuilderMCPSkillPrompt)

	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return nil
}

// builderMCPBuildToolDescription is loaded into the client's tool
// list on every connect, so it stays compact and action-focused:
// what a particle is (in author terms, not implementation terms),
// how to prepare, the input/output shape, and the status values
// the caller has to act on. Deep authoring reference lives in the
// `particle_skill` prompt / tool — pointed at, not duplicated.
const builderMCPBuildToolDescription = `Build a particle and register it so it can be invoked.

A particle is a single-file program (Particlefile.ts, .js, or .py) that
exports one or more named tools and declares the network hosts, filesystem
mounts, and credentials it needs. Particles run sandboxed — they can only
reach what they declare.

Before writing a Particlefile, load the authoring guide:
  • Preferred: ask the user to invoke the ` + "`" + builderMCPSkillPromptName + "`" + ` prompt.
  • Fallback: call the ` + "`" + builderMCPSkillToolName + "`" + ` tool with a language.
Use ` + "`" + builderMCPExampleToolName + "`" + ` to fetch a working reference Particlefile by name.

Input — exactly one of:
  sources — { path: content }, virtual filesystem for in-memory authoring.
            Paths are relative, forward-slash.
  dir     — absolute host path to a directory containing the source.
Optional:
  mounts  — { declared-mount-name: absolute-host-path }. Each path must exist
            and be a directory; unknown names or bad paths fail the call.

Output (JSON; IsError on failure):
  name, version  — the registered identity.
  capabilities   — the network and filesystem permissions the particle was
                   granted. Show this to the user; it's their audit trail.
  credentials[]  — per-credential status:
       already_configured — done.
       configured_now     — just gathered via elicitation.
       needs_setup        — unconfigured; the entry's ` + "`instruction`" + ` field has
                            the exact ` + "`particle reconfigure …`" + ` command the user
                            must run before the particle can run.
  mounts[]       — per-declared-mount status:
       already_mapped — done.
       mapped_now     — mapped from the ` + "`mounts`" + ` input.
       unmapped       — no host path. Required-and-unmapped entries carry an
                        ` + "`instruction`" + ` with the ` + "`particle mount …`" + ` command.
  warnings       — non-fatal diagnostics.

After a successful build, surface to the user: the name@version, any
needs_setup credential instructions, any required unmapped mount
instructions, and any warnings. The particle can then be invoked via
` + "`particle run <name>`" + ` or exposed over MCP via ` + "`particle serve-mcp <name>`" + `.`

const builderMCPBuildInputSchema = `{
  "type": "object",
  "properties": {
    "sources": {
      "type": "object",
      "description": "Virtual filesystem: map of file path (relative, forward-slash) to text content. Mutually exclusive with dir.",
      "additionalProperties": {"type": "string"}
    },
    "dir": {
      "type": "string",
      "description": "Absolute host path to a directory containing the particle source. Mutually exclusive with sources."
    },
    "mounts": {
      "type": "object",
      "description": "Map declared mount name to an absolute host directory. Each path must exist and be a directory; unknown mount names fail the call.",
      "additionalProperties": {"type": "string"}
    }
  }
}`

type builderMCPArgs struct {
	Sources map[string]string `json:"sources"`
	Dir     string            `json:"dir"`
	Mounts  map[string]string `json:"mounts"`
}

// handleBuilderMCPBuildTool runs one build_particle call: validates inputs,
// builds the particle, walks credentials (with elicitation where
// available), applies mount mappings, registers the result, and
// returns a structured JSON response. Errors that come from user-
// provided arguments are surfaced as IsError tool results so the LLM
// can self-correct; only protocol-level failures bubble up as Go
// errors.
func handleBuilderMCPBuildTool(ctx context.Context, db *sql.DB, cache wazero.CompilationCache, stderr interface{ Write(p []byte) (int, error) }, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args builderMCPArgs
	if raw := []byte(req.Params.Arguments); len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return mcpToolError(fmt.Sprintf("parse arguments: %v", err)), nil
		}
	}

	if (len(args.Sources) > 0) == (args.Dir != "") {
		return mcpToolError("exactly one of `sources` or `dir` must be provided"), nil
	}

	sourceFS, srcErr := openBuilderMCPSource(args)
	if srcErr != nil {
		return mcpToolError(srcErr.Error()), nil
	}

	// Validate mount host paths up front — bad paths fail the call
	// before any registry / credential writes, keeping the install
	// atomic.
	resolvedMounts, err := validateBuilderMCPMounts(args.Mounts)
	if err != nil {
		return mcpToolError(err.Error()), nil
	}

	res, err := build.Build(ctx, build.Options{
		Source:           sourceFS,
		CompilationCache: cache,
	})
	if err != nil {
		return mcpToolError(formatBuildError(err)), nil
	}

	manifest, err := readBuilderMCPManifest(res.Particle)
	if err != nil {
		return mcpToolError(err.Error()), nil
	}

	// `mounts` arg keys must reference declared mounts. We can only
	// check this after build (which produces the manifest); doing
	// it here keeps the "atomic install" property: nothing has been
	// written yet, so we bail clean.
	fc := manifest.filesystemCap()
	for name := range resolvedMounts {
		if _, ok := fc.Mounts[name]; !ok {
			return mcpToolError(fmt.Sprintf("mount %q is not declared in the particle's capabilities.filesystem.mounts", name)), nil
		}
	}

	reg, err := regsqlite.New(ctx, db)
	if err != nil {
		return mcpToolError(fmt.Sprintf("open registry: %v", err)), nil
	}
	mountBackend, err := mountsqlite.New(ctx, db)
	if err != nil {
		return mcpToolError(fmt.Sprintf("open mounts store: %v", err)), nil
	}
	mountStore := mountBackend.Scoped(manifest.Name)

	var credStore credentials.Store
	if len(manifest.Credentials) > 0 {
		backend, bErr := credsqlite.New(ctx, db, openCredentialSealer(stderr))
		if bErr != nil {
			return mcpToolError(fmt.Sprintf("open credentials store: %v", bErr)), nil
		}
		credStore = backend.Scoped(manifest.Name)
	}

	credsOut, err := processBuilderMCPCredentials(ctx, req.Session, credStore, manifest.Credentials)
	if err != nil {
		return mcpToolError(fmt.Sprintf("credentials: %v", err)), nil
	}

	mountsOut, err := applyBuilderMCPMounts(ctx, mountStore, fc.Mounts, resolvedMounts)
	if err != nil {
		return mcpToolError(fmt.Sprintf("mounts: %v", err)), nil
	}

	if err := reg.Put(ctx, manifest.Name, manifest.Version, res.Particle); err != nil {
		return mcpToolError(fmt.Sprintf("register %s@%s: %v", manifest.Name, manifest.Version, err)), nil
	}

	out := map[string]any{
		"name":         manifest.Name,
		"version":      manifest.Version,
		"capabilities": manifest.Capabilities,
		"credentials":  credsOut,
		"mounts":       mountsOut,
		"warnings":     diagnosticStrings(res.Warnings),
	}
	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return mcpToolError(fmt.Sprintf("encode response: %v", err)), nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
	}, nil
}

// openBuilderMCPSource turns the tool args into the fs.FS the build
// pipeline consumes — either an on-disk directory or an in-memory
// memfs.
func openBuilderMCPSource(args builderMCPArgs) (fs.FS, error) {
	if args.Dir != "" {
		abs, err := filepath.Abs(args.Dir)
		if err != nil {
			return nil, fmt.Errorf("resolve dir: %w", err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("stat dir: %w", err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%s is not a directory", abs)
		}
		return os.DirFS(abs), nil
	}
	out := make(memfs.FS, len(args.Sources))
	for path, content := range args.Sources {
		if !fs.ValidPath(path) {
			return nil, fmt.Errorf("source path %q is not a valid relative fs path", path)
		}
		out[path] = &memfs.File{Data: []byte(content), Mode: 0o644}
	}
	return out, nil
}

// validateBuilderMCPMounts resolves every supplied host path to an
// absolute, existing directory. Returns the resolved map. Any
// failure aborts the whole call so we never persist partial mount
// state for a fresh install.
func validateBuilderMCPMounts(raw map[string]string) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(raw))
	for name, p := range raw {
		if p == "" {
			return nil, fmt.Errorf("mount %q: empty host path", name)
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, fmt.Errorf("mount %q: resolve %q: %w", name, p, err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("mount %q: %v", name, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("mount %q: %s is not a directory", name, abs)
		}
		out[name] = abs
	}
	return out, nil
}

// builderMCPMountDecl mirrors the importer's mountDecl shape — we
// re-parse here rather than import the unexported type.
type builderMCPMountDecl struct {
	Description string `json:"description"`
	Path        string `json:"path"`
	Access      string `json:"access"`
	Required    bool   `json:"required"`
}

type builderMCPFSCap struct {
	Mounts map[string]builderMCPMountDecl `json:"mounts"`
}

type builderMCPManifest struct {
	Name         string                     `json:"name"`
	Version      string                     `json:"version"`
	Capabilities map[string]json.RawMessage `json:"capabilities"`
	Credentials  map[string]json.RawMessage `json:"credentials"`
}

func (m builderMCPManifest) filesystemCap() builderMCPFSCap {
	raw, ok := m.Capabilities["filesystem"]
	if !ok {
		return builderMCPFSCap{}
	}
	var fc builderMCPFSCap
	_ = json.Unmarshal(raw, &fc)
	return fc
}

func readBuilderMCPManifest(fsys fs.FS) (builderMCPManifest, error) {
	data, err := fs.ReadFile(fsys, "manifest.json")
	if err != nil {
		return builderMCPManifest{}, fmt.Errorf("read manifest.json: %w", err)
	}
	var m builderMCPManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return builderMCPManifest{}, fmt.Errorf("parse manifest.json: %w", err)
	}
	if m.Name == "" || m.Version == "" {
		return builderMCPManifest{}, errors.New("manifest.json missing name or version")
	}
	return m, nil
}

// applyBuilderMCPMounts produces the `mounts` array in the response
// and persists any new mappings. Iterates declared mounts (not
// caller-supplied ones) so the output describes the whole picture,
// not just what the LLM passed in.
func applyBuilderMCPMounts(ctx context.Context, store mounts.Store, declared map[string]builderMCPMountDecl, mappings map[string]string) ([]map[string]any, error) {
	if len(declared) == 0 {
		return []map[string]any{}, nil
	}
	names := make([]string, 0, len(declared))
	for n := range declared {
		names = append(names, n)
	}
	sort.Strings(names)

	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		m := declared[name]
		entry := map[string]any{
			"name":        name,
			"description": m.Description,
			"path":        m.Path,
			"access":      mountAccessLabel(m.Access),
			"required":    m.Required,
		}
		existing, found, err := store.Get(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("lookup mount %s: %w", name, err)
		}
		if hostPath, ok := mappings[name]; ok {
			if err := store.Set(ctx, name, hostPath); err != nil {
				return nil, fmt.Errorf("save mount %s: %w", name, err)
			}
			entry["status"] = "mapped_now"
			entry["host_path"] = hostPath
		} else if found && existing != "" {
			entry["status"] = "already_mapped"
			entry["host_path"] = existing
		} else {
			entry["status"] = "unmapped"
			if m.Required {
				entry["instruction"] = fmt.Sprintf("Run `particle mount %s <host-path>` before invoking the particle.", name)
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

func mountAccessLabel(s string) string {
	if s == "readonly" {
		return "read-only"
	}
	return "read-write"
}

// builderMCPCredMethod mirrors the importer's credentialMethod for the
// pieces we need here (type, optional location, optional algorithm).
// Re-declared locally to avoid plumbing exports through the importer
// package for one consumer.
type builderMCPCredMethod struct {
	Name        string
	Type        string
	Description string
	Algorithm   string
	Location    *builderMCPApplyLocation
}

type builderMCPApplyLocation struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Scheme string `json:"scheme"`
}

type builderMCPCredDecl struct {
	Name     string
	Required bool
	Methods  []builderMCPCredMethod
}

func parseBuilderMCPCredentials(raw map[string]json.RawMessage) ([]builderMCPCredDecl, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]builderMCPCredDecl, 0, len(raw))
	for name, rm := range raw {
		var shell struct {
			Required bool                       `json:"required"`
			Methods  map[string]json.RawMessage `json:"methods"`
		}
		if err := json.Unmarshal(rm, &shell); err != nil {
			return nil, fmt.Errorf("parse credentials.%s: %w", name, err)
		}
		decl := builderMCPCredDecl{Name: name, Required: shell.Required}
		for mname, rmethod := range shell.Methods {
			var m struct {
				Type        string                 `json:"type"`
				Description string                 `json:"description"`
				Algorithm   string                 `json:"algorithm"`
				Location    *builderMCPApplyLocation `json:"location"`
			}
			if err := json.Unmarshal(rmethod, &m); err != nil {
				return nil, fmt.Errorf("parse credentials.%s.methods.%s: %w", name, mname, err)
			}
			decl.Methods = append(decl.Methods, builderMCPCredMethod{
				Name:        mname,
				Type:        m.Type,
				Description: m.Description,
				Algorithm:   m.Algorithm,
				Location:    m.Location,
			})
		}
		sort.Slice(decl.Methods, func(i, j int) bool { return decl.Methods[i].Name < decl.Methods[j].Name })
		out = append(out, decl)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// processBuilderMCPCredentials walks every declared credential and
// produces the response's `credentials` array. Already-configured
// credentials are reported as-is; missing ones either get set up via
// elicitation (when the client supports it AND we have a single
// elicitable method) or are flagged needs_setup with a reconfigure
// instruction.
func processBuilderMCPCredentials(ctx context.Context, sess *mcp.ServerSession, store credentials.Store, raw map[string]json.RawMessage) ([]map[string]any, error) {
	decls, err := parseBuilderMCPCredentials(raw)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(decls))
	formElicitation := clientSupportsFormElicitation(sess)

	for _, c := range decls {
		entry := map[string]any{
			"name":     c.Name,
			"required": c.Required,
		}
		// Defensive: a particle with credentials at this point
		// implies store != nil (set by the caller), but check
		// anyway so a future caller can't deref nil.
		if store == nil {
			entry["status"] = "needs_setup"
			entry["instruction"] = fmt.Sprintf("Run `particle reconfigure %s` to set up.", c.Name)
			out = append(out, entry)
			continue
		}

		configured, err := store.ConfiguredMethod(ctx, c.Name)
		if err != nil {
			return nil, fmt.Errorf("lookup %s: %w", c.Name, err)
		}
		if configured != "" {
			entry["status"] = "already_configured"
			entry["method"] = configured
			out = append(out, entry)
			continue
		}

		if !formElicitation || len(c.Methods) != 1 || !methodElicitable(c.Methods[0]) {
			entry["status"] = "needs_setup"
			entry["instruction"] = fmt.Sprintf("Run `particle reconfigure %s` to set up.", c.Name)
			out = append(out, entry)
			continue
		}

		method := c.Methods[0]
		stored, eErr := elicitCredential(ctx, sess, store, c.Name, method)
		if eErr != nil {
			return nil, fmt.Errorf("elicit %s: %w", c.Name, eErr)
		}
		if !stored {
			// User declined / cancelled the elicitation. Surface as
			// needs_setup with the standard instruction so the LLM
			// knows the build succeeded but the cred is still
			// unconfigured.
			entry["status"] = "needs_setup"
			entry["instruction"] = fmt.Sprintf("Run `particle reconfigure %s` to set up.", c.Name)
			out = append(out, entry)
			continue
		}
		entry["status"] = "configured_now"
		entry["method"] = method.Name
		out = append(out, entry)
	}
	return out, nil
}

// clientSupportsFormElicitation reports whether the connected client
// advertised form-elicitation support. Matches the SDK's own check
// (server.go Elicit) so we predict the same outcome before sending —
// when both Form and URL are nil the SDK treats the client as form-
// capable for back-compat; we mirror that.
func clientSupportsFormElicitation(sess *mcp.ServerSession) bool {
	if sess == nil {
		return false
	}
	ip := sess.InitializeParams()
	if ip == nil || ip.Capabilities == nil || ip.Capabilities.Elicitation == nil {
		return false
	}
	e := ip.Capabilities.Elicitation
	if e.Form != nil {
		return true
	}
	return e.URL == nil
}

// methodElicitable reports whether the method's type can be set up
// via a single form elicitation. oauth2 needs a browser flow; raw
// has a destructive-by-design confirm gate that's best done in the
// interactive reconfigure command; apikey only fits when the
// manifest pins the substitution location (otherwise the user has
// to make a non-secret structural choice we'd rather not bake into
// elicitation).
func methodElicitable(m builderMCPCredMethod) bool {
	switch m.Type {
	case "basic", "signing-key":
		return true
	case "apikey":
		return m.Location != nil
	}
	return false
}

// elicitCredential drives one method's elicitation, persists the
// result on accept, and returns whether anything was stored. False
// + nil error means the user actively declined/cancelled — caller
// should mark the credential as needs_setup.
func elicitCredential(ctx context.Context, sess *mcp.ServerSession, store credentials.Store, credName string, m builderMCPCredMethod) (bool, error) {
	switch m.Type {
	case "basic":
		return elicitBasic(ctx, sess, store, credName, m)
	case "apikey":
		return elicitAPIKey(ctx, sess, store, credName, m)
	case "signing-key":
		return elicitSigningKey(ctx, sess, store, credName, m)
	}
	return false, fmt.Errorf("internal: method type %q is not elicitable", m.Type)
}

func elicitBasic(ctx context.Context, sess *mcp.ServerSession, store credentials.Store, credName string, m builderMCPCredMethod) (bool, error) {
	res, err := sess.Elicit(ctx, &mcp.ElicitParams{
		Message: fmt.Sprintf("Enter HTTP Basic credentials for %q.", credName),
		RequestedSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"username": {Type: "string", Title: "Username"},
				"password": {Type: "string", Title: "Password", Format: "password"},
			},
			Required: []string{"username", "password"},
		},
	})
	if err != nil {
		return false, err
	}
	if res.Action != "accept" {
		return false, nil
	}
	username, _ := res.Content["username"].(string)
	password, _ := res.Content["password"].(string)
	if _, err := store.Put(ctx, credName, m.Name,
		credentials.BasicMeta{Username: username},
		credentials.Secret{Role: credentials.SecretRolePassword, Value: []byte(password)},
	); err != nil {
		return false, err
	}
	return true, nil
}

func elicitAPIKey(ctx context.Context, sess *mcp.ServerSession, store credentials.Store, credName string, m builderMCPCredMethod) (bool, error) {
	loc, err := builderMCPApplyLocationToSpec(m.Location)
	if err != nil {
		return false, err
	}
	res, err := sess.Elicit(ctx, &mcp.ElicitParams{
		Message: fmt.Sprintf("Enter API key for %q. Sent as: %s", credName, describeBuilderMCPApplySpec(loc)),
		RequestedSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"key": {Type: "string", Title: "API key", Format: "password"},
			},
			Required: []string{"key"},
		},
	})
	if err != nil {
		return false, err
	}
	if res.Action != "accept" {
		return false, nil
	}
	key, _ := res.Content["key"].(string)
	if _, err := store.Put(ctx, credName, m.Name,
		credentials.APIKeyMeta{Location: loc},
		credentials.Secret{Role: credentials.SecretRoleKey, Value: []byte(key)},
	); err != nil {
		return false, err
	}
	return true, nil
}

func elicitSigningKey(ctx context.Context, sess *mcp.ServerSession, store credentials.Store, credName string, m builderMCPCredMethod) (bool, error) {
	if m.Algorithm == "" {
		return false, fmt.Errorf("manifest declares signing-key %s.%s without algorithm", credName, m.Name)
	}
	res, err := sess.Elicit(ctx, &mcp.ElicitParams{
		Message: fmt.Sprintf("Enter signing key for %q (%s).", credName, m.Algorithm),
		RequestedSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"key": {Type: "string", Title: fmt.Sprintf("Signing key (%s)", m.Algorithm), Format: "password"},
			},
			Required: []string{"key"},
		},
	})
	if err != nil {
		return false, err
	}
	if res.Action != "accept" {
		return false, nil
	}
	key, _ := res.Content["key"].(string)
	if _, err := store.Put(ctx, credName, m.Name,
		credentials.SigningKeyMeta{Algorithm: m.Algorithm},
		credentials.Secret{Role: credentials.SecretRoleKey, Value: []byte(key)},
	); err != nil {
		return false, err
	}
	return true, nil
}

// builderMCPApplyLocationToSpec mirrors importer.applyLocationFromManifest —
// shape-converts the manifest's apikey location block to the typed
// credentials.ApplySpec, rejecting missing required fields up front.
func builderMCPApplyLocationToSpec(loc *builderMCPApplyLocation) (credentials.ApplySpec, error) {
	if loc == nil {
		return credentials.ApplySpec{}, errors.New("apikey method has no location")
	}
	switch loc.Kind {
	case "header":
		if loc.Name == "" {
			return credentials.ApplySpec{}, errors.New("apikey location.kind=header requires `name`")
		}
		return credentials.ApplySpec{Kind: credentials.ApplyHeader, Name: loc.Name}, nil
	case "auth-scheme":
		if loc.Scheme == "" {
			return credentials.ApplySpec{}, errors.New("apikey location.kind=auth-scheme requires `scheme`")
		}
		return credentials.ApplySpec{Kind: credentials.ApplyAuthScheme, Scheme: loc.Scheme}, nil
	case "query-param":
		if loc.Name == "" {
			return credentials.ApplySpec{}, errors.New("apikey location.kind=query-param requires `name`")
		}
		return credentials.ApplySpec{Kind: credentials.ApplyQueryParam, Name: loc.Name}, nil
	}
	return credentials.ApplySpec{}, fmt.Errorf("apikey location: unknown kind %q", loc.Kind)
}

func describeBuilderMCPApplySpec(s credentials.ApplySpec) string {
	switch s.Kind {
	case credentials.ApplyHeader:
		return s.Name + ": <value>"
	case credentials.ApplyAuthScheme:
		return "Authorization: " + s.Scheme + " <value>"
	case credentials.ApplyQueryParam:
		return "?" + s.Name + "=<value>"
	}
	return s.Kind.String()
}

// diagnosticStrings flattens build.Diagnostic warnings into the
// JSON-friendly []string the response carries.
func diagnosticStrings(ds []build.Diagnostic) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Error())
	}
	return out
}

// formatBuildError prints the same per-phase log blocks that
// `particle build -v` shows on failure, joined into a single
// message so the LLM gets the same diagnostic surface as a human.
func formatBuildError(err error) string {
	var b strings.Builder
	b.WriteString(err.Error())
	for _, l := range errLogs(err) {
		if len(l.Bytes) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n--- %s ---\n", l.Phase)
		b.Write(l.Bytes)
		if l.Bytes[len(l.Bytes)-1] != '\n' {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// mcpToolError wraps a string in an IsError tool result. Used for
// every user-recoverable failure path so the LLM client receives
// the message in-band and can retry, rather than seeing an opaque
// protocol error.
func mcpToolError(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
		IsError: true,
	}
}

// --- particle_skill tool + prompt --------------------------------

const builderMCPSkillToolDescription = `Return the particle-authoring guide for the chosen language as a single markdown document.

Args: ` + "`language`" + ` — "typescript" or "python".

Call this once at the start of a session, before writing a Particlefile, so
you have the file layout, default-export shape, runtime API surface,
capability/credential schemas, and sandbox gotchas in context. Prefer the
` + "`" + builderMCPSkillPromptName + "`" + ` prompt where the client surfaces MCP prompts (it loads the
same content as a user message); this tool is the fallback for clients that
don't render prompts.`

const builderMCPSkillInputSchema = `{
  "type": "object",
  "properties": {
    "language": {
      "type": "string",
      "enum": ["typescript", "python"],
      "description": "Source language for the particle."
    }
  },
  "required": ["language"]
}`

const builderMCPSkillPromptDescription = `Load the particle-authoring guide as a session-bootstrap user message. Pick this at the start of a session in which you intend to write a particle, then choose the source language.`

type builderMCPSkillArgs struct {
	Language string `json:"language"`
}

func handleBuilderMCPSkillTool(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args builderMCPSkillArgs
	if raw := []byte(req.Params.Arguments); len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return mcpToolError(fmt.Sprintf("parse arguments: %v", err)), nil
		}
	}
	body, err := skills.Get(args.Language)
	if err != nil {
		return mcpToolError(err.Error()), nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: body}},
	}, nil
}

func handleBuilderMCPSkillPrompt(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	lang := req.Params.Arguments["language"]
	body, err := skills.Get(lang)
	if err != nil {
		// Prompts have no in-band error result analogous to a
		// tool's IsError — bubble up as a Go error and let the
		// SDK encode it as a JSON-RPC error response.
		return nil, err
	}
	return &mcp.GetPromptResult{
		Description: fmt.Sprintf("Particle authoring guide (%s).", lang),
		Messages: []*mcp.PromptMessage{{
			Role:    "user",
			Content: &mcp.TextContent{Text: body},
		}},
	}, nil
}

// --- particle_example tool --------------------------------------

// builderMCPExampleToolDescription enumerates the available example
// names inline (set at init from the embedded FS) so the LLM picks
// from a closed set in one round-trip — no separate list_examples
// call needed.
var builderMCPExampleToolDescription = buildExampleToolDescription()

func buildExampleToolDescription() string {
	names := examples.Names()
	var b strings.Builder
	b.WriteString(`Return one reference Particlefile from the bundled examples, by name. Use these as concrete starting points or to copy idiomatic patterns.

Args: ` + "`name`" + ` — one of:
`)
	for _, n := range names {
		fmt.Fprintf(&b, "  • %s\n", n)
	}
	b.WriteString(`
The response is a JSON object: ` + "`{ \"name\": \"<name>\", \"filename\": \"Particlefile.ts|py\", \"content\": \"...\" }`" + `.
Pick a ` + "`-py`" + ` example for Python guidance, otherwise the example is TypeScript.`)
	return b.String()
}

var builderMCPExampleInputSchema = buildExampleInputSchema()

func buildExampleInputSchema() string {
	names := examples.Names()
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = strconv.Quote(n)
	}
	return `{
  "type": "object",
  "properties": {
    "name": {
      "type": "string",
      "enum": [` + strings.Join(quoted, ", ") + `],
      "description": "Example directory name (see tool description for the list)."
    }
  },
  "required": ["name"]
}`
}

type builderMCPExampleArgs struct {
	Name string `json:"name"`
}

func handleBuilderMCPExampleTool(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args builderMCPExampleArgs
	if raw := []byte(req.Params.Arguments); len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return mcpToolError(fmt.Sprintf("parse arguments: %v", err)), nil
		}
	}
	filename, content, err := examples.Get(args.Name)
	if err != nil {
		return mcpToolError(err.Error()), nil
	}
	body, err := json.MarshalIndent(map[string]any{
		"name":     args.Name,
		"filename": filename,
		"content":  content,
	}, "", "  ")
	if err != nil {
		return mcpToolError(fmt.Sprintf("encode response: %v", err)), nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
	}, nil
}
