package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/tooling/sdd/extension/internal/sdd"
)

// breakingMarkerRepository is a repository whose branch "change" adds one
// commit, with message, on top of main. It returns the directory and the commit
// ID.
func breakingMarkerRepository(t *testing.T, message string) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("config", "user.email", "test@test.com")
	git("config", "user.name", "Test")
	git("config", "commit.gpgsign", "false")
	git("checkout", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "chore: start")
	git("checkout", "-q", "-b", "change")
	if err := os.WriteFile(filepath.Join(root, "src.go"), []byte("package src\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", message)
	return root, git("rev-parse", "HEAD")
}

// impactedJobContext is the context of a `validate --impacted` run that
// resolved baseline.
func impactedJobContext(root, baseline string) *pctx.Context {
	ctx := workspaceJobContext(root, nil)
	ctx.Selection = &pctx.Selection{Mode: pctx.SelectionModeImpacted, Scoped: true, Baseline: baseline}
	return ctx
}

// A run outside a proposal sets no message: the step passes and says it
// compared nothing, so the gate stays green on a push or a local run.
func TestBreakingMarkerValidatePassesWithANoteWithoutAMessage(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "squash-keeps-the-breaking-marker", "an-unset-message-passes-with-a-note")
	t.Setenv(sdd.ProposalMessageEnv, "")
	root, _ := breakingMarkerRepository(t, "feat(cli)!: drop the --all flag")

	outcome := runJob(t, runBreakingMarkerValidate, impactedJobContext(root, "main"))
	if outcome.err != nil || outcome.exitCode() != protocolcli.ExitSuccess {
		t.Fatalf("outcome = %v (exit %d), want a pass", outcome.err, outcome.exitCode())
	}
	if summary := outcome.summaryLine(); !strings.Contains(summary, "no proposal message to compare") {
		t.Errorf("summary = %q, want it to say nothing was compared", summary)
	}
}

// The baseline is the one an impacted selection resolved, and only that one: a
// run that chose its projects another way names no range, whatever its
// selection block carries.
func TestBreakingMarkerValidateRefusesARunThatIsNotImpacted(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "squash-keeps-the-breaking-marker", "a-missing-baseline-fails")
	t.Setenv(sdd.ProposalMessageEnv, "feat(cli): rework the selection")
	root, _ := breakingMarkerRepository(t, "feat(cli)!: drop the --all flag")

	for name, selection := range map[string]*pctx.Selection{
		"no selection":  nil,
		"all projects":  {Mode: pctx.SelectionModeAll, Baseline: "main"},
		"named project": {Mode: pctx.SelectionModeProjects, Scoped: true, Baseline: "main"},
	} {
		ctx := workspaceJobContext(root, nil)
		ctx.Selection = selection
		outcome := runJob(t, runBreakingMarkerValidate, ctx)
		if outcome.err == nil || !strings.Contains(outcome.err.Error(), "run with --impacted") {
			t.Errorf("%s: error = %v, want a refusal that asks for an --impacted run", name, outcome.err)
		}
		if code := outcome.exitCode(); code != protocolcli.ExitUsage {
			t.Errorf("%s: exit code = %d, want %d", name, code, protocolcli.ExitUsage)
		}
	}
}

// The variable reaches the step through the process environment, and a dropped
// marker fails the job with the gate's exit code and one diagnostic per commit.
func TestBreakingMarkerValidateFailsADroppedMarkerOnTheWire(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "squash-keeps-the-breaking-marker", "a-dropped-marker-fails-naming-the-commit")
	root, sha := breakingMarkerRepository(t, "feat(cli)!: drop the --all flag")

	t.Setenv(sdd.ProposalMessageEnv, "feat(cli): rework the selection\n\nThe body says nothing about a break.")
	outcome := runJob(t, runBreakingMarkerValidate, impactedJobContext(root, "main"))
	if outcome.err == nil || !strings.Contains(outcome.err.Error(), sha[:12]+" feat(cli)!: drop the --all flag") {
		t.Fatalf("error = %v, want a failure that names the commit", outcome.err)
	}
	if code := outcome.exitCode(); code != protocolcli.ExitUsage {
		t.Errorf("exit code = %d, want %d", code, protocolcli.ExitUsage)
	}
	diagnostics := outcome.diagnostics()
	if len(diagnostics) != 1 || diagnostics[0].Code != sdd.ErrorCodeBreakingMarkerDropped ||
		!strings.Contains(diagnostics[0].Message, sha[:12]) {
		t.Errorf("diagnostics = %+v, want one %s naming %s", diagnostics, sdd.ErrorCodeBreakingMarkerDropped, sha[:12])
	}

	t.Setenv(sdd.ProposalMessageEnv, "feat(cli)!: rework the selection")
	outcome = runJob(t, runBreakingMarkerValidate, impactedJobContext(root, "main"))
	if outcome.err != nil {
		t.Fatalf("a title with the marker failed: %v", outcome.err)
	}
	if summary := outcome.summaryLine(); !strings.Contains(summary, "1 commit(s) since main, 1 breaking") {
		t.Errorf("summary = %q, want the commits counted", summary)
	}
}
