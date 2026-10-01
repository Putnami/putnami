package sessionreporter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/sessionstream"
)

// fakeClock is a manual clock for the delivery worker. The worker calls After
// exactly once each time it blocks to batch, so the most recent timer is the one
// its select is waiting on, and each call reports whether a stream wake-up was
// already queued at that moment.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	timer chan time.Time
	at    time.Time
	wake  <-chan struct{}
	armed chan armedWait
}

type armedWait struct {
	wait        time.Duration
	wakePending bool
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(wait time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.timer, c.at = make(chan time.Time, 1), c.now.Add(wait)
	timer := c.timer
	c.mu.Unlock()
	c.armed <- armedWait{wait: wait, wakePending: len(c.wake) > 0}
	return timer
}

// Advance moves the clock and reports whether the worker's live timer fired.
func (c *fakeClock) Advance(d time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	if c.timer == nil || c.at.After(c.now) {
		return false
	}
	c.timer <- c.now
	c.timer = nil
	return true
}

// batchHarness runs the real delivery loop against a real session stream, a
// manual clock, and an in-memory provider that acknowledges every chunk.
type batchHarness struct {
	t      *testing.T
	log    *sessionstream.Log
	run    *Run
	clock  *fakeClock
	done   chan error
	mu     sync.Mutex
	chunks []protocolcli.SessionReportingChunk
}

const batchSessionID = "20260917-120000-abc123"

func newBatchHarness(t *testing.T) *batchHarness {
	t.Helper()
	return newCapabilityHarness(t, SessionReporter)
}

// newCapabilityHarness runs one capability's delivery loop, with that
// capability's batch interval and artifacts.
func newCapabilityHarness(t *testing.T, capability Capability) *batchHarness {
	t.Helper()
	dir := t.TempDir()
	log, err := sessionstream.Create(dir, batchSessionID)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := log.Subscribe(capability.Name, 0)
	if err != nil {
		t.Fatal(err)
	}
	p, remote := pipeProcess()
	ctx, cancel := context.WithCancel(context.Background())
	h := &batchHarness{t: t, log: log, done: make(chan error, 1)}
	h.clock = &fakeClock{now: time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC), wake: sub.Wake(), armed: make(chan armedWait, 64)}
	h.run = &Run{
		capability: capability,
		dir:        dir, events: sub, process: p, ctx: ctx, graphCtx: context.Background(), cancel: cancel,
		finish: make(chan struct{}), done: make(chan struct{}), opTimeout: time.Minute,
		clock: h.clock, batchInterval: eventsBatchInterval(context.Background(), capability),
		state: state{Version: 1, Provider: "@test/provider", SessionID: batchSessionID},
	}
	go func() {
		scanner := bufio.NewScanner(remote)
		scanner.Buffer(make([]byte, 4096), protocolcli.SessionReportingLineBytes)
		for scanner.Scan() {
			chunk, err := protocolcli.ParseSessionReportingChunk(scanner.Bytes())
			if err != nil {
				return
			}
			h.mu.Lock()
			h.chunks = append(h.chunks, *chunk)
			h.mu.Unlock()
			_ = json.NewEncoder(remote).Encode(chunk.Ack())
		}
	}()
	go func() { h.done <- h.run.deliver() }()
	t.Cleanup(func() {
		cancel()
		_ = remote.Close()
		_ = log.Close()
		_ = sub.Close()
	})
	h.settle()
	return h
}

// settle returns once the worker is blocked batching with no wake-up queued:
// every chunk it decided to send has been acknowledged.
func (h *batchHarness) settle() time.Duration {
	h.t.Helper()
	guard := time.NewTimer(time.Minute)
	defer guard.Stop()
	for {
		select {
		case armed := <-h.clock.armed:
			if !armed.wakePending {
				return armed.wait
			}
		case err := <-h.done:
			h.t.Fatalf("delivery stopped while batching: %v", err)
		case <-guard.C:
			h.t.Fatal("delivery worker never settled")
		}
	}
}

func (h *batchHarness) append(record string) {
	h.t.Helper()
	if err := h.log.Append([]byte(record)); err != nil {
		h.t.Fatal(err)
	}
}

func (h *batchHarness) sent() []protocolcli.SessionReportingChunk {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]protocolcli.SessionReportingChunk(nil), h.chunks...)
}

func TestEventsBatchingHoldsATrickleForTheInterval(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "bounded-transport", "live-chunks-wait-for-a-full-frame-or-the-interval")
	h := newBatchHarness(t)
	h.append(`{"record":"task:start"}`)
	if wait := h.settle(); wait != EventsBatchInterval {
		t.Fatalf("a trickle at the worker's start waits %s, want the whole %s interval", wait, EventsBatchInterval)
	}
	if h.clock.Advance(EventsBatchInterval - time.Second) {
		t.Fatal("the batch timer fired before the interval ended")
	}
	h.append(`{"record":"task:event"}`)
	if wait := h.settle(); wait != time.Second {
		t.Fatalf("a second record re-armed the wait for %s, want the remaining 1s", wait)
	}
	if got := h.sent(); len(got) != 0 || h.run.state.Pending != nil {
		t.Fatalf("a trickle left before the interval: %d chunks, pending=%v", len(got), h.run.state.Pending)
	}
	if !h.clock.Advance(time.Second) {
		t.Fatal("the batch timer did not fire at the end of the interval")
	}
	h.settle()
	got := h.sent()
	want := "{\"record\":\"task:start\"}\n{\"record\":\"task:event\"}\n"
	if len(got) != 1 || string(got[0].Data) != want || got[0].Offset != 0 || got[0].Sequence != 0 || got[0].Final {
		t.Fatalf("after the interval: %+v, want one chunk with both records", got)
	}
	if h.run.state.Events.Offset != int64(len(want)) || h.run.state.Pending != nil {
		t.Fatalf("checkpoint after the batch = %+v", h.run.state)
	}
}

