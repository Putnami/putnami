package agentsession

import (
	"encoding/json"
	"fmt"
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
	if !observeToolUse(cache, spooledToolUse{ToolName: "TodoWrite", ToolInput: input}) {
		t.Fatal("a new plan was not recorded")
	}
	if observeToolUse(cache, spooledToolUse{ToolName: "TodoWrite", ToolInput: input}) {
		t.Error("the same plan was recorded twice")
	}
}

func TestTheDesktopPlanToolChangesStatuses(t *testing.T) {
	spectest.Proves(t, sessionFeature, toolRequirement, "the-desktop-plan-tool-changes-the-plan")
	apply := func(plan []planStep, input string) ([]planStep, bool) {
		return applyPlanTool(desktopPlanTool, json.RawMessage(input), json.RawMessage(`[{"type":"text","text":"Plan updated."}]`), plan)
	}
	statuses := func(plan []planStep) string {
		var out []string
		for _, step := range plan {
			out = append(out, string(step.Status))
		}
		return strings.Join(out, ",")
	}
	plan, ok := apply(nil, `{"items":[{"content":"Build","status":"completed"},{"content":"Test","status":"pending"},{"content":"Ship","status":"pending"}],"scope":"continuation"}`)
	if !ok || len(plan) != 3 || plan[0].Key != planKey("Build") || statuses(plan) != "done,todo,todo" {
		t.Fatalf("items %+v %v", plan, ok)
	}
	if todo, _ := applyPlanTool("TodoWrite", json.RawMessage(`{"todos":[{"content":"Build","status":"completed"},{"content":"Test","status":"pending"},{"content":"Ship","status":"pending"}]}`), nil, nil); !slices.Equal(todo, plan) {
		t.Errorf("items %+v, TodoWrite %+v", plan, todo)
	}
	for _, tc := range []struct {
		name, input, want string
		ok                bool
	}{
		{"advance without a step in progress", `{"advance":true}`, "done,doing,todo", true},
		{"start", `{"start":3}`, "done,todo,doing", true},
		{"done and start", `{"done":[2],"start":3}`, "done,done,doing", true},
		{"done outside the plan", `{"done":[0,4]}`, "done,todo,todo", false},
		{"done again", `{"done":[1]}`, "done,todo,todo", false},
		{"empty call", `{}`, "done,todo,todo", false},
		{"bad input", `{"done":"2"}`, "done,todo,todo", false},
	} {
		got, ok := apply(plan, tc.input)
		if ok != tc.ok || statuses(got) != tc.want {
			t.Errorf("%s: %s %v, want %s %v", tc.name, statuses(got), ok, tc.want, tc.ok)
		}
	}
	doing, _ := apply(plan, `{"start":2}`)
	if got, ok := apply(doing, `{"advance":true}`); !ok || statuses(got) != "done,done,doing" {
		t.Errorf("advance %s %v", statuses(got), ok)
	}
	last, _ := apply(plan, `{"done":[2],"start":3}`)
	if got, ok := apply(last, `{"advance":true}`); !ok || statuses(got) != "done,done,done" {
		t.Errorf("advance past the last step %s %v", statuses(got), ok)
	}
	if got, ok := apply(nil, `{"advance":true}`); ok || len(got) != 0 {
		t.Errorf("advance without a plan %+v %v", got, ok)
	}
	if !isPlanTool(desktopPlanTool) {
		t.Error("the desktop plan tool is not a plan tool")
	}
}

func TestTheDesktopWrapUpCompletesTheStepsInProgress(t *testing.T) {
	spectest.Proves(t, sessionFeature, toolRequirement, "the-desktop-plan-tool-changes-the-plan")
	plan := []planStep{{Key: "a", Text: "a", Status: collab.PlanStepDone}, {Key: "b", Text: "b", Status: collab.PlanStepDoing}, {Key: "c", Text: "c", Status: collab.PlanStepTodo}}
	done, ok := applyPlanTool(wrapUpTool, json.RawMessage(`{"recap":"Done."}`), nil, plan)
	if !ok || len(done) != 3 || done[1].Status != collab.PlanStepDone || done[2].Status != collab.PlanStepTodo || plan[1].Status != collab.PlanStepDoing {
		t.Errorf("wrap-up %+v %v, plan %+v", done, ok, plan)
	}
	if _, ok := applyPlanTool(wrapUpTool, json.RawMessage(`{}`), nil, done); ok {
		t.Error("a wrap-up without a step in progress changed the plan")
	}
	if _, ok := applyPlanTool(wrapUpTool, json.RawMessage(`{}`), nil, nil); ok {
		t.Error("a wrap-up without a plan changed it")
	}
	if !isPlanTool(wrapUpTool) {
		t.Error("the desktop wrap-up is not a plan tool")
	}
}

func TestARepeatedPlanCallIsAppliedOnce(t *testing.T) {
	spectest.Proves(t, sessionFeature, toolRequirement, "a-repeated-plan-call-is-applied-once")
	cache := &sessionCache{}
	call := func(id, input string) bool {
		return observeToolUse(cache, spooledToolUse{ToolName: desktopPlanTool, ToolInput: json.RawMessage(input), ToolUseID: id})
	}
	call("toolu_1", `{"items":[{"content":"one","status":"in_progress"},{"content":"two","status":"pending"},{"content":"three","status":"pending"}]}`)
	if !call("toolu_2", `{"advance":true}`) {
		t.Fatal("the advance was not recorded")
	}
	if call("toolu_2", `{"advance":true}`) {
		t.Error("a repeated advance was applied twice")
	}
	if got := []collab.PlanStepStatus{cache.Plan[0].Status, cache.Plan[1].Status, cache.Plan[2].Status}; !slices.Equal(got, []collab.PlanStepStatus{collab.PlanStepDone, collab.PlanStepDoing, collab.PlanStepTodo}) {
		t.Errorf("statuses %v", got)
	}
	if !call("toolu_3", `{"advance":true}`) || cache.Plan[2].Status != collab.PlanStepDoing {
		t.Errorf("a new advance %+v", cache.Plan)
	}
	if call("toolu_2", `{"advance":true}`) {
		t.Error("an earlier call delivered again after a later one was applied")
	}
	if !call("", `{"done":[3]}`) || !call("", `{"start":3}`) {
		t.Error("a call without an id was not applied")
	}
	for i := range planToolUseMemory + 4 {
		call(fmt.Sprintf("toolu_x%d", i), `{"done":[1]}`)
	}
	if len(cache.PlanToolUses) != planToolUseMemory || cache.PlanToolUses[planToolUseMemory-1] != fmt.Sprintf("toolu_x%d", planToolUseMemory+3) {
		t.Errorf("remembered ids %v", cache.PlanToolUses)
	}
}
