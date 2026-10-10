package cloudcli

import (
	"maps"
	"slices"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/cloud/extension/internal/clicore/hometest"
)

// wantPublicRoot is the public root, in help order.
var wantPublicRoot = []string{
	"login", "logout", "whoami", "setup", "status", "token", "registries", "packages", "channels",
	"ci", "cache", "source", "config", "secrets", "db", "env", "deploy", "logs", "traces", "metrics",
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
		t.Fatalf("help lists %v, want the 20 public entries %v", names, wantPublicRoot)
	}
	hidden := slices.Concat(slices.Collect(maps.Keys(hiddenAliases)), machineCommands, slices.Collect(maps.Keys(operatorCommands)))
	for _, name := range hidden {
		if slices.Contains(names, name) {
			t.Fatalf("help lists the hidden command %q", name)
		}
	}
}

// TestHiddenAliasesStayOutOfThePublicRoot keeps the three lists disjoint: a
// name is public, a hidden alias, a machine command or an operator command.
func TestHiddenAliasesStayOutOfThePublicRoot(t *testing.T) {
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
	add("alias", slices.Collect(maps.Keys(hiddenAliases))...)
	add("machine", machineCommands...)
	add("operator", slices.Collect(maps.Keys(operatorCommands))...)
	for alias, target := range hiddenAliases {
		if !slices.Contains(wantPublicRoot, strings.Fields(target)[0]) {
			t.Fatalf("alias %s points at %q, which is not a public entry", alias, target)
		}
	}
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

// TestHelpNeverActs pins that `putnami cloud <name> help` only prints: an alias
// names its replacement, and an entry without verbs prints its usage instead
// of signing out, signing in or deploying. No request leaves the process and
// the stored session survives.
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
	for alias, replacement := range hiddenAliases {
		if text := run(alias, "help"); !strings.Contains(text, "older name of `putnami cloud "+replacement+"`") {
			t.Fatalf("cloud %s help = %q", alias, text)
		}
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
	if text := run("tokens", "revoke", "help"); !strings.Contains(text, "older name of `putnami cloud token create|list|revoke`") {
		t.Fatalf("cloud tokens revoke help = %q", text)
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

	var structured struct {
		Alias       string `json:"alias"`
		Replacement string `json:"replacement"`
	}
	decodeJSON(t, run("track", "help", "--output=json"), &structured)
	if structured.Alias != "cloud track" || structured.Replacement != "cloud channels follow" {
		t.Fatalf("structured alias help = %+v", structured)
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
