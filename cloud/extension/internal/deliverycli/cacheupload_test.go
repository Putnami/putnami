package deliverycli

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/cloud/extension/internal/deliverycli/internal/remotecache"
	cache "go.putnami.dev/protocol/cache"
)

// uploadCacheServer is a cache server for the task-cache write path. It serves
// the batched exchanges (upload-batch when it advertises it, find-missing,
// presigned PUTs, commit-batch) and the per-entry ones (store, commit), and
// records what reached it, so a test can assert how many exchanges a run cost
// and not only that its entries landed.
type uploadCacheServer struct {
	srv *httptest.Server

	mu sync.Mutex
	// capabilities is what the probe advertises. Nil answers 404, the answer of
	// a server that predates the probe.
	capabilities []string
	// probeBearer, when set, is the only bearer the probe answers; every other
	// one gets 401.
	probeBearer string
	// probeDrops makes the probe close the connection without an answer.
	probeDrops bool
	// findMissingStatus, when non-zero, is the refusal answered to every
	// find-missing, and commitBatchStatus the failure answered to every
	// commit-batch.
	findMissingStatus int
	commitBatchStatus int
	// uploadBatchStatus, when non-zero, is answered to every upload-batch. A
	// server that does not advertise the route answers 404.
	uploadBatchStatus int
	// uploadBatchBlobs holds the blob count of each upload-batch request, and
	// findMissingDigests the digests of each find-missing, in arrival order.
	uploadBatchBlobs   []int
	findMissingDigests [][]string
	// uncommitted are the keys the server reports as not committed.
	uncommitted map[string]bool
	// blobs is the CAS: digest -> content received over a presigned PUT.
	blobs map[string][]byte

	// The fault knobs. findMissingFailures and commitBatchFailures make the
	// next requests of the route answer 503, that many times. poisoned are
	// keys the server fails a whole commit-batch for, with a 500. blobStatus
	// is the failure answered to the PUT of a digest. blobHold makes the PUT
	// of a digest wait, once its body is read, until the channel is closed or
	// the client goes away. commitDelay delays every commit-batch answer.
	findMissingFailures, commitBatchFailures int
	poisoned                                 map[string]bool
	blobStatus                               map[string]int
	blobHold                                 map[string]chan struct{}
	commitDelay                              time.Duration
	// committed are the keys a commit-batch answered as committed, and
	// blobPuts counts the PUTs received, the failed ones included.
	committed map[string]bool
	blobPuts  int

	probes, findMissings, stores, commits int
	// windows holds the keys of each commit-batch, in arrival order, and
	// commitBatchBodies its body verbatim.
	windows           [][]string
	commitBatchBodies [][]byte
	// windowSeen receives one value per commit-batch.
	windowSeen chan struct{}
}

func newUploadCacheServer(t *testing.T, capabilities ...string) *uploadCacheServer {
	t.Helper()
	f := &uploadCacheServer{
		capabilities: capabilities,
		uncommitted:  map[string]bool{},
		blobs:        map[string][]byte{},
		poisoned:     map[string]bool{},
		blobStatus:   map[string]int{},
		blobHold:     map[string]chan struct{}{},
		committed:    map[string]bool{},
		windowSeen:   make(chan struct{}, 64),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

// batchedCapabilities is what a cache server with the batched write path
// advertises.
func batchedCapabilities() []string {
	return []string{cache.CapabilityFindMissing, cache.CapabilityCommitBatch}
}

func (f *uploadCacheServer) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.URL.Path == cache.NegotiatePath:
		writeJSON(w, http.StatusOK, cache.NegotiateResponse{ProtocolVersion: cache.ProtocolVersion})
	case r.URL.Path == cache.CapabilitiesPath:
		f.probe(w, r)
	case r.URL.Path == cache.FindMissingPath:
		var req cache.FindMissingRequest
		_ = json.Unmarshal(body, &req)
		digests := make([]string, 0, len(req.Blobs))
		for _, b := range req.Blobs {
			digests = append(digests, b.Digest)
		}
		f.mu.Lock()
		f.findMissings++
		f.findMissingDigests = append(f.findMissingDigests, digests)
		uploads := f.missing(digests)
		status := f.findMissingStatus
		if status == 0 && f.findMissingFailures > 0 {
			f.findMissingFailures--
			status = http.StatusServiceUnavailable
		}
		f.mu.Unlock()
		if status != 0 {
			writeJSON(w, status, cache.ErrorResponse{
				ProtocolVersion: cache.ProtocolVersion,
				Code:            cache.CodeUnauthorized,
				Message:         "authentication with a workspace or organization scope is required",
			})
			return
		}
		writeJSON(w, http.StatusOK, cache.FindMissingResponse{ProtocolVersion: cache.ProtocolVersion, Uploads: uploads})
	case strings.HasPrefix(r.URL.Path, "/blob/"):
		digest := strings.TrimPrefix(r.URL.Path, "/blob/")
		f.mu.Lock()
		f.blobPuts++
		status, hold := f.blobStatus[digest], f.blobHold[digest]
		f.mu.Unlock()
		if hold != nil {
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
		}
		if status != 0 {
			http.Error(w, "the storage refused the blob", status)
			return
		}
		f.mu.Lock()
		f.blobs[digest] = storedContent(body)
		f.mu.Unlock()
	case r.URL.Path == cache.UploadBatchPath:
		f.uploadBatch(w, body)
	case r.URL.Path == cache.CommitBatchPath:
		f.commitBatch(w, body)
	case r.URL.Path == cache.StorePath:
		var req cache.StoreRequest
		_ = json.Unmarshal(body, &req)
		var digests []string
		if req.Manifest != nil {
			for _, file := range req.Manifest.Files {
				digests = append(digests, file.Digest)
			}
		}
		f.mu.Lock()
		f.stores++
		uploads := f.missing(digests)
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, cache.StoreResponse{ProtocolVersion: cache.ProtocolVersion, Key: req.Key, Uploads: uploads})
	case r.URL.Path == cache.CommitPath:
		var req cache.CommitRequest
		_ = json.Unmarshal(body, &req)
		f.mu.Lock()
		f.commits++
		committed := !f.uncommitted[req.Key]
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, cache.CommitResponse{ProtocolVersion: cache.ProtocolVersion, Key: req.Key, Committed: committed})
	default:
		http.NotFound(w, r)
	}
}

