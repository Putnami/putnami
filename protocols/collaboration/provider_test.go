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
)

func boolPtr(v bool) *bool { return &v }

// providerTool builds a tool that implements one operation with annotations
// that agree with the catalog.
func providerTool(contract, operation string) extension.ToolDefinition {
	const version = 1
	spec, _ := Lookup(contract, version)
	op, _ := spec.Operation(operation)
	readOnly := op.Access == AccessRead
	declaration := map[string]any{"contract": contract, "version": version, "operation": operation}
	switch op.Preconditions {
	case PreconditionRuleDeclared:
		declaration["preconditions"] = string(PreconditionsChecked)
	case PreconditionRuleAtomic:
		declaration["preconditions"] = string(PreconditionsAtomic)
	}
	return extension.ToolDefinition{
		Description: "Local " + contract + "." + operation,
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Annotations: &extension.ToolAnnotations{
			ReadOnlyHint:    boolPtr(readOnly),
			DestructiveHint: boolPtr(op.Destructive),
			IdempotentHint:  boolPtr(readOnly),
			OpenWorldHint:   boolPtr(false),
		},
		Meta: map[string]any{
			contractMetaKey: map[string]any{"access": string(op.Access), "readOnly": readOnly, "supportsDryRun": false},
			ProviderMetaKey: declaration,
		},
		Command:            "provider",
		WorkspaceSelection: op.WorkspaceSelection,
	}
}

// fullProvider declares every required operation of one contract version.
func fullProvider(contract string) map[string]extension.ToolDefinition {
	spec, _ := Lookup(contract, 1)
	tools := map[string]extension.ToolDefinition{}
	for _, name := range spec.RequiredOperations() {
		tools["local."+contract+"."+name] = providerTool(contract, name)
	}
	return tools
}

func TestReadProviderOffer_AcceptsACompleteDeclaration(t *testing.T) {
	tools := fullProvider(ContractTasks)
	tools["local.tasks.claim"] = providerTool(ContractTasks, OperationClaim)
	for name, def := range fullProvider(ContractProposals) {
		tools[name] = def
	}
	tools["plain.tool"] = extension.ToolDefinition{Description: "not a provider tool"}

	offer, diags := ReadProviderOffer(tools)
	if diag.HasErrors(diags) {
		t.Fatalf("a complete declaration was refused: %v", diags)
	}
	if got := offer.Versions(ContractTasks); !slices.Equal(got, []int{1}) {
		t.Errorf("tasks versions = %v", got)
	}
	ops, ok := offer.Operations(ContractTasks, 1)
	if !ok || len(ops) != 6 || ops[OperationClaim].Tool != "local.tasks.claim" {
		t.Errorf("tasks v1 operations = %v", ops)
	}
	if _, ok := offer.Operations(ContractMemory, 1); ok {
		t.Error("memory is not offered")
	}
}

