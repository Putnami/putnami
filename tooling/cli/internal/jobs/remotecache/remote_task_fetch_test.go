package remotecache

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	jobmodel "go.putnami.dev/cli/model/jobs"
	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/tooling/cli/internal/cacheprovider"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
)

// fetchTestKey is the cache key of the fixture task's entry.
const fetchTestKey = "fetch-test-key"

// fetchTestReport is the bytes of the fixture task's one declared output.
const fetchTestReport = `{"protocolVersion":1,"observations":[]}`

// fetchTestOutput is the fixture task's one declared output, resolved the way
// the executor hands it to the store's ingest.
var fetchTestOutput = store.DeclaredEntryOutput{
	ID:   "report",
	Kind: extension.OutputKindFile,
	Root: extension.OutputRootCommandOutput,
	Path: "report.json",
}

// fetchTestJob is a task that declares fetchTestOutput.
func fetchTestJob() *jobmodel.ScheduledJob {
	return declaredJob("test~test", "test", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			fetchTestOutput.ID: fileDeclaration(extension.OutputRootCommandOutput, fetchTestOutput.Path, false),
		},
	})
}

// useSharedFakeProviderSessions routes every provider start of this test to a
// session of server, served in process, and returns the contexts the starts
// ran under.
func useSharedFakeProviderSessions(t *testing.T, server *fakeCacheServer) func() []context.Context {
	t.Helper()
	var mu sync.Mutex
	var started []context.Context
	orig := SpawnProviderSession
	SpawnProviderSession = func(ctx context.Context, _ cacheprovider.LaunchSpec, opts ...cacheprovider.Option) (*cacheprovider.Session, error) {
		mu.Lock()
		started = append(started, ctx)
		mu.Unlock()
		reqR, reqW := io.Pipe()
		respR, respW := io.Pipe()
		go func() {
			serveSharedFakeProvider(reqR, respW, server)
			_ = respW.Close()
			_ = reqR.Close()
		}()
		return cacheprovider.Connect(reqW, respR, opts...), nil
	}
	t.Cleanup(func() { SpawnProviderSession = orig })
	return func() []context.Context {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(started)
	}
}

// newFetchTestRemote is a provider-backed remote cache whose provider has not
// started yet.
func newFetchTestRemote(t *testing.T, trust store.CacheTrust) *RemoteCache {
	t.Helper()
	remote := newProviderRemoteCache(t.TempDir(), cacheprovider.LaunchSpec{Command: "fake-cache-provider"}, trust)
	t.Cleanup(remote.Close)
	return remote
}

func newFetchTestStore(t *testing.T) *store.CacheManager {
	t.Helper()
	return store.NewCacheManager(store.NewLocalStore(t.TempDir()))
}

// uploadFetchTestEntry publishes the fixture task's entry in a producer store
// of its own and uploads it to server, as an earlier run on another machine
// does.
func uploadFetchTestEntry(t *testing.T) {
	t.Helper()
	producer := newFetchTestStore(t)
	staging := t.TempDir()
	path := store.TaskStagingPath(staging, fetchTestOutput)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(fetchTestReport), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := producer.IngestTaskEntry(staging, store.TaskEntrySpec{
		Key:      fetchTestKey,
		Result:   &store.EntryResult{Status: "success"},
		Metadata: &store.EntryMetadata{DurationMs: 4000},
		Outputs:  []store.DeclaredEntryOutput{fetchTestOutput},
	}); err != nil {
		t.Fatalf("ingest the producer entry: %v", err)
	}
	remote := newFetchTestRemote(t, store.CacheTrustAny)
	remote.UploadTaskEntry(t.Context(), fetchTestKey, fetchTestJob(), producer)
	remote.DrainUploads()
}

// TestFetchTaskEntryPublishesWithoutCountingAReuse pins the fetch a reader of
// the store makes for a task the run did not plan: the entry lands in the
// local store with its declared output, the run's cache stats count the bytes
// and the materialize time but no provider hit, restore or time saved, and the
// provider started under the caller's context, which outlives the lookup's
// bound. RestoreTaskEntry, for a planned job, counts the same hit as a reuse.
func TestFetchTaskEntryPublishesWithoutCountingAReuse(t *testing.T) {
	server := newFakeCacheServer()
	started := useSharedFakeProviderSessions(t, server)
	uploadFetchTestEntry(t)

	consumer := newFetchTestStore(t)
	remote := newFetchTestRemote(t, store.CacheTrustAny)
	entry, err := remote.FetchTaskEntry(t.Context(), time.Minute, fetchTestKey, fetchTestJob(), consumer)
	if err != nil || entry == nil {
		t.Fatalf("FetchTaskEntry = %v, %v, want the uploaded entry", entry, err)
	}
	local, err := consumer.LookupTaskEntry(fetchTestKey)
	if err != nil || local == nil {
		t.Fatalf("the fetched entry is not in the local store (entry %v, err %v)", local, err)
	}
	if got, err := os.ReadFile(filepath.Join(local.FilesDir, fetchTestOutput.ID)); err != nil || string(got) != fetchTestReport {
		t.Fatalf("fetched output = %q (err %v), want the uploaded bytes", got, err)
	}
	stats := remote.Stats()
	if stats.Hits != 0 || stats.Restored != 0 || stats.TimeSavedMs != 0 || stats.HintsWarmed != 0 {
		t.Errorf("a fetch for a reader of the store counted a task reuse: %+v", stats)
	}
	server.mu.Lock()
	wantBytes := ManifestBytes(server.entries[store.RemoteTaskEntryKey(fetchTestKey)].manifest)
	server.mu.Unlock()
	if wantBytes <= int64(len(fetchTestReport)) || stats.BytesFetched != wantBytes {
		t.Errorf("bytes fetched = %d, want the %d bytes of the served manifest", stats.BytesFetched, wantBytes)
	}
	if countRestoreRequests(server) != 1 {
		t.Errorf("restore requests = %d, want the fetch's one", countRestoreRequests(server))
	}
	for i, ctx := range started() {
		if ctx.Err() != nil {
			t.Errorf("provider start %d ran under a context that ended with the lookup: %v", i, ctx.Err())
		}
	}

	control := newFetchTestRemote(t, store.CacheTrustAny)
	if restored, reuse := control.RestoreTaskEntry(t.Context(), fetchTestKey, fetchTestJob(), newFetchTestStore(t)); restored == nil ||
		reuse != jobmodel.ReuseRemoteCache {
		t.Fatalf("RestoreTaskEntry = %v, %s, want the remote hit", restored, reuse)
	}
	if stats := control.Stats(); stats.Hits != 1 || stats.Restored != 1 || stats.TimeSavedMs != 4000 {
		t.Errorf("a restore for a planned job = %+v, want one hit, one restore and its 4s saved", stats)
	}
}

// TestFetchTaskEntryAnswersAMissAndAnUntrustedHitWithNoEntry pins the two
// answers that are not failures: the provider has no entry, or its entry is a
// hint the trust policy refuses. Neither publishes nor warms anything.
func TestFetchTaskEntryAnswersAMissAndAnUntrustedHitWithNoEntry(t *testing.T) {
	server := newFakeCacheServer()
	useSharedFakeProviderSessions(t, server)

	consumer := newFetchTestStore(t)
	remote := newFetchTestRemote(t, store.CacheTrustCI)
	if entry, err := remote.FetchTaskEntry(t.Context(), time.Minute, fetchTestKey, fetchTestJob(), consumer); entry != nil || err != nil {
		t.Fatalf("a provider miss = %v, %v, want no entry and no error", entry, err)
	}

	uploadFetchTestEntry(t)
	server.mu.Lock()
	server.channel = cache.ChannelHint
	server.mu.Unlock()
	if entry, err := remote.FetchTaskEntry(t.Context(), time.Minute, fetchTestKey, fetchTestJob(), consumer); entry != nil || err != nil {
		t.Fatalf("an untrusted hit = %v, %v, want no entry and no error", entry, err)
	}
	if local, err := consumer.LookupTaskEntry(fetchTestKey); err != nil || local != nil {
		t.Fatalf("an untrusted hit published a local entry (entry %v, err %v)", local, err)
	}
	if stats := remote.Stats(); stats.HintsWarmed != 0 || stats.BytesFetched != 0 || stats.Hits != 0 {
		t.Errorf("an untrusted hit for a reader of the store warmed or counted something: %+v", stats)
	}
}

