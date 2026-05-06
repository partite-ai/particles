package build_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/partite-ai/particle/internal/build"
)

func TestBuild_TypeCheckPassesOnCleanProgram(t *testing.T) {
	src := fstest.MapFS{
		"Particlefile.ts": &fstest.MapFile{
			Data: []byte(`export default {
  name: "echo-tools",
  description: "Echo a string back.",
  version: "0.1.0",
  capabilities: {},
  tools: {
    echo: {
      description: "Echo the input",
      inputSchema: { type: "object", properties: { input: { type: "string" } } },
      handler: async ({ input }: { input: string }): Promise<{ result: string }> => {
        const upper: string = input.toUpperCase();
        return { result: upper };
      },
    },
  },
};
`),
		},
	}

	res, err := build.Build(context.Background(), build.Options{
		Source: src,
	})
	if err != nil {
		t.Fatalf("Build with type-check on: %v", err)
	}
	for _, w := range res.Warnings {
		// Lib bundle should make global types available; any
		// "Cannot find global type" warning means we missed a lib file.
		if strings.Contains(w.Message, "Cannot find global type") {
			t.Errorf("typecheck flagged a missing global type — lib bundle is incomplete: %v", w)
		}
	}
}

func TestBuild_TypeCheckCatchesError(t *testing.T) {
	src := fstest.MapFS{
		"Particlefile.ts": &fstest.MapFile{
			Data: []byte(`const x: string = 42;
export default {
  name: "x", description: "x", version: "0.1.0", capabilities: {},
  tools: {},
  meta: x,
};
`),
		},
	}

	_, err := build.Build(context.Background(), build.Options{
		Source: src,
	})
	if err == nil {
		t.Fatal("expected type-check failure for `const x: string = 42`")
	}
	var be *build.Error
	if !errors.As(err, &be) {
		t.Fatalf("error type: got %T, want *build.Error", err)
	}
	if be.Phase != build.PhaseTypecheck {
		t.Errorf("phase = %v, want PhaseTypecheck", be.Phase)
	}
	var saw2322 bool
	for _, d := range be.Diagnostics {
		if d.Code == 2322 {
			saw2322 = true
		}
	}
	if !saw2322 {
		t.Errorf("expected TS2322 in diagnostics, got %+v", be.Diagnostics)
	}
}
