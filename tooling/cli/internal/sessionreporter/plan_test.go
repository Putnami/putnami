package sessionreporter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/sessionstream"
)

// planAnswer is how an in-memory receiver answers one chunk.
type planAnswer func(chunk protocolcli.SessionReportingChunk) *protocolcli.SessionReportingAck

func acknowledgeEveryChunk(chunk protocolcli.SessionReportingChunk) *protocolcli.SessionReportingAck {
	ack := chunk.Ack()
	return &ack
}

// refusePlan answers every plan.json chunk ok:false, with retry or without,
// and acknowledges every other chunk.
func refusePlan(retryable bool) planAnswer {
	code := map[bool]string{true: "receiver_unavailable", false: "invalid_chunk"}[retryable]
	return func(chunk protocolcli.SessionReportingChunk) *protocolcli.SessionReportingAck {
		ack := chunk.Ack()
		if chunk.Artifact == planFile {
			ack.OK, ack.Retryable, ack.Code = false, retryable, code
		}
		return &ack
	}
}

// planHarness runs the session reporter's delivery worker over a session whose
// plan.json and first events exist before the worker starts, against an
// in-memory receiver.
type planHarness struct {
	t      *testing.T
	dir    string
	log    *sessionstream.Log
	run    *Run
	mu     sync.Mutex
	chunks []protocolcli.SessionReportingChunk
	seen   chan struct{}
}

// newPlanHarness records events (each written as one record) and writes plan,
// when not nil, beside the stream. The live batch interval is an hour, so an
// events chunk leaves while the graph runs only once a full frame is
// committed.
func newPlanHarness(t *testing.T, plan []byte, events ...[]byte) *planHarness {
	t.Helper()
	dir := t.TempDir()
	log, err := sessionstream.Create(dir, batchSessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range events {
		if err := log.Append(record); err != nil {
			t.Fatal(err)
		}
	}
	if plan != nil {
		if err := os.WriteFile(filepath.Join(dir, planFile), plan, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sub, err := log.Subscribe(SessionReporter.Name, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &planHarness{t: t, dir: dir, log: log, seen: make(chan struct{}, 1024)}
	h.run = &Run{
		capability: SessionReporter,
		dir:        dir, events: sub, ctx: ctx, graphCtx: context.Background(), cancel: cancel,
		finish: make(chan struct{}), done: make(chan struct{}), opTimeout: time.Minute,
		clock: systemClock{}, batchInterval: time.Hour,
		state: state{Version: 1, Provider: "@test/provider", SessionID: batchSessionID},
	}
	t.Cleanup(func() {
		cancel()
		_ = log.Close()
		_ = sub.Close()
	})
	return h
}

// serve starts the worker with a receiver that answers each chunk with answer.
// The pipe has no process to restart, so a test whose receiver hangs up uses a
// real reporter process instead (planReporterFixture).
func (h *planHarness) serve(answer planAnswer) {
	p, remote := pipeProcess()
	// The pipe has no process to wait for: closing it, as the worker does when
	// it stops, returns at once.
	p.done = make(chan struct{})
	close(p.done)
	h.run.process = p
	h.t.Cleanup(func() { _ = remote.Close() })
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
			h.seen <- struct{}{}
			if err := json.NewEncoder(remote).Encode(answer(*chunk)); err != nil {
				return
			}
		}
	}()
	go h.run.work()
}

func (h *planHarness) sent() []protocolcli.SessionReportingChunk {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.chunks)
}

// awaitEventsChunk returns once the receiver has read an events.jsonl chunk.
func (h *planHarness) awaitEventsChunk() {
	h.t.Helper()
	deadline := time.After(time.Minute)
	for {
		for _, chunk := range h.sent() {
			if chunk.Artifact == sessionstream.EventsFile {
				return
			}
		}
		select {
		case <-h.seen:
		case <-deadline:
			h.t.Fatalf("no events chunk left while the graph ran; sent %s", frameOrder(h.sent()))
		}
	}
}

// finish ends the graph: it closes the stream, writes session.json, and
// returns what Finish returned.
func (h *planHarness) finish() error {
	h.t.Helper()
	if err := h.log.Close(); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.dir, sessionFile), []byte(`{"protocolVersion":2}`), 0o644); err != nil {
		h.t.Fatal(err)
	}
	return h.run.Finish()
}

