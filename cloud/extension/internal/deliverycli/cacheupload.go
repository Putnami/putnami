package deliverycli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"go.putnami.dev/cloud/extension/internal/deliverycli/internal/remotecache"
	cache "go.putnami.dev/protocol/cache"
)

// The task-cache write path. upload queues a built entry and answers at once;
// one uploader goroutine stores the queue a window at a time, with one
// find-missing and one commit-batch per window instead of a store and a commit
// per entry. A window holds metadata only: the blobs stay in the blob-exchange
// directory core exported them to, and are read from there when the server asks
// for them.
const (
	// uploadWindowEntries caps the entries of one window: a CI run builds a few
	// hundred entries, so 64 keeps its writes to a handful of exchanges.
	uploadWindowEntries = 64
	// uploadWindowBlobs caps the distinct blobs of one window at the protocol's
	// per-request limit, so a window asks about its blobs in one find-missing.
	// One entry with more blobs than that still stores: it goes alone, and its
	// blobs are asked about in several find-missing requests.
	uploadWindowBlobs = cache.MaxKeysPerRequest
	// uploadWindowBytes caps the encoded entries of one window at half the
	// 1 MiB the cache server reads of a commit-batch body, so that body always
	// fits with room for its envelope. The 1 MiB is the route's own bound: every
	// cache-server route but upload-batch reads at most that, whatever the
	// server's body limit (16 MiB) allows. The window's small blobs do not count
	// here: they travel in upload-batch requests, bounded on their own
	// (remotecache.maxInlineBatchBytes).
	uploadWindowBytes = 512 << 10
	// uploadWindowBlobConcurrency bounds the blobs of one window uploading at
	// once: before windows each entry uploaded on its own goroutine, so one
	// uploader needs the bound the restore path has, or it trails the build.
	uploadWindowBlobConcurrency = restoreConcurrency
	// uploadFallbackConcurrency bounds the entries stored at once, one store and
	// one commit each, against a server without the batched path.
	uploadFallbackConcurrency = 8
)

// uploadWindowIdle flushes a partial window once no entry has arrived for this
// long: a build finishing tasks every second keeps filling one window, and a
// pause makes its entries durable well before the summary. It is a variable so
// a test can change it; production never does. A session reads it once, when it
// is created.
var uploadWindowIdle = 2 * time.Second

// uploadRetry is how a window meets a failure. A find-missing, upload-batch or
// commit-batch that fails for a reason that can pass (a 5xx, a 429, a network
// error) is tried 3 times in all, after waits of 0.5 s and 2 s. A window that
// still fails as a whole is then stored once more entry by entry, so one entry
// the server refuses does not cost the others. A 403 is never tried again: it
// closes the write latch at once. It is a variable so a test can shorten the
// waits; a session reads it once, when it is created.
var uploadRetry = remotecache.WriteRetry{Attempts: 3, Backoff: 500 * time.Millisecond, SplitWindow: true}

// uploadWindow is the queue between upload and the uploader goroutine.
type uploadWindow struct {
	mu    sync.Mutex
	queue []pendingUpload
	// last is when the newest entry was queued; idle measures from it.
	last time.Time
	// running is set while an uploader goroutine owns the queue. It exits once
	// the queue is empty, so a session that uploads nothing runs none, and the
	// summary's wait on the background work returns once everything is stored.
	running bool
	// stopped is closed when the running uploader exits. Each uploader gets a
	// new one.
	stopped chan struct{}
	// drain is set by the summary and by shutdown: the run is ending, so every
	// queued entry flushes now instead of waiting for a full window or the idle
	// delay. It stays set for the rest of the run.
	drain bool
	// wake is a one-slot signal that makes the uploader look at the queue
	// before its idle delay elapses: a full window or a drain.
	wake chan struct{}
	// idle is uploadWindowIdle as the session read it at creation.
	idle time.Duration
	// retry is uploadRetry as the session read it at creation.
	retry remotecache.WriteRetry
	// inflight counts the entries of the window being stored, so a drain that
	// gives up can say how many entries it leaves.
	inflight int
	// drainSince is when the run-end drain began. The drain measures a stall
	// from it or from the last progress, whichever is later.
	drainSince time.Time
	// progress is when an upload last moved forward, in Unix nanoseconds: a
	// byte of a blob read to be compressed or sent, a PUT answered, a control
	// exchange completed.
	progress atomic.Int64
	// probeLog logs a failed capabilities probe once, not once per window.
	probeLog sync.Once
}

// pendingUpload is one queued entry and the encoded size of its commit entry.
type pendingUpload struct {
	in    remotecache.StoreInput
	bytes int
}

