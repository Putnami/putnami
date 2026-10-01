package engine

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	diag "go.putnami.dev/protocol/diagnostic"
	protocoljob "go.putnami.dev/protocol/job"
	workspacepb "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The resolved-selection wire block. These tests pin WHAT THE RUN
// REPORTS ABOUT ITSELF, because that is what an extension acts on: a task told
// the run was narrowed validates a subset, and a task told it was not claims a
// verdict for the whole workspace.

func wireProjects() []*workspace.Project {
	return []*workspace.Project{
		{ID: "/app", Name: "app", Path: "app", Tags: []string{"deploy"}},
		{ID: "/lib", Name: "lib", Path: "lib"},
	}
}

func TestSelectProjects_WireSelectionReportsTheUnscopedDefault(t *testing.T) {
	t.Parallel()
	ws := workspace.NewWorkspace(t.TempDir(), &workspacepb.Config{}, wireProjects())
	req := &Request{Config: &workspacepb.Config{}}
	req.Global.All = true
	req.Global.Projects = "*"

	if _, code := selectProjects(req, ws); code != ExitSuccess {
		t.Fatalf("selectProjects exit code = %d, want %d", code, ExitSuccess)
	}
	assertWireSelection(t, req.selection, protocoljob.Selection{
		Mode:       protocoljob.SelectionModeAll,
		Scoped:     false,
		ProjectIDs: []string{"/app", "/lib"},
	})
}

// `--all` is the explicit spelling of the default, not a narrowing: a run with
// it and a run without it cover the same tree, so both report mode "all".
func TestSelectProjects_WireSelectionTreatsBareAndAllAlike(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/workspace-discovery-selection", "target-selection", "flag-and-wire-selection-share-one-resolver")
	ws := workspace.NewWorkspace(t.TempDir(), &workspacepb.Config{}, wireProjects())
	req := &Request{Config: &workspacepb.Config{}}
	req.Global.Projects = "*"

	if _, code := selectProjects(req, ws); code != ExitSuccess {
		t.Fatalf("selectProjects exit code = %d, want %d", code, ExitSuccess)
	}
	if req.selection.Mode != protocoljob.SelectionModeAll || req.selection.Scoped {
		t.Fatalf("selection = %+v, want the unscoped whole-workspace projection", req.selection)
	}
}

func TestSelectProjects_WorkspaceTargetIncludesDefaultExcludedProjects(t *testing.T) {
	t.Parallel()
	ws := workspace.NewWorkspace(t.TempDir(), &workspacepb.Config{}, wireProjects())
	req := &Request{Config: &workspacepb.Config{Disable: &workspacepb.DisableConfig{Tags: []string{"deploy"}}}}
	// Release-set planning rewrites --all to this explicit workspace target so
	// the coordinator can key every member, including a publishable project
	// carrying a tag excluded from ordinary commands.
	req.Global.Projects = "*"

	selected, code := selectProjects(req, ws)
	if code != ExitSuccess {
		t.Fatalf("selectProjects exit code = %d, want %d", code, ExitSuccess)
	}
	if len(selected) != 2 {
		t.Fatalf("selected projects = %+v, want the complete workspace", selected)
	}
}

func TestSelectProjects_WireSelectionReportsAnExplicitSelector(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/workspace-discovery-selection", "target-selection", "flag-and-wire-selection-share-one-resolver")
	ws := workspace.NewWorkspace(t.TempDir(), &workspacepb.Config{}, wireProjects())
	req := &Request{Config: &workspacepb.Config{}}
	req.Global.Projects = "/lib"

	if _, code := selectProjects(req, ws); code != ExitSuccess {
		t.Fatalf("selectProjects exit code = %d, want %d", code, ExitSuccess)
	}
	assertWireSelection(t, req.selection, protocoljob.Selection{
		Mode:       protocoljob.SelectionModeProjects,
		Scoped:     true,
		ProjectIDs: []string{"/lib"},
	})
}

// A tag filter narrows the run without naming a project. Reporting it as the
// unscoped default would let a task claim workspace coverage it does not have.
func TestSelectProjects_WireSelectionReportsAFilterAsScoped(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/workspace-discovery-selection", "target-selection", "flag-and-wire-selection-share-one-resolver")
	ws := workspace.NewWorkspace(t.TempDir(), &workspacepb.Config{}, wireProjects())
	req := &Request{Config: &workspacepb.Config{}}
	req.Global.All = true
	req.Global.Projects = "*"
	req.Global.FilterTag = "deploy"

	if _, code := selectProjects(req, ws); code != ExitSuccess {
		t.Fatalf("selectProjects exit code = %d, want %d", code, ExitSuccess)
	}
	assertWireSelection(t, req.selection, protocoljob.Selection{
		Mode:       protocoljob.SelectionModeProjects,
		Scoped:     true,
		ProjectIDs: []string{"/app"},
	})
}

