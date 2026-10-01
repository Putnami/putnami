package cli

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The report document's BOUNDS (doc/03-report.md).
//
// The cross-language corpus owns the report's semantics — every cross-field rule
// it carries has an accept and a reject fixture in conformance/manifest.json,
// executed identically by Go and TypeScript. These tests cover the one class of
// clause a corpus cannot hold honestly: the caps themselves. Pinning "65 jobs is
// too many" as a fixture would put 65 near-identical rows in a file whose whole
// value is that a reviewer can read it, so the documents are BUILT here from the
// exported constants instead — and the TypeScript mirror
// (test/result-v2.test.ts) builds the same documents from its own constants,
// which the schema pins to these (TestReportBoundsMatchSchema).

func reportFileFixture() ReportFile {
	dirty := false
	return ReportFile{
		ProtocolVersion: ResultProtocolVersion,
		SessionID:       "20260807-091500-abc123",
		StartTime:       "2026-08-07T09:15:00.000Z",
		EndTime:         "2026-08-07T09:15:26.000Z",
		Origin:          ReportOriginCLI,
		EnforceCoverage: true,
		Git: &ReportGit{
			Branch:   "epic/session-report",
			Sha:      "0123456789abcdef0123456789abcdef01234567",
			Dirty:    &dirty,
			Baseline: "origin/main",
		},
		Run: ReportRun{
			Outcome:    RunOutcomeSuccess,
			ExitCode:   ExitSuccess,
			Counts:     RunCounts{Total: 1, Succeeded: 1},
			Reuse:      RunReuse{LocalCache: 1},
			DurationMs: 26000,
		},
		Commands: []ReportCommand{{
			Command:     "build",
			Counts:      RunCounts{Total: 1, Succeeded: 1},
			Reuse:       RunReuse{LocalCache: 1},
			FreshWallMs: 0,
			Errors:      0,
			Warnings:    0,
		}},
		Jobs: []ReportJob{{
			Key:        "/tooling/cli:build~compile",
			Project:    "/tooling/cli",
			Task:       "build~compile",
			Command:    "build",
			Outcome:    TaskStatusSuccess,
			Reuse:      TaskReuseLocalCache,
			DurationMs: 4210,
		}},
		ElidedJobs: 0,
	}
}

func marshalReport(t *testing.T, report ReportFile) []byte {
	t.Helper()
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

// TestReportFileRoundTrip pins the whole document through the wire: every
// member the Go producer writes survives, and the result conforms.
func TestReportFileRoundTrip(t *testing.T) {
	want := reportFileFixture()
	want.Cache = &ReportCache{Hits: 3, Misses: 1, Restored: 3, Uploads: 1, TimeSavedMs: 9100, BytesFetched: 42000, BytesUploaded: 1200}
	want.Scheduler = &ReportScheduler{Parallelism: 8, CriticalPathMs: 18400}
	want.Commands[0].Tests = &ReportTests{
		Total: 12, Passed: 11, Failed: 0, Skipped: 1, FailureDetailsTruncated: 3,
	}
	want.Commands[0].Coverage = &ReportCoverage{Percentage: 87.5, Granularity: CoverageStatements, Covered: 350, Total: 400, Enforced: true}
	want.Jobs[0].Diagnostics = []Diagnostic{{Severity: "warning", Message: "unused import", File: "main.go", Line: 12, Column: 3}}

	data := marshalReport(t, want)
	var got ReportFile
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip lost a member:\n got: %+v\nwant: %+v", got, want)
	}
	if violations := ValidateDocument(DocumentReportFile, data); len(violations) != 0 {
		t.Errorf("ValidateDocument = %v, want none", violations)
	}
}

// TestReportAbsentIsNotZero pins the rule the whole document rests on: the
// optional blocks are absent from the WIRE when the run could not measure them,
// never present with zeros a consumer would read as measurements.
func TestReportAbsentIsNotZero(t *testing.T) {
	report := reportFileFixture()
	report.Git = nil
	data := marshalReport(t, report)
	for _, member := range []string{"git", "cache", "scheduler", "cpu", "coverage", "tests", "cpuMs", "truncatedCount", "failureDetailsTruncated"} {
		if strings.Contains(string(data), `"`+member+`"`) {
			t.Errorf("an unmeasured %q was serialized: %s", member, data)
		}
	}
	if violations := ValidateDocument(DocumentReportFile, data); len(violations) != 0 {
		t.Errorf("ValidateDocument = %v, want none", violations)
	}
}

