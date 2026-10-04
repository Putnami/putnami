package sessionreporter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/sessionstream"
)

func TestReportingCaptureAndProviderEnvironment(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "credential-boundary", "explicit-secret-is-withheld-from-hooks-and-tasks")
	t.Setenv(protocolcli.SessionReporterEnv, "@test/provider")
	t.Setenv(protocolcli.SessionReporterTokenEnv, "private-token")
	ctx := Capture(context.Background())
	if os.Getenv(protocolcli.SessionReporterTokenEnv) != "" || os.Getenv(protocolcli.SessionReporterEnv) != "" {
		t.Fatal("reporter authority remained ambient")
	}
	t.Setenv(protocolcli.SessionReporterTokenEnv, "reintroduced")
	ctx = Capture(ctx)
	if SessionReporter.Provider(ctx) != "@test/provider" || os.Getenv(protocolcli.SessionReporterTokenEnv) != "" {
		t.Fatal("second capture lost context or retained ambient token")
	}
	env := SessionReporter.providerEnv(ctx, []string{"PATH=/bin", protocolcli.SessionReporterTokenEnv + "=injected", protocolcli.SessionReporterEnv + "=@attacker/provider"})
	if strings.Join(env, "|") != "PATH=/bin|"+protocolcli.SessionReporterTokenEnv+"=private-token" {
		t.Fatalf("provider environment=%v", env)
	}
	if SessionReporter.Provider(context.Background()) != "" {
		t.Fatal("unconfigured context selected reporter")
	}
	if got := SessionReporter.providerEnv(context.Background(), []string{"PATH=/bin"}); len(got) != 1 {
		t.Fatal(got)
	}
}

// TestReportingCaptureKeepsEachTokenWithItsOwnProvider captures both
// capabilities at once: each provider environment holds its own token only,
// never the other capability's token or either selector.
func TestReportingCaptureKeepsEachTokenWithItsOwnProvider(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "credential-boundary", "each-token-reaches-only-its-own-provider")
	t.Setenv(protocolcli.SessionReporterEnv, " @test/session ")
	t.Setenv(protocolcli.SessionReporterTokenEnv, "session-token")
	t.Setenv(protocolcli.LogReporterEnv, "@test/logs")
	t.Setenv(protocolcli.LogReporterTokenEnv, "log-token")
	ctx := Capture(context.Background())
	for _, key := range []string{protocolcli.SessionReporterEnv, protocolcli.SessionReporterTokenEnv, protocolcli.LogReporterEnv, protocolcli.LogReporterTokenEnv} {
		if value, ok := os.LookupEnv(key); ok {
			t.Fatalf("%s remained ambient: %q", key, value)
		}
	}
	if SessionReporter.Provider(ctx) != "@test/session" || LogReporter.Provider(ctx) != "@test/logs" {
		t.Fatalf("selections = %q, %q", SessionReporter.Provider(ctx), LogReporter.Provider(ctx))
	}
	if got := Selected(ctx); len(got) != 2 || got[0].Name != protocolcli.SessionReporterCommand || got[1].Name != protocolcli.LogReporterCommand {
		t.Fatalf("selected = %+v", got)
	}
	injected := []string{
		"PATH=/bin",
		protocolcli.SessionReporterEnv + "=@attacker/provider", protocolcli.SessionReporterTokenEnv + "=injected",
		protocolcli.LogReporterEnv + "=@attacker/provider", protocolcli.LogReporterTokenEnv + "=injected",
	}
	for capability, want := range map[string]string{
		protocolcli.SessionReporterCommand: "PATH=/bin|" + protocolcli.SessionReporterTokenEnv + "=session-token",
		protocolcli.LogReporterCommand:     "PATH=/bin|" + protocolcli.LogReporterTokenEnv + "=log-token",
	} {
		for _, c := range Capabilities() {
			if c.Name == capability {
				if got := strings.Join(c.providerEnv(ctx, injected), "|"); got != want {
					t.Fatalf("%s provider environment = %s, want %s", capability, got, want)
				}
			}
		}
	}

	t.Setenv(protocolcli.LogReporterEnv, "@test/logs")
	logOnly := Capture(context.Background())
	if SessionReporter.Provider(logOnly) != "" || LogReporter.Provider(logOnly) != "@test/logs" || len(Selected(logOnly)) != 1 {
		t.Fatal("a log reporter selection selected the session reporter")
	}
	if got := LogReporter.providerEnv(logOnly, []string{"PATH=/bin"}); strings.Join(got, "|") != "PATH=/bin" {
		t.Fatalf("a capability without a token received one: %v", got)
	}
	if len(Selected(Capture(context.Background()))) != 0 {
		t.Fatal("an unset environment selected a capability")
	}
}

