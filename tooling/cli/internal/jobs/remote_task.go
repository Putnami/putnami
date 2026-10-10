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
package jobs

import (
	"context"
	"log/slog"
	"time"

	cache "go.putnami.dev/protocol/cache"
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
	job *ScheduledJob,
	cm *store.CacheManager,
) (*store.TaskEntry, ReuseKind) {
	if r == nil || cm == nil || hash == "" {
		return nil, ReuseNone
	}

	// Claim the machine-global task-owned address before the network round trip,
	// exactly as the legacy path claims the action key: sibling worktrees then
	// wait for the resulting local entry instead of each asking their provider
	// session to download the same blobs. Expiry, timeout, cancellation and store
	// errors all fall through to the direct provider call below.
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
			return entry, ReuseCoalesced
		}
	}

	sess, ok := r.ensureProvider(ctx, nil, cm)
	if !ok {
		return nil, ReuseNone
	}
	restore, err := sess.Restore(ctx, &cache.RestoreParams{Key: store.RemoteTaskEntryKey(hash)})
	if err != nil {
		//nolint:gosec // G706 false positive: dynamic values are structured slog attributes, not a format string.
		slog.Warn("remote cache: provider restore failed; building locally",
			"project", job.Project.Name, "job", job.JobDef.Name, "error", err)
		return nil, ReuseNone
	}
	if restore.Status != cache.RestoreHit || restore.Result == nil || restore.Manifest == nil {
		return nil, ReuseNone
	}
	if !r.trust.Accepts(restore.Channel) {
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
			return nil, ReuseNone
		}
		r.stats.recordFetch(manifestBytes(restore.Manifest))
		r.stats.recordMaterialize(start, time.Now())
		r.stats.recordHintWarm()
		return nil, ReuseNone
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
		Result:   entryResultFromActionResult(restore.Result),
		Metadata: taskEntryMetadata(job, restore.Result.DurationMs),
		Manifest: restore.Manifest,
	}, exchangeBlobFetcher(r.providerExchangeDir()))
	if err != nil {
		// Fail closed, exactly as the local lookup does: a legacy-format payload
		// under this address, a torn descriptor, or a manifest disagreeing with
		// the descriptor is a MISS, never a partial restore. Debug level — the
		// rejection self-heals by rebuilding, and per-key noise helps only when
		// actively tracing a rebuild loop.
		//nolint:gosec // G706 false positive: dynamic values are structured slog attributes, not a format string.
		slog.Debug("remote cache: task-owned hit is not interpretable; building locally",
			"project", job.Project.Name, "job", job.JobDef.Name, "error", err)
		return nil, ReuseNone
	}
	r.recordTaskRestoredHit(hash, entry, manifestBytes(restore.Manifest), materializeStart, time.Now())
	return entry, ReuseRemoteCache
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
		actionResultFromEntryResult(entry.Result, durationMs, sizeBytes, r.actionEvents),
		entry.Manifest,
		durationMs,
		fetchedBytes,
		materializeStart,
		materializeEnd,
	)
}

// taskUploadInput is uploadInput plus the two pieces the task-owned exchange
// needs: the CAS-backed payload subset to export, and the descriptor bytes to
// stage (the CAS never holds them — see store/task_remote.go).
type taskUploadInput struct {
	uploadInput
	payload          *cache.Manifest
	descriptor       []byte
	descriptorDigest string
}

// UploadTaskEntry schedules a freshly published task-owned entry to be shared
// with the provider and returns immediately, on the same bounded background pool
// as the legacy upload path. Best-effort: failures log and never fail the build.
func (r *RemoteCache) UploadTaskEntry(ctx context.Context, hash string, job *ScheduledJob, cm *store.CacheManager) {
	if r == nil || cm == nil {
		return
	}
	in, ok := r.prepareTaskUpload(hash, job, cm)
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
		if err := cm.ExportManifestBlobs(in.payload, exchangeDir); err != nil {
			r.stats.recordUploadFailure()
			slog.Warn("remote cache: provider export failed",
				"project", job.Project.Name, "job", job.JobDef.Name, "error", err)
			return
		}
		if err := store.StageExchangeBlob(exchangeDir, in.descriptorDigest, in.descriptor); err != nil {
			r.stats.recordUploadFailure()
			slog.Warn("remote cache: provider descriptor export failed",
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

// prepareTaskUpload turns a published task-owned entry into the input to share
// with the provider, applying the same eligibility rule as the legacy write
// path: every entry is shared whatever its build duration or size, except a
// side-effecting task's (cache.SideEffectingTask). It returns ok=false when the
// entry should not be uploaded.
func (r *RemoteCache) prepareTaskUpload(hash string, job *ScheduledJob, cm *store.CacheManager) (taskUploadInput, bool) {
	entry, err := cm.LookupTaskEntry(hash)
	if err != nil || entry == nil || entry.Result == nil {
		return taskUploadInput{}, false
	}
	if cache.SideEffectingTask(job.JobDef.Name) {
		return taskUploadInput{}, false
	}

	transfer, err := store.NewTaskEntryTransfer(entry)
	if err != nil {
		slog.Warn("remote cache: task-owned entry cannot be prepared for upload",
			"project", job.Project.Name, "job", job.JobDef.Name, "error", err)
		return taskUploadInput{}, false
	}

	var durationMs, sizeBytes int64
	if entry.Metadata != nil {
		durationMs = entry.Metadata.DurationMs
		sizeBytes = entry.Metadata.Size
	}

	return taskUploadInput{
		uploadInput: uploadInput{
			Key:      transfer.Key,
			Result:   actionResultFromEntryResult(entry.Result, durationMs, sizeBytes, r.actionEvents),
			Manifest: transfer.Manifest,
		},
		payload:          transfer.Payload,
		descriptor:       transfer.Descriptor,
		descriptorDigest: transfer.DescriptorDigest,
	}, true
}

// taskEntryMetadata builds the provenance sidecar for an entry this machine is
// publishing from a remote hit, matching what a local capture records.
func taskEntryMetadata(job *ScheduledJob, durationMs int64) *store.EntryMetadata {
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
