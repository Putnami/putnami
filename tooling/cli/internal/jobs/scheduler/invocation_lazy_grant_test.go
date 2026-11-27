package scheduler

import (
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/planner"
	"go.putnami.dev/tooling/cli/internal/store"

	extensionproto "go.putnami.dev/protocol/extension"
)

func protectedAfterLazySetup(t *testing.T, f *lazyFixture, after string, deps ...string) (*ScheduledJob, func(*Scheduler)) {
	t.Helper()
	p := f.projects["a"]
	command := writeCapabilityCommand(t, f.ws.Root, "pass.sh", `#!/bin/sh
printf '%s\n' '{"v":2,"type":"result","data":{"status":"success"}}'
`)
	effect := capabilityTestJob(f.ws.Root, p.consumer.Project, "publish~docker", "publish", command, extensionproto.SideEffectsRegistry, deps...)
	f.planned = append(f.planned, effect)
	ctx := capturedCapabilityContext(t, "resource-scoped-test-token", after)
	authorization, err := AuthorizeProcessCapabilities(ctx, f.planned, true)
	if err != nil || authorization == nil {
		t.Fatalf("AuthorizeProcessCapabilities = %v, %v", authorization, err)
	}
	return effect, func(s *Scheduler) { s.processCapabilities = newProcessCapabilityRuntime(authorization, f.planned) }
}

// A protected job after a consumer that missed is granted once the setup the
// consumer requested succeeded.
func TestLazySetup_ARequestedSetupThatSucceedsGrantsTheProtectedJob(t *testing.T) {
	spectest.Proves(t, lazySetupFeature, lazySetupRequirement, "a-consumer-that-misses-runs-after-its-setup")
	f := newLazyFixture(t, "a")
	p := f.projects["a"]
	effect, prepare := protectedAfterLazySetup(t, f, "test", p.consumer.Key())
	cm := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
	result := f.runWith(t.Context(), cm, SchedulerConfig{MaxParallel: 2}, nil, prepare)
	assertStartedOnce(t, result, p)
	if got := result.Results[effect.Key()]; got == nil || got.Status != "success" {
		t.Fatalf("protected = %+v, want success", got)
	}
}

// A protected job after a consumer whose requested setup failed is never
// granted, with or without --continue-on-error.
func TestLazySetup_ARequestedSetupThatFailsDeniesTheProtectedJob(t *testing.T) {
	spectest.Proves(t, lazySetupFeature, lazySetupRequirement, "a-consumer-that-misses-runs-after-its-setup")
	for _, coe := range []bool{false, true} {
		f := newLazyFixture(t, "a")
		p := f.projects["a"]
		fixtureTask(t, p.producer.JobDef, fixtureScript{{"exit", "7"}})
		task := p.producer.Extension.Tasks["provision"]
		task.Command = p.producer.JobDef.Command
		p.producer.Extension.Tasks["provision"] = task
		effect, prepare := protectedAfterLazySetup(t, f, "test", p.consumer.Key())
		cm := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
		result := f.runWith(t.Context(), cm, SchedulerConfig{MaxParallel: 2, ContinueOnError: coe}, nil, prepare)
		if got := result.Results[effect.Key()]; got != nil && got.Status == "success" {
			t.Fatalf("coe=%v protected = %+v, want denied", coe, got)
		}
	}
}

// A sibling consumer, not an ancestor of the protected job, requests the setup
// while the protected job's own consumer is served from the cache: the job is
// granted, as it is when the setup is eager.
func TestLazySetup_ASiblingRequestKeepsTheGrantOfAJobWhoseConsumerHit(t *testing.T) {
	spectest.Proves(t, lazySetupFeature, lazySetupRequirement, "an-all-hit-frontier-starts-no-setup-and-no-finalizer")
	for _, eager := range []bool{false, true} {
		t.Run(map[bool]string{false: "lazy", true: "eager"}[eager], func(t *testing.T) { grantWithASiblingRequest(t, eager) })
	}
}

func grantWithASiblingRequest(t *testing.T, eager bool) {
	f := newLazyFixture(t, "a")
	if eager {
		f.projects["a"].finalizer.Step.Finalizes.PruneIf = "false"
	}
	p := f.projects["a"]
	fixtureTask(t, p.producer.JobDef, fixtureScript{{"sleep", "1.5"}, {"provision"}})
	task := p.producer.Extension.Tasks["provision"]
	task.Command = p.producer.JobDef.Command
	p.producer.Extension.Tasks["provision"] = task

	race := *p.consumer
	step := *p.consumer.Step
	step.ID = "race"
	race.Step = &step
	def := *p.consumer.JobDef
	def.Name, def.StepID = extension.StepJobName("test", "race"), "race"
	def.Args = []string{"{invocationArtifactRoot}/database/dsn.env", filepath.Join(f.ws.Root, "a-race-consumed")}
	race.JobDef = &def
	race.DependsOn = []string{p.producer.Key()}
	p.finalizer.Step.Finalizes.Consumers = []string{"run", "race"}

	gateCmd := writeCapabilityCommand(t, f.ws.Root, "gate.sh", `#!/bin/sh
sleep 0.5
printf '%s\n' '{"v":2,"type":"result","data":{"status":"success"}}'
`)
	gate := capabilityTestJob(f.ws.Root, p.consumer.Project, "build~gate", "build", gateCmd, "")
	p.consumer.DependsOn = append(p.consumer.DependsOn, gate.Key())
	f.planned = append(f.planned, &race, gate)
	planner.ResolveInvocationRelations(f.planned)
	cm := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
	f.cacheConsumers(t, cm, "a")
	effect, prepare := protectedAfterLazySetup(t, f, "build", p.consumer.Key())
	result := f.runWith(t.Context(), cm, SchedulerConfig{MaxParallel: 3}, nil, prepare)
	if got := result.Results[effect.Key()]; got == nil || got.Status != "success" {
		t.Fatalf("protected = %+v, want success: its own consumer was served from the cache", got)
	}
}
