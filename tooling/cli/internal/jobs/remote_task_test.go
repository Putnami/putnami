package jobs

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/tooling/cli/internal/cacheprovider"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
)

// ---------------------------------------------------------------------------
// a real (in-process) remote cache: key -> entry, digest -> bytes
// ---------------------------------------------------------------------------

// fakeCacheServer is a shared cache the fake provider sessions talk to. Unlike
// the fixed-blob fake in remote_provider_test.go it actually STORES what one
// machine uploads and serves it to another, moving the bytes through each
// session's own blob exchange directory exactly as a real provider does. That is
// what makes the remote leg of the byte-equivalence matrix a real transfer
// rather than a mock.
type fakeCacheServer struct {
	mu       sync.Mutex
	entries  map[string]fakeCacheEntry
	blobs    map[string][]byte
	prefetch []string
	// dropped digests are accepted on upload but never served back, so a restore
	// succeeds only if the consumer already holds those bytes in its CAS. It is
	// how the mixed leg proves dedup instead of asserting it.
	dropped map[string]bool
	// channel is the provenance stamped on every hit (empty = legacy provider).
	channel cache.Channel
	// resultOnly makes initialize echo cache.CapabilityRestoreResultOnly when
	// core lists it; a result-only restore then places no blob.
	resultOnly bool
	// requests holds every raw request line, in arrival order.
	requests []string
	// placed counts the blobs restores wrote to an exchange directory.
	placed int
	// resultOnlyPrefetch holds every key a prefetch named result-only.
	resultOnlyPrefetch []string
}

type fakeCacheEntry struct {
	result   *cache.ActionResult
	manifest *cache.Manifest
}

func newFakeCacheServer() *fakeCacheServer {
	return &fakeCacheServer{
		entries: map[string]fakeCacheEntry{},
		blobs:   map[string][]byte{},
		dropped: map[string]bool{},
	}
}

func (f *fakeCacheServer) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.entries[key]
	return ok
}

func (f *fakeCacheServer) manifestOf(key string) *cache.Manifest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.entries[key].manifest
}

func (f *fakeCacheServer) prefetched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string{}, f.prefetch...)
	sort.Strings(out)
	return out
}

// echoResultOnly makes later sessions echo cache.CapabilityRestoreResultOnly.
func (f *fakeCacheServer) echoResultOnly() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resultOnly = true
}

func (f *fakeCacheServer) echoesResultOnly() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resultOnly
}

func (f *fakeCacheServer) recordRequest(line []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, string(line))
}

// requestLines returns every raw request line received, of any session.
func (f *fakeCacheServer) requestLines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.requests...)
}

// requestCount counts the requests of one operation received so far.
func (f *fakeCacheServer) requestCount(op cache.ProviderOp) int {
	count := 0
	for _, line := range f.requestLines() {
		var envelope struct {
			Op cache.ProviderOp `json:"op"`
		}
		if json.Unmarshal([]byte(line), &envelope) == nil && envelope.Op == op {
			count++
		}
	}
	return count
}

// placedBlobs counts the blobs restores wrote to an exchange directory.
func (f *fakeCacheServer) placedBlobs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.placed
}

func (f *fakeCacheServer) prefetchedResultOnly() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string{}, f.resultOnlyPrefetch...)
	sort.Strings(out)
	return out
}

func (f *fakeCacheServer) drop(digest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropped[digest] = true
}

// forgetEntry removes a key so a workspace can be forced back to a cold miss
// without disturbing the blobs already transferred.
func (f *fakeCacheServer) forgetEntry(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.entries, key)
}

func (f *fakeCacheServer) upload(key string, result *cache.ActionResult, manifest *cache.Manifest, exchangeDir string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, file := range manifest.Files {
		path, ok := cache.BlobExchangePath(exchangeDir, file.Digest)
		if !ok {
			return false
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return false // the client must stage every blob it names
		}
		f.blobs[file.Digest] = content
	}
	f.entries[key] = fakeCacheEntry{result: result, manifest: manifest}
	return true
}

func (f *fakeCacheServer) restore(key, exchangeDir string) (fakeCacheEntry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.entries[key]
	if !ok {
		return fakeCacheEntry{}, false
	}
	for _, file := range entry.manifest.Files {
		if f.dropped[file.Digest] {
			continue
		}
		content, ok := f.blobs[file.Digest]
		if !ok {
			continue
		}
		if path, ok := cache.BlobExchangePath(exchangeDir, file.Digest); ok {
			writeExchangeBlobAtomically(path, content)
			f.placed++
		}
	}
	return entry, true
}

// lookup answers a result-only restore: the entry, and no blob placed.
func (f *fakeCacheServer) lookup(key string) (fakeCacheEntry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.entries[key]
	return entry, ok
}

// useSharedFakeProvider routes every provider spawn in this test to a session
// backed by server, so two RemoteCache instances (two "machines") exchange real
// bytes through it.
func useSharedFakeProvider(t *testing.T, server *fakeCacheServer) {
	t.Helper()
	orig := spawnProviderSession
	spawnProviderSession = func(_ context.Context, _ cacheprovider.LaunchSpec, opts ...cacheprovider.Option) (*cacheprovider.Session, error) {
		reqR, reqW := io.Pipe()
		respR, respW := io.Pipe()
		go func() {
			serveSharedFakeProvider(reqR, respW, server)
			_ = respW.Close()
			_ = reqR.Close()
		}()
		return cacheprovider.Connect(reqW, respR, opts...), nil
	}
	t.Cleanup(func() { spawnProviderSession = orig })
}