func (f *uploadCacheServer) probe(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.probes++
	capabilities := append([]string(nil), f.capabilities...)
	advertised := f.capabilities != nil
	bearer, drops := f.probeBearer, f.probeDrops
	f.mu.Unlock()
	switch {
	case drops:
		if hijacker, ok := w.(http.Hijacker); ok {
			if conn, _, err := hijacker.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
	case bearer != "" && r.Header.Get(cache.AuthorizationHeader) != "Bearer "+bearer:
		writeJSON(w, http.StatusUnauthorized, cache.ErrorResponse{
			ProtocolVersion: cache.ProtocolVersion,
			Code:            cache.CodeUnauthorized,
			Message:         "this credential does not read the cache",
		})
	case !advertised:
		http.NotFound(w, r)
	default:
		writeJSON(w, http.StatusOK, cache.CapabilitiesResponse{ProtocolVersion: cache.ProtocolVersion, Capabilities: capabilities})
	}
}

// uploadBatch stores the blobs a request carries inline and answers with every
// digest, as cache-server does.
func (f *uploadCacheServer) uploadBatch(w http.ResponseWriter, body []byte) {
	var req cache.UploadBatchRequest
	_ = json.Unmarshal(body, &req)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploadBatchBlobs = append(f.uploadBatchBlobs, len(req.Blobs))
	status := f.uploadBatchStatus
	if status == 0 && !slices.Contains(f.capabilities, cache.CapabilityUploadBatch) {
		status = http.StatusNotFound
	}
	if status != 0 {
		writeJSON(w, status, cache.ErrorResponse{
			ProtocolVersion: cache.ProtocolVersion,
			Code:            "refused",
			Message:         "upload-batch refused",
		})
		return
	}
	stored := make([]string, 0, len(req.Blobs))
	for _, b := range req.Blobs {
		f.blobs[b.Digest] = storedContent(b.Data)
		stored = append(stored, b.Digest)
	}
	slices.Sort(stored)
	writeJSON(w, http.StatusOK, cache.UploadBatchResponse{ProtocolVersion: cache.ProtocolVersion, Stored: stored})
}

func (f *uploadCacheServer) commitBatch(w http.ResponseWriter, body []byte) {
	var req cache.CommitBatchRequest
	_ = json.Unmarshal(body, &req)
	keys := make([]string, 0, len(req.Entries))
	results := make([]cache.CommitResult, 0, len(req.Entries))
	f.mu.Lock()
	status := f.commitBatchStatus
	if status == 0 && f.commitBatchFailures > 0 {
		f.commitBatchFailures--
		status = http.StatusServiceUnavailable
	}
	for _, entry := range req.Entries {
		keys = append(keys, entry.Key)
		results = append(results, cache.CommitResult{Key: entry.Key, Committed: !f.uncommitted[entry.Key]})
		if status == 0 && f.poisoned[entry.Key] {
			status = http.StatusInternalServerError
		}
	}
	if status == 0 {
		for _, result := range results {
			if result.Committed {
				f.committed[result.Key] = true
			}
		}
	}
	f.windows = append(f.windows, keys)
	f.commitBatchBodies = append(f.commitBatchBodies, body)
	delay := f.commitDelay
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if status != 0 {
		http.Error(w, "commit-batch is down", status)
	} else {
		writeJSON(w, http.StatusOK, cache.CommitBatchResponse{ProtocolVersion: cache.ProtocolVersion, Results: results})
	}
	f.windowSeen <- struct{}{}
}

// missing returns a presigned PUT for each digest the CAS does not hold. The
// caller holds f.mu.
func (f *uploadCacheServer) missing(digests []string) []cache.BlobTransfer {
	var uploads []cache.BlobTransfer
	seen := map[string]bool{}
	for _, digest := range digests {
		if _, have := f.blobs[digest]; have || seen[digest] {
			continue
		}
		seen[digest] = true
		uploads = append(uploads, cache.BlobTransfer{Digest: digest, URL: f.srv.URL + "/blob/" + digest, Method: cache.TransferPut})
	}
	return uploads
}

// storedContent returns the content of a presigned PUT body, which the client
// sends gzip-compressed when that shrinks it.
func storedContent(body []byte) []byte {
	if zr, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
		if content, err := io.ReadAll(zr); err == nil {
			return content
		}
	}
	return body
}

type uploadServerCounts struct {
	probes, findMissings, stores, commits int
	windows                               [][]string
}

func (f *uploadCacheServer) counts() uploadServerCounts {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := uploadServerCounts{probes: f.probes, findMissings: f.findMissings, stores: f.stores, commits: f.commits}
	for _, window := range f.windows {
		keys := append([]string(nil), window...)
		slices.Sort(keys)
		c.windows = append(c.windows, keys)
	}
	return c
}

// awaitWindow waits for the next commit-batch to reach the server.
func (f *uploadCacheServer) awaitWindow(t *testing.T) {
	t.Helper()
	select {
	case <-f.windowSeen:
	case <-time.After(20 * time.Second):
		t.Fatal("no commit-batch reached the cache server")
	}
}

// pinUploadIdle fixes the idle delay for the sessions a test creates after the
// call, and restores it afterwards.
func pinUploadIdle(t *testing.T, d time.Duration) {
	t.Helper()
	prev := uploadWindowIdle
	uploadWindowIdle = d
	t.Cleanup(func() { uploadWindowIdle = prev })
}

// pinUploadRetryWait keeps the production retry policy, without the wait
// between two attempts, for the sessions a test creates after the call.
func pinUploadRetryWait(t *testing.T) {
	t.Helper()
	prev := uploadRetry
	uploadRetry.Backoff = time.Millisecond
	t.Cleanup(func() { uploadRetry = prev })
}

// startUploadSession opens a writing provider session against f and returns it
// with its blob-exchange directory.
func startUploadSession(t *testing.T, f *uploadCacheServer, logw io.Writer) (*providerSession, string) {
	t.Helper()
	pinUploadRetryWait(t)
	enableCacheWritesForTest(t)
	t.Setenv("PUTNAMI_CACHE_URL", f.srv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", "ci-token")
	t.Setenv(SessionReporterIngestURLEnv, "")
	exchangeDir := t.TempDir()
	s := newProviderSession(t.TempDir(), logw)
	t.Cleanup(s.close)
	mustInit(t, s, &cache.InitializeParams{ProtocolVersion: cache.ProviderProtocolMinVersion, BlobExchangeDir: exchangeDir})
	return s, exchangeDir
}

// uploadKey returns the i-th distinct cache key of a test.
func uploadKey(i int) string {
	return fmt.Sprintf("%0*x", cache.KeyLength, i+1)
}

// stageBlob writes content into the blob-exchange directory, as core does
// before it sends the upload, and returns the manifest entry that names it.
func stageBlob(t *testing.T, exchangeDir, content string) cache.FileEntry {
	t.Helper()
	digest := cache.DigestOf([]byte(content))
	path, ok := cache.BlobExchangePath(exchangeDir, digest)
	if !ok {
		t.Fatalf("invalid digest %s", digest)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return cache.FileEntry{Path: "out/" + digest[len(digest)-8:], Digest: digest, Size: int64(len(content))}
}

// mustUpload sends one upload op and requires the provider to accept it.
func mustUpload(t *testing.T, s *providerSession, key string, files ...cache.FileEntry) {
	t.Helper()
	resp := s.upload(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 2, Op: cache.OpUpload},
		&cache.UploadParams{Key: key, Result: &cache.ActionResult{Status: "success"}, Manifest: &cache.Manifest{Files: files}})
	result, diags := cache.ParseAndValidateUploadResult(resp.Payload)
	if result == nil {
		t.Fatalf("invalid upload result: %v", diags)
	}
	if !result.Accepted {
		t.Fatalf("upload %s was not accepted", short(key))
	}
}

// mustSummary sends the summary op and returns the run's statistics.
func mustSummary(t *testing.T, s *providerSession) *cache.SummaryResult {
	t.Helper()
	resp := s.summary(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 9, Op: cache.OpSummary})
	if !resp.OK {
		t.Fatalf("summary = %+v", resp)
	}
	stats, diags := cache.ParseAndValidateSummaryResult(resp.Payload)
	if stats == nil {
		t.Fatalf("invalid summary result: %v", diags)
	}
	return stats
}

