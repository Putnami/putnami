//go:build unix

// The lifecycle scenarios observe process trees the Unix way: kill(pid, 0),
// ESRCH, process groups and ps for zombie state. The runner fixture they drive
// cancels through SIGTERM and refuses to supervise on Windows.

package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	runner "go.putnami.dev/protocol/runner"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/runnerprovider"
)

// The T4 durable-lifecycle vertical: the same real composition as the
// T3 harness — the terminal adapter, the real engine, the out-of-process
// conformance provider, a real detached supervisor owning a real child engine,
// real session files — under the failures a remote placement actually meets:
// a lost acknowledgement, a duplicated submission, a CLI that dies mid-run, a
// stream that disconnects, an interrupt before, during and after execution,
// artifacts that expired, a bundle that arrived corrupt. Nothing here is
// mocked; every fault is injected into the provider or the process tree.

// heldTaskFixture is clitest.PortableFixture with a gate task that reports it is
// running, keeps a background child alive, and waits for a release file, so a
// test can act on an attempt while its engine genuinely executes.
func heldTaskFixture(t *testing.T) (root, providerRoot, hold string) {
	t.Helper()
	root, providerRoot = clitest.PortableFixture(t, false, true)
	hold = filepath.Join(t.TempDir(), "task-hold")
	t.Setenv(clitest.RunnerFixtureTaskHoldEnv, hold)
	clitest.WriteFile(t, filepath.Join(root, "extension", "gate.sh"), `echo "ran $(cat "$PUTNAMI_PROJECT_ROOT/marker")" >> "$PUTNAMI_PROJECT_ROOT/calls"
echo "task output line for $PUTNAMI_PROJECT_ROOT"
if [ -n "${PUTNAMI_RUNNER_FIXTURE_TASK_HOLD:-}" ]; then
  sleep 300 &
  echo $! > "$PUTNAMI_RUNNER_FIXTURE_TASK_HOLD.pid"
  echo running > "$PUTNAMI_RUNNER_FIXTURE_TASK_HOLD.running"
  while [ ! -f "$PUTNAMI_RUNNER_FIXTURE_TASK_HOLD" ]; do sleep 0.05; done
fi
`)
	clitest.RunGit(t, root, "add", "-A")
	clitest.RunGit(t, root, "commit", "-q", "-m", "held task")
	return root, providerRoot, hold
}

