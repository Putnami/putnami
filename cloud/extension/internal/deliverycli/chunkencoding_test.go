package deliverycli

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

const compressedFeature = "cloud/compressed-session-reporting"

// receivedChunkBody reads r's body as a receiver does. One that reads gzip
// decodes a gzip body; one that does not keeps every body as it arrived. It
// reports false for a gzip body that does not decode.
func receivedChunkBody(r *http.Request, readsGzip bool) ([]byte, bool) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, false
	}
	if !readsGzip || r.Header.Get(contentEncodingHeader) != gzipCoding || len(body) == 0 {
		return body, true
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, false
	}
	decoded, err := io.ReadAll(zr)
	if err != nil {
		return nil, false
	}
	return decoded, true
}

// eventLines is a compressible events chunk of n JSONL records.
func eventLines(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(`{"protocolVersion":2,"record":"task:event","identity":{"key":"/a:test~test"},"event":{"level":"debug","message":"=== RUN   TestSomething","type":"log"}}` + "\n")
	}
	return b.String()
}

// incompressible is n bytes that gzip does not shrink: a SHA-256 chain.
func incompressible(n int) string {
	out := make([]byte, 0, n+sha256.Size)
	sum := sha256.Sum256([]byte("seed"))
	for len(out) < n {
		out = append(out, sum[:]...)
		sum = sha256.Sum256(sum[:])
	}
	return string(out[:n])
}

func requireEncodings(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Content-Encoding of each request = %q, want %q", got, want)
	}
}

func requireOK(t *testing.T, acks []protocolcli.SessionReportingAck) {
	t.Helper()
	for i, ack := range acks {
		if !ack.OK || ack.Code != "" || ack.Retryable {
			t.Fatalf("ack %d = %+v, want ok", i, ack)
		}
	}
}

func TestAdvertisesGzip(t *testing.T) {
	cases := []struct {
		values []string
		want   bool
	}{
		{[]string{"gzip"}, true},
		{[]string{"GZIP"}, true},
		{[]string{"identity, gzip"}, true},
		{[]string{"br", "gzip;q=0.5"}, true},
		{[]string{"gzip; q=1"}, true},
		{nil, false},
		{[]string{""}, false},
		{[]string{"identity"}, false},
		{[]string{"x-gzip"}, false},
		{[]string{"*"}, false},
		{[]string{"gzip;q=0"}, false},
		{[]string{"gzip;q=0.000"}, false},
		{[]string{"gzip;q=nope"}, false},
		{[]string{"gzip;q=2"}, false},
	}
	for _, tc := range cases {
		header := http.Header{}
		for _, value := range tc.values {
			header.Add(acceptEncodingHeader, value)
		}
		if got := advertisesGzip(header); got != tc.want {
			t.Errorf("advertisesGzip(%q) = %v, want %v", tc.values, got, tc.want)
		}
	}
}

// The first chunk goes as is; the receiver's answer advertises gzip, so the
// next one goes compressed, and the receiver stores its decoded bytes. The
// zero-byte final marker goes as is and stays empty.
func TestSessionReporterCompressesOnceTheReceiverAdvertisesGzip(t *testing.T) {
	spectest.Proves(t, compressedFeature, "gzip-only-after-an-advertising-answer", "gzip-follows-an-advertising-answer")
	ingest := &fakeIngest{token: "run-secret", gzip: true}
	srv := httptest.NewServer(ingest)
	defer srv.Close()
	cfg := sessionReporterConfig{ingestURL: srv.URL + "/ingest", token: "run-secret"}

	const session = "20260915-110000-abc123"
	first, second := eventLines(20), eventLines(40)
	acks := runReporter(t, cfg,
		frame(t, session, "events.jsonl", 0, 0, first, false),
		frame(t, session, "events.jsonl", int64(len(first)), 1, second, false),
		frame(t, session, "events.jsonl", int64(len(first)+len(second)), 2, "", true),
	)
	requireOK(t, acks)
	requireEncodings(t, ingest.encodings, "", gzipCoding, "")
	stored := ingest.parts["events"]
	if len(stored) != 3 || string(stored[0].body) != first || string(stored[1].body) != second {
		t.Fatalf("stored events = %d chunks, want the frames' own bytes", len(stored))
	}
	if len(stored[2].body) != 0 || !stored[2].final {
		t.Fatalf("final marker stored as %q final=%v, want an empty final chunk", stored[2].body, stored[2].final)
	}
}

