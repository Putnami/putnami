package jobs

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
)

// interactiveTaskMarker is the sentence a manifest gives a task that a CLI
// subcommand runs on the terminal's own stdout: the CLI captures nothing, so a
// cache entry would have nothing to replay.
const interactiveTaskMarker = "Interactive: the CLI runs it with the terminal's own stdout"

// uncachedByVerdict names, by repository-relative manifest path and task name,
// the tasks whose verdict is about the commit or the invocation itself, so a
// stored verdict from another one would be wrong. A task of the same name in
// another manifest is not exempt.
var uncachedByVerdict = map[string]string{
	"typescript/extension/putnami.extension.json:version-report":      "its output is the commit's identity: sha, branch and dirty bit",
	"tooling/sdd-extension/putnami.extension.json:sdd-selfcheck-exec": "its verdict is the environment of this invocation",
}

// uncachedTaskExemption returns the uncachedByVerdict key of a task whose
// cache is off, and whether the task has an effect a cache entry cannot
// replay: a declared effect; the terminal of an interactive subcommand, which
// the marker names and which leaves the task no output port and no declared
// output to capture; or a verdict about the commit or the invocation itself.
func uncachedTaskExemption(manifestPath, name string, task extension.TaskDefinition) (string, bool) {
	key := filepath.ToSlash(manifestPath) + ":" + name
	switch {
	case task.Declares != nil && len(task.Declares.Effects) > 0:
		return key, true
	case strings.Contains(task.Description, interactiveTaskMarker) && len(task.Outputs) == 0 &&
		(task.Declares == nil || len(task.Declares.Outputs) == 0):
		return key, true
	default:
		return key, uncachedByVerdict[key] != ""
	}
}

// The exemptions rest on structured facts, not on a sentence or a bare name
// alone: the marker does not exempt a task that has an output to capture, and
// a verdict exemption does not follow its task name into another manifest.
func TestUncachedTaskExemptionsRestOnStructuredFacts(t *testing.T) {
	t.Parallel()
	interactive := extension.TaskDefinition{Description: "Lists features. " + interactiveTaskMarker + "."}
	withPort := interactive
	withPort.Outputs = map[string]extension.TaskOutputPort{"data": {}}
	withDeclaredOutput := interactive
	withDeclaredOutput.Declares = &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
		"report": {Kind: extension.OutputKindFile, Root: extension.OutputRootProject, Path: "report.json"},
	}}
	effect := extension.TaskDefinition{Declares: &extension.TaskDeclaration{Effects: []string{extension.EffectNetwork}}}
	const sdd, ts = "tooling/sdd-extension/putnami.extension.json", "typescript/extension/putnami.extension.json"

	for _, test := range []struct {
		name, manifest, task string
		def                  extension.TaskDefinition
		want                 bool
	}{
		{"interactive", sdd, "sdd-specs-list-exec", interactive, true},
		{"the marker beside an output port", sdd, "sdd-specs-list-exec", withPort, false},
		{"the marker beside a declared output", sdd, "sdd-specs-list-exec", withDeclaredOutput, false},
		{"a declared effect", sdd, "publish", effect, true},
		{"a listed verdict", ts, "version-report", extension.TaskDefinition{}, true},
		{"a listed name in another manifest", sdd, "version-report", extension.TaskDefinition{}, false},
		{"nothing", ts, "lint", extension.TaskDefinition{}, false},
	} {
		if _, got := uncachedTaskExemption(test.manifest, test.task, test.def); got != test.want {
			t.Errorf("%s: exempt = %t, want %t", test.name, got, test.want)
		}
	}
}

// Every task of this repository's extension manifests is cached unless its run
// has an effect a cache entry cannot replay (#82): a declared effect, the
// terminal of an interactive subcommand, or a verdict about the commit or the
// invocation itself. A task that turns its cache off for any other reason
// re-executes on every lane and every run, whatever its inputs.
func TestEveryUncachedTaskHasAnEffectACacheEntryCannotReplay(t *testing.T) {
	t.Parallel()
	root := findJobsRepoRoot(t)

	var manifests []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" || name == "dist" || name == "out" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() == "putnami.extension.json" {
			manifests = append(manifests, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(manifests) == 0 {
		t.Fatalf("no extension manifest under %s", root)
	}

	var offenders []string
	uncached := 0
	listed := make(map[string]bool, len(uncachedByVerdict))
	for _, path := range manifests {
		manifest, err := extension.LoadManifest(path)
		if err != nil {
			t.Fatalf("LoadManifest(%s): %v", path, err)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatalf("relative path of %s: %v", path, err)
		}
		for name, task := range manifest.Tasks {
			if task.Cache.IsEnabled() {
				continue
			}
			uncached++
			key, exempt := uncachedTaskExemption(relative, name, task)
			listed[key] = true
			if !exempt {
				offenders = append(offenders, key)
			}
		}
	}
	if uncached == 0 {
		t.Fatalf("no uncached task in %d manifests; the walk reads nothing", len(manifests))
	}
	sort.Strings(offenders)
	for _, offender := range offenders {
		t.Errorf("%s turns its cache off with no effect a cache entry cannot replay: give it a key, or declare the effect", offender)
	}
	// An exemption names an uncached task that exists, so it cannot outlive
	// its task and later cover another one by name.
	for key := range uncachedByVerdict {
		if !listed[key] {
			t.Errorf("uncachedByVerdict lists %s, which is no uncached task of this repository: remove the entry", key)
		}
	}
}
