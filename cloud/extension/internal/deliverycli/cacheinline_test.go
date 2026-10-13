package deliverycli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	cache "go.putnami.dev/protocol/cache"
)

// The prefetch tests: the prefetch fetches the small files of every hit in a
// few download-batch requests, and each restore finds them in the exchange
// directory. See cacheinline.go.

// inlineCacheServer is a fake cache server for the prefetch tests. It answers
// negotiate with each entry's files, download-batch with the blobs it holds,
// and a blob GET with the blob. It records the digests of each download-batch
// and the digest of each blob GET.
type inlineCacheServer struct {
	srv *httptest.Server
	// entries maps a cache key to the blobs of its entry, and blobs a digest to
	// its content.
	entries map[string][][]byte
	blobs   map[string][]byte

	mu sync.Mutex
	// inlineStatus answers every download-batch with this status when it is
	// not 0.
	inlineStatus int
	// omit leaves these digests out of a download-batch answer.
	omit map[string]bool
	// corrupt answers the next download-batch with wrong bytes for this digest.
	corrupt string
	// arrived, when set, receives one value per download-batch, and release
	// holds each answer until it is closed.
	arrived chan struct{}
	release chan struct{}

	inlineAsks  [][]string
	inlineBytes []int
	gets        []string
	// negotiates records each negotiate, its mode and its keys.
	negotiates []negotiateCall
}

func newInlineCacheServer(t *testing.T, entries map[string][][]byte) *inlineCacheServer {
	t.Helper()
	f := &inlineCacheServer{entries: entries, blobs: map[string][]byte{}, omit: map[string]bool{}}
	for _, blobs := range entries {
		for _, blob := range blobs {
			f.blobs[cache.DigestOf(blob)] = blob
		}
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

// hold makes every download-batch wait until the test calls the returned
// function. The test's cleanup calls it too, so the server can close.
func (f *inlineCacheServer) hold(t *testing.T) (arrived <-chan struct{}, release func()) {
	t.Helper()
	f.arrived = make(chan struct{}, 64)
	f.release = make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(f.release) }) }
	t.Cleanup(release)
	return f.arrived, release
}

func (f *inlineCacheServer) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == cache.NegotiatePath:
		f.negotiate(w, r)
	case r.URL.Path == cache.DownloadBatchPath:
		f.downloadBatch(w, r)
	case strings.HasPrefix(r.URL.Path, "/blob/"):
		digest := strings.TrimPrefix(r.URL.Path, "/blob/")
		f.mu.Lock()
		f.gets = append(f.gets, digest)
		blob, held := f.blobs[digest]
		f.mu.Unlock()
		if !held {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(blob)
	default:
		http.NotFound(w, r)
	}
}

func (f *inlineCacheServer) negotiate(w http.ResponseWriter, r *http.Request) {
	var nreq cache.NegotiateRequest
	_ = json.NewDecoder(r.Body).Decode(&nreq)
	call := negotiateCall{mode: nreq.Mode}
	for _, k := range nreq.Keys {
		call.keys = append(call.keys, k.Key)
	}
	f.mu.Lock()
	f.negotiates = append(f.negotiates, call)
	f.mu.Unlock()
	resp := cache.NegotiateResponse{ProtocolVersion: cache.ProtocolVersion}
	for _, k := range nreq.Keys {
		blobs, hit := f.entries[k.Key]
		if !hit {
			resp.Results = append(resp.Results, cache.KeyResult{Key: k.Key})
			continue
		}
		result := cache.KeyResult{Key: k.Key, Hit: true, Result: &cache.ActionResult{Status: "success"}, Manifest: &cache.Manifest{}}
		for i, blob := range blobs {
			digest := cache.DigestOf(blob)
			result.Result.SizeBytes += int64(len(blob))
			result.Manifest.Files = append(result.Manifest.Files, cache.FileEntry{Path: fmt.Sprintf("out/%d.js", i), Digest: digest, Mode: 0o644, Size: int64(len(blob))})
			if nreq.Mode != cache.ModeMinimal {
				result.Downloads = append(result.Downloads, cache.BlobTransfer{
					Digest: digest, URL: f.srv.URL + "/blob/" + digest, Method: cache.TransferGet, SizeBytes: int64(len(blob)),
				})
			}
		}
		resp.Results = append(resp.Results, result)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (f *inlineCacheServer) downloadBatch(w http.ResponseWriter, r *http.Request) {
	var req cache.DownloadBatchRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	arrived, release := f.arrived, f.release
	f.mu.Unlock()
	if arrived != nil {
		arrived <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.inlineAsks = append(f.inlineAsks, req.Digests)
	if f.inlineStatus != 0 {
		f.inlineBytes = append(f.inlineBytes, 0)
		http.Error(w, "download-batch failed", f.inlineStatus)
		return
	}
	resp := cache.DownloadBatchResponse{ProtocolVersion: cache.ProtocolVersion}
	served := 0
	for _, digest := range req.Digests {
		blob, held := f.blobs[digest]
		if !held || f.omit[digest] {
			continue
		}
		if digest == f.corrupt {
			blob = bytes.Repeat([]byte{'x'}, len(blob)) // the declared size, other content
			f.corrupt = ""
		}
		served += len(blob)
		resp.Blobs = append(resp.Blobs, cache.InlineBlob{Digest: digest, Data: blob})
	}
	f.inlineBytes = append(f.inlineBytes, served)
	writeJSON(w, http.StatusOK, resp)
}

// seen returns the digests of each download-batch and of each blob GET.
func (f *inlineCacheServer) seen() (asks [][]string, gets []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.inlineAsks), slices.Clone(f.gets)
}

// negotiateCalls returns the negotiates the server received, in order.
func (f *inlineCacheServer) negotiateCalls() []negotiateCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.negotiates)
}