func TestReadProviderOffer_WithdrawsAContractVersionOnAnyError(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]extension.ToolDefinition)
		want   string
	}{
		{"a missing required operation", func(tools map[string]extension.ToolDefinition) {
			delete(tools, "local.tasks.transition")
		}, "without its required operations: transition"},
		{"a read annotated as mutating", func(tools map[string]extension.ToolDefinition) {
			def := tools["local.tasks.find"]
			def.Annotations.ReadOnlyHint = boolPtr(false)
			tools["local.tasks.find"] = def
		}, "readOnlyHint must be true"},
		{"a mutation annotated read-only", func(tools map[string]extension.ToolDefinition) {
			def := tools["local.tasks.create"]
			def.Annotations.ReadOnlyHint = boolPtr(true)
			tools["local.tasks.create"] = def
		}, "readOnlyHint must be false"},
		{"a destructive operation annotated additive", func(tools map[string]extension.ToolDefinition) {
			def := tools["local.tasks.update"]
			def.Annotations.DestructiveHint = boolPtr(false)
			tools["local.tasks.update"] = def
		}, "destructiveHint must be true"},
		{"a destructive read", func(tools map[string]extension.ToolDefinition) {
			def := tools["local.tasks.get"]
			def.Annotations.DestructiveHint = boolPtr(true)
			tools["local.tasks.get"] = def
		}, "never destructive"},
		{"contract metadata that disagrees", func(tools map[string]extension.ToolDefinition) {
			def := tools["local.tasks.get"]
			def.Meta = cloneMeta(def.Meta)
			def.Meta[contractMetaKey] = map[string]any{"access": "mutating", "readOnly": false, "supportsDryRun": false}
			tools["local.tasks.get"] = def
		}, "declares access \"mutating\""},
		{"a dry-run claim", func(tools map[string]extension.ToolDefinition) {
			def := tools["local.tasks.get"]
			def.Meta = cloneMeta(def.Meta)
			def.Meta[contractMetaKey] = map[string]any{"access": "read", "readOnly": true, "supportsDryRun": true}
			tools["local.tasks.get"] = def
		}, "no dry-run"},
		{"an undeclared precondition", func(tools map[string]extension.ToolDefinition) {
			def := tools["local.tasks.update"]
			def.Meta = cloneMeta(def.Meta)
			def.Meta[ProviderMetaKey] = map[string]any{"contract": "tasks", "version": 1, "operation": "update"}
			tools["local.tasks.update"] = def
		}, "must declare preconditions"},
		{"a precondition on a read", func(tools map[string]extension.ToolDefinition) {
			def := tools["local.tasks.get"]
			def.Meta = cloneMeta(def.Meta)
			def.Meta[ProviderMetaKey] = map[string]any{"contract": "tasks", "version": 1, "operation": "get", "preconditions": "atomic"}
			tools["local.tasks.get"] = def
		}, "takes no revision precondition"},
		{"a non-atomic claim", func(tools map[string]extension.ToolDefinition) {
			def := providerTool(ContractTasks, OperationClaim)
			def.Meta = cloneMeta(def.Meta)
			def.Meta[ProviderMetaKey] = map[string]any{"contract": "tasks", "version": 1, "operation": "claim", "preconditions": "checked"}
			tools["local.tasks.claim"] = def
		}, "enforces it atomically"},
		{"two tools for one operation", func(tools map[string]extension.ToolDefinition) {
			tools["other.tasks.find"] = providerTool(ContractTasks, OperationFind)
		}, "the choice would be ambiguous"},
		{"an unknown operation", func(tools map[string]extension.ToolDefinition) {
			def := providerTool(ContractTasks, OperationFind)
			def.Meta = cloneMeta(def.Meta)
			def.Meta[ProviderMetaKey] = map[string]any{"contract": "tasks", "version": 1, "operation": "merge"}
			tools["local.tasks.merge"] = def
		}, "has no operation \"merge\""},
		{"missing annotations", func(tools map[string]extension.ToolDefinition) {
			def := tools["local.tasks.get"]
			def.Annotations = nil
			tools["local.tasks.get"] = def
		}, "all four safety annotations"},
		{"a malformed declaration", func(tools map[string]extension.ToolDefinition) {
			def := tools["local.tasks.get"]
			def.Meta = cloneMeta(def.Meta)
			def.Meta[ProviderMetaKey] = map[string]any{"contract": "tasks", "version": 1, "operation": "get", "fallback": true}
			tools["local.tasks.get"] = def
		}, "unknown member"},
		{"a versionless declaration", func(tools map[string]extension.ToolDefinition) {
			def := tools["local.tasks.get"]
			def.Meta = cloneMeta(def.Meta)
			def.Meta[ProviderMetaKey] = map[string]any{"contract": "tasks", "operation": "get"}
			tools["local.tasks.get"] = def
		}, "names its contract version"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tools := fullProvider(ContractTasks)
			for name, def := range tools {
				annotations := *def.Annotations
				def.Annotations = &annotations
				tools[name] = def
			}
			tc.mutate(tools)
			offer, diags := ReadProviderOffer(tools)
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
}

func TestReadProviderOffer_SelectionOperationsRequireTheWorkspaceView(t *testing.T) {
	tools := fullProvider(ContractMemory)
	def := tools["local.memory.context"]
	def.WorkspaceSelection = false
	tools["local.memory.context"] = def
	_, diags := ReadProviderOffer(tools)
	if !strings.Contains(FormatDiagnostics(diags), "must declare workspaceSelection") {
		t.Fatalf("diagnostics = %v", diags)
	}
}

func TestReadProviderOffer_IgnoresAVersionItDoesNotSpeak(t *testing.T) {
	tools := fullProvider(ContractTasks)
	future := providerTool(ContractTasks, OperationFind)
	future.Meta = cloneMeta(future.Meta)
	future.Meta[ProviderMetaKey] = map[string]any{"contract": "tasks", "version": 2, "operation": "find"}
	tools["local.tasks.find.v2"] = future
	offer, diags := ReadProviderOffer(tools)
	if diag.HasErrors(diags) {
		t.Fatalf("a newer version beside v1 must not fail v1: %v", diags)
	}
	if len(diags) != 1 || diags[0].Severity != diag.Warning {
		t.Fatalf("want one warning for the unknown version, got %v", diags)
	}
	if got := offer.Versions(ContractTasks); !slices.Equal(got, []int{1}) {
		t.Errorf("versions = %v", got)
	}
}

func cloneMeta(meta map[string]any) map[string]any {
	out := make(map[string]any, len(meta))
	for key, value := range meta {
		out[key] = value
	}
	return out
}

// serveCall runs Serve on one request and decodes the Response.
func serveCall(t *testing.T, request extension.ToolCallRequest, handlers map[OperationKey]Handler) Response {
	t.Helper()
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Serve(context.Background(), bytes.NewReader(payload), &out, handlers); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var result extension.ToolCallResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode tool result %s: %v", out.String(), err)
	}
	if len(result.Content) != 1 {
		t.Fatalf("want one content block, got %v", result.Content)
	}
	response, diags := ParseResponse([]byte(result.Content[0].Text))
	if diags != nil {
		t.Fatalf("the provider wrote an invalid response %s: %v", result.Content[0].Text, diags)
	}
	if result.IsError != (response.Outcome != OutcomeOK) {
		t.Errorf("isError %v disagrees with outcome %s", result.IsError, response.Outcome)
	}
	return *response
}

