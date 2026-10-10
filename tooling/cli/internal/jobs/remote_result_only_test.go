package jobs

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	cache "go.putnami.dev/protocol/cache"
	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	protocoljob "go.putnami.dev/protocol/job"
	"go.putnami.dev/tooling/cli/internal/cacheprovider"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// publishMatrixEntry runs the matrix task in a producer workspace of its own,
// shares its entry with server, and returns the task's cache key and the tree
// it wrote.
func publishMatrixEntry(t *testing.T, server *fakeCacheServer) (string, map[string]string) {
	t.Helper()
	producer, producerRemote := matrixFixture(t, server)
	job := matrixJob()
	hash := matrixKey(t, producer, job)
	executeMatrixTask(t, producer, "stamp\n")
	if !producer.sched.storeDeclaredCapture(job, matrixResult(), hash) {
		t.Fatal("the producer published no entry")
	}
	producerRemote.UploadTaskEntry(t.Context(), hash, job, producer.cache)
	producerRemote.DrainUploads()
	if !server.has(store.RemoteTaskEntryKey(hash)) {
		t.Fatal("the producer shared no entry")
	}
	return hash, declaredTree(t, producer)
}

// resultOnlyConsumer is a fresh workspace, with an empty store, whose runs
// materialize in mode.
func resultOnlyConsumer(t *testing.T, server *fakeCacheServer, mode cache.Mode) (*captureFixture, *RemoteCache) {
	t.Helper()
	t.Setenv(cacheModeEnv, string(mode))
	return matrixFixture(t, server)
}

// attachRemote gives f a new provider session in mode, as the next run of
// the same workspace has.
func attachRemote(t *testing.T, f *captureFixture, mode cache.Mode) *RemoteCache {
	t.Helper()
	t.Setenv(cacheModeEnv, string(mode))
	remote, notice := LoadRemoteCache(context.Background(), f.ws.Root,
		[]*extension.ExtensionDescription{fakeProviderExtension(t, nil)}, nil, store.CacheTrustAny)
	if remote == nil || !remote.isProviderBacked() {
		t.Fatalf("expected a provider-backed remote cache, got %+v (notice %q)", remote, notice)
	}
	t.Cleanup(remote.Close)
	f.sched.setRemoteCache(remote)
	return remote
}

// assertNoMatrixOutputs fails when any declared output of the matrix task
// exists in f's workspace.
func assertNoMatrixOutputs(t *testing.T, f *captureFixture) {
	t.Helper()
	for id, path := range matrixOutputs(f) {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("output %q exists at %s (err=%v); a status-only hit writes nothing", id, path, err)
		}
	}
}

// ---------------------------------------------------------------------------
// a hit no job reads
// ---------------------------------------------------------------------------

// TestResultOnlyHitPlacesNoBlobAndWritesNothing pins the acceptance case: in
// minimal mode, a hit no planned job waits for is restored result-only. The
// provider places no blob, the workspace stays untouched, the result is
// recorded in a local result-only entry, and the next run that does not read
// the files is served from it without asking the provider.
func TestResultOnlyHitPlacesNoBlobAndWritesNothing(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "result-only-remote-restore",
		"a-hit-no-job-waits-for-moves-no-bytes")
	server := newFakeCacheServer()
	server.echoResultOnly()
	hash, _ := publishMatrixEntry(t, server)
	remoteKey := store.RemoteTaskEntryKey(hash)

	consumer, remote := resultOnlyConsumer(t, server, cache.ModeMinimal)
	job := matrixJob()
	remote.Negotiate(t.Context(), consumer.ws, []*ScheduledJob{job}, nil, nil, consumer.cache, CacheBypass{})
	if got := server.prefetchedResultOnly(); !slices.Equal(got, []string{remoteKey}) {
		t.Fatalf("result-only prefetch keys = %v, want [%s]", got, remoteKey)
	}
	assertNoMatrixOutputs(t, consumer)

	placed := server.placedBlobs()
	result := consumer.runDeclaredLookup(t, job, hash)
	if result == nil || result.Reuse != ReuseRemoteCache || result.Status != "success" {
		t.Fatalf("result-only lookup = %+v, want a successful remote-cache hit", result)
	}
	if got := server.placedBlobs() - placed; got != 0 {
		t.Fatalf("the provider placed %d blobs for a result-only restore", got)
	}
	assertNoMatrixOutputs(t, consumer)
	if entry, err := consumer.cache.LookupTaskEntry(hash); err != nil || entry != nil {
		t.Fatalf("a result-only hit published a task-owned entry: %+v (err=%v)", entry, err)
	}
	if consumer.cache.LookupResultOnlyTaskEntry(hash) == nil {
		t.Fatal("a result-only hit recorded no local result-only entry")
	}
	stats := remote.Stats()
	if stats == nil || stats.Hits != 1 || stats.Misses != 0 || stats.Restored != 1 || stats.BytesFetched != 0 {
		t.Fatalf("stats = %+v, want one provider hit with zero fetched bytes", stats)
	}

	restores := server.requestCount(cache.OpRestore)
	again := consumer.runDeclaredLookup(t, job, hash)
	if again == nil || again.Reuse != ReuseLocalCache || again.Status != "success" {
		t.Fatalf("second lookup = %+v, want a local-cache hit from the result-only entry", again)
	}
	if got := server.requestCount(cache.OpRestore); got != restores {
		t.Fatalf("a local result-only hit asked the provider again (%d restores, want %d)", got, restores)
	}
	assertNoMatrixOutputs(t, consumer)
}

