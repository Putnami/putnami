// Package groupdefault drives the command groups of a fixture extension
// through the real CLI: a group whose default subcommand declares positionals
// runs that default for a word that names no subcommand. The tests change the
// working directory and swap the process streams, so they run in their own
// test binary.
package groupdefault

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

func TestMain(m *testing.M) {
	os.Exit(clitest.Main(m))
}

// The requirement these tests prove.
const (
	groupDefaultFeature     = "cli/job-planning-execution"
	groupDefaultRequirement = "a-group-default-takes-positionals"
)

// groupDefaultPositionalsFixture is a workspace whose @acme/stage extension, at
// contract 15, declares two groups. `stage` defaults to the interactive `up`,
// which takes a required project positional and runs a serve-workload command;
// `list` is a subcommand and a project name. `notes` defaults to `show`, which
// takes no positional, and its `add` takes a required text and an optional tag.
// Every task prints the arguments it receives.
const groupDefaultPositionalsFixture = "testdata/group-default-positionals"

// copyFixture copies the fixture workspace into a temporary directory, so the
// run writes nothing below testdata.
func copyFixture(t *testing.T) string {
	t.Helper()
	clitest.RequireShell(t)
	wsRoot := t.TempDir()
	if err := os.CopyFS(wsRoot, os.DirFS(groupDefaultPositionalsFixture)); err != nil {
		t.Fatal(err)
	}
	return wsRoot
}

// runStdout runs args in wsRoot and returns stdout alone.
func runStdout(t *testing.T, wsRoot string, args ...string) string {
	t.Helper()
	stdout, _ := clitest.CaptureStreams(t, func() {
		app, err := cli.NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		t.Chdir(wsRoot)
		app.Run(context.Background(), args)
	})
	return stdout
}

// A word that names no subcommand of a group whose default takes positionals
// runs the default, with the word as its first argument and the flags after it.
// A flag or "--" before the word names no subcommand either, so the group runs
// its default, which receives the arguments in the order given and parses the
// positional itself.
func TestGroupWordRunsThePositionalDefault(t *testing.T) {
	spectest.Proves(t, groupDefaultFeature, groupDefaultRequirement,
		"a-word-that-names-no-subcommand-runs-the-positional-default")
	wsRoot := copyFixture(t)
	for _, tc := range []struct {
		args []string
		argv string
	}{
		{[]string{"stage", "/apps/web", "--no-watch"}, "argv:up /apps/web --no-watch --putnamiContext "},
		{[]string{"stage", "up", "/apps/web", "--no-watch"}, "argv:up /apps/web --no-watch --putnamiContext "},
		{[]string{"stage", "--no-watch", "/apps/web"}, "argv:up --no-watch /apps/web --putnamiContext "},
		{[]string{"stage", "--", "/apps/web"}, "argv:up -- /apps/web --putnamiContext "},
	} {
		code, output := clitest.RunGateArgs(t, wsRoot, tc.args...)
		if code != cli.ExitSuccess {
			t.Fatalf("%v: exit %d, want %d:\n%s", tc.args, code, cli.ExitSuccess, output)
		}
		if !strings.Contains(output, tc.argv) {
			t.Errorf("%v did not run %q:\n%s", tc.args, tc.argv, output)
		}
	}
}

// A declared subcommand wins over a project of the same name, which the user
// addresses by its id.
func TestDeclaredSubcommandWinsOverAProjectName(t *testing.T) {
	spectest.Proves(t, groupDefaultFeature, groupDefaultRequirement,
		"a-declared-subcommand-wins-over-a-positional-word")
	wsRoot := copyFixture(t)
	code, output := clitest.RunGateArgs(t, wsRoot, "stage", "list")
	if code != cli.ExitSuccess || !strings.Contains(output, "argv:list --putnamiContext ") {
		t.Errorf("stage list: exit %d, want the list subcommand:\n%s", code, output)
	}
	code, output = clitest.RunGateArgs(t, wsRoot, "stage", "/apps/list")
	if code != cli.ExitSuccess || !strings.Contains(output, "argv:up /apps/list --putnamiContext ") {
		t.Errorf("stage /apps/list: exit %d, want up with the project id:\n%s", code, output)
	}
}

