package importer_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	_ "modernc.org/sqlite"

	"github.com/partite-ai/particles/credentials"
	credmem "github.com/partite-ai/particles/credentials/memory"
	"github.com/partite-ai/particles/importer"
	"github.com/partite-ai/particles/registry"
	regsqlite "github.com/partite-ai/particles/registry/sqlite"
)

// scriptedPrompter answers prompts from pre-seeded queues. The
// test fails if a queue runs dry — that catches "we asked the
// user a question we shouldn't have" regressions.
type scriptedPrompter struct {
	t        *testing.T
	strings  []string
	secrets  []string
	choices  []string
	confirms []bool
	infos    []string
	warns    []string
}

func (p *scriptedPrompter) String(question, def string) (string, error) {
	if len(p.strings) == 0 {
		p.t.Fatalf("prompter: out of String answers (asked: %q)", question)
	}
	v := p.strings[0]
	p.strings = p.strings[1:]
	if v == "" && def != "" {
		return def, nil
	}
	return v, nil
}

func (p *scriptedPrompter) Secret(question string) (string, error) {
	if len(p.secrets) == 0 {
		p.t.Fatalf("prompter: out of Secret answers (asked: %q)", question)
	}
	v := p.secrets[0]
	p.secrets = p.secrets[1:]
	return v, nil
}

// SecretWithKeep consumes one scripted secret. Sentinels:
//   "__keep__"  → SecretKept (preserve existing)
//   "__clear__" → SecretCleared (remove existing)
// anything else → SecretSet with that value.
func (p *scriptedPrompter) SecretWithKeep(question string) (string, importer.SecretChoice, error) {
	if len(p.secrets) == 0 {
		p.t.Fatalf("prompter: out of Secret answers (asked: %q)", question)
	}
	v := p.secrets[0]
	p.secrets = p.secrets[1:]
	switch v {
	case "__keep__":
		return "", importer.SecretKept, nil
	case "__clear__":
		return "", importer.SecretCleared, nil
	}
	return v, importer.SecretSet, nil
}

func (p *scriptedPrompter) Choice(question string, _ []importer.ChoiceOption) (string, error) {
	if len(p.choices) == 0 {
		p.t.Fatalf("prompter: out of Choice answers (asked: %q)", question)
	}
	v := p.choices[0]
	p.choices = p.choices[1:]
	return v, nil
}

func (p *scriptedPrompter) Confirm(question string, _ bool) (bool, error) {
	if len(p.confirms) == 0 {
		p.t.Fatalf("prompter: out of Confirm answers (asked: %q)", question)
	}
	v := p.confirms[0]
	p.confirms = p.confirms[1:]
	return v, nil
}

func (p *scriptedPrompter) Info(m string) { p.infos = append(p.infos, m) }
func (p *scriptedPrompter) Warn(m string) { p.warns = append(p.warns, m) }

func newRegistry(t *testing.T) registry.Registry {
	t.Helper()
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	r, err := regsqlite.New(context.Background(), db)
	if err != nil {
		t.Fatalf("regsqlite.New: %v", err)
	}
	return r
}

// mkParticleFS builds a synthetic particle FS with the given
// top-level `capabilities` and `credentials` JSON. Pass "{}" for
// either when the test doesn't exercise that field.
func mkParticleFS(name, version, capabilitiesJSON, credentialsJSON string) fstest.MapFS {
	if capabilitiesJSON == "" {
		capabilitiesJSON = "{}"
	}
	if credentialsJSON == "" {
		credentialsJSON = "{}"
	}
	manifest := fmt.Sprintf(`{"name":%q,"description":"test","version":%q,"capabilities":%s,"credentials":%s,"tools":[]}`,
		name, version, capabilitiesJSON, credentialsJSON)
	return fstest.MapFS{
		"manifest.json": &fstest.MapFile{Data: []byte(manifest)},
		"bundle.mjs":     &fstest.MapFile{Data: []byte("export default {};")},
	}
}

// -----------------------------------------------------------------------------
// Top-level Import behavior.
// -----------------------------------------------------------------------------

func TestImport_NoCredentials_RegistersDirectly(t *testing.T) {
	reg := newRegistry(t)
	entry, err := importer.Import(context.Background(), mkParticleFS("p", "0.1.0", "{}", "{}"), importer.Options{
		Registry: reg,
	})
	if err != nil {
		t.Fatal(err)
	}
	if entry.Name != "p" || entry.Version != "0.1.0" {
		t.Errorf("entry = %+v", entry)
	}
}

func TestImport_RejectsBadManifest(t *testing.T) {
	reg := newRegistry(t)
	bad := fstest.MapFS{"manifest.json": &fstest.MapFile{Data: []byte(`{"name":""}`)}}
	if _, err := importer.Import(context.Background(), bad, importer.Options{Registry: reg}); err == nil {
		t.Error("expected error for empty name/version")
	}
}

func TestImport_RequiresPrompterWhenCredsDeclared(t *testing.T) {
	reg := newRegistry(t)
	fs := mkParticleFS("p", "0.1.0", "{}", `{"auth":{"required":true,"methods":{"db":{"type":"basic"}}}}`)
	if _, err := importer.Import(context.Background(), fs, importer.Options{
		Registry:    reg,
		Credentials: credmem.New().Scoped("p"),
	}); err == nil {
		t.Error("expected error for missing Prompter")
	}
}

