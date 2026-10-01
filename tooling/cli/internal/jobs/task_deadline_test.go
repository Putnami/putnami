// Per-project task deadline tuning and the runtime handoff.
package jobs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	extensionproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func deadlineTestJob(manifestTimeoutMs int, tuning map[string]wsproto.ProjectTaskTuning) *ScheduledJob {
	project := &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"}
	if tuning != nil {
		project.Config = &wsproto.ProjectConfig{Name: "proj", Tasks: tuning}
	}
	return &ScheduledJob{
		Project:   project,
		Extension: &extension.ExtensionDescription{Name: "@putnami/go"},
		JobDef: &extension.JobDefinition{
			Name:          "lint~golangci-lint",
			CommandName:   "lint",
			ExtensionName: "@putnami/go",
			TimeoutMs:     manifestTimeoutMs,
		},
	}
}

func deadlinePtr(ms int) *int { return &ms }

func TestEffectiveTimeoutMsResolvesProjectTaskTuning(t *testing.T) {
	weight := 4.0
	for _, tc := range []struct {
		name string
		job  *ScheduledJob
		want int
	}{
		{
			name: "full step name wins over command name",
			job: deadlineTestJob(600_000, map[string]wsproto.ProjectTaskTuning{
				"lint":               {TimeoutMs: deadlinePtr(700_000)},
				"lint~golangci-lint": {TimeoutMs: deadlinePtr(900_000)},
			}),
			want: 900_000,
		},
		{
			name: "command entry applies when step has another tuning field",
			job: deadlineTestJob(600_000, map[string]wsproto.ProjectTaskTuning{
				"lint":               {TimeoutMs: deadlinePtr(700_000)},
				"lint~golangci-lint": {CPUWeight: &weight},
			}),
			want: 700_000,
		},
		{
			name: "unrelated step does not leak",
			job: deadlineTestJob(600_000, map[string]wsproto.ProjectTaskTuning{
				"lint~staticcheck": {TimeoutMs: deadlinePtr(900_000)},
			}),
			want: 600_000,
		},
		{
			name: "project tuning overrides an unbounded manifest",
			job: deadlineTestJob(-1, map[string]wsproto.ProjectTaskTuning{
				"lint": {TimeoutMs: deadlinePtr(900_000)},
			}),
			want: 900_000,
		},
		{
			name: "batch node override wins over project tuning",
			job: func() *ScheduledJob {
				job := deadlineTestJob(600_000, map[string]wsproto.ProjectTaskTuning{
					"lint": {TimeoutMs: deadlinePtr(900_000)},
				})
				job.TimeoutOverrideMs = deadlinePtr(2_700_000)
				return job
			}(),
			want: 2_700_000,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.job.EffectiveTimeoutMs(); got != tc.want {
				t.Fatalf("EffectiveTimeoutMs() = %d, want %d", got, tc.want)
			}
		})
	}

	var nilJob *ScheduledJob
	if got := nilJob.EffectiveTimeoutMs(); got != 0 {
		t.Fatalf("nil job deadline = %d, want 0", got)
	}
}

