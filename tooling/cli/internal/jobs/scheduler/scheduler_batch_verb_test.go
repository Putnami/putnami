package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	extensionproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// makeDelegatedVerbPlan is the plan of a delegated stack's lint over two
// projects: each project's lint waits for its own graph step, and b's graph
// step ends well after a's. Each lint is batchable with its cache off, as a
// delegated tool's task is.
func makeDelegatedVerbPlan(t *testing.T, logPath string) (*workspace.Workspace, []*ScheduledJob) {
	t.Helper()
	ws, lints := makeBatchSchedulerFixture(t, logPath)
	okResult := `{"v":2,"type":"result","data":{"status":"OK"}}`
	graphFast := filepath.Join(ws.Root, "graph-fast.sh")
	graphSlow := filepath.Join(ws.Root, "graph-slow.sh")
	writeExecutable(t, graphFast, "#!/bin/sh\nprintf '%s\\n' "+shellQuote(okResult)+"\n")
	writeExecutable(t, graphSlow, "#!/bin/sh\nsleep 0.4\nprintf '%s\\n' "+shellQuote(okResult)+"\n")

	planned := make([]*ScheduledJob, 0, len(lints)*2)
	for _, lint := range lints {
		command := graphFast
		if lint.Project.Path == "b" {
			command = graphSlow
		}
		graph := &ScheduledJob{
			Project:   lint.Project,
			Extension: lint.Extension,
			Step:      &extension.PipelineStep{ID: "graph", Task: "graph-run"},
			JobDef: &extension.JobDefinition{
				ExtensionName: lint.Extension.Name,
				Name:          "lint~graph",
				CommandName:   "lint",
				StepID:        "graph",
				Command:       command,
				Cwd:           "{projectRoot}",
			},
		}
		lint.JobDef.Cache = false
		lint.JobDef.TaskCachePolicy = nil
		lint.DependsOn = []string{graph.Key()}
		planned = append(planned, graph, lint)
	}
	return ws, planned
}

// delegatedVerbScheduler is the one scheduler the delegated-verb cases run:
// "a verb waits for its peers" is a property of takeAdmissiblePendingGroup and
// the coordinator's readiness together, scheduler methods both.
func delegatedVerbScheduler(ws *workspace.Workspace, planned []*ScheduledJob, serveSteps ...string) *Scheduler {
	cfg := SchedulerConfig{MaxParallel: 2}
	for _, key := range serveSteps {
		if cfg.ServeSteps == nil {
			cfg.ServeSteps = map[string]bool{}
		}
		cfg.ServeSteps[key] = true
	}
	return newScheduler(ws, planned, nil, cfg, &mockRenderer{}, nil)
}

func TestADelegatedVerbWaitsForItsPeersAndRunsOneCommand(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable")
	}
	logPath := filepath.Join(t.TempDir(), "invocations")
	ws, planned := makeDelegatedVerbPlan(t, logPath)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result := delegatedVerbScheduler(ws, planned).Run(ctx)
	if !result.Success {
		t.Fatalf("delegated verb failed: %+v", result.Results)
	}
	if got := invocationCount(t, logPath); got != 1 {
		t.Fatalf("lint invocations = %d, want one command for the verb", got)
	}
	if len(result.Results) != 4 {
		t.Fatalf("result rows = %d, want two graph steps and two lints", len(result.Results))
	}
	for _, job := range planned {
		if r := result.Results[job.Key()]; r == nil || r.Status != "success" {
			t.Fatalf("%s = %+v, want success", job.Key(), r)
		}
	}
}