func TestImport_RequiresStoreWhenCredsDeclared(t *testing.T) {
	reg := newRegistry(t)
	fs := mkParticleFS("p", "0.1.0", "{}", `{"auth":{"required":true,"methods":{"db":{"type":"basic"}}}}`)
	if _, err := importer.Import(context.Background(), fs, importer.Options{
		Registry:       reg,
		Prompter:       &scriptedPrompter{t: t},
		PermissionMode: importer.PermissionSkip,
	}); err == nil {
		t.Error("expected error for missing Credentials store")
	}
}

// -----------------------------------------------------------------------------
// Idempotency: re-importing skips already-set credentials.
// -----------------------------------------------------------------------------

func TestImport_AlreadyConfigured_SkipsPrompts(t *testing.T) {
	reg := newRegistry(t)
	store := credmem.New().Scoped("p")
	ctx := context.Background()

	// Pre-populate the credential — the fixture below declares
	// one credential named "auth" with method "db"; pre-config
	// matches.
	if _, err := store.Put(ctx, "auth", "db",
		credentials.BasicMeta{Username: "alice"},
		credentials.Secret{Role: credentials.SecretRolePassword, Value: []byte("hunter2")},
	); err != nil {
		t.Fatal(err)
	}

	// Empty-prompter: the test fails if any prompt is invoked.
	prompter := &scriptedPrompter{t: t}

	fs := mkParticleFS("p", "0.1.0", "{}", `{"auth":{"required":true,"methods":{"db":{"type":"basic"}}}}`)
	if _, err := importer.Import(ctx, fs, importer.Options{
		Registry:       reg,
		Credentials:    store,
		Prompter:       prompter,
		PermissionMode: importer.PermissionSkip,
	}); err != nil {
		t.Fatal(err)
	}

	// Re-import should also skip prompting and complete.
	if _, err := importer.Import(ctx, fs, importer.Options{
		Registry:       reg,
		Credentials:    store,
		Prompter:       prompter,
		PermissionMode: importer.PermissionSkip,
	}); err != nil {
		t.Fatal(err)
	}
}

// -----------------------------------------------------------------------------
// Per-kind prompts.
// -----------------------------------------------------------------------------

func TestImport_Basic_PromptsForUsernameAndPassword(t *testing.T) {
	reg := newRegistry(t)
	store := credmem.New().Scoped("p")
	prompter := &scriptedPrompter{
		t:       t,
		strings: []string{"alice"},
		secrets: []string{"hunter2"},
	}
	fs := mkParticleFS("p", "0.1.0", "{}", `{"auth":{"required":true,"methods":{"db":{"type":"basic"}}}}`)
	if _, err := importer.Import(context.Background(), fs, importer.Options{
		Registry: reg, Credentials: store, Prompter: prompter, PermissionMode: importer.PermissionSkip,
	}); err != nil {
		t.Fatal(err)
	}
	desc, err := store.GetByName(context.Background(), "auth")
	if err != nil {
		t.Fatal(err)
	}
	meta, ok := desc.Meta.(credentials.BasicMeta)
	if !ok || meta.Username != "alice" {
		t.Errorf("meta = %+v", desc.Meta)
	}
	pwd, _ := store.ReadSecret(context.Background(), desc.ID, credentials.SecretRolePassword)
	if string(pwd) != "hunter2" {
		t.Errorf("password = %q", pwd)
	}
}

func TestImport_APIKey_HeaderLocation(t *testing.T) {
	reg := newRegistry(t)
	store := credmem.New().Scoped("p")
	prompter := &scriptedPrompter{
		t:       t,
		choices: []string{"header"},
		strings: []string{"X-Stripe-Key"},
		secrets: []string{"sk_live_xxx"},
	}
	fs := mkParticleFS("p", "0.1.0", "{}", `{"auth":{"required":true,"methods":{"stripe":{"type":"apikey"}}}}`)
	if _, err := importer.Import(context.Background(), fs, importer.Options{
		Registry: reg, Credentials: store, Prompter: prompter, PermissionMode: importer.PermissionSkip,
	}); err != nil {
		t.Fatal(err)
	}
	desc, _ := store.GetByName(context.Background(), "auth")
	meta := desc.Meta.(credentials.APIKeyMeta)
	if meta.Location.Kind != credentials.ApplyHeader || meta.Location.Name != "X-Stripe-Key" {
		t.Errorf("location = %+v", meta.Location)
	}
}

func TestImport_APIKey_AuthSchemeLocation(t *testing.T) {
	reg := newRegistry(t)
	store := credmem.New().Scoped("p")
	prompter := &scriptedPrompter{
		t:       t,
		choices: []string{"auth-scheme"},
		strings: []string{"Token"},
		secrets: []string{"tok_xxx"},
	}
	fs := mkParticleFS("p", "0.1.0", "{}", `{"auth":{"required":true,"methods":{"k":{"type":"apikey"}}}}`)
	if _, err := importer.Import(context.Background(), fs, importer.Options{
		Registry: reg, Credentials: store, Prompter: prompter, PermissionMode: importer.PermissionSkip,
	}); err != nil {
		t.Fatal(err)
	}
	desc, _ := store.GetByName(context.Background(), "auth")
	meta := desc.Meta.(credentials.APIKeyMeta)
	if meta.Location.Kind != credentials.ApplyAuthScheme || meta.Location.Scheme != "Token" {
		t.Errorf("location = %+v", meta.Location)
	}
}

