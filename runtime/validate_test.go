package runtime

import "testing"

func TestValidateToolOutput(t *testing.T) {
	schema, err := compileToolSchema([]byte(`{"type":"object","properties":{"sum":{"type":"number"}},"required":["sum"]}`))
	if err != nil {
		t.Fatalf("compileToolSchema: %v", err)
	}

	if err := validateToolOutput(schema, []byte(`{"sum":5}`)); err != nil {
		t.Errorf("conforming output: unexpected err %v", err)
	}
	if err := validateToolOutput(schema, []byte(`{"sum":"five"}`)); err == nil {
		t.Error("wrong type: want err, got nil")
	}
	if err := validateToolOutput(schema, []byte(`{}`)); err == nil {
		t.Error("missing required: want err, got nil")
	}
	if err := validateToolOutput(schema, []byte(``)); err == nil {
		t.Error("empty output: want err, got nil (no implicit {} for outputs)")
	}
	if err := validateToolOutput(nil, []byte(`"anything"`)); err != nil {
		t.Errorf("nil schema: want nil, got %v", err)
	}
}
