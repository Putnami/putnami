package main

import (
	"bytes"
	"maps"
	"slices"
	"strings"
	"testing"
)

// TestTheManifestDeclaresChangeScreenshots pins the launch of `change
// screenshots`: an interactive subcommand with every flag the command reads,
// and an uncached task that starts in the workspace root.
func TestTheManifestDeclaresChangeScreenshots(t *testing.T) {
	t.Parallel()
	manifest := readManifest(t)
	subcommand, declared := manifest.CommandGroups["change"].Subcommands["screenshots"]
	if !declared || subcommand.Command != "change-screenshots" || !subcommand.Interactive {
		t.Fatalf("change screenshots is %+v, want an interactive change-screenshots", subcommand)
	}
	flags := map[string]string{}
	for name, flag := range manifest.Commands["change-screenshots"].Flags {
		flags[name] = flag.Type
	}
	want := map[string]string{"project": "string", "base": "string", "base-url": "string", "head-url": "string",
		"route": "array", "fixtures": "string", "storage-state": "string", "out": "string", "browser": "string"}
	if !maps.Equal(flags, want) {
		t.Errorf("change-screenshots flags %v, want %v", flags, want)
	}
	task := manifest.Tasks["change-screenshots"]
	if task.Command != "{extensionRuntime}" || !slices.Equal(task.Args, []string{"screenshots"}) || task.Cwd != "{workspaceRoot}" {
		t.Errorf("change-screenshots task %+v", task)
	}
}

// TestScreenshotsDispatchesItsUsageErrors runs `screenshots` without its
// required flags: a usage error, exit 2.
func TestScreenshotsDispatchesItsUsageErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := run([]string{"screenshots", "--project", "web"}, &stdout, &stderr); code != 2 ||
		!strings.Contains(stderr.String(), "change screenshots: --base, --head-url required") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}
