package deliverycli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"sync"

	"go.putnami.dev/cloud/extension/internal/deliverycli/internal/remotecache"
	cache "go.putnami.dev/protocol/cache"
)

// The small files of every hit, fetched by the prefetch. A small file is one of
// at most cache.MaxInlineBlobBytes, which download-batch returns inline. Each
// restore used to ask download-batch for the small files of its own entry, so
// a run that restored N entries sent N requests, most of them for one file of
// a few hundred bytes. The prefetch now asks for the small files of every hit
// it negotiated in a few requests, and a restore finds them in the exchange
// directory. A late negotiate, which several restores share, does the same for
// the hits it negotiated (see cachenegotiate.go).
//
// A restore that needs one of those files waits for the request that carries
// it, never for the others, and then reads the exchange directory. When that
// request failed, the restore asks for the files again in its own
// download-batch, as before. A file the server left out of its answer is not
// inline, so the restore downloads it on its own.

// inlineFetchConcurrency bounds the download-batch requests the prefetch, or
// one late negotiate, has in flight. A run's small files usually fit in a few
// requests; the bound keeps a large run from asking cache-server to hold more
// than about 4 answers of at most maxDownloadBatchBytes each at once.
const inlineFetchConcurrency = 4

// inlineBatch is one download-batch request of the prefetch or of a late
// negotiate.
type inlineBatch struct {
	blobs []cache.BlobTransfer
	// done is closed when the request has ended and the blobs it returned are
	// in the exchange directory.
	done chan struct{}
	// answered reports that the server answered the request. It is written
	// before done is closed and read only after.
	answered bool
}

// inlineRequests records the download-batch requests of the prefetch and of
// the late negotiates for the restores.
type inlineRequests struct {
	mu sync.Mutex
	// batchOf maps a digest to the request that asks for it.
	batchOf map[string]*inlineBatch
	// placed holds the size of each blob a request placed that no restore has
	// counted yet.
	placed map[string]int64
}

// batch returns the request that asks for digest, or nil.
func (p *inlineRequests) batch(digest string) *inlineBatch {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.batchOf[digest]
}

// record registers blobs as requests and returns them. A blob an earlier
// request asks for is left out.
func (p *inlineRequests) record(blobs []cache.BlobTransfer) []*inlineBatch {
	p.mu.Lock()
	defer p.mu.Unlock()
	blobs = slices.DeleteFunc(blobs, func(t cache.BlobTransfer) bool { return p.batchOf[t.Digest] != nil })
	if len(blobs) == 0 {
		return nil
	}
	if p.batchOf == nil {
		p.batchOf = make(map[string]*inlineBatch, len(blobs))
		p.placed = make(map[string]int64, len(blobs))
	}
	var out []*inlineBatch
	for _, chunk := range remotecache.DownloadBatches(blobs) {
		b := &inlineBatch{blobs: chunk, done: make(chan struct{})}
		for _, t := range chunk {
			p.batchOf[t.Digest] = b
		}
		out = append(out, b)
	}
	return out
}

// place records that a request wrote n bytes for digest.
func (p *inlineRequests) place(digest string, n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.placed[digest] = n
}

// claim returns the size of a blob a request placed, once: the first restore
// that uses the blob counts it, as when that restore downloaded it itself.
func (p *inlineRequests) claim(digest string) int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.placed[digest]
	delete(p.placed, digest)
	return n
}

// planInlinePrefetch registers the download-batch requests for the small files
// of the hits in batches, and returns them. runPrefetch calls it before it
// releases the restores waiting on the index, so a restore of a hit always
// finds the request that carries its files.
//
// A hit negotiated in minimal mode names no download, so the result-only keys
// add no file: the prefetch fetches only files core said it will read.
func (s *providerSession) planInlinePrefetch(batches []prefetchBatch) []*inlineBatch {
	var blobs []cache.BlobTransfer
	seen := make(map[string]bool)
	for _, b := range batches {
		if b.err != nil {
			continue
		}
		blobs = s.appendInlineBlobs(blobs, seen, b.keys, b.results)
	}
	return s.inline.record(blobs)
}

