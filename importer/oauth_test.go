package importer

import (
	"errors"
	"testing"
)

// fakePrompter is the lightest stub the tests below need —
// recordPrompts captures every String call so we can assert
// "the user wasn't asked" when the manifest pre-set a URL.
type fakePrompter struct {
	stringAnswer string
	stringErr    error
	stringCalls  []string // (label) per call
	infos        []string
}

func (p *fakePrompter) String(question, _ string) (string, error) {
	p.stringCalls = append(p.stringCalls, question)
	return p.stringAnswer, p.stringErr
}
func (p *fakePrompter) Secret(string) (string, error)                          { return "", nil }
func (p *fakePrompter) Choice(string, []ChoiceOption) (string, error)          { return "", nil }
func (p *fakePrompter) Confirm(string, bool) (bool, error)                     { return false, nil }
func (p *fakePrompter) Warn(string)                                            {}
func (p *fakePrompter) Info(m string)                                          { p.infos = append(p.infos, m) }

// Manifest-set URL → no prompt; the value flows through verbatim.
func TestResolveOAuthURL_ManifestOverridesPreset(t *testing.T) {
	p := &fakePrompter{}
	got, err := resolveOAuthURL(p, "Token URL", "https://example/token", "https://github/login/oauth/access_token")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://example/token" {
		t.Errorf("got %q, want manifest value", got)
	}
	if len(p.stringCalls) != 0 {
		t.Errorf("prompt was called %d times, want 0 — manifest override should skip prompt", len(p.stringCalls))
	}
	if len(p.infos) == 0 {
		t.Error("expected an info-level log for the manifest-supplied URL")
	}
}

// Empty manifest value → prompts; the provider preset becomes
// the prompt's default. We don't inspect the default here because
// the prompter receives it through the second parameter — see
// the StdioPrompter implementation for the visible behavior.
func TestResolveOAuthURL_EmptyManifestPrompts(t *testing.T) {
	p := &fakePrompter{stringAnswer: "https://provider/token"}
	got, err := resolveOAuthURL(p, "Token URL", "", "https://provider/token")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://provider/token" {
		t.Errorf("got %q", got)
	}
	if len(p.stringCalls) != 1 {
		t.Errorf("prompt was called %d times, want 1", len(p.stringCalls))
	}
}

// Prompter errors propagate.
func TestResolveOAuthURL_PromptErrorPropagates(t *testing.T) {
	boom := errors.New("io")
	p := &fakePrompter{stringErr: boom}
	_, err := resolveOAuthURL(p, "Token URL", "", "")
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want chain containing %v", err, boom)
	}
}
