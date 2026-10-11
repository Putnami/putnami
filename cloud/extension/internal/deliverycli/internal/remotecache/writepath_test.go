package remotecache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cache "go.putnami.dev/protocol/cache"
)

// fastRetry is the provider's retry policy with waits a test can afford.
var fastRetry = WriteRetry{Attempts: 3, Backoff: time.Millisecond}

// testKey returns a distinct, well-formed cache key.
func testKey(i int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("key %d", i)))
	return hex.EncodeToString(sum[:])
}

// entryOf builds an entry with one file per content and adds the contents to
// src.
func entryOf(key string, src mapBlobSource, contents ...[]byte) StoreInput {
	var size int64
	files := make([]cache.FileEntry, 0, len(contents))
	for i, content := range contents {
		digest := cache.DigestOf(content)
		src[digest] = content
		files = append(files, cache.FileEntry{
			Path: fmt.Sprintf("dist/%d.bin", i), Digest: digest, Mode: 0o644, Size: int64(len(content)),
		})
		size += int64(len(content))
	}
	return StoreInput{
		Key:      key,
		Result:   &cache.ActionResult{Status: "success", SizeBytes: size},
		Manifest: &cache.Manifest{Files: files},
	}
}

// assertNoPartialCommit fails when the server holds a commit for an entry
// without every blob of its manifest: the invariant every failure path keeps.
func assertNoPartialCommit(t *testing.T, f *fakeCAS, in []StoreInput, out *BatchOutcome) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range in {
		if !out.Committed[e.Key] {
			continue
		}
		if out.Failed[e.Key] != nil {
			t.Errorf("entry %s is both committed and failed", e.Key[:8])
		}
		for _, file := range e.Manifest.Files {
			if !f.present[file.Digest] {
				t.Errorf("entry %s is committed without its blob %s", e.Key[:8], file.Digest)
			}
		}
	}
}

// TestStoreBatch_RetriesATransientControlFailure pins the first defense of a
// window: an exchange that fails for a reason that can pass is sent again, and
// the window loses nothing. Before the retry, one 503 on any of the three
// exchanges dropped every entry of the window.
func TestStoreBatch_RetriesATransientControlFailure(t *testing.T) {
	routes := []string{cache.FindMissingPath, cache.UploadBatchPath, cache.CommitBatchPath}
	faults := map[string]int{
		"503":                http.StatusServiceUnavailable,
		"500":                http.StatusInternalServerError,
		"429":                http.StatusTooManyRequests,
		"408":                http.StatusRequestTimeout,
		"dropped connection": dropConnection,
	}
	for _, route := range routes {
		for name, fault := range faults {
			t.Run(route+" "+name, func(t *testing.T) {
				f := newFakeCAS()
				f.script[route] = []int{fault, fault}
				srv := httptest.NewServer(f.handler(t))
				defer srv.Close()

				in, src, digests := batchFixture()
				c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
				out, err := c.StoreBatch(context.Background(), in, src, nil, WritePath{CoalesceSmall: true, Retry: fastRetry})
				if err != nil {
					t.Fatalf("StoreBatch: %v", err)
				}
				for _, e := range in {
					if !out.Committed[e.Key] {
						t.Errorf("entry %s not committed", e.Key[:8])
					}
				}
				if len(out.Failed) != 0 {
					t.Errorf("Failed = %v, want none", out.Failed)
				}
				if got := f.count(route); got != 3 {
					t.Errorf("%s received %d requests, want 3 (two failures, then the answer)", route, got)
				}
				if out.BlobsUploaded != len(digests) {
					t.Errorf("BlobsUploaded = %d, want %d", out.BlobsUploaded, len(digests))
				}
				assertNoPartialCommit(t, f, in, out)
			})
		}
	}
}

// TestStoreBatch_GivesUpAfterItsAttempts proves the retry is bounded: a server
// that keeps failing costs the attempts of the policy and no more.
func TestStoreBatch_GivesUpAfterItsAttempts(t *testing.T) {
	for _, route := range []string{cache.FindMissingPath, cache.UploadBatchPath, cache.CommitBatchPath} {
		t.Run(route, func(t *testing.T) {
			f := newFakeCAS()
			f.always[route] = http.StatusServiceUnavailable
			srv := httptest.NewServer(f.handler(t))
			defer srv.Close()

			in, src, _ := batchFixture()
			c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
			out, err := c.StoreBatch(context.Background(), in, src, nil, WritePath{CoalesceSmall: true, Retry: fastRetry})
			if !IsStatus(err, http.StatusServiceUnavailable) {
				t.Fatalf("err = %v, want HTTP 503", err)
			}
			if out != nil {
				t.Errorf("a window that failed as a whole returns no outcome, got %+v", out)
			}
			if got := f.count(route); got != fastRetry.Attempts {
				t.Errorf("%s received %d requests, want %d", route, got, fastRetry.Attempts)
			}
			if f.nCommitBatch != 0 {
				t.Errorf("commit-batch committed %d requests, want none", f.nCommitBatch)
			}
		})
	}
}

