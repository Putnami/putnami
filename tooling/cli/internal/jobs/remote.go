package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/tooling/cli/internal/extension"
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

const clientsResourceID = "clients"

// The raw remote compatibility API remains readable for existing store data,
// but Scheduler never routes a job to it: isCacheEnabled requires a task-owned
// declaration. These helpers describe that older payload shape only.
func taskDeclaresNoOutput(job *ScheduledJob) bool {
	return job != nil && job.JobDef != nil &&
		job.JobDef.TaskCachePolicy != nil && job.JobDef.TaskCachePolicy.NoOutput
}

func sourceMutationTask(job *ScheduledJob) bool {
	return taskDeclarationOf(job) == nil && taskDeclaresNoOutput(job) && jobWritesSourceTree(job)
}

func cacheHitMissingRequiredOutput(job *ScheduledJob, entry *store.Entry) bool {
	return writesProjectGen(job) && (entry == nil || entry.FilesDir == "")
}

func writesOnlyOptionalCapture(job *ScheduledJob) bool {
	if job == nil || job.JobDef == nil {
		return false
	}
	hasClients := false
	for _, ref := range job.JobDef.Writes {
		if ref.EffectiveScope() != extension.ResourceScopeProject {
			continue
		}
		if ref.ID == genResourceID {
			return false
		}
		if ref.ID == clientsResourceID {
			hasClients = true
		}
	}
	return hasClients
}

func reportsClientOutput(data map[string]any) bool {
	if data == nil {
		return false
	}
	if raw, ok := data["clientOutput"].(string); ok {
		_, ok := cleanProjectRel(raw)
		return ok
	}
	switch raw := data["clientOutputs"].(type) {
	case []string:
		for _, value := range raw {
			if _, ok := cleanProjectRel(value); ok {
				return true
			}
		}
	case []any:
		for _, value := range raw {
			if text, ok := value.(string); ok {
				if _, ok := cleanProjectRel(text); ok {
					return true
				}
			}
		}
	}
	return false
}

func cleanProjectRel(rel string) (string, bool) {
	if rel == "" {
		return "", false
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false
	}
	return clean, true
}

// RemoteCache wires the remote build cache into scheduling by delegating to an
// out-of-process cache-provider extension (shipped by @putnami/cloud). Before
// execution it hands the whole build's key set to the provider so it can warm
// its caches; during execution it restores provider hits into the local store on
// a local miss and uploads freshly built misses. Every remote operation is
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
	trust store.CacheTrust

	// keys maps a job key to its precomputed cache key (from Negotiate); localHits
	// records keys already present locally before negotiation. Together they let
	// computeRequiredInputs tell "will execute locally" from "already satisfied".
	keys      map[string]string
	localHits map[string]bool
	// requiredInputs records hit dependency keys that feed a job expected to
	// execute locally. Such hits must materialize real output files when the
	// provider entry claims files exist; an empty manifest is treated as unusable
	// and the dependency is rebuilt locally instead (the satisfied-but-absent
	// cascade) — unless the hit is a status-only optional capture
	// (statusOnlyOptionalCaptureHit): a describe whose result reports no
	// clients legitimately has zero files.
	requiredInputs map[string]bool
	// dependedOn holds the key of every job a planned job waits for
	// (dependedOnJobs). Negotiate sets it before any job runs; nil means the
	// plan was never seen, and then every task's files are needed
	// (taskFilesNeeded).
	dependedOn map[string]bool

	// mu guards index. index records the provider hits restored so far (cache key
	// -> result); Restore writes it from worker goroutines as restores land and
	// HasFiles reads it, so both must hold mu.
	mu    sync.Mutex
	index map[string]cache.KeyResult

	// uploadWG/uploadSem run post-build uploads on a bounded background pool so
	// their round trips overlap the build instead of stalling the worker that
	// produced each entry. DrainUploads waits on uploadWG at the end of the run.
	uploadWG  sync.WaitGroup
	uploadSem chan struct{}

	// actionEvents is set when the provider reports it accepts ActionResult.Events
	// in upload payloads, so uploads carry them only when the provider supports it.
	actionEvents bool

	// stats accumulates this run's cache activity (hits, bytes, time saved vs
	// spent) for the post-run summary. Always non-nil for a live RemoteCache.
	stats *CacheStats
}

