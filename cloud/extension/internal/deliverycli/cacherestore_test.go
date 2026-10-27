package deliverycli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	cache "go.putnami.dev/protocol/cache"
)

// The restore tests: a download negotiated when the run starts does not last
// until the restore that uses it. See restoreTransferMaxAge.

// signedURLLifetime is how long the cache server signs a download URL for. A
// storage token lasts longer.
const signedURLLifetime = 15 * time.Minute

// restoreCacheServer is a fake cache server for the restore tests. It stamps
// the downloads of each negotiate with a generation, as the cache server signs
// a URL or hands out a storage token, and it refuses a download whose
// generation it has expired.
type restoreCacheServer struct {
	srv *httptest.Server
	// tokenInHeader carries the generation in an Authorization header, as a
	// storage token is carried. Otherwise it is in the URL, as a signature is.
	tokenInHeader bool
	// serveBatch makes the server answer download-batch.
	serveBatch bool

	mu sync.Mutex
	// entries maps a cache key to the blobs of its entry, and blobs a digest to
	// its content.
	entries map[string][][]byte
	blobs   map[string][]byte
	// generation is the last one stamped. A download of a generation below
	// liveFrom is answered with refusal.
	generation int
	liveFrom   int
	refusal    int
	// refuseAll answers every download with this status, whatever its generation.
	refuseAll int
	// expireAfter expires every generation stamped so far once this many
	// downloads passed.
	expireAfter int
	passed      int
	// failNegotiates answers this many of the next negotiates with a 400.
	failNegotiates int

	negotiates []negotiateCall
	downloads  []blobDownload
	batches    int
}

// blobDownload is one blob GET the server received.
type blobDownload struct {
	digest     string
	generation int
	status     int
}

func newRestoreCacheServer(t *testing.T, entries map[string][][]byte) *restoreCacheServer {
	t.Helper()
	f := &restoreCacheServer{entries: entries, blobs: map[string][]byte{}, refusal: http.StatusForbidden}
	for _, blobs := range entries {
		for _, blob := range blobs {
			f.blobs[cache.DigestOf(blob)] = blob
		}
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *restoreCacheServer) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == cache.NegotiatePath:
		f.negotiate(w, r)
	case r.URL.Path == cache.DownloadBatchPath:
		f.downloadBatch(w, r)
	case strings.HasPrefix(r.URL.Path, "/blob/"):
		f.download(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *restoreCacheServer) negotiate(w http.ResponseWriter, r *http.Request) {
	var nreq cache.NegotiateRequest
	_ = json.NewDecoder(r.Body).Decode(&nreq)
	call := negotiateCall{mode: nreq.Mode}
	for _, k := range nreq.Keys {
		call.keys = append(call.keys, k.Key)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.negotiates = append(f.negotiates, call)
	if f.failNegotiates > 0 {
		f.failNegotiates--
		http.Error(w, "negotiate failed", http.StatusBadRequest)
		return
	}
	f.generation++
	results := make([]map[string]any, 0, len(nreq.Keys))
	for _, k := range nreq.Keys {
		blobs, hit := f.entries[k.Key]
		if !hit {
			results = append(results, map[string]any{"key": k.Key, "hit": false})
			continue
		}
		var files, downloads []map[string]any
		size := 0
		for i, blob := range blobs {
			digest := cache.DigestOf(blob)
			size += len(blob)
			files = append(files, map[string]any{"path": fmt.Sprintf("out/%d.js", i), "digest": digest, "mode": 0o644, "size": len(blob)})
			transfer := map[string]any{"digest": digest, "method": cache.TransferGet, "sizeBytes": len(blob)}
			if f.tokenInHeader {
				transfer["url"] = f.srv.URL + "/blob/" + digest
				transfer["headers"] = map[string]string{"Authorization": fmt.Sprintf("Bearer generation-%d", f.generation)}
			} else {
				transfer["url"] = fmt.Sprintf("%s/blob/%s?generation=%d", f.srv.URL, digest, f.generation)
			}
			downloads = append(downloads, transfer)
		}
		entry := map[string]any{
			"key":      k.Key,
			"hit":      true,
			"result":   map[string]any{"status": "success", "sizeBytes": size},
			"manifest": map[string]any{"files": files},
		}
		if nreq.Mode != cache.ModeMinimal {
			entry["downloads"] = downloads
		}
		results = append(results, entry)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"protocolVersion": cache.ProtocolVersion, "results": results})
}

func (f *restoreCacheServer) download(w http.ResponseWriter, r *http.Request) {
	digest := strings.TrimPrefix(r.URL.Path, "/blob/")
	generation := 0
	if f.tokenInHeader {
		_, _ = fmt.Sscanf(r.Header.Get("Authorization"), "Bearer generation-%d", &generation)
	} else {
		generation, _ = strconv.Atoi(r.URL.Query().Get("generation"))
	}
	f.mu.Lock()
	blob, held := f.blobs[digest]
	status := http.StatusOK
	switch {
	case f.refuseAll != 0:
		status = f.refuseAll
	case generation < f.liveFrom:
		status = f.refusal
	case !held:
		status = http.StatusNotFound
	}
	f.downloads = append(f.downloads, blobDownload{digest: digest, generation: generation, status: status})
	if status == http.StatusOK {
		f.passed++
		if f.passed == f.expireAfter {
			f.liveFrom = f.generation + 1
		}
	}
	f.mu.Unlock()
	if status != http.StatusOK {
		http.Error(w, "this download is refused", status)
		return
	}
	_, _ = w.Write(blob)
}

func (f *restoreCacheServer) downloadBatch(w http.ResponseWriter, r *http.Request) {
	var req cache.DownloadBatchRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	f.batches++
	serve := f.serveBatch
	resp := cache.DownloadBatchResponse{ProtocolVersion: cache.ProtocolVersion}
	for _, digest := range req.Digests {
		if blob, held := f.blobs[digest]; held {
			resp.Blobs = append(resp.Blobs, cache.InlineBlob{Digest: digest, Data: blob})
		}
	}
	f.mu.Unlock()
	if !serve {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// expire makes the server refuse every download stamped so far, with status.
func (f *restoreCacheServer) expire(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.liveFrom = f.generation + 1
	f.refusal = status
}

// drop removes an entry, as an eviction does.
func (f *restoreCacheServer) drop(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.entries, key)
}

// calls returns the negotiates the server received, in order.
func (f *restoreCacheServer) calls() []negotiateCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]negotiateCall(nil), f.negotiates...)
}