func TestALaterWaveOfADelegatedVerbRunsItsOwnCommand(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable")
	}
	logPath := filepath.Join(t.TempDir(), "invocations")
	ws, planned := makeDelegatedVerbPlan(t, logPath)
	// a's lint waits for b's: it is of the verb's second wave, and must not
	// hold b's back.
	var lintA, lintB *ScheduledJob
	for _, job := range planned {
		if job.JobDef.Batchable == nil {
			continue
		}
		if job.Project.Path == "a" {
			lintA = job
		} else {
			lintB = job
		}
	}
	lintA.DependsOn = append(lintA.DependsOn, lintB.Key())

	waves := newVerbWaves(planned, nil)
	if waves == nil || waves.verbs[lintB.Key()] == "" || waves.verbs[lintA.Key()] != "" {
		t.Fatalf("first wave = %+v, want b's lint alone", waves)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result := delegatedVerbScheduler(ws, planned).Run(ctx)
	if !result.Success {
		t.Fatalf("delegated verb failed: %+v", result.Results)
	}
	if got := invocationCount(t, logPath); got != 2 {
		t.Fatalf("lint invocations = %d, want one per wave", got)
	}
}

func TestACachedBatchNeverWaitsForItsPeers(t *testing.T) {
	t.Parallel()
	_, planned := makeBatchSchedulerFixture(t, "")
	if waves := newVerbWaves(planned, nil); waves != nil {
		t.Fatalf("a plan of cached batches has verb waves %+v, want none", waves)
	}
}

func TestAWaitingTaskRunsWhenNothingIsInFlight(t *testing.T) {
	t.Parallel()
	ws, planned := makeDelegatedVerbPlan(t, "")
	s := delegatedVerbScheduler(ws, planned)
	s.verbWaves = newVerbWaves(planned, nil)
	var lintA *ScheduledJob
	for _, job := range planned {
		if job.JobDef.Batchable != nil && job.Project.Path == "a" {
			lintA = job
		}
	}
	s.verbWaves.ready(lintA)
	pending := []*ScheduledJob{lintA}
	if key, _ := s.pendingBatch(lintA); key == "" {
		t.Fatal("a delegated lint has no batch key: it would never batch")
	}

	if _, _, admitted := s.takeAdmissiblePendingGroup(pending, true); admitted {
		t.Fatal("a's lint ran while b's is not ready and work is in flight")
	}
	group, rest, admitted := s.takeAdmissiblePendingGroup(pending, false)
	if !admitted || len(group.jobs) != 1 || group.jobs[0] != lintA || len(rest) != 0 {
		t.Fatalf("with nothing in flight: group %+v, rest %d, admitted %v; want a's lint", group.jobs, len(rest), admitted)
	}
	s.resources.release(group.reservation)
}

func TestADelegatedVerbSharesItsKeyAcrossTheProjectsOwnOptions(t *testing.T) {
	t.Parallel()
	ws, planned := makeDelegatedVerbPlan(t, "")
	s := delegatedVerbScheduler(ws, planned)
	var lints []*ScheduledJob
	for _, job := range planned {
		if job.JobDef.Batchable != nil {
			lints = append(lints, job)
		}
	}
	// Each project's own layer of the extension differs, as a sync writes
	// it per project; the stack reads it from the project itself.
	for i, lint := range lints {
		var config wsproto.ProjectConfig
		if err := json.Unmarshal([]byte(fmt.Sprintf(`{"options": {%q: {"filePatterns": ["%d/x"]}, %q: {"depth": %d}, "lint": {"shared": true}}}`,
			lint.Extension.Name, i, lint.Extension.Name+":lint", i)), &config); err != nil {
			t.Fatal(err)
		}
		lint.Project.Config = &config
	}
	a, _ := s.readyBatch(lints[0])
	b, _ := s.readyBatch(lints[1])
	if a == "" || a != b {
		t.Fatalf("batch keys %q and %q, want one key for the verb", a, b)
	}
	// A cached batch keeps folding them: its group shares one context.
	for _, lint := range lints {
		lint.JobDef.Cache = true
	}
	a, _ = s.readyBatch(lints[0])
	b, _ = s.readyBatch(lints[1])
	if a == b {
		t.Fatal("a cached batch ignores the projects' own options")
	}
	// A layer every member reads alike still keys the verb.
	for _, lint := range lints {
		lint.JobDef.Cache = false
	}
	lints[1].Project.Config.Options["lint"]["shared"] = false
	a, _ = s.readyBatch(lints[0])
	if b, _ = s.readyBatch(lints[1]); a == "" || a == b {
		t.Error("a delegated verb ignores the command's own project layer")
	}
}

