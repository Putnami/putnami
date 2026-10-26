package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	putnamigit "go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

const sourceBoundFeature = "cli/job-planning-execution"
const sourceBoundRequirement = "source-bound-cache"

const sourceBoundSettleRequirement = "source-bound-settle"

// sourceBoundJob is a declared-capture task that declares cache.sourceBound,
// with one required project-rooted output directory.
func sourceBoundJob() *ScheduledJob {
	job := declaredJob("build~describe", "describe", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{"out": dirDeclaration(extension.OutputRootProject, "out", false)},
	})
	job.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{Deterministic: true, SourceBound: true}
	return job
}

// stubSourceBinding makes the scheduler's binding memo answer binding, or fail
// with err, and answer unmanaged when asked whether Git manages the root. It
// drops every value the memo computed before.
func stubSourceBinding(s *Scheduler, binding string, err error, unmanaged bool) {
	s.versionFilesMu.Lock()
	defer s.versionFilesMu.Unlock()
	memo := s.capabilitySourceBindings
	memo.invalidateAll()
	clear(memo.unmanaged)
	memo.resolve = func(string, string) (string, int, error) { return binding, 0, err }
	memo.isUnmanaged = func(string) bool { return unmanaged }
}

// storeSourceBoundEntry publishes the job's entry at entryKey, then leaves the
// output on disk different from what was captured, so a restore is
// observable.
func storeSourceBoundEntry(t *testing.T, f *captureFixture, job *ScheduledJob, entryKey string) string {
	t.Helper()
	output := filepath.Join(f.ws.Root, captureTestProject, "out", "evidence.json")
	writeFileAt(t, output, "captured\n")
	if !f.sched.storeDeclaredCapture(job, &JobResult{Status: "success"}, entryKey) {
		t.Fatal("declared capture did not publish an entry")
	}
	writeFileAt(t, output, "worktree\n")
	return output
}

// lookupSourceBound drives the real read side and returns the key it derived,
// its result and the keys it published for dependents.
func lookupSourceBound(t *testing.T, f *captureFixture, job *ScheduledJob) (string, *JobResult, map[string]string) {
	t.Helper()
	var mu sync.Mutex
	hashes := map[string]string{}
	hash, result, release := f.sched.lookupDeclaredEntry(t.Context(), job, nil, &mu, hashes)
	release()
	return hash, result, hashes
}

// A source-bound entry is found only at the binding it was stored at. Another
// binding misses before anything is written, and a hit publishes the cache key,
// not the entry key, for dependents.
func TestSourceBoundEntryIsFoundOnlyAtItsBinding(t *testing.T) {
	spectest.Proves(t, sourceBoundFeature, sourceBoundRequirement, "an-entry-is-found-only-at-its-binding")
	job := sourceBoundJob()
	f := newCaptureFixture(t, job)
	key := matrixKey(t, f, job)
	output := storeSourceBoundEntry(t, f, job, cachekey.SourceBoundEntryKey(key, "source-v1:sha256:before"))

	stubSourceBinding(f.sched, "source-v1:sha256:after", nil, false)
	if hash, result, hashes := lookupSourceBound(t, f, job); hash != key || result != nil {
		t.Fatalf("lookup at another binding = (%q, %+v), want (%q, a miss)", hash, result, key)
	} else if _, ok := hashes[job.Key()]; ok {
		t.Fatal("a miss published the task's key")
	}
	if got := readFileAt(t, output); got != "worktree\n" {
		t.Fatalf("a miss wrote the worktree: %q", got)
	}

	stubSourceBinding(f.sched, "source-v1:sha256:before", nil, false)
	_, result, hashes := lookupSourceBound(t, f, job)
	if result == nil || result.Reuse != ReuseLocalCache || hashes[job.Key()] != key {
		t.Fatalf("lookup at its binding = %+v (hashes %v), want a local hit publishing the cache key", result, hashes)
	}
	if got := readFileAt(t, output); got != "captured\n" {
		t.Fatalf("restored output = %q, want the captured bytes", got)
	}
}

// An entry stored at the plain cache key never serves a source-bound task,
// whatever the binding: nothing names the binding its outputs embed.
func TestSourceBoundEntryAtThePlainKeyNeverServes(t *testing.T) {
	spectest.Proves(t, sourceBoundFeature, sourceBoundRequirement, "an-entry-at-the-plain-key-never-serves")
	job := sourceBoundJob()
	f := newCaptureFixture(t, job)
	key := matrixKey(t, f, job)
	storeSourceBoundEntry(t, f, job, key)
	stubSourceBinding(f.sched, "source-v1:sha256:current", nil, false)
	if _, result, _ := lookupSourceBound(t, f, job); result != nil {
		t.Fatalf("inside a repository: an entry at the plain key was served: %+v", result)
	}
	stubSourceBinding(f.sched, "", errors.New("not a git repository"), true)
	if _, result, _ := lookupSourceBound(t, f, job); result != nil {
		t.Fatalf("unmanaged: an entry at the plain key was served: %+v", result)
	}
}

