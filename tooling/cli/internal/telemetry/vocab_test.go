package telemetry

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/telemetry/cliusage"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// firstJobCommands are the eleven job commands the CLI catalog listed when the
// command vocabulary first shipped, in catalog order. They head the vocabulary
// whatever the catalog lists today, because a released CLI sends them.
var firstJobCommands = []string{
	"build", "test", "lint", "serve", "run", "format",
	"package", "publish", "deploy", "generate", "validate",
}

// The shared command vocabulary names every public command that a manifest of
// FirstPartyScope in this repository declares: a public command added to such
// a manifest fails here until the wire contract, the receiver and the
// Legitimate Interests Assessment change with it. A name that no manifest
// declares any more stays, because a released CLI sends it; the protocol test
// of cliusage keeps it there.
func TestCommandVocabularyMatchesFirstPartyCommands(t *testing.T) {
	t.Parallel()
	head := len(firstJobCommands)
	if len(cliusage.Commands) < head || !slices.Equal(cliusage.Commands[:head], firstJobCommands) {
		t.Fatalf("the vocabulary must start with %v", firstJobCommands)
	}
	if tail := cliusage.Commands[head:]; !slices.IsSorted(tail) {
		t.Errorf("the names after the first %d are not sorted: %v", head, tail)
	}

	var missing []string
	for _, ext := range firstPartyExtensions(t) {
		for name := range ext.Commands {
			if ext.CommandVisibility[name] == "internal" || slices.Contains(cliusage.Commands, name) || slices.Contains(missing, name) {
				continue
			}
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		t.Errorf("public first-party commands missing from the telemetry command vocabulary: %v", missing)
	}
}

// firstPartyExtensions loads every extension manifest of this repository whose
// extension is in FirstPartyScope, the way discovery names it. The walk skips
// every dot-directory, node_modules and dist, like the first-party manifest walk
// in internal/extension, so another checkout under .claude/worktrees is never
// read.
func firstPartyExtensions(t *testing.T) []*extension.ExtensionDescription {
	t.Helper()
	root := repositoryRoot(t)
	var found []*extension.ExtensionDescription
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			name := entry.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "dist") {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Name() != "putnami.extension.json" {
			return nil
		}
		ext := extension.LoadExtensionFromDir(filepath.Dir(path), "")
		if ext != nil && strings.HasPrefix(ext.Name, FirstPartyScope) {
			found = append(found, ext)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the repository for extension manifests: %v", err)
	}
	for _, name := range []string{"@putnami/go", "@putnami/typescript", "@putnami/cloud", "@putnami/clientgen", "@putnami/sdd"} {
		if !slices.ContainsFunc(found, func(ext *extension.ExtensionDescription) bool { return ext.Name == name }) {
			t.Fatalf("the manifest walk did not load %s; the vocabulary check would pass vacuously", name)
		}
	}
	return found
}

// repositoryRoot is the nearest ancestor of the test's directory that holds
// putnami.workspace.json.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "putnami.workspace.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no putnami.workspace.json above the test directory")
		}
		dir = parent
	}
}
