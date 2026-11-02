package estimate

import (
	"encoding/json"
	"fmt"
	"strings"
)

// EpicProtocolVersion is the version of the epic plan format.
const EpicProtocolVersion = 1

// EpicSchemaFile is the JSON Schema of the epic plan format, in schemas/.
const EpicSchemaFile = "epic-plan.v1.json"

// EpicPlan is an epic plan: the issues of one outcome, their dependencies,
// and the envelope the owner approves for all of them.
type EpicPlan struct {
	ProtocolVersion int         `json:"protocolVersion"`
	Epic            string      `json:"epic"`
	Title           string      `json:"title"`
	Base            string      `json:"base"`
	Envelope        Envelope    `json:"envelope"`
	Issues          []EpicIssue `json:"issues"`
}

// Envelope is what the epic may spend: tokens and active minutes over every
// child session, and how many child sessions run at once.
type Envelope struct {
	Tokens        int `json:"tokens"`
	ActiveMinutes int `json:"activeMinutes"`
	Sessions      int `json:"sessions"`
}

// EpicIssue is one issue of an epic: one change delivered by one proposal.
// Task is absent until the issue is created.
type EpicIssue struct {
	Key       string   `json:"key"`
	Title     string   `json:"title"`
	Task      *TaskRef `json:"task,omitempty"`
	DependsOn []string `json:"dependsOn"`
	Slices    []Slice  `json:"slices"`
}

// TaskRef is the whole reference of a task, as the tasks provider answers it.
type TaskRef struct {
	Source string `json:"source"`
	ID     string `json:"id"`
}

const (
	maxEnvelopeTokens  = 1_000_000_000_000
	maxEnvelopeMinutes = 100_000
	refPattern         = "^[!-~]+$"
)

// epicRules is the epic plan format. schemas/epic-plan.v1.json states the
// same members and bounds; a test fails when the two differ.
var epicRules = []rule{
	{name: "protocolVersion", kind: kindInteger, required: true, min: EpicProtocolVersion, max: EpicProtocolVersion},
	{name: "epic", kind: kindString, required: true, min: 1, max: 64, pattern: issuePattern},
	{name: "title", kind: kindString, required: true, min: 1, max: 200, pattern: linePattern},
	{name: "base", kind: kindString, required: true, min: 1, max: 255, pattern: refPattern},
	{name: "envelope", kind: kindObject, required: true, fields: []rule{
		{name: "tokens", kind: kindInteger, required: true, min: 1, max: maxEnvelopeTokens},
		{name: "activeMinutes", kind: kindInteger, required: true, min: 1, max: maxEnvelopeMinutes},
		{name: "sessions", kind: kindInteger, required: true, min: 1, max: 20},
	}},
	{name: "issues", kind: kindObjectList, required: true, min: 1, max: 50, fields: epicIssueRules},
}

var epicIssueRules = []rule{
	{name: "key", kind: kindString, required: true, min: 1, max: 64, pattern: keyPattern},
	{name: "title", kind: kindString, required: true, min: 1, max: 200, pattern: linePattern},
	{name: "task", kind: kindObject, fields: []rule{
		{name: "source", kind: kindString, required: true, min: 1, max: 200, pattern: refPattern},
		{name: "id", kind: kindString, required: true, min: 1, max: 200, pattern: refPattern},
	}},
	{name: "dependsOn", kind: kindStringList, required: true, min: 0, max: 50, itemMin: 1, itemMax: 64, pattern: keyPattern, unique: true},
	{name: "slices", kind: kindObjectList, required: true, min: 1, max: 50, fields: sliceRules},
}

func init() {
	compileRules(epicRules)
}

// ParseEpicPlan reads an epic plan strictly, as ParsePlan reads a change
// plan. Issue keys are unique, slice keys are unique within their issue, and
// each dependency names another issue of the plan. The dependencies form no
// cycle.
func ParseEpicPlan(data []byte) (EpicPlan, error) {
	value, err := decodeStrict(data, "epic")
	if err != nil {
		return EpicPlan{}, err
	}
	if err := checkObject("epic", value, epicRules); err != nil {
		return EpicPlan{}, err
	}
	var plan EpicPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return EpicPlan{}, fmt.Errorf("epic: %w", err)
	}
	keys := map[string]int{}
	for i, issue := range plan.Issues {
		if first, repeated := keys[issue.Key]; repeated {
			return EpicPlan{}, fmt.Errorf("epic.issues[%d].key: %q repeats the key of epic.issues[%d]", i, issue.Key, first)
		}
		keys[issue.Key] = i
		slices := map[string]int{}
		for j, slice := range issue.Slices {
			if first, repeated := slices[slice.Key]; repeated {
				return EpicPlan{}, fmt.Errorf("epic.issues[%d].slices[%d].key: %q repeats the key of epic.issues[%d].slices[%d]", i, j, slice.Key, i, first)
			}
			slices[slice.Key] = j
		}
	}
	for i, issue := range plan.Issues {
		for j, dependency := range issue.DependsOn {
			at := fmt.Sprintf("epic.issues[%d].dependsOn[%d]", i, j)
			if dependency == issue.Key {
				return EpicPlan{}, fmt.Errorf("%s: %q names the issue itself", at, dependency)
			}
			if _, known := keys[dependency]; !known {
				return EpicPlan{}, fmt.Errorf("%s: %q names no issue of the epic", at, dependency)
			}
		}
	}
	if cycle := findCycle(plan.Issues, keys); len(cycle) > 0 {
		return EpicPlan{}, fmt.Errorf("epic.issues: the dependencies form a cycle: %s", strings.Join(cycle, " -> "))
	}
	return plan, nil
}

