package ci

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/cli/model/jobs"
	ciproto "go.putnami.dev/protocol/ci"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// impactPlanTestPlanner reports a complete plan of commands over project, and
// counts its calls.
func impactPlanTestPlanner(project *workspace.Project, commands []string, calls *int) Planner {
	return func(context.Context, string, bool) (PlannerResult, error) {
		*calls++
		return PlannerResult{
			Commands: commands,
			Complete: true,
			Projects: []*workspace.Project{project},
			Jobs:     []*jobs.ScheduledJob{impactPlanTestJob(project)},
		}, nil
	}
}

// TestEmitImpactPlanPinsTheRangeAndIsByteStable holds the plan to the range it
// was asked for: the full base and head commit IDs, the commit diff and the
// project that owns it, the planned command list in its order, and the same
// bytes for two runs over one range. The plan never carries the credentials of
// the checkout's remote.
func TestEmitImpactPlanPinsTheRangeAndIsByteStable(t *testing.T) {
	root, baseSHA, headSHA, project := writeImpactPlanRepository(t)
	calls := 0
	planner := func(_ context.Context, base string, noCache bool) (PlannerResult, error) {
		calls++
		if base != baseSHA || !noCache {
			t.Fatalf("planner inputs = base %q, noCache %t; want %s and true", base, noCache, baseSHA)
		}
		return PlannerResult{
			Commands: []string{"test", "lint"},
			Complete: true,
			Projects: []*workspace.Project{project},
			Jobs:     []*jobs.ScheduledJob{impactPlanTestJob(project)},
		}, nil
	}
	opts := PlanOptions{Base: baseSHA, NoCache: true, CLIVersion: "test"}

	first, err := EmitImpactPlan(context.Background(), root, opts, planner)
	if err != nil {
		t.Fatalf("EmitImpactPlan first: %v", err)
	}
	if err := ciproto.ValidateImpactPlan(first); err != nil {
		t.Fatalf("ValidateImpactPlan: %v", err)
	}
	if first.BaseSHA != baseSHA || first.HeadSHA != headSHA || !reflect.DeepEqual(first.Commands, []string{"test", "lint"}) {
		t.Fatalf("impact plan = %s..%s %v, want %s..%s [test lint]", first.BaseSHA, first.HeadSHA, first.Commands, baseSHA, headSHA)
	}
	if !reflect.DeepEqual(first.ChangedFiles, []string{"app/source.txt"}) || len(first.Tasks) != 1 ||
		len(first.Impact.DirectProjects) != 1 || first.Impact.DirectProjects[0].ID != project.ID {
		t.Fatalf("impact plan members = %+v", first)
	}
	if first.Cache.Status != "disabled" {
		t.Fatalf("cache = %+v, want disabled under --no-cache", first.Cache)
	}

	second, err := EmitImpactPlan(context.Background(), root, opts, planner)
	if err != nil {
		t.Fatalf("EmitImpactPlan second: %v", err)
	}
	firstBytes, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondBytes, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstBytes) != string(secondBytes) {
		t.Fatalf("equivalent runs differ:\n%s\n%s", firstBytes, secondBytes)
	}
	for _, credential := range []string{"token", "secret"} {
		if strings.Contains(string(firstBytes), credential) {
			t.Fatalf("impact plan carries the remote credential %q:\n%s", credential, firstBytes)
		}
	}
	if calls != 2 {
		t.Fatalf("planner calls = %d, want 2", calls)
	}
}

// TestEmitImpactPlanNeedsNoRemote holds the repository to the consumer: an
// impact plan is emitted from a checkout with no origin remote.
func TestEmitImpactPlanNeedsNoRemote(t *testing.T) {
	spectest.Proves(t, "cli/impact-plan", "pinned-revisions", "no-remote-is-needed")
	root, baseSHA, _, project := writeImpactPlanRepository(t)
	runImpactPlanGit(t, root, "remote", "remove", "origin")
	calls := 0
	planner := impactPlanTestPlanner(project, []string{"lint"}, &calls)
	opts := PlanOptions{Base: baseSHA, NoCache: true, CLIVersion: "test"}

	if _, err := EmitImpactPlan(context.Background(), root, opts, planner); err != nil {
		t.Fatalf("EmitImpactPlan without a remote: %v", err)
	}
	if calls != 1 {
		t.Fatalf("planner calls = %d, want 1", calls)
	}
}

