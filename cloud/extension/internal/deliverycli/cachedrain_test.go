package deliverycli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/cloud/extension/internal/deliverycli/internal/remotecache"
	cache "go.putnami.dev/protocol/cache"
)

// pinUploadDrain fixes the run-end drain bounds for the sessions a test
// creates after the call, and restores them afterwards.
func pinUploadDrain(t *testing.T, stall, budget, shutdown time.Duration) {
	t.Helper()
	prevStall, prevBudget, prevShutdown := uploadDrainStall, uploadDrainBudget, uploadDrainShutdown
	uploadDrainStall, uploadDrainBudget, uploadDrainShutdown = stall, budget, shutdown
	t.Cleanup(func() {
		uploadDrainStall, uploadDrainBudget, uploadDrainShutdown = prevStall, prevBudget, prevShutdown
	})
}

// holdBlob makes the server read the PUT of a staged blob and never answer it,
// until the test ends.
func (f *uploadCacheServer) holdBlob(t *testing.T, file cache.FileEntry) {
	t.Helper()
	release := make(chan struct{})
	f.mu.Lock()
	f.blobHold[file.Digest] = release
	f.mu.Unlock()
	t.Cleanup(func() { close(release) })
}

// within runs fn and fails the test when it has not returned after limit. It
// returns how long fn took.
func within(t *testing.T, limit time.Duration, what string, fn func()) time.Duration {
	t.Helper()
	start := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("%s did not return within %s", what, limit)
	}
	return time.Since(start)
}

// uploadLine is the upload request core sends for one entry.
func uploadLine(t *testing.T, id int, key string, files ...cache.FileEntry) string {
	t.Helper()
	manifest, err := json.Marshal(cache.Manifest{Files: files})
	if err != nil {
		t.Fatal(err)
	}
	return `{"protocolVersion":1,"id":` + strconv.Itoa(id) + `,"op":"upload","payload":{"key":"` + key +
		`","result":{"status":"success"},"manifest":` + string(manifest) + `}}`
}

func initializeLineFor(exchangeDir string) string {
	return `{"protocolVersion":1,"id":1,"op":"initialize","payload":{"protocolVersion":1,"blobExchangeDir":` + strconv.Quote(exchangeDir) + `}}`
}

