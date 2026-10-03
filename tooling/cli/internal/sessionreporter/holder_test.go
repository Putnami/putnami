package sessionreporter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/sessionstream"
)

// The variables that run this test binary as a reporter
// (TestReporterHolderHelperProcess): the record it appends to, and how it
// answers.
const (
	holderRecordEnv = "SESSIONREPORTER_HOLDER_RECORD"
	holderModeEnv   = "SESSIONREPORTER_HOLDER_MODE"
)

// The ways the helper reporter answers.
const (
	// v2Reporter accepts initialize and authenticate, then acknowledges
	// every chunk.
	v2Reporter = "v2"
	// v1Reporter parses every line as a chunk, and exits on any other line.
	v1Reporter = "v1"
	// refusingReporter accepts initialize and refuses authenticate.
	refusingReporter = "refuse"
	// crashingReporter is v2Reporter that exits at its first chunk.
	crashingReporter = "crash"
)

const (
	holderBearer    = "hosted-run-credential-3a7d-held-by-reporters"
	holderEnvToken  = "reporter-env-token-must-not-pass"
	holderSessionID = "20261004-120000-holder"
	holderProvider  = "@test/provider"
)

// holderRecord is one line of the helper's record: a process start with its
// environment, or a line the process read.
type holderRecord struct {
	PID  int      `json:"pid"`
	Env  []string `json:"env,omitempty"`
	Line string   `json:"line,omitempty"`
}

// TestReporterHolderHelperProcess is a reporter subprocess. It records its
// environment and every line it reads, and answers as holderModeEnv says.
func TestReporterHolderHelperProcess(t *testing.T) {
	path := os.Getenv(holderRecordEnv)
	if path == "" {
		return
	}
	record := func(r holderRecord) {
		r.PID = os.Getpid()
		data, _ := json.Marshal(r)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			os.Exit(20)
		}
		_, _ = f.Write(append(data, '\n'))
		_ = f.Close()
	}
	record(holderRecord{Env: os.Environ()})
	mode := os.Getenv(holderModeEnv)
	out := json.NewEncoder(os.Stdout)
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 4096), protocolcli.SessionReportingLineBytes)
	for in.Scan() {
		record(holderRecord{Line: in.Text()})
		if mode != v1Reporter {
			if handshake, err := protocolcli.ParseSessionReportingHandshake(in.Bytes()); err == nil {
				result := handshake.Accept()
				if mode == refusingReporter && handshake.Op == protocolcli.SessionReportingOpAuthenticate {
					result = handshake.Refuse("unauthorized")
				}
				_ = out.Encode(result)
				continue
			}
		}
		chunk, err := protocolcli.ParseSessionReportingChunk(in.Bytes())
		if err != nil || mode == crashingReporter {
			os.Exit(21)
		}
		_ = out.Encode(chunk.Ack())
		if chunk.Final {
			os.Exit(0)
		}
	}
	os.Exit(0)
}

// holderFixture is a log reporter served by the helper process.
type holderFixture struct {
	record string
	launch LaunchSpec
}

// newHolderFixture selects the log reporter with an environment token, and
// launches the helper in mode as its extension's native runtime: this test
// binary. The launch environment carries both tokens, as a CI environment
// that exports them would.
func newHolderFixture(t *testing.T, mode string) holderFixture {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "record.jsonl")
	t.Setenv(protocolcli.SessionReporterEnv, "")
	t.Setenv(protocolcli.SessionReporterTokenEnv, "")
	t.Setenv(protocolcli.LogReporterEnv, holderProvider)
	t.Setenv(protocolcli.LogReporterTokenEnv, holderEnvToken)
	env := append(os.Environ(), holderRecordEnv+"="+record, holderModeEnv+"="+mode,
		protocolcli.LogReporterTokenEnv+"="+holderEnvToken, protocolcli.SessionReporterTokenEnv+"="+holderEnvToken)
	return holderFixture{record: record, launch: LaunchSpec{
		Command: self, Args: []string{"-test.run=^TestReporterHolderHelperProcess$"}, Env: env, Runtime: self,
	}}
}

func (fx holderFixture) resolver() Resolver {
	return func(Capability) Resolve {
		return func(context.Context) (LaunchSpec, error) {
			launch := fx.launch
			launch.Env = slices.Clone(fx.launch.Env)
			return launch, nil
		}
	}
}

