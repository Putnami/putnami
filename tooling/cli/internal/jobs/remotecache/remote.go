package remotecache

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	jobmodel "go.putnami.dev/cli/model/jobs"
	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// uploadConcurrency bounds the number of post-build provider upload exchanges
// running in the background at once. Uploads are IO-bound, so a handful overlaps
// the build without overwhelming the provider or the link.
const uploadConcurrency = 8

// remoteRestoreLeaseWaitTimeout is deliberately longer than the provider's
// normal one-minute operation timeout. A healthy owner should publish or release
// first; this ceiling only keeps a live-but-wedged owner from making remote
// coordination a correctness dependency.
const remoteRestoreLeaseWaitTimeout = 90 * time.Second

// ClientsResourceID is the project resource a client generator writes.
const ClientsResourceID = "clients"

// RemoteCache wires the remote build cache into scheduling by delegating to an
// out-of-process cache-provider extension (shipped by @putnami/cloud). Before
// execution it hands the whole build's key set to the provider so it can warm
// its caches; during execution it restores task-owned provider hits into the
// local store on a local miss and uploads freshly published task-owned entries. Every remote operation is
// best-effort: any failure logs and the build falls back to local behavior, so
// the remote cache can never break a build, only accelerate it.
type RemoteCache struct {
	// provider is the out-of-process cache-provider session wiring. It is always
	// set for a live RemoteCache: LoadRemoteCache only ever returns a
	// provider-backed instance (or nil, for a purely local build).
	provider *providerRemote

	// trust is the run-authority policy applied at the restore decision seam.
	// It never changes local-store behavior. Under ci, non-trusted and legacy
	// provider hits can warm CAS blobs but cannot return a successful JobResult.
	Trust store.CacheTrust

	// keys maps a job key to its precomputed cache key (from Negotiate), only
	// for a job whose key no planned write can move (cachekey.PlanKeyMovement); localHits
	// records keys already present locally before negotiation. Neither
	// addresses a restore or an upload: both use the key the scheduler computes
	// when the job starts.
	Keys      map[string]string
	LocalHits map[string]bool
	// dependedOn holds the key of every job whose files a planned job reads
	// (dependedOnJobs). Negotiate sets it before any job runs; nil means the
	// plan was never seen, and then every task's files are needed
	// (TaskFilesNeeded).
	dependedOn map[string]bool

	// uploadWG/uploadSem run post-build uploads on a bounded background pool so
	// their round trips overlap the build instead of stalling the worker that
	// produced each entry. DrainUploads waits on uploadWG at the end of the run.
	uploadWG  sync.WaitGroup
	uploadSem chan struct{}

	// actionEvents is set when the provider reports it accepts ActionResult.Events
	// in upload payloads, so uploads carry them only when the provider supports it.
	// ensureProvider writes it under the provider lock: read it only after
	// ensureProvider returns (see providerResult).
	actionEvents bool

	// stats accumulates this run's cache activity (hits, bytes, time saved vs
	// spent) for the post-run summary. Always non-nil for a live RemoteCache.
	stats *cachekey.CacheStats

	// sourceBoundEntryKey resolves the store key a source-bound task is
	// looked up at (SetSourceBoundEntryKey). Nil means negotiation cannot
	// tell whether such a task hits locally.
	sourceBoundEntryKey func(job *jobmodel.ScheduledJob, key string) (string, bool)
}

// Stats returns this run's accumulated cache activity, or nil for a nil cache
// (the local-only path), so callers can attach a summary without a guard.
func (r *RemoteCache) Stats() *jobmodel.CacheStatsSnapshot {
	if r == nil || r.stats == nil {
		return nil
	}
	return r.stats.Snapshot()
}

// CacheStats returns the accumulator this run's cache activity is recorded in,
// or nil for a nil cache or a cache built without one.
func (r *RemoteCache) CacheStats() *cachekey.CacheStats {
	if r == nil {
		return nil
	}
	return r.stats
}

// SetCacheStats makes stats the accumulator this run's cache activity is
// recorded in. The scheduler hands its own accumulator to a cache built
// without one. A nil cache ignores it.
func (r *RemoteCache) SetCacheStats(stats *cachekey.CacheStats) {
	if r != nil {
		r.stats = stats
	}
}

// SetSourceBoundEntryKey makes resolve the way negotiation finds the store key
// a source-bound task is looked up at. The scheduler hands it the resolver its
// own lookup uses. A nil cache ignores it.
func (r *RemoteCache) SetSourceBoundEntryKey(resolve func(job *jobmodel.ScheduledJob, key string) (string, bool)) {
	if r != nil {
		r.sourceBoundEntryKey = resolve
	}
}

