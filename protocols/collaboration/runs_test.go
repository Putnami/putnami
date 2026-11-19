package collaboration

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	extension "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

// TestMissionsV2_PinsItsOperations pins what a missions version 2 provider
// implements: the version 1 operations, and start, report and runs.
func TestMissionsV2_PinsItsOperations(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "one-closed-catalog", "missions-v2-pins-its-operations")
	spec, ok := Lookup(ContractMissions, 2)
	if !ok {
		t.Fatal("missions version 2 is missing from the catalog")
	}
	if spec.Includes != 1 {
		t.Errorf("missions v2 includes version %d, want 1", spec.Includes)
	}
	if got, want := spec.RequiredOperations(), []string{OperationAsk, OperationReply, OperationReport, OperationRuns, OperationStart, OperationSync}; !slices.Equal(got, want) {
		t.Errorf("missions v2 required = %v, want %v", got, want)
	}
	if got := operationNames(spec); !slices.Equal(got, []string{OperationAsk, OperationReply, OperationReport, OperationRuns, OperationStart, OperationStatus, OperationSync}) {
		t.Errorf("missions v2 operations = %v", got)
	}
	for name, access := range map[string]Access{OperationStart: AccessMutating, OperationReport: AccessMutating, OperationRuns: AccessRead} {
		op, _ := spec.Operation(name)
		if op.Access != access || op.HostOnly || op.Preconditions != PreconditionRuleNone || op.WorkspaceSelection {
			t.Errorf("missions v2 %s is %+v", name, op)
		}
	}
	for _, other := range Catalog() {
		if other.Includes != 0 && (other.Name != ContractMissions || other.Version != 2) {
			t.Errorf("%s v%d includes version %d; only missions v2 includes another version", other.Name, other.Version, other.Includes)
		}
	}
}

// TestMissionsV2_RepeatsEveryV1OperationAndDocument holds version 2 on the
// version 1 operations: the same rules, the same Go documents, and schema
// definitions that reference the version 1 ones.
func TestMissionsV2_RepeatsEveryV1OperationAndDocument(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "missions-v2-includes-v1", "missions-v2-repeats-every-v1-operation-and-document")
	v1, _ := Lookup(ContractMissions, 1)
	v2, _ := Lookup(ContractMissions, 2)
	defs := readSchemaNode(t, ContractSchemaFile(ContractMissions, 2)).Defs
	for _, op := range v1.Operations {
		got, ok := v2.Operation(op.Name)
		if !ok {
			t.Errorf("missions v2 lacks %s", op.Name)
			continue
		}
		if got.Access != op.Access || got.Required != op.Required || got.Destructive != op.Destructive ||
			got.HostOnly != op.HostOnly || got.Summary != op.Summary || !slices.Equal(got.Members, op.Members) {
			t.Errorf("missions v2 %s differs from v1", op.Name)
		}
		if reflect.TypeOf(got.NewInput()) != reflect.TypeOf(op.NewInput()) || reflect.TypeOf(got.NewResult()) != reflect.TypeOf(op.NewResult()) {
			t.Errorf("missions v2 %s exchanges other documents than v1", op.Name)
		}
		for _, side := range []string{"Input", "Result"} {
			want := ContractSchemaFile(ContractMissions, 1) + "#/$defs/" + op.Name + side
			if node := defs[op.Name+side]; node == nil || node.Ref != want {
				t.Errorf("missions v2 $defs.%s%s does not reference %s", op.Name, side, want)
			}
		}
	}
}

