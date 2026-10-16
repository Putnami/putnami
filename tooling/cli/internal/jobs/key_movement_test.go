package jobs

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	cache "go.putnami.dev/protocol/cache"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/jobs/remotecache"
	"go.putnami.dev/tooling/cli/internal/store"
)

// layersProducer declares its project's .gen/layers tree, the shape of a task
// that produces image layers.
func layersProducer() *ScheduledJob {
	return declaredJob("package~layers", "layers", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"layers": dirDeclaration(extension.OutputRootProject, ".gen/layers", false),
		},
	})
}

// layersCheck keys on the producer's layers and records a verdict only, the
// shape of a check that verifies them.
func layersCheck() *ScheduledJob {
	job := declaredJob("package~layers-check", "layers-check", &extension.TaskDeclaration{})
	job.JobDef.FilePatterns = []string{".gen/layers/**"}
	job.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{NoOutput: true}
	return job
}

func layerPath(f *captureFixture) string {
	return filepath.Join(f.ws.Root, captureTestProject, ".gen", "layers", "base.tar")
}

// keyOf computes job's key through cm, from no published dependency key.
func keyOf(t *testing.T, f *captureFixture, job *ScheduledJob, cm *store.CacheManager) string {
	t.Helper()
	hash, err := cachekey.ComputeJobCacheHash(f.ws, job, nil, nil, cm, nil)
	if err != nil || hash == "" {
		t.Fatalf("compute the key of %s: %q (err=%v)", job.Key(), hash, err)
	}
	return hash
}

// An executed task's declared outputs move the key of a later job that reads
// them, whatever the task's verdict: the key that job computes when it starts
// is the key of the bytes it reads, not the memoized key of the bytes before.
func TestAnExecutedOutputMovesTheKeyOfAJobThatReadsIt(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "session-written-input-keys",
		"an-earlier-task-output-moves-the-key-of-its-reader")
	for _, status := range []string{"success", "failed"} {
		t.Run(status, func(t *testing.T) {
			producer, check := layersProducer(), layersCheck()
			f := newCaptureFixture(t, producer, check)
			writeFileAt(t, layerPath(f), "old layer\n")
			stale := keyOf(t, f, check, f.cache)

			writeFileAt(t, layerPath(f), "new layer\n")
			if memoized := keyOf(t, f, check, f.cache); memoized != stale {
				t.Fatal("the memo did not hold the key of the old layer, so this test cannot prove anything")
			}

			var mu sync.Mutex
			f.sched.finalizeExecutedJob(t.Context(), producer, &JobResult{Status: status},
				false, "", "", &mu, map[string]string{})

			if got, want := keyOf(t, f, check, f.cache), keyOf(t, f, check, store.NewCacheManager(nil)); got != want {
				t.Fatalf("after the producer ran, the check keys %q, want %q: the key of the layers it reads", got, want)
			}
		})
	}
}

// A restore rewrites the outputs it records, so it moves the key of a later job
// that reads them exactly as an execution does.
func TestARestoredOutputMovesTheKeyOfAJobThatReadsIt(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "session-written-input-keys",
		"an-earlier-task-output-moves-the-key-of-its-reader")
	producer, check := layersProducer(), layersCheck()
	f := newCaptureFixture(t, producer, check)
	writeFileAt(t, layerPath(f), "new layer\n")
	if !f.sched.storeDeclaredCapture(producer, capturedResult(nil), captureHashA) {
		t.Fatal("declared capture published no entry")
	}
	// This checkout still holds the old layers when the run keys the check.
	writeFileAt(t, layerPath(f), "old layer\n")
	stale := keyOf(t, f, check, f.cache)

	f.hitOrFail(t, producer, captureHashA)
	if got := readFileAt(t, layerPath(f)); got != "new layer\n" {
		t.Fatalf("the restore left %q, so this test cannot prove anything", got)
	}

	got, want := keyOf(t, f, check, f.cache), keyOf(t, f, check, store.NewCacheManager(nil))
	if got != want || got == stale {
		t.Fatalf("after the restore, the check keys %q, want %q: the key of the restored layers", got, want)
	}
}

