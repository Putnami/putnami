package clitest

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	runner "go.putnami.dev/protocol/runner"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/proctree"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/runnerprovider"
	"go.putnami.dev/tooling/cli/internal/runnersource"
)

// The local isolated test provider for the T3 and T4 verticals. It is the smallest
// out-of-process adapter that speaks the runner provider RPC for real: it
// receives the snapshot over the exchange directory, materializes it into a
// second private directory on the same machine, launches the snapshot's
// pinned entrypoint with the bound request, keeps the attempt's output and
// state on disk, and returns the canonical session bundle. It plans nothing,
// schedules nothing and judges nothing. It is a conformance harness, not a
// product.
//
// Everything about an attempt is DURABLE and lives under the provider root,
// so the RPC process is disposable: a client that dies, reconnects or comes
// back in another process is served by a fresh provider process reading the
// same attempt directory. The execution itself is owned by a detached
// supervisor process (its own session), so neither the client nor the RPC
// process going away stops a running engine — the shape a remote provider
// has, reproduced with real local processes.
//
// The test binary re-execs itself in three roles (see RunnerFixtureMain):
//
//   - PUTNAMI_RUNNER_FIXTURE=provider serves the RPC loop on stdio;
//   - PUTNAMI_RUNNER_FIXTURE=supervise <attemptDir> launches the pinned
//     entrypoint for one attempt, forwards its output to the attempt's
//     output log with cursors, terminates its process tree on SIGTERM and
//     records the one terminal state;
//   - PUTNAMI_RUNNER_FIXTURE=cli runs the real terminal adapter (App.Run) with
//     the process arguments, which is how a snapshot without its own pinned
//     entrypoint is executed, and how a test drives a whole CLI process it can
//     kill. It installs the same interrupt handling the shipped binary has, so
//     SIGTERM cancels the run and the engine tears its task trees down. A
//     snapshot whose lock says `cli.source: workspace` is executed through its
//     own ./putnamiw instead, never through this binary.
//
// PUTNAMI_RUNNER_FIXTURE_EXECUTABLE overrides the engine executable for
// manual proof runs with a real CLI build.
const (
	RunnerFixtureRoleEnv       = "PUTNAMI_RUNNER_FIXTURE"
	RunnerFixtureExecutableEnv = "PUTNAMI_RUNNER_FIXTURE_EXECUTABLE"
	RunnerFixtureRootEnv       = "PUTNAMI_RUNNER_FIXTURE_ROOT"
	RunnerFixtureNotReadyEnv   = "PUTNAMI_RUNNER_FIXTURE_NOT_READY"
	RunnerFixtureProtocolEnv   = "PUTNAMI_RUNNER_FIXTURE_PROTOCOL"
	// RunnerFixtureHoldEnv names a file the supervisor waits for between
	// materialization and launch, and beside which it writes "<file>.ready"
	// once the snapshot is materialized. A test uses it to edit the original
	// worktree after submission and prove the edit never reaches execution,
	// and to interrupt an attempt before its engine ever starts.
	RunnerFixtureHoldEnv = "PUTNAMI_RUNNER_FIXTURE_HOLD"
	// RunnerFixtureLoseAckEnv makes the provider process exit right after it
	// accepted a submission, before the acknowledgement is written: the
	// lost-acknowledgement case, with the attempt genuinely running.
	RunnerFixtureLoseAckEnv = "PUTNAMI_RUNNER_FIXTURE_LOSE_ACK"
	// RunnerFixtureDropFollowsEnv makes ONE provider process (the first to
	// reach it, recorded under the root) exit abruptly on its Nth follow: a
	// stream disconnect the client must reconnect from.
	RunnerFixtureDropFollowsEnv = "PUTNAMI_RUNNER_FIXTURE_DROP_FOLLOWS"
	// RunnerFixtureExpireFetchEnv makes fetch answer that the attempt's
	// artifacts expired.
	RunnerFixtureExpireFetchEnv = "PUTNAMI_RUNNER_FIXTURE_EXPIRE_FETCH"
	// RunnerFixtureCorruptBundleEnv makes fetch serve a session.json whose
	// bytes do not match the digest the bundle claims.
	RunnerFixtureCorruptBundleEnv = "PUTNAMI_RUNNER_FIXTURE_CORRUPT_BUNDLE"
	// RunnerFixtureTaskHoldEnv reaches the fixture's gate task through the
	// engine's environment: the task reports it is running beside a background
	// child and waits for the named file before it finishes.
	RunnerFixtureTaskHoldEnv = "PUTNAMI_RUNNER_FIXTURE_TASK_HOLD"
)

