package jsonl

import (
	"go.putnami.dev/protocol/features/spectest"

	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	runtime "go.putnami.dev/protocol/runtime"
)

// TestMain makes this package's tests hermetic with respect to the CLI that
// runs them. `New()` reads the invoking CLI's protocol advertisement, and these
// tests ARE invoked by a Putnami CLI that advertises v2 — without clearing the
// variable, every default-constructed emitter here would silently speak the
// outer run's negotiated version instead of the one under test. Tests that need
// an advertisement set it explicitly with t.Setenv.
func TestMain(m *testing.M) {
	_ = os.Unsetenv(runtime.AcceptedVersionEnv)
	os.Exit(m.Run())
}

// listeningLine is the structured log record a first-party framework writes
// when its HTTP server starts listening: the human message a developer reads,
// plus the reserved machine-readable readiness marker beside it.
func listeningLine(t *testing.T, port int) string {
	t.Helper()
	record := map[string]any{
		"severity":   "INFO",
		"message":    "⚡️ listening http://localhost:" + itoa(port),
		"timestamp":  "2026-07-28T09:00:01.000Z",
		"logger":     "http",
		"durationMs": 900,
		runtime.ReadyLogKey: runtime.ReadyMarker(runtime.ReadyData{
			Target:     runtime.ReadyTargetServer,
			Endpoints:  []runtime.ReadyEndpoint{{Scheme: runtime.ReadySchemeHTTP, Host: "localhost", Port: port}},
			DurationMs: 900,
		}),
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var digits []byte
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}
	return string(digits)
}

func decodeLines(t *testing.T, output string) []*runtime.Event {
	t.Helper()
	events, err := runtime.DecodeAll(strings.NewReader(output))
	if err != nil {
		t.Fatalf("forwarded output is not a runtime event stream: %v\n%s", err, output)
	}
	return events
}

// TestForwardLine_EmitsTypedReadinessAtV2 is the core B6a behavior: a served
// workload's listening log becomes BOTH the log line the console shows and a
// typed readiness event, in that order.
func TestForwardLine_EmitsTypedReadinessAtV2(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "event-stream-conformance", "a-typed-readiness-marker-is-emitted-at-the-negotiated-version")
	e := NewForVersion(runtime.ProtocolVersion2)
	line := listeningLine(t, 3000)
	output := captureStdout(t, func() { ForwardLine(e, line, "info") })

	events := decodeLines(t, output)
	if len(events) != 2 {
		t.Fatalf("forwarded %d events, want 2 (log then ready):\n%s", len(events), output)
	}
	if events[0].Type != runtime.EventLog {
		t.Fatalf("event[0] = %q, want the log event first so console ordering is unchanged", events[0].Type)
	}
	if !strings.Contains(events[0].Message, "listening http://") {
		t.Errorf("the human log message must survive the forwarder verbatim: %q", events[0].Message)
	}
	if events[1].Type != runtime.EventReady {
		t.Fatalf("event[1] = %q, want ready", events[1].Type)
	}
	for i, evt := range events {
		if evt.V != runtime.ProtocolVersion2 {
			t.Errorf("event[%d].v = %d, want %d — one stream speaks one version", i, evt.V, runtime.ProtocolVersion2)
		}
		if diags := runtime.ValidateEvent(evt); diag.HasErrors(diags) {
			t.Errorf("event[%d] violates the protocol: %v", i, diags)
		}
	}

	data, err := runtime.ReadyPayload(events[1])
	if err != nil {
		t.Fatal(err)
	}
	if data.Target != runtime.ReadyTargetServer || len(data.Endpoints) != 1 {
		t.Fatalf("readiness payload = %+v", data)
	}
	if got := data.Endpoints[0].URL(); got != "http://localhost:3000" {
		t.Errorf("endpoint = %q, want http://localhost:3000", got)
	}
	if data.DurationMs != 900 {
		t.Errorf("durationMs = %d, want 900", data.DurationMs)
	}
}

// TestForwardLine_ReadinessMarkerStrippedFromContext keeps the machine channel
// out of the human output: the marker is consumed, never echoed back as a log
// context field.
func TestForwardLine_ReadinessMarkerStrippedFromContext(t *testing.T) {
	e := NewForVersion(runtime.ProtocolVersion2)
	output := captureStdout(t, func() { ForwardLine(e, listeningLine(t, 3000), "info") })

	events := decodeLines(t, output)
	logEvent := events[0]
	if logEvent.Context == nil {
		t.Fatal("the log event lost its context fields")
	}
	ctx := *logEvent.Context
	if _, leaked := ctx[runtime.ReadyLogKey]; leaked {
		t.Errorf("the reserved readiness marker leaked into log context: %v", ctx)
	}
	if ctx["logger"] != "http" {
		t.Errorf("unrelated context fields must survive: %v", ctx)
	}
	if _, ok := ctx["durationMs"]; !ok {
		t.Errorf("unrelated context fields must survive: %v", ctx)
	}
}

