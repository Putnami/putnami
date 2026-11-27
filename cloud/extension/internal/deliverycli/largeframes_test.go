package deliverycli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

const largeFramesFeature = "cloud/large-session-reporting-frames"

// largeIngest is a session receiver for ADR 0131. advertise is the cap its
// answers name (0: none, as today's receiver); store is the largest decoded
// frame it stores, 413 past it (0: advertise, or 64 KiB). silentPost answers
// every POST without the headers, as a receiver rolled back mid-run.
type largeIngest struct {
	mu         sync.Mutex
	advertise  int
	store      int
	silentPost bool
	options    int
	posts      []string
	stored     map[string][][]byte
	// gateway answers the next POSTs of a version 3 size with this status and
	// no receiver headers, after storing the frame when gatewayStores is set,
	// as a gateway in front of the receiver does.
	gateway       []int
	gatewayStores bool
	// encodings is the Content-Encoding of every POST.
	encodings []string
}

func (f *largeIngest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set(acceptEncodingHeader, gzipCoding)
	if f.advertise > 0 && !(f.silentPost && r.Method == http.MethodPost) {
		w.Header().Set(sessionContractHeader, "2")
		w.Header().Set(sessionChunkBytesHeader, strconv.Itoa(f.advertise))
	}
	if r.Method == http.MethodOptions {
		f.options++
		w.WriteHeader(http.StatusNoContent)
		return
	}
	part, seq := r.Header.Get(sessionPartHeader), r.Header.Get(sessionSeqHeader)
	f.encodings = append(f.encodings, r.Header.Get(contentEncodingHeader))
	body, ok := receivedChunkBody(r, true)
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if len(f.gateway) > 0 && len(body) > protocolcli.SessionReportingChunkBytes {
		status := f.gateway[0]
		f.gateway = f.gateway[1:]
		if f.gatewayStores {
			f.stored[part] = append(f.stored[part], body)
		}
		for _, name := range []string{acceptEncodingHeader, sessionContractHeader, sessionChunkBytesHeader} {
			w.Header().Del(name)
		}
		w.WriteHeader(status)
		return
	}
	f.posts = append(f.posts, part+"/"+seq+"/"+strconv.Itoa(len(body)))
	limit := f.store
	if limit == 0 {
		limit = max(f.advertise, protocolcli.SessionReportingChunkBytes)
	}
	if len(body) > limit {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	if f.stored == nil {
		f.stored = map[string][][]byte{}
	}
	f.stored[part] = append(f.stored[part], body)
	n, _ := strconv.Atoi(seq)
	_ = json.NewEncoder(w).Encode(map[string]any{"part": part, "seq": n})
}

func newLargeIngest(t *testing.T, ingest *largeIngest, framing bool) sessionReporterConfig {
	t.Helper()
	srv := httptest.NewServer(ingest)
	t.Cleanup(srv.Close)
	return sessionReporterConfig{ingestURL: srv.URL, token: "run-secret", framing: framing}
}

const largeSession = "20261127-120000-abc123"

// large is a compressible session.json frame of n bytes.
func large(n int) string {
	return eventLines(n/150 + 1)[:n]
}

func requireLargeAck(t *testing.T, ack protocolcli.SessionReportingAck, ok bool, code string, retryable bool, cap int) {
	t.Helper()
	if ack.OK != ok || ack.Code != code || ack.Retryable != retryable || ack.ChunkBytes != cap {
		t.Fatalf("ack = %+v, want ok=%v code=%q retryable=%v chunkBytes=%d", ack, ok, code, retryable, cap)
	}
}

// A receiver that advertises 1 MiB: the first answer names the cap on the
// acknowledgement, and the next frame, version 3, is posted and stored whole,
// with no OPTIONS request.
func TestALargeFrameFollowsAnAdvertisingAnswer(t *testing.T) {
	spectest.Proves(t, largeFramesFeature, "large-frames-only-after-an-advertising-answer", "a-large-frame-follows-an-advertising-answer")
	ingest := &largeIngest{advertise: 1 << 20}
	cfg := newLargeIngest(t, ingest, true)
	data := large(200 << 10)
	acks := runReporter(t, cfg,
		frame(t, largeSession, "session.json", 0, 0, "{}", false),
		frame(t, largeSession, "session.json", 2, 1, data, false),
	)
	requireLargeAck(t, acks[0], true, "", false, 1<<20)
	requireLargeAck(t, acks[1], true, "", false, 1<<20)
	if ingest.options != 0 || len(ingest.stored["session"]) != 2 || string(ingest.stored["session"][1]) != data {
		t.Fatalf("options %d, stored %d frames: want the version 3 frame stored whole after no probe", ingest.options, len(ingest.stored["session"]))
	}
}

// A version 3 frame to a destination that has not answered in this process,
// as in a replay: one OPTIONS request first, then the POST; the next version
// 3 frame needs no probe.
func TestAnUnansweredDestinationIsProbedOnce(t *testing.T) {
	spectest.Proves(t, largeFramesFeature, "large-frames-only-after-an-advertising-answer", "an-unanswered-destination-is-probed-once")
	ingest := &largeIngest{advertise: 4 << 20}
	cfg := newLargeIngest(t, ingest, true)
	first, second := large(100<<10), large(300<<10)
	acks := runReporter(t, cfg,
		frame(t, largeSession, "session.json", 0, 0, first, false),
		frame(t, largeSession, "session.json", int64(len(first)), 1, second, false),
	)
	requireLargeAck(t, acks[0], true, "", false, 4<<20)
	requireLargeAck(t, acks[1], true, "", false, 4<<20)
	if ingest.options != 1 || len(ingest.posts) != 2 {
		t.Fatalf("options %d, posts %v: want one probe and two posts", ingest.options, ingest.posts)
	}
}

// Today's receiver advertises nothing: acknowledgements name 64 KiB, and a
// version 3 frame is refused frame_unsupported without retry and without a
// POST, whether the receiver answered before or only to the probe.
func TestAnUnadvertisedLargeFrameIsUnsupported(t *testing.T) {
	spectest.Proves(t, largeFramesFeature, "large-frames-only-after-an-advertising-answer", "an-unadvertised-large-frame-is-unsupported")
	ingest := &largeIngest{}
	cfg := newLargeIngest(t, ingest, true)
	acks := runReporter(t, cfg,
		frame(t, largeSession, "session.json", 0, 0, large(100<<10), false),
		frame(t, largeSession, "session.json", 0, 0, "{}", false),
		frame(t, largeSession, "session.json", 2, 1, large(100<<10), false),
	)
	requireLargeAck(t, acks[0], false, ackCodeFrameUnsupported, false, protocolcli.SessionReportingChunkBytes)
	requireLargeAck(t, acks[1], true, "", false, protocolcli.SessionReportingChunkBytes)
	requireLargeAck(t, acks[2], false, ackCodeFrameUnsupported, false, protocolcli.SessionReportingChunkBytes)
	if ingest.options != 1 || strings.Join(ingest.posts, ",") != "session/0/2" {
		t.Fatalf("options %d, posts %v: want one probe and only the 64 KiB frame posted", ingest.options, ingest.posts)
	}
}

// An instance that advertises 4 MiB but stores 64 KiB, as in a mixed fleet,
// answers a version 3 frame 413: it stored nothing, so the frame is
// frame_unsupported, without retry.
func TestA413OnALargeFrameIsUnsupported(t *testing.T) {
	spectest.Proves(t, largeFramesFeature, "large-frames-only-after-an-advertising-answer", "a-413-on-a-large-frame-is-unsupported")
	ingest := &largeIngest{advertise: 4 << 20, store: protocolcli.SessionReportingChunkBytes}
	cfg := newLargeIngest(t, ingest, true)
	acks := runReporter(t, cfg,
		frame(t, largeSession, "session.json", 0, 0, "{}", false),
		frame(t, largeSession, "session.json", 2, 1, large(100<<10), false),
	)
	requireLargeAck(t, acks[1], false, ackCodeFrameUnsupported, false, 4<<20)
	if len(ingest.stored["session"]) != 1 {
		t.Fatalf("stored %d session frames, want only the first", len(ingest.stored["session"]))
	}
}

// chunkBytes rides on every acknowledgement only when the engine named
// framing version 3; an earlier engine, whose parser refuses the member,
// never gets it.
func TestAcksCarryChunkBytesOnlyWhenTheEngineReadsThem(t *testing.T) {
	spectest.Proves(t, largeFramesFeature, "acknowledgements-carry-the-cap", "acks-carry-chunk-bytes-only-when-the-engine-reads-them")
	for _, framing := range []bool{false, true} {
		cfg := newLargeIngest(t, &largeIngest{advertise: 1 << 20}, framing)
		acks := runReporter(t, cfg, frame(t, largeSession, "session.json", 0, 0, "{}", false))
		if want := map[bool]int{false: 0, true: 1 << 20}[framing]; acks[0].ChunkBytes != want {
			t.Fatalf("framing %v: chunkBytes %d, want %d", framing, acks[0].ChunkBytes, want)
		}
	}
	for value, want := range map[string]bool{"3": true, "\t3\n": true, "4": true, "": false, "2": false, "1": false, "three": false} {
		if got := framingRequested(map[string]string{protocolcli.SessionReportingFramingEnv: value}); got != want {
			t.Errorf("framingRequested(%q) = %v, want %v", value, got, want)
		}
	}
	for _, tc := range []struct {
		contract, bytes string
		want            int
	}{
		{"2", "4194304", 4 << 20}, {"2", "65537", 65537}, {"2", "65536", 0}, {"2", "4194305", 0},
		{"1", "4194304", 0}, {"", "4194304", 0}, {"2", "", 0}, {"2", "big", 0},
	} {
		header := http.Header{}
		header.Set(sessionContractHeader, tc.contract)
		header.Set(sessionChunkBytesHeader, tc.bytes)
		if got := advertisedChunkBytes(header); got != tc.want {
			t.Errorf("advertisedChunkBytes(%q, %q) = %d, want %d", tc.contract, tc.bytes, got, tc.want)
		}
	}
}

// A 200 answer to a version 3 frame that does not advertise a cap this large
// is not durable acceptance: receiver_mismatch, without retry.
func TestALargeFrameAcceptedWithoutAdvertisementIsRefused(t *testing.T) {
	spectest.Proves(t, largeFramesFeature, "unadvertised-large-acceptance-fails-closed", "a-large-frame-accepted-without-advertisement-is-refused")
	ingest := &largeIngest{advertise: 4 << 20, silentPost: true}
	cfg := newLargeIngest(t, ingest, true)
	acks := runReporter(t, cfg, frame(t, largeSession, "session.json", 0, 0, large(100<<10), false))
	requireLargeAck(t, acks[0], false, ackCodeReceiverMismatch, false, protocolcli.SessionReportingChunkBytes)
	if ingest.options != 1 || len(ingest.posts) != 1 {
		t.Fatalf("options %d, posts %v", ingest.options, ingest.posts)
	}
}

// A gateway answers 504 after the receiver stored a version 3 frame: the
// answer says nothing of the receiver's cap, so the retry posts the same
// frame again and gets an identical replay, instead of frame_unsupported,
// which would make the engine cut a sequence the receiver holds again.
func TestAGatewayAnswerKeepsTheFrameWhole(t *testing.T) {
	spectest.Proves(t, largeFramesFeature, "large-frames-only-after-an-advertising-answer", "an-unadvertised-large-frame-is-unsupported")
	ingest := &largeIngest{advertise: 4 << 20, gateway: []int{http.StatusGatewayTimeout}, gatewayStores: true, stored: map[string][][]byte{}}
	cfg := newLargeIngest(t, ingest, true)
	data := large(100 << 10)
	acks := runReporter(t, cfg,
		frame(t, largeSession, "session.json", 0, 0, "{}", false),
		frame(t, largeSession, "session.json", 2, 1, data, false),
		frame(t, largeSession, "session.json", 2, 1, data, false),
	)
	requireLargeAck(t, acks[1], false, ackCodeReceiverUnavailable, true, 4<<20)
	requireLargeAck(t, acks[2], true, "", false, 4<<20)
}

// After an attempt without a decisive answer, a receiver that no longer
// advertises the cap gets a retryable receiver_unavailable for that frame,
// never frame_unsupported: it may hold the frame.
func TestAnUnsureFrameIsNeverUnsupported(t *testing.T) {
	spectest.Proves(t, largeFramesFeature, "large-frames-only-after-an-advertising-answer", "an-unadvertised-large-frame-is-unsupported")
	ingest := &largeIngest{advertise: 4 << 20, gateway: []int{http.StatusServiceUnavailable}, gatewayStores: true, stored: map[string][][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(sessionSeqHeader) == "2" {
			// The receiver was rolled back: it advertises nothing from now on.
			ingest.mu.Lock()
			ingest.advertise = 0
			ingest.mu.Unlock()
		}
		ingest.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	cfg := sessionReporterConfig{ingestURL: srv.URL, token: "run-secret", framing: true}
	data := large(100 << 10)
	acks := runReporter(t, cfg,
		frame(t, largeSession, "session.json", 0, 0, "{}", false),
		frame(t, largeSession, "events.jsonl", 0, 0, data, false),
		frame(t, largeSession, "session.json", 2, 2, "{}", false),
		frame(t, largeSession, "events.jsonl", 0, 0, data, false),
		frame(t, largeSession, "events.jsonl", 0, 1, data, false),
	)
	requireLargeAck(t, acks[1], false, ackCodeReceiverUnavailable, true, 4<<20)
	requireLargeAck(t, acks[3], false, ackCodeReceiverUnavailable, true, protocolcli.SessionReportingChunkBytes)
	// Another frame, never attempted, is unsupported as before.
	requireLargeAck(t, acks[4], false, ackCodeFrameUnsupported, false, protocolcli.SessionReportingChunkBytes)
}

// The probe's answer advertises gzip too: the version 3 frame that follows it
// goes compressed.
func TestTheProbeLetsTheNextFrameGoCompressed(t *testing.T) {
	spectest.Proves(t, largeFramesFeature, "large-frames-only-after-an-advertising-answer", "an-unanswered-destination-is-probed-once")
	ingest := &largeIngest{advertise: 4 << 20}
	cfg := newLargeIngest(t, ingest, true)
	acks := runReporter(t, cfg, frame(t, largeSession, "session.json", 0, 0, large(100<<10), false))
	requireLargeAck(t, acks[0], true, "", false, 4<<20)
	if ingest.options != 1 || strings.Join(ingest.encodings, ",") != gzipCoding {
		t.Fatalf("options %d, encodings %q: want one probe, then a gzip POST", ingest.options, ingest.encodings)
	}
}

// The log reporter refuses a version 3 frame: its route never agreed to one.
func TestTheLogReporterRefusesALargeFrame(t *testing.T) {
	spectest.Proves(t, largeFramesFeature, "large-frames-only-after-an-advertising-answer", "an-unadvertised-large-frame-is-unsupported")
	ingest := &largeIngest{advertise: 4 << 20}
	cfg := newLargeIngest(t, ingest, true)
	var out strings.Builder
	in := strings.NewReader(frame(t, largeSession, "events.jsonl", 0, 0, large(100<<10), false))
	if err := runLogReporter(context.Background(), in, &out, io.Discard, cfg, http.DefaultClient); err != nil {
		t.Fatal(err)
	}
	ack, err := protocolcli.ParseSessionReportingAck([]byte(strings.TrimSpace(out.String())))
	if err != nil || ack.OK || ack.Code != ackCodeInvalidChunk || len(ingest.posts) != 0 {
		t.Fatalf("log reporter answered %+v (%v), posts %v", ack, err, ingest.posts)
	}
}
