package remotecache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	cacheserverclient "go.putnami.dev/cloud/clients/cache-server/go"
	cache "go.putnami.dev/protocol/cache"
	diag "go.putnami.dev/protocol/diagnostic"
)

// The post-build write path. Negotiate runs before the build and cannot know a
// missed key's digests, so storing a freshly built result is a separate
// two-step exchange (see go.putnami.dev/protocol/cache):
//
//  1. Store submits the built manifest; the server returns presigned PUT
//     transfers for only the blobs it is missing (dedup).
//  2. The client uploads those blobs, then Commit finalizes the Action Cache
//     entry (key -> result + manifest).
//
// StoreResult drives the whole sequence; the individual steps are exported for
// callers that need finer control.

// StoreInput is the per-job data needed to store a freshly built result: the
// precomputed cache key, the cached outcome to record, and the built manifest
// (the file set with per-file CAS digests).
type StoreInput struct {
	Key      string
	Result   *cache.ActionResult
	Manifest *cache.Manifest
}

// BlobSource yields the bytes for a CAS digest the server asked to be uploaded.
// The CLI's local content-addressed store implements it.
type BlobSource interface {
	// OpenBlob returns a reader for the blob addressed by digest
	// ("sha256:<hex>"). The caller closes it.
	OpenBlob(digest string) (io.ReadCloser, error)
}

// BuildStoreRequest assembles the step-1 store request from a freshly built
// manifest. Remote eligibility (side-effecting or not) is already decided
// at negotiate time, so this only constructs and normalizes the wire request.
func (c *Client) BuildStoreRequest(in StoreInput) *cache.StoreRequest {
	req := &cache.StoreRequest{
		ProtocolVersion: cache.ProtocolVersion,
		Key:             in.Key,
		Manifest:        in.Manifest,
	}
	cache.NormalizeStoreRequest(req)
	return req
}

// Store sends the step-1 request and returns the presigned PUT transfers for
// the blobs the server is missing. The upload set may be empty when every blob
// is already in CAS (dedup), in which case the client commits directly.
func (c *Client) Store(ctx context.Context, req *cache.StoreRequest) (*cache.StoreResponse, error) {
	if diags := cache.ValidateStoreRequest(req); diag.HasErrors(diags) {
		return nil, fmt.Errorf("invalid store request: %s", firstError(diags))
	}

	data, err := postControl(ctx, c, "store", req, (*cacheserverclient.CacheClient).CreateV1CacheStore)
	if err != nil {
		return nil, err
	}

	out, diags := cache.ParseAndValidateStoreResponse(data)
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("invalid store response: %s", firstError(diags))
	}
	if out.Key != req.Key {
		return nil, fmt.Errorf("store response key %q does not match request key %q", out.Key, req.Key)
	}
	allowed := manifestDigests(req.Manifest)
	for _, up := range out.Uploads {
		if !allowed[up.Digest] {
			return nil, fmt.Errorf("store response requested upload for digest %s outside manifest", up.Digest)
		}
	}
	c.writes.accept()
	return out, nil
}

// Commit finalizes the Action Cache entry (step 2) after its blobs are
// uploaded. It is safe to retry: committing an already-present key is a no-op
// and still reports committed.
func (c *Client) Commit(ctx context.Context, req *cache.CommitRequest) (*cache.CommitResponse, error) {
	if diags := cache.ValidateCommitRequest(req); diag.HasErrors(diags) {
		return nil, fmt.Errorf("invalid commit request: %s", firstError(diags))
	}

	data, err := postControl(ctx, c, "commit", req, (*cacheserverclient.CacheClient).CreateV1CacheCommit)
	if err != nil {
		return nil, err
	}

	out, diags := cache.ParseAndValidateCommitResponse(data)
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("invalid commit response: %s", firstError(diags))
	}
	if out.Key != req.Key {
		return nil, fmt.Errorf("commit response key %q does not match request key %q", out.Key, req.Key)
	}
	c.writes.accept()
	return out, nil
}

// createOnlyHeader is the storage precondition a transfer carries when its PUT
// may only CREATE the blob's object: the upload succeeds when no object exists
// and answers 412 when one does.
const createOnlyHeader = "x-goog-if-generation-match"

// createOnly reports whether the transfer's PUT is create-only. Header names
// are case-insensitive, and the server's spelling is not this client's to
// assume.
func createOnly(t cache.BlobTransfer) bool {
	for name := range t.Headers {
		if strings.EqualFold(name, createOnlyHeader) {
			return true
		}
	}
	return false
}

