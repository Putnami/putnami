package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
)

func writeCollabWorkspace(t *testing.T, document string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "putnami.workspace.json"), []byte(document), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func readCollabFixture(t *testing.T, document string) collabDocument {
	t.Helper()
	read, err := readCollabDocument(writeCollabWorkspace(t, document))
	if err != nil {
		t.Fatal(err)
	}
	return read
}

func TestReadDocument_TheWorkspaceDocumentIsTheOnlySource(t *testing.T) {
	absent, err := readCollabDocument(t.TempDir())
	if err != nil || absent.Present {
		t.Fatalf("a workspace without a document: %+v %v", absent, err)
	}
	if empty, _ := readCollabDocument(""); empty.Present {
		t.Error("no root has no block")
	}
	for _, document := range []string{`{"name":"w"}`, `{"options":{"test":{"coverage":true}}}`, `not json`, `[]`} {
		if read := readCollabFixture(t, document); read.Present {
			t.Errorf("%s declares no block, read %+v", document, read)
		}
	}
	read := readCollabFixture(t, `{"name":"acme","options":{"collaboration":{"tasks":{"provider":"@acme/github","version":1}}}}`)
	if !read.Present || read.Name != "acme" || !read.bound("tasks") || read.bound("memory") || read.Bindings["tasks"].Provider != "@acme/github" {
		t.Fatalf("read %+v", read)
	}
}

