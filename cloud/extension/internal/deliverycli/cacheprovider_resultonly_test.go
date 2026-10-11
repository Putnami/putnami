package deliverycli

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"go.putnami.dev/cloud/extension/internal/deliverycli/internal/remotecache"
	cache "go.putnami.dev/protocol/cache"
)

// resultOnlyCacheServer is a fake cache server for the result-only tests. It
// answers negotiate as cache-server does: an entry's result and manifest in
// every mode, and its downloads in every mode but minimal. It records each
// negotiate, and counts every other request: they are all blob transfers.
type resultOnlyCacheServer struct {
	srv *httptest.Server
	// entries maps a cache key to the one blob of its entry.
	entries map[string][]byte
	// answerAs makes the server report a requested key's entry under another key.
	answerAs map[string]string

	mu         sync.Mutex
	negotiates []negotiateCall
	transfers  int
	// failNext answers the next negotiate of a mode with a 400, once.
	failNext map[cache.Mode]bool
}

// negotiateCall is one negotiate the server received.
type negotiateCall struct {
	mode cache.Mode
	keys []string
}

func newResultOnlyCacheServer(t *testing.T, entries map[string][]byte) *resultOnlyCacheServer {
	t.Helper()
	f := &resultOnlyCacheServer{entries: entries, failNext: map[cache.Mode]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *resultOnlyCacheServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != cache.NegotiatePath {
		f.mu.Lock()
		f.transfers++
		f.mu.Unlock()
		blob, ok := f.entries[strings.TrimPrefix(r.URL.Path, "/blob/")]
		if !ok {
			// download-batch and anything else miss: the fetcher falls back to GET.
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(blob)
		return
	}
	var nreq cache.NegotiateRequest
	_ = json.NewDecoder(r.Body).Decode(&nreq)
	call := negotiateCall{mode: nreq.Mode}
	for _, k := range nreq.Keys {
		call.keys = append(call.keys, k.Key)
	}
	f.mu.Lock()
	f.negotiates = append(f.negotiates, call)
	fail := f.failNext[nreq.Mode]
	delete(f.failNext, nreq.Mode)
	f.mu.Unlock()
	if fail {
		http.Error(w, "negotiate failed", http.StatusBadRequest)
		return
	}
	results := make([]map[string]any, 0, len(nreq.Keys))
	for _, k := range nreq.Keys {
		reported := k.Key
		if other := f.answerAs[k.Key]; other != "" {
			reported = other
		}
		blob, hit := f.entries[k.Key]
		if !hit {
			results = append(results, map[string]any{"key": reported, "hit": false})
			continue
		}
		digest := cache.DigestOf(blob)
		entry := map[string]any{
			"key":      reported,
			"hit":      true,
			"result":   map[string]any{"status": "success", "sizeBytes": len(blob)},
			"manifest": map[string]any{"files": []map[string]any{{"path": "out.js", "digest": digest, "mode": 0o644, "size": len(blob)}}},
		}
		if nreq.Mode != cache.ModeMinimal {
			entry["downloads"] = []map[string]any{{
				"digest": digest, "url": f.srv.URL + "/blob/" + k.Key, "method": cache.TransferGet, "sizeBytes": len(blob),
			}}
		}
		if r.Header.Get(remotecache.EntryProvenanceHeader) == "v1" {
			entry["producer"] = cache.ProducerCI
			entry["producerIdentity"] = "ci-runner@example.test"
			entry["channel"] = cache.ChannelTrusted
		}
		results = append(results, entry)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"protocolVersion": cache.ProtocolVersion, "results": results})
}

// calls returns the negotiates received so far, sorted by mode then keys, and
// the number of blob transfers.
func (f *resultOnlyCacheServer) calls() ([]negotiateCall, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := slices.Clone(f.negotiates)
	slices.SortStableFunc(calls, func(a, b negotiateCall) int {
		if c := strings.Compare(string(a.mode), string(b.mode)); c != 0 {
			return c
		}
		return strings.Compare(strings.Join(a.keys, ","), strings.Join(b.keys, ","))
	})
	return calls, f.transfers
}

// sameCalls reports whether two lists name the same negotiates, in order.
func sameCalls(got, want []negotiateCall) bool {
	return slices.EqualFunc(got, want, func(a, b negotiateCall) bool {
		return a.mode == b.mode && slices.Equal(a.keys, b.keys)
	})
}

// wantNegotiatesOnly fails unless the server received exactly these negotiates
// and not one blob transfer.
func (f *resultOnlyCacheServer) wantNegotiatesOnly(t *testing.T, want ...negotiateCall) {
	t.Helper()
	got, transfers := f.calls()
	if !sameCalls(got, want) {
		t.Fatalf("negotiates = %+v, want %+v", got, want)
	}
	if transfers != 0 {
		t.Fatalf("blob transfers = %d, want none", transfers)
	}
}

// resultOnlyKey returns a distinct, valid cache key per letter.
func resultOnlyKey(letter string) string {
	return strings.Repeat(letter, cache.KeyLength)
}

// startResultOnlySession opens a serving session in the given mode, with the
// capabilities core advertises.
func startResultOnlySession(t *testing.T, f *resultOnlyCacheServer, mode cache.Mode, capabilities ...string) (*providerSession, string) {
	t.Helper()
	t.Setenv("PUTNAMI_CACHE_URL", f.srv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", "test-token")
	exchangeDir := t.TempDir()
	s := newProviderSession(t.TempDir(), io.Discard)
	t.Cleanup(s.close)
	init := mustInit(t, s, &cache.InitializeParams{
		ProtocolVersion: cache.ProviderProtocolMinVersion,
		BlobExchangeDir: exchangeDir,
		Mode:            mode,
		Capabilities:    capabilities,
	})
	if !init.Ready {
		t.Fatal("expected ready with PUTNAMI_CACHE_URL set")
	}
	return s, exchangeDir
}

// prefetchKeys sends one prefetch and waits until its outcomes are published.
func prefetchKeys(t *testing.T, s *providerSession, keys, resultOnly []string) {
	t.Helper()
	resp := s.prefetch(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 2, Op: cache.OpPrefetch},
		&cache.PrefetchParams{Keys: keys, ResultOnlyKeys: resultOnly})
	if !resp.OK {
		t.Fatalf("prefetch not OK: %+v", resp.Error)
	}
	s.bg.Wait()
}

// restoreKey sends one restore and returns its validated result.
func restoreKey(t *testing.T, s *providerSession, key string, resultOnly bool) *cache.RestoreResult {
	t.Helper()
	resp := s.restore(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 3, Op: cache.OpRestore},
		&cache.RestoreParams{Key: key, ResultOnly: resultOnly})
	if !resp.OK {
		t.Fatalf("restore not OK: %+v", resp.Error)
	}
	res, diags := cache.ParseAndValidateRestoreResult(resp.Payload)
	if res == nil {
		t.Fatalf("invalid restore result: %v", diags)
	}
	return res
}

