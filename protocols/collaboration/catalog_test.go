package collaboration

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// TestCatalog_RequiredOperationsAreTheContract pins what every provider of
// each version must implement. Moving an operation between required and
// optional changes the contract, so it is a new version, never an edit.
func TestCatalog_RequiredOperationsAreTheContract(t *testing.T) {
	want := map[string][]string{
		ContractMemory:    {OperationCheckpoint, OperationContext, OperationMission},
		ContractProposals: {OperationFind, OperationReview, OperationStatus, OperationUpsert},
		ContractTasks:     {OperationCreate, OperationFind, OperationGet, OperationTransition, OperationUpdate},
	}
	optional := map[string][]string{
		ContractMemory:    {OperationSearch},
		ContractProposals: {OperationMerge},
		ContractTasks:     {OperationAssign, OperationClaim, OperationLink},
	}
	for _, contract := range ContractNames {
		spec, ok := Lookup(contract, 1)
		if !ok {
			t.Fatalf("%s version 1 is missing from the catalog", contract)
		}
		if got := spec.RequiredOperations(); !slices.Equal(got, want[contract]) {
			t.Errorf("%s v1 required = %v, want %v", contract, got, want[contract])
		}
		var gotOptional []string
		for _, op := range spec.Operations {
			if !op.Required {
				gotOptional = append(gotOptional, op.Name)
			}
		}
		if !slices.Equal(gotOptional, optional[contract]) {
			t.Errorf("%s v1 optional = %v, want %v", contract, gotOptional, optional[contract])
		}
		if got := SupportedVersions(contract); !slices.Equal(got, []int{1}) {
			t.Errorf("%s supported versions = %v, want [1]", contract, got)
		}
	}
	if SupportedVersions("issues") != nil {
		t.Error("an unknown contract has no supported version")
	}
}

// TestCatalog_OperationsAreWellFormed checks the invariants every entry must
// hold for the orchestrator's rules to be sound.
func TestCatalog_OperationsAreWellFormed(t *testing.T) {
	catalog := Catalog()
	if !slices.IsSortedFunc(catalog, func(a, b ContractSpec) int {
		if a.Name != b.Name {
			return strings.Compare(a.Name, b.Name)
		}
		return a.Version - b.Version
	}) {
		t.Error("the catalog is not in canonical (name, version) order")
	}
	for _, spec := range catalog {
		names := make([]string, 0, len(spec.Operations))
		for _, op := range spec.Operations {
			names = append(names, op.Name)
			if op.Name == OperationCapabilities {
				t.Errorf("%s declares the orchestrator's capabilities operation", spec.Name)
			}
			if strings.TrimSpace(op.Summary) == "" {
				t.Errorf("%s.%s has no summary", spec.Name, op.Name)
			}
			if op.Access != AccessRead && op.Access != AccessMutating {
				t.Errorf("%s.%s access %q", spec.Name, op.Name, op.Access)
			}
			if op.Access == AccessRead && (op.Destructive || op.Preconditions != PreconditionRuleNone) {
				t.Errorf("%s.%s is a read and cannot be destructive or take a precondition", spec.Name, op.Name)
			}
			if op.WorkspaceSelection && op.Access != AccessRead {
				t.Errorf("%s.%s: only reads take the selection arguments", spec.Name, op.Name)
			}
			if op.NewInput() == nil || op.NewResult() == nil {
				t.Errorf("%s.%s allocates no document", spec.Name, op.Name)
			}
		}
		if !slices.IsSorted(names) {
			t.Errorf("%s operations %v are not sorted", spec.Name, names)
		}
		if len(slices.Compact(slices.Clone(names))) != len(names) {
			t.Errorf("%s repeats an operation", spec.Name)
		}
	}
}

