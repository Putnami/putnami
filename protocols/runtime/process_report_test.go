package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestExtractProcessReport(t *testing.T) {
	spectest.Proves(t, "runtime/event-stream", "result-payloads-are-typed-and-bounded", "result-payloads-are-extracted")
	report, err := ExtractProcessReport(map[string]any{
		ProcessReportResultDataKey: map[string]any{
			"selfCpuMs": 12,
			"starts":    []any{map[string]any{"kind": "git", "count": 3, "cpuMs": 4}, map[string]any{"kind": "other", "count": 1}},
			"git":       []any{map[string]any{"subcommand": "rev-parse", "count": 2}, map[string]any{"subcommand": "ls-files", "count": 1}},
		},
		"testSummary": map[string]any{"total": 1, "passed": 1, "failed": 0, "skipped": 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report == nil || report.SelfCPUMs == nil || *report.SelfCPUMs != 12 || len(report.Starts) != 2 || report.Starts[0].CPUMs == nil || *report.Starts[0].CPUMs != 4 || len(report.Git) != 2 {
		t.Fatalf("report = %#v", report)
	}
	if missing, err := ExtractProcessReport(map[string]any{}); missing != nil || err != nil {
		t.Fatalf("absent report = %#v, %v; want nil, nil", missing, err)
	}
}

func TestValidateProcessReportRefusesWhatItCannotCount(t *testing.T) {
	spectest.Proves(t, "runtime/event-stream", "result-payloads-are-typed-and-bounded", "result-payloads-are-extracted")
	negative := int64(-1)
	for name, report := range map[string]*ProcessReport{
		"negative self cpu": {SelfCPUMs: &negative},
		"unknown kind":      {Starts: []ProcessStarts{{Kind: "daemon", Count: 1}}},
		"kind twice":        {Starts: []ProcessStarts{{Kind: "git", Count: 1}, {Kind: "git", Count: 2}}},
		"zero count":        {Starts: []ProcessStarts{{Kind: "git"}}},
		"negative cpu":      {Starts: []ProcessStarts{{Kind: "git", Count: 1, CPUMs: &negative}}},
		"subcommand twice":  {Git: []GitCalls{{Subcommand: "log", Count: 1}, {Subcommand: "log", Count: 1}}},
		"zero git count":    {Git: []GitCalls{{Subcommand: "log"}}},
		"empty subcommand":  {Git: []GitCalls{{Count: 1}}},
	} {
		if err := ValidateProcessReport(report); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if err := ValidateProcessReport(&ProcessReport{}); err != nil {
		t.Errorf("empty report: %v", err)
	}
}

func TestDrift_ProcessKinds(t *testing.T) {
	spectest.Proves(t, "runtime/event-stream", "the-schemas-and-the-go-types-agree", "every-vocabulary-matches-its-schema")
	data, err := os.ReadFile(filepath.Join("schemas", "payloads.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Defs map[string]struct {
			Properties map[string]struct {
				Items struct {
					Properties map[string]struct {
						Enum []string `json:"enum"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	kinds := schema.Defs["ProcessReport"].Properties["starts"].Items.Properties["kind"].Enum
	assertEnumSync(t, "process kind", kinds, ProcessKinds)
}

func TestTheProcessReportFixtureCarriesAValidReport(t *testing.T) {
	spectest.Proves(t, "runtime/event-stream", "result-payloads-are-typed-and-bounded", "result-payloads-are-extracted")
	raw, err := os.ReadFile(filepath.Join("fixtures", "valid", "process-report.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.Contains(line, ProcessReportResultDataKey) {
			continue
		}
		var event struct {
			Data struct {
				Data map[string]any `json:"data"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		report, err := ExtractProcessReport(event.Data.Data)
		if err != nil || report == nil {
			t.Fatalf("fixture report = %#v, %v", report, err)
		}
		found++
	}
	if found != 1 {
		t.Fatalf("fixtures with a process report = %d, want 1", found)
	}
}