// TestNegotiateCountsALocalResultOnlyEntryAsALocalHit pins the remote counts
// of a warm rerun: a key that a local result-only entry serves, and whose files
// no job of the run reads, is a local hit. Negotiate leaves it out of the
// provider's key set, so the rerun reports no remote miss for it. A run that
// reads the files still negotiates the key.
func TestNegotiateCountsALocalResultOnlyEntryAsALocalHit(t *testing.T) {
	server := newFakeCacheServer()
	server.echoResultOnly()
	hash, _ := publishMatrixEntry(t, server)
	remoteKey := store.RemoteTaskEntryKey(hash)

	consumer, remote := resultOnlyConsumer(t, server, cache.ModeMinimal)
	job := matrixJob()
	remote.Negotiate(t.Context(), consumer.ws, []*ScheduledJob{job}, nil, nil, consumer.cache, CacheBypass{})
	if result := consumer.runDeclaredLookup(t, job, hash); result == nil || result.Reuse != ReuseRemoteCache {
		t.Fatalf("first run = %+v, want a remote-cache hit", result)
	}
	if consumer.cache.LookupResultOnlyTaskEntry(hash) == nil {
		t.Fatal("the first run recorded no result-only entry")
	}

	rerun := attachRemote(t, consumer, cache.ModeMinimal)
	prefetches := server.requestCount(cache.OpPrefetch)
	rerun.Negotiate(t.Context(), consumer.ws, []*ScheduledJob{job}, nil, nil, consumer.cache, CacheBypass{})
	if !rerun.localHits[hash] {
		t.Fatal("negotiate did not count the local result-only entry as a local hit")
	}
	if got := server.requestCount(cache.OpPrefetch); got != prefetches {
		t.Fatalf("negotiate asked the provider for a key a local result-only entry serves (%d prefetches, want %d)", got, prefetches)
	}
	restores := server.requestCount(cache.OpRestore)
	result := consumer.runDeclaredLookup(t, job, hash)
	if result == nil || result.Reuse != ReuseLocalCache {
		t.Fatalf("rerun = %+v, want a local-cache hit from the result-only entry", result)
	}
	if got := server.requestCount(cache.OpRestore); got != restores {
		t.Fatalf("the rerun asked the provider to restore (%d restores, want %d)", got, restores)
	}
	if stats := rerun.Stats(); stats == nil || stats.KeysRequested != 0 || stats.Misses != 0 || stats.Hits != 0 || stats.LocalHits != 1 {
		t.Fatalf("rerun stats = %+v, want one local hit and no remote key", stats)
	}

	full := attachRemote(t, consumer, cache.ModeFull)
	full.Negotiate(t.Context(), consumer.ws, []*ScheduledJob{job}, nil, nil, consumer.cache, CacheBypass{})
	if full.localHits[hash] {
		t.Fatal("a run that reads the files counted the result-only entry as a local hit")
	}
	if got := server.prefetched(); !slices.Contains(got, remoteKey) || server.requestCount(cache.OpPrefetch) != prefetches+1 {
		t.Fatalf("a run that reads the files did not negotiate the key (prefetched %v)", got)
	}
	if stats := full.Stats(); stats == nil || stats.KeysRequested != 1 {
		t.Fatalf("full run stats = %+v, want the key requested from the provider", stats)
	}
}

