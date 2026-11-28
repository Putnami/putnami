package emit

import (
	"path/filepath"
	"testing"
)

// sameEmit answers, for every module, the digest of JavaScript that does not
// depend on the module's types: what Bun emits for a type-only edit.
func sameEmit(request TranspileRequest) (map[string]string, error) {
	out := make(map[string]string, len(request.Files))
	for _, file := range request.Files {
		out[file.Path] = digest([]byte("emitted " + file.Path))
	}
	return out, nil
}

func buildUnder(t *testing.T, ws string, members []Member, runner string) Manifest {
	t.Helper()
	m, err := Build(Options{WorkspaceRoot: ws, Members: members, Transpile: sameEmit, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// Only a Bun project keys a dependency's module on its emitted JavaScript.
// Under Vitest or Jest, or a runner that is not known, a type-only edit in a
// dependency moves the manifest, since the runner's own transform may read the
// types Bun strips, and the manifest says why it keys on bytes.
func TestATypeOnlyEditMovesTheManifestUnlessBunRunsTheTests(t *testing.T) {
	for _, tc := range []struct {
		runner string
		mode   string
	}{
		{RunnerBun, ModeEmit},
		{"vitest", ModeBytes},
		{"jest", ModeBytes},
		{"", ModeBytes},
	} {
		t.Run("runner "+tc.runner, func(t *testing.T) {
			ws, members := fixture(t)
			before := buildUnder(t, ws, members, tc.runner)
			if before.Mode != tc.mode {
				t.Fatalf("mode = %s (%s), want %s", before.Mode, before.Reason, tc.mode)
			}
			if tc.mode == ModeBytes && before.Reason == "" {
				t.Fatal("a bytes manifest records no reason")
			}
			writeFile(t, filepath.Join(ws, "lib", "src", "shape.ts"), "export interface Shape { w: string; h: number }\n")
			after := buildUnder(t, ws, members, tc.runner)
			moved := encoded(t, after) != encoded(t, before)
			if want := tc.mode == ModeBytes; moved != want {
				t.Fatalf("under %q a type-only edit moved the manifest: %v, want %v", tc.runner, moved, want)
			}
		})
	}
}