// uploadSessionEnv points the next provider session at f, as a writing run.
func uploadSessionEnv(t *testing.T, f *uploadCacheServer) {
	t.Helper()
	pinUploadRetryWait(t)
	enableCacheWritesForTest(t)
	t.Setenv("PUTNAMI_CACHE_URL", f.srv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", "ci-token")
	t.Setenv(SessionReporterIngestURLEnv, "")
}

// The production bounds leave core the time it needs: the summary answers
// under core's 60 s, and the shutdown under its 5 s.
func TestTheDrainBoundsFitCoresTimeouts(t *testing.T) {
	const coreSummary, coreShutdown = 60 * time.Second, 5 * time.Second
	limits := readDrainLimits()
	if limits.budget >= coreSummary || limits.shutdown >= coreShutdown {
		t.Fatalf("budget %s and shutdown %s must end before core's %s and %s", limits.budget, limits.shutdown, coreSummary, coreShutdown)
	}
	if limits.stall != objectDrainTimeout {
		t.Fatalf("stall bound %s, want the object cache's drain bound %s", limits.stall, objectDrainTimeout)
	}
	if limits.stall >= limits.budget {
		t.Fatalf("stall bound %s must end before the budget %s", limits.stall, limits.budget)
	}
}

// A blob PUT that never gets its answer must not hold the summary: core waits
// 60 s for it and then discards every count. Before the bound, the summary
// waited on the upload with no limit at all.
func TestTheSummaryAnswersWhenAnUploadStalls(t *testing.T) {
	pinUploadIdle(t, time.Hour)
	const stall = 500 * time.Millisecond
	pinUploadDrain(t, stall, 20*time.Second, time.Second)
	f := newUploadCacheServer(t, batchedCapabilities()...)
	var logw lockedWriter
	s, exchangeDir := startUploadSession(t, f, &logw)
	stalled := stageBlob(t, exchangeDir, "an output the storage never acknowledges")
	f.holdBlob(t, stalled)
	mustUpload(t, s, uploadKey(0), stalled)
	mustUpload(t, s, uploadKey(1), stalled)

	var stats *cache.SummaryResult
	took := within(t, 10*time.Second, "the summary", func() { stats = mustSummary(t, s) })
	if stats.UploadedCount != 0 || stats.UploadedBytes != 0 {
		t.Fatalf("summary counted %d entries / %d bytes of a window that is not stored", stats.UploadedCount, stats.UploadedBytes)
	}
	if took < stall {
		t.Fatalf("the summary answered after %s, before the stall bound: it did not wait for the upload", took)
	}
	log := logw.String()
	for _, want := range []string{"upload drain: no upload moved forward for 500ms", "2 entries are not stored yet", "the summary does not count them"} {
		if !strings.Contains(log, want) {
			t.Errorf("a drain that gives up must say %q, got %q", want, log)
		}
	}

	// The stalled upload is not waited for a second time: the shutdown that
	// follows the summary answers at once.
	again := within(t, 10*time.Second, "the second drain", func() {
		s.drainBackground(time.Now().Add(10*time.Second), "they are dropped")
	})
	if again > stall/2 {
		t.Errorf("a drain that already gave up on a stall waited %s again", again)
	}
}

// slowWindows queues n entries that each fill a window of their own, against a
// server that takes delay to answer each commit-batch.
func slowWindows(t *testing.T, s *providerSession, f *uploadCacheServer, exchangeDir string, n int, delay time.Duration) {
	t.Helper()
	f.mu.Lock()
	f.commitDelay = delay
	f.mu.Unlock()
	// One staged blob under enough paths that two entries do not fit one
	// commit-batch body: each entry is then stored in its own window.
	output := stageBlob(t, exchangeDir, "output")
	files := make([]cache.FileEntry, 3000)
	for i := range files {
		files[i] = cache.FileEntry{Path: fmt.Sprintf("out/%04d", i), Digest: output.Digest, Size: output.Size}
	}
	for i := range n {
		mustUpload(t, s, uploadKey(i), files...)
	}
}

// The stall bound is a bound on silence, not on the upload: a run whose
// windows take longer in all than the bound, and keep moving, is stored whole.
// Each window leaves the bound more than 2s of slack, so a loaded machine that
// pauses the upload between two windows does not read as a stall.
func TestTheSummaryWaitsForUploadsThatStillMove(t *testing.T) {
	pinUploadIdle(t, time.Hour)
	const stall, perWindow, windows = 3 * time.Second, 400 * time.Millisecond, 10
	pinUploadDrain(t, stall, 40*time.Second, time.Second)
	f := newUploadCacheServer(t, batchedCapabilities()...)
	var logw lockedWriter
	s, exchangeDir := startUploadSession(t, f, &logw)
	slowWindows(t, s, f, exchangeDir, windows, perWindow)

	var stats *cache.SummaryResult
	took := within(t, 30*time.Second, "the summary", func() { stats = mustSummary(t, s) })
	if stats.UploadedCount != windows {
		t.Fatalf("summary counted %d entries, want all %d: the drain cut uploads that still moved (log: %s)", stats.UploadedCount, windows, logw.String())
	}
	if took <= stall {
		t.Fatalf("the drain took %s, not longer than the stall bound %s: the test proves nothing", took, stall)
	}
	if c := f.counts(); len(c.windows) != windows {
		t.Fatalf("commit-batch requests = %d, want one per entry", len(c.windows))
	}
	if strings.Contains(logw.String(), "drain:") {
		t.Fatalf("a drain that finished must say nothing, got %q", logw.String())
	}
}

// The summary has a total bound too, under core's: uploads that still move
// when it is reached are left to the shutdown, and the answer counts what is
// stored by then.
func TestTheSummaryStopsAtItsBudgetWhileUploadsStillMove(t *testing.T) {
	pinUploadIdle(t, time.Hour)
	const budget, perWindow, windows = 300 * time.Millisecond, 150 * time.Millisecond, 12
	pinUploadDrain(t, 30*time.Second, budget, 30*time.Second)
	f := newUploadCacheServer(t, batchedCapabilities()...)
	var logw lockedWriter
	s, exchangeDir := startUploadSession(t, f, &logw)
	slowWindows(t, s, f, exchangeDir, windows, perWindow)

	// The windows take 1.8 s in all, six times the budget: a summary that
	// waited for them would count them all.
	var stats *cache.SummaryResult
	within(t, 10*time.Second, "the summary", func() { stats = mustSummary(t, s) })
	if stats.UploadedCount >= windows {
		t.Fatalf("summary counted all %d entries: it waited past its budget", stats.UploadedCount)
	}
	log := logw.String()
	if !strings.Contains(log, "the uploads did not finish in the time the end of a run has") || !strings.Contains(log, "the summary does not count them") {
		t.Fatalf("a drain out of time must say so, got %q", log)
	}

	// The uploads were not canceled: the shutdown's drain stores the rest.
	s.drainBackground(time.Now().Add(30*time.Second), "they are dropped")
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.committed) != windows {
		t.Fatalf("committed entries = %d after the second drain, want all %d", len(f.committed), windows)
	}
}

