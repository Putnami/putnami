package remotecache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
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

// DownloadBlob fetches one CAS blob over its presigned GET URL into dst,
// verifying the streamed bytes content-address to t.Digest. Like UploadBlob,
// the presigned URL is self-authenticating (its credential is in the URL), so
// the per-user bearer token is deliberately NOT attached — only the
// server-supplied Headers are sent. It returns the number of bytes written.
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

	resp, err := c.httpClient.Do(req) //nolint:gosec // G704: request targets the cache server's presigned download grant, not user-tainted input
	if err != nil {
		return 0, fmt.Errorf("download blob %s: %w", t.Digest, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		return 0, statusError("download", resp.StatusCode, body)
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

// BatchHitFetcher returns a blob opener for a negotiate hit that first asks the
// cache server for all small blobs inline through download-batch, then falls
// back to each blob's presigned GET when the batch path fails or omits a digest.
// Large blobs always use their presigned GET to keep response size bounded.
func (c *Client) BatchHitFetcher(ctx context.Context, result cache.KeyResult) func(digest string) (io.ReadCloser, error) {
	single := c.HitFetcher(ctx, result)
	small := make(map[string]cache.BlobTransfer)
	var digests []string
	for _, t := range result.Downloads {
		if t.SizeBytes <= 0 || t.SizeBytes > cache.MaxInlineBlobBytes {
			continue
		}
		if _, ok := small[t.Digest]; ok {
			continue
		}
		small[t.Digest] = t
		digests = append(digests, t.Digest)
	}
	if len(digests) == 0 {
		return single
	}

	var (
		once  sync.Once
		got   map[string][]byte
		batch error
	)
	load := func() {
		req := &cache.DownloadBatchRequest{ProtocolVersion: cache.ProtocolVersion, Digests: digests}
		cache.NormalizeDownloadBatchRequest(req)
		resp, err := c.DownloadBatch(ctx, req)
		if err != nil {
			batch = err
			return
		}
		got = make(map[string][]byte, len(resp.Blobs))
		sizeByDigest := make(map[string]int64, len(small))
		for d, t := range small {
			sizeByDigest[d] = t.SizeBytes
		}
		for _, b := range resp.Blobs {
			data, err := verifiedInlineBlob(b, sizeByDigest[b.Digest])
			if err != nil {
				batch = err
				return
			}
			got[b.Digest] = data
		}
	}

	return func(digest string) (io.ReadCloser, error) {
		if _, ok := small[digest]; !ok {
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