// batchRequests returns how many download-batch requests the server received.
func (f *restoreCacheServer) batchRequests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.batches
}

// failNextNegotiate answers the next negotiate with a 400.
func (f *restoreCacheServer) failNextNegotiate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNegotiates = 1
}

// seen returns how many negotiates the server received, and its blob GETs.
func (f *restoreCacheServer) seen() (int, []blobDownload) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.negotiates), append([]blobDownload(nil), f.downloads...)
}

// wantDownloads fails unless the server received exactly these negotiates and
// these blob GETs, each a generation and the status it was answered with.
func (f *restoreCacheServer) wantDownloads(t *testing.T, negotiates int, want ...blobDownload) {
	t.Helper()
	gotNegotiates, got := f.seen()
	if gotNegotiates != negotiates {
		t.Errorf("negotiates = %d, want %d", gotNegotiates, negotiates)
	}
	if len(got) != len(want) {
		t.Fatalf("blob GETs = %+v, want %d of them", got, len(want))
	}
	for i := range want {
		if got[i].generation != want[i].generation || got[i].status != want[i].status {
			t.Errorf("blob GET %d = generation %d answered %d, want generation %d answered %d",
				i, got[i].generation, got[i].status, want[i].generation, want[i].status)
		}
	}
}

// fakeClock is a session clock a test moves by hand. It is also a drainClock:
// a wait it times ends when the test moves the clock past it.
type fakeClock struct {
	mu      sync.Mutex
	t       time.Time
	waiters map[*fakeWait]bool
	// timed, when set, is closed and cleared as a wait is timed (awaitWaiter).
	timed chan struct{}
}

// awaitWaiter blocks until a wait is timed on the clock, and reports false
// when ctx ends first. A test that moves the clock as the code under test
// works holds that work until the code under test waits on the clock.
func (c *fakeClock) awaitWaiter(ctx context.Context) bool {
	for {
		c.mu.Lock()
		if len(c.waiters) > 0 {
			c.mu.Unlock()
			return true
		}
		if c.timed == nil {
			c.timed = make(chan struct{})
		}
		timed := c.timed
		c.mu.Unlock()
		select {
		case <-timed:
		case <-ctx.Done():
			return false
		}
	}
}