// Manifest-declared location skips the location prompt — only
// the key value is asked for.
func TestImport_APIKey_LocationFromManifest_SkipsLocationPrompt(t *testing.T) {
	reg := newRegistry(t)
	store := credmem.New().Scoped("p")
	prompter := &scriptedPrompter{
		t:       t,
		secrets: []string{"sk_live_xxx"},
		// Empty `choices` and `strings` queues — any prompt
		// for the location would fail the test.
	}
	caps := `{"auth":{"required":true,"methods":{"k":{
		"type":"apikey",
		"location":{"kind":"auth-scheme","scheme":"Bearer"}
	}}}}`
	fs := mkParticleFS("p", "0.1.0", "{}", caps)
	if _, err := importer.Import(context.Background(), fs, importer.Options{
		Registry: reg, Credentials: store, Prompter: prompter, PermissionMode: importer.PermissionSkip,
	}); err != nil {
		t.Fatal(err)
	}
	desc, _ := store.GetByName(context.Background(), "auth")
	meta := desc.Meta.(credentials.APIKeyMeta)
	if meta.Location.Kind != credentials.ApplyAuthScheme || meta.Location.Scheme != "Bearer" {
		t.Errorf("location = %+v, want auth-scheme Bearer", meta.Location)
	}
}

// Header location pre-set in manifest (with required `name`).
func TestImport_APIKey_LocationFromManifest_Header(t *testing.T) {
	reg := newRegistry(t)
	store := credmem.New().Scoped("p")
	prompter := &scriptedPrompter{t: t, secrets: []string{"k"}}
	caps := `{"auth":{"required":true,"methods":{"k":{
		"type":"apikey",
		"location":{"kind":"header","name":"X-API-Key"}
	}}}}`
	fs := mkParticleFS("p", "0.1.0", "{}", caps)
	if _, err := importer.Import(context.Background(), fs, importer.Options{
		Registry: reg, Credentials: store, Prompter: prompter, PermissionMode: importer.PermissionSkip,
	}); err != nil {
		t.Fatal(err)
	}
	desc, _ := store.GetByName(context.Background(), "auth")
	meta := desc.Meta.(credentials.APIKeyMeta)
	if meta.Location.Kind != credentials.ApplyHeader || meta.Location.Name != "X-API-Key" {
		t.Errorf("location = %+v", meta.Location)
	}
}

// Manifest with kind set but the discriminator's required field
// missing — surface the error pointing at the manifest, not at
// the JS handler later.
func TestImport_APIKey_LocationFromManifest_ValidationErrors(t *testing.T) {
	cases := []struct {
		label string
		json  string
	}{
		{"header missing name", `{"kind":"header"}`},
		{"auth-scheme missing scheme", `{"kind":"auth-scheme"}`},
		{"query-param missing name", `{"kind":"query-param"}`},
		{"unknown kind", `{"kind":"telepathy"}`},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			reg := newRegistry(t)
			store := credmem.New().Scoped("p")
			prompter := &scriptedPrompter{t: t}
			caps := `{"auth":{"required":true,"methods":{"k":{"type":"apikey","location":` + c.json + `}}}}`
			fs := mkParticleFS("p", "0.1.0", "{}", caps)
			_, err := importer.Import(context.Background(), fs, importer.Options{
				Registry: reg, Credentials: store, Prompter: prompter, PermissionMode: importer.PermissionSkip,
			})
			if err == nil {
				t.Errorf("expected error for %s", c.label)
			}
		})
	}
}

func TestImport_APIKey_QueryParamLocation(t *testing.T) {
	reg := newRegistry(t)
	store := credmem.New().Scoped("p")
	prompter := &scriptedPrompter{
		t:       t,
		choices: []string{"query-param"},
		strings: []string{"api_key"},
		secrets: []string{"qq"},
	}
	fs := mkParticleFS("p", "0.1.0", "{}", `{"auth":{"required":true,"methods":{"k":{"type":"apikey"}}}}`)
	if _, err := importer.Import(context.Background(), fs, importer.Options{
		Registry: reg, Credentials: store, Prompter: prompter, PermissionMode: importer.PermissionSkip,
	}); err != nil {
		t.Fatal(err)
	}
	desc, _ := store.GetByName(context.Background(), "auth")
	meta := desc.Meta.(credentials.APIKeyMeta)
	if meta.Location.Kind != credentials.ApplyQueryParam || meta.Location.Name != "api_key" {
		t.Errorf("location = %+v", meta.Location)
	}
}

func TestImport_SigningKey_PromptsForKey(t *testing.T) {
	reg := newRegistry(t)
	store := credmem.New().Scoped("p")
	prompter := &scriptedPrompter{
		t:       t,
		secrets: []string{"deadbeef"},
	}
	fs := mkParticleFS("p", "0.1.0", "{}",
		`{"auth":{"required":true,"methods":{"hmac":{"type":"signing-key","algorithm":"hmac-sha256"}}}}`)
	if _, err := importer.Import(context.Background(), fs, importer.Options{
		Registry: reg, Credentials: store, Prompter: prompter, PermissionMode: importer.PermissionSkip,
	}); err != nil {
		t.Fatal(err)
	}
	desc, _ := store.GetByName(context.Background(), "auth")
	meta := desc.Meta.(credentials.SigningKeyMeta)
	if meta.Algorithm != "hmac-sha256" {
		t.Errorf("algorithm = %q", meta.Algorithm)
	}
	v, _ := store.ReadSecret(context.Background(), desc.ID, credentials.SecretRoleKey)
	if string(v) != "deadbeef" {
		t.Errorf("key = %q", v)
	}
}

func TestImport_SigningKey_RejectsMissingAlgorithm(t *testing.T) {
	reg := newRegistry(t)
	store := credmem.New().Scoped("p")
	prompter := &scriptedPrompter{t: t}
	fs := mkParticleFS("p", "0.1.0", "{}", `{"auth":{"required":true,"methods":{"k":{"type":"signing-key"}}}}`)
	_, err := importer.Import(context.Background(), fs, importer.Options{
		Registry: reg, Credentials: store, Prompter: prompter, PermissionMode: importer.PermissionSkip,
	})
	if err == nil || !strings.Contains(err.Error(), "algorithm") {
		t.Errorf("err = %v, want one mentioning 'algorithm'", err)
	}
}