// answered returns the bytes of files each download-batch answer carried.
func (f *inlineCacheServer) answered() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.inlineBytes)
}

// startInlineSession opens a serving session in full mode. known lists the
// digests core says it already has.
func startInlineSession(t *testing.T, f *inlineCacheServer, logw io.Writer, known ...string) (*providerSession, string) {
	t.Helper()
	t.Setenv("PUTNAMI_CACHE_URL", f.srv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", "test-token")
	exchangeDir := t.TempDir()
	s := newProviderSession(t.TempDir(), logw)
	t.Cleanup(s.close)
	init := mustInit(t, s, &cache.InitializeParams{
		ProtocolVersion: cache.ProviderProtocolMinVersion,
		BlobExchangeDir: exchangeDir,
		Mode:            cache.ModeFull,
		KnownDigests:    known,
	})
	if !init.Ready {
		t.Fatal("expected ready with PUTNAMI_CACHE_URL set")
	}
	return s, exchangeDir
}

// inlineKey returns a distinct, valid cache key per index.
func inlineKey(i int) string {
	return fmt.Sprintf("%064x", i+1)
}

// smallBlob returns a distinct blob of size bytes, small enough for
// download-batch.
func smallBlob(name string, size int) []byte {
	b := bytes.Repeat([]byte{'.'}, size)
	copy(b, name)
	return b
}

// digestsOf returns the sorted digests of blobs, as a download-batch request
// lists them.
func digestsOf(blobs ...[]byte) []string {
	out := make([]string, 0, len(blobs))
	for _, b := range blobs {
		out = append(out, cache.DigestOf(b))
	}
	slices.Sort(out)
	return out
}

// A run whose hits have only small files sends one download-batch per
// maxDownloadBatchBytes of files, not one per restored entry. Before, each
// restore asked download-batch for its own entry's files: 64 hits cost 64
// requests. Each request stays within both bounds, and the restores send
// nothing more.
func TestPrefetchFetchesTheSmallFilesOfEveryHitInFewRequests(t *testing.T) {
	const (
		hits         = 64
		filesPerHit  = 3
		fileSize     = 64 << 10 // 64 of them fill one request
		filesPerAsk  = (4 << 20) / fileSize
		wantRequests = (hits*filesPerHit + filesPerAsk - 1) / filesPerAsk
	)
	entries := make(map[string][][]byte, hits)
	keys := make([]string, 0, hits)
	for h := range hits {
		key := inlineKey(h)
		keys = append(keys, key)
		for i := range filesPerHit {
			entries[key] = append(entries[key], smallBlob(fmt.Sprintf("hit %d file %d", h, i), fileSize))
		}
	}
	f := newInlineCacheServer(t, entries)
	s, exchangeDir := startInlineSession(t, f, io.Discard)

	prefetchKeys(t, s, keys, nil)
	asks, gets := f.seen()
	if len(asks) != wantRequests || wantRequests >= hits {
		t.Fatalf("download-batch requests = %d, want %d for %d files of %d hits", len(asks), wantRequests, hits*filesPerHit, hits)
	}
	asked := map[string]bool{}
	answered := f.answered()
	for i, ask := range asks {
		if len(ask) > cache.MaxKeysPerRequest {
			t.Errorf("request %d asked for %d files, more than %d", i, len(ask), cache.MaxKeysPerRequest)
		}
		if answered[i] > 4<<20 {
			t.Errorf("request %d was answered with %d bytes of files, more than 4 MiB", i, answered[i])
		}
		for _, d := range ask {
			if asked[d] {
				t.Errorf("file %s was asked for twice", short(d))
			}
			asked[d] = true
		}
	}
	if len(asked) != hits*filesPerHit || len(gets) != 0 {
		t.Fatalf("asked for %d files and sent %d blob GETs, want %d and none", len(asked), len(gets), hits*filesPerHit)
	}

	for _, key := range keys {
		if got := restoreKey(t, s, key, false).Status; got != cache.RestoreHit {
			t.Fatalf("restore %s = %q, want hit", short(key), got)
		}
		for _, blob := range entries[key] {
			wantExchangeBlob(t, exchangeDir, blob)
		}
	}
	if asks, gets := f.seen(); len(asks) != wantRequests || len(gets) != 0 {
		t.Fatalf("after the restores: %d download-batch requests and %d blob GETs, want %d and none", len(asks), len(gets), wantRequests)
	}
	if stats := mustSummary(t, s); stats.RestoredCount != hits || stats.RestoredBytes != hits*filesPerHit*fileSize {
		t.Errorf("restored = %d entries, %d bytes; want %d entries of %d bytes", stats.RestoredCount, stats.RestoredBytes, hits, hits*filesPerHit*fileSize)
	}
}

// The prefetch asks once for each small file core lacks and will read: not for
// a file core has, one already in the exchange directory, a file over
// cache.MaxInlineBlobBytes, or a file of a result-only key. A file two entries
// share is asked for once and counted once.
func TestThePrefetchAsksOnlyForSmallFilesCoreLacksAndReads(t *testing.T) {
	small1, shared, small2 := smallBlob("small 1", 100), smallBlob("shared", 200), smallBlob("small 2", 300)
	known, placed, onlyFile := smallBlob("core has it", 400), smallBlob("already placed", 500), smallBlob("result only", 600)
	large := largeBlob("L")
	a, b, c, d := inlineKey(0), inlineKey(1), inlineKey(2), inlineKey(3)
	f := newInlineCacheServer(t, map[string][][]byte{
		a: {small1, shared, large},
		b: {shared, small2, placed},
		c: {known},
		d: {onlyFile},
	})
	s, exchangeDir := startInlineSession(t, f, io.Discard, cache.DigestOf(known))
	path, _ := cache.BlobExchangePath(exchangeDir, cache.DigestOf(placed))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, placed, 0o644); err != nil {
		t.Fatal(err)
	}

	prefetchKeys(t, s, []string{a, b, c, d}, []string{d})
	if asks, gets := f.seen(); !slices.EqualFunc(asks, [][]string{digestsOf(small1, shared, small2)}, slices.Equal[[]string]) || len(gets) != 0 {
		t.Fatalf("download-batch digests = %v, blob GETs = %v; want one request for the 3 small files core lacks and reads", asks, gets)
	}

	for _, key := range []string{a, b, c} {
		if got := restoreKey(t, s, key, false).Status; got != cache.RestoreHit {
			t.Fatalf("restore %s = %q, want hit", short(key), got)
		}
	}
	if got := restoreKey(t, s, d, true).Status; got != cache.RestoreHit {
		t.Fatalf("result-only restore = %q, want hit", got)
	}
	for _, blob := range [][]byte{small1, shared, small2, placed, large} {
		wantExchangeBlob(t, exchangeDir, blob)
	}
	if asks, gets := f.seen(); len(asks) != 1 || !slices.Equal(gets, []string{cache.DigestOf(large)}) {
		t.Fatalf("after the restores: %d download-batch requests, blob GETs = %v; want 1 and the large file alone", len(asks), gets)
	}
	want := int64(len(small1) + len(shared) + len(small2) + len(large))
	if stats := mustSummary(t, s); stats.RestoredCount != 4 || stats.RestoredBytes != want {
		t.Errorf("restored = %d entries, %d bytes; want 4 entries of %d bytes, the shared file once", stats.RestoredCount, stats.RestoredBytes, want)
	}
}

