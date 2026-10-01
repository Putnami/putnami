package cli

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"sort"
	"testing"
)

// The v2 contract has three representations that must never move alone: the Go
// structs producers marshal, the validator member lists both runtimes enforce,
// and schemas/result-v2.json that non-Go surfaces bind to. These tests lock the
// three together, so adding a member anywhere fails until it is added
// everywhere.

const resultV2SchemaPath = "schemas/result-v2.json"

type resultV2Schema struct {
	ID   string `json:"$id"`
	Defs map[string]struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
		AllOf      []json.RawMessage          `json:"allOf"`
		OneOf      []json.RawMessage          `json:"oneOf"`
	} `json:"$defs"`
}

func loadResultV2Schema(t *testing.T) resultV2Schema {
	t.Helper()
	data, err := os.ReadFile(resultV2SchemaPath)
	if err != nil {
		t.Fatalf("read %s: %v", resultV2SchemaPath, err)
	}
	var schema resultV2Schema
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse %s: %v", resultV2SchemaPath, err)
	}
	return schema
}

// TestResultV2SchemaID keeps ResultV2SchemaID and the schema's $id in lock-step.
func TestResultV2SchemaID(t *testing.T) {
	if got := loadResultV2Schema(t).ID; got != ResultV2SchemaID {
		t.Errorf("schema $id = %q, ResultV2SchemaID = %q", got, ResultV2SchemaID)
	}
}

