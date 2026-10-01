package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	workspacepb "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// rootWithoutRepository returns a workspace root git discovery cannot escape,
// so the answer does not depend on where the temporary directory lives. It
// sets an environment variable, so the caller does not run in parallel.
func rootWithoutRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(root))
	return root
}

func twoProjectWorkspace(root string) *workspace.Workspace {
	return workspace.NewWorkspace(root, &workspacepb.Config{}, []*workspace.Project{
		{ID: "/app", Name: "app", Path: "app"},
		{ID: "/lib", Name: "lib", Path: "lib"},
	})
}

// oneLineRefusal asserts stderr is exactly the refusal for what: one line that
// names git and the command to run, with no raw git failure in it.
func oneLineRefusal(t *testing.T, stderr, what, root string) {
	t.Helper()
	want := "putnami: " + what + " needs git history: " + root + " is not a git repository; run git init there and commit\n"
	if stderr != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}
}

func TestSelectProjects_BareRunOutsideARepositorySelectsEveryProject(t *testing.T) {
	spectest.Proves(t, "cli/workspace-without-git", "bare-run-covers-every-project",
		"a-bare-run-outside-a-repository-selects-every-project")
	root := rootWithoutRepository(t)
	ws := twoProjectWorkspace(root)

	var notices strings.Builder
	req := &Request{Commands: []string{"build"}, Config: &workspacepb.Config{}, Stdout: &notices}
	var selected []*workspace.Project
	var code int
	stderr := captureStderr(t, func() { selected, code = selectProjects(req, ws) })
	if code != ExitSuccess {
		t.Fatalf("selectProjects exit code = %d, want %d (stderr %q)", code, ExitSuccess, stderr)
	}
	if len(selected) != 2 {
		t.Fatalf("selected = %d project(s), want both", len(selected))
	}
	if !req.Global.All || req.Global.Impacted || req.Global.Projects != "*" {
		t.Fatalf("selection flags = all:%v impacted:%v projects:%q, want every project", req.Global.All, req.Global.Impacted, req.Global.Projects)
	}
	if stderr != "" {
		t.Fatalf("a bare run outside a repository wrote %q to stderr", stderr)
	}
	if want := "all projects (no git repository) — 2 projects"; !strings.Contains(notices.String(), want) {
		t.Fatalf("notices = %q, want %q", notices.String(), want)
	}
}

func TestSelectProjects_NamedProjectOutsideARepositoryNeedsNoGit(t *testing.T) {
	spectest.Proves(t, "cli/workspace-without-git", "bare-run-covers-every-project",
		"a-named-project-outside-a-repository-needs-no-git")
	root := rootWithoutRepository(t)
	ws := twoProjectWorkspace(root)
	// No git program at all: a selection that names its project asks git nothing.
	t.Setenv("PATH", t.TempDir())

	req := &Request{Commands: []string{"build"}, Config: &workspacepb.Config{}}
	req.Global.Projects = "app"
	var selected []*workspace.Project
	var code int
	stderr := captureStderr(t, func() { selected, code = selectProjects(req, ws) })
	if code != ExitSuccess || len(selected) != 1 || selected[0].Name != "app" {
		t.Fatalf("selection = %v (code %d, stderr %q), want app alone", selected, code, stderr)
	}
}

func TestSelectProjects_ImpactedOutsideARepositoryNamesGit(t *testing.T) {
	spectest.Proves(t, "cli/workspace-without-git", "git-history-is-refused-in-one-line",
		"impacted-and-baseline-outside-a-repository-name-git")
	root := rootWithoutRepository(t)
	ws := twoProjectWorkspace(root)

	for name, mutate := range map[string]func(*GlobalFlags){
		"lenient": func(*GlobalFlags) {},
		"strict":  func(g *GlobalFlags) { g.ImpactedStrict = true },
	} {
		t.Run(name, func(t *testing.T) {
			req := &Request{Commands: []string{"build"}, Config: &workspacepb.Config{}}
			req.Global.Impacted = true
			req.Global.Projects = "[impacted]"
			mutate(&req.Global)
			var selected []*workspace.Project
			var code int
			stderr := captureStderr(t, func() { selected, code = selectProjects(req, ws) })
			if code != ExitError || selected != nil {
				t.Fatalf("selection = %v (code %d), want a refusal", selected, code)
			}
			oneLineRefusal(t, stderr, "--impacted", root)
		})
	}

	t.Run("baseline", func(t *testing.T) {
		req := &Request{Commands: []string{"build"}, Config: &workspacepb.Config{}}
		req.Global.Baseline = "main"
		var selected []*workspace.Project
		var code int
		stderr := captureStderr(t, func() { selected, code = selectProjects(req, ws) })
		if code != ExitError || selected != nil {
			t.Fatalf("selection = %v (code %d), want a refusal", selected, code)
		}
		oneLineRefusal(t, stderr, "--baseline", root)
	})
}

