package runtime

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func readyEvent(t *testing.T, data ReadyData) *Event {
	t.Helper()
	payload, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return &Event{V: ProtocolVersion2, Type: EventReady, Data: payload}
}

func TestValidateReadyEvent_Valid(t *testing.T) {
	cases := map[string]ReadyData{
		"server with one endpoint": {
			Target:    ReadyTargetServer,
			Name:      "api",
			Endpoints: []ReadyEndpoint{{Scheme: ReadySchemeHTTP, Host: "localhost", Port: 3000}},
		},
		"server with a base path": {
			Target:    ReadyTargetServer,
			Endpoints: []ReadyEndpoint{{Scheme: ReadySchemeHTTPS, Host: "0.0.0.0", Port: 8443, Path: "/api"}},
		},
		"workload without endpoints": {
			Target:     ReadyTargetWorkload,
			DurationMs: 12,
		},
		"workload with endpoints": {
			Target:    ReadyTargetWorkload,
			Endpoints: []ReadyEndpoint{{Scheme: ReadySchemeTCP, Host: "127.0.0.1", Port: 1}},
		},
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if diags := ValidateEvent(readyEvent(t, data)); diag.HasErrors(diags) {
				t.Errorf("valid ready event rejected: %v", diags)
			}
		})
	}
}

func TestValidateReadyEvent_Rejects(t *testing.T) {
	cases := []struct {
		name  string
		data  ReadyData
		code  string
		field string
	}{
		{
			name:  "missing target",
			data:  ReadyData{},
			code:  "required-field",
			field: "data.target",
		},
		{
			name:  "unknown target",
			data:  ReadyData{Target: "database"},
			code:  "invalid-enum",
			field: "data.target",
		},
		{
			name:  "server without endpoints",
			data:  ReadyData{Target: ReadyTargetServer},
			code:  "required-field",
			field: "data.endpoints",
		},
		{
			name: "unknown scheme",
			data: ReadyData{Target: ReadyTargetServer, Endpoints: []ReadyEndpoint{
				{Scheme: "ftp", Host: "localhost", Port: 21},
			}},
			code:  "invalid-enum",
			field: "data.endpoints[0].scheme",
		},
		{
			name: "missing scheme",
			data: ReadyData{Target: ReadyTargetServer, Endpoints: []ReadyEndpoint{
				{Host: "localhost", Port: 3000},
			}},
			code:  "required-field",
			field: "data.endpoints[0].scheme",
		},
		{
			name: "missing host",
			data: ReadyData{Target: ReadyTargetServer, Endpoints: []ReadyEndpoint{
				{Scheme: ReadySchemeHTTP, Port: 3000},
			}},
			code:  "required-field",
			field: "data.endpoints[0].host",
		},
		{
			name: "port below range",
			data: ReadyData{Target: ReadyTargetServer, Endpoints: []ReadyEndpoint{
				{Scheme: ReadySchemeHTTP, Host: "localhost", Port: 0},
			}},
			code:  "invalid-value",
			field: "data.endpoints[0].port",
		},
		{
			name: "port above range",
			data: ReadyData{Target: ReadyTargetServer, Endpoints: []ReadyEndpoint{
				{Scheme: ReadySchemeHTTP, Host: "localhost", Port: 65536},
			}},
			code:  "invalid-value",
			field: "data.endpoints[0].port",
		},
		{
			name: "relative path",
			data: ReadyData{Target: ReadyTargetServer, Endpoints: []ReadyEndpoint{
				{Scheme: ReadySchemeHTTP, Host: "localhost", Port: 3000, Path: "api"},
			}},
			code:  "invalid-value",
			field: "data.endpoints[0].path",
		},
		{
			name: "endpoints out of canonical order",
			data: ReadyData{Target: ReadyTargetServer, Endpoints: []ReadyEndpoint{
				{Scheme: ReadySchemeHTTPS, Host: "localhost", Port: 8443},
				{Scheme: ReadySchemeHTTP, Host: "localhost", Port: 3000},
			}},
			code:  "non-canonical-order",
			field: "data.endpoints[1]",
		},
		{
			name: "duplicate endpoints",
			data: ReadyData{Target: ReadyTargetServer, Endpoints: []ReadyEndpoint{
				{Scheme: ReadySchemeHTTP, Host: "localhost", Port: 3000},
				{Scheme: ReadySchemeHTTP, Host: "localhost", Port: 3000},
			}},
			code:  "duplicate-endpoint",
			field: "data.endpoints[1]",
		},
		{
			name:  "negative duration",
			data:  ReadyData{Target: ReadyTargetWorkload, DurationMs: -1},
			code:  "invalid-value",
			field: "data.durationMs",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diags := ValidateEvent(readyEvent(t, tc.data))
			if !diag.HasErrors(diags) {
				t.Fatalf("expected errors, got none")
			}
			found := false
			for _, d := range diags {
				if d.Code == tc.code && d.Field == tc.field {
					found = true
				}
			}
			if !found {
				t.Errorf("want diagnostic {code=%q field=%q}, got %v", tc.code, tc.field, diags)
			}
		})
	}
}