// TestCatalog_MutationsThatCreateCarryIdempotencyKeys pins the retry rule:
// every mutation that adds an item carries the key that lets a provider
// recognize a retry instead of adding a second one.
func TestCatalog_MutationsThatCreateCarryIdempotencyKeys(t *testing.T) {
	for _, key := range []OperationKey{
		{ContractTasks, 1, OperationCreate},
		{ContractProposals, 1, OperationReview},
		{ContractMemory, 1, OperationCheckpoint},
	} {
		spec, _ := Lookup(key.Contract, key.Version)
		op, _ := spec.Operation(key.Operation)
		members := goMembersOf(op.NewInput())
		if !slices.Contains(members, "idempotencyKey") {
			t.Errorf("%s input has no idempotencyKey: %v", key, members)
		}
	}
}

func goMembersOf(value any) []string {
	encoded, _ := json.Marshal(value)
	var object map[string]any
	_ = json.Unmarshal(encoded, &object)
	names := make([]string, 0, len(object))
	for name := range object {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func TestLookupRejectsUnknownOperationsAndVersions(t *testing.T) {
	cases := []struct {
		contract  string
		version   int
		operation string
		code      string
	}{
		{"issues", 1, "find", ErrorCodeUnknownContract},
		{ContractTasks, 2, "find", ErrorCodeUnsupportedVersion},
		{ContractTasks, 1, "merge", ErrorCodeUnknownOperation},
	}
	for _, tc := range cases {
		_, diags := ParseRequest(tc.contract, tc.version, tc.operation, nil)
		if got := distinctCodes(diags); !slices.Equal(got, []string{tc.code}) {
			t.Errorf("%s v%d %s: codes %v, want %s", tc.contract, tc.version, tc.operation, got, tc.code)
		}
	}
}

func TestInputSchemaIsSelfContainedForEveryOperation(t *testing.T) {
	for _, spec := range Catalog() {
		for _, name := range append([]string{OperationCapabilities}, operationNames(spec)...) {
			schema, err := InputSchema(spec.Name, spec.Version, name)
			if err != nil {
				t.Fatalf("%s.%s: %v", spec.Name, name, err)
			}
			if strings.Contains(string(schema), "$ref") || strings.Contains(string(schema), "$defs") {
				t.Errorf("%s.%s schema still references definitions: %s", spec.Name, name, schema)
			}
			var object map[string]any
			if err := json.Unmarshal(schema, &object); err != nil {
				t.Fatalf("%s.%s: %v", spec.Name, name, err)
			}
			if object["type"] != "object" || object["additionalProperties"] != false {
				t.Errorf("%s.%s schema is not a closed object: %s", spec.Name, name, schema)
			}
		}
	}
	if _, err := InputSchema(ContractTasks, 1, "merge"); err == nil {
		t.Error("an unknown operation has no input schema")
	}
	schema, err := InputSchema(ContractTasks, 1, OperationGet)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(schema), `"source"`) || !strings.Contains(string(schema), `"pattern"`) {
		t.Errorf("the inlined ref definition lost its members: %s", schema)
	}
}

func operationNames(spec ContractSpec) []string {
	names := make([]string, 0, len(spec.Operations))
	for _, op := range spec.Operations {
		names = append(names, op.Name)
	}
	return names
}

func TestStrictDecodeRefusals(t *testing.T) {
	cases := []struct {
		name string
		data string
		code string
	}{
		{"trailing data", `{"ref":{"source":"local:x","id":"1"}} {}`, ErrorCodeParseError},
		{"not an object", `[1]`, ErrorCodeParseError},
		{"not JSON", `{`, ErrorCodeParseError},
		{"wrong member type", `{"ref":{"source":1,"id":"1"}}`, ErrorCodeParseError},
		{"nested duplicate", `{"ref":{"source":"local:x","id":"1","id":"2"}}`, ErrorCodeDuplicateMember},
		{"nested unknown", `{"ref":{"source":"local:x","id":"1","url":"https://x"}}`, ErrorCodeUnknownField},
		{"too large", `{"ref":{"source":"local:x","id":"` + strings.Repeat("a", MaxDocumentBytes) + `"}}`, ErrorCodeTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, diags := ParseRequest(ContractTasks, 1, OperationGet, []byte(tc.data))
			if got := distinctCodes(diags); !slices.Equal(got, []string{tc.code}) {
				t.Fatalf("codes = %v, want %s (%v)", got, tc.code, diags)
			}
		})
	}
}