// waitForFile blocks until path exists or the deadline passes. The deadline
// bounds a NEGATIVE only: the file appears as soon as the held task runs.
func waitForFile(t *testing.T, path string, deadline time.Duration) {
	t.Helper()
	start := time.Now()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Since(start) > deadline {
			t.Fatalf("%s did not appear within %s", path, deadline)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForAttemptState blocks until the one attempt record reaches want, or the
// deadline passes. The provider's `.running` marker and the record's transition
// to running are two separate writes, by two separate processes: waiting on the
// marker says the task started, not that the CLI has recorded it. A test that
// kills the CLI on the marker alone can therefore find the record still
// `queued`, which is a known CI-only failure that never reproduced on an idle
// laptop.
func waitForAttemptState(t *testing.T, root, want string, deadline time.Duration) {
	t.Helper()
	start := time.Now()
	for {
		records := clitest.AttemptRecords(t, root)
		if len(records) == 1 && records[0].State == want {
			return
		}
		if time.Since(start) > deadline {
			t.Fatalf("the attempt did not reach %s within %s: %+v", want, deadline, records)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func providerAttempts(t *testing.T, providerRoot string) []string {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(providerRoot, "attempts"))
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// processGone reports whether pid no longer exists, waiting a bounded time
// for the kernel to reap it. The bound covers a negative only.
func processGone(pid int, deadline time.Duration) bool {
	start := time.Now()
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return true
		}
		if err == nil {
			// A zombie answers kill(0) until reaped; ask the kernel for its state.
			if out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output(); err != nil || strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
				return true
			}
		}
		if time.Since(start) > deadline {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestPortableLostAcknowledgementResolvesByKeyBeforeResubmitting(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "durable-lifecycle", "lost-acknowledgement-resolves-by-key")
	root, providerRoot := clitest.PortableFixture(t, false, true)
	gate := []string{"build", "--projects", "app", "--no-cache", "--where", "remote"}
	t.Setenv(clitest.RunnerFixtureLoseAckEnv, "1")
	code, output := clitest.RunGateArgs(t, root, gate...)
	t.Setenv(clitest.RunnerFixtureLoseAckEnv, "")
	if code != cli.ExitError || !strings.Contains(output, "session ended") {
		t.Fatalf("a lost acknowledgement must fail the observation, not fabricate one: exit %d: %s", code, output)
	}
	record := clitest.OnlyAttemptRecord(t, root)
	if record.State != runnerprovider.StateSubmitting || record.Attempt != "" {
		t.Fatalf("the record must keep the unacknowledged submission: %+v", record)
	}
	if attempts := providerAttempts(t, providerRoot); len(attempts) != 1 {
		t.Fatalf("the provider accepted %d attempts", len(attempts))
	}
	if _, err := os.Stat(filepath.Join(root, "app", "calls")); !os.IsNotExist(err) {
		t.Fatal("a task ran locally after the lost acknowledgement")
	}
	// The same command again resolves the key before it submits anything: the
	// attempt the provider already runs is adopted, never duplicated.
	code, output = clitest.RunGateArgs(t, root, gate...)
	if code != 0 || !strings.Contains(output, "resuming remote attempt") || strings.Contains(output, "executing remotely through") {
		t.Fatalf("the pending submission was not resumed: exit %d: %s", code, output)
	}
	if attempts := providerAttempts(t, providerRoot); len(attempts) != 1 {
		t.Fatalf("a second attempt was created: %v", attempts)
	}
	if got := clitest.ExecutedCalls(t, providerRoot); strings.Count(got, "ran committed\n") != 1 {
		t.Fatalf("expected exactly one execution: %q", got)
	}
	record = clitest.OnlyAttemptRecord(t, root)
	if !record.Imported || record.SessionID == "" || record.State != runner.StateCompleted {
		t.Fatalf("record after resume: %+v", record)
	}
	session, _ := clitest.ReadPortableSession(t, root, record.SessionID)
	if session.Placement == nil || session.Placement.Provenance == nil || session.Placement.Provenance.Submission != record.Submission ||
		session.Placement.Provenance.InputDigest != record.InputDigest || session.Placement.Provenance.SourceDigest != record.SourceDigest {
		t.Fatalf("the imported session does not name this submission: %+v vs %+v", session.Placement, record)
	}
	// A settled record never blocks: the same gate again is an intentional
	// retry and gets a new attempt under a new key.
	code, output = clitest.RunGateArgs(t, root, gate...)
	if code != 0 || !strings.Contains(output, "executing remotely through") {
		t.Fatalf("intentional retry: exit %d: %s", code, output)
	}
	if attempts := providerAttempts(t, providerRoot); len(attempts) != 2 {
		t.Fatalf("an intentional retry must be a new attempt: %v", attempts)
	}
	if records := clitest.AttemptRecords(t, root); len(records) != 2 {
		t.Fatalf("expected two records, found %d", len(records))
	}
}

// The provider half of the same invariant, driven over the RPC directly: one
// idempotency key is one attempt however many submit envelopes carry it, and
// lookup answers the key with that attempt; a different key is a new attempt.
func TestPortableProviderSubmitIsIdempotentByKey(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "durable-lifecycle", "duplicate-submit-is-one-attempt")
	providerRoot := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	session, err := runnerprovider.Spawn(ctx, runnerprovider.LaunchSpec{Command: executable, Dir: providerRoot,
		Env: append(os.Environ(), clitest.RunnerFixtureRoleEnv+"=provider", clitest.RunnerFixtureRootEnv+"="+providerRoot)}, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	if _, err := session.Initialize(ctx, &runner.InitializeParams{ProtocolVersion: runner.ProviderProtocolVersion, ExchangeDir: t.TempDir(),
		Capabilities: []string{runner.CapabilityExecutionRequestV1, runner.CapabilitySessionBundleV1}}); err != nil {
		t.Fatal(err)
	}
	manifest := runner.SourceManifest{Version: runner.SourceManifestVersion, Entries: []runner.SourceEntry{}}
	digest, err := runner.SourceDigest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var request runner.ExecutionRequest
	if err := json.Unmarshal(clitest.BoundFixtureRequest(t, portableFixtureRepo(t), "/app:build~check"), &request); err != nil {
		t.Fatal(err)
	}
	request.Source.Digest = digest
	missed, err := session.Lookup(ctx, &runner.LookupParams{IdempotencyKey: request.Control.IdempotencyKey})
	if err != nil || missed.Attempt != "" {
		t.Fatalf("lookup before submit = %+v, %v", missed, err)
	}
	first, err := session.Submit(ctx, &runner.SubmitParams{Request: request, Manifest: manifest})
	if err != nil {
		t.Fatal(err)
	}
	second, err := session.Submit(ctx, &runner.SubmitParams{Request: request, Manifest: manifest})
	if err != nil || second.Attempt != first.Attempt {
		t.Fatalf("a repeated submit created another attempt: %+v vs %+v (%v)", second, first, err)
	}
	found, err := session.Lookup(ctx, &runner.LookupParams{IdempotencyKey: request.Control.IdempotencyKey})
	if err != nil || found.Attempt != first.Attempt || found.ExecutionInputDigest == "" {
		t.Fatalf("lookup after submit = %+v, %v", found, err)
	}
	request.Control.IdempotencyKey = strings.Repeat("b", 32)
	third, err := session.Submit(ctx, &runner.SubmitParams{Request: request, Manifest: manifest})
	if err != nil || third.Attempt == first.Attempt {
		t.Fatalf("a new key must be a new attempt: %+v vs %+v (%v)", third, first, err)
	}
	if attempts := providerAttempts(t, providerRoot); len(attempts) != 2 {
		t.Fatalf("expected two attempts, found %v", attempts)
	}
	for _, attempt := range []string{first.Attempt, third.Attempt} {
		if _, err := session.Cancel(ctx, &runner.CancelParams{Attempt: attempt}); err != nil {
			t.Fatal(err)
		}
	}
}

// portableFixtureRepo returns a committed placement fixture for request
// building only; nothing executes in it.
func portableFixtureRepo(t *testing.T) string {
	t.Helper()
	root, _ := clitest.PortableFixture(t, false, false)
	return root
}

func TestPortableRestartMidRunResumesByReference(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "durable-lifecycle", "cli-restart-resumes-by-reference")
	root, providerRoot, hold := heldTaskFixture(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// A whole CLI process submits and follows; it is killed while its task
	// runs, exactly as a closed terminal or a crashed agent would kill it.
	process := exec.Command(executable, "build", "--projects", "app", "--no-cache", "--where", "remote")
	process.Dir = root
	process.Env = append(os.Environ(), clitest.RunnerFixtureRoleEnv+"=cli")
	process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var cliOutput strings.Builder
	process.Stdout, process.Stderr = &cliOutput, &cliOutput
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, hold+".running", 60*time.Second)
	// Kill on the state this test asserts about, not on a proxy for it.
	waitForAttemptState(t, root, runner.StateRunning, 60*time.Second)
	_ = syscall.Kill(-process.Process.Pid, syscall.SIGKILL)
	_ = process.Wait()
	record := clitest.OnlyAttemptRecord(t, root)
	if record.Attempt == "" || record.State != runner.StateRunning {
		t.Fatalf("the killed CLI left no resumable record: %+v (%s)", record, cliOutput.String())
	}
	if _, err := os.Stat(filepath.Join(root, ".putnami", "sessions")); !os.IsNotExist(err) {
		t.Fatal("a session was recorded before the attempt finished")
	}
	clitest.WriteFile(t, hold, "")
	code, output := clitest.RunGateArgs(t, root, "sessions", "inspect", "--run", record.Attempt)
	if code != 0 {
		t.Fatalf("sessions inspect --run: exit %d: %s", code, output)
	}
	for _, want := range []string{"resuming remote attempt " + record.Attempt, "remote session ", "imported", "Attempt:    " + record.Attempt, "Session:  "} {
		if !strings.Contains(output, want) {
			t.Fatalf("resume output lacks %q: %s", want, output)
		}
	}
	if attempts := providerAttempts(t, providerRoot); len(attempts) != 1 {
		t.Fatalf("resume created another attempt: %v", attempts)
	}
	if got := clitest.ExecutedCalls(t, providerRoot); strings.Count(got, "ran committed\n") != 1 {
		t.Fatalf("expected exactly one execution: %q", got)
	}
	record = clitest.OnlyAttemptRecord(t, root)
	if !record.Imported || record.SessionID == "" || record.State != runner.StateCompleted {
		t.Fatalf("record after resume: %+v", record)
	}
	session, _ := clitest.ReadPortableSession(t, root, record.SessionID)
	if session.Run.ExitCode != 0 || session.Placement == nil || session.Placement.Provenance == nil || session.Placement.Provenance.Submission != record.Submission {
		t.Fatalf("imported session: %+v %+v", session.Run, session.Placement)
	}
	// The imported attempt is shown from the store from now on, and the same
	// reference resolves through the submission key too.
	code, output = clitest.RunGateArgs(t, root, "sessions", "inspect", "--run", record.Submission)
	if code != 0 || strings.Contains(output, "resuming") || !strings.Contains(output, "Session:  "+record.SessionID) {
		t.Fatalf("inspect by submission key: exit %d: %s", code, output)
	}
	if code, output := clitest.RunGateArgs(t, root, "sessions", "inspect", "--run", "no-such-attempt"); code != cli.ExitUsage || !strings.Contains(output, "no attempt record names") {
		t.Fatalf("unknown reference: exit %d: %s", code, output)
	}
	if code, output := clitest.RunGateArgs(t, root, "sessions", "inspect", record.SessionID, "--run", record.Attempt); code != cli.ExitUsage {
		t.Fatalf("id beside --run: exit %d: %s", code, output)
	}
	// Structured output carries the inspected session document, never the
	// forwarded stream or the record header.
	code, output = clitest.RunGateArgs(t, root, "sessions", "inspect", "--run", record.Attempt, "--output=jsonl")
	if code != 0 || strings.Contains(output, "Attempt:") || !strings.Contains(output, `"`+record.SessionID+`"`) {
		t.Fatalf("structured inspect by reference: exit %d: %s", code, output)
	}
}

// A record can only be resumed through the provider it names; a workspace
// that lost its provider reports that, and a workspace whose provider changed
// refuses to guess.
func TestPortableInspectRunNeedsTheRecordedProvider(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "durable-lifecycle", "cli-restart-resumes-by-reference")
	root, _ := clitest.PortableFixture(t, false, false)
	store := runnerprovider.NewAttemptStore(root)
	record := &runnerprovider.AttemptRecord{Submission: strings.Repeat("ab", 16), Provider: "@fixture/runner", InputDigest: runner.BlobDigest(nil),
		SourceDigest: runner.BlobDigest(nil), Attempt: "fixture-attempt-gone", State: runner.StateRunning}
	if err := store.Write(record); err != nil {
		t.Fatal(err)
	}
	code, output := clitest.RunGateArgs(t, root, "sessions", "inspect", "--run", record.Attempt)
	if code != cli.ExitUsage || !strings.Contains(output, "install exactly one extension declaring") {
		t.Fatalf("no provider: exit %d: %s", code, output)
	}
	record.Provider = "@other/runner"
	if err := store.Write(record); err != nil {
		t.Fatal(err)
	}
	withProvider, _ := clitest.PortableFixture(t, false, true)
	if err := runnerprovider.NewAttemptStore(withProvider).Write(record); err != nil {
		t.Fatal(err)
	}
	code, output = clitest.RunGateArgs(t, withProvider, "sessions", "inspect", "--run", record.Attempt)
	if code != cli.ExitError || !strings.Contains(output, "went through @other/runner") {
		t.Fatalf("provider changed: exit %d: %s", code, output)
	}
	// An attempt the provider no longer knows is reported, never resubmitted.
	record.Provider = "@fixture/runner"
	if err := runnerprovider.NewAttemptStore(withProvider).Write(record); err != nil {
		t.Fatal(err)
	}
	code, output = clitest.RunGateArgs(t, withProvider, "sessions", "inspect", "--run", record.Attempt)
	if code != cli.ExitError || !strings.Contains(output, "no longer knows submission") {
		t.Fatalf("unknown submission: exit %d: %s", code, output)
	}
	if kept, err := store.Find(record.Attempt); err != nil || kept.State != runner.StateRunning {
		t.Fatalf("the record was rewritten: %+v, %v", kept, err)
	}
}

func TestPortableStreamDisconnectReplaysFromCursorWithoutDuplicates(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "durable-lifecycle", "stream-disconnect-replays-from-cursor")
	root, providerRoot := clitest.PortableFixture(t, true, true)
	t.Setenv(clitest.RunnerFixtureDropFollowsEnv, "2")
	code, output := clitest.RunGateArgs(t, root, "lint,test,build,validate,validate-workspace", "--projects", "app", "--no-cache", "--continue-on-error", "--where", "remote")
	if code != 1 {
		t.Fatalf("remote failing gate across a disconnect: exit %d: %s", code, output)
	}
	if !strings.Contains(output, "reconnecting from cursor") {
		t.Fatalf("the disconnect was not observed: %s", output)
	}
	if spawns := clitest.CountLines(t, filepath.Join(providerRoot, "spawns")); spawns != 2 {
		t.Fatalf("expected the provider to be spawned twice (once before the drop, once after), got %d", spawns)
	}
	if attempts := providerAttempts(t, providerRoot); len(attempts) != 1 {
		t.Fatalf("a reconnect created another attempt: %v", attempts)
	}
	// Every forwarded line appears exactly as often as the attempt's durable
	// output log carries it: the replay from the cursor dropped duplicates and
	// lost nothing, and the failure message survived the disconnect.
	logged := clitest.CountLines(t, filepath.Join(providerRoot, "attempts", attemptDirs(t, providerRoot)[0], clitest.FixtureOutputFile))
	record := clitest.OnlyAttemptRecord(t, root)
	if record.Cursor != int64(logged) || logged == 0 {
		t.Fatalf("cursor %d, output log %d lines", record.Cursor, logged)
	}
	data, _ := os.ReadFile(filepath.Join(providerRoot, "attempts", attemptDirs(t, providerRoot)[0], clitest.FixtureOutputFile))
	failures := strings.Count(string(data), "intentional placement fixture failure")
	if failures == 0 || strings.Count(output, "intentional placement fixture failure") != failures {
		t.Fatalf("failure message forwarded %d times, logged %d times: %s", strings.Count(output, "intentional placement fixture failure"), failures, output)
	}
	session, _ := clitest.ReadPortableSession(t, root, record.SessionID)
	if session.Run.ExitCode != 1 || !strings.Contains(string(clitest.MustJSON(t, session.Run.Failures)), "intentional placement fixture failure") {
		t.Fatalf("imported failing session lost its message: %+v", session.Run)
	}
}

func attemptDirs(t *testing.T, providerRoot string) []string {
	t.Helper()
	dirs := providerAttempts(t, providerRoot)
	if len(dirs) == 0 {
		t.Fatal("no attempt directory")
	}
	return dirs
}

func TestPortableInterruptCancelsAndTerminatesTheProcessTree(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "durable-lifecycle", "interrupt-cancels-and-cleans-up")
	root, providerRoot, hold := heldTaskFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var code int
	var output string
	go func() {
		defer close(done)
		code, output = clitest.RunGateArgsContext(t, ctx, root, "build", "--projects", "app", "--no-cache", "--where", "remote")
	}()
	waitForFile(t, hold+".running", 60*time.Second)
	pidData, err := os.ReadFile(hold + ".pid")
	if err != nil {
		t.Fatal(err)
	}
	child, _ := strconv.Atoi(strings.TrimSpace(string(pidData)))
	state := clitest.ReadFixtureState(filepath.Join(providerRoot, "attempts", attemptDirs(t, providerRoot)[0]))
	if state.Engine == 0 || child == 0 {
		t.Fatalf("no engine or child pid to observe: %+v, %d", state, child)
	}
	// Ctrl-C: the run's context is canceled exactly as the signal handler does it.
	cancel()
	<-done
	if code != cli.ExitSignalReceived {
		t.Fatalf("interrupted remote run: exit %d, want %d: %s", code, cli.ExitSignalReceived, output)
	}
	for _, want := range []string{"canceling remote attempt", "canceled"} {
		if !strings.Contains(output, want) {
			t.Fatalf("cancellation report lacks %q: %s", want, output)
		}
	}
	record := clitest.OnlyAttemptRecord(t, root)
	if record.State != runner.StateCanceled || record.Imported || record.SessionID != "" {
		t.Fatalf("record after cancel: %+v", record)
	}
	if _, err := os.Stat(filepath.Join(root, ".putnami", "sessions")); !os.IsNotExist(err) {
		t.Fatal("a canceled attempt imported a session")
	}
	if !processGone(state.Engine, 15*time.Second) {
		t.Fatalf("the executing engine %d survived the cancellation", state.Engine)
	}
	if !processGone(child, 15*time.Second) {
		_ = syscall.Kill(child, syscall.SIGKILL)
		t.Fatalf("the task's background child %d survived the cancellation: the process tree was not terminated", child)
	}
	// The attempt is terminal; inspecting it reports the observed state and
	// resubmits nothing.
	code, output = clitest.RunGateArgs(t, root, "sessions", "inspect", "--run", record.Attempt)
	if code != cli.ExitError || !strings.Contains(output, "ended canceled") {
		t.Fatalf("inspect of a canceled attempt: exit %d: %s", code, output)
	}
	if attempts := providerAttempts(t, providerRoot); len(attempts) != 1 {
		t.Fatalf("inspect resubmitted: %v", attempts)
	}
}