// A prefetch download-batch that fails never fails a restore: each restore
// fetches its own files as it did before, and every hit is restored.
func TestAFailedPrefetchDownloadLeavesTheFilesToEachRestore(t *testing.T) {
	blobs := [][]byte{smallBlob("one", 10), smallBlob("two", 20), smallBlob("three", 30)}
	entries := map[string][][]byte{}
	keys := make([]string, 0, len(blobs))
	for i, blob := range blobs {
		keys = append(keys, inlineKey(i))
		entries[inlineKey(i)] = [][]byte{blob}
	}
	f := newInlineCacheServer(t, entries)
	f.inlineStatus = http.StatusInternalServerError
	var logw lockedWriter
	s, exchangeDir := startInlineSession(t, f, &logw)

	prefetchKeys(t, s, keys, nil)
	if log := logw.String(); !strings.Contains(log, "prefetch download-batch of 3 files") {
		t.Fatalf("the failed request must be said, got %q", log)
	}
	for i, key := range keys {
		if got := restoreKey(t, s, key, false).Status; got != cache.RestoreHit {
			t.Fatalf("restore %s = %q, want hit", short(key), got)
		}
		wantExchangeBlob(t, exchangeDir, blobs[i])
	}
	asks, gets := f.seen()
	want := [][]string{digestsOf(blobs...), digestsOf(blobs[0]), digestsOf(blobs[1]), digestsOf(blobs[2])}
	if !slices.EqualFunc(asks, want, slices.Equal[[]string]) {
		t.Fatalf("download-batch digests = %v, want the prefetch's request, then one per restore", asks)
	}
	if len(gets) != len(blobs) {
		t.Fatalf("blob GETs = %d, want %d: download-batch keeps failing", len(gets), len(blobs))
	}
	if stats := mustSummary(t, s); stats.RestoredCount != 3 || stats.RestoredBytes != 60 {
		t.Errorf("restored = %d entries, %d bytes; want 3 entries of 60 bytes", stats.RestoredCount, stats.RestoredBytes)
	}
}