// Stats returns this run's accumulated cache activity, or nil for a nil cache
// (the local-only path), so callers can attach a summary without a guard.
func (r *RemoteCache) Stats() *CacheStatsSnapshot {
	if r == nil || r.stats == nil {
		return nil
	}
	return r.stats.Snapshot()
}

// RecordSetup attributes the pre-run config resolution time to this run's cache
// stats, so the cost the user saw "before negotiate" becomes visible. Safe on a
// nil cache (the local-only path), so the caller needs no guard.
func (r *RemoteCache) RecordSetup(d time.Duration) {
	if r != nil {
		r.stats.recordSetup(d.Nanoseconds())
	}
}

// RemoteCacheConfigPath is the workspace-relative location of the persisted
// remote-cache configuration, written by the cloud extension's setup command
// and read by the CLI.
func RemoteCacheConfigPath(wsRoot string) string {
	return filepath.Join(wsRoot, ".putnami", "cache.json")
}

// LoadRemoteCache builds a RemoteCache that delegates to the cloud cache-provider
// extension when one is installed and the version/capability gate confirms it can
// serve this protocol. It returns nil — so the build runs purely local — when no
// capable provider is available. Credentials and the cache URL/mode (including the
// PUTNAMI_CACHE_* env overrides) travel to the provider through its inherited
// environment, so the CLI never handles a bearer token itself. The exception is
// a hosted run's credential, which the caller passes with WithCacheRunCredential
// and the provider receives over its RPC, never through its environment.
//
// The second return value is a one-line, user-facing notice. It is non-empty only
// when remote caching was requested — .putnami/cache.json or a PUTNAMI_CACHE_*
// override is present — but cannot be activated (no capable @putnami/cloud
// provider installed, or a gate failure), so the build silently degrading to
// local-only would surprise the user. The plain unconfigured case returns ""
// because local-only is the expected default there, not a degradation. Callers
// print a non-empty notice once; per the missing-cloud policy this path must
// never be silent.
//
// discovered carries discovery's unloadable-extension records and the
// provider capabilities a hosted run removed, so a configured cache whose
// provider was skipped (unparseable manifest, contract too new) or removed
// from a path extension degrades with the root cause in the notice
// (DiscoveryResult.ProviderCause) instead of a generic "not installed". Nil is
// fine when the caller has no discovery result.
//
// The context bounds provider detection and the preparation of the extension
// runtime the provider command references; provider startup is deferred to the
// first request that needs it.
func LoadRemoteCache(ctx context.Context, wsRoot string, exts []*extension.ExtensionDescription, discovered *extension.DiscoveryResult, trust store.CacheTrust, options ...RemoteCacheOption) (*RemoteCache, string) {
	if trust == store.CacheTrustNone {
		return nil, ""
	}
	if !trust.Valid() {
		// Fail closed if an internal caller bypassed CLI validation. An unknown
		// policy must never degrade into accepting hints on an authoritative run.
		return nil, "putnami: remote cache trust policy is unresolved; building locally"
	}
	provider, notice, err := loadProviderRemoteCache(ctx, wsRoot, exts, discovered, trust)
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

func loadRemoteCacheConfig(wsRoot string) (*remoteCacheConfig, error) {
	return loadRemoteCacheConfigFile(RemoteCacheConfigPath(wsRoot))
}

// Negotiate precomputes every cacheable key and hands the build's non-local,
// non-side-effecting key set to the provider so it can warm its caches before
// execution. The provider protocol has no batch negotiate result — hits are
// discovered lazily through Restore on a local miss — so this records the key set
// as misses up front and each restore reclassifies its key. Best-effort: any
// failure logs and leaves the cache effectively empty, so every key is a miss.
//
// It is also where the provider session is STARTED, and the trigger is not only
// the key set: a run whose jobs will execute locally needs the provider even with
// nothing to warm, because the object cache those jobs reach lives behind the
// same session (the socket path is negotiated at initialize).
func (r *RemoteCache) Negotiate(
	ctx context.Context,
	ws *workspace.Workspace,
	planned []*ScheduledJob,
	commandParams map[string]any,
	versions RunVersions,
	cm *store.CacheManager,
	bypass CacheBypass,
) {
	if r == nil || cm == nil {
		return
	}
	r.dependedOn = dependedOnJobs(planned)

	keys, err := PrecomputeKeys(ws, planned, commandParams, versions, cm, bypass, r.stats)
	if err != nil {
		slog.Warn("remote cache: key precompute failed; skipping remote cache", "error", err)
		return
	}
	r.keys = keys
	r.localHits = make(map[string]bool)

	var providerKeys []string
	// declaredJobs maps each declared-capture provider key to the jobs that
	// restore it, and declaredHashes maps it to its local cache key, so the
	// result-only subset can be chosen once the provider has answered
	// initialize.
	declaredJobs := make(map[string][]*ScheduledJob)
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
		if cache.SideEffectingTask(job.JobDef.Name) {
			executesLocally = true
			continue
		}
		// A declared-capture task negotiates the FORMAT-QUALIFIED address rather
		// than the raw key: that is where its entry lives in the remote cache, and
		// the separation is what keeps a pre-B4c binary from ever receiving a
		// format-2 payload under a key it would read as a legacy capture
		// (store/task_remote.go). localHits stays keyed by the local hash, since
		// that is the key the required-input bookkeeping speaks.
		if usesDeclaredCapture(job) {
			if entry, err := cm.LookupTaskEntry(hash); err == nil && entry != nil {
				r.localHits[hash] = true
				continue
			}
			providerKey := store.RemoteTaskEntryKey(hash)
			providerKeys = append(providerKeys, providerKey)
			declaredJobs[providerKey] = append(declaredJobs[providerKey], job)
			declaredHashes[providerKey] = hash
			executesLocally = true
			continue
		}
		// Skip keys already in the local store: a local hit is served locally — it
		// is never restored from or uploaded to the remote — so negotiating it only
		// adds latency. With every planned key a local hit there is nothing to warm,
		// so a fully warm rebuild never spins up the provider. (Under --no-cache
		// PrecomputeKeys omits every key upstream, so this loop is already empty.)
		if entry, err := cm.Lookup(hash); err == nil && entry != nil {
			r.localHits[hash] = true
			continue
		}
		providerKeys = append(providerKeys, hash)
		executesLocally = true
	}
	r.requiredInputs = r.computeRequiredInputs(planned)

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
	// Record the key set as misses; Restore promotes each served key to a hit.
	r.stats.recordNegotiate(time.Since(start).Nanoseconds(), len(providerKeys), 0)
}