// fakeWait is a wait the fake clock times: fired closes once the clock reaches
// at.
type fakeWait struct {
	at    time.Time
	fired chan struct{}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	for w := range c.waiters {
		if !c.t.Before(w.at) {
			close(w.fired)
			delete(c.waiters, w)
		}
	}
}

func (c *fakeClock) after(d time.Duration) (<-chan struct{}, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := &fakeWait{at: c.t.Add(d), fired: make(chan struct{})}
	if d <= 0 {
		close(w.fired)
		return w.fired, func() {}
	}
	if c.waiters == nil {
		c.waiters = make(map[*fakeWait]bool)
	}
	c.waiters[w] = true
	if c.timed != nil {
		close(c.timed)
		c.timed = nil
	}
	return w.fired, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.waiters, w)
	}
}

// startRestoreSession opens a serving session in full mode on a clock the test
// owns. known lists the digests core says it already has.
func startRestoreSession(t *testing.T, f *restoreCacheServer, logw io.Writer, known ...string) (*providerSession, string, *fakeClock) {
	t.Helper()
	t.Setenv("PUTNAMI_CACHE_URL", f.srv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", "test-token")
	exchangeDir := t.TempDir()
	s := newProviderSession(t.TempDir(), logw)
	t.Cleanup(s.close)
	clock := &fakeClock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	s.now = clock.now
	init := mustInit(t, s, &cache.InitializeParams{
		ProtocolVersion: cache.ProviderProtocolMinVersion,
		BlobExchangeDir: exchangeDir,
		Mode:            cache.ModeFull,
		KnownDigests:    known,
	})
	if !init.Ready {
		t.Fatal("expected ready with PUTNAMI_CACHE_URL set")
	}
	return s, exchangeDir, clock
}

// largeBlob returns a blob one byte too large for download-batch: it has to
// come through its own download.
func largeBlob(letter string) []byte {
	return bytes.Repeat([]byte(letter), cache.MaxInlineBlobBytes+1)
}

// wantExchangeBlob fails unless the exchange directory holds blob.
func wantExchangeBlob(t *testing.T, exchangeDir string, blob []byte) {
	t.Helper()
	path, _ := cache.BlobExchangePath(exchangeDir, cache.DigestOf(blob))
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, blob) {
		t.Fatalf("the exchange directory does not hold the blob (%d bytes read, %v)", len(got), err)
	}
}

// The bound leaves a download time to start: a restore that begins just under
// it still has two thirds of a signed URL's life.
func TestTheRestoreBoundIsWellUnderADownloadsLife(t *testing.T) {
	if restoreTransferMaxAge <= 0 || restoreTransferMaxAge > signedURLLifetime/3 {
		t.Fatalf("restoreTransferMaxAge = %s, want more than 0 and at most a third of the %s a signed URL lasts", restoreTransferMaxAge, signedURLLifetime)
	}
	s := newProviderSession(t.TempDir(), io.Discard)
	t.Cleanup(s.close)
	if s.transferMaxAge != restoreTransferMaxAge {
		t.Fatalf("a session's bound = %s, want %s", s.transferMaxAge, restoreTransferMaxAge)
	}
}

// A restore that comes after the bound negotiates its key again before it
// downloads, so it never sends a download the storage would refuse. Before,
// it used the download the prefetch stored when the run started, and a build
// longer than the 15 minutes of a signed URL lost every later hit.
func TestARestoreNegotiatesAgainWhenItsDownloadsAreTooOld(t *testing.T) {
	for _, test := range []struct {
		name          string
		tokenInHeader bool
	}{
		{name: "signed URL"},
		{name: "storage token", tokenInHeader: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			key, blob := resultOnlyKey("a"), largeBlob("a")
			f := newRestoreCacheServer(t, map[string][][]byte{key: {blob}})
			f.tokenInHeader = test.tokenInHeader
			s, exchangeDir, clock := startRestoreSession(t, f, io.Discard)
			prefetchKeys(t, s, []string{key}, nil)

			// The storage no longer takes what the prefetch was given.
			f.expire(http.StatusForbidden)
			clock.advance(restoreTransferMaxAge)

			if got := restoreKey(t, s, key, false).Status; got != cache.RestoreHit {
				t.Fatalf("restore status = %q, want hit", got)
			}
			wantExchangeBlob(t, exchangeDir, blob)
			// One download, of the second negotiate: none was refused.
			f.wantDownloads(t, 2, blobDownload{generation: 2, status: http.StatusOK})
			if calls := f.calls(); len(calls) != 2 || !sameCalls(calls[1:], []negotiateCall{{cache.ModeFull, []string{key}}}) {
				t.Fatalf("negotiates = %+v, want a second one for this key alone in the session's mode", calls)
			}
			if stats := mustSummary(t, s); stats.RestoredCount != 1 || stats.RestoredBytes != int64(len(blob)) {
				t.Errorf("restored = %d entries, %d bytes; want 1 entry of %d bytes", stats.RestoredCount, stats.RestoredBytes, len(blob))
			}
		})
	}
}

