package collaboration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	extension "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

func boolPtr(v bool) *bool { return &v }

// providerOperation declares one operation with the precondition rule and the
// workspace view the catalog asks of it.
func providerOperation(contract, operation string) extension.ProviderOperation {
	spec, _ := Lookup(contract, 1)
	op, _ := spec.Operation(operation)
	declared := extension.ProviderOperation{
		Description:        "Local " + contract + "." + operation,
		OpenWorld:          boolPtr(false),
		WorkspaceSelection: op.WorkspaceSelection,
	}
	switch op.Preconditions {
	case PreconditionRuleDeclared:
		declared.Preconditions = string(PreconditionsChecked)
	case PreconditionRuleAtomic:
		declared.Preconditions = string(PreconditionsAtomic)
	}
	return declared
}

// fullProvider declares every required operation of one contract version.
func fullProvider(contract string) extension.ProviderDeclaration {
	spec, _ := Lookup(contract, 1)
	operations := map[string]extension.ProviderOperation{}
	for _, name := range spec.RequiredOperations() {
		operations[name] = providerOperation(contract, name)
	}
	return extension.ProviderDeclaration{Version: 1, Command: "provider", Args: []string{"provider"}, Operations: operations}
}

func TestReadProviderOffer_AcceptsACompleteDeclaration(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "truthful-provider-declarations", "a-complete-declaration-is-offered")
	tasks := fullProvider(ContractTasks)
	tasks.Operations[OperationClaim] = providerOperation(ContractTasks, OperationClaim)
	providers := map[string]extension.ProviderDeclaration{
		ContractTasks:     tasks,
		ContractProposals: fullProvider(ContractProposals),
	}

	offer, diags := ReadProviderOffer(providers)
	if diag.HasErrors(diags) {
		t.Fatalf("a complete declaration was refused: %v", diags)
	}
	if got := offer.Versions(ContractTasks); !slices.Equal(got, []int{1}) {
		t.Errorf("tasks versions = %v", got)
	}
	ops, ok := offer.Operations(ContractTasks, 1)
	if !ok || len(ops) != 6 {
		t.Fatalf("tasks v1 operations = %v", ops)
	}
	claim := ops[OperationClaim]
	if claim.Spec.Name != OperationClaim || claim.Preconditions() != PreconditionsAtomic || claim.Provider.Command != "provider" {
		t.Errorf("tasks.claim = %+v", claim)
	}
	if _, ok := offer.Operations(ContractMemory, 1); ok {
		t.Error("memory is not offered")
	}
}

func TestReadProviderOffer_WithdrawsAContractVersionOnAnyError(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "truthful-provider-declarations", "any-declaration-error-withdraws-the-contract-version")
	cases := []struct {
		name   string
		mutate func(*extension.ProviderDeclaration)
		want   string
	}{
		{"a missing required operation", func(p *extension.ProviderDeclaration) {
			delete(p.Operations, OperationTransition)
		}, "without its required operations: transition"},
		{"an undeclared precondition", func(p *extension.ProviderDeclaration) {
			op := p.Operations[OperationUpdate]
			op.Preconditions = ""
			p.Operations[OperationUpdate] = op
		}, "must declare preconditions"},
		{"a precondition on a read", func(p *extension.ProviderDeclaration) {
			op := p.Operations[OperationGet]
			op.Preconditions = string(PreconditionsAtomic)
			p.Operations[OperationGet] = op
		}, "takes no revision precondition"},
		{"a non-atomic claim", func(p *extension.ProviderDeclaration) {
			op := providerOperation(ContractTasks, OperationClaim)
			op.Preconditions = string(PreconditionsChecked)
			p.Operations[OperationClaim] = op
		}, "enforces it atomically"},
		{"an unknown operation", func(p *extension.ProviderDeclaration) {
			p.Operations["merge"] = providerOperation(ContractTasks, OperationFind)
		}, "has no operation \"merge\""},
		{"the orchestrator's own operation", func(p *extension.ProviderDeclaration) {
			p.Operations[OperationCapabilities] = providerOperation(ContractTasks, OperationFind)
		}, "has no operation \"capabilities\""},
		{"a declaration without a command", func(p *extension.ProviderDeclaration) {
			p.Command = " "
		}, "names the command its operations run"},
		{"a versionless declaration", func(p *extension.ProviderDeclaration) {
			p.Version = 0
		}, "names its contract version"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := fullProvider(ContractTasks)
			tc.mutate(&provider)
			offer, diags := ReadProviderOffer(map[string]extension.ProviderDeclaration{ContractTasks: provider})
			if !diag.HasErrors(diags) {
				t.Fatal("the declaration was accepted")
			}
			if !strings.Contains(FormatDiagnostics(diags), tc.want) {
				t.Errorf("diagnostics %q do not say %q", FormatDiagnostics(diags), tc.want)
			}
			if _, offered := offer.Operations(ContractTasks, 1); offered {
				t.Error("tasks v1 stayed in the offer despite the error")
			}
		})
	}

	// A contract this package does not know is refused, and withdraws only
	// itself.
	offer, diags := ReadProviderOffer(map[string]extension.ProviderDeclaration{
		ContractTasks: fullProvider(ContractTasks),
		"chores":      fullProvider(ContractTasks),
	})
	if !strings.Contains(FormatDiagnostics(diags), "unknown contract \"chores\"") {
		t.Errorf("diagnostics = %v", diags)
	}
	if _, offered := offer.Operations(ContractTasks, 1); !offered {
		t.Error("an unknown contract withdrew tasks v1")
	}
}