func TestReportTestsFailureDetailAccountingIsOptionalAndNonNegative(t *testing.T) {
	report := reportFileFixture()
	report.Commands[0].Tests = &ReportTests{Total: 1, Passed: 0, Failed: 1, Skipped: 0}
	if violations := ValidateDocument(DocumentReportFile, marshalReport(t, report)); len(violations) != 0 {
		t.Fatalf("legacy four-counter test block was rejected: %v", violations)
	}

	report.Commands[0].Tests.FailureDetailsTruncated = -1
	want := []Violation{{Code: ViolationInvalidValue, Path: "commands[0].tests.failureDetailsTruncated"}}
	if got := ValidateDocument(DocumentReportFile, marshalReport(t, report)); !reflect.DeepEqual(got, want) {
		t.Fatalf("ValidateDocument = %v, want %v", got, want)
	}
}

// reportWithJobs returns a report carrying count jobs, with elidedJobs and the
// run/command totals kept consistent so only the bound under test can fail.
func reportWithJobs(count, elided int) ReportFile {
	report := reportFileFixture()
	report.Jobs = make([]ReportJob, 0, count)
	for i := range count {
		name := "build~t" + itoa(i)
		report.Jobs = append(report.Jobs, ReportJob{
			Key:        "/p:" + name,
			Project:    "/p",
			Task:       name,
			Command:    "build",
			Outcome:    TaskStatusSuccess,
			Reuse:      TaskReuseNone,
			DurationMs: int64(i),
		})
	}
	total := count + elided
	report.ElidedJobs = elided
	report.Run.Counts = RunCounts{Total: total, Succeeded: total}
	report.Run.Reuse = RunReuse{}
	report.Commands[0].Counts = report.Run.Counts
	report.Commands[0].Reuse = RunReuse{}
	return report
}