// checkpoint is the durable checkpoint the worker left.
func (h *planHarness) checkpoint() state {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join(h.dir, StateFile))
	if err != nil {
		h.t.Fatal(err)
	}
	var s state
	if err := json.Unmarshal(data, &s); err != nil {
		h.t.Fatal(err)
	}
	return s
}

// frameOrder names each chunk by its artifact, with ":final" for a final
// marker.
func frameOrder(chunks []protocolcli.SessionReportingChunk) string {
	order := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		order = append(order, chunk.Artifact+map[bool]string{true: ":final", false: ""}[chunk.Final])
	}
	return strings.Join(order, ",")
}

// reassemble joins the data of every chunk of artifact, in order, and reports
// whether its final marker was sent.
func reassemble(t *testing.T, chunks []protocolcli.SessionReportingChunk, artifact string) ([]byte, bool) {
	t.Helper()
	var data bytes.Buffer
	final := false
	for _, chunk := range chunks {
		if chunk.Artifact != artifact {
			continue
		}
		if chunk.Offset != int64(data.Len()) || final {
			t.Fatalf("%s chunk at offset %d after %d bytes (final sent: %v)", artifact, chunk.Offset, data.Len(), final)
		}
		data.Write(chunk.Data)
		final = chunk.Final
	}
	return data.Bytes(), final
}

// planOf is a plan.json of exactly size bytes.
func planOf(size int) []byte {
	return bytes.Repeat([]byte("p"), size)
}

// fullFrame is an events record that, with its LF, fills one chunk.
var fullFrame = bytes.Repeat([]byte("e"), protocolcli.SessionReportingChunkBytes-1)

// TestPlanPrecedesEveryOtherFrame starts the worker with a full events frame
// already committed, which would leave at once: the receiver still reads every
// plan.json chunk and its final marker first. Then a live events chunk leaves
// before the graph ends, and session.json still closes before events.
func TestPlanPrecedesEveryOtherFrame(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "engine-lifecycle", "plan-precedes-every-other-frame")
	plan := planOf(2*protocolcli.SessionReportingChunkBytes + 21)
	h := newPlanHarness(t, plan, fullFrame, []byte(`{"record":"task:start"}`))
	h.serve(acknowledgeEveryChunk)
	h.awaitEventsChunk()
	if err := h.finish(); err != nil {
		t.Fatalf("Finish = %v", err)
	}
	sent := h.sent()
	want := "plan.json,plan.json,plan.json,plan.json:final,events.jsonl,session.json,session.json:final,events.jsonl,events.jsonl:final"
	if got := frameOrder(sent); got != want {
		t.Fatalf("frames = %s\nwant %s", got, want)
	}
	if got, final := reassemble(t, sent, planFile); !bytes.Equal(got, plan) || !final {
		t.Fatalf("plan.json arrived as %d bytes (final %v), want the %d persisted bytes", len(got), final, len(plan))
	}
	if final := sent[3]; final.Offset != int64(len(plan)) || final.Sequence != 3 || len(final.Data) != 0 {
		t.Fatalf("plan final marker = offset %d sequence %d", final.Offset, final.Sequence)
	}
	s := h.checkpoint()
	if !s.Complete || s.Pending != nil || s.PlanOmitted != "" || s.Plan != (cursor{Offset: int64(len(plan)), Sequence: 4, Final: true}) {
		t.Fatalf("checkpoint = %+v", s)
	}
}

