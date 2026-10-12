package deliverycli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cache "go.putnami.dev/protocol/cache"
)

// The late negotiate tests: restores the prefetch did not cover share one
// negotiate, and one download-batch for the small files of their hits. See
// cachenegotiate.go.

// pinLateNegotiate sets the bounds of a group for the sessions a test creates
// after the call, and restores them afterwards. A test that sends a group of a
// known size sets keys to it and the delays to an hour: the group then goes
// out when it is full, whatever the scheduler does.
func pinLateNegotiate(t *testing.T, quiet, maxWait time.Duration, keys int) {
	t.Helper()
	q, m, k := lateNegotiateQuiet, lateNegotiateMax, lateNegotiateKeys
	lateNegotiateQuiet, lateNegotiateMax, lateNegotiateKeys = quiet, maxWait, keys
	t.Cleanup(func() { lateNegotiateQuiet, lateNegotiateMax, lateNegotiateKeys = q, m, k })
}

// restoreAll sends one restore per key at once, as core's workers do, and
// returns the results in the order of keys. resultOnly lists the keys core
// restores for their result only.
func restoreAll(t *testing.T, s *providerSession, keys []string, resultOnly ...string) []*cache.RestoreResult {
	t.Helper()
	resps := make([]*cache.ProviderResponse, len(keys))
	var wg sync.WaitGroup
	for i, key := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resps[i] = s.restore(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: int64(3 + i), Op: cache.OpRestore},
				&cache.RestoreParams{Key: key, ResultOnly: slices.Contains(resultOnly, key)})
		}()
	}
	wg.Wait()
	out := make([]*cache.RestoreResult, len(keys))
	for i, resp := range resps {
		if !resp.OK {
			t.Fatalf("restore %s not OK: %+v", short(keys[i]), resp.Error)
		}
		res, diags := cache.ParseAndValidateRestoreResult(resp.Payload)
		if res == nil {
			t.Fatalf("invalid restore result: %v", diags)
		}
		out[i] = res
	}
	return out
}

// sortedCalls returns calls with the keys of each one sorted.
func sortedCalls(calls []negotiateCall) []negotiateCall {
	out := make([]negotiateCall, len(calls))
	for i, c := range calls {
		out[i] = negotiateCall{mode: c.mode, keys: slices.Sorted(slices.Values(c.keys))}
	}
	return out
}

// Restores of keys the prefetch did not cover that arrive together cost one
// negotiate and one download-batch, not two requests each. A file two hits
// share is asked for once and counted once.
func TestLateRestoresShareOneNegotiateAndOneDownloadBatch(t *testing.T) {
	const hits = 5
	pinLateNegotiate(t, time.Hour, time.Hour, hits+1)
	shared := smallBlob("shared", 300)
	entries := map[string][][]byte{}
	keys := make([]string, 0, hits+1)
	blobs := make([][]byte, 0, hits)
	wantBytes := int64(len(shared))
	for h := range hits {
		key := inlineKey(h)
		keys = append(keys, key)
		own := smallBlob(fmt.Sprintf("hit %d", h), 100+h)
		entries[key] = [][]byte{own}
		blobs = append(blobs, own)
		wantBytes += int64(len(own))
		if h < 2 {
			entries[key] = append(entries[key], shared)
		}
	}
	miss := inlineKey(hits)
	keys = append(keys, miss)
	f := newInlineCacheServer(t, entries)
	s, exchangeDir := startInlineSession(t, f, io.Discard)

	results := restoreAll(t, s, keys)
	for i, res := range results {
		want := cache.RestoreHit
		if keys[i] == miss {
			want = cache.RestoreMiss
		}
		if res.Status != want {
			t.Fatalf("restore %s = %q, want %q", short(keys[i]), res.Status, want)
		}
	}
	for _, blob := range append(blobs, shared) {
		wantExchangeBlob(t, exchangeDir, blob)
	}

	calls := sortedCalls(f.negotiateCalls())
	if want := []negotiateCall{{mode: cache.ModeFull, keys: slices.Sorted(slices.Values(keys))}}; !sameCalls(calls, want) {
		t.Fatalf("negotiates = %+v, want one for the %d keys", calls, len(keys))
	}
	asks, gets := f.seen()
	if want := [][]string{digestsOf(append(blobs, shared)...)}; !slices.EqualFunc(asks, want, slices.Equal[[]string]) || len(gets) != 0 {
		t.Fatalf("download-batch digests = %v, blob GETs = %v; want one request for the %d small files", asks, gets, len(blobs)+1)
	}
	if stats := mustSummary(t, s); stats.RestoredCount != hits || stats.RestoredBytes != wantBytes {
		t.Fatalf("restored = %d entries, %d bytes; want %d entries of %d bytes", stats.RestoredCount, stats.RestoredBytes, hits, wantBytes)
	}
}

