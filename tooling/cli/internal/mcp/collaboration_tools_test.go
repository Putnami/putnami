package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	collab "go.putnami.dev/protocol/collaboration"
	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The collaboration route, end to end on the MCP surface: a real extension
// manifest on disk, discovered by the server's own discovery, bound from the
// workspace document, invoked through the extension tool transport.
//
// The provider is this test binary re-executed (the pattern
// extension_tool_workspace_test.go explains). Its behavior is chosen per
// fixture through the manifest's tool environment, so one binary plays a
// well-behaved provider, a slow one, a crashing one and a lying one.

const (
	collabProviderModeEnv   = "PUTNAMI_MCP_COLLAB_PROVIDER_MODE"
	collabProviderRecordEnv = "PUTNAMI_MCP_COLLAB_PROVIDER_RECORD"
	// collabSecretEnv is credential-named, so the orchestrator redacts its
	// value from everything it emits.
	collabSecretEnv   = "PUTNAMI_MCP_TEST_API_TOKEN"
	collabSecretValue = "s3cr3t-value-that-must-not-leak"
	collabProvider    = "@test/collab"
	collabSource      = "local:fixture"
)

// TestCollaborationProviderHelperProcess is the fake provider. It does
// nothing unless a tool call spawned it.
func TestCollaborationProviderHelperProcess(t *testing.T) {
	mode := os.Getenv(collabProviderModeEnv)
	if mode == "" {
		t.Skip("helper process: not invoked as a collaboration provider")
	}
	payload, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	if path := os.Getenv(collabProviderRecordEnv); path != "" {
		_ = os.WriteFile(path, payload, 0o644)
	}
	switch mode {
	case "sleep":
		time.Sleep(20 * time.Second)
	case "crash":
		// A provider that dies after logging its credential: stderr must never
		// reach an envelope.
		fmt.Fprintln(os.Stderr, "backend refused token "+os.Getenv(collabSecretEnv))
		os.Exit(3)
	case "garbage":
		_, _ = os.Stdout.WriteString("not a tool result")
		os.Exit(0)
	}
	if err := collab.Serve(context.Background(), bytes.NewReader(payload), os.Stdout, fakeCollabHandlers(mode)); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func fakeTask(id string) collab.Task {
	return collab.Task{Ref: collab.Ref{Source: collabSource, ID: id}, Revision: "r1", Title: "task " + id, State: collab.TaskStateOpen}
}

func fakeProposal(change collab.Change) collab.Proposal {
	if change.Repository == "" {
		change.Repository = "local"
	}
	return collab.Proposal{Ref: collab.Ref{Source: collabSource, ID: "P-1"}, Revision: "r1", Change: change, Title: "proposal", State: collab.ProposalStateOpen}
}

func fakeCollabHandlers(mode string) map[collab.OperationKey]collab.Handler {
	tasks := func() []collab.Task {
		if mode == "empty" {
			return nil
		}
		out := make([]collab.Task, 5)
		for i := range out {
			out[i] = fakeTask("T-" + strconv.Itoa(i+1))
		}
		return out
	}
	key := func(contract, operation string) collab.OperationKey {
		return collab.OperationKey{Contract: contract, Version: 1, Operation: operation}
	}
	taskResult := func(ref collab.Ref) (any, *collab.Failure) {
		if mode == "wrong-identity" {
			ref.ID = "T-999"
		}
		task := fakeTask(ref.ID)
		task.Ref = ref
		return &collab.TaskResult{Task: task}, nil
	}
	return map[collab.OperationKey]collab.Handler{
		key("tasks", "find"): func(_ context.Context, call collab.Call) (any, *collab.Failure) {
			input := call.Input.(*collab.TaskFindInput)
			all := tasks()
			start := 0
			if input.Page.Cursor != "" {
				start, _ = strconv.Atoi(input.Page.Cursor)
			}
			end := min(start+input.Page.Size, len(all))
			if mode == "overfull" {
				end = len(all)
			}
			result := &collab.TaskListResult{Items: append([]collab.Task{}, all[start:end]...)}
			if end < len(all) && mode != "overfull" {
				result.Page.Next = strconv.Itoa(end)
			}
			return result, nil
		},
		key("tasks", "get"): func(_ context.Context, call collab.Call) (any, *collab.Failure) {
			if mode == "leak" {
				return nil, collab.Fail(collab.OutcomeDenied, "auth.refused",
					"the backend refused %s at https://user:%s@example.com", os.Getenv(collabSecretEnv), os.Getenv(collabSecretEnv))
			}
			return taskResult(call.Input.(*collab.TaskRefInput).Ref)
		},
		key("tasks", "create"): func(_ context.Context, call collab.Call) (any, *collab.Failure) {
			input := call.Input.(*collab.TaskCreateInput)
			task := fakeTask("T-9")
			task.Title = input.Title
			if mode == "leak" {
				task.Body = "created with " + os.Getenv(collabSecretEnv)
			}
			return &collab.TaskCreateResult{Task: task, Created: true}, nil
		},
		key("tasks", "update"): func(_ context.Context, call collab.Call) (any, *collab.Failure) {
			return taskResult(call.Input.(*collab.TaskUpdateInput).Ref)
		},
		key("tasks", "transition"): func(_ context.Context, call collab.Call) (any, *collab.Failure) {
			input := call.Input.(*collab.TaskTransitionInput)
			task := fakeTask(input.Ref.ID)
			task.State = input.State
			return &collab.TaskResult{Task: task}, nil
		},
		key("tasks", "assign"): func(_ context.Context, call collab.Call) (any, *collab.Failure) {
			return taskResult(call.Input.(*collab.TaskAssignInput).Ref)
		},
		key("proposals", "find"): func(_ context.Context, call collab.Call) (any, *collab.Failure) {
			change := call.Input.(*collab.ProposalFindInput).Change
			if mode == "wrong-identity" {
				change.Head = "another-branch"
			}
			return &collab.ProposalListResult{Items: []collab.Proposal{fakeProposal(change)}}, nil
		},
		key("proposals", "upsert"): func(_ context.Context, call collab.Call) (any, *collab.Failure) {
			change := call.Input.(*collab.ProposalUpsertInput).Change
			if mode == "wrong-identity" {
				change.Head = "another-branch"
			}
			return &collab.ProposalUpsertResult{Proposal: fakeProposal(change), Created: true}, nil
		},
		key("proposals", "status"): func(_ context.Context, call collab.Call) (any, *collab.Failure) {
			proposal := fakeProposal(collab.Change{Base: "main", Head: "topic"})
			proposal.Ref = call.Input.(*collab.ProposalRefInput).Ref
			return &collab.ProposalStatusResult{Proposal: proposal,
				Checks: collab.Checks{State: collab.ChecksStateUnsupported, Detail: "the fixture provider runs no hosted checks"}}, nil
		},
		key("proposals", "review"): func(_ context.Context, call collab.Call) (any, *collab.Failure) {
			input := call.Input.(*collab.ProposalReviewInput)
			return &collab.ProposalReviewResult{Review: collab.Review{Ref: collab.Ref{Source: collabSource, ID: "R-1"}, Verdict: input.Verdict}, Created: true}, nil
		},
		key("memory", "context"): func(_ context.Context, call collab.Call) (any, *collab.Failure) {
			selection, _ := json.Marshal(call.Request.Selection)
			now := "2026-09-24T08:00:00Z"
			return &collab.MemoryListResult{Items: []collab.MemoryRecord{{
				Ref: collab.Ref{Source: collabSource, ID: "N-1"}, Revision: "r1", Kind: collab.MemoryKindNote,
				Identity:   call.Input.(*collab.MemoryContextInput).Identity,
				Content:    string(selection) + fmt.Sprintf(" members=%d settings=%s", len(call.Request.WorkspaceProjects), call.Settings),
				Provenance: collab.Provenance{RecordedAt: now},
				Freshness:  collab.Freshness{UpdatedAt: now, RetrievedAt: now},
			}}}, nil
		},
		key("memory", "mission"): func(context.Context, collab.Call) (any, *collab.Failure) {
			return nil, collab.Fail(collab.OutcomeNotFound, "mission.missing", "no such mission")
		},
		key("memory", "checkpoint"): func(context.Context, collab.Call) (any, *collab.Failure) {
			return nil, collab.Fail(collab.OutcomeConflict, collab.ReasonRevisionConflict, "the mission moved")
		},
	}
}

// fakeProviderTool declares one operation of the fake provider, with
// annotations that agree with the catalog.
func fakeProviderTool(t *testing.T, contract, operation string, preconditions collab.Preconditions, env map[string]string) proto.ToolDefinition {
	t.Helper()
	spec, _ := collab.Lookup(contract, 1)
	if _, ok := spec.Operation(operation); !ok {
		t.Fatalf("no operation %s.%s", contract, operation)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	def := collabToolDefinition(t, contract, operation, preconditions, executable)
	def.Args = []string{"-test.run=^TestCollaborationProviderHelperProcess$"}
	def.Env = env
	return def
}

// collabFixture is one workspace binding the fake provider.
type collabFixture struct {
	root   string
	record string
	tools  map[string]proto.ToolDefinition
}

// newCollabFixture writes a workspace whose options.collaboration block is
// bindings, and a provider extension implementing every required operation of
// the three contracts plus tasks.assign (with no precondition support). mutate
// edits the provider's tools before the manifest is written.
func newCollabFixture(t *testing.T, mode, bindings string, mutate func(map[string]proto.ToolDefinition)) collabFixture {
	t.Helper()
	root := fixtureWorkspace(t)
	record := filepath.Join(t.TempDir(), "request.json")
	env := map[string]string{collabProviderModeEnv: mode, collabProviderRecordEnv: record}
	tools := map[string]proto.ToolDefinition{}
	for _, contract := range collab.ContractNames {
		spec, _ := collab.Lookup(contract, 1)
		for _, op := range spec.Operations {
			if !op.Required && op.Name != collab.OperationAssign {
				continue
			}
			var preconditions collab.Preconditions
			switch op.Preconditions {
			case collab.PreconditionRuleDeclared:
				preconditions = collab.PreconditionsChecked
				if op.Name == collab.OperationAssign {
					preconditions = collab.PreconditionsNone
				}
			case collab.PreconditionRuleAtomic:
				preconditions = collab.PreconditionsAtomic
			}
			tools["fixture."+contract+"."+op.Name] = fakeProviderTool(t, contract, op.Name, preconditions, env)
		}
	}
	if mutate != nil {
		mutate(tools)
	}
	manifest, err := json.Marshal(proto.Manifest{Name: collabProvider, Version: "1.2.3", CLIContract: protocolcli.CurrentContract, Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	extensionRoot := filepath.Join(t.TempDir(), "provider")
	if err := os.MkdirAll(extensionRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extensionRoot, proto.ManifestFilename), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	extensionsJSON, _ := json.Marshal([]string{extensionRoot})
	document := `{"name":"fixture","includes":["packages/app","packages/lib"],"extensions":` + string(extensionsJSON)
	if bindings != "" {
		document += `,"options":{"collaboration":` + bindings + `}`
	}
	document += "}"
	if err := os.WriteFile(filepath.Join(root, wsproto.WorkspaceConfigFilename), []byte(document), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace.InvalidateLoadCache(root)
	recordWorkspaceIndex(t, root)
	return collabFixture{root: root, record: record, tools: tools}
}

const bindAll = `{"tasks":{"provider":"@test/collab","version":1},` +
	`"proposals":{"provider":"@test/collab","version":1,"settings":{"repository":"local"}},` +
	`"memory":{"provider":"@test/collab","version":1,"settings":{"root":"memory"}}}`

func (f collabFixture) server(t *testing.T) *Server {
	t.Helper()
	srv := NewServer(Options{WorkspaceRoot: f.root, Config: wsproto.Load(f.root), ServerVersion: "test", ResolveSelection: fixtureSelectionResolver})
	return srv
}

// callCollab runs one routed tool call and decodes the envelope, checking
// that isError agrees with the outcome.
func callCollab(t *testing.T, srv *Server, name, arguments string) collab.Envelope {
	t.Helper()
	result := callEcho(t, srv, name, arguments)
	if len(result.Content) != 1 {
		t.Fatalf("%s: want one content block, got %s", name, result.detail())
	}
	text := result.Content[0].Text
	envelope, diags := collab.ParseEnvelope([]byte(text))
	if diags != nil {
		t.Fatalf("%s answered an envelope the contract refuses: %v\n%s", name, diags, text)
	}
	if isError := result.IsError; isError != (envelope.Outcome != collab.OutcomeOK) {
		t.Errorf("%s: isError %v disagrees with outcome %s", name, isError, envelope.Outcome)
	}
	return *envelope
}

func toolNames(t *testing.T, srv *Server) map[string]Tool {
	t.Helper()
	tools := map[string]Tool{}
	for _, tool := range srv.toolDefs() {
		tools[tool.Name] = tool
	}
	return tools
}

func expectOutcome(t *testing.T, envelope collab.Envelope, outcome collab.Outcome, reason string) {
	t.Helper()
	if envelope.Outcome != outcome {
		t.Fatalf("outcome = %s, want %s: %+v", envelope.Outcome, outcome, envelope.Error)
	}
	if reason != "" && (envelope.Error == nil || envelope.Error.Reason != reason) {
		t.Fatalf("reason = %+v, want %s", envelope.Error, reason)
	}
}

func TestCollaboration_AWorkspaceWithoutTheBlockReservesNothing(t *testing.T) {
	t.Parallel()
	fixture := newCollabFixture(t, "ok", "", func(tools map[string]proto.ToolDefinition) {
		tools["tasks.search"] = testExtensionToolDefinition(trueCommand(t))
	})
	srv := fixture.server(t)
	tools := toolNames(t, srv)
	for name := range tools {
		if strings.HasPrefix(name, "fixture.") || strings.HasSuffix(name, ".capabilities") {
			t.Errorf("tool %s is registered in a workspace that binds nothing", name)
		}
	}
	if _, ok := tools["tasks.search"]; !ok {
		t.Error("a workspace that binds nothing must not reserve the contract namespaces")
	}
}

func TestCollaboration_BoundContractsExposeTruthfulRoutedTools(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/collaboration-providers", "truthful-annotations", "routed-tools-carry-the-contract-access")
	spectest.Proves(t, "cli/collaboration-providers", "one-route-two-entry-paths", "provider-tools-are-reachable-only-through-the-binding")
	fixture := newCollabFixture(t, "ok", bindAll, nil)
	srv := fixture.server(t)
	tools := toolNames(t, srv)
	for name := range tools {
		if strings.HasPrefix(name, "fixture.") {
			t.Errorf("provider tool %s is exposed under its own name", name)
		}
	}
	for _, contract := range collab.ContractNames {
		spec, _ := collab.Lookup(contract, 1)
		if _, ok := tools[contract+".capabilities"]; !ok {
			t.Errorf("%s.capabilities is missing", contract)
		}
		for _, op := range spec.Operations {
			tool, registered := tools[contract+"."+op.Name]
			offered := op.Required || op.Name == collab.OperationAssign
			if registered != offered {
				t.Errorf("%s.%s registered=%v, offered=%v", contract, op.Name, registered, offered)
				continue
			}
			if !registered {
				continue
			}
			readOnly := op.Access == collab.AccessRead
			if *tool.Annotations.ReadOnlyHint != readOnly || (op.Destructive && !*tool.Annotations.DestructiveHint) {
				t.Errorf("%s annotations %+v disagree with the contract (%s, destructive=%v)", tool.Name, tool.Annotations, op.Access, op.Destructive)
			}
			var declared struct {
				Access string `json:"access"`
			}
			encoded, _ := json.Marshal(tool.Meta["putnami.dev/contract"])
			if json.Unmarshal(encoded, &declared) != nil || declared.Access != string(op.Access) {
				access := declared.Access
				t.Errorf("%s contract access %v, want %s", tool.Name, access, op.Access)
			}
			if strings.Contains(string(tool.InputSchema), "$ref") {
				t.Errorf("%s input schema is not self-contained", tool.Name)
			}
		}
	}
}

func TestCollaboration_AReservedNamespaceToolIsRefusedWhenContractsAreBound(t *testing.T) {
	t.Parallel()
	fixture := newCollabFixture(t, "ok", bindAll, func(tools map[string]proto.ToolDefinition) {})
	srv := fixture.server(t)
	shadow := testExtensionToolDefinition(trueCommand(t))
	srv.registerExtensionToolDefinitions([]*extension.ExtensionDescription{{
		Name: "@test/shadow", Path: t.TempDir(),
		Tools: map[string]extension.ToolDefinition{"tasks.search": shadow, "shadow.search": shadow},
	}})
	tools := toolNames(t, srv)
	if _, ok := tools["tasks.search"]; ok {
		t.Error("an extension tool in the tasks namespace was registered beside the binding")
	}
	if _, ok := tools["shadow.search"]; !ok {
		t.Error("an unrelated extension tool was refused")
	}
}

func TestCollaboration_CapabilitiesDescribeTheBinding(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/collaboration-providers", "capability-discovery", "capabilities-report-every-operation-and-its-support")
	fixture := newCollabFixture(t, "ok", bindAll, nil)
	envelope := callCollab(t, fixture.server(t), "tasks.capabilities", "")
	expectOutcome(t, envelope, collab.OutcomeOK, "")
	var capabilities collab.Capabilities
	if err := json.Unmarshal(envelope.Result, &capabilities); err != nil {
		t.Fatal(err)
	}
	if capabilities.Status != collab.BindingStatusBound || capabilities.Provider.Name != collabProvider || capabilities.Provider.Version != "1.2.3" {
		t.Fatalf("capabilities = %+v", capabilities)
	}
	support := map[string]collab.OperationCapability{}
	for _, op := range capabilities.Operations {
		support[op.Name] = op
	}
	if !support["find"].Supported || !support["assign"].Supported || support["claim"].Supported || support["link"].Supported {
		t.Errorf("operation support = %+v", support)
	}
	if support["assign"].Preconditions != collab.PreconditionsNone || support["update"].Preconditions != collab.PreconditionsChecked {
		t.Errorf("preconditions = %+v", support)
	}
	if support["find"].OpenWorld == nil || *support["find"].OpenWorld {
		t.Errorf("openWorld = %v", support["find"].OpenWorld)
	}
}

func TestCollaboration_AnUnboundContractIsUnsupportedAndNamesItsCandidates(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/collaboration-providers", "explicit-binding", "an-unbound-contract-is-unsupported-and-names-the-binding")
	spectest.Proves(t, "cli/collaboration-providers", "explicit-binding", "candidates-are-reported-never-selected")
	fixture := newCollabFixture(t, "ok", `{"tasks":{"provider":"@test/collab","version":1}}`, nil)
	srv := fixture.server(t)
	if _, ok := toolNames(t, srv)["memory.context"]; ok {
		t.Error("an unbound contract registered an operation tool")
	}
	envelope := callCollab(t, srv, "memory.capabilities", "")
	var capabilities collab.Capabilities
	if err := json.Unmarshal(envelope.Result, &capabilities); err != nil {
		t.Fatal(err)
	}
	if capabilities.Status != collab.BindingStatusUnbound || len(capabilities.Candidates) != 1 || capabilities.Candidates[0].Name != collabProvider {
		t.Fatalf("capabilities = %+v", capabilities)
	}
	// The same answer on the routing path, without running anything.
	routed := CallProviderOperation(context.Background(), ProviderEnvironment{
		WorkspaceRoot: fixture.root, Extensions: discovered(t, fixture.root),
	}, collab.ContractMemory, collab.OperationContext, json.RawMessage(`{}`))
	expectOutcome(t, routed, collab.OutcomeUnsupported, collab.ReasonBindingMissing)
	if !strings.Contains(routed.Error.Message, "options.collaboration.memory") || !strings.Contains(routed.Error.Message, collabProvider) {
		t.Errorf("message = %q", routed.Error.Message)
	}
	if _, err := os.Stat(fixture.record); !os.IsNotExist(err) {
		t.Error("an unbound contract started the provider")
	}
}

func discovered(t *testing.T, root string) []*extension.ExtensionDescription {
	t.Helper()
	ws, err := workspace.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, len(ws.Projects))
	for i, project := range ws.Projects {
		paths[i] = project.Path
	}
	exts, err := extension.DiscoverExtensions(root, wsproto.Load(root), paths)
	if err != nil {
		t.Fatal(err)
	}
	return exts
}

func TestCollaboration_BindingFailuresAreExplicit(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/collaboration-providers", "explicit-binding", "an-ambiguous-binding-is-refused")
	spectest.Proves(t, "cli/collaboration-providers", "explicit-binding", "a-missing-or-disabled-provider-is-unavailable")
	spectest.Proves(t, "cli/collaboration-providers", "capability-discovery", "an-unsupported-version-or-incomplete-provider-is-refused")
	cases := []struct {
		name     string
		bindings string
		mutate   func(map[string]proto.ToolDefinition)
		disable  bool
		outcome  collab.Outcome
		reason   string
	}{
		{"a contract bound twice", `{"tasks":{"provider":"@test/collab","version":1},"tasks":{"provider":"@test/other","version":1}}`,
			nil, false, collab.OutcomeUnsupported, collab.ReasonBindingAmbiguous},
		{"an unsupported version", `{"tasks":{"provider":"@test/collab","version":2}}`,
			nil, false, collab.OutcomeUnsupported, collab.ReasonBindingVersion},
		{"a provider that is not installed", `{"tasks":{"provider":"@test/absent","version":1}}`,
			nil, false, collab.OutcomeUnavailable, collab.ReasonBindingProviderMissing},
		{"a disabled provider", `{"tasks":{"provider":"@test/collab","version":1}}`,
			nil, true, collab.OutcomeUnavailable, collab.ReasonBindingProviderMissing},
		{"a provider missing a required operation", `{"tasks":{"provider":"@test/collab","version":1}}`,
			func(tools map[string]proto.ToolDefinition) { delete(tools, "fixture.tasks.transition") },
			false, collab.OutcomeUnsupported, collab.ReasonBindingIncomplete},
		{"a provider lacking an operation the binding requires", `{"tasks":{"provider":"@test/collab","version":1,"require":["claim"]}}`,
			nil, false, collab.OutcomeUnsupported, collab.ReasonBindingIncomplete},
		{"a provider whose annotations disagree", `{"tasks":{"provider":"@test/collab","version":1}}`,
			func(tools map[string]proto.ToolDefinition) {
				def := tools["fixture.tasks.find"]
				def.Annotations.ReadOnlyHint = boolPointer(false)
				def.Meta["putnami.dev/contract"] = jsonValue(t, `{"access":"mutating","readOnly":false,"supportsDryRun":false}`)
			}, false, collab.OutcomeUnsupported, collab.ReasonBindingIncomplete},
		{"credential settings", `{"tasks":{"provider":"@test/collab","version":1,"settings":{"apiToken":"x"}}}`,
			nil, false, collab.OutcomeUnsupported, collab.ReasonBindingInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fixture := newCollabFixture(t, "ok", tc.bindings, tc.mutate)
			unavailable := map[string]bool{}
			if tc.disable {
				unavailable[collabProvider] = true
			}
			envelope := CallProviderOperation(context.Background(), ProviderEnvironment{
				WorkspaceRoot: fixture.root, Extensions: discovered(t, fixture.root), Unavailable: unavailable,
			}, collab.ContractTasks, collab.OperationFind, json.RawMessage(`{}`))
			expectOutcome(t, envelope, tc.outcome, tc.reason)
			if envelope.Error.Retryable {
				t.Error("a binding failure is never retryable")
			}
			if _, err := os.Stat(fixture.record); !os.IsNotExist(err) {
				t.Error("a refused binding started the provider")
			}
		})
	}
}

// TestCollaboration_ABrokenBindingStillExposesItsFailure: the tools the
// binding promised stay registered, and each answers why it cannot serve.
func TestCollaboration_ABrokenBindingStillExposesItsFailure(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/collaboration-providers", "truthful-annotations", "a-provider-whose-annotations-disagree-is-refused")
	fixture := newCollabFixture(t, "ok", `{"tasks":{"provider":"@test/collab","version":1}}`, func(tools map[string]proto.ToolDefinition) {
		def := tools["fixture.tasks.create"]
		def.Annotations.ReadOnlyHint = boolPointer(true)
	})
	srv := fixture.server(t)
	tools := toolNames(t, srv)
	create, ok := tools["tasks.create"]
	if !ok {
		t.Fatal("tasks.create is not registered for a declared binding")
	}
	if *create.Annotations.ReadOnlyHint {
		t.Error("a lying provider's readOnly annotation reached the routed tool")
	}
	envelope := callCollab(t, srv, "tasks.create", `{"title":"t","idempotencyKey":"k"}`)
	expectOutcome(t, envelope, collab.OutcomeUnsupported, collab.ReasonBindingIncomplete)
	if !strings.Contains(envelope.Error.Message, "readOnlyHint must be false") {
		t.Errorf("message = %q", envelope.Error.Message)
	}
}

func TestCollaboration_OperationsThePathRefusesNeverReachTheProvider(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/collaboration-providers", "capability-discovery", "an-optional-operation-the-provider-lacks-is-unsupported")
	spectest.Proves(t, "cli/collaboration-providers", "capability-discovery", "a-precondition-the-provider-cannot-enforce-is-refused")
	fixture := newCollabFixture(t, "ok", bindAll, nil)
	env := ProviderEnvironment{WorkspaceRoot: fixture.root, Extensions: discovered(t, fixture.root)}
	cases := []struct {
		name      string
		operation string
		arguments string
		outcome   collab.Outcome
		reason    string
	}{
		{"an optional operation the provider lacks", collab.OperationClaim, `{"ref":{"source":"local:fixture","id":"T-1"},"holder":"a"}`,
			collab.OutcomeUnsupported, collab.ReasonOperationUnsupported},
		{"an operation the contract lacks", "merge", `{}`, collab.OutcomeInvalid, collab.ReasonRequestInvalid},
		{"a request the contract refuses", collab.OperationCreate, `{"title":"t"}`, collab.OutcomeInvalid, collab.ReasonRequestInvalid},
		{"a precondition the provider cannot compare", collab.OperationAssign,
			`{"ref":{"source":"local:fixture","id":"T-1"},"assignees":["a"],"expectedRevision":"r1"}`,
			collab.OutcomeUnsupported, collab.ReasonPreconditionUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			envelope := CallProviderOperation(context.Background(), env, collab.ContractTasks, tc.operation, json.RawMessage(tc.arguments))
			expectOutcome(t, envelope, tc.outcome, tc.reason)
		})
	}
	if _, err := os.Stat(fixture.record); !os.IsNotExist(err) {
		t.Error("a refused request started the provider")
	}
	// Without the precondition the same assignment reaches the provider.
	envelope := CallProviderOperation(context.Background(), env, collab.ContractTasks, collab.OperationAssign,
		json.RawMessage(`{"ref":{"source":"local:fixture","id":"T-1"},"assignees":["a"]}`))
	expectOutcome(t, envelope, collab.OutcomeOK, "")
}

func TestCollaboration_TheProviderReceivesTheValidatedRequestAndItsSettings(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/collaboration-providers", "one-route-two-entry-paths", "settings-and-selection-reach-the-provider")
	fixture := newCollabFixture(t, "ok", bindAll, nil)
	srv := fixture.server(t)
	envelope := callCollab(t, srv, "proposals.find", `{"change":{"base":"main","head":"topic"}}`)
	expectOutcome(t, envelope, collab.OutcomeOK, "")
	if envelope.Provider == nil || envelope.Provider.Name != collabProvider || envelope.Version != 1 || envelope.Contract != "proposals" {
		t.Errorf("envelope identity = %+v", envelope)
	}
	data, err := os.ReadFile(fixture.record)
	if err != nil {
		t.Fatal(err)
	}
	var request proto.ToolCallRequest
	if err := json.Unmarshal(data, &request); err != nil {
		t.Fatal(err)
	}
	if request.Name != "fixture.proposals.find" || request.Provider == nil ||
		request.Provider.Contract != "proposals" || request.Provider.Operation != "find" || request.Provider.Version != 1 ||
		string(request.Provider.Settings) != `{"repository":"local"}` {
		t.Fatalf("request = %s", data)
	}
	// The orchestrator forwards the normalized request: an explicit page.
	if !strings.Contains(string(request.Arguments), `"size":20`) {
		t.Errorf("arguments = %s, want the default page size made explicit", request.Arguments)
	}

	// A selection operation receives the resolved view, never the selector.
	envelope = callCollab(t, srv, "memory.context", `{"projects":["app"]}`)
	expectOutcome(t, envelope, collab.OutcomeOK, "")
	var list collab.MemoryListResult
	if err := json.Unmarshal(envelope.Result, &list); err != nil {
		t.Fatal(err)
	}
	record := list.Items[0]
	if !strings.Contains(record.Content, `"projects":["/packages/app"]`) || !strings.Contains(record.Content, "members=2") ||
		!strings.Contains(record.Content, `settings={"root":"memory"}`) {
		t.Errorf("the provider saw %s", record.Content)
	}
	if record.Identity.Workspace != "fixture" {
		t.Errorf("identity.workspace = %q, want the workspace name the orchestrator fills", record.Identity.Workspace)
	}
	envelope = callCollab(t, srv, "memory.context", `{"projects":["nowhere"]}`)
	expectOutcome(t, envelope, collab.OutcomeInvalid, collab.ReasonRequestInvalid)
}

func TestCollaboration_EmptyAndPaginatedResults(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/collaboration-providers", "bounded-pages", "empty-and-paginated-results")
	empty := newCollabFixture(t, "empty", bindAll, nil)
	envelope := callCollab(t, empty.server(t), "tasks.find", `{}`)
	expectOutcome(t, envelope, collab.OutcomeOK, "")
	var compact bytes.Buffer
	if err := json.Compact(&compact, envelope.Result); err != nil || !strings.Contains(compact.String(), `"items":[]`) {
		t.Errorf("an empty page must carry an empty list, not null: %s", envelope.Result)
	}

	fixture := newCollabFixture(t, "ok", bindAll, nil)
	srv := fixture.server(t)
	var ids []string
	cursor := ""
	for pages := 0; pages < 5; pages++ {
		arguments := `{"page":{"size":2}}`
		if cursor != "" {
			arguments = `{"page":{"size":2,"cursor":"` + cursor + `"}}`
		}
		envelope := callCollab(t, srv, "tasks.find", arguments)
		expectOutcome(t, envelope, collab.OutcomeOK, "")
		var list collab.TaskListResult
		if err := json.Unmarshal(envelope.Result, &list); err != nil {
			t.Fatal(err)
		}
		for _, task := range list.Items {
			ids = append(ids, task.Ref.ID)
		}
		if cursor = list.Page.Next; cursor == "" {
			break
		}
	}
	if !slices.Equal(ids, []string{"T-1", "T-2", "T-3", "T-4", "T-5"}) {
		t.Errorf("pages yielded %v", ids)
	}
}

func TestCollaboration_AnOversizedPageIsRefused(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/collaboration-providers", "bounded-pages", "an-oversized-page-is-refused")
	fixture := newCollabFixture(t, "overfull", bindAll, nil)
	envelope := callCollab(t, fixture.server(t), "tasks.find", `{"page":{"size":2}}`)
	expectOutcome(t, envelope, collab.OutcomeUnavailable, collab.ReasonProviderIdentity)
}

func TestCollaboration_AnAnswerForAnotherIdentityIsRefused(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/collaboration-providers", "exact-identity", "a-proposal-for-another-head-is-refused")
	spectest.Proves(t, "cli/collaboration-providers", "exact-identity", "a-mutation-answering-for-another-item-is-unresolved")
	fixture := newCollabFixture(t, "wrong-identity", bindAll, nil)
	srv := fixture.server(t)

	find := callCollab(t, srv, "proposals.find", `{"change":{"repository":"local","base":"main","head":"topic"}}`)
	expectOutcome(t, find, collab.OutcomeUnavailable, collab.ReasonProviderIdentity)

	upsert := callCollab(t, srv, "proposals.upsert", `{"change":{"base":"main","head":"topic"},"title":"t"}`)
	expectOutcome(t, upsert, collab.OutcomeUnresolved, collab.ReasonProviderIdentity)
	if upsert.Error.Retryable || !strings.Contains(upsert.Error.Reconcile, "proposals.find") {
		t.Errorf("an uncertain upsert must name its reconciliation and forbid a retry: %+v", upsert.Error)
	}

	get := callCollab(t, srv, "tasks.get", `{"ref":{"source":"local:fixture","id":"T-1"}}`)
	expectOutcome(t, get, collab.OutcomeUnavailable, collab.ReasonProviderIdentity)
	update := callCollab(t, srv, "tasks.update", `{"ref":{"source":"local:fixture","id":"T-1"},"title":"x"}`)
	expectOutcome(t, update, collab.OutcomeUnresolved, collab.ReasonProviderIdentity)

	// The exact association holds for the well-behaved provider.
	ok := newCollabFixture(t, "ok", bindAll, nil)
	good := callCollab(t, ok.server(t), "proposals.upsert", `{"change":{"repository":"local","base":"main","head":"topic"},"title":"t"}`)
	expectOutcome(t, good, collab.OutcomeOK, "")
	var upserted collab.ProposalUpsertResult
	if err := json.Unmarshal(good.Result, &upserted); err != nil {
		t.Fatal(err)
	}
	if upserted.Proposal.Change.Repository != "local" || upserted.Proposal.Change.Base != "main" || upserted.Proposal.Change.Head != "topic" {
		t.Errorf("change = %+v", upserted.Proposal.Change)
	}
}

func TestCollaboration_TimeoutsAndCancellation(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/collaboration-providers", "uncertain-writes-are-unresolved", "a-timed-out-read-is-unavailable-and-retryable")
	spectest.Proves(t, "cli/collaboration-providers", "uncertain-writes-are-unresolved", "a-timed-out-or-canceled-mutation-is-unresolved")
	fixture := newCollabFixture(t, "sleep", bindAll, func(tools map[string]proto.ToolDefinition) {
		for name, def := range tools {
			// The canceled checkpoint keeps the default timeout, so its
			// deadline cannot compete with the cancellation.
			if name == "fixture.memory.checkpoint" {
				continue
			}
			def.TimeoutMs = 300
			tools[name] = def
		}
	})
	env := ProviderEnvironment{WorkspaceRoot: fixture.root, Extensions: discovered(t, fixture.root)}

	read := CallProviderOperation(context.Background(), env, collab.ContractTasks, collab.OperationFind, json.RawMessage(`{}`))
	expectOutcome(t, read, collab.OutcomeUnavailable, collab.ReasonProviderTimeout)
	if !read.Error.Retryable {
		t.Error("a timed-out read is retryable")
	}
	write := CallProviderOperation(context.Background(), env, collab.ContractTasks, collab.OperationCreate,
		json.RawMessage(`{"title":"t","idempotencyKey":"k1"}`))
	expectOutcome(t, write, collab.OutcomeUnresolved, collab.ReasonProviderTimeout)
	if write.Error.Retryable || !strings.Contains(write.Error.Reconcile, "idempotencyKey") {
		t.Errorf("a timed-out create: %+v", write.Error)
	}

	checkpoint := json.RawMessage(`{"mission":"m1","precondition":{"mustNotExist":true},"idempotencyKey":"k1","content":"x"}`)
	// The timed-out calls above may have recorded their requests.
	if err := os.Remove(fixture.record); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	// A caller that cancels before the process starts stops a call that never
	// ran: even a mutation is unavailable, and repeating it can help.
	stopped, stop := context.WithCancel(context.Background())
	stop()
	notStarted := CallProviderOperation(stopped, env, collab.ContractMemory, collab.OperationCheckpoint, checkpoint)
	expectOutcome(t, notStarted, collab.OutcomeUnavailable, collab.ReasonProviderCanceled)
	if !notStarted.Error.Retryable {
		t.Errorf("a checkpoint canceled before its provider started is retryable: %+v", notStarted.Error)
	}
	if _, err := os.Stat(fixture.record); !os.IsNotExist(err) {
		t.Fatalf("the provider ran for a call canceled before it started: %v", err)
	}

	// The caller cancels once the provider has recorded its request: a known
	// point after the process started, whatever the host's speed.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan collab.Envelope, 1)
	go func() {
		done <- CallProviderOperation(ctx, env, collab.ContractMemory, collab.OperationCheckpoint, checkpoint)
	}()
	var canceled collab.Envelope
	for canceled.Outcome == "" {
		select {
		case envelope := <-done:
			t.Fatalf("the checkpoint ended before its provider started: %+v", envelope)
		case <-time.After(10 * time.Millisecond):
		}
		if _, err := os.Stat(fixture.record); err == nil {
			cancel()
			canceled = <-done
		}
	}
	expectOutcome(t, canceled, collab.OutcomeUnresolved, collab.ReasonProviderCanceled)
	if !strings.Contains(canceled.Error.Reconcile, "memory.mission") {
		t.Errorf("a canceled checkpoint must name its reconciliation: %+v", canceled.Error)
	}
}

func TestCollaboration_ACrashedOrGarbledProviderNeverReportsSuccess(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/collaboration-providers", "uncertain-writes-are-unresolved", "a-crashed-mutation-is-unresolved-with-a-reconciliation")
	for _, mode := range []string{"crash", "garbage"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			fixture := newCollabFixture(t, mode, bindAll, nil)
			srv := fixture.server(t)
			read := callCollab(t, srv, "tasks.get", `{"ref":{"source":"local:fixture","id":"T-1"}}`)
			if read.Outcome != collab.OutcomeUnavailable || read.Error.Retryable {
				t.Errorf("read = %s %+v", read.Outcome, read.Error)
			}
			write := callCollab(t, srv, "proposals.review",
				`{"ref":{"source":"local:fixture","id":"P-1"},"verdict":"approve","body":"ok","idempotencyKey":"k1"}`)
			expectOutcome(t, write, collab.OutcomeUnresolved, "")
			if write.Error.Retryable || !strings.Contains(write.Error.Reconcile, "idempotencyKey") {
				t.Errorf("write = %+v", write.Error)
			}
		})
	}
	// A provider that answers a failure keeps its outcome.
	fixture := newCollabFixture(t, "ok", bindAll, nil)
	srv := fixture.server(t)
	expectOutcome(t, callCollab(t, srv, "memory.mission", `{"mission":"m1"}`), collab.OutcomeNotFound, "mission.missing")
	expectOutcome(t, callCollab(t, srv, "memory.checkpoint",
		`{"mission":"m1","precondition":{"expectedRevision":"r1"},"idempotencyKey":"k","content":"x"}`),
		collab.OutcomeConflict, collab.ReasonRevisionConflict)
}

