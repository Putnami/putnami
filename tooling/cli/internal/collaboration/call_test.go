package collaboration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
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
	"go.putnami.dev/sdk/extension/scratch"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The collaboration route, end to end: a real extension manifest on disk,
// discovered by the extension discovery, bound from the workspace document,
// invoked through the provider transport.
//
// The provider is this test binary re-executed, not a script: a fixture that
// shells out depends on whatever the host happens to have, while the self-exec
// helper-process pattern depends on nothing but the Go toolchain already
// building this test. Its behavior is chosen per fixture through the
// provider's environment in the manifest, so one binary plays a well-behaved
// provider, a slow one, a crashing one and a lying one.

// TestMain isolates the machine-global putnami stores for the whole binary,
// so no test reaches the real home directory.
func TestMain(m *testing.M) {
	// scratch.New, not os.MkdirTemp: a killed binary cannot run the removal
	// below, and the next run reclaims what it left.
	dir, err := scratch.New("putnami-collaboration-test-")
	if err == nil {
		os.Setenv("PUTNAMI_STORE_DIR", filepath.Join(dir.Path(), "store"))
		os.Setenv("PUTNAMI_ARTIFACT_DIR", filepath.Join(dir.Path(), "artifacts"))
	}
	code := m.Run()
	_ = dir.Remove()
	os.Exit(code)
}

const (
	collabProviderModeEnv   = "PUTNAMI_COLLAB_PROVIDER_MODE"
	collabProviderRecordEnv = "PUTNAMI_COLLAB_PROVIDER_RECORD"
	// collabSecretEnv is credential-named, so the orchestrator redacts its
	// value from everything it emits.
	collabSecretEnv   = "PUTNAMI_COLLAB_TEST_API_TOKEN"
	collabSecretValue = "s3cr3t-value-that-must-not-leak"
	collabProvider    = "@test/collab"
	collabSource      = "local:fixture"
)

// TestCollaborationProviderHelperProcess is the fake provider. It does
// nothing unless a provider call spawned it.
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
		_, _ = os.Stdout.WriteString("not a response")
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
		key("missions", "sync"): func(_ context.Context, call collab.Call) (any, *collab.Failure) {
			input := call.Input.(*collab.MissionsSyncInput)
			result := &collab.MissionsSyncResult{
				Session:       collab.SessionStatus{State: input.State},
				Mission:       collab.MissionView{Key: input.Mission.Key, Title: input.Mission.Title},
				MinIntervalMs: collab.MinSyncIntervalMs, Now: "2026-10-26T09:00:00Z",
			}
			for _, reply := range input.Replies {
				result.Replies = append(result.Replies, collab.ReplyStatus{Message: reply.Message, State: collab.ReplyStateReplied})
			}
			if mode == "wrong-identity" {
				result.Mission.Key = "another-mission"
			}
			return result, nil
		},
		key("missions", "ask"): func(context.Context, collab.Call) (any, *collab.Failure) {
			decision := collab.MissionDecision{ID: "D-1", State: collab.DecisionStateOpen}
			if mode == "wrong-identity" {
				decision.State, decision.Answer = collab.DecisionStateAnswered, "yes"
			}
			return &collab.MissionsAskResult{Decision: decision}, nil
		},
		key("missions", "reply"): func(_ context.Context, call collab.Call) (any, *collab.Failure) {
			message := call.Input.(*collab.MissionsReplyInput).Message
			if mode == "wrong-identity" {
				message = "M-999"
			}
			return &collab.MissionsReplyResult{Message: collab.RepliedMessage{ID: message, State: collab.ReplyStateReplied}}, nil
		},
	}
}

// fakeProvider declares one contract of the fake provider, with no
// operation yet: this test binary, re-executed as the helper process, with the
// fixture's environment.
func fakeProvider(t *testing.T, env map[string]string) proto.ProviderDeclaration {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return proto.ProviderDeclaration{
		Version:    1,
		Command:    executable,
		Args:       []string{"-test.run=^TestCollaborationProviderHelperProcess$"},
		Env:        env,
		TimeoutMs:  20000,
		Operations: map[string]proto.ProviderOperation{},
	}
}

// fixtureWorkspace writes a minimal two-project workspace and returns its root.
func fixtureWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("putnami.workspace.json", `{"name":"fixture","includes":["packages/app","packages/lib"]}`)
	write("packages/app/putnami.json", `{"name":"app","type":"application","dependencies":["lib"]}`)
	write("packages/lib/putnami.json", `{"name":"lib","type":"library"}`)
	workspace.InvalidateLoadCache(dir)
	recordWorkspaceIndex(t, dir)
	return dir
}