// init prepares the queue of a new session. It sets the fields in place: an
// uploadWindow holds a mutex and is never copied.
func (w *uploadWindow) init() {
	w.wake = make(chan struct{}, 1)
	w.idle = uploadWindowIdle
	w.retry = uploadRetry
}

// touch records that an upload moved forward.
func (w *uploadWindow) touch() { w.progress.Store(time.Now().UnixNano()) }

// stoppedSignal returns a channel that is closed once no uploader runs. It is
// already closed when none does.
func (w *uploadWindow) stoppedSignal() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.running {
		return closedSignal
	}
	return w.stopped
}

// signal wakes the uploader without blocking.
func (w *uploadWindow) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// enqueueUpload queues an entry for the uploader, starting the uploader when
// none runs. It runs on the read loop, as summary and shutdown do.
func (s *providerSession) enqueueUpload(in remotecache.StoreInput) {
	size := 0
	if encoded, err := json.Marshal(cache.CommitEntry{Key: in.Key, Result: in.Result, Manifest: in.Manifest}); err == nil {
		size = len(encoded)
	}
	w := &s.uploads
	w.mu.Lock()
	w.queue = append(w.queue, pendingUpload{in: in, bytes: size})
	w.last = time.Now()
	start := !w.running
	if start {
		w.running = true
		w.stopped = make(chan struct{})
		s.bg.Add(1)
	}
	full := len(w.queue) >= uploadWindowEntries
	w.mu.Unlock()
	if start {
		go s.runUploader()
	}
	if full {
		w.signal()
	}
}

// drainUploads asks the uploader to flush every queued entry now. The caller
// then waits on the background work, which the uploader leaves once the queue
// is empty (awaitIdle bounds that wait). The first call starts the clock the
// drain measures a stall from.
func (s *providerSession) drainUploads() {
	w := &s.uploads
	w.mu.Lock()
	w.drain = true
	if w.drainSince.IsZero() {
		w.drainSince = time.Now()
	}
	w.mu.Unlock()
	w.signal()
}

// runUploader stores the queue a window at a time until it is empty. Its
// context reports each step an upload moves forward, which is what the run-end
// drain watches.
func (s *providerSession) runUploader() {
	defer s.bg.Done()
	w := &s.uploads
	ctx := remotecache.WithUploadProgress(s.ctx, w.touch)
	for {
		window, ok := s.nextUploadWindow()
		if !ok {
			return
		}
		s.flushUploadWindow(ctx, window)
		w.mu.Lock()
		w.inflight = 0
		w.mu.Unlock()
	}
}

// nextUploadWindow waits until a window is due and takes it off the queue: a
// full window at once, a partial one after the idle delay or on a drain. It
// reports false, and marks the uploader stopped, once the queue is empty or
// the session is closed. A session closes after its run-end drain (the
// summary, the shutdown, or the exit when stdin closes without them), so the
// entries dropped here are the ones that drain gave up on.
func (s *providerSession) nextUploadWindow() ([]remotecache.StoreInput, bool) {
	w := &s.uploads
	for {
		w.mu.Lock()
		if len(w.queue) == 0 || s.ctx.Err() != nil {
			w.queue = nil
			w.running = false
			close(w.stopped)
			w.mu.Unlock()
			return nil, false
		}
		n, full := uploadWindowSize(w.queue)
		wait := w.idle - time.Since(w.last)
		if full || w.drain || wait <= 0 {
			window := make([]remotecache.StoreInput, n)
			for i := range window {
				window[i] = w.queue[i].in
			}
			w.queue = w.queue[n:]
			w.inflight = n
			w.mu.Unlock()
			return window, true
		}
		w.mu.Unlock()
		timer := time.NewTimer(wait)
		select {
		case <-w.wake:
		case <-timer.C:
		case <-s.ctx.Done():
		}
		timer.Stop()
	}
}

// uploadWindowSize returns how many queued entries the next window carries,
// and whether that is a full window. A window stops at uploadWindowEntries, at
// the entry whose blobs would take it past one find-missing request, or at the
// entry whose commit entry would take it past one commit-batch body. It is
// never zero for a non-empty queue: an entry too large for any window goes
// alone. The client then asks about its blobs in find-missing requests of at
// most cache.MaxKeysPerRequest blobs each.
func uploadWindowSize(queue []pendingUpload) (int, bool) {
	blobs := make(map[string]bool)
	bytes := 0
	for i, p := range queue {
		if i == uploadWindowEntries {
			return i, true
		}
		if p.in.Manifest != nil {
			for _, f := range p.in.Manifest.Files {
				blobs[f.Digest] = true
			}
		}
		if i > 0 && (len(blobs) > uploadWindowBlobs || bytes+p.bytes > uploadWindowBytes) {
			return i, true
		}
		bytes += p.bytes
	}
	return len(queue), len(queue) == uploadWindowEntries
}