func TestValidateReadyEvent_MissingData(t *testing.T) {
	diags := ValidateEvent(&Event{V: ProtocolVersion2, Type: EventReady})
	if !diag.HasErrors(diags) {
		t.Fatal("ready event without data must be rejected")
	}
	if diags[0].Code != "required-field" || diags[0].Field != "data" {
		t.Errorf("got %v, want required-field on data", diags)
	}
}

func TestValidateReadyEvent_MalformedData(t *testing.T) {
	evt := &Event{V: ProtocolVersion2, Type: EventReady, Data: json.RawMessage(`{"target":42}`)}
	diags := ValidateEvent(evt)
	if !diag.HasErrors(diags) {
		t.Fatal("ready event with a malformed payload must be rejected")
	}
	if diags[0].Code != "invalid-ready-data" {
		t.Errorf("got %v, want invalid-ready-data", diags)
	}
}

// TestV1RejectsReadyEvent is the additivity guard on the event vocabulary:
// `ready` exists only at v2, so a v1 emitter cannot smuggle it in.
func TestV1RejectsReadyEvent(t *testing.T) {
	evt := readyEvent(t, ReadyData{Target: ReadyTargetWorkload})
	evt.V = ProtocolVersion

	diags := ValidateEvent(evt)
	if !diag.HasErrors(diags) {
		t.Fatal("a ready event at v1 must be rejected as an unknown type")
	}
	if diags[0].Code != "invalid-event-type" {
		t.Errorf("got %v, want invalid-event-type", diags)
	}
}

// TestV2AcceptsEveryV1Type pins that v2 is a superset: no v1 event shape is
// dropped by the version bump.
func TestV2AcceptsEveryV1Type(t *testing.T) {
	for typ := range validEventTypes {
		if !validEventTypesV2[typ] {
			t.Errorf("v1 event type %q missing from the v2 vocabulary", typ)
		}
	}
	if len(validEventTypesV2) != len(validEventTypes)+1 {
		t.Errorf("v2 vocabulary has %d types, want %d (v1 + ready)",
			len(validEventTypesV2), len(validEventTypes)+1)
	}
	if !validEventTypesV2[EventReady] || validEventTypes[EventReady] {
		t.Error("ready must be in the v2 vocabulary and absent from v1")
	}
}

func TestValidateEvent_UnknownVersion(t *testing.T) {
	diags := ValidateEvent(&Event{V: 3, Type: EventSummary, Message: "hi"})
	if !diag.HasErrors(diags) {
		t.Fatal("an unknown protocol version must be rejected")
	}
	if diags[0].Code != "invalid-version" {
		t.Errorf("got %v, want invalid-version", diags)
	}
	if !strings.Contains(diags[0].Message, "1 or 2") {
		t.Errorf("message %q should name both known versions", diags[0].Message)
	}
}

func TestValidateEventStream_MixedVersions(t *testing.T) {
	events := []*Event{
		{V: ProtocolVersion, Type: EventLog, Level: "info", Message: "v1"},
		{V: ProtocolVersion2, Type: EventResult},
	}
	diags := ValidateEventStream(events)
	found := false
	for _, d := range diags {
		if d.Code == "mixed-protocol-version" {
			found = true
			if !strings.Contains(d.Message, "1, 2") {
				t.Errorf("message %q should list the versions in order", d.Message)
			}
		}
	}
	if !found {
		t.Errorf("a stream mixing versions must be rejected, got %v", diags)
	}
}

func TestValidateEventStream_SingleVersionAccepted(t *testing.T) {
	for _, version := range []int{ProtocolVersion, ProtocolVersion2} {
		events := []*Event{
			{V: version, Type: EventLog, Level: "info", Message: "hello"},
			{V: version, Type: EventResult},
		}
		if diags := ValidateEventStream(events); diag.HasErrors(diags) {
			t.Errorf("uniform v%d stream rejected: %v", version, diags)
		}
	}
}

