package runnerprovider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/cli/model/extension"
	"go.putnami.dev/protocol/features/spectest"
	runner "go.putnami.dev/protocol/runner"
	"go.putnami.dev/sdk/extension/envkeys"
	"go.putnami.dev/sdk/extension/proctree"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
)

// fakeProvider answers the RPC in-process over pipes with a scripted
// initialize result and echo-shaped op results.
type fakeProvider struct {
	initialize    runner.InitializeResult
	failOp        runner.ProviderOp
	malformed     bool
	followState   string
	followRecords []runner.OutputRecord
	lookup        runner.LookupResult
}

func (f fakeProvider) serve(stdin io.Reader, stdout io.WriteCloser) {
	defer func() { _ = stdout.Close() }()
	scanner := bufio.NewScanner(stdin)
	for scanner.Scan() {
		request, err := runner.ParseProviderRequest(scanner.Bytes())
		if err != nil {
			return
		}
		if f.malformed {
			_, _ = stdout.Write([]byte("{\"protocolVersion\":1,\"id\":1,\"ok\":true,\"trusted\":true}\n"))
			return
		}
		response := runner.ProviderResponse{ProtocolVersion: runner.ProviderProtocolVersion, ID: request.ID, OK: true}
		switch {
		case request.Op == f.failOp:
			response.OK, response.Error = false, &runner.ProviderError{Code: "denied", Message: "scripted failure"}
		case request.Op == runner.OpInitialize:
			response.Payload, _ = runner.MarshalPayload(f.initialize)
		case request.Op == runner.OpPrepare:
			response.Payload, _ = runner.MarshalPayload(runner.PrepareResult{MissingBlobs: []string{}})
		case request.Op == runner.OpSubmit:
			response.Payload, _ = runner.MarshalPayload(runner.SubmitResult{Attempt: "a-1", State: runner.StateQueued})
		case request.Op == runner.OpLookup:
			response.Payload, _ = runner.MarshalPayload(f.lookup)
		case request.Op == runner.OpCancel:
			response.Payload, _ = runner.MarshalPayload(runner.CancelResult{State: runner.StateCanceled})
		case request.Op == runner.OpFollow:
			code := 0
			state := runner.StateCompleted
			if f.followState != "" {
				state = f.followState
			}
			records := []runner.OutputRecord{{Cursor: 1, Stream: "stdout", Line: "hello"}}
			if f.followRecords != nil {
				records = f.followRecords
			}
			response.Payload, _ = runner.MarshalPayload(runner.FollowResult{State: state, Cursor: 1, Records: records, ExitCode: &code})
		case request.Op == runner.OpFetch:
			response.Payload, _ = runner.MarshalPayload(runner.FetchResult{Bundle: runner.SessionBundle{Attempt: "a-1", Sessions: []runner.BundleSession{}}})
		case request.Op == runner.OpShutdown:
			response.Payload = json.RawMessage(`{}`)
		}
		line, _ := json.Marshal(response)
		if _, err := stdout.Write(append(line, '\n')); err != nil {
			return
		}
		if request.Op == runner.OpShutdown {
			return
		}
	}
}