// A run's entries are stored a window at a time: one probe for the session,
// then one find-missing and one commit-batch for the whole window, with each
// distinct blob read from the exchange directory and uploaded once. The idle
// delay is an hour here, so the summary's drain is what flushes the window.
func TestUploadsStoreAWindowWithOneFindMissingAndOneCommitBatch(t *testing.T) {
	pinUploadIdle(t, time.Hour)
	f := newUploadCacheServer(t, batchedCapabilities()...)
	s, exchangeDir := startUploadSession(t, f, io.Discard)

	first := stageBlob(t, exchangeDir, "first output")
	second := stageBlob(t, exchangeDir, "second output")
	shared := stageBlob(t, exchangeDir, "an output two entries share")
	mustUpload(t, s, uploadKey(0), first)
	mustUpload(t, s, uploadKey(1), second, shared)
	mustUpload(t, s, uploadKey(2), shared)

	if c := f.counts(); c.findMissings != 0 || len(c.windows) != 0 || c.stores != 0 {
		t.Fatalf("a partial window was sent before its idle delay or a drain: %+v", c)
	}

	stats := mustSummary(t, s)
	wantBytes := first.Size + second.Size + shared.Size
	if stats.UploadedCount != 3 || stats.UploadedBytes != wantBytes {
		t.Fatalf("summary = %d entries / %d bytes, want 3 / %d", stats.UploadedCount, stats.UploadedBytes, wantBytes)
	}

	c := f.counts()
	if c.probes != 1 || c.findMissings != 1 || len(c.windows) != 1 {
		t.Fatalf("exchanges = %d probe(s), %d find-missing, %d commit-batch; want one of each", c.probes, c.findMissings, len(c.windows))
	}
	if c.stores != 0 || c.commits != 0 {
		t.Fatalf("the batched path sent %d store and %d commit; want none", c.stores, c.commits)
	}
	if want := []string{uploadKey(0), uploadKey(1), uploadKey(2)}; !slices.Equal(c.windows[0], want) {
		t.Fatalf("commit-batch keys = %v, want %v", c.windows[0], want)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, file := range []cache.FileEntry{first, second, shared} {
		if got := cache.DigestOf(f.blobs[file.Digest]); got != file.Digest {
			t.Errorf("blob %s was not uploaded from the exchange directory", file.Digest)
		}
	}
	if len(f.blobs) != 3 {
		t.Errorf("uploaded blobs = %d, want the 3 distinct ones", len(f.blobs))
	}
}

// packedCapabilities is what a cache server that also packs small blobs
// advertises.
func packedCapabilities() []string {
	return append(batchedCapabilities(), cache.CapabilityUploadBatch)
}

// stageLargeBlob stages a blob over the inline bound, which upload-batch does
// not carry.
func stageLargeBlob(t *testing.T, exchangeDir string) cache.FileEntry {
	t.Helper()
	return stageBlob(t, exchangeDir, strings.Repeat("L", cache.MaxInlineBlobBytes+1))
}

// A window sends its small blobs inline when the server advertises
// upload-batch: one request for all of them, and find-missing is asked only
// about the blobs too large to send inline. Each small blob used to cost a
// signed URL and a PUT of its own.
func TestAWindowSendsItsSmallBlobsInOneUploadBatch(t *testing.T) {
	pinUploadIdle(t, time.Hour)
	f := newUploadCacheServer(t, packedCapabilities()...)
	s, exchangeDir := startUploadSession(t, f, io.Discard)

	first := stageBlob(t, exchangeDir, "first output")
	second := stageBlob(t, exchangeDir, "second output")
	shared := stageBlob(t, exchangeDir, "an output two entries share")
	large := stageLargeBlob(t, exchangeDir)
	mustUpload(t, s, uploadKey(0), first, large)
	mustUpload(t, s, uploadKey(1), second, shared)
	mustUpload(t, s, uploadKey(2), shared)

	stats := mustSummary(t, s)
	wantBytes := first.Size + second.Size + shared.Size + large.Size
	if stats.UploadedCount != 3 || stats.UploadedBytes != wantBytes {
		t.Fatalf("summary = %d entries / %d bytes, want 3 / %d", stats.UploadedCount, stats.UploadedBytes, wantBytes)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Equal(f.uploadBatchBlobs, []int{3}) {
		t.Fatalf("upload-batch requests carried %v blobs, want one request with the 3 small ones", f.uploadBatchBlobs)
	}
	if len(f.findMissingDigests) != 1 || !slices.Equal(f.findMissingDigests[0], []string{large.Digest}) {
		t.Fatalf("find-missing was asked about %v, want one request for the large blob only", f.findMissingDigests)
	}
	if len(f.windows) != 1 {
		t.Fatalf("commit-batch requests = %d, want 1", len(f.windows))
	}
	for _, file := range []cache.FileEntry{first, second, shared, large} {
		if got := cache.DigestOf(f.blobs[file.Digest]); got != file.Digest {
			t.Errorf("blob %s was not stored", file.Digest)
		}
	}
}

// A window loses nothing when the server advertised upload-batch and answers
// that it does not serve it: the window asks once, sends every blob through
// find-missing and a presigned PUT, and commits every entry. The write path
// stays open for the next window. The large blob is asked about before
// upload-batch, in the write that opens the session; the small ones after the
// route answered that it is not served.
func TestAWindowFallsBackWhenUploadBatchIsNotServed(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			pinUploadIdle(t, time.Hour)
			f := newUploadCacheServer(t, packedCapabilities()...)
			f.uploadBatchStatus = status
			var logw lockedWriter
			s, exchangeDir := startUploadSession(t, f, &logw)

			first := stageBlob(t, exchangeDir, "first output")
			second := stageBlob(t, exchangeDir, "second output")
			large := stageLargeBlob(t, exchangeDir)
			mustUpload(t, s, uploadKey(0), first, large)
			mustUpload(t, s, uploadKey(1), second)

			stats := mustSummary(t, s)
			if wantBytes := first.Size + second.Size + large.Size; stats.UploadedCount != 2 || stats.UploadedBytes != wantBytes {
				t.Fatalf("summary = %d entries / %d bytes, want 2 / %d", stats.UploadedCount, stats.UploadedBytes, wantBytes)
			}
			if s.writesLatched() {
				t.Fatal("an unserved upload-batch closed the write path")
			}
			if log := logw.String(); strings.Contains(log, "upload window") || strings.Contains(log, "auth refused") {
				t.Fatalf("the fallback must not be reported as a failed window, got %q", log)
			}

			f.mu.Lock()
			defer f.mu.Unlock()
			if !slices.Equal(f.uploadBatchBlobs, []int{2}) {
				t.Fatalf("upload-batch requests carried %v blobs, want one request with the 2 small ones", f.uploadBatchBlobs)
			}
			if len(f.findMissingDigests) != 2 || !slices.Equal(f.findMissingDigests[0], []string{large.Digest}) || len(f.findMissingDigests[1]) != 2 {
				t.Fatalf("find-missing was asked about %v, want the large blob, then the 2 small ones", f.findMissingDigests)
			}
			for _, file := range []cache.FileEntry{first, second, large} {
				if got := cache.DigestOf(f.blobs[file.Digest]); got != file.Digest {
					t.Errorf("blob %s was not stored", file.Digest)
				}
			}
		})
	}
}

