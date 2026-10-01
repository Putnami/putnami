package architecture

import (
	"go.putnami.dev/protocol/features/spectest"

	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func TestCloudObservabilityPilotTraversesProtocolAndGraph(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "distributed-authority", "the-generated-graph-is-a-projection-of-the-authored-manifests")
	spectest.Proves(t, "architecture/executable-contracts", "coverage", "the-snapshot-reports-project-dependency-enforcement-for-mapped-projects")
	runtime, observability := loadPilot(t)
	sources := []ManifestSource{
		{Path: "runtime/putnami.architecture.json", Manifest: runtime},
		{Path: "observability/putnami.architecture.json", Manifest: observability},
	}
	if diagnostics := ValidateRepository(sources); diag.HasErrors(diagnostics) {
		t.Fatalf("pilot repository diagnostics: %v", diagnostics)
	}
	graph := BuildGraph(sources)
	if len(graph.Domains) != 2 || len(graph.Edges) != 1 {
		t.Fatalf("pilot graph = %#v", graph)
	}
	edge := graph.Edges[0]
	if edge.ID != "observability.runtime-workspace-context.v1" || edge.Status != StatusPlanned || edge.Mode != ModeProjection {
		t.Fatalf("pilot edge = %#v", edge)
	}
	if edge.LocalModel == nil || edge.LocalModel.Name != "observability.workspace-context" || !edge.LocalModel.Rebuildable {
		t.Fatalf("pilot local model = %#v", edge.LocalModel)
	}
	if observability.Imports[0].Bootstrap.Availability != StatusPlanned || observability.Imports[0].Updates.Availability != StatusPlanned {
		t.Fatal("planned pilot pretends its API or event transport is active")
	}
	snapshot := BuildSnapshot(graph, Observations{}, nil, nil, RatchetOptions{})
	if len(snapshot.Findings) != 0 || snapshot.Coverage.Database != "not-detected" {
		t.Fatalf("pilot snapshot = %#v", snapshot)
	}
}

func TestInvalidManifestFixtures(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "distributed-authority", "an-invalid-authored-manifest-is-rejected-at-its-source")
	tests := []struct {
		path string
		code string
	}{
		{"fixtures/invalid/unknown-version.json", ErrorCodeInvalidProtocolVersion},
		{"fixtures/invalid/unknown-field.json", ErrorCodeUnknownField},
		{"fixtures/invalid/projection-missing-bootstrap.json", ErrorCodeInvalidProjection},
	}
	for _, test := range tests {
		t.Run(filepath.Base(test.path), func(t *testing.T) {
			data := readFixture(t, test.path)
			_, diagnostics := ParseAndValidateManifest(data)
			if !hasDiagnostic(diagnostics, test.code) {
				t.Fatalf("diagnostics = %v, want %s", diagnostics, test.code)
			}
		})
	}
}

func TestWaiverFixtures(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "ratchet", "waiver-documents-parse-under-the-same-strict-rules")
	valid := readFixture(t, "fixtures/waivers/valid/temporary.json")
	if _, diagnostics := ParseAndValidateWaiverFile(valid); diag.HasErrors(diagnostics) {
		t.Fatalf("valid waiver diagnostics: %v", diagnostics)
	}
	invalid := readFixture(t, "fixtures/waivers/invalid/incomplete.json")
	if _, diagnostics := ParseAndValidateWaiverFile(invalid); !hasDiagnostic(diagnostics, ErrorCodeInvalidDebtRecord) {
		t.Fatalf("incomplete waiver diagnostics = %v", diagnostics)
	}
}

func TestPublishedSchemasAreStrictJSON(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "single-interpretation", "the-published-schemas-are-strict")
	paths, err := filepath.Glob("schemas/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 4 {
		t.Fatalf("schema count = %d, want 4", len(paths))
	}
	for _, path := range paths {
		data := readFixture(t, path)
		var schema map[string]any
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if issue := publishedSchemaIssue(schema); issue != "" {
			t.Fatalf("schema %s is not identified and closed: %s", path, issue)
		}
	}
}

