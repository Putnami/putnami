package sessionreporter

import (
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
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/sessionstream"
)

// refuse answers every chunk of artifact ok:false, with retry or without, and
// acknowledges every other chunk.
func refuse(artifact string, retryable bool) planAnswer {
	code := map[bool]string{true: "receiver_unavailable", false: "invalid_chunk"}[retryable]
	return func(chunk protocolcli.SessionReportingChunk) *protocolcli.SessionReportingAck {
		ack := chunk.Ack()
		if chunk.Artifact == artifact {
			ack.OK, ack.Retryable, ack.Code = false, retryable, code
		}
		return &ack
	}
}

// misacknowledge acknowledges every chunk of artifact as the next chunk, and
// every other chunk as itself.
func misacknowledge(artifact string) planAnswer {
	return func(chunk protocolcli.SessionReportingChunk) *protocolcli.SessionReportingAck {
		ack := chunk.Ack()
		if chunk.Artifact == artifact {
			ack.Sequence++
		}
		return &ack
	}
}

// specVerificationOf is a spec-verification.json of exactly size bytes.
func specVerificationOf(size int) []byte {
	return bytes.Repeat([]byte("v"), size)
}

// writeSpecVerification writes record beside the session as the engine does,
// before Finish.
func writeSpecVerification(t *testing.T, dir string, record []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, specVerificationFile), record, 0o644); err != nil {
		t.Fatal(err)
	}
}

// sessionReporterEvidence is the delivery evidence subscribers.json in dir
// records for the session reporter.
func sessionReporterEvidence(t *testing.T, dir string) string {
	t.Helper()
	document, err := sessionstream.ReadEvidence(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range document.Subscribers {
		if entry.Name == SessionReporter.Name {
			return entry.Evidence
		}
	}
	t.Fatalf("no evidence recorded for %s", SessionReporter.Name)
	return ""
}

// TestSpecVerificationLeavesLastAfterTheEventsFinalMarker delivers a session
// whose spec-verification.json spans three chunks. Its frames leave after
// every other frame, and the first one leaves only once the checkpoint durably
// holds the acknowledged events final marker.
func TestSpecVerificationLeavesLastAfterTheEventsFinalMarker(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "spec-verification-record", "spec-verification-leaves-last-after-the-events-final-marker")
	record := specVerificationOf(2*protocolcli.SessionReportingChunkBytes + 7)
	h := newPlanHarness(t, planOf(10), []byte(`{"record":"task:start"}`))
	writeSpecVerification(t, h.dir, record)
	var mu sync.Mutex
	var atFirstChunk []byte
	h.serve(func(chunk protocolcli.SessionReportingChunk) *protocolcli.SessionReportingAck {
		if chunk.Artifact == specVerificationFile && chunk.Sequence == 0 {
			data, _ := os.ReadFile(filepath.Join(h.dir, StateFile))
			mu.Lock()
			atFirstChunk = data
			mu.Unlock()
		}
		return acknowledgeEveryChunk(chunk)
	})
	if err := h.finish(); err != nil {
		t.Fatalf("Finish = %v", err)
	}
	sent := h.sent()
	want := "plan.json,plan.json:final,session.json,session.json:final,events.jsonl,events.jsonl:final," +
		"spec-verification.json,spec-verification.json,spec-verification.json,spec-verification.json:final"
	if got := frameOrder(sent); got != want {
		t.Fatalf("frames = %s\nwant %s", got, want)
	}
	if got, final := reassemble(t, sent, specVerificationFile); !bytes.Equal(got, record) || !final {
		t.Fatalf("spec-verification.json arrived as %d bytes (final %v), want the %d persisted bytes", len(got), final, len(record))
	}
	mu.Lock()
	var before state
	err := json.Unmarshal(atFirstChunk, &before)
	mu.Unlock()
	if err != nil || !before.Events.Final || before.Pending == nil || before.Pending.Artifact != specVerificationFile || before.SpecVerification != (cursor{}) {
		t.Fatalf("checkpoint when the first spec-verification.json frame left = %+v (%v), want the events final marker acknowledged", before, err)
	}
	s := h.checkpoint()
	if !s.Complete || s.Pending != nil || s.SpecVerificationOmitted != "" || s.SpecVerification != (cursor{Offset: int64(len(record)), Sequence: 4, Final: true}) {
		t.Fatalf("checkpoint = %+v", s)
	}
	if got := sessionReporterEvidence(t, h.dir); got != protocolcli.SubscriberEvidenceDelivered {
		t.Fatalf("evidence %q, want %q", got, protocolcli.SubscriberEvidenceDelivered)
	}
}