func TestReadProviderOffer_SelectionOperationsRequireTheWorkspaceView(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "truthful-provider-declarations", "selection-operations-declare-the-workspace-view")
	memory := fullProvider(ContractMemory)
	op := memory.Operations[OperationContext]
	op.WorkspaceSelection = false
	memory.Operations[OperationContext] = op
	_, diags := ReadProviderOffer(map[string]extension.ProviderDeclaration{ContractMemory: memory})
	if !strings.Contains(FormatDiagnostics(diags), "must declare workspaceSelection") {
		t.Fatalf("diagnostics = %v", diags)
	}
}

func TestReadProviderOffer_IgnoresAVersionItDoesNotSpeak(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "truthful-provider-declarations", "a-version-nobody-speaks-draws-one-warning")
	future := fullProvider(ContractTasks)
	future.Version = 2
	offer, diags := ReadProviderOffer(map[string]extension.ProviderDeclaration{
		ContractTasks:     future,
		ContractProposals: fullProvider(ContractProposals),
	})
	if diag.HasErrors(diags) {
		t.Fatalf("a version nobody speaks must not fail another contract: %v", diags)
	}
	if len(diags) != 1 || diags[0].Severity != diag.Warning {
		t.Fatalf("want one warning for the unknown version, got %v", diags)
	}
	if got := offer.Versions(ContractTasks); len(got) != 0 {
		t.Errorf("tasks versions = %v, want none", got)
	}
	if got := offer.Versions(ContractProposals); !slices.Equal(got, []int{1}) {
		t.Errorf("proposals versions = %v", got)
	}
}

// serveCall runs Serve on one request and decodes the Response.
func serveCall(t *testing.T, request ProviderRequest, handlers map[OperationKey]Handler) Response {
	t.Helper()
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Serve(context.Background(), bytes.NewReader(payload), &out, handlers); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	// The provider's standard output is one Response document and nothing
	// else: ParseResponse refuses trailing data.
	response, diags := ParseResponse(out.Bytes())
	if diags != nil {
		t.Fatalf("the provider wrote an invalid response %s: %v", out.String(), diags)
	}
	return *response
}

func routed(contract, operation, arguments string) ProviderRequest {
	return ProviderRequest{
		Arguments: json.RawMessage(arguments),
		Provider:  &ProviderCall{Contract: contract, Version: 1, Operation: operation, Settings: json.RawMessage(`{"root":"x"}`)},
	}
}

func TestServe_RunsTheHandlerOnAValidatedRequest(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "serve-validates-both-sides", "serve-runs-the-handler-on-a-validated-request")
	var seen Call
	handlers := map[OperationKey]Handler{
		{ContractTasks, 1, OperationGet}: func(_ context.Context, call Call) (any, *Failure) {
			seen = call
			input := call.Input.(*TaskRefInput)
			return &TaskResult{Task: Task{Ref: input.Ref, Revision: "r1", Title: "t", State: TaskStateOpen}}, nil
		},
	}
	response := serveCall(t, routed(ContractTasks, OperationGet, `{"ref":{"source":"local:x","id":"T-1"}}`), handlers)
	if response.Outcome != OutcomeOK {
		t.Fatalf("outcome = %s: %+v", response.Outcome, response.Error)
	}
	if string(seen.Settings) != `{"root":"x"}` || seen.Request.Provider == nil || seen.Request.Provider.Operation != OperationGet {
		t.Errorf("the handler saw %+v", seen)
	}
	if seen.String() != "tasks.get@v1" {
		t.Errorf("operation key = %s", seen.String())
	}
}

func TestServe_RefusesWhatTheContractRefuses(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "serve-validates-both-sides", "serve-refuses-what-the-contract-refuses")
	calls := 0
	handlers := map[OperationKey]Handler{
		{ContractTasks, 1, OperationCreate}: func(context.Context, Call) (any, *Failure) {
			calls++
			return nil, nil
		},
	}
	cases := []struct {
		name    string
		request ProviderRequest
		outcome Outcome
	}{
		{"a direct call", ProviderRequest{Arguments: json.RawMessage(`{}`)}, OutcomeInvalid},
		{"an operation without handler", routed(ContractTasks, OperationFind, `{}`), OutcomeUnsupported},
		{"a request the contract refuses", routed(ContractTasks, OperationCreate, `{"title":"t"}`), OutcomeInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := serveCall(t, tc.request, handlers)
			if response.Outcome != tc.outcome {
				t.Fatalf("outcome = %s, want %s (%+v)", response.Outcome, tc.outcome, response.Error)
			}
		})
	}
	if calls != 0 {
		t.Errorf("the handler ran %d times for refused requests", calls)
	}
}