func TestTwoVerbsWaitingOnEachOtherRunWhileAServeIsInFlight(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable")
	}
	logPath := filepath.Join(t.TempDir(), "invocations")
	ws, lints := makeBatchSchedulerFixture(t, logPath)
	// lint and test are two delegated verbs. b's lint waits for a's test and
	// b's test for a's lint: each verb's ready task waits for a peer that
	// waits on the other verb.
	var planned []*ScheduledJob
	tests := map[string]*ScheduledJob{}
	for _, lint := range lints {
		lint.JobDef.Cache = false
		lint.JobDef.TaskCachePolicy = nil
		test := *lint
		def := *lint.JobDef
		test.JobDef = &def
		test.JobDef.Name = "test~check"
		test.JobDef.CommandName = "test"
		test.JobDef.StepID = "check"
		test.Step = &extension.PipelineStep{ID: "check", Task: "lint-all"}
		tests[lint.Project.Path] = &test
		planned = append(planned, lint, &test)
	}
	for _, job := range planned {
		if job.Project.Path != "b" {
			continue
		}
		other := "test~check"
		if job.JobDef.Name == "test~check" {
			other = "lint~check-only"
		}
		for _, peer := range planned {
			if peer.Project.Path == "a" && peer.JobDef.Name == other {
				job.DependsOn = append(job.DependsOn, peer.Key())
			}
		}
	}
	// A serve dispatched first runs until a task of either verb ran.
	serveCommand := filepath.Join(ws.Root, "serve.sh")
	writeExecutable(t, serveCommand, "#!/bin/sh\nwhile [ ! -s "+shellQuote(logPath)+" ]; do sleep 0.1; done\n"+
		"printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\"}}'\n")
	serve := &ScheduledJob{
		Project:        lints[0].Project,
		Extension:      lints[0].Extension,
		Step:           &extension.PipelineStep{ID: "serve", Task: "serve"},
		CriticalPathMs: 1_000_000,
		JobDef: &extension.JobDefinition{
			ExtensionName: lints[0].Extension.Name,
			Name:          "serve~serve",
			CommandName:   "serve",
			StepID:        "serve",
			Command:       serveCommand,
			Cwd:           "{projectRoot}",
		},
	}
	planned = append(planned, serve)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result := delegatedVerbScheduler(ws, planned, serve.Key()).Run(ctx)
	if !result.Success {
		t.Fatalf("two waiting verbs beside a serve: %+v", result.Results)
	}
	if len(result.Results) != len(planned) {
		t.Fatalf("result rows = %d, want %d", len(result.Results), len(planned))
	}
}

func TestAFiniteStepOfAServePipelineCountsAsWorkThatEnds(t *testing.T) {
	t.Parallel()
	_, planned := makeDelegatedVerbPlan(t, "")
	// A serve pipeline's finite step carries the serve trait too; only the
	// plan's serve steps run until interrupted.
	finite := planned[0]
	finite.JobDef.Traits.Workload = extensionproto.WorkloadServe
	serve := planned[2]
	waves := newVerbWaves(planned, map[string]bool{serve.Key(): true})
	waves.track([]*ScheduledJob{finite, serve}, 1)
	if got := waves.serves(); got != 1 {
		t.Errorf("serves = %d, want the serve step alone", got)
	}
	waves.track([]*ScheduledJob{finite, serve}, -1)
	if got := waves.serves(); got != 0 {
		t.Errorf("serves after completion = %d, want 0", got)
	}
}