func TestPublishedSchemasConditionFactMetadataOnExportModes(t *testing.T) {
	for _, path := range []string{
		"schemas/putnami-architecture.json",
		"schemas/putnami-architecture-snapshot.json",
	} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			var schema map[string]any
			if err := json.Unmarshal(readFixture(t, path), &schema); err != nil {
				t.Fatal(err)
			}
			definitions := mustSchemaObject(t, schema["$defs"], "$defs")
			fact := mustSchemaObject(t, definitions["fact"], "$defs.fact")
			if got := schemaStrings(t, fact["required"], "$defs.fact.required"); strings.Join(got, ",") != "authority,name" {
				t.Fatalf("base fact required = %v, want authority and name only", got)
			}

			pairCases := mustSchemaArray(t, fact["oneOf"], "$defs.fact.oneOf")
			if len(pairCases) != 2 {
				t.Fatalf("fact metadata pair cases = %d, want present or absent", len(pairCases))
			}
			present := mustSchemaObject(t, pairCases[0], "$defs.fact.oneOf[0]")
			if got := schemaStrings(t, present["required"], "$defs.fact.oneOf[0].required"); strings.Join(got, ",") != "classification,personalData" {
				t.Fatalf("present metadata required = %v, want the complete pair", got)
			}
			absent := mustSchemaObject(t, pairCases[1], "$defs.fact.oneOf[1]")
			not := mustSchemaObject(t, absent["not"], "$defs.fact.oneOf[1].not")
			anyOf := mustSchemaArray(t, not["anyOf"], "$defs.fact.oneOf[1].not.anyOf")
			forbidden := make([]string, 0, len(anyOf))
			for index, value := range anyOf {
				item := mustSchemaObject(t, value, fmt.Sprintf("$defs.fact.oneOf[1].not.anyOf[%d]", index))
				forbidden = append(forbidden, schemaStrings(t, item["required"], "required")...)
			}
			sort.Strings(forbidden)
			if strings.Join(forbidden, ",") != "classification,personalData" {
				t.Fatalf("absent metadata forbids = %v, want both members", forbidden)
			}

			export := mustSchemaObject(t, definitions["export"], "$defs.export")
			conditions := mustSchemaArray(t, export["allOf"], "$defs.export.allOf")
			if len(conditions) != 1 {
				t.Fatalf("export conditions = %d, want one metadata condition", len(conditions))
			}
			condition := mustSchemaObject(t, conditions[0], "$defs.export.allOf[0]")
			ifSchema := mustSchemaObject(t, condition["if"], "if")
			ifProperties := mustSchemaObject(t, ifSchema["properties"], "if.properties")
			modes := mustSchemaObject(t, ifProperties["modes"], "if.properties.modes")
			contains := mustSchemaObject(t, modes["contains"], "if.properties.modes.contains")
			if got := schemaStrings(t, contains["enum"], "if.properties.modes.contains.enum"); strings.Join(got, ",") != "command,projection,query,snapshot" {
				t.Fatalf("classified modes = %v, want every non-reference mode", got)
			}
			thenSchema := mustSchemaObject(t, condition["then"], "then")
			thenProperties := mustSchemaObject(t, thenSchema["properties"], "then.properties")
			facts := mustSchemaObject(t, thenProperties["facts"], "then.properties.facts")
			items := mustSchemaObject(t, facts["items"], "then.properties.facts.items")
			if got := schemaStrings(t, items["required"], "then.properties.facts.items.required"); strings.Join(got, ",") != "classification,personalData" {
				t.Fatalf("non-reference fact required = %v, want the complete metadata pair", got)
			}
		})
	}
}