// Inside a repository, a binding the CLI cannot compute is not the empty
// binding of an unmanaged workspace. The lookup finds nothing, even an entry at
// the empty binding, and an execution whose stamp names no binding stores
// nothing while its dependents still receive its cache key.
func TestSourceBoundBindingErrorInsideARepositoryExecutesAndStoresNothing(t *testing.T) {
	spectest.Proves(t, sourceBoundFeature, sourceBoundRequirement, "a-binding-error-inside-a-repository-executes-and-stores-nothing")
	job := sourceBoundJob()
	f := newCaptureFixture(t, job)
	key := matrixKey(t, f, job)
	storeSourceBoundEntry(t, f, job, cachekey.SourceBoundEntryKey(key, ""))
	stubSourceBinding(f.sched, "", errors.New("git ls-files failed"), false)
	if _, result, _ := lookupSourceBound(t, f, job); result != nil {
		t.Fatalf("a hit was served while the binding failed inside a repository: %+v", result)
	}

	finalize := func(result *JobResult) *captureFixture {
		t.Helper()
		g := newCaptureFixture(t, job)
		writeFileAt(t, filepath.Join(g.ws.Root, captureTestProject, "out", "evidence.json"), "executed\n")
		var mu sync.Mutex
		hashes := map[string]string{}
		g.sched.finalizeExecutedJob(context.Background(), job, result, true, key, "", nil, &mu, hashes)
		if hashes[job.Key()] != key {
			t.Fatalf("dependents received %q, want the cache key %q", hashes[job.Key()], key)
		}
		return g
	}
	stored := func(g *captureFixture, at string) bool {
		t.Helper()
		entry, err := g.cache.LookupTaskEntry(at)
		return err == nil && entry != nil
	}

	unstamped := finalize(&JobResult{Status: "success"})
	if stored(unstamped, key) || stored(unstamped, cachekey.SourceBoundEntryKey(key, "")) {
		t.Fatal("an execution without a stamped binding stored an entry")
	}
	// The same execution stamped with a binding stores at that binding only,
	// so the refusal above is the missing stamp's and not the fixture's.
	const binding = "source-v1:sha256:stamped"
	bound := finalize(&JobResult{Status: "success", SourceBinding: binding, SourceBindingStamped: true})
	if !stored(bound, cachekey.SourceBoundEntryKey(key, binding)) || stored(bound, key) {
		t.Fatal("a stamped execution did not store at its bound key alone")
	}
}

// An unmanaged workspace stamps and looks up the empty binding, so its entries
// keep serving; an entry stored at a repository's binding does not.
func TestSourceBoundEntryInAnUnmanagedWorkspaceUsesTheEmptyBinding(t *testing.T) {
	spectest.Proves(t, sourceBoundFeature, sourceBoundRequirement, "an-unmanaged-workspace-uses-the-empty-binding")
	job := sourceBoundJob()
	f := newCaptureFixture(t, job)
	key := matrixKey(t, f, job)
	storeSourceBoundEntry(t, f, job, cachekey.SourceBoundEntryKey(key, ""))
	stubSourceBinding(f.sched, "", errors.New("not a git repository"), true)
	if _, result, _ := lookupSourceBound(t, f, job); result == nil {
		t.Fatal("an unmanaged workspace missed the entry stored at the empty binding")
	}

	g := newCaptureFixture(t, job)
	storeSourceBoundEntry(t, g, job, cachekey.SourceBoundEntryKey(key, "source-v1:sha256:in-a-repository"))
	stubSourceBinding(g.sched, "", errors.New("not a git repository"), true)
	if _, result, _ := lookupSourceBound(t, g, job); result != nil {
		t.Fatalf("an unmanaged workspace was served an entry stored inside a repository: %+v", result)
	}
}

// sourceBoundMatrixJob is the remote fixture task, declaring cache.sourceBound.
func sourceBoundMatrixJob() *ScheduledJob {
	job := matrixJob()
	job.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{Deterministic: true, SourceBound: true}
	return job
}

// publishSourceBoundMatrixEntry executes the fixture task in a producer
// workspace stamped with binding, finalizes it through the scheduler and
// shares its entry with server. It returns the cache key and the tree.
func publishSourceBoundMatrixEntry(t *testing.T, server *fakeCacheServer, job *ScheduledJob, binding string) (string, map[string]string) {
	t.Helper()
	producer, producerRemote := matrixFixture(t, server)
	key := matrixKey(t, producer, job)
	executeMatrixTask(t, producer, "stamp\n")
	result := matrixResult()
	result.SourceBinding, result.SourceBindingStamped = binding, true
	var mu sync.Mutex
	producer.sched.finalizeExecutedJob(t.Context(), job, result, true, key, "", nil, &mu, map[string]string{})
	producerRemote.DrainUploads()
	if !server.has(store.RemoteTaskEntryKey(cachekey.SourceBoundEntryKey(key, binding))) {
		t.Fatal("the producer shared no entry at the bound key")
	}
	if server.has(store.RemoteTaskEntryKey(key)) {
		t.Fatal("the producer shared a source-bound entry at its plain key")
	}
	return key, declaredTree(t, producer)
}

