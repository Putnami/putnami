package cloudcli

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/cloud/extension/internal/clicore/hometest"
)

// wantPublicRoot is the public root, in help order.
var wantPublicRoot = []string{
	"login", "logout", "whoami", "setup", "status", "token", "registries", "packages", "channels",
	"ci", "cache", "source", "config", "secrets", "db", "env", "infra", "deploy", "logs", "traces", "metrics",
}

func runSurface(t *testing.T, command string, args ...string) ([]string, error) {
	t.Helper()
	var output []string
	err := RunCommand(command, IO{
		Env:    hometest.Env(t.TempDir(), map[string]string{"PUTNAMI_AUTH_URL": testBaseURL, "PUTNAMI_WORKSPACE_ROOT": t.TempDir()}),
		Stdout: func(line string) { output = append(output, line) },
		Stderr: func(string) {},
		Client: newFakeTransport(t).client(),
		Now:    fixedNow,
	}, args)
	return output, err
}

func TestHelpListsExactlyThePublicRoot(t *testing.T) {
	output, err := runSurface(t, "help")
	if err != nil {
		t.Fatalf("cloud help: %v", err)
	}
	var names []string
	listing := false
	for _, line := range output {
		switch {
		case line == "Commands:":
			listing = true
		case listing && strings.HasPrefix(line, "  "):
			names = append(names, strings.Fields(line)[0])
		case listing:
			listing = false
		}
	}
	if !slices.Equal(names, wantPublicRoot) {
		t.Fatalf("help lists %v, want the 21 public entries %v", names, wantPublicRoot)
	}
	hidden := slices.Concat(machineCommands, slices.Collect(maps.Keys(operatorCommands)))
	for _, name := range hidden {
		if slices.Contains(names, name) {
			t.Fatalf("help lists the hidden command %q", name)
		}
	}
}

// TestRootListsStayDisjoint keeps the three lists disjoint: a name is public,
// a machine command or an operator command.
func TestRootListsStayDisjoint(t *testing.T) {
	seen := map[string]string{}
	add := func(kind string, names ...string) {
		for _, name := range names {
			if previous, ok := seen[name]; ok {
				t.Fatalf("%q is both %s and %s", name, previous, kind)
			}
			seen[name] = kind
		}
	}
	add("public", wantPublicRoot...)
	add("machine", machineCommands...)
	add("operator", slices.Collect(maps.Keys(operatorCommands))...)
}

func TestEntryHelpListsTheVerbs(t *testing.T) {
	for _, tc := range []struct {
		command string
		args    []string
		want    string
	}{
		{"token", []string{"help"}, "cloud token revoke <id|name>"},
		{"packages", nil, "cloud packages namespaces activate|list"},
		{"channels", []string{"help"}, "cloud channels follow <namespace>"},
		{"db", []string{"migrations"}, "cloud db migrations publish"},
		{"status", []string{"help"}, "cloud <entry> status"},
	} {
		output, err := runSurface(t, tc.command, tc.args...)
		if err != nil {
			t.Fatalf("cloud %s %v: %v", tc.command, tc.args, err)
		}
		if text := strings.Join(output, "\n"); !strings.HasPrefix(text, "@putnami/cloud ") || !strings.Contains(text, tc.want) {
			t.Fatalf("cloud %s %v help =\n%s\nwant %q", tc.command, tc.args, text, tc.want)
		}
	}
	output, err := runSurface(t, "channels", "--output=json")
	if err != nil {
		t.Fatalf("cloud channels --output=json: %v", err)
	}
	var got struct {
		Commands []map[string]string `json:"commands"`
	}
	decodeJSON(t, strings.Join(output, "\n"), &got)
	if len(got.Commands) != 3 || got.Commands[0]["command"] != "cloud channels set <channel> --from <channel|rs_id>" {
		t.Fatalf("structured channels help = %v", got.Commands)
	}
}

