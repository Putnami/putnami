package extension

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// groupInvocationFixture is a manifest at contract 15: `stage` defaults to the
// interactive `up`, which takes a required project positional and runs a
// serve-workload command, beside `list`, `status` and `peek`; `notes`
// defaults to `show`, which takes no positional, and its interactive `add`
// carries the nested `draft`.
const groupInvocationFixture = "testdata/group-default-positionals"

func loadGroupInvocationFixture(t *testing.T) []*ExtensionDescription {
	t.Helper()
	dir, err := filepath.Abs(groupInvocationFixture)
	if err != nil {
		t.Fatal(err)
	}
	ext := LoadExtensionFromDir(dir, "@acme/stage")
	if ext == nil {
		t.Fatal("the group invocation fixture does not load")
	}
	return []*ExtensionDescription{ext}
}

// A declared word wins; any other word runs a default that takes positionals,
// as its first argument; without such a default it stays an unknown
// subcommand.
func TestResolveGroupInvocation(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "a-group-default-takes-positionals",
		"a-word-that-names-no-subcommand-runs-the-positional-default")
	extensions := loadGroupInvocationFixture(t)
	for _, tc := range []struct {
		group, word, sub string
		positional       bool
		command          string
	}{
		{"stage", "", "up", false, "stage-up"},
		{"stage", "up", "up", false, "stage-up"},
		{"stage", "list", "list", false, "stage-list"},
		{"stage", "/apps/web", "up", true, "stage-up"},
		{"stage", "web", "up", true, "stage-up"},
		{"notes", "", "show", false, "notes-show"},
		{"notes", "add", "add", false, "notes-add"},
		{"notes", "nope", "nope", false, ""},
		{"absent", "word", "word", false, ""},
	} {
		sub, positional := ResolveGroupInvocation(extensions, tc.group, tc.word)
		if sub != tc.sub || positional != tc.positional {
			t.Errorf("ResolveGroupInvocation(%s, %q) = %q, %v; want %q, %v", tc.group, tc.word, sub, positional, tc.sub, tc.positional)
		}
		if got := GroupInvocationCommand(extensions, tc.group, tc.word); got != tc.command {
			t.Errorf("GroupInvocationCommand(%s, %q) = %q, want %q", tc.group, tc.word, got, tc.command)
		}
	}
	if def := GroupPositionalDefault(extensions, "stage"); def == nil || def.Subcommand != "up" || def.CommandName != "stage-up" {
		t.Errorf("GroupPositionalDefault(stage) = %+v, want up", def)
	}
	if def := GroupPositionalDefault(extensions, "notes"); def != nil {
		t.Errorf("GroupPositionalDefault(notes) = %+v, want nil: show takes no positional", def)
	}
}

// Stamped below 15, a manifest whose group default takes positionals does not
// load: discovery skips it with the reason and the remedy.
func TestLoadExtension_PositionalDefaultBelowItsContractIsSkipped(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "a-group-default-takes-positionals",
		"a-manifest-with-a-positional-default-requires-contract-15")
	data, err := os.ReadFile(filepath.Join(groupInvocationFixture, ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	stamped := strings.Replace(string(data), `"cliContract": 15`, `"cliContract": 14`, 1)
	if stamped == string(data) {
		t.Fatal("the fixture is not stamped 15")
	}
	dir := t.TempDir()
	writeExtensionManifest(t, dir, stamped)
	ext, skip := tryLoadExtension(dir, "@acme/stage", false)
	if ext != nil || skip == nil {
		t.Fatalf("a manifest stamped 14 loaded: ext=%v skip=%v", ext, skip)
	}
	for _, want := range []string{"requires 15 for a default subcommand that takes positionals", "putnami extensions update"} {
		if !strings.Contains(skip.Reason.Error(), want) {
			t.Errorf("skip reason %q does not say %q", skip.Reason, want)
		}
	}
}

// A command that interactive subcommands alone carry, nested ones included,
// reports the path to type; one that a non-interactive subcommand also carries,
// or that no subcommand carries, does not.
func TestInteractiveOnlyCarrier(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-side-effect-boundary",
		"a-workload-carried-only-by-an-interactive-subcommand-is-refused")
	extensions := loadGroupInvocationFixture(t)
	for command, want := range map[string]string{
		"stage-up":     "stage up",
		"stage-list":   "stage list",
		"notes-add":    "notes add",
		"notes-draft":  "notes add draft",
		"notes-show":   "",
		"stage-status": "",
		"deploy":       "",
	} {
		path, ok := InteractiveOnlyCarrier(extensions, command)
		if ok != (want != "") || path != want {
			t.Errorf("InteractiveOnlyCarrier(%s) = %q, %v; want %q", command, path, ok, want)
		}
	}
}
