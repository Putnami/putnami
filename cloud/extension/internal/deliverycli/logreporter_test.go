package deliverycli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
)

// fakeLogIngest is the receiver contract the log reporter is written against
// (the delivery API's run-log ingest): a dense sequence, the offset and the
// digest verified, a proven replay answered applied:false, a contradicting one
// refused without expectedSeq, a forward gap refused with expectedSeq, the
// bearer required.
type fakeLogIngest struct {
	mu     sync.Mutex
	token  string
	chunks []storedChunk
	bytes  int64
	posts  []string
	status int // when nonzero, every request answers this status with an empty body
}

func (f *fakeLogIngest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, r.Header.Get(runLogSeqHeader)+"/"+r.Header.Get(runLogOffsetHeader)+"/"+r.Header.Get(runLogFinalHeader))
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	if r.URL.Path != "/ingest/logs" || r.Method != http.MethodPost {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	seq, seqErr := strconv.Atoi(r.Header.Get(runLogSeqHeader))
	offset, offsetErr := strconv.ParseInt(r.Header.Get(runLogOffsetHeader), 10, 64)
	final := r.Header.Get(runLogFinalHeader) == "true"
	body, _ := io.ReadAll(r.Body)
	sum := sha256.Sum256(body)
	if seqErr != nil || offsetErr != nil || hex.EncodeToString(sum[:]) != r.Header.Get(runLogSHA256Header) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	conflict := func(extra map[string]any) {
		w.WriteHeader(http.StatusConflict)
		answer := map[string]any{"error": "conflict"}
		for k, v := range extra {
			answer[k] = v
		}
		_ = json.NewEncoder(w).Encode(answer)
	}
	applied := true
	switch {
	case seq < len(f.chunks):
		if !bytes.Equal(f.chunks[seq].body, body) || f.chunks[seq].final != final {
			conflict(nil)
			return
		}
		applied = false
	case seq > len(f.chunks):
		conflict(map[string]any{"expectedSeq": len(f.chunks)})
		return
	default:
		if len(f.chunks) > 0 && f.chunks[len(f.chunks)-1].final {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if offset != f.bytes {
			conflict(nil)
			return
		}
		f.chunks = append(f.chunks, storedChunk{body: body, final: final})
		f.bytes += int64(len(body))
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"runId": "run-1", "seq": seq, "bytes": len(body), "final": final, "applied": applied})
}

func (f *fakeLogIngest) stored() string {
	var all strings.Builder
	for _, chunk := range f.chunks {
		all.Write(chunk.body)
	}
	return all.String()
}

func runLogReporterFrames(t *testing.T, cfg sessionReporterConfig, frames ...string) []protocolcli.SessionReportingAck {
	t.Helper()
	var out bytes.Buffer
	client := &http.Client{Timeout: sessionReporterRequestTimeout}
	if err := runLogReporter(context.Background(), strings.NewReader(strings.Join(frames, "")), &out, io.Discard, cfg, client); err != nil {
		t.Fatalf("log reporter: %v", err)
	}
	lines := bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n"))
	acks := make([]protocolcli.SessionReportingAck, 0, len(lines))
	for _, line := range lines {
		ack, err := protocolcli.ParseSessionReportingAck(line)
		if err != nil {
			t.Fatalf("ack %q: %v", line, err)
		}
		acks = append(acks, *ack)
	}
	return acks
}

const logSession = "20261001-110000-abc123"

// One frame is one chunk of the run's log, stored verbatim. A frame may end
// inside a line; the reporter does not look at the bytes.
func TestLogReporter_StreamsTheEventFileAndAcksEachFrame(t *testing.T) {
	ingest := &fakeLogIngest{token: "run-secret"}
	srv := httptest.NewServer(ingest)
	defer srv.Close()
	cfg := sessionReporterConfig{ingestURL: srv.URL + "/ingest/", token: "run-secret"}

	first, second := `{"record":"task:start"}`+"\n"+`{"record":"task:ev`, `ent"}`+"\n"
	acks := runLogReporterFrames(t, cfg,
		frame(t, logSession, "events.jsonl", 0, 0, first, false),
		frame(t, logSession, "events.jsonl", int64(len(first)), 1, second, false),
		frame(t, logSession, "events.jsonl", int64(len(first)+len(second)), 2, "", true),
	)
	if len(acks) != 3 {
		t.Fatalf("acks = %d, want 3", len(acks))
	}
	for i, ack := range acks {
		if !ack.OK || ack.Code != "" || ack.Retryable {
			t.Fatalf("ack %d = %+v, want ok", i, ack)
		}
	}
	if !acks[2].Final || acks[2].Sequence != 2 || acks[2].Offset != int64(len(first)+len(second)) {
		t.Fatalf("final ack echoes the wrong identity: %+v", acks[2])
	}
	want := []string{"0/0/false", "1/" + strconv.Itoa(len(first)) + "/false", "2/" + strconv.Itoa(len(first)+len(second)) + "/true"}
	if strings.Join(ingest.posts, ",") != strings.Join(want, ",") {
		t.Fatalf("posts = %v, want %v", ingest.posts, want)
	}
	if ingest.stored() != first+second {
		t.Fatalf("stored bytes = %q, want the event file verbatim", ingest.stored())
	}
	if !ingest.chunks[2].final {
		t.Fatal("the final marker was not stored as final")
	}
}