// TestStoreBatch_NeverRetriesAVerdict pins what is not transient: an answer
// that is a verdict on the credential or on the request is sent once.
func TestStoreBatch_NeverRetriesAVerdict(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusConflict, http.StatusRequestEntityTooLarge, http.StatusNotImplemented,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newFakeCAS()
			f.always[cache.FindMissingPath] = status
			srv := httptest.NewServer(f.handler(t))
			defer srv.Close()

			in, src, _ := batchFixture()
			c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
			_, err := c.StoreBatch(context.Background(), in, src, nil, WritePath{Retry: fastRetry})
			if !IsStatus(err, status) {
				t.Fatalf("err = %v, want HTTP %d", err, status)
			}
			if got := f.count(cache.FindMissingPath); got != 1 {
				t.Errorf("find-missing received %d requests, want 1", got)
			}
		})
	}
}

// TestStoreBatch_ARefusedWriteIsNeitherRetriedNorSplit pins the latch
// contract: the provider closes its write path on the first 403, so the client
// must not send a second write of any kind after one. A 401 is the same
// verdict for a bearer that cannot be renewed.
func TestStoreBatch_ARefusedWriteIsNeitherRetriedNorSplit(t *testing.T) {
	policy := WriteRetry{Attempts: 3, Backoff: time.Millisecond, SplitWindow: true}
	for _, status := range []int{http.StatusForbidden, http.StatusUnauthorized} {
		for _, route := range []string{cache.FindMissingPath, cache.UploadBatchPath, cache.CommitBatchPath} {
			t.Run(fmt.Sprintf("%d %s", status, route), func(t *testing.T) {
				f := newFakeCAS()
				f.always[route] = status
				srv := httptest.NewServer(f.handler(t))
				defer srv.Close()

				in, src, _ := batchFixture()
				c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
				out, err := c.StoreBatch(context.Background(), in, src, nil, WritePath{CoalesceSmall: true, Retry: policy})
				if !IsStatus(err, status) {
					t.Fatalf("err = %v, want HTTP %d", err, status)
				}
				if out != nil {
					t.Errorf("a refused window returns no outcome, got %+v", out)
				}
				if got := f.count(route); got != 1 {
					t.Errorf("%s received %d requests, want 1: a refusal is sent once", route, got)
				}
			})
		}
	}
}

// TestRetryWrite pins the retry loop itself: its attempts, its waits, and that
// it stops with the context.
func TestRetryWrite(t *testing.T) {
	unavailable := &StatusError{Op: "find-missing", Status: http.StatusServiceUnavailable}

	t.Run("waits Backoff, then four times longer", func(t *testing.T) {
		calls := 0
		start := time.Now()
		_, err := retryWrite(context.Background(), WriteRetry{Attempts: 3, Backoff: 20 * time.Millisecond}, func() (int, error) {
			calls++
			return 0, unavailable
		})
		if !errors.Is(err, unavailable) || calls != 3 {
			t.Fatalf("err = %v after %d calls, want the 503 after 3", err, calls)
		}
		// 20 ms, then 80 ms.
		if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
			t.Errorf("three attempts took %s, want at least the 100 ms of the two waits", elapsed)
		}
	})

	t.Run("returns the answer of a later attempt", func(t *testing.T) {
		calls := 0
		got, err := retryWrite(context.Background(), fastRetry, func() (string, error) {
			calls++
			if calls < 3 {
				return "", unavailable
			}
			return "stored", nil
		})
		if err != nil || got != "stored" || calls != 3 {
			t.Fatalf("got %q, %v after %d calls, want stored after 3", got, err, calls)
		}
	})

	t.Run("stops during a wait when the context ends", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		done := make(chan error, 1)
		go func() {
			_, err := retryWrite(ctx, WriteRetry{Attempts: 5, Backoff: time.Hour}, func() (int, error) {
				calls++
				cancel()
				return 0, unavailable
			})
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, unavailable) {
				t.Fatalf("err = %v, want the exchange's own error", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("retryWrite still waits after its context ended")
		}
		if calls != 1 {
			t.Errorf("calls = %d, want 1", calls)
		}
	})

	t.Run("the zero policy sends once", func(t *testing.T) {
		calls := 0
		_, err := retryWrite(context.Background(), WriteRetry{}, func() (int, error) {
			calls++
			return 0, unavailable
		})
		if err == nil || calls != 1 {
			t.Fatalf("err = %v after %d calls, want the 503 after 1", err, calls)
		}
	})

	t.Run("reports progress on success only", func(t *testing.T) {
		var reports atomic.Int32
		ctx := WithUploadProgress(context.Background(), func() { reports.Add(1) })
		calls := 0
		_, _ = retryWrite(ctx, fastRetry, func() (int, error) {
			calls++
			if calls < 2 {
				return 0, unavailable
			}
			return 1, nil
		})
		if reports.Load() != 1 {
			t.Errorf("progress reported %d times, want 1", reports.Load())
		}
	})
}