// TestSpecVerificationIsReadUpToItsBound frames a spec-verification.json of
// exactly SessionReportingSpecVerificationBytes to its end, and refuses one
// byte more before its first chunk.
func TestSpecVerificationIsReadUpToItsBound(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "spec-verification-record", "spec-verification-is-bounded-and-omitted-past-its-bound")
	assertReadUpToItsBound(t, specVerificationFile, protocolcli.SessionReportingSpecVerificationBytes)
}

// TestAnOmittedSpecVerificationLeavesDeliveryComplete covers every omission
// a reporter that keeps answering causes: no spec-verification.json, one past
// its bound, one that is not a regular file, and a receiver that refuses a
// chunk without retry, with retry on every attempt, or acknowledges another
// chunk. The worker records the reason, sends no further frame of it, and
// Finish succeeds with the delivery evidence delivered.
func TestAnOmittedSpecVerificationLeavesDeliveryComplete(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "spec-verification-record", "an-omitted-spec-verification-leaves-delivery-complete")
	for _, tc := range []struct {
		name   string
		record []byte
		answer planAnswer
		reason string
		// frames is how many spec-verification.json frames reach the receiver.
		frames int
	}{
		{"absent", nil, acknowledgeEveryChunk, specVerificationAbsent, 0},
		{"past its bound", specVerificationOf(protocolcli.SessionReportingSpecVerificationBytes + 1), acknowledgeEveryChunk, specVerificationTooLarge, 0},
		{"not a regular file", nil, acknowledgeEveryChunk, specVerificationUnreadable, 0},
		{"refused", specVerificationOf(10), refuse(specVerificationFile, false), specVerificationRefused, 1},
		{"refused with retry on every attempt", specVerificationOf(10), refuse(specVerificationFile, true), specVerificationRefused, maxAttempts},
		{"acknowledged as another chunk", specVerificationOf(10), misacknowledge(specVerificationFile), specVerificationRefused, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.reason == specVerificationTooLarge {
				spectest.Proves(t, "cli/native-session-reporting", "spec-verification-record", "spec-verification-is-bounded-and-omitted-past-its-bound")
			}
			events := []byte(`{"record":"task:start"}`)
			h := newPlanHarness(t, nil, events)
			switch {
			case tc.reason == specVerificationUnreadable:
				if err := os.Mkdir(filepath.Join(h.dir, specVerificationFile), 0o755); err != nil {
					t.Fatal(err)
				}
			case tc.record != nil:
				writeSpecVerification(t, h.dir, tc.record)
			}
			h.serve(tc.answer)
			if err := h.finish(); err != nil {
				t.Fatalf("Finish = %v, want an omitted spec-verification.json to leave delivery complete", err)
			}
			sent := h.sent()
			delivered := "session.json,session.json:final,events.jsonl,events.jsonl:final"
			if len(sent) != 4+tc.frames || frameOrder(sent[:4]) != delivered {
				t.Fatalf("frames = %s, want %s then %d spec-verification.json frames", frameOrder(sent), delivered, tc.frames)
			}
			first := protocolcli.NewSessionReportingChunk(batchSessionID, specVerificationFile, 0, 0, tc.record, false)
			for _, chunk := range sent[4:] {
				if !sameFrame(t, chunk, first) {
					t.Fatalf("spec-verification.json frame %+v, want only its first frame", chunk)
				}
			}
			s := h.checkpoint()
			if s.SpecVerificationOmitted != tc.reason || !s.Complete || s.Pending != nil || s.Error != "" || s.SpecVerification != (cursor{}) {
				t.Fatalf("checkpoint = %+v, want spec-verification.json omitted as %q", s, tc.reason)
			}
			if h.run.state != s {
				t.Fatalf("in-memory state %+v differs from the durable checkpoint %+v", h.run.state, s)
			}
			if got := sessionReporterEvidence(t, h.dir); got != protocolcli.SubscriberEvidenceDelivered {
				t.Fatalf("evidence %q, want %q", got, protocolcli.SubscriberEvidenceDelivered)
			}
		})
	}
}