// RunnerFixtureMain is Main for a package whose tests drive the runner fixture
// provider: the test binary re-execs itself as the local isolated provider, as
// the attempt supervisor and as the executing CLI, and each role's exit code is
// the process's.
func RunnerFixtureMain(m *testing.M) int {
	if role := os.Getenv(RunnerFixtureRoleEnv); role != "" {
		return runRunnerFixtureRole(role)
	}
	return Main(m)
}

func runRunnerFixtureRole(role string) int {
	switch role {
	case "provider":
		return serveRunnerFixtureProvider(os.Stdin, os.Stdout, os.Stderr, os.Getenv)
	case "supervise":
		if len(os.Args) < 2 {
			fmt.Fprintln(os.Stderr, "supervise: attempt directory required")
			return 2
		}
		return superviseFixtureAttempt(os.Args[1], os.Getenv)
	case "cli":
		app, err := cli.NewApp()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		// The shipped binary cancels the run on the first interrupt (cmd/putnami
		// main.go); the engine then terminates every task process group. The
		// fixture engine must behave the same for a cancellation to be real.
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		code := app.Run(ctx, os.Args[1:])
		stop()
		return code
	}
	fmt.Fprintf(os.Stderr, "unknown runner fixture role %q\n", role)
	return 2
}

// Durable attempt files under <root>/attempts/<id>/. FixtureOutputFile is the
// attempt's output log with cursors and FixtureManifestFile the source manifest
// it was submitted with; tests read both to prove what the provider saw.
const (
	fixtureMetaFile     = "meta.json"
	fixtureStateFile    = "state.json"
	FixtureOutputFile   = "output.jsonl"
	fixtureLogFile      = "supervisor.log"
	FixtureManifestFile = "manifest.json"
)

// fixtureMeta is written once at submission.
type fixtureMeta struct {
	Key         string `json:"key"`
	InputDigest string `json:"inputDigest"`
	Snapshot    string `json:"snapshot"`
	Request     string `json:"request"`
}

// FixtureState is the supervisor's statement of the attempt: one terminal
// state, written atomically, never rewritten once terminal.
type FixtureState struct {
	State      string `json:"state"`
	Reason     string `json:"reason,omitempty"`
	ExitCode   *int   `json:"exitCode,omitempty"`
	Supervisor int    `json:"supervisor,omitempty"`
	Engine     int    `json:"engine,omitempty"`
}

// ReadFixtureState reads the attempt directory's state; an attempt without
// one yet is queued.
func ReadFixtureState(dir string) FixtureState {
	data, err := os.ReadFile(filepath.Join(dir, fixtureStateFile))
	if err != nil {
		return FixtureState{State: runner.StateQueued}
	}
	var state FixtureState
	if json.Unmarshal(data, &state) != nil {
		return FixtureState{State: runner.StateQueued}
	}
	return state
}

func writeFixtureJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

// ---------------------------------------------------------------------------
// The supervisor role: one detached process per attempt.