// TestResultOnlyHintIsAQuietMiss pins the trust rule: a hint can warm blobs but
// never return a green result, and a result-only restore fetched no blob to
// warm. The task executes, and nothing is recorded or written.
func TestResultOnlyHintIsAQuietMiss(t *testing.T) {
	server := newFakeCacheServer()
	server.echoResultOnly()
	hash, _ := publishMatrixEntry(t, server)
	server.channel = cache.ChannelHint

	consumer, remote := resultOnlyConsumer(t, server, cache.ModeMinimal)
	remote.trust = store.CacheTrustCI
	job := matrixJob()
	remote.Negotiate(t.Context(), consumer.ws, []*ScheduledJob{job}, nil, nil, consumer.cache, CacheBypass{})

	placed := server.placedBlobs()
	if result := consumer.runDeclaredLookup(t, job, hash); result != nil {
		t.Fatalf("a hint served a result: %+v", result)
	}
	if got := server.placedBlobs() - placed; got != 0 {
		t.Fatalf("the provider placed %d blobs for a result-only hint", got)
	}
	if consumer.cache.LookupResultOnlyTaskEntry(hash) != nil {
		t.Fatal("a hint recorded a result-only entry")
	}
	if entry, err := consumer.cache.LookupTaskEntry(hash); err != nil || entry != nil {
		t.Fatalf("a hint published a task-owned entry: %+v (err=%v)", entry, err)
	}
	assertNoMatrixOutputs(t, consumer)
	if stats := remote.Stats(); stats.Hits != 0 || stats.HintsWarmed != 0 || stats.Restored != 0 {
		t.Fatalf("stats = %+v, want a miss with no warm", stats)
	}
}

// ---------------------------------------------------------------------------
// a hit some job reads
// ---------------------------------------------------------------------------

// TestResultOnlyRestoresTheFilesOfATaskALaterJobWaitsFor pins the reader rule:
// a task some planned job waits for, by a dependency or by write
// serialization, is restored with its files even in minimal mode, so a later
// job that executes finds its inputs. Its own key, a leaf, stays result-only.
func TestResultOnlyRestoresTheFilesOfATaskALaterJobWaitsFor(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "result-only-remote-restore",
		"a-hit-a-planned-job-waits-for-keeps-its-files")
	cases := map[string]func(later, task *ScheduledJob){
		"depends on":       func(later, task *ScheduledJob) { later.DependsOn = []string{task.Key()} },
		"serialized after": func(later, task *ScheduledJob) { later.SerializeAfter = []string{task.Key()} },
	}
	for name, wait := range cases {
		t.Run(name, func(t *testing.T) {
			server := newFakeCacheServer()
			server.echoResultOnly()
			hash, want := publishMatrixEntry(t, server)

			consumer, remote := resultOnlyConsumer(t, server, cache.ModeMinimal)
			job := matrixJob()
			later := declaredJob("test~check", "check", &extension.TaskDeclaration{})
			wait(later, job)
			planned := []*ScheduledJob{job, later}
			keys, err := PrecomputeKeys(consumer.ws, planned, nil, nil, consumer.cache, CacheBypass{})
			if err != nil || keys[later.Key()] == "" {
				t.Fatalf("precompute keys: %v (keys %v)", err, keys)
			}
			remote.Negotiate(t.Context(), consumer.ws, planned, nil, nil, consumer.cache, CacheBypass{})
			wantResultOnly := []string{store.RemoteTaskEntryKey(keys[later.Key()])}
			if got := server.prefetchedResultOnly(); !slices.Equal(got, wantResultOnly) {
				t.Fatalf("result-only prefetch keys = %v, want only the later job's %v", got, wantResultOnly)
			}

			placed := server.placedBlobs()
			result := consumer.runDeclaredLookup(t, job, hash)
			if result == nil || result.Reuse != ReuseRemoteCache {
				t.Fatalf("lookup = %+v, want a remote-cache hit", result)
			}
			if server.placedBlobs() == placed {
				t.Fatal("a task a later job waits for was restored without its blobs")
			}
			assertSameTree(t, "waited-for hit", want, declaredTree(t, consumer))
			if consumer.cache.LookupResultOnlyTaskEntry(hash) != nil {
				t.Fatal("a full restore recorded a result-only entry")
			}
		})
	}
}

