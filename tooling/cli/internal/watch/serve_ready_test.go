package watch

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// This file is the REPLACEMENT PROOF for serve readiness. Serve readiness used
// to be scraped out of the workload's log text, in one compatibility adapter
// isolated so there was a single greppable thing to delete. The proof has
// three parts, and each is a different way for the replacement to be
// incomplete:
//
//  1. the scraping is GONE and cannot come back (TestServeReadinessIsNeverLogScraped);
//  2. a "listening http://" log line no longer arms anything, and the typed
//     `ready` event does (the signal tests below);
//  3. readiness reaches the serve LOOP on the initial serve and on every restart
//     (TestServeLoop_ArmsWatcherOnTypedReadinessAcrossRestart).
//
// Part 1 exists because B6a's emission and B6b's consumption are separate slices
// on purpose: reverting the emission must not resurrect the probe.

// readyEventFor builds the RawJobEvent the CLI's reader produces for a typed
// readiness line. The payload is marshaled from the protocol's own type rather
// than hand-typed, so a payload rename cannot leave this proof passing against a
// shape nothing emits.
func readyEventFor(t *testing.T, data runtimeproto.ReadyData) jobs.RawJobEvent {
	t.Helper()

	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal readiness payload: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("decode readiness payload: %v", err)
	}
	return jobs.RawJobEvent{
		Version: runtimeproto.ProtocolVersion2,
		Type:    jobs.EventTypeReady,
		Time:    "2026-07-28T09:00:01.000Z",
		Data:    payload,
	}
}

// serverReadyEvent is the event a first-party serve wrapper emits: an addressable
// listener, the shape protocols/runtime fixtures/v2/valid/serve-restart.jsonl
// carries.
func serverReadyEvent(t *testing.T) jobs.RawJobEvent {
	t.Helper()
	return readyEventFor(t, runtimeproto.ReadyData{
		Target: runtimeproto.ReadyTargetServer,
		Endpoints: []runtimeproto.ReadyEndpoint{
			{Scheme: runtimeproto.ReadySchemeHTTP, Host: "localhost", Port: 3000},
		},
		DurationMs: 12,
	})
}

// listeningLogEvent is the human log line the probe used to arm on. The first-party
// frameworks still write it — it is what a developer reads — so it stays the
// sharpest available control for "the probe is really gone".
func listeningLogEvent() jobs.RawJobEvent {
	return jobs.RawJobEvent{
		Version: runtimeproto.ProtocolVersion2,
		Type:    jobs.EventTypeLog,
		Level:   "info",
		Message: "⚡️ listening http://localhost:3000",
		Data:    map[string]any{"message": "⚡️ listening http://localhost:3000"},
	}
}

// logScrapingSites reports the lines of src that decide something from the
// readiness log text. It looks for the substrings as GO STRING LITERALS, so the
// prose that explains what was deleted does not read as the deletion failing.
func logScrapingSites(name, src string) []string {
	var sites []string
	for i, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		if strings.Contains(line, `"listening http`) {
			sites = append(sites, name+":"+strconv.Itoa(i+1))
		}
	}
	return sites
}

// TestServeReadinessIsNeverLogScraped is the structural half of the proof: no
// production file in this package may decide readiness by matching the
// workload's log text. B6a's emission and this slice's consumption are separate
// commits precisely so that reverting the emission cannot bring the probe back —
// and a revert that did would land here, not in a serve session weeks later.
//
// The scanner is checked against a positive control first: a structural test
// that cannot see the thing it forbids passes for the wrong reason forever.
func TestServeReadinessIsNeverLogScraped(t *testing.T) {
	control := "func isReady(m string) bool { return strings.Contains(m, \"listening http://\") }"
	if got := logScrapingSites("control.go", control); len(got) != 1 {
		t.Fatalf("scanner found %v in the positive control, want exactly one site", got)
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read watch package dir: %v", err)
	}
	var sites []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		sites = append(sites, logScrapingSites(name, string(data))...)
	}
	if len(sites) != 0 {
		t.Fatalf("serve readiness is decided from log text at %v.\n"+
			"  The log-substring probe was replaced with the typed `ready` event "+
			"of runtime protocol v2; a log line is a human message, not a machine signal.", sites)
	}
}

