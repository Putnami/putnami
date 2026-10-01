package httproutes

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestSchemaFieldParity(t *testing.T) {
	data, err := os.ReadFile("schemas/http-routes.json")
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
	assertJSONFields(t, "Route", schema.Defs["routeBase"].Properties, Route{})
	assertJSONFields(t, "Provenance", schema.Defs["provenance"].Properties, Provenance{})
}

func assertJSONFields(t *testing.T, name string, schemaFields map[string]any, value any) {
	t.Helper()
	want := make([]string, 0, len(schemaFields))
	for field := range schemaFields {
		want = append(want, field)
	}
	sort.Strings(want)
	typeOf := reflect.TypeOf(value)
	got := make([]string, 0, typeOf.NumField())
	for i := 0; i < typeOf.NumField(); i++ {
		got = append(got, strings.Split(typeOf.Field(i).Tag.Get("json"), ",")[0])
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s fields differ: Go=%v schema=%v", name, got, want)
	}
}