// A key moves when another planned job may write a file it reads, or when it
// folds the key of a job whose key moves. A job no write reaches keeps its key.
// A port-reported output is unknown before its task runs, so its whole root
// counts as written.
func TestPlanKeyMovementFollowsWritersAndFoldedKeys(t *testing.T) {
	t.Run("a declared path", func(t *testing.T) {
		producer, check := layersProducer(), layersCheck()
		summary := declaredJob("package~summary", "summary", &extension.TaskDeclaration{})
		summary.JobDef.FilePatterns = []string{"summary.json"}
		summary.DependsOn = []string{check.Key()}
		lint := declaredJob("lint~lint", "lint", &extension.TaskDeclaration{})
		lint.JobDef.FilePatterns = []string{"src/**"}
		planned := []*ScheduledJob{producer, check, summary, lint}
		f := newCaptureFixture(t, planned...)

		movement := cachekey.PlanKeyMovement(cachekey.KeyInputs{Workspace: f.ws}, planned)
		want := map[string]bool{check.Key(): true, summary.Key(): true}
		if !reflect.DeepEqual(movement.Moving, want) {
			t.Fatalf("moving = %v, want %v", movement.Moving, want)
		}
		if got := movement.Writers[check.Key()]; !reflect.DeepEqual(got, []string{producer.Key()}) {
			t.Fatalf("writers of the check = %v, want the producer alone", got)
		}
	})

	t.Run("a source rewriter", func(t *testing.T) {
		sources := extension.ResourceRef{ID: extension.ResourceIDSources, Scope: extension.ResourceScopeProject}
		fixer := declaredJob("lint~fix", "fix", &extension.TaskDeclaration{MutatesSources: true}, sources)
		fixer.JobDef.FilePatterns = []string{"src/**"}
		compile := declaredJob("build~compile", "compile", &extension.TaskDeclaration{})
		compile.JobDef.FilePatterns = []string{"src/**/*.ts"}
		docs := declaredJob("build~docs", "docs", &extension.TaskDeclaration{})
		docs.JobDef.FilePatterns = []string{"doc/**"}
		planned := []*ScheduledJob{fixer, compile, docs}
		f := newCaptureFixture(t, planned...)

		movement := cachekey.PlanKeyMovement(cachekey.KeyInputs{Workspace: f.ws}, planned)
		if !movement.Moving[compile.Key()] {
			t.Fatal("a job that reads the sources a rewriter rewrites keeps its precomputed key")
		}
		if movement.Moving[docs.Key()] {
			t.Fatal("a job that reads none of the rewriter's sources was moved by it")
		}
	})

	t.Run("a port-reported path", func(t *testing.T) {
		client := declaredJob("build~client", "client", &extension.TaskDeclaration{
			Outputs: map[string]extension.DeclaredOutput{
				"client": {Kind: extension.OutputKindDirectory, PathFrom: "clientOutput", OptionalEmpty: true},
			},
		})
		lint := declaredJob("lint~lint", "lint", &extension.TaskDeclaration{})
		lint.JobDef.FilePatterns = []string{"src/**"}
		planned := []*ScheduledJob{client, lint}
		f := newCaptureFixture(t, planned...)

		if movement := cachekey.PlanKeyMovement(cachekey.KeyInputs{Workspace: f.ws}, planned); !movement.Moving[lint.Key()] {
			t.Fatal("a job that reads the project was not moved by an output whose path is unknown before its task runs")
		}
	})

	t.Run("a bounded port-reported path", func(t *testing.T) {
		spectest.Proves(t, "cli/job-planning-execution", "session-written-input-keys",
			"a-bounded-port-output-moves-only-the-keys-that-read-its-bound")
		merge := declaredJob("build~merge", "merge", &extension.TaskDeclaration{
			Outputs: map[string]extension.DeclaredOutput{
				"merged": {Kind: extension.OutputKindFile, PathFrom: "mergedConfig", Within: ".gen/conf", OptionalEmpty: true},
			},
		})
		lint := declaredJob("lint~lint", "lint", &extension.TaskDeclaration{})
		lint.JobDef.FilePatterns = []string{"src/**"}
		serve := declaredJob("build~serve", "serve", &extension.TaskDeclaration{})
		serve.JobDef.FilePatterns = []string{".gen/conf/**"}
		planned := []*ScheduledJob{merge, lint, serve}
		f := newCaptureFixture(t, planned...)

		movement := cachekey.PlanKeyMovement(cachekey.KeyInputs{Workspace: f.ws}, planned)
		if movement.Moving[lint.Key()] {
			t.Fatal("a job that reads nothing under the bound was moved by an output bounded to .gen/conf")
		}
		if !movement.Moving[serve.Key()] {
			t.Fatal("a job that reads the bound was not moved by an output whose path lies under it")
		}
	})
}