// TestAReporterThatQuitsAfterTheEventsFinalMarkerIsNotRestarted runs a real
// reporter process that exits once it acknowledged the events final marker,
// as one that does not receive spec-verification.json may. The worker omits
// the record as quit, starts no second process for it, and Finish succeeds.
func TestAReporterThatQuitsAfterTheEventsFinalMarkerIsNotRestarted(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "spec-verification-record", "an-omitted-spec-verification-leaves-delivery-complete")
	defer runcredential.SetForTest("")()
	fx := newPlanReporterFixture(t, "")
	dir := t.TempDir()
	log, err := sessionstream.Create(dir, holderSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append([]byte(`{"record":"task:start"}`)); err != nil {
		t.Fatal(err)
	}
	ctx := WithEventsBatchInterval(Capture(context.Background()), 10*time.Millisecond)
	run, err := Start(ctx, SessionReporter, log, holderSessionID, fx.resolve)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionFile), []byte(`{"protocolVersion":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSpecVerification(t, dir, specVerificationOf(100))
	if err := run.Finish(); err != nil {
		t.Fatalf("Finish = %v, want delivery to complete without spec-verification.json", err)
	}
	processes := fx.processes(t)
	if len(processes) != 1 {
		t.Fatalf("reporter processes = %d, want the one that quit and no restart", len(processes))
	}
	if got := frameOrder(processes[0]); !strings.HasSuffix(got, "events.jsonl:final") || strings.Contains(got, specVerificationFile) {
		t.Fatalf("the reporter read %s", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, StateFile))
	if err != nil {
		t.Fatal(err)
	}
	var s state
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	if s.SpecVerificationOmitted != specVerificationQuit || !s.Complete || s.Pending != nil || s.Error != "" {
		t.Fatalf("checkpoint = %+v, want spec-verification.json omitted as quit", s)
	}
	if got := sessionReporterEvidence(t, dir); got != protocolcli.SubscriberEvidenceDelivered {
		t.Fatalf("evidence %q, want %q", got, protocolcli.SubscriberEvidenceDelivered)
	}
}

// TestSpecVerificationInFlightAtTheLimitsIsOmittedAsLate holds the
// acknowledgement of the first spec-verification.json chunk: at the progress
// deadline the worker omits the record as late, and Finish succeeds, because
// every other artifact was delivered.
func TestSpecVerificationInFlightAtTheLimitsIsOmittedAsLate(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "spec-verification-record", "an-omitted-spec-verification-leaves-delivery-complete")
	h := newFinalizationHarness(t, SessionReporter, 1)
	writeSpecVerification(t, h.run.dir, specVerificationOf(10))
	h.finish()
	deadline := time.After(time.Minute)
	for held := false; !held; {
		select {
		case chunk := <-h.received:
			if held = chunk.Artifact == specVerificationFile; !held {
				h.release <- struct{}{}
			}
		case <-deadline:
			t.Fatal("no spec-verification.json frame reached the reporter")
		}
	}
	if stopped, err := h.advance(FinalizationProgressDeadline - time.Second); stopped {
		t.Fatalf("stopped before the progress deadline: %v", err)
	}
	stopped, err := h.advance(time.Second)
	if !stopped || err != nil {
		t.Fatalf("Finish = %v (stopped %v), want the late record omitted and delivery complete", err, stopped)
	}
	s := h.run.state
	if s.SpecVerificationOmitted != specVerificationLate || !s.Complete || s.Pending != nil || s.Error != "" {
		t.Fatalf("checkpoint = %+v, want spec-verification.json omitted as late", s)
	}
	if got := h.evidence().Evidence; got != protocolcli.SubscriberEvidenceDelivered {
		t.Fatalf("evidence %q, want %q", got, protocolcli.SubscriberEvidenceDelivered)
	}
}

// TestACheckpointWithoutASpecVerificationCursorStaysValid opens a completed
// checkpoint that a CLI that does not send the record wrote: it stays
// complete, the record omitted as late. A checkpoint that acknowledged the
// events final marker but not the record, as a worker that stopped between
// them leaves, sends the record on replay.
func TestACheckpointWithoutASpecVerificationCursorStaysValid(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "spec-verification-record", "a-checkpoint-without-a-spec-verification-cursor-stays-valid")
	dir := t.TempDir()
	completed := `{"version":1,"provider":"@test/provider","sessionId":"session","events":{"offset":3,"sequence":2,"final":true},"session":{"offset":2,"sequence":2,"final":true},"plan":{"offset":4,"sequence":2,"final":true},"complete":true}`
	if err := os.WriteFile(filepath.Join(dir, StateFile), []byte(completed), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := openRun(context.Background(), SessionReporter, dir, "session", "@test/provider")
	if err != nil {
		t.Fatalf("a completed checkpoint without a spec-verification cursor was refused: %v", err)
	}
	r.release()
	if r.state.SpecVerificationOmitted != specVerificationLate || !r.state.Complete || r.state.SpecVerification != (cursor{}) {
		t.Fatalf("opened state = %+v, want spec-verification.json omitted as late and the checkpoint complete", r.state)
	}
	data, err := os.ReadFile(filepath.Join(dir, StateFile))
	if err != nil {
		t.Fatal(err)
	}
	if want, err := json.Marshal(r.state); err != nil || !bytes.Equal(data, want) {
		t.Fatalf("saved checkpoint %s, want %s: %v", data, want, err)
	}

	dir = t.TempDir()
	events := []byte(`{"record":"task:start"}`)
	log, err := sessionstream.Create(dir, batchSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(events); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionFile), []byte(`{"protocolVersion":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	record := specVerificationOf(10)
	writeSpecVerification(t, dir, record)
	stopped, err := json.Marshal(state{
		Version: 1, Provider: "@test/provider", SessionID: batchSessionID,
		Events: cursor{Offset: int64(len(events)) + 1, Sequence: 2, Final: true}, Session: cursor{Offset: 21, Sequence: 2, Final: true},
		PlanOmitted: planAbsent,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, StateFile), stopped, 0o600); err != nil {
		t.Fatal(err)
	}
	sent, s := resumePlan(t, dir)
	if got := frameOrder(sent); got != "spec-verification.json,spec-verification.json:final" {
		t.Fatalf("replay frames = %s", got)
	}
	if got, final := reassemble(t, sent, specVerificationFile); !bytes.Equal(got, record) || !final {
		t.Fatalf("spec-verification.json arrived as %q (final %v)", got, final)
	}
	if !s.Complete || s.Pending != nil || s.SpecVerificationOmitted != "" || s.SpecVerification != (cursor{Offset: int64(len(record)), Sequence: 2, Final: true}) {
		t.Fatalf("replayed checkpoint = %+v", s)
	}
}

// TestReportingCursorRejectsInconsistentSpecVerificationState refuses every
// spec-verification.json state the worker never writes, and accepts the ones
// it does.
func TestReportingCursorRejectsInconsistentSpecVerificationState(t *testing.T) {
	pending := protocolcli.NewSessionReportingChunk("session", specVerificationFile, 0, 0, []byte("{}"), false)
	shifted := protocolcli.NewSessionReportingChunk("session", specVerificationFile, 1, 0, []byte("{}"), false)
	open := state{Version: 1, SessionID: "session", Provider: "@test/provider", Events: cursor{3, 2, true}, Session: cursor{2, 2, true}, PlanOmitted: planAbsent}
	for name, mutate := range map[string]func(*state){
		"open after the events final marker": func(*state) {},
		"first frame pending":                func(s *state) { s.Pending = &pending },
		"delivered":                          func(s *state) { s.SpecVerification, s.Complete = cursor{2, 2, true}, true },
		"omitted":                            func(s *state) { s.SpecVerificationOmitted, s.Complete = specVerificationQuit, true },
	} {
		r := &Run{capability: SessionReporter, state: open}
		mutate(&r.state)
		if err := r.validate(); err != nil {
			t.Errorf("%s: a state the worker writes was refused: %v", name, err)
		}
	}
	for name, mutate := range map[string]func(*state){
		"negative offset":   func(s *state) { s.SpecVerification.Offset = -1 },
		"negative sequence": func(s *state) { s.SpecVerification.Sequence = -1 },
		"unknown omission":  func(s *state) { s.SpecVerificationOmitted, s.Complete = "forgotten", true },
		"omitted after its final marker": func(s *state) {
			s.SpecVerification, s.SpecVerificationOmitted, s.Complete = cursor{2, 2, true}, specVerificationRefused, true
		},
		"cursor before the events final marker":   func(s *state) { s.Events.Final, s.SpecVerification = false, cursor{2, 1, false} },
		"omission before the events final marker": func(s *state) { s.Events.Final, s.SpecVerificationOmitted = false, specVerificationAbsent },
		"pending frame before the events final":   func(s *state) { s.Events.Final, s.Pending = false, &pending },
		"pending frame once omitted": func(s *state) {
			s.SpecVerificationOmitted, s.Pending, s.Complete = specVerificationRefused, &pending, true
		},
		"pending frame once closed":             func(s *state) { s.SpecVerification, s.Pending, s.Complete = cursor{2, 1, true}, &pending, true },
		"pending frame off cursor":              func(s *state) { s.Pending = &shifted },
		"complete with an open record":          func(s *state) { s.Complete = true },
		"incomplete with every artifact closed": func(s *state) { s.SpecVerificationOmitted = specVerificationAbsent },
	} {
		r := &Run{capability: SessionReporter, state: open}
		mutate(&r.state)
		if err := r.validate(); err == nil {
			t.Errorf("%s: invalid state accepted: %+v", name, r.state)
		}
	}
	logValid := state{Version: 1, SessionID: "session", Provider: "@test/provider", Events: cursor{3, 2, true}, Complete: true}
	for name, mutate := range map[string]func(*state){
		"cursor":        func(s *state) { s.SpecVerification.Offset = 1 },
		"final":         func(s *state) { s.SpecVerification.Final = true },
		"omission":      func(s *state) { s.SpecVerificationOmitted = specVerificationAbsent },
		"pending frame": func(s *state) { s.Complete, s.Pending = false, &pending },
	} {
		r := &Run{capability: LogReporter, state: logValid}
		mutate(&r.state)
		if err := r.validate(); err == nil {
			t.Errorf("log reporter %s: invalid state accepted: %+v", name, r.state)
		}
	}
}
