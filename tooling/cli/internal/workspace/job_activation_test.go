package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
)

// A workspace-once command may be planned whatever the project; any other one
// only in a project that uses its extension and holds one of its activation
// files.
func TestMayPlanJob(t *testing.T) {
	found := func([]string) bool { return true }
	missing := func([]string) bool { return false }
	user := &Project{ID: "/libs/app", Name: "app", Extensions: []string{"/go/extension"}}
	other := &Project{ID: "/libs/pub", Name: "pub"}
	command := &extension.JobDefinition{Name: "build", ExtensionName: "@test/go", ExtensionPath: "go/extension", ActivationFiles: []string{"go.mod"}}
	once := &extension.JobDefinition{Name: "validate-workspace", ExtensionName: "@test/go", Activation: workspaceOnceActivation, ActivationFiles: []string{"go.mod"}}
	bare := &extension.JobDefinition{Name: "lint", ExtensionName: "@test/go"}
	for _, tc := range []struct {
		name     string
		project  *Project
		job      *extension.JobDefinition
		hasFiles func([]string) bool
		want     bool
	}{
		{"a user that holds an activation file", user, command, found, true},
		{"a user without an activation file", user, command, missing, false},
		{"a project that does not use the extension", other, command, found, false},
		{"a workspace-once command", other, once, missing, true},
		{"a command without activation files, by extension name", &Project{ID: "/a", Extensions: []string{"@test/go"}}, bare, missing, true},
		{"a command without activation files, unused", other, bare, found, false},
	} {
		if got := MayPlanJob(tc.project, tc.job, tc.hasFiles); got != tc.want {
			t.Errorf("%s: MayPlanJob = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestARecursiveActivationMatchIsAnsweredByItsWitnessUntilAPathMoves holds a
// found and a missing match under their witnesses: an unchanged tree answers
// without a walk, and each change below answers what a fresh walk answers.
func TestARecursiveActivationMatchIsAnsweredByItsWitnessUntilAPathMoves(t *testing.T) {
	root := settledTree(t)
	nested := filepath.Join(root, "c")
	roots := map[string]bool{root: true, nested: true}
	ask := func(pattern string) bool { return HasActivationFiles(root, []string{pattern}, roots) }
	if !ask("**/*.go") || ask("**/*.ts") {
		t.Fatal("the first walk answered wrong")
	}
	walks := activationWalks.Load()
	if !ask("**/*.go") || ask("**/*.ts") {
		t.Fatal("a held answer differs from the walk's")
	}
	if activationWalks.Load() != walks {
		t.Fatal("an unchanged tree was walked again despite holding witnesses")
	}

	fresh := func(pattern string) bool {
		activationWitnesses.Range(func(key, _ any) bool {
			activationWitnesses.Delete(key)
			return true
		})
		return ask(pattern)
	}
	for _, change := range []struct {
		name    string
		pattern string
		write   func()
	}{
		{"a matching file created deep down", "**/*.ts", func() {
			_ = os.MkdirAll(filepath.Join(root, "a", "b", "d"), 0o755)
			_ = os.WriteFile(filepath.Join(root, "a", "b", "d", "w.ts"), []byte("w"), 0o644)
		}},
		{"the only match removed", "**/*.ts", func() {
			_ = os.Remove(filepath.Join(root, "a", "b", "d", "w.ts"))
		}},
		{"a match renamed away", "**/*.go", func() {
			for _, file := range []string{"a/x.go", "a/b/y.go"} {
				_ = os.Rename(filepath.Join(root, file), filepath.Join(root, file+".txt"))
			}
		}},
		{"a match under a nested project only", "**/*.go", func() {}},
	} {
		ask(change.pattern) // hold the answer before the change
		change.write()
		got := ask(change.pattern)
		if want := fresh(change.pattern); got != want {
			t.Errorf("%s: the held answer %v differs from a fresh walk's %v", change.name, got, want)
		}
	}
	if ask("**/*.go") {
		t.Error("a match under a nested project's root counted for the outer project")
	}
}