// processes returns what each helper process recorded, in start order.
func (fx holderFixture) processes(t *testing.T) []holderProcess {
	t.Helper()
	data, err := os.ReadFile(fx.record)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var processes []holderProcess
	index := map[int]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var r holderRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("helper record %q: %v", line, err)
		}
		if r.Env != nil {
			index[r.PID] = len(processes)
			processes = append(processes, holderProcess{env: r.Env})
			continue
		}
		processes[index[r.PID]].lines = append(processes[index[r.PID]].lines, r.Line)
	}
	return processes
}

type holderProcess struct {
	env   []string
	lines []string
}

// heldNothing fails t when the process saw the bearer or an environment token,
// in its environment or in a line it read.
func (p holderProcess) heldNothing(t *testing.T, name string) {
	t.Helper()
	for _, entry := range p.env {
		key, _, _ := strings.Cut(entry, "=")
		if key == protocolcli.SessionReporterTokenEnv || key == protocolcli.LogReporterTokenEnv ||
			strings.Contains(entry, holderBearer) || strings.Contains(entry, holderEnvToken) {
			t.Errorf("%s: the reporter environment holds %s", name, key)
		}
	}
	for _, line := range p.lines {
		if strings.Contains(line, holderBearer) || strings.Contains(line, holderEnvToken) {
			t.Errorf("%s: the reporter read a credential: %s", name, line)
		}
	}
}

// deliver runs the log reporter of one finished session through StartSelected
// and Finish, adopting what holders started. It returns the events the
// session recorded and every failure.
func deliver(t *testing.T, ctx context.Context, fx holderFixture, holders *Holders) ([]byte, []Failure) {
	t.Helper()
	dir := t.TempDir()
	log, err := sessionstream.Create(dir, holderSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append([]byte(`{"record":"task:start"}`)); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	events, err := os.ReadFile(filepath.Join(dir, sessionstream.EventsFile))
	if err != nil {
		t.Fatal(err)
	}
	runs, failures := StartSelected(WithEventsBatchInterval(ctx, 10*time.Millisecond), log, holderSessionID, fx.resolver(), holders)
	return events, append(failures, runs.Finish()...)
}

// chunkLines are the exact lines a reporter reads for events: one chunk and
// the final marker, as a v1 engine frames them.
func chunkLines(t *testing.T, events []byte) []string {
	t.Helper()
	var lines []string
	for _, chunk := range []protocolcli.SessionReportingChunk{
		protocolcli.NewSessionReportingChunk(holderSessionID, sessionstream.EventsFile, 0, 0, events, false),
		protocolcli.NewSessionReportingChunk(holderSessionID, sessionstream.EventsFile, int64(len(events)), 1, nil, true),
	} {
		data, err := json.Marshal(chunk)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(data))
	}
	return lines
}

// A hosted run starts its reporter before repository code, as a holder, and
// hands it the run credential in authenticate, after the reporter accepted
// initialize, which carries none. The session's run adopts that process, so
// the reporter that holds the credential is the one that receives the
// session. Its environment holds no token, though the engine's held both.
func TestAHostedReporterReceivesTheRunCredentialOverTheProtocol(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "providers-receive-it-over-rpc", "reporters-authenticate-after-initialize")
	fx := newHolderFixture(t, v2Reporter)
	defer runcredential.SetForTest(holderBearer)()
	ctx := Capture(context.Background())
	holders := &Holders{}
	defer holders.Close()

	if failures := holders.Start(ctx, Selected(ctx), fx.resolver()); len(failures) != 0 {
		t.Fatalf("holder start failures: %v", failures[0].Err)
	}
	if got := fx.processes(t); len(got) != 1 || len(got[0].lines) != 2 {
		t.Fatalf("before the session: %d processes, want 1 that read the handshake: %+v", len(got), got)
	}
	events, failures := deliver(t, ctx, fx, holders)
	if len(failures) != 0 {
		t.Fatalf("delivery failures: %v", failures[0].Err)
	}

	processes := fx.processes(t)
	if len(processes) != 1 {
		t.Fatalf("reporter processes = %d, want the one started before the session", len(processes))
	}
	reporter := processes[0]
	for _, entry := range reporter.env {
		key, _, _ := strings.Cut(entry, "=")
		if key == protocolcli.SessionReporterTokenEnv || key == protocolcli.LogReporterTokenEnv || strings.Contains(entry, holderBearer) {
			t.Errorf("the reporter environment holds %s", key)
		}
	}
	if len(reporter.lines) != 4 {
		t.Fatalf("reporter lines = %q, want initialize, authenticate, a chunk and the final marker", reporter.lines)
	}
	if reporter.lines[0] != `{"protocolVersion":2,"op":"initialize"}` {
		t.Errorf("first line = %s, want initialize without a credential", reporter.lines[0])
	}
	authenticate, err := protocolcli.ParseSessionReportingHandshake([]byte(reporter.lines[1]))
	if err != nil || authenticate.Op != protocolcli.SessionReportingOpAuthenticate || authenticate.RunCredential != holderBearer {
		t.Errorf("second line is not authenticate with the run credential: %v", err)
	}
	if want := chunkLines(t, events); !slices.Equal(reporter.lines[2:], want) {
		t.Errorf("chunks = %q, want %q", reporter.lines[2:], want)
	}
}