// A blob PUT has a deadline of its own. Without one, a storage endpoint that
// stops reading holds the upload, and every upload queued behind it, for as
// long as the connection stays open. The deadline is a floor plus the time the
// entity takes at a low minimum throughput:
//
//	deadline = 60 s + entity bytes / 256 KiB/s
//
// The floor covers the connection, the TLS handshake and the storage answer
// for a small blob. 256 KiB/s is about 2 Mbit/s per PUT, far below what a CI
// machine reaches, so a healthy upload is never cut: a 1 MiB entity gets 64 s,
// a 20 MiB one 140 s, a 100 MiB one 460 s and a 1 GiB one 69 min. The entity is
// what is sent, so a blob that gzip shrinks is timed on its compressed size.
// The provider runs up to 16 PUTs at once; all of them stay inside their
// deadlines on a link that carries 4 MiB/s.
const (
	blobPutFloor             = 60 * time.Second
	blobPutMinBytesPerSecond = 256 << 10
)

// blobPutTimeout returns the deadline of one PUT of length bytes.
func (c *Client) blobPutTimeout(length int64) time.Duration {
	if length < 0 || c.putBytesPerSecond <= 0 {
		return c.putFloor
	}
	return c.putFloor + time.Duration(float64(length)/float64(c.putBytesPerSecond)*float64(time.Second))
}

// putFailure names a PUT that ran past its own deadline. The transport reports
// it as a bare "context deadline exceeded", which reads like a canceled run.
func putFailure(parent, put context.Context, timeout time.Duration, err error) error {
	if parent.Err() == nil && errors.Is(put.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("no answer within the %s this upload is given: %w", timeout, err)
	}
	return err
}