// The remote cache addresses the same bound key on every leg. A full restore
// and a status-only hit, remote or local, serve only at the binding the
// producer was stamped with, and negotiation never offers the plain key. A
// warm run, whose store holds the entry at the bound key, starts no provider.
func TestSourceBoundRemoteAndStatusOnlyLegsUseTheBoundKey(t *testing.T) {
	spectest.Proves(t, sourceBoundFeature, sourceBoundRequirement, "the-remote-and-status-only-legs-use-the-bound-key")
	const stamped, other = "source-v1:sha256:producer", "source-v1:sha256:other"

	t.Run("full restore", func(t *testing.T) {
		server := newFakeCacheServer()
		job := sourceBoundMatrixJob()
		key, want := publishSourceBoundMatrixEntry(t, server, job, stamped)
		consumer, remote := resultOnlyConsumer(t, server, cache.ModeFull)
		stubSourceBinding(consumer.sched, other, nil, false)
		started := server.requestCount(cache.OpInitialize)
		remote.Negotiate(t.Context(), consumer.ws, []*ScheduledJob{job}, nil, nil, consumer.cache, cachekey.CacheBypass{})
		if slices.Contains(server.prefetched(), store.RemoteTaskEntryKey(key)) {
			t.Fatal("negotiation offered the plain key of a source-bound task")
		}
		// The store holds no entry at the bound key, so the task may execute,
		// and negotiation starts the provider for it as for any such task.
		if got := server.requestCount(cache.OpInitialize); got != started+1 {
			t.Fatalf("initialize requests after a cold negotiation = %d, want %d", got, started+1)
		}
		if result := consumer.runDeclaredLookup(t, job, key); result != nil {
			t.Fatalf("a remote entry stored at another binding was served: %+v", result)
		}
		stubSourceBinding(consumer.sched, stamped, nil, false)
		if result := consumer.runDeclaredLookup(t, job, key); result == nil || result.Reuse != ReuseRemoteCache {
			t.Fatalf("remote lookup at the producer's binding = %+v, want a remote-cache hit", result)
		}
		assertSameTree(t, "remote at the producer's binding", want, declaredTree(t, consumer))

		// The next run of the workspace finds the restored entry at the bound
		// key. Negotiation asks that key, so the warm run starts no provider.
		next := attachRemote(t, consumer, cache.ModeFull)
		warm := server.requestCount(cache.OpInitialize)
		next.Negotiate(t.Context(), consumer.ws, []*ScheduledJob{job}, nil, nil, consumer.cache, cachekey.CacheBypass{})
		if got := server.requestCount(cache.OpInitialize); got != warm {
			t.Fatalf("a warm negotiation started the provider for a source-bound task (%d initialize requests, want %d)", got, warm)
		}
		if result := consumer.runDeclaredLookup(t, job, key); result == nil || result.Reuse != ReuseLocalCache {
			t.Fatalf("warm lookup at the producer's binding = %+v, want a local-cache hit", result)
		}
		if got := server.requestCount(cache.OpInitialize); got != warm {
			t.Fatalf("a warm local hit started the provider (%d initialize requests, want %d)", got, warm)
		}
	})

	t.Run("status only", func(t *testing.T) {
		server := newFakeCacheServer()
		server.echoResultOnly()
		job := sourceBoundMatrixJob()
		key, _ := publishSourceBoundMatrixEntry(t, server, job, stamped)
		consumer, remote := resultOnlyConsumer(t, server, cache.ModeMinimal)
		stubSourceBinding(consumer.sched, other, nil, false)
		remote.Negotiate(t.Context(), consumer.ws, []*ScheduledJob{job}, nil, nil, consumer.cache, cachekey.CacheBypass{})
		if remote.TaskFilesNeeded(job) {
			t.Fatal("the fixture reads the task's files, so the status-only legs would not run")
		}
		if slices.Contains(server.prefetchedResultOnly(), store.RemoteTaskEntryKey(key)) {
			t.Fatal("negotiation offered the plain key of a source-bound task as result-only")
		}
		if result := consumer.runDeclaredLookup(t, job, key); result != nil {
			t.Fatalf("a status-only hit stored at another binding was served: %+v", result)
		}
		stubSourceBinding(consumer.sched, stamped, nil, false)
		if result := consumer.runDeclaredLookup(t, job, key); result == nil || result.Reuse != ReuseRemoteCache {
			t.Fatalf("status-only lookup at the producer's binding = %+v, want a remote-cache hit", result)
		}
		assertNoMatrixOutputs(t, consumer)
		if consumer.cache.LookupResultOnlyTaskEntry(cachekey.SourceBoundEntryKey(key, stamped)) == nil {
			t.Fatal("the status-only hit recorded no local result-only entry at the bound key")
		}
		if consumer.cache.LookupResultOnlyTaskEntry(key) != nil {
			t.Fatal("the status-only hit recorded a local result-only entry at the plain key")
		}
		if result := consumer.runDeclaredLookup(t, job, key); result == nil || result.Reuse != ReuseLocalCache {
			t.Fatalf("second lookup at the producer's binding = %+v, want a local result-only hit", result)
		}
		stubSourceBinding(consumer.sched, other, nil, false)
		if result := consumer.runDeclaredLookup(t, job, key); result != nil {
			t.Fatalf("a local result-only entry served another binding: %+v", result)
		}
	})
}

// Two sessions at the same cache key with different bindings lease different
// keys. A waiter never waits on, or is handed an entry from, another binding's
// lease; a waiter at the same binding still coalesces onto what the lease
// owner publishes.
func TestSourceBoundLeaseAtOneBindingNeverHoldsAWaiterAtAnother(t *testing.T) {
	spectest.Proves(t, sourceBoundFeature, sourceBoundRequirement, "a-lease-at-one-binding-never-holds-a-waiter-at-another")
	job := sourceBoundJob()
	f := newCaptureFixture(t, job)
	key := matrixKey(t, f, job)
	const held, other = "source-v1:sha256:held", "source-v1:sha256:other"
	heldKey := cachekey.SourceBoundEntryKey(key, held)

	winner, release := f.cache.TryClaimTaskEntry(heldKey, time.Minute)
	if !winner {
		t.Fatal("the other session could not take its lease")
	}
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(release) })
	waited := 0
	f.sched.onCacheLeaseWait = func(*ScheduledJob) {
		waited++
		// The other session publishes its entry at its own binding, then
		// ends.
		storeSourceBoundEntry(t, f, job, heldKey)
		releaseOnce.Do(release)
	}

	stubSourceBinding(f.sched, other, nil, false)
	if _, result, _ := lookupSourceBound(t, f, job); result != nil || waited != 0 {
		t.Fatalf("lookup at another binding = %+v after %d lease waits, want a miss without waiting", result, waited)
	}
	stubSourceBinding(f.sched, held, nil, false)
	if _, result, _ := lookupSourceBound(t, f, job); result == nil || result.Reuse != ReuseCoalesced || waited != 1 {
		t.Fatalf("lookup at the leased binding = %+v after %d lease waits, want one wait and a coalesced hit", result, waited)
	}
	stubSourceBinding(f.sched, other, nil, false)
	if _, result, _ := lookupSourceBound(t, f, job); result != nil || waited != 1 {
		t.Fatalf("lookup at another binding after the publish = %+v, want a miss", result)
	}
}

// The failure record lives at the cache key alone, which leaves out files the
// binding covers, so the failure cache neither records nor replays a
// source-bound failure.
func TestSourceBoundFailureIsNeverRecordedOrReplayed(t *testing.T) {
	spectest.Proves(t, sourceBoundFeature, sourceBoundRequirement, "a-source-bound-failure-is-never-replayed")
	failed := func() *JobResult {
		return &JobResult{Status: "failed", Events: []RawJobEvent{metaEvent()}, Error: &JobError{Message: "invalid putnami.features.json"}}
	}
	bound := sourceBoundJob()
	f := newCaptureFixture(t, bound)
	var mu sync.Mutex
	f.sched.finalizeExecutedJob(context.Background(), bound, failed(), true, captureHashA, "", nil, &mu, map[string]string{})
	if record := f.cache.LookupTaskFailure(captureHashA); record != nil {
		t.Fatalf("a source-bound failure was recorded for replay: %+v", record)
	}

	// A record another writer left at the key is not replayed either, and its
	// attempt counter is left alone.
	if _, err := f.cache.RecordTaskFailure(captureHashA, store.TaskFailure{
		ExitCode: 1, FirstFailedAt: time.Now(), Result: cachekey.FailureEntryResultFromJobResult(failed()),
	}); err != nil {
		t.Fatalf("seed a failure record: %v", err)
	}
	if replayed := f.sched.replayFailedEntry(bound, captureHashA); replayed != nil {
		t.Fatalf("a source-bound task replayed a recorded failure: %+v", replayed)
	}
	if record := f.cache.LookupTaskFailure(captureHashA); record == nil || record.Attempts != 1 {
		t.Fatalf("the refused replay counted an observation: %+v", record)
	}

	// The same task without the policy replays it, so the refusal is the
	// policy's and not the fixture's.
	unbound := sourceBoundJob()
	unbound.JobDef.TaskCachePolicy.SourceBound = false
	if replayed := f.sched.replayFailedEntry(unbound, captureHashA); replayed == nil {
		t.Fatal("the control task without cache.sourceBound did not replay the recorded failure")
	}
}

// Invocation pruning, and the impact plan, predict a hit from an entry's
// presence at a cache key. A source-bound entry's store key also binds the
// current binding, which they do not compute, so it never counts.
func TestSourceBoundEntryNeverPrunesAnInvocationProducer(t *testing.T) {
	spectest.Proves(t, sourceBoundFeature, sourceBoundRequirement, "a-source-bound-entry-prunes-no-producer")
	job := sourceBoundJob()
	f := newCaptureFixture(t, job)
	storeSourceBoundEntry(t, f, job, captureHashA)
	if cachedEntryPresent(f.cache, job, captureHashA) {
		t.Fatal("a source-bound entry counted as present for invocation pruning")
	}
	job.JobDef.TaskCachePolicy.SourceBound = false
	if !cachedEntryPresent(f.cache, job, captureHashA) {
		t.Fatal("the control entry without cache.sourceBound did not count as present")
	}
}