func TestReadDocument_AnAmbiguousBlockRefusesEveryContract(t *testing.T) {
	for name, document := range map[string]string{
		"options twice":       `{"options":{"collaboration":{}},"options":{"collaboration":{"tasks":{"provider":"a","version":1}}}}`,
		"collaboration twice": `{"options":{"collaboration":{},"collaboration":{"tasks":{"provider":"a","version":1}}}}`,
		"a contract twice":    `{"options":{"collaboration":{"tasks":{"provider":"a","version":1},"tasks":{"provider":"b","version":1}}}}`,
		"not an object":       `{"options":{"collaboration":[]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			read := readCollabFixture(t, document)
			if !read.Present || len(read.Issues[""]) == 0 {
				t.Fatalf("read %+v", read)
			}
			for _, contract := range collab.ContractNames {
				resolution := resolveCollab(read, contract, nil, nil)
				if resolution.Status != collab.BindingStatusInvalid {
					t.Errorf("%s status %s under an ambiguous block", contract, resolution.Status)
				}
			}
		})
	}
}

func TestReadDocument_AnUnknownContractIsReportedAndRefusesNone(t *testing.T) {
	read := readCollabFixture(t, `{"options":{"collaboration":{"task":{"provider":"a","version":1}}}}`)
	if len(read.Unknown) != 1 || len(read.Issues) != 0 {
		t.Fatalf("read %+v", read)
	}
	resolution := resolveCollab(read, "tasks", nil, nil)
	if resolution.Status != collab.BindingStatusUnbound || len(resolution.Issues) != 1 ||
		!strings.Contains(resolution.Issues[0].Message, `unknown contract "task"`) {
		t.Fatalf("resolution %+v", resolution)
	}
}

func TestReadDocument_AContractIssueStaysWithItsContract(t *testing.T) {
	read := readCollabFixture(t, `{"options":{"collaboration":{"tasks":{"provider":"a","version":1},"memory":{"provider":"b","version":3}}}}`)
	if len(read.Issues["memory"]) != 1 || read.Issues["memory"][0].Reason != collab.ReasonBindingVersion || len(read.Issues["tasks"]) != 0 {
		t.Fatalf("issues %+v", read.Issues)
	}
	if got := read.issuesFor("memory"); len(got) != 1 {
		t.Errorf("IssuesFor = %+v", got)
	}
	if resolution := resolveCollab(read, "memory", nil, nil); resolution.Status != collab.BindingStatusInvalid || resolution.Binding != nil {
		t.Errorf("memory %+v", resolution)
	}
}

func TestReadDocument_AnUnreadableDocumentIsAnError(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "putnami.workspace.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := readCollabDocument(root); err == nil {
		t.Fatal("a document that cannot be read must be an error, never an empty binding")
	}
}

// collabToolDefinition decodes one provider tool from the manifest JSON an
// extension author writes, with annotations that agree with the catalog.
func collabToolDefinition(t *testing.T, contract, operation string, preconditions collab.Preconditions, command string) proto.ToolDefinition {
	t.Helper()
	spec, _ := collab.Lookup(contract, 1)
	op, ok := spec.Operation(operation)
	if !ok {
		t.Fatalf("no operation %s.%s", contract, operation)
	}
	readOnly := op.Access == collab.AccessRead
	declaration := fmt.Sprintf(`{"contract":%q,"version":1,"operation":%q`, contract, operation)
	if preconditions != "" {
		declaration += fmt.Sprintf(`,"preconditions":%q`, preconditions)
	}
	document := fmt.Sprintf(`{"description":%q,"inputSchema":{"type":"object"},`+
		`"annotations":{"readOnlyHint":%t,"destructiveHint":%t,"idempotentHint":%t,"openWorldHint":false},`+
		`"_meta":{"putnami.dev/contract":{"access":%q,"readOnly":%t,"supportsDryRun":false},"putnami.dev/provider":%s}},`+
		`"command":%q,"timeoutMs":20000,"workspaceSelection":%t}`,
		"Fixture "+contract+"."+operation+".", readOnly, op.Destructive, readOnly, op.Access, readOnly, declaration,
		command, op.WorkspaceSelection)
	var def proto.ToolDefinition
	if err := json.Unmarshal([]byte(document), &def); err != nil {
		t.Fatalf("decode %s: %v", document, err)
	}
	return def
}

// jsonValue decodes a JSON literal into the untyped value a manifest's _meta
// holds.
func jsonValue(t *testing.T, text string) any {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func collabProviderTools(t *testing.T, contract string, extra ...string) map[string]proto.ToolDefinition {
	t.Helper()
	spec, _ := collab.Lookup(contract, 1)
	tools := map[string]proto.ToolDefinition{}
	add := func(op collab.OperationSpec) {
		var preconditions collab.Preconditions
		switch op.Preconditions {
		case collab.PreconditionRuleDeclared:
			preconditions = collab.PreconditionsChecked
		case collab.PreconditionRuleAtomic:
			preconditions = collab.PreconditionsAtomic
		}
		tools["p."+contract+"."+op.Name] = collabToolDefinition(t, contract, op.Name, preconditions, "provider")
	}
	for _, op := range spec.Operations {
		if op.Required {
			add(op)
		}
	}
	for _, name := range extra {
		if op, ok := spec.Operation(name); ok {
			add(op)
		}
	}
	return tools
}

func provider(name string, tools map[string]proto.ToolDefinition) *extension.ExtensionDescription {
	return &extension.ExtensionDescription{Name: name, Version: "1.0.0", Path: "/ext/" + name, Tools: tools}
}

func TestResolve_BindsOnlyTheNamedAvailableProvider(t *testing.T) {
	read := readCollabFixture(t, `{"options":{"collaboration":{"tasks":{"provider":"@a/one","version":1}}}}`)
	one := provider("@a/one", collabProviderTools(t, "tasks", "claim"))
	two := provider("@a/two", collabProviderTools(t, "tasks"))
	resolution := resolveCollab(read, "tasks", []*extension.ExtensionDescription{two, one, nil}, nil)
	if resolution.Status != collab.BindingStatusBound || resolution.Provider != one {
		t.Fatalf("resolution %+v", resolution)
	}
	if len(resolution.Candidates) != 2 {
		t.Errorf("candidates %+v", resolution.Candidates)
	}
	capabilities := capabilitiesOf(resolution)
	if capabilities.Provider.Name != "@a/one" || len(capabilities.Operations) != 8 {
		t.Errorf("capabilities %+v", capabilities)
	}
	if ops := routedOperations(resolution); len(ops) != 6 {
		t.Errorf("routed %d operations, want the 5 required and claim", len(ops))
	}

	duplicate := resolveCollab(read, "tasks", []*extension.ExtensionDescription{one, provider("@a/one", nil)}, nil)
	if duplicate.Status != collab.BindingStatusInvalid || duplicate.Issues[0].Reason != collab.ReasonBindingAmbiguous {
		t.Errorf("two extensions carrying the bound name: %+v", duplicate)
	}
	disabled := resolveCollab(read, "tasks", []*extension.ExtensionDescription{one}, map[string]bool{"@a/one": true})
	if disabled.Status != collab.BindingStatusInvalid || disabled.Issues[0].Reason != collab.ReasonBindingProviderMissing {
		t.Errorf("a disabled provider: %+v", disabled)
	}
	versions := provider("@a/one", collabProviderTools(t, "memory"))
	wrong := resolveCollab(read, "tasks", []*extension.ExtensionDescription{versions}, nil)
	if wrong.Status != collab.BindingStatusInvalid || !strings.Contains(wrong.Issues[0].Message, "does not implement the tasks contract") {
		t.Errorf("a provider without the contract: %+v", wrong)
	}
}

func TestRoutedOperations_ABrokenBindingKeepsWhatItPromised(t *testing.T) {
	read := readCollabFixture(t, `{"options":{"collaboration":{"tasks":{"provider":"@a/absent","version":1,"require":["claim"]}}}}`)
	resolution := resolveCollab(read, "tasks", nil, nil)
	var names []string
	for _, op := range routedOperations(resolution) {
		names = append(names, op.Name)
	}
	if strings.Join(names, ",") != "claim,create,find,get,transition,update" {
		t.Errorf("routed %v", names)
	}
	if routedOperations(resolveCollab(read, "memory", nil, nil)) != nil {
		t.Error("an undeclared contract routes nothing")
	}
	unsupported := readCollabFixture(t, `{"options":{"collaboration":{"tasks":{"provider":"@a/absent","version":7}}}}`)
	if len(routedOperations(resolveCollab(unsupported, "tasks", nil, nil))) != 0 {
		t.Error("a binding that did not parse promises nothing")
	}
}

func collabBoundCall(t *testing.T, contract, operation, arguments string) *providerCall {
	t.Helper()
	read := readCollabFixture(t, `{"name":"w","options":{"collaboration":{"`+contract+`":{"provider":"@a/one","version":1}}}}`)
	resolution := resolveCollab(read, contract, []*extension.ExtensionDescription{provider("@a/one", collabProviderTools(t, contract, "claim", "merge", "search", "assign", "link"))}, nil)
	call, answer := prepareCall(read, resolution, operation, json.RawMessage(arguments))
	if answer != nil {
		t.Fatalf("prepareCall refused: %+v", answer.Error)
	}
	return call
}

func collabRespond(t *testing.T, response collab.Response) proto.ToolCallResult {
	t.Helper()
	text, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return proto.ToolCallResult{Content: []proto.ToolContent{{Type: "text", Text: string(text)}}, IsError: response.Outcome != collab.OutcomeOK}
}

func collabOK(t *testing.T, result any) proto.ToolCallResult {
	t.Helper()
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return collabRespond(t, collab.Response{Outcome: collab.OutcomeOK, Result: encoded})
}

func TestPrepare_NormalizesTheRequest(t *testing.T) {
	call := collabBoundCall(t, "memory", "checkpoint", `{"mission":"m","precondition":{"mustNotExist":true},"idempotencyKey":"k","content":"x"}`)
	if !strings.Contains(string(call.Arguments), `"workspace":"w"`) {
		t.Errorf("identity not filled: %s", call.Arguments)
	}
	provider := call.providerMember()
	if provider.Contract != "memory" || provider.Operation != "checkpoint" || provider.Version != 1 {
		t.Errorf("provider member %+v", provider)
	}
	search := collabBoundCall(t, "memory", "search", `{"query":"q","page":{"cursor":"c"}}`)
	if !strings.Contains(string(search.Arguments), `"size":20`) || !strings.Contains(string(search.Arguments), `"cursor":"c"`) {
		t.Errorf("page not normalized: %s", search.Arguments)
	}
}

func TestInterpret_RefusesWhatTheContractRefuses(t *testing.T) {
	task := collab.Task{Ref: collab.Ref{Source: "local:x", ID: "T-1"}, Revision: "r2", Title: "t", State: collab.TaskStateDone}
	cases := []struct {
		name      string
		operation string
		contract  string
		arguments string
		result    func(t *testing.T) proto.ToolCallResult
		outcome   collab.Outcome
		reason    string
	}{
		{"two blocks", "get", "tasks", `{"ref":{"source":"local:x","id":"T-1"}}`, func(*testing.T) proto.ToolCallResult {
			return proto.ToolCallResult{Content: []proto.ToolContent{{Type: "text", Text: "{}"}, {Type: "text", Text: "{}"}}}
		}, collab.OutcomeUnavailable, collab.ReasonProviderInvalidResponse},
		{"a read answering conflict", "get", "tasks", `{"ref":{"source":"local:x","id":"T-1"}}`, func(t *testing.T) proto.ToolCallResult {
			return collabRespond(t, collab.Response{Outcome: collab.OutcomeConflict, Error: &collab.Error{Message: "x"}})
		}, collab.OutcomeUnavailable, collab.ReasonProviderInvalidResponse},
		{"isError disagreeing", "create", "tasks", `{"title":"t","idempotencyKey":"k"}`, func(t *testing.T) proto.ToolCallResult {
			result := collabOK(t, &collab.TaskCreateResult{Task: task, Created: true})
			result.IsError = true
			return result
		}, collab.OutcomeUnresolved, collab.ReasonProviderInvalidResponse},
		{"a result the contract refuses", "create", "tasks", `{"title":"t","idempotencyKey":"k"}`, func(t *testing.T) proto.ToolCallResult {
			return collabOK(t, json.RawMessage(`{"task":{"title":"no ref"},"created":true}`))
		}, collab.OutcomeUnresolved, collab.ReasonProviderInvalidResponse},
		{"a transition to another state", "transition", "tasks", `{"ref":{"source":"local:x","id":"T-1"},"state":"blocked"}`, func(t *testing.T) proto.ToolCallResult {
			return collabOK(t, &collab.TaskResult{Task: task})
		}, collab.OutcomeUnresolved, collab.ReasonProviderIdentity},
		{"a claim for another holder", "claim", "tasks", `{"ref":{"source":"local:x","id":"T-1"},"holder":"me"}`, func(t *testing.T) proto.ToolCallResult {
			held := task
			held.Holder = "someone"
			return collabOK(t, &collab.TaskClaimResult{Task: held, Held: true})
		}, collab.OutcomeUnresolved, collab.ReasonProviderIdentity},
		{"a release still held", "claim", "tasks", `{"ref":{"source":"local:x","id":"T-1"},"holder":"me","release":true}`, func(t *testing.T) proto.ToolCallResult {
			held := task
			held.Holder = "me"
			return collabOK(t, &collab.TaskClaimResult{Task: held, Held: true})
		}, collab.OutcomeUnresolved, collab.ReasonProviderIdentity},
		{"a find outside the states", "find", "tasks", `{"states":["open"]}`, func(t *testing.T) proto.ToolCallResult {
			return collabOK(t, &collab.TaskListResult{Items: []collab.Task{task}})
		}, collab.OutcomeUnavailable, collab.ReasonProviderIdentity},
		{"a status for another proposal", "status", "proposals", `{"ref":{"source":"local:x","id":"P-1"}}`, func(t *testing.T) proto.ToolCallResult {
			return collabOK(t, &collab.ProposalStatusResult{
				Proposal: collab.Proposal{Ref: collab.Ref{Source: "local:x", ID: "P-2"}, Revision: "r", Title: "t", State: collab.ProposalStateOpen,
					Change: collab.Change{Repository: "r", Base: "main", Head: "h"}},
				Checks: collab.Checks{State: collab.ChecksStateNone},
			})
		}, collab.OutcomeUnavailable, collab.ReasonProviderIdentity},
		{"a review of another commit", "review", "proposals", `{"ref":{"source":"local:x","id":"P-1"},"verdict":"approve","body":"b","commit":"abcdef1","idempotencyKey":"k"}`, func(t *testing.T) proto.ToolCallResult {
			return collabOK(t, &collab.ProposalReviewResult{Review: collab.Review{Ref: collab.Ref{Source: "local:x", ID: "R"}, Verdict: collab.ReviewVerdictApprove, Commit: "1234567"}, Created: true})
		}, collab.OutcomeUnresolved, collab.ReasonProviderIdentity},
		{"a merge that did not merge", "merge", "proposals", `{"ref":{"source":"local:x","id":"P-1"},"expectedHeadCommit":"abcdef1"}`, func(t *testing.T) proto.ToolCallResult {
			return collabOK(t, &collab.ProposalResult{Proposal: collab.Proposal{Ref: collab.Ref{Source: "local:x", ID: "P-1"}, Revision: "r", Title: "t",
				State: collab.ProposalStateOpen, Change: collab.Change{Repository: "r", Base: "main", Head: "h"}}})
		}, collab.OutcomeUnresolved, collab.ReasonProviderIdentity},
		{"an upsert on another head commit", "upsert", "proposals", `{"change":{"base":"main","head":"h","headCommit":"abcdef1"},"title":"t"}`, func(t *testing.T) proto.ToolCallResult {
			return collabOK(t, &collab.ProposalUpsertResult{Proposal: collab.Proposal{Ref: collab.Ref{Source: "local:x", ID: "P-1"}, Revision: "r", Title: "t",
				State: collab.ProposalStateOpen, Change: collab.Change{Repository: "r", Base: "main", Head: "h", HeadCommit: "1234567"}}})
		}, collab.OutcomeUnresolved, collab.ReasonProviderIdentity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call := collabBoundCall(t, tc.contract, tc.operation, tc.arguments)
			envelope := interpretResult(call, tc.result(t))
			if envelope.Outcome != tc.outcome || envelope.Error == nil || envelope.Error.Reason != tc.reason {
				t.Fatalf("envelope %s %+v, want %s %s", envelope.Outcome, envelope.Error, tc.outcome, tc.reason)
			}
			if envelope.Outcome == collab.OutcomeUnresolved && (envelope.Error.Retryable || envelope.Error.Reconcile == "") {
				t.Errorf("unresolved without its reconciliation: %+v", envelope.Error)
			}
		})
	}
}

// A provider that does not shorten its pages, as Serve does for a Go
// provider, can answer a list page above the document bound. The route
// refuses it as before, and tells the caller of a list how to get the items:
// a smaller page.size. No other operation gets that advice.
func TestInterpret_APageAboveTheDocumentBoundAsksForASmallerPageSize(t *testing.T) {
	spectest.Proves(t, "cli/collaboration-providers", "bounded-pages", "a-page-above-the-document-bound-asks-for-a-smaller-page-size")
	items := make([]collab.Task, collab.DefaultPageSize)
	for i := range items {
		items[i] = collab.Task{Ref: collab.Ref{Source: "local:x", ID: fmt.Sprintf("T-%d", i)}, Revision: "r1", Title: "t",
			Body: strings.Repeat("<", collab.MaxBodyBytes), State: collab.TaskStateOpen}
	}
	oversized := collabOK(t, &collab.TaskListResult{Items: items})
	if len(oversized.Content[0].Text) <= collab.MaxDocumentBytes {
		t.Fatalf("the page encodes to %d bytes, within the bound", len(oversized.Content[0].Text))
	}
	envelope := interpretResult(collabBoundCall(t, "tasks", "find", `{}`), oversized)
	if envelope.Outcome != collab.OutcomeUnavailable || envelope.Error.Reason != collab.ReasonProviderInvalidResponse || envelope.Error.Retryable {
		t.Fatalf("an oversized page: %s %+v", envelope.Outcome, envelope.Error)
	}
	for _, want := range []string{"exceeds 4194304 bytes", "a page of 20 items does not fit in one answer: ask for a smaller page.size"} {
		if !strings.Contains(envelope.Error.Message, want) {
			t.Errorf("the refusal %q does not say %q", envelope.Error.Message, want)
		}
	}
	if _, diags := collab.ParseEnvelope(collabJSON(t, envelope)); diags != nil {
		t.Errorf("the refusal violates the contract: %v", diags)
	}
	sized := interpretResult(collabBoundCall(t, "memory", "context", `{"page":{"size":7}}`), oversized)
	if !strings.Contains(sized.Error.Message, "a page of 7 items does not fit") {
		t.Errorf("the refusal names another size than the request's: %q", sized.Error.Message)
	}

	// A document above the bound that is not a list page gets no advice
	// about pages.
	padded := proto.ToolCallResult{Content: []proto.ToolContent{{Type: "text",
		Text: `{"outcome":"ok","result":{}}` + strings.Repeat(" ", collab.MaxDocumentBytes)}}}
	envelope = interpretResult(collabBoundCall(t, "tasks", "get", `{"ref":{"source":"local:x","id":"T-1"}}`), padded)
	if envelope.Outcome != collab.OutcomeUnavailable || !strings.Contains(envelope.Error.Message, "exceeds") || strings.Contains(envelope.Error.Message, "page.size") {
		t.Fatalf("an oversized answer to a get: %s %+v", envelope.Outcome, envelope.Error)
	}
	// Nor does a page whose own member is above its bound: a smaller page
	// carries the same member.
	oneLong := collabOK(t, &collab.TaskListResult{Items: []collab.Task{{Ref: collab.Ref{Source: "local:x", ID: "T-1"}, Revision: "r1", Title: "t",
		Body: strings.Repeat("b", collab.MaxBodyBytes+1), State: collab.TaskStateOpen}}})
	envelope = interpretResult(collabBoundCall(t, "tasks", "find", `{}`), oneLong)
	if envelope.Outcome != collab.OutcomeUnavailable || !strings.Contains(envelope.Error.Message, "exceeds") || strings.Contains(envelope.Error.Message, "page.size") {
		t.Fatalf("a page with one oversized member: %s %+v", envelope.Outcome, envelope.Error)
	}
	// Nor does a list answer the contract refuses for another reason.
	envelope = interpretResult(collabBoundCall(t, "tasks", "find", `{}`), collabOK(t, json.RawMessage(`{"items":[{"title":"no ref"}]}`)))
	if strings.Contains(envelope.Error.Message, "page.size") {
		t.Fatalf("a malformed page is advised a smaller page.size: %q", envelope.Error.Message)
	}
}

// Every list request carries the page the advice names, and no other
// request does.
func TestRequestedPage_CoversEveryListOperationOfTheCatalog(t *testing.T) {
	pageType := reflect.TypeOf((*collab.PageRequest)(nil))
	lists := 0
	for _, spec := range collab.Catalog() {
		for _, op := range spec.Operations {
			input := op.NewInput()
			if input == nil {
				if requestedPage(input) != nil {
					t.Errorf("%s.%s: a request without a document has a page", spec.Name, op.Name)
				}
				continue
			}
			field, paged := reflect.TypeOf(input).Elem().FieldByName("Page")
			paged = paged && field.Type == pageType
			normalize(input, "w")
			if got := requestedPage(input); (got != nil) != paged {
				t.Errorf("%s.%s v%d: requestedPage = %v, the request has a page: %v", spec.Name, op.Name, spec.Version, got, paged)
			}
			if paged {
				lists++
			}
		}
	}
	if lists == 0 {
		t.Fatal("no catalog request has a page; the coverage is unproven")
	}
}

func TestInterpret_MemoryIdentity(t *testing.T) {
	now := "2026-09-24T08:00:00Z"
	record := collab.MemoryRecord{Ref: collab.Ref{Source: "git:m", ID: "m1"}, Revision: "r1", Kind: collab.MemoryKindMission,
		Identity: collab.MemoryIdentity{Mission: "other"}, Content: "x",
		Provenance: collab.Provenance{RecordedAt: now}, Freshness: collab.Freshness{UpdatedAt: now, RetrievedAt: now}}
	mission := collabBoundCall(t, "memory", "mission", `{"mission":"m1"}`)
	if envelope := interpretResult(mission, collabOK(t, &collab.MemoryRecordResult{Record: record})); envelope.Outcome != collab.OutcomeUnavailable {
		t.Errorf("a record of another mission: %s", envelope.Outcome)
	}
	record.Identity.Mission = "m1"
	checkpoint := collabBoundCall(t, "memory", "checkpoint", `{"mission":"m1","precondition":{"expectedRevision":"r1"},"idempotencyKey":"k","content":"x"}`)
	if envelope := interpretResult(checkpoint, collabOK(t, &collab.MemoryCheckpointResult{Record: record})); envelope.Outcome != collab.OutcomeUnresolved {
		t.Errorf("a write that kept its revision: %s", envelope.Outcome)
	}
	if envelope := interpretResult(checkpoint, collabOK(t, &collab.MemoryCheckpointResult{Record: record, Replayed: true})); envelope.Outcome != collab.OutcomeOK {
		t.Errorf("a replay keeps the revision it produced: %s %+v", envelope.Outcome, envelope.Error)
	}
	note := record
	note.Kind = collab.MemoryKindNote
	context := collabBoundCall(t, "memory", "context", `{"kinds":["mission"],"page":{"size":1}}`)
	if envelope := interpretResult(context, collabOK(t, &collab.MemoryListResult{Items: []collab.MemoryRecord{note}})); envelope.Outcome != collab.OutcomeUnavailable {
		t.Errorf("a kind outside the request: %s", envelope.Outcome)
	}
	if envelope := interpretResult(context, collabOK(t, &collab.MemoryListResult{Items: []collab.MemoryRecord{record, record}})); envelope.Outcome != collab.OutcomeUnavailable {
		t.Errorf("an oversized page: %s", envelope.Outcome)
	}
	if envelope := interpretResult(checkpoint, collabRespond(t, collab.Response{Outcome: collab.OutcomeUnresolved,
		Error: &collab.Error{Message: "lost"}})); envelope.Error.Reconcile == "" {
		t.Error("an unresolved answer gets the default reconciliation")
	}
}

func TestTransportFailure_ClassifiesByStageAndAccess(t *testing.T) {
	read := collabBoundCall(t, "tasks", "find", `{}`)
	write := collabBoundCall(t, "tasks", "update", `{"ref":{"source":"local:x","id":"T-1"},"title":"x"}`)
	cases := []struct {
		call      *providerCall
		stage     invocationStage
		outcome   collab.Outcome
		reason    string
		retryable bool
	}{
		{read, stageStart, collab.OutcomeUnavailable, collab.ReasonProviderUnavailable, false},
		{write, stageStart, collab.OutcomeUnavailable, collab.ReasonProviderUnavailable, false},
		{read, stageTimeout, collab.OutcomeUnavailable, collab.ReasonProviderTimeout, true},
		{write, stageTimeout, collab.OutcomeUnresolved, collab.ReasonProviderTimeout, false},
		{read, stageCanceled, collab.OutcomeUnavailable, collab.ReasonProviderCanceled, true},
		{write, stageCanceled, collab.OutcomeUnresolved, collab.ReasonProviderCanceled, false},
		{read, stageRun, collab.OutcomeUnavailable, collab.ReasonProviderFailed, false},
		{write, stageRun, collab.OutcomeUnresolved, collab.ReasonProviderFailed, false},
		{read, stageOutput, collab.OutcomeUnavailable, collab.ReasonProviderInvalidResponse, false},
		{write, stageOutput, collab.OutcomeUnresolved, collab.ReasonProviderInvalidResponse, false},
		// Nothing ran before the process started, so even a write is
		// unavailable, and repeating it can help.
		{read, stageTimeoutBeforeStart, collab.OutcomeUnavailable, collab.ReasonProviderTimeout, true},
		{write, stageTimeoutBeforeStart, collab.OutcomeUnavailable, collab.ReasonProviderTimeout, true},
		{read, stageCanceledBeforeStart, collab.OutcomeUnavailable, collab.ReasonProviderCanceled, true},
		{write, stageCanceledBeforeStart, collab.OutcomeUnavailable, collab.ReasonProviderCanceled, true},
	}
	for _, tc := range cases {
		envelope := transportFailure(tc.call, tc.stage, "boom")
		if envelope.Outcome != tc.outcome || envelope.Error.Reason != tc.reason || envelope.Error.Retryable != tc.retryable {
			t.Errorf("%s stage %d: %s %s retryable=%v", tc.call.Operation.Name, tc.stage, envelope.Outcome, envelope.Error.Reason, envelope.Error.Retryable)
		}
		if _, diags := collab.ParseEnvelope(collabJSON(t, envelope)); diags != nil {
			t.Errorf("the failure envelope violates the contract: %v", diags)
		}
	}
	if hint := transportFailure(write, stageRun, "boom").Error.Reconcile; !strings.Contains(hint, "tasks.get") {
		t.Errorf("reconcile = %q", hint)
	}
}

func collabJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestRedactor(t *testing.T) {
	redactor := newRedactor([]string{
		"GH_TOKEN=ghp_0123456789abcdef", "SHORT_TOKEN=abc", "HOME=/Users/someone-long-enough",
		"DB_PASSWORD=pa\"ss\\word-long", "NOEQUALS",
	})
	text := redactor.redactText("token ghp_0123456789abcdef at https://user:pw@example.com/x and abc")
	if strings.Contains(text, "ghp_") || strings.Contains(text, "user:pw") || !strings.Contains(text, "abc") {
		t.Errorf("text = %q", text)
	}
	if long := redactor.redactText(strings.Repeat("é", maxMessageBytes)); len(long) > maxMessageBytes+len("…") {
		t.Errorf("a message is bounded, got %d bytes", len(long))
	}
	result, _ := json.Marshal(map[string]string{"body": "x ghp_0123456789abcdef y", "home": "/Users/someone-long-enough", "db": "pa\"ss\\word-long"})
	envelope := redactor.redactEnvelope(collab.Envelope{Contract: "tasks", Operation: "get", Outcome: collab.OutcomeOK, Result: result}, false)
	if strings.Contains(string(envelope.Result), "ghp_") || strings.Contains(string(envelope.Result), "word-long") ||
		!strings.Contains(string(envelope.Result), "/Users/someone-long-enough") || !json.Valid(envelope.Result) {
		t.Errorf("result = %s", envelope.Result)
	}
	clean := redactor.redactEnvelope(collab.Envelope{Contract: "tasks", Operation: "get", Outcome: collab.OutcomeOK, Result: json.RawMessage(`{"a":1}`)}, false)
	if string(clean.Result) != `{"a":1}` {
		t.Errorf("an untouched result changed: %s", clean.Result)
	}

	// URL userinfo is removed from a result even when the environment carries
	// no credential.
	bare := newRedactor(nil)
	withURL := json.RawMessage(`{"settings":{"remote":"https://agent:s3cret-pass@git.example.com/memory.git"}}`)
	stripped := bare.redactEnvelope(collab.Envelope{Contract: "memory", Operation: "capabilities", Outcome: collab.OutcomeOK, Result: withURL}, false)
	if strings.Contains(string(stripped.Result), "s3cret-pass") || !strings.Contains(string(stripped.Result), "https://"+redactedMarker+"@git.example.com") || !json.Valid(stripped.Result) {
		t.Errorf("url userinfo in result = %s", stripped.Result)
	}

	// Bytes that only spell a secret across an escape sequence do not carry
	// it: the decoded string is "x", a newline, then "secretvalue".
	escaped := newRedactor([]string{"API_TOKEN=nsecretvalue"})
	straddling := json.RawMessage(`{"body":"x\nsecretvalue"}`)
	if kept := escaped.redactEnvelope(collab.Envelope{Contract: "tasks", Operation: "get", Outcome: collab.OutcomeOK, Result: straddling}, false); kept.Outcome != collab.OutcomeOK || string(kept.Result) != string(straddling) {
		t.Errorf("a result without the secret changed: %+v", kept)
	}

	// A secret that redaction cannot remove in place — inside a number, or in
	// a result that does not decode — withholds the result.
	numeric := newRedactor([]string{"API_TOKEN=1234567890"})
	for _, raw := range []json.RawMessage{json.RawMessage(`{"pin":1234567890}`), json.RawMessage(`{"body":"1234567890"`)} {
		read := numeric.redactEnvelope(collab.Envelope{Contract: "tasks", Operation: "get", Outcome: collab.OutcomeOK, Result: raw}, false)
		write := numeric.redactEnvelope(collab.Envelope{Contract: "tasks", Operation: "create", Outcome: collab.OutcomeOK, Result: raw}, true)
		if read.Outcome != collab.OutcomeUnavailable || write.Outcome != collab.OutcomeUnresolved || read.Result != nil || write.Result != nil || write.Error.Reconcile == "" {
			t.Errorf("%s withheld: read %+v, write %+v", raw, read, write)
		}
		for _, withheld := range []collab.Envelope{read, write} {
			if diags := collab.ValidateEnvelope(&withheld); diags != nil {
				t.Errorf("the withheld %s envelope violates the contract: %v", withheld.Operation, diags)
			}
		}
	}
}

// Providers written in other languages spell a secret differently: Node and
// Python leave <, > and & raw, Python escapes non-ASCII as \u, and some
// encoders escape /. Redaction matches the decoded value, so every spelling
// is removed, and the redacted result keeps its member order.
func TestRedactor_EverySpellingOfASecretIsRemovedFromAResult(t *testing.T) {
	spectest.Proves(t, "cli/collaboration-providers", "no-credential-leakage", "credential-values-are-redacted")
	const secret = "a<b>c&d/pässwörd-long"
	redactor := newRedactor([]string{"API_TOKEN=" + secret})
	goSpelling, _ := json.Marshal(map[string]string{"body": "x " + secret + " y"})
	for name, raw := range map[string]string{
		"raw, as Node writes it":       `{"z":1,"body":"x a<b>c&d/pässwörd-long y","a":true}`,
		"escaped, as Go writes it":     string(goSpelling),
		"ASCII-only, as Python writes": `{"z":1,"body":"x a<b>c&d/p\u00e4ssw\u00f6rd-long y","a":true}`,
		"with an escaped solidus":      `{"z":1,"body":"x a<b>c&d\/pässwörd-long y","a":true}`,
		"as a member name":             `{"z":1,"a<b>c&d/pässwörd-long":"x","a":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			envelope := redactor.redactEnvelope(collab.Envelope{Contract: "tasks", Operation: "get", Outcome: collab.OutcomeOK, Result: json.RawMessage(raw)}, false)
			if envelope.Outcome != collab.OutcomeOK || !json.Valid(envelope.Result) {
				t.Fatalf("envelope = %+v", envelope)
			}
			var decoded any
			if err := json.Unmarshal(envelope.Result, &decoded); err != nil {
				t.Fatal(err)
			}
			flat, _ := json.Marshal(decoded)
			if strings.Contains(string(flat), "pässwörd") || strings.Contains(string(envelope.Result), "ssw") || !strings.Contains(string(envelope.Result), redactedMarker) {
				t.Fatalf("the secret survived: %s", envelope.Result)
			}
			if strings.HasPrefix(raw, `{"z"`) && !strings.HasPrefix(string(envelope.Result), `{"z":1,`) {
				t.Errorf("member order changed: %s", envelope.Result)
			}
		})
	}
}

