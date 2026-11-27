package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/jobs/planner"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"

	extensionproto "go.putnami.dev/protocol/extension"
)

const (
	lazySetupFeature     = "cli/job-planning-execution"
	lazySetupRequirement = "lazy-invocation-setup"
)

// lazyProject is one project's finalizes relation in a lazyFixture: the
// database test environment shape, setup, a cacheable test that reads it, and
// a teardown with runOn: finally.
type lazyProject struct {
	producer, consumer, finalizer *ScheduledJob
	resource, teardowns, consumed string
}

// lazyFixture is a workspace whose projects each hold one relation, as the Go
// and TypeScript extensions plan one per project with a test database.
type lazyFixture struct {
	ws       *workspace.Workspace
	planned  []*ScheduledJob
	projects map[string]*lazyProject
}

func newLazyFixture(t *testing.T, names ...string) *lazyFixture {
	t.Helper()
	root := t.TempDir()
	f := &lazyFixture{projects: map[string]*lazyProject{}}
	var projects []*workspace.Project
	for _, name := range names {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		project := &workspace.Project{ID: "/" + name, Name: name, Path: name}
		projects = append(projects, project)
		p := &lazyProject{
			resource:  filepath.Join(root, name+"-resource"),
			teardowns: filepath.Join(root, name+"-teardowns"),
			consumed:  filepath.Join(root, name+"-consumed"),
		}
		ext := &extension.ExtensionDescription{
			Name: "@acme/" + name,
			Path: t.TempDir(),
			Tasks: map[string]extension.TaskDefinition{
				"provision": {
					Kind: "command",
					Declares: &extensionproto.TaskDeclaration{
						Outputs: map[string]extensionproto.DeclaredOutput{
							"dsn": {
								Kind:      extensionproto.OutputKindRuntimeFile,
								Scope:     extensionproto.OutputScopeInvocation,
								Sensitive: true,
								Path:      "database/dsn.env",
							},
						},
						Effects: []string{extensionproto.EffectProcess},
					},
				},
				"consume": {
					Kind: "command",
					Declares: &extensionproto.TaskDeclaration{
						Outputs: map[string]extensionproto.DeclaredOutput{
							"report": {Kind: extensionproto.OutputKindFile, Path: "report.txt", OptionalEmpty: true},
						},
					},
				},
				"teardown": {Kind: "command"},
			},
		}
		node := func(step extension.PipelineStep, script fixtureScript, args, deps []string, cache bool) *ScheduledJob {
			local := step
			job := &ScheduledJob{
				Project:   project,
				Extension: ext,
				Step:      &local,
				JobDef: &extension.JobDefinition{
					Name:          extension.StepJobName("test", step.ID),
					ExtensionName: ext.Name,
					CommandName:   "test",
					StepID:        step.ID,
					Args:          args,
					Cache:         cache,
					TimeoutMs:     unboundedJobTimeoutMs,
				},
				DependsOn: deps,
			}
			fixtureTask(t, job.JobDef, script)
			task := ext.Tasks[step.Task]
			task.Command = job.JobDef.Command
			ext.Tasks[step.Task] = task
			return job
		}
		artifact := "{invocationArtifactRoot}/database/dsn.env"
		p.producer = node(extension.PipelineStep{ID: "setup", Task: "provision"}, provisionBody,
			[]string{artifact, "{invocationArtifactRoot}", p.resource, filepath.Join(root, name+"-observed")}, nil, false)
		p.consumer = node(extension.PipelineStep{ID: "run", Task: "consume", DependsOn: []string{"setup"}}, consumeBody,
			[]string{artifact, p.consumed}, []string{p.producer.Key()}, true)
		p.finalizer = node(extension.PipelineStep{
			ID:        "teardown",
			Task:      "teardown",
			RunOn:     extensionproto.StepRunOnFinally,
			Finalizes: &extensionproto.FinalizesRelation{Producer: "setup", Consumers: []string{"run"}},
		}, fixtureScript{{"append", "x", p.teardowns}, {"remove", p.resource}}, nil, nil, false)
		f.planned = append(f.planned, p.producer, p.consumer, p.finalizer)
		f.projects[name] = p
	}
	f.ws = workspace.NewWorkspace(root, nil, projects)
	f.ws.Name = "test-ws"
	planner.ResolveInvocationRelations(f.planned)
	return f
}