// TestTransientFailure pins which failures a second attempt can change.
func TestTransientFailure(t *testing.T) {
	status := func(code int) error { return &StatusError{Op: "commit-batch", Status: code} }
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"500", status(500), true},
		{"502", status(502), true},
		{"503", status(503), true},
		{"504", status(504), true},
		{"429", status(429), true},
		{"408", status(408), true},
		{"501 says the route is not served", status(501), false},
		{"400", status(400), false},
		{"401", status(401), false},
		{"403", status(403), false},
		{"404", status(404), false},
		{"409", status(409), false},
		{"413", status(413), false},
		{"a wrapped 503", fmt.Errorf("window: %w", status(503)), true},
		{"a network error", &url.Error{Op: "Post", URL: "http://cache.test", Err: io.EOF}, true},
		{"a dial failure", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},
		{"a truncated answer", io.ErrUnexpectedEOF, true},
		{"the attempt's own timeout", context.DeadlineExceeded, true},
		{"a canceled run", context.Canceled, false},
		{"a bearer that cannot be resolved", &bearerError{err: &net.OpError{Op: "dial", Err: errors.New("refused")}}, false},
		{"an answer the client cannot read", errors.New("invalid find-missing response"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := transientFailure(tt.err); got != tt.want {
				t.Errorf("transientFailure(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestStoreBatch_AFailedPutFailsOnlyTheEntriesOfItsBlob pins the second
// defense: a blob that cannot be uploaded costs the entries that name it, and
// the rest of the window is committed. Before, the first failed PUT canceled
// the others and dropped all the entries. The entries of the failed blob are
// never sent to commit-batch.
func TestStoreBatch_AFailedPutFailsOnlyTheEntriesOfItsBlob(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			src := mapBlobSource{}
			lost := []byte("blob the storage refuses")
			in := []StoreInput{
				entryOf(testKey(1), src, []byte("blob a")),
				entryOf(testKey(2), src, lost),
				entryOf(testKey(3), src, []byte("blob c")),
				entryOf(testKey(4), src, []byte("blob d"), lost), // shares the lost blob
			}
			f := newFakeCAS()
			f.always["/up/"+cache.DigestOf(lost)] = status
			srv := httptest.NewServer(f.handler(t))
			defer srv.Close()

			c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
			out, err := c.StoreBatch(context.Background(), in, src, nil, WritePath{Retry: fastRetry})
			if err != nil {
				t.Fatalf("StoreBatch: %v", err)
			}
			for _, i := range []int{0, 2} {
				if !out.Committed[in[i].Key] {
					t.Errorf("entry %d was not committed: it names no failed blob", i+1)
				}
			}
			for _, i := range []int{1, 3} {
				key := in[i].Key
				if out.Committed[key] {
					t.Errorf("entry %d is committed without its blob", i+1)
				}
				if cause := out.Failed[key]; cause == nil || !strings.Contains(cause.Error(), cache.DigestOf(lost)) {
					t.Errorf("entry %d: Failed = %v, want the failed blob named", i+1, cause)
				}
			}
			assertNoPartialCommit(t, f, in, out)

			f.mu.Lock()
			defer f.mu.Unlock()
			if f.nCommitBatch != 1 || f.commitBatch == nil {
				t.Fatalf("commit-batch requests = %d, want 1", f.nCommitBatch)
			}
			sent := make([]string, 0, len(f.commitBatch.Entries))
			for _, e := range f.commitBatch.Entries {
				sent = append(sent, e.Key)
			}
			want := []string{in[0].Key, in[2].Key}
			slices.Sort(sent)
			slices.Sort(want)
			if !slices.Equal(sent, want) {
				t.Errorf("commit-batch carried %d entries, want only the two whose blobs are stored", len(sent))
			}
		})
	}
}

// TestStoreBatch_AnUnreadableBlobFailsOnlyItsEntries is the same rule for a
// blob the client cannot read, on both blob paths.
func TestStoreBatch_AnUnreadableBlobFailsOnlyItsEntries(t *testing.T) {
	for _, coalesce := range []bool{false, true} {
		t.Run(fmt.Sprintf("coalesce=%v", coalesce), func(t *testing.T) {
			src := mapBlobSource{}
			gone := []byte("blob missing from the source")
			in := []StoreInput{
				entryOf(testKey(1), src, []byte("blob a")),
				entryOf(testKey(2), src, gone),
				entryOf(testKey(3), src, []byte("blob c")),
			}
			delete(src, cache.DigestOf(gone))
			f := newFakeCAS()
			srv := httptest.NewServer(f.handler(t))
			defer srv.Close()

			c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
			out, err := c.StoreBatch(context.Background(), in, src, nil, WritePath{CoalesceSmall: coalesce})
			if err != nil {
				t.Fatalf("StoreBatch: %v", err)
			}
			if !out.Committed[in[0].Key] || !out.Committed[in[2].Key] {
				t.Errorf("Committed = %v, want the two readable entries", out.Committed)
			}
			if out.Committed[in[1].Key] || out.Failed[in[1].Key] == nil {
				t.Errorf("the unreadable entry: committed=%v failed=%v", out.Committed[in[1].Key], out.Failed[in[1].Key])
			}
			assertNoPartialCommit(t, f, in, out)
		})
	}
}

// TestStoreBatch_SplitIsolatesTheEntryTheServerCannotTake pins the third
// defense: a window that still fails as a whole is stored entry by entry once,
// so the one entry the server refuses is the only one lost. No blob the window
// already stored is sent again.
func TestStoreBatch_SplitIsolatesTheEntryTheServerCannotTake(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		wantCommits int // commit-batch requests: the window's attempts, then one per entry
	}{
		{"a failing answer is retried, then split", http.StatusInternalServerError, 3 + 3},
		{"a verdict on the request is split at once", http.StatusRequestEntityTooLarge, 1 + 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in, src, digests := batchFixture()
			f := newFakeCAS()
			f.commitPoison[in[1].Key] = tt.status
			srv := httptest.NewServer(f.handler(t))
			defer srv.Close()

			policy := WriteRetry{Attempts: 3, Backoff: time.Millisecond, SplitWindow: true}
			c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
			out, err := c.StoreBatch(context.Background(), in, src, nil, WritePath{Retry: policy})
			if err != nil {
				t.Fatalf("StoreBatch: %v", err)
			}
			if !out.Committed[in[0].Key] || !out.Committed[in[2].Key] {
				t.Errorf("Committed = %v, want the two entries the server takes", out.Committed)
			}
			if out.Committed[in[1].Key] || !IsStatus(out.Failed[in[1].Key], tt.status) {
				t.Errorf("the refused entry: committed=%v failed=%v, want HTTP %d", out.Committed[in[1].Key], out.Failed[in[1].Key], tt.status)
			}
			if len(out.Failed) != 1 {
				t.Errorf("Failed holds %d entries, want 1", len(out.Failed))
			}
			if got := f.count(cache.CommitBatchPath); got != tt.wantCommits {
				t.Errorf("commit-batch received %d requests, want %d", got, tt.wantCommits)
			}
			if got := f.count("/up/"); got != len(digests) {
				t.Errorf("blob PUTs = %d, want %d: the split sends no blob again", got, len(digests))
			}
			if got := f.count(cache.FindMissingPath); got != 1 {
				t.Errorf("find-missing requests = %d, want 1: the split asks about no stored blob", got)
			}
			assertNoPartialCommit(t, f, in, out)
		})
	}
}