// The stamped binding is the one the stamp on disk names for the project
// itself: a usable binding, or the empty one with the unavailable marker. Any
// other stamp names none.
func TestStampedSourceBindingReadsOnlyAUsableOwnBinding(t *testing.T) {
	job := sourceBoundJob()
	f := newCaptureFixture(t, job)
	stamp := filepath.Join(f.ws.Root, captureTestProject, filepath.FromSlash(versionStampRelPath))
	write := func(packages ...CapabilityPackageStamp) {
		t.Helper()
		data, err := json.Marshal(VersionInfo{Name: captureTestProject, CapabilityPackages: packages})
		if err != nil {
			t.Fatal(err)
		}
		writeFileAt(t, stamp, string(data))
	}

	if _, ok := f.sched.stampedSourceBinding(job); ok {
		t.Fatal("a missing stamp named a binding")
	}
	for _, tc := range []struct {
		name     string
		packages []CapabilityPackageStamp
		want     string
		ok       bool
	}{
		{"a binding", []CapabilityPackageStamp{{Package: captureTestProject, SourceBinding: "source-v1:sha256:a"}}, "source-v1:sha256:a", true},
		{"unmanaged", []CapabilityPackageStamp{{Package: captureTestProject, SourceBindingUnavailable: true}}, "", true},
		{"failed inside a repository", []CapabilityPackageStamp{{Package: captureTestProject}}, "", false},
		{"both a binding and the marker", []CapabilityPackageStamp{{Package: captureTestProject, SourceBinding: "x", SourceBindingUnavailable: true}}, "", false},
		{"only a dependency", []CapabilityPackageStamp{{Package: "dep", SourceBinding: "source-v1:sha256:dep"}}, "", false},
		{"beside a dependency", []CapabilityPackageStamp{
			{Package: "dep", SourceBinding: "source-v1:sha256:dep"},
			{Package: captureTestProject, SourceBinding: "source-v1:sha256:own"},
		}, "source-v1:sha256:own", true},
	} {
		write(tc.packages...)
		if got, ok := f.sched.stampedSourceBinding(job); got != tc.want || ok != tc.ok {
			t.Errorf("%s: stamped binding = (%q, %t), want (%q, %t)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
	writeFileAt(t, stamp, "{torn")
	if _, ok := f.sched.stampedSourceBinding(job); ok {
		t.Fatal("a torn stamp named a binding")
	}
}

// sourceBoundEvents is the protocol v2 event stream of a script that
// succeeds.
const sourceBoundEvents = "printf '%s\\n' '{\"v\":2,\"type\":\"meta\",\"data\":{\"protocol\":2}}'\n" +
	"printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\"}}'\n"

// committedFeatureEvidence is where the Go framework's committed feature
// evidence lives, relative to its project.
const committedFeatureEvidence = "schema/feature-evidence/go-framework.json"

// sourceBoundRunFixture is a Git-managed workspace with one project, a
// describe task and a compile task whose key folds describe's.
//
// The describe task is @putnami/go's real build-describe contract: the
// fixture reads its declared outputs and its cache policy from
// go/extension/putnami.extension.json. Only its command is a script, which
// writes the evidence the way the describe binary does: the build stamp
// copied into .gen/schema/feature-evidence, then promoted to the committed
// copy. The key reads the project's Go files without test files, as the real
// sources input does; the fixture needs none of its other inputs.
func sourceBoundRunFixture(t *testing.T, describeLog, compileLog string) (*workspace.Workspace, []*ScheduledJob) {
	t.Helper()
	goExtension := extension.LoadExtensionFromDir(filepath.Join(findJobsRepoRoot(t), "go", "extension"), "/go/extension")
	if goExtension == nil {
		t.Fatal("go/extension/putnami.extension.json did not load")
	}
	describeTask, ok := goExtension.Tasks["build-describe"]
	if !ok || describeTask.Declares == nil || describeTask.Cache == nil || !describeTask.Cache.SourceBound {
		t.Fatalf("build-describe = %+v, want a declared, source-bound task", describeTask)
	}
	describePolicy := *describeTask.Cache

	root := t.TempDir()
	runVersionsGit(t, root, "init")
	writeTestFile(t, filepath.Join(root, "app", "main.go"), "package main\n")
	writeTestFile(t, filepath.Join(root, "app", "main_test.go"), "package main\n\n// first comment\n")

	scripts := t.TempDir()
	describe := filepath.Join(scripts, "describe.sh")
	writeExecutable(t, describe, "#!/bin/sh\n"+
		"printf 'run\\n' >> "+shellQuote(describeLog)+"\n"+
		"mkdir -p .gen/schema/feature-evidence schema/feature-evidence\n"+
		"cp .gen/version.json .gen/schema/feature-evidence/go-framework.json\n"+
		"cp .gen/schema/feature-evidence/go-framework.json "+committedFeatureEvidence+"\n"+
		sourceBoundEvents+"exit 0\n")
	compile := filepath.Join(scripts, "compile.sh")
	writeExecutable(t, compile, "#!/bin/sh\n"+
		"printf 'run\\n' >> "+shellQuote(compileLog)+"\n"+
		sourceBoundEvents+"exit 0\n")

	project := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
	ws := workspace.NewWorkspace(root, nil, []*workspace.Project{project})
	ws.Name = "source-bound-ws"
	ext := &extension.ExtensionDescription{
		Name:    "@test/probe",
		Version: "1.0.0",
		Path:    filepath.Join(root, "ext"),
		Tasks: map[string]extension.TaskDefinition{
			"build-describe": describeTask,
			"probe-compile":  {Declares: &extension.TaskDeclaration{}},
		},
	}
	sources := []string{"**/*.go", "!**/*_test.go"}
	job := func(name, task, command string, policy *extension.TaskCachePolicy) *ScheduledJob {
		commandName, step := jobCommandAndStep(name)
		return &ScheduledJob{
			Project:   project,
			Extension: ext,
			Step:      &extension.PipelineStep{ID: step, Task: task},
			JobDef: &extension.JobDefinition{
				ExtensionName: "@test/probe", Name: name, CommandName: commandName, StepID: step,
				Command: command, Cwd: "{projectRoot}", Cache: true, FilePatterns: sources, TaskCachePolicy: policy,
			},
		}
	}
	describeJob := job("build~describe", "build-describe", describe, &describePolicy)
	compileJob := job("build~compile", "probe-compile", compile, &extension.TaskCachePolicy{Deterministic: true, NoOutput: true})
	compileJob.DependsOn = []string{describeJob.Key()}
	return ws, []*ScheduledJob{describeJob, compileJob}
}

// sourceBoundEvidenceBinding returns the binding the committed feature
// evidence of the fixture's project names, after checking that the .gen copy
// holds the same bytes.
func sourceBoundEvidenceBinding(t *testing.T, projectRoot string) string {
	t.Helper()
	committed := readFileAt(t, filepath.Join(projectRoot, filepath.FromSlash(committedFeatureEvidence)))
	if generated := readFileAt(t, filepath.Join(projectRoot, ".gen", filepath.FromSlash(committedFeatureEvidence))); generated != committed {
		t.Fatalf("the committed evidence and its .gen copy differ:\n committed %s\n generated %s", committed, generated)
	}
	var stamp VersionInfo
	if err := json.Unmarshal([]byte(committed), &stamp); err != nil {
		t.Fatalf("decode evidence: %v", err)
	}
	for _, pkg := range stamp.CapabilityPackages {
		if pkg.Package == "app" {
			return pkg.SourceBinding
		}
	}
	t.Fatalf("evidence names no binding for app: %+v", stamp)
	return ""
}

// The defect this policy closes, end to end through the real scheduler and
// the real build-describe declaration. An edit to a test file moves the
// project's source binding and no key. The first run after it executes
// describe and stores a second entry beside the first, and the next run at the
// same tree hits it. Reverting the edit returns the tree to the earlier
// binding: the run hits the earlier entry, and its restore puts the committed
// evidence back at the earlier binding. The describe key, and the key of the
// task that folds it, never move, and the dependent is served from its key on
// every run.
func TestSourceBoundTestOnlyEditStoresBesideTheFirstEntryAndKeepsEveryKey(t *testing.T) {
	spectest.Proves(t, sourceBoundFeature, sourceBoundRequirement, "a-test-only-edit-stores-beside-the-first-entry-and-keeps-every-key")
	requireShell(t)
	logs := t.TempDir()
	describeLog, compileLog := filepath.Join(logs, "describe"), filepath.Join(logs, "compile")
	ws, planned := sourceBoundRunFixture(t, describeLog, compileLog)
	cache := sharedRuntimeCache(t)
	projectRoot := filepath.Join(ws.Root, "app")
	testFile := filepath.Join(projectRoot, "main_test.go")
	var keys []string
	run := func(name string, describeRuns int) *SchedulerResult {
		t.Helper()
		result := runSharedScheduler(t.Context(), ws, planned, SchedulerConfig{MaxParallel: 1}, cache)
		for i := range planned {
			if got := resultOf(t, result, planned, i); got.Status != "success" {
				t.Fatalf("%s: %s = %+v, want success", name, planned[i].Key(), got)
			}
		}
		now := []string{resultOf(t, result, planned, 0).CacheKey, resultOf(t, result, planned, 1).CacheKey}
		if now[0] == "" || now[1] == "" {
			t.Fatalf("%s: a job has no cache key: %v", name, now)
		}
		if keys == nil {
			keys = now
		} else if !slices.Equal(keys, now) {
			t.Fatalf("%s: keys moved from %v to %v", name, keys, now)
		}
		if got := subprocessRuns(t, describeLog); got != describeRuns {
			t.Fatalf("%s: describe executions = %d, want %d", name, got, describeRuns)
		}
		if got := subprocessRuns(t, compileLog); got != 1 {
			t.Fatalf("%s: compile executions = %d, want 1: the dependent's key must hit", name, got)
		}
		return result
	}
	evidenceBinding := func() string {
		t.Helper()
		return sourceBoundEvidenceBinding(t, projectRoot)
	}
	storedAt := func(binding string) bool {
		t.Helper()
		entry, err := cache.LookupTaskEntry(cachekey.SourceBoundEntryKey(keys[0], binding))
		return err == nil && entry != nil
	}

	run("cold", 1)
	before := evidenceBinding()
	if !strings.HasPrefix(before, "source-v1:sha256:") {
		t.Fatalf("cold evidence binding = %q, want a source-v1 binding", before)
	}
	if got := resultOf(t, run("warm", 1), planned, 0); !got.Reuse.CacheHit() {
		t.Fatalf("unchanged tree: describe = %+v, want a hit", got)
	}

	writeTestFile(t, testFile, "package main\n\n// second comment\n")
	if got := resultOf(t, run("first run after a test-only edit", 2), planned, 0); got.Reuse.CacheHit() {
		t.Fatalf("first run after a test-only edit: describe = %+v, want an execution", got)
	}
	after := evidenceBinding()
	if after == before {
		t.Fatalf("evidence still names the binding before the edit, %q", before)
	}
	if stamped := readVersionStamp(t, projectRoot); stamped.CapabilityPackages[0].SourceBinding != after {
		t.Fatalf("evidence binding %q, stamp binding %q: the evidence is not the current one", after, stamped.CapabilityPackages[0].SourceBinding)
	}
	if got := resultOf(t, run("second run after a test-only edit", 2), planned, 0); !got.Reuse.CacheHit() {
		t.Fatalf("second run after a test-only edit: describe = %+v, want a hit", got)
	}
	if got := evidenceBinding(); got != after {
		t.Fatalf("second run left evidence binding %q, want %q", got, after)
	}

	// Revert the edit. Only the restore can rewrite the committed evidence
	// now, because describe does not execute.
	writeTestFile(t, testFile, "package main\n\n// first comment\n")
	if got := resultOf(t, run("back at the earlier tree", 2), planned, 0); !got.Reuse.CacheHit() {
		t.Fatalf("back at the earlier tree: describe = %+v, want a hit on the earlier entry", got)
	}
	if got := evidenceBinding(); got != before {
		t.Fatalf("back at the earlier tree: committed evidence binding %q, want the earlier %q", got, before)
	}
	if stamped := readVersionStamp(t, projectRoot); stamped.CapabilityPackages[0].SourceBinding != before {
		t.Fatalf("back at the earlier tree: stamp binding %q, want the earlier %q", stamped.CapabilityPackages[0].SourceBinding, before)
	}

	// Both entries sit side by side at their bound keys, and none at the
	// plain key.
	if !storedAt(before) || !storedAt(after) {
		t.Fatalf("entries at the earlier binding %t and at the edited binding %t, want both", storedAt(before), storedAt(after))
	}
	if entry, err := cache.LookupTaskEntry(keys[0]); err != nil || entry != nil {
		t.Fatalf("a describe entry was stored at the plain key: %+v, %v", entry, err)
	}
}

// An earlier task of the same run rewrites a test file of the project after
// the project's stamp was written, the way a lint fix does. Describe executes
// at the binding of the rewritten tree: its evidence and its entry name that
// binding, so the next run at the same tree hits.
func TestSourceBoundExecutionAfterAnInRunRewriteReadsTheCurrentBinding(t *testing.T) {
	spectest.Proves(t, sourceBoundFeature, sourceBoundRequirement, "an-execution-after-an-in-run-rewrite-reads-the-current-binding")
	requireShell(t)
	logs := t.TempDir()
	describeLog, compileLog := filepath.Join(logs, "describe"), filepath.Join(logs, "compile")
	ws, planned := sourceBoundRunFixture(t, describeLog, compileLog)
	projectRoot := filepath.Join(ws.Root, "app")

	// The fixer is uncached, so it executes on every run and is the first
	// task of the project to execute: it writes the project's stamp, then
	// rewrites the test file.
	describeJob := planned[0]
	fixer := filepath.Join(t.TempDir(), "fix.sh")
	writeExecutable(t, fixer, "#!/bin/sh\n"+
		"printf 'package main\\n\\n// fixed comment\\n' > main_test.go\n"+
		sourceBoundEvents+"exit 0\n")
	commandName, step := jobCommandAndStep("lint~fix")
	describeJob.Extension.Tasks["probe-fix"] = extension.TaskDefinition{Declares: &extension.TaskDeclaration{}}
	fixJob := &ScheduledJob{
		Project:   describeJob.Project,
		Extension: describeJob.Extension,
		Step:      &extension.PipelineStep{ID: step, Task: "probe-fix"},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@test/probe", Name: "lint~fix", CommandName: commandName, StepID: step,
			Command: fixer, Cwd: "{projectRoot}",
		},
	}
	describeJob.DependsOn = []string{fixJob.Key()}
	planned = append(planned, fixJob)

	cache := sharedRuntimeCache(t)
	run := func(name string) *JobResult {
		t.Helper()
		result := runSharedScheduler(t.Context(), ws, planned, SchedulerConfig{MaxParallel: 1}, cache)
		for i := range planned {
			if got := resultOf(t, result, planned, i); got.Status != "success" {
				t.Fatalf("%s: %s = %+v, want success", name, planned[i].Key(), got)
			}
		}
		return resultOf(t, result, planned, 0)
	}

	if got := run("the run that rewrites the test file"); got.Reuse.CacheHit() {
		t.Fatalf("the run that rewrites the test file: describe = %+v, want an execution", got)
	}
	evidence := sourceBoundEvidenceBinding(t, projectRoot)
	if got := readFileAt(t, filepath.Join(projectRoot, "main_test.go")); !strings.Contains(got, "fixed comment") {
		t.Fatalf("the fixer did not rewrite the test file: %q", got)
	}

	second := run("the next run at the rewritten tree")
	if !second.Reuse.CacheHit() {
		t.Fatalf("the next run at the rewritten tree: describe = %+v, want a hit: the first run stored its entry at the binding before the rewrite", second)
	}
	if got := subprocessRuns(t, describeLog); got != 1 {
		t.Fatalf("describe executions = %d, want 1", got)
	}
	// The second run stamps the rewritten tree before any task runs.
	if stamped := readVersionStamp(t, projectRoot).CapabilityPackages[0].SourceBinding; stamped != evidence {
		t.Fatalf("the first run's evidence names %q, the rewritten tree's binding is %q", evidence, stamped)
	}
	if got := sourceBoundEvidenceBinding(t, projectRoot); got != evidence {
		t.Fatalf("the hit left evidence binding %q, want %q", got, evidence)
	}
}

// useConvergingDescribe points the fixture's describe job at a script that
// writes the evidence the way the fixture's own script does, then runs
// sidecar in the project directory, the way Go describe converges committed
// sidecars after its binary wrote the evidence.
func useConvergingDescribe(t *testing.T, describeJob *ScheduledJob, describeLog, sidecar string) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "describe.sh")
	writeExecutable(t, script, "#!/bin/sh\n"+
		"printf 'run\\n' >> "+shellQuote(describeLog)+"\n"+
		"mkdir -p .gen/schema/feature-evidence schema/feature-evidence\n"+
		"cp .gen/version.json .gen/schema/feature-evidence/go-framework.json\n"+
		"cp .gen/schema/feature-evidence/go-framework.json "+committedFeatureEvidence+"\n"+
		sidecar+
		sourceBoundEvents+"exit 0\n")
	describeJob.JobDef.Command = script
}

// projectTreeBinding computes the source binding of the fixture's project
// from the tree as it stands, outside any scheduler memo.
func projectTreeBinding(t *testing.T, ws *workspace.Workspace) string {
	t.Helper()
	binding, _, err := putnamigit.ProjectSourceBindingMeasured(ws.Root, filepath.Join(ws.Root, "app"))
	if err != nil {
		t.Fatalf("compute the project's source binding: %v", err)
	}
	return binding
}

// A describe that converges a committed sidecar the binding covers moves the
// binding of its own project. The scheduler executes it once more at the
// binding the tree now has: its evidence, the stamp, its entry and the tree
// name one binding, and the next run at that tree hits without executing
// describe. The settled result keeps the writes of the first execution, and no
// key moves.
func TestSourceBoundExecutionThatMovesItsBindingSettlesAtTheBindingItLeaves(t *testing.T) {
	spectest.Proves(t, sourceBoundFeature, sourceBoundSettleRequirement, "an-execution-that-moves-its-binding-settles-at-the-binding-it-leaves")
	requireShell(t)
	logs := t.TempDir()
	describeLog, compileLog := filepath.Join(logs, "describe"), filepath.Join(logs, "compile")
	ws, planned := sourceBoundRunFixture(t, describeLog, compileLog)
	projectRoot := filepath.Join(ws.Root, "app")
	sidecar := filepath.Join(projectRoot, "schema", "http-routes.json")
	writeTestFile(t, sidecar, "stale routes\n")
	useConvergingDescribe(t, planned[0], describeLog, "printf 'converged routes\\n' > schema/http-routes.json\n")
	cache := sharedRuntimeCache(t)
	before := projectTreeBinding(t, ws)

	run := func(name string, cfg SchedulerConfig) (*JobResult, *JobResult) {
		t.Helper()
		result := runSharedScheduler(t.Context(), ws, planned, cfg, cache)
		for i := range planned {
			if got := resultOf(t, result, planned, i); got.Status != "success" {
				t.Fatalf("%s: %s = %+v, want success", name, planned[i].Key(), got)
			}
		}
		return resultOf(t, result, planned, 0), resultOf(t, result, planned, 1)
	}

	describe, compile := run("the run that converges the sidecar", SchedulerConfig{MaxParallel: 1, CacheVerification: true})
	if describe.Reuse.CacheHit() {
		t.Fatalf("the run that converges the sidecar: describe = %+v, want an execution", describe)
	}
	if got := subprocessRuns(t, describeLog); got != 2 {
		t.Fatalf("describe executions = %d, want 2: the execution that moved the binding, then one at the binding it left", got)
	}
	if got := readFileAt(t, sidecar); got != "converged routes\n" {
		t.Fatalf("sidecar = %q, want the converged bytes", got)
	}
	settled := projectTreeBinding(t, ws)
	if settled == before {
		t.Fatalf("converging the sidecar left the binding at %q", before)
	}
	if got := sourceBoundEvidenceBinding(t, projectRoot); got != settled {
		t.Fatalf("the evidence names %q, the tree the run left is %q", got, settled)
	}
	if got := readVersionStamp(t, projectRoot).CapabilityPackages[0].SourceBinding; got != settled {
		t.Fatalf("the stamp names %q, the tree the run left is %q", got, settled)
	}
	if written := filepath.ToSlash(filepath.Join("app", "schema", "http-routes.json")); !slices.Contains(describe.VerificationWrites, written) {
		t.Fatalf("the settled result's writes %v miss %s, which only the first execution wrote", describe.VerificationWrites, written)
	}
	keys := []string{describe.CacheKey, compile.CacheKey}
	storedAt := func(binding string) bool {
		t.Helper()
		entry, err := cache.LookupTaskEntry(cachekey.SourceBoundEntryKey(keys[0], binding))
		return err == nil && entry != nil
	}
	if !storedAt(settled) || storedAt(before) {
		t.Fatalf("entries at the settled binding %t and at the binding before the run %t, want only the settled one", storedAt(settled), storedAt(before))
	}

	describe, compile = run("the next run at the converged tree", SchedulerConfig{MaxParallel: 1})
	if !describe.Reuse.CacheHit() {
		t.Fatalf("the next run at the converged tree: describe = %+v, want a hit", describe)
	}
	if got := subprocessRuns(t, describeLog); got != 2 {
		t.Fatalf("describe executions = %d, want 2: the next run must not execute describe", got)
	}
	if got := subprocessRuns(t, compileLog); got != 1 {
		t.Fatalf("compile executions = %d, want 1: the dependent's key must hit", got)
	}
	if now := []string{describe.CacheKey, compile.CacheKey}; !slices.Equal(now, keys) {
		t.Fatalf("keys moved from %v to %v", keys, now)
	}
	if got := sourceBoundEvidenceBinding(t, projectRoot); got != settled {
		t.Fatalf("the hit left evidence binding %q, want %q", got, settled)
	}
	if got := projectTreeBinding(t, ws); got != settled {
		t.Fatalf("the hit moved the tree's binding from %q to %q", settled, got)
	}
}