func serveSharedFakeProvider(stdin io.Reader, stdout io.Writer, server *fakeCacheServer) {
	exchangeDir := ""
	in := bufio.NewScanner(stdin)
	in.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	write := func(resp *cache.ProviderResponse) {
		b, _ := json.Marshal(resp)
		_, _ = stdout.Write(append(b, '\n'))
	}
	ok := func(protocolVersion int, id int64, payload any) {
		raw, _ := cache.MarshalPayload(payload)
		write(&cache.ProviderResponse{ProtocolVersion: protocolVersion, ID: id, OK: true, Payload: raw})
	}

	for in.Scan() {
		server.recordRequest(in.Bytes())
		req, diags := cache.ParseAndValidateProviderRequest(in.Bytes())
		if req == nil {
			write(&cache.ProviderResponse{
				ProtocolVersion: cache.ProviderProtocolVersion,
				OK:              false,
				Error:           &cache.ProviderError{Code: "bad-request", Message: diagString(diags)},
			})
			continue
		}
		switch req.Op {
		case cache.OpInitialize:
			p, _ := cache.ParseAndValidateInitializeParams(req.Payload)
			negotiated := req.ProtocolVersion
			var capabilities []string
			if p != nil {
				exchangeDir = p.BlobExchangeDir
				if containsString(p.Capabilities, cache.CapabilityProviderProtocolV2) {
					negotiated = cache.ProviderProtocolVersion
				}
				if server.echoesResultOnly() && containsString(p.Capabilities, cache.CapabilityRestoreResultOnly) {
					capabilities = append(capabilities, cache.CapabilityRestoreResultOnly)
				}
			}
			ok(req.ProtocolVersion, req.ID, &cache.InitializeResult{
				ProtocolVersion: negotiated,
				ProviderName:    fakeProviderExtensionName,
				ProviderVersion: fakeProviderVersion,
				Ready:           true,
				Capabilities:    capabilities,
			})
		case cache.OpPrefetch:
			started := 0
			if p, _ := cache.ParseAndValidatePrefetchParams(req.Payload); p != nil {
				started = len(p.Keys)
				server.mu.Lock()
				server.prefetch = append(server.prefetch, p.Keys...)
				server.resultOnlyPrefetch = append(server.resultOnlyPrefetch, p.ResultOnlyKeys...)
				server.mu.Unlock()
			}
			ok(req.ProtocolVersion, req.ID, &cache.PrefetchResult{Started: started})
		case cache.OpRestore:
			p, _ := cache.ParseAndValidateRestoreParams(req.Payload)
			if p == nil {
				ok(req.ProtocolVersion, req.ID, &cache.RestoreResult{Status: cache.RestoreMiss})
				continue
			}
			var entry fakeCacheEntry
			var hit bool
			if p.ResultOnly {
				entry, hit = server.lookup(p.Key)
			} else {
				entry, hit = server.restore(p.Key, exchangeDir)
			}
			if !hit {
				ok(req.ProtocolVersion, req.ID, &cache.RestoreResult{Status: cache.RestoreMiss})
				continue
			}
			res := &cache.RestoreResult{Status: cache.RestoreHit, Result: entry.result, Manifest: entry.manifest}
			applyFakeRestoreProvenance(res, server.channel, req.ProtocolVersion)
			ok(req.ProtocolVersion, req.ID, res)
		case cache.OpUpload:
			accepted := false
			if p, _ := cache.ParseAndValidateUploadParams(req.Payload); p != nil && p.Manifest != nil {
				accepted = server.upload(p.Key, p.Result, p.Manifest, exchangeDir)
			}
			ok(req.ProtocolVersion, req.ID, &cache.UploadResult{Accepted: accepted})
		case cache.OpSummary:
			ok(req.ProtocolVersion, req.ID, &cache.SummaryResult{})
		case cache.OpShutdown:
			ok(req.ProtocolVersion, req.ID, nil)
			return
		default:
			write(&cache.ProviderResponse{
				ProtocolVersion: req.ProtocolVersion, ID: req.ID, OK: false,
				Error: &cache.ProviderError{Code: "unsupported", Message: string(req.Op)},
			})
		}
	}
}

// ---------------------------------------------------------------------------
// the matrix fixture
// ---------------------------------------------------------------------------

// matrixKey is the cache key the whole matrix is asserted on. It is COMPUTED
// (never a literal) so every leg exercises the key the scheduler itself derives,
// and matrixFixture pins that it is identical in every workspace — a key that
// varied with the checkout path could not address a machine-global store, let
// alone a shared remote cache.
func matrixKey(t *testing.T, f *captureFixture, job *ScheduledJob) string {
	t.Helper()
	keys, err := PrecomputeKeys(f.ws, []*ScheduledJob{job}, nil, nil, f.cache, CacheBypass{})
	if err != nil {
		t.Fatalf("precompute keys: %v", err)
	}
	hash := keys[job.Key()]
	if hash == "" {
		t.Fatal("no cache key precomputed for the fixture task")
	}
	return hash
}

// volatileStampPath is the per-run build stamp. It is excluded from the key by
// design (a rebuilt project must not invalidate an otherwise identical tree), so
// it is the one path the matrix does not compare — and the control below proves
// the exclusion is the ONLY difference two cold executions have.
const volatileStampPath = "version.json"

// matrixJob is the fixture task: one command-output directory, one project
// directory, one workspace file, and one optional output the task legitimately
// never produces. It covers all three declaration roots, both output kinds, and
// the explicit-empty state in a single entry.
func matrixJob() *ScheduledJob {
	return declaredJob("build~emit", "emit", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"lib":      dirDeclaration(extension.OutputRootCommandOutput, "lib", false),
			"gen":      dirDeclaration(extension.OutputRootProject, ".gen", false),
			"lock":     fileDeclaration(extension.OutputRootWorkspace, "bun.lock", false),
			"coverage": fileDeclaration(extension.OutputRootProject, "coverage.json", true),
		},
	})
}

// matrixResult is the fixture task's execution result.
func matrixResult() *JobResult {
	return &JobResult{Status: "success", Duration: 2 * time.Second}
}

// matrixOutputs are the fixture's declared outputs, resolved to their real
// workspace locations.
func matrixOutputs(f *captureFixture) map[string]string {
	return map[string]string{
		"lib":  filepath.Join(f.commandDir("build"), "lib"),
		"gen":  filepath.Join(f.ws.Root, captureTestProject, ".gen"),
		"lock": filepath.Join(f.ws.Root, "bun.lock"),
	}
}

// executeMatrixTask writes what the fixture task produces. stamp is the volatile
// per-run value; everything else is deterministic.
func executeMatrixTask(t *testing.T, f *captureFixture, stamp string) {
	t.Helper()
	out := matrixOutputs(f)
	writeFileAt(t, filepath.Join(out["lib"], "index.js"), "lib bytes\n")
	writeFileAt(t, filepath.Join(out["lib"], "nested", "chunk.js"), "chunk bytes\n")
	writeFileAt(t, filepath.Join(out["gen"], "client.ts"), "generated\n")
	writeFileAt(t, filepath.Join(out["gen"], volatileStampPath), stamp)
	writeFileAt(t, out["lock"], "lockfile\n")
	// coverage.json is deliberately never written: an optionalEmpty output the
	// task legitimately produces nothing for.
}

// declaredTree snapshots every declared output's real bytes, excluding the
// volatile stamp. A missing optional output is recorded as absent, so a leg that
// wrongly created it fails.
func declaredTree(t *testing.T, f *captureFixture) map[string]string {
	t.Helper()
	tree := map[string]string{}
	out := matrixOutputs(f)
	for _, id := range []string{"lib", "gen"} {
		root := out[id]
		err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				if os.IsNotExist(err) && p == root {
					return nil
				}
				return err
			}
			if info.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if rel == volatileStampPath {
				return nil // key-excluded by design; see the control below
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			tree[id+"/"+rel] = string(data)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if data, err := os.ReadFile(out["lock"]); err == nil {
		tree["lock"] = string(data)
	}
	if _, err := os.Stat(filepath.Join(f.ws.Root, captureTestProject, "coverage.json")); err == nil {
		tree["coverage"] = "PRESENT"
	}
	return tree
}

// mutateDeclaredOutputs makes the destinations wrong before a restore, so a leg
// that no-ops instead of restoring cannot pass.
func mutateDeclaredOutputs(t *testing.T, f *captureFixture) {
	t.Helper()
	out := matrixOutputs(f)
	writeFileAt(t, filepath.Join(out["lib"], "index.js"), "STALE\n")
	if err := os.RemoveAll(filepath.Join(out["lib"], "nested")); err != nil {
		t.Fatal(err)
	}
	writeFileAt(t, filepath.Join(out["gen"], "client.ts"), "STALE\n")
	writeFileAt(t, out["lock"], "STALE\n")
}

// matrixFixture builds a workspace whose scheduler talks to server through a
// provider-backed RemoteCache.
func matrixFixture(t *testing.T, server *fakeCacheServer) (*captureFixture, *RemoteCache) {
	t.Helper()
	enableProviderRemoteCache(t)
	useSharedFakeProvider(t, server)

	job := matrixJob()
	f := newCaptureFixture(t, job)
	remote, notice := LoadRemoteCache(context.Background(), f.ws.Root,
		[]*extension.ExtensionDescription{fakeProviderExtension(t, nil)}, nil, store.CacheTrustAny)
	if remote == nil || !remote.isProviderBacked() {
		t.Fatalf("expected a provider-backed remote cache, got %+v (notice %q)", remote, notice)
	}
	t.Cleanup(remote.Close)
	f.sched.setRemoteCache(remote)
	return f, remote
}

// declaredLookup drives the real read side: local lookup, remote restore, then
// claim-or-wait — the same entry point executeJob uses. It returns the key the
// read side derived alongside the result, and never calls t.Fatal, so the
// coalesced leg can run it on another goroutine.
func (f *captureFixture) declaredLookup(ctx context.Context, job *ScheduledJob) (string, *JobResult) {
	var mu sync.Mutex
	hashes := map[string]string{}
	hash, result, release := f.sched.lookupDeclaredEntry(ctx, job, nil, &mu, hashes)
	release()
	return hash, result
}

// leaseWaitGate turns a scheduler handoff into two deterministic phases: the
// waiter signals after it observed the leased miss, then remains before its real
// store wait until the winner has captured and made the workspace stale. Opening
// the gate is idempotent so cleanup cannot strand a blocked test goroutine.
type leaseWaitGate struct {
	arrived     chan struct{}
	proceed     chan struct{}
	arrivedOnce sync.Once
	proceedOnce sync.Once
}

func newLeaseWaitGate() *leaseWaitGate {
	return &leaseWaitGate{arrived: make(chan struct{}), proceed: make(chan struct{})}
}

func (g *leaseWaitGate) arriveAndWait() {
	g.arrivedOnce.Do(func() { close(g.arrived) })
	<-g.proceed
}

func (g *leaseWaitGate) open() {
	g.proceedOnce.Do(func() { close(g.proceed) })
}

// leaseObservedContext gates the first WaitForTaskEntry cancellation check.
// RemoteCache reaches it only after its task-entry claim lost to a live lease.
type leaseObservedContext struct {
	context.Context
	gate *leaseWaitGate
}

func (c *leaseObservedContext) Err() error {
	c.gate.arriveAndWait()
	return c.Context.Err()
}

// waitSignalCoalescer preserves the real declared coalescer while exposing the
// instant immediately before it waits for the lease owner. It is test-only
// synchronization, not a replacement for any cache operation.
type waitSignalCoalescer struct {
	inner cacheCoalescer
	gate  *leaseWaitGate
}

func (c *waitSignalCoalescer) claim(hash string, estimatedCost time.Duration) (bool, func()) {
	return c.inner.claim(hash, estimatedCost)
}

func (c *waitSignalCoalescer) wait(ctx context.Context, hash string, timeout time.Duration) error {
	c.gate.arriveAndWait()
	return c.inner.wait(ctx, hash, timeout)
}

func (c *waitSignalCoalescer) restore(ctx context.Context, hash string) *JobResult {
	return c.inner.restore(ctx, hash)
}

// runDeclaredLookup is declaredLookup with the key pinned, so no leg can
// silently address a different entry than the one under test.
func (f *captureFixture) runDeclaredLookup(t *testing.T, job *ScheduledJob, want string) *JobResult {
	t.Helper()
	hash, result := f.declaredLookup(t.Context(), job)
	if hash != want {
		t.Fatalf("the read side derived key %q, want %q", hash, want)
	}
	return result
}

// ---------------------------------------------------------------------------
// the acceptance deliverable
// ---------------------------------------------------------------------------

// TestDeclaredOutputsByteEquivalenceMatrix is the remote-delivery acceptance
// deliverable (binding invariant 3): cold, local-hit, remote-hit, mixed and
// coalesced runs all materialize BYTE-IDENTICAL declared outputs, on one fixture
// task, with the volatile build stamp excluded.
//
// The legs are not mocks of each other. Cold really executes and publishes;
// remote really uploads through a blob exchange and downloads into a second
// workspace with an empty store; mixed really serves part of the payload from
// the consumer's own CAS (the server refuses to serve one digest, so the restore
// can only succeed by dedup); coalesced really blocks on the task-owned lease
// until a sibling publishes.
func TestDeclaredOutputsByteEquivalenceMatrix(t *testing.T) {
	server := newFakeCacheServer()

	// --- leg 1: cold. Executes, publishes locally, shares remotely. -------
	cold, coldRemote := matrixFixture(t, server)
	coldJob := matrixJob()
	if !usesDeclaredCapture(coldJob) {
		t.Fatal("the fixture task must take the declared-capture path")
	}
	hash := matrixKey(t, cold, coldJob)
	remoteKey := store.RemoteTaskEntryKey(hash)

	if result := cold.runDeclaredLookup(t, coldJob, hash); result != nil {
		t.Fatalf("a cold key must miss everywhere, got %+v", result)
	}
	executeMatrixTask(t, cold, "cold-stamp\n")
	if !cold.sched.storeDeclaredCapture(coldJob, matrixResult(), hash) {
		t.Fatal("cold run published no entry")
	}
	coldRemote.UploadTaskEntry(t.Context(), hash, coldJob, cold.cache)
	coldRemote.DrainUploads()

	want := declaredTree(t, cold)
	if len(want) < 4 {
		t.Fatalf("the reference tree is too small to be meaningful: %v", want)
	}
	if _, ok := want["coverage"]; ok {
		t.Fatal("the optionalEmpty output must not exist after a cold run")
	}
	if !server.has(remoteKey) {
		t.Fatalf("the cold run did not share its entry under %s", remoteKey)
	}

	// --- leg 2: local hit. Same workspace, mutated outputs, local entry. ---
	mutateDeclaredOutputs(t, cold)
	localResult := cold.runDeclaredLookup(t, coldJob, hash)
	if localResult == nil || localResult.Reuse != ReuseLocalCache {
		t.Fatalf("local leg = %+v, want a local-cache hit", localResult)
	}
	assertSameTree(t, "local-hit", want, declaredTree(t, cold))

	// --- leg 3: remote hit. Fresh workspace, EMPTY store. ------------------
	remoteWS, _ := matrixFixture(t, server)
	remoteJob := matrixJob()
	if got := matrixKey(t, remoteWS, remoteJob); got != hash {
		t.Fatalf("the fixture key varies with the checkout (%s vs %s); no shared cache could address it", got, hash)
	}
	if entry, err := remoteWS.cache.LookupTaskEntry(hash); err != nil || entry != nil {
		t.Fatalf("the remote leg must start from an empty store, got %+v (err=%v)", entry, err)
	}
	remoteResult := remoteWS.runDeclaredLookup(t, remoteJob, hash)
	if remoteResult == nil || remoteResult.Reuse != ReuseRemoteCache {
		t.Fatalf("remote leg = %+v, want a remote-cache hit", remoteResult)
	}
	assertSameTree(t, "remote-hit", want, declaredTree(t, remoteWS))
	// The provider hit became an ordinary local entry, so a second run of the
	// same workspace is served locally without touching the network.
	if entry, err := remoteWS.cache.LookupTaskEntry(hash); err != nil || entry == nil {
		t.Fatalf("a remote hit must publish a local task-owned entry, got %+v (err=%v)", entry, err)
	}

	// --- leg 4: mixed. Some payload bytes already in the consumer's CAS. ---
	mixedWS, _ := matrixFixture(t, server)
	mixedJob := matrixJob()
	shared := payloadDigestFor(t, server, remoteKey, "gen/client.ts")
	// Warm exactly that blob into the consumer's CAS through an unrelated
	// legacy entry, then make the server refuse to serve it. The restore can now
	// only succeed by deduplicating against local bytes.
	warmCASWith(t, mixedWS, "generated\n")
	server.drop(shared)
	mixedResult := mixedWS.runDeclaredLookup(t, mixedJob, hash)
	if mixedResult == nil || mixedResult.Reuse != ReuseRemoteCache {
		t.Fatalf("mixed leg = %+v, want a remote-cache hit served partly from local CAS", mixedResult)
	}
	assertSameTree(t, "mixed", want, declaredTree(t, mixedWS))

	// --- leg 5: coalesced. A sibling publishes while this run waits. -------
	coalescedWS, _ := matrixFixture(t, server)
	coalescedJob := matrixJob()
	// The key must be a genuine miss everywhere, or the waiter would be served
	// before it ever blocks on the lease.
	server.forgetEntry(remoteKey)

	winner, release := coalescedWS.cache.TryClaimTaskEntry(hash, 0)
	if !winner {
		t.Fatal("could not take the task-owned lease")
	}
	var releaseOnce sync.Once
	releaseLease := func() { releaseOnce.Do(release) }
	type lookupOutcome struct {
		hash   string
		result *JobResult
	}
	waiterDone := make(chan lookupOutcome, 1)
	waiterGate := newLeaseWaitGate()
	t.Cleanup(waiterGate.open)
	t.Cleanup(releaseLease)
	go func() {
		ctx := &leaseObservedContext{Context: context.Background(), gate: waiterGate}
		gotHash, gotResult := coalescedWS.declaredLookup(ctx, coalescedJob)
		waiterDone <- lookupOutcome{hash: gotHash, result: gotResult}
	}()
	// CacheManager checks ctx.Err only after RemoteCache has lost the task-entry
	// claim and entered WaitForTaskEntry. Hold the waiter through capture and the
	// stale-output control mutation so its restore cannot race either write.
	select {
	case <-waiterGate.arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("the remote coalesced waiter never observed the leased miss")
	}
	executeMatrixTask(t, coalescedWS, "coalesced-stamp\n")
	if !coalescedWS.sched.storeDeclaredCapture(coalescedJob, matrixResult(), hash) {
		t.Fatal("the lease winner published no entry")
	}
	mutateDeclaredOutputs(t, coalescedWS)
	releaseLease()
	waiterGate.open()

	select {
	case got := <-waiterDone:
		if got.hash != hash {
			t.Fatalf("the coalesced waiter derived key %q, want %q", got.hash, hash)
		}
		if got.result == nil || got.result.Reuse != ReuseCoalesced {
			t.Fatalf("coalesced leg = %+v, want a coalesced result", got.result)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the coalesced waiter never consumed the winner's entry")
	}
	assertSameTree(t, "coalesced", want, declaredTree(t, coalescedWS))

	// --- control: the excluded stamp is the ONLY thing two colds differ in --
	// Without this, "byte-identical modulo the stamp" could be hiding a
	// comparator that ignores everything that actually varies.
	controlWS, _ := matrixFixture(t, server)
	executeMatrixTask(t, controlWS, "control-stamp\n")
	assertSameTree(t, "second cold execution", want, declaredTree(t, controlWS))
	coldStamp := readFileAt(t, filepath.Join(matrixOutputs(cold)["gen"], volatileStampPath))
	controlStamp := readFileAt(t, filepath.Join(matrixOutputs(controlWS)["gen"], volatileStampPath))
	if coldStamp == controlStamp {
		t.Fatal("the control must vary the excluded stamp, or it proves nothing")
	}
}

func assertSameTree(t *testing.T, leg string, want, got map[string]string) {
	t.Helper()
	for path, wantContent := range want {
		gotContent, ok := got[path]
		if !ok {
			t.Errorf("%s leg is missing declared output file %q", leg, path)
			continue
		}
		if gotContent != wantContent {
			t.Errorf("%s leg: %q = %q, want %q", leg, path, gotContent, wantContent)
		}
	}
	for path := range got {
		if _, ok := want[path]; !ok {
			t.Errorf("%s leg materialized an unexpected file %q", leg, path)
		}
	}
}

// payloadDigestFor returns the digest the server holds for one manifest path of
// a stored entry.
func payloadDigestFor(t *testing.T, server *fakeCacheServer, key, path string) string {
	t.Helper()
	manifest := server.manifestOf(key)
	if manifest == nil {
		t.Fatalf("no manifest stored for %s", key)
	}
	for _, f := range manifest.Files {
		if f.Path == path {
			return f.Digest
		}
	}
	t.Fatalf("manifest for %s has no file %q", key, path)
	return ""
}

// warmCASWith puts content into a workspace's CAS through an ordinary legacy
// entry, so a later remote restore of the same bytes deduplicates instead of
// fetching.
func warmCASWith(t *testing.T, f *captureFixture, content string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "warm.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	warmHash := "dd" + strings.Repeat("2", cache.KeyLength-2)
	if err := f.cache.Save(warmHash, &store.EntryResult{Status: "success"},
		&store.EntryMetadata{Extension: "@test/ext", Task: "warm", Project: captureTestProject}, dir); err != nil {
		t.Fatalf("warm CAS: %v", err)
	}
}

// ---------------------------------------------------------------------------
// format skew, negotiation, and the rules that must survive the remote leg
// ---------------------------------------------------------------------------

// TestTaskEntryIsSharedUnderTheFormatQualifiedKey is the cross-version skew
// guard on the REMOTE cache. A pre-B4c binary asks the provider for the raw
// cache key; if a format-2 payload were published there it would materialize it
// as a legacy capture and restore a tree laid out by declared-output id. The
// upload must therefore be invisible at the raw key.
func TestTaskEntryIsSharedUnderTheFormatQualifiedKey(t *testing.T) {
	server := newFakeCacheServer()
	f, remote := matrixFixture(t, server)
	job := matrixJob()
	hash := matrixKey(t, f, job)

	executeMatrixTask(t, f, "stamp\n")
	if !f.sched.storeDeclaredCapture(job, matrixResult(), hash) {
		t.Fatal("declared capture published no entry")
	}
	remote.UploadTaskEntry(t.Context(), hash, job, f.cache)
	remote.DrainUploads()

	if server.has(hash) {
		t.Fatal("a task-owned entry was published under the RAW cache key: an older CLI would read it as a legacy capture")
	}
	if !server.has(store.RemoteTaskEntryKey(hash)) {
		t.Fatal("the entry was not published under its format-qualified key")
	}
	// The descriptor travels as ordinary content, at the reserved path.
	manifest := server.manifestOf(store.RemoteTaskEntryKey(hash))
	found := false
	for _, file := range manifest.Files {
		if file.Path == store.RemoteEntryDescriptorPath {
			found = true
		}
	}
	if !found {
		t.Fatalf("the uploaded manifest carries no entry descriptor: %+v", manifest)
	}
}

// TestRestoreTaskEntry_RejectsALegacyRemotePayload is the fail-closed direction
// on the read side: a payload without a descriptor (what a pre-B4 CLI shares) is
// a MISS, never a partial restore, and publishes nothing locally.
func TestRestoreTaskEntry_RejectsALegacyRemotePayload(t *testing.T) {
	server := newFakeCacheServer()
	producer, producerRemote := matrixFixture(t, server)
	job := matrixJob()
	hash := matrixKey(t, producer, job)

	executeMatrixTask(t, producer, "stamp\n")
	if !producer.sched.storeDeclaredCapture(job, matrixResult(), hash) {
		t.Fatal("declared capture published no entry")
	}
	producerRemote.UploadTaskEntry(t.Context(), hash, job, producer.cache)
	producerRemote.DrainUploads()

	// Strip the descriptor from what the server serves: the blobs are all there,
	// so only the missing self-description can make this a miss.
	remoteKey := store.RemoteTaskEntryKey(hash)
	server.mu.Lock()
	entry := server.entries[remoteKey]
	files := make([]cache.FileEntry, 0, len(entry.manifest.Files))
	for _, f := range entry.manifest.Files {
		if f.Path != store.RemoteEntryDescriptorPath {
			files = append(files, f)
		}
	}
	server.entries[remoteKey] = fakeCacheEntry{result: entry.result, manifest: &cache.Manifest{Files: files}}
	server.mu.Unlock()

	consumer, consumerRemote := matrixFixture(t, server)
	if got, reuse := consumerRemote.RestoreTaskEntry(t.Context(), hash, matrixJob(), consumer.cache); got != nil || reuse != ReuseNone {
		t.Fatalf("a descriptor-less remote payload was accepted: %+v (%s)", got, reuse)
	}
	if entry, err := consumer.cache.LookupTaskEntry(hash); err != nil || entry != nil {
		t.Fatalf("a rejected remote payload published a local entry: %+v (err=%v)", entry, err)
	}
	// The declared outputs were never touched.
	if _, err := os.Stat(matrixOutputs(consumer)["lib"]); !os.IsNotExist(err) {
		t.Fatalf("a rejected remote payload materialized outputs: %v", err)
	}

	// Positive control: restore the descriptor and the same hit is accepted.
	server.mu.Lock()
	server.entries[remoteKey] = entry
	server.mu.Unlock()
	control, controlRemote := matrixFixture(t, server)
	if got, reuse := controlRemote.RestoreTaskEntry(t.Context(), hash, matrixJob(), control.cache); got == nil || reuse != ReuseRemoteCache {
		t.Fatalf("the same hit with its descriptor must be accepted, got %+v (%s)", got, reuse)
	}
}

// TestNegotiateAsksForTheTaskOwnedAddress pins the read side of decision 1: a
// declared-capture MISS is negotiated under the format-qualified key (so the
// provider can warm the entry this run will actually consume), while a published
// entry is recognized as the local hit it is and never negotiated at all.
func TestNegotiateAsksForTheTaskOwnedAddress(t *testing.T) {
	server := newFakeCacheServer()
	f, remote := matrixFixture(t, server)
	job := matrixJob()

	keys, err := PrecomputeKeys(f.ws, []*ScheduledJob{job}, nil, nil, f.cache, CacheBypass{})
	if err != nil {
		t.Fatalf("precompute keys: %v", err)
	}
	hash := keys[job.Key()]
	if hash == "" {
		t.Fatal("no cache key precomputed for the declared job")
	}

	remote.Negotiate(t.Context(), f.ws, []*ScheduledJob{job}, nil, nil, f.cache, CacheBypass{})
	want := []string{store.RemoteTaskEntryKey(hash)}
	if got := server.prefetched(); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("negotiated keys = %v, want %v (the raw key %s must never be asked for)", got, want, hash)
	}

	// Publish the entry and negotiate again: it is now a local hit, so nothing
	// more is asked of the provider — a fully warm rebuild must not need one.
	writeFileAt(t, filepath.Join(f.commandDir("build"), "lib", "index.js"), "cold\n")
	writeFileAt(t, filepath.Join(f.ws.Root, captureTestProject, ".gen", "client.ts"), "cold\n")
	writeFileAt(t, filepath.Join(f.ws.Root, "bun.lock"), "cold\n")
	if !f.sched.storeDeclaredCapture(job, matrixResult(), hash) {
		t.Fatal("declared capture published no entry")
	}
	warm, warmRemote := matrixFixture(t, server)
	// Re-publish the same entry in the warm workspace's own store.
	executeMatrixTask(t, warm, "stamp\n")
	if !warm.sched.storeDeclaredCapture(job, matrixResult(), hash) {
		t.Fatal("warm workspace published no entry")
	}
	before := len(server.prefetched())
	warmRemote.Negotiate(t.Context(), warm.ws, []*ScheduledJob{job}, nil, nil, warm.cache, CacheBypass{})
	if got := server.prefetched(); len(got) != before {
		t.Fatalf("a locally-published entry was negotiated anyway: %v", got)
	}
	if !warmRemote.localHits[hash] {
		t.Fatal("a published task-owned entry was not recognized as a local hit")
	}
}

// A source rewriter may publish a clean status entry, but an execution whose
// keyed inputs moved must never be restored as green. Both v3 signals are
// checked independently because strict validation pairs them, while this
// fail-safe rule must also hold for an invalid manifest carrying only one.
//
// Local hits, remote hits, and coalesced waiters all converge on
// restoreDeclaredCacheHit, so pinning rejection here covers all three legs.
func TestDeclaredSourceRewriterRejectsMutatingStatusEntry(t *testing.T) {
	sources := extension.ResourceRef{ID: sourcesResourceID, Scope: extension.ResourceScopeProject}
	cases := []struct {
		name        string
		declaration *extension.TaskDeclaration
		writes      []extension.ResourceRef
	}{
		{
			name:        "sources write resource without mutatesSources",
			declaration: &extension.TaskDeclaration{},
			writes:      []extension.ResourceRef{sources},
		},
		{
			name:        "mutatesSources with the sources write resource",
			declaration: &extension.TaskDeclaration{MutatesSources: true},
			writes:      []extension.ResourceRef{sources},
		},
		{
			name:        "mutatesSources without the sources write resource",
			declaration: &extension.TaskDeclaration{MutatesSources: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := declaredJob("lint~fix", "fix", tc.declaration, tc.writes...)
			job.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{NoOutput: true}

			if !isCacheEnabled(job, CacheBypass{}) {
				t.Fatal("a declared source rewriter did not reach clean-only caching")
			}
			if !declaredSourceRewriter(job) {
				t.Fatal("neither v3 source-mutation signal selected the restore guard")
			}
			if sourceMutationTask(job) {
				t.Fatal("a v3 task still infers the v2 source-mutation marker")
			}

			f := newCaptureFixture(t, job)
			var mu sync.Mutex
			hashes := map[string]string{}
			result := f.sched.finalizeExecutedJob(t.Context(), job, capturedResult(nil),
				true, captureHashA, "", &mu, hashes)
			if !result.SourceMutated {
				t.Fatal("an indeterminate pre-run source digest was not marked as source-mutating")
			}

			entry, err := f.cache.LookupTaskEntry(captureHashA)
			if err != nil || entry == nil {
				t.Fatalf("source-mutation marker entry = %v (err=%v)", entry, err)
			}
			if restored := f.sched.restoreDeclaredCacheHit(
				t.Context(), job, captureHashA, entry, &mu, hashes); restored != nil {
				t.Fatalf("mutating source-rewriter entry restored as green: %+v", restored)
			}
			if entry, err := f.cache.Lookup(captureHashA); err != nil || entry != nil {
				t.Fatalf("source rewriter also published a legacy entry: %v (err=%v)", entry, err)
			}
		})
	}

	// A v2 sibling also stays outside the cache boundary: it can run, but it
	// cannot reach a raw restore or publish path without an output contract.
	v2 := declaredJob("lint~fix", "fix", nil, sources)
	v2.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{NoOutput: true}
	if isCacheEnabled(v2, CacheBypass{}) {
		t.Fatal("the v2 sibling reached a cache path without a declaration")
	}
	if got, want := cacheTaskName(v2), "lint~fix"; got != want {
		t.Fatalf("cache task name = %q, want %q", got, want)
	}
}

// TestPrepareTaskUpload_AppliesTheSameGatesAsTheLegacyPath keeps the write-path
// policy identical across the two entry models: side-effecting tasks are never
// shared, a status-only entry is always shared, and the wire manifest always
// carries the descriptor.
func TestPrepareTaskUpload_AppliesTheSameGatesAsTheLegacyPath(t *testing.T) {
	job := declaredJob("build~emit", "emit", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"coverage": fileDeclaration(extension.OutputRootProject, "coverage.json", true),
		},
	})
	f := newCaptureFixture(t, job)
	if !f.sched.storeDeclaredCapture(job, capturedResult(nil), captureHashA) {
		t.Fatal("declared capture published no entry")
	}

	r := newUploadTestCache()
	in, ok := r.prepareTaskUpload(captureHashA, job, f.cache)
	if !ok {
		t.Fatal("an all-empty (status-only) task-owned entry must be eligible for upload")
	}
	if len(in.payload.Files) != 0 {
		t.Fatalf("payload = %+v, want no files", in.payload)
	}
	if len(in.Manifest.Files) != 1 || in.Manifest.Files[0].Path != store.RemoteEntryDescriptorPath {
		t.Fatalf("wire manifest = %+v, want exactly the descriptor", in.Manifest)
	}

	publish := declaredJob("publish", "publish", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"coverage": fileDeclaration(extension.OutputRootProject, "coverage.json", true),
		},
	})
	if _, ok := r.prepareTaskUpload(captureHashA, publish, f.cache); ok {
		t.Fatal("a side-effecting task must never be shared with the provider")
	}
}