// The fallback branch is the ONE case where `--impacted` runs everything: the
// change set could not be computed, so the engine covers the whole workspace
// rather than report a green gate over an unverified tree.
//
// The wire block must say so. Reporting mode "impacted" here would tell an
// extension it may validate a subset of a run that actually covered everything
// — a silent UNDER-validation, in exactly the case where the baseline was
// already untrustworthy. No baseline is reported either: none produced this
// project set.
func TestSelectProjects_WireSelectionReportsTheImpactedFallbackAsAll(t *testing.T) {
	t.Parallel()
	// A temp dir with no git repository: the baseline cannot resolve, which is
	// the branch under test.
	ws := workspace.NewWorkspace(t.TempDir(), &workspacepb.Config{}, wireProjects())
	req := &Request{Config: &workspacepb.Config{}}
	req.Global.Impacted = true
	req.Global.Projects = "[impacted]"

	selected, code := selectProjects(req, ws)
	if code != ExitSuccess {
		t.Fatalf("selectProjects exit code = %d, want %d", code, ExitSuccess)
	}
	if len(selected) != 2 {
		t.Fatalf("selected projects = %d, want the whole workspace (2)", len(selected))
	}
	assertWireSelection(t, req.selection, protocoljob.Selection{
		Mode:       protocoljob.SelectionModeAll,
		Scoped:     false,
		ProjectIDs: []string{"/app", "/lib"},
	})
	if req.Global.Impacted != true {
		t.Fatal("the flag stays set; only the REPORTED selection changes")
	}
}

// The engine used to drop BaselineSource on the floor. A consumer cannot
// distrust a fallback-tier ref from the ref alone, so the tier travels with it.
func TestSelectProjects_WireSelectionCarriesTheImpactedBaselineAndTier(t *testing.T) {
	t.Parallel()
	wsRoot := t.TempDir()
	initCLISelectionGitRepo(t, wsRoot)
	runCLISelectionGit(t, wsRoot, "update-ref", "refs/remotes/origin/main", "HEAD")
	runCLISelectionGit(t, wsRoot, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	runCLISelectionGit(t, wsRoot, "checkout", "-b", "feature")

	projects := []*workspace.Project{
		{ID: "/packages/app", Name: "app", Path: "packages/app"},
		{ID: "/packages/lib", Name: "lib", Path: "packages/lib"},
	}
	ws := workspace.NewWorkspace(wsRoot, &workspacepb.Config{}, projects)
	libFile := filepath.Join(wsRoot, "packages", "lib", "lib.go")
	if err := os.MkdirAll(filepath.Dir(libFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(libFile, []byte("package lib\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := &Request{Commands: []string{"build"}, Config: &workspacepb.Config{}}
	req.Global.Impacted = true
	req.Global.Projects = "[impacted]"

	if _, code := selectProjects(req, ws); code != ExitSuccess {
		t.Fatalf("selectProjects exit code = %d, want %d", code, ExitSuccess)
	}
	assertWireSelection(t, req.selection, protocoljob.Selection{
		Mode:           protocoljob.SelectionModeImpacted,
		Scoped:         true,
		Baseline:       "origin/main",
		BaselineSource: "trunk",
		ProjectIDs:     []string{"/packages/lib"},
	})
}

// assertWireSelection compares the resolved block field by field and then runs
// it through the contract's own validator, so a block that reads right but the
// contract rejects fails here rather than at the process boundary.
func assertWireSelection(t *testing.T, got *protocoljob.Selection, want protocoljob.Selection) {
	t.Helper()
	if got == nil {
		t.Fatalf("selection = nil, want %+v", want)
	}
	if got.Mode != want.Mode {
		t.Errorf("selection.mode = %q, want %q", got.Mode, want.Mode)
	}
	if got.Scoped != want.Scoped {
		t.Errorf("selection.scoped = %t, want %t", got.Scoped, want.Scoped)
	}
	if got.Baseline != want.Baseline {
		t.Errorf("selection.baseline = %q, want %q", got.Baseline, want.Baseline)
	}
	if got.BaselineSource != want.BaselineSource {
		t.Errorf("selection.baselineSource = %q, want %q", got.BaselineSource, want.BaselineSource)
	}
	if len(got.ProjectIDs) != len(want.ProjectIDs) {
		t.Fatalf("selection.projects = %v, want %v", got.ProjectIDs, want.ProjectIDs)
	}
	for i, id := range want.ProjectIDs {
		if got.ProjectIDs[i] != id {
			t.Errorf("selection.projects[%d] = %q, want %q", i, got.ProjectIDs[i], id)
		}
	}

	ctx := &protocoljob.Context{
		ProtocolVersion: protocoljob.ProtocolVersion2,
		WorkspaceRoot:   "/ws",
		OutputPath:      "/ws/out",
		CacheRoot:       "/ws/cache",
		Workspace:       protocoljob.Workspace{Name: "ws"},
		Project:         protocoljob.Project{Name: "p", Path: "p", FullPath: "/ws/p"},
		Extension:       protocoljob.Extension{Name: "e", Root: "/e"},
		Job:             protocoljob.Job{Name: "build"},
		Identity: &protocoljob.TaskIdentity{
			Key:      "/p:build",
			Scope:    protocoljob.TaskScopeProject,
			Project:  protocoljob.ProjectIdentity{ID: "/p", Name: "p"},
			Task:     protocoljob.TaskRef{Name: "build", Command: "build", Kind: "build"},
			Provider: protocoljob.ProviderIdentity{Extension: "e"},
		},
		Params:    protocoljob.Params{},
		Selection: got,
	}
	if diags := protocoljob.Validate(ctx); diag.HasErrors(diags) {
		t.Errorf("the resolved selection violates the job context contract: %v", diags)
	}
}
