package remotecache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"sync"

	cache "go.putnami.dev/protocol/cache"
)

// The CAS read path: lazy materialization ("Build without the Bytes"). A
// negotiate hit carries presigned GET transfers for the blobs that materialize
// it; bytes move only when something actually consumes them — a local miss that
// needs the hit as an input, or a requested deliverable. DownloadBlob is the
// verified single-blob primitive (the counterpart to UploadBlob); HitFetcher
// adapts a hit's downloads into a per-digest opener the local store drives.

// downloadOp labels the StatusError of a refused blob download.
const downloadOp = "download"

// DownloadRefused reports whether err is a blob download the storage refused
// in a way an expired or withdrawn transfer produces: 400 (the storage answers
// an expired signed URL with it), 401 (an expired storage token), 403, 404 or
// 410. A transfer negotiated again may then succeed; a transport failure or a
// 5xx says nothing about the transfer.
func DownloadRefused(err error) bool {
	var status *StatusError
	if !errors.As(err, &status) || status.Op != downloadOp {
		return false
	}
	switch status.Status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone:
		return true
	}
	return false
}

// DownloadBlob fetches one CAS blob over its GET transfer into dst, verifying
// the streamed bytes content-address to t.Digest. Like UploadBlob, the transfer
// is self-authenticating — a presigned URL carries its credential in the URL,
// and a plain object URL comes with a storage token in its Headers — so the
// per-user bearer token is deliberately NOT attached: only the server-supplied
// Headers are sent, verbatim. A redirect that leaves the transfer's origin is
// followed without the transfer's credential (see transferClient). It returns
// the number of bytes written.
//
// A refusal is a *StatusError whose Op is "download", so a caller can tell a
// transfer the storage no longer accepts (its signature or token expired, or
// the object is gone) from a failure of the bytes themselves.
func (c *Client) DownloadBlob(ctx context.Context, t cache.BlobTransfer, dst io.Writer) (int64, error) {
	if t.Method != cache.TransferGet {
		return 0, fmt.Errorf("blob %s: transfer method %q, want %s", t.Digest, t.Method, cache.TransferGet)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.URL, nil)
	if err != nil {
		return 0, fmt.Errorf("build download request for %s: %w", t.Digest, err)
	}
	for k, v := range t.Headers {
		req.Header.Set(k, v)
	}
	setUserAgent(req)

	resp, err := c.transfers.Do(req) //nolint:gosec // G704: request targets the cache server's presigned download grant, not user-tainted input
	if err != nil {
		return 0, fmt.Errorf("download blob %s: %w", t.Digest, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		return 0, statusError(downloadOp, resp.StatusCode, body)
	}

	// The blob may be stored gzipped (transport compression); decompress so the
	// digest is verified against the uncompressed content. The bound is applied
	// to the DECOMPRESSED stream so a gzip bomb can't expand past the declared
	// size — one byte past it so an oversized blob is detected.
	body, err := decodeBlobReader(resp.Body, resp.ContentLength, t.SizeBytes)
	if err != nil {
		return 0, fmt.Errorf("blob %s: %w", t.Digest, err)
	}
	if t.SizeBytes > 0 {
		body = io.LimitReader(body, t.SizeBytes+1)
	} else {
		body = io.LimitReader(body, maxResponseBytes)
	}

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h), body)
	if err != nil {
		return n, fmt.Errorf("read blob %s: %w", t.Digest, err)
	}
	if t.SizeBytes > 0 && n != t.SizeBytes {
		return n, fmt.Errorf("blob %s: read %d bytes, want %d", t.Digest, n, t.SizeBytes)
	}

	got := cache.DigestAlgorithm + ":" + hex.EncodeToString(h.Sum(nil))
	if got != t.Digest {
		return n, fmt.Errorf("blob %s content-addresses to %s", t.Digest, got)
	}
	return n, nil
}

// HitFetcher returns a per-digest blob opener for a negotiate hit, streaming
// each referenced CAS blob over its presigned GET transfer on demand. It is the
// lazy half of "Build without the Bytes": a blob downloads only when the opener
// is called for it. The opener's signature matches the local store's
// materialize fetch contract, so a hit downloads straight into the store
// without remotecache and store depending on each other. Bytes stream through
// an io.Pipe, so nothing is buffered whole in memory; a download error
// (transport, HTTP status, or digest mismatch) surfaces on the reader.
func (c *Client) HitFetcher(ctx context.Context, result cache.KeyResult) func(digest string) (io.ReadCloser, error) {
	index := make(map[string]cache.BlobTransfer, len(result.Downloads))
	for _, t := range result.Downloads {
		index[t.Digest] = t
	}
	return func(digest string) (io.ReadCloser, error) {
		t, ok := index[digest]
		if !ok {
			return nil, fmt.Errorf("no presigned download for digest %s", digest)
		}
		pr, pw := io.Pipe()
		go func() {
			_, err := c.DownloadBlob(ctx, t, pw)
			_ = pw.CloseWithError(err)
		}()
		return pr, nil
	}
}

// maxDownloadBatchBytes bounds the declared size of the blobs one
// download-batch request asks for. cache-server answers with every blob it can
// serve inline and bounds only each blob (cache.MaxInlineBlobBytes), never the
// whole answer, while Cloud Run refuses a response over 32 MiB and the client
// reads at most maxResponseBytes. JSON carries the bytes in base64, a third
// more, so 4 MiB of blobs comes back in about 5.4 MiB, plus about 100 bytes a
// blob. It is the bound of one upload-batch request (maxInlineBatchBytes), so
// cache-server holds about as much for one download-batch as for one
// upload-batch.
const maxDownloadBatchBytes = 4 << 20 // 4 MiB