func TestImport_Raw_RequiresConfirmation(t *testing.T) {
	reg := newRegistry(t)
	store := credmem.New().Scoped("p")

	// User declines the warning → setup aborts.
	declined := &scriptedPrompter{t: t, confirms: []bool{false}}
	fs := mkParticleFS("p", "0.1.0", "{}", `{"auth":{"required":true,"methods":{"r":{"type":"raw"}}}}`)
	if _, err := importer.Import(context.Background(), fs, importer.Options{
		Registry: reg, Credentials: store, Prompter: declined, PermissionMode: importer.PermissionSkip,
	}); err == nil {
		t.Error("expected abort error when user declines raw warning")
	}

	// User accepts and provides the value.
	accepted := &scriptedPrompter{t: t, confirms: []bool{true}, secrets: []string{"sneaky"}}
	if _, err := importer.Import(context.Background(), fs, importer.Options{
		Registry: reg, Credentials: store, Prompter: accepted, PermissionMode: importer.PermissionSkip,
	}); err != nil {
		t.Fatal(err)
	}
	desc, _ := store.GetByName(context.Background(), "auth")
	v, _ := store.ReadSecret(context.Background(), desc.ID, credentials.SecretRoleValue)
	if string(v) != "sneaky" {
		t.Errorf("value = %q", v)
	}
	// Warning text must surface to the prompter.
	if len(accepted.warns) == 0 {
		t.Error("expected at least one warn() call before raw-value prompt")
	}
}

// -----------------------------------------------------------------------------
// Multiple credentials in one Import: sorted iteration + all set.
// -----------------------------------------------------------------------------
// Method choice across alternatives.
// -----------------------------------------------------------------------------

// Multiple alternatives + user picks one → only that method's
// type-specific prompt fires; the others stay untouched.
func TestImport_MultipleMethods_PicksOne(t *testing.T) {
	reg := newRegistry(t)
	store := credmem.New().Scoped("p")
	prompter := &scriptedPrompter{
		t:       t,
		choices: []string{"pat"}, // user picks the apikey alternative
		strings: []string{"X-API-Key"},
		secrets: []string{"hunter2"},
	}
	caps := `{"auth":{"required":true,"methods":{
		"oauth":{"type":"oauth2","flows":["authorization-code-pkce"],"scopes":["repo"]},
		"pat":{"type":"apikey"}
	}}}`
	// Need a Choice for apikey location too — script it.
	prompter.choices = append(prompter.choices, "header")

	fs := mkParticleFS("p", "0.1.0", "{}", caps)
	if _, err := importer.Import(context.Background(), fs, importer.Options{
		Registry: reg, Credentials: store, Prompter: prompter, PermissionMode: importer.PermissionSkip,
	}); err != nil {
		t.Fatal(err)
	}

	// "auth" exists, backed by the "pat" method — NOT "oauth".
	desc, err := store.GetByName(context.Background(), "auth")
	if err != nil {
		t.Fatalf("expected auth configured: %v", err)
	}
	if desc.Method != "pat" {
		t.Errorf("auth.Method = %q, want pat (the alternative the user picked)", desc.Method)
	}
}

// Any one of the declared methods being already configured makes
// re-import skip prompting entirely.
func TestImport_AnyMethodConfigured_SkipsPrompting(t *testing.T) {
	reg := newRegistry(t)
	store := credmem.New().Scoped("p")
	ctx := context.Background()

	// Pre-populate credential "auth" with method "pat" — re-import
	// must NOT prompt for the "oauth" alternative method.
	if _, err := store.Put(ctx, "auth", "pat",
		credentials.APIKeyMeta{Location: credentials.ApplySpec{Kind: credentials.ApplyHeader, Name: "X-Key"}},
		credentials.Secret{Role: credentials.SecretRoleKey, Value: []byte("k")},
	); err != nil {
		t.Fatal(err)
	}

	prompter := &scriptedPrompter{t: t} // empty queues; any prompt fails the test

	caps := `{"auth":{"required":true,"methods":{
		"oauth":{"type":"oauth2","flows":["authorization-code-pkce"],"scopes":["repo"]},
		"pat":{"type":"apikey"}
	}}}`
	fs := mkParticleFS("p", "0.1.0", "{}", caps)
	if _, err := importer.Import(ctx, fs, importer.Options{
		Registry: reg, Credentials: store, Prompter: prompter, PermissionMode: importer.PermissionSkip,
	}); err != nil {
		t.Fatal(err)
	}
}

