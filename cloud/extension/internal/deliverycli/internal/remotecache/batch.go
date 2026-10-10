package remotecache

import (
	"context"
	"fmt"
	"io"
	"net/http"
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
// fall back to store/commit otherwise. Bytes still stream per blob — only the
// control plane is batched.

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
type BatchOutcome struct {
	Committed     map[string]bool
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
}

// StoreBatch runs the batched write path for a window of built entries (the same
// StoreInput as the per-job StoreResult): upload the window's blobs, then
// commit-batch (one round trip). Blobs are deduped across the whole window, so a
// blob shared by two entries (or files) is uploaded once. wp picks the upload
// mechanism from the server's capabilities — small blobs may be coalesced into
// an upload-batch request and the rest go via the direct grant or find-missing;
// commit-batch always finalizes.
//
// When known is non-nil, blobs it reports present are skipped from the
// discovery/upload step. That skip is an optimistic bet: a blob the cache evicted
// since the prior run would then be absent, so its entry fails to commit.
// StoreBatch recovers in the same run by re-running the exchange for the
// uncommitted entries with the presence filter disabled. The optimistic skip
// never risks a bad commit — commit-batch verifies blob presence server-side —
// only an extra round for the rare eviction.
func (c *Client) StoreBatch(ctx context.Context, in []StoreInput, src BlobSource, known PresenceCache, wp WritePath) (*BatchOutcome, error) {
	out := &BatchOutcome{Committed: make(map[string]bool, len(in))}
	if len(in) == 0 {
		return out, nil
	}

	flush := func(entries []StoreInput, knownArg PresenceCache) error {
		return c.flushBatch(ctx, entries, src, knownArg, wp, out)
	}

	if err := flush(in, known); err != nil {
		return nil, err
	}

	// Recovery: any entry that did not commit referenced a blob the optimistic
	// skip wrongly assumed present (the cache evicted it). Re-run those entries
	// with no presence filter so the upload re-sends the evicted blob.
	if known != nil {
		var retry []StoreInput
		for _, e := range in {
			if !out.Committed[e.Key] {
				retry = append(retry, e)
			}
		}
		if len(retry) > 0 {
			if err := flush(retry, nil); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// flushBatch uploads a window's blobs and commit-batches its entries, into out.
// Blobs that known reports present are skipped (counted as deduped). With
// wp.CoalesceSmall, blobs at or below cache.MaxInlineBlobBytes are sent via
// upload-batch; the rest take the direct grant or find-missing. Confirmed gains
// every blob this pass verified present — not the known-skipped ones, which the
// caller already has.
func (c *Client) flushBatch(ctx context.Context, entries []StoreInput, src BlobSource, known PresenceCache, wp WritePath, out *BatchOutcome) error {
	sizeByDigest := make(map[string]int64)
	for _, e := range entries {
		for d, s := range manifestSizes(e.Manifest) {
			if _, ok := sizeByDigest[d]; !ok {
				sizeByDigest[d] = s
			}
		}
	}

	// Partition into blobs to upload vs. blobs a prior run already confirmed.
	toUpload := make([]cache.BlobRef, 0, len(sizeByDigest))
	for d, s := range sizeByDigest {
		if known != nil && known.Present(d) {
			out.BytesDeduped += s // already present remotely; not sent or uploaded
			continue
		}
		toUpload = append(toUpload, cache.BlobRef{Digest: d, SizeBytes: s})
	}

	// Coalesce small blobs into upload-batch when enabled; the rest take the
	// per-blob path.
	perBlob := toUpload
	if wp.CoalesceSmall {
		var small, large []cache.BlobRef
		for _, b := range toUpload {
			if b.SizeBytes <= cache.MaxInlineBlobBytes {
				small = append(small, b)
			} else {
				large = append(large, b)
			}
		}
		if err := c.uploadInlineBatches(ctx, small, src, out); err != nil {
			return err
		}
		perBlob = large
	}

	if wp.Grant != nil {
		if err := c.directUploadBlobs(ctx, wp.Grant, perBlob, src, out); err != nil {
			return err
		}
	} else if err := c.findMissingUpload(ctx, perBlob, src, sizeByDigest, out); err != nil {
		return err
	}

	commitReq := &cache.CommitBatchRequest{ProtocolVersion: cache.ProtocolVersion}
	for _, e := range entries {
		commitReq.Entries = append(commitReq.Entries, cache.CommitEntry{
			Key:      e.Key,
			Result:   e.Result,
			Manifest: e.Manifest,
		})
	}
	cache.NormalizeCommitBatchRequest(commitReq)
	commitResp, err := c.CommitBatch(ctx, commitReq)
	if err != nil {
		return err
	}
	for _, r := range commitResp.Results {
		out.Committed[r.Key] = r.Committed
	}
	return nil
}

// findMissingUpload uploads blobs via the find-missing path: one batched
// existence check, then a presigned PUT for each blob the server lacks. Blobs the
// server already has are counted deduped; all are confirmed present afterward.
func (c *Client) findMissingUpload(ctx context.Context, blobs []cache.BlobRef, src BlobSource, sizeByDigest map[string]int64, out *BatchOutcome) error {
	if len(blobs) == 0 {
		return nil
	}

	findReq := &cache.FindMissingRequest{ProtocolVersion: cache.ProtocolVersion, Blobs: blobs}
	cache.NormalizeFindMissingRequest(findReq)
	findResp, err := c.FindMissing(ctx, findReq)
	if err != nil {
		return err
	}

	so := &StoreOutcome{}
	if err := c.uploadBlobs(ctx, findResp.Uploads, src, sizeByDigest, so); err != nil {
		return err
	}
	out.BlobsUploaded += so.BlobsUploaded
	out.BytesUploaded += so.BytesUploaded

	requested := make(map[string]bool, len(findResp.Uploads))
	for _, up := range findResp.Uploads {
		requested[up.Digest] = true
	}
	for _, b := range blobs {
		if !requested[b.Digest] {
			out.BytesDeduped += sizeByDigest[b.Digest]
		}
		out.Confirmed = append(out.Confirmed, b.Digest)
	}
	return nil
}

// maxInlineBatchBytes bounds one upload-batch request's total inline payload, so
// a window's small blobs split across a few requests rather than one body the
// server would reject.
const maxInlineBatchBytes = 4 << 20 // 4 MiB

// uploadInlineBatches sends small blobs inline via upload-batch, chunked so no
// request exceeds maxInlineBatchBytes. Each blob the server reports stored is
// counted uploaded and confirmed; a blob omitted from the response is left
// unconfirmed, so its entry fails commit and the recovery pass retries it.
func (c *Client) uploadInlineBatches(ctx context.Context, blobs []cache.BlobRef, src BlobSource, out *BatchOutcome) error {
	var (
		chunk    []cache.InlineBlob
		chunkLen int64
		sizes    = make(map[string]int64, len(blobs))
	)
	flush := func() error {
		if len(chunk) == 0 {
			return nil
		}
		resp, err := c.UploadBatch(ctx, &cache.UploadBatchRequest{ProtocolVersion: cache.ProtocolVersion, Blobs: chunk})
		if err != nil {
			return err
		}
		stored := make(map[string]bool, len(resp.Stored))
		for _, d := range resp.Stored {
			stored[d] = true
		}
		for _, b := range chunk {
			if stored[b.Digest] {
				out.BlobsUploaded++
				out.BytesUploaded += sizes[b.Digest]
				out.Confirmed = append(out.Confirmed, b.Digest)
			}
		}
		chunk, chunkLen = nil, 0
		return nil
	}

	for _, b := range blobs {
		rc, err := src.OpenBlob(b.Digest)
		if err != nil {
			return fmt.Errorf("open blob %s: %w", b.Digest, err)
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return fmt.Errorf("read blob %s: %w", b.Digest, err)
		}
		if int64(len(data)) > cache.MaxInlineBlobBytes {
			return fmt.Errorf("blob %s: %d bytes exceed declared size %d", b.Digest, len(data), b.SizeBytes)
		}
		if chunkLen+int64(len(data)) > maxInlineBatchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		sizes[b.Digest] = b.SizeBytes
		chunk = append(chunk, cache.InlineBlob{Digest: b.Digest, Data: data})
		chunkLen += int64(len(data))
	}
	return flush()
}

// directUploadBlobs conditionally PUTs each blob straight to CAS, concurrently
// (bounded by blobUploadConcurrency), accumulating the upload/dedup split and the
// confirmed digests into out. Every attempted blob is present afterward (either
// it was already there — the conditional create was rejected — or it was just
// uploaded), so all are confirmed. The first error cancels the rest, matching the
// find-missing path's behavior.
func (c *Client) directUploadBlobs(ctx context.Context, grant *cache.UploadGrant, blobs []cache.BlobRef, src BlobSource, out *BatchOutcome) error {
	if len(blobs) == 0 {
		return nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)
	sem := make(chan struct{}, blobUploadConcurrency)
	for _, b := range blobs {
		wg.Add(1)
		go func(b cache.BlobRef) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()

			uploaded, err := c.uploadDirect(ctx, grant, b, src)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				return
			}
			if uploaded {
				out.BlobsUploaded++
				out.BytesUploaded += b.SizeBytes
			} else {
				out.BytesDeduped += b.SizeBytes // conditional create rejected: already present
			}
			out.Confirmed = append(out.Confirmed, b.Digest)
		}(b)
	}
	wg.Wait()
	return firstErr
}

// uploadDirect PUTs one blob to its grant-derived URL. It returns uploaded=true
// when the bytes were stored (2xx) and uploaded=false when the blob already
// existed (the grant's ConditionalExistsStatus — a dedup skip, not an error).
// The grant's headers (credential, content type, conditional-create header) are
// sent verbatim, keeping the client backend-agnostic.
func (c *Client) uploadDirect(ctx context.Context, grant *cache.UploadGrant, b cache.BlobRef, src BlobSource) (bool, error) {
	if src == nil {
		return false, fmt.Errorf("blob %s: no blob source", b.Digest)
	}
	hexDigest := strings.TrimPrefix(b.Digest, cache.DigestAlgorithm+":")
	url := strings.ReplaceAll(grant.URLTemplate, cache.DigestPlaceholder, hexDigest)

	body, length, cleanup, err := compressedBody(src, b.Digest, b.SizeBytes)
	if err != nil {
		return false, fmt.Errorf("open blob %s: %w", b.Digest, err)
	}
	defer func() { _ = cleanup() }()

	req, err := http.NewRequestWithContext(ctx, grant.Method, url, body)
	if err != nil {
		return false, fmt.Errorf("build direct upload for %s: %w", b.Digest, err)
	}
	for k, v := range grant.Headers {
		req.Header.Set(k, v)
	}
	setUserAgent(req)
	req.ContentLength = length

	resp, err := c.httpClient.Do(req) //nolint:gosec // G704: request targets the cache server's presigned upload grant, not user-tainted input
	if err != nil {
		return false, fmt.Errorf("direct upload %s: %w", b.Digest, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))

	if grant.ConditionalExistsStatus != 0 && resp.StatusCode == grant.ConditionalExistsStatus {
		return false, nil // already present: conditional create rejected → dedup
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Errorf("direct upload %s: HTTP %d", b.Digest, resp.StatusCode)
	}
	return true, nil
}