// A restore inside the bound uses the download it has: one negotiate for the
// run, not one per restore.
func TestARestoreInsideTheBoundNegotiatesNothing(t *testing.T) {
	key, blob := resultOnlyKey("a"), largeBlob("a")
	f := newRestoreCacheServer(t, map[string][][]byte{key: {blob}})
	s, exchangeDir, clock := startRestoreSession(t, f, io.Discard)
	prefetchKeys(t, s, []string{key}, nil)
	clock.advance(restoreTransferMaxAge - time.Second)

	if got := restoreKey(t, s, key, false).Status; got != cache.RestoreHit {
		t.Fatalf("restore status = %q, want hit", got)
	}
	wantExchangeBlob(t, exchangeDir, blob)
	f.wantDownloads(t, 1, blobDownload{generation: 1, status: http.StatusOK})
}

// An old restore that uses no download of its negotiate costs no request: the
// age matters only for a download the restore is about to send.
func TestAnOldRestoreThatUsesNoDownloadNegotiatesNothing(t *testing.T) {
	key := resultOnlyKey("a")
	large, small := largeBlob("a"), []byte("a small output")

	t.Run("a small file comes through download-batch", func(t *testing.T) {
		f := newRestoreCacheServer(t, map[string][][]byte{key: {small}})
		f.serveBatch = true
		s, exchangeDir, clock := startRestoreSession(t, f, io.Discard)
		prefetchKeys(t, s, []string{key}, nil)
		f.expire(http.StatusForbidden)
		clock.advance(time.Hour)

		if got := restoreKey(t, s, key, false).Status; got != cache.RestoreHit {
			t.Fatalf("restore status = %q, want hit", got)
		}
		wantExchangeBlob(t, exchangeDir, small)
		f.wantDownloads(t, 1)
		if n := f.batchRequests(); n != 1 {
			t.Fatalf("download-batch requests = %d, want 1", n)
		}
	})

	t.Run("core already has the file", func(t *testing.T) {
		f := newRestoreCacheServer(t, map[string][][]byte{key: {large}})
		s, exchangeDir, clock := startRestoreSession(t, f, io.Discard, cache.DigestOf(large))
		prefetchKeys(t, s, []string{key}, nil)
		f.expire(http.StatusForbidden)
		clock.advance(time.Hour)

		if got := restoreKey(t, s, key, false).Status; got != cache.RestoreHit {
			t.Fatalf("restore status = %q, want hit", got)
		}
		f.wantDownloads(t, 1)
		if n := exchangeFiles(t, exchangeDir); n != 0 {
			t.Fatalf("exchange directory holds %d files, want none", n)
		}
	})

	t.Run("an earlier restore placed the file", func(t *testing.T) {
		f := newRestoreCacheServer(t, map[string][][]byte{key: {large}})
		s, exchangeDir, clock := startRestoreSession(t, f, io.Discard)
		prefetchKeys(t, s, []string{key}, nil)
		path, _ := cache.BlobExchangePath(exchangeDir, cache.DigestOf(large))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, large, 0o644); err != nil {
			t.Fatal(err)
		}
		f.expire(http.StatusForbidden)
		clock.advance(time.Hour)

		if got := restoreKey(t, s, key, false).Status; got != cache.RestoreHit {
			t.Fatalf("restore status = %q, want hit", got)
		}
		f.wantDownloads(t, 1)
	})

	t.Run("core asks for the result only", func(t *testing.T) {
		f := newRestoreCacheServer(t, map[string][][]byte{key: {large}})
		s, exchangeDir, clock := startRestoreSession(t, f, io.Discard)
		prefetchKeys(t, s, []string{key}, nil)
		f.expire(http.StatusForbidden)
		clock.advance(time.Hour)

		if got := restoreKey(t, s, key, true).Status; got != cache.RestoreHit {
			t.Fatalf("restore status = %q, want hit", got)
		}
		f.wantDownloads(t, 1)
		if n := exchangeFiles(t, exchangeDir); n != 0 {
			t.Fatalf("exchange directory holds %d files, want none", n)
		}
	})

	t.Run("the key was a miss", func(t *testing.T) {
		f := newRestoreCacheServer(t, map[string][][]byte{})
		s, _, clock := startRestoreSession(t, f, io.Discard)
		prefetchKeys(t, s, []string{key}, nil)
		clock.advance(time.Hour)

		if got := restoreKey(t, s, key, false).Status; got != cache.RestoreMiss {
			t.Fatalf("restore status = %q, want miss", got)
		}
		f.wantDownloads(t, 1)
	})
}