// Without a run credential the engine starts no reporter early and sends no
// handshake: the reporter reads exactly the chunks a v1 engine frames, and
// receives its token in its environment, as before.
func TestWithoutACredentialAReporterReadsTheSameFrames(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "flag-off-changes-nothing", "flag-off-reporter-frames-unchanged")
	fx := newHolderFixture(t, v1Reporter)
	defer runcredential.SetForTest("")()
	ctx := Capture(context.Background())
	holders := &Holders{}
	defer holders.Close()

	if failures := holders.Start(ctx, Selected(ctx), fx.resolver()); failures != nil || fx.processes(t) != nil {
		t.Fatalf("a run without a credential started a reporter early: %v", failures)
	}
	events, failures := deliver(t, ctx, fx, holders)
	if len(failures) != 0 {
		t.Fatalf("delivery failures: %v", failures[0].Err)
	}
	processes := fx.processes(t)
	if len(processes) != 1 {
		t.Fatalf("reporter processes = %d, want 1", len(processes))
	}
	if !slices.Contains(processes[0].env, protocolcli.LogReporterTokenEnv+"="+holderEnvToken) {
		t.Error("the reporter lost its token from its environment")
	}
	if slices.ContainsFunc(processes[0].env, func(entry string) bool {
		return strings.HasPrefix(entry, protocolcli.SessionReporterTokenEnv+"=")
	}) {
		t.Error("the log reporter received the session reporter's token")
	}
	if want := chunkLines(t, events); !slices.Equal(processes[0].lines, want) {
		t.Errorf("reporter lines = %q, want exactly %q", processes[0].lines, want)
	}
}

// On a hosted run a reporter that cannot hold the run credential gets none:
// one that does not accept initialize, such as a v1 reporter, and one that is
// not its extension's native runtime. Holders.Start names it, and its session
// is delivered by a process started without a credential. Neither process
// sees the bearer or a token, in its environment or on its stdin.
func TestAHostedReporterThatCannotHoldTheCredentialGetsNone(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "providers-receive-it-over-rpc", "a-reporter-that-cannot-hold-it-gets-none")
	for _, tc := range []struct {
		name, mode string
		runtime    func(self string) string
		reason     string
		// started is how many processes Holders.Start starts.
		started int
	}{
		{name: "v1 reporter", mode: v1Reporter, runtime: func(self string) string { return self },
			reason: "did not accept initialize of session reporting protocol 2", started: 1},
		{name: "not the native runtime", mode: v2Reporter, runtime: func(string) string { return "/opt/runtime" },
			reason: "is not its extension's runtime executable", started: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newHolderFixture(t, tc.mode)
			fx.launch.Runtime = tc.runtime(fx.launch.Command)
			defer runcredential.SetForTest(holderBearer)()
			ctx := Capture(context.Background())
			holders := &Holders{}
			defer holders.Close()

			failures := holders.Start(ctx, Selected(ctx), fx.resolver())
			if len(failures) != 1 {
				t.Fatalf("holder start failures = %d, want 1", len(failures))
			}
			for _, want := range []string{"the log reporter of " + holderProvider + " cannot hold the run credential", tc.reason, "it starts without a credential"} {
				if !strings.Contains(failures[0].Err.Error(), want) {
					t.Errorf("failure %q lacks %q", failures[0].Err, want)
				}
			}
			if got := len(fx.processes(t)); got != tc.started {
				t.Fatalf("processes before the session = %d, want %d", got, tc.started)
			}
			events, failures := deliver(t, ctx, fx, holders)
			if len(failures) != 0 {
				t.Fatalf("delivery failures: %v", failures[0].Err)
			}
			processes := fx.processes(t)
			if len(processes) != tc.started+1 {
				t.Fatalf("processes = %d, want %d", len(processes), tc.started+1)
			}
			for i, p := range processes {
				p.heldNothing(t, fmt.Sprintf("process %d", i))
			}
			if tc.started == 1 && !slices.Equal(processes[0].lines, []string{`{"protocolVersion":2,"op":"initialize"}`}) {
				t.Errorf("the v1 reporter read %q, want initialize alone", processes[0].lines)
			}
			if want := chunkLines(t, events); !slices.Equal(processes[tc.started].lines, want) {
				t.Errorf("the plain reporter read %q, want exactly %q", processes[tc.started].lines, want)
			}
		})
	}
}