// TestHelpNeverActs pins that `putnami cloud <name> help` only prints: an entry
// without verbs prints its usage instead of signing out, signing in or
// deploying. No request leaves the process and the stored session survives.
func TestHelpNeverActs(t *testing.T) {
	home := t.TempDir()
	env := hometest.Env(home, map[string]string{"PUTNAMI_AUTH_URL": testBaseURL, "PUTNAMI_WORKSPACE_ROOT": t.TempDir()})
	writeTestAuth(t, home)
	transport := newFakeTransport(t)
	run := func(command string, args ...string) string {
		t.Helper()
		var output []string
		err := RunCommand(command, IO{
			Env: env, Stdout: func(line string) { output = append(output, line) }, Stderr: func(string) {},
			Client: transport.client(), Now: fixedNow,
		}, args)
		if err != nil {
			t.Fatalf("cloud %s %v: %v", command, args, err)
		}
		return strings.Join(output, "\n")
	}
	for _, command := range wantPublicRoot {
		if selfHelpEntries[command] {
			continue
		}
		if text := run(command, "help"); !strings.HasPrefix(text, "@putnami/cloud "+command+" commands:") {
			t.Fatalf("cloud %s help = %q", command, text)
		}
	}
	if text := run("logout", "--help"); !strings.Contains(text, "sign out") {
		t.Fatalf("cloud logout --help = %q", text)
	}
	// A `help` after a verb asks for help too: it never runs the verb.
	if text := run("registries", "logout", "help"); !strings.HasPrefix(text, "@putnami/cloud registries commands:") {
		t.Fatalf("cloud registries logout help = %q", text)
	}
	if text := run("cache", "disable", "help"); !strings.Contains(text, "cloud cache disable") {
		t.Fatalf("cloud cache disable help = %q", text)
	}
	if text := run("channels", "--help"); !strings.Contains(text, "cloud channels follow") {
		t.Fatalf("cloud channels --help = %q", text)
	}
	if len(transport.requests) != 0 {
		t.Fatalf("help sent %d request(s): %+v", len(transport.requests), transport.requests)
	}
	if auth, err := clicore.ReadAuth(env, false); err != nil || auth == nil || auth.RefreshToken != "refresh-token" {
		t.Fatalf("stored session after help = %+v, %v", auth, err)
	}
}

// TestOperatorCommandsNameTheirNewHome pins that the public CLI
// answers each moved command with a usage error naming the operator command.
func TestOperatorCommandsNameTheirNewHome(t *testing.T) {
	for _, tc := range []struct {
		command string
		args    []string
		want    string
	}{
		{"sql-proxy", nil, "putnami operator sql-proxy"},
		{"publish-doc", nil, "putnami operator publish-doc"},
		{"source", []string{"bind"}, "putnami operator source bind"},
		{"source", []string{"unbind"}, "putnami operator source unbind"},
		{"token", []string{"--global"}, "putnami operator token --global"},
	} {
		_, err := runSurface(t, tc.command, tc.args...)
		if clicore.ExitCode(err) != ExitUsage || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("cloud %s %v error = %v, want a usage error naming %q", tc.command, tc.args, err, tc.want)
		}
	}
}

// TestDeletedAliasesAreUnknownCommands pins that the nine root names that
// used to alias a public entry now fail like any other unknown command.
func TestDeletedAliasesAreUnknownCommands(t *testing.T) {
	for _, name := range []string{
		"tokens", "distribution", "oci", "publish-archives", "track",
		"report", "validate-config", "publish-config", "publish-migration",
	} {
		_, err := runSurface(t, name)
		if clicore.ExitCode(err) != ExitUsage || !strings.Contains(err.Error(), "unknown @putnami/cloud command: "+name) {
			t.Fatalf("cloud %s error = %v, want an unknown command usage error", name, err)
		}
	}
}

func TestEntriesRefuseUnknownVerbs(t *testing.T) {
	for _, tc := range []struct {
		command string
		args    []string
		want    string
	}{
		{"packages", []string{"bogus"}, "expected namespaces, grants"},
		{"channels", []string{"bogus"}, "expected set, status or follow"},
		{"channels", []string{"follow", "@acme", "stable", "extra"}, "takes <namespace> and one"},
		{"db", []string{"migrations", "bogus"}, "expected publish"},
		{"env", []string{"status", "prod", "dev"}, "at most one environment"},
		{"env", []string{"status", "dev", "--env", "prod"}, "takes one environment"},
		{"token", []string{"npm", "--for", "go"}, "takes one target"},
		{"infra", []string{"bogus"}, "expected plan"},
		{"bogus", nil, "unknown @putnami/cloud command"},
	} {
		_, err := runSurface(t, tc.command, tc.args...)
		if clicore.ExitCode(err) != ExitUsage || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("cloud %s %v error = %v, want a usage error containing %q", tc.command, tc.args, err, tc.want)
		}
	}
}