// cacheConsumers records a green entry at the key each named project's
// consumer computes over the tree as it stands, as an earlier run left it.
func (f *lazyFixture) cacheConsumers(t *testing.T, cm *store.CacheManager, names ...string) {
	t.Helper()
	keys, err := cachekey.PrecomputeKeys(f.ws, f.planned, nil, nil, cm, cachekey.CacheBypass{})
	if err != nil {
		t.Fatalf("PrecomputeKeys: %v", err)
	}
	for _, name := range names {
		consumer := f.projects[name].consumer
		hash := keys[consumer.Key()]
		if hash == "" {
			t.Fatalf("the consumer of %s has no key", name)
		}
		if _, err := cm.IngestTaskEntry(t.TempDir(), store.TaskEntrySpec{
			Key:      hash,
			Result:   &store.EntryResult{Status: "success"},
			Metadata: &store.EntryMetadata{Extension: consumer.Extension.Name, Task: "test~run", Project: name},
		}); err != nil {
			t.Fatalf("publish the entry of %s: %v", name, err)
		}
	}
}

func (f *lazyFixture) run(t *testing.T, cm *store.CacheManager, maxParallel int, params extension.ParamMap) *SchedulerResult {
	t.Helper()
	return f.runWith(context.Background(), cm, SchedulerConfig{MaxParallel: maxParallel}, params, nil)
}

// runWith runs the plan with cfg, after prepare sets what a case needs on the
// scheduler; the fixture's one construction site.
func (f *lazyFixture) runWith(ctx context.Context, cm *store.CacheManager, cfg SchedulerConfig, params extension.ParamMap, prepare func(*Scheduler)) *SchedulerResult {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	s := newScheduler(f.ws, f.planned, params, cfg, &mockRenderer{}, cm)
	if prepare != nil {
		prepare(s)
	}
	return s.Run(ctx)
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "x")
}

// assertNotStarted fails when the project's setup or teardown started a
// process, and checks the rows they left.
func assertNotStarted(t *testing.T, result *SchedulerResult, p *lazyProject) {
	t.Helper()
	producer := result.Results[p.producer.Key()]
	if producer == nil || producer.Status != "skipped" || producer.Execution != nil {
		t.Fatalf("setup %s = %+v, want skipped with no process", p.producer.Key(), producer)
	}
	if producer.Error == nil || producer.Error.Message != notNeededMessage {
		t.Fatalf("setup %s diagnostic = %+v, want %q", p.producer.Key(), producer.Error, notNeededMessage)
	}
	finalizer := result.Results[p.finalizer.Key()]
	if finalizer == nil || finalizer.Status != "skipped" || finalizer.Execution != nil {
		t.Fatalf("teardown %s = %+v, want skipped with no process", p.finalizer.Key(), finalizer)
	}
	if got := countLines(t, p.teardowns); got != 0 {
		t.Fatalf("teardown %s ran %d times, want never", p.finalizer.Key(), got)
	}
	if _, err := os.Stat(p.resource); !os.IsNotExist(err) {
		t.Fatalf("setup %s provisioned its resource", p.producer.Key())
	}
}

// assertStartedOnce fails unless the project's setup ran once, its consumer
// executed against the resource after it, and its teardown ran once.
func assertStartedOnce(t *testing.T, result *SchedulerResult, p *lazyProject) {
	t.Helper()
	producer := result.Results[p.producer.Key()]
	if producer == nil || producer.Status != "success" || producer.Execution == nil {
		t.Fatalf("setup %s = %+v, want one execution", p.producer.Key(), producer)
	}
	consumer := result.Results[p.consumer.Key()]
	if consumer == nil || consumer.Status != "success" || consumer.ReuseKind().CacheHit() {
		t.Fatalf("consumer %s = %+v, want an execution", p.consumer.Key(), consumer)
	}
	// The consumer exits 3 without the credential the setup wrote, so a
	// success here is an execution after the setup.
	if data, err := os.ReadFile(p.consumed); err != nil || string(data) != "ok" {
		t.Fatalf("consumer %s did not run against its environment: %q, %v", p.consumer.Key(), data, err)
	}
	finalizer := result.Results[p.finalizer.Key()]
	if finalizer == nil || finalizer.Status != "success" {
		t.Fatalf("teardown %s = %+v, want success", p.finalizer.Key(), finalizer)
	}
	if got := countLines(t, p.teardowns); got != 1 {
		t.Fatalf("teardown %s ran %d times, want exactly once", p.finalizer.Key(), got)
	}
}