// TestRunThatReadsTheFilesNeverServesAResultOnlyEntry pins the two later runs
// of a workspace that holds a local result-only entry: one that reads the
// files restores the full entry from the provider, and executes the task when
// that restore fails. The result-only entry serves neither.
func TestRunThatReadsTheFilesNeverServesAResultOnlyEntry(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "result-only-remote-restore",
		"a-run-that-reads-the-files-restores-them-or-executes")
	cases := []struct {
		name         string
		breakRestore func(t *testing.T, server *fakeCacheServer, hash string)
	}{
		{name: "the full restore succeeds"},
		{
			name: "the full restore fails",
			breakRestore: func(t *testing.T, server *fakeCacheServer, hash string) {
				server.drop(payloadDigestFor(t, server, store.RemoteTaskEntryKey(hash), "lib/index.js"))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newFakeCacheServer()
			server.echoResultOnly()
			hash, want := publishMatrixEntry(t, server)

			consumer, remote := resultOnlyConsumer(t, server, cache.ModeMinimal)
			job := matrixJob()
			remote.Negotiate(t.Context(), consumer.ws, []*ScheduledJob{job}, nil, nil, consumer.cache, CacheBypass{})
			if result := consumer.runDeclaredLookup(t, job, hash); result == nil || result.Reuse != ReuseRemoteCache {
				t.Fatalf("minimal run = %+v, want a remote-cache hit", result)
			}
			if consumer.cache.LookupResultOnlyTaskEntry(hash) == nil {
				t.Fatal("the minimal run recorded no result-only entry")
			}
			if tc.breakRestore != nil {
				tc.breakRestore(t, server, hash)
			}

			full := attachRemote(t, consumer, cache.ModeFull)
			resultOnlyBefore := server.prefetchedResultOnly()
			full.Negotiate(t.Context(), consumer.ws, []*ScheduledJob{job}, nil, nil, consumer.cache, CacheBypass{})
			if got := server.prefetchedResultOnly(); !slices.Equal(got, resultOnlyBefore) {
				t.Fatalf("a full run named result-only keys: %v", got)
			}
			result := consumer.runDeclaredLookup(t, job, hash)
			if tc.breakRestore == nil {
				if result == nil || result.Reuse != ReuseRemoteCache {
					t.Fatalf("full run = %+v, want a remote-cache hit restored with its files", result)
				}
				assertSameTree(t, "full run", want, declaredTree(t, consumer))
				if entry, err := consumer.cache.LookupTaskEntry(hash); err != nil || entry == nil {
					t.Fatalf("the full restore published no task-owned entry: %+v (err=%v)", entry, err)
				}
				return
			}
			if result != nil {
				t.Fatalf("full run = %+v, want a miss that executes the task", result)
			}
			if entry, err := consumer.cache.LookupTaskEntry(hash); err != nil || entry != nil {
				t.Fatalf("a failed restore published a task-owned entry: %+v (err=%v)", entry, err)
			}
		})
	}
}

// TestAFailedFullRestoreNeverFallsBackToTheResultOnlyEntry pins the lookup
// order: a result-only entry serves only when no full local entry exists. When
// the store holds both and the full entry's restore fails partway, the run
// keeps the full fallback, the provider's full restore or an execution, which
// rewrites every output; it never reports the status alone over a half-written
// tree.
func TestAFailedFullRestoreNeverFallsBackToTheResultOnlyEntry(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "result-only-remote-restore",
		"a-run-that-reads-the-files-restores-them-or-executes")
	server := newFakeCacheServer()
	server.echoResultOnly()
	hash, _ := publishMatrixEntry(t, server)

	consumer, remote := resultOnlyConsumer(t, server, cache.ModeMinimal)
	job := matrixJob()
	remote.Negotiate(t.Context(), consumer.ws, []*ScheduledJob{job}, nil, nil, consumer.cache, CacheBypass{})
	if result := consumer.runDeclaredLookup(t, job, hash); result == nil || result.Reuse != ReuseRemoteCache {
		t.Fatalf("minimal run = %+v, want a remote-cache hit", result)
	}
	full := attachRemote(t, consumer, cache.ModeFull)
	full.Negotiate(t.Context(), consumer.ws, []*ScheduledJob{job}, nil, nil, consumer.cache, CacheBypass{})
	if result := consumer.runDeclaredLookup(t, job, hash); result == nil || result.Reuse != ReuseRemoteCache {
		t.Fatalf("full run = %+v, want a remote-cache hit restored with its files", result)
	}
	entry, err := consumer.cache.LookupTaskEntry(hash)
	if err != nil || entry == nil || consumer.cache.LookupResultOnlyTaskEntry(hash) == nil {
		t.Fatalf("the store does not hold both entries: full %+v (err=%v)", entry, err)
	}
	// Outputs restore in id order, so removing the last one's payload fails the
	// restore after the earlier outputs were swapped in.
	if err := os.Remove(filepath.Join(entry.FilesDir, "lock")); err != nil {
		t.Fatalf("break the full entry: %v", err)
	}

	resultOnlyRestores := func() int {
		count := 0
		for _, line := range server.requestLines() {
			if strings.Contains(line, `"op":"restore"`) && strings.Contains(line, `"resultOnly":true`) {
				count++
			}
		}
		return count
	}
	// A publish task always executes, so the rerun starts the provider and
	// learns its echo even though the matrix task is a local hit.
	rerun := attachRemote(t, consumer, cache.ModeMinimal)
	publisher := declaredJob("publish~ship", "ship", &extension.TaskDeclaration{})
	rerun.Negotiate(t.Context(), consumer.ws, []*ScheduledJob{job, publisher}, nil, nil, consumer.cache, CacheBypass{})
	if rerun.taskFilesNeeded(job) {
		t.Fatal("the rerun needs the files, so it cannot tell the two lookup orders apart")
	}
	// The full fallback finds the same local entry, fails the same way, and
	// leaves the task to execute.
	resultOnly := resultOnlyRestores()
	if result := consumer.runDeclaredLookup(t, job, hash); result != nil {
		t.Fatalf("a failed full restore = %+v, want a miss that executes the task", result)
	}
	if got := resultOnlyRestores(); got != resultOnly {
		t.Fatal("the fallback asked the provider for the result only")
	}
}

