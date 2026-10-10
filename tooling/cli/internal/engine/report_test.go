package engine

import (
	"bytes"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/machine"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func sampleTuning() *jobs.TuningReport {
	return &jobs.TuningReport{
		Parallel: jobs.ParallelDecision{
			Mode:             "auto",
			Workers:          8,
			LogicalCPU:       16,
			CPUCapacity:      12,
			MemoryTotalMiB:   32000,
			MemoryUsableMiB:  24000,
			MemoryCapWorkers: 8,
		},
		HeavyJobRatio: 0.25,
		ReadyWait: []jobs.ReadyWaitByCommand{
			{Command: "test", Jobs: 3, TotalMs: 1200, MaxMs: 800},
		},
		CriticalPath: &jobs.CriticalPath{
			DurationMs: 2500,
			Chain: []jobs.CriticalPathNode{
				{Job: "/app:build~types", Command: "build", DurationMs: 1700},
				{Job: "/app:test~test", Command: "test", DurationMs: 800},
			},
		},
		CPUBudgets: []jobs.JobCPUBudget{
			{Job: "/app:test~test", Weight: 3, ExpectedCPUMs: 80_000, Budget: 6},
		},
	}
}

func TestReportSchedulerTuning_VerboseShowsParallelOnly(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	reportSchedulerTuning(&buf, &GlobalFlags{Verbose: true}, sampleTuning())

	out := buf.String()
	if !strings.Contains(out, "Scheduler: auto") || !strings.Contains(out, "8 workers") {
		t.Errorf("verbose summary missing parallelism line: %q", out)
	}
	if !strings.Contains(out, "memory cap 8") {
		t.Errorf("verbose summary missing memory cap: %q", out)
	}
	if !strings.Contains(out, "12 allocatable CPU") || !strings.Contains(out, "24000 usable") {
		t.Errorf("verbose summary missing stable admission capacity: %q", out)
	}
	if !strings.Contains(out, "heavy 25%") {
		t.Errorf("verbose summary missing heavy ratio: %q", out)
	}
	// Critical path / ready wait detail is debug/profile only.
	if strings.Contains(out, "Critical path") || strings.Contains(out, "Ready wait") {
		t.Errorf("verbose summary should not include detail sections: %q", out)
	}
}

func TestReportSchedulerTuning_ProfileShowsDetail(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	reportSchedulerTuning(&buf, &GlobalFlags{TraceProfile: "trace.json"}, sampleTuning())

	out := buf.String()
	if !strings.Contains(out, "Critical path (2s)") {
		t.Errorf("profile output missing critical path: %q", out)
	}
	if !strings.Contains(out, "/app:build~types") {
		t.Errorf("profile output missing critical path node: %q", out)
	}
	if !strings.Contains(out, "Ready wait by command") || !strings.Contains(out, "test") {
		t.Errorf("profile output missing ready wait: %q", out)
	}
	if !strings.Contains(out, "weight 3.0 · expected cpu 1m20s") {
		t.Errorf("profile output does not explain CPU grant: %q", out)
	}
}

func TestReportSchedulerTuning_QuietAndNilAreSilent(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	reportSchedulerTuning(&buf, &GlobalFlags{Quiet: true, Debug: true}, sampleTuning())
	if buf.Len() != 0 {
		t.Errorf("quiet should suppress output, got %q", buf.String())
	}

	buf.Reset()
	reportSchedulerTuning(&buf, &GlobalFlags{Verbose: true}, nil)
	if buf.Len() != 0 {
		t.Errorf("nil tuning should produce no output, got %q", buf.String())
	}

	// Default (no verbose/debug/profile) prints nothing.
	buf.Reset()
	reportSchedulerTuning(&buf, &GlobalFlags{}, sampleTuning())
	if buf.Len() != 0 {
		t.Errorf("default mode should print nothing, got %q", buf.String())
	}
}

func TestReportCacheSummary_HeadlineShowsByDefault(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	c := &jobs.CacheStatsSnapshot{
		SetupMs: 90, NegotiateMs: 60, UploadMs: 30,
		KeysRequested: 15, Hits: 12, Misses: 3,
		Restored: 12, TimeSavedMs: 260000,
		BytesFetched: 8_400_000, BytesUploaded: 2_100_000, BytesDeduped: 5_000_000,
	}
	reportCacheSummary(&buf, &GlobalFlags{}, c)

	out := buf.String()
	// Headline (hits/miss, saved vs spent) shows even without verbose.
	if !strings.Contains(out, "12 hit") || !strings.Contains(out, "3 miss") {
		t.Errorf("headline missing hit/miss counts: %q", out)
	}
	if !strings.Contains(out, "saved 4m20s") {
		t.Errorf("headline should show time saved: %q", out)
	}
	if !strings.Contains(out, "spent 180ms") || !strings.Contains(out, "setup 90ms") {
		t.Errorf("headline should show spent breakdown: %q", out)
	}
	// Byte-movement detail is verbose/debug only.
	if strings.Contains(out, "fetched") {
		t.Errorf("default mode must not show byte detail: %q", out)
	}
}