// TestPlanIsReadUpToItsBound frames a plan.json of exactly
// SessionReportingPlanBytes to its end, and refuses one byte more before its
// first chunk. Only a plan.json missing before its first chunk is absent; one
// removed or truncated after it, or not a regular file, is unreadable.
func TestPlanIsReadUpToItsBound(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "bounded-transport", "plan-is-bounded-and-omitted-past-its-bound")
	dir := t.TempDir()
	path := filepath.Join(dir, planFile)
	bound := int64(protocolcli.SessionReportingPlanBytes)
	resize := func(size int64) {
		t.Helper()
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, size); err != nil {
			t.Fatal(err)
		}
	}
	resize(bound)
	for _, c := range []cursor{{}, {Offset: bound - 5, Sequence: 1}, {Offset: bound, Sequence: 2}} {
		data, err := readPlan(dir, c)
		if want := min(bound-c.Offset, protocolcli.SessionReportingChunkBytes); err != nil || int64(len(data)) != want {
			t.Fatalf("plan.json at its bound, cursor %+v: %d bytes, %v; want %d bytes", c, len(data), err, want)
		}
	}
	resize(bound + 1)
	for _, c := range []cursor{{}, {Offset: bound - 5, Sequence: 1}} {
		if _, err := readPlan(dir, c); !errors.Is(err, errPlanTooLarge) {
			t.Fatalf("plan.json past its bound, cursor %+v: %v, want errPlanTooLarge", c, err)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := readPlan(dir, cursor{}); !errors.Is(err, errPlanAbsent) {
		t.Fatalf("missing plan.json: %v, want errPlanAbsent", err)
	}
	if _, err := readPlan(dir, cursor{Offset: 5, Sequence: 1}); !errors.Is(err, errPlanUnreadable) {
		t.Fatalf("plan.json removed after its first chunk: %v, want errPlanUnreadable", err)
	}
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPlan(dir, cursor{Offset: 5, Sequence: 1}); !errors.Is(err, errPlanUnreadable) {
		t.Fatalf("plan.json truncated below its cursor: %v, want errPlanUnreadable", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := readPlan(dir, cursor{}); !errors.Is(err, errPlanUnreadable) {
		t.Fatalf("plan.json as a directory: %v, want errPlanUnreadable", err)
	}
}

// TestAnOmittedPlanLeavesTheOtherArtifactsDelivered covers every omission the
// worker decides alone: no plan.json, a plan.json past its bound, one that is
// not a regular file, and a receiver that refuses a plan.json chunk without
// retry. The worker records the reason, sends no further plan.json frame,
// delivers events.jsonl and session.json, and Finish succeeds.
func TestAnOmittedPlanLeavesTheOtherArtifactsDelivered(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "engine-lifecycle", "an-omitted-plan-leaves-delivery-and-the-verdict-unchanged")
	for _, tc := range []struct {
		name   string
		plan   []byte
		answer planAnswer
		reason string
		// planFrames is how many plan.json frames reach the receiver.
		planFrames int
	}{
		{"absent", nil, acknowledgeEveryChunk, planAbsent, 0},
		{"past its bound", planOf(protocolcli.SessionReportingPlanBytes + 1), acknowledgeEveryChunk, planTooLarge, 0},
		{"not a regular file", nil, acknowledgeEveryChunk, planUnreadable, 0},
		{"refused", planOf(10), refusePlan(false), planRefused, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "past its bound" {
				spectest.Proves(t, "cli/native-session-reporting", "bounded-transport", "plan-is-bounded-and-omitted-past-its-bound")
			}
			events := []byte(`{"record":"task:start"}`)
			h := newPlanHarness(t, tc.plan, events)
			if tc.reason == planUnreadable {
				if err := os.Mkdir(filepath.Join(h.dir, planFile), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			h.serve(tc.answer)
			if err := h.finish(); err != nil {
				t.Fatalf("Finish = %v, want an omitted plan.json to leave delivery complete", err)
			}
			sent := h.sent()
			planFrames := 0
			for _, chunk := range sent {
				if chunk.Artifact == planFile {
					if chunk.Offset != 0 || chunk.Sequence != 0 || chunk.Final {
						t.Fatalf("plan.json frame %+v after the omission", chunk)
					}
					planFrames++
				}
			}
			if planFrames != tc.planFrames {
				t.Fatalf("plan.json frames = %d, want %d (%s)", planFrames, tc.planFrames, frameOrder(sent))
			}
			if got := frameOrder(sent[planFrames:]); got != "session.json,session.json:final,events.jsonl,events.jsonl:final" {
				t.Fatalf("after plan.json: %s", got)
			}
			if got, final := reassemble(t, sent, sessionstream.EventsFile); string(got) != string(events)+"\n" || !final {
				t.Fatalf("events.jsonl arrived as %q (final %v)", got, final)
			}
			s := h.checkpoint()
			if s.PlanOmitted != tc.reason || !s.Complete || s.Pending != nil || s.Error != "" || s.Plan.Final {
				t.Fatalf("checkpoint = %+v, want plan.json omitted as %q", s, tc.reason)
			}
			if h.run.state != s {
				t.Fatalf("in-memory state %+v differs from the durable checkpoint %+v", h.run.state, s)
			}
		})
	}
}

// sameFrame reports whether two frames have the same identity and bytes.
func sameFrame(t *testing.T, a, b protocolcli.SessionReportingChunk) bool {
	t.Helper()
	left, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	right, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(left, right)
}

// resumePlan replays the session reporter over the finalized session in dir,
// from the checkpoint there, as sessions replay does, against a receiver that
// acknowledges every chunk. It returns the chunks the receiver read and the
// checkpoint the worker left.
func resumePlan(t *testing.T, dir string) ([]protocolcli.SessionReportingChunk, state) {
	t.Helper()
	log, err := sessionstream.Open(dir, batchSessionID)
	if err != nil {
		t.Fatal(err)
	}
	r, err := openRun(context.Background(), SessionReporter, dir, batchSessionID, "@test/provider")
	if err != nil {
		t.Fatalf("replay refused the checkpoint: %v", err)
	}
	if err := r.subscribe(log); err != nil {
		r.release()
		t.Fatal(err)
	}
	p, remote := pipeProcess()
	p.done = make(chan struct{})
	close(p.done)
	r.process = p
	var chunks []protocolcli.SessionReportingChunk
	received := make(chan struct{})
	go func() {
		defer close(received)
		scanner := bufio.NewScanner(remote)
		scanner.Buffer(make([]byte, 4096), protocolcli.SessionReportingLineBytes)
		for scanner.Scan() {
			chunk, err := protocolcli.ParseSessionReportingChunk(scanner.Bytes())
			if err != nil {
				return
			}
			chunks = append(chunks, *chunk)
			if err := json.NewEncoder(remote).Encode(chunk.Ack()); err != nil {
				return
			}
		}
	}()
	go r.work()
	err = r.Finish()
	_ = remote.Close()
	<-received
	if err != nil {
		t.Fatalf("replay Finish = %v (sent %s)", err, frameOrder(chunks))
	}
	data, err := os.ReadFile(filepath.Join(dir, StateFile))
	if err != nil {
		t.Fatal(err)
	}
	var s state
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	return chunks, s
}

// TestAPlanRefusedWithRetryOnEveryAttemptFailsDelivery has the receiver answer
// plan.json ok:false with retry on every attempt. The plan is not omitted:
// delivery fails with the frame pending, as for any artifact, and a replay
// sends that exact frame first, then session.json and events.jsonl.
func TestAPlanRefusedWithRetryOnEveryAttemptFailsDelivery(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "durable-replay", "replay-sends-an-undelivered-plan-first")
	plan := planOf(10)
	events := []byte(`{"record":"task:start"}`)
	h := newPlanHarness(t, plan, events)
	h.serve(refusePlan(true))
	if err := h.finish(); !errors.Is(err, errRetryBudget) {
		t.Fatalf("Finish = %v, want the retry budget to fail delivery", err)
	}
	frame := protocolcli.NewSessionReportingChunk(batchSessionID, planFile, 0, 0, plan, false)
	sent := h.sent()
	if len(sent) != maxAttempts {
		t.Fatalf("frames = %s, want %d plan.json attempts", frameOrder(sent), maxAttempts)
	}
	for _, chunk := range sent {
		if !sameFrame(t, chunk, frame) {
			t.Fatalf("attempt %+v, want the identical plan.json frame %+v", chunk, frame)
		}
	}
	s := h.checkpoint()
	if s.Pending == nil || !sameFrame(t, *s.Pending, frame) || s.PlanOmitted != "" || s.Plan != (cursor{}) || s.Complete || s.Error != "delivery_incomplete" {
		t.Fatalf("checkpoint = %+v, want the plan.json frame pending and the plan not omitted", s)
	}

	replayed, s := resumePlan(t, h.dir)
	if got := frameOrder(replayed); got != "plan.json,plan.json:final,session.json,session.json:final,events.jsonl,events.jsonl:final" {
		t.Fatalf("replay frames = %s", got)
	}
	if !sameFrame(t, replayed[0], frame) {
		t.Fatalf("replay sent %+v first, want the pending frame %+v", replayed[0], frame)
	}
	if got, final := reassemble(t, replayed, planFile); !bytes.Equal(got, plan) || !final {
		t.Fatalf("plan.json arrived as %q (final %v)", got, final)
	}
	if got, final := reassemble(t, replayed, sessionstream.EventsFile); string(got) != string(events)+"\n" || !final {
		t.Fatalf("events.jsonl arrived as %q (final %v)", got, final)
	}
	if !s.Complete || s.Pending != nil || s.PlanOmitted != "" || s.Error != "" || s.Plan != (cursor{Offset: int64(len(plan)), Sequence: 2, Final: true}) {
		t.Fatalf("replayed checkpoint = %+v", s)
	}
}

// TestACrashMidPlanResumesAtItsPendingFrame replays checkpoints a worker left
// when it stopped partway through plan.json: with its next frame pending, the
// replay sends that exact frame first; with only its cursor, the frame at the
// cursor. Either way the rest of plan.json, then session.json and events.jsonl
// follow. A plan.json removed since its first chunk is omitted as unreadable,
// and the other artifacts still arrive.
func TestACrashMidPlanResumesAtItsPendingFrame(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "durable-replay", "a-crash-mid-plan-resumes-at-its-pending-frame")
	size := protocolcli.SessionReportingChunkBytes
	plan := make([]byte, 2*size+10)
	for i := range plan {
		plan[i] = byte('a' + i%26)
	}
	events := []byte(`{"record":"task:start"}`)
	midway := cursor{Offset: int64(size), Sequence: 1}
	// A pending frame shorter than a read at its cursor would be shows that
	// the replay sends the saved frame, not a fresh read.
	short := protocolcli.NewSessionReportingChunk(batchSessionID, planFile, midway.Offset, midway.Sequence, plan[size:size+100], false)
	atCursor := protocolcli.NewSessionReportingChunk(batchSessionID, planFile, midway.Offset, midway.Sequence, plan[size:2*size], false)
	delivered := "plan.json,plan.json,plan.json:final,session.json,session.json:final,events.jsonl,events.jsonl:final"
	for _, tc := range []struct {
		name    string
		plan    []byte
		pending *protocolcli.SessionReportingChunk
		first   *protocolcli.SessionReportingChunk
		frames  string
		omitted string
	}{
		{"pending frame", plan, &short, &short, delivered, ""},
		{"cursor only", plan, nil, &atCursor, delivered, ""},
		{"plan.json removed", nil, nil, nil, "session.json,session.json:final,events.jsonl,events.jsonl:final", planUnreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
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
			if tc.plan != nil {
				if err := os.WriteFile(filepath.Join(dir, planFile), tc.plan, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			crashed, err := json.Marshal(state{Version: 1, Provider: "@test/provider", SessionID: batchSessionID, Plan: midway, Pending: tc.pending, Error: "delivery_incomplete"})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, StateFile), crashed, 0o600); err != nil {
				t.Fatal(err)
			}

			sent, s := resumePlan(t, dir)
			if got := frameOrder(sent); got != tc.frames {
				t.Fatalf("replay frames = %s\nwant %s", got, tc.frames)
			}
			if tc.first != nil && !sameFrame(t, sent[0], *tc.first) {
				t.Fatalf("replay sent %+v first, want %+v", sent[0], *tc.first)
			}
			offset, sequence, final := midway.Offset, midway.Sequence, false
			for _, chunk := range sent {
				if chunk.Artifact != planFile {
					continue
				}
				end := offset + int64(len(chunk.Data))
				if final || chunk.Offset != offset || chunk.Sequence != sequence || end > int64(len(plan)) || !bytes.Equal(chunk.Data, plan[offset:end]) {
					t.Fatalf("plan.json chunk at offset %d sequence %d, want offset %d sequence %d (final sent: %v)", chunk.Offset, chunk.Sequence, offset, sequence, final)
				}
				offset, sequence, final = end, sequence+1, chunk.Final
			}
			if tc.omitted == "" && (offset != int64(len(plan)) || !final) {
				t.Fatalf("plan.json ended at %d (final %v), want %d", offset, final, len(plan))
			}
			if got, final := reassemble(t, sent, sessionstream.EventsFile); string(got) != string(events)+"\n" || !final {
				t.Fatalf("events.jsonl arrived as %q (final %v)", got, final)
			}
			wantPlan := cursor{Offset: int64(len(plan)), Sequence: 4, Final: true}
			if tc.omitted != "" {
				wantPlan = midway
			}
			if !s.Complete || s.Pending != nil || s.PlanOmitted != tc.omitted || s.Error != "" || s.Plan != wantPlan {
				t.Fatalf("replayed checkpoint = %+v, want plan %+v omitted as %q", s, wantPlan, tc.omitted)
			}
		})
	}
}