// flushUploadWindow stores one window. A window the latch closed is dropped
// unsent. The write path comes from the session's one capabilities probe: the
// batched path when the server serves find-missing and commit-batch, else one
// store and one commit per entry. A failure is logged and the run goes on: an
// upload never changes the run's verdict.
//
// A window does not fail as one. An entry is counted as uploaded only when
// the server committed it, and the server is asked to commit it only once all
// its blobs are stored; an entry whose blob could not be stored is logged and
// left out, and the other entries of the window still commit.
func (s *providerSession) flushUploadWindow(ctx context.Context, window []remotecache.StoreInput) {
	if s.writesLatched() {
		return
	}
	window = uniqueUploadKeys(window)
	src := blobExchangeSource{dir: s.exchangeDir}
	wp, batched := s.taskWritePath()
	if !batched {
		s.storeEach(ctx, window, src)
		return
	}
	out, err := s.client.StoreBatch(ctx, window, src, nil, wp)
	if err != nil {
		s.logUploadError(fmt.Sprintf("upload window (%d entries)", len(window)), err)
		return
	}
	committed, failed := 0, 0
	var firstKey string
	for _, in := range window {
		switch {
		case out.Committed[in.Key]:
			committed++
		case out.Failed[in.Key] != nil:
			if failed == 0 {
				firstKey = in.Key
			}
			failed++
		}
	}
	s.addStats(func(st *cache.SummaryResult) {
		st.UploadedCount += committed
		st.UploadedBytes += out.BytesUploaded
	})
	if failed > 0 {
		s.logUploadError(fmt.Sprintf("upload window: %d of %d entries are not stored; first failure, entry %s", failed, len(window), short(firstKey)), out.Failed[firstKey])
	}
	if refused := len(window) - committed - failed; refused > 0 {
		s.log(fmt.Sprintf("upload window: the cache server committed %d of %d entries", committed, len(window)))
	}
}

// uniqueUploadKeys keeps the first entry of each key: a key addresses one
// entry, and commit-batch answers one result per key.
func uniqueUploadKeys(window []remotecache.StoreInput) []remotecache.StoreInput {
	seen := make(map[string]bool, len(window))
	out := window[:0:0]
	for _, in := range window {
		if seen[in.Key] {
			continue
		}
		seen[in.Key] = true
		out = append(out, in)
	}
	return out
}

// storeEach stores a window entry by entry, one store and one commit each,
// for a server without the batched path. Entries store concurrently, as they
// did before windows, and each one checks the latch before it is sent.
func (s *providerSession) storeEach(ctx context.Context, window []remotecache.StoreInput, src remotecache.BlobSource) {
	sem := make(chan struct{}, uploadFallbackConcurrency)
	var wg sync.WaitGroup
	for _, in := range window {
		wg.Add(1)
		go func(in remotecache.StoreInput) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if s.writesLatched() {
				return
			}
			out, err := s.client.StoreResult(ctx, in, src)
			if err != nil {
				s.logUploadError("upload "+short(in.Key), err)
				return
			}
			if out.Commit == nil || !out.Commit.Committed {
				s.log(fmt.Sprintf("upload %s: the cache server did not commit the entry", short(in.Key)))
				return
			}
			s.addStats(func(st *cache.SummaryResult) {
				st.UploadedCount++
				st.UploadedBytes += out.BytesUploaded
			})
		}(in)
	}
	wg.Wait()
}

// logUploadError logs a failed store. A 403 that latched the write path is
// left out: the auth record and the latch line already said it once.
func (s *providerSession) logUploadError(scope string, err error) {
	if status, refused := cacheAuthRefusal(err); refused && status == http.StatusForbidden && s.writesLatched() {
		return
	}
	s.logCacheError(scope, err)
}

// taskWritePath reads the task-cache write path from the session's one
// capabilities probe. A probe that failed selects the per-entry path, which
// every server serves, and is logged once.
func (s *providerSession) taskWritePath() (remotecache.WritePath, bool) {
	caps, err := s.serverCapabilities()
	if err != nil {
		s.uploads.probeLog.Do(func() { s.logCacheError("upload capabilities", err) })
		return remotecache.WritePath{}, false
	}
	wp, batched := remotecache.TaskWritePath(caps)
	wp.BlobConcurrency = uploadWindowBlobConcurrency
	wp.Retry = s.uploads.retry
	return wp, batched
}