// findCycle returns the keys of one dependency cycle, its first key repeated
// at the end, or nil when the dependencies form none. It walks the issues in
// plan order, so the same plan names the same cycle.
func findCycle(issues []EpicIssue, keys map[string]int) []string {
	const (
		unvisited = iota
		visiting
		done
	)
	state := make([]int, len(issues))
	var stack []string
	var visit func(int) []string
	visit = func(i int) []string {
		state[i] = visiting
		stack = append(stack, issues[i].Key)
		for _, dependency := range issues[i].DependsOn {
			j := keys[dependency]
			switch state[j] {
			case visiting:
				start := 0
				for k, key := range stack {
					if key == dependency {
						start = k
					}
				}
				return append(append([]string{}, stack[start:]...), dependency)
			case unvisited:
				if cycle := visit(j); cycle != nil {
					return cycle
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[i] = done
		return nil
	}
	for i := range issues {
		if state[i] == unvisited {
			if cycle := visit(i); cycle != nil {
				return cycle
			}
		}
	}
	return nil
}

// EpicResult is the estimate of an epic plan: each issue's estimate and wave,
// and the epic's sums.
type EpicResult struct {
	ProtocolVersion int             `json:"protocolVersion"`
	Plan            string          `json:"plan"`
	Epic            string          `json:"epic"`
	Title           string          `json:"title"`
	Base            string          `json:"base"`
	Envelope        Envelope        `json:"envelope"`
	Size            Size            `json:"size"`
	Tokens          Range           `json:"tokens"`
	Minutes         Range           `json:"minutes"`
	Waves           [][]string      `json:"waves"`
	Issues          []IssueEstimate `json:"issues"`
}

// IssueEstimate is the estimate of one issue of an epic. Its wave is 1 with
// no dependency, else one more than the latest wave of its dependencies.
type IssueEstimate struct {
	Key       string   `json:"key"`
	Title     string   `json:"title"`
	Task      *TaskRef `json:"task,omitempty"`
	DependsOn []string `json:"dependsOn"`
	Wave      int      `json:"wave"`
	Size      Size     `json:"size"`
	Tokens    Range    `json:"tokens"`
	Minutes   Range    `json:"minutes"`
}

// EstimateEpic sizes each issue of plan from its slices with table, sums the
// issues, and orders them in waves. Issues of a wave keep plan order. plan
// comes from ParseEpicPlan, so its dependencies are known and acyclic.
func EstimateEpic(plan EpicPlan, table Table, path string) EpicResult {
	result := EpicResult{
		ProtocolVersion: ResultProtocolVersion, Plan: path, Epic: plan.Epic, Title: plan.Title,
		Base: plan.Base, Envelope: plan.Envelope, Waves: [][]string{}, Issues: []IssueEstimate{},
	}
	waves := Waves(plan.Issues)
	sensitive := false
	for _, issue := range plan.Issues {
		estimate := Estimate(Plan{ProtocolVersion: PlanProtocolVersion, Issue: plan.Epic, Slices: issue.Slices}, table, path)
		dependsOn := append([]string{}, issue.DependsOn...)
		result.Issues = append(result.Issues, IssueEstimate{
			Key: issue.Key, Title: issue.Title, Task: issue.Task, DependsOn: dependsOn, Wave: waves[issue.Key],
			Size: estimate.Size, Tokens: estimate.Tokens, Minutes: estimate.Minutes,
		})
		result.Tokens = result.Tokens.add(estimate.Tokens)
		result.Minutes = result.Minutes.add(estimate.Minutes)
		for _, slice := range issue.Slices {
			sensitive = sensitive || len(slice.SensitiveZones) > 0
		}
		for len(result.Waves) < waves[issue.Key] {
			result.Waves = append(result.Waves, []string{})
		}
		result.Waves[waves[issue.Key]-1] = append(result.Waves[waves[issue.Key]-1], issue.Key)
	}
	result.Size = table.size(result.Minutes, sensitive)
	return result
}

// Waves returns the wave of each issue by key: 1 with no dependency, else one
// more than the latest wave of its dependencies. The dependencies must be
// known and acyclic.
func Waves(issues []EpicIssue) map[string]int {
	byKey := map[string]EpicIssue{}
	for _, issue := range issues {
		byKey[issue.Key] = issue
	}
	waves := map[string]int{}
	var wave func(string) int
	wave = func(key string) int {
		if w, ok := waves[key]; ok {
			return w
		}
		w := 1
		for _, dependency := range byKey[key].DependsOn {
			if d := wave(dependency) + 1; d > w {
				w = d
			}
		}
		waves[key] = w
		return w
	}
	for _, issue := range issues {
		wave(issue.Key)
	}
	return waves
}
