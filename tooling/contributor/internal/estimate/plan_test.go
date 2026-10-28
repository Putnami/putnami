package estimate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

const (
	estimateFeature    = "tooling/change-estimate"
	planValidation     = "plan-validation"
	estimateDeterminim = "estimate-determinism"
	estimatePolicy     = "estimate-policy"
)

func readExample(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// mutate decodes the example plan, applies change to it, and encodes it.
func mutate(t *testing.T, change func(plan map[string]any)) []byte {
	t.Helper()
	var plan map[string]any
	if err := json.Unmarshal(readExample(t), &plan); err != nil {
		t.Fatal(err)
	}
	change(plan)
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func slice(plan map[string]any, i int) map[string]any {
	return plan["slices"].([]any)[i].(map[string]any)
}

// TestAPlanInTheFormatIsReadWhole reads the example plan into every member
// of the Go types.
func TestAPlanInTheFormatIsReadWhole(t *testing.T) {
	t.Parallel()
	plan, err := ParsePlan(readExample(t))
	if err != nil {
		t.Fatal(err)
	}
	want := Plan{ProtocolVersion: 1, Issue: "42", Slices: []Slice{
		{Key: "plan-format", Title: "The plan format and its strict decoder", Projects: []string{"@putnami/contributor"},
			Files: 6, SensitiveZones: []string{}},
		{Key: "estimate-command", Title: "putnami change estimate sizes a plan",
			Projects: []string{"@putnami/contributor", "go.putnami.dev/protocol/telemetry"},
			Files:    9, Endpoints: 1, SensitiveZones: []string{"public API"}, Effort: &Effort{Model: "opus", Reasoning: "high"}},
	}}
	if !reflect.DeepEqual(plan, want) {
		t.Fatalf("plan %+v, want %+v", plan, want)
	}
	spectest.Proves(t, estimateFeature, planValidation, "a-plan-in-the-format-is-read-whole")
}

// TestAnInvalidPlanNamesTheValueItRefuses pins each refusal and the path its
// message names.
func TestAnInvalidPlanNamesTheValueItRefuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{"invalid UTF-8", []byte("{\"issue\": \"\xff\"}"), "plan: not valid UTF-8"},
		{"not JSON", []byte("{"), "plan: invalid JSON"},
		{"two values", []byte("{} {}"), "plan: more than one JSON value"},
		{"not an object", []byte("[]"), "plan: must be an object"},
		{"repeated member", []byte(`{"protocolVersion":1,"protocolVersion":1}`), `plan: member "protocolVersion" appears twice`},
		{"unknown member", mutate(t, func(p map[string]any) { p["owner"] = "me" }), `plan: unknown member "owner"`},
		{"unknown slice member", mutate(t, func(p map[string]any) { slice(p, 1)["hours"] = 3 }), `plan.slices[1]: unknown member "hours"`},
		{"unknown effort member", mutate(t, func(p map[string]any) { slice(p, 1)["effort"].(map[string]any)["budget"] = 1 }), `plan.slices[1].effort: unknown member "budget"`},
		{"other version", mutate(t, func(p map[string]any) { p["protocolVersion"] = 2 }), "plan.protocolVersion: must be 1"},
		{"missing issue", mutate(t, func(p map[string]any) { delete(p, "issue") }), "plan.issue: required"},
		{"issue as a number", mutate(t, func(p map[string]any) { p["issue"] = 42 }), "plan.issue: must be a string"},
		{"issue as a path", mutate(t, func(p map[string]any) { p["issue"] = "../42" }), `plan.issue: "../42" does not match`},
		{"no slice", mutate(t, func(p map[string]any) { p["slices"] = []any{} }), "plan.slices: must hold from 1 to 50 items"},
		{"missing files", mutate(t, func(p map[string]any) { delete(slice(p, 0), "files") }), "plan.slices[0].files: required"},
		{"negative files", mutate(t, func(p map[string]any) { slice(p, 0)["files"] = -1 }), "plan.slices[0].files: must be an integer from 0 to 10000"},
		{"fractional files", []byte(strings.Replace(string(readExample(t)), `"files": 6`, `"files": 6.0`, 1)), "plan.slices[0].files: must be an integer"},
		{"files as text", mutate(t, func(p map[string]any) { slice(p, 0)["files"] = "6" }), "plan.slices[0].files: must be an integer"},
		{"too many endpoints", mutate(t, func(p map[string]any) { slice(p, 0)["endpoints"] = 1001 }), "plan.slices[0].endpoints: must be an integer from 0 to 1000"},
		{"key with a capital", mutate(t, func(p map[string]any) { slice(p, 0)["key"] = "Plan" }), `plan.slices[0].key: "Plan" does not match`},
		{"repeated key", mutate(t, func(p map[string]any) { slice(p, 1)["key"] = "plan-format" }), `plan.slices[1].key: "plan-format" repeats the key of plan.slices[0]`},
		{"title on two lines", mutate(t, func(p map[string]any) { slice(p, 0)["title"] = "one\ntwo" }), "plan.slices[0].title: "},
		{"long title", mutate(t, func(p map[string]any) { slice(p, 0)["title"] = strings.Repeat("é", 201) }), "plan.slices[0].title: must be from 1 to 200 characters"},
		{"no project", mutate(t, func(p map[string]any) { slice(p, 0)["projects"] = []any{} }), "plan.slices[0].projects: must hold from 1 to 50 items"},
		{"project with a space", mutate(t, func(p map[string]any) { slice(p, 0)["projects"] = []any{"my project"} }), `plan.slices[0].projects[0]: "my project" does not match`},
		{"repeated project", mutate(t, func(p map[string]any) { slice(p, 0)["projects"] = []any{"a", "a"} }), "plan.slices[0].projects[1]: repeats plan.slices[0].projects[0]"},
		{"zones as text", mutate(t, func(p map[string]any) { slice(p, 0)["sensitiveZones"] = "public API" }), "plan.slices[0].sensitiveZones: must be an array"},
		{"empty zone", mutate(t, func(p map[string]any) { slice(p, 0)["sensitiveZones"] = []any{""} }), "plan.slices[0].sensitiveZones[0]: must be from 1 to 100 characters"},
		{"effort without reasoning", mutate(t, func(p map[string]any) { delete(slice(p, 1)["effort"].(map[string]any), "reasoning") }), "plan.slices[1].effort.reasoning: required"},
		{"effort with a space", mutate(t, func(p map[string]any) { slice(p, 1)["effort"].(map[string]any)["model"] = "big model" }), `plan.slices[1].effort.model: "big model" does not match`},
		{"null member", mutate(t, func(p map[string]any) { slice(p, 0)["screens"] = nil }), "plan.slices[0].screens: must be an integer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParsePlan(tc.data)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want one that contains %q", err, tc.want)
			}
		})
	}
	spectest.Proves(t, estimateFeature, planValidation, "an-invalid-plan-names-the-value-it-refuses")
}