// An unchanged run, whose consumers are all served from the cache, starts no
// setup and no teardown: each setup is skipped as not needed, and its
// teardown, never armed, is skipped too.
func TestLazySetup_AnAllHitFrontierStartsNoSetupAndNoFinalizer(t *testing.T) {
	spectest.Proves(t, lazySetupFeature, lazySetupRequirement, "an-all-hit-frontier-starts-no-setup-and-no-finalizer")
	t.Parallel()
	f := newLazyFixture(t, "a", "b", "c")
	cm := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
	f.cacheConsumers(t, cm, "a", "b", "c")

	result := f.run(t, cm, 2, nil)
	if !result.Success {
		t.Fatalf("run failed: %+v", result.Results)
	}
	for _, name := range []string{"a", "b", "c"} {
		p := f.projects[name]
		if consumer := result.Results[p.consumer.Key()]; consumer == nil || !consumer.ReuseKind().CacheHit() {
			t.Fatalf("consumer %s = %+v, want a cache hit", name, consumer)
		}
		assertNotStarted(t, result, p)
	}
	if entries, err := os.ReadDir(store.InvocationsRoot(f.ws.Root)); err == nil && len(entries) != 0 {
		t.Fatalf("an all-hit run created invocation scratch: %v", entries)
	}
}

// One consumer that misses the cache starts exactly its own setup and
// teardown, once; the projects whose consumers hit start none.
func TestLazySetup_OneMissingConsumerStartsExactlyItsSetupAndFinalizer(t *testing.T) {
	spectest.Proves(t, lazySetupFeature, lazySetupRequirement, "one-missing-consumer-starts-exactly-its-setup-and-finalizer")
	t.Parallel()
	f := newLazyFixture(t, "a", "b", "c")
	cm := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
	f.cacheConsumers(t, cm, "a", "c")

	result := f.run(t, cm, 2, nil)
	if !result.Success {
		t.Fatalf("run failed: %+v", result.Results)
	}
	assertStartedOnce(t, result, f.projects["b"])
	assertNotStarted(t, result, f.projects["a"])
	assertNotStarted(t, result, f.projects["c"])
}

// A consumer that misses runs after its setup even on a single worker: the
// deferred consumer gives its worker back, so its setup can run, and nothing
// waits for a worker it holds.
func TestLazySetup_AConsumerThatMissesRunsAfterItsSetupOnOneWorker(t *testing.T) {
	spectest.Proves(t, lazySetupFeature, lazySetupRequirement, "a-consumer-that-misses-runs-after-its-setup")
	t.Parallel()
	f := newLazyFixture(t, "a", "b")
	cm := store.NewCacheManager(store.NewLocalStore(t.TempDir()))

	result := f.run(t, cm, 1, nil)
	if !result.Success {
		t.Fatalf("run failed: %+v", result.Results)
	}
	assertStartedOnce(t, result, f.projects["a"])
	assertStartedOnce(t, result, f.projects["b"])
}

// A run with no cache at all still provisions for each consumer, which then
// executes against it.
func TestLazySetup_ARunWithoutACacheStartsEverySetup(t *testing.T) {
	spectest.Proves(t, lazySetupFeature, lazySetupRequirement, "a-consumer-that-misses-runs-after-its-setup")
	t.Parallel()
	f := newLazyFixture(t, "a")
	result := f.run(t, nil, 2, nil)
	if !result.Success {
		for k, r := range result.Results {
			t.Logf("%s %s %+v", k, r.Status, r.Error)
		}
		t.Fatalf("run failed: %+v", result.Results)
	}
	assertStartedOnce(t, result, f.projects["a"])
}

