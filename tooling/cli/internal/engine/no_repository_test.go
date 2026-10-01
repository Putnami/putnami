package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	workspacepb "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
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

	// A baseline beside a target changes no selection, so it is not refused.
	t.Run("baseline beside a target", func(t *testing.T) {
		for name, mutate := range map[string]func(*GlobalFlags){
			"named project": func(g *GlobalFlags) { g.Projects = "app" },
			"--all":         func(g *GlobalFlags) { g.All, g.Projects = true, "*" },
		} {
			req := &Request{Commands: []string{"build"}, Config: &workspacepb.Config{}}
			req.Global.Baseline = "main"
			mutate(&req.Global)
			var selected []*workspace.Project
			var code int
			stderr := captureStderr(t, func() { selected, code = selectProjects(req, ws) })
			if code != ExitSuccess || len(selected) == 0 || stderr != "" {
				t.Fatalf("%s: selection = %v (code %d, stderr %q), want the target, unrefused", name, selected, code, stderr)
			}
		}
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

// readStampScript is the fixture task: it records its run in the file its
// first argument names, then copies the project's version stamp into its
// declared output. It uses shell builtins only, so it runs with no other
// program on PATH.
const readStampScript = `#!/bin/sh
echo run >> "$1"
[ -f .gen/version.json ] || exit 3
: > stamp.json
while IFS= read -r line || [ -n "$line" ]; do
  printf '%s\n' "$line" >> stamp.json
done < .gen/version.json
`

// buildWithoutRepositoryFixture writes a workspace whose app project builds
// with one cached task that reads the version stamp, at a root git discovery
// cannot escape. It returns the root and the file the task records its runs in.
func buildWithoutRepositoryFixture(t *testing.T) (string, string) {
	t.Helper()
	root := rootWithoutRepository(t)
	tools := t.TempDir()
	script := filepath.Join(tools, "read-stamp.sh")
	runs := filepath.Join(tools, "runs")
	write := func(path string, data []byte, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, mode); err != nil {
			t.Fatal(err)
		}
	}
	write(script, []byte(readStampScript), 0o755)
	enabled := true
	manifest, err := json.Marshal(extensionproto.Manifest{
		Name: "@test/read-stamp", Version: "1.0.0", CLIContract: 4,
		Commands: map[string]extensionproto.CommandDefinition{
			"build": {Run: []extensionproto.PipelineStep{{ID: "build", Task: "read-stamp"}}},
		},
		Tasks: map[string]extensionproto.TaskDefinition{
			"read-stamp": {
				Kind: "command", Command: script, Args: []string{runs}, Cwd: "{projectRoot}",
				Inputs: map[string]extensionproto.TaskInputPort{"sources": {From: "project", Files: []string{"src.txt"}}},
				Cache:  &extensionproto.TaskCachePolicy{Enabled: &enabled},
				Declares: &extensionproto.TaskDeclaration{Outputs: map[string]extensionproto.DeclaredOutput{
					"stamp": {Kind: extensionproto.OutputKindFile, Root: extensionproto.OutputRootProject, Path: "stamp.json"},
				}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(root, "putnami.workspace.json"), []byte(`{"name":"no-repository","includes":["app","extension"]}`), 0o644)
	write(filepath.Join(root, "app", "putnami.json"), []byte(`{"name":"app","extensions":["/extension"]}`), 0o644)
	write(filepath.Join(root, "app", "src.txt"), []byte("source\n"), 0o644)
	write(filepath.Join(root, "extension", "putnami.json"), []byte(`{"name":"@test/read-stamp"}`), 0o644)
	write(filepath.Join(root, "extension", "putnami.extension.json"), manifest, 0o644)
	workspace.InvalidateLoadCache(root)
	return root, runs
}

// buildOnce runs a bare build at root and returns its exit code, its stderr,
// and the session it wrote.
func buildOnce(t *testing.T, root string) (int, string, protocolcli.SessionFile) {
	t.Helper()
	before := workspace_state.NewSessionStore(root).LatestID()
	var result SessionResult
	var err error
	stderr := captureStderr(t, func() {
		result, err = New().Run(context.Background(), Request{
			WorkspaceRoot: root, Config: workspacepb.Load(root), Commands: []string{"build"}, Stdout: io.Discard,
		}, discardEvents{})
	})
	if err != nil {
		t.Fatalf("Run = %v (stderr %q)", err, stderr)
	}
	id := workspace_state.NewSessionStore(root).LatestID()
	if id == "" || id == before {
		t.Fatalf("the build wrote no session (exit %d, stderr %q)", result.ExitCode, stderr)
	}
	data, err := os.ReadFile(filepath.Join(root, ".putnami", "sessions", id, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var session protocolcli.SessionFile
	if err := json.Unmarshal(data, &session); err != nil {
		t.Fatal(err)
	}
	return result.ExitCode, stderr, session
}

// A build at a root Git does not manage runs end to end: it selects every
// project, stamps each capability package with no source claim, runs a cached
// task that reads the stamp, writes its session, and serves the next identical
// build from the local cache.
func TestRun_BuildOutsideARepositoryRunsEndToEnd(t *testing.T) {
	spectest.Proves(t, "cli/workspace-without-git", "no-source-claim",
		"a-build-outside-a-repository-runs-end-to-end")
	if runtime.GOOS == "windows" {
		t.Skip("the fixture task is a POSIX shell script")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("the fixture task runs through /bin/sh")
	}
	for name, noGitProgram := range map[string]bool{"git installed": false, "no git program": true} {
		t.Run(name, func(t *testing.T) {
			root, runs := buildWithoutRepositoryFixture(t)
			if noGitProgram {
				t.Setenv("PATH", t.TempDir())
			}

			code, stderr, session := buildOnce(t, root)
			if code != ExitSuccess || session.Run.Outcome != "success" {
				t.Fatalf("first build = exit %d, outcome %q (stderr %q), want success", code, session.Run.Outcome, stderr)
			}
			if session.Git != nil || session.Tree != nil {
				t.Fatalf("session git = %+v, tree = %+v, want neither outside a repository", session.Git, session.Tree)
			}
			data, err := os.ReadFile(filepath.Join(root, "app", "stamp.json"))
			if err != nil {
				t.Fatalf("the task wrote no copy of the stamp: %v", err)
			}
			var stamp jobs.VersionInfo
			if err := json.Unmarshal(data, &stamp); err != nil {
				t.Fatalf("the stamp the task read is not a version stamp: %v\n%s", err, data)
			}
			if len(stamp.CapabilityPackages) == 0 {
				t.Fatalf("the stamp the task read has no capability package:\n%s", data)
			}
			for _, pkg := range stamp.CapabilityPackages {
				if !pkg.SourceBindingUnavailable || pkg.SourceBinding != "" || pkg.SourceRoot == "" {
					t.Fatalf("package %s stamp = %+v, want no source claim", pkg.Package, pkg)
				}
			}

			code, stderr, session = buildOnce(t, root)
			if code != ExitSuccess || session.Run.Reuse.LocalCache != 1 {
				t.Fatalf("second build = exit %d, local cache hits %d (stderr %q), want success served from the cache",
					code, session.Run.Reuse.LocalCache, stderr)
			}
			if data, _ := os.ReadFile(runs); string(data) != "run\n" {
				t.Fatalf("task runs = %q, want one run across both builds", data)
			}
		})
	}
}

// A tree state the run captured is git answering inside the root, so the run's
// cache manager answers the source state of its keys and stamps without asking
// again. A portable request's tree state is its submitter's and proves nothing
// about the root it runs in.
func TestNewRunCacheManager_ACapturedTreeStateAnswersTheSourceState(t *testing.T) {
	spectest.Proves(t, "cli/workspace-without-git", "source-state-keys-the-cache",
		"the-key-and-the-stamp-read-one-answer")
	root := rootWithoutRepository(t)
	ws := twoProjectWorkspace(root)
	// No git program: any question the manager asks answers unmanaged.
	t.Setenv("PATH", t.TempDir())

	snapshot := &git.VersionInfo{SHA: "abc1234"}
	captured := &Request{WorkspaceRoot: root, VersionSnapshot: snapshot}
	if got := newRunCacheManager(captured, ws).SourceState(root); got != "" {
		t.Fatalf("source state after a captured tree state = %q, want empty without asking git", got)
	}
	for name, req := range map[string]*Request{
		"portable":         {WorkspaceRoot: root, VersionSnapshot: snapshot, Portable: &PortableExecution{}},
		"no tree captured": {WorkspaceRoot: root},
	} {
		if got := newRunCacheManager(req, ws).SourceState(root); got != store.SourceStateUnmanaged {
			t.Errorf("%s: source state = %q, want %q from asking git", name, got, store.SourceStateUnmanaged)
		}
	}
}