// A window whose upload-batch the server keeps failing is a failed window,
// like one whose commit-batch fails: nothing is counted, nothing is sent
// through the other path, and the run goes on. The request is sent 3 times. The
// one find-missing is the write that opens the session, which asks about no
// blob.
func TestAFailedUploadBatchFailsItsWindow(t *testing.T) {
	f := newUploadCacheServer(t, packedCapabilities()...)
	f.uploadBatchStatus = http.StatusInternalServerError
	var logw lockedWriter
	s, exchangeDir := startUploadSession(t, f, &logw)
	mustUpload(t, s, uploadKey(0), stageBlob(t, exchangeDir, "output"))

	if stats := mustSummary(t, s); stats.UploadedCount != 0 || stats.UploadedBytes != 0 {
		t.Fatalf("summary counted %d entries / %d bytes after a failed window", stats.UploadedCount, stats.UploadedBytes)
	}
	if !strings.Contains(logw.String(), "upload window (1 entries)") {
		t.Fatalf("a failed window must be said on the provider log, got %q", logw.String())
	}
	if c := f.counts(); c.findMissings != 1 || len(c.windows) != 0 {
		t.Fatalf("exchanges = %d find-missing, %d commit-batch; want 1 and 0 after a failed upload-batch", c.findMissings, len(c.windows))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.findMissingDigests[0]) != 0 || f.blobPuts != 0 {
		t.Fatalf("the per-blob path was used after a failed upload-batch: find-missing about %v, %d PUT", f.findMissingDigests, f.blobPuts)
	}
	if !slices.Equal(f.uploadBatchBlobs, []int{1, 1, 1}) {
		t.Fatalf("upload-batch requests carried %v blobs, want the request sent 3 times", f.uploadBatchBlobs)
	}
}

// The uploader asks for the bound a window needs: one key's default would make
// one uploader trail the build.
func TestTheUploaderRaisesTheBlobBoundForAWindow(t *testing.T) {
	f := newUploadCacheServer(t, batchedCapabilities()...)
	s, _ := startUploadSession(t, f, io.Discard)
	wp, batched := s.taskWritePath()
	if !batched {
		t.Fatal("a server with find-missing and commit-batch must select the batched path")
	}
	if wp.BlobConcurrency != uploadWindowBlobConcurrency || wp.Grant != nil || wp.CoalesceSmall {
		t.Fatalf("write path = %+v, want only BlobConcurrency=%d", wp, uploadWindowBlobConcurrency)
	}
}

// A commit-batch carries what core built and nothing a caller asserted about
// who built it: the server derives the producer and the channel from the bearer.
func TestUploadWindowDoesNotForwardAssertedProvenance(t *testing.T) {
	f := newUploadCacheServer(t, batchedCapabilities()...)
	s, exchangeDir := startUploadSession(t, f, io.Discard)

	resp := s.upload(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 2, Op: cache.OpUpload}, &cache.UploadParams{
		Key:      uploadKey(0),
		Result:   &cache.ActionResult{Status: "success"},
		Manifest: &cache.Manifest{Files: []cache.FileEntry{stageBlob(t, exchangeDir, "output")}},
		Producer: cache.ProducerCI, ProducerIdentity: "forged-ci", Channel: cache.ChannelTrusted,
	})
	if !resp.OK {
		t.Fatalf("upload = %+v", resp)
	}
	mustSummary(t, s)

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.commitBatchBodies) != 1 {
		t.Fatalf("commit-batch requests = %d, want 1", len(f.commitBatchBodies))
	}
	body := string(f.commitBatchBodies[0])
	for _, field := range []string{"producer", "producerIdentity", "channel", "forged-ci"} {
		if strings.Contains(body, field) {
			t.Errorf("commit-batch forwarded untrusted %q: %s", field, body)
		}
	}
}

// A full window is stored at once, without waiting for the idle delay or the
// summary, and the entries after it wait for the next window.
func TestAFullUploadWindowFlushesBeforeTheSummary(t *testing.T) {
	pinUploadIdle(t, time.Hour)
	f := newUploadCacheServer(t, batchedCapabilities()...)
	s, exchangeDir := startUploadSession(t, f, io.Discard)
	output := stageBlob(t, exchangeDir, "the same output for every entry")

	for i := range uploadWindowEntries {
		mustUpload(t, s, uploadKey(i), output)
	}
	f.awaitWindow(t)
	if c := f.counts(); len(c.windows) != 1 || len(c.windows[0]) != uploadWindowEntries {
		t.Fatalf("windows = %d, first carries %d entries; want one full window of %d", len(c.windows), len(c.windows[0]), uploadWindowEntries)
	}

	mustUpload(t, s, uploadKey(uploadWindowEntries), output)
	stats := mustSummary(t, s)
	if stats.UploadedCount != uploadWindowEntries+1 {
		t.Fatalf("summary counted %d entries, want %d", stats.UploadedCount, uploadWindowEntries+1)
	}
	c := f.counts()
	if len(c.windows) != 2 || len(c.windows[1]) != 1 {
		t.Fatalf("windows = %v, want the full one and a second with the last entry", c.windows)
	}
	if c.probes != 1 {
		t.Fatalf("capabilities probes = %d, want 1 for the session", c.probes)
	}
}

// A partial window is stored once no entry has arrived for the idle delay, and
// an entry that arrives after the uploader went idle starts it again.
func TestAnIdleUploadWindowFlushesBeforeTheSummary(t *testing.T) {
	pinUploadIdle(t, 10*time.Millisecond)
	f := newUploadCacheServer(t, batchedCapabilities()...)
	s, exchangeDir := startUploadSession(t, f, io.Discard)
	output := stageBlob(t, exchangeDir, "output")

	mustUpload(t, s, uploadKey(0), output)
	f.awaitWindow(t)
	mustUpload(t, s, uploadKey(1), output)
	f.awaitWindow(t)

	c := f.counts()
	if want := [][]string{{uploadKey(0)}, {uploadKey(1)}}; len(c.windows) != 2 || !slices.Equal(c.windows[0], want[0]) || !slices.Equal(c.windows[1], want[1]) {
		t.Fatalf("windows = %v, want %v", c.windows, want)
	}
	if c.probes != 1 {
		t.Fatalf("capabilities probes = %d, want 1 for the session", c.probes)
	}
	if stats := mustSummary(t, s); stats.UploadedCount != 2 {
		t.Fatalf("summary counted %d entries, want 2", stats.UploadedCount)
	}
}