func connectFake(t *testing.T, provider fakeProvider) *Session {
	t.Helper()
	requestReader, requestWriter := io.Pipe()
	responseReader, responseWriter := io.Pipe()
	go provider.serve(requestReader, responseWriter)
	session := Connect(requestWriter, responseReader)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// minimalRequest is a well-formed request: every array and object present,
// as the strict provider parser requires on the wire.
func minimalRequest() runner.ExecutionRequest {
	return runner.ExecutionRequest{
		Version:  runner.ExecutionRequestVersion,
		Protocol: runner.ProtocolBlock{Version: runner.ProviderProtocolVersion, Capabilities: []string{}},
		Source:   runner.SourceBlock{Digest: runner.BlobDigest(nil), IndexDigest: strings.Repeat("0", 64), Versions: []runner.LineVersion{}},
		Invocation: runner.InvocationBlock{Commands: []string{"build"}, Params: map[string]runner.ParamValue{},
			Flags: runner.ExecutionFlags{ResourceBudgets: map[string]int{}}, Cwd: "."},
		Selection: runner.SelectionBlock{RequestedMode: runner.SelectionModeProjects, Mode: runner.SelectionModeProjects, Scoped: true,
			Projects: []string{"/app"}, ChangedPaths: []string{}, Diagnostics: []string{}, NoCacheProjects: []string{}},
		Plan:        runner.PlanBlock{Tasks: []runner.PlannedTask{}},
		Environment: runner.EnvironmentBlock{CLI: runner.PinnedCLI{Source: runner.CLISourceUnpinned}, Extensions: []runner.PinnedComponent{}, Toolchains: []runner.PinnedComponent{}, Platform: runner.Platform{OS: "linux", Arch: "arm64"}},
		Control:     runner.ControlBlock{Caller: runner.CallerCLI, IdempotencyKey: strings.Repeat("a", 32), Deadline: "2030-01-01T00:00:00Z"},
	}
}

func readyProvider() fakeProvider {
	return fakeProvider{initialize: runner.InitializeResult{ProtocolVersion: runner.ProviderProtocolVersion, ProviderName: "@fixture/runner",
		Capabilities: []string{runner.CapabilityExecutionRequestV1, runner.CapabilitySessionBundleV1}, Ready: true}}
}

func TestSessionNegotiatesAndDrivesEveryOp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	session := connectFake(t, readyProvider())
	initialized, err := session.Initialize(ctx, &runner.InitializeParams{ProtocolVersion: runner.ProviderProtocolVersion, ExchangeDir: "/x", Capabilities: []string{}})
	if err != nil || initialized.ProviderName != "@fixture/runner" {
		t.Fatalf("initialize = %+v, %v", initialized, err)
	}
	manifest := runner.SourceManifest{Version: runner.SourceManifestVersion, Entries: []runner.SourceEntry{}}
	if prepared, err := session.Prepare(ctx, &runner.PrepareParams{SourceDigest: runner.BlobDigest(nil), Manifest: manifest}); err != nil || len(prepared.MissingBlobs) != 0 {
		t.Fatalf("prepare = %+v, %v", prepared, err)
	}
	submitted, err := session.Submit(ctx, &runner.SubmitParams{Request: minimalRequest(), Manifest: manifest})
	if err != nil || submitted.Attempt != "a-1" {
		t.Fatalf("submit = %+v, %v", submitted, err)
	}
	followed, err := session.Follow(ctx, &runner.FollowParams{Attempt: "a-1"})
	if err != nil || followed.State != runner.StateCompleted || len(followed.Records) != 1 || followed.Records[0].Line != "hello" {
		t.Fatalf("follow = %+v, %v", followed, err)
	}
	fetched, err := session.Fetch(ctx, &runner.FetchParams{Attempt: "a-1"})
	if err != nil || fetched.Bundle.Attempt != "a-1" {
		t.Fatalf("fetch = %+v, %v", fetched, err)
	}
	missed, err := session.Lookup(ctx, &runner.LookupParams{IdempotencyKey: strings.Repeat("a", 32)})
	if err != nil || missed.Attempt != "" {
		t.Fatalf("lookup miss = %+v, %v", missed, err)
	}
	canceled, err := session.Cancel(ctx, &runner.CancelParams{Attempt: "a-1"})
	if err != nil || canceled.State != runner.StateCanceled {
		t.Fatalf("cancel = %+v, %v", canceled, err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Prepare(ctx, &runner.PrepareParams{}); err == nil {
		t.Fatal("an op after close succeeded")
	}
}

func TestSessionInitializeFailsClosedOnIncompatibleProviders(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := map[string]struct {
		provider fakeProvider
		want     string
	}{
		"other protocol":     {fakeProvider{initialize: runner.InitializeResult{ProtocolVersion: 2, Capabilities: []string{runner.CapabilityExecutionRequestV1, runner.CapabilitySessionBundleV1}, Ready: true}}, "protocol version"},
		"missing capability": {fakeProvider{initialize: runner.InitializeResult{ProtocolVersion: 1, Capabilities: []string{runner.CapabilitySessionBundleV1}, Ready: true}}, "does not support"},
		"not ready":          {fakeProvider{initialize: runner.InitializeResult{ProtocolVersion: 1, Capabilities: []string{runner.CapabilityExecutionRequestV1, runner.CapabilitySessionBundleV1}, Ready: false, Reason: "no capacity"}}, "no capacity"},
		"op failure":         {fakeProvider{failOp: runner.OpInitialize}, "scripted failure"},
		"malformed response": {fakeProvider{malformed: true}, "malformed"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			session := connectFake(t, c.provider)
			_, err := session.Initialize(ctx, &runner.InitializeParams{ProtocolVersion: runner.ProviderProtocolVersion, ExchangeDir: "/x", Capabilities: []string{}})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("initialize error = %v, want %q", err, c.want)
			}
			var opErr *OpError
			if name == "op failure" && (!errors.As(err, &opErr) || opErr.Code != "denied") {
				t.Fatalf("op failure was not typed: %v", err)
			}
		})
	}
}