// exchangeFiles returns how many files the blob-exchange directory holds.
func exchangeFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return err
	})
	if err != nil {
		t.Fatalf("walk the exchange dir: %v", err)
	}
	return n
}

// The provider echoes restore-result-only only to a core that lists it: core
// sends resultOnly and resultOnlyKeys only after the echo, so an older core
// keeps the wire it already has.
func TestInitializeEchoesRestoreResultOnlyOnlyWhenCoreAdvertisesIt(t *testing.T) {
	for _, tc := range []struct {
		name         string
		capabilities []string
		want         bool
	}{
		{"core lists it", []string{cache.CapabilityRestoreResultOnly}, true},
		{"core lists it next to protocol v2", []string{cache.CapabilityProviderProtocolV2, cache.CapabilityRestoreResultOnly}, true},
		{"core lists another capability", []string{cache.CapabilityProviderProtocolV2}, false},
		{"core lists nothing", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newResultOnlyCacheServer(t, nil)
			t.Setenv("PUTNAMI_CACHE_URL", f.srv.URL)
			t.Setenv("PUTNAMI_CACHE_TOKEN", "test-token")
			s := newProviderSession(t.TempDir(), io.Discard)
			defer s.close()
			init := mustInit(t, s, &cache.InitializeParams{
				ProtocolVersion: cache.ProviderProtocolMinVersion,
				BlobExchangeDir: t.TempDir(),
				Capabilities:    tc.capabilities,
			})
			if got := slices.Contains(init.Capabilities, cache.CapabilityRestoreResultOnly); got != tc.want {
				t.Fatalf("echoed %s = %v (capabilities %v), want %v", cache.CapabilityRestoreResultOnly, got, init.Capabilities, tc.want)
			}
		})
	}
}

