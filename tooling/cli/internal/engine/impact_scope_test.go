package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	modeljobs "go.putnami.dev/cli/model/jobs"
	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/jobs/planner"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/output"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// scopedJob is a planned job whose command declares no traits and takes none
// from a vocabulary: the core gives no command a trait, whatever its name.
func scopedJob(project *workspace.Project, extRelPath, name string, predecessors ...string) *scheduler.ScheduledJob {
	return &scheduler.ScheduledJob{
		Project:   project,
		Extension: &extension.ExtensionDescription{Name: "@fixture/" + filepath.Base(extRelPath), RelPath: extRelPath},
		JobDef:    &extension.JobDefinition{Name: name},
		DependsOn: predecessors,
	}
}

// ofKind gives a planned job the command kind its command's resolved traits
// declare.
func ofKind(kind string, job *scheduler.ScheduledJob) *scheduler.ScheduledJob {
	job.JobDef.Traits.Kind = kind
	return job
}

// withSideEffects gives job the side effects its command declares.
func withSideEffects(job *scheduler.ScheduledJob, sideEffects string) *scheduler.ScheduledJob {
	job.JobDef.Traits.SideEffects = sideEffects
	job.JobDef.TraitsDeclared = true
	return job
}

// A task-scoped project keeps the jobs of the extensions in its scope and
// whatever those depend on, however many hops away and whichever extension
// owns the predecessor; a full project keeps everything; a command that
// declares side effects is never narrowed. The kept plan is a closed graph.
func TestNarrowToTaskScopes_KeepsTheScopedExtensionsJobsAndTheirPredecessors(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "changed-files-select-owners-and-transitive-dependents")
	goExt := &workspace.Project{ID: "/go/extension", Name: "@putnami/go", Path: "go/extension"}
	tsExt := &workspace.Project{ID: "/typescript/extension", Name: "@putnami/typescript", Path: "typescript/extension"}
	lib := &workspace.Project{ID: "/lib", Name: "lib", Path: "lib"}
	app := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
	full := &workspace.Project{ID: "/full", Name: "full", Path: "full"}
	ws := workspace.NewWorkspace("/workspace", nil, []*workspace.Project{goExt, tsExt, lib, app, full})

	planned := []*scheduler.ScheduledJob{
		// lib: scoped to the Go extension. Its Go test depends on a TS
		// generate step, which is kept through the closure.
		scopedJob(lib, "typescript/extension", "generate"),
		scopedJob(lib, "go/extension", "build", "/lib:generate"),
		scopedJob(lib, "go/extension", "test", "/lib:build"),
		scopedJob(lib, "typescript/extension", "lint"),
		scopedJob(lib, "typescript/extension", "check"),
		// app: scoped to the TS extension; its publish declares registry side
		// effects and keeps the Go build it depends on, while the Go test is
		// dropped, and so is a deploy that declares no side effect: its name
		// gives it none.
		scopedJob(app, "go/extension", "build"),
		scopedJob(app, "typescript/extension", "bundle"),
		scopedJob(app, "go/extension", "test", "/app:build"),
		withSideEffects(scopedJob(app, "go/extension", "publish", "/app:build"), extensionproto.SideEffectsRegistry),
		scopedJob(app, "go/extension", "deploy", "/app:build"),
		// full: not in the scope map, everything runs.
		scopedJob(full, "go/extension", "build"),
		scopedJob(full, "typescript/extension", "lint"),
		nil,
	}
	scopes := map[string][]string{"/lib": {"/go/extension"}, "/app": {"/typescript/extension"}}

	narrowed := narrowToTaskScopes(ws, planned, scopes)

	want := []string{
		"/lib:generate", "/lib:build", "/lib:test",
		"/app:build", "/app:bundle", "/app:publish",
		"/full:build", "/full:lint",
	}
	var got []string
	for _, job := range narrowed {
		if job != nil {
			got = append(got, job.Key())
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("narrowed = %v, want %v", got, want)
	}
	kept := make(map[string]bool)
	for _, key := range got {
		kept[key] = true
	}
	for _, job := range narrowed {
		if job == nil {
			continue
		}
		for _, predecessor := range job.SchedulingPredecessors() {
			if !kept[predecessor] {
				t.Errorf("kept job %s depends on dropped job %s", job.Key(), predecessor)
			}
		}
	}
	if len(narrowed) != len(want)+1 {
		t.Errorf("narrowed has %d entries, want %d kept jobs and the nil entry", len(narrowed), len(want)+1)
	}

	// No scope: the plan is returned as is, the same slice.
	if same := narrowToTaskScopes(ws, planned, nil); len(same) != len(planned) || &same[0] != &planned[0] {
		t.Error("an unscoped selection must return the plan untouched")
	}
	// A registry extension shares the project's name and has no RelPath: it
	// is not the workspace project the scope names.
	foreign := scopedJob(lib, "", "build")
	foreign.Extension.Name = "@putnami/go"
	if got := narrowToTaskScopes(ws, []*scheduler.ScheduledJob{foreign}, scopes); len(got) != 0 {
		t.Errorf("a registry extension named like the scoped project was kept: %v", got)
	}
	// A registry build pinned over that project replaces it: the scope that
	// names the project keeps the pinned build's jobs.
	pinned := scopedJob(lib, "", "build")
	pinned.Extension.Name, pinned.Extension.PinnedOver = "@putnami/go", "go/extension"
	if got := narrowToTaskScopes(ws, []*scheduler.ScheduledJob{pinned}, scopes); len(got) != 1 {
		t.Errorf("a registry build pinned over the scoped project was dropped: %v", got)
	}
}

