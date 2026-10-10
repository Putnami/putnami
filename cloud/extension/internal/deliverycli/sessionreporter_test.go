package deliverycli

import (
	"bytes"
	"context"
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

// fakeIngest is the receiver contract the reporter is written against: a dense
// per-part sequence, identical replay accepted, conflicting replay refused
// without expectedSeq, forward gap refused with expectedSeq, bearer required.
type fakeIngest struct {
	mu     sync.Mutex
	token  string
	parts  map[string][]storedChunk
	posts  []string
	status int // when nonzero, every request answers this status with an empty body
	// acceptPlan makes it a Delivery that knows the plan part; without
	// it, it answers that part 400, as a Delivery that predates it does.
	acceptPlan bool
}

type storedChunk struct {
	body  []byte
	final bool
}

func (f *fakeIngest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, r.Header.Get(sessionPartHeader)+"/"+r.Header.Get(sessionSeqHeader)+"/"+r.Header.Get(sessionFinalHeader))
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	if r.URL.Path != "/ingest/sessions" || r.Method != http.MethodPost {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	part := r.Header.Get(sessionPartHeader)
	seq, err := strconv.Atoi(r.Header.Get(sessionSeqHeader))
	if err != nil || (part != "session" && part != "events" && (part != "plan" || !f.acceptPlan)) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	final := r.Header.Get(sessionFinalHeader) == "true"
	body, _ := io.ReadAll(r.Body)
	if f.parts == nil {
		f.parts = map[string][]storedChunk{}
	}
	stored := f.parts[part]
	applied := true
	switch {
	case seq < len(stored):
		if !bytes.Equal(stored[seq].body, body) || stored[seq].final != final {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "replay conflict"})
			return
		}
		applied = false
	case seq > len(stored):
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "gap", "expectedSeq": len(stored)})
		return
	default:
		if len(stored) > 0 && stored[len(stored)-1].final {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.parts[part] = append(stored, storedChunk{body: body, final: final})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"part": part, "seq": seq, "final": final, "applied": applied})
}

func frame(t *testing.T, session, artifact string, offset, seq int64, data string, final bool) string {
	t.Helper()
	chunk := protocolcli.NewSessionReportingChunk(session, artifact, offset, seq, []byte(data), final)
	if err := chunk.Validate(); err != nil {
		t.Fatalf("fixture chunk: %v", err)
	}
	encoded, err := json.Marshal(chunk)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded) + "\n"
}