// A server without find-missing or commit-batch gets one store and one commit
// per entry, as before windows.
func TestUploadsFallBackToOneStoreAndOneCommitPerEntry(t *testing.T) {
	for _, test := range []struct {
		name         string
		capabilities []string
	}{
		{name: "no capabilities route"},
		{name: "find-missing without commit-batch", capabilities: []string{cache.CapabilityFindMissing}},
		{name: "commit-batch without find-missing", capabilities: []string{cache.CapabilityCommitBatch}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newUploadCacheServer(t, test.capabilities...)
			s, exchangeDir := startUploadSession(t, f, io.Discard)
			files := []cache.FileEntry{
				stageBlob(t, exchangeDir, "output 0"),
				stageBlob(t, exchangeDir, "output 1"),
				stageBlob(t, exchangeDir, "output 2"),
			}
			for i, file := range files {
				mustUpload(t, s, uploadKey(i), file)
			}

			stats := mustSummary(t, s)
			wantBytes := files[0].Size + files[1].Size + files[2].Size
			if stats.UploadedCount != 3 || stats.UploadedBytes != wantBytes {
				t.Fatalf("summary = %d entries / %d bytes, want 3 / %d", stats.UploadedCount, stats.UploadedBytes, wantBytes)
			}
			c := f.counts()
			if c.stores != 3 || c.commits != 3 {
				t.Fatalf("per-entry exchanges = %d store, %d commit; want 3 of each", c.stores, c.commits)
			}
			if c.findMissings != 0 || len(c.windows) != 0 {
				t.Fatalf("the per-entry path sent %d find-missing and %d commit-batch; want none", c.findMissings, len(c.windows))
			}
			if c.probes != 1 {
				t.Fatalf("capabilities probes = %d, want 1 for the session", c.probes)
			}
		})
	}
}

// An entry the server did not commit is not durable, so the summary does not
// count it, on either write path.
func TestOnlyCommittedEntriesAreCountedAsUploaded(t *testing.T) {
	for _, test := range []struct {
		name         string
		capabilities []string
		said         string
	}{
		{name: "batched", capabilities: batchedCapabilities(), said: "the cache server committed 2 of 3 entries"},
		{name: "per entry", said: "upload " + short(uploadKey(1)) + ": the cache server did not commit the entry"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newUploadCacheServer(t, test.capabilities...)
			f.uncommitted[uploadKey(1)] = true
			var logw lockedWriter
			s, exchangeDir := startUploadSession(t, f, &logw)
			output := stageBlob(t, exchangeDir, "output")
			for i := range 3 {
				mustUpload(t, s, uploadKey(i), output)
			}

			if stats := mustSummary(t, s); stats.UploadedCount != 2 {
				t.Fatalf("summary counted %d entries, want the 2 the server committed", stats.UploadedCount)
			}
			if !strings.Contains(logw.String(), test.said) {
				t.Fatalf("the entry that did not commit must be said, got %q", logw.String())
			}
		})
	}
}

// A key addresses one entry, and commit-batch answers one result per key, so a
// key queued twice in a window is committed once.
func TestAKeyQueuedTwiceIsCommittedOncePerWindow(t *testing.T) {
	pinUploadIdle(t, time.Hour)
	f := newUploadCacheServer(t, batchedCapabilities()...)
	s, exchangeDir := startUploadSession(t, f, io.Discard)
	output := stageBlob(t, exchangeDir, "output")
	mustUpload(t, s, uploadKey(0), output)
	mustUpload(t, s, uploadKey(1), output)
	mustUpload(t, s, uploadKey(0), output)

	if stats := mustSummary(t, s); stats.UploadedCount != 2 {
		t.Fatalf("summary counted %d entries, want 2", stats.UploadedCount)
	}
	if c := f.counts(); len(c.windows) != 1 || !slices.Equal(c.windows[0], []string{uploadKey(0), uploadKey(1)}) {
		t.Fatalf("windows = %v, want one with each key once", c.windows)
	}
}

// A window that fails is logged and the run goes on: the summary answers, no
// entry is counted, and the session keeps serving.
func TestAFailedUploadWindowNeverChangesTheRunVerdict(t *testing.T) {
	f := newUploadCacheServer(t, batchedCapabilities()...)
	f.commitBatchStatus = http.StatusInternalServerError
	var logw lockedWriter
	s, exchangeDir := startUploadSession(t, f, &logw)
	mustUpload(t, s, uploadKey(0), stageBlob(t, exchangeDir, "output"))

	stats := mustSummary(t, s)
	if stats.UploadedCount != 0 || stats.UploadedBytes != 0 {
		t.Fatalf("summary counted %d entries / %d bytes after a failed window", stats.UploadedCount, stats.UploadedBytes)
	}
	if !strings.Contains(logw.String(), "upload window (1 entries)") {
		t.Fatalf("a failed window must be said on the provider log, got %q", logw.String())
	}
	restore := s.restore(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 4, Op: cache.OpRestore},
		&cache.RestoreParams{Key: uploadKey(0)})
	if !restore.OK {
		t.Fatalf("restore after a failed window = %+v, want a normal answer", restore)
	}
}

// A run whose windows are all refused persists nothing, so the summary says so
// as loudly as it does for a refused store: the refused exchange is
// named with its auth source, and the cold-cache warning is printed.
func TestARefusedUploadWindowIsReportedAtSummary(t *testing.T) {
	f := newUploadCacheServer(t, batchedCapabilities()...)
	f.findMissingStatus = http.StatusUnauthorized
	var logw lockedWriter
	s, exchangeDir := startUploadSession(t, f, &logw)
	mustUpload(t, s, uploadKey(0), stageBlob(t, exchangeDir, "output"))

	if stats := mustSummary(t, s); stats.UploadedCount != 0 {
		t.Fatalf("summary counted %d entries after a refused window", stats.UploadedCount)
	}
	log := logw.String()
	for _, want := range []string{
		"auth refused:", "op=find-missing", "status=401",
		"source=" + string(remotecache.TokenClassEnv),
		"1 of them on the write path",
		"next run starts cold",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("the summary of a refused window must say %q, got %q", want, log)
		}
	}
	if s.writesLatched() {
		t.Error("a 401 latched the write path; only a 403 is a verdict on the credential's scope")
	}
	if strings.Contains(log, "ci-token") {
		t.Errorf("the provider log leaked the cache bearer: %q", log)
	}
}

// A probe that gets no answer selects the per-entry path, which every server
// serves, and is said once for the session, not once per window.
func TestAnUnansweredProbeSelectsThePerEntryPath(t *testing.T) {
	f := newUploadCacheServer(t, batchedCapabilities()...)
	f.probeDrops = true
	var logw lockedWriter
	s, exchangeDir := startUploadSession(t, f, &logw)
	output := stageBlob(t, exchangeDir, "output")

	mustUpload(t, s, uploadKey(0), output)
	if stats := mustSummary(t, s); stats.UploadedCount != 1 {
		t.Fatalf("summary counted %d entries, want 1", stats.UploadedCount)
	}
	mustUpload(t, s, uploadKey(1), output)
	if stats := mustSummary(t, s); stats.UploadedCount != 2 {
		t.Fatalf("summary counted %d entries, want 2", stats.UploadedCount)
	}

	c := f.counts()
	if c.stores != 2 || c.commits != 2 || len(c.windows) != 0 {
		t.Fatalf("exchanges = %d store, %d commit, %d commit-batch; want the per-entry path for both windows", c.stores, c.commits, len(c.windows))
	}
	if got := strings.Count(logw.String(), "upload capabilities:"); got != 1 {
		t.Fatalf("the unanswered probe was said %d times, want once: %q", got, logw.String())
	}
}