// A task-level scope (ADR 0044) keeps the jobs it names and their predecessors,
// whichever extension owns them. An entry qualified by an extension project id
// additionally requires the job to be that extension's, so a rebuilt tool does
// not keep another extension's job of the same command and step.
func TestNarrowToTaskScopes_MatchesTheJobsCommandAndStep(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "changed-files-select-owners-and-transitive-dependents")
	goExt := &workspace.Project{ID: "/go/extension", Name: "@putnami/go", Path: "go/extension"}
	tsExt := &workspace.Project{ID: "/typescript/extension", Name: "@putnami/typescript", Path: "typescript/extension"}
	lib := &workspace.Project{ID: "/lib", Name: "lib", Path: "lib"}
	tool := &workspace.Project{ID: "/tool", Name: "tool", Path: "tool"}
	ws := workspace.NewWorkspace("/workspace", nil, []*workspace.Project{goExt, tsExt, lib, tool})

	planned := []*scheduler.ScheduledJob{
		// lib runs its tests: the scope names test~test, and the closure keeps
		// the generate it sits behind whichever extension owns it.
		scopedJob(lib, "typescript/extension", "build~generate"),
		scopedJob(lib, "go/extension", "test~test", "/lib:build~generate"),
		scopedJob(lib, "go/extension", "build~compile"),
		scopedJob(lib, "typescript/extension", "lint~check"),
		// tool runs the Go extension's tasks because that tool was rebuilt.
		// Its TypeScript job of the SAME command and step is not that tool's;
		// the planner namespaces the display name, and the scope matches on the
		// command and step, so only the qualifier tells the two apart.
		scopedJob(tool, "go/extension", "build~generate"),
		namespacedJob(tool, "typescript/extension", "build", "generate"),
		scopedJob(tool, "go/extension", "test~test"),
	}
	scopes := map[string][]string{
		"/lib":  {"test~test"},
		"/tool": {"/go/extension#build~generate"},
	}

	var got []string
	for _, job := range narrowToTaskScopes(ws, planned, scopes) {
		got = append(got, job.Key())
	}
	want := []string{"/lib:build~generate", "/lib:test~test", "/tool:build~generate"}
	if !slices.Equal(got, want) {
		t.Fatalf("narrowed = %v, want %v", got, want)
	}
}

// namespacedJob is a job the planner namespaced because two extensions serve
// one command on one project: its display name carries the extension, while its
// command and step stay the manifest's.
func namespacedJob(project *workspace.Project, extRelPath, command, step string) *scheduler.ScheduledJob {
	job := scopedJob(project, extRelPath, command+"~"+filepath.Base(extRelPath)+":"+step)
	job.JobDef.CommandName = command
	job.JobDef.StepID = step
	return job
}

