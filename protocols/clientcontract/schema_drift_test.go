package clientcontract

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestSchemaIdentityAndVersionMatchContract(t *testing.T) {
	schema := loadSchema(t)
	if got := schema["$id"]; got != SchemaURL {
		t.Fatalf("schema $id = %v, want %q", got, SchemaURL)
	}
	defs := schema["$defs"].(map[string]any)
	document := defs["document"].(map[string]any)
	properties := document["properties"].(map[string]any)
	version := properties["protocolVersion"].(map[string]any)
	if got := int(version["const"].(float64)); got != ProtocolVersion {
		t.Fatalf("schema protocolVersion = %d, want %d", got, ProtocolVersion)
	}
}

func TestSchemaPropertiesMatchGoWireTypes(t *testing.T) {
	spectest.Proves(t, "client-contract/first-party-generated-clients", "generated-inventory", "published-schemas-match-the-wire-types")
	schema := loadSchema(t)
	defs := schema["$defs"].(map[string]any)
	cases := []struct {
		definition string
		value      any
	}{
		{"document", DocumentV1{}},
		{"service", Service{}},
		{"credentialProfile", CredentialProfile{}},
		{"operation", OperationV1{}},
		{"messageShapes", MessageShapes{}},
		{"transport", Transport{}},
		{"websocketTransport", WebSocketTransport{}},
		{"sseTransport", SSETransport{}},
		{"sseContinuation", SSEContinuation{}},
		{"sseCursor", SSECursor{}},
		{"security", Security{}},
		{"authorization", Authorization{}},
		{"declaredError", DeclaredError{}},
		{"idempotency", Idempotency{}},
		{"resilience", ResiliencePolicy{}},
		{"cachePolicy", CachePolicy{}},
		{"protobufDescriptor", ProtobufDescriptor{}},
		{"protobufService", ProtobufService{}},
		{"protobufMethod", ProtobufMethod{}},
		{"protobufMessage", ProtobufMessage{}},
		{"protobufField", ProtobufField{}},
		{"protobufMap", ProtobufMap{}},
		{"protobufEnum", ProtobufEnum{}},
		{"schema", Schema{}},
	}
	for _, tc := range cases {
		t.Run(tc.definition, func(t *testing.T) {
			got := schemaPropertyNames(defs[tc.definition])
			want := goJSONFieldNames(reflect.TypeOf(tc.value))
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("properties drift:\n schema: %v\n     Go: %v", got, want)
			}
		})
	}
}

func TestGeneratedManifestSchemaMatchesGoWireTypes(t *testing.T) {
	data, err := os.ReadFile("schemas/generated-client-manifest-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	if got := schema["$id"]; got != GeneratedManifestSchemaURL {
		t.Fatalf("schema $id = %v, want %q", got, GeneratedManifestSchemaURL)
	}
	defs := schema["$defs"].(map[string]any)
	cases := []struct {
		definition any
		value      any
	}{
		{schema, GeneratedClientManifestV1{}},
		{defs["binding"], GeneratedBinding{}},
		{defs["bindingClient"], GeneratedBindingClient{}},
		{defs["operation"], GeneratedOperation{}},
		{defs["file"], GeneratedFile{}},
	}
	for _, tc := range cases {
		got := schemaPropertyNames(tc.definition)
		want := goJSONFieldNames(reflect.TypeOf(tc.value))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("properties drift for %T: schema=%v Go=%v", tc.value, got, want)
		}
	}
}

// TestGeneratedManifestSchemaEnumeratesTheImplementedRuntimeCapabilities pins
// the published capability vocabulary to the one the Go reader enforces, so a
// capability cannot be added to one without the other.
func TestGeneratedManifestSchemaEnumeratesTheImplementedRuntimeCapabilities(t *testing.T) {
	spectest.Proves(t, "client-contract/first-party-generated-clients", "response-cache-policy", "the-published-schemas-carry-the-cache-policy-and-the-capability-vocabulary")
	data, err := os.ReadFile("schemas/generated-client-manifest-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			RuntimeCapabilities struct {
				Items struct {
					Enum []string `json:"enum"`
				} `json:"items"`
			} `json:"runtimeCapabilities"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	got := schema.Properties.RuntimeCapabilities.Items.Enum
	want := make([]string, 0, len(ImplementedRuntimeCapabilities))
	for _, capability := range ImplementedRuntimeCapabilities {
		want = append(want, string(capability))
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime capability enum drift: schema=%v Go=%v", got, want)
	}
	defs := loadSchema(t)["$defs"].(map[string]any)
	resilience := defs["resilience"].(map[string]any)["properties"].(map[string]any)
	if _, ok := resilience["cache"]; !ok {
		t.Fatal("the resilience schema does not publish the cache policy")
	}
	required := defs["cachePolicy"].(map[string]any)["required"].([]any)
	if len(required) != 1 || required[0] != "freshMs" {
		t.Fatalf("cachePolicy.required = %v, want [freshMs]", required)
	}
}

func loadSchema(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("schemas/x-putnami-client-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	return schema
}

func schemaPropertyNames(raw any) []string {
	definition := raw.(map[string]any)
	properties := map[string]bool{}
	if direct, ok := definition["properties"].(map[string]any); ok {
		for name := range direct {
			properties[name] = true
		}
	}
	if alternatives, ok := definition["oneOf"].([]any); ok {
		for _, alternative := range alternatives {
			if candidate, ok := alternative.(map[string]any); ok {
				if nested, ok := candidate["properties"].(map[string]any); ok {
					for name := range nested {
						properties[name] = true
					}
				}
			}
		}
	}
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func goJSONFieldNames(typ reflect.Type) []string {
	names := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		tag := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if tag != "" && tag != "-" {
			names = append(names, tag)
		}
	}
	sort.Strings(names)
	return names
}