// TestMissionsV2_DeclarationIsOfferedV1 checks that a provider of version 2
// serves the workspaces bound to version 1.
func TestMissionsV2_DeclarationIsOfferedV1(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "missions-v2-includes-v1", "a-v2-declaration-is-offered-v1")
	operations := map[string]extension.ProviderOperation{}
	for _, name := range []string{OperationAsk, OperationReply, OperationReport, OperationRuns, OperationStart, OperationStatus, OperationSync} {
		operations[name] = providerOperation(ContractMissions, name)
	}
	declaration := extension.ProviderDeclaration{Version: 2, Command: "provider", Operations: operations}
	offer, diags := ReadProviderOffer(map[string]extension.ProviderDeclaration{ContractMissions: declaration})
	if diag.HasErrors(diags) {
		t.Fatalf("a complete version 2 declaration was refused: %v", diags)
	}
	if got := offer.Versions(ContractMissions); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("missions versions = %v, want [1 2]", got)
	}
	v1, _ := offer.Operations(ContractMissions, 1)
	if got := sortedKeys(v1); !slices.Equal(got, []string{OperationAsk, OperationReply, OperationStatus, OperationSync}) {
		t.Errorf("missions v1 offer = %v", got)
	}
	v1Spec, _ := Lookup(ContractMissions, 1)
	for name, provided := range v1 {
		want, _ := v1Spec.Operation(name)
		if provided.Spec.Summary != want.Summary || provided.Provider.Version != 2 {
			t.Errorf("missions v1 %s resolves to %+v", name, provided)
		}
	}
	v2, _ := offer.Operations(ContractMissions, 2)
	if len(v2) != 7 {
		t.Errorf("missions v2 offer = %v", sortedKeys(v2))
	}

	// A version 2 declaration without a run operation offers neither version.
	delete(operations, OperationRuns)
	offer, diags = ReadProviderOffer(map[string]extension.ProviderDeclaration{ContractMissions: declaration})
	if !strings.Contains(FormatDiagnostics(diags), "required operations: runs") || len(offer.Versions(ContractMissions)) != 0 {
		t.Errorf("an incomplete version 2 declaration: versions %v, diagnostics %v", offer.Versions(ContractMissions), diags)
	}
}