// The end-to-end shape of the fix: a one-line change to an extension's
// implementation runs, on every project under it, that extension's jobs and
// nothing else. Two extensions serve two commands here; the changed one is
// `build`, and no `lint` job is planned on the consumers even though the
// run asks for both.
func TestRunPlansOnlyTheChangedExtensionsJobsOnItsConsumers(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "changed-files-select-owners-and-transitive-dependents")
	root := t.TempDir()
	write := func(rel, content string, mode os.FileMode) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	// The task declares what it reads, so the run builds a task index and the
	// scope is stated in tasks. The extension's own implementation.txt stands
	// for its implementation, and no task declares it, which is what keeps the
	// extension full.
	task := taskCommand(t, fixtureproc.Program{})
	manifest := func(name, command string) string {
		return `{"name":"` + name + `","version":"1.0.0","cliContract":4,` +
			`"commands":{"` + command + `":{"run":[{"id":"` + command + `","task":"mark"}]}},` +
			`"tasks":{"mark":{"kind":"command","command":` + task + `,"cache":false,` +
			`"inputs":{"sources":{"from":"project","files":["**/*.src"]}}}}}`
	}
	write(".gitignore", ".putnami/\n", 0o644)
	write("putnami.workspace.json", `{"name":"task-scope","includes":["app","lib","builder","linter"]}`, 0o644)
	write("lib/putnami.json", `{"name":"lib","extensions":["/builder","/linter"]}`, 0o644)
	write("app/putnami.json", `{"name":"app","dependencies":["lib"],"extensions":["/builder","/linter"]}`, 0o644)
	write("builder/putnami.json", `{"name":"@fixture/builder"}`, 0o644)
	write("builder/putnami.extension.json", manifest("@fixture/builder", "build"), 0o644)
	write("builder/implementation.txt", "v1\n", 0o644)
	write("linter/putnami.json", `{"name":"@fixture/linter"}`, 0o644)
	write("linter/putnami.extension.json", manifest("@fixture/linter", "lint"), 0o644)
	write("linter/implementation.txt", "v1\n", 0o644)
	initCLISelectionGitRepo(t, root)
	runCLISelectionGit(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
	runCLISelectionGit(t, root, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	runCLISelectionGit(t, root, "checkout", "-b", "feature")
	// The implementation change: one line in the builder's implementation.
	write("builder/implementation.txt", "v2\n", 0o644)
	workspace.InvalidateLoadCache(root)

	var live bytes.Buffer
	renderer := output.NewRenderer(output.Config{Output: "jsonl", Command: "build", Out: &live, Err: &live})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := New().Run(ctx, Request{
		WorkspaceRoot: root, Config: wsproto.Load(root), Commands: []string{"lint", "build"},
		Global: GlobalFlags{Output: "jsonl", Impacted: true, Projects: impactedProjectsSentinel, NoCache: true, MaxParallel: 1},
	}, renderer)
	if err != nil || result.ExitCode != ExitSuccess {
		t.Fatalf("impacted engine run: exit=%d err=%v\n%s", result.ExitCode, err, live.String())
	}

	var started []string
	var trace workspace.ImpactTraceRecord
	for _, line := range strings.Split(strings.TrimSpace(live.String()), "\n") {
		var record protocolcli.SessionStreamRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode live record %q: %v", line, err)
		}
		switch {
		case record.Record == protocolcli.RecordTaskStart && record.Identity != nil:
			started = append(started, record.Identity.Key)
		case record.Event["type"] == workspace.ImpactTraceRecordType:
			encoded, _ := json.Marshal(record.Event)
			if err := json.Unmarshal(encoded, &trace); err != nil {
				t.Fatalf("decode the trace %s: %v", encoded, err)
			}
		}
	}
	slices.Sort(started)
	if want := []string{"/app:build~build", "/lib:build~build"}; !slices.Equal(started, want) {
		t.Errorf("tasks started = %v, want %v: the builder's jobs on both consumers and no lint", started, want)
	}
	// The builder's implementation is not the input of any task, so the
	// extension stays full and its consumers are scoped to the tasks the
	// extension declares, named as that extension runs them.
	builderBuild := []string{"/builder#build~build"}
	if want := []workspace.ImpactTraceScope{{Project: "/app", Tasks: builderBuild}, {Project: "/lib", Tasks: builderBuild}}; !reflect.DeepEqual(trace.Scopes, want) {
		t.Errorf("trace scopes = %+v, want %+v", trace.Scopes, want)
	}
	if want := []string{"/app", "/builder", "/lib"}; !slices.Equal(slices.Sorted(slices.Values(projectIDs(result.Projects))), want) {
		t.Errorf("selected projects = %v, want %v", projectIDs(result.Projects), want)
	}
}

