package remotecache

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"

	cacheserverclient "go.putnami.dev/cloud/clients/cache-server/go"
	cache "go.putnami.dev/protocol/cache"
	diag "go.putnami.dev/protocol/diagnostic"
)

// The batched write path (protocol/cache v1.x). store/commit finalize one key
// per round trip, so a window of M built misses costs ~2M control round trips.
// These collapse that: FindMissing dedups a whole window's blobs in one request
// and CommitBatch finalizes the window's entries in one request. Capabilities
// lets the client discover whether the server implements the batched path and
// fall back to store/commit otherwise. Bytes still stream per blob, except the
// small ones: UploadBatch carries those inline, a few requests per window, on
// a server that advertises it.

// Capabilities probes the server for the optional capabilities it supports
// beyond the v1 baseline. The endpoint is a discovery PROBE only, and the server
// handler's documented contract (cache-server internal/api/write capabilities)
// is that the client "switches to the batched path only when find-missing is
// advertised, falling back to store/commit on any non-2xx." So ANY received
// non-2xx status is reported as an empty capability set, not an error, and the
// caller degrades cleanly to the store/commit baseline: a 404 (server predates
// this endpoint) as before, and now also a 401/403 (this credential lacks
// cache.read at the introspection layer — a workspace machine token without that
// scope) or any other non-2xx. Degrading the probe never masks a
// real auth failure: negotiate/store/commit carry their own bearer and surface
// their own non-2xx errors. Only a transport fault — a nil response or a
// dial/read error — still errors here.
func (c *Client) Capabilities(ctx context.Context) (*cache.CapabilitiesResponse, error) {
	data, status, err := c.doAuthed(ctx, "capabilities", c.controlAttempt("capabilities", false, func(ctx context.Context, api *cacheserverclient.CacheClient) error {
		_, err := api.ListV1CacheCapabilities(ctx, cacheserverclient.ListV1CacheCapabilitiesInput{})
		return err
	}))
	if err != nil {
		return nil, err
	}

	// The probe degrades to the baseline (store/commit) on ANY non-2xx — the
	// server handler's documented contract. A 404 means the server predates the
	// endpoint; a 401/403 means this credential lacks cache.read at introspection
	// (workspace machine tokens); any other non-2xx is treated the same.
	// The client falls back cleanly rather than failing the build; the real
	// store/commit calls still surface their own auth errors. Transport faults
	// (a nil response, a dial/read error) already returned above — only a received
	// HTTP status degrades here. A 401 on a renewable token source is retried
	// once with a re-minted bearer before it degrades (doAuthed), so an expired
	// bearer no longer silently strands the run on the unbatched write path.
	if status < 200 || status >= 300 {
		return &cache.CapabilitiesResponse{ProtocolVersion: cache.ProtocolVersion}, nil
	}

	out, diags := cache.ParseAndValidateCapabilitiesResponse(data)
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("invalid capabilities response: %s", firstError(diags))
	}
	return out, nil
}

// FindMissing reports which of the given blobs the CAS still needs, returning
// presigned PUT transfers for only those (the batched analog of Store).
func (c *Client) FindMissing(ctx context.Context, req *cache.FindMissingRequest) (*cache.FindMissingResponse, error) {
	if diags := cache.ValidateFindMissingRequest(req); diag.HasErrors(diags) {
		return nil, fmt.Errorf("invalid find-missing request: %s", firstError(diags))
	}

	data, err := postControl(ctx, c, "find-missing", req, (*cacheserverclient.CacheClient).CreateV1CacheFindMissing)
	if err != nil {
		return nil, err
	}

	out, diags := cache.ParseAndValidateFindMissingResponse(data)
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("invalid find-missing response: %s", firstError(diags))
	}
	allowed := make(map[string]bool, len(req.Blobs))
	for _, b := range req.Blobs {
		allowed[b.Digest] = true
	}
	for _, up := range out.Uploads {
		if !allowed[up.Digest] {
			return nil, fmt.Errorf("find-missing response requested upload for digest %s outside request", up.Digest)
		}
	}
	c.writes.accept()
	return out, nil
}