func TestPortableInterruptBeforeExecutionNeverStartsTheEngine(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "durable-lifecycle", "interrupt-cancels-and-cleans-up")
	root, providerRoot := clitest.PortableFixture(t, false, true)
	hold := filepath.Join(t.TempDir(), "launch-hold")
	t.Setenv(clitest.RunnerFixtureHoldEnv, hold)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var code int
	var output string
	go func() {
		defer close(done)
		code, output = clitest.RunGateArgsContext(t, ctx, root, "build", "--projects", "app", "--no-cache", "--where", "remote")
	}()
	waitForFile(t, hold+".ready", 60*time.Second)
	cancel()
	<-done
	if code != cli.ExitSignalReceived || !strings.Contains(output, "canceled") {
		t.Fatalf("interrupt before execution: exit %d: %s", code, output)
	}
	dir := filepath.Join(providerRoot, "attempts", attemptDirs(t, providerRoot)[0])
	if state := clitest.ReadFixtureState(dir); state.State != runner.StateCanceled || state.Engine != 0 {
		t.Fatalf("the engine started despite the cancellation: %+v", state)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "putnami-source-*", "app", "calls")); len(matches) != 0 {
		t.Fatalf("a task executed after the cancellation: %v", matches)
	}
	if record := clitest.OnlyAttemptRecord(t, root); record.State != runner.StateCanceled {
		t.Fatalf("record: %+v", record)
	}
}