func TestServeReadySignal_ClosesOnTypedReadyEvent(t *testing.T) {
	var signal serveReadySignal
	ready := signal.reset()

	signal.observe(serverReadyEvent(t))

	select {
	case <-ready:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("serve ready signal was not closed by a typed ready event")
	}
}

// A workload that binds nothing — a worker, a queue consumer — is still
// legitimately ready, and the protocol says so by making endpoints optional for
// the `workload` target. The watcher must not require an address it was never
// promised.
func TestServeReadySignal_ClosesOnWorkloadReadiness(t *testing.T) {
	var signal serveReadySignal
	ready := signal.reset()

	signal.observe(readyEventFor(t, runtimeproto.ReadyData{Target: runtimeproto.ReadyTargetWorkload}))

	select {
	case <-ready:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("a workload readiness claim must arm the watcher")
	}
}

// The deletion proof at the behavior level: the exact log line the probe armed
// on is now inert. This is the assertion that fails if the probe is ever wired
// back in beside the typed consumer.
func TestServeReadySignal_IgnoresTheListeningLogLine(t *testing.T) {
	var signal serveReadySignal
	ready := signal.reset()

	signal.observe(listeningLogEvent())

	select {
	case <-ready:
		t.Fatal("a log line closed the readiness signal; the substring probe is back")
	case <-time.After(30 * time.Millisecond):
	}
}

// A `ready` stamped v1 is a wire violation, not an early adopter: only v2 admits
// the type. Arming on it would trust a stream that has already contradicted its
// own envelope, so it fails closed.
func TestServeReadySignal_IgnoresReadyEventAtProtocolV1(t *testing.T) {
	var signal serveReadySignal
	ready := signal.reset()

	event := serverReadyEvent(t)
	event.Version = runtimeproto.ProtocolVersion
	signal.observe(event)

	select {
	case <-ready:
		t.Fatal("a v1 ready event closed the readiness signal")
	case <-time.After(30 * time.Millisecond):
	}
}

// A readiness claim a consumer cannot act on is not a readiness claim. Both
// rejections fail closed — an unarmed watcher costs a hot reload, while arming
// early restarts a server that has not bound its port.
func TestServeReadySignal_IgnoresUnactionableReadyPayloads(t *testing.T) {
	// A target the protocol does not define, stated through the protocol's own
	// payload type: the vocabulary is closed, so an extension inventing a member
	// of it is making a claim no consumer can act on.
	unknownTarget := readyEventFor(t, runtimeproto.ReadyData{Target: "database"})

	notAPayload := readyEventFor(t, runtimeproto.ReadyData{Target: runtimeproto.ReadyTargetServer})
	notAPayload.Data["target"] = 7

	cases := map[string]jobs.RawJobEvent{
		"no payload at all": {
			Version: runtimeproto.ProtocolVersion2,
			Type:    jobs.EventTypeReady,
		},
		"target outside the closed vocabulary":    unknownTarget,
		"payload that is not a readiness payload": notAPayload,
	}

	for name, event := range cases {
		t.Run(name, func(t *testing.T) {
			var signal serveReadySignal
			ready := signal.reset()

			signal.observe(event)

			select {
			case <-ready:
				t.Fatal("readiness signal closed on a payload the watcher cannot act on")
			case <-time.After(30 * time.Millisecond):
			}
		})
	}
}

// Restarts reuse the signal. A second ready event on the SAME signal must not
// close it twice (that panics), and each reset must hand out a fresh signal —
// otherwise the next serve iteration is considered ready before it starts.
func TestServeReadySignal_ResetArmsAFreshSignal(t *testing.T) {
	var signal serveReadySignal
	first := signal.reset()
	signal.observe(serverReadyEvent(t))
	signal.observe(serverReadyEvent(t))
	<-first

	second := signal.reset()
	select {
	case <-second:
		t.Fatal("a restart's readiness signal was already closed")
	case <-time.After(20 * time.Millisecond):
	}
	signal.observe(serverReadyEvent(t))
	select {
	case <-second:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("restart readiness signal was not closed")
	}
}