// CommitBatch finalizes several Action Cache entries in one request (the batched
// analog of Commit). Each entry is self-contained and idempotent.
func (c *Client) CommitBatch(ctx context.Context, req *cache.CommitBatchRequest) (*cache.CommitBatchResponse, error) {
	if diags := cache.ValidateCommitBatchRequest(req); diag.HasErrors(diags) {
		return nil, fmt.Errorf("invalid commit-batch request: %s", firstError(diags))
	}

	data, err := postControl(ctx, c, "commit-batch", req, (*cacheserverclient.CacheClient).CreateV1CacheCommitBatch)
	if err != nil {
		return nil, err
	}

	out, diags := cache.ParseAndValidateCommitBatchResponse(data)
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("invalid commit-batch response: %s", firstError(diags))
	}
	requested := make(map[string]bool, len(req.Entries))
	for _, e := range req.Entries {
		requested[e.Key] = true
	}
	seen := make(map[string]bool, len(out.Results))
	for _, res := range out.Results {
		if !requested[res.Key] {
			return nil, fmt.Errorf("commit-batch response key %q was not requested", res.Key)
		}
		if seen[res.Key] {
			return nil, fmt.Errorf("commit-batch response contains duplicate result for key %q", res.Key)
		}
		seen[res.Key] = true
	}
	for key := range requested {
		if !seen[key] {
			return nil, fmt.Errorf("commit-batch response missing result for key %q", key)
		}
	}
	c.writes.accept()
	return out, nil
}

// UploadBatch stores several small blobs inline in one request (the analog of
// Bazel's BatchUpdateBlobs). Used only when the server advertises
// CapabilityUploadBatch. cache-server serves the route and stores the blobs of
// one request as one pack object, so the call goes through its generated
// client like the other control operations.
func (c *Client) UploadBatch(ctx context.Context, req *cache.UploadBatchRequest) (*cache.UploadBatchResponse, error) {
	if diags := cache.ValidateUploadBatchRequest(req); diag.HasErrors(diags) {
		return nil, fmt.Errorf("invalid upload-batch request: %s", firstError(diags))
	}

	data, err := postControl(ctx, c, "upload-batch", req, (*cacheserverclient.CacheClient).CreateV1CacheUploadBatch)
	if err != nil {
		return nil, err
	}

	out, diags := cache.ParseAndValidateUploadBatchResponse(data)
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("invalid upload-batch response: %s", firstError(diags))
	}
	c.writes.accept()
	return out, nil
}

// DownloadBatch fetches several small blobs inline in one request (the read
// path analog of UploadBatch). Used only when the server advertises
// CapabilityDownloadBatch.
func (c *Client) DownloadBatch(ctx context.Context, req *cache.DownloadBatchRequest) (*cache.DownloadBatchResponse, error) {
	if diags := cache.ValidateDownloadBatchRequest(req); diag.HasErrors(diags) {
		return nil, fmt.Errorf("invalid download-batch request: %s", firstError(diags))
	}

	data, err := postControl(ctx, c, "download-batch", req, (*cacheserverclient.CacheClient).CreateV1CacheDownloadBatch)
	if err != nil {
		return nil, err
	}

	out, diags := cache.ParseAndValidateDownloadBatchResponse(data)
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("invalid download-batch response: %s", firstError(diags))
	}
	requested := make(map[string]bool, len(req.Digests))
	for _, d := range req.Digests {
		requested[d] = true
	}
	seen := make(map[string]bool, len(out.Blobs))
	for _, b := range out.Blobs {
		if !requested[b.Digest] {
			return nil, fmt.Errorf("download-batch response included digest %s outside request", b.Digest)
		}
		if seen[b.Digest] {
			return nil, fmt.Errorf("download-batch response contains duplicate blob %s", b.Digest)
		}
		seen[b.Digest] = true
	}
	return out, nil
}

// PresenceCache reports whether a CAS blob is already known to be present
// remotely. StoreBatch consults it to skip find-missing and upload for blobs a
// prior run already confirmed, trimming round trips and bytes on a warm cache. A
// nil PresenceCache treats every blob as unknown (the safe default).
type PresenceCache interface {
	Present(digest string) bool
}

