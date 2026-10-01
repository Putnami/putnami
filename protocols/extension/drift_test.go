package extension

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// schemaProperties extracts property names from a schema $defs entry.
func schemaDefProperties(t *testing.T, defName string) []string {
	t.Helper()
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("read schema %s: %v", schemaPath, err)
	}
	var schema struct {
		Defs map[string]struct {
			Properties map[string]any `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse schema %s: %v", schemaPath, err)
	}
	def, ok := schema.Defs[defName]
	if !ok {
		t.Fatalf("no $defs/%s in %s", defName, schemaPath)
	}
	names := make([]string, 0, len(def.Properties))
	for name := range def.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// goTypeJSONFields extracts json tag field names from a struct type.
func goTypeJSONFields(t *testing.T, v any) []string {
	t.Helper()
	typ := reflect.TypeOf(v)
	if typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	var names []string
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		tag := field.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func assertFieldParity(t *testing.T, typeName string, schemaFields, goFields []string) {
	t.Helper()
	schemaSet := make(map[string]bool)
	for _, f := range schemaFields {
		schemaSet[f] = true
	}
	goSet := make(map[string]bool)
	for _, f := range goFields {
		goSet[f] = true
	}
	for _, f := range schemaFields {
		if !goSet[f] {
			t.Errorf("%s: field %q exists in schema but not in Go type", typeName, f)
		}
	}
	for _, f := range goFields {
		if !schemaSet[f] {
			t.Errorf("%s: field %q exists in Go type but not in schema", typeName, f)
		}
	}
}

// schemaRootProperties extracts the property names of the schema's root
// object — the manifest's own top-level members.
func schemaRootProperties(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("read schema %s: %v", schemaPath, err)
	}
	var schema struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse schema %s: %v", schemaPath, err)
	}
	names := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

const schemaPath = "schemas/extension.json"

// TestDrift_Manifest pins the manifest ROOT against the schema. Without it, a
// top-level section could be added to one side alone — which is exactly how the
// runtime and workspace sections could have drifted in.
func TestDrift_Manifest(t *testing.T) {
	assertFieldParity(t, "Manifest", schemaRootProperties(t), goTypeJSONFields(t, Manifest{}))
}

func TestDrift_RuntimeDefinition(t *testing.T) {
	schemaFields := schemaDefProperties(t, "runtimeDefinition")
	goFields := goTypeJSONFields(t, RuntimeDefinition{})
	assertFieldParity(t, "RuntimeDefinition", schemaFields, goFields)
}

func TestDrift_RuntimePrepare(t *testing.T) {
	schemaFields := schemaDefProperties(t, "runtimePrepare")
	goFields := goTypeJSONFields(t, RuntimePrepare{})
	assertFieldParity(t, "RuntimePrepare", schemaFields, goFields)
}

func TestDrift_RuntimeToolchain(t *testing.T) {
	assertFieldParity(t, "RuntimeToolchain", schemaDefProperties(t, "runtimeToolchain"), goTypeJSONFields(t, RuntimeToolchain{}))
}

func TestDrift_RuntimeToolchainCandidate(t *testing.T) {
	assertFieldParity(t, "RuntimeToolchainCandidate", schemaDefProperties(t, "runtimeToolchainCandidate"), goTypeJSONFields(t, RuntimeToolchainCandidate{}))
}

func TestDrift_RuntimeToolchainProbe(t *testing.T) {
	assertFieldParity(t, "RuntimeToolchainProbe", schemaDefProperties(t, "runtimeToolchainProbe"), goTypeJSONFields(t, RuntimeToolchainProbe{}))
}

func TestDrift_RuntimeToolchainEnvironment(t *testing.T) {
	assertFieldParity(t, "RuntimeToolchainEnvironment", schemaDefProperties(t, "runtimeToolchainEnvironment"), goTypeJSONFields(t, RuntimeToolchainEnvironment{}))
}

func TestDrift_WorkspaceAdapter(t *testing.T) {
	schemaFields := schemaDefProperties(t, "workspaceAdapter")
	goFields := goTypeJSONFields(t, WorkspaceAdapter{})
	assertFieldParity(t, "WorkspaceAdapter", schemaFields, goFields)
}

// The three ecosystem-profile types. A profile is the ONLY place an ecosystem
// is defined, so a field on one side alone would let an extension declare a
// rule the schema cannot express, or the schema promise one Go never reads.
func TestDrift_EcosystemProfile(t *testing.T) {
	schemaFields := schemaDefProperties(t, "ecosystemProfile")
	goFields := goTypeJSONFields(t, EcosystemProfile{})
	assertFieldParity(t, "EcosystemProfile", schemaFields, goFields)
}

func TestDrift_PatternRule(t *testing.T) {
	schemaFields := schemaDefProperties(t, "patternRule")
	goFields := goTypeJSONFields(t, PatternRule{})
	assertFieldParity(t, "PatternRule", schemaFields, goFields)
}

func TestDrift_VersionRule(t *testing.T) {
	schemaFields := schemaDefProperties(t, "versionRule")
	goFields := goTypeJSONFields(t, VersionRule{})
	assertFieldParity(t, "VersionRule", schemaFields, goFields)
}

// schemaDefEnum returns the enum members of a $defs entry's property.
func schemaDefEnum(t *testing.T, defName, property string) []string {
	t.Helper()
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("read schema %s: %v", schemaPath, err)
	}
	var schema struct {
		Defs map[string]struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse schema %s: %v", schemaPath, err)
	}
	def, ok := schema.Defs[defName]
	if !ok {
		t.Fatalf("no $defs/%s in %s", defName, schemaPath)
	}
	prop, ok := def.Properties[property]
	if !ok {
		t.Fatalf("no $defs/%s/properties/%s in %s", defName, property, schemaPath)
	}
	members := append([]string(nil), prop.Enum...)
	sort.Strings(members)
	return members
}

// TestDrift_DeclaredOutputVocabularies pins the declared-output kind and scope
// enums against the Go vocabularies. A kind or scope that exists on one side
// alone is a manifest a consumer accepts and a producer cannot write, or the
// reverse.
func TestDrift_DeclaredOutputVocabularies(t *testing.T) {
	if got := schemaDefEnum(t, "declaredOutput", "kind"); !reflect.DeepEqual(got, ValidOutputKinds) {
		t.Errorf("declared output kind drift: schema %v, Go %v", got, ValidOutputKinds)
	}
	if got := schemaDefEnum(t, "declaredOutput", "scope"); !reflect.DeepEqual(got, ValidOutputScopes) {
		t.Errorf("declared output scope drift: schema %v, Go %v", got, ValidOutputScopes)
	}
}

// TestDrift_LifecyclePrimitiveSurfaces mechanically binds the closed primitive
// registry to the protocol vocabulary it classifies. This prevents the
// ratchet from staying green while a real lifecycle field is renamed, removed,
// or no longer observed by ManifestLifecyclePrimitives.
func TestDrift_LifecyclePrimitiveSurfaces(t *testing.T) {
	expectedRegistry := []LifecyclePrimitiveID{
		LifecyclePrimitiveInvocation,
		LifecyclePrimitiveRuntime,
		LifecyclePrimitiveWorkspace,
	}
	if !reflect.DeepEqual(ValidLifecyclePrimitives, expectedRegistry) {
		t.Fatalf("lifecycle primitive registry drift: got %v, want %v",
			ValidLifecyclePrimitives, expectedRegistry)
	}
	if LifecyclePrimitiveInvocation != LifecyclePrimitiveID(OutputScopeInvocation) {
		t.Errorf("invocation primitive %q drifted from output scope %q",
			LifecyclePrimitiveInvocation, OutputScopeInvocation)
	}
	if LifecyclePrimitiveInvocation != LifecyclePrimitiveID(OutputRootInvocation) {
		t.Errorf("invocation primitive %q drifted from output root %q",
			LifecyclePrimitiveInvocation, OutputRootInvocation)
	}

	rootFields := make(map[string]bool)
	for _, field := range schemaRootProperties(t) {
		rootFields[field] = true
	}
	for _, primitive := range []LifecyclePrimitiveID{
		LifecyclePrimitiveRuntime,
		LifecyclePrimitiveWorkspace,
	} {
		if !rootFields[string(primitive)] {
			t.Errorf("lifecycle primitive %q does not name a manifest schema surface", primitive)
		}
	}

	pipelineFields := make(map[string]bool)
	for _, field := range schemaDefProperties(t, "pipelineStep") {
		pipelineFields[field] = true
	}
	for _, field := range []string{"runOn", "finalizes"} {
		if !pipelineFields[field] {
			t.Errorf("invocation primitive finalizer surface %q is absent from pipelineStep", field)
		}
	}
	if got := schemaDefEnum(t, "pipelineStep", "runOn"); !containsString(got, StepRunOnFinally) {
		t.Errorf("invocation primitive finalizer value %q is absent from runOn enum %v",
			StepRunOnFinally, got)
	}

	invocationOutput := TaskDefinition{Declares: &TaskDeclaration{
		Outputs: map[string]DeclaredOutput{
			"lease": {Kind: OutputKindRuntimeFile, Scope: OutputScopeInvocation, Path: "lease"},
		},
	}}
	cases := []struct {
		name string
		want LifecyclePrimitiveID
		m    *Manifest
	}{
		{
			name: "runtime section",
			want: LifecyclePrimitiveRuntime,
			m:    &Manifest{Runtime: &RuntimeDefinition{}},
		},
		{
			name: "workspace section",
			want: LifecyclePrimitiveWorkspace,
			m:    &Manifest{Workspace: &WorkspaceAdapter{}},
		},
		{
			name: "invocation output",
			want: LifecyclePrimitiveInvocation,
			m:    &Manifest{Tasks: map[string]TaskDefinition{"setup": invocationOutput}},
		},
		{
			name: "runOn finalizer",
			want: LifecyclePrimitiveInvocation,
			m: &Manifest{Commands: map[string]CommandDefinition{
				"test": {Run: []PipelineStep{{RunOn: StepRunOnFinally}}},
			}},
		},
		{
			name: "finalizes relation",
			want: LifecyclePrimitiveInvocation,
			m: &Manifest{Commands: map[string]CommandDefinition{
				"test": {Run: []PipelineStep{{Finalizes: &FinalizesRelation{}}}},
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ManifestLifecyclePrimitives(tc.m)
			want := []LifecyclePrimitiveID{tc.want}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("ManifestLifecyclePrimitives() = %v, want %v", got, want)
			}
			if version := ManifestProtocolVersion(tc.m); version != ProtocolVersionV3 {
				t.Errorf("classified lifecycle surface reports protocol v%d, want v%d",
					version, ProtocolVersionV3)
			}
		})
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// TestDrift_StepRunOn pins the step run-condition enum. It is also the deletion
// ratchet for `when`: the schema must carry exactly one step-gating runtime
// field, and a re-added `when` property would fail TestDrift_PipelineStep as an
// unmatched schema member.
func TestDrift_StepRunOn(t *testing.T) {
	if got := schemaDefEnum(t, "pipelineStep", "runOn"); !reflect.DeepEqual(got, ValidStepRunOn) {
		t.Errorf("step run condition drift: schema %v, Go %v", got, ValidStepRunOn)
	}
	for _, field := range schemaDefProperties(t, "pipelineStep") {
		if field == "when" {
			t.Error(`the schema declares "when"; contract v3 deleted it — runOn is the single step-gating field`)
		}
	}
}

func TestDrift_CommandDefinition(t *testing.T) {
	schemaFields := schemaDefProperties(t, "commandDefinition")
	goFields := goTypeJSONFields(t, CommandDefinition{})
	// "activation" is an intentionally undocumented internal field.
	// It is not part of the public schema contract.
	filtered := make([]string, 0, len(goFields))
	for _, f := range goFields {
		if f == "activation" {
			continue
		}
		filtered = append(filtered, f)
	}
	assertFieldParity(t, "CommandDefinition", schemaFields, filtered)
}

func TestDrift_SessionPrerequisiteDefinition(t *testing.T) {
	assertFieldParity(t, "SessionPrerequisiteDefinition",
		schemaDefProperties(t, "sessionPrerequisiteDefinition"),
		goTypeJSONFields(t, SessionPrerequisiteDefinition{}))
}

func TestDrift_SessionPrerequisiteParamBinding(t *testing.T) {
	assertFieldParity(t, "SessionPrerequisiteParamBinding",
		schemaDefProperties(t, "sessionPrerequisiteParamBinding"),
		goTypeJSONFields(t, SessionPrerequisiteParamBinding{}))
}

func TestDrift_ToolDefinition(t *testing.T) {
	schemaFields := schemaDefProperties(t, "toolDefinition")
	goFields := goTypeJSONFields(t, ToolDefinition{})
	assertFieldParity(t, "ToolDefinition", schemaFields, goFields)
}

func TestDrift_AgentContentContribution(t *testing.T) {
	schemaFields := schemaDefProperties(t, "agentContentContribution")
	goFields := goTypeJSONFields(t, AgentContentContribution{})
	assertFieldParity(t, "AgentContentContribution", schemaFields, goFields)
}

func TestDrift_ToolAnnotations(t *testing.T) {
	schemaFields := schemaDefProperties(t, "toolAnnotations")
	goFields := goTypeJSONFields(t, ToolAnnotations{})
	assertFieldParity(t, "ToolAnnotations", schemaFields, goFields)
}

func TestDrift_HookDefinition(t *testing.T) {
	schemaFields := schemaDefProperties(t, "commandHookDefinition")
	goFields := goTypeJSONFields(t, HookDefinition{})
	assertFieldParity(t, "HookDefinition", schemaFields, goFields)
}

func TestDrift_PipelineStep(t *testing.T) {
	schemaFields := schemaDefProperties(t, "pipelineStep")
	goFields := goTypeJSONFields(t, PipelineStep{})
	assertFieldParity(t, "PipelineStep", schemaFields, goFields)
}

func TestDrift_FinalizesRelation(t *testing.T) {
	schemaFields := schemaDefProperties(t, "finalizesRelation")
	goFields := goTypeJSONFields(t, FinalizesRelation{})
	assertFieldParity(t, "FinalizesRelation", schemaFields, goFields)
}

// The three defs the workspace-adapter closure extended had no drift test, so
// a field could be added to the schema or to Go alone. They have one now.

func TestDrift_StepActivation(t *testing.T) {
	schemaFields := schemaDefProperties(t, "stepActivation")
	goFields := goTypeJSONFields(t, StepActivation{})
	assertFieldParity(t, "StepActivation", schemaFields, goFields)
}

func TestDrift_TaskInputPort(t *testing.T) {
	schemaFields := schemaDefProperties(t, "taskInputPort")
	goFields := goTypeJSONFields(t, TaskInputPort{})
	assertFieldParity(t, "TaskInputPort", schemaFields, goFields)
}

// TestDrift_TaskInputSourceVocabulary pins the schema's closed `from` enum
// against the Go constants and the strict validator's own set, so an input
// source can never be added to one side alone.
func TestDrift_TaskInputSourceVocabulary(t *testing.T) {
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("read schema %s: %v", schemaPath, err)
	}
	var schema struct {
		Defs map[string]struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse schema %s: %v", schemaPath, err)
	}
	schemaSources := append([]string(nil), schema.Defs["taskInputPort"].Properties["from"].Enum...)
	sort.Strings(schemaSources)

	goSources := []string{
		TaskInputFromProject, TaskInputFromWorkspace, TaskInputFromClosure,
		TaskInputFromTask, TaskInputFromParams, TaskInputFromEnv, TaskInputFromRuntime,
	}
	sort.Strings(goSources)
	if !reflect.DeepEqual(schemaSources, goSources) {
		t.Errorf("task input source vocabulary drift: schema %v, Go %v", schemaSources, goSources)
	}

	// The strict validator enforces the same set: a source the schema admits but
	// the validator rejects is a manifest that passes authoring and fails load.
	for _, source := range goSources {
		m := &Manifest{
			Name:     "@acme/probe",
			Version:  "1.0.0",
			Commands: map[string]CommandDefinition{"test": {Run: []PipelineStep{{ID: "s", Task: "probe"}}}},
			Tasks: map[string]TaskDefinition{
				"probe": {Kind: "command", Command: "probe", Inputs: map[string]TaskInputPort{"in": {From: source}}},
			},
		}
		for _, d := range ValidateManifest(m) {
			if strings.Contains(d.Message, "invalid input source") {
				t.Errorf("strict validation rejects the schema-admitted source %q: %s", source, d.Message)
			}
		}
	}
}

func TestDrift_TaskCacheKey(t *testing.T) {
	schemaFields := schemaDefProperties(t, "taskCacheKey")
	goFields := goTypeJSONFields(t, TaskCacheKey{})
	assertFieldParity(t, "TaskCacheKey", schemaFields, goFields)
}

func TestDrift_TaskDefinition(t *testing.T) {
	schemaFields := schemaDefProperties(t, "taskDefinition")
	goFields := goTypeJSONFields(t, TaskDefinition{})
	assertFieldParity(t, "TaskDefinition", schemaFields, goFields)
}

func TestDrift_TaskDeclaration(t *testing.T) {
	schemaFields := schemaDefProperties(t, "taskDeclaration")
	goFields := goTypeJSONFields(t, TaskDeclaration{})
	assertFieldParity(t, "TaskDeclaration", schemaFields, goFields)
}

func TestDrift_DeclaredOutput(t *testing.T) {
	schemaFields := schemaDefProperties(t, "declaredOutput")
	goFields := goTypeJSONFields(t, DeclaredOutput{})
	assertFieldParity(t, "DeclaredOutput", schemaFields, goFields)
}

// TestDrift_TaskEffectVocabulary pins the closed effect enum in the schema
// against the Go vocabulary, so an effect can never be added to one side alone.
func TestDrift_TaskEffectVocabulary(t *testing.T) {
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("read schema %s: %v", schemaPath, err)
	}
	var schema struct {
		Defs map[string]struct {
			Enum []string `json:"enum"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse schema %s: %v", schemaPath, err)
	}
	def, ok := schema.Defs["taskEffect"]
	if !ok {
		t.Fatalf("no $defs/taskEffect in %s", schemaPath)
	}
	schemaEffects := append([]string(nil), def.Enum...)
	sort.Strings(schemaEffects)
	if !reflect.DeepEqual(schemaEffects, ValidTaskEffects) {
		t.Errorf("task effect vocabulary drift: schema %v, Go %v", schemaEffects, ValidTaskEffects)
	}
}