// A restore that arrives alone waits for the quiet delay, then sends its
// negotiate: it never waits for a key that does not come.
func TestALateRestoreAloneGoesOutAfterTheQuietDelay(t *testing.T) {
	const quiet = 20 * time.Millisecond
	pinLateNegotiate(t, quiet, time.Hour, cache.MaxKeysPerRequest)
	key := inlineKey(0)
	f := newInlineCacheServer(t, map[string][][]byte{key: {smallBlob("only", 100)}})
	s, _ := startInlineSession(t, f, io.Discard)

	start := time.Now()
	if got := restoreKey(t, s, key, false).Status; got != cache.RestoreHit {
		t.Fatalf("restore = %q, want hit", got)
	}
	if waited := time.Since(start); waited < quiet {
		t.Fatalf("the restore was answered after %s, before the quiet delay of %s", waited, quiet)
	}
	if calls := f.negotiateCalls(); len(calls) != 1 || len(calls[0].keys) != 1 {
		t.Fatalf("negotiates = %+v, want one for the key", calls)
	}
}

// A group goes out once no key joined it for the quiet delay, at the latest
// the longest delay after its first key, and at once when it is full. A key
// that comes after that opens a new group, and each mode has its own.
func TestALateGroupIsDueAfterTheQuietDelayOrTheLongestOne(t *testing.T) {
	const quiet, longest = 20 * time.Millisecond, 50 * time.Millisecond
	l := &lateNegotiates{quiet: quiet, max: longest, keys: 3}
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	a, b, c, d := inlineKey(0), inlineKey(1), inlineKey(2), inlineKey(3)

	g, start := l.join(cache.ModeFull, a, t0)
	if !start {
		t.Fatal("the first key did not open a group")
	}
	if wait := l.due(g, t0); wait != quiet {
		t.Fatalf("a group of one key waits %s, want the quiet delay %s", wait, quiet)
	}
	if again, start := l.join(cache.ModeFull, a, t0.Add(10*time.Millisecond)); again != g || start || len(g.keys) != 1 {
		t.Fatalf("a key that joins twice is negotiated once: same group %t, new %t, %d keys", again == g, start, len(g.keys))
	}
	if wait := l.due(g, t0.Add(10*time.Millisecond)); wait != quiet {
		t.Fatalf("a key that joined restarts the quiet delay: wait %s, want %s", wait, quiet)
	}
	if other, start := l.join(cache.ModeMinimal, b, t0.Add(40*time.Millisecond)); other == g || !start {
		t.Fatal("a key of another mode joined the group")
	}
	if again, _ := l.join(cache.ModeFull, b, t0.Add(40*time.Millisecond)); again != g {
		t.Fatal("a key of the same mode did not join the open group")
	}
	if wait := l.due(g, t0.Add(40*time.Millisecond)); wait != 10*time.Millisecond {
		t.Fatalf("a group near its longest delay waits %s, want the 10ms left of it", wait)
	}
	if wait := l.due(g, t0.Add(longest)); wait != 0 || !g.sealed {
		t.Fatalf("a group at its longest delay waits %s and sealed %t, want 0 and sealed", wait, g.sealed)
	}
	if next, start := l.join(cache.ModeFull, c, t0.Add(longest)); next == g || !start {
		t.Fatal("a key that comes after the group is due joined it")
	}

	full, _ := l.join(cache.ModeFull, d, t0.Add(longest))
	if len(full.keys) != 2 {
		t.Fatalf("the new group holds %d keys, want 2", len(full.keys))
	}
	l.join(cache.ModeFull, a, t0.Add(longest))
	select {
	case <-full.full:
	default:
		t.Fatal("a group that holds its most keys did not say it is full")
	}
	if wait := l.due(full, t0.Add(longest)); wait != 0 || !full.sealed {
		t.Fatalf("a full group waits %s, want 0", wait)
	}
}