// TestPrepareTaskUpload_SharesACheapEntryWithItsBytes pins the eligibility rule
// on the task-owned path: a 5 ms task whose declared outputs carry bytes is
// shared with its payload, because no duration or size rule leaves a
// non-side-effecting result out of the remote cache. The same entry under a
// side-effecting task name is never shared.
func TestPrepareTaskUpload_SharesACheapEntryWithItsBytes(t *testing.T) {
	job := matrixJob()
	f := newCaptureFixture(t, job)
	executeMatrixTask(t, f, "stamp\n")
	if !f.sched.storeDeclaredCapture(job, &JobResult{Status: "success", Duration: 5 * time.Millisecond}, captureHashA) {
		t.Fatal("declared capture published no entry")
	}

	r := newUploadTestCache()
	in, ok := r.prepareTaskUpload(captureHashA, job, f.cache)
	if !ok {
		t.Fatal("a cheap bytes-carrying task-owned entry must be eligible for upload")
	}
	if in.Key != store.RemoteTaskEntryKey(captureHashA) {
		t.Fatalf("upload key = %q, want the format-qualified key", in.Key)
	}
	if len(in.payload.Files) == 0 || manifestBytes(in.payload) == 0 {
		t.Fatalf("payload = %+v, want the task's output bytes", in.payload)
	}
	if len(in.Manifest.Files) != len(in.payload.Files)+1 {
		t.Fatalf("wire manifest has %d files, want the %d payload files plus the descriptor",
			len(in.Manifest.Files), len(in.payload.Files))
	}
	if in.Result == nil || in.Result.DurationMs != 5 {
		t.Fatalf("upload result = %+v, want the recorded 5 ms duration", in.Result)
	}

	publish := matrixJob()
	publish.JobDef.Name = "publish~emit"
	if _, ok := r.prepareTaskUpload(captureHashA, publish, f.cache); ok {
		t.Fatal("a side-effecting task must never be shared with the provider")
	}
}

