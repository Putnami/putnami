package main

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	wire "go.putnami.dev/protocol/extension/hostrpc"
	"go.putnami.dev/protocol/features/spectest"
)

// versionFeature names the feature the version commands prove.
const versionFeature = "tooling/version-commands"

// versionCommands are the manifest's commands and tasks, sorted: one for each
// subcommand of the version group.
var versionCommands = []string{"version-get", "version-tag"}

// TestTheManifestDeclaresTheVersionGroup pins how putnami reaches the version
// commands: the version group defaults to get, both subcommands are
// interactive, so putnami opens the host channel for them, and each runs the
// executable with its own arguments in the workspace root, uncached. tag
// declares dry-run, the global flag putnami forwards only to a command that
// declares it.
func TestTheManifestDeclaresTheVersionGroup(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, versionFeature, "version-group-runs-the-executable", "the-manifest-declares-the-version-group")
	manifest := readManifest(t)
	if manifest.Runtime == nil || manifest.Runtime.Executable != "compiled/putnami-dev" || manifest.Runtime.Prepare == nil {
		t.Fatalf("runtime %+v, want compiled/putnami-dev with a prepare step", manifest.Runtime)
	}
	group, declared := manifest.CommandGroups["version"]
	if !declared || group.Default != "get" {
		t.Fatalf("version group %+v, want one whose default is get", group)
	}
	if got := slices.Sorted(maps.Keys(group.Subcommands)); !slices.Equal(got, []string{"get", "tag"}) {
		t.Fatalf("version subcommands %v, want get and tag", got)
	}
	for sub, command := range map[string]string{"get": "version-get", "tag": "version-tag"} {
		subcommand := group.Subcommands[sub]
		if subcommand.Command != command || !subcommand.Interactive || subcommand.RunsWithoutWorkspace() {
			t.Errorf("version %s = %+v, want the interactive command %s that needs a workspace", sub, subcommand, command)
		}
		definition := manifest.Commands[command]
		if len(definition.Run) != 1 || definition.Run[0].Task != command {
			t.Errorf("command %s runs %+v, want the task %s", command, definition.Run, command)
		}
		if !slices.Equal(definition.ActivationFiles, []string{"putnami.extension.json"}) {
			t.Errorf("command %s activation files %v, want the manifest", command, definition.ActivationFiles)
		}
		task := manifest.Tasks[command]
		if task.Kind != "command" || task.Command != "{extensionRuntime}" || task.Cwd != "{workspaceRoot}" ||
			!slices.Equal(task.Args, []string{"version", sub}) {
			t.Errorf("task %s = %+v, want the runtime with version %s in the workspace root", command, task, sub)
		}
		if task.Cache == nil || task.Cache.IsEnabled() {
			t.Errorf("task %s caches its result", command)
		}
	}
	if _, ok := manifest.Commands["version-tag"].Flags["dry-run"]; !ok {
		t.Error("version-tag does not declare dry-run, so putnami would not forward --dry-run")
	}
	for _, name := range []string{"scope", "push", "yes"} {
		if _, ok := manifest.Commands["version-tag"].Flags[name]; !ok {
			t.Errorf("version-tag does not declare %s", name)
		}
	}
	if _, ok := manifest.Commands["version-get"].Flags["scope"]; !ok {
		t.Error("version-get does not declare scope")
	}
}

// TestRunDispatchesTheVersionCommands pins that the executable serves the
// version commands without the job context argument putnami appends: an
// argument error is the command's own usage error, and a launch without a
// host channel names the interactive launch that opens one.
func TestRunDispatchesTheVersionCommands(t *testing.T) {
	t.Setenv(wire.SocketEnv, "")
	t.Setenv(wire.TokenEnv, "")
	spectest.Proves(t, versionFeature, "version-group-runs-the-executable", "the-executable-dispatches-the-version-commands")
	spectest.Proves(t, versionFeature, "usage-and-host-errors", "no-channel-names-the-interactive-launch")
	context := writeJobContext(t, `{}`)
	for _, tc := range []struct {
		args       []string
		code       int
		stderrPart string
	}{
		{[]string{"version", "get", "extra", "--putnamiContext", context}, 2, "usage: putnami version get [--scope <line>]"},
		{[]string{"version", "tag", "a", "b", "--putnamiContext", context}, 2, "usage: putnami version tag"},
		{[]string{"version", "get", "--putnamiContext", context}, 1, "run `putnami version get` (a hosted run opens no channel; " + wire.SocketEnv + " and " + wire.TokenEnv + " not set)"},
		{[]string{"version", "tag", "--scope", "tooling", "--putnamiContext", context}, 1, "run `putnami version tag`"},
		{[]string{"version", "get"}, 1, "this command needs an interactive launch by putnami"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(tc.args, &stdout, &stderr); code != tc.code {
			t.Errorf("%v: exit %d, want %d; stderr %q", tc.args, code, tc.code, stderr.String())
		}
		if !strings.Contains(stderr.String(), tc.stderrPart) {
			t.Errorf("%v: stderr %q, want it to name %q", tc.args, stderr.String(), tc.stderrPart)
		}
		if stdout.Len() != 0 {
			t.Errorf("%v: stdout %q, want nothing", tc.args, stdout.String())
		}
	}
}

// TestRunReadsTheOutputModeAndDryRunFromTheJobContext pins that the output
// mode and --dry-run, which putnami consumes and forwards in the job context,
// reach the command: tag refuses structured output before it reads anything.
func TestRunReadsTheOutputModeAndDryRunFromTheJobContext(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, versionFeature, "release-refusals", "structured-output-is-refused")
	for _, mode := range []string{"json", "jsonl"} {
		var stdout, stderr bytes.Buffer
		context := writeJobContext(t, `{"output":"`+mode+`","dry-run":true}`)
		if code := run([]string{"version", "tag", "--scope", "tooling", "--putnamiContext", context}, &stdout, &stderr); code != 2 {
			t.Errorf("%s: exit %d, want 2", mode, code)
		}
		if want := "--output=" + mode + " is not supported by \"version tag\"\n"; stderr.String() != want {
			t.Errorf("%s: stderr %q, want %q", mode, stderr.String(), want)
		}
		if stdout.Len() != 0 {
			t.Errorf("%s: stdout %q, want nothing", mode, stdout.String())
		}
	}
}

// TestRunReportsAJobContextItCannotRead pins that a launch whose job context
// does not parse fails and names the file's error.
func TestRunReportsAJobContextItCannotRead(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	missing := filepath.Join(t.TempDir(), "absent.json")
	if code := run([]string{"version", "get", "--putnamiContext", missing}, &stdout, &stderr); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.HasPrefix(stderr.String(), "version get: ") || !strings.Contains(stderr.String(), "absent.json") {
		t.Errorf("stderr %q, want the context file's error", stderr.String())
	}
}

// writeJobContext writes a job context whose params are params, and returns
// its path.
func writeJobContext(t *testing.T, params string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "context.json")
	if err := os.WriteFile(path, []byte(`{"params":`+params+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