func TestDrift_TaskBatchPolicy(t *testing.T) {
	schemaFields := schemaDefProperties(t, "taskBatchPolicy")
	goFields := goTypeJSONFields(t, TaskBatchPolicy{})
	assertFieldParity(t, "TaskBatchPolicy", schemaFields, goFields)
}

func TestDrift_StepCacheOverride(t *testing.T) {
	schemaFields := schemaDefProperties(t, "stepCacheOverride")
	goFields := goTypeJSONFields(t, StepCacheOverride{})
	assertFieldParity(t, "StepCacheOverride", schemaFields, goFields)
}

func TestDrift_CommandGroupDefinition(t *testing.T) {
	schemaFields := schemaDefProperties(t, "commandGroupDefinition")
	goFields := goTypeJSONFields(t, CommandGroupDefinition{})
	assertFieldParity(t, "CommandGroupDefinition", schemaFields, goFields)
}

func TestDrift_SubcommandDefinition(t *testing.T) {
	schemaFields := schemaDefProperties(t, "subcommandDefinition")
	goFields := goTypeJSONFields(t, SubcommandDefinition{})
	assertFieldParity(t, "SubcommandDefinition", schemaFields, goFields)
}

func TestDrift_ExampleDefinition(t *testing.T) {
	schemaFields := schemaDefProperties(t, "exampleDefinition")
	goFields := goTypeJSONFields(t, ExampleDefinition{})
	assertFieldParity(t, "ExampleDefinition", schemaFields, goFields)
}

func TestHookDefinition_OutputField(t *testing.T) {
	input := `{
		"kind": "command",
		"command": "node",
		"args": ["prebuild.js"],
		"output": "jsonl"
	}`
	var hook HookDefinition
	if err := json.Unmarshal([]byte(input), &hook); err != nil {
		t.Fatalf("unmarshal hook: %v", err)
	}
	if hook.Output != "jsonl" {
		t.Errorf("Output = %q, want jsonl", hook.Output)
	}
}