func projectIDs(projects []*workspace.Project) []string {
	ids := make([]string, 0, len(projects))
	for _, p := range projects {
		ids = append(ids, p.ID)
	}
	return ids
}

// A task-scoped project keeps the jobs of every command of the criteria kind,
// whatever the command is called. Their steps read the verification reports
// the session recorded rather than files, so no changed file ever names them
// and narrowing would retire the spec gate for exactly the projects task-level
// selection newly creates. The name alone keeps nothing: a `validate` command
// without the kind is narrowed like any other.
func TestNarrowToTaskScopes_NeverNarrowsACriteriaKindCommand(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "a-criteria-kind-command-is-never-narrowed")
	criteria := extensionproto.CommandKindCriteria
	project := &workspace.Project{ID: "/libs/http", Name: "http", Path: "libs/http"}
	ws := workspace.NewWorkspace("/ws", nil, []*workspace.Project{project})
	planned := []*scheduler.ScheduledJob{
		namespacedJob(project, "go/extension", "build", "compile"),
		ofKind(extensionproto.CommandKindVerification, namespacedJob(project, "go/extension", "test", "test")),
		ofKind(criteria, namespacedJob(project, "tooling/sdd-extension", "validate", "features")),
		ofKind(criteria, namespacedJob(project, "tooling/sdd-extension", "validate", "specs")),
		ofKind(criteria, namespacedJob(project, "acme/extension", "audit", "rules")),
		namespacedJob(project, "legacy/extension", "validate", "lint"),
	}
	scopes := map[string][]string{"/libs/http": {"build~compile"}}

	kept := narrowToTaskScopes(ws, planned, scopes)

	var names []string
	for _, job := range kept {
		names = append(names, jobTaskScope(job))
	}
	if want := []string{"build~compile", "validate~features", "validate~specs", "audit~rules"}; !slices.Equal(names, want) {
		t.Errorf("narrowed plan = %v, want %v: the scope names build~compile, and the criteria commands are never narrowed", names, want)
	}
}

// declaredJob is a pipeline step whose manifest task carries a v3 declaration,
// the shape the spec gate's producer predicates read.
func declaredJob(project *workspace.Project, extRelPath, command, step string,
	declaration *extension.TaskDeclaration, predecessors ...string) *scheduler.ScheduledJob {
	job := scopedJob(project, extRelPath, command+"~"+step, predecessors...)
	task := command + "-" + step
	job.JobDef.CommandName, job.JobDef.StepID = command, step
	job.Step = &extension.PipelineStep{ID: step, Task: task}
	job.Extension.Tasks = map[string]extension.TaskDefinition{task: {Kind: "command", Declares: declaration}}
	return job
}

// declaresFile is a v3 declaration of one command-output file.
func declaresFile(path string) *extension.TaskDeclaration {
	return &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
		"file": {Kind: extension.OutputKindFile, Root: extension.OutputRootCommandOutput, Path: path},
	}}
}

