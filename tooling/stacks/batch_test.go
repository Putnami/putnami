package stacks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/procstat"
)

// batchProfile is a delegated profile whose lint runs over several
// components as one command of the stub tool.
const batchProfile = `{
  "id": "tool",
  "delegated": true,
  "markers": ["tool.json"],
  "toolchain": {"command": ["tool", "--version"], "pattern": "(?m)^tool (?<version>[0-9.]+)$"},
  "inputs": {"sources": ["**"]},
  "tasks": {
    "lint": {
      "steps": [["tool", "run", "{component}:lint"]],
      "batch": {"steps": [["tool", "run", "{component}:lint", "--projects={components}"]]},
      "inputs": ["sources"],
      "dir": "root"
    }
  }
}`

// batchStack is the stack of batchProfile over the components a, b and c of
// one root, with read as its report reader.
func batchStack(t *testing.T, read func(BatchReport) map[string]BatchOutcome) Stack {
	t.Helper()
	profile, err := Decode([]byte(batchProfile))
	if err != nil {
		t.Fatal(err)
	}
	return Stack{
		Extension: "@putnami/tool",
		Profile:   profile,
		Detect: func(_ string, request wsproto.ProbeRequest) ([]Component, []diag.Diagnostic) {
			var out []Component
			for _, p := range request.Paths {
				out = append(out, Component{Path: p, Name: filepath.Base(p), Root: "."})
			}
			return out, nil
		},
		ReadBatch: read,
	}
}