func routed(contract, operation, arguments string) extension.ToolCallRequest {
	return extension.ToolCallRequest{
		Name:      "local." + contract + "." + operation,
		Arguments: json.RawMessage(arguments),
		Provider:  &extension.ToolProviderCall{Contract: contract, Version: 1, Operation: operation, Settings: json.RawMessage(`{"root":"x"}`)},
	}
}

func TestServe_RunsTheHandlerOnAValidatedRequest(t *testing.T) {
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
	if string(seen.Settings) != `{"root":"x"}` || seen.Request.Name != "local.tasks.get" {
		t.Errorf("the handler saw %+v", seen)
	}
	if seen.String() != "tasks.get@v1" {
		t.Errorf("operation key = %s", seen.String())
	}
}

func TestServe_RefusesWhatTheContractRefuses(t *testing.T) {
	calls := 0
	handlers := map[OperationKey]Handler{
		{ContractTasks, 1, OperationCreate}: func(context.Context, Call) (any, *Failure) {
			calls++
			return nil, nil
		},
	}
	cases := []struct {
		name    string
		request extension.ToolCallRequest
		outcome Outcome
	}{
		{"a direct call", extension.ToolCallRequest{Name: "local.tasks.create", Arguments: json.RawMessage(`{}`)}, OutcomeInvalid},
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
	handlers := map[OperationKey]Handler{
		{ContractTasks, 1, OperationFind}: func(context.Context, Call) (any, *Failure) { return &TaskListResult{}, nil },
		{ContractProposals, 1, OperationFind}: func(context.Context, Call) (any, *Failure) {
			return &ProposalListResult{}, nil
		},
		{ContractMemory, 1, OperationSearch}: func(context.Context, Call) (any, *Failure) { return &MemoryListResult{}, nil },
	}
	for _, request := range []extension.ToolCallRequest{
		routed(ContractTasks, OperationFind, `{}`),
		routed(ContractProposals, OperationFind, `{"change":{"base":"main","head":"h"}}`),
		routed(ContractMemory, OperationSearch, `{"query":"q"}`),
	} {
		response := serveCall(t, request, handlers)
		if response.Outcome != OutcomeOK || !strings.Contains(string(response.Result), `"items":[]`) {
			t.Errorf("%s: %s %s", request.Name, response.Outcome, response.Result)
		}
	}
}

func TestServe_RejectsAnUndecodableRequest(t *testing.T) {
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
