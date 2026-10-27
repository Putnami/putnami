package agentsession

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

func TestAWholePlanKeepsItsStepKeys(t *testing.T) {
	spectest.Proves(t, sessionFeature, toolRequirement, "post-tool-use-records-the-plan-without-git-or-provider")
	first := replacePlan([]wholePlanStep{{"Build", "in_progress"}, {"Test", "pending"}, {"Test", "pending"}, {"  ", "pending"}, {"Ship", "later"}})
	second := replacePlan([]wholePlanStep{{"build", "completed"}, {"Test", "in_progress"}})
	if len(first) != 4 || first[1].Key == first[2].Key || first[2].Key != first[1].Key+"-2" || first[3].Status != collab.PlanStepTodo {
		t.Errorf("first %+v", first)
	}
	if second[0].Key != first[0].Key || second[0].Status != collab.PlanStepDone || second[1].Key != first[1].Key {
		t.Errorf("a step changed its key: %+v then %+v", first, second)
	}
	text := planText("\x1b[31mred\x1b[0m\tstep\n" + strings.Repeat("é", 400))
	if !strings.HasPrefix(text, "red step ") || utf8.RuneCountInString(text) != sessionPlanMaxText || !strings.HasSuffix(text, "…") {
		t.Errorf("plan text %q", text)
	}
	many := make([]wholePlanStep, 0, sessionPlanMaxSteps+5)
	for i := range sessionPlanMaxSteps + 5 {
		many = append(many, wholePlanStep{text: strings.Repeat("x", i+1), status: "pending"})
	}
	if plan := replacePlan(many); len(plan) != sessionPlanMaxSteps {
		t.Errorf("%d steps", len(plan))
	}
}

func TestTheTaskToolsChangeOneStep(t *testing.T) {
	spectest.Proves(t, sessionFeature, toolRequirement, "post-tool-use-records-the-plan-without-git-or-provider")
	apply := func(plan []planStep, tool, input, response string) ([]planStep, bool) {
		return applyPlanTool(tool, json.RawMessage(input), json.RawMessage(response), plan)
	}
	plan, ok := apply(nil, "TaskCreate", `{"subject":"write"}`, `{"task":{"id":7}}`)
	if !ok || !slices.Equal(plan, []planStep{{Key: "task-7", Status: collab.PlanStepTodo, Text: "write"}}) {
		t.Fatalf("create %+v %v", plan, ok)
	}
	if again, ok := apply(plan, "TaskCreate", `{"subject":"write it"}`, `{"task":{"id":"7"}}`); !ok || len(again) != 1 || again[0].Text != "write it" {
		t.Errorf("a repeated create %+v", again)
	}
	for _, tc := range []struct {
		name, tool, input, response string
		ok                          bool
		want                        []planStep
	}{
		{"progress", "TaskUpdate", `{"taskId":"7","status":"in_progress","subject":"write more"}`, `{"success":true}`, true, []planStep{{Key: "task-7", Status: collab.PlanStepDoing, Text: "write more"}}},
		{"unknown status", "TaskUpdate", `{"taskId":"7","status":"blocked"}`, `{}`, true, plan},
		{"failed update", "TaskUpdate", `{"taskId":"7","status":"completed"}`, `{"success":false}`, false, plan},
		{"unknown task", "TaskUpdate", `{"taskId":"9","status":"completed"}`, `{}`, true, append(slices.Clone(plan), planStep{Key: "task-9", Status: collab.PlanStepDone, Text: "Task 9"})},
		{"delete", "TaskUpdate", `{"taskId":"7","status":"deleted"}`, `{}`, true, []planStep{}},
		{"delete unknown", "TaskUpdate", `{"taskId":"8","status":"deleted"}`, `{}`, false, plan},
		{"bad id", "TaskUpdate", `{"taskId":"a b","status":"completed"}`, `{}`, false, plan},
		{"create without id", "TaskCreate", `{"subject":"x"}`, `{"task":{}}`, false, plan},
		{"create without text", "TaskCreate", `{"subject":" "}`, `{"task":{"id":"8"}}`, false, plan},
		{"todo without list", "TodoWrite", `{}`, `{}`, false, plan},
		{"codex without list", "update_plan", `{"explanation":"x"}`, `""`, false, plan},
		{"other tool", "Bash", `{}`, `{}`, false, plan},
	} {
		got, ok := apply(plan, tc.tool, tc.input, tc.response)
		if ok != tc.ok || len(got) != len(tc.want) || (len(got) > 0 && !slices.Equal(got, tc.want)) {
			t.Errorf("%s: %+v %v, want %+v %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
	// A cleared list is an empty plan.
	if cleared, ok := apply(plan, "TodoWrite", `{"todos":[]}`, `{}`); !ok || len(cleared) != 0 {
		t.Errorf("cleared %+v %v", cleared, ok)
	}
}

func TestAnUnchangedPlanIsNotRecorded(t *testing.T) {
	spectest.Proves(t, sessionFeature, toolRequirement, "post-tool-use-records-the-plan-without-git-or-provider")
	cache := &sessionCache{}
	input := json.RawMessage(`{"todos":[{"content":"one","status":"pending"}]}`)
	if !observeToolUse(cache, "TodoWrite", input, nil) {
		t.Fatal("a new plan was not recorded")
	}
	if observeToolUse(cache, "TodoWrite", input, nil) {
		t.Error("the same plan was recorded twice")
	}
}