func TestPublishedSchemaGuardRejectsMissingIDAndOpenDefinitions(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "single-interpretation", "the-schema-guard-rejects-an-open-definition")
	for _, test := range []struct {
		name   string
		schema map[string]any
	}{
		{
			name:   "missing id",
			schema: map[string]any{"type": "object", "additionalProperties": false},
		},
		{
			name:   "empty id",
			schema: map[string]any{"$id": "", "type": "object", "additionalProperties": false},
		},
		{
			name: "open definition",
			schema: map[string]any{
				"$id": "https://putnami.dev/schemas/test.json", "type": "object", "additionalProperties": false,
				"$defs": map[string]any{"open": map[string]any{"type": "object"}},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if issue := publishedSchemaIssue(test.schema); issue == "" {
				t.Fatal("invalid schema passed the publication guard")
			}
		})
	}
}

func publishedSchemaIssue(schema map[string]any) string {
	id, ok := schema["$id"].(string)
	if !ok || id == "" {
		return "$id must be a non-empty string"
	}
	if path := firstOpenObjectSchema(schema, "$"); path != "" {
		return path + " must set additionalProperties to false"
	}
	return ""
}

func firstOpenObjectSchema(value any, path string) string {
	switch typed := value.(type) {
	case map[string]any:
		if typed["type"] == "object" {
			closed, ok := typed["additionalProperties"].(bool)
			if !ok || closed {
				return path
			}
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if open := firstOpenObjectSchema(typed[key], path+"."+key); open != "" {
				return open
			}
		}
	case []any:
		for index, item := range typed {
			if open := firstOpenObjectSchema(item, fmt.Sprintf("%s[%d]", path, index)); open != "" {
				return open
			}
		}
	}
	return ""
}

func mustSchemaObject(t *testing.T, value any, field string) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("schema field %s = %T, want object", field, value)
	}
	return object
}

func mustSchemaArray(t *testing.T, value any, field string) []any {
	t.Helper()
	array, ok := value.([]any)
	if !ok {
		t.Fatalf("schema field %s = %T, want array", field, value)
	}
	return array
}

func schemaStrings(t *testing.T, value any, field string) []string {
	t.Helper()
	array := mustSchemaArray(t, value, field)
	stringsOut := make([]string, 0, len(array))
	for _, item := range array {
		text, ok := item.(string)
		if !ok {
			t.Fatalf("schema field %s contains %T, want string", field, item)
		}
		stringsOut = append(stringsOut, text)
	}
	sort.Strings(stringsOut)
	return stringsOut
}

func TestStrictParserRejectsNonIntegerVersionAndNull(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "single-interpretation", "the-protocol-owns-strict-parsing")
	for _, document := range []string{
		`{"protocolVersion":1.0,"domain":"x","owner":"x","projects":[],"exports":[],"imports":[]}`,
		`{"protocolVersion":1,"domain":"x","owner":"x","projects":null,"exports":[],"imports":[]}`,
	} {
		if manifest, diagnostics := ParseManifest([]byte(document)); manifest != nil || !diag.HasErrors(diagnostics) {
			t.Fatalf("ParseManifest(%s) = %#v, %v", document, manifest, diagnostics)
		}
	}
}

func TestStrictParserRejectsDuplicateFieldsAtAnyDepth(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "single-interpretation", "strict-parsing-rejects-duplicate-fields-at-any-depth")
	for _, test := range []struct {
		name     string
		document string
		field    string
	}{
		{
			name:     "top level",
			document: `{"protocolVersion":1,"domain":"x","domain":"y","owner":"x","projects":[],"exports":[],"imports":[]}`,
			field:    "domain",
		},
		{
			name:     "nested array object",
			document: `{"protocolVersion":1,"domain":"x","owner":"x","projects":[],"exports":[],"imports":[{"id":"x.y.v1","id":"x.z.v1"}]}`,
			field:    "imports[0].id",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			manifest, diagnostics := ParseManifest([]byte(test.document))
			if manifest != nil || len(diagnostics) != 1 || diagnostics[0].Code != ErrorCodeDuplicateField || diagnostics[0].Field != test.field {
				t.Fatalf("ParseManifest = %#v, %+v", manifest, diagnostics)
			}
		})
	}
}

