package estimate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

const (
	epicFeature        = "tooling/epic"
	epicPlanValidation = "epic-plan-validation"
	epicWaves          = "epic-waves"
)

func readEpicExample(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "epic.json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// mutateEpic decodes the example epic plan, applies change to it, and
// encodes it.
func mutateEpic(t *testing.T, change func(plan map[string]any)) []byte {
	t.Helper()
	var plan map[string]any
	if err := json.Unmarshal(readEpicExample(t), &plan); err != nil {
		t.Fatal(err)
	}
	change(plan)
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func epicIssue(plan map[string]any, i int) map[string]any {
	return plan["issues"].([]any)[i].(map[string]any)
}

// TestAnEpicPlanInTheFormatIsReadWhole reads the example epic plan into
// every member of the Go types.
func TestAnEpicPlanInTheFormatIsReadWhole(t *testing.T) {
	t.Parallel()
	plan, err := ParseEpicPlan(readEpicExample(t))
	if err != nil {
		t.Fatal(err)
	}
	if plan.ProtocolVersion != 1 || plan.Epic != "40" || plan.Title != "Plans are sized and launched in waves" || plan.Base != "main" {
		t.Fatalf("epic %+v", plan)
	}
	if plan.Envelope != (Envelope{Tokens: 330_000_000, ActiveMinutes: 640, Sessions: 2}) {
		t.Fatalf("envelope %+v", plan.Envelope)
	}
	want := EpicIssue{
		Key: "read-contract", Title: "The missions contract gains a read operation",
		Task: &TaskRef{Source: "github:acme/app", ID: "41"}, DependsOn: []string{},
		Slices: []Slice{{Key: "status-operation", Title: "missions.status reads a mission's sessions",
			Projects: []string{"go.putnami.dev/protocol/collaboration"}, Files: 9, Endpoints: 1,
			SensitiveZones: []string{"public API"}}},
	}
	if len(plan.Issues) != 4 || !reflect.DeepEqual(plan.Issues[0], want) {
		t.Fatalf("issues %+v, want first %+v", plan.Issues, want)
	}
	if plan.Issues[2].Task != nil || !slices.Equal(plan.Issues[2].DependsOn, []string{"read-contract", "plan-command"}) || len(plan.Issues[2].Slices) != 2 {
		t.Fatalf("third issue %+v", plan.Issues[2])
	}
	spectest.Proves(t, epicFeature, epicPlanValidation, "an-epic-plan-in-the-format-is-read-whole")
}

// TestAnInvalidEpicPlanNamesTheValueItRefuses pins each refusal and the path
// its message names.
func TestAnInvalidEpicPlanNamesTheValueItRefuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{"repeated member", []byte(`{"epic": "1", "epic": "2"}`), `epic: member "epic" appears twice`},
		{"unknown member", mutateEpic(t, func(p map[string]any) { p["owner"] = "me" }), `epic: unknown member "owner"`},
		{"missing envelope", mutateEpic(t, func(p map[string]any) { delete(p, "envelope") }), "epic.envelope: required"},
		{"sessions out of bounds", mutateEpic(t, func(p map[string]any) {
			p["envelope"].(map[string]any)["sessions"] = 21
		}), "epic.envelope.sessions: must be an integer from 1 to 20"},
		{"bad epic id", mutateEpic(t, func(p map[string]any) { p["epic"] = "../x" }), "epic.epic:"},
		{"task without id", mutateEpic(t, func(p map[string]any) {
			epicIssue(p, 0)["task"] = map[string]any{"source": "github:acme/app"}
		}), "epic.issues[0].task.id: required"},
		{"slice out of bounds", mutateEpic(t, func(p map[string]any) {
			epicIssue(p, 1)["slices"].([]any)[0].(map[string]any)["files"] = -1
		}), "epic.issues[1].slices[0].files: must be an integer from 0 to 10000"},
		{"repeated issue key", mutateEpic(t, func(p map[string]any) { epicIssue(p, 1)["key"] = "read-contract" }),
			`epic.issues[1].key: "read-contract" repeats the key of epic.issues[0]`},
		{"repeated slice key", mutateEpic(t, func(p map[string]any) {
			epicIssue(p, 2)["slices"].([]any)[1].(map[string]any)["key"] = "status"
		}), `epic.issues[2].slices[1].key: "status" repeats the key of epic.issues[2].slices[0]`},
		{"repeated dependency", mutateEpic(t, func(p map[string]any) {
			epicIssue(p, 3)["dependsOn"] = []any{"plan-command", "plan-command"}
		}), "epic.issues[3].dependsOn[1]: repeats epic.issues[3].dependsOn[0]"},
		{"unknown dependency", mutateEpic(t, func(p map[string]any) { epicIssue(p, 3)["dependsOn"] = []any{"nothing"} }),
			`epic.issues[3].dependsOn[0]: "nothing" names no issue of the epic`},
		{"self dependency", mutateEpic(t, func(p map[string]any) { epicIssue(p, 3)["dependsOn"] = []any{"skill"} }),
			`epic.issues[3].dependsOn[0]: "skill" names the issue itself`},
		{"cycle", mutateEpic(t, func(p map[string]any) { epicIssue(p, 0)["dependsOn"] = []any{"skill"} }),
			"epic.issues: the dependencies form a cycle: read-contract -> skill -> status-command -> read-contract"},
	} {
		if _, err := ParseEpicPlan(tc.data); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want %q", tc.name, err, tc.want)
		}
	}
	spectest.Proves(t, epicFeature, epicPlanValidation, "an-invalid-epic-plan-names-the-value-it-refuses")
}