// A receiver that does not advertise gzip stores every body as it arrives,
// so the reporter never compresses for it.
func TestSessionReporterNeverCompressesForAReceiverThatDoesNotAdvertiseGzip(t *testing.T) {
	spectest.Proves(t, compressedFeature, "gzip-only-after-an-advertising-answer", "no-gzip-without-an-advertising-answer")
	ingest := &fakeIngest{token: "run-secret"}
	srv := httptest.NewServer(ingest)
	defer srv.Close()
	cfg := sessionReporterConfig{ingestURL: srv.URL + "/ingest", token: "run-secret"}

	const session = "20260915-110000-abc123"
	data := eventLines(40)
	acks := runReporter(t, cfg,
		frame(t, session, "events.jsonl", 0, 0, data, false),
		frame(t, session, "events.jsonl", int64(len(data)), 1, data, false),
		frame(t, session, "session.json", 0, 0, data, false),
	)
	requireOK(t, acks)
	requireEncodings(t, ingest.encodings, "", "", "")
	if got := string(ingest.parts["events"][1].body); got != data {
		t.Fatal("a receiver that does not read gzip stored other bytes than the frame's")
	}
}

// The receiver compares a resent sequence on its decoded bytes: a frame first
// sent as is and resent compressed is the same frame, and a resend with other
// bytes is still a conflict.
func TestSessionReporterResentSequenceComparesEqualCompressedOrNot(t *testing.T) {
	spectest.Proves(t, compressedFeature, "the-chunk-bytes-are-what-the-receiver-stores", "a-resent-sequence-compares-decoded")
	ingest := &fakeIngest{token: "run-secret", gzip: true}
	srv := httptest.NewServer(ingest)
	defer srv.Close()
	cfg := sessionReporterConfig{ingestURL: srv.URL + "/ingest", token: "run-secret"}

	const session = "20260915-110000-abc123"
	data := eventLines(40)
	acks := runReporter(t, cfg,
		frame(t, session, "events.jsonl", 0, 0, data, false),
		frame(t, session, "events.jsonl", 0, 0, data, false),
		frame(t, session, "events.jsonl", 0, 0, eventLines(41), false),
	)
	requireEncodings(t, ingest.encodings, "", gzipCoding, gzipCoding)
	requireOK(t, acks[:2])
	if acks[2].OK || acks[2].Code != ackCodeReplayConflict || acks[2].Retryable {
		t.Fatalf("resend with other bytes = %+v, want a terminal replay_conflict", acks[2])
	}
	if stored := ingest.parts["events"]; len(stored) != 1 || string(stored[0].body) != data {
		t.Fatalf("stored = %d chunks, want the frame stored once with its own bytes", len(stored))
	}
}

// After the advertisement, a chunk gzip does not shrink and a chunk with no
// bytes go as is.
func TestSessionReporterSendsEmptyAndIncompressibleChunksAsIs(t *testing.T) {
	spectest.Proves(t, compressedFeature, "the-chunk-bytes-are-what-the-receiver-stores", "empty-and-incompressible-chunks-go-as-is")
	ingest := &fakeIngest{token: "run-secret", gzip: true}
	srv := httptest.NewServer(ingest)
	defer srv.Close()
	cfg := sessionReporterConfig{ingestURL: srv.URL + "/ingest", token: "run-secret"}

	const session = "20260915-110000-abc123"
	noise := incompressible(protocolcli.SessionReportingChunkBytes)
	acks := runReporter(t, cfg,
		frame(t, session, "session.json", 0, 0, "{}", false),
		frame(t, session, "session.json", 2, 1, noise, false),
		frame(t, session, "session.json", int64(2+len(noise)), 2, "", true),
	)
	requireOK(t, acks)
	requireEncodings(t, ingest.encodings, "", "", "")
	if stored := ingest.parts["session"]; len(stored) != 3 || string(stored[1].body) != noise || len(stored[2].body) != 0 {
		t.Fatal("an incompressible or empty chunk was not stored as its own bytes")
	}
}

// switchingIngest serves inner, a receiver that reads gzip, except for the
// requests status names: those get an answer of that status without the
// advertisement, as an instance that predates gzip gives, and a 200 stores
// the body as it arrived. A request drop names gets no answer at all.
type switchingIngest struct {
	t      *testing.T
	inner  http.Handler
	status map[int]int
	drop   map[int]bool

	mu       sync.Mutex
	requests int
	encoding []string
	stored   [][]byte
}

func (s *switchingIngest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	n := s.requests
	s.requests++
	s.encoding = append(s.encoding, r.Header.Get(contentEncodingHeader))
	s.mu.Unlock()
	if s.drop[n] {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			s.t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
		return
	}
	if status, old := s.status[n]; old {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(status)
		if status == http.StatusOK {
			s.mu.Lock()
			s.stored = append(s.stored, body)
			s.mu.Unlock()
			seq := r.Header.Get(sessionSeqHeader) + r.Header.Get(runLogSeqHeader)
			_, _ = io.WriteString(w, `{"part":"events","seq":`+seq+`,"applied":true}`)
		}
		return
	}
	s.inner.ServeHTTP(w, r)
}