// A prefetch asks for the result-only keys in minimal mode and for the others in
// the session's mode, in two negotiates, and downloads nothing. The summary
// counts the prefetched keys the server holds.
func TestPrefetchNegotiatesResultOnlyKeysInMinimalModeAndDownloadsNothing(t *testing.T) {
	onlyHit, onlyMiss := resultOnlyKey("a"), resultOnlyKey("b")
	filesHit, filesMiss := resultOnlyKey("c"), resultOnlyKey("d")
	f := newResultOnlyCacheServer(t, map[string][]byte{onlyHit: []byte("result only"), filesHit: []byte("with files")})
	s, exchangeDir := startResultOnlySession(t, f, cache.ModeFull, cache.CapabilityRestoreResultOnly)

	prefetchKeys(t, s, []string{onlyHit, onlyMiss, filesHit, filesMiss}, []string{onlyHit, onlyMiss})

	f.wantNegotiatesOnly(t,
		negotiateCall{cache.ModeFull, []string{filesHit, filesMiss}},
		negotiateCall{cache.ModeMinimal, []string{onlyHit, onlyMiss}},
	)
	if n := exchangeFiles(t, exchangeDir); n != 0 {
		t.Fatalf("the prefetch placed %d files, want none", n)
	}
	if stats := mustSummary(t, s); stats.PrefetchedCount != 2 {
		t.Fatalf("prefetchedCount = %d, want 2: the prefetched keys the server holds", stats.PrefetchedCount)
	}
}

// A session in minimal mode has one mode to ask in, so its prefetch is one
// negotiate whatever core marks result-only.
func TestPrefetchInAMinimalSessionIsOneNegotiate(t *testing.T) {
	only, files := resultOnlyKey("a"), resultOnlyKey("c")
	f := newResultOnlyCacheServer(t, map[string][]byte{only: []byte("result only"), files: []byte("with files")})
	s, _ := startResultOnlySession(t, f, cache.ModeMinimal, cache.CapabilityRestoreResultOnly)

	prefetchKeys(t, s, []string{only, files}, []string{only})

	f.wantNegotiatesOnly(t, negotiateCall{cache.ModeMinimal, []string{only, files}})
}

