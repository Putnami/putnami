package infra

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// schemaDefProperties extracts property names from a schema $defs entry
// in the schema file at schemaPath.
func schemaDefProperties(t *testing.T, schemaPath, defName string) []string {
	t.Helper()
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("read schema %s: %v", schemaPath, err)
	}
	var schema struct {
		Defs map[string]struct {
			Properties map[string]any `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse schema %s: %v", schemaPath, err)
	}
	def, ok := schema.Defs[defName]
	if !ok {
		t.Fatalf("no $defs/%s in %s", defName, schemaPath)
	}
	names := make([]string, 0, len(def.Properties))
	for name := range def.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// schemaSubtreeProperties returns every property name reachable from a schema
// $defs entry, following $ref edges. Unlike schemaDefProperties it does not
// stop at the named definition, so a field buried one level down in a
// referenced definition is still visible to a guard.
func schemaSubtreeProperties(t *testing.T, schemaPath, rootDef string) []string {
	t.Helper()
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("read schema %s: %v", schemaPath, err)
	}
	var schema struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse schema %s: %v", schemaPath, err)
	}
	if _, ok := schema.Defs[rootDef]; !ok {
		t.Fatalf("no $defs/%s in %s", rootDef, schemaPath)
	}

	var names []string
	visited := map[string]bool{}
	var visit func(defName string)
	visit = func(defName string) {
		if visited[defName] {
			return
		}
		visited[defName] = true
		raw, ok := schema.Defs[defName]
		if !ok {
			return
		}
		var def struct {
			Properties map[string]struct {
				Ref string `json:"$ref"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &def); err != nil {
			t.Fatalf("parse $defs/%s in %s: %v", defName, schemaPath, err)
		}
		for name, prop := range def.Properties {
			names = append(names, name)
			if ref := strings.TrimPrefix(prop.Ref, "#/$defs/"); ref != "" && ref != prop.Ref {
				visit(ref)
			}
		}
	}
	visit(rootDef)
	sort.Strings(names)
	return names
}

// goTypeSubtreeJSONFields returns every json tag name reachable from the
// struct underlying v, descending into nested struct fields. The schema-side
// walker follows $ref, so the Go-side walker has to follow the equivalent
// edge or the two guards would not cover the same ground.
func goTypeSubtreeJSONFields(t *testing.T, v any) []string {
	t.Helper()
	var names []string
	visited := map[reflect.Type]bool{}
	var visit func(typ reflect.Type)
	visit = func(typ reflect.Type) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || visited[typ] {
			return
		}
		visited[typ] = true
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			tag := field.Tag.Get("json")
			if tag == "" || tag == "-" {
				continue
			}
			names = append(names, strings.Split(tag, ",")[0])
			visit(field.Type)
		}
	}
	visit(reflect.TypeOf(v))
	sort.Strings(names)
	return names
}