func TestRun_PublishAndDeployOutsideARepositoryNameGit(t *testing.T) {
	spectest.Proves(t, "cli/workspace-without-git", "git-history-is-refused-in-one-line",
		"publish-and-deploy-outside-a-repository-name-git")
	root := rootWithoutRepository(t)

	for _, commands := range [][]string{{"publish"}, {"deploy"}, {"build", "publish"}} {
		t.Run(strings.Join(commands, ","), func(t *testing.T) {
			var result SessionResult
			var err error
			stderr := captureStderr(t, func() {
				result, err = New().Run(context.Background(), Request{
					WorkspaceRoot: root, Config: &workspacepb.Config{}, Commands: commands,
				}, nil)
			})
			if result.ExitCode != ExitError || !errors.Is(err, git.ErrNotRepository) {
				t.Fatalf("Run = exit %d, %v, want a refusal for a missing repository", result.ExitCode, err)
			}
			what := "publish"
			if commands[0] == "deploy" {
				what = "deploy"
			}
			oneLineRefusal(t, stderr, what, root)
		})
	}
}

// The refusal belongs to the commands that ship a commit. Every other command
// runs where Git does not manage the root, and the guard asks git nothing for it.
func TestRequireRepository(t *testing.T) {
	root := rootWithoutRepository(t)
	request := func(commands ...string) *Request {
		return &Request{WorkspaceRoot: root, Commands: commands}
	}
	for _, commands := range [][]string{{"build"}, {"test", "lint"}, {"serve"}, nil} {
		if err := requireRepository(request(commands...)); err != nil {
			t.Errorf("requireRepository(%v) = %v, want nil", commands, err)
		}
	}
	if err := requireRepository(request("publish")); !errors.Is(err, git.ErrNotRepository) {
		t.Fatalf("requireRepository(publish) = %v, want ErrNotRepository", err)
	}

	// A preview prints its plan and ships nothing.
	plan := request("deploy")
	plan.Global.Plan = true
	dryRun := request("deploy")
	dryRun.Global.DryRun = true
	for name, preview := range map[string]*Request{"--plan": plan, "--dry-run": dryRun} {
		if err := requireRepository(preview); err != nil {
			t.Errorf("requireRepository(deploy %s) = %v, want nil", name, err)
		}
	}
	// A dry run the extension interprets executes its jobs, so it is refused.
	rehearsal := request("publish")
	rehearsal.Global.DryRun, rehearsal.ExecutesUnderDryRun = true, true
	if err := requireRepository(rehearsal); !errors.Is(err, git.ErrNotRepository) {
		t.Errorf("requireRepository(executing dry run) = %v, want ErrNotRepository", err)
	}

	// A captured tree state proves a repository answered.
	captured := request("publish")
	captured.VersionSnapshot = &git.VersionInfo{SHA: "abc1234"}
	if err := requireRepository(captured); err != nil {
		t.Errorf("requireRepository with a captured tree state = %v, want nil", err)
	}
	// A portable request carries the versions its submitter resolved, and the
	// snapshot it runs in holds no repository.
	portable := request("publish")
	portable.Portable = &PortableExecution{}
	if err := requireRepository(portable); err != nil {
		t.Errorf("requireRepository for a portable request = %v, want nil", err)
	}

	t.Setenv("PATH", t.TempDir())
	err := requireRepository(request("deploy"))
	if !errors.Is(err, git.ErrNoProgram) {
		t.Fatalf("requireRepository without a git program = %v, want ErrNoProgram", err)
	}
	want := "deploy needs git history: no git program is on PATH; install git, then run git init in " + root + " and commit"
	if err.Error() != want {
		t.Fatalf("message = %q, want %q", err.Error(), want)
	}
	if err := requireRepository(request("build")); err != nil {
		t.Errorf("requireRepository(build) without a git program = %v, want nil", err)
	}
}