// An interrupt that races the completion converges on the one outcome the
// provider recorded: the completed attempt is imported and the exit code is
// the remote gate's, whichever side won the race.
func TestPortableInterruptAfterCompletionKeepsTheVerdict(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "durable-lifecycle", "interrupt-cancels-and-cleans-up")
	root, providerRoot, hold := heldTaskFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var code int
	var output string
	go func() {
		defer close(done)
		code, output = clitest.RunGateArgsContext(t, ctx, root, "build", "--projects", "app", "--no-cache", "--where", "remote")
	}()
	waitForFile(t, hold+".running", 60*time.Second)
	clitest.WriteFile(t, hold, "")
	dir := filepath.Join(providerRoot, "attempts", attemptDirs(t, providerRoot)[0])
	start := time.Now()
	for clitest.ReadFixtureState(dir).State != runner.StateCompleted {
		if time.Since(start) > 60*time.Second {
			t.Fatalf("the attempt did not complete: %+v", clitest.ReadFixtureState(dir))
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if code != 0 {
		t.Fatalf("interrupt after completion must keep the remote verdict: exit %d: %s", code, output)
	}
	record := clitest.OnlyAttemptRecord(t, root)
	if !record.Imported || record.SessionID == "" || record.State != runner.StateCompleted {
		t.Fatalf("record: %+v", record)
	}
	if attempts := providerAttempts(t, providerRoot); len(attempts) != 1 {
		t.Fatalf("attempts: %v", attempts)
	}
	// Cancel after the fact through the RPC: the terminal state is kept.
	executable, _ := os.Executable()
	session, err := runnerprovider.Spawn(ctx, runnerprovider.LaunchSpec{Command: executable, Dir: providerRoot,
		Env: append(os.Environ(), clitest.RunnerFixtureRoleEnv+"=provider", clitest.RunnerFixtureRootEnv+"="+providerRoot)}, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	if _, err := session.Initialize(context.Background(), &runner.InitializeParams{ProtocolVersion: runner.ProviderProtocolVersion, ExchangeDir: t.TempDir(),
		Capabilities: []string{runner.CapabilityExecutionRequestV1, runner.CapabilitySessionBundleV1}}); err != nil {
		t.Fatal(err)
	}
	if ack, err := session.Cancel(context.Background(), &runner.CancelParams{Attempt: record.Attempt}); err != nil || ack.State != runner.StateCompleted {
		t.Fatalf("cancel of a completed attempt = %+v, %v", ack, err)
	}
}

func TestPortableExpiredArtifactsAreSurfacedAndNeverRerun(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "durable-lifecycle", "expired-artifacts-surfaced-not-rerun")
	root, providerRoot := clitest.PortableFixture(t, false, true)
	t.Setenv(clitest.RunnerFixtureExpireFetchEnv, "1")
	code, output := clitest.RunGateArgs(t, root, "build", "--projects", "app", "--no-cache", "--where", "remote")
	if code != cli.ExitError || !strings.Contains(output, "could not be retrieved") || !strings.Contains(output, "expired") {
		t.Fatalf("expired artifacts must be an explicit failure: exit %d: %s", code, output)
	}
	record := clitest.OnlyAttemptRecord(t, root)
	if record.State != runner.StateCompleted || record.ExitCode == nil || *record.ExitCode != 0 || record.Imported || record.SessionID != "" || !strings.Contains(record.Error, "expired") {
		t.Fatalf("the remote verdict must be kept beside the retrieval failure: %+v", record)
	}
	if _, err := os.Stat(filepath.Join(root, ".putnami", "sessions")); !os.IsNotExist(err) {
		t.Fatal("a session was fabricated for expired artifacts")
	}
	// Inspecting the attempt while the artifacts are still gone reports it
	// again; nothing is rerun and nothing runs locally.
	code, output = clitest.RunGateArgs(t, root, "sessions", "inspect", "--run", record.Attempt)
	if code != cli.ExitError || !strings.Contains(output, "expired") {
		t.Fatalf("inspect of expired artifacts: exit %d: %s", code, output)
	}
	if attempts := providerAttempts(t, providerRoot); len(attempts) != 1 {
		t.Fatalf("a retrieval failure reran the work: %v", attempts)
	}
	if _, err := os.Stat(filepath.Join(root, "app", "calls")); !os.IsNotExist(err) {
		t.Fatal("a task ran locally after the retrieval failure")
	}
	// Once the artifacts are served again, the same reference imports them
	// through the one import path.
	t.Setenv(clitest.RunnerFixtureExpireFetchEnv, "")
	code, output = clitest.RunGateArgs(t, root, "sessions", "inspect", "--run", record.Attempt)
	if code != 0 || !strings.Contains(output, "imported") {
		t.Fatalf("import after the artifacts returned: exit %d: %s", code, output)
	}
	record = clitest.OnlyAttemptRecord(t, root)
	if !record.Imported || record.SessionID == "" || record.Error != "" {
		t.Fatalf("record after import: %+v", record)
	}
	if got := clitest.ExecutedCalls(t, providerRoot); strings.Count(got, "ran committed\n") != 1 {
		t.Fatalf("expected exactly one execution: %q", got)
	}
}

func TestPortableCorruptImportKeepsTheRemoteVerdict(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "durable-lifecycle", "corrupt-import-keeps-remote-verdict")
	root, providerRoot := clitest.PortableFixture(t, true, true)
	t.Setenv(clitest.RunnerFixtureCorruptBundleEnv, "1")
	code, output := clitest.RunGateArgs(t, root, "build", "--projects", "app", "--no-cache", "--where", "remote")
	if code != cli.ExitError || !strings.Contains(output, "could not be imported") || !strings.Contains(output, "digest") {
		t.Fatalf("a corrupt bundle must be an explicit import failure: exit %d: %s", code, output)
	}
	record := clitest.OnlyAttemptRecord(t, root)
	if record.State != runner.StateCompleted || record.ExitCode == nil || *record.ExitCode != 1 || record.Imported || record.SessionID != "" || !strings.Contains(record.Error, "digest") {
		t.Fatalf("the remote failure must be kept, unrewritten, beside the import failure: %+v", record)
	}
	if _, err := os.Stat(filepath.Join(root, ".putnami", "sessions")); !os.IsNotExist(err) {
		t.Fatal("a corrupt bundle left a session behind")
	}
	t.Setenv(clitest.RunnerFixtureCorruptBundleEnv, "")
	code, output = clitest.RunGateArgs(t, root, "sessions", "inspect", "--run", record.Attempt)
	if code != 0 || !strings.Contains(output, "imported") || !strings.Contains(output, "intentional placement fixture failure") {
		t.Fatalf("import of the intact bundle: exit %d: %s", code, output)
	}
	record = clitest.OnlyAttemptRecord(t, root)
	session, _ := clitest.ReadPortableSession(t, root, record.SessionID)
	if session.Run.ExitCode != 1 || session.Run.Counts.Failed != 1 {
		t.Fatalf("the imported verdict was rewritten: %+v", session.Run)
	}
	if attempts := providerAttempts(t, providerRoot); len(attempts) != 1 {
		t.Fatalf("an import failure reran the work: %v", attempts)
	}
	if got := clitest.ExecutedCalls(t, providerRoot); strings.Count(got, "ran committed\n") != 1 {
		t.Fatalf("expected exactly one execution: %q", got)
	}
}

