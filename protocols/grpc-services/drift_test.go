package grpcservices

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestSchemaFieldParity(t *testing.T) {
	spectest.Proves(t, "grpc-services/service-inventory-contract", "the-identifiers-are-pinned", "the-schema-members-and-enums-match-the-go-types")
	data, err := os.ReadFile("schemas/grpc-services.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]any `json:"properties"`
		Defs       map[string]struct {
			Properties map[string]any `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	assertJSONFields(t, "Manifest", schema.Properties, Manifest{})
	assertJSONFields(t, "Service", schema.Defs["serviceBase"].Properties, Service{})
	assertJSONFields(t, "Method", schema.Defs["method"].Properties, Method{})
	assertJSONFields(t, "Provenance", schema.Defs["provenance"].Properties, Provenance{})
}

func TestSchemaEnumParity(t *testing.T) {
	spectest.Proves(t, "grpc-services/service-inventory-contract", "the-identifiers-are-pinned", "the-schema-members-and-enums-match-the-go-types")
	data, err := os.ReadFile("schemas/grpc-services.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Defs map[string]struct {
			Enum []string `json:"enum"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	assertSet(t, "transport", schema.Defs["transport"].Enum,
		[]string{string(TransportGRPC), string(TransportConnect)})
	kinds := make([]string, 0, len(validSourceKinds))
	for kind := range validSourceKinds {
		kinds = append(kinds, string(kind))
	}
	assertSet(t, "sourceKind", schema.Defs["sourceKind"].Enum, kinds)
}

func assertJSONFields(t *testing.T, name string, schemaFields map[string]any, value any) {
	t.Helper()
	want := make([]string, 0, len(schemaFields))
	for field := range schemaFields {
		want = append(want, field)
	}
	typeOf := reflect.TypeOf(value)
	got := make([]string, 0, typeOf.NumField())
	for i := range typeOf.NumField() {
		got = append(got, strings.Split(typeOf.Field(i).Tag.Get("json"), ",")[0])
	}
	assertSet(t, name+" fields", got, want)
}

func assertSet(t *testing.T, name string, got, want []string) {
	t.Helper()
	got = append([]string(nil), got...)
	want = append([]string(nil), want...)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s differ: Go=%v schema=%v", name, got, want)
	}
}