// A reporter that restarts after repository code ran gets no credential: the
// restart fails like any holder's (runcredential.CustodyError), names the
// reporter, and the delivery fails with it instead of running a reporter
// without a credential. A reporter that refuses the credential is named with
// the refusal's code.
func TestAReporterRestartAfterRepositoryCodeGetsNoCredential(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "hostile-process-finds-nothing", "no-credential-after-repository-code")
	fx := newHolderFixture(t, crashingReporter)
	defer runcredential.SetForTest(holderBearer)()
	ctx := Capture(context.Background())
	holders := &Holders{}
	defer holders.Close()
	if failures := holders.Start(ctx, Selected(ctx), fx.resolver()); len(failures) != 0 {
		t.Fatalf("holder start failures: %v", failures[0].Err)
	}
	runcredential.MarkRepositoryCodeStarted("hook hooks.commands.build.before")

	_, failures := deliver(t, ctx, fx, holders)
	if len(failures) != 1 {
		t.Fatalf("delivery failures = %d, want the restart's refusal", len(failures))
	}
	var custody *runcredential.CustodyError
	if !errors.As(failures[0].Err, &custody) || custody.Holder != "the log reporter of "+holderProvider {
		t.Fatalf("delivery failure = %v, want a custody error naming the log reporter", failures[0].Err)
	}
	if got := len(fx.processes(t)); got != 1 {
		t.Errorf("reporter processes = %d, want the first one alone", got)
	}
	if _, err := startHolder(ctx, "the log reporter of "+holderProvider, fx.launch, time.Second); !errors.As(err, &custody) {
		t.Errorf("a holder start after repository code = %v, want a custody error", err)
	}

	t.Run("refused", func(t *testing.T) {
		fx := newHolderFixture(t, refusingReporter)
		defer runcredential.SetForTest(holderBearer)()
		_, err := startHolder(context.Background(), "the log reporter of "+holderProvider, fx.launch, 5*time.Second)
		if err == nil || err.Error() != "--credential-fd: the log reporter of "+holderProvider+" refused the run credential: unauthorized" {
			t.Fatalf("refused authenticate = %v", err)
		}
		if startsWithoutCredential(err) {
			t.Error("a reporter that refused the credential would start without one")
		}
	})
}

// A hosted run keeps no reporter token: Capture drops both from the
// environment and keeps neither, and no reporter environment gets one, so a
// reporter holds the run credential only through the protocol.
func TestAHostedRunIgnoresTheReporterTokens(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "credential-boundary", "hosted-run-ignores-reporter-tokens")
	t.Setenv(protocolcli.SessionReporterEnv, "@test/session")
	t.Setenv(protocolcli.SessionReporterTokenEnv, "session-token")
	t.Setenv(protocolcli.LogReporterEnv, "@test/logs")
	t.Setenv(protocolcli.LogReporterTokenEnv, "log-token")
	defer runcredential.SetForTest(holderBearer)()
	ctx := Capture(context.Background())
	if SessionReporter.Provider(ctx) != "@test/session" || LogReporter.Provider(ctx) != "@test/logs" {
		t.Fatal("a hosted run lost the reporter selection")
	}
	for _, key := range []string{protocolcli.SessionReporterTokenEnv, protocolcli.LogReporterTokenEnv} {
		if _, ok := os.LookupEnv(key); ok {
			t.Errorf("%s remained ambient", key)
		}
	}
	injected := []string{"PATH=/bin", protocolcli.SessionReporterTokenEnv + "=injected", protocolcli.LogReporterTokenEnv + "=injected"}
	for _, capability := range Capabilities() {
		if capability.selection(ctx).token != "" {
			t.Errorf("%s kept its token", capability.Label)
		}
		if got := capability.providerEnv(ctx, injected); !slices.Equal(got, []string{"PATH=/bin"}) {
			t.Errorf("%s environment = %v, want no token", capability.Label, got)
		}
	}
}