// TestTheEpicSchemaStatesWhatTheDecoderApplies holds the published JSON
// Schema of the epic plan, the decoder's rules and the Go types to the same
// members and bounds.
func TestTheEpicSchemaStatesWhatTheDecoderApplies(t *testing.T) {
	t.Parallel()
	data, err := schemaFiles.ReadFile("schemas/" + EpicSchemaFile)
	if err != nil {
		t.Fatal(err)
	}
	var root schemaNode
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	compareObject(t, "epic", &root, epicRules, root.Defs)
	for _, tc := range []struct {
		typ   reflect.Type
		rules []rule
	}{
		{reflect.TypeOf(EpicPlan{}), epicRules},
		{reflect.TypeOf(Envelope{}), epicRules[4].fields},
		{reflect.TypeOf(EpicIssue{}), epicIssueRules},
		{reflect.TypeOf(TaskRef{}), epicIssueRules[2].fields},
	} {
		if got, want := jsonMembers(tc.typ), ruleNames(tc.rules); !slices.Equal(got, want) {
			t.Errorf("%s members %v, rules %v", tc.typ.Name(), got, want)
		}
	}
	spectest.Proves(t, epicFeature, epicPlanValidation, "the-epic-schema-states-what-the-decoder-applies")
}

// TestAnIssueRunsOneWaveAfterItsLatestDependency sizes each issue of the
// example from its slices and orders the issues in waves.
func TestAnIssueRunsOneWaveAfterItsLatestDependency(t *testing.T) {
	t.Parallel()
	plan, err := ParseEpicPlan(readEpicExample(t))
	if err != nil {
		t.Fatal(err)
	}
	result := EstimateEpic(plan, DefaultTable(), "epic.json")
	if want := [][]string{{"read-contract", "plan-command"}, {"status-command"}, {"skill"}}; !reflect.DeepEqual(result.Waves, want) {
		t.Fatalf("waves %v, want %v", result.Waves, want)
	}
	for _, tc := range []struct {
		key     string
		wave    int
		size    Size
		tokens  Range
		minutes Range
	}{
		{"read-contract", 1, SizeM, Range{17_600_000, 48_500_000}, Range{44, 144}},
		{"plan-command", 1, SizeS, Range{5_600_000, 16_700_000}, Range{31, 86}},
		{"status-command", 2, SizeM, Range{23_200_000, 65_200_000}, Range{75, 230}},
		{"skill", 3, SizeS, Range{5_600_000, 16_700_000}, Range{31, 86}},
	} {
		i := slices.IndexFunc(result.Issues, func(e IssueEstimate) bool { return e.Key == tc.key })
		got := result.Issues[i]
		if got.Wave != tc.wave || got.Size != tc.size || got.Tokens != tc.tokens || got.Minutes != tc.minutes {
			t.Errorf("%s: %+v, want wave %d, %s, %v tokens, %v minutes", tc.key, got, tc.wave, tc.size, tc.tokens, tc.minutes)
		}
	}
	if result.Size != SizeL || result.Tokens != (Range{52_000_000, 147_100_000}) || result.Minutes != (Range{181, 546}) {
		t.Fatalf("epic %s %v %v, want L, 52.0M to 147.1M tokens, 181 to 546 minutes", result.Size, result.Tokens, result.Minutes)
	}
	if again := EstimateEpic(plan, DefaultTable(), "epic.json"); !reflect.DeepEqual(again, result) {
		t.Fatal("the same epic plan gave two results")
	}
	spectest.Proves(t, epicFeature, epicWaves, "an-issue-runs-one-wave-after-its-latest-dependency")
}