// A write moves a key only when it can reach a file the key selects: a tree
// the key's exclusions cover, a file its patterns do not match, and a path
// beside every file its Go embed directives name leave it unmoved, while a
// write that can change those directives makes every write in the key's
// region count again.
func TestPlanKeyMovementCountsOnlyWritesThatReachASelectedFile(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "session-written-input-keys",
		"a-write-that-reaches-no-selected-file-leaves-a-key-unmoved")
	sources := []string{"**/*.go", "!**/*_test.go", "go-embed:build", "!**/.gen/**"}
	compile := func() *ScheduledJob {
		job := declaredJob("build~compile", "compile", &extension.TaskDeclaration{})
		job.JobDef.FilePatterns = sources
		return job
	}
	writer := func(name string, output extension.DeclaredOutput) *ScheduledJob {
		return declaredJob(name, name, &extension.TaskDeclaration{
			Outputs: map[string]extension.DeclaredOutput{"out": output},
		})
	}
	key := func(name string) string { return "/" + captureTestProject + ":" + name }
	writeSources := func(t *testing.T, f *captureFixture) {
		project := filepath.Join(f.ws.Root, captureTestProject)
		writeFileAt(t, filepath.Join(project, "go.mod"), "module example.com/proj\n")
		writeFileAt(t, filepath.Join(project, "version.go"),
			"package proj\nimport _ \"embed\"\n//go:embed .gen/version.json\nvar version []byte\n")
		writeFileAt(t, filepath.Join(project, ".gen", "version.json"), "{}")
	}

	t.Run("an excluded tree and an unselected file", func(t *testing.T) {
		generate := writer("build~generate", dirDeclaration(extension.OutputRootProject, ".gen/schema", false))
		infra := writer("build~infra", fileDeclaration(extension.OutputRootProject, "infra/runtime.json", true))
		check := declaredJob("build~check", "check", &extension.TaskDeclaration{})
		check.JobDef.FilePatterns = []string{".gen/schema/**"}
		planned := []*ScheduledJob{generate, infra, compile(), check}
		f := newCaptureFixture(t, planned...)
		writeSources(t, f)

		movement := cachekey.PlanKeyMovement(cachekey.KeyInputs{Workspace: f.ws}, planned)
		if movement.Moving[key("build~compile")] {
			t.Fatalf("compile moved by writes none of its files can lie under: writers %v", movement.Writers[key("build~compile")])
		}
		if got := movement.Writers[key("build~check")]; !reflect.DeepEqual(got, []string{key("build~generate")}) {
			t.Fatalf("writers of a reader of the generated tree = %v, want the generator alone", got)
		}
	})

	t.Run("an embedded file", func(t *testing.T) {
		stamp := writer("build~stamp", fileDeclaration(extension.OutputRootProject, ".gen/version.json", false))
		conf := writer("build~conf", dirDeclaration(extension.OutputRootProject, ".gen/conf", true))
		planned := []*ScheduledJob{stamp, conf, compile()}
		f := newCaptureFixture(t, planned...)
		writeSources(t, f)

		movement := cachekey.PlanKeyMovement(cachekey.KeyInputs{Workspace: f.ws}, planned)
		if got := movement.Writers[key("build~compile")]; !reflect.DeepEqual(got, []string{key("build~stamp")}) {
			t.Fatalf("writers of compile = %v, want the writer of the file it embeds alone", got)
		}
	})

	t.Run("a write that can change a directive", func(t *testing.T) {
		conf := writer("build~conf", dirDeclaration(extension.OutputRootProject, ".gen/conf", true))
		client := writer("build~client", dirDeclaration(extension.OutputRootProject, "client", true))
		planned := []*ScheduledJob{conf, client, compile()}
		f := newCaptureFixture(t, planned...)
		writeSources(t, f)

		movement := cachekey.PlanKeyMovement(cachekey.KeyInputs{Workspace: f.ws}, planned)
		if got := movement.Writers[key("build~compile")]; !reflect.DeepEqual(got, []string{key("build~client"), key("build~conf")}) {
			t.Fatalf("writers of compile = %v, want both: a source the client writes may embed the conf tree", got)
		}
	})
}