// Result-only restores negotiate in minimal mode and the others in the
// session's mode, so the two go out in two negotiates. The download-batch asks
// only for the files of the restores that read them.
func TestLateRestoresGroupByMode(t *testing.T) {
	pinLateNegotiate(t, time.Hour, time.Hour, 2)
	a, b, c, d := inlineKey(0), inlineKey(1), inlineKey(2), inlineKey(3)
	blobA, blobB, blobC, blobD := smallBlob("a", 100), smallBlob("b", 200), smallBlob("c", 300), smallBlob("d", 400)
	f := newInlineCacheServer(t, map[string][][]byte{a: {blobA}, b: {blobB}, c: {blobC}, d: {blobD}})
	s, exchangeDir := startInlineSession(t, f, io.Discard)

	for _, res := range restoreAll(t, s, []string{a, b, c, d}, c, d) {
		if res.Status != cache.RestoreHit {
			t.Fatalf("restore = %q, want hit", res.Status)
		}
	}
	calls := sortedCalls(f.negotiateCalls())
	slices.SortFunc(calls, func(x, y negotiateCall) int { return strings.Compare(string(x.mode), string(y.mode)) })
	want := sortedCalls([]negotiateCall{{mode: cache.ModeFull, keys: []string{a, b}}, {mode: cache.ModeMinimal, keys: []string{c, d}}})
	slices.SortFunc(want, func(x, y negotiateCall) int { return strings.Compare(string(x.mode), string(y.mode)) })
	if !sameCalls(calls, want) {
		t.Fatalf("negotiates = %+v, want %+v", calls, want)
	}
	asks, gets := f.seen()
	if want := [][]string{digestsOf(blobA, blobB)}; !slices.EqualFunc(asks, want, slices.Equal[[]string]) || len(gets) != 0 {
		t.Fatalf("download-batch digests = %v, blob GETs = %v; want one request for the files of the restores that read them", asks, gets)
	}
	if n := exchangeFiles(t, exchangeDir); n != 2 {
		t.Fatalf("the exchange directory holds %d files, want the 2 of the restores that read them", n)
	}
}

// A group whose negotiate fails leaves each restore to negotiate its own key,
// as before: the failure of the shared request costs no restore.
func TestAFailedLateNegotiateLeavesEachRestoreToNegotiateItsOwnKey(t *testing.T) {
	pinLateNegotiate(t, time.Hour, time.Hour, 3)
	a, b, c := resultOnlyKey("a"), resultOnlyKey("b"), resultOnlyKey("c")
	f := newRestoreCacheServer(t, map[string][][]byte{a: {largeBlob("a")}, b: {largeBlob("b")}, c: {largeBlob("c")}})
	var logw lockedWriter
	s, _, _ := startRestoreSession(t, f, &logw)
	f.failNextNegotiate()

	for _, res := range restoreAll(t, s, []string{a, b, c}) {
		if res.Status != cache.RestoreHit {
			t.Fatalf("restore = %q, want hit", res.Status)
		}
	}
	calls := f.calls()
	if len(calls) != 4 || len(calls[0].keys) != 3 {
		t.Fatalf("negotiates = %+v, want the failed one for the 3 keys, then one per key", calls)
	}
	single := make([]string, 0, len(calls)-1)
	for _, call := range calls[1:] {
		if len(call.keys) != 1 || call.mode != cache.ModeFull {
			t.Fatalf("negotiate after the failure = %+v, want one key in the session's mode", call)
		}
		single = append(single, call.keys[0])
	}
	if slices.Sort(single); !slices.Equal(single, []string{a, b, c}) {
		t.Fatalf("keys negotiated one by one = %v, want each key once", single)
	}
	if log := logw.String(); !strings.Contains(log, "negotiate of 3 late keys") {
		t.Fatalf("the failed group must be said once, got %q", log)
	}
}