// The object cache probes the server at initialize; the uploader reuses that
// answer, so a session costs one capabilities call.
func TestOneCapabilitiesProbeServesTheObjectCacheAndTheUploader(t *testing.T) {
	f := newUploadCacheServer(t, cache.CapabilityFindMissing, cache.CapabilityCommitBatch, cache.CapabilityObjectCache)
	enableCacheWritesForTest(t)
	t.Setenv("PUTNAMI_CACHE_URL", f.srv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", "ci-token")
	t.Setenv(SessionReporterIngestURLEnv, "")
	exchangeDir := shortExchangeDir(t)
	s := newProviderSession(t.TempDir(), io.Discard)
	t.Cleanup(s.close)
	result := mustInit(t, s, &cache.InitializeParams{
		ProtocolVersion: cache.ProviderProtocolMinVersion,
		BlobExchangeDir: exchangeDir,
		Capabilities:    coreCapabilities(),
	})
	if result.ObjectCacheSocket == "" {
		t.Fatal("precondition: the object cache was not negotiated")
	}
	if c := f.counts(); c.probes != 1 {
		t.Fatalf("capabilities probes at initialize = %d, want 1", c.probes)
	}

	mustUpload(t, s, uploadKey(0), stageBlob(t, exchangeDir, "output"))
	if stats := mustSummary(t, s); stats.UploadedCount != 1 {
		t.Fatalf("summary counted %d entries, want 1", stats.UploadedCount)
	}
	c := f.counts()
	if c.probes != 1 {
		t.Fatalf("capabilities probes = %d, want the one initialize made", c.probes)
	}
	if len(c.windows) != 1 || c.stores != 0 {
		t.Fatalf("the uploader did not take the batched path the probe advertised: %+v", c)
	}
}

// Initialize probes with the configured token. A hosted run replaces it with
// its own cache token at authenticate, so an answer the configured token was
// refused is not reused: the run's token probes again and gets the batched path.
func TestTheRunsCacheTokenProbesAgain(t *testing.T) {
	f := newUploadCacheServer(t, cache.CapabilityFindMissing, cache.CapabilityCommitBatch, cache.CapabilityObjectCache)
	f.probeBearer = testRunCacheToken
	delivery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{"protocolVersion": 1, "token": testRunCacheToken, "expiresAt": "2026-10-10T06:00:00Z"})
	}))
	t.Cleanup(delivery.Close)
	enableCacheWritesForTest(t)
	t.Setenv("PUTNAMI_CACHE_URL", f.srv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", testConfiguredToken)
	t.Setenv(SessionReporterIngestURLEnv, delivery.URL+"/v1/delivery/records")
	stubCacheProviderGuards(t, nil)

	exchangeDir := shortExchangeDir(t)
	s := newProviderSession(t.TempDir(), io.Discard)
	t.Cleanup(s.close)
	result := mustInit(t, s, &cache.InitializeParams{
		ProtocolVersion: cache.ProviderProtocolMinVersion,
		BlobExchangeDir: exchangeDir,
		Capabilities:    append(coreCapabilities(), cache.CapabilityRunCredential),
	})
	if result.ObjectCacheSocket != "" {
		t.Fatal("precondition: the configured token's probe was refused, so no object cache is negotiated")
	}
	auth := s.authenticate(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolMinVersion, ID: 2, Op: cache.OpAuthenticate},
		&cache.AuthenticateParams{Credential: testRunCredential})
	if !auth.OK {
		t.Fatalf("authenticate = %+v", auth.Error)
	}

	mustUpload(t, s, uploadKey(0), stageBlob(t, exchangeDir, "output"))
	if stats := mustSummary(t, s); stats.UploadedCount != 1 {
		t.Fatalf("summary counted %d entries, want 1", stats.UploadedCount)
	}
	c := f.counts()
	if c.probes != 2 {
		t.Fatalf("capabilities probes = %d, want one per bearer", c.probes)
	}
	if len(c.windows) != 1 || c.stores != 0 {
		t.Fatalf("the run's token did not get the batched path: %+v", c)
	}
}