// TestAPlanThatAReporterCannotReceiveIsOmittedAndTheReporterRestarts runs a
// real reporter process that exits whenever it reads a plan.json frame, as a
// receiver whose parser does not know the artifact may: every attempt fails
// in transport, the worker omits plan.json as undeliverable, starts the
// reporter again for the next artifact, and delivers the session.
func TestAPlanThatAReporterCannotReceiveIsOmittedAndTheReporterRestarts(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "engine-lifecycle", "an-omitted-plan-leaves-delivery-and-the-verdict-unchanged")
	defer runcredential.SetForTest("")()
	fx := newPlanReporterFixture(t, planExitReporter)
	dir := t.TempDir()
	log, err := sessionstream.Create(dir, holderSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append([]byte(`{"record":"task:start"}`)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, planFile), planOf(100), 0o644); err != nil {
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
	if err := run.Finish(); err != nil {
		t.Fatalf("Finish = %v, want delivery to complete without plan.json", err)
	}
	processes := fx.processes(t)
	if len(processes) != maxAttempts+1 {
		t.Fatalf("reporter processes = %d, want %d that read plan.json and one more", len(processes), maxAttempts+1)
	}
	for i, p := range processes[:maxAttempts] {
		if len(p) != 1 || p[0].Artifact != planFile || p[0].Sequence != 0 {
			t.Fatalf("process %d read %s, want the first plan.json frame alone", i, frameOrder(p))
		}
	}
	if got := frameOrder(processes[maxAttempts]); got != "session.json,session.json:final,events.jsonl,events.jsonl:final" &&
		got != "events.jsonl,session.json,session.json:final,events.jsonl:final" {
		t.Fatalf("the restarted reporter read %s", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, StateFile))
	if err != nil {
		t.Fatal(err)
	}
	var s state
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	if s.PlanOmitted != planUndeliverable || !s.Complete || s.Pending != nil || s.Error != "" {
		t.Fatalf("checkpoint = %+v, want plan.json omitted as undeliverable", s)
	}
}