// TestIncludes_AnIncompleteDeclarationIsNotOfferedTheIncludedVersion: a
// version that includes another repeats every operation the included one
// requires, as a required one, and a declaration that leaves one out is not
// offered the included version.
func TestIncludes_AnIncompleteDeclarationIsNotOfferedTheIncludedVersion(t *testing.T) {
	for _, spec := range Catalog() {
		if spec.Includes == 0 {
			continue
		}
		included, ok := Lookup(spec.Name, spec.Includes)
		if !ok {
			t.Fatalf("%s v%d includes version %d, which the catalog lacks", spec.Name, spec.Version, spec.Includes)
		}
		for _, op := range included.Operations {
			if got, ok := spec.Operation(op.Name); !ok || (op.Required && !got.Required) {
				t.Errorf("%s v%d does not require %s, which v%d requires", spec.Name, spec.Version, op.Name, spec.Includes)
			}
		}
	}
	spec, _ := Lookup(ContractMissions, 2)
	operations := map[string]ProvidedOperation{}
	for _, op := range spec.Operations {
		if op.Name != OperationSync {
			operations[op.Name] = ProvidedOperation{Spec: op}
		}
	}
	if _, ok := includedOperations(spec, operations); ok {
		t.Error("a declaration without sync is offered missions v1")
	}
	if _, ok := includedOperations(ContractSpec{Name: ContractMissions, Version: 9, Includes: 8}, operations); ok {
		t.Error("an included version the catalog lacks is offered")
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// validRun returns a valid run of kind in state.
func validRun(kind RunKind, state RunState) Run {
	run := Run{
		ID: "R-1", Mission: MissionName{Repository: "acme/app", Key: "issue-7"},
		Kind: kind, State: state, Input: validRunInput(kind), StartedAt: "2026-11-19T09:00:00Z",
	}
	if state != RunStateRunning {
		run.EndedAt = "2026-11-19T09:05:00Z"
	}
	switch state {
	case RunStateSucceeded:
		result := validRunResult(kind)
		run.Result = &result
	case RunStateFailed:
		run.Failure = "the gate failed"
	}
	return run
}

func validRunInput(kind RunKind) RunInput {
	change := Change{Base: "main", Head: "topic"}
	switch kind {
	case RunKindAudit:
		return RunInput{Audit: &AuditRunInput{Scope: "protocols/collaboration", Revision: "abc1234"}}
	case RunKindBuild:
		return RunInput{Build: &BuildRunInput{Task: &Ref{Source: "github:acme/app", ID: "7"}}}
	case RunKindRebase:
		return RunInput{Rebase: &RebaseRunInput{Change: change}}
	case RunKindReview:
		return RunInput{Review: &ReviewRunInput{Change: change, Commit: "abc1234"}}
	case RunKindTriage:
		return RunInput{Triage: &TriageRunInput{Task: Ref{Source: "github:acme/app", ID: "7"}}}
	}
	return RunInput{}
}

func validRunResult(kind RunKind) RunResult {
	findings := []Finding{{Severity: FindingSeverityHigh, Title: "Unchecked error", File: "a.go", Line: 12, Body: "Check it."}}
	switch kind {
	case RunKindAudit:
		return RunResult{Audit: &AuditRunResult{Scope: "protocols/collaboration", Revision: "abc1234", Verdict: AuditVerdictConcerns, Findings: findings}}
	case RunKindBuild:
		return RunResult{Build: &BuildRunResult{Stage: LoopStageMerge, Commit: "abc1234"}}
	case RunKindRebase:
		return RunResult{Rebase: &RebaseRunResult{Commit: "def5678", Onto: "abc1234"}}
	case RunKindReview:
		return RunResult{Review: &ReviewRunResult{Commit: "abc1234", Verdict: ReviewVerdictRequestChanges, Findings: findings}}
	case RunKindTriage:
		return RunResult{Triage: &TriageRunResult{Summary: "A duplicate.", State: TaskStateCanceled, Labels: []string{"duplicate"}}}
	}
	return RunResult{}
}

// TestRuns_EveryKindHasAClosedInputAndResult parses a run of every kind and
// state, and refuses a member its kind does not declare.
func TestRuns_EveryKindHasAClosedInputAndResult(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "missions-typed-runs", "every-kind-has-a-closed-input-and-result")
	for _, kind := range ValidRunKinds {
		for _, state := range ValidRunStates {
			document, err := json.Marshal(MissionsRunResult{Run: validRun(kind, state)})
			if err != nil {
				t.Fatal(err)
			}
			if _, diags := ParseResult(ContractMissions, 2, OperationStart, document); diags != nil {
				t.Errorf("%s %s run refused: %v", kind, state, diags)
			}
		}
		// Every kind's input and result is closed: an undeclared member is
		// refused.
		run, _ := json.Marshal(MissionsRunResult{Run: validRun(kind, RunStateSucceeded)})
		for _, path := range []string{`"input":{"` + string(kind) + `":{`, `"result":{"` + string(kind) + `":{`} {
			open := strings.Replace(string(run), path, path+`"extra":1,`, 1)
			if open == string(run) {
				t.Fatalf("%s: %s not found in %s", kind, path, run)
			}
			if _, diags := ParseResult(ContractMissions, 2, OperationStart, []byte(open)); !slices.Contains(distinctCodes(diags), ErrorCodeUnknownField) {
				t.Errorf("%s: an undeclared member under %s is accepted: %v", kind, path, diags)
			}
		}
	}
	// The run documents exist only in version 2.
	if _, diags := ParseRequest(ContractMissions, 1, OperationStart, []byte(`{}`)); !slices.Equal(distinctCodes(diags), []string{ErrorCodeUnknownOperation}) {
		t.Errorf("missions v1 start: %v", diags)
	}
}

// TestRuns_AnInputOrResultOfAnotherKindIsRefused checks that input and result
// carry exactly the member the kind names.
func TestRuns_AnInputOrResultOfAnotherKindIsRefused(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "missions-typed-runs", "a-run-whose-input-or-result-names-another-kind-is-refused")
	review := validRunResult(RunKindReview)
	both := validRunInput(RunKindReview)
	both.Audit = validRunInput(RunKindAudit).Audit
	cases := map[string]Run{
		"input of another kind": func() Run {
			r := validRun(RunKindAudit, RunStateRunning)
			r.Input = validRunInput(RunKindReview)
			return r
		}(),
		"result of another kind": func() Run {
			r := validRun(RunKindAudit, RunStateSucceeded)
			r.Result = &review
			return r
		}(),
		"two input members": func() Run {
			r := validRun(RunKindReview, RunStateRunning)
			r.Input = both
			return r
		}(),
		"no input member": func() Run {
			r := validRun(RunKindReview, RunStateRunning)
			r.Input = RunInput{}
			return r
		}(),
		"succeeded without a result": func() Run {
			r := validRun(RunKindReview, RunStateSucceeded)
			r.Result = nil
			return r
		}(),
		"running with a result": func() Run {
			r := validRun(RunKindReview, RunStateRunning)
			r.Result = &review
			return r
		}(),
		"failed without a failure": func() Run {
			r := validRun(RunKindReview, RunStateFailed)
			r.Failure = ""
			return r
		}(),
		"running with an end": func() Run {
			r := validRun(RunKindReview, RunStateRunning)
			r.EndedAt = "2026-11-19T09:05:00Z"
			return r
		}(),
		"ended before it started": func() Run {
			r := validRun(RunKindReview, RunStateCanceled)
			r.EndedAt = "2026-11-19T08:00:00Z"
			return r
		}(),
		"unknown kind": func() Run {
			r := validRun(RunKindReview, RunStateRunning)
			r.Kind = "deploy"
			return r
		}(),
	}
	for name, run := range cases {
		if diags := ValidateResult(&MissionsRunResult{Run: run}); !diag.HasErrors(diags) {
			t.Errorf("%s: accepted", name)
		}
	}
	report := &MissionsReportInput{Run: "R-1", State: RunStateSucceeded, Result: &RunResult{}}
	if diags := ValidateInput(report); !diag.HasErrors(diags) {
		t.Error("a report with an empty result is accepted")
	}
	report = &MissionsReportInput{Run: "R-1", State: RunStateRunning}
	if diags := ValidateInput(report); !diag.HasErrors(diags) {
		t.Error("a report that leaves the run running is accepted")
	}
}