// TestCollaboration_NoCredentialReachesAnEnvelope sets a credential-named
// variable the provider inherits; the provider writes its value to stderr, to
// its error message, to a URL's userinfo and to a result. None of it may reach
// what the orchestrator emits.
func TestCollaboration_NoCredentialReachesAnEnvelope(t *testing.T) {
	spectest.Proves(t, "cli/collaboration-providers", "no-credential-leakage", "provider-stderr-never-reaches-an-envelope")
	spectest.Proves(t, "cli/collaboration-providers", "no-credential-leakage", "credential-values-are-redacted")
	spectest.Proves(t, "cli/collaboration-providers", "no-credential-leakage", "credential-settings-are-refused")
	t.Setenv(collabSecretEnv, collabSecretValue)
	for _, mode := range []string{"crash", "leak"} {
		fixture := newCollabFixture(t, mode, bindAll, nil)
		srv := fixture.server(t)
		for _, call := range []struct{ name, arguments string }{
			{"tasks.get", `{"ref":{"source":"local:fixture","id":"T-1"}}`},
			{"tasks.create", `{"title":"t","idempotencyKey":"k1"}`},
		} {
			resps := runSession(t, srv, req(1, "tools/call", echoCallParams{Name: call.name, Arguments: json.RawMessage(call.arguments)}))
			encoded, _ := json.Marshal(resps)
			if strings.Contains(string(encoded), collabSecretValue) {
				t.Errorf("%s %s leaked the credential: %s", mode, call.name, encoded)
			}
			if mode == "leak" && !strings.Contains(string(encoded), "[redacted]") {
				t.Errorf("%s %s: the redaction left no trace: %s", mode, call.name, encoded)
			}
		}
	}
	fixture := newCollabFixture(t, "ok", `{"tasks":{"provider":"@test/collab","version":1,"settings":{"auth":{"password":"hunter2hunter2"}}}}`, nil)
	envelope := callCollab(t, fixture.server(t), "tasks.capabilities", "")
	if strings.Contains(string(envelope.Result), "hunter2") {
		t.Errorf("a credential setting was echoed: %s", envelope.Result)
	}
	if !strings.Contains(string(envelope.Result), string(collab.ReasonBindingInvalid)) {
		t.Errorf("a credential setting must invalidate the binding: %s", envelope.Result)
	}
}