// A setup that fails blocks the consumer that deferred to it with the setup's
// diagnostic, and its teardown still runs once, since it started.
func TestLazySetup_AFailedSetupBlocksTheDeferredConsumer(t *testing.T) {
	spectest.Proves(t, lazySetupFeature, lazySetupRequirement, "a-consumer-that-misses-runs-after-its-setup")
	t.Parallel()
	f := newLazyFixture(t, "a")
	p := f.projects["a"]
	fixtureTask(t, p.producer.JobDef, fixtureScript{{"exit", "7"}})
	task := p.producer.Extension.Tasks["provision"]
	task.Command = p.producer.JobDef.Command
	p.producer.Extension.Tasks["provision"] = task
	cm := store.NewCacheManager(store.NewLocalStore(t.TempDir()))

	result := f.run(t, cm, 2, nil)
	if producer := result.Results[p.producer.Key()]; producer == nil || producer.Status != "failed" {
		t.Fatalf("setup = %+v, want failed", producer)
	}
	consumer := result.Results[p.consumer.Key()]
	if consumer == nil || consumer.Status != "skipped" || consumer.Error == nil ||
		consumer.Error.Code != extensionproto.FailureSensitiveSetupFailed {
		t.Fatalf("consumer = %+v, want skipped with %s", consumer, extensionproto.FailureSensitiveSetupFailed)
	}
	if _, err := os.Stat(p.consumed); !os.IsNotExist(err) {
		t.Fatal("the consumer executed although its setup failed")
	}
	if got := countLines(t, p.teardowns); got != 1 {
		t.Fatalf("teardown ran %d times after a started setup, want exactly once", got)
	}
}

// A false pruneIf keeps the setup eager: an imperative request such as
// --infra-down provisions and tears down although every consumer hits. A
// valid true one keeps it lazy.
func TestLazySetup_AFalsePruneIfKeepsTheSetupEager(t *testing.T) {
	spectest.Proves(t, lazySetupFeature, lazySetupRequirement, "an-all-hit-frontier-starts-no-setup-and-no-finalizer")
	for _, infraDown := range []bool{false, true} {
		t.Run(map[bool]string{false: "lazy", true: "infra-down"}[infraDown], func(t *testing.T) {
			t.Parallel()
			f := newLazyFixture(t, "a")
			p := f.projects["a"]
			p.finalizer.Step.Finalizes.PruneIf = "!params.infra-down"
			params := extension.ParamMap{"infra-down": infraDown}
			cm := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
			keys, err := cachekey.PrecomputeKeys(f.ws, f.planned, params, nil, cm, cachekey.CacheBypass{})
			if err != nil {
				t.Fatalf("PrecomputeKeys: %v", err)
			}
			if _, err := cm.IngestTaskEntry(t.TempDir(), store.TaskEntrySpec{
				Key:      keys[p.consumer.Key()],
				Result:   &store.EntryResult{Status: "success"},
				Metadata: &store.EntryMetadata{Extension: p.consumer.Extension.Name, Task: "test~run", Project: "a"},
			}); err != nil {
				t.Fatal(err)
			}
			result := f.run(t, cm, 2, params)
			if consumer := result.Results[p.consumer.Key()]; consumer == nil || !consumer.ReuseKind().CacheHit() {
				t.Fatalf("consumer = %+v, want a cache hit", consumer)
			}
			if !infraDown {
				assertNotStarted(t, result, p)
				return
			}
			if producer := result.Results[p.producer.Key()]; producer == nil || producer.Status != "success" || producer.Execution == nil {
				t.Fatalf("setup = %+v, want it to run for --infra-down", producer)
			}
			if got := countLines(t, p.teardowns); got != 1 {
				t.Fatalf("teardown ran %d times, want exactly once", got)
			}
		})
	}
}

// A job outside the frontier that waits for the setup keeps it eager: it
// would otherwise wait for a setup no consumer requests.
func TestLazySetup_AJobOutsideTheFrontierKeepsTheSetupEager(t *testing.T) {
	spectest.Proves(t, lazySetupFeature, lazySetupRequirement, "an-all-hit-frontier-starts-no-setup-and-no-finalizer")
	f := newLazyFixture(t, "a")
	p := f.projects["a"]
	outsider := *p.finalizer
	outsider.Step = &extension.PipelineStep{ID: "report", Task: "teardown", DependsOn: []string{"setup"}}
	outsider.JobDef = &extension.JobDefinition{Name: extension.StepJobName("test", "report"), CommandName: "test", StepID: "report"}
	outsider.DependsOn = []string{p.producer.Key()}
	planned := append(append([]*ScheduledJob(nil), f.planned...), &outsider)
	rt := newInvocationRuntime(f.ws, planned)
	rt.decideLazy(planned, func(*ScheduledJob) extension.ParamMap { return nil })
	if rt.relations[0].lazy {
		t.Fatal("a setup another job waits for was made lazy")
	}
	rt = newInvocationRuntime(f.ws, f.planned)
	rt.decideLazy(f.planned, func(*ScheduledJob) extension.ParamMap { return nil })
	if !rt.relations[0].lazy {
		t.Fatal("a setup only its consumers wait for was kept eager")
	}
}