func TestSemanticValidationRequiresExplicitCollections(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "single-interpretation", "the-protocol-owns-semantic-validation")
	manifest, diagnostics := ParseAndValidateManifest([]byte(`{"protocolVersion":1,"domain":"x","owner":"x"}`))
	if manifest == nil {
		t.Fatalf("strict parse unexpectedly failed: %+v", diagnostics)
	}
	for _, field := range []string{"projects", "exports", "imports"} {
		found := false
		for _, diagnostic := range diagnostics {
			if diagnostic.Code == ErrorCodeParseError && diagnostic.Field == field {
				found = true
			}
		}
		if !found {
			t.Errorf("missing required collection %q was accepted: %+v", field, diagnostics)
		}
	}

	if _, baselineDiagnostics := ParseAndValidateBaseline([]byte(`{"protocolVersion":1}`)); !hasDiagnostic(baselineDiagnostics, ErrorCodeParseError) {
		t.Fatalf("missing baseline findings accepted: %+v", baselineDiagnostics)
	}
	if _, waiverDiagnostics := ParseAndValidateWaiverFile([]byte(`{"protocolVersion":1}`)); !hasDiagnostic(waiverDiagnostics, ErrorCodeParseError) {
		t.Fatalf("missing waiver collection accepted: %+v", waiverDiagnostics)
	}
}