// TestEmitImpactPlanRefusalsNameImpactPlan holds every refusal of the shared
// revision checks and planning stage to the command that ran it.
func TestEmitImpactPlanRefusalsNameImpactPlan(t *testing.T) {
	spectest.Proves(t, "cli/impact-plan", "pinned-revisions", "the-range-is-refused-before-planning")
	root, baseSHA, _, project := writeImpactPlanRepository(t)
	complete := func(context.Context, string, bool) (PlannerResult, error) {
		return PlannerResult{Commands: []string{"lint"}, Complete: true, Projects: []*workspace.Project{project}}, nil
	}
	plannerErr := errors.New("plan lint: engine refused the plan")
	orphanSHA := impactPlanGitOutput(t, root, "commit-tree", "HEAD^{tree}", "-m", "orphan")
	for _, tc := range []struct {
		name    string
		opts    PlanOptions
		planner Planner
		want    string
	}{
		{"no base", PlanOptions{}, complete, "impact-plan requires --base <commit>"},
		{"unknown head", PlanOptions{Base: baseSHA, Head: "no-such-ref"}, complete, "impact-plan cannot resolve --head"},
		{"head not checked out", PlanOptions{Base: baseSHA, Head: baseSHA}, complete, "impact-plan --head must resolve to the checked-out HEAD"},
		{"unknown base", PlanOptions{Base: "no-such-ref"}, complete, "impact-plan cannot resolve --base"},
		{"base not an ancestor", PlanOptions{Base: orphanSHA}, complete, "impact-plan --base must be an ancestor of the checked-out HEAD"},
		{"no planner", PlanOptions{Base: baseSHA}, nil, "impact-plan planner is not configured"},
		{"planner error", PlanOptions{Base: baseSHA}, func(context.Context, string, bool) (PlannerResult, error) {
			return PlannerResult{}, plannerErr
		}, "plan lint: engine refused the plan"},
		{"incomplete", PlanOptions{Base: baseSHA}, func(context.Context, string, bool) (PlannerResult, error) {
			return PlannerResult{Commands: []string{"lint", "test"}}, nil
		}, "impact-plan could not produce a complete lint,test plan"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := EmitImpactPlan(context.Background(), root, tc.opts, tc.planner)
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("EmitImpactPlan error = %v, want prefix %q", err, tc.want)
			}
		})
	}
}

// TestEmitImpactPlanRefusesANonCheckoutHeadBeforePlanning holds the head to
// the checkout: a --head other than the checked-out HEAD is refused before the
// planner runs.
func TestEmitImpactPlanRefusesANonCheckoutHeadBeforePlanning(t *testing.T) {
	spectest.Proves(t, "cli/impact-plan", "pinned-revisions", "the-range-is-refused-before-planning")
	root, baseSHA, _, project := writeImpactPlanRepository(t)
	calls := 0
	_, err := EmitImpactPlan(context.Background(), root, PlanOptions{Base: baseSHA, Head: baseSHA, NoCache: true, CLIVersion: "test"},
		impactPlanTestPlanner(project, []string{"lint"}, &calls))
	if err == nil || err.Error() != "impact-plan --head must resolve to the checked-out HEAD" {
		t.Fatalf("EmitImpactPlan error = %v, want the checked-out HEAD refusal", err)
	}
	if calls != 0 {
		t.Fatal("planner ran for a head that is not the checkout")
	}
}

// TestEmitImpactPlanRefusesPlanningThatChangesTheCheckout holds the recheck
// after planning: a plan whose planning wrote into the worktree is refused.
func TestEmitImpactPlanRefusesPlanningThatChangesTheCheckout(t *testing.T) {
	spectest.Proves(t, "cli/impact-plan", "pinned-revisions", "planning-that-changes-the-checkout-is-refused")
	root, baseSHA, _, _ := writeImpactPlanRepository(t)
	_, err := EmitImpactPlan(context.Background(), root, PlanOptions{Base: baseSHA},
		func(context.Context, string, bool) (PlannerResult, error) {
			writeImpactPlanFile(t, filepath.Join(root, "app", "generated.txt"), "written by planning\n")
			return PlannerResult{Commands: []string{"lint"}, Complete: true}, nil
		})
	if err == nil || err.Error() != "impact-plan planning changed the checked-out source state" {
		t.Fatalf("EmitImpactPlan error = %v, want the changed source state refusal", err)
	}
}

// TestEmitImpactPlanRefusesADirtyWorktreeBeforePlanning holds the clean
// worktree requirement: source bytes the revisions do not identify are never
// planned.
func TestEmitImpactPlanRefusesADirtyWorktreeBeforePlanning(t *testing.T) {
	spectest.Proves(t, "cli/impact-plan", "pinned-revisions", "a-dirty-worktree-is-refused-before-planning")
	root, baseSHA, _, project := writeImpactPlanRepository(t)
	writeImpactPlanFile(t, filepath.Join(root, "app", "source.txt"), "uncommitted\n")
	calls := 0
	_, err := EmitImpactPlan(context.Background(), root, PlanOptions{Base: baseSHA}, impactPlanTestPlanner(project, []string{"lint"}, &calls))
	if err == nil || err.Error() != "impact-plan requires a clean worktree" || !errors.Is(err, cmderr.ErrInvalidConfig) {
		t.Fatalf("EmitImpactPlan error = %v, want the clean worktree refusal", err)
	}
	if calls != 0 {
		t.Fatal("planner ran on a dirty worktree")
	}
}

