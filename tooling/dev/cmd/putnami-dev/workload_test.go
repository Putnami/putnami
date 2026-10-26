package main

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	extension "go.putnami.dev/protocol/extension"
	wire "go.putnami.dev/protocol/extension/hostrpc"
	"go.putnami.dev/protocol/features/spectest"
)

// workloadFeature names the feature compose and qualify prove.
const workloadFeature = "tooling/workload-qualification"

// workloadCommands are the manifest's compose and qualify commands and tasks,
// sorted: one for each group's only subcommand.
var workloadCommands = []string{"compose-up", "qualify-run"}

// workloadGroup is what the manifest declares for one of the compose and
// qualify groups.
type workloadGroup struct {
	group, sub, command string
	args                []string
	workload            string
	flags               map[string]string
}

var workloadGroups = []workloadGroup{
	{
		group: "compose", sub: "up", command: "compose-up", args: []string{"compose", "up"},
		workload: extension.WorkloadServe,
		flags:    map[string]string{"port": "number", "watch": "boolean", "ready-timeout": "string"},
	},
	{
		group: "qualify", sub: "run", command: "qualify-run", args: []string{"qualify", "run"},
		workload: extension.WorkloadOnce,
		flags: map[string]string{
			"target": "string", "expect-sha": "string", "platform-prefix": "string",
			"ready-timeout": "string", "request-timeout": "string", "print-contract": "boolean",
		},
	},
}

// TestTheManifestDeclaresTheComposeAndQualifyGroups pins how putnami reaches
// compose and qualify: each group defaults to its only subcommand, which is
// interactive and takes the project as a required positional, so
// `putnami compose <project>` and `putnami qualify <project>` run it. Each
// runs the executable with its own arguments in the workspace root, uncached
// and without a deadline. compose-up declares the serve workload, which makes
// --watch its own flag and keeps it out of every portable or committed run;
// qualify-run declares the once workload.
func TestTheManifestDeclaresTheComposeAndQualifyGroups(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, workloadFeature, "the-dev-extension-runs-compose-and-qualify", "the-manifest-declares-the-compose-and-qualify-groups")
	manifest := readManifest(t)
	for _, want := range workloadGroups {
		group, declared := manifest.CommandGroups[want.group]
		if !declared || group.Default != want.sub {
			t.Fatalf("%s group %+v, want one whose default is %s", want.group, group, want.sub)
		}
		if got := slices.Sorted(maps.Keys(group.Subcommands)); !slices.Equal(got, []string{want.sub}) {
			t.Fatalf("%s subcommands %v, want only %s", want.group, got, want.sub)
		}
		subcommand := group.Subcommands[want.sub]
		if subcommand.Command != want.command || !subcommand.Interactive || subcommand.RunsWithoutWorkspace() {
			t.Errorf("%s %s = %+v, want the interactive command %s that needs a workspace", want.group, want.sub, subcommand, want.command)
		}
		if want := []extension.PositionalDefinition{{Name: "project", Required: true}}; !slices.Equal(subcommand.Positionals, want) {
			t.Errorf("%s positionals %+v, want %+v", subcommand.Command, subcommand.Positionals, want)
		}
		definition := manifest.Commands[want.command]
		if len(definition.Run) != 1 || definition.Run[0].Task != want.command {
			t.Errorf("command %s runs %+v, want the task %s", want.command, definition.Run, want.command)
		}
		if !slices.Equal(definition.ActivationFiles, []string{"putnami.extension.json"}) {
			t.Errorf("command %s activation files %v, want the manifest", want.command, definition.ActivationFiles)
		}
		if definition.Traits == nil || *definition.Traits != (extension.CommandTraits{Workload: want.workload}) {
			t.Errorf("command %s traits %+v, want only the %s workload", want.command, definition.Traits, want.workload)
		}
		flags := make(map[string]string, len(definition.Flags))
		for name, flag := range definition.Flags {
			flags[name] = flag.Type
		}
		if !maps.Equal(flags, want.flags) {
			t.Errorf("command %s flags %v, want %v", want.command, flags, want.flags)
		}
		task := manifest.Tasks[want.command]
		if task.Kind != "command" || task.Command != "{extensionRuntime}" || task.Cwd != "{workspaceRoot}" ||
			!slices.Equal(task.Args, want.args) {
			t.Errorf("task %s = %+v, want the runtime with %v in the workspace root", want.command, task, want.args)
		}
		if task.Cache == nil || task.Cache.IsEnabled() {
			t.Errorf("task %s caches its result", want.command)
		}
		if task.TimeoutMs != -1 {
			t.Errorf("task %s timeoutMs %v, want -1: the command runs until interrupted or its verdict", want.command, task.TimeoutMs)
		}
	}
	if watch := manifest.Commands["compose-up"].Flags["watch"]; watch.Default != true {
		t.Errorf("compose-up watch default %v, want true: the target watches unless --no-watch", watch.Default)
	}
}