func superviseFixtureAttempt(dir string, getenv func(string) string) int {
	var meta fixtureMeta
	if data, err := os.ReadFile(filepath.Join(dir, fixtureMetaFile)); err != nil || json.Unmarshal(data, &meta) != nil {
		return 1
	}
	state := FixtureState{State: runner.StateQueued, Supervisor: os.Getpid()}
	fail := func(err error) int {
		state.State, state.Reason = runner.StateFailed, err.Error()
		_ = writeFixtureJSON(filepath.Join(dir, fixtureStateFile), state)
		return 1
	}
	// A cancellation that arrived before supervision started wins: the
	// attempt is terminal and nothing may run for it.
	if runner.TerminalState(ReadFixtureState(dir).State) {
		return 0
	}
	if err := writeFixtureJSON(filepath.Join(dir, fixtureStateFile), state); err != nil {
		return 1
	}
	interrupt := make(chan os.Signal, 2)
	signal.Notify(interrupt, syscall.SIGTERM, syscall.SIGINT)

	if hold := getenv(RunnerFixtureHoldEnv); hold != "" {
		_ = os.WriteFile(hold+".ready", []byte(meta.Snapshot), 0o600)
		for {
			if _, err := os.Stat(hold); err == nil {
				break
			}
			select {
			case <-interrupt:
				// Interrupted before execution: nothing ever ran, nothing to kill.
				state.State, state.Reason = runner.StateCanceled, "canceled before the engine started"
				_ = writeFixtureJSON(filepath.Join(dir, fixtureStateFile), state)
				return 0
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	select {
	case <-interrupt:
		state.State, state.Reason = runner.StateCanceled, "canceled before the engine started"
		_ = writeFixtureJSON(filepath.Join(dir, fixtureStateFile), state)
		return 0
	default:
	}
	command, roleEnv, err := fixturePinnedEntrypoint(meta.Snapshot, getenv)
	if err != nil {
		return fail(err)
	}
	cmd := exec.Command(command)
	cmd.Dir = meta.Snapshot
	// The executing engine gets a clean run-level environment: nothing the
	// SUBMITTING run exported to its own tasks (its session id, its cache
	// sockets, its reporter capability) may describe the isolated run.
	env := []string{}
	for _, entry := range os.Environ() {
		key := strings.SplitN(entry, "=", 2)[0]
		switch {
		case key == protocolcli.ParentSessionEnv, key == runnerprovider.BoundRequestEnv, key == RunnerFixtureRoleEnv, key == RunnerFixtureHoldEnv,
			key == protocolcli.SessionReporterEnv, key == protocolcli.SessionReporterTokenEnv,
			key == protocolcli.LogReporterEnv, key == protocolcli.LogReporterTokenEnv,
			strings.HasPrefix(key, "PUTNAMI_CACHE_"), key == "PUTNAMI_SPEC_FRAGMENTS":
			continue
		}
		env = append(env, entry)
	}
	env = append(env, roleEnv...)
	env = append(env, runnerprovider.BoundRequestEnv+"="+meta.Request)
	cmd.Env = env
	// The engine gets its own process group: the supervisor terminates the
	// whole tree through it, and the engine terminates its task groups itself
	// when it is asked to stop.
	tree := proctree.New(cmd)
	defer func() { _ = tree.Close() }()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fail(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fail(err)
	}
	output, err := os.OpenFile(filepath.Join(dir, FixtureOutputFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = output.Close() }()
	if err := tree.Start(); err != nil {
		return fail(err)
	}
	state.State, state.Engine = runner.StateRunning, cmd.Process.Pid
	if err := writeFixtureJSON(filepath.Join(dir, fixtureStateFile), state); err != nil {
		return fail(err)
	}
	var outputMu sync.Mutex
	var cursor int64
	var wg sync.WaitGroup
	forward := func(reader io.Reader, stream string) {
		defer wg.Done()
		buffered := bufio.NewReaderSize(reader, 64<<10)
		for {
			line, err := readBoundedLine(buffered, runner.MaxOutputRecordBytes)
			if len(line) > 0 || err == nil {
				outputMu.Lock()
				cursor++
				record, _ := json.Marshal(runner.OutputRecord{Cursor: cursor, Stream: stream, Line: string(line)})
				_, _ = output.Write(append(record, '\n'))
				_ = output.Sync()
				outputMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(2)
	go forward(stdout, "stdout")
	go forward(stderr, "stderr")
	waited := make(chan error, 1)
	go func() {
		wg.Wait()
		waited <- cmd.Wait()
	}()
	canceled := false
	var waitErr error
	select {
	case waitErr = <-waited:
	case <-interrupt:
		// One observed terminal outcome: the engine is asked to stop, given a
		// bounded grace to tear its task trees down, then its group is killed.
		// Whatever exit the process then reports, the attempt is canceled —
		// the alternative select arm above is the only way to be "completed".
		canceled = true
		_ = tree.Terminate()
		select {
		case waitErr = <-waited:
		case <-time.After(5 * time.Second):
			_ = tree.Kill()
			select {
			case waitErr = <-waited:
			case <-time.After(3 * time.Second):
				// A grandchild outside the engine's group may still hold the
				// output pipe; the tree is down and the outcome is known.
				state.State, state.Reason = runner.StateCanceled, "canceled on request; process tree killed"
				_ = writeFixtureJSON(filepath.Join(dir, fixtureStateFile), state)
				return 0
			}
		}
		_ = tree.Kill()
	}
	exitCode := 0
	if waitErr != nil {
		var exit *exec.ExitError
		if !errors.As(waitErr, &exit) {
			return fail(waitErr)
		}
		exitCode = exit.ExitCode()
	}
	state.ExitCode = &exitCode
	if canceled {
		state.State, state.Reason = runner.StateCanceled, "canceled on request; process tree terminated"
	} else {
		state.State = runner.StateCompleted
	}
	_ = writeFixtureJSON(filepath.Join(dir, fixtureStateFile), state)
	return 0
}

// readBoundedLine reads one line, cutting anything past the bound so a single
// oversized line never stops the forwarding of what follows it.
func readBoundedLine(reader *bufio.Reader, bound int) ([]byte, error) {
	var line []byte
	truncated := false
	for {
		part, isPrefix, err := reader.ReadLine()
		if len(part) > 0 && !truncated {
			if len(line)+len(part) > bound {
				part = part[:bound-len(line)]
				truncated = true
			}
			line = append(line, part...)
		}
		if err != nil {
			return line, err
		}
		if !isPrefix {
			return line, nil
		}
	}
}

// fixturePinnedEntrypoint honors the snapshot's own engine pin: a source
// workspace runs its ./putnamiw, which builds from the snapshot's tree;
// anything else runs the configured executable or this binary in its CLI
// role. A baked binary never replaces changed CLI source.
func fixturePinnedEntrypoint(snapshot string, getenv func(string) string) (string, []string, error) {
	if data, err := os.ReadFile(filepath.Join(snapshot, "putnami.lock.json")); err == nil {
		var lock wsproto.Lock
		if json.Unmarshal(data, &lock) == nil && lock.CLI != nil && lock.CLI.IsWorkspaceSource() {
			wrapper := filepath.Join(snapshot, "putnamiw")
			if info, err := os.Stat(wrapper); err != nil || info.Mode()&0o111 == 0 {
				return "", nil, fmt.Errorf("snapshot pins cli.source=workspace but has no executable ./putnamiw")
			}
			return wrapper, []string{RunnerFixtureRoleEnv + "="}, nil
		}
	}
	if executable := getenv(RunnerFixtureExecutableEnv); executable != "" {
		return executable, []string{RunnerFixtureRoleEnv + "="}, nil
	}
	self, err := os.Executable()
	if err != nil {
		return "", nil, err
	}
	return self, []string{RunnerFixtureRoleEnv + "=cli"}, nil
}

// ---------------------------------------------------------------------------
// The RPC role: a disposable process over the durable attempt directories.

type fixtureProvider struct {
	root        string
	store       *runnersource.Store
	exchangeDir string
	getenv      func(string) string
	stderr      io.Writer
	follows     int
	// exit asks the serving loop to end the process abruptly after the
	// current answer (or instead of it): the fault-injection channel.
	exit func(int)
}

func serveRunnerFixtureProvider(stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	root := getenv(RunnerFixtureRootEnv)
	if root == "" {
		var err error
		root, err = os.MkdirTemp("", "putnami-runner-fixture-*")
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	store, err := runnersource.OpenStore(filepath.Join(root, "source"))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer func() { _ = store.Close() }()
	for _, dir := range []string{"attempts", "keys"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	countFixtureEvent(root, "spawns")
	provider := &fixtureProvider{root: root, store: store, getenv: getenv, stderr: stderr, exit: os.Exit}
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 0, 64<<10), 64<<20)
	var writeMu sync.Mutex
	reply := func(response *runner.ProviderResponse) {
		line, _ := json.Marshal(response)
		writeMu.Lock()
		defer writeMu.Unlock()
		_, _ = stdout.Write(append(line, '\n'))
	}
	for scanner.Scan() {
		request, err := runner.ParseProviderRequest(scanner.Bytes())
		if err != nil {
			fmt.Fprintln(stderr, "fixture provider: malformed request:", err)
			return 1
		}
		if request.Op == runner.OpShutdown {
			reply(&runner.ProviderResponse{ProtocolVersion: runner.ProviderProtocolVersion, ID: request.ID, OK: true, Payload: json.RawMessage(`{}`)})
			return 0
		}
		payload, err := provider.handle(request)
		response := &runner.ProviderResponse{ProtocolVersion: runner.ProviderProtocolVersion, ID: request.ID, OK: err == nil}
		if err != nil {
			response.Error = &runner.ProviderError{Code: fixtureErrorCode(err), Message: err.Error()}
		} else {
			response.Payload, _ = runner.MarshalPayload(payload)
		}
		reply(response)
	}
	return 0
}

// fixtureErrorCode derives the wire code from the fault a handler raised.
func fixtureErrorCode(err error) string {
	if errors.Is(err, errFixtureExpired) {
		return "expired"
	}
	return "fixture"
}

var errFixtureExpired = errors.New("attempt artifacts expired")

// countFixtureEvent appends one line to a counter file under the root; tests
// read the line count to prove how many provider processes served a run.
func countFixtureEvent(root, name string) {
	file, err := os.OpenFile(filepath.Join(root, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = file.WriteString(strconv.Itoa(os.Getpid()) + "\n")
	_ = file.Close()
}

func (p *fixtureProvider) handle(request *runner.ProviderRequest) (any, error) {
	switch request.Op {
	case runner.OpInitialize:
		var params runner.InitializeParams
		if err := json.Unmarshal(request.Payload, &params); err != nil {
			return nil, err
		}
		if params.ProtocolVersion != runner.ProviderProtocolVersion || !filepath.IsAbs(params.ExchangeDir) {
			return nil, fmt.Errorf("unsupported initialize parameters")
		}
		p.exchangeDir = params.ExchangeDir
		result := runner.InitializeResult{ProtocolVersion: runner.ProviderProtocolVersion, ProviderName: "@fixture/runner", ProviderVersion: "1.0.0",
			Capabilities: []string{runner.CapabilityExecutionRequestV1, runner.CapabilitySessionBundleV1}, Ready: true}
		if p.getenv(RunnerFixtureNotReadyEnv) == "1" {
			result.Ready, result.Reason = false, "fixture provider is not linked to any execution capacity"
		}
		if forced := p.getenv(RunnerFixtureProtocolEnv); forced != "" {
			version, err := strconv.Atoi(forced)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", RunnerFixtureProtocolEnv, err)
			}
			result.ProtocolVersion = version
		}
		return result, nil
	case runner.OpPrepare:
		var params runner.PrepareParams
		if err := json.Unmarshal(request.Payload, &params); err != nil {
			return nil, err
		}
		digest, err := runner.SourceDigest(params.Manifest)
		if err != nil {
			return nil, err
		}
		if digest != params.SourceDigest {
			return nil, fmt.Errorf("manifest digest %s does not match %s", digest, params.SourceDigest)
		}
		missing := []string{}
		for _, entry := range params.Manifest.Entries {
			if entry.Kind == "file" && !p.store.Has(entry) {
				missing = append(missing, entry.Digest)
			}
		}
		return runner.PrepareResult{MissingBlobs: runner.SortStrings(missing)}, nil
	case runner.OpSubmit:
		return p.submit(request.Payload)
	case runner.OpLookup:
		var params runner.LookupParams
		if err := json.Unmarshal(request.Payload, &params); err != nil {
			return nil, err
		}
		id, ok := p.attemptForKey(params.IdempotencyKey)
		if !ok {
			return runner.LookupResult{}, nil
		}
		meta, err := p.meta(id)
		if err != nil {
			return nil, err
		}
		return runner.LookupResult{Attempt: id, State: ReadFixtureState(p.attemptDir(id)).State, ExecutionInputDigest: meta.InputDigest}, nil
	case runner.OpFollow:
		var params runner.FollowParams
		if err := json.Unmarshal(request.Payload, &params); err != nil {
			return nil, err
		}
		p.follows++
		if drop, _ := strconv.Atoi(p.getenv(RunnerFixtureDropFollowsEnv)); drop > 0 && p.follows == drop && p.claimOnce("dropped") {
			// The transport dies mid-stream, once: the attempt keeps running.
			p.exit(3)
		}
		return p.follow(params)
	case runner.OpCancel:
		var params runner.CancelParams
		if err := json.Unmarshal(request.Payload, &params); err != nil {
			return nil, err
		}
		return p.cancel(params.Attempt)
	case runner.OpFetch:
		var params runner.FetchParams
		if err := json.Unmarshal(request.Payload, &params); err != nil {
			return nil, err
		}
		return p.fetch(params.Attempt)
	}
	return nil, fmt.Errorf("unsupported op %s", request.Op)
}

func (p *fixtureProvider) attemptDir(id string) string { return filepath.Join(p.root, "attempts", id) }

func (p *fixtureProvider) meta(id string) (fixtureMeta, error) {
	var meta fixtureMeta
	data, err := os.ReadFile(filepath.Join(p.attemptDir(id), fixtureMetaFile))
	if err != nil {
		return meta, fmt.Errorf("unknown attempt %q", id)
	}
	return meta, json.Unmarshal(data, &meta)
}

func (p *fixtureProvider) attemptForKey(key string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(p.root, "keys", key))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

// claimOnce records a named fault under the root and reports whether this
// call was the first to claim it, so a fault injected by environment fires in
// exactly one provider process however many are spawned.
func (p *fixtureProvider) claimOnce(name string) bool {
	file, err := os.OpenFile(filepath.Join(p.root, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	_ = file.Close()
	return true
}

func (p *fixtureProvider) submit(payload json.RawMessage) (any, error) {
	var params runner.SubmitParams
	if err := json.Unmarshal(payload, &params); err != nil {
		return nil, err
	}
	// Strict admission: the request is re-parsed from its canonical bytes so
	// unknown members and semantic violations are refused before any source
	// is materialized, and the manifest must be the one the request names.
	canonical, err := runner.CanonicalExecutionRequest(params.Request)
	if err != nil {
		return nil, fmt.Errorf("request refused: %w", err)
	}
	if _, err := runner.ParseExecutionRequest(canonical); err != nil {
		return nil, fmt.Errorf("request refused: %w", err)
	}
	digest, err := runner.SourceDigest(params.Manifest)
	if err != nil {
		return nil, err
	}
	if digest != params.Request.Source.Digest {
		return nil, fmt.Errorf("manifest digest %s does not match the request's %s", digest, params.Request.Source.Digest)
	}
	// One idempotency key is one attempt: a repeated submit — a retried
	// envelope, a client that never saw its acknowledgement — answers with
	// the attempt already accepted under the key and starts nothing.
	key := params.Request.Control.IdempotencyKey
	if id, ok := p.attemptForKey(key); ok {
		return runner.SubmitResult{Attempt: id, State: ReadFixtureState(p.attemptDir(id)).State}, nil
	}
	inputDigest, err := runner.ExecutionInputDigest(params.Request)
	if err != nil {
		return nil, err
	}
	for _, entry := range params.Manifest.Entries {
		if entry.Kind != "file" || p.store.Has(entry) {
			continue
		}
		if err := p.ingest(entry); err != nil {
			return nil, err
		}
	}
	id := fmt.Sprintf("fixture-attempt-%d-%s", os.Getpid(), key[:8])
	attemptRoot := p.attemptDir(id)
	if err := os.MkdirAll(attemptRoot, 0o700); err != nil {
		return nil, err
	}
	snapshot, err := p.store.Materialize(attemptRoot, params.Manifest)
	if err != nil {
		return nil, fmt.Errorf("materialize: %w", err)
	}
	requestPath := filepath.Join(attemptRoot, "request.json")
	if err := os.WriteFile(requestPath, canonical, 0o600); err != nil {
		return nil, err
	}
	// The admitted manifest is kept beside the request so a test can read
	// which entries the snapshot carried and how they were flagged.
	manifestBytes, err := runner.CanonicalSourceManifest(params.Manifest)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(attemptRoot, FixtureManifestFile), manifestBytes, 0o600); err != nil {
		return nil, err
	}
	if err := writeFixtureJSON(filepath.Join(attemptRoot, fixtureMetaFile), fixtureMeta{Key: key, InputDigest: inputDigest, Snapshot: snapshot, Request: requestPath}); err != nil {
		return nil, err
	}
	if err := writeFixtureJSON(filepath.Join(attemptRoot, fixtureStateFile), FixtureState{State: runner.StateQueued}); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(p.root, "keys", key), []byte(id+"\n"), 0o600); err != nil {
		return nil, err
	}
	if err := p.startSupervisor(attemptRoot); err != nil {
		return nil, err
	}
	if p.getenv(RunnerFixtureLoseAckEnv) == "1" {
		// The submission was accepted and is running; the acknowledgement
		// never reaches the client because the transport dies here.
		p.exit(3)
	}
	return runner.SubmitResult{Attempt: id, State: runner.StateQueued}, nil
}

// startSupervisor launches the detached supervisor for one attempt: its own
// session, its own log, no inherited pipe — so the RPC process's death, and
// the client's, leave the execution running.
func (p *fixtureProvider) startSupervisor(attemptRoot string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(attemptRoot, fixtureLogFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = log.Close() }()
	cmd := exec.Command(self, attemptRoot)
	cmd.Env = append(os.Environ(), RunnerFixtureRoleEnv+"=supervise")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, log, log
	if err := startDetachedSupervisor(cmd); err != nil {
		return fmt.Errorf("start supervisor: %w", err)
	}
	// The supervisor is reaped by init once this process ends; releasing it
	// here keeps this process from holding a handle it never waits on.
	return cmd.Process.Release()
}

func (p *fixtureProvider) ingest(entry runner.SourceEntry) error {
	path, ok := runner.ExchangeBlobPath(p.exchangeDir, entry.Digest)
	if !ok {
		return fmt.Errorf("invalid digest %s", entry.Digest)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("blob %s was not transferred: %w", entry.Digest, err)
	}
	defer func() { _ = file.Close() }()
	digest, err := p.store.Ingest(file, entry.Size)
	if err != nil {
		return err
	}
	if digest != entry.Digest {
		return fmt.Errorf("transferred blob %s has digest %s", entry.Digest, digest)
	}
	// One line per blob that actually crossed the exchange directory, so a
	// test can prove a second snapshot transferred only what changed.
	countFixtureEvent(p.root, "ingested")
	return nil
}

// follow answers from the attempt's durable output log and state: records
// strictly after the cursor, at most MaxFollowRecords of them, and the state
// the supervisor last wrote. It blocks briefly for new output so the client
// does not busy-poll; the wait paces observation and never bounds execution.
func (p *fixtureProvider) follow(params runner.FollowParams) (any, error) {
	dir := p.attemptDir(params.Attempt)
	if _, err := p.meta(params.Attempt); err != nil {
		return nil, err
	}
	var records []runner.OutputRecord
	var state FixtureState
	for waited := time.Duration(0); ; waited += 20 * time.Millisecond {
		records = readFixtureOutput(dir, params.Cursor, runner.MaxFollowRecords)
		state = ReadFixtureState(dir)
		if len(records) > 0 || runner.TerminalState(state.State) || waited >= 200*time.Millisecond {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	result := runner.FollowResult{State: state.State, Cursor: params.Cursor, Records: []runner.OutputRecord{}, Reason: state.Reason}
	if len(records) > 0 {
		result.Records = records
		result.Cursor = records[len(records)-1].Cursor
	}
	if runner.TerminalState(state.State) && state.ExitCode != nil {
		code := *state.ExitCode
		result.ExitCode = &code
	}
	return result, nil
}

func readFixtureOutput(dir string, after int64, limit int) []runner.OutputRecord {
	file, err := os.Open(filepath.Join(dir, FixtureOutputFile))
	if err != nil {
		return nil
	}
	defer func() { _ = file.Close() }()
	var records []runner.OutputRecord
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64<<10), runner.MaxOutputRecordBytes+1024)
	for scanner.Scan() {
		var record runner.OutputRecord
		if json.Unmarshal(scanner.Bytes(), &record) != nil || record.Cursor <= after {
			continue
		}
		records = append(records, record)
		if len(records) == limit {
			break
		}
	}
	return records
}

// cancel converges on one terminal outcome: an attempt already terminal keeps
// its state; a running one is asked to stop through its supervisor, which
// terminates the engine's process tree, and the state observed after a bounded
// wait is answered — terminal when the tree went down in time, still running
// otherwise, never a promise.
func (p *fixtureProvider) cancel(id string) (any, error) {
	dir := p.attemptDir(id)
	if _, err := p.meta(id); err != nil {
		return nil, err
	}
	state := ReadFixtureState(dir)
	if runner.TerminalState(state.State) {
		return runner.CancelResult{State: state.State}, nil
	}
	if state.Supervisor == 0 {
		// Accepted but not yet supervised (a start still in flight): mark the
		// attempt canceled so a late supervisor start finds nothing to run.
		_ = writeFixtureJSON(filepath.Join(dir, fixtureStateFile), FixtureState{State: runner.StateCanceled, Reason: "canceled before supervision started"})
		return runner.CancelResult{State: runner.StateCanceled}, nil
	}
	_ = stopSupervisor(state.Supervisor)
	for waited := time.Duration(0); waited < 2*time.Second; waited += 25 * time.Millisecond {
		state = ReadFixtureState(dir)
		if runner.TerminalState(state.State) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	return runner.CancelResult{State: state.State}, nil
}

// fetch collects the bundle at fetch time, into the CURRENT exchange
// directory: a later provider process serving a resumed client exports the
// same durable session files again. The faults a test injects here are the
// ones an importer must survive: expired artifacts and corrupt bytes.
func (p *fixtureProvider) fetch(id string) (any, error) {
	dir := p.attemptDir(id)
	meta, err := p.meta(id)
	if err != nil {
		return nil, err
	}
	state := ReadFixtureState(dir)
	if state.State != runner.StateCompleted || state.ExitCode == nil {
		return nil, fmt.Errorf("attempt %s is %s; no bundle", id, state.State)
	}
	if p.getenv(RunnerFixtureExpireFetchEnv) == "1" {
		return nil, fmt.Errorf("%w: attempt %s is past its retention", errFixtureExpired, id)
	}
	bundle, err := p.collectBundle(id, meta.Snapshot, *state.ExitCode)
	if err != nil {
		return nil, fmt.Errorf("collect session bundle: %w", err)
	}
	if p.getenv(RunnerFixtureCorruptBundleEnv) == "1" && bundle.SessionID != "" {
		for _, file := range bundle.Sessions[0].Files {
			if file.Name == runner.BundleSessionFile {
				path, _ := runner.ExchangeBlobPath(p.exchangeDir, file.Digest)
				_ = os.WriteFile(path, []byte(strings.Repeat("x", int(file.Size))), 0o600)
			}
		}
	}
	return runner.FetchResult{Bundle: *bundle}, nil
}

// collectBundle copies every recorded session out of the snapshot into the
// exchange directory by digest, unchanged. The gate session is the one the
// engine linked as latest; the others are nested runs it spawned.
func (p *fixtureProvider) collectBundle(id, snapshot string, exitCode int) (*runner.SessionBundle, error) {
	bundle := &runner.SessionBundle{Attempt: id, ExitCode: exitCode, Sessions: []runner.BundleSession{}}
	sessionsRoot := filepath.Join(snapshot, ".putnami", "sessions")
	entries, err := os.ReadDir(sessionsRoot)
	if os.IsNotExist(err) {
		return bundle, nil
	}
	if err != nil {
		return nil, err
	}
	if target, err := os.Readlink(filepath.Join(sessionsRoot, "latest")); err == nil {
		bundle.SessionID = filepath.Base(target)
	}
	var sessions []runner.BundleSession
	for _, entry := range entries {
		if !entry.IsDir() || !runner.ValidSessionID(entry.Name()) {
			continue
		}
		session := runner.BundleSession{ID: entry.Name()}
		dir := filepath.Join(sessionsRoot, entry.Name())
		for _, name := range []string{runner.BundleSessionFile, runner.BundlePlanFile, runner.BundleEventsFile, runner.BundleReportFile, runner.BundleSpecFile} {
			file, ok, err := p.exportSessionFile(filepath.Join(dir, name), name)
			if err != nil {
				return nil, err
			}
			if ok {
				session.Files = append(session.Files, file)
			}
		}
		file, ok, err := p.exportSessionFile(filepath.Join(snapshot, ".putnami", "reports", entry.Name()+".json"), runner.BundleRunReportFile)
		if err != nil {
			return nil, err
		}
		if ok {
			session.Files = append(session.Files, file)
		}
		data, err := os.ReadFile(filepath.Join(dir, runner.BundleSessionFile))
		if err != nil {
			continue // an unfinished directory (a crashed nested run) carries no evidence
		}
		var document protocolcli.SessionFile
		if err := json.Unmarshal(data, &document); err == nil {
			session.ParentID = document.ParentSessionID
		}
		sessions = append(sessions, session)
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].ID == bundle.SessionID {
			return true
		}
		if sessions[j].ID == bundle.SessionID {
			return false
		}
		return sessions[i].ID < sessions[j].ID
	})
	bundle.Sessions = sessions
	if bundle.SessionID != "" && (len(sessions) == 0 || sessions[0].ID != bundle.SessionID) {
		bundle.SessionID = ""
	}
	return bundle, nil
}

func (p *fixtureProvider) exportSessionFile(path, name string) (runner.BundleFile, bool, error) {
	return ExportSessionFile(p.exchangeDir, path, name)
}

// ExportSessionFile copies one session file into the exchange directory as a
// content-addressed blob and returns its bundle entry; a missing file is not
// an error, it is simply absent from the bundle.
func ExportSessionFile(exchangeDir, path, name string) (runner.BundleFile, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return runner.BundleFile{}, false, nil
	}
	if err != nil {
		return runner.BundleFile{}, false, err
	}
	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	target, _ := runner.ExchangeBlobPath(exchangeDir, digest)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return runner.BundleFile{}, false, err
	}
	if err := os.WriteFile(target, data, 0o600); err != nil && !os.IsExist(err) {
		return runner.BundleFile{}, false, err
	}
	return runner.BundleFile{Name: name, Digest: digest, Size: int64(len(data))}, true, nil
}
