package sessionreporter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/sessionstream"
)

const framesFeature = "cli/native-session-reporting"

// largeReceiver is an in-memory reporter that reads frames of every size,
// version 3 ones included, and answers each with answer.
type largeReceiver struct {
	mu     sync.Mutex
	chunks []protocolcli.SessionReportingChunk
	done   chan struct{}
}

// serveLarge hands r a piped reporter served by answer and starts its worker.
func serveLarge(t *testing.T, r *Run, answer planAnswer) *largeReceiver {
	t.Helper()
	p, remote := pipeProcess()
	p.done = make(chan struct{})
	close(p.done)
	r.process = p
	rec := &largeReceiver{done: make(chan struct{})}
	t.Cleanup(func() { _ = remote.Close() })
	go func() {
		defer close(rec.done)
		scanner := bufio.NewScanner(remote)
		scanner.Buffer(make([]byte, 4096), protocolcli.SessionReportingMaxLineBytes)
		for scanner.Scan() {
			chunk, err := protocolcli.ParseSessionReportingChunk(scanner.Bytes())
			if err != nil {
				t.Errorf("the engine sent an invalid frame: %v", err)
				return
			}
			rec.mu.Lock()
			rec.chunks = append(rec.chunks, *chunk)
			rec.mu.Unlock()
			if err := json.NewEncoder(remote).Encode(answer(*chunk)); err != nil {
				return
			}
		}
	}()
	go r.work()
	return rec
}

func (rec *largeReceiver) sent() []protocolcli.SessionReportingChunk {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return slices.Clone(rec.chunks)
}

// withCap acknowledges as answer does, and names cap on every acknowledgement.
func withCap(cap int, answer planAnswer) planAnswer {
	return func(chunk protocolcli.SessionReportingChunk) *protocolcli.SessionReportingAck {
		ack := answer(chunk)
		ack.ChunkBytes = cap
		return ack
	}
}

// finishWith ends the harness's graph with session as session.json and
// returns what Finish returned.
func finishWith(t *testing.T, h *planHarness, session []byte) error {
	t.Helper()
	if err := h.log.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.dir, sessionFile), session, 0o644); err != nil {
		t.Fatal(err)
	}
	return h.run.Finish()
}

// framesOf is every chunk of artifact, in order.
func framesOf(chunks []protocolcli.SessionReportingChunk, artifact string) []protocolcli.SessionReportingChunk {
	var out []protocolcli.SessionReportingChunk
	for _, chunk := range chunks {
		if chunk.Artifact == artifact {
			out = append(out, chunk)
		}
	}
	return out
}

func sizes(chunks []protocolcli.SessionReportingChunk) []int {
	out := make([]int, 0, len(chunks))
	for _, chunk := range chunks {
		out = append(out, len(chunk.Data))
	}
	return out
}