// TestServe_AnUncertainHandlerIsUnresolvedForAMutation is the provider half
// of the reconciliation rule: a handler whose effect is unknown never reports
// a retryable failure for a write.
func TestServe_AnUncertainHandlerIsUnresolvedForAMutation(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "serve-validates-both-sides", "an-uncertain-handler-is-unresolved-for-a-mutation")
	panicking := func(context.Context, Call) (any, *Failure) { panic("backend exploded") }
	badResult := func(context.Context, Call) (any, *Failure) {
		return &TaskCreateResult{Task: Task{Title: "no ref"}}, nil
	}
	okFailure := func(context.Context, Call) (any, *Failure) {
		return nil, &Failure{Outcome: OutcomeOK, Error: Error{Message: "contradiction"}}
	}
	unencodable := func(context.Context, Call) (any, *Failure) { return map[string]any{"x": make(chan int)}, nil }
	for name, handler := range map[string]Handler{
		"panic": panicking, "invalid result": badResult, "ok failure": okFailure, "unencodable": unencodable,
	} {
		t.Run(name, func(t *testing.T) {
			handlers := map[OperationKey]Handler{
				{ContractTasks, 1, OperationCreate}: handler,
				{ContractTasks, 1, OperationFind}:   handler,
			}
			write := serveCall(t, routed(ContractTasks, OperationCreate, `{"title":"t","idempotencyKey":"k1"}`), handlers)
			if write.Outcome != OutcomeUnresolved || write.Error.Retryable {
				t.Errorf("mutation outcome = %s retryable=%v, want unresolved and not retryable", write.Outcome, write.Error.Retryable)
			}
			read := serveCall(t, routed(ContractTasks, OperationFind, `{}`), handlers)
			if read.Outcome != OutcomeUnavailable {
				t.Errorf("read outcome = %s, want unavailable", read.Outcome)
			}
		})
	}
}

func TestServe_AFailureKeepsItsOutcomeAndCannotRetryUnresolved(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "serve-validates-both-sides", "a-handler-failure-keeps-its-outcome")
	handlers := map[OperationKey]Handler{
		{ContractTasks, 1, OperationGet}: func(context.Context, Call) (any, *Failure) {
			return nil, Fail(OutcomeNotFound, "task.missing", "no task %s", "T-9")
		},
		{ContractTasks, 1, OperationCreate}: func(context.Context, Call) (any, *Failure) {
			failure := Fail(OutcomeUnresolved, ReasonProviderTimeout, "the backend timed out")
			failure.Error.Retryable = true
			return nil, failure
		},
	}
	response := serveCall(t, routed(ContractTasks, OperationGet, `{"ref":{"source":"local:x","id":"T-9"}}`), handlers)
	if response.Outcome != OutcomeNotFound || response.Error.Message != "no task T-9" || response.Error.Reason != "task.missing" {
		t.Errorf("response = %+v %+v", response, response.Error)
	}
	response = serveCall(t, routed(ContractTasks, OperationCreate, `{"title":"t","idempotencyKey":"k"}`), handlers)
	if response.Outcome != OutcomeUnresolved || response.Error.Retryable {
		t.Errorf("an unresolved failure became retryable: %+v", response.Error)
	}
}

func TestServe_ANilPageIsTheEmptyList(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "serve-validates-both-sides", "a-nil-page-is-the-empty-list")
	handlers := map[OperationKey]Handler{
		{ContractTasks, 1, OperationFind}: func(context.Context, Call) (any, *Failure) { return &TaskListResult{}, nil },
		{ContractProposals, 1, OperationFind}: func(context.Context, Call) (any, *Failure) {
			return &ProposalListResult{}, nil
		},
		{ContractMemory, 1, OperationSearch}: func(context.Context, Call) (any, *Failure) { return &MemoryListResult{}, nil },
		{ContractProposals, 1, OperationComments}: func(context.Context, Call) (any, *Failure) {
			return &ProposalCommentListResult{}, nil
		},
	}
	for _, request := range []ProviderRequest{
		routed(ContractTasks, OperationFind, `{}`),
		routed(ContractProposals, OperationFind, `{"change":{"base":"main","head":"h"}}`),
		routed(ContractProposals, OperationComments, `{"ref":{"source":"local:x","id":"P-1"}}`),
		routed(ContractMemory, OperationSearch, `{"query":"q"}`),
	} {
		response := serveCall(t, request, handlers)
		if response.Outcome != OutcomeOK || !strings.Contains(string(response.Result), `"items":[]`) {
			t.Errorf("%s.%s: %s %s", request.Provider.Contract, request.Provider.Operation, response.Outcome, response.Result)
		}
	}
}

func TestServe_RejectsAnUndecodableRequest(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "serve-validates-both-sides", "an-undecodable-request-writes-nothing")
	var out bytes.Buffer
	err := Serve(context.Background(), strings.NewReader("not json"), &out, nil)
	if err == nil || out.Len() != 0 {
		t.Fatalf("err = %v, out = %q", err, out.String())
	}
	if err := writeResponse(failingWriter{}, Response{Outcome: OutcomeOK, Result: json.RawMessage(`{}`)}); err == nil {
		t.Error("a write failure must surface")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }
