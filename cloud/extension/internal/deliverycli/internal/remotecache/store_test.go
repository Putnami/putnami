package remotecache

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	cache "go.putnami.dev/protocol/cache"
)

// mapBlobSource serves blob bytes by digest from an in-memory map.
type mapBlobSource map[string][]byte

func (m mapBlobSource) OpenBlob(digest string) (io.ReadCloser, error) {
	b, ok := m[digest]
	if !ok {
		return nil, fmt.Errorf("no blob for %s", digest)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// casIngest mimics how a CAS consumer recovers a blob's logical content: a
// gzip-compressed upload is decompressed, a raw upload is returned as-is. It
// returns whether the body arrived gzipped so tests can assert compression.
func casIngest(body []byte) (content []byte, gzipped bool) {
	if len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b {
		if zr, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
			if dec, err := io.ReadAll(zr); err == nil {
				return dec, true
			}
		}
	}
	return body, false
}

// fakeCAS is an httptest stand-in for the cache server + object storage: it
// answers store/commit and accepts presigned PUT uploads, recording what it
// saw so tests can assert the round trip.
type fakeCAS struct {
	mu          sync.Mutex
	present     map[string]bool   // digests already in CAS (deduped away)
	uploaded    map[string][]byte // digest -> bytes received over PUT
	commit      *cache.CommitRequest
	commitBatch *cache.CommitBatchRequest
	storeAuth   string
	commitAuth  string
	uploadAuth  string
	capsAuth    string
	// capabilities advertised at CapabilitiesPath; caps404 makes the probe 404
	// (an older, baseline-only server). capsStatus, when non-zero, makes the probe
	// reply with that HTTP status plus a cache.ErrorResponse body — e.g. 403 for a
	// workspace machine token lacking cache.read at introspection. The
	// client must degrade any non-2xx probe to the baseline (empty capabilities,
	// no error), matching the handler's "fall back on any non-2xx" contract.
	capabilities []string
	caps404      bool
	capsStatus   int
	// control-plane round-trip counters so tests can assert batching collapses
	// per-job store/commit into per-window find-missing/commit-batch.
	nStore, nCommit, nFindMissing, nCommitBatch, nCapabilities int
	// Phase-2 direct-write counters.
	nUploadGrant, nDirectPut, nUploadBatch int
	directPutAuth                          string
	// nGzipUploads counts uploads (find-missing or direct) whose body arrived
	// gzip-compressed, so tests can assert transport compression engaged.
	nGzipUploads int
	// findMissingSeen accumulates every digest find-missing was asked about, so
	// tests can assert known-present blobs are excluded from the probe.
	findMissingSeen map[string]bool
}

func newFakeCAS(present ...string) *fakeCAS {
	f := &fakeCAS{present: map[string]bool{}, uploaded: map[string][]byte{}, findMissingSeen: map[string]bool{}}
	for _, d := range present {
		f.present[d] = true
	}
	return f
}

func (f *fakeCAS) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc(cache.StorePath, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.nStore++
		f.storeAuth = r.Header.Get(cache.AuthorizationHeader)
		f.mu.Unlock()

		body, _ := io.ReadAll(r.Body)
		req, diags := cache.ParseAndValidateStoreRequest(body)
		if req == nil {
			t.Errorf("server got invalid store request: %v", diags)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		var uploads []cache.BlobTransfer
		for _, file := range req.Manifest.Files {
			f.mu.Lock()
			have := f.present[file.Digest]
			f.mu.Unlock()
			if have {
				continue // dedup: already in CAS, no upload needed
			}
			uploads = append(uploads, cache.BlobTransfer{
				Digest:  file.Digest,
				URL:     "http://" + r.Host + "/up/" + file.Digest,
				Method:  cache.TransferPut,
				Headers: map[string]string{"Content-Type": "application/octet-stream"},
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cache.StoreResponse{
			ProtocolVersion: cache.ProtocolVersion, Key: req.Key, Uploads: uploads,
		})
	})

	mux.HandleFunc("/up/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("upload method = %s, want PUT", r.Method)
		}
		f.mu.Lock()
		f.uploadAuth = r.Header.Get(cache.AuthorizationHeader)
		f.mu.Unlock()

		digest := strings.TrimPrefix(r.URL.Path, "/up/")
		raw, _ := io.ReadAll(r.Body)
		content, gzipped := casIngest(raw)
		if got := cache.DigestOf(content); got != digest {
			t.Errorf("uploaded bytes for %s content-address to %s", digest, got)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.uploaded[digest] = content
		f.present[digest] = true
		if gzipped {
			f.nGzipUploads++
		}
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc(cache.CommitPath, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.nCommit++
		f.commitAuth = r.Header.Get(cache.AuthorizationHeader)
		f.mu.Unlock()

		body, _ := io.ReadAll(r.Body)
		req, diags := cache.ParseAndValidateCommitRequest(body)
		if req == nil {
			t.Errorf("server got invalid commit request: %v", diags)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// Every referenced blob must be in CAS before the entry is committed.
		for _, file := range req.Manifest.Files {
			f.mu.Lock()
			have := f.present[file.Digest]
			f.mu.Unlock()
			if !have {
				t.Errorf("commit before blob %s was uploaded", file.Digest)
				w.WriteHeader(http.StatusConflict)
				return
			}
		}
		f.mu.Lock()
		f.commit = req
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cache.CommitResponse{
			ProtocolVersion: cache.ProtocolVersion, Key: req.Key, Committed: true,
		})
	})

	mux.HandleFunc(cache.CapabilitiesPath, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.nCapabilities++
		f.capsAuth = r.Header.Get(cache.AuthorizationHeader)
		caps404 := f.caps404
		capsStatus := f.capsStatus
		capsv := append([]string(nil), f.capabilities...)
		f.mu.Unlock()

		if caps404 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if capsStatus != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(capsStatus)
			_ = json.NewEncoder(w).Encode(cache.ErrorResponse{
				ProtocolVersion: cache.ProtocolVersion,
				Code:            cache.CodeUnauthorized,
				Message:         "cache token lacks cache.read",
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cache.CapabilitiesResponse{
			ProtocolVersion: cache.ProtocolVersion, Capabilities: capsv,
		})
	})

	mux.HandleFunc(cache.FindMissingPath, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.nFindMissing++
		f.mu.Unlock()

		body, _ := io.ReadAll(r.Body)
		req, diags := cache.ParseAndValidateFindMissingRequest(body)
		if req == nil {
			t.Errorf("server got invalid find-missing request: %v", diags)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		var uploads []cache.BlobTransfer
		for _, b := range req.Blobs {
			f.mu.Lock()
			f.findMissingSeen[b.Digest] = true
			have := f.present[b.Digest]
			f.mu.Unlock()
			if have {
				continue // dedup: already in CAS across the whole window
			}
			uploads = append(uploads, cache.BlobTransfer{
				Digest:  b.Digest,
				URL:     "http://" + r.Host + "/up/" + b.Digest,
				Method:  cache.TransferPut,
				Headers: map[string]string{"Content-Type": "application/octet-stream"},
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cache.FindMissingResponse{
			ProtocolVersion: cache.ProtocolVersion, Uploads: uploads,
		})
	})

	mux.HandleFunc(cache.CommitBatchPath, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.nCommitBatch++
		f.commitAuth = r.Header.Get(cache.AuthorizationHeader)
		f.mu.Unlock()

		body, _ := io.ReadAll(r.Body)
		req, diags := cache.ParseAndValidateCommitBatchRequest(body)
		if req == nil {
			t.Errorf("server got invalid commit-batch request: %v", diags)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var results []cache.CommitResult
		for _, e := range req.Entries {
			// An entry is servable only once every blob it references is in CAS;
			// otherwise it reports committed:false (per the protocol contract), so a
			// client that optimistically skipped an evicted blob can recover.
			committed := true
			for _, file := range e.Manifest.Files {
				f.mu.Lock()
				have := f.present[file.Digest]
				f.mu.Unlock()
				if !have {
					committed = false
					break
				}
			}
			results = append(results, cache.CommitResult{Key: e.Key, Committed: committed})
		}
		f.mu.Lock()
		f.commitBatch = req
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cache.CommitBatchResponse{
			ProtocolVersion: cache.ProtocolVersion, Results: results,
		})
	})

	// Phase-2 direct write: issue a grant pointing at the conditional-create
	// /dput/ endpoint below, and accept conditional PUTs there.
	mux.HandleFunc(cache.UploadGrantPath, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.nUploadGrant++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cache.UploadGrant{
			ProtocolVersion:         cache.ProtocolVersion,
			URLTemplate:             "http://" + r.Host + "/dput/" + cache.DigestPlaceholder,
			Method:                  cache.TransferPut,
			Headers:                 map[string]string{"x-test-conditional": "create"},
			ConditionalExistsStatus: http.StatusPreconditionFailed,
		})
	})

	mux.HandleFunc("/dput/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("direct upload method = %s, want PUT", r.Method)
		}
		f.mu.Lock()
		f.directPutAuth = r.Header.Get(cache.AuthorizationHeader)
		f.mu.Unlock()

		digest := cache.DigestAlgorithm + ":" + strings.TrimPrefix(r.URL.Path, "/dput/")
		raw, _ := io.ReadAll(r.Body)
		content, gzipped := casIngest(raw)
		if got := cache.DigestOf(content); got != digest {
			t.Errorf("direct-uploaded bytes for %s content-address to %s", digest, got)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.present[digest] {
			w.WriteHeader(http.StatusPreconditionFailed) // conditional create: already exists
			return
		}
		f.uploaded[digest] = content
		f.present[digest] = true
		if gzipped {
			f.nGzipUploads++
		}
		f.nDirectPut++
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc(cache.UploadBatchPath, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.nUploadBatch++
		f.mu.Unlock()

		body, _ := io.ReadAll(r.Body)
		req, diags := cache.ParseAndValidateUploadBatchRequest(body)
		if req == nil {
			t.Errorf("server got invalid upload-batch request: %v", diags)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var stored []string
		for _, b := range req.Blobs {
			content, gzipped := casIngest(b.Data)
			if got := cache.DigestOf(content); got != b.Digest {
				t.Errorf("inline blob %s content-addresses to %s", b.Digest, got)
				continue // server validates and omits a mismatch from Stored
			}
			f.mu.Lock()
			f.uploaded[b.Digest] = content
			f.present[b.Digest] = true
			if gzipped {
				f.nGzipUploads++
			}
			f.mu.Unlock()
			stored = append(stored, b.Digest)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cache.UploadBatchResponse{
			ProtocolVersion: cache.ProtocolVersion, Stored: stored,
		})
	})

	return mux
}

// storeFixture builds a one-file store input plus a blob source that can serve
// its bytes.
func storeFixture() (key string, content []byte, in StoreInput, src mapBlobSource) {
	key = strings.Repeat("a", cache.KeyLength)
	content = []byte("freshly built artifact bytes")
	digest := cache.DigestOf(content)
	src = mapBlobSource{digest: content}
	in = StoreInput{
		Key:    key,
		Result: &cache.ActionResult{Status: "success", DurationMs: 1000, SizeBytes: int64(len(content))},
		Manifest: &cache.Manifest{Files: []cache.FileEntry{
			{Path: "dist/index.js", Digest: digest, Mode: 0o644, Size: int64(len(content))},
		}},
	}
	return key, content, in, src
}

func TestStoreResult_UploadsThenCommits(t *testing.T) {
	f := newFakeCAS()
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	key, content, in, src := storeFixture()
	digest := cache.DigestOf(content)

	c := NewClient(srv.URL, "secret-token", WithHTTPClient(srv.Client()))
	resp, err := c.StoreResult(context.Background(), in, src)
	if err != nil {
		t.Fatalf("StoreResult: %v", err)
	}
	if !resp.Commit.Committed || resp.Commit.Key != key {
		t.Errorf("commit response = %+v, want committed for %s", resp.Commit, key)
	}
	if resp.BlobsUploaded != 1 || resp.BytesUploaded != int64(len(content)) {
		t.Errorf("upload outcome = %d blobs / %d bytes, want 1 / %d", resp.BlobsUploaded, resp.BytesUploaded, len(content))
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if !bytes.Equal(f.uploaded[digest], content) {
		t.Errorf("uploaded bytes for %s = %q, want %q", digest, f.uploaded[digest], content)
	}
	if f.commit == nil || f.commit.Result == nil || f.commit.Result.Status != "success" {
		t.Errorf("commit did not record the result: %+v", f.commit)
	}
	if f.storeAuth != "Bearer secret-token" || f.commitAuth != "Bearer secret-token" {
		t.Errorf("store/commit must carry the bearer token: store=%q commit=%q", f.storeAuth, f.commitAuth)
	}
	if f.uploadAuth != "" {
		t.Errorf("presigned upload must NOT carry the bearer token, got %q", f.uploadAuth)
	}
}

func TestStoreResult_FullDedupSkipsUpload(t *testing.T) {
	_, content, in, src := storeFixture()
	digest := cache.DigestOf(content)
	f := newFakeCAS(digest) // already present → server asks for no uploads
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	resp, err := c.StoreResult(context.Background(), in, src)
	if err != nil {
		t.Fatalf("StoreResult: %v", err)
	}
	if !resp.Commit.Committed {
		t.Error("expected commit on full dedup")
	}
	if resp.BlobsUploaded != 0 || resp.BytesUploaded != 0 {
		t.Errorf("full dedup should upload nothing, got %d blobs / %d bytes", resp.BlobsUploaded, resp.BytesUploaded)
	}
	if resp.BytesDeduped != int64(len(content)) {
		t.Errorf("full dedup should count %d deduped bytes, got %d", len(content), resp.BytesDeduped)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.uploaded) != 0 {
		t.Errorf("expected no uploads on full dedup, got %d", len(f.uploaded))
	}
	if f.commit == nil {
		t.Error("expected a commit even when nothing was uploaded")
	}
}

func TestStoreResult_UploadsManyBlobsConcurrently(t *testing.T) {
	// A manifest with several files: one shared blob is already in CAS (dedup),
	// the rest must upload. Exercises the concurrent blob-upload path and the
	// uploaded-vs-deduped byte split.
	const n = 12
	files := make([]cache.FileEntry, 0, n)
	src := mapBlobSource{}
	var wantUploaded, wantDeduped int64
	present := []string{}
	for i := range n {
		content := fmt.Appendf(nil, "artifact blob number %d with distinct bytes", i)
		digest := cache.DigestOf(content)
		src[digest] = content
		files = append(files, cache.FileEntry{Path: fmt.Sprintf("dist/%d.js", i), Digest: digest, Mode: 0o644, Size: int64(len(content))})
		if i%4 == 0 { // a quarter are already in CAS
			present = append(present, digest)
			wantDeduped += int64(len(content))
		} else {
			wantUploaded += int64(len(content))
		}
	}

	f := newFakeCAS(present...)
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	in := StoreInput{
		Key:      strings.Repeat("e", cache.KeyLength),
		Result:   &cache.ActionResult{Status: "success", DurationMs: 2000},
		Manifest: &cache.Manifest{Files: files},
	}
	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	resp, err := c.StoreResult(context.Background(), in, src)
	if err != nil {
		t.Fatalf("StoreResult: %v", err)
	}
	if !resp.Commit.Committed {
		t.Error("expected commit after concurrent uploads")
	}
	wantBlobs := n - len(present)
	if resp.BlobsUploaded != wantBlobs {
		t.Errorf("BlobsUploaded = %d, want %d", resp.BlobsUploaded, wantBlobs)
	}
	if resp.BytesUploaded != wantUploaded || resp.BytesDeduped != wantDeduped {
		t.Errorf("bytes = %d uploaded / %d deduped, want %d / %d",
			resp.BytesUploaded, resp.BytesDeduped, wantUploaded, wantDeduped)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.uploaded) != wantBlobs {
		t.Errorf("server received %d uploads, want %d", len(f.uploaded), wantBlobs)
	}
}

func TestStoreResult_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(cache.ErrorResponse{
			ProtocolVersion: cache.ProtocolVersion,
			Code:            cache.CodeUnauthorized,
			Message:         "token expired",
		})
	}))
	defer srv.Close()

	_, _, in, src := storeFixture()
	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	_, err := c.StoreResult(context.Background(), in, src)
	if err == nil {
		t.Fatal("expected an error for a 401 store response")
	}
	if !strings.Contains(err.Error(), "store failed") || !strings.Contains(err.Error(), cache.CodeUnauthorized) {
		t.Errorf("error should carry the store op and server code, got: %v", err)
	}
}

