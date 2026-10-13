package deliverycli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
)

// specVerificationRecord is the body of the spec-verification.json fixtures.
const specVerificationRecord = `{"protocolVersion":1,"generatedAt":"2026-10-13T09:00:00Z","groups":[]}`

// specVerificationFrame encodes a spec-verification.json frame as the engine
// sends it, without validating it, so a fixture may break the protocol.
func specVerificationFrame(t *testing.T, offset, seq int64, data string, final bool) string {
	t.Helper()
	encoded, err := json.Marshal(specVerificationChunk(offset, seq, data, final))
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded) + "\n"
}

func specVerificationChunk(offset, seq int64, data string, final bool) protocolcli.SessionReportingChunk {
	return protocolcli.NewSessionReportingChunk(planSession, specVerificationArtifact, offset, seq, []byte(data), final)
}

// The engine sends spec-verification.json last, once the events final marker
// is acknowledged. Each frame is the spec-verification part's chunk, and its
// ACK names the frame.
func TestSessionReporter_PostsTheSpecVerificationRecordAsItsPart(t *testing.T) {
	ingest := &fakeIngest{token: "run-secret", acceptSpecVerification: true}
	srv := httptest.NewServer(ingest)
	defer srv.Close()
	acks := reportFrames(t, runSessionReporter, sessionReporterConfig{ingestURL: srv.URL + "/ingest", token: "run-secret"},
		frame(t, planSession, "events.jsonl", 0, 0, "a\n", false),
		frame(t, planSession, "events.jsonl", 2, 1, "", true),
		specVerificationFrame(t, 0, 0, specVerificationRecord, false),
		specVerificationFrame(t, int64(len(specVerificationRecord)), 1, "", true),
	)
	wants := []protocolcli.SessionReportingChunk{
		protocolcli.NewSessionReportingChunk(planSession, "events.jsonl", 0, 0, []byte("a\n"), false),
		protocolcli.NewSessionReportingChunk(planSession, "events.jsonl", 2, 1, nil, true),
		specVerificationChunk(0, 0, specVerificationRecord, false),
		specVerificationChunk(int64(len(specVerificationRecord)), 1, "", true),
	}
	if len(acks) != len(wants) {
		t.Fatalf("acks = %d, want %d", len(acks), len(wants))
	}
	for i, want := range wants {
		if !acks[i].OK || !acks[i].Matches(want) {
			t.Fatalf("ack %d = %+v, want acceptance of %+v", i, acks[i], want)
		}
	}
	if got := strings.Join(ingest.posts, ","); got != "events/0/false,events/1/true,spec-verification/0/false,spec-verification/1/true" {
		t.Fatalf("posts = %s", got)
	}
	stored := ingest.parts["spec-verification"]
	if len(stored) != 2 || string(stored[0].body) != specVerificationRecord || !stored[1].final {
		t.Fatalf("spec-verification part = %+v, want the record's bytes then its final marker", stored)
	}
}

// A receiver that predates the spec-verification part answers 400. The record
// is refused without retry, so the engine omits it, and the events the
// receiver already stored are unchanged.
func TestSessionReporter_AReceiverWithoutTheSpecVerificationPartRefusesItTerminally(t *testing.T) {
	ingest := &fakeIngest{token: "run-secret"}
	srv := httptest.NewServer(ingest)
	defer srv.Close()
	acks := reportFrames(t, runSessionReporter, sessionReporterConfig{ingestURL: srv.URL + "/ingest", token: "run-secret"},
		frame(t, planSession, "events.jsonl", 0, 0, "a\n", false),
		frame(t, planSession, "events.jsonl", 2, 1, "", true),
		specVerificationFrame(t, 0, 0, specVerificationRecord, false),
	)
	if !acks[0].OK || !acks[1].OK {
		t.Fatalf("events acks = %+v, want both accepted", acks[:2])
	}
	if acks[2].OK || acks[2].Code != ackCodeInvalidChunk || acks[2].Retryable || !acks[2].Matches(specVerificationChunk(0, 0, specVerificationRecord, false)) {
		t.Fatalf("spec-verification ack = %+v, want a terminal invalid_chunk", acks[2])
	}
	if events := ingest.parts["events"]; len(events) != 2 || !events[1].final {
		t.Fatalf("events part = %+v, want both chunks stored, the final marker last", events)
	}
	if _, stored := ingest.parts["spec-verification"]; stored {
		t.Fatal("a refused spec-verification.json chunk must not be stored")
	}
}

// The engine omits spec-verification.json once its attempts at a frame end,
// whatever the last answer was, so a retryable fault on the record stays
// retryable: unlike plan.json, it never has to turn terminal to protect the
// session.
func TestSessionReporter_SpecVerificationRefusalsStayRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	attempt := specVerificationFrame(t, 0, 0, specVerificationRecord, false)
	acks := reportFrames(t, runSessionReporter, sessionReporterConfig{ingestURL: srv.URL, token: "x"},
		attempt, attempt, attempt, attempt)
	for i, ack := range acks {
		if ack.OK || ack.Code != ackCodeReceiverUnavailable || !ack.Retryable {
			t.Fatalf("attempt %d ack = %+v, want a retryable receiver_unavailable NACK", i+1, ack)
		}
	}
}

func TestSessionReporter_MalformedSpecVerificationFrameStopsTheProvider(t *testing.T) {
	big := strings.Repeat("s", 1024)
	for name, line := range map[string]string{
		"past the spec-verification.json bound": specVerificationFrame(t, protocolcli.SessionReportingSpecVerificationBytes-int64(len(big))+1, 0, big, false),
		"unknown member":                        withMember(t, specVerificationFrame(t, 0, 0, "{}", false), "extra", "1"),
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			err := runSessionReporter(context.Background(), strings.NewReader(line), &out, io.Discard,
				sessionReporterConfig{ingestURL: "http://127.0.0.1:1", token: "x"}, &http.Client{Timeout: time.Second})
			if err == nil {
				t.Fatal("a malformed spec-verification.json frame must fail the provider")
			}
			if out.Len() != 0 {
				t.Fatalf("no ack may be written for a malformed frame: %q", out.String())
			}
		})
	}
}

// The log reporter refuses a spec-verification.json frame as it refuses
// plan.json: the run's log is the event stream alone.
func TestLogReporter_RefusesASpecVerificationFrame(t *testing.T) {
	ingest := &fakeLogIngest{token: "run-secret"}
	srv := httptest.NewServer(ingest)
	defer srv.Close()
	acks := reportFrames(t, runLogReporter, sessionReporterConfig{ingestURL: srv.URL + "/ingest", token: "run-secret"},
		frame(t, planSession, "events.jsonl", 0, 0, "a\n", false),
		specVerificationFrame(t, 0, 0, specVerificationRecord, false),
	)
	if !acks[0].OK {
		t.Fatalf("events ack = %+v", acks[0])
	}
	if acks[1].OK || acks[1].Code != ackCodeInvalidChunk || acks[1].Retryable || !acks[1].Matches(specVerificationChunk(0, 0, specVerificationRecord, false)) {
		t.Fatalf("spec-verification ack = %+v, want a terminal invalid_chunk", acks[1])
	}
	if len(ingest.posts) != 1 {
		t.Fatalf("a spec-verification.json frame must never reach the log: %v", ingest.posts)
	}
}