// TestStoreBatch_SplitStopsWhenTheServerKeepsFailing proves the split is
// bounded too: after three entries in a row failed for a transient reason the
// rest are not sent, and the window reports its own failure.
func TestStoreBatch_SplitStopsWhenTheServerKeepsFailing(t *testing.T) {
	src := mapBlobSource{}
	in := make([]StoreInput, 0, 8)
	for i := range cap(in) {
		in = append(in, entryOf(testKey(i), src, []byte(fmt.Sprintf("blob %d", i))))
	}
	f := newFakeCAS()
	f.always[cache.CommitBatchPath] = http.StatusServiceUnavailable
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	policy := WriteRetry{Attempts: 2, Backoff: time.Millisecond, SplitWindow: true}
	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	out, err := c.StoreBatch(context.Background(), in, src, nil, WritePath{Retry: policy})
	if !IsStatus(err, http.StatusServiceUnavailable) || out != nil {
		t.Fatalf("out = %+v, err = %v, want no outcome and HTTP 503", out, err)
	}
	// Two attempts for the window, then splitStopAfter entries sent once each.
	if got, want := f.count(cache.CommitBatchPath), policy.Attempts+splitStopAfter; got != want {
		t.Errorf("commit-batch received %d requests, want %d", got, want)
	}
}

// TestStoreBatch_SplitNeedsItsPolicyAndTwoEntries pins when no split happens:
// a caller without the policy, and a window of one entry, which is its own
// split.
func TestStoreBatch_SplitNeedsItsPolicyAndTwoEntries(t *testing.T) {
	tests := []struct {
		name    string
		entries int
		policy  WriteRetry
	}{
		{"no policy", 3, WriteRetry{}},
		{"one entry", 1, WriteRetry{SplitWindow: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in, src, _ := batchFixture()
			in = in[:tt.entries]
			f := newFakeCAS()
			f.always[cache.CommitBatchPath] = http.StatusInternalServerError
			srv := httptest.NewServer(f.handler(t))
			defer srv.Close()

			c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
			_, err := c.StoreBatch(context.Background(), in, src, nil, WritePath{Retry: tt.policy})
			if !IsStatus(err, http.StatusInternalServerError) {
				t.Fatalf("err = %v, want HTTP 500", err)
			}
			if got := f.count(cache.CommitBatchPath); got != 1 {
				t.Errorf("commit-batch received %d requests, want 1", got)
			}
		})
	}
}

// TestBlobPutTimeout_ScalesWithTheEntity pins the deadline's numbers: 60 s,
// plus one second per 256 KiB.
func TestBlobPutTimeout_ScalesWithTheEntity(t *testing.T) {
	c := NewClient("http://cache.test", "tok")
	tests := []struct {
		length int64
		want   time.Duration
	}{
		{0, 60 * time.Second},
		{-1, 60 * time.Second},
		{256 << 10, 61 * time.Second},
		{1 << 20, 64 * time.Second},
		{20 << 20, 140 * time.Second},
		{100 << 20, 460 * time.Second},
		{1 << 30, 4156 * time.Second},
	}
	for _, tt := range tests {
		if got := c.blobPutTimeout(tt.length); got != tt.want {
			t.Errorf("blobPutTimeout(%d) = %s, want %s", tt.length, got, tt.want)
		}
	}
}