// A task-scoped project whose kept criteria command projects its spec criteria
// keeps its verification command, whose reports those criteria are judged
// against: the step that writes them, its predecessors, and the finalizer that
// tears down its environment. The spec gate then reads the same reports a full
// plan gives it. A project whose kept plan projects no criteria still drops
// the tests its scope does not name, and so does a command of another kind.
func TestNarrowToTaskScopes_KeepsTheTestsAKeptSpecGateJudges(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "changed-files-select-owners-and-transitive-dependents")
	sdd := &workspace.Project{ID: "/tooling/sdd-extension", Name: "@putnami/sdd", Path: "tooling/sdd-extension"}
	http := &workspace.Project{ID: "/libs/http", Name: "http", Path: "libs/http"}
	core := &workspace.Project{ID: "/libs/core", Name: "core", Path: "libs/core"}
	ws := workspace.NewWorkspace("/ws", nil, []*workspace.Project{sdd, http, core})
	report := declaresFile(features.VerificationReportFilename)
	projection := declaresFile(features.SpecCriteriaProjectionFilename)
	verification, criteria := extensionproto.CommandKindVerification, extensionproto.CommandKindCriteria
	plan := func(project *workspace.Project, withSpecs bool) []*scheduler.ScheduledJob {
		id := project.ID
		planned := []*scheduler.ScheduledJob{
			declaredJob(project, "go/extension", "build", "compile", nil),
			ofKind(verification, declaredJob(project, "go/extension", "test", "generate", nil)),
			ofKind(verification, declaredJob(project, "go/extension", "test", "test", report, id+":build~compile", id+":test~generate")),
			ofKind(verification, declaredJob(project, "go/extension", "test", "test-env-teardown", nil)),
			ofKind(extensionproto.CommandKindDiagnostics, declaredJob(project, "go/extension", "lint", "check", nil)),
			ofKind(criteria, declaredJob(project, "tooling/sdd-extension", "validate", "features", nil)),
		}
		if withSpecs {
			planned = append(planned, ofKind(criteria, declaredJob(project, "tooling/sdd-extension", "validate", "specs", projection)))
		}
		return planned
	}
	planned := append(plan(http, true), plan(core, false)...)
	sddTasks := []string{"/tooling/sdd-extension#validate~features", "/tooling/sdd-extension#validate~specs"}
	scopes := map[string][]string{"/libs/http": sddTasks, "/libs/core": sddTasks}

	var got []string
	for _, job := range narrowToTaskScopes(ws, planned, scopes) {
		got = append(got, job.Key())
	}
	want := []string{
		"/libs/http:build~compile", "/libs/http:test~generate", "/libs/http:test~test", "/libs/http:test~test-env-teardown",
		"/libs/http:validate~features", "/libs/http:validate~specs",
		"/libs/core:validate~features",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("narrowed = %v, want %v", got, want)
	}
}

// The end-to-end shape of the spec gate rule: a change to the extension that
// provides the spec gate reaches its consumers for that extension's tasks only,
// and each consumer still runs the test that writes the verification report its
// kept `validate~validate` is judged against. Each manifest declares its
// command's kind, as a first-party manifest or the @putnami/dev vocabulary
// does.
func TestRunKeepsTheTestsOfAConsumerReachedThroughItsSpecGate(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "changed-files-select-owners-and-transitive-dependents")
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	task := taskCommand(t, fixtureproc.Program{})
	manifest := func(name, command, kind, output string) string {
		return `{"name":"` + name + `","version":"1.0.0","cliContract":11,` +
			`"commands":{"` + command + `":{"traits":{"kind":"` + kind + `"},"run":[{"id":"` + command + `","task":"mark"}]}},` +
			`"tasks":{"mark":{"kind":"command","command":` + task + `,"cache":false,` +
			`"inputs":{"sources":{"from":"project","files":["**/*.src"]}},` +
			`"declares":{"outputs":{"file":{"kind":"file","root":"command-output","path":"` + output + `","optionalEmpty":true}}}}}}`
	}
	write(".gitignore", ".putnami/\n")
	write("putnami.workspace.json", `{"name":"spec-gate-scope","includes":["app","lib","sdd","tester"]}`)
	write("lib/putnami.json", `{"name":"lib","extensions":["/sdd","/tester"]}`)
	write("app/putnami.json", `{"name":"app","dependencies":["lib"],"extensions":["/sdd","/tester"]}`)
	write("sdd/putnami.json", `{"name":"@fixture/sdd"}`)
	write("sdd/putnami.extension.json", manifest("@fixture/sdd", "validate", extensionproto.CommandKindCriteria, features.SpecCriteriaProjectionFilename))
	write("sdd/implementation.txt", "v1\n")
	write("tester/putnami.json", `{"name":"@fixture/tester"}`)
	write("tester/putnami.extension.json", manifest("@fixture/tester", "test", extensionproto.CommandKindVerification, features.VerificationReportFilename))
	initCLISelectionGitRepo(t, root)
	runCLISelectionGit(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
	runCLISelectionGit(t, root, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	runCLISelectionGit(t, root, "checkout", "-b", "feature")
	write("sdd/implementation.txt", "v2\n")
	workspace.InvalidateLoadCache(root)

	var live bytes.Buffer
	renderer := output.NewRenderer(output.Config{Output: "jsonl", Command: "test", Out: &live, Err: &live})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := New().Run(ctx, Request{
		WorkspaceRoot: root, Config: wsproto.Load(root), Commands: []string{"test", "validate"},
		Global: GlobalFlags{Output: "jsonl", Impacted: true, Projects: impactedProjectsSentinel, NoCache: true, MaxParallel: 1},
	}, renderer)
	if err != nil || result.ExitCode != ExitSuccess {
		t.Fatalf("impacted engine run: exit=%d err=%v\n%s", result.ExitCode, err, live.String())
	}

	var started []string
	for _, line := range strings.Split(strings.TrimSpace(live.String()), "\n") {
		var record protocolcli.SessionStreamRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode live record %q: %v", line, err)
		}
		if record.Record == protocolcli.RecordTaskStart && record.Identity != nil {
			started = append(started, record.Identity.Key)
		}
	}
	slices.Sort(started)
	want := []string{"/app:test~test", "/app:validate~validate", "/lib:test~test", "/lib:validate~validate"}
	if !slices.Equal(started, want) {
		t.Errorf("tasks started = %v, want %v: each consumer's spec gate and the test it judges", started, want)
	}
}