// Before any reset there is no signal to close; observing must not panic.
func TestServeReadySignal_ObserveBeforeResetIsANoop(t *testing.T) {
	var signal serveReadySignal
	signal.observe(serverReadyEvent(t))
}

// servingRunner is a scripted engine seam that behaves like a real serve job: it
// announces readiness on its iteration's event stream and then stays up until
// the loop cancels it.
type servingRunner struct {
	mu     sync.Mutex
	starts int
	event  jobs.RawJobEvent
}

func (r *servingRunner) run(ctx context.Context, _ []*workspace.Project, sink jobs.Renderer) IterationResult {
	r.mu.Lock()
	r.starts++
	r.mu.Unlock()

	sink.JobEvent(nil, r.event)
	<-ctx.Done()
	return IterationResult{Aborted: true}
}

func (r *servingRunner) started() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.starts
}

// freePort returns a port nothing is listening on, so the loop's between-restart
// port-release wait returns immediately instead of polling for two seconds.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return port
}

// TestServeLoop_ArmsWatcherOnTypedReadinessAcrossRestart is the loop half of the
// proof, and the one the epic's B6 dataset names: typed readiness across the
// initial serve AND a restart.
//
// The loop refuses to watch anything until the iteration reports ready — that is
// the whole point of the gate, because the watcher's own startup writes would
// otherwise restart the server it just started. So a restart happening at all
// proves the typed event was consumed on the initial serve; a second restart
// proves it was consumed again on the iteration that the first restart created,
// where a one-shot signal that forgot to re-arm would leave the watcher dead.
func TestServeLoop_ArmsWatcherOnTypedReadinessAcrossRestart(t *testing.T) {
	tmp := t.TempDir()
	appSrc := filepath.Join(tmp, "packages", "app", "src")
	if err := os.MkdirAll(appSrc, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	out := &syncBuffer{}
	runner := &servingRunner{event: serverReadyEvent(t)}
	s := &Session{
		cfg: SessionConfig{
			Workspace:     makeTestWorkspace(),
			Renderer:      &fakeRenderer{},
			RunIteration:  runner.run,
			CommandParams: map[string]any{"port": freePort(t)},
			ServeMode:     true,
		},
		watcher: NewWatcher(WatcherConfig{
			Roots:        []string{tmp},
			PollInterval: 5 * time.Millisecond,
			Debounce:     10 * time.Millisecond,
		}),
		watchRend: NewWatchRenderer(&fakeRenderer{}, out, true),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- s.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("serve loop did not stop within 3s of cancel")
		}
	}()

	// Two restarts, each driven by its own change. The write is retried under a
	// new filename because the loop re-snapshots when it arms: a file created
	// before that snapshot is part of the baseline, not a change.
	for restart := 1; restart <= 2; restart++ {
		// A hang detector, not a latency budget: a restart needs the poller,
		// the debounce and a fresh iteration to land, and a short bound let host
		// load fail the proof for an answer that was on its way.
		deadline := time.Now().Add(60 * time.Second)
		for attempt := 0; ; attempt++ {
			if strings.Count(out.String(), "restarting server") >= restart {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("restart %d never happened — the typed ready event did not arm the watcher.\n"+
					"iterations started: %d\noutput:\n%s", restart, runner.started(), out.String())
			}
			name := filepath.Join(appSrc, "change-"+strconv.Itoa(restart)+"-"+strconv.Itoa(attempt)+".ts")
			if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
				t.Fatalf("write file: %v", err)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}

	// Three iterations: the initial serve plus the two restarts it armed for.
	// "restarting server" is printed before the replacement iteration starts, so
	// the second restart's iteration can still be on its way when the loop above
	// exits. Wait for it; the bound is a hang detector, not a latency budget.
	deadline := time.Now().Add(60 * time.Second)
	for runner.started() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("serve iterations = %d, want 3 (initial serve + 2 restarts)\noutput:\n%s",
				runner.started(), out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