// A task whose every execution moves its binding executes once more and no
// further. Its entry is stored at the binding its last execution was stamped
// with, which is the binding its outputs embed.
func TestSourceBoundExecutionSettlesAtMostOnce(t *testing.T) {
	spectest.Proves(t, sourceBoundFeature, sourceBoundSettleRequirement, "a-task-that-keeps-moving-its-binding-executes-at-most-once-more")
	requireShell(t)
	logs := t.TempDir()
	describeLog, compileLog := filepath.Join(logs, "describe"), filepath.Join(logs, "compile")
	ws, planned := sourceBoundRunFixture(t, describeLog, compileLog)
	projectRoot := filepath.Join(ws.Root, "app")
	useConvergingDescribe(t, planned[0], describeLog, "printf 'run\\n' >> schema/http-routes.json\n")
	cache := sharedRuntimeCache(t)

	result := runSharedScheduler(t.Context(), ws, planned, SchedulerConfig{MaxParallel: 1}, cache)
	for i := range planned {
		if got := resultOf(t, result, planned, i); got.Status != "success" {
			t.Fatalf("%s = %+v, want success", planned[i].Key(), got)
		}
	}
	if got := subprocessRuns(t, describeLog); got != 2 {
		t.Fatalf("describe executions = %d, want 2: one execution and one settling execution", got)
	}
	evidence := sourceBoundEvidenceBinding(t, projectRoot)
	if tree := projectTreeBinding(t, ws); tree == evidence {
		t.Fatalf("the settling execution left the binding at %q; the fixture must move it on every execution", tree)
	}
	key := resultOf(t, result, planned, 0).CacheKey
	if entry, err := cache.LookupTaskEntry(cachekey.SourceBoundEntryKey(key, evidence)); err != nil || entry == nil {
		t.Fatalf("no entry at the binding the outputs embed, %q: %+v, %v", evidence, entry, err)
	}
}