// TestProviderWithoutTheEchoKeepsTheFullRestore pins the negotiation: a
// provider that does not echo the capability receives neither new field, and
// a minimal run restores every hit with its files exactly as before.
func TestProviderWithoutTheEchoKeepsTheFullRestore(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "result-only-remote-restore",
		"a-provider-without-the-capability-sees-no-new-field")
	server := newFakeCacheServer()
	hash, want := publishMatrixEntry(t, server)

	consumer, remote := resultOnlyConsumer(t, server, cache.ModeMinimal)
	job := matrixJob()
	remote.Negotiate(t.Context(), consumer.ws, []*ScheduledJob{job}, nil, nil, consumer.cache, CacheBypass{})
	result := consumer.runDeclaredLookup(t, job, hash)
	if result == nil || result.Reuse != ReuseRemoteCache {
		t.Fatalf("lookup = %+v, want a remote-cache hit", result)
	}
	assertSameTree(t, "no echo", want, declaredTree(t, consumer))
	if consumer.cache.LookupResultOnlyTaskEntry(hash) != nil {
		t.Fatal("a provider without the echo produced a result-only entry")
	}
	for _, line := range server.requestLines() {
		if strings.Contains(line, `"resultOnly`) {
			t.Fatalf("a provider without the echo received a result-only field: %s", line)
		}
	}
	if server.requestCount(cache.OpPrefetch) == 0 || server.requestCount(cache.OpRestore) == 0 {
		t.Fatal("the run never prefetched or restored, so the wire check proves nothing")
	}
}

// ---------------------------------------------------------------------------
// a whole run
// ---------------------------------------------------------------------------

// resultOnlyRun is a workspace with two declared-capture /bin/sh tasks of one
// project: build writes out.txt into the project, and check, which depends on
// build, fails unless out.txt holds what build wrote.
type resultOnlyRun struct {
	ws      *workspace.Workspace
	root    string
	project *workspace.Project
	ext     *extension.ExtensionDescription
	build   string
	check   string
}

func newResultOnlyRun(t *testing.T, server *fakeCacheServer) *resultOnlyRun {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable")
	}
	enableProviderRemoteCache(t)
	useSharedFakeProvider(t, server)

	root := t.TempDir()
	project := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
	if err := os.MkdirAll(filepath.Join(root, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, root, "app", "src.txt", "one")
	result := `printf '%s\n' '{"v":2,"type":"result","data":{"status":"success"}}'` + "\n"
	build := filepath.Join(root, "build.sh")
	writeExecutable(t, build, "#!/bin/sh\nprintf 'built\\n' > out.txt\n"+result)
	check := filepath.Join(root, "check.sh")
	writeExecutable(t, check, "#!/bin/sh\n[ \"$(cat out.txt 2>/dev/null)\" = built ] || exit 1\n"+result)
	ext := &extension.ExtensionDescription{
		Name:    "@putnami/test",
		Version: "1.0.0",
		Path:    filepath.Join(root, "extension"),
		Tasks: map[string]extension.TaskDefinition{
			"build-run": {Declares: &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
				"out": fileDeclaration(extension.OutputRootProject, "out.txt", false),
			}}},
			"check-run": {Declares: &extension.TaskDeclaration{}},
		},
	}
	return &resultOnlyRun{
		ws:      testWorkspace(root, project),
		root:    root,
		project: project,
		ext:     ext,
		build:   build,
		check:   check,
	}
}

// jobs returns a fresh plan: build, and check when withCheck is set.
func (f *resultOnlyRun) jobs(withCheck bool) []*ScheduledJob {
	build := &ScheduledJob{
		Project:   f.project,
		Extension: f.ext,
		Step:      &extension.PipelineStep{ID: "run", Task: "build-run"},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@putnami/test",
			Name:          "build",
			CommandName:   "build",
			Command:       f.build,
			Cwd:           "{projectRoot}",
			Cache:         true,
			FilePatterns:  []string{"src.txt"},
			TimeoutMs:     unboundedJobTimeoutMs,
		},
	}
	if !withCheck {
		return []*ScheduledJob{build}
	}
	check := &ScheduledJob{
		Project:   f.project,
		Extension: f.ext,
		Step:      &extension.PipelineStep{ID: "run", Task: "check-run"},
		JobDef: &extension.JobDefinition{
			ExtensionName:   "@putnami/test",
			Name:            "check",
			CommandName:     "check",
			Command:         f.check,
			Cwd:             "{projectRoot}",
			Cache:           true,
			FilePatterns:    []string{"src.txt"},
			TimeoutMs:       unboundedJobTimeoutMs,
			TaskCachePolicy: &extension.TaskCachePolicy{Deterministic: true, NoOutput: true},
		},
		DependsOn: []string{build.Key()},
	}
	return []*ScheduledJob{build, check}
}