// A task scope keeps a job by what its command declares, never by the command's
// name. A command named `ship` that declares registry side effects survives a
// scope that does not name it, while a `publish` and a `deploy` whose manifest
// declares traits without side effects are narrowed like any command that
// declares none: the core gives no name a side effect. The manifest goes
// through the loader and the planner a run uses, so the traits are the ones a
// run resolves.
func TestNarrowToTaskScopes_FollowsTheDeclaredSideEffectsNotTheCommandName(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "a-task-scope-keeps-every-job-that-declares-side-effects")
	root := t.TempDir()
	for rel, content := range map[string]string{
		"lib/a.src": "a\n",
		"release/putnami.extension.json": `{"name":"@fixture/release","version":"1.0.0","cliContract":4,"commands":{` +
			`"build":{"run":[{"id":"compile","task":"compile"}]},` +
			`"ship":{"traits":{"sideEffects":"registry"},"run":[{"id":"upload","task":"upload"}]},` +
			`"publish":{"traits":{},"run":[{"id":"notes","task":"notes"}]},` +
			`"deploy":{"traits":{},"run":[{"id":"roll","task":"roll"}]},` +
			`"docs":{"traits":{},"run":[{"id":"render","task":"render"}]}},` +
			`"tasks":{` +
			`"compile":{"kind":"command","command":"fixture","cache":false,"inputs":{"sources":{"from":"project","files":["**/*.src"]}}},` +
			`"upload":{"kind":"command","command":"fixture","cache":false},` +
			`"notes":{"kind":"command","command":"fixture","cache":false},` +
			`"roll":{"kind":"command","command":"fixture","cache":false},` +
			`"render":{"kind":"command","command":"fixture","cache":false}}}`,
	} {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := extension.LoadManifest(filepath.Join(root, "release", "putnami.extension.json")); err != nil {
		t.Fatalf("load the fixture manifest: %v", err)
	}
	ext := extension.LoadExtensionFromDir(filepath.Join(root, "release"), "@fixture/release")
	lib := &workspace.Project{ID: "/lib", Name: "lib", Path: "lib", Extensions: []string{"@fixture/release"}, Config: &wsproto.ProjectConfig{}}
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "scope"}, []*workspace.Project{lib})
	ws.Graph = workspace.BuildGraph(ws.Projects)
	planned, err := planner.Plan(ws, []string{"build", "ship", "publish", "deploy", "docs"}, ws.Projects, []*extension.ExtensionDescription{ext}, nil, nil, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if got, want := sortedJobKeys(planned), []string{"/lib:build~compile", "/lib:deploy~roll", "/lib:docs~render", "/lib:publish~notes", "/lib:ship~upload"}; !slices.Equal(got, want) {
		t.Fatalf("planned = %v, want %v", got, want)
	}

	narrowed := narrowToTaskScopes(ws, planned, map[string][]string{"/lib": {"build~compile"}})

	if got, want := sortedJobKeys(narrowed), []string{"/lib:build~compile", "/lib:ship~upload"}; !slices.Equal(got, want) {
		t.Errorf("narrowed = %v, want %v: ship declares registry side effects, publish, deploy and docs declare none", got, want)
	}
}