// A download the storage refuses the way it refuses an expired one makes the
// restore negotiate its key again, once, and download with the new answer.
// Before, the refusal was the restore's answer: an error, and the task ran.
func TestARefusedDownloadIsNegotiatedAgainOnce(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone,
	} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			key, blob := resultOnlyKey("a"), largeBlob("a")
			f := newRestoreCacheServer(t, map[string][][]byte{key: {blob}})
			s, exchangeDir, _ := startRestoreSession(t, f, io.Discard)
			prefetchKeys(t, s, []string{key}, nil)
			// The clock does not move: the storage alone says the download expired.
			f.expire(status)

			if got := restoreKey(t, s, key, false).Status; got != cache.RestoreHit {
				t.Fatalf("restore status = %q, want hit", got)
			}
			wantExchangeBlob(t, exchangeDir, blob)
			f.wantDownloads(t, 2,
				blobDownload{generation: 1, status: status},
				blobDownload{generation: 2, status: http.StatusOK})
			if stats := mustSummary(t, s); stats.RestoredCount != 1 || stats.RestoredBytes != int64(len(blob)) {
				t.Errorf("restored = %d entries, %d bytes; want 1 entry of %d bytes", stats.RestoredCount, stats.RestoredBytes, len(blob))
			}
		})
	}
}

// A small file takes its own download when the server does not answer
// download-batch. The age check leaves small files out, so the refusal is what
// makes the restore negotiate again.
func TestAnOldSmallFileWithoutDownloadBatchIsNegotiatedAgainOnTheRefusal(t *testing.T) {
	key, blob := resultOnlyKey("a"), []byte("a small output")
	f := newRestoreCacheServer(t, map[string][][]byte{key: {blob}})
	s, exchangeDir, clock := startRestoreSession(t, f, io.Discard)
	prefetchKeys(t, s, []string{key}, nil)
	f.expire(http.StatusBadRequest)
	clock.advance(time.Hour)

	if got := restoreKey(t, s, key, false).Status; got != cache.RestoreHit {
		t.Fatalf("restore status = %q, want hit", got)
	}
	wantExchangeBlob(t, exchangeDir, blob)
	f.wantDownloads(t, 2,
		blobDownload{generation: 1, status: http.StatusBadRequest},
		blobDownload{generation: 2, status: http.StatusOK})
}

// A download refused again after the second negotiate is an error: the restore
// negotiates once more, never in a loop.
func TestADownloadRefusedTwiceIsAnError(t *testing.T) {
	key := resultOnlyKey("a")
	f := newRestoreCacheServer(t, map[string][][]byte{key: {largeBlob("a")}})
	f.refuseAll = http.StatusForbidden
	var logw lockedWriter
	s, exchangeDir, _ := startRestoreSession(t, f, &logw)
	prefetchKeys(t, s, []string{key}, nil)

	if got := restoreKey(t, s, key, false).Status; got != cache.RestoreError {
		t.Fatalf("restore status = %q, want error", got)
	}
	f.wantDownloads(t, 2,
		blobDownload{generation: 1, status: http.StatusForbidden},
		blobDownload{generation: 2, status: http.StatusForbidden})
	if n := exchangeFiles(t, exchangeDir); n != 0 {
		t.Fatalf("exchange directory holds %d files after a refused download, want none", n)
	}
	if log := logw.String(); !strings.Contains(log, "restore "+short(key)+": materialize:") {
		t.Fatalf("the failed restore must be said, got %q", log)
	}
	if stats := mustSummary(t, s); stats.RestoredCount != 0 {
		t.Errorf("restored = %d entries, want none", stats.RestoredCount)
	}
}