// BatchOutcome reports what a batch flush moved and which entries committed.
// The byte counts split the window's distinct blobs into uploaded versus what
// the server already had in CAS (dedup). Confirmed lists the digests this flush
// verified present in CAS (uploaded or server-deduped), so the caller can add
// them to its PresenceCache; it deliberately excludes digests that were skipped
// because the PresenceCache already had them.
//
// Failed holds, by key, why an entry was not stored: a blob of its manifest
// could not be uploaded, or its own store failed after the window was split.
// Such an entry was never sent to commit-batch with a blob missing, so it is
// never in Committed. An entry in neither map was sent and the server answered
// that it did not commit it.
type BatchOutcome struct {
	Committed     map[string]bool
	Failed        map[string]error
	BlobsUploaded int
	BytesUploaded int64
	BytesDeduped  int64
	Confirmed     []string
}

// WritePath selects how StoreBatch uploads a window's blobs, from the server's
// advertised capabilities. The zero value is the baseline find-missing path.
type WritePath struct {
	// Grant, when non-nil (CapabilityDirectCASPut), uploads blobs straight to CAS
	// via a conditional PUT instead of a find-missing round trip.
	Grant *cache.UploadGrant
	// CoalesceSmall, when true (CapabilityUploadBatch), sends blobs at or below
	// cache.MaxInlineBlobBytes inline in one upload-batch request; larger blobs
	// still take the Grant/find-missing path.
	CoalesceSmall bool
	// BlobConcurrency bounds how many blobs upload at once over their presigned
	// PUTs after a find-missing. Zero selects blobUploadConcurrency, the bound
	// for one key's blobs; a caller that stores many keys in one window raises it.
	BlobConcurrency int
	// Retry bounds the retries of a control exchange that fails for a transient
	// reason, and says whether a window that still fails is split. The zero
	// value sends each exchange once and splits nothing.
	Retry WriteRetry
}

// TaskWritePath reads the task-cache write path from a capabilities probe.
// batched is true only when the server advertises both find-missing and
// commit-batch, the two exchanges StoreBatch sends; otherwise the caller stores
// entry by entry with StoreResult, which every server serves. A nil probe
// selects the per-entry path. CoalesceSmall follows upload-batch, as it does for
// ObjectWritePath.
func TaskWritePath(caps *cache.CapabilitiesResponse) (wp WritePath, batched bool) {
	if caps == nil {
		return WritePath{}, false
	}
	var findMissing, commitBatch bool
	for _, capability := range caps.Capabilities {
		switch capability {
		case cache.CapabilityFindMissing:
			findMissing = true
		case cache.CapabilityCommitBatch:
			commitBatch = true
		case cache.CapabilityUploadBatch:
			wp.CoalesceSmall = true
		}
	}
	if !findMissing || !commitBatch {
		return WritePath{}, false
	}
	return wp, true
}

// blobState is what one StoreBatch or StoreObjects call knows about its blobs.
// It outlives the passes of the call (the window, the entries of a split
// window, the recovery of an evicted blob), so no pass sends a blob that an
// earlier pass stored, and none tries again a blob whose upload failed.
type blobState struct {
	mu  sync.Mutex
	out *BatchOutcome
	// stored holds the digests this call saw in CAS: it uploaded them, or the
	// server reported that it already had them.
	stored map[string]bool
	// failed holds the digests whose upload failed, with the cause. A failed
	// digest stays failed for the whole call.
	failed map[string]error
	// first is the first failure recorded.
	first error
}

func newBlobState(out *BatchOutcome) *blobState {
	return &blobState{out: out, stored: make(map[string]bool), failed: make(map[string]error)}
}

// uploaded records a blob this call stored.
func (st *blobState) uploaded(digest string, size int64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.stored[digest] = true
	st.out.BlobsUploaded++
	st.out.BytesUploaded += size
	st.out.Confirmed = append(st.out.Confirmed, digest)
}

// present records a blob the server already had.
func (st *blobState) present(digest string, size int64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.stored[digest] = true
	st.out.BytesDeduped += size
	st.out.Confirmed = append(st.out.Confirmed, digest)
}

// fail records a blob that could not be uploaded.
func (st *blobState) fail(digest string, err error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, already := st.failed[digest]; already {
		return
	}
	st.failed[digest] = err
	if st.first == nil {
		st.first = err
	}
}

// settled reports whether the call already has an answer for the blob: it is
// stored, or its upload failed.
func (st *blobState) settled(digest string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.stored[digest] {
		return true
	}
	_, failed := st.failed[digest]
	return failed
}