// Shutdown stores the queued entries before it is acknowledged, without
// waiting for the idle delay: a run that ends without a summary keeps them.
func TestShutdownStoresTheQueuedUploads(t *testing.T) {
	pinUploadIdle(t, time.Hour)
	f := newUploadCacheServer(t, batchedCapabilities()...)
	enableCacheWritesForTest(t)
	t.Setenv("PUTNAMI_CACHE_URL", f.srv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", "ci-token")
	t.Setenv(SessionReporterIngestURLEnv, "")
	exchangeDir := t.TempDir()
	output := stageBlob(t, exchangeDir, "output")
	manifest, err := json.Marshal(cache.Manifest{Files: []cache.FileEntry{output}})
	if err != nil {
		t.Fatal(err)
	}
	reqs := []string{
		`{"protocolVersion":1,"id":1,"op":"initialize","payload":{"protocolVersion":1,"blobExchangeDir":` + strconv.Quote(exchangeDir) + `}}`,
		`{"protocolVersion":1,"id":2,"op":"upload","payload":{"key":"` + uploadKey(0) + `","result":{"status":"success"},"manifest":` + string(manifest) + `}}`,
		`{"protocolVersion":1,"id":3,"op":"shutdown"}`,
	}

	var out, logw bytes.Buffer
	if err := runCacheProviderSession(strings.NewReader(strings.Join(reqs, "\n")+"\n"), &out, &logw, t.TempDir()); err != nil {
		t.Fatalf("session: %v (log: %s)", err, logw.String())
	}
	resps := decodeResponses(t, &out)
	if len(resps) != len(reqs) || resps[len(resps)-1].ID != 3 {
		t.Fatalf("responses = %d, want %d ending with the shutdown ack (log: %s)", len(resps), len(reqs), logw.String())
	}
	if c := f.counts(); len(c.windows) != 1 || !slices.Equal(c.windows[0], []string{uploadKey(0)}) {
		t.Fatalf("windows = %v, want the queued entry stored before the shutdown ack", c.windows)
	}
}

// A refused write closes the write path for the windows already queued too: the
// run sends the one refused exchange and nothing after it.
func TestARefusedWindowDropsTheQueuedOnesUnsent(t *testing.T) {
	pinUploadIdle(t, time.Hour)
	enableCacheWritesForTest(t)
	srv, writes := writeRefusingCacheServer(t)
	t.Setenv("PUTNAMI_CACHE_URL", srv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", "developer-token")
	t.Setenv(SessionReporterIngestURLEnv, "")
	var logw lockedWriter
	s := newProviderSession(t.TempDir(), &logw)
	t.Cleanup(s.close)
	mustInit(t, s, &cache.InitializeParams{ProtocolVersion: cache.ProviderProtocolMinVersion, BlobExchangeDir: t.TempDir()})

	for i := range 2*uploadWindowEntries + 5 {
		s.upload(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 2, Op: cache.OpUpload}, latchTestUpload(uploadKey(i)))
	}
	if stats := mustSummary(t, s); stats.UploadedCount != 0 {
		t.Fatalf("summary counted %d entries after every write was refused", stats.UploadedCount)
	}
	if !s.writesLatched() {
		t.Fatal("a 403 on a window did not latch the write path")
	}
	if got := writes.Load(); got != 1 {
		t.Fatalf("write requests = %d, want exactly the one refused exchange", got)
	}
	if strings.Contains(logw.String(), "upload window") {
		t.Fatalf("the refusal that latched was logged again as a failed window: %q", logw.String())
	}
}

// uploadWindowSize cuts a window at the entry count, at one find-missing's
// blob limit, and at the encoded size that fits one commit-batch body. An entry
// too large for any window still goes, alone.
func TestUploadWindowSize(t *testing.T) {
	entry := func(bytes int, digests ...string) pendingUpload {
		manifest := &cache.Manifest{}
		for _, digest := range digests {
			manifest.Files = append(manifest.Files, cache.FileEntry{Path: "out", Digest: digest, Size: 1})
		}
		return pendingUpload{in: remotecache.StoreInput{Manifest: manifest}, bytes: bytes}
	}
	digests := func(from, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = cache.DigestOf([]byte(strconv.Itoa(from + i)))
		}
		return out
	}
	repeat := func(n int, p pendingUpload) []pendingUpload {
		out := make([]pendingUpload, n)
		for i := range out {
			out[i] = p
		}
		return out
	}

	for _, test := range []struct {
		name  string
		queue []pendingUpload
		n     int
		full  bool
	}{
		{name: "empty queue", queue: nil, n: 0},
		{name: "partial window", queue: repeat(3, entry(100, digests(0, 1)...)), n: 3},
		{name: "exactly one window", queue: repeat(uploadWindowEntries, entry(100)), n: uploadWindowEntries, full: true},
		{name: "more than one window", queue: repeat(uploadWindowEntries+7, entry(100)), n: uploadWindowEntries, full: true},
		{
			name: "blob limit",
			queue: []pendingUpload{
				entry(100, digests(0, uploadWindowBlobs-1)...),
				entry(100, digests(0, 1)...), // already counted: still fits
				entry(100, digests(uploadWindowBlobs, 2)...),
			},
			n: 2, full: true,
		},
		{
			name:  "encoded size",
			queue: []pendingUpload{entry(uploadWindowBytes / 2), entry(uploadWindowBytes / 2), entry(1)},
			n:     2, full: true,
		},
		{
			name:  "an oversized entry goes alone",
			queue: []pendingUpload{entry(2 * uploadWindowBytes), entry(1)},
			n:     1, full: true,
		},
		{
			name:  "an entry with a nil manifest",
			queue: []pendingUpload{{in: remotecache.StoreInput{}, bytes: 10}},
			n:     1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			n, full := uploadWindowSize(test.queue)
			if n != test.n || full != test.full {
				t.Fatalf("uploadWindowSize = (%d, %t), want (%d, %t)", n, full, test.n, test.full)
			}
		})
	}
}

// A find-missing or a commit-batch that fails for a reason that can pass is
// sent again, and the window loses nothing. Before the retry, one 503 dropped
// every entry of the window.
func TestAWindowIsSentAgainWhenTheServerFailsForAMoment(t *testing.T) {
	for _, test := range []struct {
		name string
		fail func(f *uploadCacheServer)
	}{
		{name: "find-missing", fail: func(f *uploadCacheServer) { f.findMissingFailures = 2 }},
		{name: "commit-batch", fail: func(f *uploadCacheServer) { f.commitBatchFailures = 2 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			pinUploadIdle(t, time.Hour)
			f := newUploadCacheServer(t, batchedCapabilities()...)
			test.fail(f)
			var logw lockedWriter
			s, exchangeDir := startUploadSession(t, f, &logw)
			for i := range 3 {
				mustUpload(t, s, uploadKey(i), stageBlob(t, exchangeDir, fmt.Sprintf("output %d", i)))
			}

			if stats := mustSummary(t, s); stats.UploadedCount != 3 {
				t.Fatalf("summary counted %d entries, want all 3 (log: %s)", stats.UploadedCount, logw.String())
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.committed) != 3 || len(f.blobs) != 3 {
				t.Fatalf("committed = %d entries with %d blobs, want 3 and 3", len(f.committed), len(f.blobs))
			}
			if got := f.findMissings + len(f.windows); got != 4 {
				t.Fatalf("find-missing + commit-batch requests = %d, want 4: two failures, then one of each", got)
			}
			if strings.Contains(logw.String(), "upload window") {
				t.Fatalf("a window that stored after a retry must not be reported, got %q", logw.String())
			}
		})
	}
}