// sourceBoundHitsLocally reports whether the local store holds a full entry at
// the store key a source-bound task is looked up at, for the project's binding
// as negotiation sees it. Without a resolver or a binding it answers false, so
// the task counts as one that may execute.
func (r *RemoteCache) sourceBoundHitsLocally(job *jobmodel.ScheduledJob, hash string, cm *store.CacheManager) bool {
	if r.sourceBoundEntryKey == nil {
		return false
	}
	entryKey, ok := r.sourceBoundEntryKey(job, hash)
	if !ok {
		return false
	}
	entry, err := cm.LookupTaskEntry(entryKey)
	return err == nil && entry != nil
}

// RecordSetup attributes the pre-run provider resolution time to this run's cache
// stats, so the cost the user saw "before negotiate" becomes visible. Safe on a
// nil cache (the local-only path), so the caller needs no guard.
func (r *RemoteCache) RecordSetup(d time.Duration) {
	if r != nil {
		r.stats.RecordSetup(d.Nanoseconds())
	}
}

// LoadRemoteCache builds a RemoteCache that delegates to the cache-provider
// extension: the one extension that declares the reserved provider command. It
// returns nil, so the build runs purely local, when no extension declares it or
// trust is "none". The core reads no remote cache configuration. The provider
// owns it (its file, its environment overrides, its defaults) and says at
// initialize whether it serves the run (InitializeResult.Ready) and in which
// materialization mode. A provider that is not ready leaves the run building
// locally with no notice: that is the unconfigured default. Credentials travel
// to the provider through its inherited environment, so the CLI never handles a
// bearer token itself. The exception is a hosted run's credential, which the
// caller passes with WithCacheRunCredential and the provider receives over its
// RPC, never through its environment.
//
// The second return value is a one-line, user-facing notice, non-empty only
// when a declared provider cannot serve the run: a hosted run removed the
// provider command from a path extension (discovered's
// OnlyFromPathExtensions), the provider resolution failed, or trust is
// unresolved. Callers print a non-empty notice once. A provider that fails to
// start or to answer initialize says so itself, once, when the first operation
// that needs it starts it. discovered may be nil when the caller has no
// discovery result.
//
// The context bounds provider detection and the preparation of the extension
// runtime the provider command references; provider startup is deferred to the
// first request that needs it. launcher prepares that launch; a nil launcher
// leaves a declared provider unserved, with a notice.
func LoadRemoteCache(ctx context.Context, wsRoot string, exts []*extension.ExtensionDescription, discovered *extension.DiscoveryResult, trust store.CacheTrust, launcher Launcher, options ...RemoteCacheOption) (*RemoteCache, string) {
	if trust == store.CacheTrustNone {
		return nil, ""
	}
	if !trust.Valid() {
		// Fail closed if an internal caller bypassed CLI validation. An unknown
		// policy must never degrade into accepting hints on an authoritative run.
		return nil, "putnami: remote cache trust policy is unresolved; building locally"
	}
	provider, notice, err := loadProviderRemoteCache(ctx, wsRoot, exts, discovered, trust, launcher)
	if err != nil {
		// A gate failure (e.g. an @putnami/cloud too old to serve this protocol) is
		// a degradation, not the unconfigured default — surface it rather than
		// dropping to a redirection-dependent structured log.
		return nil, fmt.Sprintf("putnami: remote cache provider unavailable (%v); building locally", err)
	}
	if provider != nil {
		for _, option := range options {
			option(provider.provider)
		}
	}
	return provider, notice
}

// RemoteCacheOption adjusts the provider-backed cache LoadRemoteCache builds.
type RemoteCacheOption func(*providerRemote)

// WithCacheRunCredential hands bearer, the hosted run's credential, to the
// cache provider over its RPC: the provider receives it in one
// cache.OpAuthenticate when it echoes cache.CapabilityRunCredential. The
// provider's launch environment does not change. An empty bearer is the same
// as no option.
func WithCacheRunCredential(bearer string) RemoteCacheOption {
	return func(p *providerRemote) { p.runCredential = bearer }
}

