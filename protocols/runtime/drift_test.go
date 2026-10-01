package runtime

import (
	"encoding/json"
	"os"
	"sort"
	"testing"
)

// schemaEventTypes extracts the enum values from the EventType $defs entry in the schema.
func schemaEventTypes(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile("schemas/event.json")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var schema struct {
		Defs map[string]struct {
			Enum []string `json:"enum"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	def, ok := schema.Defs["EventType"]
	if !ok {
		t.Fatal("no $defs/EventType in schema")
	}
	result := make([]string, len(def.Enum))
	copy(result, def.Enum)
	sort.Strings(result)
	return result
}

// goEventTypes returns the event type constants defined in Go.
func goEventTypes() []string {
	types := []string{
		string(EventLog),
		string(EventProgress),
		string(EventArtifact),
		string(EventDiagnostic),
		string(EventMetric),
		string(EventPhase),
		string(EventSummary),
		string(EventResult),
		string(EventMeta),
	}
	sort.Strings(types)
	return types
}

func TestDrift_EventTypes(t *testing.T) {
	schemaTypes := schemaEventTypes(t)
	goTypes := goEventTypes()

	schemaSet := make(map[string]bool)
	for _, s := range schemaTypes {
		schemaSet[s] = true
	}
	goSet := make(map[string]bool)
	for _, g := range goTypes {
		goSet[g] = true
	}

	for _, s := range schemaTypes {
		if !goSet[s] {
			t.Errorf("event type %q exists in schema but not in Go constants", s)
		}
	}
	for _, g := range goTypes {
		if !schemaSet[g] {
			t.Errorf("event type %q exists in Go constants but not in schema", g)
		}
	}
}

func schemaLogLevels(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile("schemas/event.json")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var schema struct {
		Defs map[string]struct {
			Enum []string `json:"enum"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	def, ok := schema.Defs["LogLevel"]
	if !ok {
		t.Fatal("no $defs/LogLevel in schema")
	}
	result := make([]string, len(def.Enum))
	copy(result, def.Enum)
	sort.Strings(result)
	return result
}

func goLogLevels() []string {
	levels := []string{
		string(LevelDebug),
		string(LevelInfo),
		string(LevelWarn),
		string(LevelError),
	}
	sort.Strings(levels)
	return levels
}

func TestDrift_LogLevels(t *testing.T) {
	schemaLevels := schemaLogLevels(t)
	goLevels := goLogLevels()

	schemaSet := make(map[string]bool)
	for _, s := range schemaLevels {
		schemaSet[s] = true
	}
	goSet := make(map[string]bool)
	for _, g := range goLevels {
		goSet[g] = true
	}

	for _, s := range schemaLevels {
		if !goSet[s] {
			t.Errorf("log level %q exists in schema but not in Go constants", s)
		}
	}
	for _, g := range goLevels {
		if !schemaSet[g] {
			t.Errorf("log level %q exists in Go constants but not in schema", g)
		}
	}
}

func TestDrift_ResultStatuses(t *testing.T) {
	// Extract result statuses from the ResultEvent data.status enum in schema.
	data, err := os.ReadFile("schemas/event.json")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var schema struct {
		Defs map[string]struct {
			Properties map[string]struct {
				Properties map[string]struct {
					Enum []string `json:"enum"`
				} `json:"properties"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse schema: %v", err)
	}

	resultDef, ok := schema.Defs["ResultEvent"]
	if !ok {
		t.Fatal("no $defs/ResultEvent in schema")
	}
	dataProp, ok := resultDef.Properties["data"]
	if !ok {
		t.Fatal("no data property in ResultEvent")
	}
	statusProp, ok := dataProp.Properties["status"]
	if !ok {
		t.Fatal("no status property in ResultEvent.data")
	}

	schemaStatuses := make([]string, len(statusProp.Enum))
	copy(schemaStatuses, statusProp.Enum)
	sort.Strings(schemaStatuses)

	goStatuses := []string{
		string(ResultOK),
		string(ResultFailed),
		string(ResultSkip),
	}
	sort.Strings(goStatuses)

	schemaSet := make(map[string]bool)
	for _, s := range schemaStatuses {
		schemaSet[s] = true
	}
	goSet := make(map[string]bool)
	for _, g := range goStatuses {
		goSet[g] = true
	}

	for _, s := range schemaStatuses {
		if !goSet[s] {
			t.Errorf("result status %q exists in schema but not in Go constants", s)
		}
	}
	for _, g := range goStatuses {
		if !schemaSet[g] {
			t.Errorf("result status %q exists in Go constants but not in schema", g)
		}
	}
}

func TestDrift_DiagnosticSeverities(t *testing.T) {
	data, err := os.ReadFile("schemas/event.json")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var schema struct {
		Defs map[string]struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse schema: %v", err)
	}

	diagDef, ok := schema.Defs["DiagnosticEvent"]
	if !ok {
		t.Fatal("no $defs/DiagnosticEvent in schema")
	}
	sevProp, ok := diagDef.Properties["severity"]
	if !ok {
		t.Fatal("no severity property in DiagnosticEvent")
	}

	schemaSeverities := make([]string, len(sevProp.Enum))
	copy(schemaSeverities, sevProp.Enum)
	sort.Strings(schemaSeverities)

	goSeverities := []string{
		string(SeverityError),
		string(SeverityWarning),
		string(SeverityInfo),
		string(SeverityHint),
	}
	sort.Strings(goSeverities)

	assertEnumSync(t, "diagnostic severity", schemaSeverities, goSeverities)
}

func TestDrift_PhaseActions(t *testing.T) {
	data, err := os.ReadFile("schemas/event.json")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var schema struct {
		Defs map[string]struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse schema: %v", err)
	}

	phaseDef, ok := schema.Defs["PhaseEvent"]
	if !ok {
		t.Fatal("no $defs/PhaseEvent in schema")
	}

	// Check action enum
	actionProp, ok := phaseDef.Properties["action"]
	if !ok {
		t.Fatal("no action property in PhaseEvent")
	}
	schemaActions := make([]string, len(actionProp.Enum))
	copy(schemaActions, actionProp.Enum)
	sort.Strings(schemaActions)

	goActions := []string{string(PhaseStart), string(PhaseEnd)}
	sort.Strings(goActions)

	assertEnumSync(t, "phase action", schemaActions, goActions)

	// Check status enum
	statusProp, ok := phaseDef.Properties["status"]
	if !ok {
		t.Fatal("no status property in PhaseEvent")
	}
	schemaStatuses := make([]string, len(statusProp.Enum))
	copy(schemaStatuses, statusProp.Enum)
	sort.Strings(schemaStatuses)

	goStatuses := []string{string(PhaseSuccess), string(PhaseFailed), string(PhaseSkipped)}
	sort.Strings(goStatuses)

	assertEnumSync(t, "phase status", schemaStatuses, goStatuses)
}

// assertEnumSync checks that two sorted string slices match, reporting drift.
func assertEnumSync(t *testing.T, label string, schemaVals, goVals []string) {
	t.Helper()
	schemaSet := make(map[string]bool, len(schemaVals))
	for _, s := range schemaVals {
		schemaSet[s] = true
	}
	goSet := make(map[string]bool, len(goVals))
	for _, g := range goVals {
		goSet[g] = true
	}
	for _, s := range schemaVals {
		if !goSet[s] {
			t.Errorf("%s %q exists in schema but not in Go constants", label, s)
		}
	}
	for _, g := range goVals {
		if !schemaSet[g] {
			t.Errorf("%s %q exists in Go constants but not in schema", label, g)
		}
	}
}
