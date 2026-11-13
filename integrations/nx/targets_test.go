package nx

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/stacks"
)

const namedRequirement = "named-targets-run-as-tasks"

// namedTargets is a project.json whose targets are named by variant.
const namedTargets = `{"name": "@demo/pricing", "targets": {"build": {"executor": "nx:run-commands"}, "test:lib": {"executor": "nx:run-commands"}, "test:eslint": {"executor": "nx:run-commands"}, "test:oxlint": {"executor": "nx:run-commands"}, "test:types": {"executor": "nx:run-commands"}}}`

// mapWorkspace writes the workspace file of root in the mode, with the
// targets mapping when it is not empty and the extensions beside @putnami/nx.
func mapWorkspace(t *testing.T, root, mode, mapping string, extensions ...string) {
	t.Helper()
	names := `"` + strings.Join(append([]string{ExtensionName}, extensions...), `", "`) + `"`
	options := `"mode": "` + mode + `"`
	if mapping != "" {
		options += `, "targets": ` + mapping
	}
	writeFile(t, root, "putnami.workspace.json", `{"name": "demo", "extensions": [`+names+`], "options": {"@putnami/nx": {`+options+`}}}`)
}

// namedWorkspace writes a Nx 21 workspace whose project packages/pricing
// names its targets by variant.
func namedWorkspace(t *testing.T, mode, mapping string, extensions ...string) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "nx.json", "{}\n")
	writeFile(t, root, "package.json", `{"name": "demo", "private": true, "devDependencies": {"nx": "21.0.0"}}`)
	writeFile(t, root, "packages/pricing/project.json", namedTargets)
	mapWorkspace(t, root, mode, mapping, extensions...)
	return root
}

// stepsOf are the steps the project's putnami.json gives the task under its
// override of the Nx profile, each joined by spaces.
func stepsOf(t *testing.T, root, dir, task string) []string {
	t.Helper()
	options, _ := readProjectFile(t, root, dir)["options"].(map[string]any)
	layer, _ := options[ExtensionName].(map[string]any)
	tasks, _ := layer["tasks"].(map[string]any)
	entry, _ := tasks[task].(map[string]any)
	list, _ := entry["steps"].([]any)
	var out []string
	for _, step := range list {
		out = append(out, strings.Join(stringList(step), " "))
	}
	return out
}

func disabledJobs(t *testing.T, root, dir string) []string {
	t.Helper()
	disable, _ := readProjectFile(t, root, dir)["disable"].(map[string]any)
	return stringList(disable["jobs"])
}

// runSyncJob runs a job of the extension at root and answers its status and
// the events it printed.
func runSyncJob(t *testing.T, root string, job func(*pctx.Context, *jsonl.Emitter, []string) (string, map[string]any, error)) (string, map[string]any, string) {
	t.Helper()
	var events strings.Builder
	emit := jsonl.New()
	emit.Observe(func(message string) { events.WriteString(message + "\n") })
	status, data, err := job(&pctx.Context{WorkspaceRoot: root}, emit, nil)
	if err != nil {
		events.WriteString(err.Error() + "\n")
	}
	return status, data, events.String()
}

const (
	runLib    = "npx --no nx run {component}:test:lib --outputStyle=static"
	runESLint = "npx --no nx run {component}:test:eslint --outputStyle=static"
	runOxlint = "npx --no nx run {component}:test:oxlint --outputStyle=static"
)