// failure returns why a manifest cannot be stored: the first of its blobs, in
// file order, whose upload failed. It returns nil when none failed.
func (st *blobState) failure(m *cache.Manifest) error {
	if m == nil {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, f := range m.Files {
		if err, failed := st.failed[f.Digest]; failed {
			return err
		}
	}
	return nil
}

// firstFailure returns the first blob failure of the call, or nil.
func (st *blobState) firstFailure() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.first
}

// StoreBatch runs the batched write path for a window of built entries (the same
// StoreInput as the per-job StoreResult): upload the window's blobs, then
// commit-batch (one round trip). Blobs are deduped across the whole window, so a
// blob shared by two entries (or files) is uploaded once. wp picks the upload
// mechanism from the server's capabilities — small blobs may be coalesced into
// an upload-batch request and the rest go via the direct grant or find-missing;
// commit-batch always finalizes.
//
// A failure costs as few entries as it can:
//
//   - A blob that cannot be uploaded (its PUT failed, or its bytes cannot be
//     read) fails only the entries whose manifest names it. They are reported
//     in BatchOutcome.Failed and are not sent to commit-batch; the other
//     entries of the window are committed. An entry is never committed, or
//     reported stored, without every one of its blobs in CAS.
//   - A control exchange that fails for a transient reason is sent again, as
//     wp.Retry allows.
//   - A window that still fails as a whole is stored one entry at a time, once
//     and without retries, when wp.Retry.SplitWindow is set: one entry the
//     server cannot take then costs that entry alone. A blob an earlier pass
//     stored is not sent again. The split ends after splitStopAfter entries in
//     a row failed for a transient reason.
//
// StoreBatch returns an error, and no outcome, when the window stored nothing
// for a reason every entry shares: the server refused the credential (a 401 or
// a 403, never retried and never split), ctx is done, or the server kept
// failing. Otherwise it returns the outcome, with the entries that failed.
//
// When known is non-nil, blobs it reports present are skipped from the
// discovery/upload step. That skip is an optimistic bet: a blob the cache evicted
// since the prior run would then be absent, so its entry fails to commit.
// StoreBatch recovers in the same run by re-running the exchange for the
// uncommitted entries with the presence filter disabled. The optimistic skip
// never risks a bad commit — commit-batch verifies blob presence server-side —
// only an extra round for the rare eviction.
func (c *Client) StoreBatch(ctx context.Context, in []StoreInput, src BlobSource, known PresenceCache, wp WritePath) (*BatchOutcome, error) {
	out := &BatchOutcome{Committed: make(map[string]bool, len(in)), Failed: make(map[string]error)}
	if len(in) == 0 {
		return out, nil
	}
	st := newBlobState(out)

	if err := c.flushBatch(ctx, in, src, known, wp, st); err != nil {
		if !wp.Retry.SplitWindow || len(in) < 2 || authRefused(err) || ctx.Err() != nil {
			return nil, err
		}
		if err := c.storeSplit(ctx, in, src, known, wp, st, err); err != nil {
			return nil, err
		}
	}

	// Recovery: any entry that did not commit referenced a blob the optimistic
	// skip wrongly assumed present (the cache evicted it). Re-run those entries
	// with no presence filter so the upload re-sends the evicted blob. An entry
	// that failed for a cause of its own is left as it is.
	if known != nil {
		var retry []StoreInput
		for _, e := range in {
			if _, failed := out.Failed[e.Key]; !failed && !out.Committed[e.Key] {
				retry = append(retry, e)
			}
		}
		if len(retry) > 0 {
			if err := c.flushBatch(ctx, retry, src, nil, wp, st); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// storeSplit stores the entries of a window that failed as a whole one at a
// time, once, and records in the outcome why each one that fails did. cause is
// the window's failure. It returns an error when the split stored nothing: the
// server refused the credential, or no entry got through, and the window's
// failure is then the one to report.
func (c *Client) storeSplit(ctx context.Context, in []StoreInput, src BlobSource, known PresenceCache, wp WritePath, st *blobState, cause error) error {
	out := st.out
	single := wp
	single.Retry = WriteRetry{} // each entry is sent once: the window already had its retries

	var (
		last      = cause
		transient int
		stored    bool
	)
	for _, e := range in {
		if out.Committed[e.Key] {
			stored = true
			continue
		}
		if _, failed := out.Failed[e.Key]; failed {
			continue
		}
		if ctx.Err() != nil || transient >= splitStopAfter {
			out.Failed[e.Key] = last
			continue
		}
		err := c.flushBatch(ctx, []StoreInput{e}, src, known, single, st)
		switch {
		case err == nil:
			transient = 0
			stored = stored || out.Committed[e.Key]
		case authRefused(err):
			return err
		default:
			out.Failed[e.Key] = err
			last = err
			if transientFailure(err) {
				transient++
			} else {
				transient = 0
			}
		}
	}
	if !stored {
		return cause
	}
	return nil
}

// flushBatch uploads the blobs of entries and commit-batches the entries whose
// blobs are all in CAS, into the call's outcome. Blobs that known reports
// present are skipped (counted as deduped), and so are the blobs an earlier
// pass of the call already settled. An entry with a blob that could not be
// uploaded is recorded in Failed and left out of the commit; it does not fail
// the pass.
//
// The error it returns is a failure of the pass as a whole: a control exchange
// failed, and nothing says which entry is the cause.
func (c *Client) flushBatch(ctx context.Context, entries []StoreInput, src BlobSource, known PresenceCache, wp WritePath, st *blobState) error {
	out := st.out
	sizeByDigest := make(map[string]int64)
	for _, e := range entries {
		if st.failure(e.Manifest) != nil {
			continue // already lost: none of its other blobs is worth sending
		}
		for d, s := range manifestSizes(e.Manifest) {
			if _, ok := sizeByDigest[d]; !ok {
				sizeByDigest[d] = s
			}
		}
	}

	// Partition into blobs to upload vs. blobs a prior run already confirmed.
	toUpload := make([]cache.BlobRef, 0, len(sizeByDigest))
	for d, s := range sizeByDigest {
		if st.settled(d) {
			continue
		}
		if known != nil && known.Present(d) {
			out.BytesDeduped += s // already present remotely; not sent or uploaded
			continue
		}
		toUpload = append(toUpload, cache.BlobRef{Digest: d, SizeBytes: s})
	}
	// A stable order: the requests of a window do not depend on map iteration.
	slices.SortFunc(toUpload, func(a, b cache.BlobRef) int { return strings.Compare(a.Digest, b.Digest) })

	if err := c.uploadBlobSet(ctx, toUpload, src, wp, st); err != nil {
		return err
	}

	// Commit only the entries whose blobs are all in CAS. The server checks
	// presence too; leaving the others out keeps a known-incomplete entry off
	// the wire, and names its cause.
	commit := make([]cache.CommitEntry, 0, len(entries))
	for _, e := range entries {
		if cause := st.failure(e.Manifest); cause != nil {
			out.Failed[e.Key] = cause
			continue
		}
		commit = append(commit, cache.CommitEntry{Key: e.Key, Result: e.Result, Manifest: e.Manifest})
	}
	// commit-batch takes at most cache.MaxKeysPerRequest entries a request.
	for chunk := range slices.Chunk(commit, cache.MaxKeysPerRequest) {
		commitReq := &cache.CommitBatchRequest{ProtocolVersion: cache.ProtocolVersion, Entries: chunk}
		cache.NormalizeCommitBatchRequest(commitReq)
		commitResp, err := retryWrite(ctx, wp.Retry, func() (*cache.CommitBatchResponse, error) {
			return c.CommitBatch(ctx, commitReq)
		})
		if err != nil {
			return err
		}
		for _, r := range commitResp.Results {
			out.Committed[r.Key] = r.Committed
		}
	}
	return nil
}

// uploadBlobSet puts a set of distinct blobs into CAS, by the path wp selects,
// and records each blob's outcome in st. StoreBatch and StoreObjects share it.
//
// The exchanges go in an order that keeps blob bytes off the wire until the
// server has accepted a write of this client's bearer (see firstWrite):
//
//  1. find-missing for the blobs that go over a PUT. When no write has been
//     accepted yet and there are small blobs to send inline, it is sent even
//     with no blob to ask about: one small request, once per session.
//  2. upload-batch for the small blobs, when the server advertises it.
//  3. find-missing for the small blobs upload-batch did not take.
//  4. The PUTs.
//
// A direct grant replaces the find-missing of steps 1 and 3: the server issued
// it to this bearer for writing, which is the accepted write.
//
// The error it returns is a control exchange that failed. A blob whose own
// upload failed is recorded in st instead, and the other blobs go on.
func (c *Client) uploadBlobSet(ctx context.Context, blobs []cache.BlobRef, src BlobSource, wp WritePath, st *blobState) error {
	if len(blobs) == 0 {
		return nil
	}
	var small []cache.BlobRef
	perBlob := blobs
	if wp.CoalesceSmall {
		perBlob = nil
		for _, b := range blobs {
			if b.SizeBytes <= cache.MaxInlineBlobBytes {
				small = append(small, b)
			} else {
				perBlob = append(perBlob, b)
			}
		}
	}

	if wp.Grant != nil {
		rest, err := c.uploadInlineBatches(ctx, small, src, wp.Retry, st)
		if err != nil {
			return err
		}
		c.directUploadBlobs(ctx, wp.Grant, append(perBlob, rest...), src, st)
		return nil
	}

	probe := false
	if len(small) > 0 {
		var err error
		if probe, err = c.writes.enter(ctx); err != nil {
			return err
		}
	}
	var transfers []cache.BlobTransfer
	if len(perBlob) > 0 || probe {
		var err error
		transfers, err = c.findMissingBlobs(ctx, perBlob, wp.Retry, st)
		if probe {
			c.writes.leave()
		}
		if err != nil {
			return err
		}
	}

	rest, err := c.uploadInlineBatches(ctx, small, src, wp.Retry, st)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		more, err := c.findMissingBlobs(ctx, rest, wp.Retry, st)
		if err != nil {
			return err
		}
		transfers = append(transfers, more...)
	}

	sizes := make(map[string]int64, len(blobs))
	for _, b := range blobs {
		sizes[b.Digest] = b.SizeBytes
	}
	c.putBlobs(ctx, transfers, src, sizes, wp.BlobConcurrency, st)
	return nil
}

// findMissingBlobs asks the server which of blobs it lacks and returns the PUT
// transfers for those. The blobs it already has are recorded present. One
// find-missing takes at most cache.MaxKeysPerRequest blobs, so a larger set is
// asked about in several requests: an entry with more files than that is stored
// like any other. An empty set still sends one request, the write without
// bytes that opens a session (see uploadBlobSet).
func (c *Client) findMissingBlobs(ctx context.Context, blobs []cache.BlobRef, retry WriteRetry, st *blobState) ([]cache.BlobTransfer, error) {
	var transfers []cache.BlobTransfer
	for first := true; first || len(blobs) > 0; first = false {
		chunk := blobs[:min(len(blobs), cache.MaxKeysPerRequest)]
		blobs = blobs[len(chunk):]

		// A copy: the request is sorted in place, and an empty set is sent as
		// an empty list, not as null.
		findReq := &cache.FindMissingRequest{ProtocolVersion: cache.ProtocolVersion, Blobs: append([]cache.BlobRef{}, chunk...)}
		cache.NormalizeFindMissingRequest(findReq)
		findResp, err := retryWrite(ctx, retry, func() (*cache.FindMissingResponse, error) {
			return c.FindMissing(ctx, findReq)
		})
		if err != nil {
			return nil, err
		}
		requested := make(map[string]bool, len(findResp.Uploads))
		for _, up := range findResp.Uploads {
			requested[up.Digest] = true
		}
		for _, b := range chunk {
			if !requested[b.Digest] {
				st.present(b.Digest, b.SizeBytes)
			}
		}
		transfers = append(transfers, findResp.Uploads...)
	}
	return transfers, nil
}

// putBlobs uploads each transfer's blob over its PUT, at most limit at once
// (blobUploadConcurrency when limit is not positive), and records each blob's
// outcome in st. A PUT that fails does not stop the others: it costs the
// entries that name its blob, and no other.
func (c *Client) putBlobs(ctx context.Context, ups []cache.BlobTransfer, src BlobSource, sizes map[string]int64, limit int, st *blobState) {
	if len(ups) == 0 {
		return
	}
	if limit <= 0 {
		limit = blobUploadConcurrency
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, limit)
	for _, up := range ups {
		wg.Add(1)
		go func(up cache.BlobTransfer) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				st.fail(up.Digest, fmt.Errorf("upload blob %s: %w", up.Digest, ctx.Err()))
				return
			}
			defer func() { <-sem }()

			size := blobSize(up, sizes)
			if err := c.uploadFrom(ctx, up, src, size); err != nil {
				st.fail(up.Digest, err)
				return
			}
			st.uploaded(up.Digest, size)
		}(up)
	}
	wg.Wait()
}