func TestReferenceOnlyFactsMayOmitDataMetadata(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "distributed-authority", "reference-only-code-facts-may-omit-data-metadata")

	exportFor := func(modes ...AccessMode) Export {
		return Export{
			ID:          "producer.contracts.v1",
			Version:     1,
			Status:      StatusActive,
			Description: "A published code contract.",
			Facts: []Fact{{
				Name:      "contract_definitions",
				Authority: "producer",
			}},
			Modes: modes,
			Compatibility: Compatibility{
				Strategy:               CompatibilityAdditive,
				MinimumConsumerVersion: 1,
			},
		}
	}
	manifestFor := func(export Export) *Manifest {
		return &Manifest{
			ProtocolVersion: ProtocolVersion,
			Domain:          "producer",
			Owner:           "producer",
			Projects:        []string{},
			Exports:         []Export{export},
			Imports:         []Import{},
		}
	}

	reference := manifestFor(exportFor(ModeReference))
	if diagnostics := ValidateManifest(reference); diag.HasErrors(diagnostics) {
		t.Fatalf("reference-only code fact requires filler metadata: %+v", diagnostics)
	}
	data, err := MarshalManifest(reference)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"classification"`) || strings.Contains(string(data), `"personalData"`) {
		t.Fatalf("canonical reference-only fact retained empty metadata:\n%s", data)
	}
	parsed, diagnostics := ParseAndValidateManifest(data)
	if parsed == nil || diag.HasErrors(diagnostics) {
		t.Fatalf("strict reader rejected canonical reference-only fact:\n%s\n%+v", data, diagnostics)
	}
	explicitEmpty := strings.Replace(string(data), `"authority": "producer"`, `"authority": "producer", "classification": "", "personalData": ""`, 1)
	if _, diagnostics := ParseAndValidateManifest([]byte(explicitEmpty)); !hasInvalidFactAt(diagnostics, "exports[0].facts[0].classification") ||
		!hasInvalidFactAt(diagnostics, "exports[0].facts[0].personalData") {
		t.Fatalf("explicit empty metadata tokens were treated as omission: %+v", diagnostics)
	}

	for name, modes := range map[string][]AccessMode{
		"query":                    {ModeQuery},
		"snapshot":                 {ModeSnapshot},
		"projection":               {ModeProjection},
		"command":                  {ModeCommand},
		"reference plus data mode": {ModeReference, ModeQuery},
	} {
		t.Run(name, func(t *testing.T) {
			diagnostics := ValidateManifest(manifestFor(exportFor(modes...)))
			for _, field := range []string{"exports[0].facts[0].classification", "exports[0].facts[0].personalData"} {
				if !hasInvalidFactAt(diagnostics, field) {
					t.Errorf("missing metadata field %q was accepted: %+v", field, diagnostics)
				}
			}
		})
	}

	partial := exportFor(ModeReference)
	partial.Facts[0].Classification = ClassificationPublic
	if diagnostics := ValidateManifest(manifestFor(partial)); !hasInvalidFactAt(diagnostics, "exports[0].facts[0].personalData") {
		t.Fatalf("partially declared metadata was accepted: %+v", diagnostics)
	}
}

// TestCanonicalWritingRoundTripsThroughTheStrictReader closes the loop the
// canonical writer and the strict reader form: whatever the writer emits, the
// reader must accept unchanged.
//
// It is not decorative. A required collection that is present but EMPTY —
// `"exports": []`, the way a domain says it publishes nothing — used to
// canonicalize to `"exports": null`, and the reader then refused the writer's
// own output. Anything holding a committed file to a canonical rendering (the
// SDK's authoring pin, an `architecture sync` suggestion) is broken by that,
// and nothing else in this package noticed, because both halves were only ever
// tested against documents whose collections were non-empty.
func TestCanonicalWritingRoundTripsThroughTheStrictReader(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "single-interpretation", "canonical-writing-round-trips-through-the-strict-reader")
	empty := &Manifest{
		Schema:          ManifestSchemaURL,
		ProtocolVersion: ProtocolVersion,
		Domain:          "x",
		Owner:           "x",
		Projects:        []string{},
		Exports:         []Export{},
		Imports:         []Import{},
	}
	runtime, observability := loadPilot(t)
	for name, manifest := range map[string]*Manifest{
		"every required collection empty": empty,
		"the cloud-pilot producer":        runtime,
		"the cloud-pilot consumer":        observability,
	} {
		t.Run(name, func(t *testing.T) {
			data, err := MarshalManifest(manifest)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			parsed, diagnostics := ParseAndValidateManifest(data)
			if parsed == nil || diag.HasErrors(diagnostics) {
				t.Fatalf("the strict reader rejected the canonical writer's output:\n%s\n%+v", data, diagnostics)
			}
			again, err := MarshalManifest(parsed)
			if err != nil {
				t.Fatalf("re-marshal: %v", err)
			}
			if string(again) != string(data) {
				t.Errorf("canonical rendering is not a fixed point:\n first:\n%s\n second:\n%s", data, again)
			}
		})
	}
}

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func loadPilot(t *testing.T) (*Manifest, *Manifest) {
	t.Helper()
	runtime, runtimeDiagnostics := ParseAndValidateManifest(readFixture(t, "fixtures/valid/cloud-pilot/runtime.json"))
	if diag.HasErrors(runtimeDiagnostics) {
		t.Fatalf("runtime fixture diagnostics: %v", runtimeDiagnostics)
	}
	observability, observabilityDiagnostics := ParseAndValidateManifest(readFixture(t, "fixtures/valid/cloud-pilot/observability.json"))
	if diag.HasErrors(observabilityDiagnostics) {
		t.Fatalf("observability fixture diagnostics: %v", observabilityDiagnostics)
	}
	return runtime, observability
}

func hasDiagnostic(diagnostics []diag.Diagnostic, code string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}

func hasInvalidFactAt(diagnostics []diag.Diagnostic, field string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == ErrorCodeInvalidFact && diagnostic.Field == field {
			return true
		}
	}
	return false
}

func diagnosticCodes(diagnostics []diag.Diagnostic) string {
	codes := make([]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		codes = append(codes, diagnostic.Code)
	}
	return strings.Join(codes, ",")
}