// providerKeysAsked returns every key the provider was asked about in op.
func providerKeysAsked(server *fakeCacheServer, op cache.ProviderOp) []string {
	var keys []string
	for _, line := range server.requestLines() {
		request, _ := cache.ParseAndValidateProviderRequest([]byte(line))
		if request == nil || request.Op != op {
			continue
		}
		switch op {
		case cache.OpRestore:
			if params, _ := cache.ParseAndValidateRestoreParams(request.Payload); params != nil {
				keys = append(keys, params.Key)
			}
		case cache.OpUpload:
			if params, _ := cache.ParseAndValidateUploadParams(request.Payload); params != nil {
				keys = append(keys, params.Key)
			}
		}
	}
	return keys
}

// A remote negotiation precomputes the check's key over the layers the
// checkout holds before the producer rewrites them. That key is never recorded
// as the check's key, never looked up and never uploaded: the check is looked
// up, and its verdict shared, at the key of the layers it reads.
func TestAPrecomputedKeyAnEarlierWriteMovesIsNeverLookedUpNorUploaded(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "session-written-input-keys",
		"a-precomputed-key-a-write-may-move-is-never-looked-up-or-shared")
	server := newFakeCacheServer()
	enableProviderRemoteCache(t)
	useSharedFakeProvider(t, server)

	producer, check := layersProducer(), layersCheck()
	planned := []*ScheduledJob{producer, check}
	f := newCaptureFixture(t, planned...)
	remote, notice := remotecache.LoadRemoteCache(context.Background(), f.ws.Root,
		[]*extension.ExtensionDescription{fakeProviderExtension(t, nil)}, nil, store.CacheTrustAny, fakeProviderLaunch)
	if remote == nil || !remote.IsProviderBacked() {
		t.Fatalf("expected a provider-backed remote cache, got %+v (notice %q)", remote, notice)
	}
	t.Cleanup(remote.Close)
	f.sched.setRemoteCache(remote)

	writeFileAt(t, layerPath(f), "old layer\n")
	stale := keyOf(t, f, check, store.NewCacheManager(nil))

	remote.Negotiate(t.Context(), f.ws, planned, nil, nil, f.cache, cachekey.CacheBypass{})
	if recorded, ok := remote.Keys[check.Key()]; ok {
		t.Fatalf("the negotiation recorded %q as the check's key, though the producer may rewrite its input", recorded)
	}
	if remote.Keys[producer.Key()] == "" {
		t.Fatal("the negotiation dropped the key of a job no planned write can move")
	}

	writeFileAt(t, layerPath(f), "new layer\n")
	var mu sync.Mutex
	f.sched.finalizeExecutedJob(t.Context(), producer, capturedResult(nil), false, "", "", &mu, map[string]string{})
	fresh := keyOf(t, f, check, store.NewCacheManager(nil))

	hash, served := f.declaredLookup(t.Context(), check)
	if hash != fresh {
		t.Fatalf("the check was looked up at %q, want %q: the key of the layers it reads", hash, fresh)
	}
	if served != nil {
		t.Fatalf("the check was served %+v with no entry at its key", served)
	}
	f.sched.finalizeExecutedJob(t.Context(), check, capturedResult(nil), true, hash, "", &mu, map[string]string{})
	remote.DrainUploads()

	for _, op := range []cache.ProviderOp{cache.OpRestore, cache.OpUpload} {
		for _, key := range providerKeysAsked(server, op) {
			if key == store.RemoteTaskEntryKey(stale) {
				t.Errorf("the provider received a %s at the precomputed key of the old layers", op)
			}
		}
	}
	if !server.has(store.RemoteTaskEntryKey(fresh)) {
		t.Fatal("the check's verdict was not shared at the key of the layers it read")
	}
}