// RunRemoteCache is the remote cache of one run, loaded at most once, so the
// auto-selection run-marker lookup and the build read it through one provider
// process. The zero value is ready to use. A nil RunRemoteCache holds nothing:
// each Load loads a new remote cache, which its caller closes. Whoever creates
// a RunRemoteCache closes it, after the last stage of the run that reads it.
type RunRemoteCache struct {
	mu     sync.Mutex
	loaded bool
	remote *RemoteCache
	notice string
}

// Load returns the remote cache of the run and its notice: what the first Load
// got from LoadRemoteCache with these arguments, whatever later calls pass. A
// caller that prints the notice prints it once per run, and a nil c returns
// LoadRemoteCache's answer as is.
func (c *RunRemoteCache) Load(ctx context.Context, wsRoot string, exts []*extension.ExtensionDescription, discovered *extension.DiscoveryResult, trust store.CacheTrust, launcher Launcher, options ...RemoteCacheOption) (*RemoteCache, string) {
	if c == nil {
		return LoadRemoteCache(ctx, wsRoot, exts, discovered, trust, launcher, options...)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		c.loaded = true
		c.remote, c.notice = LoadRemoteCache(ctx, wsRoot, exts, discovered, trust, launcher, options...)
	}
	return c.remote, c.notice
}

// Close stops the provider of the remote cache Load returned, if any. Closing a
// RemoteCache twice is safe, so a stage that closes the one it read changes
// nothing here. A nil RunRemoteCache does nothing.
func (c *RunRemoteCache) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.remote.Close()
}

