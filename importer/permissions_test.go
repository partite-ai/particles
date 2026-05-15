package importer_test

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	credmem "github.com/partite-ai/particle/credentials/memory"
	"github.com/partite-ai/particle/importer"
)

// Permission confirmation lives in the importer; the tests
// drive Import with manifests that declare capabilities and a
// scripted prompter, and check what gets asked.

// Fresh install (no prior version) prompts to confirm caps and
// proceeds when the user accepts.
func TestImport_Permission_FreshInstall_Prompts(t *testing.T) {
	reg := newRegistry(t)
	store := credmem.New()
	prompter := &scriptedPrompter{
		t:        t,
		confirms: []bool{true}, // accept perms
	}
	fs := mkParticleFS("p", "0.1.0", `{"http":{"allowedHosts":["api.example.com"]}}`, "{}")
	if _, err := importer.Import(context.Background(), fs, importer.Options{
		Registry: reg, Credentials: store, Prompter: prompter,
	}); err != nil {
		t.Fatal(err)
	}
	if len(prompter.infos) == 0 || !strings.Contains(prompter.infos[0], "p@0.1.0 requests") {
		t.Errorf("summary not surfaced: infos=%v", prompter.infos)
	}
}

// User declines → Import errors with "permissions declined".
func TestImport_Permission_Declined_Aborts(t *testing.T) {
	reg := newRegistry(t)
	store := credmem.New()
	prompter := &scriptedPrompter{
		t:        t,
		confirms: []bool{false}, // decline
	}
	fs := mkParticleFS("p", "0.1.0", `{"http":{"allowedHosts":["api.example.com"]}}`, "{}")
	_, err := importer.Import(context.Background(), fs, importer.Options{
		Registry: reg, Credentials: store, Prompter: prompter,
	})
	if err == nil || !strings.Contains(err.Error(), "declined") {
		t.Errorf("err = %v, want one mentioning declined", err)
	}
}

// Re-install with identical caps → no prompt (silent reinstall).
func TestImport_Permission_UnchangedCaps_Silent(t *testing.T) {
	reg := newRegistry(t)
	store := credmem.New()
	caps := `{"http":{"allowedHosts":["api.example.com"]}}`

	// First install: accept perms.
	first := &scriptedPrompter{t: t, confirms: []bool{true}}
	if _, err := importer.Import(context.Background(),
		mkParticleFS("p", "0.1.0", caps, "{}"),
		importer.Options{Registry: reg, Credentials: store, Prompter: first},
	); err != nil {
		t.Fatal(err)
	}

	// Second install: same caps, fresh version. Empty prompter
	// queue — any prompt fails the test.
	second := &scriptedPrompter{t: t}
	if _, err := importer.Import(context.Background(),
		mkParticleFS("p", "0.2.0", caps, "{}"),
		importer.Options{Registry: reg, Credentials: store, Prompter: second},
	); err != nil {
		t.Fatal(err)
	}
}

// Re-install with different caps → prompt.
func TestImport_Permission_ChangedCaps_Prompts(t *testing.T) {
	reg := newRegistry(t)
	store := credmem.New()

	// First install: one allowed host.
	first := &scriptedPrompter{t: t, confirms: []bool{true}}
	if _, err := importer.Import(context.Background(),
		mkParticleFS("p", "0.1.0", `{"http":{"allowedHosts":["api.example.com"]}}`, "{}"),
		importer.Options{Registry: reg, Credentials: store, Prompter: first},
	); err != nil {
		t.Fatal(err)
	}

	// Second install: an extra host. Permission summary must
	// re-fire.
	second := &scriptedPrompter{t: t, confirms: []bool{true}}
	if _, err := importer.Import(context.Background(),
		mkParticleFS("p", "0.2.0", `{"http":{"allowedHosts":["api.example.com","other.example.com"]}}`, "{}"),
		importer.Options{Registry: reg, Credentials: store, Prompter: second},
	); err != nil {
		t.Fatal(err)
	}
	if len(second.infos) == 0 || !strings.Contains(second.infos[0], "changed from 0.1.0") {
		t.Errorf("summary should mention changed-from; infos=%v", second.infos)
	}
}

// PermissionSkip mode auto-accepts even on a fresh install.
func TestImport_Permission_SkipMode_NoPrompt(t *testing.T) {
	reg := newRegistry(t)
	store := credmem.New()
	prompter := &scriptedPrompter{t: t} // empty — any prompt fails

	if _, err := importer.Import(context.Background(),
		mkParticleFS("p", "0.1.0", `{"http":{"allowedHosts":["api.example.com"]}}`, "{}"),
		importer.Options{
			Registry: reg, Credentials: store, Prompter: prompter,
			PermissionMode: importer.PermissionSkip,
		},
	); err != nil {
		t.Fatal(err)
	}
}