// TestTaskTimeoutTuningLeavesCacheKeyUnchanged proves the execution-only
// promise at the actual cache-key boundary. The task declares putnami.json as
// an input exactly as extension manifests do; changing a scheduler tuning must
// not split its cache history, while source and non-tuning config edits still
// must move the key.
func TestTaskTimeoutTuningLeavesCacheKeyUnchanged(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	configPath := filepath.Join(ws.Root, "proj", wsproto.ConfigFilename)
	writeFileAt(t, filepath.Join(ws.Root, "proj", "src", "main.go"), "package main\n")

	writeConfig := func(body string) {
		t.Helper()
		writeFileAt(t, configPath, body)
	}
	hashFor := func() string {
		t.Helper()
		job := deadlineTestJob(600_000, nil)
		job.JobDef.Cache = true
		job.JobDef.FilePatterns = []string{"src/**/*", wsproto.ConfigFilename}
		declareCacheTestTask(job, "golangci-lint")
		cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
		hash, err := computeJobCacheHash(ws, job, nil, nil, cache, nil)
		if err != nil {
			t.Fatalf("compute cache key: %v", err)
		}
		return hash
	}

	const baseConfig = `{"name":"proj","tags":["go"]}`
	writeConfig(baseConfig)
	baseline := hashFor()

	writeFileAt(t, filepath.Join(ws.Root, "proj", "src", "main.go"), "package main // changed\n")
	if moved := hashFor(); moved == baseline {
		t.Fatal("a source edit did not move the cache key; the fixture is not live")
	}
	writeFileAt(t, filepath.Join(ws.Root, "proj", "src", "main.go"), "package main\n")

	writeConfig(`{"name":"proj","tags":["go","changed"]}`)
	if moved := hashFor(); moved == baseline {
		t.Fatal("a non-tuning project config edit did not move the cache key")
	}

	for _, config := range []string{
		`{"name":"proj","tags":["go"],"tasks":{"lint":{"timeoutMs":900000}}}`,
		`{"name":"proj","tags":["go"],"tasks":{"lint~golangci-lint":{"timeoutMs":120000}}}`,
		`{"name":"proj","tags":["go"],"tasks":{"lint":{"cpuWeight":4},"build":{"timeoutMs":1}}}`,
	} {
		writeConfig(config)
		if got := hashFor(); got != baseline {
			t.Errorf("cache key = %s, want untuned key %s", got, baseline)
		}
	}
}

func TestTunedDeadlineReachesEverySchedulerConsumer(t *testing.T) {
	const tuned = 900_000
	tuning := map[string]wsproto.ProjectTaskTuning{
		"lint~golangci-lint": {TimeoutMs: deadlinePtr(tuned)},
	}

	t.Run("runner context", func(t *testing.T) {
		root := t.TempDir()
		ws := &workspace.Workspace{Root: root, Name: "test"}
		job := deadlineTestJob(5_000, tuning)
		job.Project.Path = "."
		job.Extension.Path = root
		job.JobDef.Kind = "command"
		job.JobDef.Command = "/bin/echo"

		ctx, cancel, invocation, err := prepareJobInvocation(context.Background(), ws, job, nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("prepare job invocation: %v", err)
		}
		defer cancel()
		defer func() { _ = os.Remove(invocation.contextFile) }()
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= time.Minute {
			t.Fatalf("runner deadline = %v (present=%t), want tuned %dms", deadline, ok, tuned)
		}
	})

	t.Run("coalesced waiter", func(t *testing.T) {
		scheduler := &Scheduler{cfg: SchedulerConfig{Retry: 2}}
		if got, want := scheduler.cacheLeaseWaitTimeout(deadlineTestJob(5_000, tuning)), 3*tuned*time.Millisecond; got != want {
			t.Fatalf("waiter deadline = %v, want %v", got, want)
		}
	})

	t.Run("finalizer", func(t *testing.T) {
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		cleanup, release := finalizerCleanupContext(canceled, deadlineTestJob(5_000, tuning))
		defer release()
		deadline, ok := cleanup.Deadline()
		if !ok || time.Until(deadline) <= time.Minute {
			t.Fatalf("finalizer deadline = %v (present=%t), want tuned %dms", deadline, ok, tuned)
		}
	})

	t.Run("single member batch leader", func(t *testing.T) {
		leader := batchLeader([]taskWork{{job: deadlineTestJob(5_000, tuning)}})
		if got := leader.EffectiveTimeoutMs(); got != tuned {
			t.Fatalf("leader deadline = %d, want %d", got, tuned)
		}
	})
}

func TestBatchGroupTimeoutMsUsesLargestMemberDeadline(t *testing.T) {
	member := func(manifest int, override *int) taskWork {
		var tuning map[string]wsproto.ProjectTaskTuning
		if override != nil {
			tuning = map[string]wsproto.ProjectTaskTuning{"lint": {TimeoutMs: override}}
		}
		return taskWork{job: deadlineTestJob(manifest, tuning)}
	}

	for _, tc := range []struct {
		name string
		work []taskWork
		want int
	}{
		{"largest override scales by group size", []taskWork{member(600_000, deadlinePtr(700_000)), member(600_000, deadlinePtr(900_000)), member(600_000, nil)}, 2_700_000},
		{"default is normalized before max", []taskWork{member(0, nil), member(100, nil)}, DefaultTimeoutMs * 2},
		{"unbounded member leaves group unbounded", []taskWork{member(600_000, deadlinePtr(900_000)), member(-1, nil)}, -1},
		{"singleton keeps its own sentinel", []taskWork{member(0, nil)}, 0},
		{"empty group uses default sentinel", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := batchGroupTimeoutMs(tc.work); got != tc.want {
				t.Fatalf("batchGroupTimeoutMs() = %d, want %d", got, tc.want)
			}
		})
	}

	if got := boundedDeadlineMs(int(^uint(0)>>1), 2); got <= 0 {
		t.Fatalf("overflow-safe deadline = %d, want a positive saturated value", got)
	}
}