// TestUploadBlob_EndsAtItsDeadline pins the deadline itself. Before it, a PUT
// to an endpoint that read the request and never answered did not return.
func TestUploadBlob_EndsAtItsDeadline(t *testing.T) {
	content := []byte("blob the storage never acknowledges")
	digest := cache.DigestOf(content)
	f := newFakeCAS()
	release := make(chan struct{})
	f.hold["/up/"+digest] = release
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	defer close(release)

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	c.putFloor, c.putBytesPerSecond = 100*time.Millisecond, 1<<30

	done := make(chan error, 1)
	go func() {
		done <- c.UploadBlob(context.Background(), cache.BlobTransfer{
			Digest: digest, URL: srv.URL + "/up/" + digest, Method: cache.TransferPut,
		}, bytes.NewReader(content), int64(len(content)))
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the PUT's deadline", err)
		}
		if !strings.Contains(err.Error(), "no answer within") {
			t.Errorf("err = %v, want it to name the deadline", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("UploadBlob did not return: a stalled PUT has no deadline")
	}
}

// TestUploadBlob_ACanceledRunIsNotReportedAsADeadline keeps the two causes
// apart in the log: the caller's cancellation is not a slow storage.
func TestUploadBlob_ACanceledRunIsNotReportedAsADeadline(t *testing.T) {
	content := []byte("blob of a run that ends")
	digest := cache.DigestOf(content)
	f := newFakeCAS()
	release := make(chan struct{})
	f.hold["/up/"+digest] = release
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	defer close(release)

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	err := c.UploadBlob(ctx, cache.BlobTransfer{
		Digest: digest, URL: srv.URL + "/up/" + digest, Method: cache.TransferPut,
	}, bytes.NewReader(content), int64(len(content)))
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "no answer within") {
		t.Fatalf("err = %v, want the cancellation, not a deadline", err)
	}
}

// TestStoreBatch_AStalledPutCostsOnlyItsEntries joins the deadline and the
// per-blob failure: the window returns, and it commits the entries whose
// blobs did store.
func TestStoreBatch_AStalledPutCostsOnlyItsEntries(t *testing.T) {
	src := mapBlobSource{}
	stalled := []byte("blob the storage never acknowledges")
	in := []StoreInput{
		entryOf(testKey(1), src, []byte("blob a")),
		entryOf(testKey(2), src, stalled),
		entryOf(testKey(3), src, []byte("blob c")),
	}
	f := newFakeCAS()
	release := make(chan struct{})
	f.hold["/up/"+cache.DigestOf(stalled)] = release
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	defer close(release)

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	c.putFloor, c.putBytesPerSecond = 100*time.Millisecond, 1<<30

	type result struct {
		out *BatchOutcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := c.StoreBatch(context.Background(), in, src, nil, WritePath{})
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("StoreBatch: %v", r.err)
		}
		if !r.out.Committed[in[0].Key] || !r.out.Committed[in[2].Key] {
			t.Errorf("Committed = %v, want the two entries whose blobs stored", r.out.Committed)
		}
		if cause := r.out.Failed[in[1].Key]; !errors.Is(cause, context.DeadlineExceeded) {
			t.Errorf("the stalled entry: Failed = %v, want the PUT's deadline", cause)
		}
		assertNoPartialCommit(t, f, in, r.out)
	case <-time.After(10 * time.Second):
		t.Fatal("StoreBatch did not return: a stalled PUT holds the window")
	}
}

// manyBlobEntry builds one entry of n distinct files. Their bytes are not kept:
// the tests that use it mark the blobs present on the server.
func manyBlobEntry(key string, n int) (StoreInput, []string) {
	files := make([]cache.FileEntry, n)
	digests := make([]string, n)
	for i := range files {
		digests[i] = cache.DigestOf([]byte(fmt.Sprintf("file %d", i)))
		files[i] = cache.FileEntry{Path: fmt.Sprintf("dist/%05d.js", i), Digest: digests[i], Mode: 0o644, Size: int64(len(fmt.Sprintf("file %d", i)))}
	}
	return StoreInput{
		Key:      key,
		Result:   &cache.ActionResult{Status: "success"},
		Manifest: &cache.Manifest{Files: files},
	}, digests
}