func executedCallsPath(t *testing.T, providerRoot string) string {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(providerRoot, "attempts", "*", "putnami-source-*", "app", "calls"))
	if len(matches) != 1 {
		t.Fatalf("expected one executed snapshot, found %v", matches)
	}
	return matches[0]
}

// A task inside the snapshot that runs the CLI again records a NESTED session
// beside the gate's; the bundle carries both with their parent link, the
// import keeps it, and the sessions ledger counts one gate.
func TestPortableNestedSessionsAreImportedAndCountedOnce(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "durable-lifecycle", "nested-sessions-accounted-once")
	root, providerRoot := clitest.PortableFixture(t, false, true)
	clitest.WriteFile(t, filepath.Join(root, "lib", "putnami.json"), `{"name":"lib","extensions":["@fixture/gate"]}`)
	clitest.WriteFile(t, filepath.Join(root, "lib", "gate.project"), "")
	clitest.WriteFile(t, filepath.Join(root, "lib", "marker"), "nested\n")
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"placement-fixture","includes":["extension","runner","app","lib"]}`)
	clitest.WriteFile(t, filepath.Join(root, "extension", "gate.sh"), `echo "ran $(cat "$PUTNAMI_PROJECT_ROOT/marker")" >> "$PUTNAMI_PROJECT_ROOT/calls"
