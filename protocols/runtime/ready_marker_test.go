package runtime

import (
	"encoding/json"
	"testing"
)

// decodeLogRecord round-trips a workload-shaped log record through JSON, which
// is what a forwarder actually holds: the marker arrives as map[string]any with
// float64 numbers, never as the Go struct the workload wrote.
func decodeLogRecord(t *testing.T, record map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestReadyMarkerFromLogRecord_RoundTrip(t *testing.T) {
	record := decodeLogRecord(t, map[string]any{
		"severity":  "INFO",
		"message":   "⚡️ listening http://localhost:3000",
		"timestamp": "2026-07-28T09:00:01.000Z",
		ReadyLogKey: ReadyMarker(ReadyData{
			Target:     ReadyTargetServer,
			Name:       "api",
			Endpoints:  []ReadyEndpoint{{Scheme: ReadySchemeHTTP, Host: "localhost", Port: 3000}},
			DurationMs: 42,
		}),
	})

	data, ok := ReadyMarkerFromLogRecord(record)
	if !ok {
		t.Fatal("a well-formed marker must be recognized")
	}
	if data.Target != ReadyTargetServer || data.Name != "api" || data.DurationMs != 42 {
		t.Fatalf("marker lost members: %+v", data)
	}
	if len(data.Endpoints) != 1 || data.Endpoints[0].URL() != "http://localhost:3000" {
		t.Fatalf("marker lost its endpoint: %+v", data.Endpoints)
	}
}

// TestReadyMarker_Canonicalizes pins that the marker is deterministic at the
// workload, not only at the emitter: the log bytes a served app writes must not
// depend on the order it happened to bind its listeners in.
func TestReadyMarker_Canonicalizes(t *testing.T) {
	unsorted := []ReadyEndpoint{
		{Scheme: ReadySchemeHTTPS, Host: "localhost", Port: 8443},
		{Scheme: ReadySchemeGRPC, Host: "localhost", Port: 50051},
	}
	marker := ReadyMarker(ReadyData{Target: ReadyTargetServer, Endpoints: unsorted})
	if marker.Endpoints[0].Scheme != ReadySchemeGRPC {
		t.Errorf("ReadyMarker did not canonicalize: %+v", marker.Endpoints)
	}
	if unsorted[0].Scheme != ReadySchemeHTTPS {
		t.Error("ReadyMarker mutated the caller's slice")
	}
}

// TestReadyMarkerFromLogRecord_UnsortedMarkerIsAccepted: the extraction
// canonicalizes before validating, so a workload that writes its endpoints in
// bind order still produces a valid, deterministic event.
func TestReadyMarkerFromLogRecord_UnsortedMarkerIsAccepted(t *testing.T) {
	record := decodeLogRecord(t, map[string]any{
		"severity": "INFO",
		"message":  "listening",
		ReadyLogKey: map[string]any{
			"target": ReadyTargetServer,
			"endpoints": []map[string]any{
				{"scheme": ReadySchemeHTTPS, "host": "localhost", "port": 8443},
				{"scheme": ReadySchemeHTTP, "host": "localhost", "port": 3000},
			},
		},
	})
	data, ok := ReadyMarkerFromLogRecord(record)
	if !ok {
		t.Fatal("an unsorted but otherwise valid marker must be accepted")
	}
	if data.Endpoints[0].Scheme != ReadySchemeHTTP {
		t.Errorf("extraction did not canonicalize: %+v", data.Endpoints)
	}
}

// TestReadyMarkerFromLogRecord_RejectsMalformed is the fail-closed guard: a
// marker that would produce an invalid `ready` line yields no readiness at all,
// because an invalid line poisons the WHOLE stream for its consumer while a
// missing one only costs this signal.
func TestReadyMarkerFromLogRecord_RejectsMalformed(t *testing.T) {
	cases := map[string]any{
		"no target":           map[string]any{"endpoints": []map[string]any{{"scheme": "http", "host": "h", "port": 1}}},
		"unknown target":      map[string]any{"target": "database"},
		"server without addr": map[string]any{"target": ReadyTargetServer},
		"unknown scheme":      map[string]any{"target": ReadyTargetServer, "endpoints": []map[string]any{{"scheme": "amqp", "host": "h", "port": 5672}}},
		"port out of range":   map[string]any{"target": ReadyTargetServer, "endpoints": []map[string]any{{"scheme": "http", "host": "h", "port": 70000}}},
		"relative path":       map[string]any{"target": ReadyTargetServer, "endpoints": []map[string]any{{"scheme": "http", "host": "h", "port": 80, "path": "api"}}},
		"negative duration":   map[string]any{"target": ReadyTargetWorkload, "durationMs": -1},
		"duplicate endpoint":  map[string]any{"target": ReadyTargetServer, "endpoints": []map[string]any{{"scheme": "http", "host": "h", "port": 80}, {"scheme": "http", "host": "h", "port": 80}}},
		"not an object":       "ready",
		"wrong member types":  map[string]any{"target": ReadyTargetServer, "endpoints": "http://localhost"},
		"empty":               map[string]any{},
	}
	for name, marker := range cases {
		record := decodeLogRecord(t, map[string]any{
			"severity":  "INFO",
			"message":   "listening",
			ReadyLogKey: marker,
		})
		if data, ok := ReadyMarkerFromLogRecord(record); ok {
			t.Errorf("%s: malformed marker accepted as %+v", name, data)
		}
	}
}

func TestReadyMarkerFromLogRecord_AbsentMarker(t *testing.T) {
	if _, ok := ReadyMarkerFromLogRecord(nil); ok {
		t.Error("a nil record carries no marker")
	}
	record := decodeLogRecord(t, map[string]any{"severity": "INFO", "message": "listening"})
	if _, ok := ReadyMarkerFromLogRecord(record); ok {
		t.Error("a record without the reserved key carries no marker")
	}
	record[ReadyLogKey] = nil
	if _, ok := ReadyMarkerFromLogRecord(record); ok {
		t.Error("an explicitly null marker carries no readiness")
	}
}

// TestReadyLogKey_IsNamespaced pins the reserved key's spelling: it is a wire
// contract two languages and three extensions agree on, so renaming it is a
// migration, not an edit.
func TestReadyLogKey_IsNamespaced(t *testing.T) {
	if ReadyLogKey != "putnami.ready" {
		t.Errorf("ReadyLogKey = %q; renaming it breaks every workload already logging it", ReadyLogKey)
	}
}