// TestRenderImpactPlanWritesOneEnvelope holds the structured output to one
// result envelope whose data is the ImpactPlan, which a consumer validates
// with the protocol package alone, and the human output to a summary.
func TestRenderImpactPlanWritesOneEnvelope(t *testing.T) {
	spectest.Proves(t, "cli/impact-plan", "one-structured-result", "the-render-writes-one-envelope")
	root, baseSHA, headSHA, project := writeImpactPlanRepository(t)
	calls := 0
	document, err := EmitImpactPlan(context.Background(), root, PlanOptions{Base: baseSHA, NoCache: true, CLIVersion: "test"},
		impactPlanTestPlanner(project, []string{"lint", "test"}, &calls))
	if err != nil {
		t.Fatalf("EmitImpactPlan: %v", err)
	}

	printed, err := iox.CaptureStdout(func() error { return RenderImpactPlan("json", document) })
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var envelope struct {
		ProtocolVersion int                `json:"protocolVersion"`
		Command         string             `json:"command"`
		Status          string             `json:"status"`
		Data            ciproto.ImpactPlan `json:"data"`
	}
	if err := json.Unmarshal([]byte(printed), &envelope); err != nil {
		t.Fatalf("decode printed plan: %v\n%s", err, printed)
	}
	if strings.Count(strings.TrimSpace(printed), "\n") != 0 {
		t.Fatalf("structured output is not one envelope line:\n%s", printed)
	}
	if envelope.ProtocolVersion != protocolcli.ResultProtocolVersion || envelope.Command != "impact-plan" || envelope.Status == "" {
		t.Fatalf("envelope = %+v, want a v%d impact-plan result", envelope, protocolcli.ResultProtocolVersion)
	}
	if err := ciproto.ValidateImpactPlan(envelope.Data); err != nil {
		t.Fatalf("printed plan does not validate: %v", err)
	}
	printedBytes, err := json.Marshal(envelope.Data)
	if err != nil {
		t.Fatal(err)
	}
	documentBytes, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if string(printedBytes) != string(documentBytes) {
		t.Fatalf("printed plan differs from the emitted plan:\n%s\n%s", printedBytes, documentBytes)
	}

	human, err := iox.CaptureStdout(func() error { return RenderImpactPlan("", document) })
	if err != nil {
		t.Fatalf("render human: %v", err)
	}
	if !strings.Contains(human, "ImpactPlan lint,test") ||
		!strings.Contains(human, baseSHA[:12]+".."+headSHA[:12]+" (1 changed file(s), 1 impacted project(s), 1 task(s))") {
		t.Fatalf("human output = %q", human)
	}
}

// writeImpactPlanRepository is a two-commit workspace repository whose head
// changes one file of its one project, app. Its origin remote carries
// credentials. It returns the root, both commits and the project.
func writeImpactPlanRepository(t *testing.T) (root, baseSHA, headSHA string, project *workspace.Project) {
	t.Helper()
	root = t.TempDir()
	writeImpactPlanFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"fixture","includes":["app"]}`)
	writeImpactPlanFile(t, filepath.Join(root, "app", "putnami.json"), `{"name":"app"}`)
	writeImpactPlanFile(t, filepath.Join(root, "app", "source.txt"), "base\n")
	runImpactPlanGit(t, root, "init")
	runImpactPlanGit(t, root, "config", "user.email", "fixture@example.test")
	runImpactPlanGit(t, root, "config", "user.name", "Fixture")
	runImpactPlanGit(t, root, "remote", "add", "origin", "https://token:secret@example.test/org/repo.git")
	runImpactPlanGit(t, root, "add", ".")
	runImpactPlanGit(t, root, "commit", "-m", "base")
	var err error
	baseSHA, err = git.HeadSHA(root)
	if err != nil {
		t.Fatalf("base HEAD: %v", err)
	}
	writeImpactPlanFile(t, filepath.Join(root, "app", "source.txt"), "head\n")
	runImpactPlanGit(t, root, "add", "app/source.txt")
	runImpactPlanGit(t, root, "commit", "-m", "head")
	headSHA, err = git.HeadSHA(root)
	if err != nil {
		t.Fatalf("head HEAD: %v", err)
	}
	workspace.InvalidateLoadCache(root)
	ws, err := workspace.Load(root)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	if len(ws.Projects) != 1 {
		t.Fatalf("projects = %d, want 1", len(ws.Projects))
	}
	return root, baseSHA, headSHA, ws.Projects[0]
}

func impactPlanTestJob(project *workspace.Project) *jobs.ScheduledJob {
	return &jobs.ScheduledJob{
		Project:   project,
		Extension: &extension.ExtensionDescription{Name: "@putnami/test", Version: "1.0.0"},
		JobDef: &extension.JobDefinition{
			Name:           "lint",
			CommandName:    "lint",
			ContractDigest: "tc1:test-contract",
			Reads:          []extension.ResourceRef{{ID: extension.ResourceIDSources, Scope: extension.ResourceScopeProject}},
		},
	}
}

func writeImpactPlanFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func runImpactPlanGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func impactPlanGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output))
}