if [ "$(basename "$PUTNAMI_PROJECT_ROOT")" = app ] && [ "${PUTNAMI_RUNNER_FIXTURE:-}" = cli ]; then
  "$PUTNAMI_CLI_EXECUTABLE" build --projects lib --no-cache || exit 9
fi
`)
	clitest.RunGit(t, root, "add", "-A")
	clitest.RunGit(t, root, "commit", "-q", "-m", "nested")
	code, output := clitest.RunGateArgs(t, root, "build", "--projects", "app", "--no-cache", "--where", "remote")
	if code != 0 {
		t.Fatalf("remote gate with a nested run: exit %d: %s", code, output)
	}
	if !strings.Contains(output, "(2 recorded, 0 already present)") {
		t.Fatalf("expected the gate and its nested session to be imported: %s", output)
	}
	record := clitest.OnlyAttemptRecord(t, root)
	gate, _ := clitest.ReadPortableSession(t, root, record.SessionID)
	if gate.ParentSessionID != "" {
		t.Fatalf("the gate session carries a parent: %+v", gate.ParentSessionID)
	}
	code, output = clitest.RunGateArgs(t, root, "sessions", "summary", "--output=json")
	if code != 0 {
		t.Fatalf("sessions summary: exit %d: %s", code, output)
	}
	var envelope struct {
		Data []struct {
			SessionID       string   `json:"sessionId"`
			ParentSessionID string   `json:"parentSessionId"`
			Nested          bool     `json:"nested"`
			Commands        []string `json:"commands"`
		} `json:"data"`
	}
	start := strings.Index(output, "{")
	if start < 0 || json.Unmarshal([]byte(output[start:]), &envelope) != nil || len(envelope.Data) != 2 {
		t.Fatalf("summary rows: %s", output)
	}
	nested, top := 0, 0
	for _, row := range envelope.Data {
		switch {
		case row.Nested && row.ParentSessionID == record.SessionID && row.SessionID != record.SessionID:
			nested++
		case !row.Nested && row.SessionID == record.SessionID:
			top++
		default:
			t.Fatalf("unexpected row: %+v", row)
		}
	}
	if nested != 1 || top != 1 {
		t.Fatalf("nested %d top %d", nested, top)
	}
	code, output = clitest.RunGateArgs(t, root, "sessions", "summary", "--command", "build", "--output=json")
	start = strings.Index(output, "{")
	if code != 0 || start < 0 || json.Unmarshal([]byte(output[start:]), &envelope) != nil || len(envelope.Data) != 1 || envelope.Data[0].SessionID != record.SessionID {
		t.Fatalf("the nested run was counted as a gate: %s", output)
	}
	if calls := clitest.ExecutedCalls(t, providerRoot); strings.Count(calls, "ran committed\n") != 1 {
		t.Fatalf("app executed %q", calls)
	}
	nestedCalls, err := os.ReadFile(strings.Replace(executedCallsPath(t, providerRoot), "/app/calls", "/lib/calls", 1))
	if err != nil || string(nestedCalls) != "ran nested\n" {
		t.Fatalf("the nested run did not execute lib once in the snapshot: %q (%v)", nestedCalls, err)
	}
}

func TestPortableSessionsListFindsTheJudgedRevision(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "durable-lifecycle", "sessions-list-finds-judged-revision")
	root, _ := clitest.PortableFixture(t, false, true)
	head := strings.TrimSpace(clitest.GitOutput(t, root, "rev-parse", "HEAD"))
	if code, output := clitest.RunGateArgs(t, root, "build", "--projects", "app", "--no-cache", "--where", "local"); code != 0 {
		t.Fatalf("local: %d %s", code, output)
	}
	localID := clitest.LatestSessionID(t, root)
	if code, output := clitest.RunGateArgs(t, root, "build", "--projects", "app", "--no-cache", "--where", "remote"); code != 0 {
		t.Fatalf("remote: %d %s", code, output)
	}
	remoteID := clitest.LatestSessionID(t, root)
	code, output := clitest.RunGateArgs(t, root, "sessions", "list", "--revision", head[:10])
	if code != 0 || !strings.Contains(output, localID) || !strings.Contains(output, remoteID) || !strings.Contains(output, "REVISION") {
		t.Fatalf("sessions list --revision: exit %d: %s", code, output)
	}
	for _, line := range strings.Split(output, "\n") {
		switch {
		case strings.Contains(line, localID) && !strings.Contains(line, " local"):
			t.Fatalf("local placement missing: %s", line)
		case strings.Contains(line, remoteID) && !strings.Contains(line, " remote"):
			t.Fatalf("remote placement missing: %s", line)
		}
	}
	code, output = clitest.RunGateArgs(t, root, "sessions", "list", "--revision", strings.Repeat("0", 12))
	if code != 0 || !strings.Contains(output, "No recorded session judged revision") || strings.Contains(output, remoteID) {
		t.Fatalf("unmatched revision: exit %d: %s", code, output)
	}
	for _, bad := range []string{"main", "HEAD", "abc"} {
		if code, output := clitest.RunGateArgs(t, root, "sessions", "list", "--revision", bad); code != cli.ExitUsage {
			t.Fatalf("--revision %s: exit %d: %s", bad, code, output)
		}
	}
	code, output = clitest.RunGateArgs(t, root, "sessions", "list", "--revision", head, "--output=jsonl")
	if code != 0 {
		t.Fatalf("jsonl: %d %s", code, output)
	}
	// `sessions` is a structured command: its rows travel as the shared result
	// envelope's data array, each carrying the additive identity members.
	var envelope struct {
		Data []struct {
			ID        string                        `json:"id"`
			Revision  string                        `json:"revision"`
			Placement *protocolcli.SessionPlacement `json:"placement"`
		} `json:"data"`
	}
	start := strings.Index(output, "{")
	if start < 0 || json.Unmarshal([]byte(output[start:]), &envelope) != nil || len(envelope.Data) != 2 {
		t.Fatalf("expected two rows: %s", output)
	}
	for _, row := range envelope.Data {
		if row.Revision != head || row.Placement == nil {
			t.Fatalf("row lacks revision or placement: %+v", row)
		}
		if row.ID == remoteID && (row.Placement.Actual != "remote" || row.Placement.Provenance == nil) {
			t.Fatalf("remote row: %+v", row)
		}
		if row.ID == localID && (row.Placement.Actual != "local" || row.Placement.Provenance != nil) {
			t.Fatalf("local row: %+v", row)
		}
	}
}