func TestReadyEndpoint_URL(t *testing.T) {
	cases := []struct {
		endpoint ReadyEndpoint
		want     string
	}{
		{ReadyEndpoint{Scheme: ReadySchemeHTTP, Host: "localhost", Port: 3000}, "http://localhost:3000"},
		{ReadyEndpoint{Scheme: ReadySchemeHTTPS, Host: "0.0.0.0", Port: 8443, Path: "/api"}, "https://0.0.0.0:8443/api"},
		{ReadyEndpoint{Scheme: ReadySchemeHTTP, Host: "::1", Port: 3000}, "http://[::1]:3000"},
		{ReadyEndpoint{Scheme: ReadySchemeHTTPS, Host: "::1", Port: 8443, Path: "/api"}, "https://[::1]:8443/api"},
		{ReadyEndpoint{Scheme: ReadySchemeHTTP, Host: "localhost", Port: 3000, Path: "/"}, "http://localhost:3000"},
		{ReadyEndpoint{Scheme: ReadySchemeTCP, Host: "db"}, "tcp://db"},
	}
	for _, tc := range cases {
		if got := tc.endpoint.URL(); got != tc.want {
			t.Errorf("URL() = %q, want %q", got, tc.want)
		}
	}
}

func TestSortReadyEndpoints_Canonical(t *testing.T) {
	endpoints := []ReadyEndpoint{
		{Scheme: ReadySchemeHTTP, Host: "localhost", Port: 3000, Path: "/b"},
		{Scheme: ReadySchemeHTTP, Host: "localhost", Port: 3000, Path: "/a"},
		{Scheme: ReadySchemeHTTP, Host: "localhost", Port: 80},
		{Scheme: ReadySchemeHTTP, Host: "a-host", Port: 3000},
		{Scheme: ReadySchemeGRPC, Host: "localhost", Port: 50051},
	}
	SortReadyEndpoints(endpoints)

	want := []string{
		"grpc://localhost:50051",
		"http://a-host:3000",
		"http://localhost:80",
		"http://localhost:3000/a",
		"http://localhost:3000/b",
	}
	for i, endpoint := range endpoints {
		if got := endpoint.URL(); got != want[i] {
			t.Errorf("endpoint[%d] = %q, want %q", i, got, want[i])
		}
	}

	// The order is total and stable: sorting an already-sorted list is a no-op.
	sorted := make([]ReadyEndpoint, len(endpoints))
	copy(sorted, endpoints)
	SortReadyEndpoints(sorted)
	for i := range sorted {
		if CompareReadyEndpoints(sorted[i], endpoints[i]) != 0 {
			t.Errorf("sort is not idempotent at index %d", i)
		}
	}
	if CompareReadyEndpoints(endpoints[0], endpoints[0]) != 0 {
		t.Error("an endpoint must compare equal to itself")
	}
}