// The engine resends the frame whose acknowledgement it lost. The receiver
// proves the bytes and answers applied:false, which is acceptance: the log holds
// the line once.
func TestLogReporter_ALostAckResendIsAcceptedOnce(t *testing.T) {
	ingest := &fakeLogIngest{token: "run-secret"}
	srv := httptest.NewServer(ingest)
	defer srv.Close()
	cfg := sessionReporterConfig{ingestURL: srv.URL + "/ingest", token: "run-secret"}

	acks := runLogReporterFrames(t, cfg,
		frame(t, logSession, "events.jsonl", 0, 0, "line one\n", false),
		frame(t, logSession, "events.jsonl", 0, 0, "line one\n", false),
		frame(t, logSession, "events.jsonl", 9, 1, "line two\n", false),
	)
	for i, ack := range acks {
		if !ack.OK {
			t.Fatalf("ack %d = %+v, want ok", i, ack)
		}
	}
	if ingest.stored() != "line one\nline two\n" {
		t.Fatalf("stored bytes = %q, want each line once", ingest.stored())
	}
}

// Bytes that contradict the committed log are never spliced in, and the engine
// is told not to retry.
func TestLogReporter_ConflictAndGapAreTerminal(t *testing.T) {
	ingest := &fakeLogIngest{token: "run-secret"}
	srv := httptest.NewServer(ingest)
	defer srv.Close()
	cfg := sessionReporterConfig{ingestURL: srv.URL + "/ingest", token: "run-secret"}

	acks := runLogReporterFrames(t, cfg,
		frame(t, logSession, "events.jsonl", 0, 0, "line one\n", false),
		frame(t, logSession, "events.jsonl", 0, 0, "other one\n", false), // a committed sequence, other bytes
		frame(t, logSession, "events.jsonl", 40, 1, "line two\n", false), // does not continue the committed bytes
		frame(t, logSession, "events.jsonl", 9, 3, "line four\n", false), // forward gap
	)
	if !acks[0].OK {
		t.Fatalf("first append = %+v", acks[0])
	}
	for i, want := range map[int]string{1: ackCodeReplayConflict, 2: ackCodeReplayConflict, 3: ackCodeSequenceGap} {
		if acks[i].OK || acks[i].Code != want || acks[i].Retryable {
			t.Fatalf("ack %d = %+v, want terminal %s", i, acks[i], want)
		}
	}
	if ingest.stored() != "line one\n" {
		t.Fatalf("stored bytes = %q, a refused frame must not land", ingest.stored())
	}
}

func TestLogReporter_ReceiverOutageIsRetryableRefusalsAreNot(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		code      string
		retryable bool
	}{
		{"bad gateway", http.StatusBadGateway, ackCodeReceiverUnavailable, true},
		{"ingest not configured", http.StatusInternalServerError, ackCodeReceiverUnavailable, true},
		{"not a run credential", http.StatusUnauthorized, ackCodeUnauthorized, false},
		{"workspace token", http.StatusForbidden, ackCodeUnauthorized, false},
		{"dormant route", http.StatusNotFound, ackCodeIngestUnavailable, false},
		{"no instance free", http.StatusTooManyRequests, ackCodeReceiverUnavailable, true},
		{"request timed out upstream", http.StatusRequestTimeout, ackCodeReceiverUnavailable, true},
		{"too large", http.StatusRequestEntityTooLarge, ackCodeChunkTooLarge, false},
		{"run past its log caps", http.StatusBadRequest, ackCodeInvalidChunk, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(&fakeLogIngest{status: tc.status})
			defer srv.Close()
			acks := runLogReporterFrames(t, sessionReporterConfig{ingestURL: srv.URL, token: "x"},
				frame(t, logSession, "events.jsonl", 0, 0, "line\n", false))
			if acks[0].OK || acks[0].Code != tc.code || acks[0].Retryable != tc.retryable {
				t.Fatalf("ack = %+v, want code %s retryable %v", acks[0], tc.code, tc.retryable)
			}
			if !acks[0].Matches(*mustChunk(t, logSession, "events.jsonl", 0, 0, "line\n", false)) {
				t.Fatalf("nack must echo the frame identity: %+v", acks[0])
			}
		})
	}
}