// maxInlineBatchBytes bounds the blob bytes of one upload-batch request, so a
// window's small blobs split across a few requests rather than one body the
// server would reject. 4 MiB of blob bytes is about 5.4 MiB of JSON.
// cache-server reads an upload-batch body of up to 16 MiB and stores at most
// 16 MiB of decoded content per request (cas.MaxPackBytes), so one request
// stays well inside both bounds.
const maxInlineBatchBytes = 4 << 20 // 4 MiB

// uploadInlineBatches sends small blobs inline via upload-batch, chunked so no
// request exceeds maxInlineBatchBytes of blob bytes or the protocol's
// cache.MaxKeysPerRequest blobs. Each blob the server reports stored is
// recorded uploaded in st. A blob whose bytes cannot be read is recorded
// failed there, and the others go on.
//
// It returns the blobs upload-batch did not take, which the caller sends
// through the per-blob path so they are not lost:
//
//   - a blob the answer omits: the contract lets a server store a subset;
//   - every blob not stored yet once the server answers that it does not serve
//     the route (uploadBatchUnserved). No later chunk is sent.
//
// Any other failure is returned and fails the pass, after the retries the
// policy allows: the server refused the write or is failing, and the per-blob
// path would meet the same answer.
func (c *Client) uploadInlineBatches(ctx context.Context, blobs []cache.BlobRef, src BlobSource, retry WriteRetry, st *blobState) ([]cache.BlobRef, error) {
	var (
		chunk    []cache.InlineBlob
		refs     []cache.BlobRef
		chunkLen int64
		rest     []cache.BlobRef
		unserved bool
	)
	flush := func() error {
		if len(chunk) == 0 {
			return nil
		}
		req := &cache.UploadBatchRequest{ProtocolVersion: cache.ProtocolVersion, Blobs: chunk}
		resp, err := retryWrite(ctx, retry, func() (*cache.UploadBatchResponse, error) {
			return c.UploadBatch(ctx, req)
		})
		switch {
		case uploadBatchUnserved(err):
			unserved = true
			rest = append(rest, refs...)
		case err != nil:
			return err
		default:
			stored := make(map[string]bool, len(resp.Stored))
			for _, d := range resp.Stored {
				stored[d] = true
			}
			for _, b := range refs {
				if !stored[b.Digest] {
					rest = append(rest, b)
					continue
				}
				st.uploaded(b.Digest, b.SizeBytes)
			}
		}
		chunk, refs, chunkLen = nil, nil, 0
		return nil
	}

	for i, b := range blobs {
		data, err := readInlineBlob(src, b)
		if err != nil {
			st.fail(b.Digest, err)
			continue
		}
		if len(chunk) == cache.MaxKeysPerRequest || chunkLen+int64(len(data)) > maxInlineBatchBytes {
			if err := flush(); err != nil {
				return nil, err
			}
			if unserved {
				return append(rest, blobs[i:]...), nil
			}
		}
		chunk = append(chunk, cache.InlineBlob{Digest: b.Digest, Data: data})
		refs = append(refs, b)
		chunkLen += int64(len(data))
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return rest, nil
}

// readInlineBlob reads the bytes of a small blob to send inline.
func readInlineBlob(src BlobSource, b cache.BlobRef) ([]byte, error) {
	if src == nil {
		return nil, fmt.Errorf("blob %s: no blob source", b.Digest)
	}
	rc, err := src.OpenBlob(b.Digest)
	if err != nil {
		return nil, fmt.Errorf("open blob %s: %w", b.Digest, err)
	}
	// One byte past the inline limit, so a longer source is detected without
	// reading it whole.
	data, err := io.ReadAll(io.LimitReader(rc, cache.MaxInlineBlobBytes+1))
	_ = rc.Close()
	if err != nil {
		return nil, fmt.Errorf("read blob %s: %w", b.Digest, err)
	}
	if int64(len(data)) > cache.MaxInlineBlobBytes {
		return nil, fmt.Errorf("blob %s: its bytes exceed the declared size %d", b.Digest, b.SizeBytes)
	}
	return data, nil
}

// uploadBatchUnserved reports an answer that says the server does not serve
// upload-batch: 404, 405 or 501. A server can give one after it advertised the
// route, for example while a deploy that removes it rolls out and an older
// probe answer is still in use. The blobs then take the per-blob path, which
// every server with the batched write path serves.
func uploadBatchUnserved(err error) bool {
	return IsStatus(err, http.StatusNotFound) ||
		IsStatus(err, http.StatusMethodNotAllowed) ||
		IsStatus(err, http.StatusNotImplemented)
}

// directUploadBlobs conditionally PUTs each blob straight to CAS, concurrently
// (bounded by blobUploadConcurrency), and records each blob's outcome in st.
// A blob the conditional create rejected was already there, so it is recorded
// present. A PUT that fails does not stop the others, as on the find-missing
// path.
func (c *Client) directUploadBlobs(ctx context.Context, grant *cache.UploadGrant, blobs []cache.BlobRef, src BlobSource, st *blobState) {
	if len(blobs) == 0 {
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, blobUploadConcurrency)
	for _, b := range blobs {
		wg.Add(1)
		go func(b cache.BlobRef) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				st.fail(b.Digest, fmt.Errorf("direct upload %s: %w", b.Digest, ctx.Err()))
				return
			}
			defer func() { <-sem }()

			uploaded, err := c.uploadDirect(ctx, grant, b, src)
			switch {
			case err != nil:
				st.fail(b.Digest, err)
			case uploaded:
				st.uploaded(b.Digest, b.SizeBytes)
			default:
				st.present(b.Digest, b.SizeBytes) // conditional create rejected: already present
			}
		}(b)
	}
	wg.Wait()
}