func TestClientOffersInvocationProvidersOnlyForARequestThatCarriesThem(t *testing.T) {
	t.Parallel()
	base := []string{runner.CapabilityExecutionRequestV1, runner.CapabilitySessionBundleV1}
	if got := requestCapabilities(runner.ExecutionRequest{}); !slices.Equal(got, base) {
		t.Fatalf("a request without providers offers %v, want %v", got, base)
	}
	var request runner.ExecutionRequest
	request.Invocation.Providers = []string{"install"}
	want := append(slices.Clone(base), runner.CapabilityInvocationProvidersV1)
	if got := requestCapabilities(request); !slices.Equal(got, want) {
		t.Fatalf("a request with providers offers %v, want %v", got, want)
	}
}

func TestSessionSubmitRequiresAnAttemptReference(t *testing.T) {
	t.Parallel()
	provider := readyProvider()
	provider.failOp = runner.OpSubmit
	session := connectFake(t, provider)
	manifest := runner.SourceManifest{Version: runner.SourceManifestVersion, Entries: []runner.SourceEntry{}}
	if _, err := session.Submit(context.Background(), &runner.SubmitParams{Request: minimalRequest(), Manifest: manifest}); err == nil || !strings.Contains(err.Error(), "scripted failure") {
		t.Fatalf("submit failure = %v", err)
	}
}

func TestLaunchSpecForResolvesTheReservedCommandOnly(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ext := &extension.ExtensionDescription{Name: "@fixture/runner", Version: "1.0.0", Path: filepath.Join(root, "ext"),
		Commands: map[string]string{runner.ProviderCommandName: "provider"},
		Jobs: map[string]*extension.JobDefinition{runner.ProviderCommandName: {
			Command: "{extensionRoot}/provider", Args: []string{"--root", "{workspaceRoot}"}, Cwd: "{extensionRoot}", Env: map[string]string{"FIXTURE": "{workspaceRoot}"},
		}}}
	provider := &extension.ResolvedProvider{ExtensionName: "@fixture/runner", Command: runner.ProviderCommandName}
	spec, err := LaunchSpecFor(context.Background(), root, []*extension.ExtensionDescription{ext}, provider)
	if err != nil {
		t.Fatal(err)
	}
	// The expansion keeps the manifest's "/" after {extensionRoot}, which
	// Windows accepts in a program path, so the command compares cleaned.
	if filepath.Clean(spec.Command) != filepath.Join(root, "ext", "provider") || strings.Join(spec.Args, " ") != "--root "+root || spec.Dir != filepath.Join(root, "ext") {
		// Never the whole spec: its Env carries this process's environment.
		t.Fatalf("launch spec command = %q, args = %q, dir = %q", spec.Command, spec.Args, spec.Dir)
	}
	found := false
	for _, entry := range spec.Env {
		found = found || entry == "FIXTURE="+root
	}
	if !found {
		t.Fatalf("job env FIXTURE = %q, want %q", envkeys.Host.Last(spec.Env, "FIXTURE"), root)
	}
	if _, err := LaunchSpecFor(context.Background(), root, nil, provider); err == nil {
		t.Fatal("unloaded extension resolved")
	}
	if _, err := LaunchSpecFor(context.Background(), root, []*extension.ExtensionDescription{ext}, nil); err == nil {
		t.Fatal("nil provider resolved")
	}
	ext.Jobs[runner.ProviderCommandName].Command = " "
	if _, err := LaunchSpecFor(context.Background(), root, []*extension.ExtensionDescription{ext}, provider); err == nil {
		t.Fatal("empty executable resolved")
	}
}

func TestSpawnReportsAMissingExecutable(t *testing.T) {
	t.Parallel()
	if _, err := Spawn(context.Background(), LaunchSpec{Command: filepath.Join(t.TempDir(), "missing")}, os.Stderr); err == nil {
		t.Fatal("spawn of a missing executable succeeded")
	}
}