// TestResultV2ProtocolVersionPinned makes a version bump deliberate: the
// constant, the schema's const, and every corpus fixture move together.
func TestResultV2ProtocolVersionPinned(t *testing.T) {
	if ResultProtocolVersion != 2 {
		t.Fatalf("ResultProtocolVersion = %d, want 2 (a bump must update the schema const and the corpus)", ResultProtocolVersion)
	}
	data, err := os.ReadFile(resultV2SchemaPath)
	if err != nil {
		t.Fatalf("read %s: %v", resultV2SchemaPath, err)
	}
	var schema struct {
		Defs struct {
			ProtocolVersion struct {
				Const *int `json:"const"`
			} `json:"protocolVersion"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse %s: %v", resultV2SchemaPath, err)
	}
	if schema.Defs.ProtocolVersion.Const == nil {
		t.Fatalf("%s: $defs.protocolVersion declares no const", resultV2SchemaPath)
	}
	if *schema.Defs.ProtocolVersion.Const != ResultProtocolVersion {
		t.Errorf("schema protocolVersion const = %d, ResultProtocolVersion = %d",
			*schema.Defs.ProtocolVersion.Const, ResultProtocolVersion)
	}
}

// TestResultV2ValidatorMatchesSchema pins the validator's member lists against
// the schema's $defs — both the member names and which of them are required.
// The validator is what the cross-language corpus exercises, so a schema member
// nothing validates would be a documented promise no test keeps.
func TestResultV2ValidatorMatchesSchema(t *testing.T) {
	schema := loadResultV2Schema(t)
	for name, fields := range v2FieldSets {
		t.Run(name, func(t *testing.T) {
			def, ok := schema.Defs[name]
			if !ok {
				t.Fatalf("%s has no $defs/%s", resultV2SchemaPath, name)
			}
			if got, want := keys(def.Properties), sortedStrings(fieldNames(fields)); !reflect.DeepEqual(got, want) {
				t.Errorf("%s members drift:\n schema:    %v\n validator: %v", name, got, want)
			}
			if got, want := sortedStrings(def.Required), sortedStrings(requiredFieldNames(fields)); !reflect.DeepEqual(got, want) {
				t.Errorf("%s required drift:\n schema:    %v\n validator: %v", name, got, want)
			}
		})
	}
}

// TestResultV2SchemaDefsAreValidated is the reverse direction: every object
// $def in the schema has a validator. Value-only $defs (enums, scalars) are
// exempt because they declare no properties.
func TestResultV2SchemaDefsAreValidated(t *testing.T) {
	schema := loadResultV2Schema(t)
	for name, def := range schema.Defs {
		if len(def.Properties) == 0 {
			continue
		}
		if _, ok := v2FieldSets[name]; !ok {
			t.Errorf("%s declares object $defs/%s but no validator enforces it", resultV2SchemaPath, name)
		}
	}
}

// v2StructDefs maps each v2 Go struct onto the schema $def it serializes as.
var v2StructDefs = map[string]any{
	"projectIdentity":          ProjectIdentity{},
	"taskRef":                  TaskRef{},
	"providerIdentity":         ProviderIdentity{},
	"taskIdentity":             TaskIdentity{},
	"resultError":              ResultError{},
	"diagnostic":               Diagnostic{},
	"executionRecord":          ExecutionRecord{},
	"taskRecord":               TaskRecord{},
	"testCase":                 TestCase{},
	"taskFailure":              TaskFailure{},
	"runCounts":                RunCounts{},
	"runReuse":                 RunReuse{},
	"dockerPublishTimings":     DockerPublishTimings{},
	"dockerPublishConcurrency": DockerPublishConcurrency{},
	"dockerPublication":        DockerPublication{},
	"runCpu":                   RunCPU{},
	"runLocalCache":            RunLocalCache{},
	"runCache":                 RunCache{},
	"runSummary":               RunSummary{},
	"streamRunSummary":         StreamRunSummary{},
	"cgroupThrottle":           CgroupThrottle{},
	"cgroupCpu":                CgroupCPU{},
	"cpuPressure":              CPUPressure{},
	"hostCpuTime":              HostCPUTime{},
	"cgroupMemoryLimit":        CgroupMemoryLimit{},
	"memoryCapacity":           MemoryCapacity{},
	"cgroupMemoryComposition":  CgroupMemoryComposition{},
	"cgroupMemoryLifetimePeak": CgroupMemoryLifetimePeak{},
	"cgroupMemoryClosing":      CgroupMemoryClosing{},
	"cgroupMemoryEvents":       CgroupMemoryEvents{},
	"cgroupMemory":             CgroupMemory{},
	"memoryPressure":           MemoryPressure{},
	"sessionTree":              SessionTree{},
	"sessionPlacement":         SessionPlacement{},
	"sessionProvenance":        SessionProvenance{},
	"sessionEnvironment":       SessionEnvironment{},
	"preparationPhaseRecord":   PreparationPhaseRecord{},
	"sessionPreparation":       SessionPreparation{},
	"machineOutputBudget":      MachineOutputBudget{},
	"machineOutputElision":     MachineOutputElision{},
	"machineOutputElisions":    MachineOutputElisions{},
	"machineOutputArtifact":    MachineOutputArtifact{},
	"machineOutputSummary":     MachineOutputSummary{},
	"planMetrics":              PlanMetrics{},
	"plannedTask":              PlannedTask{},
	"planSummary":              PlanSummary{},
	"resultEnvelope":           ResultV2{},
	"sessionStreamRecord":      SessionStreamRecord{},
	"boundedSessionEndRecord":  BoundedSessionEndRecord{},
	"mcpResult":                MCPResult{},
	"sessionFile":              SessionFile{},
	"sessionPlanFile":          SessionPlanFile{},
	"reportGit":                ReportGit{},
	"reportRun":                ReportRun{},
	"reportTests":              ReportTests{},
	"reportCoverage":           ReportCoverage{},
	"reportCommand":            ReportCommand{},
	"reportJob":                ReportJob{},
	"reportCache":              ReportCache{},
	"reportScheduler":          ReportScheduler{},
	"reportFile":               ReportFile{},
}

// TestResultV2StructsMatchSchema pins the Go structs producers marshal against
// the same $defs. Together with the validator test above, the struct, the
// validator and the schema are one shape.
func TestResultV2StructsMatchSchema(t *testing.T) {
	schema := loadResultV2Schema(t)
	for name, value := range v2StructDefs {
		t.Run(name, func(t *testing.T) {
			def, ok := schema.Defs[name]
			if !ok {
				t.Fatalf("%s has no $defs/%s", resultV2SchemaPath, name)
			}
			if got, want := keys(def.Properties), jsonFieldNames(t, value); !reflect.DeepEqual(got, want) {
				t.Errorf("%s fields drift:\n schema: %v\n struct: %v", name, got, want)
			}
		})
	}
	if len(v2StructDefs) != len(v2FieldSets) {
		t.Errorf("v2StructDefs covers %d $defs, v2FieldSets covers %d — every validated object needs a Go struct",
			len(v2StructDefs), len(v2FieldSets))
	}
}

// TestSessionGitMatchesSchema covers the one object the schema inlines rather
// than naming as a $def.
func TestSessionGitMatchesSchema(t *testing.T) {
	schema := loadResultV2Schema(t)
	def, ok := schema.Defs["sessionFile"]
	if !ok {
		t.Fatal("schema has no $defs/sessionFile")
	}
	var git struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(def.Properties["git"], &git); err != nil {
		t.Fatalf("parse $defs/sessionFile.properties.git: %v", err)
	}
	if got, want := keys(git.Properties), sortedStrings(fieldNames(sessionGitFields)); !reflect.DeepEqual(got, want) {
		t.Errorf("sessionFile.git members drift:\n schema:    %v\n validator: %v", got, want)
	}
	if got, want := keys(git.Properties), jsonFieldNames(t, SessionGit{}); !reflect.DeepEqual(got, want) {
		t.Errorf("sessionFile.git fields drift:\n schema: %v\n struct: %v", got, want)
	}
}

// TestSessionSelectionMatchesSchema covers the second object the schema inlines
// rather than naming as a $def, the same way TestSessionGitMatchesSchema covers
// the git block: the struct, the validator member list, the required set and the
// mode vocabulary are one shape.
func TestSessionSelectionMatchesSchema(t *testing.T) {
	schema := loadResultV2Schema(t)
	def, ok := schema.Defs["sessionFile"]
	if !ok {
		t.Fatal("schema has no $defs/sessionFile")
	}
	var selection struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(def.Properties["selection"], &selection); err != nil {
		t.Fatalf("parse $defs/sessionFile.properties.selection: %v", err)
	}
	if got, want := keys(selection.Properties), sortedStrings(fieldNames(sessionSelectionFields)); !reflect.DeepEqual(got, want) {
		t.Errorf("sessionFile.selection members drift:\n schema:    %v\n validator: %v", got, want)
	}
	if got, want := keys(selection.Properties), jsonFieldNames(t, SessionSelection{}); !reflect.DeepEqual(got, want) {
		t.Errorf("sessionFile.selection fields drift:\n schema: %v\n struct: %v", got, want)
	}
	if got, want := sortedStrings(selection.Required), sortedStrings(requiredFieldNames(sessionSelectionFields)); !reflect.DeepEqual(got, want) {
		t.Errorf("sessionFile.selection required drift:\n schema:    %v\n validator: %v", got, want)
	}
	var mode struct {
		Enum []string `json:"enum"`
	}
	if err := json.Unmarshal(selection.Properties["mode"], &mode); err != nil {
		t.Fatalf("parse sessionFile.selection.mode: %v", err)
	}
	if got, want := sortedStrings(mode.Enum), sortedStrings(SessionSelectionModes); !reflect.DeepEqual(got, want) {
		t.Errorf("sessionFile.selection.mode enum drift:\n schema: %v\n Go:     %v", got, want)
	}
}

// TestSessionFileCarriesTheNestingAndSelectionMembers states, as a contract
// fact rather than an implementation detail, that a recorded session can name
// the run that spawned it and how that run selected. Both members are optional,
// so a record written before they existed keeps validating.
func TestSessionFileCarriesTheNestingAndSelectionMembers(t *testing.T) {
	names := fieldNames(sessionFileFields)
	required := requiredFieldNames(sessionFileFields)
	for _, member := range []string{"parentSessionId", "selection"} {
		if !slices.Contains(names, member) {
			t.Errorf("sessionFile declares no %q member", member)
		}
		if slices.Contains(required, member) {
			t.Errorf("%q is required; every session recorded before it existed would stop validating", member)
		}
	}
}

// TestReportBoundsMatchSchema pins the exported report caps against the schema's
// own bounds.
//
// The caps are a CONTRACT clause, so three parties have to agree on them: the Go
// constants a producer sizes its lists from, the schema a non-Go consumer binds
// to, and the TypeScript mirror (whose test applies the same pin against the
// same schema). Without this, Go could bound at 64 while the schema published 50
// and nothing would fail until a consumer rejected a conforming document.
func TestReportBoundsMatchSchema(t *testing.T) {
	data, err := os.ReadFile(resultV2SchemaPath)
	if err != nil {
		t.Fatalf("read %s: %v", resultV2SchemaPath, err)
	}
	var schema struct {
		Defs struct {
			ReportFile struct {
				Properties struct {
					Jobs struct {
						MaxItems *int `json:"maxItems"`
					} `json:"jobs"`
				} `json:"properties"`
			} `json:"reportFile"`
			ReportJob struct {
				Properties struct {
					Diagnostics struct {
						MaxItems *int `json:"maxItems"`
						Items    struct {
							Properties struct {
								Message struct {
									MaxLength *int `json:"maxLength"`
								} `json:"message"`
							} `json:"properties"`
						} `json:"items"`
					} `json:"diagnostics"`
				} `json:"properties"`
			} `json:"reportJob"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse %s: %v", resultV2SchemaPath, err)
	}
	bounds := []struct {
		name   string
		schema *int
		want   int
	}{
		{"reportFile.jobs.maxItems", schema.Defs.ReportFile.Properties.Jobs.MaxItems, ReportMaxJobs},
		{"reportJob.diagnostics.maxItems", schema.Defs.ReportJob.Properties.Diagnostics.MaxItems, ReportMaxJobDiagnostics},
		{
			"reportJob.diagnostics.items.message.maxLength",
			schema.Defs.ReportJob.Properties.Diagnostics.Items.Properties.Message.MaxLength,
			ReportMaxMessageBytes,
		},
	}
	for _, bound := range bounds {
		if bound.schema == nil {
			t.Errorf("%s: schema declares no bound, want %d", bound.name, bound.want)
			continue
		}
		if *bound.schema != bound.want {
			t.Errorf("%s: schema = %d, Go constant = %d", bound.name, *bound.schema, bound.want)
		}
	}
}

// TestTestCaseBoundsMatchSchema pins the three test-case bounds against the
// schema, the same way TestReportBoundsMatchSchema pins the report caps: the Go
// constants BoundTestCases applies, the schema a non-Go consumer binds to, and
// the TypeScript mirror (pinned against the same file) cannot disagree.
func TestTestCaseBoundsMatchSchema(t *testing.T) {
	data, err := os.ReadFile(resultV2SchemaPath)
	if err != nil {
		t.Fatalf("read %s: %v", resultV2SchemaPath, err)
	}
	type bounded struct {
		MaxLength *int `json:"maxLength"`
	}
	var schema struct {
		Defs struct {
			TestCaseMaxPerTask struct {
				Const *int `json:"const"`
			} `json:"testCaseMaxPerTask"`
			TestCaseMaxBytesPerTask struct {
				Const *int `json:"const"`
			} `json:"testCaseMaxBytesPerTask"`
			TestCaseMaxBytesPerBatch struct {
				Const *int `json:"const"`
			} `json:"testCaseMaxBytesPerBatch"`
			TestCase struct {
				Properties struct {
					Name   bounded `json:"name"`
					Suite  bounded `json:"suite"`
					File   bounded `json:"file"`
					Output bounded `json:"output"`
					Status struct {
						Enum []string `json:"enum"`
					} `json:"status"`
				} `json:"properties"`
			} `json:"testCase"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse %s: %v", resultV2SchemaPath, err)
	}
	properties := schema.Defs.TestCase.Properties
	bounds := []struct {
		name   string
		schema *int
		want   int
	}{
		{"testCaseMaxPerTask.const", schema.Defs.TestCaseMaxPerTask.Const, TestCaseMaxPerTask},
		{"testCaseMaxBytesPerTask.const", schema.Defs.TestCaseMaxBytesPerTask.Const, TestCaseMaxBytesPerTask},
		{"testCaseMaxBytesPerBatch.const", schema.Defs.TestCaseMaxBytesPerBatch.Const, TestCaseMaxBytesPerBatch},
		{"testCase.name.maxLength", properties.Name.MaxLength, TestCaseMaxTextBytes},
		{"testCase.suite.maxLength", properties.Suite.MaxLength, TestCaseMaxTextBytes},
		{"testCase.file.maxLength", properties.File.MaxLength, TestCaseMaxTextBytes},
		{"testCase.output.maxLength", properties.Output.MaxLength, TestCaseMaxOutputBytes},
	}
	for _, bound := range bounds {
		if bound.schema == nil {
			t.Errorf("%s: schema declares no bound, want %d", bound.name, bound.want)
			continue
		}
		if *bound.schema != bound.want {
			t.Errorf("%s: schema = %d, Go constant = %d", bound.name, *bound.schema, bound.want)
		}
	}
	want := []string{TestCaseStatusFailed, TestCaseStatusPassed, TestCaseStatusSkipped}
	if got := sortedStrings(properties.Status.Enum); !reflect.DeepEqual(got, want) {
		t.Errorf("testCase.status enum = %v, Go statuses = %v", got, want)
	}
}

// TestMachineOutputBudgetsMatchSchema pins the two fixed live profiles across
// producer constants and the schema conditionals non-Go consumers validate.
func TestMachineOutputBudgetsMatchSchema(t *testing.T) {
	def, ok := loadResultV2Schema(t).Defs["machineOutputSummary"]
	if !ok {
		t.Fatal("schema has no $defs/machineOutputSummary")
	}
	type schemaBudgetRule struct {
		If struct {
			Properties struct {
				Mode struct {
					Const string `json:"const"`
				} `json:"mode"`
			} `json:"properties"`
		} `json:"if"`
		Then struct {
			Properties struct {
				Budget struct {
					Properties map[string]struct {
						Const *int64 `json:"const"`
					} `json:"properties"`
				} `json:"budget"`
			} `json:"properties"`
		} `json:"then"`
	}
	wantByMode := map[string]MachineOutputBudget{}
	for _, mode := range []string{MachineOutputModeNormal, MachineOutputModeVerbose} {
		budget, known := MachineOutputBudgetFor(mode)
		if !known {
			t.Fatalf("mode %q has no Go budget", mode)
		}
		wantByMode[mode] = budget
	}
	seen := map[string]bool{}
	for i, raw := range def.AllOf {
		var rule schemaBudgetRule
		if err := json.Unmarshal(raw, &rule); err != nil {
			t.Fatalf("parse machineOutputSummary.allOf[%d]: %v", i, err)
		}
		mode := rule.If.Properties.Mode.Const
		want, known := wantByMode[mode]
		if !known {
			t.Fatalf("machineOutputSummary.allOf[%d] selects unknown mode %q", i, mode)
		}
		seen[mode] = true
		values := map[string]int64{
			"maxBytes":              want.MaxBytes,
			"maxRecords":            int64(want.MaxRecords),
			"failureReserveBytes":   want.FailureReserveBytes,
			"failureReserveRecords": int64(want.FailureReserveRecords),
			"finalReserveBytes":     want.FinalReserveBytes,
			"finalReserveRecords":   int64(want.FinalReserveRecords),
		}
		for name, expected := range values {
			member := rule.Then.Properties.Budget.Properties[name]
			if member.Const == nil || *member.Const != expected {
				t.Errorf("schema %s budget %s = %v, Go constant = %d", mode, name, member.Const, expected)
			}
		}
	}
	if len(seen) != len(wantByMode) {
		t.Errorf("schema budget modes = %v, want normal and verbose", seen)
	}
	if got := len(loadResultV2Schema(t).Defs["machineOutputElision"].OneOf); got != 2 {
		t.Errorf("schema machineOutputElision oneOf branches = %d, want zero-pair and positive-pair", got)
	}
}

// TestResultV2ErrorMatchesV1 keeps the error member identical across contract
// versions: v2 reuses ResultError deliberately, so a v1 consumer's error
// handling ports unchanged.
func TestResultV2ErrorMatchesV1(t *testing.T) {
	v1 := loadResultSchema(t)
	v2 := loadResultV2Schema(t)
	v1Def, ok := v1.Defs["resultError"]
	if !ok {
		t.Fatal("v1 schema has no $defs/resultError")
	}
	v2Def, ok := v2.Defs["resultError"]
	if !ok {
		t.Fatal("v2 schema has no $defs/resultError")
	}
	if got, want := keys(v2Def.Properties), keys(v1Def.Properties); !reflect.DeepEqual(got, want) {
		t.Errorf("resultError drifted between contract versions:\n v2: %v\n v1: %v", got, want)
	}
}

func fieldNames(fields []field) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, f.name)
	}
	return out
}

func requiredFieldNames(fields []field) []string {
	var out []string
	for _, f := range fields {
		if f.required {
			out = append(out, f.name)
		}
	}
	return out
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
