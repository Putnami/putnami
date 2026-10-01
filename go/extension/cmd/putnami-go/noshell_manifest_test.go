package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/go/extension/internal/jobs/cachepolicy"
	"go.putnami.dev/sdk/extension/cli"
)

// contributorOnlyCommands are the manifest commands that run a file from the
// extension sources. runtime.prepare builds the runtime from those sources, so
// it runs in the repository that develops the extension, never in a consumer
// workspace that installed a published runtime.
var contributorOnlyCommands = map[string]string{
	"runtime.prepare": "{extensionRoot}/bin/prepare",
}

// shellCommands are programs a manifest command must not name: a task that
// starts one depends on a shell the host may not have (Windows has none of
// them), which is what the lifecycle jobs moved into the runtime to avoid.
var shellCommands = map[string]bool{
	"sh": true, "bash": true, "dash": true, "zsh": true, "ksh": true,
	"cmd": true, "powershell": true, "pwsh": true, "env": true,
}

// manifestCommand is one "command" value of the manifest and where it is.
type manifestCommand struct {
	path  string
	value string
}

// manifestCommands returns every string "command" member of the manifest, at
// any depth, with its dotted path.
func manifestCommands(t *testing.T) []manifestCommand {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	var found []manifestCommand
	var walk func(value any, path string)
	walk = func(value any, path string) {
		switch node := value.(type) {
		case map[string]any:
			if command, ok := node["command"].(string); ok {
				found = append(found, manifestCommand{path: strings.TrimPrefix(path, "."), value: command})
			}
			for key, child := range node {
				walk(child, path+"."+key)
			}
		case []any:
			for _, child := range node {
				walk(child, path+"[]")
			}
		}
	}
	walk(document, "")
	sort.Slice(found, func(i, j int) bool { return found[i].path < found[j].path })
	return found
}

// TestConsumerTasksStartNoShell is the manifest half of the promise that
// `putnami install` and `putnami upgrade` start no shell in a consumer
// workspace (the job packages hold the other half): apart from the
// contributor-only runtime build, no command the manifest declares runs a
// file from the extension's bin directory or names a shell, and every task
// that runs the runtime names a subcommand the runtime dispatches.
func TestConsumerTasksStartNoShell(t *testing.T) {
	commands := manifestCommands(t)
	if len(commands) < 20 {
		t.Fatalf("found %d manifest commands; the walk no longer reaches the tasks", len(commands))
	}
	for _, command := range commands {
		if want, ok := contributorOnlyCommands[command.path]; ok {
			if command.value != want {
				t.Errorf("%s runs %q, want %q", command.path, command.value, want)
			}
			continue
		}
		if strings.HasPrefix(command.value, "{extensionRoot}/") {
			t.Errorf("%s runs %q, a file of the extension sources; consumer tasks run {extensionRuntime}", command.path, command.value)
		}
		program := strings.TrimSuffix(filepath.Base(filepath.FromSlash(command.value)), ".exe")
		if shellCommands[program] || strings.HasSuffix(program, ".sh") {
			t.Errorf("%s runs the shell %q", command.path, command.value)
		}
	}

	var registered map[string]cli.JobFunc
	if err := runEntrypoint(
		[]string{"build"},
		&bytes.Buffer{},
		func(string) string { return "" },
		func(got map[string]cli.JobFunc) { registered = got },
	); err != nil {
		t.Fatalf("runEntrypoint(build): %v", err)
	}
	// The reserved workspace-level commands runEntrypoint answers before job
	// dispatch.
	reserved := map[string]bool{cachepolicy.PhaseClean: true, cachepolicy.PhaseGC: true}
	manifest := loadExtensionManifest(t)
	for _, name := range sortedNames(manifest.Tasks) {
		task := manifest.Tasks[name]
		if task.Command != "{extensionRuntime}" {
			continue
		}
		if len(task.Args) == 0 || (registered[task.Args[0]] == nil && !reserved[task.Args[0]]) {
			t.Errorf("task %s runs the runtime with %v, which names no registered subcommand", name, task.Args)
		}
	}
	for _, name := range []string{"workspace-fetch-exec", "workspace-install-exec", "deps-upgrade-exec"} {
		if task := manifest.Tasks[name]; task.Command != "{extensionRuntime}" {
			t.Errorf("task %s runs %q, want the runtime", name, task.Command)
		}
	}
}

// The scripts the lifecycle jobs replaced are gone from the extension, so
// nothing can start them again: bin/ holds the contributor-only runtime
// build and nothing else.
func TestTheExtensionShipsNoJobScripts(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("..", "..", "bin"))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if len(names) != 1 || names[0] != "prepare" {
		t.Fatalf("bin/ holds %v, want only prepare", names)
	}
}