// Every request member that carries a write precondition is recognized, so a
// provider that declares preconditions none never receives one, and an
// operation whose rule takes no precondition carries no such member.
func TestRequestPrecondition_CoversEveryPreconditionMemberOfTheCatalog(t *testing.T) {
	spectest.Proves(t, "cli/collaboration-providers", "capability-discovery", "a-precondition-the-provider-cannot-enforce-is-refused")
	covered := map[string]bool{}
	for _, spec := range collab.Catalog() {
		for _, op := range spec.Operations {
			input := op.NewInput()
			if input == nil {
				continue
			}
			members := preconditionMembers(reflect.TypeOf(input).Elem(), nil, "")
			id := fmt.Sprintf("%s.%s v%d", spec.Name, op.Name, spec.Version)
			if op.Preconditions == collab.PreconditionRuleNone && len(members) > 0 {
				t.Errorf("%s takes no precondition, yet its request carries %v", id, members)
			}
			if got := requestPrecondition(input); got != "" {
				t.Errorf("%s: an empty request carries precondition %q", id, got)
			}
			for _, member := range members {
				request := op.NewInput()
				field := reflect.ValueOf(request).Elem().FieldByIndex(member.index)
				switch field.Kind() {
				case reflect.String:
					field.SetString("r1")
				case reflect.Bool:
					field.SetBool(true)
				}
				if requestPrecondition(request) == "" {
					t.Errorf("%s: %s is not recognized as a precondition", id, member.name)
				}
				covered[member.name] = true
			}
		}
	}
	for _, name := range []string{"ExpectedRevision", "Precondition.ExpectedRevision", "Precondition.MustNotExist"} {
		if !covered[name] {
			t.Errorf("no catalog request carries %s; the guard's coverage is unproven", name)
		}
	}
}

type preconditionMember struct {
	index []int
	name  string
}

// preconditionMembers lists the members of a request type that carry a write
// precondition: every ExpectedRevision string and MustNotExist bool, at any
// struct depth.
func preconditionMembers(typ reflect.Type, prefix []int, path string) []preconditionMember {
	var members []preconditionMember
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		index := append(append([]int(nil), prefix...), i)
		switch {
		case field.Name == "ExpectedRevision" && field.Type.Kind() == reflect.String,
			field.Name == "MustNotExist" && field.Type.Kind() == reflect.Bool:
			members = append(members, preconditionMember{index: index, name: path + field.Name})
		case field.Type.Kind() == reflect.Struct:
			members = append(members, preconditionMembers(field.Type, index, path+field.Name+".")...)
		}
	}
	return members
}