// run runs planned in mode against the store at storeDir, through a new
// provider session and the object-cache fixture's scheduler runner.
func (f *resultOnlyRun) run(t *testing.T, mode cache.Mode, storeDir string, planned []*ScheduledJob) (*SchedulerResult, *store.CacheManager) {
	t.Helper()
	t.Setenv(cacheModeEnv, string(mode))
	cm := store.NewCacheManager(store.NewLocalStore(storeDir))
	remote, notice := LoadRemoteCache(context.Background(), f.ws.Root,
		[]*extension.ExtensionDescription{fakeProviderExtension(t, nil)}, nil, store.CacheTrustAny)
	if remote == nil || !remote.isProviderBacked() {
		t.Fatalf("expected a provider-backed remote cache, got %+v (notice %q)", remote, notice)
	}
	t.Cleanup(remote.Close)
	runner := &objectCacheFixture{ws: f.ws, planned: planned, cache: cm, remote: remote}
	return runner.run(t, SchedulerConfig{MaxParallel: 1}), cm
}

// runOutcome renders each job's status, reuse and error, in key order.
func runOutcome(result *SchedulerResult) string {
	keys := make([]string, 0, len(result.Results))
	for key := range result.Results {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var lines []string
	for _, key := range keys {
		job := result.Results[key]
		line := key + ": " + job.Status + " reuse=" + string(job.Reuse)
		if job.Error != nil {
			line += " error=" + job.Error.Message
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "; ")
}

func (f *resultOnlyRun) output() string {
	return filepath.Join(f.root, "app", "out.txt")
}

// TestMinimalRunServesEveryHitWithoutItsFiles drives whole runs. A minimal run
// whose every job is a hit nothing reads passes with no blob placed and the
// workspace untouched. A later minimal run that plans a job reading those
// files restores them from the provider, though the store holds a result-only
// entry for the key, and that job executes against them.
func TestMinimalRunServesEveryHitWithoutItsFiles(t *testing.T) {
	for _, check := range []string{
		"a-hit-no-job-waits-for-moves-no-bytes",
		"a-hit-a-planned-job-waits-for-keeps-its-files",
	} {
		spectest.Proves(t, "cli/job-planning-execution", "result-only-remote-restore", check)
	}
	server := newFakeCacheServer()
	server.echoResultOnly()
	f := newResultOnlyRun(t, server)

	produced, producerStore := f.run(t, cache.ModeFull, filepath.Join(f.root, "producer-store"), f.jobs(true))
	if !produced.Success {
		t.Fatalf("producer run failed: %s", runOutcome(produced))
	}
	planned := f.jobs(true)
	keys, err := PrecomputeKeys(f.ws, planned, nil, nil, producerStore, CacheBypass{})
	if err != nil {
		t.Fatalf("precompute keys: %v", err)
	}
	buildKey, checkKey := keys[planned[0].Key()], keys[planned[1].Key()]
	for _, key := range []string{buildKey, checkKey} {
		if key == "" || !server.has(store.RemoteTaskEntryKey(key)) {
			t.Fatalf("the producer run shared no entry for %q", key)
		}
	}
	if err := os.Remove(f.output()); err != nil {
		t.Fatal(err)
	}

	consumerStore := filepath.Join(f.root, "consumer-store")
	placed := server.placedBlobs()
	minimal, cm := f.run(t, cache.ModeMinimal, consumerStore, f.jobs(false))
	if !minimal.Success {
		t.Fatalf("minimal run failed: %s", runOutcome(minimal))
	}
	if got := minimal.Results[planned[0].Key()]; got == nil || got.Reuse != ReuseRemoteCache {
		t.Fatalf("build = %+v, want a remote-cache hit", got)
	}
	if got := server.placedBlobs() - placed; got != 0 {
		t.Fatalf("a minimal run of hits nothing reads placed %d blobs", got)
	}
	if _, err := os.Lstat(f.output()); !os.IsNotExist(err) {
		t.Fatalf("a status-only hit wrote %s (err=%v)", f.output(), err)
	}
	if minimal.Cache == nil || minimal.Cache.BytesFetched != 0 || minimal.Cache.Hits != 1 {
		t.Fatalf("cache stats = %+v, want one provider hit with zero fetched bytes", minimal.Cache)
	}
	if cm.LookupResultOnlyTaskEntry(buildKey) == nil {
		t.Fatal("the minimal run recorded no result-only entry")
	}

	// check must execute, so the provider forgets its entry.
	server.forgetEntry(store.RemoteTaskEntryKey(checkKey))
	placed = server.placedBlobs()
	reader, _ := f.run(t, cache.ModeMinimal, consumerStore, f.jobs(true))
	if !reader.Success {
		t.Fatalf("a run whose executing job reads a hit's files failed: %s", runOutcome(reader))
	}
	if got := reader.Results[planned[0].Key()]; got == nil || got.Reuse != ReuseRemoteCache {
		t.Fatalf("build = %+v, want a remote-cache hit restored with its files", got)
	}
	if got := reader.Results[planned[1].Key()]; got == nil || got.Reuse != ReuseNone {
		t.Fatalf("check = %+v, want an execution", got)
	}
	if server.placedBlobs() == placed {
		t.Fatal("the files a job reads were not fetched")
	}
	if got := readFileAt(t, f.output()); got != "built\n" {
		t.Fatalf("%s = %q, want the restored bytes", f.output(), got)
	}
}

// ---------------------------------------------------------------------------
// the rule
// ---------------------------------------------------------------------------

// initializedSession returns a session that completed initialize against the
// fake provider, which echoes cache.CapabilityRestoreResultOnly when echo is
// set. The session is the only record of the echo.
func initializedSession(t *testing.T, mode cache.Mode, echo bool) *cacheprovider.Session {
	t.Helper()
	server := newFakeCacheServer()
	if echo {
		server.echoResultOnly()
	}
	reqR, reqW := io.Pipe()
	respR, respW := io.Pipe()
	go func() {
		serveSharedFakeProvider(reqR, respW, server)
		_ = respW.Close()
		_ = reqR.Close()
	}()
	sess := cacheprovider.Connect(reqW, respR)
	t.Cleanup(func() { _ = sess.Close() })
	if _, err := sess.Initialize(t.Context(), &cache.InitializeParams{
		ProtocolVersion: cache.ProviderProtocolVersion,
		BlobExchangeDir: t.TempDir(),
		Mode:            mode,
	}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if got := sess.RestoreResultOnly(); got != echo {
		t.Fatalf("session echo = %v, want %v", got, echo)
	}
	return sess
}

func resultOnlyRemote(t *testing.T, mode cache.Mode, echo bool, dependedOn ...string) *RemoteCache {
	t.Helper()
	waited := map[string]bool{}
	for _, key := range dependedOn {
		waited[key] = true
	}
	return &RemoteCache{
		provider:   &providerRemote{mode: mode, ready: true, session: initializedSession(t, mode, echo)},
		dependedOn: waited,
	}
}

func selectedJob(selected ...string) *ScheduledJob {
	job := matrixJob()
	job.Selection = &protocoljob.Selection{ProjectIDs: selected}
	return job
}

func TestTaskFilesNeeded(t *testing.T) {
	leaf := selectedJob("/other")
	cases := []struct {
		name   string
		remote *RemoteCache
		job    *ScheduledJob
		want   bool
	}{
		{"no remote cache", nil, leaf, true},
		{"provider without the echo", resultOnlyRemote(t, cache.ModeMinimal, false), leaf, true},
		{"plan never negotiated", &RemoteCache{provider: &providerRemote{mode: cache.ModeMinimal, ready: true, session: initializedSession(t, cache.ModeMinimal, true)}}, leaf, true},
		{"provider not started", &RemoteCache{provider: &providerRemote{mode: cache.ModeMinimal}, dependedOn: map[string]bool{}}, leaf, true},
		{"provider not ready", &RemoteCache{provider: &providerRemote{mode: cache.ModeMinimal, session: initializedSession(t, cache.ModeMinimal, true)}, dependedOn: map[string]bool{}}, leaf, true},
		{"full mode", resultOnlyRemote(t, cache.ModeFull, true), leaf, true},
		{"minimal mode, nothing waits", resultOnlyRemote(t, cache.ModeMinimal, true), leaf, false},
		{"minimal mode, a job waits", resultOnlyRemote(t, cache.ModeMinimal, true, leaf.Key()), leaf, true},
		{"toplevel mode, project not selected", resultOnlyRemote(t, cache.ModeToplevel, true), leaf, false},
		{"toplevel mode, project selected", resultOnlyRemote(t, cache.ModeToplevel, true), selectedJob("/" + captureTestProject), true},
		{"toplevel mode, no selection", resultOnlyRemote(t, cache.ModeToplevel, true), matrixJob(), true},
		{"toplevel mode, workspace-once job", resultOnlyRemote(t, cache.ModeToplevel, true), func() *ScheduledJob {
			job := selectedJob("/other")
			job.SelectedProjects = []*workspace.Project{{ID: "/other"}}
			return job
		}(), true},
		{"minimal mode, post-run report", resultOnlyRemote(t, cache.ModeMinimal, true), declaredJob("test~verify", "verify",
			&extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
				"report": fileDeclaration(extension.OutputRootCommandOutput, features.VerificationReportFilename, true),
			}}), true},
		{"minimal mode, policed drift", resultOnlyRemote(t, cache.ModeMinimal, true), func() *ScheduledJob {
			output := dirDeclaration(extension.OutputRootProject, "client", false)
			output.Drift = extension.OutputDriftFail
			return declaredJob("generate~client", "client",
				&extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{"client": output}})
		}(), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.remote.taskFilesNeeded(tc.job); got != tc.want {
				t.Fatalf("taskFilesNeeded = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDeclaresPostRunReport(t *testing.T) {
	commandOutputFile := func(path string) extension.DeclaredOutput {
		return fileDeclaration(extension.OutputRootCommandOutput, path, true)
	}
	runTimeFile := extension.DeclaredOutput{Kind: extension.OutputKindFile, Root: extension.OutputRootCommandOutput, PathFrom: "report"}
	cases := map[string]struct {
		output extension.DeclaredOutput
		want   bool
	}{
		"verification report":          {commandOutputFile(features.VerificationReportFilename), true},
		"criteria projection":          {commandOutputFile(features.SpecCriteriaProjectionFilename), true},
		"command-output run-time file": {runTimeFile, true},
		"command-output directory":     {dirDeclaration(extension.OutputRootCommandOutput, "bin", false), false},
		"other command-output file":    {commandOutputFile("package.tgz"), false},
		"report name in the project":   {fileDeclaration(extension.OutputRootProject, features.VerificationReportFilename, true), false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			job := declaredJob("test~task", "task", &extension.TaskDeclaration{
				Outputs: map[string]extension.DeclaredOutput{"out": tc.output},
			})
			if got := declaresPostRunReport(job); got != tc.want {
				t.Fatalf("declaresPostRunReport = %v, want %v", got, tc.want)
			}
		})
	}
	if declaresPostRunReport(declaredJob("test~task", "task", nil)) {
		t.Fatal("a job without a declaration declares no report")
	}
}

// TestNegotiateNamesAKeyResultOnlyOnlyWhenNoJobSharingItReadsTheFiles pins the
// subset rule: a key two planned jobs restore is result-only only when neither
// reads the files.
func TestNegotiateNamesAKeyResultOnlyOnlyWhenNoJobSharingItReadsTheFiles(t *testing.T) {
	remote := resultOnlyRemote(t, cache.ModeMinimal, true, "/proj:reader")
	leaf := matrixJob()
	waited := declaredJob("reader", "reader", &extension.TaskDeclaration{})
	declared := map[string][]*ScheduledJob{
		"k-leaf":   {leaf},
		"k-shared": {leaf, waited},
		"k-waited": {waited},
	}
	got := remote.resultOnlyKeys([]string{"k-waited", "k-leaf", "k-shared", "k-leaf", "k-legacy"}, declared)
	if want := []string{"k-leaf"}; !slices.Equal(got, want) {
		t.Fatalf("resultOnlyKeys = %v, want %v", got, want)
	}
}

// TestStatusOnlyHitRefusesASourceMutatingResult pins the source-rewriter rule
// on the status-only path: recorded edits must reach the worktree, so the
// result cannot serve without them.
func TestStatusOnlyHitRefusesASourceMutatingResult(t *testing.T) {
	sources := extension.ResourceRef{ID: sourcesResourceID, Scope: extension.ResourceScopeProject}
	job := declaredJob("lint~fix", "fix", &extension.TaskDeclaration{MutatesSources: true}, sources)
	f := newCaptureFixture(t, job)
	var mu sync.Mutex
	hashes := map[string]string{}
	mutated := &store.ResultOnlyTaskEntry{
		Key:    captureHashA,
		Result: entryResultFromJobResult(&JobResult{Status: "success", SourceMutated: true}),
	}
	if result := f.sched.serveStatusOnlyHit(job, captureHashA, mutated, ReuseRemoteCache, &mu, hashes); result != nil {
		t.Fatalf("a source-mutating result served as a status-only hit: %+v", result)
	}
	if _, ok := hashes[job.Key()]; ok {
		t.Fatal("a refused hit recorded the task's key for its dependents")
	}

	clean := &store.ResultOnlyTaskEntry{Key: captureHashA, Result: &store.EntryResult{Status: "success"}}
	result := f.sched.serveStatusOnlyHit(job, captureHashA, clean, ReuseRemoteCache, &mu, hashes)
	if result == nil || result.Reuse != ReuseRemoteCache || hashes[job.Key()] != captureHashA {
		t.Fatalf("a clean result = %+v (hashes %v), want a remote-cache hit keyed for dependents", result, hashes)
	}
}
