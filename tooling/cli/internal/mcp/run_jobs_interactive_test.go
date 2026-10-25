package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// interactiveCarriedFixture is a manifest whose `stage up`, `stage sync` and
// `notes show` subcommands are interactive and whose `stage status` and
// `notes peek` are not: stage-up, of the once workload, is carried only
// interactively, stage-sync too but without a workload, notes-show by both
// kinds, notes-draft, of the once workload, by a nested subcommand of the
// interactive `notes add`, and notes-count by none. It is named @acme/demo, outside the first-party scope,
// so the repository's first-party command checks do not read it.
const interactiveCarriedFixture = "testdata/interactive-carried"

func loadInteractiveCarriedFixture(t *testing.T) []*extension.ExtensionDescription {
	t.Helper()
	dir, err := filepath.Abs(interactiveCarriedFixture)
	if err != nil {
		t.Fatal(err)
	}
	ext := extension.LoadExtensionFromDir(dir, "@acme/demo")
	if ext == nil {
		t.Fatal("the interactive-carried fixture does not load")
	}
	return []*extension.ExtensionDescription{ext}
}

// A command whose traits declare a workload and that a loaded extension
// carries only under interactive subcommands is refused with the subcommand
// to type in a terminal, whatever its name; a command without a workload, one
// any non-interactive subcommand carries, and one that no subcommand carries
// are not refused by that rule.
func TestRejectLongLivedCommands_RefusesInteractiveOnlyCommands(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-side-effect-boundary",
		"a-workload-carried-only-by-an-interactive-subcommand-is-refused")
	extensions := loadInteractiveCarriedFixture(t)
	for command, path := range map[string]string{
		"stage-up":     "stage up",
		"notes-draft":  "notes add draft",
		"stage-sync":   "",
		"notes-show":   "",
		"stage-status": "",
		"notes-count":  "",
	} {
		err := rejectLongLivedCommands(extensions, []string{"build", command})
		if path == "" {
			if err != nil {
				t.Errorf("%s: refused: %v", command, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: accepted, want a refusal naming `putnami %s`", command, path)
			continue
		}
		for _, want := range []string{"`putnami " + path + "`", "in a terminal instead", "interactive subcommand"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: refusal %q does not say %q", command, err, want)
			}
		}
	}
}

// run_jobs applies the refusal before it runs anything, and a dry run still
// plans the command. The fixture replaces the job fixture's @putnami/demo
// manifest, under that name, which the job fixture's projects name.
func TestRunJobs_RefusesAnInteractiveOnlyCommand(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-side-effect-boundary",
		"a-workload-carried-only-by-an-interactive-subcommand-is-refused")
	dir := jobFixture(t)
	manifest, err := os.ReadFile(filepath.Join(interactiveCarriedFixture, extension.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	manifest = []byte(strings.Replace(string(manifest), `"@acme/demo"`, `"@putnami/demo"`, 1))
	if err := os.WriteFile(filepath.Join(dir, "tools", "demo", extension.ManifestFilename), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	workspace.InvalidateLoadCache(dir)
	srv := NewServer(Options{WorkspaceRoot: dir, Config: wsproto.Load(dir), ServerVersion: "test"})

	_, err = srv.toolRunJobs(context.Background(), json.RawMessage(`{"commands":["stage-up"]}`))
	if err == nil || !strings.Contains(err.Error(), "Run `putnami stage up` in a terminal instead") {
		t.Fatalf("run_jobs stage-up = %v, want the interactive refusal", err)
	}
	if _, err := srv.toolRunJobs(context.Background(), json.RawMessage(`{"commands":["stage-up"],"dryRun":true}`)); err != nil &&
		strings.Contains(err.Error(), "cannot run") {
		t.Fatalf("planning stage-up hit the refusal, which guards execution only: %v", err)
	}
}
