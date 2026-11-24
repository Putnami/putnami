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

// stubBatchTool puts first on PATH a tool that prints its version, logs each
// run, prints "output of <arg>" for each target it runs and exits with
// STUB_EXIT.
func stubBatchTool(t *testing.T, root string) {
	t.Helper()
	bin := t.TempDir()
	writeFile(t, bin, "tool", "#!/bin/sh\ncase \"$1\" in --version) echo 'tool 1.0.0'; exit 0;; esac\n"+
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
	var selected []pctx.ProjectRef
	for _, p := range []string{"a", "b", "c"} {
		writeFile(t, root, p+"/tool.json", "{}")
		selected = append(selected, pctx.ProjectRef{ID: "/" + p, Name: p, Path: p, FullPath: filepath.Join(root, p), OutputPath: filepath.Join(root, ".putnami/out", p, "lint")})
	}
	meta, _ := json.Marshal(Metadata{Component: "a", Root: "."})
	ctx := &pctx.Context{
		WorkspaceRoot:    root,
		Project:          pctx.Project{Name: "a", Path: "a", FullPath: filepath.Join(root, "a"), Metadata: map[string]json.RawMessage{s.Extension: meta}},
		SelectedProjects: selected,
	}
	status, data, err := s.Jobs()[TaskLint](ctx, jsonl.New(), []string{"--report-dir", selected[0].OutputPath})
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

func TestExpandBatchStepRepeatsAComponentArgument(t *testing.T) {
	step := []string{"nx", "run-many", "-t", "lint", "-p", "{components}", "--cwd={root}", "{component}:x"}
	got := expandBatchStep([][]string{step, step}, "/w", []string{"a", "b"})
	want := []string{"nx", "run-many", "-t", "lint", "-p", "a,b", "--cwd=/w", "a:x", "b:x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("expandBatchStep = %q, want %q", got, want)
	}
	// Each component names its own target, as a project whose moon task is
	// named otherwise does.
	got = expandBatchStep([][]string{{"moon", "run", "{component}:test"}, {"moon", "run", "{component}:unit"}}, "/w", []string{"api", "pricing"})
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