// A provider that exits while a process it started still holds its output
// ends the session within seconds, not at the caller's deadline, and the
// process it left behind is killed.
func TestAProviderExitIsSeenWhileItsChildHoldsItsOutput(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/portable-runner", "durable-lifecycle", "provider-exit-is-seen-while-a-child-holds-output")
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	provider := fixtureproc.Write(t, filepath.Join(dir, "provider"), fixtureproc.Program{HoldOutput: release, ReadLines: 1})
	session, err := Spawn(context.Background(), LaunchSpec{Command: provider, Env: os.Environ()}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	holder := fixtureproc.Holder(t, release)
	// A copy that outlived a failed test lets go instead of waiting a minute.
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o644) })
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	started := time.Now()
	_, err = session.Initialize(ctx, &runner.InitializeParams{ProtocolVersion: runner.ProviderProtocolVersion, ExchangeDir: dir, Capabilities: []string{}})
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), "output stayed open") {
		t.Fatalf("initialize after the provider exited = %v, want the session to end naming the held output", err)
	}
	// The copy also holds stderr, so Wait returns only after WaitDelay; the
	// bound leaves room for that and a loaded machine. The fault waits out
	// the one-minute deadline.
	if elapsed > 30*time.Second {
		t.Fatalf("the session ended %s after the provider exited, want within seconds", elapsed)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(30 * time.Second); proctree.ProcessAlive(holder); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the process the provider left holding its output (pid %d) outlived the session", holder)
		}
	}
}

// observingClient wires a client over the in-process fake with a record that
// already names attempt a-1, the state every observation test starts from.
func observingClient(t *testing.T, provider fakeProvider, cursor int64) (*client, *strings.Builder, *strings.Builder) {
	t.Helper()
	var stdout, stderr strings.Builder
	c := &client{wsRoot: t.TempDir(), provider: "@fixture/runner", stdout: &stdout, stderr: &stderr, session: connectFake(t, provider)}
	c.store = NewAttemptStore(c.wsRoot)
	c.record = &AttemptRecord{Submission: strings.Repeat("a", 32), Provider: "@fixture/runner", InputDigest: runner.BlobDigest(nil),
		SourceDigest: runner.BlobDigest(nil), Attempt: "a-1", State: runner.StateRunning, Cursor: cursor}
	if err := c.store.Write(c.record); err != nil {
		t.Fatal(err)
	}
	return c, &stdout, &stderr
}

// An attempt state this protocol version does not define is a protocol
// error: observe returns it instead of waiting on it forever.
func TestObserveRefusesAnUnknownAttemptState(t *testing.T) {
	t.Parallel()
	provider := readyProvider()
	provider.followState = "hibernating"
	c, stdout, _ := observingClient(t, provider, 0)
	_, err := c.observe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unknown attempt state") {
		t.Fatalf("unknown state: %v", err)
	}
	if stdout.String() != "hello\n" {
		t.Fatalf("records before the unknown state were not forwarded: %q", stdout.String())
	}
}

// A reconnect replays from the persisted cursor: records at or below it are
// dropped, so a line is never printed twice, while a provider that answers
// out of order within one answer is refused.
func TestObserveDropsReplayedRecordsAndRefusesDisorder(t *testing.T) {
	t.Parallel()
	provider := readyProvider()
	provider.followRecords = []runner.OutputRecord{
		{Cursor: 1, Stream: "stdout", Line: "one"}, {Cursor: 2, Stream: "stdout", Line: "two"},
		{Cursor: 3, Stream: "stderr", Line: "three"}, {Cursor: 4, Stream: "stdout", Line: "four"},
	}
	c, stdout, stderr := observingClient(t, provider, 2)
	final, err := c.observe(context.Background())
	if err != nil || final.State != runner.StateCompleted {
		t.Fatalf("observe = %+v, %v", final, err)
	}
	if stdout.String() != "four\n" || stderr.String() != "three\n" {
		t.Fatalf("replayed records were not dropped: stdout %q stderr %q", stdout.String(), stderr.String())
	}
	if c.record.Cursor != 4 {
		t.Fatalf("cursor = %d, want 4", c.record.Cursor)
	}
	disorder := readyProvider()
	disorder.followRecords = []runner.OutputRecord{{Cursor: 5, Stream: "stdout", Line: "five"}, {Cursor: 5, Stream: "stdout", Line: "five again"}}
	c, _, _ = observingClient(t, disorder, 0)
	if _, err := c.observe(context.Background()); err == nil || !strings.Contains(err.Error(), "cursor 5 after 5") {
		t.Fatalf("disorder within one answer accepted: %v", err)
	}
	// An answer past the contract's bound is refused, not buffered.
	flood := readyProvider()
	flood.followRecords = make([]runner.OutputRecord, runner.MaxFollowRecords+1)
	for index := range flood.followRecords {
		flood.followRecords[index] = runner.OutputRecord{Cursor: int64(index + 1), Stream: "stdout", Line: "x"}
	}
	session := connectFake(t, flood)
	if _, err := session.Follow(context.Background(), &runner.FollowParams{Attempt: "a-1"}); err == nil || !strings.Contains(err.Error(), "bounds an answer") {
		t.Fatalf("unbounded answer accepted: %v", err)
	}
}