func TestStoreResult_RejectsUploadOutsideManifest(t *testing.T) {
	_, _, in, src := storeFixture()
	extra := cache.DigestOf([]byte("not part of this result"))
	src[extra] = []byte("not part of this result")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case cache.StorePath:
			_ = json.NewEncoder(w).Encode(cache.StoreResponse{
				ProtocolVersion: cache.ProtocolVersion,
				Key:             in.Key,
				Uploads: []cache.BlobTransfer{{
					Digest: extra,
					URL:    "http://" + r.Host + "/up/" + extra,
					Method: cache.TransferPut,
				}},
			})
		case "/up/" + extra:
			t.Fatal("client uploaded a blob that was not in the manifest")
		default:
			t.Fatalf("unexpected request path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	_, err := c.StoreResult(context.Background(), in, src)
	if err == nil {
		t.Fatal("expected an error for an upload outside the manifest")
	}
	if !strings.Contains(err.Error(), "outside manifest") {
		t.Errorf("error should explain the rejected digest, got: %v", err)
	}
}

func TestStore_RejectsMismatchedResponseKey(t *testing.T) {
	_, _, in, _ := storeFixture()
	otherKey := strings.Repeat("b", cache.KeyLength)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(cache.StoreResponse{
			ProtocolVersion: cache.ProtocolVersion,
			Key:             otherKey,
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	_, err := c.Store(context.Background(), c.BuildStoreRequest(in))
	if err == nil {
		t.Fatal("expected an error for a mismatched store response key")
	}
	if !strings.Contains(err.Error(), "does not match request key") {
		t.Errorf("error should mention the key mismatch, got: %v", err)
	}
}

func TestCommit_RejectsMismatchedResponseKey(t *testing.T) {
	_, _, in, _ := storeFixture()
	otherKey := strings.Repeat("b", cache.KeyLength)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(cache.CommitResponse{
			ProtocolVersion: cache.ProtocolVersion,
			Key:             otherKey,
			Committed:       true,
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	req := &cache.CommitRequest{
		ProtocolVersion: cache.ProtocolVersion,
		Key:             in.Key,
		Result:          in.Result,
		Manifest:        in.Manifest,
	}
	_, err := c.Commit(context.Background(), req)
	if err == nil {
		t.Fatal("expected an error for a mismatched commit response key")
	}
	if !strings.Contains(err.Error(), "does not match request key") {
		t.Errorf("error should mention the key mismatch, got: %v", err)
	}
}

func TestUploadBlob_RejectsNonPut(t *testing.T) {
	c := NewClient("https://cache.example", "tok")
	err := c.UploadBlob(context.Background(), cache.BlobTransfer{
		Digest: cache.DigestOf([]byte("x")), URL: "https://cas/x", Method: cache.TransferGet,
	}, strings.NewReader("x"))
	if err == nil {
		t.Fatal("expected an error for a non-PUT transfer")
	}
}

func TestBuildStoreRequest_Normalizes(t *testing.T) {
	c := NewClient("https://cache.example", "tok")
	dX := cache.DigestAlgorithm + ":" + strings.Repeat("d", cache.KeyLength)
	dY := cache.DigestAlgorithm + ":" + strings.Repeat("e", cache.KeyLength)
	req := c.BuildStoreRequest(StoreInput{
		Key: strings.Repeat("a", cache.KeyLength),
		Manifest: &cache.Manifest{Files: []cache.FileEntry{
			{Path: "z.js", Digest: dY}, {Path: "a.js", Digest: dX},
		}},
	})
	if req.ProtocolVersion != cache.ProtocolVersion {
		t.Errorf("ProtocolVersion = %d, want %d", req.ProtocolVersion, cache.ProtocolVersion)
	}
	if req.Manifest.Files[0].Path != "a.js" {
		t.Errorf("manifest files not normalized/sorted: %v", req.Manifest.Files)
	}
}