// The chosen method shows up as the (single) credential row in
// the store; the runtime resolves it via
// credentials.Store.ConfiguredMethod.
func TestImport_RecordsSelectedMethodInCredentialsStore(t *testing.T) {
	reg := newRegistry(t)
	store := credmem.New().Scoped("p")
	prompter := &scriptedPrompter{
		t:       t,
		choices: []string{"pat", "header"}, // pick "pat", then header location
		strings: []string{"X-API-Key"},
		secrets: []string{"k"},
	}
	caps := `{"auth":{"required":true,"methods":{
		"oauth":{"type":"oauth2","flows":["authorization-code-pkce"],"scopes":["repo"]},
		"pat":{"type":"apikey"}
	}}}`
	fs := mkParticleFS("p", "0.1.0", "{}", caps)
	if _, err := importer.Import(context.Background(), fs, importer.Options{
		Registry: reg, Credentials: store, Prompter: prompter, PermissionMode: importer.PermissionSkip,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := store.ConfiguredMethod(context.Background(), "auth")
	if err != nil {
		t.Fatal(err)
	}
	if got != "pat" {
		t.Errorf("ConfiguredMethod = %q, want pat", got)
	}
}

// required=false + nothing configured → setup is skipped (no
// prompts), particle still registers.
func TestImport_OptionalAuth_SkipsSetup(t *testing.T) {
	reg := newRegistry(t)
	store := credmem.New().Scoped("p")
	prompter := &scriptedPrompter{t: t} // empty; any prompt fails the test

	caps := `{"auth":{"required":false,"methods":{
		"pat":{"type":"apikey"}
	}}}`
	fs := mkParticleFS("p", "0.1.0", "{}", caps)
	if _, err := importer.Import(context.Background(), fs, importer.Options{
		Registry: reg, Credentials: store, Prompter: prompter, PermissionMode: importer.PermissionSkip,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetByName(context.Background(), "auth"); !errors.Is(err, credentials.ErrNotFound) {
		t.Errorf("expected nothing configured for optional auth; err = %v", err)
	}
}

// -----------------------------------------------------------------------------
// Reconfigure
// -----------------------------------------------------------------------------

// reconfigureSetup runs Import once to register a particle and
// pick an initial method, returning the registry/store/manifest
// the Reconfigure tests then operate on.
func reconfigureSetup(t *testing.T, manifestCaps string, initialChoices, initialStrings, initialSecrets []string) (registry.Registry, credentials.Store, string) {
	t.Helper()
	reg := newRegistry(t)
	store := credmem.New().Scoped("p")
	prompter := &scriptedPrompter{
		t: t, choices: initialChoices, strings: initialStrings, secrets: initialSecrets,
	}
	fs := mkParticleFS("p", "0.1.0", "{}", manifestCaps)
	if _, err := importer.Import(context.Background(), fs, importer.Options{
		Registry: reg, Credentials: store, Prompter: prompter, PermissionMode: importer.PermissionSkip,
	}); err != nil {
		t.Fatalf("initial Import: %v", err)
	}
	return reg, store, manifestCaps
}

// Switching methods removes the previous credential AFTER the new
// one writes successfully — preserving the invariant that a
// successful reconfigure always leaves exactly one method
// configured.
func TestReconfigure_SwitchMethod_RemovesPrevious(t *testing.T) {
	caps := `{"auth":{"required":true,"methods":{
		"oauth":{"type":"oauth2","flows":["authorization-code-pkce"],"scopes":["repo"]},
		"pat":{"type":"apikey","location":{"kind":"auth-scheme","scheme":"Bearer"}}
	}}}`
	reg, store, _ := reconfigureSetup(t, caps,
		[]string{"pat"}, // initial choice = pat
		nil,
		[]string{"old-pat"},
	)

	// Now reconfigure: re-pick the same method to refresh its
	// value. With two methods declared the prompter still asks
	// for a method choice — script it.
	prompter := &scriptedPrompter{
		t:       t,
		choices: []string{"pat"},
		secrets: []string{"new-pat"},
	}
	_, method, err := importer.Reconfigure(context.Background(), "p", "", importer.Options{
		Registry: reg, Credentials: store, Prompter: prompter, PermissionMode: importer.PermissionSkip,
	})
	if err != nil {
		t.Fatal(err)
	}
	if method != "pat" {
		t.Errorf("Reconfigure returned method = %q, want pat", method)
	}
	desc, _ := store.GetByName(context.Background(), "auth")
	v, _ := store.ReadSecret(context.Background(), desc.ID, credentials.SecretRoleKey)
	if string(v) != "new-pat" {
		t.Errorf("pat key = %q, want new-pat (overwritten in place)", v)
	}
}

// Picking a different method deletes the prior credential.
func TestReconfigure_DifferentMethod_DropsPrevious(t *testing.T) {
	caps := `{"auth":{"required":true,"methods":{
		"a":{"type":"basic"},
		"b":{"type":"apikey","location":{"kind":"header","name":"X-K"}}
	}}}`
	reg, store, _ := reconfigureSetup(t, caps,
		[]string{"a"},       // pick "a"
		[]string{"alice"},   // username
		[]string{"hunter2"}, // password
	)

	// Reconfigure: pick "b" instead.
	prompter := &scriptedPrompter{
		t:       t,
		choices: []string{"b"},         // method choice
		secrets: []string{"key-value"}, // apikey secret
	}
	_, method, err := importer.Reconfigure(context.Background(), "p", "", importer.Options{
		Registry: reg, Credentials: store, Prompter: prompter, PermissionMode: importer.PermissionSkip,
	})
	if err != nil {
		t.Fatal(err)
	}
	if method != "b" {
		t.Errorf("Reconfigure returned method = %q, want b", method)
	}
	// One row under credential "auth"; method = "b". Method switch
	// in Put wiped the prior method's secrets in the same tx.
	desc, err := store.GetByName(context.Background(), "auth")
	if err != nil {
		t.Fatalf("expected auth configured: %v", err)
	}
	if desc.Method != "b" {
		t.Errorf("auth.Method = %q, want b", desc.Method)
	}
	// The prior method's password secret must have been wiped.
	if _, err := store.ReadSecret(context.Background(), desc.ID, credentials.SecretRolePassword); !errors.Is(err, credentials.ErrSecretNotSet) {
		t.Errorf("prior basic password should be wiped after method switch; err = %v", err)
	}
}

// If the new method's setup errors out, the previous credential
// must remain intact — we don't drop it until the new one writes
// successfully.
func TestReconfigure_SetupError_PreservesPrevious(t *testing.T) {
	caps := `{"auth":{"required":true,"methods":{
		"a":{"type":"basic"},
		"b":{"type":"raw"}
	}}}`
	reg, store, _ := reconfigureSetup(t, caps,
		[]string{"a"},
		[]string{"alice"},
		[]string{"pw"},
	)

	// Reconfigure: pick "b" but decline the raw confirmation —
	// setup aborts. Prior credential must survive.
	prompter := &scriptedPrompter{
		t:        t,
		choices:  []string{"b"},
		confirms: []bool{false},
	}
	if _, _, err := importer.Reconfigure(context.Background(), "p", "", importer.Options{
		Registry: reg, Credentials: store, Prompter: prompter, PermissionMode: importer.PermissionSkip,
	}); err == nil {
		t.Fatal("expected error for declined raw setup")
	}
	// The "auth" credential is still configured with method "a";
	// the failed setup never wrote the new row.
	desc, err := store.GetByName(context.Background(), "auth")
	if err != nil {
		t.Fatalf("previous credential should still be present after failed reconfigure; err = %v", err)
	}
	if desc.Method != "a" {
		t.Errorf("auth.Method = %q, want a (preserved)", desc.Method)
	}
	method, _ := store.ConfiguredMethod(context.Background(), "auth")
	if method != "a" {
		t.Errorf("ConfiguredMethod = %q, want a (preserved)", method)
	}
}

// Reconfiguring an unregistered particle is an error pointing at
// the registry, not a silent no-op.
func TestReconfigure_NotRegistered(t *testing.T) {
	reg := newRegistry(t)
	store := credmem.New().Scoped("p")
	_, _, err := importer.Reconfigure(context.Background(), "absent", "", importer.Options{
		Registry: reg, Credentials: store, Prompter: &scriptedPrompter{t: t}, PermissionMode: importer.PermissionSkip,
	})
	if err == nil {
		t.Error("expected error for unregistered particle")
	}
}

// A particle without a credentials capability has nothing to
// reconfigure — surface that as an error rather than a vacuous
// success.
func TestReconfigure_NoCredentialsDeclared(t *testing.T) {
	reg := newRegistry(t)
	if err := reg.Put(context.Background(), "p", "0.1.0", mkParticleFS("p", "0.1.0", "{}", "{}")); err != nil {
		t.Fatal(err)
	}
	_, _, err := importer.Reconfigure(context.Background(), "p", "", importer.Options{
		Registry: reg, Credentials: credmem.New().Scoped("p"), Prompter: &scriptedPrompter{t: t}, PermissionMode: importer.PermissionSkip,
	})
	if err == nil || !strings.Contains(err.Error(), "no credentials") {
		t.Errorf("err = %v, want one mentioning 'no credentials'", err)
	}
}

// -----------------------------------------------------------------------------
// Unrecognized type — surface a useful error rather than silently
// proceeding.
// -----------------------------------------------------------------------------

func TestImport_UnknownCredentialType(t *testing.T) {
	reg := newRegistry(t)
	store := credmem.New().Scoped("p")
	prompter := &scriptedPrompter{t: t}
	fs := mkParticleFS("p", "0.1.0", "{}", `{"auth":{"required":true,"methods":{"x":{"type":"telepathy"}}}}`)
	_, err := importer.Import(context.Background(), fs, importer.Options{
		Registry: reg, Credentials: store, Prompter: prompter, PermissionMode: importer.PermissionSkip,
	})
	if err == nil || !strings.Contains(err.Error(), "telepathy") {
		t.Errorf("err = %v, want mention of unknown type", err)
	}
}

// -----------------------------------------------------------------------------
// Storage errors propagate.
// -----------------------------------------------------------------------------

type errStore struct{ err error }

func (s *errStore) GetByID(context.Context, string) (credentials.Descriptor, error) {
	return credentials.Descriptor{}, s.err
}
func (s *errStore) GetByName(context.Context, string) (credentials.Descriptor, error) {
	return credentials.Descriptor{}, s.err
}
func (s *errStore) List(context.Context) ([]credentials.ListEntry, error) { return nil, s.err }
func (s *errStore) Put(context.Context, string, string, credentials.Metadata, ...credentials.Secret) (credentials.Descriptor, error) {
	return credentials.Descriptor{}, s.err
}
func (s *errStore) Delete(context.Context, string) error { return s.err }
func (s *errStore) ConfiguredMethod(context.Context, string) (string, error) {
	return "", s.err
}
func (s *errStore) ReadSecret(context.Context, string, credentials.SecretRole) ([]byte, error) {
	return nil, s.err
}
func (s *errStore) WriteSecrets(context.Context, string, ...credentials.Secret) error {
	return s.err
}
func (s *errStore) DeleteSecret(context.Context, string, credentials.SecretRole) error {
	return s.err
}

func TestImport_StoreLookupError_Propagates(t *testing.T) {
	reg := newRegistry(t)
	boom := errors.New("disk failed")
	prompter := &scriptedPrompter{t: t}
	fs := mkParticleFS("p", "0.1.0", "{}", `{"auth":{"required":true,"methods":{"db":{"type":"basic"}}}}`)
	_, err := importer.Import(context.Background(), fs, importer.Options{
		Registry: reg, Credentials: &errStore{err: boom}, Prompter: prompter, PermissionMode: importer.PermissionSkip,
	})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want chain containing %v", err, boom)
	}
}

// -----------------------------------------------------------------------------
// Keep-current affordance
// -----------------------------------------------------------------------------

// Reconfigure with the same method: pressing Enter on the secret
// prompt (sentinel "__keep__") and on the visible field (empty
// string → take default) leaves both fields exactly as they were.
func TestReconfigure_SameMethod_KeepCurrent_PreservesAll(t *testing.T) {
	caps := `{"auth":{"required":true,"methods":{"basic":{"type":"basic"}}}}`
	reg, store, _ := reconfigureSetup(t, caps,
		nil,                 // single method, no choice
		[]string{"alice"},   // username
		[]string{"hunter2"}, // password
	)

	// Reconfigure: keep both. "" with default → existing username;
	// "__keep__" on SecretWithKeep → skip secret write.
	prompter := &scriptedPrompter{
		t:       t,
		strings: []string{""},          // empty → default ("alice")
		secrets: []string{"__keep__"},  // skip write
	}
	_, _, err := importer.Reconfigure(context.Background(), "p", "", importer.Options{
		Registry: reg, Credentials: store, Prompter: prompter, PermissionMode: importer.PermissionSkip,
	})
	if err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}
	desc, err := store.GetByName(context.Background(), "auth")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	meta, ok := desc.Meta.(credentials.BasicMeta)
	if !ok || meta.Username != "alice" {
		t.Errorf("username = %+v, want BasicMeta{Username:\"alice\"}", desc.Meta)
	}
	pw, err := store.ReadSecret(context.Background(), desc.ID, credentials.SecretRolePassword)
	if err != nil {
		t.Fatalf("ReadSecret: %v", err)
	}
	if string(pw) != "hunter2" {
		t.Errorf("password = %q, want hunter2 (preserved via keep-current)", pw)
	}
}

// Mixing keep + change: keep the username, rotate the password.
// Verifies that the kept field stays and the changed one
// overwrites — i.e. keep-current is per-field, not all-or-nothing.
func TestReconfigure_SameMethod_PartialKeep(t *testing.T) {
	caps := `{"auth":{"required":true,"methods":{"basic":{"type":"basic"}}}}`
	reg, store, _ := reconfigureSetup(t, caps,
		nil,
		[]string{"alice"},
		[]string{"hunter2"},
	)
	prompter := &scriptedPrompter{
		t:       t,
		strings: []string{""},        // keep username
		secrets: []string{"newpass"}, // rotate password
	}
	if _, _, err := importer.Reconfigure(context.Background(), "p", "", importer.Options{
		Registry: reg, Credentials: store, Prompter: prompter, PermissionMode: importer.PermissionSkip,
	}); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}
	desc, _ := store.GetByName(context.Background(), "auth")
	if got := desc.Meta.(credentials.BasicMeta).Username; got != "alice" {
		t.Errorf("username = %q, want alice", got)
	}
	pw, _ := store.ReadSecret(context.Background(), desc.ID, credentials.SecretRolePassword)
	if string(pw) != "newpass" {
		t.Errorf("password = %q, want newpass", pw)
	}
}

// Explicit "clear" sentinel removes the stored secret: after
// reconfigure with "__clear__", ReadSecret on that role returns
// ErrSecretNotSet.
func TestReconfigure_SameMethod_ClearSecret(t *testing.T) {
	caps := `{"auth":{"required":true,"methods":{"basic":{"type":"basic"}}}}`
	reg, store, _ := reconfigureSetup(t, caps,
		nil,
		[]string{"alice"},
		[]string{"hunter2"},
	)
	prompter := &scriptedPrompter{
		t:       t,
		strings: []string{""},          // keep username
		secrets: []string{"__clear__"}, // explicit clear
	}
	if _, _, err := importer.Reconfigure(context.Background(), "p", "", importer.Options{
		Registry: reg, Credentials: store, Prompter: prompter, PermissionMode: importer.PermissionSkip,
	}); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}
	desc, _ := store.GetByName(context.Background(), "auth")
	if got := desc.Meta.(credentials.BasicMeta).Username; got != "alice" {
		t.Errorf("username = %q, want alice (kept)", got)
	}
	if _, err := store.ReadSecret(context.Background(), desc.ID, credentials.SecretRolePassword); !errors.Is(err, credentials.ErrSecretNotSet) {
		t.Errorf("password should be cleared; ReadSecret err = %v, want ErrSecretNotSet", err)
	}
}