func TestReportCacheSummary_AttributesLocalOnlyServing(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	reportCacheSummary(&buf, &GlobalFlags{}, &jobs.CacheStatsSnapshot{
		LocalHits:             120,
		LocalMisses:           0,
		LocalServedMs:         11_200,
		LocalKeysMs:           3_100,
		LocalBindingsMs:       7_400,
		LocalRestoreVerifyMs:  700,
		LocalSpawnedProcesses: 526,
	})
	out := buf.String()
	for _, want := range []string{
		"120 local hit", "0 miss", "served in 11s", "keys 3s",
		"bindings 7s", "restore-verify 700ms", "spawned 526 processes",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("local cache summary missing %q: %q", want, out)
		}
	}
	if strings.Contains(out, "Remote cache:") {
		t.Errorf("local-only summary invented a remote leg: %q", out)
	}
}

func TestReportCacheSummary_VerboseAddsBytes(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	c := &jobs.CacheStatsSnapshot{KeysRequested: 2, Hits: 1, Misses: 1, BytesFetched: 8_400_000}
	reportCacheSummary(&buf, &GlobalFlags{Verbose: true}, c)
	if !strings.Contains(buf.String(), "8.0 MB fetched") {
		t.Errorf("verbose should show fetched bytes: %q", buf.String())
	}
}

func TestReportCacheSummary_SilentWhenIdleOrQuietOrNil(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	// No keys, no uploads, no restores: stay silent rather than print zeros.
	reportCacheSummary(&buf, &GlobalFlags{}, &jobs.CacheStatsSnapshot{SetupMs: 5})
	if buf.Len() != 0 {
		t.Errorf("idle cache should print nothing, got %q", buf.String())
	}

	buf.Reset()
	reportCacheSummary(&buf, &GlobalFlags{Quiet: true}, &jobs.CacheStatsSnapshot{KeysRequested: 5, Hits: 5})
	if buf.Len() != 0 {
		t.Errorf("quiet should suppress the summary, got %q", buf.String())
	}

	buf.Reset()
	reportCacheSummary(&buf, &GlobalFlags{Verbose: true}, nil)
	if buf.Len() != 0 {
		t.Errorf("nil snapshot (local-only run) should print nothing, got %q", buf.String())
	}
}

func TestReportCacheSummary_FlagsUploadErrors(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	c := &jobs.CacheStatsSnapshot{KeysRequested: 4, Misses: 4, Uploads: 4, UploadErrors: 2}
	reportCacheSummary(&buf, &GlobalFlags{}, c)
	if !strings.Contains(buf.String(), "2 upload error(s)") {
		t.Errorf("upload errors should be surfaced: %q", buf.String())
	}
}

func TestReportCacheSummary_ExplainsWarmedHints(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	reportCacheSummary(&buf, &GlobalFlags{}, &jobs.CacheStatsSnapshot{
		KeysRequested: 1, Misses: 1, HintsWarmed: 1,
	})
	out := buf.String()
	if !strings.Contains(out, "1 hint(s) warmed") || !strings.Contains(out, "jobs re-executed") {
		t.Fatalf("hint policy explanation missing: %q", out)
	}
}

func TestFormatBytes(t *testing.T) {
	t.Parallel()
	cases := map[int64]string{
		0:         "0 B",
		512:       "512 B",
		1024:      "1.0 KB",
		1536:      "1.5 KB",
		1048576:   "1.0 MB",
		8_400_000: "8.0 MB",
	}
	for in, want := range cases {
		if got := formatBytes(in); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestRecordedPlanIncludesSerializeAfter keeps the recorded plan's TWO edge
// kinds apart: a functional dependency and a serialize-after edge are different
// scheduling facts, and a snapshot that merged them would make a recorded plan
// unreplayable. It moved from the deleted v1 PlanSnapshot onto the v2 document
// that became the only recorded plan — the same projection execute.go
// writes.
func TestRecordedPlanIncludesSerializeAfter(t *testing.T) {
	t.Parallel()
	planned := []*jobs.ScheduledJob{
		{
			Project:        &workspace.Project{ID: "/app", Name: "app", Path: "app"},
			Extension:      &extension.ExtensionDescription{Name: "@putnami/typescript"},
			JobDef:         &extension.JobDefinition{Name: "build~compile", Cache: true},
			DependsOn:      []string{"/app:build~generate"},
			SerializeAfter: []string{"/app:lint~format"},
		},
	}

	snapshot := machine.SessionPlanFile("session-1", []string{"lint", "build"}, planned)
	if len(snapshot.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(snapshot.Tasks))
	}
	task := snapshot.Tasks[0]
	if len(task.DependsOn) != 1 || task.DependsOn[0] != "/app:build~generate" {
		t.Errorf("DependsOn = %v, want functional dependency", task.DependsOn)
	}
	if len(task.After) != 1 || task.After[0] != "/app:lint~format" {
		t.Errorf("After = %v, want serialize edge", task.After)
	}
	if task.Identity.Key != "/app:build~compile" {
		t.Errorf("identity key = %q, want the plan key", task.Identity.Key)
	}
}

func TestFormatDurationMs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ms   int64
		want string
	}{
		{0, "0ms"},
		{250, "250ms"},
		{2000, "2s"},
		{65000, "1m05s"},
		{3_725_000, "1h02m05s"},
	}
	for _, tt := range tests {
		if got := formatDurationMs(tt.ms); got != tt.want {
			t.Errorf("formatDurationMs(%d) = %q, want %q", tt.ms, got, tt.want)
		}
	}
}
