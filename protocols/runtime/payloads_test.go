package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// The test-case payload has one definition, the CLI result contract's testCase.
// The runtime schema refers to it and restates none of its members or bounds.
func TestTestCasesSchemaReferencesTheCLIContract(t *testing.T) {
	const authority = "https://putnami.dev/schemas/putnami-cli-result-v2.json#/$defs/testCase"
	data, err := os.ReadFile(filepath.Join("schemas", "payloads.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	testCases := schemaObject(t, schema, "$defs", "TestCases")
	if testCases["type"] != "array" {
		t.Errorf("TestCases type = %#v, want array", testCases["type"])
	}
	if _, restated := testCases["maxItems"]; restated {
		t.Error("TestCases restates the per-task cap the CLI contract owns")
	}
	items := schemaObject(t, schema, "$defs", "TestCases", "items")
	if items["$ref"] != authority {
		t.Errorf("TestCases items $ref = %#v, want %q", items["$ref"], authority)
	}
	if _, redeclared := items["properties"]; redeclared {
		t.Error("TestCases redeclares the CLI contract's test-case members")
	}
	dropped := schemaObject(t, schema, "$defs", "TestCasesDropped")
	if dropped["type"] != "integer" || dropped["minimum"] != float64(1) {
		t.Errorf("TestCasesDropped = %#v, want an integer of at least 1", dropped)
	}
}

func TestExtractResultPayloads(t *testing.T) {
	data := map[string]any{
		"testSummary":     map[string]any{"total": 10, "passed": 8, "failed": 1, "skipped": 1, "failureDetailsTruncated": 3},
		"coverageSummary": map[string]any{"percentage": 81.5, "granularity": "statements", "covered": 163, "total": 200},
		"binaryPath":      "dist/app", // extension-specific extras are ignored
	}
	ts, cs, ls, err := ExtractResultPayloads(data)
	if err != nil {
		t.Fatal(err)
	}
	if ts == nil || ts.Total != 10 || ts.Passed != 8 || ts.Failed != 1 || ts.Skipped != 1 || ts.FailureDetailsTruncated != 3 {
		t.Errorf("testSummary = %+v", ts)
	}
	if cs == nil || cs.Percentage != 81.5 || cs.Granularity != CoverageStatements {
		t.Errorf("coverageSummary = %+v", cs)
	}
	if ls != nil {
		t.Errorf("lintSummary should be nil, got %+v", ls)
	}
	if diags := ValidateTestSummary(ts); diag.HasErrors(diags) {
		t.Errorf("valid testSummary produced errors: %v", diags)
	}
	if diags := ValidateCoverageSummary(cs); diag.HasErrors(diags) {
		t.Errorf("valid coverageSummary produced errors: %v", diags)
	}
}

func TestExtractResultPayloads_Empty(t *testing.T) {
	ts, cs, ls, err := ExtractResultPayloads(nil)
	if err != nil || ts != nil || cs != nil || ls != nil {
		t.Errorf("nil data should yield nil payloads, got %v %v %v %v", ts, cs, ls, err)
	}
}

func TestValidateTestSummary(t *testing.T) {
	bad := &TestSummary{Total: 2, Passed: 2, Failed: 1}
	if diags := ValidateTestSummary(bad); !diag.HasErrors(diags) {
		t.Error("overcounted summary should produce errors")
	}
	neg := &TestSummary{Total: -1}
	if diags := ValidateTestSummary(neg); !diag.HasErrors(diags) {
		t.Error("negative counts should produce errors")
	}
	negOmitted := &TestSummary{FailureDetailsTruncated: -1}
	if diags := ValidateTestSummary(negOmitted); !diag.HasErrors(diags) {
		t.Error("negative failure-details accounting should produce errors")
	}
}

func TestValidateCoverageSummary(t *testing.T) {
	cases := []CoverageSummary{
		{Percentage: 120, Granularity: CoverageLines},
		{Percentage: 50, Granularity: "loops"},
		{Percentage: 50, Granularity: CoverageLines, Covered: 5, Total: 3},
	}
	for i, c := range cases {
		if diags := ValidateCoverageSummary(&c); !diag.HasErrors(diags) {
			t.Errorf("case %d should produce errors: %+v", i, c)
		}
	}
}