func (s *switchingIngest) encodings() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.encoding...)
}

func (s *switchingIngest) storedBodies() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.stored...)
}

// Every answer is read: one without the advertisement, or no answer at all,
// sends the next chunk as is, and the next advertising answer turns gzip back
// on.
func TestSessionReporterStopsCompressingAfterAnAnswerWithoutTheAdvertisement(t *testing.T) {
	spectest.Proves(t, compressedFeature, "gzip-only-after-an-advertising-answer", "no-gzip-without-an-advertising-answer")
	receiver := &fakeIngest{token: "run-secret", gzip: true}
	ingest := &switchingIngest{t: t, inner: receiver,
		status: map[int]int{1: http.StatusServiceUnavailable}, drop: map[int]bool{4: true}}
	srv := httptest.NewServer(ingest)
	defer srv.Close()
	cfg := sessionReporterConfig{ingestURL: srv.URL + "/ingest", token: "run-secret"}

	const session = "20260915-110000-abc123"
	a, b, c := eventLines(20), eventLines(30), eventLines(40)
	acks := runReporter(t, cfg,
		frame(t, session, "events.jsonl", 0, 0, a, false),
		frame(t, session, "events.jsonl", int64(len(a)), 1, b, false), // old instance: 503
		frame(t, session, "events.jsonl", int64(len(a)), 1, b, false), // resend, as is
		frame(t, session, "events.jsonl", int64(len(a)+len(b)), 2, c, false),
		frame(t, session, "events.jsonl", int64(len(a)+len(b)+len(c)), 3, c, false), // no answer
		frame(t, session, "events.jsonl", int64(len(a)+len(b)+len(c)), 3, c, false), // resend, as is
	)
	requireEncodings(t, ingest.encodings(), "", gzipCoding, "", gzipCoding, gzipCoding, "")
	if acks[1].OK || acks[1].Code != ackCodeReceiverUnavailable || !acks[1].Retryable {
		t.Fatalf("503 ack = %+v, want a retryable receiver_unavailable", acks[1])
	}
	if acks[4].OK || acks[4].Code != ackCodeReceiverUnavailable || !acks[4].Retryable {
		t.Fatalf("dropped request ack = %+v, want a retryable receiver_unavailable", acks[4])
	}
	requireOK(t, []protocolcli.SessionReportingAck{acks[0], acks[2], acks[3], acks[5]})
	stored := receiver.parts["events"]
	if len(stored) != 4 || string(stored[1].body) != b || string(stored[2].body) != c || string(stored[3].body) != c {
		t.Fatalf("stored = %d chunks, want each frame's own bytes", len(stored))
	}
}

// A 200 to a gzip body that does not advertise gzip comes from a receiver
// that may have stored the compressed bytes: the frame is not acknowledged.
func TestSessionReporterRefusesAGzipChunkAcceptedWithoutTheAdvertisement(t *testing.T) {
	spectest.Proves(t, compressedFeature, "unadvertised-gzip-acceptance-fails-closed", "gzip-accepted-without-advertisement-is-refused")
	ingest := &switchingIngest{t: t, inner: &fakeIngest{token: "run-secret", gzip: true}, status: map[int]int{1: http.StatusOK}}
	srv := httptest.NewServer(ingest)
	defer srv.Close()
	cfg := sessionReporterConfig{ingestURL: srv.URL + "/ingest", token: "run-secret"}

	const session = "20260915-110000-abc123"
	a, b := eventLines(20), eventLines(30)
	acks := runReporter(t, cfg,
		frame(t, session, "events.jsonl", 0, 0, a, false),
		frame(t, session, "events.jsonl", int64(len(a)), 1, b, false),
	)
	requireEncodings(t, ingest.encodings(), "", gzipCoding)
	if !acks[0].OK {
		t.Fatalf("first ack = %+v", acks[0])
	}
	if acks[1].OK || acks[1].Code != ackCodeReceiverMismatch || acks[1].Retryable {
		t.Fatalf("ack of a gzip chunk an old instance accepted = %+v, want a terminal receiver_mismatch", acks[1])
	}
	if stored := ingest.storedBodies(); len(stored) != 1 || string(stored[0]) == b {
		t.Fatal("the old instance should have stored the compressed bytes this test guards against")
	}
}

