package ci

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	ciproto "go.putnami.dev/protocol/ci"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
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
			Jobs:     []*jobs.ScheduledJob{changePlanTestJob(project)},
		}, nil
	}
}

// TestEmitImpactPlanIsTheChangePlanBeforeItsRepository holds the one
// projection end to end: the ChangePlan change-plan emits is exactly
// ChangePlanFromImpactPlan of the ImpactPlan impact-plan emits for the same
// range and planner, with the origin remote as the repository.
func TestEmitImpactPlanIsTheChangePlanBeforeItsRepository(t *testing.T) {
	spectest.Proves(t, "cli/impact-plan", "one-projection", "change-plan-is-the-projection-of-impact-plan")
	root, baseSHA, headSHA, project := writeChangePlanRepository(t)
	calls := 0
	planner := impactPlanTestPlanner(project, []string{"test", "lint"}, &calls)
	opts := PlanOptions{Base: baseSHA, NoCache: true, CLIVersion: "test"}

	impact, err := EmitImpactPlan(context.Background(), root, opts, planner)
	if err != nil {
		t.Fatalf("EmitImpactPlan: %v", err)
	}
	if err := ciproto.ValidateImpactPlan(impact); err != nil {
		t.Fatalf("ValidateImpactPlan: %v", err)
	}
	if impact.BaseSHA != baseSHA || impact.HeadSHA != headSHA || !reflect.DeepEqual(impact.Commands, []string{"test", "lint"}) {
		t.Fatalf("impact plan = %s..%s %v, want %s..%s [test lint]", impact.BaseSHA, impact.HeadSHA, impact.Commands, baseSHA, headSHA)
	}
	if !reflect.DeepEqual(impact.ChangedFiles, []string{"app/source.txt"}) || len(impact.Tasks) != 1 ||
		len(impact.Impact.DirectProjects) != 1 || impact.Impact.DirectProjects[0].ID != project.ID {
		t.Fatalf("impact plan members = %+v", impact)
	}
	if impact.Cache.Status != "disabled" {
		t.Fatalf("cache = %+v, want disabled under --no-cache", impact.Cache)
	}

	change, err := EmitChangePlan(context.Background(), root, opts, planner)
	if err != nil {
		t.Fatalf("EmitChangePlan: %v", err)
	}
	projected, err := ciproto.ChangePlanFromImpactPlan(impact, change.Repository)
	if err != nil {
		t.Fatalf("ChangePlanFromImpactPlan: %v", err)
	}
	changeBytes, err := json.Marshal(change)
	if err != nil {
		t.Fatal(err)
	}
	projectedBytes, err := json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}
	if string(changeBytes) != string(projectedBytes) {
		t.Fatalf("change-plan differs from the projection of impact-plan:\n%s\n%s", changeBytes, projectedBytes)
	}
	if calls != 2 {
		t.Fatalf("planner calls = %d, want 2", calls)
	}
}

// TestEmitImpactPlanNeedsNoRemote holds the repository to the consumer: an
// impact plan is emitted from a checkout with no origin remote, which
// change-plan refuses before it plans anything.
func TestEmitImpactPlanNeedsNoRemote(t *testing.T) {
	spectest.Proves(t, "cli/impact-plan", "pinned-revisions", "no-remote-is-needed")
	root, baseSHA, _, project := writeChangePlanRepository(t)
	runChangePlanGit(t, root, "remote", "remove", "origin")
	calls := 0
	planner := impactPlanTestPlanner(project, []string{"lint"}, &calls)
	opts := PlanOptions{Base: baseSHA, NoCache: true, CLIVersion: "test"}

	if _, err := EmitImpactPlan(context.Background(), root, opts, planner); err != nil {
		t.Fatalf("EmitImpactPlan without a remote: %v", err)
	}
	_, err := EmitChangePlan(context.Background(), root, opts, planner)
	if err == nil || err.Error() != "change-plan requires a credential-free origin remote URL" {
		t.Fatalf("EmitChangePlan error = %v, want the origin remote refusal", err)
	}
	if calls != 1 {
		t.Fatalf("planner calls = %d, want 1: change-plan must refuse before planning", calls)
	}
}

// TestEmitImpactPlanRefusalsNameImpactPlan holds every refusal of the shared
// revision checks and planning stage to the command that ran it.
func TestEmitImpactPlanRefusalsNameImpactPlan(t *testing.T) {
	spectest.Proves(t, "cli/impact-plan", "pinned-revisions", "the-range-is-refused-before-planning")
	root, baseSHA, _, project := writeChangePlanRepository(t)
	complete := func(context.Context, string, bool) (PlannerResult, error) {
		return PlannerResult{Commands: []string{"lint"}, Complete: true, Projects: []*workspace.Project{project}}, nil
	}
	plannerErr := errors.New("plan lint: engine refused the plan")
	orphanSHA := changePlanGitOutput(t, root, "commit-tree", "HEAD^{tree}", "-m", "orphan")
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

// TestEmitImpactPlanRefusesPlanningThatChangesTheCheckout holds the recheck
// after planning: a plan whose planning wrote into the worktree is refused.
func TestEmitImpactPlanRefusesPlanningThatChangesTheCheckout(t *testing.T) {
	spectest.Proves(t, "cli/impact-plan", "pinned-revisions", "planning-that-changes-the-checkout-is-refused")
	root, baseSHA, _, _ := writeChangePlanRepository(t)
	_, err := EmitImpactPlan(context.Background(), root, PlanOptions{Base: baseSHA},
		func(context.Context, string, bool) (PlannerResult, error) {
			writeChangePlanFile(t, filepath.Join(root, "app", "generated.txt"), "written by planning\n")
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
	root, baseSHA, _, project := writeChangePlanRepository(t)
	writeChangePlanFile(t, filepath.Join(root, "app", "source.txt"), "uncommitted\n")
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
	root, baseSHA, headSHA, project := writeChangePlanRepository(t)
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

func changePlanGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output))
}
