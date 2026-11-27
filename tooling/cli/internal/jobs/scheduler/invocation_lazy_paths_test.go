package scheduler

import (
	"context"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/planner"
	"go.putnami.dev/tooling/cli/internal/store"

	extensionproto "go.putnami.dev/protocol/extension"
)

// A consumer that defers to its setup asks the stores once: the run counts
// one local miss for it, and its second opening claims the key it missed at.
func TestLazySetup_ADeferredConsumerCountsOneMiss(t *testing.T) {
	spectest.Proves(t, lazySetupFeature, lazySetupRequirement, "a-consumer-that-misses-runs-after-its-setup")
	t.Parallel()
	f := newLazyFixture(t, "a", "b", "c")
	cm := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
	f.cacheConsumers(t, cm, "a", "c")
	result := f.run(t, cm, 2, nil)
	if !result.Success {
		t.Fatalf("run failed: %+v", result.Results)
	}
	assertStartedOnce(t, result, f.projects["b"])
	if result.Cache == nil || result.Cache.LocalMisses != 1 || result.Cache.LocalHits != 2 {
		t.Fatalf("cache stats = %+v, want 2 local hits and the one consumer's single miss", result.Cache)
	}
}

// A consumer that waits for the setup only through another consumer (the
// test-race shape) starts the setup on its own miss, after the first
// consumer was served from the cache.
func TestLazySetup_AChainedConsumerThatMissesStartsTheSetup(t *testing.T) {
	spectest.Proves(t, lazySetupFeature, lazySetupRequirement, "a-consumer-that-misses-runs-after-its-setup")
	t.Parallel()
	f := newLazyFixture(t, "a")
	p := f.projects["a"]
	race := *p.consumer
	step := *p.consumer.Step
	step.ID, step.DependsOn = "race", []string{"run"}
	race.Step = &step
	def := *p.consumer.JobDef
	def.Name, def.StepID = extension.StepJobName("test", "race"), "race"
	raced := filepath.Join(f.ws.Root, "a-race-consumed")
	def.Args = []string{"{invocationArtifactRoot}/database/dsn.env", raced}
	race.JobDef = &def
	race.DependsOn = []string{p.consumer.Key()}
	p.finalizer.Step.Finalizes.Consumers = []string{"run", "race"}
	f.planned = append(f.planned, &race)
	planner.ResolveInvocationRelations(f.planned)
	cm := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
	f.cacheConsumers(t, cm, "a")

	result := f.run(t, cm, 1, nil)
	if !result.Success {
		t.Fatalf("run failed: %+v", result.Results)
	}
	if got := result.Results[p.consumer.Key()]; got == nil || !got.ReuseKind().CacheHit() {
		t.Fatalf("first consumer = %+v, want a cache hit", got)
	}
	if got := result.Results[race.Key()]; got == nil || got.Status != "success" || got.ReuseKind().CacheHit() {
		t.Fatalf("chained consumer = %+v, want an execution", got)
	}
	if producer := result.Results[p.producer.Key()]; producer == nil || producer.Status != "success" {
		t.Fatalf("setup = %+v, want it to run for the chained consumer", producer)
	}
	if got := countLines(t, p.teardowns); got != 1 {
		t.Fatalf("teardown ran %d times, want exactly once", got)
	}
}

// A job with external effects after a frontier served from the cache keeps
// its grant: the setup nobody requested is not an ancestor it waits for.
func TestLazySetup_AProtectedJobAfterAnAllHitFrontierKeepsItsGrant(t *testing.T) {
	spectest.Proves(t, lazySetupFeature, lazySetupRequirement, "an-all-hit-frontier-starts-no-setup-and-no-finalizer")
	f := newLazyFixture(t, "a")
	p := f.projects["a"]
	command := writeCapabilityCommand(t, f.ws.Root, "pass.sh", `#!/bin/sh
printf '%s\n' '{"v":2,"type":"result","data":{"status":"success"}}'
`)
	effect := capabilityTestJob(f.ws.Root, p.consumer.Project, "publish~docker", "publish", command, extensionproto.SideEffectsRegistry, p.consumer.Key())
	f.planned = append(f.planned, effect)
	cm := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
	f.cacheConsumers(t, cm, "a")
	ctx := capturedCapabilityContext(t, "resource-scoped-test-token", "test")
	authorization, err := AuthorizeProcessCapabilities(ctx, f.planned, true)
	if err != nil || authorization == nil {
		t.Fatalf("AuthorizeProcessCapabilities = %v, %v", authorization, err)
	}
	result := f.runWith(ctx, cm, SchedulerConfig{MaxParallel: 2}, nil, func(s *Scheduler) {
		s.processCapabilities = newProcessCapabilityRuntime(authorization, f.planned)
	})
	if got := result.Results[effect.Key()]; got == nil || got.Status != "success" {
		t.Fatalf("protected job = %+v, want it to run", got)
	}
	assertNotStarted(t, result, p)
}

// A setup a consumer requested still tears down exactly once when an
// unrelated failure stops the run while the setup runs, with or without
// --continue-on-error.
func TestLazySetup_ARequestedSetupTearsDownOnceWhenTheRunStops(t *testing.T) {
	spectest.Proves(t, lazySetupFeature, lazySetupRequirement, "a-consumer-that-misses-runs-after-its-setup")
	for _, continueOnError := range []bool{false, true} {
		f := newLazyFixture(t, "a")
		p := f.projects["a"]
		fixtureTask(t, p.producer.JobDef, fixtureScript{{"sleep", "1.5"}, {"provision"}})
		task := p.producer.Extension.Tasks["provision"]
		task.Command = p.producer.JobDef.Command
		p.producer.Extension.Tasks["provision"] = task
		failing := &extension.JobDefinition{Name: extension.StepJobName("test", "boom"), ExtensionName: p.producer.Extension.Name, CommandName: "test", StepID: "boom", TimeoutMs: unboundedJobTimeoutMs}
		fixtureTask(t, failing, fixtureScript{{"sleep", "0.5"}, {"exit", "7"}})
		f.planned = append(f.planned, &ScheduledJob{Project: p.producer.Project, Extension: p.producer.Extension, Step: &extension.PipelineStep{ID: "boom", Task: "teardown"}, JobDef: failing})
		cm := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
		f.runWith(context.Background(), cm, SchedulerConfig{MaxParallel: 3, ContinueOnError: continueOnError}, nil, nil)
		if got := countLines(t, p.teardowns); got != 1 {
			t.Fatalf("continue-on-error=%v: teardown ran %d times, want exactly once", continueOnError, got)
		}
	}
}