func TestValidationBoundsAndTimestamps(t *testing.T) {
	record := MemoryRecord{
		Ref: Ref{Source: "local:x", ID: "m1"}, Revision: "r1", Kind: MemoryKindMission,
		Identity:   MemoryIdentity{Mission: "m1"},
		Content:    "x",
		Provenance: Provenance{RecordedAt: "yesterday"},
		Freshness:  Freshness{UpdatedAt: "2026-09-24T08:00:00Z", RetrievedAt: "2026-09-24T08:00:00Z"},
	}
	if got := distinctCodes(ValidateResult(&MemoryRecordResult{Record: record})); !slices.Equal(got, []string{ErrorCodeInvalidTimestamp}) {
		t.Errorf("a malformed timestamp: codes %v", got)
	}
	items := make([]Task, MaxListMembers+1)
	for i := range items {
		items[i] = Task{Ref: Ref{Source: "local:x", ID: "T"}, Revision: "r", Title: "t", State: TaskStateOpen}
	}
	if got := distinctCodes(ValidateResult(&TaskListResult{Items: items})); !slices.Equal(got, []string{ErrorCodeTooLarge}) {
		t.Errorf("an oversized page: codes %v", got)
	}
	if diags := ValidateInput(&TaskCreateInput{Title: strings.Repeat("t", MaxTitleLength+1), IdempotencyKey: "k"}); !slices.Equal(distinctCodes(diags), []string{ErrorCodeTooLarge}) {
		t.Errorf("an oversized title: %v", diags)
	}
	if diags := ValidateInput(&TaskCreateInput{Title: "two\nlines", IdempotencyKey: "k"}); !slices.Equal(distinctCodes(diags), []string{ErrorCodeInvalidValue}) {
		t.Errorf("a multi-line title: %v", diags)
	}
	if diags := ValidateInput(struct{}{}); !diag.HasErrors(diags) {
		t.Error("an unknown document type has no validator")
	}
	if diags := ValidateResult(struct{}{}); !diag.HasErrors(diags) {
		t.Error("an unknown result type has no validator")
	}
}

// TestReconcileOperationFindsTheNamedOperation: a hint is prose for a person,
// and names the operation automation runs first.
func TestReconcileOperationFindsTheNamedOperation(t *testing.T) {
	cases := []struct {
		contract, hint, want string
	}{
		{"", "read the mission with memory.mission; repeat the checkpoint", "memory.mission"},
		{"proposals", "run proposals.find for the same change, then repeat proposals.upsert", "proposals.find"},
		{"tasks", "e.g. read it with tasks.get.", "tasks.get"},
		{"tasks", "run proposals.find, then tasks.get", "tasks.get"},
		{"tasks", "inspect the item before any retry", ""},
		{"", "run tasks.fetch", ""},
	}
	for _, tc := range cases {
		contract, operation, ok := ReconcileOperation(tc.contract, tc.hint)
		if got := contract + "." + operation; (ok && got != tc.want) || ok != (tc.want != "") {
			t.Errorf("ReconcileOperation(%q, %q) = %s, %v; want %q", tc.contract, tc.hint, got, ok, tc.want)
		}
	}
	// A Response names no contract, so any operation of the catalog is one.
	if _, diags := ParseResponse([]byte(`{"outcome":"unresolved","error":{"message":"x","reconcile":"inspect it"}}`)); !slices.Equal(distinctCodes(diags), []string{ErrorCodeInvalidValue}) {
		t.Errorf("a response whose reconcile names no operation: %v", diags)
	}
}