// PermissionForce mode prompts even when caps are unchanged.
func TestImport_Permission_ForceMode_PromptsEvenIfUnchanged(t *testing.T) {
	reg := newRegistry(t)
	store := credmem.New()
	caps := `{"http":{"allowedHosts":["api.example.com"]}}`

	if _, err := importer.Import(context.Background(),
		mkParticleFS("p", "0.1.0", caps, "{}"),
		importer.Options{
			Registry: reg, Credentials: store,
			Prompter: &scriptedPrompter{t: t, confirms: []bool{true}},
		},
	); err != nil {
		t.Fatal(err)
	}

	// Same caps, force mode: must prompt anyway.
	second := &scriptedPrompter{t: t, confirms: []bool{true}}
	if _, err := importer.Import(context.Background(),
		mkParticleFS("p", "0.2.0", caps, "{}"),
		importer.Options{
			Registry: reg, Credentials: store, Prompter: second,
			PermissionMode: importer.PermissionForce,
		},
	); err != nil {
		t.Fatal(err)
	}
	if len(second.infos) == 0 {
		t.Errorf("force mode should have shown a summary; infos=%v", second.infos)
	}
}

// Particle declaring no capabilities → no prompt in auto mode.
func TestImport_Permission_NoCapabilities_Silent(t *testing.T) {
	reg := newRegistry(t)
	prompter := &scriptedPrompter{t: t} // any prompt fails

	if _, err := importer.Import(context.Background(),
		mkParticleFS("p", "0.1.0", `{}`, "{}"),
		importer.Options{
			Registry: reg, Prompter: prompter,
		},
	); err != nil {
		t.Fatal(err)
	}
}

// Reordering / whitespace differences in the capability JSON
// don't trigger a fresh prompt — the canonical comparison
// catches them.
func TestImport_Permission_CanonicalComparison(t *testing.T) {
	reg := newRegistry(t)
	store := credmem.New()

	// First install with one key order.
	first := &scriptedPrompter{t: t, confirms: []bool{true}}
	if _, err := importer.Import(context.Background(),
		mkParticleFS("p", "0.1.0",
			`{"http":{"allowedHosts":["a","b"]},"sockets":{"allowedEndpoints":[]}}`, "{}"),
		importer.Options{Registry: reg, Credentials: store, Prompter: first},
	); err != nil {
		t.Fatal(err)
	}

	// Re-install with keys flipped. Empty prompter — any prompt
	// fails. The byte-for-byte JSON is different but the
	// semantics are identical.
	second := &scriptedPrompter{t: t}
	if _, err := importer.Import(context.Background(),
		mkParticleFS("p", "0.2.0",
			`{"sockets":{"allowedEndpoints":[]},"http":{"allowedHosts":["a","b"]}}`, "{}"),
		importer.Options{Registry: reg, Credentials: store, Prompter: second},
	); err != nil {
		t.Fatal(err)
	}
}

// The summary text covers each capability category the
// particle's manifest declares — a quick spot-check that the
// formatter actually mentions hosts and credential methods when
// present. kv is intentionally NOT a capability (every particle
// gets a KV store unconditionally), and env is input data not a
// permission — neither belongs in the summary.
func TestImport_Permission_SummaryNamesEachCategory(t *testing.T) {
	reg := newRegistry(t)
	store := credmem.New()
	caps := `{"http":{"allowedHosts":["api.example.com"]}}`
	creds := `{"svc":{"required":true,"methods":{"pat":{"type":"apikey","description":"PAT"}}}}`
	prompter := &scriptedPrompter{t: t, confirms: []bool{false}}
	_, _ = importer.Import(context.Background(),
		mkParticleFS("p", "0.1.0", caps, creds),
		importer.Options{Registry: reg, Credentials: store, Prompter: prompter},
	)
	if len(prompter.infos) == 0 {
		t.Fatal("no summary recorded")
	}
	summary := strings.Join(prompter.infos, "\n")
	for _, want := range []string{"api.example.com", "pat", "apikey", "PAT"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}
}

