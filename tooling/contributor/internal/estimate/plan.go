// Package estimate reads a change plan and sizes it: putnami change estimate.
//
// A plan names the issue a change delivers and its slices in order. Each
// slice states the projects it changes, the files it touches, the endpoints
// and screens it adds or changes, and the sensitive zones it touches. The
// estimate maps each slice to a range of tokens and minutes through a table
// of coefficients, sums the slices, and sizes the result S, M or L. The same
// plan and table always give the same estimate.
package estimate

import (
	"embed"
	"encoding/json"
	"fmt"
)

// PlanProtocolVersion is the version of the plan format.
const PlanProtocolVersion = 1

// PlanSchemaFile is the JSON Schema of the plan format, in schemas/.
const PlanSchemaFile = "change-plan.v1.json"

//go:embed schemas/*.json
var schemaFiles embed.FS

// Plan is a change plan.
type Plan struct {
	ProtocolVersion int     `json:"protocolVersion"`
	Issue           string  `json:"issue"`
	Slices          []Slice `json:"slices"`
}

// Slice is one step of a plan the owner can validate on its own.
type Slice struct {
	Key            string   `json:"key"`
	Title          string   `json:"title"`
	Projects       []string `json:"projects"`
	Files          int      `json:"files"`
	Endpoints      int      `json:"endpoints"`
	Screens        int      `json:"screens"`
	SensitiveZones []string `json:"sensitiveZones"`
	Effort         *Effort  `json:"effort,omitempty"`
}

// Effort is the model and the reasoning level a slice is planned for. Both
// are free tokens: the estimate does not read them.
type Effort struct {
	Model     string `json:"model"`
	Reasoning string `json:"reasoning"`
}

const (
	effortPattern  = "^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$"
	issuePattern   = "^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$"
	keyPattern     = "^[a-z0-9][a-z0-9-]{0,63}$"
	linePattern    = "^[^\\u0000-\\u001F\\u007F]+$"
	projectPattern = "^[!-~]+$"
)

// planRules is the plan format. schemas/change-plan.v1.json states the same
// members and bounds; a test fails when the two differ.
var planRules = []rule{
	{name: "protocolVersion", kind: kindInteger, required: true, min: PlanProtocolVersion, max: PlanProtocolVersion},
	{name: "issue", kind: kindString, required: true, min: 1, max: 64, pattern: issuePattern},
	{name: "slices", kind: kindObjectList, required: true, min: 1, max: 50, fields: sliceRules},
}

var sliceRules = []rule{
	{name: "key", kind: kindString, required: true, min: 1, max: 64, pattern: keyPattern},
	{name: "title", kind: kindString, required: true, min: 1, max: 200, pattern: linePattern},
	{name: "projects", kind: kindStringList, required: true, min: 1, max: 50, itemMin: 1, itemMax: 214, pattern: projectPattern, unique: true},
	{name: "files", kind: kindInteger, required: true, min: 0, max: 10000},
	{name: "endpoints", kind: kindInteger, required: true, min: 0, max: 1000},
	{name: "screens", kind: kindInteger, required: true, min: 0, max: 1000},
	{name: "sensitiveZones", kind: kindStringList, required: true, min: 0, max: 20, itemMin: 1, itemMax: 100, pattern: linePattern, unique: true},
	{name: "effort", kind: kindObject, fields: []rule{
		{name: "model", kind: kindString, required: true, min: 1, max: 64, pattern: effortPattern},
		{name: "reasoning", kind: kindString, required: true, min: 1, max: 64, pattern: effortPattern},
	}},
}

func init() {
	compileRules(planRules)
	compileRules(tableRules)
}

// ParsePlan reads a plan strictly: valid UTF-8, one JSON object, no repeated
// member, no member the format does not name, and every value within its
// bounds. Slice keys are unique. An error names the path of the first value
// it refuses.
func ParsePlan(data []byte) (Plan, error) {
	value, err := decodeStrict(data, "plan")
	if err != nil {
		return Plan{}, err
	}
	if err := checkObject("plan", value, planRules); err != nil {
		return Plan{}, err
	}
	var plan Plan
	if err := json.Unmarshal(data, &plan); err != nil {
		return Plan{}, fmt.Errorf("plan: %w", err)
	}
	keys := map[string]int{}
	for i, slice := range plan.Slices {
		if first, repeated := keys[slice.Key]; repeated {
			return Plan{}, fmt.Errorf("plan.slices[%d].key: %q repeats the key of plan.slices[%d]", i, slice.Key, first)
		}
		keys[slice.Key] = i
	}
	return plan, nil
}