// TestForwardLine_NoReadinessLeakAtV1 is the no-leak guard: a CLI that never
// advertised v2 acceptance receives the byte-identical v1 stream it always did,
// with no `ready` line it would reject the whole stream for.
func TestForwardLine_NoReadinessLeakAtV1(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "event-stream-conformance", "no-readiness-marker-leaks-at-a-version-that-lacks-it")
	e := New() // no advertisement in the test environment ⇒ v1
	if e.SupportsReady() {
		t.Fatal("an un-negotiated emitter must not claim readiness support")
	}
	output := captureStdout(t, func() { ForwardLine(e, listeningLine(t, 3000), "info") })

	events := decodeLines(t, output)
	if len(events) != 1 {
		t.Fatalf("forwarded %d events at v1, want only the log event:\n%s", len(events), output)
	}
	if events[0].V != runtime.ProtocolVersion || events[0].Type != runtime.EventLog {
		t.Fatalf("v1 forwarding changed shape: v=%d type=%q", events[0].V, events[0].Type)
	}
}

// TestForwardLine_MalformedMarkerFailsClosed: a workload that writes a broken
// marker loses its typed readiness, but its log line — and the rest of the
// stream — stays valid.
func TestForwardLine_MalformedMarkerFailsClosed(t *testing.T) {
	e := NewForVersion(runtime.ProtocolVersion2)
	line := `{"severity":"INFO","message":"listening http://localhost:3000","putnami.ready":{"target":"server"}}`
	output := captureStdout(t, func() { ForwardLine(e, line, "info") })

	events := decodeLines(t, output)
	if len(events) != 1 {
		t.Fatalf("forwarded %d events, want only the log event:\n%s", len(events), output)
	}
	if diags := runtime.ValidateEvent(events[0]); diag.HasErrors(diags) {
		t.Errorf("the log event must stay valid: %v", diags)
	}
}

// TestForwardPipe_ReemitsReadinessOnEveryRestart is the restart guard. Every
// serve wrapper's restart loop (go/extension runWithWatch, python/extension
// Serve, and the CLI's own per-iteration respawn) spawns a fresh process with
// fresh pipes and forwards them through the SAME emitter, so readiness must
// re-announce itself once per iteration. Nothing in the forwarder is allowed to
// remember that it already saw a readiness marker.
func TestForwardPipe_ReemitsReadinessOnEveryRestart(t *testing.T) {
	e := NewForVersion(runtime.ProtocolVersion2)
	line := listeningLine(t, 8080)

	output := captureStdout(t, func() {
		for iteration := 0; iteration < 3; iteration++ {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			go func() {
				_, _ = io.WriteString(w, line+"\n")
				_ = w.Close()
			}()
			var wg sync.WaitGroup
			wg.Add(1)
			ForwardPipe(e, r, "info", &wg)
			wg.Wait()
			_ = r.Close()
		}
	})

	events := decodeLines(t, output)
	ready := 0
	for _, evt := range events {
		if evt.Type == runtime.EventReady {
			ready++
		}
	}
	if ready != 3 {
		t.Fatalf("3 serve iterations produced %d ready events, want 3:\n%s", ready, output)
	}
}

// TestEmitter_ReadyReportsRefusalAtV1 pins the SDK wrapper's contract: it never
// writes an invalid line, and it tells the caller when it wrote nothing.
func TestEmitter_ReadyReportsRefusalAtV1(t *testing.T) {
	data := runtime.ReadyData{
		Target:    runtime.ReadyTargetServer,
		Endpoints: []runtime.ReadyEndpoint{{Scheme: runtime.ReadySchemeHTTP, Host: "localhost", Port: 3000}},
	}

	v1 := NewForVersion(runtime.ProtocolVersion)
	var emitted bool
	output := captureStdout(t, func() { emitted = v1.Ready(data) })
	if emitted {
		t.Error("Ready must report false on a v1 stream")
	}
	if strings.TrimSpace(output) != "" {
		t.Errorf("Ready wrote %q on a v1 stream, want nothing", output)
	}

	v2 := NewForVersion(runtime.ProtocolVersion2)
	output = captureStdout(t, func() { emitted = v2.Ready(data) })
	if !emitted {
		t.Error("Ready must report true on a v2 stream")
	}
	if strings.TrimSpace(output) == "" {
		t.Error("Ready wrote nothing on a v2 stream")
	}
}

// TestNew_ReadsNegotiationFromEnvironment pins the SDK side of the negotiation
// seam: the emitter's version comes from the CLI's advertisement, resolved by
// the protocol package's shared helper so writer and reader cannot disagree.
func TestNew_ReadsNegotiationFromEnvironment(t *testing.T) {
	cases := []struct {
		advertised string
		want       int
	}{
		{"", runtime.ProtocolVersion},
		{"1", runtime.ProtocolVersion},
		{"2", runtime.ProtocolVersion2},
		{"garbage", runtime.ProtocolVersion},
	}
	for _, tc := range cases {
		t.Setenv(runtime.AcceptedVersionEnv, tc.advertised)
		if got := New().Version(); got != tc.want {
			t.Errorf("advertisement %q ⇒ emitter version %d, want %d", tc.advertised, got, tc.want)
		}
	}
}

// TestNewForVersion_UnknownVersionFailsClosed keeps an out-of-range version
// from producing a stream nobody can parse.
func TestNewForVersion_UnknownVersionFailsClosed(t *testing.T) {
	for _, version := range []int{0, -1, 42} {
		if got := NewForVersion(version).Version(); got != runtime.ProtocolVersion {
			t.Errorf("NewForVersion(%d).Version() = %d, want %d", version, got, runtime.ProtocolVersion)
		}
	}
}
