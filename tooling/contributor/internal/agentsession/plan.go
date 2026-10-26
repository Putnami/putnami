package agentsession

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	collab "go.putnami.dev/protocol/collaboration"
)

const (
	// sessionPlanMaxSteps is the step bound of missions.sync.
	sessionPlanMaxSteps = collab.MaxListMembers
	sessionPlanMaxText  = collab.MaxPlanStepTextLength
)

var sessionTaskIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// planStep is one step of a session plan, in the contract's vocabulary:
// status is todo, doing or done.
type planStep = collab.PlanStep

// planStatus maps the status of an engine's plan tool to the contract's
// vocabulary. Claude Code and Codex share pending, in_progress and completed.
func planStatus(raw string) (collab.PlanStepStatus, bool) {
	switch raw {
	case "pending":
		return collab.PlanStepTodo, true
	case "in_progress":
		return collab.PlanStepDoing, true
	case "completed":
		return collab.PlanStepDone, true
	}
	return "", false
}

// isPlanTool reports whether a tool call can change the plan.
func isPlanTool(toolName string) bool {
	switch toolName {
	case "TodoWrite", "update_plan", "TaskCreate", "TaskUpdate":
		return true
	}
	return false
}

// applyPlanTool returns the plan after one tool call, and false when the call
// leaves the plan as it was.
//
//   - Claude Code TodoWrite sends tool_input.todos[{content,status}] and
//     Codex update_plan sends tool_input.plan[{step,status}]: both replace the
//     whole plan, and a step keeps its key while its text stays the same.
//   - Claude Code TaskCreate and TaskUpdate change one task, keyed by the task
//     id that TaskCreate answers in tool_response.task.id.
func applyPlanTool(toolName string, input, response json.RawMessage, plan []planStep) ([]planStep, bool) {
	switch toolName {
	case "TodoWrite":
		var body struct {
			Todos []struct {
				Content string `json:"content"`
				Status  string `json:"status"`
			} `json:"todos"`
		}
		if json.Unmarshal(input, &body) != nil || body.Todos == nil {
			return plan, false
		}
		steps := make([]wholePlanStep, 0, len(body.Todos))
		for _, todo := range body.Todos {
			steps = append(steps, wholePlanStep{text: todo.Content, status: todo.Status})
		}
		return replacePlan(steps), true
	case "update_plan":
		var body struct {
			Plan []struct {
				Step   string `json:"step"`
				Status string `json:"status"`
			} `json:"plan"`
		}
		if json.Unmarshal(input, &body) != nil || body.Plan == nil {
			return plan, false
		}
		steps := make([]wholePlanStep, 0, len(body.Plan))
		for _, item := range body.Plan {
			steps = append(steps, wholePlanStep{text: item.Step, status: item.Status})
		}
		return replacePlan(steps), true
	case "TaskCreate":
		return applyTaskCreate(input, response, plan)
	case "TaskUpdate":
		return applyTaskUpdate(input, response, plan)
	}
	return plan, false
}

func applyTaskCreate(input, response json.RawMessage, plan []planStep) ([]planStep, bool) {
	var in struct {
		Subject string `json:"subject"`
	}
	var out struct {
		Task struct {
			ID      json.RawMessage `json:"id"`
			Subject string          `json:"subject"`
		} `json:"task"`
	}
	if json.Unmarshal(input, &in) != nil || json.Unmarshal(response, &out) != nil {
		return plan, false
	}
	id, ok := taskID(out.Task.ID)
	text := planText(firstString(out.Task.Subject, in.Subject))
	if !ok || text == "" {
		return plan, false
	}
	next := clonePlan(plan)
	if index := planIndex(next, "task-"+id); index >= 0 {
		next[index].Text = text
		return next, true
	}
	if len(next) >= sessionPlanMaxSteps {
		return plan, false
	}
	return append(next, planStep{Key: "task-" + id, Text: text, Status: collab.PlanStepTodo}), true
}

func applyTaskUpdate(input, response json.RawMessage, plan []planStep) ([]planStep, bool) {
	var in struct {
		TaskID  json.RawMessage `json:"taskId"`
		Status  string          `json:"status"`
		Subject string          `json:"subject"`
	}
	var out struct {
		Success *bool `json:"success"`
	}
	if json.Unmarshal(input, &in) != nil {
		return plan, false
	}
	if json.Unmarshal(response, &out) == nil && out.Success != nil && !*out.Success {
		return plan, false
	}
	id, ok := taskID(in.TaskID)
	if !ok {
		return plan, false
	}
	key := "task-" + id
	next := clonePlan(plan)
	index := planIndex(next, key)
	if in.Status == "deleted" {
		if index < 0 {
			return plan, false
		}
		return append(next[:index], next[index+1:]...), true
	}
	status, known := planStatus(in.Status)
	text := planText(in.Subject)
	if index < 0 {
		if len(next) >= sessionPlanMaxSteps {
			return plan, false
		}
		if !known {
			status = collab.PlanStepTodo
		}
		return append(next, planStep{Key: key, Text: firstString(text, "Task "+id), Status: status}), true
	}
	if known {
		next[index].Status = status
	}
	if text != "" {
		next[index].Text = text
	}
	return next, true
}

type wholePlanStep struct {
	text, status string
}

// replacePlan builds a whole plan. A step's key is a digest of its
// normalized text, so the provider sees the same step across updates; a
// repeated text gets a numbered key.
func replacePlan(steps []wholePlanStep) []planStep {
	plan := make([]planStep, 0, min(len(steps), sessionPlanMaxSteps))
	used := map[string]int{}
	for _, step := range steps {
		if len(plan) == sessionPlanMaxSteps {
			break
		}
		text := planText(step.text)
		if text == "" {
			continue
		}
		status, ok := planStatus(step.status)
		if !ok {
			status = collab.PlanStepTodo
		}
		key := planKey(text)
		used[key]++
		if used[key] > 1 {
			key += "-" + strconv.Itoa(used[key])
		}
		plan = append(plan, planStep{Key: key, Text: text, Status: status})
	}
	return plan
}

func planKey(text string) string {
	normalized := strings.ToLower(strings.Join(strings.Fields(text), " "))
	sum := sha256.Sum256([]byte(normalized))
	return "s" + hex.EncodeToString(sum[:6])
}

// planText is one line of at most sessionPlanMaxText characters, without
// control characters: the contract refuses a plan step with a line break,
// and an item copied from terminal output can hold ESC.
func planText(raw string) string {
	text := lineText(raw)
	if utf8.RuneCountInString(text) > sessionPlanMaxText {
		text = string([]rune(text)[:sessionPlanMaxText-1]) + "…"
	}
	return text
}

// taskID reads a task id written as a JSON string or number.
func taskID(raw json.RawMessage) (string, bool) {
	var text string
	if json.Unmarshal(raw, &text) != nil {
		var number json.Number
		if json.Unmarshal(raw, &number) != nil {
			return "", false
		}
		text = number.String()
	}
	return text, sessionTaskIDPattern.MatchString(text)
}

func planIndex(plan []planStep, key string) int {
	for i, step := range plan {
		if step.Key == key {
			return i
		}
	}
	return -1
}

func clonePlan(plan []planStep) []planStep {
	return append([]planStep(nil), plan...)
}

// planDigest identifies a plan, so a sync sends it only when it changed.
func planDigest(plan []planStep) string {
	data, _ := json.Marshal(plan)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// firstString returns the first value that is not empty.
func firstString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