// An old restore negotiates before it downloads; its downloads are then as
// new as they can be, and a refusal is final. The restore costs two negotiates
// at most, whatever made it ask again.
func TestAnOldRestoreRefusedAfterItsNegotiateIsAnError(t *testing.T) {
	key := resultOnlyKey("a")
	f := newRestoreCacheServer(t, map[string][][]byte{key: {largeBlob("a")}})
	f.refuseAll = http.StatusForbidden
	s, _, clock := startRestoreSession(t, f, io.Discard)
	prefetchKeys(t, s, []string{key}, nil)
	clock.advance(time.Hour)

	if got := restoreKey(t, s, key, false).Status; got != cache.RestoreError {
		t.Fatalf("restore status = %q, want error", got)
	}
	f.wantDownloads(t, 2, blobDownload{generation: 2, status: http.StatusForbidden})
}

// An entry the server no longer holds at the second negotiate is a miss, not
// an error: the task runs, as for any miss.
func TestAnEntryGoneAtTheSecondNegotiateIsAMiss(t *testing.T) {
	for _, test := range []struct {
		name      string
		age       time.Duration
		downloads []blobDownload
	}{
		{name: "asked again for its age", age: time.Hour},
		{name: "asked again for a refusal", downloads: []blobDownload{{generation: 1, status: http.StatusNotFound}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			key := resultOnlyKey("a")
			f := newRestoreCacheServer(t, map[string][][]byte{key: {largeBlob("a")}})
			s, exchangeDir, clock := startRestoreSession(t, f, io.Discard)
			prefetchKeys(t, s, []string{key}, nil)
			f.drop(key)
			f.expire(http.StatusNotFound)
			clock.advance(test.age)

			if got := restoreKey(t, s, key, false).Status; got != cache.RestoreMiss {
				t.Fatalf("restore status = %q, want miss", got)
			}
			f.wantDownloads(t, 2, test.downloads...)
			if n := exchangeFiles(t, exchangeDir); n != 0 {
				t.Fatalf("exchange directory holds %d files, want none", n)
			}
			if stats := mustSummary(t, s); stats.RestoredCount != 0 {
				t.Errorf("restored = %d entries, want none", stats.RestoredCount)
			}
		})
	}
}

// A failure that says nothing about the download's age is not negotiated
// again: a 500 is the storage failing, and a new download would fail the same.
func TestAFailedDownloadThatIsNoRefusalIsNotNegotiatedAgain(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusTooManyRequests} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			key := resultOnlyKey("a")
			f := newRestoreCacheServer(t, map[string][][]byte{key: {largeBlob("a")}})
			f.refuseAll = status
			s, _, _ := startRestoreSession(t, f, io.Discard)
			prefetchKeys(t, s, []string{key}, nil)

			if got := restoreKey(t, s, key, false).Status; got != cache.RestoreError {
				t.Fatalf("restore status = %q, want error", got)
			}
			f.wantDownloads(t, 1, blobDownload{generation: 1, status: status})
		})
	}
}

// A restore the prefetch did not cover negotiates its own key, and that
// negotiate is the only one: its downloads are new, so a refusal is final.
func TestARestoreOutsideThePrefetchNegotiatesOnce(t *testing.T) {
	key, blob := resultOnlyKey("a"), largeBlob("a")

	t.Run("hit", func(t *testing.T) {
		f := newRestoreCacheServer(t, map[string][][]byte{key: {blob}})
		s, exchangeDir, clock := startRestoreSession(t, f, io.Discard)
		clock.advance(time.Hour)

		if got := restoreKey(t, s, key, false).Status; got != cache.RestoreHit {
			t.Fatalf("restore status = %q, want hit", got)
		}
		wantExchangeBlob(t, exchangeDir, blob)
		f.wantDownloads(t, 1, blobDownload{generation: 1, status: http.StatusOK})
	})

	t.Run("refused", func(t *testing.T) {
		f := newRestoreCacheServer(t, map[string][][]byte{key: {blob}})
		f.refuseAll = http.StatusForbidden
		s, _, _ := startRestoreSession(t, f, io.Discard)

		if got := restoreKey(t, s, key, false).Status; got != cache.RestoreError {
			t.Fatalf("restore status = %q, want error", got)
		}
		f.wantDownloads(t, 1, blobDownload{generation: 1, status: http.StatusForbidden})
	})
}