// planInlineLate registers the download-batch requests for the small files of
// the hits of one late negotiate, and returns them. sendLateNegotiate calls it
// before it answers the restores of the group, as runPrefetch does. Every key
// of the group is a restore core sent, so the request fetches only files core
// will read: a result-only restore negotiates in minimal mode, which names no
// download.
func (s *providerSession) planInlineLate(keys []string, results map[string]providerCacheResult) []*inlineBatch {
	return s.inline.record(s.appendInlineBlobs(nil, make(map[string]bool), keys, results))
}

// appendInlineBlobs appends to blobs the small files of the hits among keys
// that a download-batch should fetch, in the order of keys. It leaves out a
// file core already has, one the exchange directory holds, and one seen
// records. record then leaves out one an earlier request asks for.
func (s *providerSession) appendInlineBlobs(blobs []cache.BlobTransfer, seen map[string]bool, keys []string, results map[string]providerCacheResult) []cache.BlobTransfer {
	for _, k := range keys {
		r := results[k]
		if !r.result.Hit {
			continue
		}
		for _, t := range r.result.Downloads {
			if !remotecache.InlineDownload(t) || seen[t.Digest] || s.known[t.Digest] {
				continue
			}
			seen[t.Digest] = true
			path, ok := cache.BlobExchangePath(s.exchangeDir, t.Digest)
			if !ok || blobPresent(path, t.SizeBytes) {
				continue
			}
			blobs = append(blobs, t)
		}
	}
	return blobs
}

// fetchInline sends the planned requests, inlineFetchConcurrency at a time,
// and closes the done channel of every one of them, whatever happens. scope
// names the sender in the log: the prefetch or the late restores.
func (s *providerSession) fetchInline(scope string, batches []*inlineBatch) {
	sem := make(chan struct{}, inlineFetchConcurrency)
	var wg sync.WaitGroup
	for _, b := range batches {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(b.done)
			select {
			case sem <- struct{}{}:
			case <-s.ctx.Done():
				return
			}
			defer func() { <-sem }()
			s.fetchInlineBatch(scope, b)
		}()
	}
	wg.Wait()
}

// fetchInlineBatch sends one request and writes each blob of the answer to the
// exchange directory, atomically. A request that fails places nothing: the
// restores of its files ask for them again. A blob of the answer that does not
// verify is not placed, and the restore of its file downloads it.
func (s *providerSession) fetchInlineBatch(scope string, b *inlineBatch) {
	data, unverified, err := s.client.DownloadInline(s.ctx, b.blobs)
	if err != nil {
		s.logCacheError(fmt.Sprintf("%s download-batch of %d files", scope, len(b.blobs)), err)
		return
	}
	b.answered = true
	if len(unverified) > 0 {
		s.log(fmt.Sprintf("%s download-batch: %d of %d files do not verify; their restores download them: %v", scope, len(unverified), len(b.blobs), unverified[0]))
	}
	var failed int
	var first error
	for _, t := range b.blobs {
		blob, ok := data[t.Digest]
		if !ok {
			continue
		}
		path, _ := cache.BlobExchangePath(s.exchangeDir, t.Digest) // appendInlineBlobs kept only valid digests
		open := func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(blob)), nil }
		n, err := writeExchangeBlob(open, t.Digest, path)
		if err != nil {
			failed++
			if first == nil {
				first = err
			}
			continue
		}
		s.inline.place(t.Digest, n)
	}
	if failed > 0 {
		s.log(fmt.Sprintf("%s download-batch: %d files not placed; their restores download them: %v", scope, failed, first))
	}
}

// awaitInline waits for the requests that carry blobs and sorts the blobs they
// did not place. retry holds the blobs of a request that failed: the restore
// asks for them again in its own download-batch. direct holds the blobs the
// server answered without: they are not inline, so the restore downloads each
// one. counted is the size of the placed blobs this restore is the first to
// use. It returns early with the context's error.
func (s *providerSession) awaitInline(ctx context.Context, blobs []cache.BlobTransfer) (retry, direct []cache.BlobTransfer, counted int64, err error) {
	for _, t := range blobs {
		b := s.inline.batch(t.Digest)
		select {
		case <-b.done:
		case <-ctx.Done():
			return nil, nil, counted, ctx.Err()
		}
		path, _ := cache.BlobExchangePath(s.exchangeDir, t.Digest)
		switch {
		case blobPresent(path, t.SizeBytes):
			counted += s.inline.claim(t.Digest)
		case b.answered:
			direct = append(direct, t)
		default:
			retry = append(retry, t)
		}
	}
	return retry, direct, counted, nil
}
