// Remote delivery for declared-capture tasks.
//
// An earlier change switched v3 tasks onto the task-owned entry model but left
// them off the remote cache: a declared miss went straight to the local
// claim-or-wait, and a declared hit was never shared. This file removes that
// hole without adding a second restore path.
//
// The shape is deliberately the same as the legacy RemoteCache.Restore/Upload
// pair — claim the machine-global address, ask the provider, verify, publish
// locally — and stops there. It never touches the workspace: a successful
// restore returns the PUBLISHED LOCAL ENTRY, and the scheduler then materializes
// it through restoreDeclaredCacheHit, the same function that serves a local hit
// and a coalesced hit. So remote, local and coalesced share one materialize (the
// one atomic primitive), and a remote hit is byte-identical to a local one by
// construction rather than by review.
//
// Two rules of the legacy remote path are deliberately NOT carried over, because
// the declared model makes them unrepresentable rather than merely unnecessary:
//
//   - The files-less acceptance rules (statusOnlyOptionalCaptureHit,
//     skippedFilesLessHit, taskDeclaresNoOutput) exist because a legacy
//     manifest cannot say whether "no files" means "produced
//     nothing" or "captured nothing". A task-owned descriptor states it per
//     output: a files-less entry means every declared output is recorded EMPTY,
//     and IngestTaskEntry refuses to publish an entry whose required output was
//     not produced. The acceptance question is answered by the payload itself.
//   - The required-input bookkeeping (requiredInputs/HasFiles) exists for the
//     same reason and is likewise subsumed: an accepted task-owned hit restores
//     exactly the declared set, so a downstream job cannot be left without an
//     input the entry claimed to hold.
//
// What IS carried over unchanged: the trust policy (a hint warms blobs and can
// never return a green result), the source-mutation rejection, side-effecting
// task exclusion, and best-effort semantics — every failure logs and degrades
// to local execution, never to a broken build.
//
// A hit whose files no job of the run reads takes the result-only path instead
// (remote_result_only.go). That path writes nothing, so it cannot leave a reader
// without an input: a task a planned job waits for always comes through here.
package remotecache

import (
	"context"
	"log/slog"
	"time"

	jobmodel "go.putnami.dev/cli/model/jobs"
	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/store"
)

// RestoreTaskEntry attempts to satisfy a declared-capture miss from the
// provider, returning the task-owned entry it published into the local store
// and how that entry was obtained.
//
// It returns (nil, ReuseNone) for a provider miss, an untrusted hit, a payload
// this build cannot interpret, or any failure — every one of which leaves the
// caller to execute the task. Crucially it returns an ENTRY, not a result: the
// caller materializes it through the same path a local hit takes, so there is no
// remote-specific delivery to keep in sync.
func (r *RemoteCache) RestoreTaskEntry(
	ctx context.Context,
	hash string,
	job *jobmodel.ScheduledJob,
	cm *store.CacheManager,
) (*store.TaskEntry, jobmodel.ReuseKind) {
	if r == nil || cm == nil || hash == "" {
		return nil, jobmodel.ReuseNone
	}
	entry, reuse, _ := r.restoreTaskEntry(ctx, hash, job, cm, true)
	return entry, reuse
}

