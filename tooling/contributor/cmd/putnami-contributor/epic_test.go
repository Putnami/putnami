package main

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestTheManifestDeclaresTheEpicCommands pins the launch of epic plan and
// epic status: interactive subcommands that need a workspace, the same two
// flags, and uncached tasks that start in the workspace root.
func TestTheManifestDeclaresTheEpicCommands(t *testing.T) {
	t.Parallel()
	manifest := readManifest(t)
	for _, name := range []string{"plan", "status"} {
		command := "epic-" + name
		subcommand, declared := manifest.CommandGroups["epic"].Subcommands[name]
		if !declared || subcommand.Command != command || !subcommand.Interactive || subcommand.RunsWithoutWorkspace() {
			t.Fatalf("epic %s is %+v, want an interactive %s that needs a workspace", name, subcommand, command)
		}
		flags := map[string]string{}
		for flag, declared := range manifest.Commands[command].Flags {
			flags[flag] = declared.Type
		}
		if want := map[string]string{"epic": "string", "plan-file": "string"}; !maps.Equal(flags, want) {
			t.Errorf("%s flags %v, want %v", command, flags, want)
		}
		task := manifest.Tasks[command]
		if task.Command != "{extensionRuntime}" || !slices.Equal(task.Args, []string{command}) || task.Cwd != "{workspaceRoot}" {
			t.Errorf("%s runs %s %v in %q, want {extensionRuntime} %s in {workspaceRoot}", command, task.Command, task.Args, task.Cwd, command)
		}
		if task.Cache == nil || task.Cache.IsEnabled() {
			t.Errorf("%s is cacheable; it reads files and providers no cache key covers", command)
		}
	}
}

// TestEpicCommandsRunInTheWorkspaceRoot pins the dispatch of epic-plan and
// epic-status: both read .putnami/<epic>/epic.json under the working
// directory putnami launches them in, and epic-status run without the host
// channel names the launch that opens it.
func TestEpicCommandsRunInTheWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	plan, err := os.ReadFile(filepath.Join("..", "..", "internal", "estimate", "testdata", "epic.json"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".putnami", "40")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "epic.json"), plan, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"epic-plan", "--epic", "40"}, &stdout, &stderr); code != 0 ||
		!strings.HasPrefix(stdout.String(), "Epic "+filepath.Join(".putnami", "40", "epic.json")+": ") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"epic-plan"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "--epic <id>") {
		t.Fatalf("exit %d, stderr %q; want a usage error", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"epic-status", "--epic", "40"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "putnami epic status") {
		t.Fatalf("exit %d, stdout %q, stderr %q; want the remedy that names putnami epic status", code, stdout.String(), stderr.String())
	}
}