// A refused credential is refused for every key: the restores of the group
// report the refusal and send no negotiate of their own.
func TestARefusedLateNegotiateIsNotSentAgainPerKey(t *testing.T) {
	pinLateNegotiate(t, time.Hour, time.Hour, 3)
	var negotiates atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != cache.NegotiatePath {
			http.NotFound(w, r)
			return
		}
		negotiates.Add(1)
		writeJSON(w, http.StatusForbidden, cache.ErrorResponse{ProtocolVersion: cache.ProtocolVersion, Code: "forbidden", Message: "this credential does not read the cache"})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("PUTNAMI_CACHE_URL", srv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", "test-token")
	s := newProviderSession(t.TempDir(), io.Discard)
	t.Cleanup(s.close)
	mustInit(t, s, &cache.InitializeParams{ProtocolVersion: cache.ProviderProtocolMinVersion, BlobExchangeDir: t.TempDir(), Mode: cache.ModeFull})

	for _, res := range restoreAll(t, s, []string{inlineKey(0), inlineKey(1), inlineKey(2)}) {
		if res.Status != cache.RestoreError {
			t.Fatalf("restore = %q, want error", res.Status)
		}
	}
	if n := negotiates.Load(); n != 1 {
		t.Fatalf("negotiates = %d, want the one refused group", n)
	}
}

// Restores whose indexed downloads are too old negotiate again together: the
// downloads they use are then the ones that group signed.
func TestOldRestoresNegotiateAgainTogether(t *testing.T) {
	pinLateNegotiate(t, time.Hour, time.Hour, 2)
	a, b := resultOnlyKey("a"), resultOnlyKey("b")
	f := newRestoreCacheServer(t, map[string][][]byte{a: {largeBlob("a")}, b: {largeBlob("b")}})
	s, exchangeDir, clock := startRestoreSession(t, f, io.Discard)
	prefetchKeys(t, s, []string{a, b}, nil)
	clock.advance(restoreTransferMaxAge)

	for _, res := range restoreAll(t, s, []string{a, b}) {
		if res.Status != cache.RestoreHit {
			t.Fatalf("restore = %q, want hit", res.Status)
		}
	}
	wantExchangeBlob(t, exchangeDir, largeBlob("a"))
	wantExchangeBlob(t, exchangeDir, largeBlob("b"))
	calls := sortedCalls(f.calls())
	if want := sortedCalls([]negotiateCall{{mode: cache.ModeFull, keys: []string{a, b}}, {mode: cache.ModeFull, keys: []string{a, b}}}); !sameCalls(calls, want) {
		t.Fatalf("negotiates = %+v, want the prefetch and one more for both keys", calls)
	}
	f.wantDownloads(t, 2, blobDownload{generation: 2, status: http.StatusOK}, blobDownload{generation: 2, status: http.StatusOK})
}

// Closing the session answers a restore that waits in a group: it never
// outlives the session.
func TestClosingTheSessionAnswersTheRestoresOfAnOpenGroup(t *testing.T) {
	pinLateNegotiate(t, time.Hour, time.Hour, cache.MaxKeysPerRequest)
	key := inlineKey(0)
	f := newInlineCacheServer(t, map[string][][]byte{key: {smallBlob("only", 100)}})
	s, _ := startInlineSession(t, f, io.Discard)

	answered := make(chan *cache.ProviderResponse, 1)
	go func() {
		answered <- s.restore(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 3, Op: cache.OpRestore},
			&cache.RestoreParams{Key: key})
	}()
	for joined := false; !joined; {
		s.late.mu.Lock()
		joined = s.late.open[cache.ModeFull] != nil
		s.late.mu.Unlock()
		if !joined {
			time.Sleep(time.Millisecond)
		}
	}
	s.close()

	select {
	case resp := <-answered:
		res, _ := cache.ParseAndValidateRestoreResult(resp.Payload)
		if res == nil || res.Status != cache.RestoreError {
			t.Fatalf("restore = %+v, want an error", resp)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the restore was not answered after the session closed")
	}
	if calls := f.negotiateCalls(); len(calls) != 0 {
		t.Fatalf("negotiates = %+v, want none from a closed session", calls)
	}
}

// writeJSONLine encodes v as one line of the provider protocol.
func writeJSONLine(t *testing.T, w io.Writer, v any) {
	t.Helper()
	line, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
}