// TestRuns_FindingsAreBounded holds the findings on their count, their texts
// and their lines.
func TestRuns_FindingsAreBounded(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "missions-typed-runs", "findings-are-bounded")
	withFindings := func(findings []Finding) *MissionsRunResult {
		run := validRun(RunKindReview, RunStateSucceeded)
		run.Result.Review.Findings = findings
		return &MissionsRunResult{Run: run}
	}
	many := func(count, body int) []Finding {
		out := make([]Finding, count)
		for i := range out {
			out[i] = Finding{Severity: FindingSeverityLow, Title: fmt.Sprintf("Finding %d", i), Body: strings.Repeat("x", body)}
		}
		return out
	}
	for _, tc := range []struct {
		name     string
		findings []Finding
		valid    bool
	}{
		{"none", []Finding{}, true},
		{"absent", nil, false},
		{"the most findings", many(MaxRunFindings, 0), true},
		{"one finding too many", many(MaxRunFindings+1, 0), false},
		{"texts above the total bound", many(10, MaxFindingBodyLength), false},
		{"a body above its bound", many(1, MaxFindingBodyLength+1), false},
		{"a line without a file", []Finding{{Severity: FindingSeverityInfo, Title: "t", Line: 3}}, false},
		{"a negative line", []Finding{{Severity: FindingSeverityInfo, Title: "t", File: "a.go", Line: -1}}, false},
		{"an unknown severity", []Finding{{Severity: "critical", Title: "t"}}, false},
		{"no title", []Finding{{Severity: FindingSeverityInfo}}, false},
	} {
		if diags := ValidateResult(withFindings(tc.findings)); (diags == nil) != tc.valid {
			t.Errorf("%s: valid %v, want %v (%v)", tc.name, diags == nil, tc.valid, diags)
		}
	}
}