func TestReportingCursorDurabilityOwnershipAndBounds(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "durable-replay", "outage-retains-identical-frame-and-replay-runs-no-workload")
	dir := t.TempDir()
	r, err := openRun(context.Background(), SessionReporter, dir, "session", "@test/provider")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openRun(context.Background(), SessionReporter, dir, "session", "@test/provider"); err == nil {
		t.Fatal("concurrent reporter acquired cursor")
	}
	// The log reporter holds its own lock and checkpoint beside the session.
	logRun, err := openRun(context.Background(), LogReporter, dir, "session", "@test/provider")
	if err != nil {
		t.Fatalf("the session reporter's lock blocked the log reporter: %v", err)
	}
	logRun.release()
	pending := protocolcli.NewSessionReportingChunk("session", "events.jsonl", 0, 0, []byte("short live chunk"), false)
	r.state.Pending = &pending
	if err := r.save(); err != nil {
		t.Fatal(err)
	}
	r.release()
	if _, err := openRun(context.Background(), SessionReporter, dir, "session", "@other/provider"); err == nil {
		t.Fatal("changed provider reused prior acknowledgements")
	}
	r, err = openRun(context.Background(), SessionReporter, dir, "session", "@test/provider")
	if err != nil {
		t.Fatal(err)
	}
	if r.state.Pending == nil || !r.state.Pending.Ack().Matches(pending) || !bytes.Equal(r.state.Pending.Data, pending.Data) {
		t.Fatal("pending identity was not durably retained")
	}
	r.release()
	if err := os.WriteFile(filepath.Join(dir, StateFile), bytes.Repeat([]byte("x"), 2*protocolcli.SessionReportingLineBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openRun(context.Background(), SessionReporter, dir, "session", "@test/provider"); err == nil {
		t.Fatal("oversized cursor accepted")
	}
}

func TestReportingCursorRejectsCorruption(t *testing.T) {
	for _, mutate := range []func(*state){
		func(s *state) { s.Events.Offset = -1 }, func(s *state) { s.Session.Sequence = -1 }, func(s *state) { s.Events.Final = true }, func(s *state) { s.Complete = true },
		func(s *state) {
			p := protocolcli.NewSessionReportingChunk("other", "events.jsonl", 0, 0, []byte("a"), false)
			s.Pending = &p
		},
		func(s *state) {
			p := protocolcli.NewSessionReportingChunk("session", "events.jsonl", 1, 0, []byte("a"), false)
			s.Pending = &p
		},
	} {
		r := &Run{capability: SessionReporter, state: state{Version: 1, SessionID: "session", Provider: "@test/provider"}}
		mutate(&r.state)
		if err := r.validate(); err == nil {
			t.Fatalf("invalid state accepted: %+v", r.state)
		}
	}
}

// TestLogReportingCursorHasNoSessionArtifact pins the log reporter's checkpoint
// rules: it completes on the events final marker alone and never holds a
// session.json cursor or frame.
func TestLogReportingCursorHasNoSessionArtifact(t *testing.T) {
	valid := &Run{capability: LogReporter, state: state{Version: 1, SessionID: "session", Provider: "@test/provider", Events: cursor{Offset: 3, Sequence: 2, Final: true}, Complete: true}}
	if err := valid.validate(); err != nil || !valid.complete() {
		t.Fatalf("a finalized log reporter cursor was refused: %v", err)
	}
	pending := protocolcli.NewSessionReportingChunk("session", "session.json", 0, 0, []byte("{}"), false)
	for name, mutate := range map[string]func(*state){
		"session cursor":         func(s *state) { s.Session.Offset = 1 },
		"session final":          func(s *state) { s.Session.Final = true },
		"incomplete after final": func(s *state) { s.Complete = false },
		"complete before final":  func(s *state) { s.Events.Final = false },
		"session pending frame":  func(s *state) { s.Events.Final, s.Complete, s.Pending = false, false, &pending },
	} {
		r := &Run{capability: LogReporter, state: valid.state}
		mutate(&r.state)
		if err := r.validate(); err == nil {
			t.Fatalf("%s: invalid log reporter state accepted: %+v", name, r.state)
		}
	}
	session := &Run{capability: SessionReporter, state: valid.state}
	if err := session.validate(); err == nil {
		t.Fatal("the session reporter accepted an events final marker before session.json closed")
	}
}

func pipeProcess() (*process, net.Conn) {
	local, remote := net.Pipe()
	scanner := bufio.NewScanner(local)
	scanner.Buffer(make([]byte, 4096), protocolcli.SessionReportingLineBytes)
	return &process{stdin: local, stdout: local, scanner: scanner}, remote
}

func TestReportingOperationBoundsBlockedWritesAndReads(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "bounded-transport", "writes-reads-retries-and-finalization-are-bounded")
	for _, read := range []bool{false, true} {
		p, remote := pipeProcess()
		if read {
			go func() { _, _ = io.Copy(io.Discard, remote) }()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		_, err := p.call(ctx, protocolcli.NewSessionReportingChunk("session", "events.jsonl", 0, 0, bytes.Repeat([]byte("x"), protocolcli.SessionReportingChunkBytes), false))
		cancel()
		_ = remote.Close()
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("read=%v error=%v", read, err)
		}
	}
}