func TestEmitter_Ready(t *testing.T) {
	var buf bytes.Buffer
	unsorted := []ReadyEndpoint{
		{Scheme: ReadySchemeHTTPS, Host: "localhost", Port: 8443},
		{Scheme: ReadySchemeHTTP, Host: "localhost", Port: 3000},
	}
	err := NewEmitterForVersion(&buf, ProtocolVersion2).Ready(ReadyData{
		Target:     ReadyTargetServer,
		Name:       "api",
		Endpoints:  unsorted,
		DurationMs: 42,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The caller's slice must not be reordered under it.
	if unsorted[0].Scheme != ReadySchemeHTTPS {
		t.Error("Ready mutated the caller's endpoint slice")
	}

	evt, err := ParseEvent(bytes.TrimSpace(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if evt.V != ProtocolVersion2 {
		t.Errorf("emitted v = %d, want %d", evt.V, ProtocolVersion2)
	}
	if evt.Type != EventReady {
		t.Errorf("emitted type = %q, want ready", evt.Type)
	}
	if evt.Time == "" {
		t.Error("emitted event has no timestamp")
	}
	if diags := ValidateEvent(evt); diag.HasErrors(diags) {
		t.Errorf("emitted ready event violates the protocol: %v", diags)
	}

	payload, err := ReadyPayload(evt)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Endpoints[0].Scheme != ReadySchemeHTTP {
		t.Errorf("emitter did not canonicalize endpoint order: %+v", payload.Endpoints)
	}
	if payload.Name != "api" || payload.DurationMs != 42 {
		t.Errorf("payload lost members: %+v", payload)
	}
}

// TestEmitter_ReadyDeterministic pins the determinism claim: the same readiness
// facts produce the same bytes no matter what order the listeners bound in.
func TestEmitter_ReadyDeterministic(t *testing.T) {
	emit := func(endpoints []ReadyEndpoint) string {
		var buf bytes.Buffer
		if err := NewEmitterForVersion(&buf, ProtocolVersion2).Ready(ReadyData{
			Target:    ReadyTargetServer,
			Endpoints: endpoints,
		}); err != nil {
			t.Fatal(err)
		}
		line := map[string]any{}
		if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &line); err != nil {
			t.Fatal(err)
		}
		delete(line, "time") // wall clock is the one member that legitimately varies
		out, err := json.Marshal(line)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}

	a := emit([]ReadyEndpoint{
		{Scheme: ReadySchemeHTTP, Host: "localhost", Port: 3000},
		{Scheme: ReadySchemeGRPC, Host: "localhost", Port: 50051},
	})
	b := emit([]ReadyEndpoint{
		{Scheme: ReadySchemeGRPC, Host: "localhost", Port: 50051},
		{Scheme: ReadySchemeHTTP, Host: "localhost", Port: 3000},
	})
	if a != b {
		t.Errorf("readiness serialization is order-dependent:\n%s\n%s", a, b)
	}
}

func TestReadyPayload_AbsentAndNil(t *testing.T) {
	payload, err := ReadyPayload(nil)
	if err != nil || payload != nil {
		t.Errorf("ReadyPayload(nil) = %v, %v; want nil, nil", payload, err)
	}
	payload, err = ReadyPayload(&Event{V: ProtocolVersion2, Type: EventReady})
	if err != nil || payload != nil {
		t.Errorf("ReadyPayload(no data) = %v, %v; want nil, nil", payload, err)
	}
	if _, err := ReadyPayload(&Event{Data: json.RawMessage(`{"target":1}`)}); err == nil {
		t.Error("a malformed payload must return an error")
	}
}

func TestExtractReadyData(t *testing.T) {
	payload, err := ExtractReadyData(nil)
	if err != nil || payload != nil {
		t.Errorf("ExtractReadyData(nil) = %v, %v; want nil, nil", payload, err)
	}

	payload, err = ExtractReadyData(map[string]any{
		"target": ReadyTargetServer,
		"endpoints": []any{
			map[string]any{"scheme": "http", "host": "localhost", "port": 3000},
		},
		"durationMs": 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if payload.Target != ReadyTargetServer || len(payload.Endpoints) != 1 {
		t.Fatalf("decoded payload = %+v", payload)
	}
	if got := payload.Endpoints[0].URL(); got != "http://localhost:3000" {
		t.Errorf("endpoint = %q, want http://localhost:3000", got)
	}
	if payload.DurationMs != 7 {
		t.Errorf("durationMs = %d, want 7", payload.DurationMs)
	}

	if _, err := ExtractReadyData(map[string]any{"target": 1}); err == nil {
		t.Error("a malformed data map must return an error")
	}
}

// TestOuterSessionStreamIsNotOwnedHere pins the ownership boundary recorded in
// doc/adr/0002-delete-the-producerless-stream-envelopes.md: this package owns
// the INNER subprocess event only. The outer session-stream record is
// protocols/cli's SessionStreamRecord (task:* records, protocolVersion 2).
//
// It reads the package's own source because the thing being asserted is an
// ABSENCE. The unversioned job:*/session:end envelopes this package used to
// declare were deleted — with their schema and corpus — once their last producer
// was removed. A second declaration of them here would mean two packages own the
// outer record again, which is the state that deletion resolved.
func TestOuterSessionStreamIsNotOwnedHere(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	// The outer record's discriminators. A source file declaring one of these as
	// a Go string literal is this package re-growing the envelope.
	forbidden := []string{`"job:start"`, `"job:end"`, `"session:end"`}
	scanned := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, readErr := os.ReadFile(name)
		if readErr != nil {
			t.Fatal(readErr)
		}
		scanned++
		for _, token := range forbidden {
			if strings.Contains(string(data), token) {
				t.Errorf("%s declares the outer session-stream discriminator %s — that record is "+
					"protocols/cli's SessionStreamRecord (task:*, protocolVersion 2). This package "+
					"owns the inner subprocess event only; see "+
					"doc/adr/0002-delete-the-producerless-stream-envelopes.md.", name, token)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no production sources — the assertion would pass vacuously")
	}
}

func TestValidReadyEndpoint(t *testing.T) {
	cases := map[string]struct {
		endpoint ReadyEndpoint
		want     bool
	}{
		"http":           {ReadyEndpoint{Scheme: ReadySchemeHTTP, Host: "localhost", Port: 8080}, true},
		"with path":      {ReadyEndpoint{Scheme: ReadySchemeHTTPS, Host: "h", Port: 443, Path: "/api"}, true},
		"port 0":         {ReadyEndpoint{Scheme: ReadySchemeHTTP, Host: "localhost"}, false},
		"no host":        {ReadyEndpoint{Scheme: ReadySchemeHTTP, Port: 80}, false},
		"unknown scheme": {ReadyEndpoint{Scheme: "amqp", Host: "h", Port: 5672}, false},
		"relative path":  {ReadyEndpoint{Scheme: ReadySchemeHTTP, Host: "h", Port: 80, Path: "api"}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := ValidReadyEndpoint(tc.endpoint); got != tc.want {
				t.Errorf("ValidReadyEndpoint(%+v) = %v, want %v", tc.endpoint, got, tc.want)
			}
		})
	}
}