// A shutdown is acknowledged inside core's 5 s whatever the uploads do. Before
// the bound it waited on a stalled PUT without limit, and core killed the
// process.
func TestShutdownIsAcknowledgedWhenAnUploadStalls(t *testing.T) {
	pinUploadIdle(t, time.Hour)
	pinUploadDrain(t, 10*time.Second, 10*time.Second, 200*time.Millisecond)
	f := newUploadCacheServer(t, batchedCapabilities()...)
	uploadSessionEnv(t, f)
	exchangeDir := t.TempDir()
	stalled := stageBlob(t, exchangeDir, "an output the storage never acknowledges")
	f.holdBlob(t, stalled)
	reqs := []string{
		initializeLineFor(exchangeDir),
		uploadLine(t, 2, uploadKey(0), stalled),
		`{"protocolVersion":1,"id":3,"op":"shutdown"}`,
	}

	var out bytes.Buffer
	var logw lockedWriter
	within(t, 5*time.Second, "the session", func() {
		if err := runCacheProviderSession(strings.NewReader(strings.Join(reqs, "\n")+"\n"), &out, &logw, t.TempDir()); err != nil {
			t.Errorf("session: %v", err)
		}
	})
	resps := decodeResponses(t, &out)
	if len(resps) != len(reqs) || resps[len(resps)-1].ID != 3 || !resps[len(resps)-1].OK {
		t.Fatalf("responses = %d, want %d ending with the shutdown ack (log: %s)", len(resps), len(reqs), logw.String())
	}
	if log := logw.String(); !strings.Contains(log, "1 entries are not stored yet: they are dropped") {
		t.Fatalf("a shutdown that leaves an entry must say so, got %q", log)
	}
}