func TestAMappedTargetRunsAsItsTask(t *testing.T) {
	spectest.Proves(t, feature, namedRequirement, "a-mapped-target-runs-as-its-task")
	const mapping = `{"test:lib": "test", "test:eslint": "lint", "test:oxlint": "lint"}`
	root := namedWorkspace(t, ModeDelegate, mapping)
	result := sync(t, root, SyncOptions{})

	if jobs := disabledJobs(t, root, "packages/pricing"); len(jobs) != 0 {
		t.Errorf("disable.jobs = %v, want build, test and lint on", jobs)
	}
	for task, want := range map[string][]string{
		stacks.TaskTest:  {runLib},
		stacks.TaskLint:  {runESLint, runOxlint},
		stacks.TaskBuild: nil,
	} {
		if got := stepsOf(t, root, "packages/pricing", task); !slices.Equal(got, want) {
			t.Errorf("%s steps = %q, want %q", task, got, want)
		}
	}
	// The step of a mapped target is the profile's own, with the target's name.
	for _, task := range lifecycleTasks {
		if steps := stack(t).Profile.Tasks[task].Steps; len(steps) != 1 || !slices.Equal(steps[0], runStep(task)) {
			t.Errorf("the profile's %s steps = %q, want the one step %q", task, steps, runStep(task))
		}
	}
	// A mapped target is no longer listed; one with no entry still is.
	listed := map[string]bool{}
	for _, u := range result.Unmapped {
		if u.Reason == unrunReason(u.Project, u.Target) {
			listed[u.Target] = true
		}
	}
	if !reflect.DeepEqual(listed, map[string]bool{"test:types": true}) {
		t.Errorf("targets listed as run by no task = %v, want test:types alone", listed)
	}

	// Each task runs its mapped targets through nx run, in name order.
	stubNx(t, readDemo(t))
	rootReal, _ := filepath.EvalSymlinks(root)
	for task, want := range map[string][]string{
		stacks.TaskTest: {"npx --no nx run @demo/pricing:test:lib --outputStyle=static"},
		stacks.TaskLint: {"npx --no nx run @demo/pricing:test:eslint --outputStyle=static", "npx --no nx run @demo/pricing:test:oxlint --outputStyle=static"},
	} {
		t.Setenv("STUB_LOG", filepath.Join(t.TempDir(), "log"))
		status, data, events := runJob(t, root, "packages/pricing", task)
		if status != "OK" || data["delegatedTo"] != "nx" {
			t.Errorf("%s: status %s, data %v, events %s", task, status, data, events)
		}
		var ran []string
		for _, line := range stubLog(t) {
			if call, ok := strings.CutPrefix(line, rootReal+" "); ok && strings.Contains(call, " nx run ") {
				ran = append(ran, call)
			}
		}
		if !slices.Equal(ran, want) {
			t.Errorf("%s ran %q at the root, want %q", task, ran, want)
		}
	}

	// A mapping the project files do not follow yet is stale.
	mapWorkspace(t, root, ModeDelegate, `{"test:lib": "test"}`)
	if status, _, events := runSyncJob(t, root, RunCheck); status != "FAILED" || !strings.Contains(events, CodeSyncStale) {
		t.Errorf("validate after a mapping change: %s, want %s\n%s", status, CodeSyncStale, events)
	}

	// In import mode a mapped target keeps running through Nx: the
	// TypeScript task of its name is off.
	root = namedWorkspace(t, ModeImport, mapping, TypeScriptExtension)
	writeFile(t, root, "packages/pricing/package.json", `{"name": "@demo/pricing", "private": true}`)
	writeFile(t, root, "packages/pricing/tsconfig.json", "{}\n")
	sync(t, root, SyncOptions{})
	jobs := disabledJobs(t, root, "packages/pricing")
	for _, want := range []string{TypeScriptExtension + ":test", TypeScriptExtension + ":lint"} {
		if !slices.Contains(jobs, want) {
			t.Errorf("import mode disable.jobs = %v, want %s", jobs, want)
		}
	}
	if slices.Contains(jobs, "test") || slices.Contains(jobs, ExtensionName+":test") || slices.Contains(jobs, "lint") || slices.Contains(jobs, ExtensionName+":lint") {
		t.Errorf("import mode disable.jobs = %v, want the Nx test and lint on", jobs)
	}
	if got := stepsOf(t, root, "packages/pricing", stacks.TaskTest); !slices.Equal(got, []string{runLib}) {
		t.Errorf("import mode test steps = %q, want %q", got, runLib)
	}
}

func TestATargetWithNoMappingKeepsTheSummaryLine(t *testing.T) {
	spectest.Proves(t, feature, namedRequirement, "a-target-with-no-mapping-keeps-the-summary-line")
	root := t.TempDir()
	writeFile(t, root, "nx.json", "{}\n")
	mapWorkspace(t, root, ModeDelegate, `{"test:lib": "test"}`)
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		writeFile(t, root, "packages/"+name+"/project.json", `{"name": "@demo/`+name+`", "targets": {"build": {"executor": "nx:run-commands"}, "test:lib": {"executor": "nx:run-commands"}, "test:eslint": {"executor": "nx:run-commands"}}}`)
	}
	status, _, out := runSyncJob(t, root, RunSync)
	if status != "OK" {
		t.Fatalf("sync: %s\n%s", status, out)
	}
	if want := "target test:eslint of 5 projects (@demo/a, @demo/b, @demo/c and 2 more): no Putnami task runs it: run nx run <project>:test:eslint"; !strings.Contains(out, want) {
		t.Errorf("the sync output lacks %q:\n%s", want, out)
	}
	if strings.Contains(out, "target test:lib of") || strings.Contains(out, "run nx run <project>:test:lib") {
		t.Errorf("the mapped test:lib is listed as run by no task:\n%s", out)
	}
	for _, name := range []string{"a", "e"} {
		if jobs := disabledJobs(t, root, "packages/"+name); !slices.Equal(jobs, []string{"lint"}) {
			t.Errorf("packages/%s disable.jobs = %v, want lint alone", name, jobs)
		}
		if got := stepsOf(t, root, "packages/"+name, stacks.TaskTest); !slices.Equal(got, []string{runLib}) {
			t.Errorf("packages/%s test steps = %q, want %q", name, got, runLib)
		}
	}
}