// TestDeclaredCoalescerServesTheWinnersEntry covers the OTHER coalescing route:
// a purely local build (no provider), where the waiter reaches
// coalesceMiss/declaredCoalescer instead of the remote step's lease wait. Both
// routes must deliver the same tree through the same materialize, so the
// byte-equivalence invariant cannot depend on whether a remote cache is
// configured.
func TestDeclaredCoalescerServesTheWinnersEntry(t *testing.T) {
	job := matrixJob()
	f := newCaptureFixture(t, job)
	if f.sched.remote != nil {
		t.Fatal("this leg must run without a remote cache")
	}
	hash := matrixKey(t, f, job)

	// Reference: what an execution of this task leaves on disk.
	executeMatrixTask(t, f, "winner-stamp\n")
	want := declaredTree(t, f)

	winner, release := f.cache.TryClaimTaskEntry(hash, 0)
	if !winner {
		t.Fatal("could not take the task-owned lease")
	}
	var releaseOnce sync.Once
	releaseLease := func() { releaseOnce.Do(release) }
	type outcome struct {
		result *JobResult
	}
	done := make(chan outcome, 1)
	waiterGate := newLeaseWaitGate()
	t.Cleanup(waiterGate.open)
	t.Cleanup(releaseLease)
	var mu sync.Mutex
	hashes := map[string]string{}
	// Keep claim, wait, and restore on the production declaredCoalescer; the
	// wrapper only holds the exact handoff before its store wait.
	coalescer := &waitSignalCoalescer{
		inner: declaredCoalescer{
			scheduler: f.sched,
			job:       job,
			mu:        &mu,
			hashes:    hashes,
		},
		gate: waiterGate,
	}
	go func() {
		gotResult, release := f.sched.coalesceMiss(context.Background(), job, hash, coalescer)
		release()
		done <- outcome{result: gotResult}
	}()
	select {
	case <-waiterGate.arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("the local coalesced waiter never reached the lease wait")
	}

	if !f.sched.storeDeclaredCapture(job, matrixResult(), hash) {
		t.Fatal("the lease winner published no entry")
	}
	mutateDeclaredOutputs(t, f)
	releaseLease()
	waiterGate.open()

	select {
	case got := <-done:
		if got.result == nil || got.result.Reuse != ReuseCoalesced {
			t.Fatalf("local coalesced leg = %+v, want a coalesced result", got.result)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the waiter never consumed the winner's entry")
	}
	assertSameTree(t, "local coalesced", want, declaredTree(t, f))
}

// TestFinalizeExecutedJob_SharesDeclaredEntryWithTheProvider pins the SCHEDULER
// wiring of the write path, not just the RemoteCache method: after a real
// execution of a declared-capture task, finalizeExecutedJob must publish the
// task-owned entry locally AND share it with the provider under its
// format-qualified key. B4b's temporary "declared tasks never upload" fallback
// lived exactly here, and nothing else would notice its return.
func TestFinalizeExecutedJob_SharesDeclaredEntryWithTheProvider(t *testing.T) {
	server := newFakeCacheServer()
	f, remote := matrixFixture(t, server)
	job := matrixJob()
	hash := matrixKey(t, f, job)

	executeMatrixTask(t, f, "stamp\n")

	var mu sync.Mutex
	hashes := map[string]string{}
	result := f.sched.finalizeExecutedJob(t.Context(), job, matrixResult(), true, hash, "", &mu, hashes)
	if result == nil || result.Status != "success" {
		t.Fatalf("finalizeExecutedJob = %+v, want the executed result", result)
	}
	if hashes[job.Key()] != hash {
		t.Fatalf("finalize recorded key %q, want %q", hashes[job.Key()], hash)
	}
	if entry, err := f.cache.LookupTaskEntry(hash); err != nil || entry == nil {
		t.Fatalf("finalize published no task-owned entry: %+v (err=%v)", entry, err)
	}

	remote.DrainUploads()
	if !server.has(store.RemoteTaskEntryKey(hash)) {
		t.Fatal("finalizeExecutedJob did not share the declared entry with the provider")
	}
	if server.has(hash) {
		t.Fatal("the declared entry was shared under the raw cache key")
	}
}