func TestBatchLeaderDeadlineDoesNotMutateMembers(t *testing.T) {
	shared := &extension.JobDefinition{
		Name: "lint~golangci-lint", CommandName: "lint", ExtensionName: "@putnami/go", TimeoutMs: 600_000,
	}
	work := make([]taskWork, 0, 3)
	members := make([]*ScheduledJob, 0, 3)
	for i := 0; i < 3; i++ {
		job := deadlineTestJob(600_000, map[string]wsproto.ProjectTaskTuning{
			"lint": {TimeoutMs: deadlinePtr(700_000)},
		})
		job.Project.ID = fmt.Sprintf("/proj-%d", i)
		job.JobDef = shared
		members = append(members, job)
		work = append(work, taskWork{job: job})
	}

	leader := batchLeader(work)
	if got, want := leader.EffectiveTimeoutMs(), 2_100_000; got != want {
		t.Fatalf("leader deadline = %d, want %d", got, want)
	}
	if shared.TimeoutMs != 600_000 {
		t.Fatalf("shared manifest timeout = %d, want 600000", shared.TimeoutMs)
	}
	for i, member := range members {
		if member.TimeoutOverrideMs != nil || member.EffectiveTimeoutMs() != 700_000 {
			t.Fatalf("member %d was mutated by batch leader: %+v", i, member)
		}
	}
}

func TestApplyTaskDeadline(t *testing.T) {
	prefix := extensionproto.TaskDeadlineMsEnv + "="
	lastValue := func(env []string) (string, bool) {
		value, found := "", false
		for _, entry := range env {
			if strings.HasPrefix(entry, prefix) {
				value, found = strings.TrimPrefix(entry, prefix), true
			}
		}
		return value, found
	}

	tuned := deadlineTestJob(600_000, map[string]wsproto.ProjectTaskTuning{
		"lint": {TimeoutMs: deadlinePtr(900_000)},
	})
	if got, ok := lastValue(applyTaskDeadline([]string{"PATH=/bin"}, tuned)); !ok || got != "900000" {
		t.Fatalf("exported deadline = %q (present=%t), want 900000", got, ok)
	}

	if got, ok := lastValue(applyTaskDeadline([]string{"PATH=/bin"}, deadlineTestJob(0, nil))); !ok || got != fmt.Sprint(DefaultTimeoutMs) {
		t.Fatalf("default deadline = %q (present=%t), want %d", got, ok, DefaultTimeoutMs)
	}

	for name, job := range map[string]*ScheduledJob{
		"unbounded job": deadlineTestJob(-1, nil),
		"nil job":       nil,
		"unbounded batch": batchLeader([]taskWork{
			{job: deadlineTestJob(600_000, nil)}, {job: deadlineTestJob(-1, nil)},
		}),
	} {
		env := applyTaskDeadline([]string{"PATH=/bin", prefix + "123", "HOME=/tmp", prefix + "456"}, job)
		if got, ok := lastValue(env); ok {
			t.Errorf("%s left deadline %q in child environment", name, got)
		}
		if got, want := strings.Join(env, ","), "PATH=/bin,HOME=/tmp"; got != want {
			t.Errorf("%s environment = %q, want %q", name, got, want)
		}
	}

	leader := batchLeader([]taskWork{
		{job: deadlineTestJob(600_000, nil)},
		{job: deadlineTestJob(600_000, map[string]wsproto.ProjectTaskTuning{"lint": {TimeoutMs: deadlinePtr(900_000)}})},
	})
	if got, ok := lastValue(applyTaskDeadline([]string{"PATH=/bin"}, leader)); !ok || got != "1800000" {
		t.Fatalf("batch export = %q (present=%t), want 1800000", got, ok)
	}
}