// Core can close stdin without a summary or a shutdown: its session failed, or
// it went away. The entries it was told were accepted are still stored before
// the provider exits. Before, closing the session canceled them unsent.
func TestAClosedStdinStoresTheQueuedUploads(t *testing.T) {
	pinUploadIdle(t, time.Hour)
	f := newUploadCacheServer(t, batchedCapabilities()...)
	uploadSessionEnv(t, f)
	exchangeDir := t.TempDir()
	reqs := []string{
		initializeLineFor(exchangeDir),
		uploadLine(t, 2, uploadKey(0), stageBlob(t, exchangeDir, "first output")),
		uploadLine(t, 3, uploadKey(1), stageBlob(t, exchangeDir, "second output")),
	}

	var out bytes.Buffer
	var logw lockedWriter
	within(t, 10*time.Second, "the session", func() {
		if err := runCacheProviderSession(strings.NewReader(strings.Join(reqs, "\n")+"\n"), &out, &logw, t.TempDir()); err != nil {
			t.Errorf("session: %v", err)
		}
	})
	if c := f.counts(); len(c.windows) != 1 || !slices.Equal(c.windows[0], []string{uploadKey(0), uploadKey(1)}) {
		t.Fatalf("windows = %v, want the two queued entries stored before the exit (log: %s)", c.windows, logw.String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.committed[uploadKey(0)] || !f.committed[uploadKey(1)] || len(f.blobs) != 2 {
		t.Fatalf("committed = %v with %d blobs, want both entries and their 2 blobs", f.committed, len(f.blobs))
	}
}

// failingWriter fails every write after the first ok ones.
type failingWriter struct {
	mu sync.Mutex
	ok int
}

func (w *failingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ok == 0 {
		return 0, errors.New("the response pipe is closed")
	}
	w.ok--
	return len(p), nil
}

// The other way a session ends without a shutdown is a response it cannot
// write. The entry the failed answer was about is already queued, and it is
// stored too.
func TestABrokenResponsePipeStoresTheQueuedUploads(t *testing.T) {
	pinUploadIdle(t, time.Hour)
	f := newUploadCacheServer(t, batchedCapabilities()...)
	uploadSessionEnv(t, f)
	exchangeDir := t.TempDir()
	reqs := []string{
		initializeLineFor(exchangeDir),
		uploadLine(t, 2, uploadKey(0), stageBlob(t, exchangeDir, "output")),
		`{"protocolVersion":1,"id":3,"op":"summary","payload":{}}`,
	}

	var logw lockedWriter
	var err error
	within(t, 10*time.Second, "the session", func() {
		err = runCacheProviderSession(strings.NewReader(strings.Join(reqs, "\n")+"\n"), &failingWriter{ok: 1}, &logw, t.TempDir())
	})
	if err == nil {
		t.Fatal("a response that cannot be written must end the session with its error")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.committed[uploadKey(0)] {
		t.Fatalf("the queued entry was not stored before the exit (log: %s)", logw.String())
	}
}

// The exit is bounded like the other drains: a stalled upload does not keep
// the process alive after core is gone.
func TestAClosedStdinWithAStalledUploadStillExits(t *testing.T) {
	pinUploadIdle(t, time.Hour)
	pinUploadDrain(t, 150*time.Millisecond, 10*time.Second, 10*time.Second)
	f := newUploadCacheServer(t, batchedCapabilities()...)
	uploadSessionEnv(t, f)
	exchangeDir := t.TempDir()
	stalled := stageBlob(t, exchangeDir, "an output the storage never acknowledges")
	f.holdBlob(t, stalled)
	reqs := []string{initializeLineFor(exchangeDir), uploadLine(t, 2, uploadKey(0), stalled)}

	var out bytes.Buffer
	var logw lockedWriter
	within(t, 5*time.Second, "the session", func() {
		if err := runCacheProviderSession(strings.NewReader(strings.Join(reqs, "\n")+"\n"), &out, &logw, t.TempDir()); err != nil {
			t.Errorf("session: %v", err)
		}
	})
	if log := logw.String(); !strings.Contains(log, "1 entries are not stored yet: they are dropped") {
		t.Fatalf("an exit that leaves an entry must say so, got %q", log)
	}
}

// The exit waits for the uploader and nothing else: nobody reads the answer of
// a prefetch or a restore once stdin is closed, so they are stopped at once.
func TestAClosedStdinDoesNotWaitForAPrefetch(t *testing.T) {
	pinUploadDrain(t, 30*time.Second, 30*time.Second, 30*time.Second)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path != cache.NegotiatePath {
			http.NotFound(w, r)
			return
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	t.Setenv("PUTNAMI_CACHE_URL", srv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", "ci-token")
	reqs := []string{
		initializeLineFor(t.TempDir()),
		`{"protocolVersion":1,"id":2,"op":"prefetch","payload":{"keys":["` + uploadKey(0) + `"]}}`,
	}

	var out bytes.Buffer
	var logw lockedWriter
	took := within(t, 10*time.Second, "the session", func() {
		if err := runCacheProviderSession(strings.NewReader(strings.Join(reqs, "\n")+"\n"), &out, &logw, t.TempDir()); err != nil {
			t.Errorf("session: %v", err)
		}
	})
	if took > 5*time.Second {
		t.Fatalf("the exit took %s: it waited for a prefetch nobody reads", took)
	}
}

// background is the wait group of the session's background work whose wait
// can be given up.
func TestBackground(t *testing.T) {
	var bg background
	select {
	case <-bg.idle():
	default:
		t.Fatal("a background with no work must be idle")
	}
	bg.Wait() // returns at once

	bg.Add(2)
	busy := bg.idle()
	select {
	case <-busy:
		t.Fatal("a background with work must not be idle")
	default:
	}
	bg.Done()
	select {
	case <-busy:
		t.Fatal("one piece of work is still running")
	default:
	}
	bg.Done()
	select {
	case <-busy:
	case <-time.After(time.Second):
		t.Fatal("the idle channel did not close when the last work ended")
	}

	// Work that starts again gets a new channel: a waiter of the first round
	// is not held by the second.
	bg.Add(1)
	select {
	case <-bg.idle():
		t.Fatal("work started again: the background must not be idle")
	default:
	}
	waited := make(chan struct{})
	go func() {
		bg.Wait()
		close(waited)
	}()
	bg.Done()
	select {
	case <-waited:
	case <-time.After(time.Second):
		t.Fatal("Wait did not return when the work ended")
	}
}

// The uploader reports its progress through the upload context, which is what
// keeps a moving upload from being read as a stall.
func TestTheUploaderRecordsItsProgress(t *testing.T) {
	f := newUploadCacheServer(t, batchedCapabilities()...)
	s, exchangeDir := startUploadSession(t, f, io.Discard)
	if s.uploads.progress.Load() != 0 {
		t.Fatal("a session that uploaded nothing has no progress")
	}
	before := time.Now().UnixNano()
	mustUpload(t, s, uploadKey(0), stageBlob(t, exchangeDir, "output"))
	if stats := mustSummary(t, s); stats.UploadedCount != 1 {
		t.Fatalf("summary counted %d entries, want 1", stats.UploadedCount)
	}
	if got := s.uploads.progress.Load(); got < before {
		t.Fatalf("progress = %d, want a time after the upload started (%d)", got, before)
	}
	s.uploads.mu.Lock()
	defer s.uploads.mu.Unlock()
	if s.uploads.inflight != 0 || s.uploads.running {
		t.Fatalf("after the drain: inflight=%d running=%v, want an idle uploader", s.uploads.inflight, s.uploads.running)
	}
}

// The retry policy of a window, as production reads it.
func TestTheUploadRetryPolicy(t *testing.T) {
	want := remotecache.WriteRetry{Attempts: 3, Backoff: 500 * time.Millisecond, SplitWindow: true}
	if uploadRetry != want {
		t.Fatalf("uploadRetry = %+v, want %+v", uploadRetry, want)
	}
	f := newUploadCacheServer(t, batchedCapabilities()...)
	enableCacheWritesForTest(t)
	t.Setenv("PUTNAMI_CACHE_URL", f.srv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", "ci-token")
	s := newProviderSession(t.TempDir(), io.Discard)
	t.Cleanup(s.close)
	mustInit(t, s, &cache.InitializeParams{ProtocolVersion: cache.ProviderProtocolMinVersion, BlobExchangeDir: t.TempDir()})
	if wp, batched := s.taskWritePath(); !batched || wp.Retry != want {
		t.Fatalf("write path = %+v (batched=%v), want Retry=%+v", wp, batched, want)
	}
}
