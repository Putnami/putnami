package scheduler

import (
	"encoding/json"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// typeScriptEvidence is where the TypeScript framework writes its feature
// evidence, relative to its project. No committed copy exists: the file lives
// only inside build-generate's declared .gen output.
const typeScriptEvidence = ".gen/schema/feature-evidence/typescript-framework.json"

// typeScriptGenerateProjects are the fixture's two TypeScript projects.
var typeScriptGenerateProjects = []string{"a", "b"}

// typeScriptGenerateFixture is a Git-managed workspace with two TypeScript
// projects. Each runs @putnami/typescript's real build-generate contract: its
// declared outputs, its cache policy and its batch policy are read from
// typescript/extension/putnami.extension.json, and its key files are derived
// from the task's declared inputs. Only the commands are scripts. The generate
// script writes the evidence the way the framework producer does, the build
// stamp copied to the evidence path, for every project it is handed, batched
// or solo, and logs one line per invocation to shapeLog: batch or solo. Each
// project also runs a transpile task whose key folds generate's.
//
// The planned jobs are generate a, generate b, transpile a, transpile b, in
// that order.
func typeScriptGenerateFixture(t *testing.T, generateLog, shapeLog, transpileLog string) (*workspace.Workspace, []*ScheduledJob) {
	t.Helper()
	typeScriptExtension := extension.LoadExtensionFromDir(
		filepath.Join(findJobsRepoRoot(t), "typescript", "extension"), "/typescript/extension")
	if typeScriptExtension == nil {
		t.Fatal("typescript/extension/putnami.extension.json did not load")
	}
	generateTask, ok := typeScriptExtension.Tasks["build-generate"]
	if !ok || generateTask.Declares == nil || generateTask.Cache == nil || generateTask.Batchable == nil {
		t.Fatalf("build-generate = %+v, want a declared, cached, batchable task", generateTask)
	}
	generatePolicy := *generateTask.Cache
	generateKeyFiles := extension.DeriveTaskCacheKey(generateTask.Inputs).Files

	root := t.TempDir()
	runVersionsGit(t, root, "init")
	for _, project := range typeScriptGenerateProjects {
		writeTestFile(t, filepath.Join(root, project, "package.json"), "{\"name\":\""+project+"\"}\n")
		writeTestFile(t, filepath.Join(root, project, "src", "main.ts"), "export const name = '"+project+"';\n")
		writeTestFile(t, filepath.Join(root, project, "test", "main.test.ts"), "// first comment\n")
	}

	scripts := t.TempDir()
	generate := filepath.Join(scripts, "generate.sh")
	writeExecutable(t, generate, "#!/bin/sh\n"+
		"if [ -n \"$PUTNAMI_SELECTED_PROJECT_IDS\" ]; then shape=batch; else shape=solo; fi\n"+
		"printf '%s\\n' \"$shape\" >> "+shellQuote(shapeLog)+"\n"+
		"roots=\"$PUTNAMI_SELECTED_PROJECT_ROOTS\"\n"+
		"if [ -z \"$roots\" ]; then roots=\"$PWD\"; fi\n"+
		"IFS=','\n"+
		"for root in $roots; do\n"+
		"  printf '%s\\n' \"$(basename \"$root\")\" >> "+shellQuote(generateLog)+"\n"+
		"  mkdir -p \"$root/.gen/schema/feature-evidence\"\n"+
		"  cp \"$root/.gen/version.json\" \"$root/"+typeScriptEvidence+"\"\n"+
		"done\n"+
		"unset IFS\n"+
		"printf '%s\\n' '{\"v\":2,\"type\":\"meta\",\"data\":{\"protocol\":2}}'\n"+
		"if [ -n \"$PUTNAMI_SELECTED_PROJECT_IDS\" ]; then\n"+
		"  results=''\n"+
		"  IFS=','\n"+
		"  for id in $PUTNAMI_SELECTED_PROJECT_IDS; do\n"+
		"    if [ -n \"$results\" ]; then results=\"$results,\"; fi\n"+
		"    results=\"$results{\\\"projectId\\\":\\\"$id\\\",\\\"status\\\":\\\"OK\\\"}\"\n"+
		"  done\n"+
		"  unset IFS\n"+
		"  printf '%s\\n' \"{\\\"v\\\":2,\\\"type\\\":\\\"result\\\",\\\"data\\\":{\\\"status\\\":\\\"OK\\\",\\\"data\\\":{\\\"batchResults\\\":[$results]}}}\"\n"+
		"else\n"+
		"  printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\"}}'\n"+
		"fi\n"+
		"exit 0\n")
	transpile := filepath.Join(scripts, "transpile.sh")
	writeExecutable(t, transpile, "#!/bin/sh\n"+
		"printf '%s\\n' \"$(basename \"$PWD\")\" >> "+shellQuote(transpileLog)+"\n"+
		sourceBoundEvents+"exit 0\n")

	projects := make([]*workspace.Project, 0, len(typeScriptGenerateProjects))
	for _, project := range typeScriptGenerateProjects {
		projects = append(projects, &workspace.Project{ID: "/" + project, Name: project, Path: project})
	}
	ws := workspace.NewWorkspace(root, nil, projects)
	ws.Name = "source-bound-typescript-ws"
	ext := &extension.ExtensionDescription{
		Name:    "@putnami/typescript",
		Version: "1.0.0",
		Path:    filepath.Join(root, "ext"),
		Tasks: map[string]extension.TaskDefinition{
			"build-generate":  generateTask,
			"probe-transpile": {Declares: &extension.TaskDeclaration{}},
		},
	}
	job := func(project *workspace.Project, name, task, command string, files []string, policy *extension.TaskCachePolicy) *ScheduledJob {
		commandName, step := jobCommandAndStep(name)
		return &ScheduledJob{
			Project:   project,
			Extension: ext,
			Step:      &extension.PipelineStep{ID: step, Task: task},
			JobDef: &extension.JobDefinition{
				ExtensionName: "@putnami/typescript", Name: name, CommandName: commandName, StepID: step,
				Command: command, Cwd: "{projectRoot}", Cache: true, FilePatterns: files, TaskCachePolicy: policy,
			},
		}
	}
	planned := make([]*ScheduledJob, 0, 2*len(projects))
	for _, project := range projects {
		generateJob := job(project, "build~generate", "build-generate", generate, generateKeyFiles, &generatePolicy)
		generateJob.JobDef.Batchable = generateTask.Batchable
		planned = append(planned, generateJob)
	}
	for i, project := range projects {
		transpileJob := job(project, "build~transpile", "probe-transpile", transpile,
			[]string{"src/**/*.ts", "src/**/*.tsx"}, &extension.TaskCachePolicy{Deterministic: true, NoOutput: true})
		transpileJob.DependsOn = []string{planned[i].Key()}
		planned = append(planned, transpileJob)
	}
	return ws, planned
}

// typeScriptEvidenceBinding returns the binding the TypeScript feature
// evidence of project names for that project.
func typeScriptEvidenceBinding(t *testing.T, ws *workspace.Workspace, project string) string {
	t.Helper()
	var evidence VersionInfo
	raw := readFileAt(t, filepath.Join(ws.Root, project, filepath.FromSlash(typeScriptEvidence)))
	if err := json.Unmarshal([]byte(raw), &evidence); err != nil {
		t.Fatalf("decode evidence of %s: %v", project, err)
	}
	for _, pkg := range evidence.CapabilityPackages {
		if pkg.Package == project {
			return pkg.SourceBinding
		}
	}
	t.Fatalf("evidence of %s names no binding for it: %+v", project, evidence)
	return ""
}

// typeScriptStampBinding returns the binding the build stamp of project names
// for that project: the binding of its tree as the run left it.
func typeScriptStampBinding(t *testing.T, ws *workspace.Workspace, project string) string {
	t.Helper()
	for _, pkg := range readVersionStamp(t, filepath.Join(ws.Root, project)).CapabilityPackages {
		if pkg.Package == project {
			return pkg.SourceBinding
		}
	}
	t.Fatalf("the build stamp of %s names no binding for it", project)
	return ""
}

// executionsByProject counts the lines of an execution log per project. A
// project that never executed has no entry.
func executionsByProject(t *testing.T, logPath string) map[string]int {
	t.Helper()
	counts := map[string]int{}
	if subprocessRuns(t, logPath) == 0 {
		return counts
	}
	for _, project := range strings.Fields(readFileAt(t, logPath)) {
		counts[project]++
	}
	return counts
}

// TypeScript build-generate writes feature evidence that names its project's
// source binding, and its key reads only src, public and package.json. An
// edit to a test file of one project moves that project's binding and no key.
// The first run after it executes that project's generate, whose evidence
// names the new binding, while the other project's generate, a batch sibling,
// and both transpiles stay served from the cache. The next run at the same
// tree hits, and a revert hits the earlier entry and gets its evidence back.
func TestTypeScriptGenerateEditOutsideItsKeyExecutesAtTheNewBinding(t *testing.T) {
	requireShell(t)
	logs := t.TempDir()
	generateLog, shapeLog, transpileLog := filepath.Join(logs, "generate"), filepath.Join(logs, "shape"), filepath.Join(logs, "transpile")
	ws, planned := typeScriptGenerateFixture(t, generateLog, shapeLog, transpileLog)
	cache := sharedRuntimeCache(t)
	testFile := filepath.Join(ws.Root, "a", "test", "main.test.ts")
	var keys []string
	run := func(name string, wantGenerate map[string]int, wantHit map[string]bool) {
		t.Helper()
		result := runSharedScheduler(t.Context(), ws, planned, SchedulerConfig{MaxParallel: 2}, cache)
		now := make([]string, len(planned))
		for i := range planned {
			got := resultOf(t, result, planned, i)
			if got.Status != "success" {
				t.Fatalf("%s: %s = %+v, want success", name, planned[i].Key(), got)
			}
			if got.CacheKey == "" {
				t.Fatalf("%s: %s has no cache key", name, planned[i].Key())
			}
			now[i] = got.CacheKey
		}
		if keys == nil {
			keys = now
		} else if !slices.Equal(keys, now) {
			t.Fatalf("%s: keys moved from %v to %v", name, keys, now)
		}
		for i, project := range typeScriptGenerateProjects {
			if got := resultOf(t, result, planned, i).Reuse.CacheHit(); got != wantHit[project] {
				t.Fatalf("%s: generate of %s cache hit = %t, want %t", name, project, got, wantHit[project])
			}
		}
		if got := executionsByProject(t, generateLog); !maps.Equal(got, wantGenerate) {
			t.Fatalf("%s: generate executions = %v, want %v", name, got, wantGenerate)
		}
		if got := executionsByProject(t, transpileLog); !maps.Equal(got, map[string]int{"a": 1, "b": 1}) {
			t.Fatalf("%s: transpile executions = %v, want one each: a dependent's key must hit", name, got)
		}
		for _, project := range typeScriptGenerateProjects {
			if evidence, stamp := typeScriptEvidenceBinding(t, ws, project), typeScriptStampBinding(t, ws, project); evidence != stamp {
				t.Fatalf("%s: the evidence of %s names %q, its tree's binding is %q", name, project, evidence, stamp)
			}
		}
	}

	run("cold", map[string]int{"a": 1, "b": 1}, map[string]bool{})
	if got := invocationLog(t, shapeLog); !slices.Equal(got, []string{"batch"}) {
		t.Fatalf("cold generate invocations = %v, want one batch: each member binds its own project", got)
	}
	beforeA, beforeB := typeScriptEvidenceBinding(t, ws, "a"), typeScriptEvidenceBinding(t, ws, "b")
	if !strings.HasPrefix(beforeA, "source-v1:sha256:") {
		t.Fatalf("cold evidence binding of a = %q, want a source-v1 binding", beforeA)
	}
	run("warm", map[string]int{"a": 1, "b": 1}, map[string]bool{"a": true, "b": true})

	writeTestFile(t, testFile, "// second comment\n")
	run("first run after a test-only edit", map[string]int{"a": 2, "b": 1}, map[string]bool{"b": true})
	afterA := typeScriptEvidenceBinding(t, ws, "a")
	if afterA == beforeA {
		t.Fatalf("the evidence of a still names the binding before the edit, %q", beforeA)
	}
	if got := typeScriptEvidenceBinding(t, ws, "b"); got != beforeB {
		t.Fatalf("the evidence of b moved from %q to %q on an edit in a", beforeB, got)
	}
	run("second run after a test-only edit", map[string]int{"a": 2, "b": 1}, map[string]bool{"a": true, "b": true})

	// Revert the edit. Generate does not execute, so only the restore of the
	// earlier entry can put the earlier evidence back.
	writeTestFile(t, testFile, "// first comment\n")
	run("back at the earlier tree", map[string]int{"a": 2, "b": 1}, map[string]bool{"a": true, "b": true})
	if got := typeScriptEvidenceBinding(t, ws, "a"); got != beforeA {
		t.Fatalf("back at the earlier tree: the evidence of a names %q, want the earlier %q", got, beforeA)
	}
}
