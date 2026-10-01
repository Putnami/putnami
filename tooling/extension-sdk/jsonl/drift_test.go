package jsonl

import (
	"go.putnami.dev/protocol/features/spectest"

	"encoding/json"
	"sort"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	runtime "go.putnami.dev/protocol/runtime"
)

// TestDrift_EmitterConformsToRuntimeProtocol pushes one of every event
// the SDK can emit through the runtime protocol parser and validator.
// If the emitter and the protocol ever disagree on the wire format,
// this test fails — the protocol package is the source of truth.
func TestDrift_EmitterConformsToRuntimeProtocol(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "event-stream-conformance", "the-emitter-conforms-to-the-runtime-protocol-wire-shape")
	out := captureStdout(t, func() {
		e := New()
		e.Meta("@putnami/test", "build")
		e.Log("info", "plain log")
		e.LogEvent("error", "forwarded log",
			map[string]any{"requestId": "r-1"},
			map[string]any{"message": "boom", "stack": "stack-trace"})
		e.Info("info helper")
		e.Warn("warn helper")
		e.Error("error helper")
		e.Debug("debug helper")
		e.PhaseStart("compile")
		e.PhaseEnd("compile", "success")
		e.Progress(3, 10, "compiling")
		e.Diagnostic("warning", "unused variable", "main.go", 12)
		e.DiagnosticWithCode("error", "type mismatch", "main.go", 4, 9, "TS2322")
		e.Metric("tests-total", 42, "count")
		e.Metric("coverage", 81.5, "percent")
		e.Metric("binary-size", int64(1024), "bytes")
		e.Metric("numeric-string", "17", "count")
		e.Artifact("bin", "app", "binary", "dist/app")
		e.ArtifactWithData("pkg", "lib", "package", "dist/lib.tgz",
			map[string]any{"registry": "npm", "version": "1.2.3"})
		e.Summary("all good")
		e.SummaryWithData("42 tests", map[string]any{"tests": 42, "failures": 0})
		e.Result("OK", map[string]any{"binaryPath": "dist/app"})
	})

	lines := strings.Split(strings.TrimSpace(out), "\n")
	const wantLines = 21
	if len(lines) != wantLines {
		t.Fatalf("emitted %d lines, want %d:\n%s", len(lines), wantLines, out)
	}

	for i, line := range lines {
		evt, err := runtime.ParseEvent([]byte(line))
		if err != nil {
			t.Errorf("line %d does not parse as a runtime protocol event: %v\n%s", i, err, line)
			continue
		}
		if diags := runtime.ValidateEvent(evt); diag.HasErrors(diags) {
			t.Errorf("line %d violates the runtime protocol: %v\n%s", i, diags, line)
		}
	}
}

// TestDrift_ReadyWireShape pins the exact member set of a readiness line. Its
// counterpart is the "matches the Go SDK readiness wire shape" case in
// typescript/framework/runtime/test/jobs/events.test.ts: the two emitters must
// produce the same bytes for the same facts, so a member added, renamed, or
// dropped on one side fails a test on both.
//
// The `url` assertion is the load-bearing one: an endpoint's address is DERIVED
// from its members and must never travel, or an endpoint could ship a url that
// disagrees with its own scheme/host/port.
func TestDrift_ReadyWireShape(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "event-stream-conformance", "the-readiness-marker-has-the-protocol-wire-shape")
	out := captureStdout(t, func() {
		NewForVersion(runtime.ProtocolVersion2).Ready(runtime.ReadyData{
			Target: runtime.ReadyTargetServer,
			Name:   "api",
			Endpoints: []runtime.ReadyEndpoint{
				{Scheme: runtime.ReadySchemeHTTPS, Host: "localhost", Port: 8443},
				{Scheme: runtime.ReadySchemeHTTP, Host: "localhost", Port: 3000, Path: "/api"},
			},
			DurationMs: 420,
		})
	})

	raw := strings.TrimSpace(out)
	var line map[string]any
	if err := json.Unmarshal([]byte(raw), &line); err != nil {
		t.Fatalf("ready line does not parse: %v\n%s", err, out)
	}

	// The byte-for-byte pin. Its TypeScript twin is the "matches the Go SDK
	// readiness wire shape byte for byte" case; the wall-clock timestamp is the
	// one member that legitimately varies, so it is normalized away.
	stamp, _ := line["time"].(string)
	if stamp == "" {
		t.Fatal("ready line carries no timestamp")
	}
	const wantLine = `{"v":2,"type":"ready","time":"T","data":{"target":"server","name":"api",` +
		`"endpoints":[{"scheme":"http","host":"localhost","port":3000,"path":"/api"},` +
		`{"scheme":"https","host":"localhost","port":8443}],"durationMs":420}}`
	if got := strings.Replace(raw, stamp, "T", 1); got != wantLine {
		t.Errorf("ready wire bytes drifted from the TypeScript emitter:\n got %s\nwant %s", got, wantLine)
	}

	assertKeys(t, "envelope", line, []string{"data", "time", "type", "v"})
	if line["v"] != float64(runtime.ProtocolVersion2) {
		t.Errorf("ready line v = %v, want %d", line["v"], runtime.ProtocolVersion2)
	}
	if line["type"] != string(runtime.EventReady) {
		t.Errorf("ready line type = %v, want %q", line["type"], runtime.EventReady)
	}

	data, ok := line["data"].(map[string]any)
	if !ok {
		t.Fatalf("ready line data = %v, want an object", line["data"])
	}
	assertKeys(t, "data", data, []string{"durationMs", "endpoints", "name", "target"})

	endpoints, ok := data["endpoints"].([]any)
	if !ok || len(endpoints) != 2 {
		t.Fatalf("data.endpoints = %v, want 2 entries", data["endpoints"])
	}
	// Canonical order: http before https, byte order on scheme.
	first, _ := endpoints[0].(map[string]any)
	assertKeys(t, "endpoints[0]", first, []string{"host", "path", "port", "scheme"})
	if first["scheme"] != runtime.ReadySchemeHTTP {
		t.Errorf("endpoints[0].scheme = %v, want %q — endpoints travel in canonical order",
			first["scheme"], runtime.ReadySchemeHTTP)
	}
	second, _ := endpoints[1].(map[string]any)
	// path is omitted when empty, on both sides.
	assertKeys(t, "endpoints[1]", second, []string{"host", "port", "scheme"})
	for i, endpoint := range endpoints {
		if _, carried := endpoint.(map[string]any)["url"]; carried {
			t.Errorf("endpoints[%d] carries a url; the address is derived, never emitted", i)
		}
	}
}

func assertKeys(t *testing.T, what string, obj map[string]any, want []string) {
	t.Helper()
	got := make([]string, 0, len(obj))
	for key := range obj {
		got = append(got, key)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s members = %v, want %v", what, got, want)
	}
}

// TestDrift_NonNumericMetricDropped pins the SDK's handling of values
// the protocol forbids: the metric is replaced by a debug log rather
// than emitting a schema-violating event.
func TestDrift_NonNumericMetricDropped(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "event-stream-conformance", "an-unrepresentable-value-is-dropped-rather-than-emitted-malformed")
	out := captureStdout(t, func() {
		New().Metric("bad", struct{}{}, "count")
	})
	evt, err := runtime.ParseEvent([]byte(strings.TrimSpace(out)))
	if err != nil {
		t.Fatalf("fallback line does not parse: %v", err)
	}
	if evt.Type != runtime.EventLog {
		t.Fatalf("non-numeric metric emitted %q event, want debug log", evt.Type)
	}
}