// TestFramesFollowTheAcknowledgedCap has the reporter name a 1 MiB cap on
// every acknowledgement: the first frame is cut at 64 KiB, every later one at
// 1 MiB, a frame above 64 KiB is version 3, and every artifact arrives whole.
// Without a cap the frames stay at 64 KiB.
func TestFramesFollowTheAcknowledgedCap(t *testing.T) {
	spectest.Proves(t, framesFeature, "negotiated-frame-size", "frames-follow-the-acknowledged-cap")
	const cap = 1 << 20
	plan := planOf(200 << 10)
	session := bytes.Repeat([]byte("s"), 2*cap+300<<10)
	record := bytes.Repeat([]byte("e"), 100<<10-1)

	h := newPlanHarness(t, plan, record, record, record)
	rec := serveLarge(t, h.run, withCap(cap, acknowledgeEveryChunk))
	if err := finishWith(t, h, session); err != nil {
		t.Fatalf("Finish = %v (sent %s)", err, frameOrder(rec.sent()))
	}
	sent := rec.sent()
	if first := sent[0]; first.Artifact != planFile || len(first.Data) != protocolcli.SessionReportingChunkBytes || first.ProtocolVersion != protocolcli.SessionReportingVersion {
		t.Fatalf("first frame %s of %d bytes at version %d, want plan.json at 64 KiB, version 1", first.Artifact, len(first.Data), first.ProtocolVersion)
	}
	if got, want := sizes(framesOf(sent, planFile)), []int{64 << 10, 136 << 10, 0}; !slices.Equal(got, want) {
		t.Fatalf("plan.json frames %v, want %v", got, want)
	}
	if got, want := sizes(framesOf(sent, sessionFile)), []int{cap, cap, 300 << 10, 0}; !slices.Equal(got, want) {
		t.Fatalf("session.json frames %v, want %v", got, want)
	}
	for _, chunk := range sent {
		if want := protocolcli.SessionReportingFrameVersion(len(chunk.Data)); chunk.ProtocolVersion != want || len(chunk.Data) > cap {
			t.Fatalf("%s frame %d: %d bytes at version %d", chunk.Artifact, chunk.Sequence, len(chunk.Data), chunk.ProtocolVersion)
		}
	}
	for artifact, want := range map[string][]byte{planFile: plan, sessionFile: session, sessionstream.EventsFile: bytes.Repeat(append(record, '\n'), 3)} {
		got, final := reassemble(t, sent, artifact)
		if !final || !bytes.Equal(got, want) {
			t.Fatalf("%s arrived as %d bytes (final %v), want %d", artifact, len(got), final, len(want))
		}
	}
	if events := sizes(framesOf(sent, sessionstream.EventsFile)); len(events) != 2 {
		t.Fatalf("events.jsonl frames %v, want one frame and its final marker", events)
	}

	// No cap named: every frame stays at 64 KiB, version 1.
	plain := newPlanHarness(t, plan, record)
	rec = serveLarge(t, plain.run, acknowledgeEveryChunk)
	if err := finishWith(t, plain, session); err != nil {
		t.Fatal(err)
	}
	for _, chunk := range rec.sent() {
		if len(chunk.Data) > protocolcli.SessionReportingChunkBytes || chunk.ProtocolVersion != protocolcli.SessionReportingVersion {
			t.Fatalf("without a cap, %s frame %d has %d bytes", chunk.Artifact, chunk.Sequence, len(chunk.Data))
		}
	}

	// The log reporter ignores a cap.
	logged := newPlanHarness(t, nil, record, record)
	logged.run.capability = LogReporter
	logged.run.state = state{Version: LogReporter.checkpointVersion(), Provider: "@test/provider", SessionID: batchSessionID}
	rec = serveLarge(t, logged.run, withCap(cap, acknowledgeEveryChunk))
	if err := finishWith(t, logged, session); err != nil {
		t.Fatal(err)
	}
	for _, chunk := range rec.sent() {
		if len(chunk.Data) > protocolcli.SessionReportingChunkBytes {
			t.Fatalf("the log reporter cut a frame of %d bytes", len(chunk.Data))
		}
	}
}