// API-key reconfigure with __keep__ preserves the stored key
// even though it's a single-secret credential.
func TestReconfigure_APIKey_KeepCurrent(t *testing.T) {
	caps := `{"auth":{"required":true,"methods":{
		"pat":{"type":"apikey","location":{"kind":"auth-scheme","scheme":"Bearer"}}
	}}}`
	reg, store, _ := reconfigureSetup(t, caps, nil, nil, []string{"orig-key"})
	prompter := &scriptedPrompter{t: t, secrets: []string{"__keep__"}}
	if _, _, err := importer.Reconfigure(context.Background(), "p", "", importer.Options{
		Registry: reg, Credentials: store, Prompter: prompter, PermissionMode: importer.PermissionSkip,
	}); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}
	desc, _ := store.GetByName(context.Background(), "auth")
	key, err := store.ReadSecret(context.Background(), desc.ID, credentials.SecretRoleKey)
	if err != nil {
		t.Fatalf("ReadSecret: %v", err)
	}
	if string(key) != "orig-key" {
		t.Errorf("apikey = %q, want orig-key (preserved)", key)
	}
}

// Switching method while a prior credential exists should NOT
// trigger the keep-current path — the typed metadata doesn't
// match, so the helper sees no existing state and asks for every
// field fresh.
func TestReconfigure_SwitchMethod_DoesNotKeepCurrent(t *testing.T) {
	caps := `{"auth":{"required":true,"methods":{
		"a":{"type":"basic"},
		"b":{"type":"apikey","location":{"kind":"header","name":"X-K"}}
	}}}`
	reg, store, _ := reconfigureSetup(t, caps,
		[]string{"a"},
		[]string{"alice"},
		[]string{"pw"},
	)
	// Reconfigure: pick "b", provide a real key. If sameMethodMeta
	// returned the prior BasicMeta by mistake, setupAPIKey would
	// take the keep-current branch and the scripted "real-key"
	// would go unconsumed — t.Fatalf would fire.
	prompter := &scriptedPrompter{
		t:       t,
		choices: []string{"b"},
		secrets: []string{"real-key"},
	}
	if _, _, err := importer.Reconfigure(context.Background(), "p", "", importer.Options{
		Registry: reg, Credentials: store, Prompter: prompter, PermissionMode: importer.PermissionSkip,
	}); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}
	if len(prompter.secrets) != 0 {
		t.Errorf("scripted secret was kept-as-current instead of consumed: %v", prompter.secrets)
	}
}