// The durable record is what a later process resolves a submission by: it
// round-trips exactly, is found by either reference, and only an unsettled
// record for the same inputs and provider counts as work in flight.
func TestAttemptStoreResolvesReferencesAndPendingWork(t *testing.T) {
	t.Parallel()
	store := NewAttemptStore(t.TempDir())
	key := strings.Repeat("b", 32)
	record := &AttemptRecord{Submission: key, Provider: "@fixture/runner", InputDigest: "sha256:" + strings.Repeat("1", 64), SourceDigest: runner.BlobDigest(nil), Commands: []string{"build"}, State: StateSubmitting}
	if err := store.Write(record); err != nil {
		t.Fatal(err)
	}
	pending, err := store.Pending("@fixture/runner", record.InputDigest)
	if err != nil || pending == nil || pending.Submission != key {
		t.Fatalf("a lost acknowledgement is pending work: %+v, %v", pending, err)
	}
	if other, _ := store.Pending("@other/runner", record.InputDigest); other != nil {
		t.Fatal("another provider's submission counted as pending")
	}
	record.Attempt, record.State, record.Cursor = "a-7", runner.StateRunning, 12
	if err := store.Write(record); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"a-7", key} {
		found, err := store.Find(ref)
		if err != nil || found.Attempt != "a-7" || found.Cursor != 12 || found.State != runner.StateRunning {
			t.Fatalf("find %s = %+v, %v", ref, found, err)
		}
	}
	if _, err := store.Find("a-8"); !errors.Is(err, ErrAttemptNotFound) {
		t.Fatalf("unknown reference: %v", err)
	}
	code := 1
	record.State, record.ExitCode, record.Error = runner.StateCompleted, &code, "its session could not be imported"
	if err := store.Write(record); err != nil {
		t.Fatal(err)
	}
	if settled, _ := store.Pending("@fixture/runner", record.InputDigest); settled != nil {
		t.Fatal("a settled attempt blocked a new submission")
	}
	read, err := store.Read(key)
	if err != nil || read.Error == "" || read.ExitCode == nil || *read.ExitCode != 1 || read.SessionID != "" {
		t.Fatalf("a retrieval failure must be kept beside the verdict: %+v, %v", read, err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir(), strings.Repeat("c", 32)+".json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	records, err := store.List()
	if err != nil || len(records) != 1 {
		t.Fatalf("a torn record must not hide the others: %d, %v", len(records), err)
	}
	if err := store.Write(&AttemptRecord{Submission: "not-a-key"}); err == nil {
		t.Fatal("an invalid key was written")
	}
	// Pruning keeps every record that still has something to resolve.
	for index := 0; index < 3; index++ {
		imported := &AttemptRecord{Submission: strings.Repeat(string(rune('d'+index)), 32), Provider: "@fixture/runner", InputDigest: record.InputDigest,
			SourceDigest: record.SourceDigest, Attempt: fmt.Sprintf("a-%d", index), State: runner.StateCompleted, Imported: true, SessionID: "20260916-10150" + strconv.Itoa(index) + "-0a1b2c",
			SubmittedAt: fmt.Sprintf("2026-09-16T10:15:0%dZ", index)}
		if err := store.Write(imported); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Prune(1); err != nil {
		t.Fatal(err)
	}
	records, err = store.List()
	if err != nil {
		t.Fatal(err)
	}
	kept := map[string]bool{}
	for _, entry := range records {
		kept[entry.Attempt] = true
	}
	if len(records) != 2 || !kept["a-7"] || !kept["a-2"] {
		t.Fatalf("prune kept %v: the unimported verdict and the newest imported record must survive", kept)
	}
}