// Negotiate precomputes every cacheable key and hands the build's non-local,
// non-side-effecting key set to the provider so it can warm its caches before
// execution. The provider protocol has no batch negotiate result — hits are
// discovered lazily through RestoreTaskEntry on a local miss — so this records the key set
// as misses up front and each restore reclassifies its key. Best-effort: any
// failure logs and leaves the cache effectively empty, so every key is a miss.
//
// It is also where the provider session is STARTED, and the trigger is not only
// the key set: a run whose jobs will execute locally needs the provider even with
// nothing to warm, because the object cache those jobs reach lives behind the
// same session (the socket path is negotiated at initialize).
//
// A precomputed key that an earlier task's writes may move is still offered to
// the provider to warm, and still counts as warm when the local store holds it:
// both are hints, right whenever those writes leave the bytes as they were. It
// is never recorded as the job's key.
func (r *RemoteCache) Negotiate(
	ctx context.Context,
	ws *workspace.Workspace,
	planned []*jobmodel.ScheduledJob,
	commandParams map[string]any,
	versions cachekey.RunVersions,
	cm *store.CacheManager,
	bypass cachekey.CacheBypass,
) {
	if r == nil || cm == nil {
		return
	}
	var movement cachekey.KeyMovement
	if ws != nil {
		movement = cachekey.PlanKeyMovement(cachekey.KeyInputs{Workspace: ws, CommandParams: commandParams, Resolver: cm.InputResolver()}, planned)
		r.dependedOn = dependedOnJobs(planned, &movement)
	} else {
		r.dependedOn = dependedOnJobs(planned, nil)
	}

	keys, err := cachekey.PrecomputeKeys(ws, planned, commandParams, versions, cm, bypass, r.stats)
	if err != nil {
		slog.Warn("remote cache: key precompute failed; skipping remote cache", "error", err)
		return
	}
	r.Keys = keys
	if len(movement.Moving) > 0 {
		r.Keys = make(map[string]string, len(keys))
		for jobKey, hash := range keys {
			if !movement.Moving[jobKey] {
				r.Keys[jobKey] = hash
			}
		}
	}
	r.LocalHits = make(map[string]bool)

	var providerKeys []string
	// declaredJobs maps each declared-capture provider key to the jobs that
	// restore it, and declaredHashes maps it to its local cache key, so the
	// result-only subset can be chosen once the provider has answered
	// initialize.
	declaredJobs := make(map[string][]*jobmodel.ScheduledJob)
	declaredHashes := make(map[string]string)
	// executesLocally records whether ANY planned job will run a process on this
	// machine. It is not the same question as "is there a key to warm": a run of
	// only side-effecting or uncacheable tasks warms nothing yet still spawns
	// jobs, and those jobs need the provider's object cache (its socket travels
	// to them through the job environment).
	executesLocally := false
	for _, job := range planned {
		hash, ok := keys[job.Key()]
		if !ok {
			// No cache key: the job always executes.
			executesLocally = true
			continue
		}
		if jobmodel.HasExternalEffects(job) {
			executesLocally = true
			continue
		}
		// A source-bound task's entry lives at a store key that also binds the
		// project's current source binding (cachekey.SourceBoundEntryKey).
		// Nothing is ever published at the address of its plain key, so it is
		// not offered. Whether it may execute is asked at its bound key: a local
		// entry there is a hit that needs no provider, so a fully warm rebuild
		// still spins up nothing, and without one the task may execute, which
		// keeps the provider up for its restore and its object cache. It stays
		// out of LocalHits, because a planned write can still move the binding.
		if cachekey.TaskIsSourceBound(job) {
			if !r.sourceBoundHitsLocally(job, hash, cm) {
				executesLocally = true
			}
			continue
		}
		// Only a declared-capture task reuses an entry (cachekey.IsCacheEnabled),
		// so any other keyed job executes here and negotiates nothing.
		if !jobmodel.UsesDeclaredCapture(job) {
			executesLocally = true
			continue
		}
		// A declared-capture task negotiates the FORMAT-QUALIFIED address rather
		// than the raw key: that is where its entry lives in the remote cache, and
		// the separation keeps a binary that reads the raw key as a format-1
		// entry from ever receiving a format-2 payload (store/task_remote.go). A
		// local hit is served locally and never negotiated, so a fully warm
		// rebuild never spins up the provider. LocalHits stays keyed by the local
		// hash.
		if entry, err := cm.LookupTaskEntry(hash); err == nil && entry != nil {
			r.LocalHits[hash] = true
			continue
		}
		providerKey := store.RemoteTaskEntryKey(hash)
		providerKeys = append(providerKeys, providerKey)
		declaredJobs[providerKey] = append(declaredJobs[providerKey], job)
		declaredHashes[providerKey] = hash
		executesLocally = true
	}

	// Spawn the provider when there is something to warm, OR when a job will run
	// here and could use the object cache. A fully warm rebuild still spins up
	// nothing: every planned job is a local hit, so neither condition holds. A
	// key a local result-only entry may serve still counts here, because the
	// echo that decides it arrives with the session.
	// --no-cache consults nothing remote, so it keeps the provider down too
	// (PrecomputeKeys already omitted every key, which is why this flag has to be
	// read explicitly rather than inferred from an empty key set).
	if len(providerKeys) == 0 && (bypass.All || !executesLocally) {
		return
	}
	start := time.Now()
	sess, ok := r.ensureProvider(ctx, ws, cm)
	if !ok {
		return
	}
	// Only now is the provider's echo known, so only now can a local
	// result-only entry count as a local hit. Before it, the rule answers that
	// every task needs its files, and the key stays with the provider.
	providerKeys = r.dropLocalResultOnlyHits(providerKeys, declaredJobs, declaredHashes, cm)
	if len(providerKeys) == 0 {
		// Object-cache-only spawn, or every key a local result-only hit: nothing
		// to prefetch, and no negotiation to record — recording a zero-key
		// negotiate would report a round trip the run never made.
		return
	}
	prefetch := &cache.PrefetchParams{Keys: providerKeys, ResultOnlyKeys: r.resultOnlyKeys(providerKeys, declaredJobs)}
	if _, err := sess.Prefetch(ctx, prefetch); err != nil {
		slog.Warn("remote cache: provider prefetch failed; building locally", "error", err)
	}
	// Record the key set as misses; a restore promotes each served key to a hit.
	r.stats.RecordNegotiate(time.Since(start).Nanoseconds(), len(providerKeys), 0)
}

// resultOnlyKeys returns the declared-capture provider keys, in providerKeys
// order and once each, whose files no job of this run reads. A key shared by
// several jobs is result-only only when none of them needs the files.
func (r *RemoteCache) resultOnlyKeys(providerKeys []string, declaredJobs map[string][]*jobmodel.ScheduledJob) []string {
	var out []string
	seen := make(map[string]bool, len(declaredJobs))
	for _, key := range providerKeys {
		if seen[key] {
			continue
		}
		seen[key] = true
		if !r.filesNeededByAny(declaredJobs[key]) {
			out = append(out, key)
		}
	}
	return out
}