// stubBatchTool puts first on PATH a tool that counts each of its starts in
// the calls file, prints its version, logs each run, prints "output of <arg>"
// for each target it runs and exits with STUB_EXIT.
func stubBatchTool(t *testing.T, root string) {
	t.Helper()
	bin := t.TempDir()
	writeFile(t, bin, "tool", "#!/bin/sh\necho >> "+filepath.Join(root, "calls")+"\ncase \"$1\" in --version) echo 'tool 1.0.0'; exit 0;; esac\n"+
		"echo \"$*\" >> "+filepath.Join(root, "log")+"\nfor a in \"$@\"; do echo \"output of $a\"; done\nexit ${STUB_EXIT:-0}\n")
	if err := os.Chmod(filepath.Join(bin, "tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// runLintBatch runs the lint job as a batch's leader over a, b and c, and
// answers each project's result by its id.
func runLintBatch(t *testing.T, s Stack, root string) map[string]batchResult {
	t.Helper()
	return runTaskBatch(t, s, root, TaskLint)
}

// runTaskBatch runs a task's job as a batch's leader over a, b and c, and
// answers each project's result by its id.
func runTaskBatch(t *testing.T, s Stack, root, task string) map[string]batchResult {
	t.Helper()
	var selected []pctx.ProjectRef
	for _, p := range []string{"a", "b", "c"} {
		writeFile(t, root, p+"/tool.json", "{}")
		selected = append(selected, pctx.ProjectRef{ID: "/" + p, Name: p, Path: p, FullPath: filepath.Join(root, p), OutputPath: filepath.Join(root, ".putnami/out", p)})
	}
	meta, _ := json.Marshal(Metadata{Component: "a", Root: "."})
	ctx := &pctx.Context{
		WorkspaceRoot:    root,
		Project:          pctx.Project{Name: "a", Path: "a", FullPath: filepath.Join(root, "a"), Metadata: map[string]json.RawMessage{s.Extension: meta}},
		SelectedProjects: selected,
	}
	status, data, err := s.Jobs()[task](ctx, jsonl.New(), []string{"--report-dir", selected[0].OutputPath})
	if status != statusOK || err != nil {
		t.Fatalf("batch status %s: %v", status, err)
	}
	raw, _ := json.Marshal(data[batchResultsKey])
	var results []batchResult
	if err := json.Unmarshal(raw, &results); err != nil {
		t.Fatalf("batch results %s: %v", raw, err)
	}
	byID := map[string]batchResult{}
	for _, r := range results {
		byID[r.ProjectID] = r
	}
	if len(byID) != 3 {
		t.Fatalf("batch results = %s, want one per selected project", raw)
	}
	return byID
}

func TestABatchRunsOneCommandOverItsComponents(t *testing.T) {
	root := t.TempDir()
	stubBatchTool(t, root)
	results := runLintBatch(t, batchStack(t, nil), root)
	log, _ := os.ReadFile(filepath.Join(root, "log"))
	if got := strings.TrimSpace(string(log)); got != "run a:lint b:lint c:lint --projects=a,b,c" {
		t.Errorf("the tool ran %q, want one command over a, b and c", got)
	}
	for id, r := range results {
		if r.Status != statusOK || r.Data["delegatedTo"] != "tool" {
			t.Errorf("%s = %+v, want a pass delegated to tool", id, r)
		}
	}
}

func TestABatchCountsEveryToolProcessItStarts(t *testing.T) {
	root := t.TempDir()
	stubBatchTool(t, root)
	before := procstat.Take().Of(procstat.Other).Starts
	runLintBatch(t, batchStack(t, nil), root)
	started := procstat.Take().Of(procstat.Other).Starts - before
	calls, _ := os.ReadFile(filepath.Join(root, "calls"))
	if want := int64(strings.Count(string(calls), "\n")); started != want || want == 0 {
		t.Errorf("the ledger counted %d tool processes, want the %d the tool saw", started, want)
	}
}

func TestABatchReadsEachResultFromTheToolsReport(t *testing.T) {
	root := t.TempDir()
	stubBatchTool(t, root)
	t.Setenv("STUB_EXIT", "1")
	var read BatchReport
	s := batchStack(t, func(report BatchReport) map[string]BatchOutcome {
		read = report
		return map[string]BatchOutcome{"a": {Failed: true, Message: "tool reports a:lint failed: two lint errors"}, "b": {}}
	})
	results := runLintBatch(t, s, root)

	if read.Task != TaskLint || read.Root != root || !reflect.DeepEqual(read.Components, []string{"a", "b", "c"}) || !strings.Contains(string(read.Output), "output of b:lint") || read.Started.IsZero() ||
		!reflect.DeepEqual(read.Command, []string{"tool", "run", "a:lint", "b:lint", "c:lint", "--projects=a,b,c"}) {
		t.Errorf("the report reader read %+v", read)
	}
	if a := results["/a"]; a.Status != statusFailed || len(a.Diagnostics) != 1 || a.Diagnostics[0].Description != "tool reports a:lint failed: two lint errors" || a.Diagnostics[0].Category != CodeStepFailed {
		t.Errorf("a = %+v, want a failure with the tool's message", a)
	}
	if b := results["/b"]; b.Status != statusOK || len(b.Diagnostics) != 0 {
		t.Errorf("b = %+v, want the pass the tool reported", b)
	}
	if c := results["/c"]; c.Status != statusFailed || len(c.Diagnostics) != 1 || !strings.Contains(c.Diagnostics[0].Description, "did not run c: it failed on a") {
		t.Errorf("c = %+v, want a failure as not run", c)
	}
}

func TestAFailedBatchWhoseReportNamesNoneFailsEveryProject(t *testing.T) {
	root := t.TempDir()
	stubBatchTool(t, root)
	t.Setenv("STUB_EXIT", "2")
	results := runLintBatch(t, batchStack(t, func(BatchReport) map[string]BatchOutcome { return nil }), root)
	for id, r := range results {
		if r.Status != statusFailed || len(r.Diagnostics) != 1 || !strings.Contains(r.Diagnostics[0].Description, "exit status 2") {
			t.Errorf("%s = %+v, want the command's failure", id, r)
		}
	}
}

func TestAFailedBatchWhoseReportNamesNoFailureFailsEveryProject(t *testing.T) {
	root := t.TempDir()
	stubBatchTool(t, root)
	t.Setenv("STUB_EXIT", "137")
	// The tool was killed after it reported a and b passed.
	results := runLintBatch(t, batchStack(t, func(BatchReport) map[string]BatchOutcome {
		return map[string]BatchOutcome{"a": {}, "b": {}}
	}), root)
	for id, r := range results {
		if r.Status != statusFailed || len(r.Diagnostics) != 1 || !strings.Contains(r.Diagnostics[0].Description, "exit status 137") {
			t.Errorf("%s = %+v, want the command's failure", id, r)
		}
	}
}

func TestAComponentThatPassedAnEarlierStepFailsWhenALaterOneNeverRan(t *testing.T) {
	root := t.TempDir()
	stubBatchTool(t, root)
	t.Setenv("STUB_EXIT", "1")
	s := batchStack(t, func(BatchReport) map[string]BatchOutcome {
		return map[string]BatchOutcome{"a": {Failed: true, Message: "a failed"}, "b": {}, "c": {}}
	})
	lint := s.Profile.Tasks[TaskLint]
	lint.Batch = &Batch{Steps: [][]string{{"tool", "first", "{component}"}, {"tool", "second", "{component}"}}}
	s.Profile.Tasks[TaskLint] = lint
	results := runLintBatch(t, s, root)
	if log, _ := os.ReadFile(filepath.Join(root, "log")); strings.Contains(string(log), "second") {
		t.Fatalf("the second step ran after the first failed: %s", log)
	}
	for _, id := range []string{"/b", "/c"} {
		if r := results[id]; r.Status != statusFailed || len(r.Diagnostics) != 1 || !strings.Contains(r.Diagnostics[0].Description, "did not run") {
			t.Errorf("%s = %+v, want a failure: its second step never ran", id, r)
		}
	}
	if a := results["/a"]; a.Status != statusFailed || a.Diagnostics[0].Description != "a failed" {
		t.Errorf("a = %+v, want the tool's message", a)
	}
}

func TestAJobWithoutASelectionRunsItsOwnSteps(t *testing.T) {
	root := t.TempDir()
	stubBatchTool(t, root)
	s := batchStack(t, nil)
	writeFile(t, root, "a/tool.json", "{}")
	meta, _ := json.Marshal(Metadata{Component: "a", Root: "."})
	ctx := &pctx.Context{WorkspaceRoot: root, Project: pctx.Project{Path: "a", FullPath: filepath.Join(root, "a"), Metadata: map[string]json.RawMessage{s.Extension: meta}}}
	if status, _, err := s.Jobs()[TaskLint](ctx, jsonl.New(), []string{"--report-dir", filepath.Join(root, "out")}); status != statusOK || err != nil {
		t.Fatalf("status %s: %v", status, err)
	}
	if log, _ := os.ReadFile(filepath.Join(root, "log")); strings.TrimSpace(string(log)) != "run a:lint" {
		t.Errorf("a lone task ran %q, want its own step", log)
	}
}

func TestABatchIsADelegatedTaskAtTheStackRoot(t *testing.T) {
	for name, edit := range map[string]func(p map[string]any, lint map[string]any){
		"not delegated": func(p, _ map[string]any) { p["delegated"] = false },
		"project dir":   func(_, lint map[string]any) { lint["dir"] = "project" },
		"no component":  func(_, lint map[string]any) { lint["batch"] = map[string]any{"steps": [][]string{{"tool", "run"}}} },
		"no step":       func(_, lint map[string]any) { lint["batch"] = map[string]any{"steps": [][]string{}} },
		"a placeholder": func(_, lint map[string]any) {
			lint["batch"] = map[string]any{"steps": [][]string{{"tool", "{output}", "{component}"}}}
		},
		"with artifacts": func(_, lint map[string]any) { lint["artifacts"] = []string{"dist/{component}"} },
	} {
		var p map[string]any
		if err := json.Unmarshal([]byte(batchProfile), &p); err != nil {
			t.Fatal(err)
		}
		edit(p, p["tasks"].(map[string]any)["lint"].(map[string]any))
		data, _ := json.Marshal(p)
		if _, err := Decode(data); err == nil || !strings.Contains(err.Error(), "tasks.lint.batch.steps") {
			t.Errorf("%s: Decode = %v, want a tasks.lint.batch.steps error", name, err)
		}
	}
}

func TestMembersWhoseExpectedVersionDiffersRunSeparately(t *testing.T) {
	root := t.TempDir()
	stubBatchTool(t, root)
	profile, err := Decode([]byte(strings.Replace(batchProfile, `"pattern"`, `"files": [{"path": "tool.version"}], "pattern"`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	s := batchStack(t, nil)
	s.Profile = profile
	writeFile(t, root, "tool.version", "1.0.0")
	// c's own directory states another version: its check is its own.
	writeFile(t, root, "c/tool.version", "1.0")
	t.Setenv("STUB_EXIT", "0")
	runLintBatch(t, s, root)
	log, _ := os.ReadFile(filepath.Join(root, "log"))
	if lines := strings.Split(strings.TrimSpace(string(log)), "\n"); len(lines) != 2 {
		t.Errorf("the tool ran %q, want one command for a and b and one for c", lines)
	}
}

func TestMembersThatStateOneVersionInTwoFilesRunOneCommand(t *testing.T) {
	root := t.TempDir()
	stubBatchTool(t, root)
	profile, err := Decode([]byte(strings.Replace(batchProfile, `"pattern"`, `"files": [{"path": "tool.version"}], "pattern"`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	s := batchStack(t, nil)
	s.Profile = profile
	writeFile(t, root, "tool.version", "1.0.0")
	writeFile(t, root, "c/tool.version", "1.0.0")
	runLintBatch(t, s, root)
	log, _ := os.ReadFile(filepath.Join(root, "log"))
	if lines := strings.Split(strings.TrimSpace(string(log)), "\n"); len(lines) != 1 {
		t.Errorf("the tool ran %q, want one command: one version, two files", lines)
	}
}

func TestExpandBatchStepRepeatsAComponentArgument(t *testing.T) {
	vars := map[string]string{"{root}": "/w", BatchDirPlaceholder: "/b"}
	step := []string{"nx", "run-many", "-t", "lint", "-p", "{components}", "--cwd={root}", "--events={batch}/e.json", "{component}:x"}
	got := expandBatchStep([][]string{step, step}, vars, []string{"a", "b"})
	want := []string{"nx", "run-many", "-t", "lint", "-p", "a,b", "--cwd=/w", "--events=/b/e.json", "a:x", "b:x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("expandBatchStep = %q, want %q", got, want)
	}
	// Each component names its own target, as a project whose moon task is
	// named otherwise does.
	got = expandBatchStep([][]string{{"moon", "run", "{component}:test"}, {"moon", "run", "{component}:unit"}}, vars, []string{"api", "pricing"})
	if want := []string{"moon", "run", "api:test", "pricing:unit"}; !reflect.DeepEqual(got, want) {
		t.Errorf("expandBatchStep = %q, want %q", got, want)
	}
	if batchShape([][]string{{"moon", "run", "{component}:test"}}) != batchShape([][]string{{"moon", "run", "{component}:unit"}}) ||
		batchShape([][]string{{"nx", "-t", "test"}}) == batchShape([][]string{{"nx", "-t", "unit"}}) {
		t.Error("a shape keeps every argument but the component's")
	}
}

func TestAnOverrideOfTheStepsDropsTheBatchItDoesNotRestate(t *testing.T) {
	s := batchStack(t, nil)
	p, err := s.Profile.Resolve(json.RawMessage(`{"tasks":{"lint":{"steps":[["tool","check","{component}"]]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Tasks[TaskLint].Batch != nil {
		t.Errorf("batch = %+v, want none for steps the override replaced", p.Tasks[TaskLint].Batch)
	}
	p, err = s.Profile.Resolve(json.RawMessage(`{"tasks":{"lint":{"steps":[["tool","check","{component}"]],"batch":{"steps":[["tool","check","{component}"]]}}}}`))
	if err != nil || p.Tasks[TaskLint].Batch == nil || p.Tasks[TaskLint].Batch.Steps[0][1] != "check" {
		t.Errorf("a restated batch = %+v (%v), want it kept", p.Tasks[TaskLint].Batch, err)
	}
}

func TestAStepsOverrideWritesItsBatchBesideTheSteps(t *testing.T) {
	o := StepsOverride{Extension: "@putnami/tool", Tasks: []string{TaskLint, TaskTest}, Synced: func(any) bool { return true },
		Batch: func(task string, steps [][]string) [][]string { return steps }}
	doc := map[string]any{}
	o.Apply(doc, map[string][][]string{TaskLint: {{"tool", "run", "{component}:check"}}})
	task := doc["options"].(map[string]any)["@putnami/tool"].(map[string]any)["tasks"].(map[string]any)[TaskLint].(map[string]any)
	if _, ok := task["batch"]; !ok {
		t.Fatalf("task = %v, want its batch beside its steps", task)
	}
	o.Apply(doc, nil)
	if _, ok := doc["options"]; ok {
		t.Errorf("doc = %v, want the steps and the batch removed together", doc)
	}
}

func TestTailBufferKeepsTheEnd(t *testing.T) {
	b := &tailBuffer{limit: 4}
	_, _ = b.Write([]byte("abc"))
	_, _ = b.Write([]byte("defg"))
	if got := string(b.Bytes()); got != "defg" {
		t.Errorf("tail = %q, want defg", got)
	}
}

func TestALaterStepReadsNeitherTheReportNorTheOutputOfAnEarlierOne(t *testing.T) {
	root := t.TempDir()
	bin := t.TempDir()
	// first writes the tool's report and prints a section; second fails
	// before it writes either.
	writeFile(t, bin, "tool", "#!/bin/sh\ncase \"$1\" in --version) echo 'tool 1.0.0'; exit 0;; esac\n"+
		"if [ \"$1\" = first ]; then echo stale > report.txt; echo 'section of first'; exit 0; fi\nexit 1\n")
	if err := os.Chmod(filepath.Join(bin, "tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var read BatchReport
	var reportLeft bool
	s := batchStack(t, func(report BatchReport) map[string]BatchOutcome {
		read = report
		_, err := os.Stat(filepath.Join(report.Root, "report.txt"))
		reportLeft = err == nil
		return nil
	})
	s.BatchReports = []string{"report.txt"}
	lint := s.Profile.Tasks[TaskLint]
	lint.Batch = &Batch{Steps: [][]string{{"tool", "first", "{component}"}, {"tool", "second", "{component}"}}}
	s.Profile.Tasks[TaskLint] = lint
	runLintBatch(t, s, root)
	if reportLeft {
		t.Error("the second step's reader found the first step's report")
	}
	if strings.Contains(string(read.Output), "section of first") || read.Command[1] != "second" {
		t.Errorf("the reader read %q of %q, want the second step's output alone", read.Output, read.Command)
	}
}

func TestAPassingBatchHandsItsReportToTheStackAndPassesEveryProject(t *testing.T) {
	root := t.TempDir()
	stubBatchTool(t, root)
	var read []BatchReport
	var dirExisted bool
	// The reader's outcomes do not count when the command passed.
	results := runLintBatch(t, batchStack(t, func(report BatchReport) map[string]BatchOutcome {
		read = append(read, report)
		_, err := os.Stat(report.Dir)
		dirExisted = err == nil
		return map[string]BatchOutcome{"a": {Failed: true, Message: "a failed"}}
	}), root)
	if len(read) != 1 || read[0].Failed || read[0].Dir == "" || !dirExisted || !reflect.DeepEqual(read[0].Command, []string{"tool", "run", "a:lint", "b:lint", "c:lint", "--projects=a,b,c"}) {
		t.Fatalf("the reader read %+v, want one passing report of the command with its batch directory", read)
	}
	if _, err := os.Stat(read[0].Dir); !os.IsNotExist(err) {
		t.Errorf("the batch directory %s remains after the command: %v", read[0].Dir, err)
	}
	for id, r := range results {
		if r.Status != statusOK || len(r.Diagnostics) != 0 {
			t.Errorf("%s = %+v, want a pass", id, r)
		}
	}
}

// reportProfile is a delegated profile whose test runs over several
// components as one command that writes its events into {batch}, then
// collects each component's JUnit report into its output.
const reportProfile = `{
  "id": "tool",
  "delegated": true,
  "markers": ["tool.json"],
  "toolchain": {"command": ["tool", "--version"], "pattern": "(?m)^tool (?<version>[0-9.]+)$"},
  "inputs": {"sources": ["**"]},
  "tasks": {
    "test": {
      "steps": [["tool", "test", "--events={output}/events", "{component}"]],
      "reportSteps": [["tool", "collect", "{output}/events", "{component}", "{output}"]],
      "batch": {
        "steps": [["tool", "test", "--events={batch}/events", "{component}"]],
        "reportSteps": [["tool", "collect", "{batch}/events", "{component}", "{output}"]]
      },
      "inputs": ["sources"],
      "dir": "root",
      "reports": {"junit": ["{output}/reports/*.xml"]}
    }
  }
}`

// stubReportTool puts first on PATH a tool that logs each call; tool test
// writes "events" into the directory --events names, and tool collect
// <events> <component> <output> writes the component's JUnit report, a
// failed case for the component FAIL_COMPONENT names, and exits with
// COLLECT_EXIT for the component COLLECT_FAILS names.
func stubReportTool(t *testing.T, root string) {
	t.Helper()
	bin := t.TempDir()
	writeFile(t, bin, "tool", `#!/bin/sh
case "$1" in --version) echo 'tool 1.0.0'; exit 0;; esac
echo "$*" >> `+filepath.Join(root, "log")+`
case "$1" in
  test) events="${2#--events=}"; echo events > "$events"; exit ${STUB_EXIT:-0};;
  collect)
    [ -f "$2" ] || { echo "no events at $2"; exit 3; }
    mkdir -p "$4/reports"
    if [ "$3" = "$FAIL_COMPONENT" ]; then case_body='<failure message="boom"/>'; else case_body=''; fi
    printf '<testsuite name="%s"><testcase classname="%s" name="t">%s</testcase></testsuite>\n' "$3" "$3" "$case_body" > "$4/reports/$3.xml"
    [ "$3" = "$COLLECT_FAILS" ] && exit ${COLLECT_EXIT:-0}
    exit 0;;
esac
`)
	if err := os.Chmod(filepath.Join(bin, "tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func reportStack(t *testing.T, profileJSON string, read func(BatchReport) map[string]BatchOutcome) Stack {
	t.Helper()
	s := batchStack(t, read)
	profile, err := Decode([]byte(profileJSON))
	if err != nil {
		t.Fatal(err)
	}
	s.Profile = profile
	return s
}

func TestBatchReportStepsRunForEachComponentWithItsOwnOutput(t *testing.T) {
	root := t.TempDir()
	stubReportTool(t, root)
	t.Setenv("FAIL_COMPONENT", "b")
	t.Setenv("STUB_EXIT", "1")
	var dir string
	results := runTaskBatch(t, reportStack(t, reportProfile, func(report BatchReport) map[string]BatchOutcome {
		dir = report.Dir
		return nil
	}), root, TaskTest)
	log, _ := os.ReadFile(filepath.Join(root, "log"))
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	want := []string{"test --events=" + dir + "/events a b c"}
	for _, c := range []string{"a", "b", "c"} {
		want = append(want, "collect "+dir+"/events "+c+" "+filepath.Join(root, ".putnami/out", c, TaskTest))
	}
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("the tool ran %q, want one command then one collect per component:\n%q", lines, want)
	}
	// The command failed and its report names no project: b fails with its
	// failed case, a and c with the command's failure; each read its own
	// report.
	for id, r := range results {
		summary, _ := r.Data["testSummary"].(map[string]any)
		if r.Status != statusFailed || summary["total"] != float64(1) {
			t.Errorf("%s = %+v, want a failure that read its own report", id, r)
		}
	}
	if b := results["/b"]; b.Data["testSummary"].(map[string]any)["failed"] != float64(1) {
		t.Errorf("b = %+v, want its failed case", b)
	}
}

func TestAFailedBatchReportStepFailsItsComponentAlone(t *testing.T) {
	root := t.TempDir()
	stubReportTool(t, root)
	t.Setenv("COLLECT_FAILS", "c")
	t.Setenv("COLLECT_EXIT", "4")
	results := runTaskBatch(t, reportStack(t, reportProfile, nil), root, TaskTest)
	for _, id := range []string{"/a", "/b"} {
		if r := results[id]; r.Status != statusOK {
			t.Errorf("%s = %+v, want a pass", id, r)
		}
	}
	if c := results["/c"]; c.Status != statusFailed || len(c.Diagnostics) != 1 || !strings.Contains(c.Diagnostics[0].Description, "collect") || !strings.Contains(c.Diagnostics[0].Description, "exit status 4") {
		t.Errorf("c = %+v, want its report step's failure", c)
	}
}

// sharedProfile is a delegated profile whose batch writes every component's
// JUnit report into one directory under the stack root.
const sharedProfile = `{
  "id": "tool",
  "delegated": true,
  "markers": ["tool.json"],
  "toolchain": {"command": ["tool", "--version"], "pattern": "(?m)^tool (?<version>[0-9.]+)$"},
  "inputs": {"sources": ["**"]},
  "tasks": {
    "test": {
      "steps": [["tool", "test", "{component}"]],
      "batch": {"steps": [["tool", "test", "{component}"]]},
      "inputs": ["sources"],
      "dir": "root",
      "reports": {"junit": ["{root}/reports/*.xml"]}
    }
  }
}`

func TestEachComponentReadsTheSharedReportsTheStackNamesItsOwn(t *testing.T) {
	root := t.TempDir()
	bin := t.TempDir()
	// The tool writes one report per component it runs, b's with a failed
	// case, and one report of the whole command.
	writeFile(t, bin, "tool", `#!/bin/sh
case "$1" in --version) echo 'tool 1.0.0'; exit 0;; esac
shift
mkdir -p reports
for c in "$@"; do
  body=''; [ "$c" = b ] && body='<failure message="boom"/>'
  printf '<testsuite><testcase classname="%s" name="t">%s</testcase></testsuite>\n' "$c" "$body" > "reports/$c.cases.xml"
done
echo '<testsuite><testcase classname="whole" name="t"/></testsuite>' > reports/whole.xml
echo "wrote the reports of $*"
[ -f reports/b.cases.xml ] && exit 1
exit 0
`)
	if err := os.Chmod(filepath.Join(bin, "tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	s := reportStack(t, sharedProfile, func(BatchReport) map[string]BatchOutcome {
		return map[string]BatchOutcome{"a": {}, "b": {Failed: true, Message: "b failed"}, "c": {}}
	})
	var asked [][]string
	files := 0
	s.BatchReportOwner = func(report BatchReport) func(string) (string, bool) {
		asked = append(asked, report.Components)
		if !strings.Contains(string(report.Output), "wrote the reports of a b c") || report.Root != root {
			t.Errorf("the owner read the report %+v", report)
		}
		return func(file string) (string, bool) {
			files++
			owner, _, _ := strings.Cut(strings.TrimPrefix(file, "reports/"), ".")
			if owner == "whole" {
				return "", false
			}
			return owner, true
		}
	}
	results := runTaskBatch(t, s, root, TaskTest)
	// The owner is built once for the command, and asked of each file each
	// component reads.
	if len(asked) != 1 || !reflect.DeepEqual(asked[0], []string{"a", "b", "c"}) || files != 3*4 {
		t.Errorf("the owner was built for %v and asked %d times, want once for the components of the command, then of each of the 4 files for each of the 3 components", asked, files)
	}
	for id, want := range map[string]struct {
		status        string
		total, failed float64
	}{"/a": {statusOK, 2, 0}, "/b": {statusFailed, 2, 1}, "/c": {statusOK, 2, 0}} {
		r := results[id]
		summary, _ := r.Data["testSummary"].(map[string]any)
		if r.Status != want.status || summary["total"] != want.total || summary["failed"] != want.failed {
			t.Errorf("%s = %s %v, want %s with its own cases and the command's: %+v", id, r.Status, summary, want.status, r)
		}
	}
}

func TestABatchReportStepIsAComponentsOwn(t *testing.T) {
	for name, edit := range map[string]func(test map[string]any){
		"a task report step without the batch's": func(test map[string]any) {
			delete(test["batch"].(map[string]any), "reportSteps")
		},
		"a placeholder": func(test map[string]any) {
			test["batch"].(map[string]any)["reportSteps"] = [][]string{{"tool", "{components}"}}
		},
		"no program": func(test map[string]any) {
			test["batch"].(map[string]any)["reportSteps"] = [][]string{{}}
		},
	} {
		var p map[string]any
		if err := json.Unmarshal([]byte(reportProfile), &p); err != nil {
			t.Fatal(err)
		}
		edit(p["tasks"].(map[string]any)["test"].(map[string]any))
		data, _ := json.Marshal(p)
		if _, err := Decode(data); err == nil || !strings.Contains(err.Error(), "tasks.test.batch.reportSteps") {
			t.Errorf("%s: Decode = %v, want a tasks.test.batch.reportSteps error", name, err)
		}
	}
}
