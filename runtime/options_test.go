package runtime

import (
	"reflect"
	"testing"
)

// activeCredentialNames returns the input slice the wasi:http
// policy iterates for substitution. The "selected method"
// invariant means at most one — these tests pin that contract so
// future changes (e.g., supporting concurrent multi-method
// substitution) are loud rather than silent.

func TestActiveCredentialNames_Empty(t *testing.T) {
	if got := activeCredentialNames(""); got != nil {
		t.Errorf("activeCredentialNames(\"\") = %v, want nil", got)
	}
}

func TestActiveCredentialNames_Selected(t *testing.T) {
	got := activeCredentialNames("oauth")
	want := []string{"oauth"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("activeCredentialNames(\"oauth\") = %v, want %v", got, want)
	}
}

func TestWithSelectedAuthenticationMethod_PopulatesConfig(t *testing.T) {
	cfg := particleConfig{}
	WithSelectedAuthenticationMethod("pat")(&cfg)
	if cfg.selectedAuthenticationMethod != "pat" {
		t.Errorf("config.selectedAuthenticationMethod = %q, want pat", cfg.selectedAuthenticationMethod)
	}
}