// InlineDownload reports whether download-batch may carry the blob: its size
// is known and at most cache.MaxInlineBlobBytes. Any other blob takes its own
// download.
func InlineDownload(t cache.BlobTransfer) bool {
	return t.SizeBytes > 0 && t.SizeBytes <= cache.MaxInlineBlobBytes
}

// DownloadBatches splits small blobs into download-batch requests and keeps
// their order. A request asks for at most cache.MaxKeysPerRequest blobs and at
// most maxDownloadBatchBytes of declared size. The caller passes each blob
// once, and only blobs InlineDownload accepts, so one blob always fits. Each
// request is a slice of blobs whose capacity ends with it, so appending to one
// request never overwrites the next.
func DownloadBatches(blobs []cache.BlobTransfer) [][]cache.BlobTransfer {
	var out [][]cache.BlobTransfer
	start, size := 0, int64(0)
	for i, b := range blobs {
		if i > start && (i-start == cache.MaxKeysPerRequest || size+b.SizeBytes > maxDownloadBatchBytes) {
			out = append(out, blobs[start:i:i])
			start, size = i, 0
		}
		size += b.SizeBytes
	}
	if start < len(blobs) {
		out = append(out, blobs[start:len(blobs):len(blobs)])
	}
	return out
}

// DownloadInline sends one download-batch request for blobs and returns the
// bytes of each blob the answer carries, by digest. Each blob is verified
// against its digest and declared size. A blob the answer leaves out is not an
// error, and neither is a blob that does not verify: it is left out too, and
// unverified reports why. The caller downloads such a blob another way, so one
// bad blob does not cost the other blobs of the answer. A failed request is an
// error, and then no blob is returned.
func (c *Client) DownloadInline(ctx context.Context, blobs []cache.BlobTransfer) (got map[string][]byte, unverified []error, err error) {
	size := make(map[string]int64, len(blobs))
	digests := make([]string, 0, len(blobs))
	for _, b := range blobs {
		if _, dup := size[b.Digest]; dup {
			continue
		}
		size[b.Digest] = b.SizeBytes
		digests = append(digests, b.Digest)
	}
	req := &cache.DownloadBatchRequest{ProtocolVersion: cache.ProtocolVersion, Digests: digests}
	cache.NormalizeDownloadBatchRequest(req)
	resp, err := c.DownloadBatch(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	got = make(map[string][]byte, len(resp.Blobs))
	for _, b := range resp.Blobs {
		data, err := verifiedInlineBlob(b, size[b.Digest])
		if err != nil {
			unverified = append(unverified, err)
			continue
		}
		got[b.Digest] = data
	}
	return got, unverified, nil
}

// BatchHitFetcher returns a blob opener for a negotiate hit that first asks the
// cache server for all small blobs inline through download-batch, then falls
// back to each blob's presigned GET when the batch path fails, omits a digest,
// or carries bytes that do not verify.
// Large blobs always use their presigned GET to keep response size bounded. It
// splits the small blobs with DownloadBatches, so one request asks for at most
// cache.MaxKeysPerRequest blobs and maxDownloadBatchBytes of them.
func (c *Client) BatchHitFetcher(ctx context.Context, result cache.KeyResult) func(digest string) (io.ReadCloser, error) {
	single := c.HitFetcher(ctx, result)
	small := make(map[string]bool)
	var blobs []cache.BlobTransfer
	for _, t := range result.Downloads {
		if !InlineDownload(t) || small[t.Digest] {
			continue
		}
		small[t.Digest] = true
		blobs = append(blobs, t)
	}
	if len(blobs) == 0 {
		return single
	}

	var (
		once  sync.Once
		got   map[string][]byte
		batch error
	)
	load := func() {
		got = make(map[string][]byte, len(blobs))
		for _, chunk := range DownloadBatches(blobs) {
			data, _, err := c.DownloadInline(ctx, chunk)
			if err != nil {
				batch = err
				return
			}
			maps.Copy(got, data)
		}
	}

	return func(digest string) (io.ReadCloser, error) {
		if !small[digest] {
			return single(digest)
		}
		once.Do(load)
		if batch == nil {
			if data, ok := got[digest]; ok {
				return io.NopCloser(bytes.NewReader(data)), nil
			}
		}
		return single(digest)
	}
}

// VerifiedInlineBlob decodes an inline blob (gzipped or raw — the digest is
// always the uncompressed content hash) and returns its bytes only when they
// content-address to the declared digest and match the declared size. It is
// exported because the object-cache read path stages inline bytes straight into
// the blob-exchange directory and must apply the same verification the batched
// hit fetcher does.
func VerifiedInlineBlob(b cache.InlineBlob, sizeBytes int64) ([]byte, error) {
	return verifiedInlineBlob(b, sizeBytes)
}

func verifiedInlineBlob(b cache.InlineBlob, sizeBytes int64) ([]byte, error) {
	body, err := decodeBlobReader(bytes.NewReader(b.Data), int64(len(b.Data)), sizeBytes)
	if err != nil {
		return nil, fmt.Errorf("blob %s: %w", b.Digest, err)
	}
	if sizeBytes > 0 {
		body = io.LimitReader(body, sizeBytes+1)
	} else {
		body = io.LimitReader(body, maxResponseBytes)
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("read blob %s: %w", b.Digest, err)
	}
	if sizeBytes > 0 && int64(len(data)) != sizeBytes {
		return nil, fmt.Errorf("blob %s: read %d bytes, want %d", b.Digest, len(data), sizeBytes)
	}
	sum := sha256.Sum256(data)
	got := cache.DigestAlgorithm + ":" + hex.EncodeToString(sum[:])
	if got != b.Digest {
		return nil, fmt.Errorf("blob %s content-addresses to %s", b.Digest, got)
	}
	return data, nil
}
