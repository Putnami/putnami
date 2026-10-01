package qualify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func readVerdictFixture(t *testing.T, name string) *Verdict {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("fixtures", "valid", name))
	if err != nil {
		t.Fatal(err)
	}
	verdict, diags := ParseAndValidateVerdict(data)
	if diag.HasErrors(diags) {
		t.Fatalf("fixture %s: %v", name, diags)
	}
	return verdict
}

func cloneVerdict(t *testing.T, verdict *Verdict) *Verdict {
	t.Helper()
	data, err := json.Marshal(verdict)
	if err != nil {
		t.Fatal(err)
	}
	var clone Verdict
	if err := json.Unmarshal(data, &clone); err != nil {
		t.Fatal(err)
	}
	return &clone
}

func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

type schemaNode struct {
	Properties map[string]*schemaNode `json:"properties"`
	Items      *schemaNode            `json:"items"`
	Defs       map[string]*schemaNode `json:"$defs"`
	Ref        string                 `json:"$ref"`
	Enum       []string               `json:"enum"`
}

func readSchema(t *testing.T, name string) *schemaNode {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("schemas", name))
	if err != nil {
		t.Fatal(err)
	}
	var node schemaNode
	if err := json.Unmarshal(data, &node); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return &node
}

// resolve follows one local $ref.
func (root *schemaNode) resolve(node *schemaNode) *schemaNode {
	if node == nil || !strings.HasPrefix(node.Ref, "#/$defs/") {
		return node
	}
	return root.Defs[strings.TrimPrefix(node.Ref, "#/$defs/")]
}

func propertyNames(node *schemaNode) []string {
	names := make([]string, 0, len(node.Properties))
	for name := range node.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func jsonFieldNames(value any) []string {
	typ := reflect.TypeOf(value)
	var names []string
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		names = append(names, strings.Split(tag, ",")[0])
	}
	sort.Strings(names)
	return names
}

// TestDrift_SchemasMatchGoTypes keeps the published schemas and the Go types
// on one member set, so neither can gain a wire field the other lacks.
func TestDrift_SchemasMatchGoTypes(t *testing.T) {
	contract := readSchema(t, "contract.json")
	verdict := readSchema(t, "verdict.json")
	phases := verdict.Properties["phases"].Items
	cases := []struct {
		name   string
		schema *schemaNode
		goType any
	}{
		{"Contract", contract, Contract{}},
		{"Request", contract.Defs["request"], Request{}},
		{"Source", contract.Defs["source"], Source{}},
		{"Verdict", verdict, Verdict{}},
		{"Target", verdict.Properties["target"], Target{}},
		{"Binding", verdict.Properties["binding"], Binding{}},
		{"ContractRef", verdict.Properties["contract"], ContractRef{}},
		{"ContractRef.derivedFrom", verdict.Properties["contract"].Properties["derivedFrom"].Items, Source{}},
		{"Phase", phases, Phase{}},
		{"Phase.diagnostics", phases.Properties["diagnostics"].Items, diag.Diagnostic{}},
		{"RequestResult", verdict.Properties["requests"].Items, RequestResult{}},
		{"Cleanup", verdict.Properties["cleanup"], Cleanup{}},
	}
	for _, tc := range cases {
		if got, want := propertyNames(tc.schema), jsonFieldNames(tc.goType); !slices.Equal(got, want) {
			t.Errorf("%s: schema members %v, Go members %v", tc.name, got, want)
		}
	}

	states := make([]string, 0, len(ValidStates))
	for _, state := range ValidStates {
		states = append(states, string(state))
	}
	if got := verdict.Defs["state"].Enum; !slices.Equal(got, states) {
		t.Errorf("verdict state enum %v, want ValidStates %v", got, states)
	}
	if got := phases.Properties["name"].Enum; !slices.Equal(got, PhaseNames) {
		t.Errorf("phase name enum %v, want PhaseNames %v", got, PhaseNames)
	}
	if verdict.resolve(verdict.Properties["state"]) != verdict.Defs["state"] {
		t.Error("verdict.state must reference $defs/state")
	}
}

// TestDrift_ShapesMatchGoTypes keeps the case-sensitive member walk in step
// with the Go types, so strictDecode never refuses a member the type declares
// or admits one it does not.
func TestDrift_ShapesMatchGoTypes(t *testing.T) {
	cases := []struct {
		name   string
		shape  *member
		goType any
	}{
		{"Contract", contractShape, Contract{}},
		{"Request", requestShape, Request{}},
		{"Source", sourceShape, Source{}},
		{"Verdict", verdictShape, Verdict{}},
		{"Target", verdictShape.fields["target"], Target{}},
		{"Binding", verdictShape.fields["binding"], Binding{}},
		{"ContractRef", verdictShape.fields["contract"], ContractRef{}},
		{"Phase", verdictShape.fields["phases"].items, Phase{}},
		{"Diagnostic", verdictShape.fields["phases"].items.fields["diagnostics"].items, diag.Diagnostic{}},
		{"RequestResult", verdictShape.fields["requests"].items, RequestResult{}},
		{"Cleanup", verdictShape.fields["cleanup"], Cleanup{}},
	}
	for _, tc := range cases {
		var names []string
		for name := range tc.shape.fields {
			names = append(names, name)
		}
		sort.Strings(names)
		if want := jsonFieldNames(tc.goType); !slices.Equal(names, want) {
			t.Errorf("%s: shape members %v, Go members %v", tc.name, names, want)
		}
	}
}
