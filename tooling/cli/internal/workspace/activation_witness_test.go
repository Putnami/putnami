package workspace

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/fileindex"
)

// activationTree writes a project with Go sources, an environment config, a
// nested project and an install tree, and waits past the file index's margin.
func activationTree(t *testing.T) (root, project string) {
	t.Helper()
	root = t.TempDir()
	project = filepath.Join(root, "lib")
	files := map[string]string{
		"lib/a.go":                     "package lib\n",
		"lib/b/b.go":                   "package b\n",
		"lib/nested/putnami.json":      "{}\n",
		"lib/nested/doc/n.md":          "# n\n",
		"lib/node_modules/x/README.md": "# x\n",
		"lib/conf/.env.local.yaml":     "a: 1\n",
		"dist/README.md":               "# dist\n",
	}
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(fileindex.Margin + 100*time.Millisecond)
	return root, project
}

func TestAnActivationWalkReadsTheProjectsOwnTree(t *testing.T) {
	t.Parallel()
	root, project := activationTree(t)
	roots := map[string]bool{filepath.Join(project, "nested"): true}
	// A nested project and an install tree are not the project's own files.
	if HasActivationFilesProbed(project, []string{"**/*.md"}, roots, nil) {
		t.Error("a document outside the project's own tree matched")
	}
	if !HasActivationFilesProbed(project, []string{"conf/.env.yaml", "conf/.env.*.yaml"}, roots, nil) {
		t.Error("an environment config was not found")
	}
	if HasActivationFilesProbed(filepath.Join(root, "missing"), []string{"conf/.env.yaml"}, roots, nil) {
		t.Error("a missing project matched")
	}
	// A project rooted at a directory the walk skips below its root is still
	// walked.
	if !HasActivationFilesProbed(filepath.Join(root, "dist"), []string{"**/*.md"}, nil, nil) {
		t.Error("a project named dist was not walked")
	}
}

// answer probes the gate "a Markdown file lies in the project" and keeps it.
func answer(root, project string) bool {
	probe := NewActivationProbe(root)
	roots := map[string]bool{filepath.Join(project, "nested"): true}
	active := HasActivationFilesProbed(project, []string{"**/*.md"}, roots, probe)
	RecordActivation(root, "lib\x00docs", active, probe)
	return active
}

func TestAnActivationAnswerIsKeptWhileItsWitnessesHold(t *testing.T) {
	root, project := activationTree(t)
	t.Cleanup(forgetActivationWitnesses)
	if answer(root, project) {
		t.Fatal("a document outside the project's own tree matched")
	}
	SaveActivationWitnesses(root, SnapshotWritePolicy{})
	if _, err := os.Stat(filepath.Join(root, ".putnami", ActivationWitnessFilename)); err != nil {
		t.Fatalf("no witness file: %v", err)
	}

	// The next run reads the answer from the file.
	forgetActivationWitnesses()
	if active, ok := HeldActivation(root, "lib\x00docs"); !ok || active {
		t.Fatalf("held = %v, %v; want the kept answer", active, ok)
	}
	// An unchanged run whose answers all held writes nothing.
	before, _ := os.Stat(filepath.Join(root, ".putnami", ActivationWitnessFilename))
	SaveActivationWitnesses(root, SnapshotWritePolicy{})
	after, _ := os.Stat(filepath.Join(root, ".putnami", ActivationWitnessFilename))
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("an unchanged run rewrote the witness file")
	}

	// A document joins a directory the walk listed: the answer is no longer
	// held.
	if err := os.WriteFile(filepath.Join(project, "b", "README.md"), []byte("# b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := HeldActivation(root, "lib\x00docs"); ok {
		t.Error("an answer outlived the directory it listed")
	}
}

func TestAnActivationAnswerFallsWhenADirectoryItListedChanges(t *testing.T) {
	root, project := activationTree(t)
	t.Cleanup(forgetActivationWitnesses)
	probe := NewActivationProbe(root)
	if HasActivationFilesProbed(project, []string{"infra/requirements.json"}, nil, probe) {
		t.Fatal("an absent file matched")
	}
	RecordActivation(root, "lib\x00infra", false, probe)
	if _, ok := HeldActivation(root, "lib\x00infra"); !ok {
		t.Fatal("the absent answer was not kept")
	}
	if err := os.MkdirAll(filepath.Join(project, "infra"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := HeldActivation(root, "lib\x00infra"); ok {
		t.Error("an absent answer outlived the directory that would hold the file")
	}
}

func TestAFreshAnswerIsNotKept(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(forgetActivationWitnesses)
	project := filepath.Join(root, "lib")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "README.md"), []byte("# lib\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Written within the file index's margin: a stat could repeat.
	answer(root, project)
	if _, ok := HeldActivation(root, "lib\x00docs"); ok {
		t.Error("an answer read within the margin was kept")
	}
}

func TestAPreviewKeepsNoActivationWitness(t *testing.T) {
	root, project := activationTree(t)
	t.Cleanup(forgetActivationWitnesses)
	answer(root, project)
	SaveActivationWitnesses(root, SnapshotWritePolicy{DryRun: true})
	SaveActivationWitnesses(root, SnapshotWritePolicy{Plan: true})
	if _, err := os.Stat(filepath.Join(root, ".putnami", ActivationWitnessFilename)); !os.IsNotExist(err) {
		t.Errorf("a preview wrote the witness file: %v", err)
	}
}