func TestAdoptFollowPositionalsMapsOntoTrackFlags(t *testing.T) {
	for _, tc := range []struct {
		name        string
		params      map[string]any
		positionals []string
		want        map[string]any
	}{
		{"channel", map[string]any{}, []string{"@acme", "stable"}, map[string]any{"namespace": "@acme", "channel": "stable"}},
		{"release set", map[string]any{}, []string{"@acme", "rs_abc"}, map[string]any{"namespace": "@acme", "release-set": "rs_abc"}},
		{"namespace only", map[string]any{}, []string{"@acme"}, map[string]any{"namespace": "@acme"}},
		{"flags win", map[string]any{"namespace": "@flag", "channel": "canary"}, []string{"@acme", "stable"}, map[string]any{"namespace": "@flag", "channel": "canary"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := adoptFollowPositionals(tc.params, tc.positionals); err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(tc.params, tc.want) {
				t.Fatalf("params = %v, want %v", tc.params, tc.want)
			}
		})
	}
}

func TestConfigShowArgsSelectsTheView(t *testing.T) {
	for _, tc := range []struct {
		name    string
		params  map[string]any
		args    []string
		want    []string
		wantApp string
	}{
		{"values", map[string]any{}, []string{"show"}, []string{"show"}, ""},
		{"keys", map[string]any{"keys": true}, []string{"show", "--env", "prod"}, []string{"list", "--env", "prod"}, ""},
		{"metadata", map[string]any{"metadata": true}, []string{"show"}, []string{"resolve"}, ""},
		{"schema of a named project", map[string]any{"schema": true}, []string{"show", "api", "--env", "prod"}, []string{"--env", "prod"}, "api"},
		{"declared keys of the current project", map[string]any{"declared": true, "app": "web"}, []string{"show"}, []string{}, "web"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := configShowArgs(tc.params, tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("args = %v, want %v", got, tc.want)
			}
			if app := stringParam(tc.params, "app"); app != tc.wantApp {
				t.Fatalf("app = %q, want %q", app, tc.wantApp)
			}
		})
	}
	if _, err := configShowArgs(map[string]any{"key": "DATABASE_URL"}, []string{"show"}); clicore.ExitCode(err) != ExitUsage {
		t.Fatalf("config show --key without a project error = %v, want a usage error", err)
	}
}