func TestATargetNamedAfterTheTaskOutranksAMappedOne(t *testing.T) {
	spectest.Proves(t, feature, namedRequirement, "a-target-named-after-the-task-outranks-a-mapped-one")
	root := namedWorkspace(t, ModeDelegate, `{"test:lib": "test"}`)
	writeFile(t, root, "packages/pricing/project.json", `{"name": "@demo/pricing", "targets": {"test": {"executor": "nx:run-commands"}, "test:lib": {"executor": "nx:run-commands"}}}`)
	result := sync(t, root, SyncOptions{})
	if got := stepsOf(t, root, "packages/pricing", stacks.TaskTest); got != nil {
		t.Errorf("test steps = %q, want the profile's own step, which runs the test target", got)
	}
	if jobs := disabledJobs(t, root, "packages/pricing"); !slices.Equal(jobs, []string{"build", "lint"}) {
		t.Errorf("disable.jobs = %v, want build and lint", jobs)
	}
	want := shadowedReason("@demo/pricing", "test:lib", "test")
	if !slices.ContainsFunc(result.Unmapped, func(u Unmapped) bool {
		return u.Project == "@demo/pricing" && u.Target == "test:lib" && u.Reason == want
	}) {
		t.Errorf("unmapped = %+v, want test:lib listed with %q", result.Unmapped, want)
	}
	if !strings.Contains(want, "nx run @demo/pricing:test:lib") || !strings.Contains(want, "the project's test target") {
		t.Errorf("reason = %q, want the test target and the nx run command named", want)
	}

	// Several projects that list the target for that reason share one line.
	root = t.TempDir()
	writeFile(t, root, "nx.json", "{}\n")
	mapWorkspace(t, root, ModeDelegate, `{"test:lib": "test"}`)
	for _, name := range []string{"a", "b", "c", "d"} {
		writeFile(t, root, "packages/"+name+"/project.json", `{"name": "@demo/`+name+`", "targets": {"test": {"executor": "nx:run-commands"}, "test:lib": {"executor": "nx:run-commands"}}}`)
	}
	status, _, out := runSyncJob(t, root, RunSync)
	line := "target test:lib of 4 projects (@demo/a, @demo/b, @demo/c and 1 more): " + shadowedReason("<project>", "test:lib", "test")
	if status != "OK" || !strings.Contains(out, line) || strings.Count(out, "test:lib") != 2 {
		t.Errorf("sync: %s, want the one line %q:\n%s", status, line, out)
	}
}

func TestTheSyncOwnsTheStepsItWrites(t *testing.T) {
	spectest.Proves(t, feature, namedRequirement, "the-sync-owns-the-steps-it-writes")
	const mapping = `{"test:lib": "test"}`
	root := namedWorkspace(t, ModeDelegate, mapping)
	writeFile(t, root, "packages/pricing/jest.config.js", "module.exports = {reporters: ['default', ['jest-junit', {outputDirectory: '<rootDir>/reports'}]]};\n")
	sync(t, root, SyncOptions{})
	if got := stepsOf(t, root, "packages/pricing", stacks.TaskTest); !slices.Equal(got, []string{runLib}) {
		t.Fatalf("test steps = %q, want %q", got, runLib)
	}
	// The reports of a mapped test sit beside its steps.
	options, _ := readProjectFile(t, root, "packages/pricing")["options"].(map[string]any)
	test, _ := options[ExtensionName].(map[string]any)["tasks"].(map[string]any)["test"].(map[string]any)
	if reports, _ := test["reports"].(map[string]any); !reflect.DeepEqual(reports["junit"], []any{"reports/junit.xml"}) {
		t.Errorf("test = %v, want the junit report the jest configuration names", test)
	}
	if result := sync(t, root, SyncOptions{}); len(result.Written) != 0 {
		t.Errorf("a second sync rewrites %v", result.Written)
	}

	// Without the entry, the sync removes its steps and turns the task off.
	mapWorkspace(t, root, ModeDelegate, "")
	sync(t, root, SyncOptions{})
	if _, ok := readProjectFile(t, root, "packages/pricing")["options"]; ok {
		t.Errorf("options = %v, want the steps and the reports removed", readProjectFile(t, root, "packages/pricing")["options"])
	}
	if jobs := disabledJobs(t, root, "packages/pricing"); !slices.Equal(jobs, []string{"lint", "test"}) {
		t.Errorf("disable.jobs = %v, want lint and test", jobs)
	}

	// Steps of another shape are the project's own: the sync keeps them,
	// with the entry and without. It replaces steps of its shape.
	const own = `["npx", "--no", "nx", "run", "{component}:test:lib", "--configuration=ci"]`
	writeFile(t, root, "packages/pricing/putnami.json", `{"extensions": ["@putnami/nx"], "options": {"@putnami/nx": {"tasks": {`+
		`"test": {"steps": [`+own+`]}, `+
		`"lint": {"steps": [["npx", "--no", "nx", "run", "{component}:e2e", "--outputStyle=static"]]}}}}}`)
	for _, entry := range []string{mapping, ""} {
		mapWorkspace(t, root, ModeDelegate, entry)
		sync(t, root, SyncOptions{})
		if got := stepsOf(t, root, "packages/pricing", stacks.TaskTest); !slices.Equal(got, []string{"npx --no nx run {component}:test:lib --configuration=ci"}) {
			t.Errorf("with the mapping %q, test steps = %q, want the project's own kept", entry, got)
		}
		if got := stepsOf(t, root, "packages/pricing", stacks.TaskLint); got != nil {
			t.Errorf("with the mapping %q, lint steps = %q, want the steps of the sync's shape removed", entry, got)
		}
	}

	// A project that leaves the graph loses the steps the sync wrote.
	mapWorkspace(t, root, ModeDelegate, mapping)
	writeFile(t, root, "packages/pricing/putnami.json", `{"extensions": ["@putnami/nx"]}`)
	sync(t, root, SyncOptions{})
	if got := stepsOf(t, root, "packages/pricing", stacks.TaskTest); !slices.Equal(got, []string{runLib}) {
		t.Fatalf("test steps = %q, want %q", got, runLib)
	}
	writeFile(t, root, "packages/other/project.json", `{"name": "@demo/other", "targets": {}}`)
	if err := os.Remove(filepath.Join(root, "packages/pricing/project.json")); err != nil {
		t.Fatal(err)
	}
	result := sync(t, root, SyncOptions{})
	if !slices.Contains(result.Retracted, "packages/pricing/putnami.json") {
		t.Fatalf("retracted = %v, want packages/pricing/putnami.json", result.Retracted)
	}
	if got := stepsOf(t, root, "packages/pricing", stacks.TaskTest); got != nil {
		t.Errorf("a former project keeps the steps %q", got)
	}
}