// goTypeJSONFields extracts json tag names from the struct underlying v.
func goTypeJSONFields(t *testing.T, v any) []string {
	t.Helper()
	typ := reflect.TypeOf(v)
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	var names []string
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		tag := field.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func assertFieldParity(t *testing.T, typeName string, schemaFields, goFields []string) {
	t.Helper()
	schemaSet := make(map[string]bool, len(schemaFields))
	for _, f := range schemaFields {
		schemaSet[f] = true
	}
	goSet := make(map[string]bool, len(goFields))
	for _, f := range goFields {
		goSet[f] = true
	}
	for _, f := range schemaFields {
		if !goSet[f] {
			t.Errorf("%s: field %q exists in schema but not in Go type", typeName, f)
		}
	}
	for _, f := range goFields {
		if !schemaSet[f] {
			t.Errorf("%s: field %q exists in Go type but not in schema", typeName, f)
		}
	}
}

const (
	perProjectSchemaPath = "schemas/infra.json"
	aggregatedSchemaPath = "schemas/infra-aggregated.json"
	overridesSchemaPath  = "schemas/infra-overrides.json"
)

func TestDrift_Database(t *testing.T) {
	assertFieldParity(t, "Database",
		schemaDefProperties(t, perProjectSchemaPath, "database"),
		goTypeJSONFields(t, Database{}))
}

func TestDrift_Events(t *testing.T) {
	assertFieldParity(t, "Events",
		schemaDefProperties(t, perProjectSchemaPath, "events"),
		goTypeJSONFields(t, Events{}))
}

func TestDrift_Subscription(t *testing.T) {
	assertFieldParity(t, "Subscription",
		schemaDefProperties(t, perProjectSchemaPath, "subscription"),
		goTypeJSONFields(t, Subscription{}))
}

func TestDrift_StorageBucket(t *testing.T) {
	assertFieldParity(t, "StorageBucket",
		schemaDefProperties(t, perProjectSchemaPath, "storageBucket"),
		goTypeJSONFields(t, StorageBucket{}))
}

func TestDrift_ScheduledJob(t *testing.T) {
	assertFieldParity(t, "ScheduledJob",
		schemaDefProperties(t, perProjectSchemaPath, "scheduledJob"),
		goTypeJSONFields(t, ScheduledJob{}))
}

func TestDrift_AggregatedDatabase(t *testing.T) {
	assertFieldParity(t, "AggregatedDatabase",
		schemaDefProperties(t, aggregatedSchemaPath, "aggregatedDatabase"),
		goTypeJSONFields(t, AggregatedDatabase{}))
}

func TestDrift_AggregatedTopic(t *testing.T) {
	assertFieldParity(t, "AggregatedTopic",
		schemaDefProperties(t, aggregatedSchemaPath, "aggregatedTopic"),
		goTypeJSONFields(t, AggregatedTopic{}))
}

func TestDrift_AggregatedStorage(t *testing.T) {
	assertFieldParity(t, "AggregatedStorage",
		schemaDefProperties(t, aggregatedSchemaPath, "aggregatedStorage"),
		goTypeJSONFields(t, AggregatedStorage{}))
}

func TestDrift_AggregatedSecret(t *testing.T) {
	assertFieldParity(t, "AggregatedSecret",
		schemaDefProperties(t, aggregatedSchemaPath, "aggregatedSecret"),
		goTypeJSONFields(t, AggregatedSecret{}))
}

func TestDrift_AggregatedScheduledJob(t *testing.T) {
	assertFieldParity(t, "AggregatedScheduledJob",
		schemaDefProperties(t, aggregatedSchemaPath, "aggregatedScheduledJob"),
		goTypeJSONFields(t, AggregatedScheduledJob{}))
}

func TestDrift_Source(t *testing.T) {
	assertFieldParity(t, "Source",
		schemaDefProperties(t, aggregatedSchemaPath, "source"),
		goTypeJSONFields(t, Source{}))
}

func TestDrift_Runtime(t *testing.T) {
	assertFieldParity(t, "Runtime",
		schemaDefProperties(t, aggregatedSchemaPath, "runtime"),
		goTypeJSONFields(t, Runtime{}))
}

func TestDrift_Ingress(t *testing.T) {
	assertFieldParity(t, "Ingress",
		schemaDefProperties(t, aggregatedSchemaPath, "ingress"),
		goTypeJSONFields(t, Ingress{}))
}

func TestDrift_Scaling(t *testing.T) {
	assertFieldParity(t, "Scaling",
		schemaDefProperties(t, aggregatedSchemaPath, "scaling"),
		goTypeJSONFields(t, Scaling{}))
}

// TestDrift_RuntimeCostPolicyIsDeployerOwned is the regression contract for
// ADR 0003: no cost-policy knob may reappear anywhere under the runtime block.
//
// It walks the whole runtime subtree on both sides rather than checking the
// two definitions the removed field happened to live in. `cpuIdle` and
// `cpuAlwaysAllocated` would naturally be added next to `cpu` and `memory` —
// that is, under `resources`, not `scaling` — so a guard that only inspected
// `runtime` and `scaling` would miss the reintroduction it exists to catch.
func TestDrift_RuntimeCostPolicyIsDeployerOwned(t *testing.T) {
	for _, field := range schemaSubtreeProperties(t, aggregatedSchemaPath, "runtime") {
		if reason, forbidden := RemovedRuntimeFields[field]; forbidden {
			t.Errorf("the runtime schema subtree exposes deployer-owned cost policy field %q (%s)", field, reason)
		}
	}
	for _, field := range goTypeSubtreeJSONFields(t, Runtime{}) {
		if reason, forbidden := RemovedRuntimeFields[field]; forbidden {
			t.Errorf("the Runtime type subtree exposes deployer-owned cost policy field %q (%s)", field, reason)
		}
	}
}

// TestDrift_RemovedRuntimeFieldsCoversTheRemovedField pins the guard's own
// input: RemovedRuntimeFields is what both the drift check and the reader
// diagnostics consult, so an entry silently dropped from it would disarm both
// at once without failing anything.
func TestDrift_RemovedRuntimeFieldsCoversTheRemovedField(t *testing.T) {
	for _, name := range []string{"min", "billing", "requestBased", "cpuIdle", "cpuAlwaysAllocated"} {
		if _, ok := RemovedRuntimeFields[name]; !ok {
			t.Errorf("RemovedRuntimeFields is missing %q", name)
		}
	}
}

func TestDrift_Resources(t *testing.T) {
	assertFieldParity(t, "Resources",
		schemaDefProperties(t, aggregatedSchemaPath, "resources"),
		goTypeJSONFields(t, Resources{}))
}

func TestDrift_ResourceLimits(t *testing.T) {
	assertFieldParity(t, "ResourceLimits",
		schemaDefProperties(t, aggregatedSchemaPath, "resourceLimits"),
		goTypeJSONFields(t, ResourceLimits{}))
}

func TestDrift_Security(t *testing.T) {
	assertFieldParity(t, "Security",
		schemaDefProperties(t, aggregatedSchemaPath, "security"),
		goTypeJSONFields(t, Security{}))
}

func TestDrift_RuntimeProtocols(t *testing.T) {
	assertFieldParity(t, "RuntimeProtocols",
		schemaDefProperties(t, aggregatedSchemaPath, "runtimeProtocols"),
		goTypeJSONFields(t, RuntimeProtocols{}))
}

func TestDrift_IgnoreRules(t *testing.T) {
	assertFieldParity(t, "IgnoreRules",
		schemaDefProperties(t, overridesSchemaPath, "ignoreRules"),
		goTypeJSONFields(t, IgnoreRules{}))
}

func TestDrift_DatabaseRef(t *testing.T) {
	assertFieldParity(t, "DatabaseRef",
		schemaDefProperties(t, overridesSchemaPath, "databaseRef"),
		goTypeJSONFields(t, DatabaseRef{}))
}

func TestDrift_EventsRef(t *testing.T) {
	assertFieldParity(t, "EventsRef",
		schemaDefProperties(t, overridesSchemaPath, "eventsRef"),
		goTypeJSONFields(t, EventsRef{}))
}