// ---------------------------------------------------------------------------
// invocation pruning on a consumer key a planned write may move
// ---------------------------------------------------------------------------

// layersOf is the file the layers writer rewrites and the consumer keys on.
func (f *invocationFixture) layersOf() string {
	return filepath.Join(f.ws.Root, "layers", "base.tar")
}

// cacheConsumerOfLayers makes the consumer cacheable and keyed on the project's
// layers tree.
func (f *invocationFixture) cacheConsumerOfLayers() {
	f.consumer.Step.Task = "consume-cached"
	f.consumer.JobDef.Cache = true
	f.consumer.JobDef.FilePatterns = []string{"layers/**"}
	f.consumer.Extension.Tasks["consume-cached"] = extension.TaskDefinition{
		Kind:    "command",
		Command: f.consumer.JobDef.Command,
		Declares: &extensionproto.TaskDeclaration{
			Outputs: map[string]extensionproto.DeclaredOutput{
				"report": {Kind: extensionproto.OutputKindFile, Path: "report.txt", OptionalEmpty: true},
			},
		},
	}
}

// planLayersWriter plans an uncached task that writes content into the
// project's layers tree, which it declares, after the jobs named in after.
func (f *invocationFixture) planLayersWriter(t *testing.T, content string, after ...string) *ScheduledJob {
	t.Helper()
	ext := f.producer.Extension
	step := extension.PipelineStep{ID: "layers", Task: "layers"}
	writer := &ScheduledJob{
		Project:   f.producer.Project,
		Extension: ext,
		Step:      &step,
		JobDef: &extension.JobDefinition{
			Name:          extension.StepJobName("test", step.ID),
			ExtensionName: ext.Name,
			CommandName:   "test",
			StepID:        step.ID,
			TimeoutMs:     unboundedJobTimeoutMs,
		},
		DependsOn: after,
	}
	fixtureTask(t, writer.JobDef, fixtureScript{{"write", content, f.layersOf()}})
	ext.Tasks[step.Task] = extension.TaskDefinition{
		Kind:    "command",
		Command: writer.JobDef.Command,
		Declares: &extensionproto.TaskDeclaration{
			Outputs: map[string]extensionproto.DeclaredOutput{
				"layers": {Kind: extensionproto.OutputKindDirectory, Path: "layers"},
			},
		},
	}
	f.planned = append([]*ScheduledJob{writer}, f.planned...)
	return writer
}