// uploadDirect PUTs one blob to its grant-derived URL. It returns uploaded=true
// when the bytes were stored (2xx) and uploaded=false when the blob already
// existed (the grant's ConditionalExistsStatus — a dedup skip, not an error).
// The grant's headers (credential, content type, conditional-create header) are
// sent verbatim, keeping the client backend-agnostic. The PUT has the deadline
// and the redirect policy of UploadBlob.
func (c *Client) uploadDirect(ctx context.Context, grant *cache.UploadGrant, b cache.BlobRef, src BlobSource) (bool, error) {
	if src == nil {
		return false, fmt.Errorf("blob %s: no blob source", b.Digest)
	}
	hexDigest := strings.TrimPrefix(b.Digest, cache.DigestAlgorithm+":")
	url := strings.ReplaceAll(grant.URLTemplate, cache.DigestPlaceholder, hexDigest)

	body, length, cleanup, err := compressedBody(ctx, src, b.Digest, b.SizeBytes)
	if err != nil {
		return false, fmt.Errorf("open blob %s: %w", b.Digest, err)
	}
	defer func() { _ = cleanup() }()

	timeout := c.blobPutTimeout(length)
	putCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(putCtx, grant.Method, url, body)
	if err != nil {
		return false, fmt.Errorf("build direct upload for %s: %w", b.Digest, err)
	}
	for k, v := range grant.Headers {
		req.Header.Set(k, v)
	}
	setUserAgent(req)
	req.ContentLength = length
	replayable(req, body)
	trackUpload(req)

	resp, err := c.transfers.Do(req) //nolint:gosec // G704: request targets the cache server's presigned upload grant, not user-tainted input
	if err != nil {
		return false, fmt.Errorf("direct upload %s: %w", b.Digest, putFailure(ctx, putCtx, timeout, err))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	reportUploadProgress(ctx)

	if grant.ConditionalExistsStatus != 0 && resp.StatusCode == grant.ConditionalExistsStatus {
		return false, nil // already present: conditional create rejected → dedup
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Errorf("direct upload %s: HTTP %d", b.Digest, resp.StatusCode)
	}
	return true, nil
}
