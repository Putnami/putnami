package clientcontract

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

func TestSharedPageTransportOwnerConfirmationJSON(t *testing.T) {
	spectest.Proves(t, "client-contract/first-party-generated-clients", "shared-page-transport", "absent-owner-confirmation-is-omitted-without-dropping-zero-watermark")
	for _, test := range []struct {
		name      string
		confirmed time.Time
		want      string
	}{
		{"absent", time.Time{}, `{"watermark":0,"relation":"accounts","rows":[]}`},
		{"confirmed", time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC), `{"watermark":0,"ownerConfirmedAt":"2026-09-21T00:00:00Z","relation":"accounts","rows":[]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			page := Page[json.RawMessage]{OwnerConfirmedAt: test.confirmed, Relation: "accounts", Rows: json.RawMessage(`[]`)}
			raw, err := json.Marshal(page)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != test.want {
				t.Fatalf("page JSON = %s, want %s", raw, test.want)
			}
		})
	}
}

func TestSharedPageTransportConformance(t *testing.T) {
	spectest.Proves(t, "client-contract/first-party-generated-clients", "shared-page-transport", "shared-page-schemas-preserve-owner-specialization-and-exact-wire-types")
	raw, err := os.ReadFile("fixtures/page/schemas.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct{ Query, Envelope Schema }
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	query, envelope := PageTransportSchemas()
	if !reflect.DeepEqual(query, fixture.Query) || !reflect.DeepEqual(envelope, fixture.Envelope) {
		t.Fatal("shared page schemas differ from the cross-language corpus")
	}
	if err := ValidatePageTransportSchemas(query, envelope); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*Schema, *Schema)
	}{
		{"renamed cursor", func(q, _ *Schema) {
			q.Properties["cursor"] = q.Properties[QueryParamAfterKey]
			delete(q.Properties, QueryParamAfterKey)
		}},
		{"missing required watermark", func(_, e *Schema) { e.Required = []string{"relation", "rows"} }},
		{"watermark loses width", func(_, e *Schema) { e.Properties["watermark"] = Schema{Type: "number"} }},
		{"negative watermark", func(_, e *Schema) {
			n := json.Number("-1")
			s := e.Properties["watermark"]
			s.Minimum = &n
			e.Properties["watermark"] = s
		}},
		{"zero limit", func(q, _ *Schema) {
			n := json.Number("0")
			s := q.Properties[QueryParamLimit]
			s.Minimum = &n
			q.Properties[QueryParamLimit] = s
		}},
		{"nullable relation", func(_, e *Schema) { yes := true; e.Properties["relation"] = Schema{Type: "string", Nullable: &yes} }},
		{"invalid rows", func(_, e *Schema) { e.Properties["rows"] = Schema{} }},
		{"nullable envelope", func(_, e *Schema) { yes := true; e.Nullable = &yes }},
	} {
		t.Run(test.name, func(t *testing.T) {
			q, e := PageTransportSchemas()
			test.mutate(&q, &e)
			if err := ValidatePageTransportSchemas(q, e); err == nil {
				t.Fatal("incompatible owner accepted")
			}
		})
	}
	query.Properties[QueryParamRelation] = Schema{Type: "string", Enum: []json.RawMessage{json.RawMessage(`"accounts"`)}}
	envelope.Properties["rows"] = Schema{Type: "array", Items: &Schema{Type: "string"}}
	if err := ValidatePageTransportSchemas(query, envelope); err != nil {
		t.Fatalf("owner specialization: %v", err)
	}
	var page Page[json.RawMessage]
	if err := json.Unmarshal([]byte(`{"watermark":9007199254740993,"relation":"accounts","rows":[],"nextKey":"opaque"}`), &page); err != nil {
		t.Fatal(err)
	}
	if page.Watermark != 9007199254740993 || page.NextKey == "" || string(page.Rows) != "[]" {
		t.Fatalf("page lost integer/cursor/rows: %+v", page)
	}
}

func TestOperationPathsUseTheSharedCorpus(t *testing.T) {
	spectest.Proves(t, "client-contract/first-party-generated-clients", "shared-page-transport", "caller-operation-paths-refuse-unknown-or-unsafe-targets")
	raw, err := os.ReadFile("fixtures/page/paths.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct{ Valid, Invalid []string }
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	op := OperationV1{Stream: StreamUnary, Transports: []Transport{{Protocol: TransportRESTJSON, Encoding: EncodingJSON, Path: "/page"}}}
	for _, path := range corpus.Valid {
		if err := ValidateOperationPaths(map[string]OperationV1{"page": op}, map[string]string{"page": path}); err != nil {
			t.Fatalf("%q: %v", path, err)
		}
	}
	for _, path := range corpus.Invalid {
		if err := ValidateOperationPaths(map[string]OperationV1{"page": op}, map[string]string{"page": path}); err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
	if err := ValidateOperationPaths(map[string]OperationV1{"page": op}, map[string]string{"other": "/x"}); err == nil {
		t.Fatal("unknown operation accepted")
	}
	for _, mutate := range []func(*OperationV1){func(o *OperationV1) { o.Stream = StreamServer }, func(o *OperationV1) { o.Transports[0].Protocol = TransportConnect }, func(o *OperationV1) { o.Transports[0].Path = "/page/{relation}" }} {
		copy := op
		copy.Transports = append([]Transport(nil), op.Transports...)
		mutate(&copy)
		if err := ValidateOperationPaths(map[string]OperationV1{"page": copy}, map[string]string{"page": "/x"}); err == nil {
			t.Fatal("incompatible transport accepted")
		}
	}
}