// The ways the plan reporter helper answers.
const (
	planReporterRecordEnv = "SESSIONREPORTER_PLAN_RECORD"
	planReporterModeEnv   = "SESSIONREPORTER_PLAN_MODE"
	// planExitReporter exits without an answer whenever it reads a plan.json
	// frame, and acknowledges every other chunk.
	planExitReporter = "exit-on-plan"
)

// TestPlanReporterHelperProcess is a session reporter subprocess. It appends
// every chunk it reads, as one JSON line naming its process, to the record,
// then answers as planReporterModeEnv says.
func TestPlanReporterHelperProcess(t *testing.T) {
	path := os.Getenv(planReporterRecordEnv)
	if path == "" {
		return
	}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 4096), protocolcli.SessionReportingLineBytes)
	for in.Scan() {
		chunk, err := protocolcli.ParseSessionReportingChunk(in.Bytes())
		if err != nil {
			os.Exit(20)
		}
		line, _ := json.Marshal(struct {
			PID   int                               `json:"pid"`
			Chunk protocolcli.SessionReportingChunk `json:"chunk"`
		}{os.Getpid(), *chunk})
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			os.Exit(21)
		}
		_, _ = f.Write(append(line, '\n'))
		_ = f.Close()
		if chunk.Artifact == planFile && os.Getenv(planReporterModeEnv) == planExitReporter {
			os.Exit(22)
		}
		_ = json.NewEncoder(os.Stdout).Encode(chunk.Ack())
		if chunk.Final && chunk.Artifact == sessionstream.EventsFile {
			os.Exit(0)
		}
	}
	os.Exit(0)
}

