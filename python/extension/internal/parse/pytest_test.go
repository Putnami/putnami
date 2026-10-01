package parse

import (
	"go.putnami.dev/protocol/features/spectest"

	"testing"
)

func TestPytestResults(t *testing.T) {
	spectest.Proves(t, "python/experimental-workspace-integration", "native-tools", "pytest-results-are-parsed-into-a-structured-report")
	tests := []struct {
		name   string
		output string
		want   TestCounts
	}{
		{
			name:   "empty output",
			output: "",
			want:   TestCounts{},
		},
		{
			name:   "all passed",
			output: "5 passed in 0.12s",
			want:   TestCounts{Passed: 5, Total: 5},
		},
		{
			name:   "all failed",
			output: "3 failed in 1.23s",
			want:   TestCounts{Failed: 3, Total: 3},
		},
		{
			name:   "all skipped",
			output: "2 skipped in 0.01s",
			want:   TestCounts{Skipped: 2, Total: 2},
		},
		{
			name:   "mixed results",
			output: "10 passed, 2 failed, 1 skipped in 5.00s",
			want:   TestCounts{Passed: 10, Failed: 2, Skipped: 1, Total: 13},
		},
		{
			name:   "only passed and skipped",
			output: "7 passed, 3 skipped",
			want:   TestCounts{Passed: 7, Skipped: 3, Total: 10},
		},
		{
			name:   "multiline output with summary at end",
			output: "FAILED test_module.py::test_foo - assert False\nFAILED test_module.py::test_bar - assert False\n\n2 failed, 8 passed in 2.34s",
			want:   TestCounts{Passed: 8, Failed: 2, Total: 10},
		},
		{
			name:   "no test counts in output",
			output: "collecting ... \nno tests ran",
			want:   TestCounts{},
		},
		{
			name:   "numbers not followed by passed/failed/skipped are ignored",
			output: "collected 10 items\n5 passed in 1.00s",
			want:   TestCounts{Passed: 5, Total: 5},
		},
		{
			name:   "total is sum of all parts",
			output: "1 passed, 1 failed, 1 skipped",
			want:   TestCounts{Passed: 1, Failed: 1, Skipped: 1, Total: 3},
		},
		{
			name:   "large numbers",
			output: "1000 passed, 500 failed, 250 skipped",
			want:   TestCounts{Passed: 1000, Failed: 500, Skipped: 250, Total: 1750},
		},
		{
			name:   "word boundary prevents partial match",
			output: "5 passedby in 1s",
			want:   TestCounts{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PytestResults(tt.output)
			if got != tt.want {
				t.Errorf("PytestResults(%q) = %+v, want %+v", tt.output, got, tt.want)
			}
		})
	}
}

func TestPytestResults_TotalIsComputed(t *testing.T) {
	// Verify that Total is always Passed + Failed + Skipped, not parsed separately
	output := "3 passed, 2 failed, 1 skipped"
	got := PytestResults(output)
	if got.Total != got.Passed+got.Failed+got.Skipped {
		t.Errorf("Total (%d) != Passed+Failed+Skipped (%d+%d+%d=%d)",
			got.Total, got.Passed, got.Failed, got.Skipped, got.Passed+got.Failed+got.Skipped)
	}
}
