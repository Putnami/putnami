package engine

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/cli/model/jobs"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
)

func sampleOrchestration() *scheduler.SchedulerResult {
	return &scheduler.SchedulerResult{Session: &jobs.SessionResult{
		Status: jobs.TaskCounts{Succeeded: 10},
		Orchestration: &jobs.OrchestrationSnapshot{
			Phases: []jobs.OrchestrationPhaseWall{
				{Phase: protocolcli.OrchestrationPhaseWorkspaceLoad, Wall: 120 * time.Millisecond},
				{Phase: protocolcli.OrchestrationPhasePlan, Wall: 64 * time.Millisecond},
			},
			Engine: jobs.ProcessLedger{
				Kinds: []jobs.ProcessCount{{Kind: "job", Starts: 10}, {Kind: "git", Starts: 7, CPU: 40 * time.Millisecond}},
				Git:   map[string]int64{"rev-parse": 7},
			},
			Jobs: jobs.ProcessLedger{
				Kinds: []jobs.ProcessCount{{Kind: "other", Starts: 10, CPU: 30 * time.Second}},
			},
			CPUMeasured:  true,
			EngineCPU:    2 * time.Second,
			ExtensionCPU: 460 * time.Millisecond,
			RunCPU:       40 * time.Second,
		},
	}}
}

func TestReportOrchestration_ProfilePrintsTheCPUItsShareAndThePerTaskCost(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "a-run-reports-its-orchestration-cpu", "the-trace-profile-summary-prints-the-orchestration-cpu-share-and-ms-per-task")
	var buf bytes.Buffer
	reportOrchestration(&buf, &GlobalFlags{TraceProfile: "trace.json"}, sampleOrchestration())
	out := buf.String()
	for _, want := range []string{
		"Orchestration CPU: 2s of 40s (6.2%) · 250ms per task",
		"engine 2s · extensions 460ms · control 0ms · cli 0ms · git 40ms",
		"Engine phases: workspace-load 120ms · plan 64ms",
		"Processes (engine): job 10 · git 7",
		"git: rev-parse 7",
		"Processes (jobs): other 10",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

func TestReportOrchestration_SilentWithoutProfileQuietOrReport(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "a-run-reports-its-orchestration-cpu", "the-trace-profile-summary-prints-the-orchestration-cpu-share-and-ms-per-task")
	for name, flags := range map[string]*GlobalFlags{
		"default": {},
		"verbose": {Verbose: true},
		"quiet":   {Quiet: true, TraceProfile: "trace.json"},
	} {
		var buf bytes.Buffer
		reportOrchestration(&buf, flags, sampleOrchestration())
		if buf.Len() != 0 {
			t.Errorf("%s printed %q", name, buf.String())
		}
	}
	var buf bytes.Buffer
	reportOrchestration(&buf, &GlobalFlags{Debug: true}, &scheduler.SchedulerResult{})
	if buf.Len() != 0 {
		t.Errorf("a run without a report printed %q", buf.String())
	}
}