// TestAnUnsupportedLargeFrameIsCutAgainAt64KiB has the reporter refuse the
// first version 3 frame with frame_unsupported: the engine sends the same
// offset and sequence again as a 64 KiB frame, keeps the cap the refusal
// named, and delivers every artifact whole.
func TestAnUnsupportedLargeFrameIsCutAgainAt64KiB(t *testing.T) {
	spectest.Proves(t, framesFeature, "negotiated-frame-size", "an-unsupported-large-frame-is-cut-again-at-64-kib")
	session := bytes.Repeat([]byte("s"), 300<<10)
	refused := false
	answer := func(chunk protocolcli.SessionReportingChunk) *protocolcli.SessionReportingAck {
		ack := chunk.Ack()
		if refused {
			ack.ChunkBytes = protocolcli.SessionReportingChunkBytes
			return &ack
		}
		ack.ChunkBytes = 1 << 20
		if chunk.ProtocolVersion == protocolcli.SessionReportingLargeFrameVersion {
			refused = true
			ack.OK, ack.Code, ack.ChunkBytes = false, protocolcli.SessionReportingCodeFrameUnsupported, protocolcli.SessionReportingChunkBytes
		}
		return &ack
	}
	h := newPlanHarness(t, planOf(10))
	rec := serveLarge(t, h.run, answer)
	if err := finishWith(t, h, session); err != nil {
		t.Fatalf("Finish = %v (sent %s)", err, frameOrder(rec.sent()))
	}
	frames := framesOf(rec.sent(), sessionFile)
	if len(frames) < 2 || frames[0].ProtocolVersion != protocolcli.SessionReportingLargeFrameVersion {
		t.Fatalf("session.json frames %v: the first is not version 3", sizes(frames))
	}
	again := frames[1]
	if again.Offset != frames[0].Offset || again.Sequence != frames[0].Sequence || len(again.Data) != protocolcli.SessionReportingChunkBytes || again.ProtocolVersion != protocolcli.SessionReportingVersion {
		t.Fatalf("after frame_unsupported the engine sent offset %d sequence %d, %d bytes, version %d", again.Offset, again.Sequence, len(again.Data), again.ProtocolVersion)
	}
	// What the receiver stored: every frame but the refused one.
	stored := append([]protocolcli.SessionReportingChunk{}, frames[1:]...)
	got, final := reassemble(t, stored, sessionFile)
	if !final || !bytes.Equal(got, session) {
		t.Fatalf("session.json arrived as %d bytes (final %v)", len(got), final)
	}
	for _, chunk := range stored {
		if len(chunk.Data) > protocolcli.SessionReportingChunkBytes {
			t.Fatalf("a frame of %d bytes followed a 64 KiB cap", len(chunk.Data))
		}
	}
	if s := h.checkpoint(); !s.Complete || s.Pending != nil {
		t.Fatalf("checkpoint after the fallback: complete %v, pending %v", s.Complete, s.Pending != nil)
	}

	// Any other refusal of a version 3 frame keeps its meaning: delivery fails
	// with the frame pending.
	other := newPlanHarness(t, planOf(10))
	serveLarge(t, other.run, func(chunk protocolcli.SessionReportingChunk) *protocolcli.SessionReportingAck {
		ack := chunk.Ack()
		ack.ChunkBytes = 1 << 20
		if chunk.ProtocolVersion == protocolcli.SessionReportingLargeFrameVersion {
			ack.OK, ack.Code = false, "invalid_chunk"
		}
		return &ack
	})
	if err := finishWith(t, other, session); err == nil || !strings.Contains(err.Error(), "invalid_chunk") {
		t.Fatalf("Finish after invalid_chunk on a version 3 frame = %v", err)
	}
	if p := other.checkpoint().Pending; p == nil || p.ProtocolVersion != protocolcli.SessionReportingLargeFrameVersion {
		t.Fatal("the refused version 3 frame is not pending")
	}
}

// TestAPendingLargeFrameReplaysUnchanged leaves a version 3 session.json frame
// pending in a checkpoint, as an interrupted run does, and replays it: the
// checkpoint opens, and the first frame the reporter reads is that frame,
// byte for byte, although a new engine starts at 64 KiB.
func TestAPendingLargeFrameReplaysUnchanged(t *testing.T) {
	spectest.Proves(t, framesFeature, "negotiated-frame-size", "a-pending-large-frame-replays-unchanged")
	dir := t.TempDir()
	log, err := sessionstream.Create(dir, batchSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append([]byte("e")); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	session := bytes.Repeat([]byte("s"), 3<<20)
	if err := os.WriteFile(filepath.Join(dir, sessionFile), session, 0o644); err != nil {
		t.Fatal(err)
	}
	pending := protocolcli.NewSessionReportingChunk(batchSessionID, sessionFile, 0, 0, session[:2<<20], false)
	checkpoint := state{Version: viewCheckpointVersion, Provider: "@test/provider", SessionID: batchSessionID, PlanOmitted: planAbsent, Pending: &pending}
	data, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, StateFile), data, 0o600); err != nil {
		t.Fatal(err)
	}

	events, err := sessionstream.Open(dir, batchSessionID)
	if err != nil {
		t.Fatal(err)
	}
	r, err := openRun(context.Background(), SessionReporter, dir, batchSessionID, "@test/provider")
	if err != nil {
		t.Fatalf("replay refused a checkpoint with a version 3 pending frame: %v", err)
	}
	if err := r.subscribe(events); err != nil {
		r.release()
		t.Fatal(err)
	}
	rec := serveLarge(t, r, acknowledgeEveryChunk)
	if err := r.Finish(); err != nil {
		t.Fatalf("replay Finish = %v (sent %s)", err, frameOrder(rec.sent()))
	}
	sent := rec.sent()
	if !sameFrame(t, sent[0], pending) {
		t.Fatalf("the replay's first frame is %s %d (%d bytes), not the pending frame", sent[0].Artifact, sent[0].Sequence, len(sent[0].Data))
	}
	got, final := reassemble(t, sent, sessionFile)
	if !final || !bytes.Equal(got, session) {
		t.Fatalf("session.json arrived as %d bytes (final %v)", len(got), final)
	}
}