// Every first-party command keeps, under a task scope, the jobs of `publish`,
// `deploy`, `validate` and `validate-workspace`, and no other. Every
// first-party `publish` resolves to `registry`, by default or by declaration,
// and no other first-party command declares side effects. Every first-party
// `validate` and `validate-workspace` resolves to the criteria kind, by its
// own declaration or by the @putnami/dev vocabulary, and no other first-party
// command does. The manifests are discovered, so a new first-party extension
// is covered without an edit here, and the vocabulary pass runs over them all
// as discovery runs it over a workspace's extensions.
func TestNarrowToTaskScopes_KeepsWhatEveryFirstPartyCommandKept(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "a-task-scope-keeps-every-job-that-declares-side-effects")
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "a-criteria-kind-command-is-never-narrowed")
	keptByName := map[string]bool{"publish": true, "deploy": true, "validate": true, "validate-workspace": true}
	repoRoot := filepath.Join(engineModuleRoot(t), "..", "..")
	project := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
	ws := workspace.NewWorkspace("/ws", nil, []*workspace.Project{project})
	scopes := map[string][]string{"/app": {"none~none"}}
	var dirs []string
	err := filepath.WalkDir(repoRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Hidden trees (.git, .gen, .putnami), installs, builds and fixtures hold no first-party manifest.
		if entry.IsDir() {
			switch name := entry.Name(); {
			case path != repoRoot && strings.HasPrefix(name, "."), name == "node_modules", name == "testdata", name == "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() == "putnami.extension.json" {
			dir, err := filepath.Rel(repoRoot, filepath.Dir(path))
			if err != nil {
				return err
			}
			dirs = append(dirs, filepath.ToSlash(dir))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("discover the first-party manifests: %v", err)
	}
	// The repository holds sixteen first-party extensions; fewer means the walk missed some.
	if len(dirs) < 16 {
		t.Fatalf("discovered %d first-party manifests %v, want at least 16", len(dirs), dirs)
	}
	loaded := make([]*extension.ExtensionDescription, 0, len(dirs))
	for _, dir := range dirs {
		ext := extension.LoadExtensionFromDir(filepath.Join(repoRoot, dir), dir)
		if ext == nil {
			t.Fatalf("first-party extension %s did not load", dir)
		}
		loaded = append(loaded, ext)
	}
	if warnings := extension.ApplyVocabularyTraits(loaded); len(warnings) > 0 {
		t.Fatalf("the first-party vocabularies merge with warnings: %v", warnings)
	}
	kept := make(map[string]int)
	for i, ext := range loaded {
		dir := dirs[i]
		for _, def := range ext.Jobs {
			job := &scheduler.ScheduledJob{Project: project, Extension: ext, JobDef: def}
			command := modeljobs.JobCommandName(job)
			got := len(narrowToTaskScopes(ws, []*scheduler.ScheduledJob{job}, scopes)) == 1
			if got != keptByName[command] {
				t.Errorf("%s %s: kept under a scope that does not name it = %t, want %t (traits %+v)",
					dir, command, got, keptByName[command], def.Traits)
			}
			if got {
				kept[command]++
			}
		}
	}
	// The pin is only as strong as the commands it reached.
	for _, command := range []string{"publish", "validate", "validate-workspace"} {
		if kept[command] == 0 {
			t.Errorf("no first-party extension declares %s: the pin no longer covers it", command)
		}
	}
}

func sortedJobKeys(planned []*scheduler.ScheduledJob) []string {
	keys := make([]string, 0, len(planned))
	for _, job := range planned {
		if job != nil {
			keys = append(keys, job.Key())
		}
	}
	slices.Sort(keys)
	return keys
}
