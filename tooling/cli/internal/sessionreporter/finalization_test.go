package sessionreporter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/sessionstream"
)

// finalizationHarness drives Finish over the real delivery worker of one
// capability, with a manual clock and an in-memory provider that acknowledges
// a chunk only when the test releases it. Only drain arms the clock: the
// worker starts after Finish enabled terminal transmission, so it never
// batches.
type finalizationHarness struct {
	t        *testing.T
	run      *Run
	clock    *fakeClock
	started  time.Time
	received chan protocolcli.SessionReportingChunk
	release  chan struct{}
	result   chan error
}

// newFinalizationHarness retains a finalized session whose event stream holds
// frames full chunks, beside a finalized session.json.
func newFinalizationHarness(t *testing.T, capability Capability, frames int) *finalizationHarness {
	t.Helper()
	dir := t.TempDir()
	log, err := sessionstream.Create(dir, batchSessionID)
	if err != nil {
		t.Fatal(err)
	}
	for range frames {
		// Each record and its LF fill exactly one chunk.
		if err := log.Append(bytes.Repeat([]byte("x"), protocolcli.SessionReportingChunkBytes-1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionFile), []byte(`{"protocolVersion":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sub, err := log.Subscribe(capability.Name, 0)
	if err != nil {
		t.Fatal(err)
	}
	p, remote := pipeProcess()
	// The pipe has no process to wait for: closing it, as the worker does when
	// a canceled call fails and when it stops, returns at once.
	p.done = make(chan struct{})
	close(p.done)
	ctx, cancel := context.WithCancel(context.Background())
	h := &finalizationHarness{
		t: t, received: make(chan protocolcli.SessionReportingChunk), release: make(chan struct{}), result: make(chan error, 1),
		clock: &fakeClock{now: time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC), armed: make(chan armedWait, 64)},
	}
	h.started = h.clock.Now()
	h.run = &Run{
		capability: capability,
		dir:        dir, events: sub, process: p, ctx: ctx, graphCtx: context.Background(), cancel: cancel,
		finish: make(chan struct{}), done: make(chan struct{}), opTimeout: time.Hour,
		clock: h.clock, batchInterval: capability.BatchInterval,
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
			h.received <- *chunk
			if _, ok := <-h.release; !ok {
				return
			}
			if err := json.NewEncoder(remote).Encode(chunk.Ack()); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		close(h.release)
		_ = remote.Close()
		_ = sub.Close()
	})
	return h
}

// finish calls Finish, waits for drain to arm its first deadline, then starts
// the worker, which therefore sees terminal transmission enabled at once.
func (h *finalizationHarness) finish() time.Duration {
	h.t.Helper()
	go func() { h.result <- h.run.Finish() }()
	select {
	case armed := <-h.clock.armed:
		go h.run.work()
		return armed.wait
	case <-time.After(time.Minute):
		h.t.Fatal("drain never armed its deadline")
		return 0
	}
}

// advance moves the clock by d. When drain's deadline passes, it waits until
// drain armed its next one, or until Finish returned, which it reports.
func (h *finalizationHarness) advance(d time.Duration) (bool, error) {
	h.t.Helper()
	if !h.clock.Advance(d) {
		return false, nil
	}
	select {
	case <-h.clock.armed:
		return false, nil
	case err := <-h.result:
		return true, err
	case <-time.After(time.Minute):
		h.t.Fatal("drain neither armed its next deadline nor returned")
		return true, nil
	}
}

// acknowledgeEvery acknowledges each chunk step after it reached the
// provider, until Finish returns. It returns what reached the provider and
// what Finish returned. The chunk in flight when Finish returns is the last
// one and was never acknowledged.
func (h *finalizationHarness) acknowledgeEvery(step time.Duration) ([]protocolcli.SessionReportingChunk, error) {
	h.t.Helper()
	var sent []protocolcli.SessionReportingChunk
	for {
		select {
		case chunk := <-h.received:
			sent = append(sent, chunk)
			if stopped, err := h.advance(step); stopped {
				return sent, err
			}
			h.release <- struct{}{}
		case err := <-h.result:
			return sent, err
		case <-time.After(time.Minute):
			h.t.Fatal("the worker neither sent a chunk nor finished")
		}
	}
}

func (h *finalizationHarness) elapsed() time.Duration { return h.clock.Now().Sub(h.started) }

func (h *finalizationHarness) evidence() protocolcli.SessionSubscriberEvidence {
	h.t.Helper()
	document, err := sessionstream.ReadEvidence(h.run.dir)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, entry := range document.Subscribers {
		if entry.Name == h.run.capability.Name {
			return entry
		}
	}
	h.t.Fatalf("no evidence recorded for %s", h.run.capability.Name)
	return protocolcli.SessionSubscriberEvidence{}
}

// chunksOf is how many chunks the capability sends for frames full event
// chunks: each artifact's data, then its final marker.
func chunksOf(capability Capability, frames int) int {
	if capability.sends(sessionFile) {
		return frames + 3
	}
	return frames + 1
}

// TestFinalizationKeepsAReporterThatKeepsAcknowledging acknowledges a chunk
// every 20 s for minutes: each acknowledgement, of either artifact, moves the
// progress deadline, so the reporter delivers every artifact and its final
// marker, far past 30 s.
func TestFinalizationKeepsAReporterThatKeepsAcknowledging(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "bounded-transport", "writes-reads-retries-and-finalization-are-bounded")
	const frames, step = 8, 20 * time.Second
	for _, capability := range Capabilities() {
		t.Run(capability.Name, func(t *testing.T) {
			h := newFinalizationHarness(t, capability, frames)
			if wait := h.finish(); wait != FinalizationProgressDeadline {
				t.Fatalf("finalization first waits %s, want the %s progress deadline", wait, FinalizationProgressDeadline)
			}
			sent, err := h.acknowledgeEvery(step)
			if err != nil {
				t.Fatalf("a reporter acknowledging every %s was stopped after %s: %v", step, h.elapsed(), err)
			}
			if want := chunksOf(capability, frames); len(sent) != want {
				t.Fatalf("delivered %d chunks, want %d", len(sent), want)
			}
			if h.elapsed() <= FinalizationProgressDeadline {
				t.Fatalf("finalization took %s, which does not exercise the %s deadline", h.elapsed(), FinalizationProgressDeadline)
			}
			last := sent[len(sent)-1]
			if last.Artifact != sessionstream.EventsFile || !last.Final || !h.run.state.Complete || h.run.state.Pending != nil {
				t.Fatalf("last chunk %s final=%v, checkpoint %+v", last.Artifact, last.Final, h.run.state)
			}
			if got := h.evidence().Evidence; got != protocolcli.SubscriberEvidenceDelivered {
				t.Fatalf("evidence %q, want %q", got, protocolcli.SubscriberEvidenceDelivered)
			}
		})
	}
}

// TestFinalizationStopsAReporterWithoutProgress acknowledges the first chunk
// 20 s into finalization, then nothing: the deadline moves to 30 s after that
// acknowledgement, not 30 s after finalization began, and the diagnostic names
// the missing progress. The exact pending frame stays for replay.
func TestFinalizationStopsAReporterWithoutProgress(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "bounded-transport", "writes-reads-retries-and-finalization-are-bounded")
	for _, capability := range Capabilities() {
		t.Run(capability.Name, func(t *testing.T) {
			h := newFinalizationHarness(t, capability, 2)
			h.finish()
			<-h.received
			if stopped, err := h.advance(20 * time.Second); stopped {
				t.Fatalf("stopped before the first acknowledgement: %v", err)
			}
			h.release <- struct{}{}
			held := <-h.received
			if stopped, err := h.advance(FinalizationProgressDeadline - time.Second); stopped {
				t.Fatalf("stopped %s after finalization began, %s after an acknowledgement: %v", h.elapsed(), FinalizationProgressDeadline-time.Second, err)
			}
			stopped, err := h.advance(time.Second)
			if !stopped {
				t.Fatalf("still delivering %s after the last acknowledgement", FinalizationProgressDeadline)
			}
			if want := "reporting budget exhausted: no chunk acknowledged for 30s"; err == nil || err.Error() != want {
				t.Fatalf("diagnostic %q, want %q", err, want)
			}
			if !errors.Is(err, errBudgetExhausted) {
				t.Fatalf("the diagnostic lost the budget error: %v", err)
			}
			if h.elapsed() != 20*time.Second+FinalizationProgressDeadline {
				t.Fatalf("stopped %s after finalization began, want 50s", h.elapsed())
			}
			pending := h.run.state.Pending
			if pending == nil || !pending.Ack().Matches(held) || h.run.state.Error != "delivery_incomplete" || h.run.state.Complete {
				t.Fatalf("the unacknowledged frame was not retained: %+v", h.run.state)
			}
			if got := h.evidence().Evidence; got == protocolcli.SubscriberEvidenceDelivered {
				t.Fatalf("a stopped reporter recorded %q", got)
			}
		})
	}
}

// TestFinalizationCapsAReporterThatKeepsAcknowledging acknowledges a chunk
// every 20 s with more chunks than five minutes allow: finalization stops at
// exactly the cap, and the diagnostic names the cap, not missing progress.
func TestFinalizationCapsAReporterThatKeepsAcknowledging(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "bounded-transport", "writes-reads-retries-and-finalization-are-bounded")
	const frames, step = 20, 20 * time.Second
	for _, capability := range Capabilities() {
		t.Run(capability.Name, func(t *testing.T) {
			h := newFinalizationHarness(t, capability, frames)
			h.finish()
			sent, err := h.acknowledgeEvery(step)
			if want := "reporting budget exhausted: finalization reached its 5m0s cap"; err == nil || err.Error() != want {
				t.Fatalf("diagnostic %q after %s, want %q", err, h.elapsed(), want)
			}
			if h.elapsed() != FinalizationCap {
				t.Fatalf("stopped %s after finalization began, want the %s cap", h.elapsed(), FinalizationCap)
			}
			if acknowledged := len(sent) - 1; acknowledged != int(FinalizationCap/step)-1 || len(sent) >= chunksOf(capability, frames) {
				t.Fatalf("acknowledged %d of %d chunks before the cap", acknowledged, chunksOf(capability, frames))
			}
			if h.run.state.Pending == nil || h.run.state.Complete {
				t.Fatalf("the capped delivery left checkpoint %+v", h.run.state)
			}
		})
	}
}

// TestFinalizationLimits pins the limits the diagnostics and documents name.
func TestFinalizationLimits(t *testing.T) {
	if FinalizationProgressDeadline != 30*time.Second || FinalizationCap != 5*time.Minute || CanceledFinalizationBudget != 2*time.Second {
		t.Fatalf("limits = %s, %s, %s; want 30s, 5m, 2s", FinalizationProgressDeadline, FinalizationCap, CanceledFinalizationBudget)
	}
}