// schemaNode is the part of a JSON Schema the drift test compares.
type schemaNode struct {
	Type                 string                 `json:"type"`
	Ref                  string                 `json:"$ref"`
	Const                *int                   `json:"const"`
	Minimum              *int                   `json:"minimum"`
	Maximum              *int                   `json:"maximum"`
	MinLength            *int                   `json:"minLength"`
	MaxLength            *int                   `json:"maxLength"`
	MinItems             *int                   `json:"minItems"`
	MaxItems             *int                   `json:"maxItems"`
	UniqueItems          bool                   `json:"uniqueItems"`
	Pattern              string                 `json:"pattern"`
	AdditionalProperties *bool                  `json:"additionalProperties"`
	Required             []string               `json:"required"`
	Properties           map[string]*schemaNode `json:"properties"`
	Items                *schemaNode            `json:"items"`
	Defs                 map[string]*schemaNode `json:"$defs"`
}

func value(n *int) int {
	if n == nil {
		return -1
	}
	return *n
}

// compareObject reports every difference between an object schema and the
// rules of the same object.
func compareObject(t *testing.T, path string, node *schemaNode, rules []rule, defs map[string]*schemaNode) {
	t.Helper()
	node = deref(t, node, defs)
	if node.Type != "object" || node.AdditionalProperties == nil || *node.AdditionalProperties {
		t.Errorf("%s: the schema must be an object that refuses additional properties", path)
	}
	names := make([]string, 0, len(rules))
	var required []string
	for _, r := range rules {
		names = append(names, r.name)
		if r.required {
			required = append(required, r.name)
		}
	}
	properties := make([]string, 0, len(node.Properties))
	for name := range node.Properties {
		properties = append(properties, name)
	}
	sort.Strings(names)
	sort.Strings(properties)
	sort.Strings(required)
	schemaRequired := slices.Clone(node.Required)
	sort.Strings(schemaRequired)
	if !slices.Equal(names, properties) {
		t.Errorf("%s: rules name %v, the schema %v", path, names, properties)
	}
	if !slices.Equal(required, schemaRequired) {
		t.Errorf("%s: rules require %v, the schema %v", path, required, schemaRequired)
	}
	for _, r := range rules {
		if p := node.Properties[r.name]; p != nil {
			compareRule(t, path+"."+r.name, p, r, defs)
		}
	}
}