// UploadBlob uploads one CAS blob over its PUT transfer. The transfer is
// self-authenticating — a presigned URL carries its credential in the URL, and
// a plain object URL comes with a storage token in its Headers — so the per-user
// bearer token is deliberately NOT attached: only the server-supplied Headers
// are sent, verbatim. body is streamed as the request entity and must yield
// exactly length bytes, which is sent as the Content-Length. The length is the
// entity's own, never the transfer's declared size: a gzip-compressed blob is
// shorter than the blob it encodes.
//
// A 412 on a create-only transfer means another writer stored the same digest
// first: the object the upload was to create exists, so the blob is reported
// as stored. Nothing is lost by not replacing it, because every reader checks
// a blob against its digest. A 412 on any other transfer, and every other
// refusal (a 403 included), still fails the upload.
//
// The PUT ends at its deadline (see blobPutTimeout), whatever the caller's
// context allows. A redirect that leaves the transfer's origin is followed
// without the transfer's credential (see transferClient).
func (c *Client) UploadBlob(ctx context.Context, t cache.BlobTransfer, body io.Reader, length int64) error {
	if t.Method != cache.TransferPut {
		return fmt.Errorf("blob %s: transfer method %q, want %s", t.Digest, t.Method, cache.TransferPut)
	}
	timeout := c.blobPutTimeout(length)
	putCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(putCtx, http.MethodPut, t.URL, body)
	if err != nil {
		return fmt.Errorf("build upload request for %s: %w", t.Digest, err)
	}
	for k, v := range t.Headers {
		req.Header.Set(k, v)
	}
	setUserAgent(req)
	req.ContentLength = length
	replayable(req, body)
	trackUpload(req)

	resp, err := c.transfers.Do(req) //nolint:gosec // G704: request targets the cache server's presigned upload grant, not user-tainted input
	if err != nil {
		return fmt.Errorf("upload blob %s: %w", t.Digest, putFailure(ctx, putCtx, timeout, err))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	reportUploadProgress(ctx)

	if resp.StatusCode == http.StatusPreconditionFailed && createOnly(t) {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upload blob %s: HTTP %d", t.Digest, resp.StatusCode)
	}
	return nil
}

// StoreOutcome reports what a write exchange moved, so callers can show the
// cache's write-side cost and the dedup it earned. Commit is the finalization
// response; the byte counts split the manifest into what was actually uploaded
// versus what the server already had in CAS.
type StoreOutcome struct {
	Commit        *cache.CommitResponse
	BlobsUploaded int   // distinct blobs the server requested (cache misses)
	BytesUploaded int64 // bytes pushed for those blobs
	BytesDeduped  int64 // manifest bytes the server already had (no upload)
}

// StoreResult runs the full post-build write path for one key: Store to learn
// which blobs are missing, upload each missing blob from src over its presigned
// URL, then Commit to finalize the Action Cache entry. A store that finds
// nothing missing (full dedup) skips straight to commit. The outcome reports
// the commit response plus the uploaded-versus-deduped byte split.
func (c *Client) StoreResult(ctx context.Context, in StoreInput, src BlobSource) (*StoreOutcome, error) {
	storeResp, err := c.Store(ctx, c.BuildStoreRequest(in))
	if err != nil {
		return nil, err
	}

	// Size each distinct manifest blob so the upload/dedup split can be reported
	// in bytes even when the server omits SizeBytes on the transfer.
	sizeByDigest := manifestSizes(in.Manifest)
	out := &StoreOutcome{}

	// Upload the missing blobs concurrently: they are independent presigned PUTs,
	// so transferring them in parallel turns N sequential round trips into ~N/cap
	// and shortens the post-build write that was stalling the worker.
	if err := c.uploadBlobs(ctx, storeResp.Uploads, src, sizeByDigest, out, blobUploadConcurrency); err != nil {
		return nil, err
	}
	// Everything in the manifest the server did NOT ask for was already in CAS.
	requested := make(map[string]bool, len(storeResp.Uploads))
	for _, up := range storeResp.Uploads {
		requested[up.Digest] = true
	}
	for digest, size := range sizeByDigest {
		if !requested[digest] {
			out.BytesDeduped += size
		}
	}

	commitReq := &cache.CommitRequest{
		ProtocolVersion: cache.ProtocolVersion,
		Key:             in.Key,
		Result:          in.Result,
		Manifest:        in.Manifest,
	}
	cache.NormalizeCommitRequest(commitReq)
	commit, err := c.Commit(ctx, commitReq)
	if err != nil {
		return nil, err
	}
	out.Commit = commit
	return out, nil
}

// manifestSizes maps each distinct CAS digest in the manifest to its byte size,
// collapsing duplicate files that share content so a blob is counted once.
func manifestSizes(m *cache.Manifest) map[string]int64 {
	sizes := make(map[string]int64)
	if m == nil {
		return sizes
	}
	for _, f := range m.Files {
		if _, ok := sizes[f.Digest]; !ok {
			sizes[f.Digest] = f.Size
		}
	}
	return sizes
}

// blobSize prefers the size the server stated on the transfer, falling back to
// the manifest's recorded size for the digest.
func blobSize(t cache.BlobTransfer, sizeByDigest map[string]int64) int64 {
	if t.SizeBytes > 0 {
		return t.SizeBytes
	}
	return sizeByDigest[t.Digest]
}

// blobUploadConcurrency bounds how many of one key's missing blobs upload at
// once. CAS blobs are independent presigned PUTs, so a handful in flight hides
// per-request latency without flooding the link.
const blobUploadConcurrency = 4

// uploadBlobs transfers the missing blobs concurrently, at most limit at once
// (blobUploadConcurrency when limit is not positive), accumulating the uploaded
// blob/byte counts into out. It returns the first upload error (and cancels the
// rest) so a failed write surfaces just as it did when serial.
func (c *Client) uploadBlobs(ctx context.Context, ups []cache.BlobTransfer, src BlobSource, sizeByDigest map[string]int64, out *StoreOutcome, limit int) error {
	if len(ups) == 0 {
		return nil
	}
	if limit <= 0 {
		limit = blobUploadConcurrency
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)
	sem := make(chan struct{}, limit)
	for _, up := range ups {
		wg.Add(1)
		go func(up cache.BlobTransfer) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()

			if err := c.uploadFrom(ctx, up, src, blobSize(up, sizeByDigest)); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel() // stop the remaining uploads; one failure fails the store
				}
				mu.Unlock()
				return
			}
			mu.Lock()
			out.BlobsUploaded++
			out.BytesUploaded += blobSize(up, sizeByDigest)
			mu.Unlock()
		}(up)
	}
	wg.Wait()
	return firstErr
}

// uploadFrom opens one blob from src and uploads it (gzip-compressed when that
// shrinks it), ensuring the source is closed and any spool file removed even
// when the upload fails.
func (c *Client) uploadFrom(ctx context.Context, t cache.BlobTransfer, src BlobSource, sizeBytes int64) error {
	body, length, cleanup, err := compressedBody(ctx, src, t.Digest, sizeBytes)
	if err != nil {
		return fmt.Errorf("open blob %s: %w", t.Digest, err)
	}
	defer func() { _ = cleanup() }()
	return c.UploadBlob(ctx, t, body, length)
}

func manifestDigests(m *cache.Manifest) map[string]bool {
	out := make(map[string]bool)
	if m == nil {
		return out
	}
	for _, f := range m.Files {
		out[f.Digest] = true
	}
	return out
}
