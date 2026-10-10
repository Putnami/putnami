package observabilitycli

import (
	"testing"
)

// TestDecodedItemsKeepTheJSONLBytes pins the --output=jsonl bytes of the three
// surfaces: each item prints in the server's field order and keeps the fields
// the server always writes, whatever key order the input used and whichever
// members it left out.
func TestDecodedItemsKeepTheJSONLBytes(t *testing.T) {
	entries, cursor, err := decodeLogsPage([]byte(`{"nextCursor":"c1","entries":[
		{"attributes":{"k":"v"},"body":"boom","severity":17,"severityText":"ERROR","spanId":"s1","timestamp":"2026-07-03T12:00:00Z","traceId":"t1"},
		{"body":"no time"}
	]}`))
	if err != nil || cursor != "c1" {
		t.Fatalf("decodeLogsPage cursor=%q err=%v", cursor, err)
	}
	assertJSONL(t, "logs", entries, []string{
		`{"timestamp":"2026-07-03T12:00:00Z","severity":17,"severityText":"ERROR","body":"boom","traceId":"t1","spanId":"s1","attributes":{"k":"v"}}`,
		`{"timestamp":"0001-01-01T00:00:00Z","body":"no time"}`,
	})

	traces, cursor, err := decodeTracesPage([]byte(`{"traces":[
		{"spanCount":2,"start":"2026-07-03T12:00:00Z","name":"GET /a","duration":523000000,"traceId":"t1","attributes":{"k":"v"},
		 "spans":[{"duration":5,"start":"2026-07-03T12:00:00Z","name":"root","spanId":"s1","traceId":"t1","parentSpanId":"p0"}]},
		{"traceId":"t2"}
	]}`))
	if err != nil || cursor != "" {
		t.Fatalf("decodeTracesPage cursor=%q err=%v", cursor, err)
	}
	assertJSONL(t, "traces", traces, []string{
		`{"traceId":"t1","name":"GET /a","start":"2026-07-03T12:00:00Z","duration":523000000,"spanCount":2,"attributes":{"k":"v"},` +
			`"spans":[{"traceId":"t1","spanId":"s1","parentSpanId":"p0","name":"root","start":"2026-07-03T12:00:00Z","duration":5}]}`,
		`{"traceId":"t2","name":"","start":"0001-01-01T00:00:00Z","duration":0,"spanCount":0}`,
	})

	series, _, err := decodeMetricsPage([]byte(`{"series":[
		{"points":[{"value":0,"timestamp":"2026-07-03T12:00:00Z"}],"labels":{"a":"b"},"name":"n"},
		{"name":"empty","points":[]},
		{"name":"absent"}
	]}`))
	if err != nil {
		t.Fatalf("decodeMetricsPage: %v", err)
	}
	assertJSONL(t, "metrics", series, []string{
		`{"name":"n","labels":{"a":"b"},"points":[{"timestamp":"2026-07-03T12:00:00Z","value":0}]}`,
		`{"name":"empty","points":[]}`,
		`{"name":"absent","points":null}`,
	})
}

func assertJSONL[T any](t *testing.T, surface string, items []T, want []string) {
	t.Helper()
	if len(items) != len(want) {
		t.Fatalf("%s items = %d, want %d", surface, len(items), len(want))
	}
	for i, item := range items {
		if got := compactJSON(item); got != want[i] {
			t.Errorf("%s item %d\n got %s\nwant %s", surface, i, got, want[i])
		}
	}
}