// planReporterFixture selects the session reporter alone, served by the plan
// reporter helper in one mode.
type planReporterFixture struct {
	record  string
	resolve Resolve
}

func newPlanReporterFixture(t *testing.T, mode string) planReporterFixture {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(protocolcli.SessionReporterEnv, holderProvider)
	t.Setenv(protocolcli.SessionReporterTokenEnv, "")
	t.Setenv(protocolcli.LogReporterEnv, "")
	t.Setenv(protocolcli.LogReporterTokenEnv, "")
	record := filepath.Join(t.TempDir(), "record.jsonl")
	launch := LaunchSpec{
		Command: self, Args: []string{"-test.run=^TestPlanReporterHelperProcess$"},
		Env: append(os.Environ(), planReporterRecordEnv+"="+record, planReporterModeEnv+"="+mode), Runtime: self,
	}
	return planReporterFixture{record: record, resolve: func(context.Context) (LaunchSpec, error) {
		spec := launch
		spec.Env = slices.Clone(launch.Env)
		return spec, nil
	}}
}

// processes returns the chunks each helper process read, in start order.
func (fx planReporterFixture) processes(t *testing.T) [][]protocolcli.SessionReportingChunk {
	t.Helper()
	data, err := os.ReadFile(fx.record)
	if err != nil {
		t.Fatal(err)
	}
	var processes [][]protocolcli.SessionReportingChunk
	index := map[int]int{}
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var entry struct {
			PID   int                               `json:"pid"`
			Chunk protocolcli.SessionReportingChunk `json:"chunk"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("helper record %q: %v", line, err)
		}
		i, ok := index[entry.PID]
		if !ok {
			i = len(processes)
			index[entry.PID] = i
			processes = append(processes, nil)
		}
		processes[i] = append(processes[i], entry.Chunk)
	}
	return processes
}

// TestACheckpointWithoutAPlanCursorStaysValid opens checkpoints that hold no
// plan member. Once events.jsonl or session.json progressed, plan.json can no
// longer precede them: it is omitted as late, and a completed checkpoint stays
// complete. A checkpoint that framed nothing yet still sends plan.json first.
func TestACheckpointWithoutAPlanCursorStaysValid(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "durable-replay", "a-checkpoint-without-a-plan-cursor-stays-valid")
	pending, err := json.Marshal(protocolcli.NewSessionReportingChunk("session", "events.jsonl", 0, 0, []byte("abc"), false))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		checkpoint string
		omitted    string
		complete   bool
	}{
		{"events progressed", `{"version":1,"provider":"@test/provider","sessionId":"session","events":{"offset":3,"sequence":1,"final":false},"session":{"offset":0,"sequence":0,"final":false},"complete":false}`, planLate, false},
		{"completed", `{"version":1,"provider":"@test/provider","sessionId":"session","events":{"offset":3,"sequence":2,"final":true},"session":{"offset":2,"sequence":2,"final":true},"complete":true}`, planLate, true},
		{"events frame pending", `{"version":1,"provider":"@test/provider","sessionId":"session","events":{"offset":0,"sequence":0,"final":false},"session":{"offset":0,"sequence":0,"final":false},"pending":` + string(pending) + `,"complete":false}`, planLate, false},
		{"nothing framed", `{"version":1,"provider":"@test/provider","sessionId":"session","events":{"offset":0,"sequence":0,"final":false},"session":{"offset":0,"sequence":0,"final":false},"complete":false,"error":"provider_unavailable"}`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, StateFile), []byte(tc.checkpoint), 0o600); err != nil {
				t.Fatal(err)
			}
			r, err := openRun(context.Background(), SessionReporter, dir, "session", "@test/provider")
			if err != nil {
				t.Fatalf("a checkpoint without a plan cursor was refused: %v", err)
			}
			r.release()
			if r.state.PlanOmitted != tc.omitted || r.state.Complete != tc.complete || r.state.Plan != (cursor{}) {
				t.Fatalf("opened state = %+v, want plan.json omitted as %q and complete=%v", r.state, tc.omitted, tc.complete)
			}
			data, err := os.ReadFile(filepath.Join(dir, StateFile))
			if err != nil {
				t.Fatal(err)
			}
			if want, err := json.Marshal(r.state); err != nil || !bytes.Equal(data, want) {
				t.Fatalf("saved checkpoint %s, want %s: %v", data, want, err)
			}
		})
	}
	// The log reporter never holds plan.json state: an old checkpoint opens
	// unchanged.
	dir := t.TempDir()
	logCheckpoint := `{"version":1,"provider":"@test/provider","sessionId":"session","events":{"offset":3,"sequence":2,"final":true},"session":{"offset":0,"sequence":0,"final":false},"complete":true}`
	if err := os.WriteFile(filepath.Join(dir, LogStateFile), []byte(logCheckpoint), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := openRun(context.Background(), LogReporter, dir, "session", "@test/provider")
	if err != nil {
		t.Fatalf("a log reporter checkpoint was refused: %v", err)
	}
	r.release()
	if r.state.PlanOmitted != "" || r.state.Plan != (cursor{}) || !r.state.Complete {
		t.Fatalf("log reporter state = %+v", r.state)
	}
}

// TestReportingCursorRejectsInconsistentPlanState refuses every plan.json
// state the worker never writes.
func TestReportingCursorRejectsInconsistentPlanState(t *testing.T) {
	planPending := protocolcli.NewSessionReportingChunk("session", planFile, 0, 0, []byte("{}"), false)
	shiftedPlan := protocolcli.NewSessionReportingChunk("session", planFile, 1, 0, []byte("{}"), false)
	valid := state{Version: 1, SessionID: "session", Provider: "@test/provider"}
	if err := (&Run{capability: SessionReporter, state: valid}).validate(); err != nil {
		t.Fatalf("a fresh session reporter state was refused: %v", err)
	}
	for name, mutate := range map[string]func(*state){
		"negative plan offset":            func(s *state) { s.Plan.Offset = -1 },
		"negative plan sequence":          func(s *state) { s.Plan.Sequence = -1 },
		"unknown omission":                func(s *state) { s.PlanOmitted = "forgotten" },
		"omitted after its final marker":  func(s *state) { s.Plan.Final, s.PlanOmitted = true, planRefused },
		"events past an open plan":        func(s *state) { s.Events = cursor{Offset: 3, Sequence: 1} },
		"session past an open plan":       func(s *state) { s.Session = cursor{Offset: 2, Sequence: 1} },
		"complete with an open plan":      func(s *state) { s.Events, s.Session, s.Complete = cursor{3, 2, true}, cursor{2, 2, true}, true },
		"incomplete with a closed plan":   func(s *state) { s.Events, s.Session, s.PlanOmitted = cursor{3, 2, true}, cursor{2, 2, true}, planLate },
		"pending plan frame once omitted": func(s *state) { s.PlanOmitted, s.Pending = planRefused, &planPending },
		"pending plan frame once closed":  func(s *state) { s.Plan, s.Pending = cursor{2, 1, true}, &planPending },
		"pending plan frame off cursor":   func(s *state) { s.Pending = &shiftedPlan },
	} {
		r := &Run{capability: SessionReporter, state: valid}
		mutate(&r.state)
		if err := r.validate(); err == nil {
			t.Errorf("%s: invalid state accepted: %+v", name, r.state)
		}
	}
	logValid := state{Version: 1, SessionID: "session", Provider: "@test/provider"}
	for name, mutate := range map[string]func(*state){
		"plan cursor":        func(s *state) { s.Plan.Offset = 1 },
		"plan final":         func(s *state) { s.Plan.Final = true },
		"plan omission":      func(s *state) { s.PlanOmitted = planAbsent },
		"pending plan frame": func(s *state) { s.Pending = &planPending },
	} {
		r := &Run{capability: LogReporter, state: logValid}
		mutate(&r.state)
		if err := r.validate(); err == nil {
			t.Errorf("log reporter %s: invalid state accepted: %+v", name, r.state)
		}
	}
}

// TestTheLogReporterNeverSendsPlan drains the log reporter beside a plan.json:
// it never frames it.
func TestTheLogReporterNeverSendsPlan(t *testing.T) {
	h := newCapabilityHarness(t, LogReporter)
	if err := os.WriteFile(filepath.Join(h.run.dir, planFile), planOf(10), 0o644); err != nil {
		t.Fatal(err)
	}
	h.append(`{"record":"task:end"}`)
	h.settle()
	if err := h.log.Close(); err != nil {
		t.Fatal(err)
	}
	h.settle()
	close(h.run.finish)
	if err := <-h.done; err != nil {
		t.Fatalf("terminal drain: %v", err)
	}
	if got := frameOrder(h.sent()); got != "events.jsonl,events.jsonl:final" {
		t.Fatalf("the log reporter sent %s", got)
	}
	if h.run.state.Plan != (cursor{}) || h.run.state.PlanOmitted != "" {
		t.Fatalf("the log reporter recorded plan.json state: %+v", h.run.state)
	}
}