func runReporter(t *testing.T, cfg sessionReporterConfig, frames ...string) []protocolcli.SessionReportingAck {
	t.Helper()
	var out bytes.Buffer
	client := &http.Client{Timeout: sessionReporterRequestTimeout}
	if err := runSessionReporter(context.Background(), strings.NewReader(strings.Join(frames, "")), &out, io.Discard, cfg, client); err != nil {
		t.Fatalf("reporter: %v", err)
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

func TestSessionReporter_StreamsBothPartsAndAcksEachFrame(t *testing.T) {
	ingest := &fakeIngest{token: "run-secret"}
	srv := httptest.NewServer(http.StripPrefix("", ingest))
	defer srv.Close()
	cfg := sessionReporterConfig{ingestURL: srv.URL + "/ingest/", token: "run-secret"}

	const session = "20260915-110000-abc123"
	acks := runReporter(t, cfg,
		frame(t, session, "events.jsonl", 0, 0, `{"record":"task:start"}`+"\n", false),
		frame(t, session, "events.jsonl", 24, 1, `{"record":"task:end"}`+"\n", false),
		frame(t, session, "session.json", 0, 0, `{"run":{"outcome":"success"}}`, false),
		frame(t, session, "session.json", 29, 1, "", true),
		frame(t, session, "events.jsonl", 46, 2, "", true),
	)
	if len(acks) != 5 {
		t.Fatalf("acks = %d, want 5", len(acks))
	}
	for i, ack := range acks {
		if !ack.OK || ack.Code != "" || ack.Retryable {
			t.Fatalf("ack %d = %+v, want ok", i, ack)
		}
	}
	if acks[3].Artifact != "session.json" || !acks[3].Final || acks[3].Sequence != 1 || acks[3].Offset != 29 {
		t.Fatalf("session final ack echoes wrong identity: %+v", acks[3])
	}
	want := []string{"events/0/false", "events/1/false", "session/0/false", "session/1/true", "events/2/true"}
	if strings.Join(ingest.posts, ",") != strings.Join(want, ",") {
		t.Fatalf("posts = %v, want %v", ingest.posts, want)
	}
	if got := string(ingest.parts["events"][0].body) + string(ingest.parts["events"][1].body); got != `{"record":"task:start"}`+"\n"+`{"record":"task:end"}`+"\n" {
		t.Fatalf("stored events bytes = %q", got)
	}
	if !ingest.parts["session"][1].final || !ingest.parts["events"][2].final {
		t.Fatal("final markers were not stored as final")
	}
}

func TestSessionReporter_IdenticalReplayIsOKConflictIsTerminal(t *testing.T) {
	ingest := &fakeIngest{token: "run-secret"}
	srv := httptest.NewServer(ingest)
	defer srv.Close()
	cfg := sessionReporterConfig{ingestURL: srv.URL + "/ingest", token: "run-secret"}
	const session = "20260915-110000-abc123"

	acks := runReporter(t, cfg,
		frame(t, session, "session.json", 0, 0, `{"a":1}`, false),
		frame(t, session, "session.json", 0, 0, `{"a":1}`, false), // identical replay
		frame(t, session, "session.json", 0, 0, `{"a":2}`, false), // conflicting replay
		frame(t, session, "session.json", 7, 3, "", true),         // forward gap
	)
	if !acks[0].OK || !acks[1].OK {
		t.Fatalf("first append / identical replay must be OK: %+v %+v", acks[0], acks[1])
	}
	if acks[2].OK || acks[2].Code != ackCodeReplayConflict || acks[2].Retryable {
		t.Fatalf("conflicting replay ack = %+v", acks[2])
	}
	if acks[3].OK || acks[3].Code != ackCodeSequenceGap || acks[3].Retryable {
		t.Fatalf("gap ack = %+v", acks[3])
	}
}

func TestSessionReporter_ReceiverOutageIsRetryableAuthIsNot(t *testing.T) {
	const session = "20260915-110000-abc123"
	cases := []struct {
		name      string
		status    int
		code      string
		retryable bool
	}{
		{"bad gateway", http.StatusBadGateway, ackCodeReceiverUnavailable, true},
		{"service unavailable", http.StatusServiceUnavailable, ackCodeReceiverUnavailable, true},
		{"unauthorized", http.StatusUnauthorized, ackCodeUnauthorized, false},
		{"forbidden", http.StatusForbidden, ackCodeUnauthorized, false},
		{"dormant route", http.StatusNotFound, ackCodeIngestUnavailable, false},
		{"no instance free", http.StatusTooManyRequests, ackCodeReceiverUnavailable, true},
		{"request timed out upstream", http.StatusRequestTimeout, ackCodeReceiverUnavailable, true},
		{"too large", http.StatusRequestEntityTooLarge, ackCodeChunkTooLarge, false},
		{"bad request", http.StatusBadRequest, ackCodeInvalidChunk, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(&fakeIngest{status: tc.status})
			defer srv.Close()
			acks := runReporter(t, sessionReporterConfig{ingestURL: srv.URL, token: "x"},
				frame(t, session, "events.jsonl", 0, 0, "line\n", false))
			if acks[0].OK || acks[0].Code != tc.code || acks[0].Retryable != tc.retryable {
				t.Fatalf("ack = %+v, want code %s retryable %v", acks[0], tc.code, tc.retryable)
			}
			if !acks[0].Matches(*mustChunk(t, session, "events.jsonl", 0, 0, "line\n", false)) {
				t.Fatalf("nack must echo the frame identity: %+v", acks[0])
			}
		})
	}
}

// shedFirstIngest answers the first shed requests with status, as the platform
// does when no instance is free (429) or a request times out upstream (408),
// then serves the receiver contract.
type shedFirstIngest struct {
	*fakeIngest
	status int
	shed   int
	mu     sync.Mutex
	posts  int
}

func (s *shedFirstIngest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.posts++
	shed := s.posts <= s.shed
	s.mu.Unlock()
	if shed {
		w.WriteHeader(s.status)
		return
	}
	s.fakeIngest.ServeHTTP(w, r)
}

func (s *shedFirstIngest) postCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.posts
}