func TestAnUnmatchedOrInvalidMappingIsReported(t *testing.T) {
	spectest.Proves(t, feature, namedRequirement, "an-unmatched-or-invalid-mapping-is-reported")
	root := namedWorkspace(t, ModeDelegate, `{"test:lbi": "test", "test:lib": "test"}`)
	result := sync(t, root, SyncOptions{})
	if !slices.Equal(result.UnmatchedTargets, []string{"test:lbi"}) {
		t.Errorf("unmatched targets = %v, want test:lbi", result.UnmatchedTargets)
	}
	for name, job := range map[string]func(*pctx.Context, *jsonl.Emitter, []string) (string, map[string]any, error){"sync": RunSync, "validate": RunCheck} {
		status, _, events := runSyncJob(t, root, job)
		if status != "OK" || !strings.Contains(events, `options["@putnami/nx"].targets maps "test:lbi", a target no Nx project of nx.json has`) {
			t.Errorf("%s: %s, want the %s warning\n%s", name, status, CodeTargetUnmatched, events)
		}
		if strings.Contains(events, `maps "test:lib", a target`) {
			t.Errorf("%s reports test:lib, which packages/pricing has:\n%s", name, events)
		}
	}

	for mapping, want := range map[string]string{
		`{"test:lib": "check"}`: `maps "test:lib" to "check", which is no task: choose build, lint or test`,
		`{"test:lib": 1}`:       `maps "test:lib" to 1, which is no task`,
		`{"test": "lint"}`:      `maps "test", the target the test task runs without an entry`,
		`{"": "test"}`:          `maps an empty target name`,
		`["test:lib"]`:          `is ["test:lib"], not an object that maps a Nx target name to build, lint or test`,
	} {
		root := namedWorkspace(t, ModeDelegate, mapping)
		_, err := Sync(t.Context(), root, SyncOptions{})
		if err == nil || !strings.Contains(err.Error(), `options["@putnami/nx"].targets `+want) {
			t.Errorf("the mapping %s: %v, want %q", mapping, err, want)
		}
		if _, err := os.Stat(filepath.Join(root, "packages/pricing/putnami.json")); err == nil {
			t.Errorf("the mapping %s: the sync wrote packages/pricing/putnami.json", mapping)
		}
		for name, job := range map[string]func(*pctx.Context, *jsonl.Emitter, []string) (string, map[string]any, error){"sync": RunSync, "validate": RunCheck} {
			if status, _, events := runSyncJob(t, root, job); status != "FAILED" || !strings.Contains(events, CodeSyncFailed) || !strings.Contains(events, want) {
				t.Errorf("%s with the mapping %s: %s, want %s\n%s", name, mapping, status, CodeSyncFailed, events)
			}
		}
	}
}
