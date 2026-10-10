package deliverycli

import (
	"encoding/json"
	"testing"

	deliveryapiclient "go.putnami.dev/cloud/clients/delivery-api/go"
)

// TestCILogEntryFromKeepsTheJSONLBytes pins the `ci logs --output=jsonl` bytes
// of a live-tail entry: the generated entry converts into CILogEntry, which
// prints in the server's field order and always writes the timestamp.
func TestCILogEntryFromKeepsTheJSONLBytes(t *testing.T) {
	for _, tc := range []struct{ wire, want string }{
		{
			`{"attributes":{"k":"v"},"body":"boom","severity":17,"severityText":"ERROR","spanId":"s1","timestamp":"2026-07-03T12:00:00Z","traceId":"t1"}`,
			`{"timestamp":"2026-07-03T12:00:00Z","severity":17,"severityText":"ERROR","body":"boom","traceId":"t1","spanId":"s1","attributes":{"k":"v"}}`,
		},
		{`{"body":"no time"}`, `{"timestamp":"0001-01-01T00:00:00Z","body":"no time"}`},
	} {
		var entry deliveryapiclient.LogEntry
		if err := json.Unmarshal([]byte(tc.wire), &entry); err != nil {
			t.Fatalf("decode %s: %v", tc.wire, err)
		}
		if got := ciCompactJSON(ciLogEntryFrom(entry)); got != tc.want {
			t.Errorf("entry %s\n got %s\nwant %s", tc.wire, got, tc.want)
		}
	}
}

const ciTaskEventIdentity = `"identity":{"key":"k","project":{"id":"1","name":"libs/cli"},"task":{"name":"test","command":"test"}}`

// Since the engine ships its event file, most of a run's log is
// `task:event` records. Each one reads as the task's own line.
func TestCIRenderTaskEvent(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{
			"a line the task printed",
			`{"protocolVersion":2,"record":"task:event",` + ciTaskEventIdentity + `,"event":{"type":"log","level":"info","message":"ok  \texample.com/libs/cli\t1.2s"}}`,
			"<libs/cli:test> ok  \texample.com/libs/cli\t1.2s",
		},
		{
			"a finding names where it points",
			`{"record":"task:event",` + ciTaskEventIdentity + `,"event":{"type":"diagnostic","severity":"error","code":"unused","message":"x declared and not used","location":{"file":"ci.go","line":42}}}`,
			"<libs/cli:test> x declared and not used  (ci.go:42)",
		},
		{
			"an event with no message renders its facts",
			`{"record":"task:event",` + ciTaskEventIdentity + `,"event":{"type":"metric","name":"coverage","value":81.5,"unit":"percent","data":{"nested":true}}}`,
			"<libs/cli:test> metric name=coverage unit=percent value=81.5",
		},
		{
			"a record with half an identity",
			`{"record":"task:event","identity":{"task":{"name":"lint"}},"event":{"type":"log","message":"done"}}`,
			"<lint> done",
		},
		{
			"a record with no identity",
			`{"record":"task:event","event":{"type":"summary","message":"3 packages"}}`,
			"3 packages",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ciRenderRecordBody(tc.body)
			if !ok || got != tc.want {
				t.Fatalf("rendered = %q (%v), want %q", got, ok, tc.want)
			}
		})
	}
}

// A `task:event` with no event object is not a shape this renderer speaks. It
// prints raw; it is never dropped.
func TestCIRenderTaskEventWithoutAnEventPrintsRaw(t *testing.T) {
	if got, ok := ciRenderRecordBody(`{"record":"task:event"}`); ok {
		t.Fatalf("rendered = %q, want the raw fallback", got)
	}
}

// `--level` filters on what the record asserts: the level of the task's line,
// or the severity of the finding.
func TestCIEntrySeverityReadsATaskEvent(t *testing.T) {
	for _, tc := range []struct {
		name, event string
		want        ciSeverity
	}{
		{"an error line", `{"type":"log","level":"error","message":"boom"}`, ciSeverityError},
		{"a warning finding", `{"type":"diagnostic","severity":"warning","message":"unused"}`, ciSeverityWarn},
		{"an info line", `{"type":"log","level":"info","message":"ok"}`, ciSeverityInfo},
		{"an unknown word raises nothing", `{"type":"log","level":"loud","message":"?"}`, ciSeverityUnset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := CILogEntry{Body: `{"record":"task:event","event":` + tc.event + `}`}
			if got := ciEntrySeverity(entry); got != tc.want {
				t.Fatalf("severity = %v, want %v", got, tc.want)
			}
		})
	}
}