// TestFetchTaskEntryEndsAtItsBound pins the bound: a lease another process
// holds on the entry, or a provider that never answers, keeps the caller no
// longer than within, and the error says the lookup ran out of time. A lookup
// whose bound passed while it waited on the lease sends the provider nothing.
func TestFetchTaskEntryEndsAtItsBound(t *testing.T) {
	const within = 300 * time.Millisecond

	t.Run("a held lease", func(t *testing.T) {
		server := newFakeCacheServer()
		useSharedFakeProviderSessions(t, server)
		uploadFetchTestEntry(t)
		consumer := newFetchTestStore(t)
		winner, release := consumer.TryClaimTaskEntry(fetchTestKey, 0)
		if !winner {
			t.Fatal("the test could not hold the entry's lease")
		}
		defer release()
		remote := newFetchTestRemote(t, store.CacheTrustAny)
		requestsBefore := countRestoreRequests(server)

		start := time.Now()
		entry, err := remote.FetchTaskEntry(t.Context(), within, fetchTestKey, fetchTestJob(), consumer)
		if entry != nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("FetchTaskEntry behind a held lease = %v, %v, want a deadline error", entry, err)
		}
		if elapsed := time.Since(start); elapsed > 10*within {
			t.Errorf("the lookup took %s, want about %s", elapsed, within)
		}
		if got := countRestoreRequests(server); got != requestsBefore {
			t.Errorf("a lookup past its bound sent %d restore request(s)", got-requestsBefore)
		}
	})

	t.Run("a provider that never answers", func(t *testing.T) {
		useSilentRestoreProvider(t, true)
		remote := newFetchTestRemote(t, store.CacheTrustAny)
		start := time.Now()
		entry, err := remote.FetchTaskEntry(t.Context(), within, fetchTestKey, fetchTestJob(), newFetchTestStore(t))
		if entry != nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("FetchTaskEntry against a silent provider = %v, %v, want a deadline error", entry, err)
		}
		if elapsed := time.Since(start); elapsed > 10*within {
			t.Errorf("the lookup took %s, want about %s", elapsed, within)
		}
	})
}

// TestFetchTaskEntryTellsAnUnconfiguredProviderFromAFailure pins that a
// provider serving nothing by configuration reads as no remote cache, with no
// error, while a closed provider is a failure the caller can name.
func TestFetchTaskEntryTellsAnUnconfiguredProviderFromAFailure(t *testing.T) {
	useSilentRestoreProvider(t, false)
	unconfigured := newFetchTestRemote(t, store.CacheTrustAny)
	if entry, err := unconfigured.FetchTaskEntry(t.Context(), time.Minute, fetchTestKey, fetchTestJob(), newFetchTestStore(t)); entry != nil ||
		err != nil {
		t.Fatalf("an unconfigured provider = %v, %v, want no entry and no error", entry, err)
	}

	closed := newFetchTestRemote(t, store.CacheTrustAny)
	closed.Close()
	if entry, err := closed.FetchTaskEntry(t.Context(), time.Minute, fetchTestKey, fetchTestJob(), newFetchTestStore(t)); entry != nil ||
		!errors.Is(err, errProviderClosed) {
		t.Fatalf("a closed provider = %v, %v, want %v", entry, err, errProviderClosed)
	}
}

// useSilentRestoreProvider routes every provider start of this test to an
// in-process provider that answers initialize with ready, and shutdown, and
// never answers any other request.
func useSilentRestoreProvider(t *testing.T, ready bool) {
	t.Helper()
	orig := SpawnProviderSession
	SpawnProviderSession = func(_ context.Context, _ cacheprovider.LaunchSpec, opts ...cacheprovider.Option) (*cacheprovider.Session, error) {
		reqR, reqW := io.Pipe()
		respR, respW := io.Pipe()
		go func() {
			serveSilentRestoreProvider(reqR, respW, ready)
			_ = respW.Close()
			_ = reqR.Close()
		}()
		return cacheprovider.Connect(reqW, respR, opts...), nil
	}
	t.Cleanup(func() { SpawnProviderSession = orig })
}

func serveSilentRestoreProvider(stdin io.Reader, stdout io.Writer, ready bool) {
	in := bufio.NewScanner(stdin)
	in.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	answer := func(req *cache.ProviderRequest, payload any) {
		raw, _ := cache.MarshalPayload(payload)
		line, _ := json.Marshal(&cache.ProviderResponse{ProtocolVersion: req.ProtocolVersion, ID: req.ID, OK: true, Payload: raw})
		_, _ = stdout.Write(append(line, '\n'))
	}
	for in.Scan() {
		req, _ := cache.ParseAndValidateProviderRequest(in.Bytes())
		if req == nil {
			continue
		}
		switch req.Op {
		case cache.OpInitialize:
			answer(req, &cache.InitializeResult{
				ProtocolVersion: req.ProtocolVersion,
				ProviderName:    fakeProviderExtensionName,
				ProviderVersion: fakeProviderVersion,
				Ready:           ready,
			})
		case cache.OpShutdown:
			answer(req, nil)
			return
		}
	}
}

// countRestoreRequests counts the restore requests server received.
func countRestoreRequests(server *fakeCacheServer) int {
	server.mu.Lock()
	defer server.mu.Unlock()
	count := 0
	for _, line := range server.requests {
		if strings.Contains(line, `"op":"`+string(cache.OpRestore)+`"`) {
			count++
		}
	}
	return count
}