// recordWorkspaceIndex writes the recorded workspace view a real workspace has
// after `putnami projects sync`: project identity and dependency edges are a
// projection of the provider answers.
func recordWorkspaceIndex(t *testing.T, dir string) {
	t.Helper()
	ws, err := workspace.Load(dir)
	if err != nil {
		t.Fatalf("load fixture workspace: %v", err)
	}
	if _, err := workspace.RefreshSnapshot(ws, workspace.SnapshotWritePolicy{}); err != nil {
		t.Fatalf("record fixture workspace index: %v", err)
	}
	workspace.InvalidateLoadCache(dir)
}

// collabFixture is one workspace binding the fake provider.
type collabFixture struct {
	root      string
	record    string
	providers map[string]proto.ProviderDeclaration
}

// newCollabFixture writes a workspace whose options.collaboration block is
// bindings, and a provider extension implementing every required operation of
// every contract plus tasks.assign (with no precondition support). mutate
// edits the provider's declarations before the manifest is written.
func newCollabFixture(t *testing.T, mode, bindings string, mutate func(map[string]proto.ProviderDeclaration)) collabFixture {
	t.Helper()
	root := fixtureWorkspace(t)
	record := filepath.Join(t.TempDir(), "request.json")
	env := map[string]string{collabProviderModeEnv: mode, collabProviderRecordEnv: record}
	providers := map[string]proto.ProviderDeclaration{}
	for _, contract := range collab.ContractNames {
		spec, _ := collab.Lookup(contract, 1)
		declaration := fakeProvider(t, env)
		for _, op := range spec.Operations {
			if !op.Required && op.Name != collab.OperationAssign {
				continue
			}
			preconditions := catalogPreconditions(op)
			if op.Name == collab.OperationAssign {
				preconditions = collab.PreconditionsNone
			}
			declaration.Operations[op.Name] = collabOperation(t, contract, op.Name, preconditions)
		}
		providers[contract] = declaration
	}
	if mutate != nil {
		mutate(providers)
	}
	manifest, err := json.Marshal(proto.Manifest{Name: collabProvider, Version: "1.2.3", CLIContract: protocolcli.ProviderContract, Providers: providers})
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
	return collabFixture{root: root, record: record, providers: providers}
}

const bindAll = `{"tasks":{"provider":"@test/collab","version":1},` +
	`"proposals":{"provider":"@test/collab","version":1,"settings":{"repository":"local"}},` +
	`"memory":{"provider":"@test/collab","version":1,"settings":{"root":"memory"}},` +
	`"missions":{"provider":"@test/collab","version":1}}`

// env is what an entry path hands the route for this fixture: the discovered
// extensions and a workspace view attached through AttachWorkspaceView, the
// way the command line attaches it.
func (f collabFixture) env(t *testing.T) ProviderEnvironment {
	t.Helper()
	return ProviderEnvironment{
		WorkspaceRoot: f.root,
		Extensions:    discovered(t, f.root),
		AttachView:    fixtureWorkspaceView(f.root),
	}
}

// call routes one operation and decodes the envelope an entry path prints,
// checking that the contract accepts it.
func (f collabFixture) call(t *testing.T, contract, operation, arguments string) collab.Envelope {
	t.Helper()
	var args json.RawMessage
	if arguments != "" {
		args = json.RawMessage(arguments)
	}
	envelope := CallProviderOperation(context.Background(), f.env(t), contract, operation, args)
	text, err := EnvelopeText(envelope, true)
	if err != nil {
		t.Fatal(err)
	}
	parsed, diags := collab.ParseEnvelope([]byte(text))
	if diags != nil {
		t.Fatalf("%s.%s answered an envelope the contract refuses: %v\n%s", contract, operation, diags, text)
	}
	return *parsed
}

// recorded reads the ProviderRequest the provider process read on stdin. It
// decodes strictly: a member the wire does not declare fails the test.
func (f collabFixture) recorded(t *testing.T) collab.ProviderRequest {
	t.Helper()
	data, err := os.ReadFile(f.record)
	if err != nil {
		t.Fatalf("the provider recorded no request: %v", err)
	}
	var request collab.ProviderRequest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		t.Fatalf("the provider read no ProviderRequest: %v\n%s", err, data)
	}
	return request
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

func boolPointer(value bool) *bool { return &value }

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