func TestEventsBatchingSendsAFullFrameAtOnce(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "bounded-transport", "live-chunks-wait-for-a-full-frame-or-the-interval")
	h := newBatchHarness(t)
	h.append(strings.Repeat("x", protocolcli.SessionReportingChunkBytes))
	wait := h.settle()
	got := h.sent()
	if len(got) != 1 || len(got[0].Data) != protocolcli.SessionReportingChunkBytes {
		t.Fatalf("a committed full frame was held: %d chunks", len(got))
	}
	if wait != EventsBatchInterval {
		t.Fatalf("the 1-byte remainder waits %s, want a fresh %s interval from the frame's send", wait, EventsBatchInterval)
	}
}

func TestEventsBatchingDrainsAtOnceWhenTerminal(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "bounded-transport", "live-chunks-wait-for-a-full-frame-or-the-interval")
	h := newBatchHarness(t)
	h.append(`{"record":"task:end"}`)
	h.append(`{"record":"session:end"}`)
	h.settle()
	if err := h.log.Close(); err != nil {
		t.Fatal(err)
	}
	h.settle()
	session := []byte(`{"protocolVersion":2}`)
	if err := os.WriteFile(filepath.Join(h.run.dir, "session.json"), session, 0o644); err != nil {
		t.Fatal(err)
	}
	close(h.run.finish)
	if err := <-h.done; err != nil {
		t.Fatalf("terminal drain: %v", err)
	}
	got := h.sent()
	order := make([]string, 0, len(got))
	for _, chunk := range got {
		order = append(order, chunk.Artifact+map[bool]string{true: ":final", false: ""}[chunk.Final])
	}
	if strings.Join(order, ",") != "session.json,session.json:final,events.jsonl,events.jsonl:final" ||
		!bytes.Equal(got[0].Data, session) || string(got[2].Data) != "{\"record\":\"task:end\"}\n{\"record\":\"session:end\"}\n" {
		t.Fatalf("terminal drain sent %v", order)
	}
	if !h.run.state.Complete {
		t.Fatal("terminal drain left reporting incomplete")
	}
}

// TestEventsBatchingBoundsChunksByDuration is the production case: a steady
// trickle far below one frame per interval sends one chunk per interval, not one
// per record or per acknowledgement round trip.
func TestEventsBatchingBoundsChunksByDuration(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "bounded-transport", "live-chunks-wait-for-a-full-frame-or-the-interval")
	const seconds = 600
	h := newBatchHarness(t)
	var stream bytes.Buffer
	for second := 0; second < seconds; second++ {
		record := `{"record":"task:event","second":` + strings.Repeat("0", 180) + `}`
		h.append(record)
		stream.WriteString(record + "\n")
		h.settle()
		if h.clock.Advance(time.Second) {
			h.settle()
		}
	}
	got := h.sent()
	if want := seconds / int(EventsBatchInterval/time.Second); len(got) != want {
		t.Fatalf("%ds of steady trickle sent %d chunks, want %d", seconds, len(got), want)
	}
	var delivered bytes.Buffer
	for i, chunk := range got {
		if chunk.Sequence != int64(i) || chunk.Offset != int64(delivered.Len()) || bytes.Count(chunk.Data, []byte("\n")) != 10 {
			t.Fatalf("chunk %d = offset %d sequence %d with %d records", i, chunk.Offset, chunk.Sequence, bytes.Count(chunk.Data, []byte("\n")))
		}
		delivered.Write(chunk.Data)
	}
	if !bytes.Equal(delivered.Bytes(), stream.Bytes()) {
		t.Fatal("batched chunks do not reassemble the stream")
	}
}

// TestOnlyTestsShortenTheEventsBatchInterval pins the production interval and
// keeps its test seam out of every production file of the module.
func TestOnlyTestsShortenTheEventsBatchInterval(t *testing.T) {
	if EventsBatchInterval != 10*time.Second || LogEventsBatchInterval != 2*time.Second {
		t.Fatalf("EventsBatchInterval = %s, LogEventsBatchInterval = %s, want 10s and 2s", EventsBatchInterval, LogEventsBatchInterval)
	}
	for capability, want := range map[string]time.Duration{SessionReporter.Name: EventsBatchInterval, LogReporter.Name: LogEventsBatchInterval} {
		for _, c := range Capabilities() {
			if c.Name != capability {
				continue
			}
			if got := eventsBatchInterval(context.Background(), c); got != want {
				t.Fatalf("an unconfigured %s batches every %s, want %s", capability, got, want)
			}
			if got := eventsBatchInterval(WithEventsBatchInterval(context.Background(), time.Millisecond), c); got != time.Millisecond {
				t.Fatalf("the test seam was ignored for %s: %s", capability, got)
			}
		}
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	var callers []string
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if name := info.Name(); name == "testdata" || name == ".putnami" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, "WithEventsBatchInterval(") && !strings.HasPrefix(line, "func WithEventsBatchInterval(") && !strings.HasPrefix(strings.TrimSpace(line), "//") {
				callers = append(callers, path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(callers) != 0 {
		t.Fatalf("production code shortens the events batch interval: %v", callers)
	}
}