// A group whose default takes no positional keeps refusing a word that names
// no subcommand, and still runs its default without a word.
func TestWordWithoutAPositionalDefaultIsUnknown(t *testing.T) {
	spectest.Proves(t, groupDefaultFeature, groupDefaultRequirement,
		"a-word-without-a-positional-default-stays-an-unknown-subcommand")
	wsRoot := copyFixture(t)
	code, output := clitest.RunGateArgs(t, wsRoot, "notes", "nope")
	if code != cli.ExitError {
		t.Errorf("notes nope: exit %d, want %d", code, cli.ExitError)
	}
	for _, want := range []string{"putnami: unknown subcommand: notes nope", "Available: add, show"} {
		if !strings.Contains(output, want) {
			t.Errorf("notes nope does not say %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "argv:") {
		t.Errorf("notes nope ran a task:\n%s", output)
	}
	code, output = clitest.RunGateArgs(t, wsRoot, "notes")
	if code != cli.ExitSuccess || !strings.Contains(output, "argv:show --putnamiContext ") {
		t.Errorf("notes: exit %d, want the show default:\n%s", code, output)
	}
}

// Help prints each positional in the usage line, and the spelling without the
// default's name for a group whose default takes positionals.
func TestGroupHelpPrintsThePositional(t *testing.T) {
	spectest.Proves(t, groupDefaultFeature, groupDefaultRequirement,
		"the-group-help-prints-the-positional-in-its-usage-line")
	wsRoot := copyFixture(t)
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"stage", "--help"}, []string{
			"Usage: putnami stage [up] <project> [flags]\n       putnami stage <subcommand> [flags]\n",
			"A word that names no subcommand runs `putnami stage up` with that word as <project>. " +
				"A project named like a subcommand is addressed by its id.",
			"  list\n  up\n",
		}},
		{[]string{"stage", "/apps/web", "--help"}, []string{"Usage: putnami stage [up] <project> [flags]\n\nStage one project."}},
		{[]string{"stage", "up", "--help"}, []string{"Usage: putnami stage [up] <project> [flags]\n\nStage one project."}},
		{[]string{"notes", "--help"}, []string{"Usage: putnami notes <subcommand> [flags]\n"}},
		{[]string{"notes", "add", "--help"}, []string{"Usage: putnami notes add <text> [<tag>] [flags]\n"}},
	} {
		code, output := clitest.RunGateArgs(t, wsRoot, tc.args...)
		if code != cli.ExitSuccess {
			t.Errorf("%v: exit %d, want %d", tc.args, code, cli.ExitSuccess)
		}
		for _, want := range tc.want {
			if !strings.Contains(output, want) {
				t.Errorf("%v does not print %q:\n%s", tc.args, want, output)
			}
		}
	}
}

// positionalJSON is one positional of the machine help.
type positionalJSON struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
}

// The machine help carries the positionals of a subcommand and marks the
// group default.
func TestGroupHelpJSONCarriesThePositionals(t *testing.T) {
	spectest.Proves(t, groupDefaultFeature, groupDefaultRequirement,
		"the-group-help-prints-the-positional-in-its-usage-line")
	wsRoot := copyFixture(t)
	output := runStdout(t, wsRoot, "stage", "up", "--help", "--output=jsonl")
	var sub struct {
		Command     string           `json:"command"`
		Positionals []positionalJSON `json:"positionals"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &sub); err != nil {
		t.Fatalf("stage up --help --output=jsonl: %v\n%s", err, output)
	}
	if sub.Command != "stage up" || !slices.Equal(sub.Positionals, []positionalJSON{{Name: "project", Required: true}}) {
		t.Errorf("help = %+v, want stage up with the required project", sub)
	}

	output = runStdout(t, wsRoot, "stage", "--help", "--output=json")
	type summary struct {
		Command     string           `json:"command"`
		Default     bool             `json:"default"`
		Positionals []positionalJSON `json:"positionals"`
	}
	var summaries []summary
	if err := json.Unmarshal([]byte(output), &summaries); err != nil {
		t.Fatalf("stage --help --output=json: %v\n%s", err, output)
	}
	want := []summary{
		{Command: "stage list"},
		{Command: "stage up", Default: true, Positionals: []positionalJSON{{Name: "project", Required: true}}},
	}
	if len(summaries) != len(want) {
		t.Fatalf("summaries = %+v, want %+v", summaries, want)
	}
	for i := range want {
		if summaries[i].Command != want[i].Command || summaries[i].Default != want[i].Default ||
			!slices.Equal(summaries[i].Positionals, want[i].Positionals) {
			t.Errorf("summary %d = %+v, want %+v", i, summaries[i], want[i])
		}
	}
}