// On a developer machine, the local-sessions route's answers carry the
// advertisement too, and the session reporter compresses for it the same way.
func TestSessionReporterLocalModeCompressesOnceTheRouteAdvertisesGzip(t *testing.T) {
	spectest.Proves(t, compressedFeature, "gzip-only-after-an-advertising-answer", "gzip-follows-an-advertising-answer")
	stubReporterDenyInspection(t, func() error { return nil })
	r := newLocalReporter(t, "fresh", true)
	r.cp.ingest.gzip = true

	first, second := eventLines(20), eventLines(40)
	session := r.serve(t,
		localFrame(t, "events.jsonl", 0, 0, first, false),
		localFrame(t, "events.jsonl", int64(len(first)), 1, second, false),
		localFrame(t, "events.jsonl", int64(len(first)+len(second)), 2, "", true),
	)
	requireLocalAcks(t, session, "", "", "")
	r.cp.mu.Lock()
	encodings := append([]string(nil), r.cp.ingest.encodings...)
	r.cp.mu.Unlock()
	requireEncodings(t, encodings, "", gzipCoding, "")
	_, stored := r.cp.received("events")
	if len(stored) != 3 || string(stored[0].body) != first || string(stored[1].body) != second {
		t.Fatal("the local route did not store the frames' own bytes")
	}
}

// The log reporter compresses once the ingest advertises gzip, and its offset
// and digest headers keep describing the frame's own bytes, which the ingest
// checks against the decoded body.
func TestLogReporterCompressesOnceTheIngestAdvertisesGzip(t *testing.T) {
	spectest.Proves(t, compressedFeature, "the-chunk-bytes-are-what-the-receiver-stores", "log-offset-and-digest-describe-the-chunk")
	ingest := &fakeLogIngest{token: "run-secret", gzip: true}
	srv := httptest.NewServer(ingest)
	defer srv.Close()
	cfg := sessionReporterConfig{ingestURL: srv.URL + "/ingest", token: "run-secret"}

	const session = "20260915-110000-abc123"
	first, second := eventLines(20), eventLines(40)
	acks := runLogReporterFrames(t, cfg,
		frame(t, session, "events.jsonl", 0, 0, first, false),
		frame(t, session, "events.jsonl", int64(len(first)), 1, second, false),
		frame(t, session, "events.jsonl", int64(len(first)), 1, second, false),
		frame(t, session, "events.jsonl", int64(len(first)+len(second)), 2, "", true),
	)
	requireOK(t, acks)
	requireEncodings(t, ingest.encodings, "", gzipCoding, gzipCoding, "")
	if got := ingest.stored(); got != first+second {
		t.Fatalf("stored log = %d bytes, want the frames' own %d bytes", len(got), len(first+second))
	}
}

// The log reporter never compresses for an ingest that does not advertise
// gzip, and refuses a gzip chunk an ingest accepted without the
// advertisement.
func TestLogReporterCompressesOnlyForAnAdvertisingIngest(t *testing.T) {
	spectest.Proves(t, compressedFeature, "gzip-only-after-an-advertising-answer", "no-gzip-without-an-advertising-answer")
	spectest.Proves(t, compressedFeature, "unadvertised-gzip-acceptance-fails-closed", "gzip-accepted-without-advertisement-is-refused")
	const session = "20260915-110000-abc123"
	first, second := eventLines(20), eventLines(40)
	frames := []string{
		frame(t, session, "events.jsonl", 0, 0, first, false),
		frame(t, session, "events.jsonl", int64(len(first)), 1, second, false),
	}

	plain := &fakeLogIngest{token: "run-secret"}
	plainSrv := httptest.NewServer(plain)
	defer plainSrv.Close()
	requireOK(t, runLogReporterFrames(t, sessionReporterConfig{ingestURL: plainSrv.URL + "/ingest", token: "run-secret"}, frames...))
	requireEncodings(t, plain.encodings, "", "")

	old := &switchingIngest{t: t, inner: &fakeLogIngest{token: "run-secret", gzip: true}, status: map[int]int{1: http.StatusOK}}
	switched := httptest.NewServer(old)
	defer switched.Close()
	acks := runLogReporterFrames(t, sessionReporterConfig{ingestURL: switched.URL + "/ingest", token: "run-secret"}, frames...)
	requireEncodings(t, old.encodings(), "", gzipCoding)
	if !acks[0].OK {
		t.Fatalf("first ack = %+v", acks[0])
	}
	if acks[1].OK || acks[1].Code != ackCodeReceiverMismatch || acks[1].Retryable {
		t.Fatalf("ack of a gzip chunk an old ingest accepted = %+v, want a terminal receiver_mismatch", acks[1])
	}
}