// pruneIf reads the params the producer receives and its command's flag
// defaults: a declared --infra-down that the command line leaves out is false,
// so a plain run is lazy, while a param no layer sets keeps the setup eager.
func TestLazySetup_PruneIfReadsTheProducersDefaults(t *testing.T) {
	spectest.Proves(t, lazySetupFeature, lazySetupRequirement, "an-all-hit-frontier-starts-no-setup-and-no-finalizer")
	for _, tc := range []struct {
		name     string
		declared bool
		pruneIf  string
	}{
		{"declared flag", true, "!params.infra-down"},
		{"declared flag read by its alias", true, "!params.infraDown"},
		{"undeclared param", false, "!params.infra-down"},
	} {
		declared := tc.declared
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLazyFixture(t, "a")
			p := f.projects["a"]
			p.finalizer.Step.Finalizes.PruneIf = tc.pruneIf
			if declared {
				// The flag is the command's: a pipeline step's own job
				// definition carries none.
				p.producer.Extension.Jobs = map[string]*extension.JobDefinition{"test": {
					Name: "test",
					Flags: map[string]extension.FlagDefinition{
						"infra-down": {Type: "boolean", Default: false},
					},
				}}
			}
			cm := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
			f.cacheConsumers(t, cm, "a")
			result := f.run(t, cm, 2, nil)
			if consumer := result.Results[p.consumer.Key()]; consumer == nil || !consumer.ReuseKind().CacheHit() {
				t.Fatalf("consumer = %+v, want a cache hit", consumer)
			}
			if declared {
				assertNotStarted(t, result, p)
				return
			}
			if producer := result.Results[p.producer.Key()]; producer == nil || producer.Status != "success" || producer.Execution == nil {
				t.Fatalf("setup = %+v, want an eager setup for an invalid pruneIf", producer)
			}
		})
	}
}

// A value the command line sets wins over the command's flag default, under
// either spelling pruneIf reads: --infra-down keeps the setup eager although
// the flag defaults to false and every consumer hits.
func TestLazySetup_ACommandLineValueWinsOverTheFlagDefault(t *testing.T) {
	spectest.Proves(t, lazySetupFeature, lazySetupRequirement, "an-all-hit-frontier-starts-no-setup-and-no-finalizer")
	for _, pruneIf := range []string{"!params.infra-down", "!params.infraDown"} {
		t.Run(pruneIf, func(t *testing.T) {
			t.Parallel()
			f := newLazyFixture(t, "a")
			p := f.projects["a"]
			p.finalizer.Step.Finalizes.PruneIf = pruneIf
			p.producer.Extension.Jobs = map[string]*extension.JobDefinition{"test": {
				Name:  "test",
				Flags: map[string]extension.FlagDefinition{"infra-down": {Type: "boolean", Default: false}},
			}}
			params := extension.ParamMap{"infra-down": true}
			cm := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
			keys, err := cachekey.PrecomputeKeys(f.ws, f.planned, params, nil, cm, cachekey.CacheBypass{})
			if err != nil {
				t.Fatalf("PrecomputeKeys: %v", err)
			}
			if _, err := cm.IngestTaskEntry(t.TempDir(), store.TaskEntrySpec{
				Key:      keys[p.consumer.Key()],
				Result:   &store.EntryResult{Status: "success"},
				Metadata: &store.EntryMetadata{Extension: p.consumer.Extension.Name, Task: "test~run", Project: "a"},
			}); err != nil {
				t.Fatal(err)
			}
			result := f.run(t, cm, 2, params)
			if consumer := result.Results[p.consumer.Key()]; consumer == nil || !consumer.ReuseKind().CacheHit() {
				t.Fatalf("consumer = %+v, want a cache hit", consumer)
			}
			if producer := result.Results[p.producer.Key()]; producer == nil || producer.Status != "success" || producer.Execution == nil {
				t.Fatalf("setup = %+v, want it to run for --infra-down", producer)
			}
			if got := countLines(t, p.teardowns); got != 1 {
				t.Fatalf("teardown ran %d times, want exactly once", got)
			}
		})
	}
}