// TestInfraPlanIsDeclaredAndRouted pins the three places `putnami cloud infra
// plan` is wired: the subcommand the CLI lists, the command it runs, and the
// task that starts the runtime with the `infra` verb.
func TestInfraPlanIsDeclaredAndRouted(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	var manifest struct {
		CommandGroups struct {
			Cloud struct {
				Subcommands map[string]struct {
					Command     string `json:"command"`
					Interactive bool   `json:"interactive"`
					Examples    []struct {
						Command string `json:"command"`
					} `json:"examples"`
				} `json:"subcommands"`
			} `json:"cloud"`
		} `json:"commandGroups"`
		Commands map[string]struct {
			Run []struct {
				Task string `json:"task"`
			} `json:"run"`
		} `json:"commands"`
		Tasks map[string]struct {
			Kind    string          `json:"kind"`
			Command string          `json:"command"`
			Args    []string        `json:"args"`
			Cache   json.RawMessage `json:"cache"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}

	sub, ok := manifest.CommandGroups.Cloud.Subcommands["infra"]
	if !ok || sub.Command != "cloud-infra" || !sub.Interactive {
		t.Fatalf("cloud infra subcommand = %+v (declared %t), want the interactive cloud-infra command", sub, ok)
	}
	planned := false
	for _, example := range sub.Examples {
		planned = planned || strings.HasPrefix(example.Command, "putnami cloud infra plan")
	}
	if !planned {
		t.Fatalf("cloud infra examples = %+v, want one that runs `putnami cloud infra plan`", sub.Examples)
	}
	command, ok := manifest.Commands["cloud-infra"]
	if !ok || len(command.Run) != 1 || command.Run[0].Task != "cloud-infra" {
		t.Fatalf("cloud-infra command = %+v (declared %t), want it to run the cloud-infra task", command, ok)
	}
	task, ok := manifest.Tasks["cloud-infra"]
	if !ok || task.Kind != "command" || task.Command != "{extensionRuntime}" || !slices.Equal(task.Args, []string{"infra"}) {
		t.Fatalf("cloud-infra task = %+v (declared %t), want a runtime command with the infra verb", task, ok)
	}
	if string(task.Cache) != "false" {
		t.Fatalf("cloud-infra task cache = %s, want false: the plan reads files no cache key covers", task.Cache)
	}
	if !slices.Contains(wantPublicRoot, "infra") {
		t.Fatal("infra is not in the public root")
	}
}

// runInfra runs `cloud infra <args>` the way the CLI starts it: with a job
// context that lists the workspace projects.
func runInfra(t *testing.T, projects []map[string]any, args ...string) ([]string, error) {
	t.Helper()
	root := t.TempDir()
	contextFile := filepath.Join(t.TempDir(), "context.json")
	writeJSONFile(t, contextFile, map[string]any{
		"workspaceRoot":     root,
		"params":            map[string]any{},
		"workspaceProjects": projects,
	})
	return runSurface(t, "infra", append(slices.Clone(args), "--putnamiContext", contextFile)...)
}

func TestInfraPlanReadsTheWorkspaceFromTheJobContext(t *testing.T) {
	apiDir := filepath.Join(t.TempDir(), "apps", "api")
	if err := os.MkdirAll(filepath.Join(apiDir, ".gen"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(apiDir, ".gen", "requirements.json"),
		[]byte(`{"protocolVersion":2,"workload":"example/api"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	projects := []map[string]any{
		{"name": "example/core", "path": "libs/core", "fullPath": filepath.Join(t.TempDir(), "libs", "core"), "type": "library"},
		{"name": "example/api", "path": "apps/api", "fullPath": apiDir, "type": "application"},
	}

	for _, args := range [][]string{{"plan"}, {}, {"plan", "example/api"}} {
		output, err := runInfra(t, projects, args...)
		if err != nil {
			t.Fatalf("cloud infra %v: %v", args, err)
		}
		want := []string{
			"Workload: example/api",
			strings.Repeat("─", len("Workload: example/api")),
			"  (no infrastructure declared)",
		}
		if !slices.Equal(output, want) {
			t.Fatalf("cloud infra %v printed %q, want %q", args, output, want)
		}
	}

	_, err := runInfra(t, projects, "plan", "example/core")
	if clicore.ExitCode(err) != ExitUsage || !strings.Contains(err.Error(), `project "example/core" is not an application workload (type="library")`) {
		t.Fatalf("cloud infra plan example/core error = %v, want a usage error naming the library", err)
	}
}

// TestInfraPlanDeclinesAProjectWithoutAType pins the rule for a job context
// from a CLI that predates the project type: the command fails and does not
// read the missing type as application.
func TestInfraPlanDeclinesAProjectWithoutAType(t *testing.T) {
	projects := []map[string]any{
		{"name": "example/api", "path": "apps/api", "fullPath": filepath.Join(t.TempDir(), "apps", "api")},
	}
	for _, args := range [][]string{{"plan"}, {"plan", "--output=json"}} {
		output, err := runInfra(t, projects, args...)
		if err == nil || !strings.Contains(err.Error(), `"example/api"`) || !strings.Contains(err.Error(), "upgrade the CLI") {
			t.Fatalf("cloud infra %v error = %v, want a refusal that names the project and says to upgrade the CLI", args, err)
		}
		if len(output) != 0 {
			t.Fatalf("cloud infra %v printed %q before it refused the context", args, output)
		}
	}
}