// helper: a manifest fragment passes through verbatim into the
// canonical comparison (no normalization quirks for nested arrays).
func TestImport_Permission_NestedArrayOrderMatters(t *testing.T) {
	// Allow-list order IS a semantic change — [a,b] vs [b,a]
	// describes the same SET but JSON has no set type; we treat
	// arrays as ordered for the purposes of "did anything
	// change". That's intentional: a permission diff that
	// reordered a list should still resurface the prompt.
	reg := newRegistry(t)
	store := credmem.New()

	if _, err := importer.Import(context.Background(),
		mkParticleFS("p", "0.1.0", `{"http":{"allowedHosts":["a.com","b.com"]}}`, "{}"),
		importer.Options{Registry: reg, Credentials: store, Prompter: &scriptedPrompter{t: t, confirms: []bool{true}}},
	); err != nil {
		t.Fatal(err)
	}

	second := &scriptedPrompter{t: t, confirms: []bool{true}}
	if _, err := importer.Import(context.Background(),
		mkParticleFS("p", "0.2.0", `{"http":{"allowedHosts":["b.com","a.com"]}}`, "{}"),
		importer.Options{Registry: reg, Credentials: store, Prompter: second},
	); err != nil {
		t.Fatal(err)
	}
	if len(second.infos) == 0 {
		t.Error("reordered allow-list should still re-prompt — array order matters")
	}
}

// OAuth2 method declarations must surface the manifest-supplied
// authorization / token URLs in the install-time summary. A
// hostile manifest can otherwise slip a phishing URL past the
// permission prompt (the URL only shows up later, during the
// browser launch — which most users won't scrutinize).
func TestImport_Permission_OAuthURLs_Surfaced(t *testing.T) {
	reg := newRegistry(t)
	store := credmem.New()
	creds := `{
		"github":{"required":true,"methods":{
			"github_oauth":{
				"type":"oauth2",
				"description":"Sign in with GitHub",
				"authorizationUrl":"https://accounts.gloogle.com/o/oauth2/v2/auth",
				"tokenUrl":"https://oauth2.gloogle.com/token",
				"scopes":["repo","user:email"]
			}
		}}
	}`
	prompter := &scriptedPrompter{t: t, confirms: []bool{false}}
	_, _ = importer.Import(context.Background(),
		mkParticleFS("p", "0.1.0", "{}", creds),
		importer.Options{Registry: reg, Credentials: store, Prompter: prompter},
	)
	if len(prompter.infos) == 0 {
		t.Fatal("no summary")
	}
	summary := strings.Join(prompter.infos, "\n")
	for _, want := range []string{
		"github_oauth",
		"accounts.gloogle.com",
		"oauth2.gloogle.com",
		"repo",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}
}

// API-key method declarations with a manifest-supplied location
// must surface that location so the user knows whether their key
// will be sent in a header (typical) or in a URL query parameter
// (logged by CDNs / proxies).
func TestImport_Permission_APIKeyLocation_Surfaced(t *testing.T) {
	reg := newRegistry(t)
	store := credmem.New()
	creds := `{
		"svc":{"required":true,"methods":{
			"shifty":{
				"type":"apikey",
				"description":"Service API key",
				"location":{"kind":"query-param","name":"api_key"}
			}
		}}
	}`
	prompter := &scriptedPrompter{t: t, confirms: []bool{false}}
	_, _ = importer.Import(context.Background(),
		mkParticleFS("p", "0.1.0", "{}", creds),
		importer.Options{Registry: reg, Credentials: store, Prompter: prompter},
	)
	summary := strings.Join(prompter.infos, "\n")
	if !strings.Contains(summary, "?api_key=<value>") {
		t.Errorf("query-param location should be visible in summary:\n%s", summary)
	}
}

// Quick check that the formatter handles unknown capability
// categories without crashing.
func TestImport_Permission_UnknownCategory_Surfaced(t *testing.T) {
	reg := newRegistry(t)
	prompter := &scriptedPrompter{t: t, confirms: []bool{false}}
	// "telepathy" isn't a real capability; the formatter should
	// still print SOMETHING so the user sees what they're
	// agreeing to.
	_, _ = importer.Import(context.Background(),
		mkParticleFS("p", "0.1.0", `{"telepathy":{"reach":"galaxy"}}`, "{}"),
		importer.Options{Registry: reg, Prompter: prompter},
	)
	if len(prompter.infos) == 0 || !strings.Contains(prompter.infos[0], "telepathy") {
		t.Errorf("summary should name unknown category; infos=%v", prompter.infos)
	}
}

// Convince myself the fs sample is still working in this file.
var _ = fstest.MapFS{}