// publishConsumerEntry records the green verdict a previous run left at the
// consumer's key over the tree as it stands.
func (f *invocationFixture) publishConsumerEntry(t *testing.T, cm *store.CacheManager) {
	t.Helper()
	resolveInvocationRelations(f.planned)
	keys, err := cachekey.PrecomputeKeys(f.ws, f.planned, nil, nil, cm, cachekey.CacheBypass{})
	if err != nil {
		t.Fatalf("PrecomputeKeys: %v", err)
	}
	hash := keys[f.consumer.Key()]
	if hash == "" {
		t.Fatal("the consumer must be cacheable for this case to mean anything")
	}
	if _, err := cm.IngestTaskEntry(t.TempDir(), store.TaskEntrySpec{
		Key:      hash,
		Result:   &store.EntryResult{Status: "success"},
		Metadata: &store.EntryMetadata{Extension: "@acme/provider", Task: "test~run", Project: "proj"},
	}); err != nil {
		t.Fatalf("publish consumer entry: %v", err)
	}
}

// A relation pruned on a consumer key that a write before the producer may
// move is confirmed when the producer starts. A write that left the layers as
// they were keeps the pruning; a write that changed them withdraws it, so the
// producer provisions the resource the consumer then runs against, instead of
// the consumer being served a verdict about bytes it no longer reads.
func TestInvocation_PruningOnAMovableConsumerKeyIsConfirmedWhenTheProducerStarts(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "session-written-input-keys",
		"pruning-on-a-movable-consumer-key-is-confirmed-when-the-producer-starts")
	cases := []struct {
		name       string
		written    string
		wantPruned bool
	}{
		{name: "the write leaves the layers as they were", written: "old layer\n", wantPruned: true},
		{name: "the write changes the layers", written: "new layer\n", wantPruned: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fixture := newInvocationFixture(t, provisionBody, consumeBody)
			fixture.cacheConsumerOfLayers()
			writer := fixture.planLayersWriter(t, tc.written)
			fixture.producer.DependsOn = append(fixture.producer.DependsOn, writer.Key())
			writeFileAt(t, fixture.layersOf(), "old layer\n")
			cm := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
			fixture.publishConsumerEntry(t, cm)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			result := fixture.run(t, ctx, SchedulerConfig{MaxParallel: 2}, cm)

			producer := result.Results[fixture.producer.Key()]
			consumer := result.Results[fixture.consumer.Key()]
			if consumer == nil || consumer.Status != "success" {
				t.Fatalf("consumer result = %+v, want success", consumer)
			}
			if tc.wantPruned {
				if producer == nil || producer.Status != "skipped" {
					t.Fatalf("producer result = %+v, want pruned", producer)
				}
				if !consumer.CacheHit {
					t.Fatalf("consumer result = %+v, want the cached verdict it was pruned for", consumer)
				}
				if got := fixture.teardownCount(t); got != 0 {
					t.Fatalf("pruned finalizer ran %d times, want never", got)
				}
				return
			}
			if producer == nil || producer.Status != "success" {
				t.Fatalf("producer result = %+v, want it to provision for a consumer whose key moved", producer)
			}
			if consumer.CacheHit {
				t.Fatalf("consumer result = %+v, want an execution over the new layers", consumer)
			}
			if _, err := os.Stat(fixture.consumed); err != nil {
				t.Fatalf("the consumer did not run against the provisioned resource: %v", err)
			}
			if got := fixture.teardownCount(t); got != 1 {
				t.Fatalf("finalizer ran %d times, want exactly once", got)
			}
		})
	}
}