func TestCollaboration_OnlyADeclaredBlockRoutes(t *testing.T) {
	t.Parallel()
	if Declared(newCollabFixture(t, "ok", "", nil).root) {
		t.Error("a workspace without options.collaboration declares the block")
	}
	if !Declared(newCollabFixture(t, "ok", bindAll, nil).root) {
		t.Error("a workspace with options.collaboration does not declare the block")
	}
	if Declared(t.TempDir()) {
		t.Error("a directory without a workspace document declares the block")
	}
}

// TestCollaboration_ProviderOperationsAreReachableOnlyThroughTheBinding: the
// route reaches a provider operation only as the operation of a contract the
// workspace binds. Neither the extension's name nor a dotted
// contract.operation is a contract or an operation, and an installed provider
// is never started for a contract the workspace does not bind to it.
func TestCollaboration_ProviderOperationsAreReachableOnlyThroughTheBinding(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/collaboration-providers", "one-route-two-entry-paths", "provider-operations-are-reachable-only-through-the-binding")
	fixture := newCollabFixture(t, "ok", bindAll, nil)
	env := fixture.env(t)
	for _, name := range []struct{ contract, operation string }{
		{collabProvider, "tasks.find"},
		{collabProvider + ".tasks", "find"},
		{collab.ContractTasks, "tasks.find"},
	} {
		envelope := CallProviderOperation(context.Background(), env, name.contract, name.operation, json.RawMessage(`{}`))
		expectOutcome(t, envelope, collab.OutcomeInvalid, collab.ReasonRequestInvalid)
	}
	if _, err := os.Stat(fixture.record); !os.IsNotExist(err) {
		t.Fatal("a call naming the provider instead of a bound operation started the provider")
	}
	expectOutcome(t, fixture.call(t, collab.ContractTasks, collab.OperationFind, `{}`), collab.OutcomeOK, "")
	if request := fixture.recorded(t); request.Provider == nil ||
		request.Provider.Contract != collab.ContractTasks || request.Provider.Operation != collab.OperationFind {
		t.Errorf("the bound operation reached the provider as %+v, want tasks.find", request.Provider)
	}

	tasksOnly := newCollabFixture(t, "ok", `{"tasks":{"provider":"@test/collab","version":1}}`, nil)
	expectOutcome(t, tasksOnly.call(t, collab.ContractMemory, collab.OperationContext, `{}`),
		collab.OutcomeUnsupported, collab.ReasonBindingMissing)
	if _, err := os.Stat(tasksOnly.record); !os.IsNotExist(err) {
		t.Error("an installed provider was started for a contract the workspace does not bind")
	}
}

// TestCollaboration_CapabilitiesMarkAHostOnlyOperation: missions.capabilities
// lists sync, as it lists every operation, and says that only the host calls
// it.
func TestCollaboration_CapabilitiesMarkAHostOnlyOperation(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/collaboration-providers", "host-only-operations", "capabilities-mark-a-host-only-operation")
	fixture := newCollabFixture(t, "ok", bindAll, nil)
	envelope := fixture.call(t, collab.ContractMissions, collab.OperationCapabilities, "")
	var capabilities collab.Capabilities
	if err := json.Unmarshal(envelope.Result, &capabilities); err != nil {
		t.Fatal(err)
	}
	hostOnly := map[string]bool{}
	for _, op := range capabilities.Operations {
		hostOnly[op.Name] = op.HostOnly
	}
	if want := map[string]bool{collab.OperationAsk: false, collab.OperationReply: false, collab.OperationSync: true}; !maps.Equal(hostOnly, want) {
		t.Errorf("missions.capabilities marks host-only %v, want %v", hostOnly, want)
	}
	envelope = fixture.call(t, collab.ContractTasks, collab.OperationCapabilities, "")
	if strings.Contains(string(envelope.Result), "hostOnly") {
		t.Errorf("tasks.capabilities names hostOnly: %s", envelope.Result)
	}
	if _, err := os.Stat(fixture.record); !os.IsNotExist(err) {
		t.Error("a capabilities call started the provider")
	}
}

func TestCollaboration_CapabilitiesDescribeTheBinding(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/collaboration-providers", "capability-discovery", "capabilities-report-every-operation-and-its-support")
	fixture := newCollabFixture(t, "ok", bindAll, nil)
	envelope := fixture.call(t, collab.ContractTasks, collab.OperationCapabilities, "")
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
	// The command line renders its help from the same resolution.
	direct := Capabilities(fixture.root, collab.ContractTasks, discovered(t, fixture.root), nil)
	if direct.Status != capabilities.Status || direct.Provider == nil || direct.Provider.Name != collabProvider ||
		len(direct.Operations) != len(capabilities.Operations) {
		t.Errorf("Capabilities = %+v, want the routed capabilities %+v", direct, capabilities)
	}
}