// resultOnlyKeys returns the declared-capture provider keys, in providerKeys
// order and once each, whose files no job of this run reads. A key shared by
// several jobs is result-only only when none of them needs the files.
func (r *RemoteCache) resultOnlyKeys(providerKeys []string, declaredJobs map[string][]*ScheduledJob) []string {
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
	declaredJobs map[string][]*ScheduledJob,
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
				r.localHits[hash] = true
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
func (r *RemoteCache) filesNeededByAny(jobs []*ScheduledJob) bool {
	if len(jobs) == 0 {
		return true
	}
	for _, job := range jobs {
		if r.taskFilesNeeded(job) {
			return true
		}
	}
	return false
}

// Restore attempts to satisfy a local miss from the provider cache. On a provider
// hit it materializes the result (and, outside minimal mode, the output files)
// into the local store and returns the restored result; the entry is then
// indistinguishable from a local hit. It returns nil on a provider miss or any
// failure, so the caller falls back to building the job locally.
func (r *RemoteCache) Restore(ctx context.Context, hash string, job *ScheduledJob, cm *store.CacheManager) *JobResult {
	if r == nil || cm == nil {
		return nil
	}

	// The provider's Restore operation performs the network download into its
	// session-private exchange directory. Claim the machine-global action key
	// before invoking it so sibling worktrees wait for the resulting local entry
	// instead of each asking its provider session to download the same blobs.
	// Known-cheap jobs bypass coordination through the same break-even floor as
	// compute leases; expiry, timeout, cancellation, and store errors fall through
	// to the previous direct-provider behavior.
	winner, release := cm.TryClaim(hash, remoteRestoreEstimatedCost(job))
	if winner {
		defer release()
	} else if err := cm.WaitForPublish(ctx, hash, remoteRestoreLeaseWaitTimeout); err == nil {
		entry, lookupErr := cm.LookupAndTouch(hash)
		if lookupErr == nil && entry != nil && entry.Result != nil && !cacheHitMissingRequiredOutput(job, entry) {
			var durationMs, sizeBytes int64
			if entry.Metadata != nil {
				durationMs = entry.Metadata.DurationMs
				sizeBytes = entry.Metadata.Size
			}
			result := jobResultFromEntryResult(entry.Result)
			if sourceMutationTask(job) && result.SourceMutated {
				return nil
			}
			r.recordRestoredHit(
				hash,
				actionResultFromEntryResult(entry.Result, durationMs, sizeBytes, r.actionEvents),
				entry.Manifest,
				durationMs,
				0,
				time.Time{},
				time.Time{},
			)
			// A sibling's remote restore published this entry into the local store
			// while we waited; CacheStats already counted it as a remote hit, so the
			// provenance is remote even though the bytes came from the local store.
			result.MarkReuse(ReuseRemoteCache)
			return result
		}
	}

	sess, ok := r.ensureProvider(ctx, nil, cm)
	if !ok {
		return nil
	}
	restore, err := sess.Restore(ctx, &cache.RestoreParams{Key: hash})
	if err != nil {
		//nolint:gosec // G706 false positive: dynamic values are structured slog attributes, not a format string.
		slog.Warn("remote cache: provider restore failed; building locally",
			"project", job.Project.Name, "job", job.JobDef.Name, "error", err)
		return nil
	}
	if restore.Status != cache.RestoreHit || restore.Result == nil || restore.Manifest == nil {
		return nil
	}
	if !r.trust.Accepts(restore.Channel) {
		// A hint (or a channel-less legacy hit) is useful as bytes but not as
		// evidence. Verify and admit its manifest into the local CAS, then return
		// nil so the scheduler must execute the job. Crucially, PrefetchBlobs does
		// not publish result.json/meta.json under the action key, so neither this
		// worker nor a later local lookup can mistake the hint for a green result.
		start := time.Now()
		if err := cm.PrefetchBlobs(restore.Manifest, exchangeBlobFetcher(r.providerExchangeDir())); err != nil {
			//nolint:gosec // G706 false positive: dynamic values are structured slog attributes, not a format string.
			slog.Warn("remote cache: provider hint prefetch failed; building locally",
				"project", job.Project.Name, "job", job.JobDef.Name, "error", err)
			return nil
		}
		r.stats.recordFetch(manifestBytes(restore.Manifest))
		r.stats.recordMaterialize(start, time.Now())
		r.stats.recordHintWarm()
		return nil
	}
	// A required-input hit must materialize real output files — accepting an
	// empty manifest would leave the downstream local job without its
	// inputs — except when the entry is legitimately files-less: a
	// status-only optional capture (a describe run on a project with no
	// typed clients), a deterministic skip (a config-merge with nothing to
	// merge), or a task declared noOutput (tidy, lint — its result is pure
	// data), none of which capture artifacts locally either. Mirrors
	// cacheHitMissingRequiredOutput's asymmetry on the local-hit path. Logged
	// at debug: the rejection self-heals by rebuilding, and every legitimate
	// files-less shape is exempted above, so per-key noise helps only when
	// actively tracing a rebuild loop.
	if r.requiresMaterializedInput(hash) && len(restore.Manifest.Files) == 0 &&
		!statusOnlyOptionalCaptureHit(job, restore.Result) && !skippedFilesLessHit(restore.Result) &&
		!taskDeclaresNoOutput(job) {
		//nolint:gosec // G706 false positive: dynamic values are structured slog attributes, not a format string.
		slog.Debug("remote cache: provider hit has no output files for a required dependency; building locally",
			"project", job.Project.Name, "job", job.JobDef.Name)
		return nil
	}

	result := jobResultFromActionResult(restore.Result)
	if sourceMutationTask(job) && result.SourceMutated {
		return nil
	}

	entry := entryFromProviderRestore(restore, job)
	materializeStart := time.Now()
	if err := cm.Materialize(hash, entry, exchangeBlobFetcher(r.providerExchangeDir())); err != nil {
		// nolint:gosec // G706: the three logged values are workspace-manifest data,
		// not user input — job.Project.Name comes from package.json/go.mod,
		// job.JobDef.Name from an extension manifest, and err from a local
		// filesystem materialize. That is the same trust boundary .golangci.yml
		// already documents for G204 ("CLI spawns build tools from extension
		// manifests — not user input"), so the finding is a false positive of that
		// class rather than a new exposure.
		//
		// It first appeared when routing the watch loop through Engine.Run gave
		// gosec's INTERPROCEDURAL taint analysis a fresh path from the file
		// watcher into this call, without touching this file directly. The
		// values and their provenance are unchanged. Suppressed at the site rather
		// than added to the global gosec excludes, because MCP now accepts
		// agent-supplied input and disabling G706 tree-wide would give up the rule
		// exactly where it earns its keep.
		slog.Warn("remote cache: provider materialize failed; building locally",
			"project", job.Project.Name, "job", job.JobDef.Name, "error", err)
		return nil
	}
	materializeEnd := time.Now()
	r.recordRestoredHit(
		hash,
		restore.Result,
		restore.Manifest,
		restore.Result.DurationMs,
		manifestBytes(restore.Manifest),
		materializeStart,
		materializeEnd,
	)

	result.MarkReuse(ReuseRemoteCache)
	return result
}

func remoteRestoreEstimatedCost(job *ScheduledJob) time.Duration {
	if job == nil || job.ExpectedWallMs <= 0 {
		return 0
	}
	return time.Duration(job.ExpectedWallMs) * time.Millisecond
}

func (r *RemoteCache) recordRestoredHit(
	hash string,
	result *cache.ActionResult,
	manifest *cache.Manifest,
	durationMs int64,
	fetchedBytes int64,
	materializeStart time.Time,
	materializeEnd time.Time,
) {
	r.mu.Lock()
	if r.index == nil {
		r.index = make(map[string]cache.KeyResult)
	}
	r.index[hash] = cache.KeyResult{Key: hash, Hit: true, Result: result, Manifest: manifest}
	r.mu.Unlock()

	r.stats.recordFetch(fetchedBytes)
	r.stats.recordMaterialize(materializeStart, materializeEnd)
	r.stats.recordRestore(durationMs)
	// The provider has no batch negotiate result, so hits are discovered here, one
	// per successful restore. Negotiate seeded misses = keysRequested; move this
	// key into the hit column so a provider- or sibling-served run reports "N hit"
	// instead of "0 hit · N miss".
	r.stats.recordProviderHit()
}

// HasFiles reports whether the provider hit for hash carries output files, so the
// caller can decide whether to refresh the out/ symlink after a Restore.
func (r *RemoteCache) HasFiles(hash string) bool {
	if r == nil {
		return false
	}
	// index is mutated from worker goroutines as restores land (see Restore), so
	// this read must hold mu too — otherwise a HasFiles here can race a concurrent
	// index write and trip Go's fatal concurrent map read/write.
	r.mu.Lock()
	defer r.mu.Unlock()
	res, ok := r.index[hash]
	return ok && res.Manifest != nil && len(res.Manifest.Files) > 0
}

// Prefetch is the scheduler's pre-execution hook. The provider warms its caches
// from the key set handed to it in Negotiate, so there is no separate
// CLI-driven prefetch pass; this is retained as a no-op for the scheduler's call
// site and to keep a single place to document where speculative fetching lives.
func (r *RemoteCache) Prefetch(context.Context, []*ScheduledJob, *store.CacheManager) {}

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

// computeRequiredInputs returns dependency keys that feed a job which may execute
// locally: every planned job except those already served by a LOCAL hit. If such
// a dependency is a provider hit that materializes no files, accepting it would
// leave the downstream local job without its inputs, so the guard in Restore
// rebuilds the dependency instead.
//
// A local hit is excluded because it is served from the store and never executes,
// so its inputs are never consumed. A provider hit is deliberately NOT excluded:
// it can still fall back to local execution when its own materialize fails (e.g.
// a corrupt blob), and it then consumes its inputs like any built job. Treating a
// hit job's dependencies as optional is what let a files-less dependency hit
// silently satisfy a job that then rebuilt locally without its inputs.
func (r *RemoteCache) computeRequiredInputs(planned []*ScheduledJob) map[string]bool {
	if r == nil || len(r.keys) == 0 {
		return nil
	}
	out := make(map[string]bool)
	for _, job := range planned {
		if r.localHits[r.keys[job.Key()]] {
			continue
		}
		for _, depKey := range job.DependsOn {
			depHash := r.keys[depKey]
			if depHash != "" {
				out[depHash] = true
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (r *RemoteCache) requiresMaterializedInput(hash string) bool {
	return r != nil && r.requiredInputs[hash]
}

// statusOnlyOptionalCaptureHit reports whether a files-less restore is a
// legitimate status-only result rather than a missing required output. It holds
// only when the job's sole captured resource is optional (writesOnlyOptionalCapture
// — the clients-writing describe task) AND the restored result reports no clients
// directory. A files-less hit whose result still names a clients output is NOT
// status-only: materializeCachedOutput deletes that directory when the entry has
// no files, so accepting it would starve the downstream compile of the generated
// client sources — that case keeps the files-less rejection.
func statusOnlyOptionalCaptureHit(job *ScheduledJob, result *cache.ActionResult) bool {
	if !writesOnlyOptionalCapture(job) {
		return false
	}
	if result == nil {
		return true
	}
	return !reportsClientOutput(result.Data)
}

// skippedFilesLessHit reports whether a files-less restore carries a
// deterministic "skipped" result. A skip never captures artifacts locally
// (storeResult stores skips status-only, and the local-hit path accepts them),
// so an empty manifest is that entry's true shape: restoring it leaves the
// downstream job with exactly what a local re-execution would have produced —
// nothing. Rejecting these made every skip-resulting step that feeds another
// job a PERMANENT remote miss loop: the rejected hit re-executed, re-uploaded
// the same files-less entry, and was rejected again on every following build
// (46 config-merge keys per run in this workspace alone). The clients check
// mirrors statusOnlyOptionalCaptureHit: a result that names a
// generated clients directory is never accepted files-less, because
// materializeCachedOutput would delete that directory.
func skippedFilesLessHit(result *cache.ActionResult) bool {
	if result == nil || result.Status != "skipped" {
		return false
	}
	return !reportsClientOutput(result.Data)
}

// Upload schedules a freshly built local entry to be shared with the provider and
// returns immediately. The exchange runs on the bounded background pool so its
// round trips overlap the rest of the build rather than stalling the worker, and
// DrainUploads waits for the in-flight transfers at the end of the run.
// Best-effort: failures log and never fail the build.
func (r *RemoteCache) Upload(ctx context.Context, hash string, job *ScheduledJob, cm *store.CacheManager) {
	if r == nil || cm == nil {
		return
	}
	in, ok := r.prepareUpload(hash, job, cm)
	if !ok {
		return
	}

	r.uploadWG.Go(func() {
		select {
		case r.uploadSem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-r.uploadSem }()
		sess, ready := r.ensureProvider(ctx, nil, cm)
		if !ready {
			return
		}
		if err := cm.ExportManifestBlobs(in.Manifest, r.providerExchangeDir()); err != nil {
			r.stats.recordUploadFailure()
			slog.Warn("remote cache: provider export failed",
				"project", job.Project.Name, "job", job.JobDef.Name, "error", err)
			return
		}
		out, err := sess.Upload(ctx, &cache.UploadParams{Key: in.Key, Result: in.Result, Manifest: in.Manifest})
		if err != nil || out == nil || !out.Accepted {
			r.stats.recordUploadFailure()
			slog.Warn("remote cache: provider upload failed",
				"project", job.Project.Name, "job", job.JobDef.Name, "error", err)
		}
	})
}

// DrainUploads waits for the background uploads to finish, then asks the provider
// for its run summary (counts/bytes restored and uploaded) and records the wall
// time the write path added to the makespan after the build's own work
// completed. Safe to call when no upload was scheduled. The scheduler calls this
// only after all workers finish, so no Upload races the drain.
func (r *RemoteCache) DrainUploads() {
	if r == nil {
		return
	}
	start := time.Now()
	r.uploadWG.Wait()
	ctx, cancel := providerContext()
	r.providerDrain(ctx)
	cancel()
	r.stats.recordUploadWall(time.Since(start).Nanoseconds())
}

// prepareUpload turns a freshly built local entry into the input to share with
// the provider, applying the eligibility gates the write path always has: skip
// side-effecting tasks and entries below the break-even guard (counted so the
// summary can explain why a cheap miss was not shared). It returns ok=false when
// the entry should not be uploaded.
func (r *RemoteCache) prepareUpload(hash string, job *ScheduledJob, cm *store.CacheManager) (uploadInput, bool) {
	entry, err := cm.Lookup(hash)
	if err != nil || entry == nil || entry.Result == nil {
		return uploadInput{}, false
	}
	if cache.SideEffectingTask(job.JobDef.Name) {
		return uploadInput{}, false
	}

	var durationMs, sizeBytes int64
	if entry.Metadata != nil {
		durationMs = entry.Metadata.DurationMs
		sizeBytes = entry.Metadata.Size
	}
	// A status-only result (lint, test, describe, config-merge, …) produces a
	// pass/fail outcome but no output files, so it has no manifest locally. It is
	// still worth sharing: the cached status lets another machine skip the rebuild.
	// Share it with an empty-file manifest — the wire contract accepts a zero-file
	// manifest (the validators reject only a nil one), so the exchange transfers no
	// blobs and just registers the key→result mapping. Without this, every
	// lint/test/describe key was a permanent remote miss, which dominated the
	// workspace miss count.
	manifest := entry.Manifest
	if manifest == nil {
		manifest = &cache.Manifest{Files: []cache.FileEntry{}}
	}
	// The break-even guard prices the actual transfer, so it gets the
	// manifest's bytes — a files-less manifest moves nothing and is always
	// shared (WorthRemoteCaching), however cheap the task: excluding it made
	// its key a permanent remote miss that re-executed on every cold-store run.
	if !cache.EligibleForRemote(cache.KeyRequest{Task: job.JobDef.Name, DurationMs: durationMs, SizeBytes: manifestBytes(manifest)}, cache.DefaultBreakEven) {
		// Too cheap to be worth sharing: the predicted transfer would cost more than
		// the rebuild it saves. Counted so the summary can explain why a freshly
		// built miss never populated the remote cache — otherwise the skip is
		// invisible and the cache looks like it "does nothing".
		r.stats.recordUploadSkipped()
		return uploadInput{}, false
	}

	in := uploadInput{
		Key:      hash,
		Manifest: manifest,
		Result:   actionResultFromEntryResult(entry.Result, durationMs, sizeBytes, r.actionEvents),
	}
	return in, true
}

func entryFromProviderRestore(res *cache.RestoreResult, job *ScheduledJob) *store.Entry {
	return &store.Entry{
		Result:   entryResultFromActionResult(res.Result),
		Manifest: res.Manifest,
		Metadata: &store.EntryMetadata{
			Extension:  job.Extension.Name,
			Task:       job.JobDef.Name,
			Project:    job.Project.Name,
			DurationMs: res.Result.DurationMs,
		},
	}
}
