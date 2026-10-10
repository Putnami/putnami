package reportstream

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// updateTwins regenerates the twin golden files (run: go test -run
// TestReportTwins -update-twins).
var updateTwins = flag.Bool("update-twins", false, "regenerate delivery-records twin golden files")

// TestReportTwinsMatchGolden pins the CLI-side report structs (the twins of
// the delivery records Tests, Coverage and Builds reports) to a fixed wire
// shape. If a twin drifts, this fails.
//
// The goldens are the CLI half of the twin contract: the server side validates
// identical copies against the delivery-records contract schemas. This module
// stays dependency-free (it is built GOWORK=off on the publish path), so the
// schema check runs on the server side rather than importing a validator here —
// keep the golden copies in sync.
func TestReportTwinsMatchGolden(t *testing.T) {
	cases := []struct {
		name    string
		payload any
	}{
		{"tests-report", fullTestsReport()},
		{"coverage-report", fullCoverageReport()},
		{"builds-report", fullBuildsReport()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.MarshalIndent(tc.payload, "", "  ")
			if err != nil {
				t.Fatalf("marshal %s twin: %v", tc.name, err)
			}
			got = append(got, '\n')
			path := filepath.Join("testdata", "twins", tc.name+".json")
			if *updateTwins {
				if err := os.WriteFile(path, got, 0o600); err != nil {
					t.Fatalf("write golden %s: %v", path, err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden %s (regenerate with -update-twins): %v", path, err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("CLI %s twin drifted from its golden.\n--- got ---\n%s\n--- want ---\n%s\n"+
					"Regenerate with -update-twins, then update the server-side copy and "+
					"confirm it still satisfies the %s contract schema.", tc.name, got, want, tc.name)
			}
		})
	}
}

// The fixtures are fully populated so a marshaled twin exercises every field.
// The CLI coverage twin has no deltaPercentage (server-computed at ingest); the
// schema keeps that field optional so both twins validate.

func fullTestsReport() TestsReport {
	return TestsReport{Projects: []ProjectTests{{
		Project: "libs/cli", Passed: 40, Failed: 1, Skipped: 2, Total: 43,
		FailingCases: []FailingCase{{Name: "TestThing", Message: "boom", File: "thing_test.go", Line: 12}},
	}}}
}

func fullCoverageReport() CoverageReport {
	return CoverageReport{Projects: []ProjectCoverage{{
		Project: "libs/cli", CoveredStatements: 120, TotalStatements: 150, Percentage: 80.0,
		Files: []FileCoverage{{File: "reportstream.go", CoveredStatements: 60, TotalStatements: 75, Percentage: 80.0}},
	}}}
}

func fullBuildsReport() BuildsReport {
	return BuildsReport{Tasks: []TaskBuild{{
		Project: "libs/cli", Task: "build~cross-compile", Status: "success", Reuse: "none", DurationMS: 211, CacheHit: false,
	}}}
}