func compareRule(t *testing.T, path string, node *schemaNode, r rule, defs map[string]*schemaNode) {
	t.Helper()
	node = deref(t, node, defs)
	switch r.kind {
	case kindInteger:
		low, high := value(node.Minimum), value(node.Maximum)
		if node.Const != nil {
			low, high = *node.Const, *node.Const
		}
		if node.Type != "integer" || low != r.min || high != r.max {
			t.Errorf("%s: schema %s [%d, %d], rule integer [%d, %d]", path, node.Type, low, high, r.min, r.max)
		}
	case kindString:
		if node.Type != "string" || value(node.MinLength) != r.min || value(node.MaxLength) != r.max || node.Pattern != r.pattern {
			t.Errorf("%s: schema %s [%d, %d] %q, rule string [%d, %d] %q", path, node.Type, value(node.MinLength), value(node.MaxLength), node.Pattern, r.min, r.max, r.pattern)
		}
	case kindObject:
		compareObject(t, path, node, r.fields, defs)
	case kindStringList, kindObjectList:
		if node.Type != "array" || value(node.MinItems) != r.min || value(node.MaxItems) != r.max || node.UniqueItems != r.unique || node.Items == nil {
			t.Errorf("%s: schema %s [%d, %d] unique %v, rule array [%d, %d] unique %v", path, node.Type, value(node.MinItems), value(node.MaxItems), node.UniqueItems, r.min, r.max, r.unique)
			return
		}
		if r.kind == kindObjectList {
			compareObject(t, path+"[]", node.Items, r.fields, defs)
			return
		}
		compareRule(t, path+"[]", node.Items, rule{kind: kindString, min: r.itemMin, max: r.itemMax, pattern: r.pattern}, defs)
	}
}

func deref(t *testing.T, node *schemaNode, defs map[string]*schemaNode) *schemaNode {
	t.Helper()
	if node.Ref == "" {
		return node
	}
	target := defs[strings.TrimPrefix(node.Ref, "#/$defs/")]
	if target == nil {
		t.Fatalf("dangling reference %s", node.Ref)
	}
	return target
}

// jsonMembers returns the JSON member names of a struct type.
func jsonMembers(typ reflect.Type) []string {
	var names []string
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func ruleNames(rules []rule) []string {
	names := make([]string, 0, len(rules))
	for _, r := range rules {
		names = append(names, r.name)
	}
	sort.Strings(names)
	return names
}

// TestTheSchemaStatesWhatTheDecoderApplies holds the published JSON Schema,
// the decoder's rules and the Go types to the same members and bounds.
func TestTheSchemaStatesWhatTheDecoderApplies(t *testing.T) {
	t.Parallel()
	data, err := schemaFiles.ReadFile("schemas/" + PlanSchemaFile)
	if err != nil {
		t.Fatal(err)
	}
	var root schemaNode
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	compareObject(t, "plan", &root, planRules, root.Defs)
	for _, tc := range []struct {
		typ   reflect.Type
		rules []rule
	}{
		{reflect.TypeOf(Plan{}), planRules},
		{reflect.TypeOf(Slice{}), sliceRules},
		{reflect.TypeOf(Effort{}), sliceRules[len(sliceRules)-1].fields},
	} {
		if got, want := jsonMembers(tc.typ), ruleNames(tc.rules); !slices.Equal(got, want) {
			t.Errorf("%s members %v, rules %v", tc.typ.Name(), got, want)
		}
	}
	spectest.Proves(t, estimateFeature, planValidation, "the-schema-states-what-the-decoder-applies")
}
