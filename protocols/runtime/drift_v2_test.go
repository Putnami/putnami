package runtime

import (
	"encoding/json"
	"os"
	"sort"
	"testing"
)

// v2Schema is the decoded shape of schemas/event-v2.json the drift tests read.
type v2Schema struct {
	Properties map[string]struct {
		Const *int `json:"const"`
	} `json:"properties"`
	Defs map[string]struct {
		Enum       []string `json:"enum"`
		Required   []string `json:"required"`
		Properties map[string]struct {
			Enum    []string `json:"enum"`
			Minimum *int     `json:"minimum"`
			Maximum *int     `json:"maximum"`
			Pattern string   `json:"pattern"`
		} `json:"properties"`
	} `json:"$defs"`
}

func loadV2Schema(t *testing.T) v2Schema {
	t.Helper()
	data, err := os.ReadFile("schemas/event-v2.json")
	if err != nil {
		t.Fatalf("read v2 schema: %v", err)
	}
	var schema v2Schema
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse v2 schema: %v", err)
	}
	return schema
}

func TestDriftV2_ProtocolVersionConst(t *testing.T) {
	schema := loadV2Schema(t)
	v, ok := schema.Properties["v"]
	if !ok || v.Const == nil {
		t.Fatal("v2 schema has no const for the v property")
	}
	if *v.Const != ProtocolVersion2 {
		t.Errorf("v2 schema pins v = %d, Go says %d", *v.Const, ProtocolVersion2)
	}
	ready, ok := schema.Defs["ReadyEvent"]
	if !ok {
		t.Fatal("no $defs/ReadyEvent in the v2 schema")
	}
	// A ready event without data has nothing a consumer can act on.
	requiresData := false
	for _, field := range ready.Required {
		if field == "data" {
			requiresData = true
		}
	}
	if !requiresData {
		t.Error("the v2 schema must require data on a ready event, as the Go validator does")
	}
}

func TestDriftV2_EventTypes(t *testing.T) {
	schema := loadV2Schema(t)
	def, ok := schema.Defs["EventType"]
	if !ok {
		t.Fatal("no $defs/EventType in the v2 schema")
	}
	schemaTypes := append([]string(nil), def.Enum...)
	sort.Strings(schemaTypes)

	goTypes := make([]string, 0, len(validEventTypesV2))
	for typ := range validEventTypesV2 {
		goTypes = append(goTypes, string(typ))
	}
	sort.Strings(goTypes)

	assertEnumSync(t, "v2 event type", schemaTypes, goTypes)
}

func TestDriftV2_ReadyVocabularies(t *testing.T) {
	schema := loadV2Schema(t)

	readyData, ok := schema.Defs["ReadyData"]
	if !ok {
		t.Fatal("no $defs/ReadyData in the v2 schema")
	}
	targets := append([]string(nil), readyData.Properties["target"].Enum...)
	sort.Strings(targets)
	goTargets := append([]string(nil), ValidReadyTargets...)
	sort.Strings(goTargets)
	assertEnumSync(t, "ready target", targets, goTargets)

	endpoint, ok := schema.Defs["ReadyEndpoint"]
	if !ok {
		t.Fatal("no $defs/ReadyEndpoint in the v2 schema")
	}
	schemes := append([]string(nil), endpoint.Properties["scheme"].Enum...)
	sort.Strings(schemes)
	goSchemes := append([]string(nil), ValidReadyEndpointSchemes...)
	sort.Strings(goSchemes)
	assertEnumSync(t, "ready endpoint scheme", schemes, goSchemes)

	port := endpoint.Properties["port"]
	if port.Minimum == nil || *port.Minimum != 1 {
		t.Errorf("schema port minimum = %v, Go validator enforces 1", port.Minimum)
	}
	if port.Maximum == nil || *port.Maximum != maxPort {
		t.Errorf("schema port maximum = %v, Go validator enforces %d", port.Maximum, maxPort)
	}
	if got := endpoint.Properties["path"].Pattern; got != "^/" {
		t.Errorf("schema path pattern = %q, Go validator requires a leading slash", got)
	}
}

// TestDriftV2_ExportedVocabulariesAreCanonical pins that the exported closed
// vocabularies are sorted: they are what conformance harnesses enumerate, and a
// vocabulary in authoring order would make their output depend on this file's
// line order.
func TestDriftV2_ExportedVocabulariesAreCanonical(t *testing.T) {
	for name, values := range map[string][]string{
		"ValidReadyTargets":         ValidReadyTargets,
		"ValidReadyEndpointSchemes": ValidReadyEndpointSchemes,
	} {
		if !sort.StringsAreSorted(values) {
			t.Errorf("%s is not in canonical (sorted) order: %v", name, values)
		}
	}
	if len(validReadyTargets) != len(ValidReadyTargets) {
		t.Error("the ready-target lookup set and its exported slice disagree")
	}
	if len(validReadyEndpointSchemes) != len(ValidReadyEndpointSchemes) {
		t.Error("the endpoint-scheme lookup set and its exported slice disagree")
	}
}

// TestDriftV2_V1SchemaFrozen is the additivity guard on the schema corpus: the
// v1 schema keeps pinning v = 1 and must never learn the v2-only event type.
func TestDriftV2_V1SchemaFrozen(t *testing.T) {
	data, err := os.ReadFile("schemas/event.json")
	if err != nil {
		t.Fatalf("read v1 schema: %v", err)
	}
	var schema v2Schema
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse v1 schema: %v", err)
	}
	v, ok := schema.Properties["v"]
	if !ok || v.Const == nil || *v.Const != ProtocolVersion {
		t.Fatalf("v1 schema no longer pins v = %d", ProtocolVersion)
	}
	for _, typ := range schema.Defs["EventType"].Enum {
		if typ == string(EventReady) {
			t.Errorf("the v1 schema lists %q; readiness is v2-only", EventReady)
		}
	}
}