// dropLocalResultOnlyHits returns providerKeys without the declared-capture
// keys a local result-only entry serves, and records each such key's local
// hash as a local hit. A key qualifies only when no job sharing it reads its
// files and no full local entry exists for it (Negotiate already counted those),
// which is the condition under which lookupDeclaredEntry serves the result-only
// entry. The result keeps providerKeys' order.
func (r *RemoteCache) dropLocalResultOnlyHits(
	providerKeys []string,
	declaredJobs map[string][]*jobmodel.ScheduledJob,
	declaredHashes map[string]string,
	cm *store.CacheManager,
) []string {
	served := make(map[string]bool, len(declaredHashes))
	kept := make([]string, 0, len(providerKeys))
	for _, key := range providerKeys {
		hit, decided := served[key]
		if !decided {
			hash, declared := declaredHashes[key]
			hit = declared && !r.filesNeededByAny(declaredJobs[key]) && cm.LookupResultOnlyTaskEntry(hash) != nil
			served[key] = hit
			if hit {
				r.LocalHits[hash] = true
			}
		}
		if !hit {
			kept = append(kept, key)
		}
	}
	return kept
}

// filesNeededByAny reports whether any job restoring one key reads the key's
// files. A key no declared-capture job restores needs its files.
func (r *RemoteCache) filesNeededByAny(jobs []*jobmodel.ScheduledJob) bool {
	if len(jobs) == 0 {
		return true
	}
	for _, job := range jobs {
		if r.TaskFilesNeeded(job) {
			return true
		}
	}
	return false
}

func remoteRestoreEstimatedCost(job *jobmodel.ScheduledJob) time.Duration {
	if job == nil || job.ExpectedWallMs <= 0 {
		return 0
	}
	return time.Duration(job.ExpectedWallMs) * time.Millisecond
}

func (r *RemoteCache) recordRestoredHit(
	durationMs int64,
	fetchedBytes int64,
	materializeStart time.Time,
	materializeEnd time.Time,
) {
	r.stats.RecordFetch(fetchedBytes)
	r.stats.RecordMaterialize(materializeStart, materializeEnd)
	r.stats.RecordRestore(durationMs)
	// The provider has no batch negotiate result, so hits are discovered here, one
	// per successful restore. Negotiate seeded misses = keysRequested; move this
	// key into the hit column so a provider- or sibling-served run reports "N hit"
	// instead of "0 hit · N miss".
	r.stats.RecordProviderHit()
}

// Prefetch is the scheduler's pre-execution hook. The provider warms its caches
// from the key set handed to it in Negotiate, so there is no separate
// CLI-driven prefetch pass; this is retained as a no-op for the scheduler's call
// site and to keep a single place to document where speculative fetching lives.
func (r *RemoteCache) Prefetch(context.Context, []*jobmodel.ScheduledJob, *store.CacheManager) {}

// Stop is the scheduler's end-of-run hook. Delegated caching does its prefetching
// inside the provider, so there is nothing to unwind here; crucially Stop does
// NOT close the provider session, because the post-run success marker is
// published afterwards (recordSuccessfulBuild) and still needs it. The owner
// releases the session with Close.
func (r *RemoteCache) Stop() {
	_ = r
}

// Close releases the provider session and its resources. It is the owner's final
// teardown — distinct from Stop, which only marks end-of-run — and must run after
// the post-run success marker is published, since publishing reuses the
// still-open session. Safe on a nil cache and to call more than once.
func (r *RemoteCache) Close() {
	if r == nil {
		return
	}
	r.closeProvider()
}

// providerResult returns the result an upload sends to the started provider:
// the cached events ride along only when the provider advertised
// action-events. The upload calls it after ensureProvider, which records that
// answer, so the first upload of a run, which may start the provider, already
// knows it. The prepared result is not modified.
func (r *RemoteCache) providerResult(result *cache.ActionResult) *cache.ActionResult {
	if r.actionEvents || result == nil || result.Events == nil {
		return result
	}
	withoutEvents := *result
	withoutEvents.Events = nil
	return &withoutEvents
}

// DrainUploads waits for the background uploads to finish, then asks the provider
// for its run summary (counts/bytes restored and uploaded) and records the wall
// time the write path added to the makespan after the build's own work
// completed. Safe to call when no upload was scheduled. The scheduler calls this
// only after all workers finish, so no upload races the drain.
func (r *RemoteCache) DrainUploads() {
	if r == nil {
		return
	}
	start := time.Now()
	r.uploadWG.Wait()
	ctx, cancel := providerContext()
	r.providerDrain(ctx)
	cancel()
	r.stats.RecordUploadWall(time.Since(start).Nanoseconds())
}

// uploadInput is the per-job data needed to share a freshly built result with the
// cache provider: the precomputed cache key, the cached outcome to record, and
// the built manifest (the file set with per-file CAS digests).
type uploadInput struct {
	Key      string
	Result   *cache.ActionResult
	Manifest *cache.Manifest
}