// A blob the storage refuses costs the entries that name it and no other: the
// rest of the window is committed and counted. The entry that lost its blob is
// never sent to commit-batch. Before, the first failed PUT dropped the window.
func TestAFailedBlobCostsOnlyTheEntriesThatNameIt(t *testing.T) {
	pinUploadIdle(t, time.Hour)
	f := newUploadCacheServer(t, batchedCapabilities()...)
	var logw lockedWriter
	s, exchangeDir := startUploadSession(t, f, &logw)
	first := stageBlob(t, exchangeDir, "first output")
	lost := stageBlob(t, exchangeDir, "an output the storage refuses")
	third := stageBlob(t, exchangeDir, "third output")
	f.blobStatus[lost.Digest] = http.StatusInternalServerError
	mustUpload(t, s, uploadKey(0), first)
	mustUpload(t, s, uploadKey(1), lost)
	mustUpload(t, s, uploadKey(2), third)
	mustUpload(t, s, uploadKey(3), third, lost)

	stats := mustSummary(t, s)
	if stats.UploadedCount != 2 || stats.UploadedBytes != first.Size+third.Size {
		t.Fatalf("summary = %d entries / %d bytes, want the 2 whose blobs stored / %d", stats.UploadedCount, stats.UploadedBytes, first.Size+third.Size)
	}
	log := logw.String()
	if !strings.Contains(log, "upload window: 2 of 4 entries are not stored; first failure, entry "+short(uploadKey(1))) {
		t.Fatalf("the entries that lost a blob must be said, got %q", log)
	}
	if strings.Contains(log, "the cache server committed") {
		t.Fatalf("an entry that was never sent is not one the server refused, got %q", log)
	}

	c := f.counts()
	if want := []string{uploadKey(0), uploadKey(2)}; len(c.windows) != 1 || !slices.Equal(c.windows[0], want) {
		t.Fatalf("commit-batch keys = %v, want one request with %v", c.windows, want)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, stored := f.blobs[lost.Digest]; stored || f.committed[uploadKey(1)] || f.committed[uploadKey(3)] {
		t.Fatal("an entry was committed without its blob")
	}
}

// A window that still fails as a whole is stored entry by entry, once: the one
// entry the server cannot take is the only one lost.
func TestAWindowTheServerCannotTakeIsStoredEntryByEntry(t *testing.T) {
	pinUploadIdle(t, time.Hour)
	f := newUploadCacheServer(t, batchedCapabilities()...)
	f.poisoned[uploadKey(1)] = true
	var logw lockedWriter
	s, exchangeDir := startUploadSession(t, f, &logw)
	for i := range 3 {
		mustUpload(t, s, uploadKey(i), stageBlob(t, exchangeDir, fmt.Sprintf("output %d", i)))
	}

	if stats := mustSummary(t, s); stats.UploadedCount != 2 {
		t.Fatalf("summary counted %d entries, want the 2 the server takes (log: %s)", stats.UploadedCount, logw.String())
	}
	if log := logw.String(); !strings.Contains(log, "upload window: 1 of 3 entries are not stored; first failure, entry "+short(uploadKey(1))) {
		t.Fatalf("the entry the server cannot take must be said, got %q", log)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.committed[uploadKey(0)] || f.committed[uploadKey(1)] || !f.committed[uploadKey(2)] {
		t.Fatalf("committed = %v, want every entry but the refused one", f.committed)
	}
	// Three attempts for the window, then each entry once. No blob is sent twice.
	if len(f.windows) != 6 || f.blobPuts != 3 || f.findMissings != 1 {
		t.Fatalf("commit-batch=%d PUT=%d find-missing=%d, want 6, 3 and 1", len(f.windows), f.blobPuts, f.findMissings)
	}
}

// An entry with more files than one find-missing takes is stored like any
// other: it goes alone, and its blobs are asked about in several requests.
// Before, the client refused its own request, and such an entry was never
// stored.
func TestAnEntryWithMoreBlobsThanOneFindMissingTakesIsStored(t *testing.T) {
	pinUploadIdle(t, time.Hour)
	f := newUploadCacheServer(t, batchedCapabilities()...)
	var logw lockedWriter
	s, exchangeDir := startUploadSession(t, f, &logw)

	// The server already holds every blob but one, so only that one is read
	// from the exchange directory.
	const blobs = cache.MaxKeysPerRequest + 1
	files := make([]cache.FileEntry, 0, blobs)
	files = append(files, stageBlob(t, exchangeDir, "the one output the server lacks"))
	for i := 1; i < blobs; i++ {
		content := []byte(fmt.Sprintf("output %d", i))
		digest := cache.DigestOf(content)
		f.blobs[digest] = content
		files = append(files, cache.FileEntry{Path: fmt.Sprintf("out/%05d", i), Digest: digest, Size: int64(len(content))})
	}
	mustUpload(t, s, uploadKey(0), files...)
	mustUpload(t, s, uploadKey(1), stageBlob(t, exchangeDir, "another entry"))

	if stats := mustSummary(t, s); stats.UploadedCount != 2 {
		t.Fatalf("summary counted %d entries, want 2 (log: %s)", stats.UploadedCount, logw.String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	asked := make([]int, 0, len(f.findMissingDigests))
	for _, digests := range f.findMissingDigests {
		asked = append(asked, len(digests))
	}
	// The large entry alone, in two requests, then the next window.
	if want := []int{cache.MaxKeysPerRequest, 1, 1}; !slices.Equal(asked, want) {
		t.Fatalf("find-missing requests asked about %v blobs, want %v", asked, want)
	}
	if !f.committed[uploadKey(0)] || !f.committed[uploadKey(1)] {
		t.Fatalf("committed = %v, want both entries", f.committed)
	}
}

// A run whose credential may not write learns it from a request that carries
// no blob. Before, a server that packs small blobs got upload-batch as the
// first write, so every read-only run uploaded its first window's small blobs
// to be told 403.
func TestAReadOnlyRunSendsNoBlobBytes(t *testing.T) {
	pinUploadIdle(t, time.Hour)
	pinUploadRetryWait(t)
	enableCacheWritesForTest(t)
	var mu sync.Mutex
	requests := map[string]int{}
	var writeBytes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case cache.NegotiatePath:
			writeJSON(w, http.StatusOK, cache.NegotiateResponse{ProtocolVersion: cache.ProtocolVersion})
		case cache.CapabilitiesPath:
			writeJSON(w, http.StatusOK, cache.CapabilitiesResponse{ProtocolVersion: cache.ProtocolVersion, Capabilities: packedCapabilities()})
		default:
			mu.Lock()
			requests[r.URL.Path]++
			writeBytes += len(body)
			mu.Unlock()
			writeJSON(w, http.StatusForbidden, cache.ErrorResponse{
				ProtocolVersion: cache.ProtocolVersion, Code: "forbidden", Message: "this credential reads the cache; only CI writes",
			})
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("PUTNAMI_CACHE_URL", srv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", "developer-token")
	t.Setenv(SessionReporterIngestURLEnv, "")
	var logw lockedWriter
	s := newProviderSession(t.TempDir(), &logw)
	t.Cleanup(s.close)
	exchangeDir := t.TempDir()
	mustInit(t, s, &cache.InitializeParams{ProtocolVersion: cache.ProviderProtocolMinVersion, BlobExchangeDir: exchangeDir})

	// More than two windows of small blobs, 64 KiB each. Once the refusal
	// closes the write path, the provider declines the uploads that follow.
	staged, accepted := 0, 0
	for i := range 2*uploadWindowEntries + 5 {
		file := stageBlob(t, exchangeDir, strings.Repeat(fmt.Sprintf("%04d", i), 16<<10))
		staged += int(file.Size)
		resp := s.upload(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 2, Op: cache.OpUpload},
			&cache.UploadParams{Key: uploadKey(i), Result: &cache.ActionResult{Status: "success"}, Manifest: &cache.Manifest{Files: []cache.FileEntry{file}}})
		if result, _ := cache.ParseAndValidateUploadResult(resp.Payload); result != nil && result.Accepted {
			accepted++
		}
	}
	if accepted < uploadWindowEntries {
		t.Fatalf("the provider accepted %d uploads, want at least the %d of a first window", accepted, uploadWindowEntries)
	}
	if stats := mustSummary(t, s); stats.UploadedCount != 0 {
		t.Fatalf("summary counted %d entries after every write was refused", stats.UploadedCount)
	}
	if !s.writesLatched() {
		t.Fatal("the refused write did not latch the write path")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 1 || requests[cache.FindMissingPath] != 1 {
		t.Fatalf("write requests = %v, want one find-missing and nothing else", requests)
	}
	if writeBytes > 512 {
		t.Fatalf("the refusing server received %d bytes of the %d staged, want one request of a few bytes", writeBytes, staged)
	}
	if log := logw.String(); !strings.Contains(log, "op=find-missing") || !strings.Contains(log, "this credential reads the shared cache and writes nothing to it") {
		t.Fatalf("the refusal must be named and read as the developer one, got %q", log)
	}
}