// FetchTaskEntry fetches the task-owned entry at hash for a task this run did
// not plan, and publishes it into the local store for a reader of the store:
// the spec gate reads an unplanned attester's verification report in place
// from it. It never writes the workspace.
//
// It shares RestoreTaskEntry's lease, provider restore, trust policy and local
// publication. No planned job consumes the entry, so it counts no task reuse
// (no provider hit, no restore, no time saved): the run's cache stats gain only
// the bytes it fetched and the time it spent materializing them. An untrusted
// hit is a miss and warms nothing.
//
// When this call is the first operation that needs the provider, the provider
// starts under ctx, as it does for any operation of the run: its process lives
// as long as the context it starts under. within bounds the rest, the wait on
// a lease another process holds and the provider round trip, so neither keeps
// the caller longer than that.
//
// It returns (nil, nil) for a provider miss, an untrusted hit, and a provider
// that serves nothing by configuration. It returns an error when the provider
// cannot answer in time or at all, when it answers that it could not restore
// the entry (cache.RestoreError), or when its payload is not interpretable.
// Each lookup that reached the provider, or failed to, counts in the run's
// cache stats (CacheStats.RecordRemoteLookup).
func (r *RemoteCache) FetchTaskEntry(
	ctx context.Context,
	within time.Duration,
	hash string,
	job *jobmodel.ScheduledJob,
	cm *store.CacheManager,
) (*store.TaskEntry, error) {
	if !r.IsProviderBacked() || cm == nil || hash == "" {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, ok := r.ensureProvider(ctx, nil, cm); !ok {
		r.recordUnavailable()
		return nil, r.provider.unavailable()
	}
	bounded, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	entry, _, err := r.restoreTaskEntry(bounded, hash, job, cm, false)
	return entry, err
}

// restoreTaskEntry is the provider restore of a task-owned entry that
// RestoreTaskEntry and FetchTaskEntry share. planned says whether a planned
// job consumes the entry: only then does an untrusted hit warm the CAS for the
// task's execution, does a fetched hit count as a task reuse, and does a
// failure log that the task builds locally. The error says why the provider
// did not answer; a miss and an untrusted hit carry none.
func (r *RemoteCache) restoreTaskEntry(
	ctx context.Context,
	hash string,
	job *jobmodel.ScheduledJob,
	cm *store.CacheManager,
	planned bool,
) (*store.TaskEntry, jobmodel.ReuseKind, error) {
	// Claim the machine-global task-owned address before the network round trip,
	// exactly as the legacy path claims the action key: sibling worktrees then
	// wait for the resulting local entry instead of each asking their provider
	// session to download the same blobs. Expiry, timeout and store errors all
	// fall through to the direct provider call below.
	winner, release := cm.TryClaimTaskEntry(hash, remoteRestoreEstimatedCost(job))
	if winner {
		defer release()
	} else if err := cm.WaitForTaskEntry(ctx, hash, remoteRestoreLeaseWaitTimeout); err == nil {
		if entry, lookupErr := cm.LookupTaskEntry(hash); lookupErr == nil && entry != nil {
			// The lease owner published while we waited. Whether it restored the
			// entry from the provider or executed the task is not observable from
			// here, so this is reported as COALESCED and deliberately NOT counted as
			// a provider hit: the owner's own run already accounts for whatever it
			// did. (The legacy path attributes this case to the remote instead;
			// keeping that would put a local execution in the provider hit column.)
			return entry, jobmodel.ReuseCoalesced, nil
		}
	}
	// A caller whose context ended asks the provider nothing: the request would
	// still reach it, and it would download an entry nobody reads.
	if err := ctx.Err(); err != nil {
		return nil, jobmodel.ReuseNone, err
	}

	sess, ok := r.ensureProvider(ctx, nil, cm)
	if !ok {
		r.recordUnavailable()
		return nil, jobmodel.ReuseNone, r.provider.unavailable()
	}
	restore, err := sess.Restore(ctx, &cache.RestoreParams{Key: store.RemoteTaskEntryKey(hash)})
	r.recordLookup(restoreFailure(restore, err))
	if err != nil {
		if planned {
			//nolint:gosec // G706 false positive: dynamic values are structured slog attributes, not a format string.
			slog.Warn("remote cache: provider restore failed; building locally",
				"project", job.Project.Name, "job", job.JobDef.Name, "error", err)
		}
		return nil, jobmodel.ReuseNone, err
	}
	if restore.Status == cache.RestoreError {
		// The provider could not reach the entry: not a miss, so a reader of
		// the store learns the remote cache did not answer.
		return nil, jobmodel.ReuseNone, errProviderRestoreFailed
	}
	if restore.Status != cache.RestoreHit || restore.Result == nil || restore.Manifest == nil {
		return nil, jobmodel.ReuseNone, nil
	}
	if !r.Trust.Accepts(restore.Channel) {
		if !planned {
			// No job of this run executes the task, so there is no execution for
			// the hint's bytes to warm.
			return nil, jobmodel.ReuseNone, nil
		}
		// A hint (or a channel-less legacy hit) is useful as bytes but not as
		// evidence: verify and admit its blobs into the CAS, then report a miss so
		// the scheduler executes the task. No entry is published, so neither this
		// worker nor a later lookup can mistake the hint for a green result. The
		// descriptor blob rides along into the CAS here (it is in the manifest and
		// PrefetchBlobs is deliberately format-agnostic); it is a few hundred
		// unreferenced bytes that the next GC sweep reclaims, which is cheaper than
		// teaching the prefetch path to know about entry formats.
		start := time.Now()
		if err := cm.PrefetchBlobs(restore.Manifest, exchangeBlobFetcher(r.providerExchangeDir())); err != nil {
			//nolint:gosec // G706 false positive: dynamic values are structured slog attributes, not a format string.
			slog.Warn("remote cache: provider hint prefetch failed; building locally",
				"project", job.Project.Name, "job", job.JobDef.Name, "error", err)
			return nil, jobmodel.ReuseNone, err
		}
		r.stats.RecordFetch(ManifestBytes(restore.Manifest))
		r.stats.RecordMaterialize(start, time.Now())
		r.stats.RecordHintWarm()
		return nil, jobmodel.ReuseNone, nil
	}
	// No source-mutation rejection here, unlike the legacy remote path — but for
	// a different reason than "no such entry exists". A v3 source rewriter DOES
	// publish under this address now (clean-only status entries), so the rule is
	// enforced where every restore leg converges instead: a mutating result is
	// never uploaded (scheduler_exec.go), and restoreDeclaredCacheHit rejects the
	// marker whatever produced the entry. Returning the ENTRY rather than a
	// result is what makes that one rejection cover this path too.
	materializeStart := time.Now()
	entry, err := cm.MaterializeTaskEntry(store.RemoteTaskEntryHit{
		Key:      hash,
		Result:   cachekey.EntryResultFromActionResult(restore.Result),
		Metadata: taskEntryMetadata(job, restore.Result.DurationMs),
		Manifest: restore.Manifest,
	}, exchangeBlobFetcher(r.providerExchangeDir()))
	if err != nil {
		// Fail closed, exactly as the local lookup does: a legacy-format payload
		// under this address, a torn descriptor, or a manifest disagreeing with
		// the descriptor is a MISS, never a partial restore. Debug level — the
		// rejection self-heals by rebuilding, and per-key noise helps only when
		// actively tracing a rebuild loop.
		if planned {
			//nolint:gosec // G706 false positive: dynamic values are structured slog attributes, not a format string.
			slog.Debug("remote cache: task-owned hit is not interpretable; building locally",
				"project", job.Project.Name, "job", job.JobDef.Name, "error", err)
		}
		return nil, jobmodel.ReuseNone, err
	}
	materializeEnd := time.Now()
	if planned {
		r.recordTaskRestoredHit(hash, entry, ManifestBytes(restore.Manifest), materializeStart, materializeEnd)
	} else {
		// A fetch for a reader of the store is transfer, not reuse.
		r.stats.RecordFetch(ManifestBytes(restore.Manifest))
		r.stats.RecordMaterialize(materializeStart, materializeEnd)
	}
	return entry, jobmodel.ReuseRemoteCache, nil
}

// recordTaskRestoredHit folds a task-owned provider hit into this run's cache
// stats through the legacy accounting, so hits, fetched bytes and saved time
// read the same however the entry model differs. It is called only for a hit
// this call actually fetched: an entry a sibling published while we waited is
// reported as coalesced and left out of the provider hit column.
func (r *RemoteCache) recordTaskRestoredHit(
	hash string,
	entry *store.TaskEntry,
	fetchedBytes int64,
	materializeStart, materializeEnd time.Time,
) {
	var durationMs, sizeBytes int64
	if entry.Metadata != nil {
		durationMs = entry.Metadata.DurationMs
		sizeBytes = entry.Metadata.Size
	}
	r.recordRestoredHit(
		hash,
		cachekey.ActionResultFromEntryResult(entry.Result, durationMs, sizeBytes, r.actionEvents),
		entry.Manifest,
		durationMs,
		fetchedBytes,
		materializeStart,
		materializeEnd,
	)
}

// TaskUploadInput is uploadInput plus the two pieces the task-owned exchange
// needs: the CAS-backed payload subset to export, and the descriptor bytes to
// stage (the CAS never holds them — see store/task_remote.go).
type TaskUploadInput struct {
	uploadInput
	Payload          *cache.Manifest
	descriptor       []byte
	descriptorDigest string
}

// UploadTaskEntry schedules a freshly published task-owned entry to be shared
// with the provider and returns immediately, on the same bounded background pool
// as the legacy upload path. Best-effort: failures log and never fail the build.
func (r *RemoteCache) UploadTaskEntry(ctx context.Context, hash string, job *jobmodel.ScheduledJob, cm *store.CacheManager) {
	if r == nil || cm == nil {
		return
	}
	in, ok := r.PrepareTaskUpload(hash, job, cm)
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
		exchangeDir := r.providerExchangeDir()
		if err := cm.ExportManifestBlobs(in.Payload, exchangeDir); err != nil {
			r.stats.RecordUploadFailure()
			slog.Warn("remote cache: provider export failed",
				"project", job.Project.Name, "job", job.JobDef.Name, "error", err)
			return
		}
		if err := store.StageExchangeBlob(exchangeDir, in.descriptorDigest, in.descriptor); err != nil {
			r.stats.RecordUploadFailure()
			slog.Warn("remote cache: provider descriptor export failed",
				"project", job.Project.Name, "job", job.JobDef.Name, "error", err)
			return
		}
		out, err := sess.Upload(ctx, &cache.UploadParams{Key: in.Key, Result: r.providerResult(in.Result), Manifest: in.Manifest})
		if err != nil || out == nil || !out.Accepted {
			r.stats.RecordUploadFailure()
			slog.Warn("remote cache: provider upload failed",
				"project", job.Project.Name, "job", job.JobDef.Name, "error", err)
		}
	})
}