// A file the server leaves out of the prefetch's answer is not inline: the
// restore downloads it with its own GET and does not ask download-batch again.
func TestAFileTheServerLeftOutComesThroughItsOwnDownload(t *testing.T) {
	served, left := smallBlob("served", 10), smallBlob("left out", 20)
	key := inlineKey(0)
	f := newInlineCacheServer(t, map[string][][]byte{key: {served, left}})
	f.omit[cache.DigestOf(left)] = true
	s, exchangeDir := startInlineSession(t, f, io.Discard)

	prefetchKeys(t, s, []string{key}, nil)
	if got := restoreKey(t, s, key, false).Status; got != cache.RestoreHit {
		t.Fatalf("restore = %q, want hit", got)
	}
	wantExchangeBlob(t, exchangeDir, served)
	wantExchangeBlob(t, exchangeDir, left)
	if asks, gets := f.seen(); len(asks) != 1 || !slices.Equal(gets, []string{cache.DigestOf(left)}) {
		t.Fatalf("download-batch requests = %d, blob GETs = %v; want 1 and the left-out file alone", len(asks), gets)
	}
	if stats := mustSummary(t, s); stats.RestoredBytes != 30 {
		t.Errorf("restored bytes = %d, want 30", stats.RestoredBytes)
	}
}

// An answer with a file that does not match its digest places its other files:
// the restore of the bad file downloads it with its own GET, and no restore
// asks download-batch again.
func TestAPrefetchAnswerPlacesEveryFileButTheOneThatDoesNotVerify(t *testing.T) {
	bad, good := smallBlob("bad", 10), smallBlob("good", 20)
	a, b := inlineKey(0), inlineKey(1)
	f := newInlineCacheServer(t, map[string][][]byte{a: {bad}, b: {good}})
	f.corrupt = cache.DigestOf(bad)
	var logw lockedWriter
	s, exchangeDir := startInlineSession(t, f, &logw)

	prefetchKeys(t, s, []string{a, b}, nil)
	if n := exchangeFiles(t, exchangeDir); n != 1 {
		t.Fatalf("the prefetch placed %d files, want the 1 that verifies", n)
	}
	wantExchangeBlob(t, exchangeDir, good)
	if log := logw.String(); !strings.Contains(log, "content-addresses to") {
		t.Fatalf("the rejected file must be said, got %q", log)
	}
	for _, key := range []string{a, b} {
		if got := restoreKey(t, s, key, false).Status; got != cache.RestoreHit {
			t.Fatalf("restore %s = %q, want hit", short(key), got)
		}
	}
	wantExchangeBlob(t, exchangeDir, bad)
	if asks, gets := f.seen(); len(asks) != 1 || !slices.Equal(gets, []string{cache.DigestOf(bad)}) {
		t.Fatalf("download-batch requests = %d, blob GETs = %v; want 1 and the bad file alone", len(asks), gets)
	}
}