// When the negotiate an old restore sends fails, the restore still tries the
// downloads it has: they may last longer than the bound assumes. A storage
// token lasts 20 to 60 minutes.
func TestAFailedNegotiateOfAnOldRestoreKeepsTheDownloadsItHas(t *testing.T) {
	key, blob := resultOnlyKey("a"), largeBlob("a")
	f := newRestoreCacheServer(t, map[string][][]byte{key: {blob}})
	var logw lockedWriter
	s, exchangeDir, clock := startRestoreSession(t, f, &logw)
	prefetchKeys(t, s, []string{key}, nil)
	clock.advance(restoreTransferMaxAge)
	f.failNextNegotiate()

	if got := restoreKey(t, s, key, false).Status; got != cache.RestoreHit {
		t.Fatalf("restore status = %q, want hit", got)
	}
	wantExchangeBlob(t, exchangeDir, blob)
	f.wantDownloads(t, 2, blobDownload{generation: 1, status: http.StatusOK})
	if log := logw.String(); !strings.Contains(log, "restore "+short(key)+" negotiate again") {
		t.Fatalf("the failed negotiate must be said, got %q", log)
	}
}

// When the negotiate that follows a refused download fails, the restore is an
// error: it has no download left to try.
func TestAFailedNegotiateAfterARefusedDownloadIsAnError(t *testing.T) {
	key := resultOnlyKey("a")
	f := newRestoreCacheServer(t, map[string][][]byte{key: {largeBlob("a")}})
	var logw lockedWriter
	s, _, _ := startRestoreSession(t, f, &logw)
	prefetchKeys(t, s, []string{key}, nil)
	f.expire(http.StatusForbidden)
	f.failNextNegotiate()

	if got := restoreKey(t, s, key, false).Status; got != cache.RestoreError {
		t.Fatalf("restore status = %q, want error", got)
	}
	f.wantDownloads(t, 2, blobDownload{generation: 1, status: http.StatusForbidden})
	if log := logw.String(); !strings.Contains(log, "restore "+short(key)+" negotiate again") {
		t.Fatalf("the failed negotiate must be said, got %q", log)
	}
}

// A download that expires in the middle of an entry costs the files not yet
// placed, and no more: the second pass downloads only those, and the restore
// counts the bytes of both passes.
func TestARestoreKeepsTheFilesPlacedBeforeTheRefusal(t *testing.T) {
	key := resultOnlyKey("a")
	first, second := largeBlob("a"), largeBlob("b")
	f := newRestoreCacheServer(t, map[string][][]byte{key: {first, second}})
	f.expireAfter = 1
	s, exchangeDir, _ := startRestoreSession(t, f, io.Discard)
	prefetchKeys(t, s, []string{key}, nil)

	if got := restoreKey(t, s, key, false).Status; got != cache.RestoreHit {
		t.Fatalf("restore status = %q, want hit", got)
	}
	wantExchangeBlob(t, exchangeDir, first)
	wantExchangeBlob(t, exchangeDir, second)
	f.wantDownloads(t, 2,
		blobDownload{generation: 1, status: http.StatusOK},
		blobDownload{generation: 1, status: http.StatusForbidden},
		blobDownload{generation: 2, status: http.StatusOK})
	_, downloads := f.seen()
	if downloads[0].digest == downloads[1].digest || downloads[1].digest != downloads[2].digest {
		t.Fatalf("blob GETs = %+v, want the placed file left alone and the refused one asked again", downloads)
	}
	if stats, want := mustSummary(t, s), int64(len(first)+len(second)); stats.RestoredCount != 1 || stats.RestoredBytes != want {
		t.Errorf("restored = %d entries, %d bytes; want 1 entry of %d bytes", stats.RestoredCount, stats.RestoredBytes, want)
	}
}