func TestCollaboration_AnUnboundContractIsUnsupportedAndNamesItsCandidates(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/collaboration-providers", "explicit-binding", "an-unbound-contract-is-unsupported-and-names-the-binding")
	spectest.Proves(t, "cli/collaboration-providers", "explicit-binding", "candidates-are-reported-never-selected")
	fixture := newCollabFixture(t, "ok", `{"tasks":{"provider":"@test/collab","version":1}}`, nil)
	envelope := fixture.call(t, collab.ContractMemory, collab.OperationCapabilities, "")
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

func TestCollaboration_BindingFailuresAreExplicit(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/collaboration-providers", "explicit-binding", "an-ambiguous-binding-is-refused")
	spectest.Proves(t, "cli/collaboration-providers", "explicit-binding", "a-missing-or-disabled-provider-is-unavailable")
	spectest.Proves(t, "cli/collaboration-providers", "capability-discovery", "an-unsupported-version-or-incomplete-provider-is-refused")
	cases := []struct {
		name     string
		bindings string
		mutate   func(map[string]proto.ProviderDeclaration)
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
			func(providers map[string]proto.ProviderDeclaration) { delete(providers["tasks"].Operations, "transition") },
			false, collab.OutcomeUnsupported, collab.ReasonBindingIncomplete},
		{"a provider lacking an operation the binding requires", `{"tasks":{"provider":"@test/collab","version":1,"require":["claim"]}}`,
			nil, false, collab.OutcomeUnsupported, collab.ReasonBindingIncomplete},
		{"a provider declaring a precondition the operation does not take", `{"tasks":{"provider":"@test/collab","version":1}}`,
			func(providers map[string]proto.ProviderDeclaration) {
				find := providers["tasks"].Operations["find"]
				find.Preconditions = string(collab.PreconditionsAtomic)
				providers["tasks"].Operations["find"] = find
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

// TestCollaboration_ABrokenBindingAnswersItsFailure: an operation the binding
// promised answers why it cannot serve, without starting the provider.
func TestCollaboration_ABrokenBindingAnswersItsFailure(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "collaboration/provider-contract", "truthful-provider-declarations", "the-cli-refuses-a-call-to-a-withdrawn-contract-version")
	fixture := newCollabFixture(t, "ok", `{"tasks":{"provider":"@test/collab","version":1}}`, func(providers map[string]proto.ProviderDeclaration) {
		update := providers["tasks"].Operations["update"]
		update.Preconditions = ""
		providers["tasks"].Operations["update"] = update
	})
	envelope := fixture.call(t, collab.ContractTasks, collab.OperationCreate, `{"title":"t","idempotencyKey":"k"}`)
	expectOutcome(t, envelope, collab.OutcomeUnsupported, collab.ReasonBindingIncomplete)
	if !strings.Contains(envelope.Error.Message, "tasks.update must declare preconditions") {
		t.Errorf("message = %q", envelope.Error.Message)
	}
	if _, err := os.Stat(fixture.record); !os.IsNotExist(err) {
		t.Error("a broken binding started the provider")
	}
}

func TestCollaboration_OperationsThePathRefusesNeverReachTheProvider(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "collaboration/provider-contract", "refused-before-the-provider", "the-cli-refuses-an-operation-the-provider-does-not-offer")
	spectest.Proves(t, "collaboration/provider-contract", "refused-before-the-provider", "the-cli-refuses-a-precondition-the-provider-cannot-enforce")
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
	envelope := fixture.call(t, collab.ContractProposals, collab.OperationFind, `{"change":{"base":"main","head":"topic"}}`)
	expectOutcome(t, envelope, collab.OutcomeOK, "")
	if envelope.Provider == nil || envelope.Provider.Name != collabProvider || envelope.Version != 1 || envelope.Contract != "proposals" {
		t.Errorf("envelope identity = %+v", envelope)
	}
	request := fixture.recorded(t)
	if request.Provider == nil ||
		request.Provider.Contract != "proposals" || request.Provider.Operation != "find" || request.Provider.Version != 1 ||
		string(request.Provider.Settings) != `{"repository":"local"}` {
		t.Fatalf("request = %+v", request)
	}
	// The orchestrator forwards the normalized request: an explicit page.
	if !strings.Contains(string(request.Arguments), `"size":20`) {
		t.Errorf("arguments = %s, want the default page size made explicit", request.Arguments)
	}

	// A selection operation receives the resolved view, never the selector.
	envelope = fixture.call(t, collab.ContractMemory, collab.OperationContext, `{"projects":["app"]}`)
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
	envelope = fixture.call(t, collab.ContractMemory, collab.OperationContext, `{"projects":["nowhere"]}`)
	expectOutcome(t, envelope, collab.OutcomeInvalid, collab.ReasonRequestInvalid)
}

func TestCollaboration_EmptyAndPaginatedResults(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "collaboration/provider-contract", "bounded-pages", "the-cli-answers-empty-and-paginated-lists")
	empty := newCollabFixture(t, "empty", bindAll, nil)
	envelope := empty.call(t, collab.ContractTasks, collab.OperationFind, `{}`)
	expectOutcome(t, envelope, collab.OutcomeOK, "")
	var compact bytes.Buffer
	if err := json.Compact(&compact, envelope.Result); err != nil || !strings.Contains(compact.String(), `"items":[]`) {
		t.Errorf("an empty page must carry an empty list, not null: %s", envelope.Result)
	}

	fixture := newCollabFixture(t, "ok", bindAll, nil)
	var ids []string
	cursor := ""
	for pages := 0; pages < 5; pages++ {
		arguments := `{"page":{"size":2}}`
		if cursor != "" {
			arguments = `{"page":{"size":2,"cursor":"` + cursor + `"}}`
		}
		envelope := fixture.call(t, collab.ContractTasks, collab.OperationFind, arguments)
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
	spectest.Proves(t, "collaboration/provider-contract", "bounded-pages", "the-cli-refuses-a-page-above-the-requested-size")
	fixture := newCollabFixture(t, "overfull", bindAll, nil)
	envelope := fixture.call(t, collab.ContractTasks, collab.OperationFind, `{"page":{"size":2}}`)
	expectOutcome(t, envelope, collab.OutcomeUnavailable, collab.ReasonProviderIdentity)
}

func TestCollaboration_AnAnswerForAnotherIdentityIsRefused(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "collaboration/provider-contract", "exact-identity", "the-cli-refuses-a-read-that-answers-for-something-else")
	spectest.Proves(t, "collaboration/provider-contract", "exact-identity", "the-cli-makes-a-mutation-that-answers-for-something-else-unresolved")
	fixture := newCollabFixture(t, "wrong-identity", bindAll, nil)

	find := fixture.call(t, collab.ContractProposals, collab.OperationFind, `{"change":{"repository":"local","base":"main","head":"topic"}}`)
	expectOutcome(t, find, collab.OutcomeUnavailable, collab.ReasonProviderIdentity)

	upsert := fixture.call(t, collab.ContractProposals, collab.OperationUpsert, `{"change":{"base":"main","head":"topic"},"title":"t"}`)
	expectOutcome(t, upsert, collab.OutcomeUnresolved, collab.ReasonProviderIdentity)
	if upsert.Error.Retryable || !strings.Contains(upsert.Error.Reconcile, "proposals.find") {
		t.Errorf("an uncertain upsert must name its reconciliation and forbid a retry: %+v", upsert.Error)
	}

	get := fixture.call(t, collab.ContractTasks, collab.OperationGet, `{"ref":{"source":"local:fixture","id":"T-1"}}`)
	expectOutcome(t, get, collab.OutcomeUnavailable, collab.ReasonProviderIdentity)
	update := fixture.call(t, collab.ContractTasks, collab.OperationUpdate, `{"ref":{"source":"local:fixture","id":"T-1"},"title":"x"}`)
	expectOutcome(t, update, collab.OutcomeUnresolved, collab.ReasonProviderIdentity)

	// The exact association holds for the well-behaved provider.
	ok := newCollabFixture(t, "ok", bindAll, nil)
	good := ok.call(t, collab.ContractProposals, collab.OperationUpsert, `{"change":{"repository":"local","base":"main","head":"topic"},"title":"t"}`)
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
	spectest.Proves(t, "collaboration/provider-contract", "uncertain-writes-are-unresolved", "the-cli-makes-a-timed-out-read-unavailable-and-retryable")
	spectest.Proves(t, "collaboration/provider-contract", "uncertain-writes-are-unresolved", "the-cli-makes-a-timed-out-or-canceled-mutation-unresolved")
	fixture := newCollabFixture(t, "sleep", bindAll, func(providers map[string]proto.ProviderDeclaration) {
		for contract, declaration := range providers {
			// The canceled checkpoint keeps its long timeout, so its deadline
			// cannot compete with the cancellation.
			if contract == collab.ContractMemory {
				continue
			}
			declaration.TimeoutMs = 300
			providers[contract] = declaration
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
	spectest.Proves(t, "collaboration/provider-contract", "uncertain-writes-are-unresolved", "the-cli-makes-a-crashed-mutation-unresolved-with-a-reconcile")
	spectest.Proves(t, "collaboration/provider-contract", "invalid-answers-are-refused", "the-cli-keeps-a-provider-failure-outcome")
	for _, mode := range []string{"crash", "garbage"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			fixture := newCollabFixture(t, mode, bindAll, nil)
			read := fixture.call(t, collab.ContractTasks, collab.OperationGet, `{"ref":{"source":"local:fixture","id":"T-1"}}`)
			if read.Outcome != collab.OutcomeUnavailable || read.Error.Retryable {
				t.Errorf("read = %s %+v", read.Outcome, read.Error)
			}
			write := fixture.call(t, collab.ContractProposals, collab.OperationReview,
				`{"ref":{"source":"local:fixture","id":"P-1"},"verdict":"approve","body":"ok","idempotencyKey":"k1"}`)
			expectOutcome(t, write, collab.OutcomeUnresolved, "")
			if write.Error.Retryable || !strings.Contains(write.Error.Reconcile, "idempotencyKey") {
				t.Errorf("write = %+v", write.Error)
			}
		})
	}
	// A provider that answers a failure keeps its outcome.
	fixture := newCollabFixture(t, "ok", bindAll, nil)
	expectOutcome(t, fixture.call(t, collab.ContractMemory, collab.OperationMission, `{"mission":"m1"}`), collab.OutcomeNotFound, "mission.missing")
	expectOutcome(t, fixture.call(t, collab.ContractMemory, collab.OperationCheckpoint,
		`{"mission":"m1","precondition":{"expectedRevision":"r1"},"idempotencyKey":"k","content":"x"}`),
		collab.OutcomeConflict, collab.ReasonRevisionConflict)
}

// TestCollaboration_NoCredentialReachesAnEnvelope sets a credential-named
// variable the provider inherits; the provider writes its value to stderr, to
// its error message, to a URL's userinfo and to a result. None of it may reach
// the envelope an entry path prints, indented or compact.
func TestCollaboration_NoCredentialReachesAnEnvelope(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "no-credential-in-an-envelope", "the-cli-keeps-provider-stderr-out-of-envelopes")
	spectest.Proves(t, "collaboration/provider-contract", "no-credential-in-an-envelope", "the-cli-redacts-every-credential-value")
	spectest.Proves(t, "collaboration/provider-contract", "no-credential-in-a-binding", "the-cli-reports-a-credential-setting-without-echoing-it")
	t.Setenv(collabSecretEnv, collabSecretValue)
	for _, mode := range []string{"crash", "leak"} {
		fixture := newCollabFixture(t, mode, bindAll, nil)
		for _, call := range []struct{ operation, arguments string }{
			{collab.OperationGet, `{"ref":{"source":"local:fixture","id":"T-1"}}`},
			{collab.OperationCreate, `{"title":"t","idempotencyKey":"k1"}`},
		} {
			envelope := CallProviderOperation(context.Background(), fixture.env(t), collab.ContractTasks, call.operation, json.RawMessage(call.arguments))
			for _, indent := range []bool{true, false} {
				text, err := EnvelopeText(envelope, indent)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(text, collabSecretValue) {
					t.Errorf("%s tasks.%s leaked the credential: %s", mode, call.operation, text)
				}
				if mode == "leak" && !strings.Contains(text, "[redacted]") {
					t.Errorf("%s tasks.%s: the redaction left no trace: %s", mode, call.operation, text)
				}
			}
		}
	}
	fixture := newCollabFixture(t, "ok", `{"tasks":{"provider":"@test/collab","version":1,"settings":{"auth":{"password":"hunter2hunter2"}}}}`, nil)
	envelope := fixture.call(t, collab.ContractTasks, collab.OperationCapabilities, "")
	if strings.Contains(string(envelope.Result), "hunter2") {
		t.Errorf("a credential setting was echoed: %s", envelope.Result)
	}
	if !strings.Contains(string(envelope.Result), string(collab.ReasonBindingInvalid)) {
		t.Errorf("a credential setting must invalidate the binding: %s", envelope.Result)
	}
}
