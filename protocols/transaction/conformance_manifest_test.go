package transaction

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

const conformanceSchemaPath = "schemas/conformance-manifest.json"

// TestConformanceManifest parses the real corpus strictly, requires it to
// validate clean, and pins the required cases so a scenario cannot silently
// disappear. This is the non-gated guard: the manifest's own structure is
// conformance-checked even without a Postgres, exactly like the events manifest.
func TestConformanceManifest(t *testing.T) {
	m, diags := ParseAndValidateManifest(ConformanceManifestJSON())
	if diag.HasErrors(diags) {
		t.Fatalf("conformance manifest produced errors: %v", diags)
	}
	if m.Protocol != ConformanceProtocol {
		t.Fatalf("manifest protocol = %q, want %q", m.Protocol, ConformanceProtocol)
	}
	if m.Suite != ConformanceSuite {
		t.Fatalf("manifest suite = %q, want %q", m.Suite, ConformanceSuite)
	}
	if m.ProtocolVersion != ProtocolVersion {
		t.Fatalf("manifest protocolVersion = %d, want %d", m.ProtocolVersion, ProtocolVersion)
	}

	// The six required acceptance scenarios. Both adapters run this exact set.
	required := map[string]bool{
		"concurrent.consume.one-credential":     false,
		"sequential.consume.device-code":        false,
		"concurrent.rotate.refresh-once":        false,
		"atomic.rotate.all-or-nothing":          false,
		"nested.uow.join-outer-rollback":        false,
		"boundary.rollback-releases-connection": false,
	}
	for _, c := range m.Cases {
		if _, ok := required[c.ID]; ok {
			required[c.ID] = true
		}
	}
	for id, found := range required {
		if !found {
			t.Errorf("conformance manifest missing required case %q", id)
		}
	}
}

// TestConformanceManifestJSON_MatchesFile pins the exported embedded corpus to
// the on-disk conformance/manifest.json byte-for-byte, so the single source of
// truth cannot silently drift from the accessor the exported runners consume.
func TestConformanceManifestJSON_MatchesFile(t *testing.T) {
	onDisk, err := os.ReadFile(filepath.Join("conformance", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, ConformanceManifestJSON()) {
		t.Fatalf("embedded conformance corpus does not match conformance/manifest.json on disk")
	}
}

// TestConformanceManifest_Fixtures runs the shared manifest fixture corpus: every
// document under fixtures/conformance-manifest/valid must parse+validate clean,
// every document under .../invalid must produce at least one diagnostic. This is
// the guard-test pattern that keeps the corpus structure honest without Postgres.
func TestConformanceManifest_Fixtures(t *testing.T) {
	runManifestFixtureDir(t, "valid", false)
	runManifestFixtureDir(t, "invalid", true)
}

func runManifestFixtureDir(t *testing.T, kind string, wantErrors bool) {
	t.Helper()
	glob := filepath.Join("fixtures", "conformance-manifest", kind, "*.json")
	files, err := filepath.Glob(glob)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no fixtures matched %s", glob)
	}
	for _, path := range files {
		t.Run(kind+"/"+filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			_, diags := ParseAndValidateManifest(data)
			hasErr := diag.HasErrors(diags)
			if wantErrors && !hasErr {
				t.Errorf("invalid fixture %s should produce errors but none found", path)
			}
			if !wantErrors && hasErr {
				t.Errorf("valid fixture %s produced errors: %v", path, diags)
			}
		})
	}
}

// TestConformanceManifest_RejectCodes pins each acute reject branch to a specific
// fixture, so relaxing any single validation check turns a red fixture green and
// fails here.
func TestConformanceManifest_RejectCodes(t *testing.T) {
	cases := map[string]string{
		"duplicate-case-id.json":             ConformanceErrDuplicate,
		"unknown-primitive.json":             ConformanceErrEnum,
		"bad-protocol.json":                  ConformanceErrProtocol,
		"concurrency-multiset-mismatch.json": ConformanceErrConcurrency,
		"operation-without-expect.json":      ConformanceErrRequired,
		"rotate-missing-successor.json":      ConformanceErrArgs,
		"unknown-table-ref.json":             ConformanceErrReference,
	}
	for name, wantCode := range cases {
		data, err := os.ReadFile(filepath.Join("fixtures", "conformance-manifest", "invalid", name))
		if err != nil {
			t.Fatal(err)
		}
		_, diags := ParseAndValidateManifest(data)
		if !hasCode(diags, wantCode) {
			t.Errorf("fixture %s should trigger %q, got %v", name, wantCode, diags)
		}
	}
}

func hasCode(diags []diag.Diagnostic, code string) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

// TestConformanceManifest_SchemaEnumParity asserts the closed enums in the JSON
// schema match the Go enum values, so a schema edit that drops or adds a value —
// silently letting a runner accept a document the other rejects — fails here.
func TestConformanceManifest_SchemaEnumParity(t *testing.T) {
	var root any
	if err := json.Unmarshal(readSchemaBytes(t, conformanceSchemaPath), &root); err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	checks := []struct {
		name string
		path []string
		want []string
	}{
		{"level", []string{"$defs", "case", "properties", "level", "enum"},
			[]string{string(LevelRequired), string(LevelRecommended), string(LevelOptional)}},
		{"primitive", []string{"$defs", "operation", "properties", "primitive", "enum"},
			[]string{string(PrimitiveCompareAndSet), string(PrimitiveConsumeOnce), string(PrimitiveRotate)}},
		{"boundary", []string{"$defs", "operation", "properties", "boundary", "enum"},
			[]string{string(BoundaryNone), string(BoundaryTransaction), string(BoundaryNestedTransaction)}},
		{"fault", []string{"$defs", "operation", "properties", "fault", "enum"},
			[]string{string(FaultNone), string(FaultCallbackError)}},
		{"columnType", []string{"$defs", "column", "properties", "type", "enum"},
			[]string{string(ColumnText), string(ColumnBoolean), string(ColumnInteger)}},
		{"outcome", []string{"$defs", "outcome", "enum"},
			[]string{string(OutcomeApplied), string(OutcomeAlreadyConsumedConflict), string(OutcomeNotFound), string(OutcomeRetryableSerializationFailure)}},
	}
	for _, ch := range checks {
		t.Run(ch.name, func(t *testing.T) {
			got := schemaEnumAt(t, root, ch.path...)
			if !reflect.DeepEqual(sortedStrings(got), sortedStrings(ch.want)) {
				t.Errorf("%s enum = %v, want %v", ch.name, got, ch.want)
			}
		})
	}
}

// schemaEnumAt navigates the decoded schema to path and returns the string enum
// array there.
func schemaEnumAt(t *testing.T, root any, path ...string) []string {
	t.Helper()
	cur := root
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("schema path %v: %q is not an object", path, key)
		}
		cur = m[key]
	}
	arr, ok := cur.([]any)
	if !ok {
		t.Fatalf("schema path %v does not resolve to an array", path)
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("schema path %v has a non-string enum value %v", path, v)
		}
		out = append(out, s)
	}
	return out
}
