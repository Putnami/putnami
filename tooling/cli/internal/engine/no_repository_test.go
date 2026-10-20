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
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
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

// declaringWorkspace writes, at root, a workspace whose one extension declares
// build, test, lint and serve without traits, publish with registry side
// effects and deploy with cloud side effects. The core gives no command a side
// effect, so these declarations are the only ones the repository guard reads.
func declaringWorkspace(t *testing.T, root string) *workspacepb.Config {
	t.Helper()
	step := func(id string) []extensionproto.PipelineStep {
		return []extensionproto.PipelineStep{{ID: id, Task: "noop"}}
	}
	manifest, err := json.Marshal(extensionproto.Manifest{
		Name: "@test/ship", Version: "1.0.0", CLIContract: 4,
		Commands: map[string]extensionproto.CommandDefinition{
			"build": {Run: step("build")},
			"test":  {Run: step("test")},
			"lint":  {Run: step("lint")},
			"serve": {Run: step("serve")},
			"publish": {Run: step("publish"), Traits: &extensionproto.CommandTraits{
				InjectPublishChannels: true, SideEffects: extensionproto.SideEffectsRegistry,
			}},
			"deploy": {Run: step("deploy"), Traits: &extensionproto.CommandTraits{SideEffects: extensionproto.SideEffectsCloud}},
		},
		Tasks: map[string]extensionproto.TaskDefinition{
			"noop": {Kind: "command", Command: "true", Cwd: "{projectRoot}", TimeoutMs: -1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string][]byte{
		"putnami.workspace.json":           []byte(`{"name":"ship","includes":["app","extension"],"extensions":["/extension"]}`),
		"app/putnami.json":                 []byte(`{"name":"app","extensions":["/extension"]}`),
		"extension/putnami.json":           []byte(`{"name":"@test/ship"}`),
		"extension/putnami.extension.json": manifest,
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	workspace.InvalidateLoadCache(root)
	return workspacepb.Load(root)
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
		var code int
		stderr := captureStderr(t, func() { code = applyStandaloneBaseline(req, ws) })
		if code != ExitError {
			t.Fatalf("applyStandaloneBaseline exit code = %d, want a refusal", code)
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
			stderr := captureStderr(t, func() {
				if code = applyStandaloneBaseline(req, ws); code == ExitSuccess {
					selected, code = selectProjects(req, ws)
				}
			})
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
	cfg := declaringWorkspace(t, root)

	for _, commands := range [][]string{{"publish"}, {"deploy"}, {"build", "publish"}} {
		t.Run(strings.Join(commands, ","), func(t *testing.T) {
			var result SessionResult
			var err error
			stderr := captureStderr(t, func() {
				result, err = New().Run(context.Background(), Request{
					WorkspaceRoot: root, Config: cfg, Commands: commands,
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

// A refusal that Run returns is reported on Request.ErrorOut, once. An
// adapter that reports the returned error itself discards that report, and
// stderr then carries none, whichever stage refused.
func TestRun_ReportsAReturnedErrorOnErrorOut(t *testing.T) {
	root := rootWithoutRepository(t)
	cfg := declaringWorkspace(t, root)

	for name, commands := range map[string][]string{
		"a command no extension declares": {"bundle"},
		"a repository the command needs":  {"publish"},
	} {
		t.Run(name, func(t *testing.T) {
			run := func(out io.Writer) (string, error) {
				var err error
				stderr := captureStderr(t, func() {
					_, err = New().Run(context.Background(), Request{
						WorkspaceRoot: root, Config: cfg, Commands: commands, ErrorOut: out,
					}, nil)
				})
				return stderr, err
			}

			stderr, err := run(io.Discard)
			if err == nil || stderr != "" {
				t.Fatalf("with io.Discard: error = %v, stderr = %q, want the error returned and nothing on stderr", err, stderr)
			}

			var reported strings.Builder
			stderr, err = run(&reported)
			if err == nil || stderr != "" || reported.String() != "putnami: "+err.Error()+"\n" {
				t.Fatalf("with a writer: error = %v, stderr = %q, report = %q, want the error reported once on the writer", err, stderr, reported.String())
			}

			stderr, err = run(nil)
			if err == nil || stderr != "putnami: "+err.Error()+"\n" {
				t.Fatalf("with no writer: error = %v, stderr = %q, want the report on stderr", err, stderr)
			}
		})
	}
}

// When the run fails and an after-hook fails too, Run returns the run's error
// and not the hook's, so the hook error goes to stderr and never to ErrorOut:
// an adapter that discards ErrorOut still shows the user the hook failure.
func TestRun_AHookErrorTheRunDoesNotReturnReachesStderr(t *testing.T) {
	root := rootWithoutRepository(t)
	cfg := declaringWorkspace(t, root)
	hooks := &workspacepb.HooksConfig{Commands: map[string]*workspacepb.HookPhaseConfig{
		"bundle": {After: []string{"false"}},
	}}
	if runtime.GOOS == "windows" {
		t.Skip("hooks need sh")
	}

	var reported strings.Builder
	var err error
	stderr := captureStderr(t, func() {
		_, err = New().Run(context.Background(), Request{
			WorkspaceRoot: root, Config: cfg, Commands: []string{"bundle"}, Hooks: hooks, ErrorOut: &reported,
		}, nil)
	})
	if err == nil || !strings.Contains(err.Error(), "no extension of this workspace declares bundle") {
		t.Fatalf("Run error = %v, want the refusal of bundle", err)
	}
	if want := "putnami: " + err.Error() + "\n"; reported.String() != want {
		t.Fatalf("ErrorOut = %q, want only the returned error %q", reported.String(), want)
	}
	if strings.Count(stderr, "hook after error") != 1 {
		t.Fatalf("stderr = %q, want the hook error once", stderr)
	}
}

// shippingWorkspace writes a workspace at a root git discovery cannot escape.
// Its local extension serves build, and publish with registry side effects
// too when servesPublish is set, with a task that records each run in the
// returned runs file. declareCloud
// adds the registry extension @putnami/cloud, which is never installed.
func shippingWorkspace(t *testing.T, servesPublish, declareCloud bool) (root, runs string) {
	t.Helper()
	root = rootWithoutRepository(t)
	tools := t.TempDir()
	runs = filepath.Join(tools, "runs")
	script := filepath.Join(tools, "ship.sh")
	write := func(path string, data []byte, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, mode); err != nil {
			t.Fatal(err)
		}
	}
	write(script, []byte("#!/bin/sh\necho run >> \"$1\"\n"), 0o755)
	manifest := extensionproto.Manifest{
		Name: "@test/ship", Version: "1.0.0", CLIContract: 4,
		Commands: map[string]extensionproto.CommandDefinition{
			"build": {Run: []extensionproto.PipelineStep{{ID: "build", Task: "ship"}}},
		},
		Tasks: map[string]extensionproto.TaskDefinition{
			"ship": {Kind: "command", Command: script, Args: []string{runs}, Cwd: "{projectRoot}"},
		},
	}
	if servesPublish {
		manifest.Commands["publish"] = extensionproto.CommandDefinition{
			Run:    []extensionproto.PipelineStep{{ID: "publish", Task: "ship"}},
			Traits: &extensionproto.CommandTraits{SideEffects: extensionproto.SideEffectsRegistry},
		}
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	extensions := `"/extension"`
	if declareCloud {
		extensions += `,"@putnami/cloud"`
	}
	write(filepath.Join(root, "putnami.workspace.json"), []byte(`{"name":"ship","includes":["app","extension"],"extensions":[`+extensions+`]}`), 0o644)
	write(filepath.Join(root, "app", "putnami.json"), []byte(`{"name":"app","extensions":["/extension"]}`), 0o644)
	write(filepath.Join(root, "extension", "putnami.json"), []byte(`{"name":"@test/ship"}`), 0o644)
	write(filepath.Join(root, "extension", "putnami.extension.json"), data, 0o644)
	workspace.InvalidateLoadCache(root)
	return root, runs
}

// A publish whose own extension is not installed reports that extension, and
// no repository refusal: nothing loaded declares that it ships a commit, so
// the run reaches the missing-extension guard of planning. With its extension
// present, the refusal stands, also beside a missing extension the command
// does not need, and no hook runs. Either way no job runs.
func TestRun_PublishOutsideARepositoryReportsAMissingExtensionFirst(t *testing.T) {
	spectest.Proves(t, "cli/workspace-without-git", "git-history-is-refused-in-one-line",
		"publish-and-deploy-outside-a-repository-name-git")
	requireSh(t)
	for _, tc := range []struct {
		name                        string
		servesPublish, declareCloud bool
		wantMissing                 bool
	}{
		{name: "its extension is missing", declareCloud: true, wantMissing: true},
		{name: "every extension is present", servesPublish: true},
		{name: "an extension it does not need is missing", servesPublish: true, declareCloud: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, runs := shippingWorkspace(t, tc.servesPublish, tc.declareCloud)
			hookRuns := filepath.Join(t.TempDir(), "hook-runs")
			var result SessionResult
			var err error
			stderr := captureStderr(t, func() {
				result, err = New().Run(context.Background(), Request{
					WorkspaceRoot: root, Config: workspacepb.Load(root), Commands: []string{"publish"},
					Global: GlobalFlags{All: true, Projects: "*"}, Stdout: io.Discard,
					Hooks: &workspacepb.HooksConfig{CLI: &workspacepb.HookPhaseConfig{
						Before: []string{"echo ran >> " + shPath(hookRuns)},
					}},
				}, discardEvents{})
			})
			if result.ExitCode != ExitError {
				t.Fatalf("Run = exit %d, %v (stderr %q), want %d", result.ExitCode, err, stderr, ExitError)
			}
			if tc.wantMissing {
				if err != nil || !strings.Contains(stderr, "@putnami/cloud") ||
					strings.Count(stderr, "run `putnami install`") != 1 || strings.Contains(stderr, "needs git history") {
					t.Fatalf("Run = %v, stderr %q, want the missing @putnami/cloud with one install hint and no refusal", err, stderr)
				}
			} else {
				if !errors.Is(err, git.ErrNotRepository) {
					t.Fatalf("Run = %v, want the refusal for a missing repository", err)
				}
				oneLineRefusal(t, stderr, "publish", root)
			}
			stopped := map[string]string{"a job": runs}
			if !tc.wantMissing {
				stopped["a before hook"] = hookRuns
			}
			for what, path := range stopped {
				if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
					t.Errorf("%s ran before the run stopped", what)
				}
			}
		})
	}
}

// The refusal belongs to the commands that ship a commit. Every other command
// runs where Git does not manage the root, and the guard asks git nothing for it.
func TestRequireRepository(t *testing.T) {
	root := rootWithoutRepository(t)
	cfg := declaringWorkspace(t, root)
	request := func(commands ...string) *Request {
		return &Request{WorkspaceRoot: root, Config: cfg, Commands: commands}
	}
	for _, commands := range [][]string{{"build"}, {"test", "lint"}, {"serve"}, nil} {
		if _, err := requireRepository(request(commands...)); err != nil {
			t.Errorf("requireRepository(%v) = %v, want nil", commands, err)
		}
	}
	if _, err := requireRepository(request("publish")); !errors.Is(err, git.ErrNotRepository) {
		t.Fatalf("requireRepository(publish) = %v, want ErrNotRepository", err)
	}

	// A preview prints its plan and ships nothing.
	plan := request("deploy")
	plan.Global.Plan = true
	dryRun := request("deploy")
	dryRun.Global.DryRun = true
	for name, preview := range map[string]*Request{"--plan": plan, "--dry-run": dryRun} {
		if _, err := requireRepository(preview); err != nil {
			t.Errorf("requireRepository(deploy %s) = %v, want nil", name, err)
		}
	}
	// A dry run the extension interprets executes its jobs, so it is refused.
	rehearsal := request("publish")
	rehearsal.Global.DryRun, rehearsal.ExecutesUnderDryRun = true, true
	if _, err := requireRepository(rehearsal); !errors.Is(err, git.ErrNotRepository) {
		t.Errorf("requireRepository(executing dry run) = %v, want ErrNotRepository", err)
	}

	// A captured tree state proves a repository answered.
	captured := request("publish")
	captured.VersionSnapshot = &git.VersionInfo{SHA: "abc1234"}
	if _, err := requireRepository(captured); err != nil {
		t.Errorf("requireRepository with a captured tree state = %v, want nil", err)
	}
	// A portable request carries the versions its submitter resolved, and the
	// snapshot it runs in holds no repository.
	portable := request("publish")
	portable.Portable = &PortableExecution{}
	if _, err := requireRepository(portable); err != nil {
		t.Errorf("requireRepository for a portable request = %v, want nil", err)
	}

	t.Setenv("PATH", t.TempDir())
	_, err := requireRepository(request("deploy"))
	if !errors.Is(err, git.ErrNoProgram) {
		t.Fatalf("requireRepository without a git program = %v, want ErrNoProgram", err)
	}
	want := "deploy needs git history: no git program is on PATH; install git, then run git init in " + root + " and commit"
	if err.Error() != want {
		t.Fatalf("message = %q, want %q", err.Error(), want)
	}
	if _, err := requireRepository(request("build")); err != nil {
		t.Errorf("requireRepository(build) without a git program = %v, want nil", err)
	}
}

// The refusal follows the side effects a command's jobs declare, not its name,
// and the core gives no command a side effect. A verb of the extension's own
// that declares registry side effects is refused; a publish whose declared
// traits name none is not; deploy, which no loaded extension declares, is not
// either: the run refuses it as a command no extension declares.
func TestRequireRepository_FollowsOnlyTheDeclaredSideEffects(t *testing.T) {
	spectest.Proves(t, "cli/workspace-without-git", "git-history-is-refused-in-one-line",
		"a-declared-side-effect-outside-a-repository-names-git")
	spectest.Proves(t, "cli/job-planning-execution", "vocabulary-default-traits", "side-effects-come-only-from-loaded-jobs")
	root := rootWithoutRepository(t)
	step := func(id string) []extensionproto.PipelineStep {
		return []extensionproto.PipelineStep{{ID: id, Task: "noop"}}
	}
	manifest, err := json.Marshal(extensionproto.Manifest{
		Name: "@test/release", Version: "1.0.0", CLIContract: 4,
		Commands: map[string]extensionproto.CommandDefinition{
			"build": {Run: step("build")},
			"release": {Run: step("release"), Traits: &extensionproto.CommandTraits{
				SideEffects: extensionproto.SideEffectsRegistry,
			}},
			"publish": {Run: step("publish"), Traits: &extensionproto.CommandTraits{InjectPublishChannels: true}},
		},
		Tasks: map[string]extensionproto.TaskDefinition{
			"noop": {Kind: "command", Command: "true", Cwd: "{projectRoot}"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string][]byte{
		"putnami.workspace.json":           []byte(`{"name":"release","includes":["app","extension"],"extensions":["/extension"]}`),
		"app/putnami.json":                 []byte(`{"name":"app","extensions":["/extension"]}`),
		"extension/putnami.json":           []byte(`{"name":"@test/release"}`),
		"extension/putnami.extension.json": manifest,
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	workspace.InvalidateLoadCache(root)
	request := func(commands ...string) *Request {
		return &Request{WorkspaceRoot: root, Config: workspacepb.Load(root), Commands: commands}
	}

	for _, commands := range [][]string{{"release"}, {"build", "release"}} {
		_, err := requireRepository(request(commands...))
		what := commands[len(commands)-1]
		if !errors.Is(err, git.ErrNotRepository) || !strings.HasPrefix(err.Error(), what+" needs git history: ") {
			t.Errorf("requireRepository(%v) = %v, want the refusal naming %s", commands, err, what)
		}
	}
	for _, commands := range [][]string{{"build"}, {"publish"}, {"build", "publish"}, {"deploy"}} {
		if _, err := requireRepository(request(commands...)); err != nil {
			t.Errorf("requireRepository(%v) = %v, want nil: no job of the run declares a side effect", commands, err)
		}
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
			var stamp scheduler.VersionInfo
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
