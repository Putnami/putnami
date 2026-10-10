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

// planSession is the engine session of the plan.json fixtures.
const planSession = "20261005-090000-abc123"

// planFrame encodes a plan.json frame as the engine sends it.
func planFrame(t *testing.T, offset, seq int64, data string, final bool) string {
	t.Helper()
	encoded, err := json.Marshal(protocolcli.NewSessionReportingChunk(planSession, planArtifact, offset, seq, []byte(data), final))
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded) + "\n"
}

// withMember returns the frame line with one more member set to raw.
func withMember(t *testing.T, line, name, raw string) string {
	t.Helper()
	return strings.Replace(line, "{", "{\""+name+"\":"+raw+",", 1)
}

type reporterLoop func(ctx context.Context, in io.Reader, out, logw io.Writer, cfg sessionReporterConfig, client *http.Client) error

// reportFrames serves frames through run and returns its ACKs. Every ACK
// must pass the protocol's ACK parser.
func reportFrames(t *testing.T, run reporterLoop, cfg sessionReporterConfig, frames ...string) []protocolcli.SessionReportingAck {
	t.Helper()
	var out bytes.Buffer
	if err := run(context.Background(), strings.NewReader(strings.Join(frames, "")), &out, io.Discard, cfg, &http.Client{Timeout: sessionReporterRequestTimeout}); err != nil {
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

func planChunk(offset, seq int64, data string, final bool) protocolcli.SessionReportingChunk {
	return protocolcli.NewSessionReportingChunk(planSession, planArtifact, offset, seq, []byte(data), final)
}

// A plan.json frame is the plan part's chunk, and its ACK names the frame.
func TestSessionReporter_PostsThePlanAsThePlanPart(t *testing.T) {
	ingest := &fakeIngest{token: "run-secret", acceptPlan: true}
	srv := httptest.NewServer(ingest)
	defer srv.Close()
	body := `{"protocolVersion":1,"tasks":[]}`
	acks := reportFrames(t, runSessionReporter, sessionReporterConfig{ingestURL: srv.URL + "/ingest", token: "run-secret"},
		planFrame(t, 0, 0, body, false),
		planFrame(t, int64(len(body)), 1, "", true),
		frame(t, planSession, "session.json", 0, 0, "{}", false),
	)
	wants := []protocolcli.SessionReportingChunk{
		planChunk(0, 0, body, false),
		planChunk(int64(len(body)), 1, "", true),
		protocolcli.NewSessionReportingChunk(planSession, "session.json", 0, 0, []byte("{}"), false),
	}
	for i, want := range wants {
		if !acks[i].OK || !acks[i].Matches(want) {
			t.Fatalf("ack %d = %+v, want acceptance of %+v", i, acks[i], want)
		}
	}
	if got := strings.Join(ingest.posts, ","); got != "plan/0/false,plan/1/true,session/0/false" {
		t.Fatalf("posts = %s", got)
	}
	if got := string(ingest.parts["plan"][0].body); got != body {
		t.Fatalf("plan part chunk = %q, want the frame's bytes", got)
	}
}

// A Delivery that predates the plan part answers 400. The plan is refused
// without retry, so the engine omits it and delivers the rest of the session.
func TestSessionReporter_ADeliveryWithoutThePlanPartRefusesThePlanTerminally(t *testing.T) {
	ingest := &fakeIngest{token: "run-secret"}
	srv := httptest.NewServer(ingest)
	defer srv.Close()
	acks := reportFrames(t, runSessionReporter, sessionReporterConfig{ingestURL: srv.URL + "/ingest", token: "run-secret"},
		planFrame(t, 0, 0, "{}", false),
		frame(t, planSession, "session.json", 0, 0, "{}", false),
	)
	if acks[0].OK || acks[0].Code != ackCodeInvalidChunk || acks[0].Retryable || !acks[0].Matches(planChunk(0, 0, "{}", false)) {
		t.Fatalf("plan ack = %+v, want a terminal invalid_chunk", acks[0])
	}
	if !acks[1].OK {
		t.Fatalf("session.json after a refused plan = %+v", acks[1])
	}
}

// The engine attempts a frame three times and fails the whole session's
// delivery when the last attempt is refused with retry. A plan.json fault
// therefore turns terminal on the third retryable answer, and never touches
// session.json or events.jsonl.
func TestSessionReporter_TheThirdRetryablePlanRefusalIsTerminal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	cfg := sessionReporterConfig{ingestURL: srv.URL, token: "x"}
	plan := planFrame(t, 0, 0, "{}", false)
	events := frame(t, planSession, "events.jsonl", 0, 0, "a\n", false)
	session := frame(t, planSession, "session.json", 0, 0, "{}", false)
	acks := reportFrames(t, runSessionReporter, cfg, plan, plan, plan, plan, events, events, events, events, session, session, session)

	wantRetryable := []bool{true, true, false, false, true, true, true, true, true, true, true}
	for i, want := range wantRetryable {
		if acks[i].OK || acks[i].Code != ackCodeReceiverUnavailable || acks[i].Retryable != want {
			t.Fatalf("ack %d = %+v, want receiver_unavailable retryable=%v", i, acks[i], want)
		}
	}
}

// The count spans the plan, not one frame: a plan that met two transient
// faults on its first chunk has one left for the rest.
func TestSessionReporter_PlanRefusalsCountAcrossItsChunks(t *testing.T) {
	ingest := &fakeIngest{token: "x", acceptPlan: true, status: http.StatusTooManyRequests}
	srv := httptest.NewServer(ingest)
	defer srv.Close()
	cfg := sessionReporterConfig{ingestURL: srv.URL, token: "x"}
	acks := reportFrames(t, runSessionReporter, cfg,
		planFrame(t, 0, 0, "{}", false), planFrame(t, 0, 0, "{}", false),
		planFrame(t, 2, 1, "", true))
	if !acks[0].Retryable || !acks[1].Retryable || acks[2].Retryable {
		t.Fatalf("acks = %+v, want retryable, retryable, terminal", acks)
	}
}

func TestSessionReporter_MalformedPlanFrameStopsTheProvider(t *testing.T) {
	big := strings.Repeat("p", 1024)
	for name, line := range map[string]string{
		"past the plan.json bound": planFrame(t, protocolcli.SessionReportingPlanBytes-int64(len(big))+1, 0, big, false),
		"unknown member":           withMember(t, planFrame(t, 0, 0, "{}", false), "extra", "1"),
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			err := runSessionReporter(context.Background(), strings.NewReader(line), &out, io.Discard,
				sessionReporterConfig{ingestURL: "http://127.0.0.1:1", token: "x"}, &http.Client{Timeout: time.Second})
			if err == nil {
				t.Fatal("a malformed plan.json frame must fail the provider")
			}
			if out.Len() != 0 {
				t.Fatalf("no ack may be written for a malformed frame: %q", out.String())
			}
		})
	}
}

// The log reporter reads a plan.json frame too, since the two reporters share
// the parser, and refuses it as it refuses session.json: the run's log is the
// event stream alone.
func TestLogReporter_RefusesAPlanFrame(t *testing.T) {
	ingest := &fakeLogIngest{token: "run-secret"}
	srv := httptest.NewServer(ingest)
	defer srv.Close()
	acks := reportFrames(t, runLogReporter, sessionReporterConfig{ingestURL: srv.URL + "/ingest", token: "run-secret"},
		planFrame(t, 0, 0, "{}", false),
		frame(t, planSession, "events.jsonl", 0, 0, "a\n", false),
	)
	if acks[0].OK || acks[0].Code != ackCodeInvalidChunk || acks[0].Retryable || !acks[0].Matches(planChunk(0, 0, "{}", false)) {
		t.Fatalf("plan ack = %+v, want a terminal invalid_chunk", acks[0])
	}
	if !acks[1].OK {
		t.Fatalf("events after a refused plan = %+v", acks[1])
	}
	if len(ingest.posts) != 1 {
		t.Fatalf("a plan.json frame must never reach the log: %v", ingest.posts)
	}
}
