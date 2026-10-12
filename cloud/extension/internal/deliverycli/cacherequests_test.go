package deliverycli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	cache "go.putnami.dev/protocol/cache"
)

// The cache-server requests of a whole run. A CI run should send fewer than 20.
// This test drives the provider the way core does, through its protocol, and
// counts what reaches the cache server.

// requestCountServer is a cache server that counts the requests of each
// cache-server route. It answers the reads with an inlineCacheServer and the
// writes with an uploadCacheServer. Blob downloads and uploads go to the
// storage URLs those return, which are not cache-server routes.
type requestCountServer struct {
	srv    *httptest.Server
	reads  *inlineCacheServer
	writes *uploadCacheServer

	mu     sync.Mutex
	counts map[string]int
}

func newRequestCountServer(t *testing.T, entries map[string][][]byte) *requestCountServer {
	t.Helper()
	f := &requestCountServer{
		reads:  newInlineCacheServer(t, entries),
		writes: newUploadCacheServer(t, packedCapabilities()...),
		counts: map[string]int{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.counts[r.URL.Path]++
		f.mu.Unlock()
		switch r.URL.Path {
		case cache.NegotiatePath, cache.DownloadBatchPath:
			f.reads.handle(w, r)
		default:
			f.writes.handle(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// requests returns the requests of each route, and their total.
func (f *requestCountServer) requests() (map[string]int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, n := range f.counts {
		total += n
	}
	return maps.Clone(f.counts), total
}

// providerPipe runs a provider session over its protocol, as core does.
type providerPipe struct {
	t         *testing.T
	in        *io.PipeWriter
	responses chan *cache.ProviderResponse
	ended     chan error
	id        int64
}

func startProviderPipe(t *testing.T) *providerPipe {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	p := &providerPipe{t: t, in: inW, responses: make(chan *cache.ProviderResponse, 1024), ended: make(chan error, 1)}
	root := t.TempDir()
	go func() {
		p.ended <- runCacheProviderSession(inR, outW, io.Discard, root)
		_ = outW.Close()
	}()
	go func() {
		defer close(p.responses)
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 0, 64*1024), maxProviderRequestBytes)
		for sc.Scan() {
			resp, _ := cache.ParseAndValidateProviderResponse(sc.Bytes())
			p.responses <- resp
		}
	}()
	t.Cleanup(func() { _ = inW.Close() })
	return p
}

// providerCall is one request of a batch the test sends at once.
type providerCall struct {
	op      cache.ProviderOp
	payload any
}

// send writes the calls at once, as core sends the restores of its workers,
// and returns their responses in the order of calls.
func (p *providerPipe) send(calls ...providerCall) []*cache.ProviderResponse {
	t := p.t
	t.Helper()
	index := make(map[int64]int, len(calls))
	for i, c := range calls {
		p.id++
		req := cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolMinVersion, ID: p.id, Op: c.op}
		if c.payload != nil {
			payload, err := json.Marshal(c.payload)
			if err != nil {
				t.Fatal(err)
			}
			req.Payload = payload
		}
		index[p.id] = i
		writeJSONLine(t, p.in, req)
	}
	out := make([]*cache.ProviderResponse, len(calls))
	for left := len(calls); left > 0; left-- {
		select {
		case resp, ok := <-p.responses:
			if !ok || resp == nil {
				t.Fatal("the provider closed its output or sent a response that does not conform")
			}
			i, mine := index[resp.ID]
			if !mine {
				t.Fatalf("response to request %d, which this batch did not send", resp.ID)
			}
			if !resp.OK {
				t.Fatalf("%s not OK: %+v", calls[i].op, resp.Error)
			}
			out[i] = resp
		case <-time.After(30 * time.Second):
			t.Fatalf("no answer to %d of %d requests", left, len(calls))
		}
	}
	return out
}

// restoreCalls returns one restore per key.
func restoreCalls(keys []string) []providerCall {
	out := make([]providerCall, len(keys))
	for i, key := range keys {
		out[i] = providerCall{op: cache.OpRestore, payload: cache.RestoreParams{Key: key}}
	}
	return out
}

// A realistic CI run: one prefetch of the keys core knows at the start, the
// restores of those keys, waves of restores of keys computed once their
// upstream tasks finished, then the uploads of the tasks it built, in bursts.
// It sends 16 cache-server requests. Before the late negotiates were shared and
// the windows grew, the same run sent 60: one negotiate and one download-batch
// per late restore, and one window per pause of the build.
//
// The build pauses 50 ms between bursts of uploads, as a build pauses well
// under the production idle delay of a window. The test keeps that delay at
// its production value: staging a burst takes longer than any scaled-down
// delay on a loaded machine, which flushed a second window. Each wave of
// restores comes at once, as when a finished task releases the tasks that
// depend on it. The quiet delay of a late negotiate is long enough that every
// restore of a wave joins it on a loaded machine.
func TestARealisticRunSendsFewCacheServerRequests(t *testing.T) {
	pinLateNegotiate(t, 250*time.Millisecond, time.Second, cache.MaxKeysPerRequest)
	pinUploadRetryWait(t)
	enableCacheWritesForTest(t)

	const prefetchedHits, prefetchedMisses, filesPerHit = 32, 8, 2
	lateWaves := []int{6, 3, 8, 1, 5}
	uploadBursts := []int{70, 20, 40, 20}

	entries := map[string][][]byte{}
	var restoredBytes int64
	hit := func(i int) string {
		key := inlineKey(i)
		for f := range filesPerHit {
			blob := smallBlob(fmt.Sprintf("entry %d file %d", i, f), 1000+i)
			entries[key] = append(entries[key], blob)
			restoredBytes += int64(len(blob))
		}
		return key
	}
	next := 0
	prefetched := make([]string, 0, prefetchedHits+prefetchedMisses)
	for range prefetchedHits {
		prefetched = append(prefetched, hit(next))
		next++
	}
	for range prefetchedMisses {
		prefetched = append(prefetched, inlineKey(next))
		next++
	}
	waves := make([][]string, 0, len(lateWaves))
	late := 0
	for _, n := range lateWaves {
		var wave []string
		for range n {
			wave = append(wave, hit(next))
			next++
		}
		waves = append(waves, wave)
		late += n
	}

	f := newRequestCountServer(t, entries)
	t.Setenv("PUTNAMI_CACHE_URL", f.srv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", "ci-token")
	t.Setenv(SessionReporterIngestURLEnv, "")
	exchangeDir := t.TempDir()
	p := startProviderPipe(t)
	p.send(providerCall{op: cache.OpInitialize, payload: cache.InitializeParams{
		ProtocolVersion: cache.ProviderProtocolMinVersion, BlobExchangeDir: exchangeDir, Mode: cache.ModeFull,
	}})

	p.send(providerCall{op: cache.OpPrefetch, payload: cache.PrefetchParams{Keys: prefetched}})
	restored := 0
	for _, wave := range append([][]string{prefetched}, waves...) {
		for _, resp := range p.send(restoreCalls(wave)...) {
			if res, _ := cache.ParseAndValidateRestoreResult(resp.Payload); res != nil && res.Status == cache.RestoreHit {
				restored++
			}
		}
	}
	if want := prefetchedHits + late; restored != want {
		t.Fatalf("restored %d entries, want %d", restored, want)
	}

	uploaded := 0
	for i, n := range uploadBursts {
		if i > 0 {
			time.Sleep(50 * time.Millisecond)
		}
		calls := make([]providerCall, n)
		for j := range calls {
			file := stageBlob(t, exchangeDir, fmt.Sprintf("built output %d %s", uploaded, strings.Repeat(".", 200)))
			calls[j] = providerCall{op: cache.OpUpload, payload: cache.UploadParams{
				Key: uploadKey(10_000 + uploaded), Result: &cache.ActionResult{Status: "success"}, Manifest: &cache.Manifest{Files: []cache.FileEntry{file}},
			}}
			uploaded++
		}
		p.send(calls...)
	}

	summary, diags := cache.ParseAndValidateSummaryResult(p.send(providerCall{op: cache.OpSummary})[0].Payload)
	if summary == nil {
		t.Fatalf("invalid summary: %v", diags)
	}
	p.send(providerCall{op: cache.OpShutdown})
	if err := <-p.ended; err != nil {
		t.Fatalf("session: %v", err)
	}
	if summary.RestoredCount != prefetchedHits+late || summary.RestoredBytes != restoredBytes || summary.UploadedCount != uploaded {
		t.Fatalf("summary = %d restored (%d bytes), %d uploaded; want %d (%d bytes), %d",
			summary.RestoredCount, summary.RestoredBytes, summary.UploadedCount, prefetchedHits+late, restoredBytes, uploaded)
	}

	counts, total := f.requests()
	t.Logf("cache-server requests: %d in all, %v", total, counts)
	want := map[string]int{
		cache.CapabilitiesPath: 1,
		// The prefetch, then one per wave of late restores.
		cache.NegotiatePath:     1 + len(lateWaves),
		cache.DownloadBatchPath: 1 + len(lateWaves),
		// The writes of one window, stored at the summary: the first write,
		// which carries no blob, the small blobs, and the commit.
		cache.FindMissingPath: 1,
		cache.UploadBatchPath: 1,
		cache.CommitBatchPath: 1,
	}
	if !maps.Equal(counts, want) {
		t.Fatalf("cache-server requests = %v, want %v", counts, want)
	}
	if total >= 20 {
		t.Fatalf("the run sent %d cache-server requests, want fewer than 20", total)
	}
	if _, gets := f.reads.seen(); len(gets) != 0 {
		t.Fatalf("the restores downloaded %d small files one by one, want none", len(gets))
	}
	f.writes.mu.Lock()
	committed := len(f.writes.committed)
	f.writes.mu.Unlock()
	if committed != uploaded {
		t.Fatalf("the cache server committed %d entries, want %d", committed, uploaded)
	}
}