// A frame the platform shed lands when the engine resends it, and the reporter
// never retries on its own: every engine attempt is exactly one POST, so the
// engine's attempt bound is the only bound. A frame shed on every attempt the
// engine makes is never stored, and the reporter answers each attempt with the
// same retryable NACK.
func TestSessionReporter_APlatformShedFrameLandsOnTheEngineResend(t *testing.T) {
	const session = "20260915-110000-abc123"
	for _, status := range []int{http.StatusTooManyRequests, http.StatusRequestTimeout} {
		t.Run(strconv.Itoa(status)+" then accepted", func(t *testing.T) {
			ingest := &shedFirstIngest{fakeIngest: &fakeIngest{token: "tok"}, status: status, shed: 2}
			srv := httptest.NewServer(ingest)
			defer srv.Close()
			attempt := frame(t, session, "events.jsonl", 0, 0, "line\n", false)
			acks := runReporter(t, sessionReporterConfig{ingestURL: srv.URL + "/ingest", token: "tok"},
				attempt, attempt, attempt)
			for i := 0; i < 2; i++ {
				if acks[i].OK || acks[i].Code != ackCodeReceiverUnavailable || !acks[i].Retryable {
					t.Fatalf("attempt %d ack = %+v, want a retryable receiver_unavailable NACK", i+1, acks[i])
				}
			}
			if !acks[2].OK || acks[2].Retryable {
				t.Fatalf("resend ack = %+v, want the frame accepted", acks[2])
			}
			if got := ingest.postCount(); got != 3 {
				t.Fatalf("POSTs = %d for three engine attempts, want one per attempt", got)
			}
			if stored := ingest.parts["events"]; len(stored) != 1 || string(stored[0].body) != "line\n" {
				t.Fatalf("stored = %+v, want the frame stored once", stored)
			}
		})
		t.Run(strconv.Itoa(status)+" on every attempt", func(t *testing.T) {
			ingest := &shedFirstIngest{fakeIngest: &fakeIngest{token: "tok"}, status: status, shed: 1 << 30}
			srv := httptest.NewServer(ingest)
			defer srv.Close()
			const engineAttempts = 3
			attempts := make([]string, engineAttempts)
			for i := range attempts {
				attempts[i] = frame(t, session, "session.json", 0, 0, "{}", false)
			}
			acks := runReporter(t, sessionReporterConfig{ingestURL: srv.URL + "/ingest", token: "tok"}, attempts...)
			if len(acks) != engineAttempts {
				t.Fatalf("acks = %d, want one per engine attempt", len(acks))
			}
			for i, ack := range acks {
				if ack.OK || ack.Code != ackCodeReceiverUnavailable || !ack.Retryable {
					t.Fatalf("attempt %d ack = %+v, want a retryable receiver_unavailable NACK", i+1, ack)
				}
			}
			if got := ingest.postCount(); got != engineAttempts {
				t.Fatalf("POSTs = %d, want exactly one per engine attempt and no retry of the reporter's own", got)
			}
			if len(ingest.parts) != 0 {
				t.Fatalf("stored = %+v, want nothing stored from a shed frame", ingest.parts)
			}
		})
	}
}

func TestSessionReporter_TransportFailureIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	var out bytes.Buffer
	client := &http.Client{Timeout: 50 * time.Millisecond}
	cfg := sessionReporterConfig{ingestURL: srv.URL, token: "x"}
	in := frame(t, "20260915-110000-abc123", "events.jsonl", 0, 0, "line\n", false)
	if err := runSessionReporter(context.Background(), strings.NewReader(in), &out, io.Discard, cfg, client); err != nil {
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

func TestSessionReporter_AcceptanceMustNameTheFrame(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"part": "events", "seq": 7, "applied": true})
	}))
	defer srv.Close()
	acks := runReporter(t, sessionReporterConfig{ingestURL: srv.URL, token: "x"},
		frame(t, "20260915-110000-abc123", "events.jsonl", 0, 0, "line\n", false))
	if acks[0].OK || acks[0].Code != ackCodeReceiverMismatch || acks[0].Retryable {
		t.Fatalf("ack = %+v", acks[0])
	}
}

func TestSessionReporter_BindsOneSessionAndRequiresConfiguration(t *testing.T) {
	ingest := &fakeIngest{token: "run-secret"}
	srv := httptest.NewServer(ingest)
	defer srv.Close()
	cfg := sessionReporterConfig{ingestURL: srv.URL + "/ingest", token: "run-secret"}
	acks := runReporter(t, cfg,
		frame(t, "20260915-110000-abc123", "events.jsonl", 0, 0, "a\n", false),
		frame(t, "20260915-110000-ffffff", "events.jsonl", 0, 0, "a\n", false),
	)
	if !acks[0].OK {
		t.Fatalf("first session frame = %+v", acks[0])
	}
	if acks[1].OK || acks[1].Code != ackCodeSessionMismatch || acks[1].Retryable {
		t.Fatalf("second session frame = %+v", acks[1])
	}
	if len(ingest.posts) != 1 {
		t.Fatalf("a foreign session must never reach the receiver: %v", ingest.posts)
	}

	for _, cfg := range []sessionReporterConfig{{ingestURL: srv.URL}, {token: "run-secret"}} {
		acks := runReporter(t, cfg, frame(t, "20260915-110000-abc123", "session.json", 0, 0, "{}", false))
		if acks[0].OK || acks[0].Code != ackCodeNotConfigured || acks[0].Retryable {
			t.Fatalf("unconfigured reporter ack = %+v", acks[0])
		}
	}
	if len(ingest.posts) != 1 {
		t.Fatalf("an unconfigured reporter must not POST: %v", ingest.posts)
	}
}

func TestSessionReporter_MalformedFrameStopsTheProvider(t *testing.T) {
	var out bytes.Buffer
	err := runSessionReporter(context.Background(), strings.NewReader("{\"protocolVersion\":1}\n"), &out, io.Discard,
		sessionReporterConfig{ingestURL: "http://127.0.0.1:1", token: "x"}, &http.Client{Timeout: time.Second})
	if err == nil {
		t.Fatal("malformed frame must fail the provider: it has no identity to acknowledge")
	}
	if out.Len() != 0 {
		t.Fatalf("no ack may be written for a malformed frame: %q", out.String())
	}
}

func mustChunk(t *testing.T, session, artifact string, offset, seq int64, data string, final bool) *protocolcli.SessionReportingChunk {
	t.Helper()
	chunk := protocolcli.NewSessionReportingChunk(session, artifact, offset, seq, []byte(data), final)
	return &chunk
}