// TestOnlyTheSessionReporterReadsTheFramingVariable pins who learns the
// framing version: the session reporter's launch environment names it, the
// log reporter's does not, and an inherited value never reaches either.
func TestOnlyTheSessionReporterReadsTheFramingVariable(t *testing.T) {
	spectest.Proves(t, framesFeature, "negotiated-frame-size", "only-the-session-reporter-reads-the-framing-variable")
	inherited := []string{"PATH=/bin", protocolcli.SessionReportingFramingEnv + "=99"}
	framing := protocolcli.SessionReportingFramingEnv + "=5"
	if got := SessionReporter.launchEnv(context.Background(), inherited); !slices.Equal(got, []string{"PATH=/bin", framing}) {
		t.Fatalf("session reporter launch environment = %v", got)
	}
	if got := LogReporter.launchEnv(context.Background(), inherited); !slices.Equal(got, []string{"PATH=/bin"}) {
		t.Fatalf("log reporter launch environment = %v", got)
	}
	for _, c := range Capabilities() {
		if c.LargeFrames != (c.Name == protocolcli.SessionReporterCommand) {
			t.Errorf("%s LargeFrames = %v", c.Label, c.LargeFrames)
		}
	}
}

// TestAFrameUnsupportedAfterAnEarlierAttemptIsNotCutAgain refuses a version 3
// frame with retry, then frame_unsupported: the first attempt may have been
// stored, so the engine keeps the frame pending and fails the delivery
// instead of cutting its sequence again. A frame read back from a checkpoint
// is never cut again either.
func TestAFrameUnsupportedAfterAnEarlierAttemptIsNotCutAgain(t *testing.T) {
	spectest.Proves(t, framesFeature, "negotiated-frame-size", "an-unsupported-large-frame-is-cut-again-at-64-kib")
	session := bytes.Repeat([]byte("s"), 300<<10)
	attempts := 0
	h := newPlanHarness(t, planOf(10))
	rec := serveLarge(t, h.run, func(chunk protocolcli.SessionReportingChunk) *protocolcli.SessionReportingAck {
		ack := chunk.Ack()
		ack.ChunkBytes = 1 << 20
		if chunk.ProtocolVersion == protocolcli.SessionReportingLargeFrameVersion {
			attempts++
			ack.OK, ack.Retryable, ack.Code = false, attempts == 1, map[bool]string{true: "receiver_unavailable", false: protocolcli.SessionReportingCodeFrameUnsupported}[attempts == 1]
		}
		return &ack
	})
	if err := finishWith(t, h, session); err == nil || !strings.Contains(err.Error(), protocolcli.SessionReportingCodeFrameUnsupported) {
		t.Fatalf("Finish = %v, want the delivery to fail on frame_unsupported", err)
	}
	for _, chunk := range framesOf(rec.sent(), sessionFile) {
		if chunk.ProtocolVersion != protocolcli.SessionReportingLargeFrameVersion {
			t.Fatalf("session.json sequence %d was cut again at %d bytes", chunk.Sequence, len(chunk.Data))
		}
	}
	pending := h.checkpoint().Pending
	if pending == nil || pending.ProtocolVersion != protocolcli.SessionReportingLargeFrameVersion {
		t.Fatal("the version 3 frame is not pending")
	}

	// A replay reads the frame back from the checkpoint: frame_unsupported on
	// its first attempt still does not cut it again.
	events, err := sessionstream.Open(h.dir, batchSessionID)
	if err != nil {
		t.Fatal(err)
	}
	r, err := openRun(context.Background(), SessionReporter, h.dir, batchSessionID, "@test/provider")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.subscribe(events); err != nil {
		r.release()
		t.Fatal(err)
	}
	replayed := serveLarge(t, r, func(chunk protocolcli.SessionReportingChunk) *protocolcli.SessionReportingAck {
		ack := chunk.Ack()
		ack.OK, ack.Code, ack.ChunkBytes = false, protocolcli.SessionReportingCodeFrameUnsupported, protocolcli.SessionReportingChunkBytes
		return &ack
	})
	if err := r.Finish(); err == nil {
		t.Fatal("a replay cut a restored version 3 frame again")
	}
	if sent := replayed.sent(); len(sent) != 1 || !sameFrame(t, sent[0], *pending) {
		t.Fatalf("the replay sent %s, want only the pending frame", frameOrder(sent))
	}
}
