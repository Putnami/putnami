package scheduler

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
)

// uncachedByVerdict names, by repository-relative manifest path and task name,
// the tasks whose result is about the commit or the invocation itself, so a
// stored one from another commit or invocation would be wrong. A task of the
// same name in another manifest is not exempt.
var uncachedByVerdict = map[string]string{
	"tooling/sdd-extension/putnami.extension.json:sdd-selfcheck-exec":                "its verdict is the environment of this invocation",
	"tooling/sdd-extension/putnami.extension.json:breaking-marker-validate":          "its verdict reads the proposed squash message of this invocation and the commits since its baseline",
	"tooling/sdd-extension/putnami.extension.json:agent-attribution-validate":        "its verdict reads the proposed squash message of this invocation",
	"tooling/sdd-extension/putnami.extension.json:feature-coverage-ratchet-validate": "its verdict reads the debt list at the merge base of this invocation's baseline",
	"cloud/extension/putnami.extension.json:cloud-package-config-member":             "its member embeds the HEAD commit, the origin repository and the linked workspace",
	"cloud/extension/putnami.extension.json:cloud-package-site-content":              "its bundle embeds the HEAD commit",
}

// interactiveTasks returns the tasks of a manifest that run only as
// interactive subcommands: every command that runs the task is the target of a
// subcommand marked interactive, and of no other. The CLI gives such a run the
// terminal's own stdout and captures nothing, so a cache entry would have
// nothing to replay.
func interactiveTasks(manifest *extension.Manifest) map[string]bool {
	interactive, captured := map[string]bool{}, map[string]bool{}
	var visit func(sub extension.SubcommandDefinition, target string)
	visit = func(sub extension.SubcommandDefinition, target string) {
		if sub.Command != "" {
			target = sub.Command
		}
		if target != "" {
			if sub.Interactive {
				interactive[target] = true
			} else {
				captured[target] = true
			}
		}
		for _, nested := range sub.Subcommands {
			// The CLI reads Interactive on the top-level subcommand it
			// dispatches; a nested one inherits it.
			nested.Interactive = sub.Interactive
			visit(nested, target)
		}
	}
	for _, group := range manifest.CommandGroups {
		for _, sub := range group.Subcommands {
			visit(sub, "")
		}
	}
	tasks, others := map[string]bool{}, map[string]bool{}
	for name, command := range manifest.Commands {
		for _, step := range command.Run {
			if interactive[name] && !captured[name] {
				tasks[step.Task] = true
			} else {
				others[step.Task] = true
			}
		}
	}
	for task := range others {
		delete(tasks, task)
	}
	return tasks
}

// uncachedTaskExemption returns the key of a task whose cache is off, and
// whether the task has an effect a cache entry cannot replay: a declared
// effect; the terminal of an interactive subcommand, which leaves the task no
// output port and no declared output to capture; or a result about the commit
// or the invocation itself.
func uncachedTaskExemption(manifestPath, name string, task extension.TaskDefinition, interactive bool) (string, bool) {
	key := filepath.ToSlash(manifestPath) + ":" + name
	switch {
	case task.Declares != nil && len(task.Declares.Effects) > 0:
		return key, true
	case interactive && len(task.Outputs) == 0 &&
		(task.Declares == nil || len(task.Declares.Outputs) == 0):
		return key, true
	default:
		return key, uncachedByVerdict[key] != ""
	}
}

// The exemptions rest on structured facts, not on a description or a bare name:
// an interactive task that has an output to capture is not exempt, a task that
// a captured command also runs is not interactive, and a verdict exemption does
// not follow its task name into another manifest.
func TestUncachedTaskExemptionsRestOnStructuredFacts(t *testing.T) {
	t.Parallel()
	bare := extension.TaskDefinition{}
	withPort := extension.TaskDefinition{Outputs: map[string]extension.TaskOutputPort{"data": {}}}
	withDeclaredOutput := extension.TaskDefinition{Declares: &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
		"report": {Kind: extension.OutputKindFile, Root: extension.OutputRootProject, Path: "report.json"},
	}}}
	effect := extension.TaskDefinition{Declares: &extension.TaskDeclaration{Effects: []string{extension.EffectNetwork}}}
	const sdd, ts = "tooling/sdd-extension/putnami.extension.json", "typescript/extension/putnami.extension.json"

	for _, test := range []struct {
		name, manifest, task string
		def                  extension.TaskDefinition
		interactive, want    bool
	}{
		{"interactive", sdd, "sdd-specs-list-exec", bare, true, true},
		{"interactive with an output port", sdd, "sdd-specs-list-exec", withPort, true, false},
		{"interactive with a declared output", sdd, "sdd-specs-list-exec", withDeclaredOutput, true, false},
		{"a declared effect", sdd, "publish", effect, false, true},
		{"a listed verdict", sdd, "sdd-selfcheck-exec", bare, false, true},
		{"a listed name in another manifest", ts, "sdd-selfcheck-exec", bare, false, false},
		{"nothing", ts, "lint", bare, false, false},
	} {
		if _, got := uncachedTaskExemption(test.manifest, test.task, test.def, test.interactive); got != test.want {
			t.Errorf("%s: exempt = %t, want %t", test.name, got, test.want)
		}
	}

	step := func(task string) extension.CommandDefinition {
		return extension.CommandDefinition{Run: []extension.PipelineStep{{ID: "run", Task: task}}}
	}
	manifest := &extension.Manifest{
		Commands: map[string]extension.CommandDefinition{
			"x-login": step("login"), "x-list": step("list"), "validate": step("list"), "x-check": step("check"),
		},
		CommandGroups: map[string]extension.CommandGroupDefinition{"x": {Subcommands: map[string]extension.SubcommandDefinition{
			"login": {Command: "x-login", Interactive: true},
			"list":  {Command: "x-list", Interactive: true},
			"check": {Command: "x-check"},
		}}},
	}
	got := interactiveTasks(manifest)
	if !got["login"] || got["list"] || got["check"] || len(got) != 1 {
		t.Errorf("interactiveTasks = %v, want login alone: validate also runs list, and check is captured", got)
	}
}

// Every task of this repository's extension manifests is cached unless its run
// has an effect a cache entry cannot replay: a declared effect, the
// terminal of an interactive subcommand, or a result about the commit or the
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
		interactive := interactiveTasks(manifest)
		for name, task := range manifest.Tasks {
			if task.Cache.IsEnabled() {
				continue
			}
			uncached++
			key, exempt := uncachedTaskExemption(relative, name, task, interactive[name])
			if !exempt {
				offenders = append(offenders, key)
			} else if uncachedByVerdict[key] != "" {
				listed[key] = true
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