// TestRuns_SchemaTracksTheGoTypes holds the run documents and their
// vocabularies on the Go types and the validator's bounds.
func TestRuns_SchemaTracksTheGoTypes(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "schemas-track-the-go-types", "entity-schemas-match-the-go-types")
	defs := readSchemaNode(t, ContractSchemaFile(ContractMissions, 2)).Defs
	for name, goType := range map[string]any{
		"finding": Finding{}, "buildInput": BuildRunInput{}, "buildResult": BuildRunResult{},
		"reviewInput": ReviewRunInput{}, "reviewResult": ReviewRunResult{}, "rebaseInput": RebaseRunInput{},
		"rebaseResult": RebaseRunResult{}, "auditInput": AuditRunInput{}, "auditResult": AuditRunResult{},
		"triageInput": TriageRunInput{}, "triageResult": TriageRunResult{}, "runInput": RunInput{},
		"runResult": RunResult{}, "run": Run{},
	} {
		node := defs[name]
		if node == nil {
			t.Errorf("$defs.%s is missing", name)
			continue
		}
		if got, want := sortedProperties(node), goMembers(reflect.TypeOf(goType)); !slices.Equal(got, want) {
			t.Errorf("%s: schema members %v, Go members %v", name, got, want)
		}
	}
	// A build names its task or its intention, as the validator requires.
	var alternatives [][]string
	for _, alternative := range defs["buildInput"].AnyOf {
		alternatives = append(alternatives, alternative.Required)
	}
	if want := [][]string{{"task"}, {"intention"}}; !slices.EqualFunc(alternatives, want, slices.Equal[[]string]) {
		t.Errorf("buildInput anyOf requires %v, want %v", alternatives, want)
	}
	for name, kinds := range map[string]*schemaNode{"runInput": defs["runInput"], "runResult": defs["runResult"]} {
		if got := sortedProperties(kinds); !slices.Equal(got, stringsOf(ValidRunKinds)) {
			t.Errorf("%s members %v, run kinds %v", name, got, ValidRunKinds)
		}
	}
	for _, tc := range []struct {
		name string
		enum []string
		want []string
	}{
		{"runKind", defs["runKind"].Enum, stringsOf(ValidRunKinds)},
		{"runState", defs["runState"].Enum, stringsOf(ValidRunStates)},
		{"findingSeverity", defs["findingSeverity"].Enum, stringsOf(ValidFindingSeverities)},
		{"auditVerdict", defs["auditVerdict"].Enum, stringsOf(ValidAuditVerdicts)},
		{"reportInput.state", defs["reportInput"].Properties["state"].Enum, []string{"canceled", "failed", "succeeded"}},
	} {
		if !slices.Equal(tc.enum, tc.want) || !slices.IsSorted(tc.want) {
			t.Errorf("%s enum %v, Go vocabulary %v", tc.name, tc.enum, tc.want)
		}
	}
	for _, tc := range []struct {
		name string
		got  *int
		want int
	}{
		{"findings maxItems", defs["findings"].MaxItems, MaxRunFindings},
		{"finding.body maxLength", defs["finding"].Properties["body"].MaxLength, MaxFindingBodyLength},
		{"finding.title maxLength", defs["finding"].Properties["title"].MaxLength, MaxTitleLength},
		{"finding.file maxLength", defs["finding"].Properties["file"].MaxLength, MaxFilePathBytes},
		{"finding.line maximum", defs["finding"].Properties["line"].Maximum, MaxFindingLine},
		{"buildInput.intention maxLength", defs["buildInput"].Properties["intention"].MaxLength, MaxRunTextLength},
		{"triageResult.summary maxLength", defs["triageResult"].Properties["summary"].MaxLength, MaxRunTextLength},
		{"auditInput.scope maxLength", defs["auditInput"].Properties["scope"].MaxLength, MaxAuditScopeLength},
		{"run.failure maxLength", defs["run"].Properties["failure"].MaxLength, MaxErrorLength},
		{"runsResult.items maxItems", defs["runsResult"].Properties["items"].MaxItems, MaxListMembers},
	} {
		if tc.got == nil || *tc.got != tc.want {
			t.Errorf("%s is %v in the schema, %d in Go", tc.name, tc.got, tc.want)
		}
	}
}