// -----------------------------------------------------------------------------
// ReauthOAuth
// -----------------------------------------------------------------------------

// Non-oauth2 credential → clear error pointing to the full
// reconfigure path.
func TestReauthOAuth_RejectsNonOAuth(t *testing.T) {
	caps := `{"auth":{"required":true,"methods":{
		"pat":{"type":"apikey","location":{"kind":"auth-scheme","scheme":"Bearer"}}
	}}}`
	reg, store, _ := reconfigureSetup(t, caps, nil, nil, []string{"k"})
	_, _, err := importer.ReauthOAuth(context.Background(), "p", "", importer.Options{
		Registry: reg, Credentials: store, Prompter: &scriptedPrompter{t: t},
	})
	if err == nil || !strings.Contains(err.Error(), "oauth2") {
		t.Errorf("err = %v, want one mentioning oauth2", err)
	}
}

// Credential not yet configured → message pointing user at full
// reconfigure first.
func TestReauthOAuth_NotConfigured(t *testing.T) {
	reg := newRegistry(t)
	store := credmem.New().Scoped("p")
	caps := `{"auth":{"required":false,"methods":{"oauth":{"type":"oauth2","flows":["authorization-code-pkce"]}}}}`
	if err := reg.Put(context.Background(), "p", "0.1.0", mkParticleFS("p", "0.1.0", "{}", caps)); err != nil {
		t.Fatal(err)
	}
	_, _, err := importer.ReauthOAuth(context.Background(), "p", "", importer.Options{
		Registry: reg, Credentials: store, Prompter: &scriptedPrompter{t: t},
	})
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("err = %v, want one mentioning 'not configured'", err)
	}
}