// TestReportJobBudget pins the job cap and the two rules that make eliding
// honest: the list plus the elided count accounts for every selected task, and
// nothing is elided until the list is full.
func TestReportJobBudget(t *testing.T) {
	cases := []struct {
		name  string
		count int
		// elided is what the report claims it left out.
		elided int
		want   []Violation
	}{
		{name: "full list carries the cap", count: ReportMaxJobs, elided: 0},
		{name: "a full list may elide the rest", count: ReportMaxJobs, elided: 681},
		{name: "a short list elides nothing", count: 3, elided: 0},
		{
			name:   "past the cap",
			count:  ReportMaxJobs + 1,
			elided: 0,
			want:   []Violation{{Code: ViolationInvalidValue, Path: "jobs"}},
		},
		{
			name:   "eliding before the budget is full",
			count:  ReportMaxJobs - 1,
			elided: 12,
			want:   []Violation{{Code: ViolationCountMismatch, Path: "jobs"}},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			data := marshalReport(t, reportWithJobs(testCase.count, testCase.elided))
			got := ValidateDocument(DocumentReportFile, data)
			if len(got) == 0 {
				got = nil
			}
			if !reflect.DeepEqual(got, testCase.want) {
				t.Errorf("ValidateDocument = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestReportElidedJobsAccountForTheRun pins the accounting rule: a truncated
// report still states the run's whole task total, so a small run and a
// truncated one are never confusable.
func TestReportElidedJobsAccountForTheRun(t *testing.T) {
	report := reportWithJobs(ReportMaxJobs, 681)
	report.ElidedJobs = 680 // one task accounted for by neither the list nor the count
	data := marshalReport(t, report)
	want := []Violation{{Code: ViolationCountMismatch, Path: "elidedJobs"}}
	if got := ValidateDocument(DocumentReportFile, data); !reflect.DeepEqual(got, want) {
		t.Errorf("ValidateDocument = %v, want %v", got, want)
	}
}

// TestReportMessageBoundIsBytes pins the message cap at the byte it is stated
// in. A multi-byte message that fits the cap in CODE POINTS but not in bytes is
// rejected — the budget is spent in bytes, and the schema's maxLength (which
// counts code points) is deliberately the weaker of the two checks.
func TestReportMessageBoundIsBytes(t *testing.T) {
	// "é" is two UTF-8 bytes, so half the cap in runes is exactly the cap in
	// bytes and one rune more is over it.
	atCap := strings.Repeat("é", ReportMaxMessageBytes/2)
	report := reportFileFixture()
	report.Jobs[0].Diagnostics = []Diagnostic{{Severity: "warning", Message: atCap}}
	if violations := ValidateDocument(DocumentReportFile, marshalReport(t, report)); len(violations) != 0 {
		t.Errorf("a message exactly at the cap was rejected: %v", violations)
	}

	report.Jobs[0].Diagnostics = []Diagnostic{{Severity: "warning", Message: atCap + "é"}}
	want := []Violation{{Code: ViolationInvalidValue, Path: "jobs[0].diagnostics[0].message"}}
	if got := ValidateDocument(DocumentReportFile, marshalReport(t, report)); !reflect.DeepEqual(got, want) {
		t.Errorf("ValidateDocument = %v, want %v", got, want)
	}
}

// TestReportDiagnosticsBudget pins the per-job diagnostic cap and the honest
// truncation clause. Typed producer omissions may accompany a short list; any
// additional report-side omission requires the list to be full.
func TestReportDiagnosticsBudget(t *testing.T) {
	diagnostics := func(count int) []Diagnostic {
		out := make([]Diagnostic, 0, count)
		for i := range count {
			out = append(out, Diagnostic{Severity: "warning", Message: "w" + itoa(i)})
		}
		return out
	}
	cases := []struct {
		name      string
		count     int
		truncated int
		producer  int
		want      []Violation
	}{
		{name: "a full list may report what it dropped", count: ReportMaxJobDiagnostics, truncated: 123},
		{name: "a short complete list", count: 3},
		{
			name:  "past the cap",
			count: ReportMaxJobDiagnostics + 1,
			want:  []Violation{{Code: ViolationInvalidValue, Path: "jobs[0].diagnostics"}},
		},
		{
			name:      "a truncation count must be positive",
			count:     3,
			truncated: -1,
			want:      []Violation{{Code: ViolationInvalidValue, Path: "jobs[0].truncatedCount"}},
		},
		{
			name:      "a short list may report producer omissions",
			count:     3,
			truncated: 9,
			producer:  9,
		},
		{
			name:      "a short list cannot hide report omissions",
			count:     3,
			truncated: 9,
			producer:  3,
			want:      []Violation{{Code: ViolationCountMismatch, Path: "jobs[0].truncatedCount"}},
		},
		{
			name:      "producer omissions cannot exceed the total",
			count:     3,
			truncated: 3,
			producer:  4,
			want:      []Violation{{Code: ViolationCountMismatch, Path: "jobs[0].truncatedCount"}},
		},
		{
			name:     "producer omissions require a total",
			count:    3,
			producer: 4,
			want:     []Violation{{Code: ViolationCountMismatch, Path: "jobs[0].truncatedCount"}},
		},
		{
			name:     "producer omissions must be positive",
			count:    3,
			producer: -1,
			want:     []Violation{{Code: ViolationInvalidValue, Path: "jobs[0].failureDetailsTruncated"}},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			report := reportFileFixture()
			report.Jobs[0].Diagnostics = diagnostics(testCase.count)
			report.Jobs[0].TruncatedCount = testCase.truncated
			report.Jobs[0].FailureDetailsTruncated = testCase.producer
			got := ValidateDocument(DocumentReportFile, marshalReport(t, report))
			if len(got) == 0 {
				got = nil
			}
			if !reflect.DeepEqual(got, testCase.want) {
				t.Errorf("ValidateDocument = %v, want %v", got, testCase.want)
			}
		})
	}
}
