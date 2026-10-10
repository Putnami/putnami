package ci

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	ciproto "go.putnami.dev/protocol/ci"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestEmitChangePlanPinsCheckedOutHeadAndIsByteStable(t *testing.T) {
	root, baseSHA, headSHA, project := writeChangePlanRepository(t)
	calls := 0
	planner := func(_ context.Context, base string, noCache bool) (PlannerResult, error) {
		calls++
		if base != baseSHA || !noCache {
			t.Fatalf("unexpected planner inputs: base=%q noCache=%t", base, noCache)
		}
		return PlannerResult{
			Commands: []string{"lint"},
			Complete: true,
			Projects: []*workspace.Project{project},
			Jobs:     []*jobs.ScheduledJob{changePlanTestJob(project)},
		}, nil
	}

	opts := PlanOptions{Base: baseSHA, NoCache: true, CLIVersion: "test"}
	first, err := EmitChangePlan(context.Background(), root, opts, planner)
	if err != nil {
		t.Fatalf("emit first plan: %v", err)
	}
	second, err := EmitChangePlan(context.Background(), root, opts, planner)
	if err != nil {
		t.Fatalf("emit second plan: %v", err)
	}
	if calls != 2 {
		t.Fatalf("planner calls = %d, want 2", calls)
	}
	if first.BaseSHA != baseSHA || first.HeadSHA != headSHA {
		t.Fatalf("revisions = %s..%s, want %s..%s", first.BaseSHA, first.HeadSHA, baseSHA, headSHA)
	}
	if first.Repository.URL != "https://example.test/org/repo.git" || strings.Contains(first.Repository.URL, "token") {
		t.Fatalf("repository URL = %q, want credential-free URL", first.Repository.URL)
	}
	if !reflect.DeepEqual(first.ChangedFiles, []string{"app/source.txt"}) {
		t.Fatalf("changed files = %#v", first.ChangedFiles)
	}
	if len(first.Impact.DirectProjects) != 1 || first.Impact.DirectProjects[0].ID != project.ID {
		t.Fatalf("direct impact = %#v, want %s", first.Impact.DirectProjects, project.ID)
	}
	firstBytes, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal first: %v", err)
	}
	secondBytes, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("marshal second: %v", err)
	}
	if string(firstBytes) != string(secondBytes) || first.Digest != second.Digest {
		t.Fatalf("equivalent runs differ:\n%s\n%s", firstBytes, secondBytes)
	}

	// A consumer reads the plan from the structured result's data member and
	// validates it with the protocol package alone.
	printed, err := iox.CaptureStdout(func() error { return RenderChangePlan("json", first) })
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var envelope struct {
		Data ciproto.ChangePlan `json:"data"`
	}
	if err := json.Unmarshal([]byte(printed), &envelope); err != nil {
		t.Fatalf("decode printed plan: %v\n%s", err, printed)
	}
	if err := ciproto.ValidateChangePlan(envelope.Data); err != nil {
		t.Fatalf("printed plan does not validate: %v", err)
	}
	if recomputed, err := ciproto.RecomputeChangePlanDigest(envelope.Data); err != nil || recomputed != first.Digest {
		t.Fatalf("printed plan digest = %s (%v), want %s", recomputed, err, first.Digest)
	}
}

func TestEmitChangePlanNamesThePlannedCommandsWhenIncomplete(t *testing.T) {
	root, _, _, project := writeChangePlanRepository(t)
	gate := []string{"lint", "test", "build", "validate"}
	_, err := EmitChangePlan(context.Background(), root, PlanOptions{Base: "HEAD~1", NoCache: true, CLIVersion: "test"},
		func(context.Context, string, bool) (PlannerResult, error) {
			return PlannerResult{Commands: gate, Projects: []*workspace.Project{project}}, nil
		})
	want := "change-plan could not produce a complete lint,test,build,validate plan"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

func TestEmitChangePlanRejectsNonCheckoutHead(t *testing.T) {
	root, baseSHA, _, _ := writeChangePlanRepository(t)
	called := false
	_, err := EmitChangePlan(context.Background(), root, PlanOptions{Base: baseSHA, Head: baseSHA, NoCache: true, CLIVersion: "test"},
		func(context.Context, string, bool) (PlannerResult, error) {
			called = true
			return PlannerResult{}, nil
		})
	if err == nil || !strings.Contains(err.Error(), "checked-out HEAD") {
		t.Fatalf("error = %v, want checked-out HEAD rejection", err)
	}
	if called {
		t.Fatal("planner ran for a non-checked-out head")
	}
}

func writeChangePlanRepository(t *testing.T) (root, baseSHA, headSHA string, project *workspace.Project) {
	t.Helper()
	root = t.TempDir()
	writeChangePlanFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"fixture","includes":["app"]}`)
	writeChangePlanFile(t, filepath.Join(root, "app", "putnami.json"), `{"name":"app"}`)
	writeChangePlanFile(t, filepath.Join(root, "app", "source.txt"), "base\n")
	runChangePlanGit(t, root, "init")
	runChangePlanGit(t, root, "config", "user.email", "fixture@example.test")
	runChangePlanGit(t, root, "config", "user.name", "Fixture")
	runChangePlanGit(t, root, "remote", "add", "origin", "https://token:secret@example.test/org/repo.git")
	runChangePlanGit(t, root, "add", ".")
	runChangePlanGit(t, root, "commit", "-m", "base")
	var err error
	baseSHA, err = git.HeadSHA(root)
	if err != nil {
		t.Fatalf("base HEAD: %v", err)
	}
	writeChangePlanFile(t, filepath.Join(root, "app", "source.txt"), "head\n")
	runChangePlanGit(t, root, "add", "app/source.txt")
	runChangePlanGit(t, root, "commit", "-m", "head")
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

func changePlanTestJob(project *workspace.Project) *jobs.ScheduledJob {
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

func writeChangePlanFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func runChangePlanGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}