// When the manifest no longer lists the stored flow (e.g. the
// particle author removed device-code), reauth surfaces a clear
// message rather than silently picking a different flow.
func TestReauthOAuth_StoredFlowNoLongerDeclared(t *testing.T) {
	// Hand-seed the store with an oauth2 descriptor whose Flow
	// isn't in the registered manifest's flows list. We bypass
	// the real OAuth setup (which would actually open a browser)
	// by writing directly via Store.Put.
	reg := newRegistry(t)
	store := credmem.New().Scoped("p")
	caps := `{"auth":{"required":true,"methods":{"oauth":{"type":"oauth2","flows":["authorization-code-pkce"]}}}}`
	if err := reg.Put(context.Background(), "p", "0.1.0", mkParticleFS("p", "0.1.0", "{}", caps)); err != nil {
		t.Fatal(err)
	}
	meta := credentials.OAuth2Meta{
		AuthorizationURL: "https://example/auth",
		TokenURL:         "https://example/token",
		ClientID:         "cid",
		Flow:             "device-code", // not in manifest's flows
	}
	bundle := credentials.AccessToken{Token: "stale"}
	if _, err := store.Put(context.Background(), "auth", "oauth", meta,
		credentials.Secret{Role: credentials.SecretRoleAccessToken, Value: bundle.Marshal()},
	); err != nil {
		t.Fatal(err)
	}
	_, _, err := importer.ReauthOAuth(context.Background(), "p", "", importer.Options{
		Registry: reg, Credentials: store, Prompter: &scriptedPrompter{t: t},
	})
	if err == nil || !strings.Contains(err.Error(), "no longer listed") {
		t.Errorf("err = %v, want one mentioning 'no longer listed'", err)
	}
}

// Method removed from the manifest entirely.
func TestReauthOAuth_StoredMethodGone(t *testing.T) {
	reg := newRegistry(t)
	store := credmem.New().Scoped("p")
	caps := `{"auth":{"required":true,"methods":{"oauth":{"type":"oauth2","flows":["authorization-code-pkce"]}}}}`
	if err := reg.Put(context.Background(), "p", "0.1.0", mkParticleFS("p", "0.1.0", "{}", caps)); err != nil {
		t.Fatal(err)
	}
	meta := credentials.OAuth2Meta{Flow: "authorization-code-pkce"}
	if _, err := store.Put(context.Background(), "auth", "old-method-name", meta,
		credentials.Secret{Role: credentials.SecretRoleAccessToken, Value: credentials.AccessToken{Token: "x"}.Marshal()},
	); err != nil {
		t.Fatal(err)
	}
	_, _, err := importer.ReauthOAuth(context.Background(), "p", "", importer.Options{
		Registry: reg, Credentials: store, Prompter: &scriptedPrompter{t: t},
	})
	if err == nil || !strings.Contains(err.Error(), "no longer declares") {
		t.Errorf("err = %v, want one mentioning 'no longer declares'", err)
	}
}