// The confirmation computes consumer keys under the run's lock
// (Scheduler.cachedAtCurrentKey), which the coordinator holds while it takes
// every relation's lock to find the finalizers to run (runReadyFinalizers). The
// confirmation therefore holds no relation's lock while it waits for the run's:
// here the coordinator takes the run's lock while the confirmation is under
// way, and both must finish.
func TestInvocation_PruningConfirmationHoldsNoRelationAcrossTheRunLock(t *testing.T) {
	fixture := newInvocationFixture(t, provisionBody, consumeBody)
	invocations := newInvocationRuntime(fixture.ws, fixture.planned)
	rel := invocations.byProducer[fixture.producer.Key()]
	if rel == nil {
		t.Fatal("the fixture's producer heads no relation")
	}
	rel.pruned, rel.recheck = true, true

	var mu sync.Mutex
	confirming, coordinating, coordinated := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var enter sync.Once
	go func() {
		<-confirming
		mu.Lock()
		close(coordinating)
		invocations.pendingFinalizers(map[string]*JobResult{}, true)
		mu.Unlock()
		close(coordinated)
	}()
	confirmed := make(chan bool, 1)
	go func() {
		confirmed <- invocations.isPruned(fixture.producer, func(*ScheduledJob) bool {
			enter.Do(func() { close(confirming) })
			<-coordinating
			// The consumer's key is computed under the run's lock, and it has no
			// entry.
			mu.Lock()
			defer mu.Unlock()
			return false
		})
	}()

	deadline := time.After(30 * time.Second)
	select {
	case pruned := <-confirmed:
		if pruned {
			t.Fatal("a consumer with no entry at its current key left the relation pruned")
		}
	case <-deadline:
		t.Fatal("the pruning confirmation and the finalizer pass wait for each other's lock")
	}
	select {
	case <-coordinated:
	case <-deadline:
		t.Fatal("the finalizer pass never finished")
	}
}

// A consumer key that a write may move after the producer starts cannot be
// confirmed in time, so the relation is not pruned: the producer provisions,
// and the consumer runs against the resource once the write moved its key.
func TestInvocation_ConsumerKeyAWriteMayMoveAfterTheProducerStartsIsNeverPruned(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "session-written-input-keys",
		"pruning-on-a-movable-consumer-key-is-confirmed-when-the-producer-starts")
	t.Parallel()
	fixture := newInvocationFixture(t, provisionBody, consumeBody)
	fixture.cacheConsumerOfLayers()
	writer := fixture.planLayersWriter(t, "new layer\n", fixture.producer.Key())
	fixture.consumer.DependsOn = append(fixture.consumer.DependsOn, writer.Key())
	writeFileAt(t, fixture.layersOf(), "old layer\n")
	cm := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
	fixture.publishConsumerEntry(t, cm)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := fixture.run(t, ctx, SchedulerConfig{MaxParallel: 2}, cm)

	if producer := result.Results[fixture.producer.Key()]; producer == nil || producer.Status != "success" {
		t.Fatalf("producer result = %+v, want it to provision", producer)
	}
	if consumer := result.Results[fixture.consumer.Key()]; consumer == nil || consumer.Status != "success" || consumer.CacheHit {
		t.Fatalf("consumer result = %+v, want an execution over the new layers", consumer)
	}
	if got := fixture.teardownCount(t); got != 1 {
		t.Fatalf("finalizer ran %d times, want exactly once", got)
	}
}

// The paths a bounded port output may have written, which a run drops the
// memoized digests under, are the reported file, the bound when the port
// reported nothing, and the whole project when it reported a path outside the
// bound.
func TestDeclaredOutputPathsOfABoundedPortOutput(t *testing.T) {
	job := declaredJob("build~merge", "merge", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"merged": {Kind: extension.OutputKindFile, PathFrom: "mergedConfig", Within: ".gen/conf", OptionalEmpty: true},
		},
	})
	f := newCaptureFixture(t, job)
	project := filepath.Join(f.ws.Root, captureTestProject)
	cases := []struct {
		name   string
		result *JobResult
		want   string
	}{
		{"a path inside the bound", portResult("mergedConfig", ".gen/conf/.env.local.yaml"), filepath.Join(project, ".gen", "conf", ".env.local.yaml")},
		{"no path", capturedResult(nil), filepath.Join(project, ".gen", "conf")},
		{"a path outside the bound", portResult("mergedConfig", "conf/.env.local.yaml"), project},
	}
	for _, tc := range cases {
		got := cachekey.DeclaredOutputPaths(f.sched.outputRoots(), job, tc.result)
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s: DeclaredOutputPaths = %v, want [%s]", tc.name, got, tc.want)
		}
	}
}
