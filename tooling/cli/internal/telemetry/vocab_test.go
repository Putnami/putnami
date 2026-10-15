package telemetry

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/telemetry/cliusage"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// The shared command vocabulary is exactly what Putnami ships: the public job
// commands of the catalog in catalog order, then every public command that a
// manifest of FirstPartyScope in this repository declares, sorted. A public
// command added to such a manifest fails here until the wire contract, the
// receiver and the Legitimate Interests Assessment change with it.
func TestCommandVocabularyMatchesFirstPartyCommands(t *testing.T) {
	t.Parallel()
	var want []string
	for _, command := range commandmeta.PublicJobCommands() {
		want = append(want, command.Name)
	}
	var declared []string
	for _, ext := range firstPartyExtensions(t) {
		for name := range ext.Commands {
			if ext.CommandVisibility[name] == "internal" || slices.Contains(want, name) || slices.Contains(declared, name) {
				continue
			}
			declared = append(declared, name)
		}
	}
	slices.Sort(declared)
	want = append(want, declared...)

	if !slices.Equal(cliusage.Commands, want) {
		var missing, extra []string
		for _, name := range want {
			if !slices.Contains(cliusage.Commands, name) {
				missing = append(missing, name)
			}
		}
		for _, name := range cliusage.Commands {
			if !slices.Contains(want, name) {
				extra = append(extra, name)
			}
		}
		t.Fatalf("telemetry command vocabulary differs from the first-party commands\nmissing: %v\nextra:   %v\nwant order: %v",
			missing, extra, want)
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