func TestPrefetchBatches(t *testing.T) {
	a, b, c := resultOnlyKey("a"), resultOnlyKey("b"), resultOnlyKey("c")
	for _, tc := range []struct {
		name       string
		keys       []string
		resultOnly []string
		mode       cache.Mode
		want       []negotiateCall
	}{
		{"no result-only key", []string{a, b}, nil, cache.ModeFull, []negotiateCall{{cache.ModeFull, []string{a, b}}}},
		{"every key result-only", []string{a, b}, []string{a, b}, cache.ModeFull, []negotiateCall{{cache.ModeMinimal, []string{a, b}}}},
		{"both kinds", []string{a, b, c}, []string{b}, cache.ModeToplevel, []negotiateCall{{cache.ModeMinimal, []string{b}}, {cache.ModeToplevel, []string{a, c}}}},
		{"minimal session", []string{a, b}, []string{a}, cache.ModeMinimal, []negotiateCall{{cache.ModeMinimal, []string{a, b}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []negotiateCall
			for _, batch := range prefetchBatches(tc.keys, tc.resultOnly, tc.mode) {
				got = append(got, negotiateCall{batch.mode, batch.keys})
			}
			if !sameCalls(got, tc.want) {
				t.Fatalf("batches = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A result-only restore answers from the prefetch index with the entry's exact
// result, manifest and provenance, and places no file: no negotiate and no blob
// transfer follow the prefetch.
func TestAResultOnlyRestorePlacesNoFile(t *testing.T) {
	hit, miss := resultOnlyKey("a"), resultOnlyKey("b")
	blob := []byte("compiled output bytes")
	f := newResultOnlyCacheServer(t, map[string][]byte{hit: blob})
	s, exchangeDir := startResultOnlySession(t, f, cache.ModeFull, cache.CapabilityProviderProtocolV2, cache.CapabilityRestoreResultOnly)
	prefetchKeys(t, s, []string{hit, miss}, []string{hit, miss})

	res := restoreKey(t, s, hit, true)
	if res.Status != cache.RestoreHit {
		t.Fatalf("restore status = %q, want hit", res.Status)
	}
	if res.Result == nil || res.Result.Status != "success" || res.Result.SizeBytes != int64(len(blob)) {
		t.Errorf("result = %+v, want the entry's result", res.Result)
	}
	if res.Manifest == nil || len(res.Manifest.Files) != 1 || res.Manifest.Files[0].Digest != cache.DigestOf(blob) || res.Manifest.Files[0].Path != "out.js" {
		t.Errorf("manifest = %+v, want the entry's full manifest", res.Manifest)
	}
	if res.Producer != cache.ProducerCI || res.ProducerIdentity != "ci-runner@example.test" || res.Channel != cache.ChannelTrusted {
		t.Errorf("provenance = (%q, %q, %q), want the entry's", res.Producer, res.ProducerIdentity, res.Channel)
	}
	if got := restoreKey(t, s, miss, true).Status; got != cache.RestoreMiss {
		t.Errorf("restore of the absent key = %q, want miss", got)
	}

	f.wantNegotiatesOnly(t, negotiateCall{cache.ModeMinimal, []string{hit, miss}})
	if n := exchangeFiles(t, exchangeDir); n != 0 {
		t.Fatalf("the result-only restore placed %d files, want none", n)
	}
	stats := mustSummary(t, s)
	if stats.RestoredCount != 1 || stats.RestoredBytes != 0 {
		t.Errorf("restored = %d entries, %d bytes; want 1 entry and no byte", stats.RestoredCount, stats.RestoredBytes)
	}
}

// A result-only restore of a key no prefetch covered negotiates that one key in
// minimal mode, and still places no file.
func TestAResultOnlyRestoreOutsideThePrefetchNegotiatesItsKeyInMinimalMode(t *testing.T) {
	key := resultOnlyKey("a")
	f := newResultOnlyCacheServer(t, map[string][]byte{key: []byte("compiled output bytes")})
	s, exchangeDir := startResultOnlySession(t, f, cache.ModeFull, cache.CapabilityRestoreResultOnly)

	if got := restoreKey(t, s, key, true).Status; got != cache.RestoreHit {
		t.Fatalf("restore status = %q, want hit", got)
	}

	f.wantNegotiatesOnly(t, negotiateCall{cache.ModeMinimal, []string{key}})
	if n := exchangeFiles(t, exchangeDir); n != 0 {
		t.Fatalf("the result-only restore placed %d files, want none", n)
	}
}

// Core does not verify a result-only hit, so the provider answers with the
// entry stored under the requested key or reports a miss: an entry the server
// reports under another key is never returned.
func TestAResultOnlyRestoreNeverAnswersWithAnotherKeysEntry(t *testing.T) {
	for _, prefetched := range []bool{true, false} {
		t.Run("prefetched="+strconv.FormatBool(prefetched), func(t *testing.T) {
			asked, other := resultOnlyKey("a"), resultOnlyKey("b")
			f := newResultOnlyCacheServer(t, map[string][]byte{asked: []byte("compiled output bytes")})
			f.answerAs = map[string]string{asked: other}
			s, _ := startResultOnlySession(t, f, cache.ModeFull, cache.CapabilityRestoreResultOnly)
			if prefetched {
				prefetchKeys(t, s, []string{asked}, []string{asked})
			}

			if got := restoreKey(t, s, asked, true).Status; got != cache.RestoreMiss {
				t.Fatalf("restore status = %q, want miss: the server answered under another key", got)
			}
		})
	}
}

// A restore without resultOnly downloads the entry's files. The prefetch
// answered the key in the session's mode, so the restore needs no negotiate.
func TestAFullRestoreDownloadsTheEntrysFiles(t *testing.T) {
	key := resultOnlyKey("a")
	blob := []byte("compiled output bytes")
	f := newResultOnlyCacheServer(t, map[string][]byte{key: blob})
	s, exchangeDir := startResultOnlySession(t, f, cache.ModeFull, cache.CapabilityRestoreResultOnly)
	prefetchKeys(t, s, []string{key}, nil)
	f.wantNegotiatesOnly(t, negotiateCall{cache.ModeFull, []string{key}})

	if got := restoreKey(t, s, key, false).Status; got != cache.RestoreHit {
		t.Fatalf("restore status = %q, want hit", got)
	}

	path, _ := cache.BlobExchangePath(exchangeDir, cache.DigestOf(blob))
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, blob) {
		t.Fatalf("exchange blob = %q, %v; want the restore to download it", got, err)
	}
	calls, transfers := f.calls()
	if len(calls) != 1 || transfers == 0 {
		t.Fatalf("negotiates = %d, blob transfers = %d; want the one prefetch negotiate and a download", len(calls), transfers)
	}
	if stats := mustSummary(t, s); stats.RestoredCount != 1 || stats.RestoredBytes != int64(len(blob)) {
		t.Errorf("restored = %d entries, %d bytes; want 1 entry of %d bytes", stats.RestoredCount, stats.RestoredBytes, len(blob))
	}
}

// When core restores the files of a key it prefetched as result-only, the index
// holds no download for it: the restore negotiates the key again in the
// session's mode, then downloads. A minimal miss needs no second ask.
func TestAFullRestoreOfAResultOnlyKeyNegotiatesItAgainInTheSessionMode(t *testing.T) {
	hit, miss := resultOnlyKey("a"), resultOnlyKey("b")
	blob := []byte("compiled output bytes")
	f := newResultOnlyCacheServer(t, map[string][]byte{hit: blob})
	s, exchangeDir := startResultOnlySession(t, f, cache.ModeFull, cache.CapabilityRestoreResultOnly)
	prefetchKeys(t, s, []string{hit, miss}, []string{hit, miss})

	if got := restoreKey(t, s, miss, false).Status; got != cache.RestoreMiss {
		t.Fatalf("restore of the absent key = %q, want miss", got)
	}
	f.wantNegotiatesOnly(t, negotiateCall{cache.ModeMinimal, []string{hit, miss}})

	if got := restoreKey(t, s, hit, false).Status; got != cache.RestoreHit {
		t.Fatalf("restore status = %q, want hit", got)
	}
	path, _ := cache.BlobExchangePath(exchangeDir, cache.DigestOf(blob))
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, blob) {
		t.Fatalf("exchange blob = %q, %v; want the restore to download it", got, err)
	}
	calls, _ := f.calls()
	want := []negotiateCall{{cache.ModeFull, []string{hit}}, {cache.ModeMinimal, []string{hit, miss}}}
	if !sameCalls(calls, want) {
		t.Fatalf("negotiates = %+v, want %+v", calls, want)
	}
}

// A prefetch negotiate that fails leaves its own keys to their restores and
// keeps the outcomes of the one that succeeded.
func TestAFailedPrefetchNegotiateLeavesOnlyItsKeysToTheirRestores(t *testing.T) {
	only, files := resultOnlyKey("a"), resultOnlyKey("c")
	f := newResultOnlyCacheServer(t, map[string][]byte{only: []byte("result only"), files: []byte("with files")})
	f.failNext[cache.ModeMinimal] = true
	s, _ := startResultOnlySession(t, f, cache.ModeFull, cache.CapabilityRestoreResultOnly)
	prefetchKeys(t, s, []string{only, files}, []string{only})

	if got := restoreKey(t, s, files, true).Status; got != cache.RestoreHit {
		t.Fatalf("restore of the key whose negotiate succeeded = %q, want hit", got)
	}
	f.wantNegotiatesOnly(t,
		negotiateCall{cache.ModeFull, []string{files}},
		negotiateCall{cache.ModeMinimal, []string{only}},
	)

	if got := restoreKey(t, s, only, true).Status; got != cache.RestoreHit {
		t.Fatalf("restore of the key whose negotiate failed = %q, want hit from its own negotiate", got)
	}
	f.wantNegotiatesOnly(t,
		negotiateCall{cache.ModeFull, []string{files}},
		negotiateCall{cache.ModeMinimal, []string{only}},
		negotiateCall{cache.ModeMinimal, []string{only}},
	)
}

// A second prefetch adds the keys the index does not hold yet, and the summary
// counts each key once.
func TestASecondPrefetchAddsItsKeysToTheIndex(t *testing.T) {
	first, second := resultOnlyKey("a"), resultOnlyKey("b")
	f := newResultOnlyCacheServer(t, map[string][]byte{first: []byte("one"), second: []byte("two")})
	s, _ := startResultOnlySession(t, f, cache.ModeFull, cache.CapabilityRestoreResultOnly)
	prefetchKeys(t, s, []string{first}, []string{first})
	prefetchKeys(t, s, []string{first, second}, []string{first, second})

	if got := restoreKey(t, s, second, true).Status; got != cache.RestoreHit {
		t.Fatalf("restore status = %q, want hit from the second prefetch", got)
	}

	f.wantNegotiatesOnly(t,
		negotiateCall{cache.ModeMinimal, []string{first}},
		negotiateCall{cache.ModeMinimal, []string{first, second}},
	)
	if stats := mustSummary(t, s); stats.PrefetchedCount != 2 {
		t.Fatalf("prefetchedCount = %d, want 2: each key once", stats.PrefetchedCount)
	}
}

// The whole exchange over the wire: core lists the capability, names one
// result-only key in the prefetch, restores it for its result and the other key
// for its files. Only the second key's blob reaches the exchange directory.
func TestRunCacheProviderSession_ResultOnlyRoundTrip(t *testing.T) {
	only, files := resultOnlyKey("a"), resultOnlyKey("c")
	onlyBlob, filesBlob := []byte("result only"), []byte("with files")
	f := newResultOnlyCacheServer(t, map[string][]byte{only: onlyBlob, files: filesBlob})
	t.Setenv("PUTNAMI_CACHE_URL", f.srv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", "test-token")

	exchangeDir := t.TempDir()
	reqs := []string{
		`{"protocolVersion":1,"id":1,"op":"initialize","payload":{"protocolVersion":1,"blobExchangeDir":` + strconv.Quote(exchangeDir) + `,"mode":"full","capabilities":["restore-result-only"]}}`,
		`{"protocolVersion":1,"id":2,"op":"prefetch","payload":{"keys":["` + only + `","` + files + `"],"resultOnlyKeys":["` + only + `"]}}`,
		`{"protocolVersion":1,"id":3,"op":"restore","payload":{"key":"` + only + `","resultOnly":true}}`,
		`{"protocolVersion":1,"id":4,"op":"restore","payload":{"key":"` + files + `"}}`,
		`{"protocolVersion":1,"id":5,"op":"summary","payload":{}}`,
		`{"protocolVersion":1,"id":6,"op":"shutdown"}`,
	}
	var out, logw bytes.Buffer
	if err := runCacheProviderSession(strings.NewReader(strings.Join(reqs, "\n")+"\n"), &out, &logw, t.TempDir()); err != nil {
		t.Fatalf("session: %v (log: %s)", err, logw.String())
	}

	byID := map[int64]*cache.ProviderResponse{}
	for _, r := range decodeResponses(t, &out) {
		if !r.OK {
			t.Fatalf("response id %d not OK: %+v (log: %s)", r.ID, r.Error, logw.String())
		}
		byID[r.ID] = r
	}
	if len(byID) != len(reqs) {
		t.Fatalf("got %d responses, want %d (log: %s)", len(byID), len(reqs), logw.String())
	}
	init, diags := cache.ParseAndValidateInitializeResult(byID[1].Payload)
	if init == nil || !slices.Contains(init.Capabilities, cache.CapabilityRestoreResultOnly) {
		t.Fatalf("initialize = %+v (%v), want the capability echoed", init, diags)
	}
	for _, id := range []int64{3, 4} {
		res, diags := cache.ParseAndValidateRestoreResult(byID[id].Payload)
		if res == nil || res.Status != cache.RestoreHit || res.Result == nil || res.Manifest == nil {
			t.Fatalf("restore %d = %+v (%v), want a hit with its result and manifest", id, res, diags)
		}
	}
	stats, diags := cache.ParseAndValidateSummaryResult(byID[5].Payload)
	if stats == nil || stats.PrefetchedCount != 2 || stats.RestoredCount != 2 || stats.RestoredBytes != int64(len(filesBlob)) {
		t.Fatalf("summary = %+v (%v), want 2 prefetched, 2 restored, %d bytes", stats, diags, len(filesBlob))
	}

	onlyPath, _ := cache.BlobExchangePath(exchangeDir, cache.DigestOf(onlyBlob))
	if _, err := os.Stat(onlyPath); !os.IsNotExist(err) {
		t.Errorf("the result-only key's blob is in the exchange dir (stat err = %v), want it absent", err)
	}
	filesPath, _ := cache.BlobExchangePath(exchangeDir, cache.DigestOf(filesBlob))
	if got, err := os.ReadFile(filesPath); err != nil || !bytes.Equal(got, filesBlob) {
		t.Errorf("exchange blob = %q, %v; want the full restore to download it", got, err)
	}
	calls, _ := f.calls()
	want := []negotiateCall{{cache.ModeFull, []string{files}}, {cache.ModeMinimal, []string{only}}}
	if !sameCalls(calls, want) {
		t.Fatalf("negotiates = %+v, want %+v", calls, want)
	}
}