// TestRunDispatchesTheComposeAndQualifyCommands pins that the executable
// serves `compose up` and `qualify run` from the job context putnami appends,
// and refuses a launch it cannot run before anything starts: one without a
// job context, one whose context carries no workspace membership, and one
// without the absolute putnami executable the members' serve runs start.
func TestRunDispatchesTheComposeAndQualifyCommands(t *testing.T) {
	t.Setenv(wire.SocketEnv, "")
	t.Setenv(wire.TokenEnv, "")
	spectest.Proves(t, workloadFeature, "the-dev-executable-reads-its-launch-from-the-job-context", "the-executable-dispatches-the-compose-and-qualify-commands")
	root := t.TempDir()
	member := `"workspaceRoot":` + quote(t, root) + `,"workspaceProjects":[{"id":"/app","name":"app","path":"app","type":"application"}]`
	noMembership := writeJobContext(t, `{}`)
	text := writeLaunchContext(t, `{`+member+`,"params":{}}`)
	structured := writeLaunchContext(t, `{`+member+`,"params":{"output":"json"}}`)
	executable := filepath.Join(root, "bin", "putnami")

	for _, tc := range []struct {
		name       string
		args       []string
		executable string
		code       int
		stderr     string
		envelope   bool
	}{
		{"compose without a job context", []string{"compose", "up", "app"}, executable, 1,
			"putnami: compose needs the job context of an interactive launch by putnami; run `putnami compose <project>`\n", false},
		{"qualify without a job context", []string{"qualify", "run", "app"}, executable, 1,
			"putnami: qualify needs the job context of an interactive launch by putnami; run `putnami qualify <project>`\n", false},
		{"compose without membership", []string{"compose", "up", "app", "--putnamiContext", noMembership}, executable, 1,
			"putnami: compose: the job context carries no workspace membership; run `putnami compose <project>` in a workspace\n", false},
		{"compose without the executable", []string{"compose", "up", "app", "--putnamiContext", text}, "", 1,
			"putnami: compose: PUTNAMI_CLI_EXECUTABLE names no absolute putnami executable to start the serve runs with; run `putnami compose <project>`\n", false},
		{"qualify with a relative executable", []string{"qualify", "run", "app", "--putnamiContext", structured}, "putnami", 1,
			"putnami: qualify: PUTNAMI_CLI_EXECUTABLE names no absolute putnami executable to start the serve runs with; run `putnami qualify <project>`\n", true},
		{"compose reaches its own usage", []string{"compose", "up", "--putnamiContext", text}, executable, 2,
			"putnami: compose takes exactly one project: putnami compose <project> [--port <n>] [--no-watch] [--ready-timeout <duration>]\n", false},
		{"qualify reaches its own usage", []string{"qualify", "run", "--putnamiContext", structured}, executable, 2,
			"putnami: qualify takes exactly one project: putnami qualify <project> --target <local|url>\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(cliExecutableEnv, tc.executable)
			var stdout, stderr bytes.Buffer
			if code := run(tc.args, &stdout, &stderr); code != tc.code {
				t.Errorf("exit %d, want %d; stderr %q", code, tc.code, stderr.String())
			}
			if stderr.String() != tc.stderr {
				t.Errorf("stderr %q, want %q", stderr.String(), tc.stderr)
			}
			if !tc.envelope {
				if stdout.Len() != 0 {
					t.Errorf("stdout %q, want nothing", stdout.String())
				}
				return
			}
			var envelope protocolcli.ResultV2
			if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
				t.Fatalf("stdout %q is not one result envelope: %v", stdout.String(), err)
			}
			if envelope.Status != protocolcli.StatusFailure || envelope.Command != tc.args[0] || envelope.Error == nil {
				t.Errorf("envelope %+v, want the failure of %s", envelope, tc.args[0])
			}
		})
	}
}

