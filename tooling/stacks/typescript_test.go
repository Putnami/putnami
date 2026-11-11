package stacks

import (
	"reflect"
	"strings"
	"testing"
)

func TestTypeScriptRunsTheToolOfTheScript(t *testing.T) {
	for _, c := range []struct {
		task, script string
		runs         bool
	}{
		{TaskBuild, "bun build src/index.ts --outdir dist", true},
		{TaskBuild, "tsc", true},
		{TaskBuild, "tsc -p tsconfig.lib.json", true},
		{TaskBuild, "webpack", false},
		{TaskBuild, "tsc && cp -r assets dist", false},
		{TaskTest, "vitest run", true},
		{TaskTest, "jest --ci", true},
		{TaskTest, "bun test", true},
		{TaskTest, "mocha", false},
		{TaskTest, "jest --config jest.ci.config.js", false},
		{TaskTest, "vitest run --project unit", false},
		{TaskTest, "bun test test/unit", false},
		{TaskTest, "npx vitest run --coverage", true},
		{TaskTest, "bun test run", false},
		{TaskTest, "", false},
		{TaskLint, "eslint .", true},
		{TaskLint, "eslint --max-warnings 0 src", false},
		{TaskLint, "biome check .", true},
		{TaskLint, "eslint . && prettier --check .", false},
		{TaskLint, "tslint", false},
	} {
		if got := TypeScriptRuns(c.task, c.script); got != c.runs {
			t.Errorf("TypeScriptRuns(%s, %q) = %v, want %v", c.task, c.script, got, c.runs)
		}
	}
	if !TypeScriptBuildsWithBun("bun build src/index.ts") || TypeScriptBuildsWithBun("tsc") || TypeScriptBuildsWithBun("bun build a && cp b c") {
		t.Error("only a bun build alone builds with Bun")
	}
	if TestRunner("npx vitest run") != "vitest" || TestRunner("jest") != "jest" || TestRunner("bun test") != "" {
		t.Error("TestRunner reads vitest and jest alone")
	}
	if !LintsWithESLint("eslint") || LintsWithESLint("biome check") || LintsWithESLint("eslint --fix src") {
		t.Error("LintsWithESLint reads a bare ESLint alone")
	}
}

func TestTypeScriptUnmappedReasonSaysWhy(t *testing.T) {
	keeps := "it keeps running through the tool"
	for script, want := range map[string]string{
		"":                            "runs no command",
		"eslint . && prettier .":      "chains another command",
		"eslint --max-warnings 0 src": "takes arguments",
		"mocha":                       `not "mocha"`,
		"jest --config ci.js":         `takes arguments ("--config ci.js")`,
	} {
		task := TaskTest
		if strings.Contains(script, "eslint") {
			task = TaskLint
		}
		if got := TypeScriptUnmappedReason(task, script, keeps); !strings.Contains(got, want) || !strings.HasSuffix(got, keeps) {
			t.Errorf("reason for %q = %q, want %q", script, got, want)
		}
	}
	if got := TypeScriptUnmappedReason(TaskBuild, "webpack", keeps); !strings.Contains(got, "bun build or tsc alone") {
		t.Errorf("a build reason names tsc: %q", got)
	}
}

func TestTheRunnerAndTheLinterOptions(t *testing.T) {
	doc := map[string]any{}
	SetTypeScriptRunner(doc, "vitest")
	SetTypeScriptLinter(doc, true)
	want := map[string]any{"options": map[string]any{
		TypeScriptRunnerLayer: map[string]any{"runner": "vitest"},
		TypeScriptExtension:   map[string]any{"lint": map[string]any{"linter": ESLintLinter}},
	}}
	if !reflect.DeepEqual(doc, want) {
		t.Errorf("doc = %v", doc)
	}
	SetTypeScriptRunner(doc, "")
	SetTypeScriptLinter(doc, false)
	if len(doc) != 0 {
		t.Errorf("a cleared doc = %v", doc)
	}
	// A linter the project chose stays.
	doc = map[string]any{"options": map[string]any{TypeScriptExtension: map[string]any{"lint": map[string]any{"linter": "biome"}}}}
	SetTypeScriptLinter(doc, true)
	SetTypeScriptLinter(doc, false)
	if doc["options"].(map[string]any)[TypeScriptExtension].(map[string]any)["lint"].(map[string]any)["linter"] != "biome" {
		t.Errorf("the project's linter changed: %v", doc)
	}
}

func TestStepsOverrideKeepsTheProjectsOwnSteps(t *testing.T) {
	o := StepsOverride{Extension: testExtension, Tasks: []string{TaskBuild, TaskTest, TaskLint}, Dir: DirProject, Synced: func(steps any) bool {
		list, _ := steps.([]any)
		return steps == nil || (len(list) == 1 && reflect.DeepEqual(list[0], []any{"run", TaskTest}))
	}}
	own := []any{[]any{"make", "lint"}}
	doc := map[string]any{"options": map[string]any{testExtension: map[string]any{"tasks": map[string]any{
		TaskLint: map[string]any{"steps": own},
		TaskTest: map[string]any{"reports": map[string]any{"junit": []any{"r.xml"}}},
	}}}}
	o.Apply(doc, map[string][][]string{TaskTest: {{"run", TaskTest}}, TaskLint: {{"run", TaskLint}}})
	tasks := doc["options"].(map[string]any)[testExtension].(map[string]any)["tasks"].(map[string]any)
	if !reflect.DeepEqual(tasks[TaskLint], map[string]any{"steps": own}) {
		t.Errorf("the project's own lint steps changed: %v", tasks[TaskLint])
	}
	if !reflect.DeepEqual(tasks[TaskTest], map[string]any{"steps": []any{[]any{"run", TaskTest}}, "dir": DirProject, "reports": map[string]any{"junit": []any{"r.xml"}}}) {
		t.Errorf("test = %v", tasks[TaskTest])
	}
	o.Apply(doc, nil)
	tasks = doc["options"].(map[string]any)[testExtension].(map[string]any)["tasks"].(map[string]any)
	if !reflect.DeepEqual(tasks[TaskTest], map[string]any{"reports": map[string]any{"junit": []any{"r.xml"}}}) || tasks[TaskLint] == nil {
		t.Errorf("after a removal tasks = %v", tasks)
	}
	empty := map[string]any{}
	o.Apply(empty, nil)
	if len(empty) != 0 {
		t.Errorf("an empty doc = %v", empty)
	}
}