// Restores that core sends while the prefetch's download-batch is still on the
// wire wait for it: none of them answers before it, and none asks for the files
// again. They also race the negotiate before it, which they already waited for.
func TestRestoresRacingThePrefetchDownloadWaitForIt(t *testing.T) {
	const hits = 24
	shared := smallBlob("shared by every entry", 50)
	entries := map[string][][]byte{}
	keys := make([]string, 0, hits)
	want := int64(len(shared))
	for h := range hits {
		key := inlineKey(h)
		blob := smallBlob(fmt.Sprintf("entry %d", h), 100+h)
		keys = append(keys, key)
		entries[key] = [][]byte{blob, shared}
		want += int64(len(blob))
	}
	f := newInlineCacheServer(t, entries)
	arrived, release := f.hold(t)
	s, exchangeDir := startInlineSession(t, f, io.Discard)

	resp := s.prefetch(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 1, Op: cache.OpPrefetch},
		&cache.PrefetchParams{Keys: keys})
	if !resp.OK {
		t.Fatalf("prefetch not OK: %+v", resp.Error)
	}
	answers := make(chan *cache.ProviderResponse, hits)
	for i, key := range keys {
		go func() {
			answers <- s.restore(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: int64(10 + i), Op: cache.OpRestore},
				&cache.RestoreParams{Key: key})
		}()
	}

	select {
	case <-arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("the prefetch sent no download-batch")
	}
	select {
	case a := <-answers:
		t.Fatalf("restore %d answered before the download-batch that carries its files: %+v", a.ID, a)
	case <-time.After(100 * time.Millisecond):
	}
	release()

	for range hits {
		var a *cache.ProviderResponse
		select {
		case a = <-answers:
		case <-time.After(30 * time.Second):
			t.Fatal("a restore did not answer after the download-batch ended")
		}
		res, diags := cache.ParseAndValidateRestoreResult(a.Payload)
		if !a.OK || res == nil || res.Status != cache.RestoreHit {
			t.Fatalf("restore %d = %+v (%v), want hit", a.ID, a, diags)
		}
	}
	for _, blobs := range entries {
		wantExchangeBlob(t, exchangeDir, blobs[0])
	}
	wantExchangeBlob(t, exchangeDir, shared)
	if asks, gets := f.seen(); len(asks) != 1 || len(asks[0]) != hits+1 || len(gets) != 0 {
		t.Fatalf("download-batch requests = %d, blob GETs = %d; want 1 request for %d files and no GET", len(asks), len(gets), hits+1)
	}
	if stats := mustSummary(t, s); stats.RestoredCount != hits || stats.RestoredBytes != want {
		t.Errorf("restored = %d entries, %d bytes; want %d entries of %d bytes, the shared file once", stats.RestoredCount, stats.RestoredBytes, hits, want)
	}
}

// A restore waiting for the prefetch's download-batch ends with an error when
// the session closes; it never hangs the shutdown.
func TestARestoreWaitingForThePrefetchEndsWhenTheSessionCloses(t *testing.T) {
	key := inlineKey(0)
	f := newInlineCacheServer(t, map[string][][]byte{key: {smallBlob("held", 10)}})
	arrived, _ := f.hold(t)
	var logw lockedWriter
	s, _ := startInlineSession(t, f, &logw)

	resp := s.prefetch(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 1, Op: cache.OpPrefetch},
		&cache.PrefetchParams{Keys: []string{key}})
	if !resp.OK {
		t.Fatalf("prefetch not OK: %+v", resp.Error)
	}
	select {
	case <-arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("the prefetch sent no download-batch")
	}
	answer := make(chan *cache.ProviderResponse, 1)
	go func() {
		answer <- s.restore(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 2, Op: cache.OpRestore},
			&cache.RestoreParams{Key: key})
	}()
	select {
	case a := <-answer:
		t.Fatalf("the restore answered while its files were still on the wire: %+v", a)
	case <-time.After(50 * time.Millisecond):
	}
	s.close()

	select {
	case a := <-answer:
		res, diags := cache.ParseAndValidateRestoreResult(a.Payload)
		if !a.OK || res == nil || res.Status != cache.RestoreError {
			t.Fatalf("restore = %+v (%v), want error", a, diags)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the restore did not end when the session closed")
	}
}