// TestStoreBatch_AnEntryWithMoreBlobsThanOneFindMissingTakes pins that such an
// entry is stored. One find-missing takes cache.MaxKeysPerRequest blobs;
// before, the client sent them all in one request, refused that request
// itself, and the entry could never be stored. The blobs are asked about in
// requests of at most the bound, and the missing ones are uploaded.
func TestStoreBatch_AnEntryWithMoreBlobsThanOneFindMissingTakes(t *testing.T) {
	const files = cache.MaxKeysPerRequest + 1
	in, digests := manyBlobEntry(testKey(1), files)
	// Two blobs are missing from the server.
	src := mapBlobSource{}
	missing := map[string]bool{}
	for _, i := range []int{7, files - 1} {
		src[digests[i]] = []byte(fmt.Sprintf("file %d", i))
		missing[digests[i]] = true
	}
	var present []string
	for _, d := range digests {
		if !missing[d] {
			present = append(present, d)
		}
	}
	f := newFakeCAS(present...)
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	out, err := c.StoreBatch(context.Background(), []StoreInput{in}, src, nil, WritePath{})
	if err != nil {
		t.Fatalf("StoreBatch: %v", err)
	}
	if !out.Committed[in.Key] {
		t.Fatalf("the entry of %d blobs was not committed: %v", files, out.Failed[in.Key])
	}
	if out.BlobsUploaded != len(missing) {
		t.Errorf("BlobsUploaded = %d, want %d", out.BlobsUploaded, len(missing))
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if want := []int{cache.MaxKeysPerRequest, 1}; !slices.Equal(f.findMissingBlobs, want) {
		t.Errorf("find-missing asked about %v blobs, want %v", f.findMissingBlobs, want)
	}
	if len(f.findMissingSeen) != files {
		t.Errorf("find-missing asked about %d distinct blobs, want all %d", len(f.findMissingSeen), files)
	}
}

// TestStoreBatch_CommitsInRequestsOfAtMostTheProtocolBound is the same rule
// for commit-batch, which takes cache.MaxKeysPerRequest entries.
func TestStoreBatch_CommitsInRequestsOfAtMostTheProtocolBound(t *testing.T) {
	const entries = cache.MaxKeysPerRequest + 1
	in := make([]StoreInput, entries)
	for i := range in {
		in[i] = StoreInput{Key: testKey(i), Result: &cache.ActionResult{Status: "success"}, Manifest: &cache.Manifest{}}
	}
	f := newFakeCAS()
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	out, err := c.StoreBatch(context.Background(), in, mapBlobSource{}, nil, WritePath{})
	if err != nil {
		t.Fatalf("StoreBatch: %v", err)
	}
	committed := 0
	for _, e := range in {
		if out.Committed[e.Key] {
			committed++
		}
	}
	if committed != entries {
		t.Errorf("committed %d entries, want %d", committed, entries)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if want := []int{cache.MaxKeysPerRequest, 1}; !slices.Equal(f.commitBatchEntries, want) {
		t.Errorf("commit-batch carried %v entries, want %v", f.commitBatchEntries, want)
	}
}

// TestStoreBatch_TheFirstWriteCarriesNoBlobBytes pins what a credential that
// may not write costs. The server answers its first write with a 403. Before,
// that first write was an upload-batch: a read-only run sent the bytes of its
// small blobs to learn that it may not write. Now the first write is a
// find-missing, and the refusing server receives no blob byte.
func TestStoreBatch_TheFirstWriteCarriesNoBlobBytes(t *testing.T) {
	src := mapBlobSource{}
	in := make([]StoreInput, 0, 4)
	blobBytes := 0
	for i := range cap(in) {
		content := bytes.Repeat([]byte{byte('a' + i)}, 64<<10)
		blobBytes += len(content)
		in = append(in, entryOf(testKey(i), src, content))
	}
	f := newFakeCAS()
	f.always[cache.FindMissingPath] = http.StatusForbidden
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	for window := 1; window <= 2; window++ {
		_, err := c.StoreBatch(context.Background(), in, src, nil, WritePath{CoalesceSmall: true, Retry: fastRetry})
		if !IsStatus(err, http.StatusForbidden) {
			t.Fatalf("window %d: err = %v, want HTTP 403", window, err)
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.requests[cache.UploadBatchPath] != 0 || f.requests["/up/"] != 0 || f.requests[cache.CommitBatchPath] != 0 {
		t.Errorf("a refusing server received upload-batch=%d PUT=%d commit-batch=%d, want none",
			f.requests[cache.UploadBatchPath], f.requests["/up/"], f.requests[cache.CommitBatchPath])
	}
	// One small request per refused window, and nothing else.
	if f.requests[cache.FindMissingPath] != 2 {
		t.Errorf("find-missing requests = %d, want one per window", f.requests[cache.FindMissingPath])
	}
	var total int64
	for _, n := range f.requestBytes {
		total += n
	}
	if total > 1024 {
		t.Errorf("the refusing server received %d bytes for %d blob bytes offered, want two requests of a few bytes", total, blobBytes)
	}
}

// TestStoreBatch_ProbesOncePerSession pins the cost of that order on a server
// that accepts writes: one find-missing with no blob, once. After the first
// accepted write a window opens with its upload-batch.
func TestStoreBatch_ProbesOncePerSession(t *testing.T) {
	f := newFakeCAS()
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	for window := range 3 {
		src := mapBlobSource{}
		in := []StoreInput{entryOf(testKey(window), src, []byte(fmt.Sprintf("window %d", window)))}
		out, err := c.StoreBatch(context.Background(), in, src, nil, WritePath{CoalesceSmall: true})
		if err != nil || !out.Committed[in[0].Key] {
			t.Fatalf("window %d: out = %+v, err = %v", window, out, err)
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if want := []int{0}; !slices.Equal(f.findMissingBlobs, want) {
		t.Errorf("find-missing requests asked about %v blobs, want one request about none", f.findMissingBlobs)
	}
	if f.nUploadBatch != 3 {
		t.Errorf("upload-batch requests = %d, want 3", f.nUploadBatch)
	}
}

// TestStoreBatch_AnAcceptedWriteOfAnyKindOpensTheSession proves the first
// write need not be the probe: a window with a blob over the inline bound asks
// about it in the find-missing it sends anyway, so it costs no extra request.
func TestStoreBatch_AnAcceptedWriteOfAnyKindOpensTheSession(t *testing.T) {
	f := newFakeCAS()
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	src := mapBlobSource{}
	large := bytes.Repeat([]byte("L"), cache.MaxInlineBlobBytes+1)
	in := []StoreInput{entryOf(testKey(1), src, large, []byte("small"))}
	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	out, err := c.StoreBatch(context.Background(), in, src, nil, WritePath{CoalesceSmall: true})
	if err != nil || !out.Committed[in[0].Key] {
		t.Fatalf("out = %+v, err = %v", out, err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if want := []int{1}; !slices.Equal(f.findMissingBlobs, want) {
		t.Errorf("find-missing requests asked about %v blobs, want one request about the large blob", f.findMissingBlobs)
	}
	if f.nUploadBatch != 1 {
		t.Errorf("upload-batch requests = %d, want 1", f.nUploadBatch)
	}
}

// TestStoreBatch_ANewBearerMakesTheNextWriteAFirstWrite pins that the verdict
// belongs to the bearer: a hosted run replaces the configured token with its
// own, and what the server accepted from the first says nothing about the
// second.
func TestStoreBatch_ANewBearerMakesTheNextWriteAFirstWrite(t *testing.T) {
	f := newFakeCAS()
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	c := NewClient(srv.URL, "configured-token", WithHTTPClient(srv.Client()))
	store := func(i int) {
		t.Helper()
		src := mapBlobSource{}
		in := []StoreInput{entryOf(testKey(i), src, []byte(fmt.Sprintf("blob %d", i)))}
		if _, err := c.StoreBatch(context.Background(), in, src, nil, WritePath{CoalesceSmall: true}); err != nil {
			t.Fatalf("StoreBatch %d: %v", i, err)
		}
	}
	store(1)
	c.UseRunBearer("run-token")
	store(2)
	store(3)

	f.mu.Lock()
	defer f.mu.Unlock()
	if want := []int{0, 0}; !slices.Equal(f.findMissingBlobs, want) {
		t.Errorf("find-missing requests asked about %v blobs, want one probe per bearer", f.findMissingBlobs)
	}
	if f.findMissingAuth != "Bearer run-token" {
		t.Errorf("the second probe carried %q, want the run's bearer", f.findMissingAuth)
	}
}

// TestStoreBatch_ConcurrentFirstWritesSendOneProbe proves the order holds when
// several callers open the session at once (the uploader and the object cache
// share one client): one sends the find-missing, the others wait for its
// answer, and no upload-batch leaves before it.
func TestStoreBatch_ConcurrentFirstWritesSendOneProbe(t *testing.T) {
	f := newFakeCAS()
	release := make(chan struct{})
	f.hold[cache.FindMissingPath] = release
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	const callers = 4
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			src := mapBlobSource{}
			in := []StoreInput{entryOf(testKey(i), src, []byte(fmt.Sprintf("caller %d", i)))}
			_, err := c.StoreBatch(context.Background(), in, src, nil, WritePath{CoalesceSmall: true})
			errs <- err
		}()
	}

	// Wait for the probe, then give the other callers time to send a request
	// they must not send.
	deadline := time.Now().Add(5 * time.Second)
	for f.count(cache.FindMissingPath) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	if probes, batches := f.count(cache.FindMissingPath), f.count(cache.UploadBatchPath); probes != 1 || batches != 0 {
		t.Errorf("while the first write is unanswered: find-missing=%d upload-batch=%d, want 1 and 0", probes, batches)
	}

	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("StoreBatch: %v", err)
		}
	}
	if probes, batches := f.count(cache.FindMissingPath), f.count(cache.UploadBatchPath); probes != 1 || batches != callers {
		t.Errorf("find-missing=%d upload-batch=%d, want 1 and %d", probes, batches, callers)
	}
}

// TestFirstWrite_AWaiterGivesUpWithItsContext keeps a caller from waiting
// forever behind a first write that never gets its answer.
func TestFirstWrite_AWaiterGivesUpWithItsContext(t *testing.T) {
	fw := newFirstWrite()
	probe, err := fw.enter(context.Background())
	if err != nil || !probe {
		t.Fatalf("the first caller: probe=%v err=%v, want the slot", probe, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if probe, err := fw.enter(ctx); probe || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a waiter whose context ended: probe=%v err=%v", probe, err)
	}
	// A refused first write leaves the next caller to ask again.
	fw.leave()
	if probe, err := fw.enter(context.Background()); err != nil || !probe {
		t.Fatalf("after a refused first write: probe=%v err=%v, want the slot", probe, err)
	}
	fw.accept()
	fw.leave()
	if probe, err := fw.enter(context.Background()); err != nil || probe {
		t.Fatalf("after an accepted write: probe=%v err=%v, want no probe", probe, err)
	}
}

// TestStoreObjects_TheFirstWriteCarriesNoBlobBytes is the object cache's side
// of the same rule: it shares the blob path, and its writes are refused on
// every run the cache write lease does not cover.
func TestStoreObjects_TheFirstWriteCarriesNoBlobBytes(t *testing.T) {
	f := newFakeCAS()
	f.always[cache.FindMissingPath] = http.StatusForbidden
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	content := bytes.Repeat([]byte("o"), 32<<10)
	digest := cache.DigestOf(content)
	src := mapBlobSource{digest: content}
	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	_, err := c.StoreObjects(context.Background(), "go-build", []ObjectOffer{
		{ID: strings.Repeat("1", 64), Digest: digest, SizeBytes: int64(len(content))},
	}, src, WritePath{CoalesceSmall: true})
	if !IsStatus(err, http.StatusForbidden) {
		t.Fatalf("err = %v, want HTTP 403", err)
	}
	if got := f.count(cache.UploadBatchPath) + f.count("/up/"); got != 0 {
		t.Errorf("the refusing server received %d requests with blob bytes, want none", got)
	}
}

// TestUploadProgress_ReportsEachStep proves a caller that waits for uploads
// can tell a moving upload from a stalled one: the write path reports the
// control exchanges it completes and the blob bytes it sends.
func TestUploadProgress_ReportsEachStep(t *testing.T) {
	f := newFakeCAS()
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	in, src, _ := batchFixture()
	var reports atomic.Int32
	ctx := WithUploadProgress(context.Background(), func() { reports.Add(1) })
	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	if _, err := c.StoreBatch(ctx, in, src, nil, WritePath{}); err != nil {
		t.Fatalf("StoreBatch: %v", err)
	}
	// One find-missing, one commit-batch, and for each of the three PUTs at
	// least a body read and the answer.
	if got := reports.Load(); got < 8 {
		t.Errorf("progress reported %d times, want at least 8", got)
	}
}

// TestTrackUpload pins the progress wrapper of a request entity: it reports on
// the first send and on a replay, keeps the replay possible, and leaves alone
// a request it has nothing to report for.
func TestTrackUpload(t *testing.T) {
	var reports atomic.Int32
	ctx := WithUploadProgress(context.Background(), func() { reports.Add(1) })

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://storage.test/blob", bytes.NewReader([]byte("entity")))
	if err != nil {
		t.Fatal(err)
	}
	trackUpload(req)
	if got, _ := io.ReadAll(req.Body); string(got) != "entity" || reports.Load() == 0 {
		t.Fatalf("first send read %q with %d reports", got, reports.Load())
	}
	if req.GetBody == nil {
		t.Fatal("the request lost its replay")
	}
	reports.Store(0)
	replay, err := req.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(replay); string(got) != "entity" || reports.Load() == 0 {
		t.Fatalf("replay read %q with %d reports", got, reports.Load())
	}

	empty, _ := http.NewRequestWithContext(ctx, http.MethodPut, "http://storage.test/blob", http.NoBody)
	trackUpload(empty)
	if empty.Body != http.NoBody {
		t.Error("an empty entity must stay http.NoBody")
	}

	plain, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, "http://storage.test/blob", bytes.NewReader([]byte("entity")))
	before := plain.Body
	trackUpload(plain)
	if plain.Body != before {
		t.Error("a request without a progress hook must not be wrapped")
	}
	if WithUploadProgress(context.Background(), nil) != context.Background() {
		t.Error("a nil hook must leave the context as it is")
	}
}

// TestBatchHitFetcher_AsksInRequestsOfAtMostTheProtocolBound is the read side
// of the per-request bound: a hit with more small files than one
// download-batch takes still gets them inline, in several requests. Before,
// the client refused its own oversized request and fetched every file with a
// GET of its own.
func TestBatchHitFetcher_AsksInRequestsOfAtMostTheProtocolBound(t *testing.T) {
	const files = cache.MaxKeysPerRequest + 1
	blobs := make(map[string][]byte, files)
	result := cache.KeyResult{Key: testKey(1), Hit: true}
	for i := range files {
		content := []byte(fmt.Sprintf("file %d", i))
		digest := cache.DigestOf(content)
		blobs[digest] = content
		result.Downloads = append(result.Downloads, cache.BlobTransfer{
			Digest: digest, URL: "http://storage.invalid/" + digest, Method: cache.TransferGet, SizeBytes: int64(len(content)),
		})
	}

	var mu sync.Mutex
	var asked []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != cache.DownloadBatchPath {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		req, diags := cache.ParseAndValidateDownloadBatchRequest(body)
		if req == nil {
			t.Errorf("server got an invalid download-batch request: %v", diags)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		asked = append(asked, len(req.Digests))
		mu.Unlock()
		resp := cache.DownloadBatchResponse{ProtocolVersion: cache.ProtocolVersion}
		for _, d := range req.Digests {
			resp.Blobs = append(resp.Blobs, cache.InlineBlob{Digest: d, Data: blobs[d]})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	fetch := c.BatchHitFetcher(context.Background(), result)
	for _, digest := range []string{result.Downloads[0].Digest, result.Downloads[files-1].Digest} {
		rc, err := fetch(digest)
		if err != nil {
			t.Fatalf("fetch %s: %v", digest, err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil || !bytes.Equal(got, blobs[digest]) {
			t.Fatalf("fetch %s read %q, %v", digest, got, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	slices.Sort(asked)
	if want := []int{1, cache.MaxKeysPerRequest}; !slices.Equal(asked, want) {
		t.Errorf("download-batch asked for %v blobs, want %v", asked, want)
	}
}