// prepareTaskUpload turns a published task-owned entry into the input to share
// with the provider, applying the same eligibility rule as the legacy write
// path: every entry is shared whatever its build duration or size, except the
// entry of a job with external effects (jobmodel.HasExternalEffects). The
// input keeps the cached events, which the upload drops when the provider did
// not advertise action-events (providerResult). It returns ok=false when the
// entry should not be uploaded.
func (r *RemoteCache) PrepareTaskUpload(hash string, job *jobmodel.ScheduledJob, cm *store.CacheManager) (TaskUploadInput, bool) {
	entry, err := cm.LookupTaskEntry(hash)
	if err != nil || entry == nil || entry.Result == nil {
		return TaskUploadInput{}, false
	}
	if jobmodel.HasExternalEffects(job) {
		return TaskUploadInput{}, false
	}

	transfer, err := store.NewTaskEntryTransfer(entry)
	if err != nil {
		slog.Warn("remote cache: task-owned entry cannot be prepared for upload",
			"project", job.Project.Name, "job", job.JobDef.Name, "error", err)
		return TaskUploadInput{}, false
	}

	var durationMs, sizeBytes int64
	if entry.Metadata != nil {
		durationMs = entry.Metadata.DurationMs
		sizeBytes = entry.Metadata.Size
	}

	return TaskUploadInput{
		uploadInput: uploadInput{
			Key:      transfer.Key,
			Result:   cachekey.ActionResultFromEntryResult(entry.Result, durationMs, sizeBytes, true),
			Manifest: transfer.Manifest,
		},
		Payload:          transfer.Payload,
		descriptor:       transfer.Descriptor,
		descriptorDigest: transfer.DescriptorDigest,
	}, true
}

// taskEntryMetadata builds the provenance sidecar for an entry this machine is
// publishing from a remote hit, matching what a local capture records.
func taskEntryMetadata(job *jobmodel.ScheduledJob, durationMs int64) *store.EntryMetadata {
	meta := &store.EntryMetadata{DurationMs: durationMs}
	if job == nil {
		return meta
	}
	if job.Extension != nil {
		meta.Extension = job.Extension.Name
	}
	if job.JobDef != nil {
		meta.Task = job.JobDef.Name
	}
	if job.Project != nil {
		meta.Project = job.Project.Name
	}
	return meta
}