func TestEnvelopeValidationCoversCapabilitiesAndVersions(t *testing.T) {
	ok := json.RawMessage(`{"task":{"ref":{"source":"local:x","id":"T-1"},"revision":"r1","title":"t","state":"open"}}`)
	cases := []struct {
		name     string
		envelope Envelope
		codes    []string
	}{
		{"ok without version", Envelope{Contract: ContractTasks, Operation: OperationGet, Outcome: OutcomeOK, Result: ok}, []string{ErrorCodeRequired}},
		{"unknown contract", Envelope{Contract: "issues", Operation: OperationGet, Outcome: OutcomeNotFound, Error: &Error{Message: "x"}}, []string{ErrorCodeUnknownContract}},
		{"missing operation", Envelope{Contract: ContractTasks, Outcome: OutcomeNotFound, Error: &Error{Message: "x"}}, []string{ErrorCodeRequired}},
		{"result on failure", Envelope{Contract: ContractTasks, Version: 1, Operation: OperationGet, Outcome: OutcomeNotFound, Result: ok, Error: &Error{Message: "x"}}, []string{ErrorCodeInvalidOutcome}},
		{"current outside a conflict", Envelope{Contract: ContractTasks, Version: 1, Operation: OperationGet, Outcome: OutcomeNotFound, Error: &Error{Message: "x", Current: "r1"}}, []string{ErrorCodeInvalidOutcome}},
		{"reconcile outside unresolved", Envelope{Contract: ContractTasks, Version: 1, Operation: OperationGet, Outcome: OutcomeUnavailable, Error: &Error{Message: "x", Reconcile: "tasks.get"}}, []string{ErrorCodeInvalidOutcome}},
		{"reconcile naming no operation", Envelope{Contract: ContractTasks, Version: 1, Operation: OperationCreate, Outcome: OutcomeUnresolved, Error: &Error{Message: "x", Reconcile: "inspect the item before any retry"}}, []string{ErrorCodeInvalidValue}},
		{"reconcile naming another contract", Envelope{Contract: ContractTasks, Version: 1, Operation: OperationCreate, Outcome: OutcomeUnresolved, Error: &Error{Message: "x", Reconcile: "run proposals.find"}}, []string{ErrorCodeInvalidValue}},
		{"reconcile naming an unknown operation", Envelope{Contract: ContractTasks, Version: 1, Operation: OperationCreate, Outcome: OutcomeUnresolved, Error: &Error{Message: "x", Reconcile: "run tasks.fetch"}}, []string{ErrorCodeInvalidValue}},
		{"reconcile naming the read", Envelope{Contract: ContractTasks, Version: 1, Operation: OperationCreate, Outcome: OutcomeUnresolved, Error: &Error{Message: "x", Reconcile: "Repeat tasks.create with the same idempotencyKey."}}, nil},
		{"error missing", Envelope{Contract: ContractTasks, Version: 1, Operation: OperationGet, Outcome: OutcomeDenied}, []string{ErrorCodeInvalidOutcome}},
		{"invalid capabilities", Envelope{Contract: ContractTasks, Operation: OperationCapabilities, Outcome: OutcomeOK,
			Result: json.RawMessage(`{"contract":"tasks","supportedVersions":[1],"status":"invalid","operations":[]}`)}, []string{ErrorCodeRequired}},
		{"bound capabilities without provider", Envelope{Contract: ContractTasks, Operation: OperationCapabilities, Outcome: OutcomeOK,
			Result: json.RawMessage(`{"contract":"tasks","supportedVersions":[1],"status":"bound","operations":[{"name":"find","access":"write","required":true,"supported":true,"preconditions":"sometimes"}]}`)},
			[]string{ErrorCodeInvalidValue, ErrorCodeRequired}},
		{"negative version", Envelope{Contract: ContractTasks, Version: -1, Operation: OperationGet, Outcome: OutcomeNotFound, Error: &Error{Message: "x"}}, []string{ErrorCodeInvalidValue}},
		{"nameless provider", Envelope{Contract: ContractTasks, Version: 1, Operation: OperationGet, Provider: &ProviderIdentity{}, Outcome: OutcomeNotFound, Error: &Error{Message: "x"}}, []string{ErrorCodeRequired}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := distinctCodes(ValidateEnvelope(&tc.envelope)); !slices.Equal(got, tc.codes) {
				t.Fatalf("codes = %v, want %v", got, tc.codes)
			}
		})
	}
}