func TestLogReporter_TransportFailureIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	var out bytes.Buffer
	in := frame(t, logSession, "events.jsonl", 0, 0, "line\n", false)
	cfg := sessionReporterConfig{ingestURL: srv.URL, token: "x"}
	if err := runLogReporter(context.Background(), strings.NewReader(in), &out, io.Discard, cfg, &http.Client{Timeout: 50 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	ack, err := protocolcli.ParseSessionReportingAck(bytes.TrimSpace(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if ack.OK || ack.Code != ackCodeReceiverUnavailable || !ack.Retryable {
		t.Fatalf("ack = %+v", ack)
	}
}

func TestLogReporter_AcceptanceMustNameTheFrame(t *testing.T) {
	for name, answer := range map[string]map[string]any{
		"another sequence": {"seq": 7, "applied": true},
		"no sequence":      {"applied": true},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(answer)
			}))
			defer srv.Close()
			acks := runLogReporterFrames(t, sessionReporterConfig{ingestURL: srv.URL, token: "x"},
				frame(t, logSession, "events.jsonl", 0, 0, "line\n", false))
			if acks[0].OK || acks[0].Code != ackCodeReceiverMismatch || acks[0].Retryable {
				t.Fatalf("ack = %+v", acks[0])
			}
		})
	}
}

// The log is the event stream alone, from one session, and only when the
// launcher gave the reporter a destination and a credential.
func TestLogReporter_RefusesWhatIsNotThisRunsEventStream(t *testing.T) {
	ingest := &fakeLogIngest{token: "run-secret"}
	srv := httptest.NewServer(ingest)
	defer srv.Close()
	cfg := sessionReporterConfig{ingestURL: srv.URL + "/ingest", token: "run-secret"}

	acks := runLogReporterFrames(t, cfg,
		frame(t, logSession, "events.jsonl", 0, 0, "a\n", false),
		frame(t, "20261001-110000-ffffff", "events.jsonl", 0, 0, "a\n", false),
		frame(t, logSession, "session.json", 0, 0, "{}", false),
	)
	if !acks[0].OK {
		t.Fatalf("first frame = %+v", acks[0])
	}
	if acks[1].OK || acks[1].Code != ackCodeSessionMismatch || acks[1].Retryable {
		t.Fatalf("foreign session frame = %+v", acks[1])
	}
	if acks[2].OK || acks[2].Code != ackCodeInvalidChunk || acks[2].Retryable {
		t.Fatalf("session.json frame = %+v", acks[2])
	}
	if len(ingest.posts) != 1 {
		t.Fatalf("only the run's event stream may reach the receiver: %v", ingest.posts)
	}

	for _, cfg := range []sessionReporterConfig{{ingestURL: srv.URL}, {token: "run-secret"}} {
		acks := runLogReporterFrames(t, cfg, frame(t, logSession, "events.jsonl", 0, 0, "a\n", false))
		if acks[0].OK || acks[0].Code != ackCodeNotConfigured || acks[0].Retryable {
			t.Fatalf("unconfigured reporter ack = %+v", acks[0])
		}
	}
	if len(ingest.posts) != 1 {
		t.Fatalf("an unconfigured reporter must not POST: %v", ingest.posts)
	}
}

func TestLogReporter_MalformedFrameStopsTheProvider(t *testing.T) {
	var out, diagnostics bytes.Buffer
	err := runLogReporter(context.Background(), strings.NewReader("{\"protocolVersion\":1}\n"), &out, &diagnostics,
		sessionReporterConfig{ingestURL: "http://127.0.0.1:1", token: "x"}, &http.Client{Timeout: time.Second})
	if err == nil {
		t.Fatal("a malformed frame must fail the provider: it has no identity to acknowledge")
	}
	if out.Len() != 0 {
		t.Fatalf("no ack may be written for a malformed frame: %q", out.String())
	}
	if !strings.Contains(diagnostics.String(), "putnami-cloud log-reporter:") {
		t.Fatalf("the diagnostic must name the provider: %q", diagnostics.String())
	}
}