func TestReportingRetryBudgetAndAckIdentity(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "bounded-transport", "writes-reads-retries-and-finalization-are-bounded")
	for _, mode := range []string{"retry", "wrong", "refused", "oversized", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			p, remote := pipeProcess()
			defer func() { _ = remote.Close(); _ = p.stdin.Close() }()
			calls := make(chan int, 1)
			go func() {
				scanner := bufio.NewScanner(remote)
				count := 0
				for scanner.Scan() {
					count++
					chunk, err := protocolcli.ParseSessionReportingChunk(scanner.Bytes())
					if err != nil {
						break
					}
					ack := chunk.Ack()
					switch mode {
					case "retry":
						ack.OK = false
						ack.Retryable = true
						ack.Code = "unavailable"
					case "wrong":
						ack.Offset++
					case "refused":
						ack.OK = false
						ack.Code = "forbidden"
					case "oversized":
						_, _ = remote.Write(bytes.Repeat([]byte("x"), protocolcli.SessionReportingLineBytes+1))
						calls <- count
						return
					case "malformed":
						_, _ = remote.Write([]byte("secret-token\n"))
						calls <- count
						return
					}
					_ = json.NewEncoder(remote).Encode(ack)
					if mode != "retry" || count == maxAttempts {
						calls <- count
						return
					}
				}
				calls <- count
			}()
			chunk := protocolcli.NewSessionReportingChunk("session", "events.jsonl", 0, 0, []byte("abc"), false)
			var err error
			if mode == "oversized" || mode == "malformed" {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				_, err = p.call(ctx, chunk)
				cancel()
			} else {
				r := &Run{ctx: context.Background(), process: p, opTimeout: time.Second}
				err = r.send(chunk)
			}
			if err == nil || strings.Contains(err.Error(), "secret-token") {
				t.Fatalf("unsafe error: %v", err)
			}
			_ = remote.Close()
			count := <-calls
			want := 1
			if mode == "retry" {
				want = maxAttempts
			}
			if count != want {
				t.Fatalf("calls=%d want=%d", count, want)
			}
		})
	}
}

func TestReportingArtifactChunkBoundaries(t *testing.T) {
	dir := t.TempDir()
	data := bytes.Repeat([]byte("x"), protocolcli.SessionReportingChunkBytes+7)
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	events, err := sessionstream.Open(dir, "session")
	if err != nil {
		t.Fatal(err)
	}
	sub, err := events.Subscribe(protocolcli.SessionReporterCommand, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	r := &Run{dir: dir, events: sub, state: state{SessionID: "session"}}
	first, err := r.next("events.jsonl", false)
	if err != nil || len(first.Data) != protocolcli.SessionReportingChunkBytes {
		t.Fatalf("first=%v err=%v", first, err)
	}
	r.state.Events.Offset = int64(len(first.Data))
	r.state.Events.Sequence = 1
	last, err := r.next("events.jsonl", false)
	if err != nil || len(last.Data) != 7 || last.Final {
		t.Fatalf("last=%v err=%v", last, err)
	}
	r.state.Events.Offset += 7
	r.state.Events.Sequence++
	if chunk, err := r.next("events.jsonl", false); err != nil || chunk != nil {
		t.Fatal("live EOF was finalized")
	}
	final, err := r.next("events.jsonl", true)
	if err != nil || !final.Final || len(final.Data) != 0 || final.Sequence != 2 {
		t.Fatalf("final=%v err=%v", final, err)
	}
	r.state.Events.Offset++
	if _, err := r.next("events.jsonl", false); err == nil {
		t.Fatal("truncated artifact accepted")
	}
}

func TestReportingReplayRequiresValidFinalizedRecord(t *testing.T) {
	dir := t.TempDir()
	for _, data := range []string{"", `{}`, `{"protocolVersion":2,"sessionId":"other","endTime":"2026-09-14T12:00:00Z"}`} {
		if err := os.WriteFile(filepath.Join(dir, "session.json"), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := validateFinalizedSession(dir, "session"); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}

func TestReportingCancellationDuringDrainShortensTheTotalBudget(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "bounded-transport", "writes-reads-retries-and-finalization-are-bounded")
	graphCtx, cancelGraph := context.WithCancel(context.Background())
	defer cancelGraph()
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()
	r := &Run{ctx: workerCtx, graphCtx: graphCtx, cancel: cancelWorker, finish: make(chan struct{}), done: make(chan struct{}), clock: systemClock{}}
	go func() { <-workerCtx.Done(); r.err = errors.New("pending delivery"); close(r.done) }()
	result := make(chan error, 1)
	go func() { result <- r.Finish() }()
	<-r.finish
	cancelGraph()
	// Finish always returns, so the wait is unbounded and the verdict comes from
	// the elapsed time: a regression keeps the full FinalizationProgressDeadline,
	// which a wait of a few seconds could only tell apart on an idle host.
	started := time.Now()
	err := <-result
	if elapsed := time.Since(started); elapsed >= FinalizationProgressDeadline {
		t.Fatalf("cancellation during drain kept the normal %s progress deadline: Finish took %s", FinalizationProgressDeadline, elapsed)
	}
	if err == nil {
		t.Fatal("incomplete canceled drain reported success")
	}
}