// writeLaunchContext writes a whole job context document and returns its
// path.
func writeLaunchContext(t *testing.T, document string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "context.json")
	if err := os.WriteFile(path, []byte(document), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// quote returns s as a JSON string.
func quote(t *testing.T, s string) string {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

// TestRunWorkloadReadsTheOutputFlagsFromTheJobContext pins that a compose or
// qualify launch takes --verbose (or --debug), --quiet and --no-color from the
// job context params putnami writes for them: putnami consumes the tokens as
// its own flags, so the context is the only place the command reads them.
// Each is off when the context does not carry it.
func TestRunWorkloadReadsTheOutputFlagsFromTheJobContext(t *testing.T) {
	t.Setenv(wire.SocketEnv, "")
	t.Setenv(wire.TokenEnv, "")
	spectest.Proves(t, workloadFeature, "the-dev-executable-reads-its-launch-from-the-job-context", "the-executable-reads-the-output-flags-from-the-job-context")
	root := t.TempDir()
	t.Setenv(cliExecutableEnv, filepath.Join(root, "bin", "putnami"))
	member := `"workspaceRoot":` + quote(t, root) + `,"workspaceProjects":[{"id":"/app","name":"app","path":"app","type":"application"}]`
	for _, tc := range []struct {
		name                    string
		params                  string
		verbose, quiet, noColor bool
	}{
		{name: "none typed", params: `{}`},
		{name: "verbose", params: `{"verbose":true}`, verbose: true},
		{name: "quiet without color", params: `{"quiet":true,"color":false}`, quiet: true, noColor: true},
		{name: "color", params: `{"color":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contextFile := writeLaunchContext(t, `{`+member+`,"params":`+tc.params+`}`)
			var launched workloadLaunch
			var stdout, stderr bytes.Buffer
			code := runWorkload("compose", []string{"app"}, contextFile, &stdout, &stderr, func(launch workloadLaunch) int {
				launched = launch
				return 0
			})
			if code != 0 {
				t.Fatalf("exit %d; stderr %q", code, stderr.String())
			}
			if launched.verbose != tc.verbose || launched.quiet != tc.quiet || launched.noColor != tc.noColor {
				t.Errorf("launch verbose %t quiet %t noColor %t, want %t %t %t",
					launched.verbose, launched.quiet, launched.noColor, tc.verbose, tc.quiet, tc.noColor)
			}
		})
	}
}

// TestRunWorkloadReadsTheWarningsFromTheJobContext pins that a compose or
// qualify launch takes the warnings of putnami's workspace load from the job
// context's workspaceWarnings, and that an envelope the executable writes
// carries them, a launch failure's included, without writing them on stderr
// again: putnami wrote them there. A context without the member hands none.
func TestRunWorkloadReadsTheWarningsFromTheJobContext(t *testing.T) {
	t.Setenv(wire.SocketEnv, "")
	t.Setenv(wire.TokenEnv, "")
	spectest.Proves(t, workloadFeature, "compose-and-qualify-carry-their-launch-warnings", "the-executable-reads-the-warnings-from-the-job-context")
	root := t.TempDir()
	member := `"workspaceRoot":` + quote(t, root) + `,"workspaceProjects":[{"id":"/app","name":"app","path":"app","type":"application"}]`
	warnings := []protocolcli.WorkspaceWarning{
		{Code: "workspace.jobs_ignored", Message: "project /app declares jobs, which putnami ignores"},
		{Code: "workspace.name_divergence", Message: "project /lib is named lib in putnami.json and @acme/lib in package.json"},
	}
	encoded, err := json.Marshal(warnings)
	if err != nil {
		t.Fatal(err)
	}
	warned := writeLaunchContext(t, `{`+member+`,"params":{"output":"json"},"workspaceWarnings":`+string(encoded)+`}`)
	clean := writeLaunchContext(t, `{`+member+`,"params":{"output":"json"}}`)

	t.Run("launch", func(t *testing.T) {
		t.Setenv(cliExecutableEnv, filepath.Join(root, "bin", "putnami"))
		for contextFile, want := range map[string][]protocolcli.WorkspaceWarning{warned: warnings, clean: nil} {
			var launched workloadLaunch
			var stdout, stderr bytes.Buffer
			code := runWorkload("qualify", []string{"app"}, contextFile, &stdout, &stderr, func(launch workloadLaunch) int {
				launched = launch
				return 0
			})
			if code != 0 {
				t.Fatalf("exit %d; stderr %q", code, stderr.String())
			}
			if !slices.Equal(launched.warnings, want) {
				t.Errorf("launch warnings = %+v, want %+v", launched.warnings, want)
			}
		}
	})

	for _, tc := range []struct {
		name       string
		args       []string
		executable string
		code       int
	}{
		{"compose usage", []string{"compose", "up", "--putnamiContext", warned}, filepath.Join(root, "bin", "putnami"), protocolcli.ExitUsage},
		{"qualify usage", []string{"qualify", "run", "--putnamiContext", warned}, filepath.Join(root, "bin", "putnami"), protocolcli.ExitUsage},
		{"qualify launch failure", []string{"qualify", "run", "app", "--putnamiContext", warned}, "putnami", protocolcli.ExitFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(cliExecutableEnv, tc.executable)
			var stdout, stderr bytes.Buffer
			if code := run(tc.args, &stdout, &stderr); code != tc.code {
				t.Fatalf("exit %d, want %d; stderr %q", code, tc.code, stderr.String())
			}
			var envelope protocolcli.ResultV2
			if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
				t.Fatalf("stdout %q is not one result envelope: %v", stdout.String(), err)
			}
			if !slices.Equal(envelope.WorkspaceWarnings, warnings) {
				t.Errorf("workspaceWarnings = %+v, want %+v", envelope.WorkspaceWarnings, warnings)
			}
			if strings.Contains(stderr.String(), "putnami: warning: ") {
				t.Errorf("the executable wrote a launch warning on stderr: %q", stderr.String())
			}
		})
	}
}