// A run without the cache settles too: the evidence describe leaves names the
// tree it leaves whether or not an entry is stored. An execution that moves
// nothing executes once.
func TestSourceBoundExecutionWithoutTheCacheSettlesToo(t *testing.T) {
	spectest.Proves(t, sourceBoundFeature, sourceBoundSettleRequirement, "a-run-without-the-cache-settles-too")
	requireShell(t)
	logs := t.TempDir()
	describeLog, compileLog := filepath.Join(logs, "describe"), filepath.Join(logs, "compile")
	ws, planned := sourceBoundRunFixture(t, describeLog, compileLog)
	projectRoot := filepath.Join(ws.Root, "app")
	writeTestFile(t, filepath.Join(projectRoot, "schema", "http-routes.json"), "stale routes\n")
	useConvergingDescribe(t, planned[0], describeLog, "printf 'converged routes\\n' > schema/http-routes.json\n")
	cache := sharedRuntimeCache(t)

	run := func(name string, describeRuns int) {
		t.Helper()
		result := runSharedScheduler(t.Context(), ws, planned, SchedulerConfig{MaxParallel: 1, NoCache: true}, cache)
		for i := range planned {
			if got := resultOf(t, result, planned, i); got.Status != "success" {
				t.Fatalf("%s: %s = %+v, want success", name, planned[i].Key(), got)
			}
		}
		if got := subprocessRuns(t, describeLog); got != describeRuns {
			t.Fatalf("%s: describe executions = %d, want %d", name, got, describeRuns)
		}
		if evidence, tree := sourceBoundEvidenceBinding(t, projectRoot), projectTreeBinding(t, ws); evidence != tree {
			t.Fatalf("%s: the evidence names %q, the tree the run left is %q", name, evidence, tree)
		}
	}
	run("the run that converges the sidecar", 2)
	run("the next run at the converged tree", 3)
}
