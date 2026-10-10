package remotecache

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	cache "go.putnami.dev/protocol/cache"
)

func TestCapabilities_AdvertisedSet(t *testing.T) {
	f := newFakeCAS()
	f.capabilities = []string{cache.CapabilityFindMissing, cache.CapabilityCommitBatch}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	c := NewClient(srv.URL, "secret-token", WithHTTPClient(srv.Client()))
	caps, err := c.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !slices.Contains(caps.Capabilities, cache.CapabilityFindMissing) ||
		!slices.Contains(caps.Capabilities, cache.CapabilityCommitBatch) {
		t.Errorf("capabilities = %v, want find-missing + commit-batch", caps.Capabilities)
	}
	if f.capsAuth != "Bearer secret-token" {
		t.Errorf("capabilities probe must carry the bearer token, got %q", f.capsAuth)
	}
}

func TestCapabilities_404IsBaseline(t *testing.T) {
	f := newFakeCAS()
	f.caps404 = true // older, baseline-only server
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	caps, err := c.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("a 404 probe must not be an error: %v", err)
	}
	if len(caps.Capabilities) != 0 {
		t.Errorf("baseline server should advertise no capabilities, got %v", caps.Capabilities)
	}
}

// TestCapabilities_NonAdvertisedStatusFallsBack pins the probe's documented
// contract: ANY non-2xx status degrades to the baseline (empty capabilities, no
// error) so the caller falls back to store/commit — never a hard error. 403 is
// the status a workspace machine token gets when it is authenticated but lacks
// cache.read; a hard statusError on it would fail the build.
func TestCapabilities_NonAdvertisedStatusFallsBack(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized,        // 401
		http.StatusForbidden,           // 403 — machine token lacking cache.read
		http.StatusInternalServerError, // 500
		http.StatusServiceUnavailable,  // 503
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newFakeCAS()
			f.capsStatus = status
			srv := httptest.NewServer(f.handler(t))
			defer srv.Close()

			c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
			caps, err := c.Capabilities(context.Background())
			if err != nil {
				t.Fatalf("a %d capabilities probe must degrade to the baseline, not error: %v", status, err)
			}
			if caps == nil || len(caps.Capabilities) != 0 {
				t.Errorf("a non-2xx probe must advertise no capabilities (fall back to store/commit), got %+v", caps)
			}
		})
	}
}

// TestCapabilities_TransportErrorStillErrors proves the non-2xx fallback is
// confined to RECEIVED HTTP statuses: a transport fault (here a canceled
// context, standing in for a dial/read failure) still errors rather than
// silently degrading, so a broken endpoint is never mistaken for a baseline-only
// server that simply lacks the batched path.
func TestCapabilities_TransportErrorStillErrors(t *testing.T) {
	c := NewClient("http://127.0.0.1:0", "tok")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // no HTTP status is ever received: the request fails in transport
	if _, err := c.Capabilities(ctx); err == nil {
		t.Fatal("a transport/dial failure must still error, not degrade to empty capabilities")
	}
}

// batchFixture builds three single-file entries with distinct content plus a
// blob source that can serve their bytes.
func batchFixture() (in []StoreInput, src mapBlobSource, digests []string) {
	keys := []string{
		strings.Repeat("a", cache.KeyLength),
		strings.Repeat("b", cache.KeyLength),
		strings.Repeat("c", cache.KeyLength),
	}
	contents := [][]byte{
		[]byte("artifact A bytes"),
		[]byte("artifact B bytes"),
		[]byte("artifact C bytes"),
	}
	src = mapBlobSource{}
	for i, k := range keys {
		d := cache.DigestOf(contents[i])
		digests = append(digests, d)
		src[d] = contents[i]
		in = append(in, StoreInput{
			Key:    k,
			Result: &cache.ActionResult{Status: "success", SizeBytes: int64(len(contents[i]))},
			Manifest: &cache.Manifest{Files: []cache.FileEntry{
				{Path: "dist/" + k[:4] + ".js", Digest: d, Mode: 0o644, Size: int64(len(contents[i]))},
			}},
		})
	}
	return in, src, digests
}

func TestStoreBatch_OneFindMissingOneCommitForWholeWindow(t *testing.T) {
	f := newFakeCAS()
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	in, src, digests := batchFixture()
	var wantBytes int64
	for _, d := range digests {
		wantBytes += int64(len(src[d]))
	}

	c := NewClient(srv.URL, "secret-token", WithHTTPClient(srv.Client()))
	out, err := c.StoreBatch(context.Background(), in, src, nil, WritePath{})
	if err != nil {
		t.Fatalf("StoreBatch: %v", err)
	}

	// The whole window costs ONE find-missing + ONE commit-batch — not the
	// per-job store+commit (which would be 3 + 3 round trips).
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nFindMissing != 1 || f.nCommitBatch != 1 {
		t.Errorf("window should be 1 find-missing + 1 commit-batch, got %d + %d", f.nFindMissing, f.nCommitBatch)
	}
	if f.nStore != 0 || f.nCommit != 0 {
		t.Errorf("batched path must not call the per-job store/commit, got %d + %d", f.nStore, f.nCommit)
	}
	if out.BlobsUploaded != 3 || out.BytesUploaded != wantBytes {
		t.Errorf("upload outcome = %d blobs / %d bytes, want 3 / %d", out.BlobsUploaded, out.BytesUploaded, wantBytes)
	}
	for _, e := range in {
		if !out.Committed[e.Key] {
			t.Errorf("entry %s not committed", e.Key)
		}
	}
	if f.commitBatch == nil || len(f.commitBatch.Entries) != 3 {
		t.Errorf("commit-batch should carry all 3 entries, got %+v", f.commitBatch)
	}
	for _, d := range digests {
		if !bytes.Equal(f.uploaded[d], src[d]) {
			t.Errorf("blob %s not uploaded correctly", d)
		}
	}
	if f.commitAuth != "Bearer secret-token" {
		t.Errorf("commit-batch must carry the bearer token, got %q", f.commitAuth)
	}
	if f.uploadAuth != "" {
		t.Errorf("presigned upload must NOT carry the bearer token, got %q", f.uploadAuth)
	}
}

func TestStoreBatch_DedupsAcrossWindow(t *testing.T) {
	in, src, digests := batchFixture()
	// The server already has the first entry's blob: it must be deduped (not
	// re-uploaded) but its entry still commits.
	f := newFakeCAS(digests[0])
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	out, err := c.StoreBatch(context.Background(), in, src, nil, WritePath{})
	if err != nil {
		t.Fatalf("StoreBatch: %v", err)
	}

	if out.BlobsUploaded != 2 {
		t.Errorf("BlobsUploaded = %d, want 2 (one deduped)", out.BlobsUploaded)
	}
	if out.BytesDeduped != int64(len(src[digests[0]])) {
		t.Errorf("BytesDeduped = %d, want %d", out.BytesDeduped, len(src[digests[0]]))
	}
	for _, e := range in {
		if !out.Committed[e.Key] {
			t.Errorf("entry %s not committed", e.Key)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, uploaded := f.uploaded[digests[0]]; uploaded {
		t.Errorf("deduped blob %s should not have been uploaded", digests[0])
	}
}

func TestStoreBatch_SharedBlobUploadedOnce(t *testing.T) {
	f := newFakeCAS()
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	content := []byte("shared compiled output")
	digest := cache.DigestOf(content)
	src := mapBlobSource{digest: content}
	// Two distinct keys whose manifests reference the SAME blob (e.g. identical
	// output from different inputs).
	in := []StoreInput{
		{Key: strings.Repeat("a", cache.KeyLength), Result: &cache.ActionResult{Status: "success"},
			Manifest: &cache.Manifest{Files: []cache.FileEntry{{Path: "a.js", Digest: digest, Mode: 0o644, Size: int64(len(content))}}}},
		{Key: strings.Repeat("b", cache.KeyLength), Result: &cache.ActionResult{Status: "success"},
			Manifest: &cache.Manifest{Files: []cache.FileEntry{{Path: "b.js", Digest: digest, Mode: 0o644, Size: int64(len(content))}}}},
	}

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	out, err := c.StoreBatch(context.Background(), in, src, nil, WritePath{})
	if err != nil {
		t.Fatalf("StoreBatch: %v", err)
	}
	if out.BlobsUploaded != 1 {
		t.Errorf("a blob shared across entries should upload once, got %d", out.BlobsUploaded)
	}
	if !out.Committed[in[0].Key] || !out.Committed[in[1].Key] {
		t.Errorf("both entries should commit, got %v", out.Committed)
	}
}

func TestStoreBatch_CompressesCompressibleBlob(t *testing.T) {
	f := newFakeCAS()
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	content := bytes.Repeat([]byte("compiled output line\n"), 2000) // very compressible
	digest := cache.DigestOf(content)
	src := mapBlobSource{digest: content}
	in := []StoreInput{{
		Key:    strings.Repeat("a", cache.KeyLength),
		Result: &cache.ActionResult{Status: "success"},
		Manifest: &cache.Manifest{Files: []cache.FileEntry{
			{Path: "dist/out.js", Digest: digest, Mode: 0o644, Size: int64(len(content))},
		}},
	}}

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	if _, err := c.StoreBatch(context.Background(), in, src, nil, WritePath{}); err != nil {
		t.Fatalf("StoreBatch: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nGzipUploads != 1 {
		t.Errorf("a compressible blob should be uploaded gzipped, nGzipUploads = %d", f.nGzipUploads)
	}
	// The server recovered the original content from the gzipped upload.
	if !bytes.Equal(f.uploaded[digest], content) {
		t.Error("decompressed upload does not match the original content")
	}
}

func TestStoreBatch_CoalescesSmallBlobs(t *testing.T) {
	f := newFakeCAS()
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	in, src, digests := batchFixture() // three tiny blobs, all under the inline cap
	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	out, err := c.StoreBatch(context.Background(), in, src, nil, WritePath{CoalesceSmall: true})
	if err != nil {
		t.Fatalf("StoreBatch: %v", err)
	}

	if out.BlobsUploaded != 3 {
		t.Errorf("BlobsUploaded = %d, want 3", out.BlobsUploaded)
	}
	for _, e := range in {
		if !out.Committed[e.Key] {
			t.Errorf("entry %s not committed", e.Key)
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nUploadBatch < 1 {
		t.Errorf("small blobs should be coalesced into upload-batch, got %d", f.nUploadBatch)
	}
	if f.nFindMissing != 0 || f.nDirectPut != 0 {
		t.Errorf("all-small window must not hit per-blob paths, got find-missing=%d direct=%d", f.nFindMissing, f.nDirectPut)
	}
	for _, d := range digests {
		if _, ok := f.uploaded[d]; !ok {
			t.Errorf("blob %s not stored", d)
		}
	}
}

func TestStoreBatch_CoalesceSplitsBySize(t *testing.T) {
	f := newFakeCAS()
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	in, src, _ := batchFixture() // three small
	large := bytes.Repeat([]byte("L"), cache.MaxInlineBlobBytes+1024)
	largeDigest := cache.DigestOf(large)
	src[largeDigest] = large
	in = append(in, StoreInput{
		Key:    strings.Repeat("d", cache.KeyLength),
		Result: &cache.ActionResult{Status: "success"},
		Manifest: &cache.Manifest{Files: []cache.FileEntry{
			{Path: "big.bin", Digest: largeDigest, Mode: 0o644, Size: int64(len(large))},
		}},
	})

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	out, err := c.StoreBatch(context.Background(), in, src, nil, WritePath{CoalesceSmall: true})
	if err != nil {
		t.Fatalf("StoreBatch: %v", err)
	}
	for _, e := range in {
		if !out.Committed[e.Key] {
			t.Errorf("entry %s not committed", e.Key)
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nUploadBatch < 1 {
		t.Errorf("small blobs should coalesce, got upload-batch=%d", f.nUploadBatch)
	}
	if f.nFindMissing != 1 {
		t.Errorf("the over-cap blob should take the find-missing path, got %d", f.nFindMissing)
	}
	if _, ok := f.uploaded[largeDigest]; !ok {
		t.Error("the large blob should be uploaded via the per-blob path")
	}
}

func TestStoreBatch_CoalesceWithDirectGrant(t *testing.T) {
	f := newFakeCAS()
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	in, src, _ := batchFixture() // all small
	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	grant, err := c.UploadGrant(context.Background())
	if err != nil {
		t.Fatalf("UploadGrant: %v", err)
	}
	out, err := c.StoreBatch(context.Background(), in, src, nil, WritePath{Grant: grant, CoalesceSmall: true})
	if err != nil {
		t.Fatalf("StoreBatch: %v", err)
	}
	for _, e := range in {
		if !out.Committed[e.Key] {
			t.Errorf("entry %s not committed", e.Key)
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	// Small blobs coalesce even when a direct grant is available; nothing is left
	// for the direct PUT path.
	if f.nUploadBatch < 1 {
		t.Errorf("small blobs should coalesce, got upload-batch=%d", f.nUploadBatch)
	}
	if f.nDirectPut != 0 {
		t.Errorf("no large blobs, so the direct path should be unused, got %d", f.nDirectPut)
	}
}

func TestStoreBatch_EmptyWindowNoCalls(t *testing.T) {
	f := newFakeCAS()
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	out, err := c.StoreBatch(context.Background(), nil, mapBlobSource{}, nil, WritePath{})
	if err != nil {
		t.Fatalf("StoreBatch: %v", err)
	}
	if len(out.Committed) != 0 {
		t.Errorf("empty window should commit nothing, got %v", out.Committed)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nFindMissing != 0 || f.nCommitBatch != 0 {
		t.Errorf("empty window should make no calls, got %d find-missing / %d commit-batch", f.nFindMissing, f.nCommitBatch)
	}
}

func TestFindMissing_RejectsUploadOutsideRequest(t *testing.T) {
	requested := cache.DigestOf([]byte("requested blob"))
	extra := cache.DigestOf([]byte("not part of this window"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != cache.FindMissingPath {
			t.Fatalf("unexpected request path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(cache.FindMissingResponse{
			ProtocolVersion: cache.ProtocolVersion,
			Uploads: []cache.BlobTransfer{{
				Digest: extra,
				URL:    "http://" + r.Host + "/up/" + extra,
				Method: cache.TransferPut,
			}},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	_, err := c.FindMissing(context.Background(), &cache.FindMissingRequest{
		ProtocolVersion: cache.ProtocolVersion,
		Blobs:           []cache.BlobRef{{Digest: requested}},
	})
	if err == nil {
		t.Fatal("expected an error for an upload outside the find-missing request")
	}
	if !strings.Contains(err.Error(), "outside request") {
		t.Errorf("error should explain the rejected digest, got: %v", err)
	}
}

func TestCommitBatch_RejectsUnexpectedOrMissingResults(t *testing.T) {
	requestedKey := strings.Repeat("a", cache.KeyLength)
	extraKey := strings.Repeat("b", cache.KeyLength)

	cases := []struct {
		name    string
		results []cache.CommitResult
		wantErr string
	}{
		{
			name:    "extra",
			results: []cache.CommitResult{{Key: requestedKey, Committed: true}, {Key: extraKey, Committed: true}},
			wantErr: "was not requested",
		},
		{
			name:    "duplicate",
			results: []cache.CommitResult{{Key: requestedKey, Committed: true}, {Key: requestedKey, Committed: true}},
			wantErr: "duplicate result",
		},
		{
			name:    "missing",
			results: nil,
			wantErr: "missing result",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != cache.CommitBatchPath {
					t.Fatalf("unexpected request path %s", r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(cache.CommitBatchResponse{
					ProtocolVersion: cache.ProtocolVersion,
					Results:         tc.results,
				})
			}))
			defer srv.Close()

			c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
			_, err := c.CommitBatch(context.Background(), &cache.CommitBatchRequest{
				ProtocolVersion: cache.ProtocolVersion,
				Entries: []cache.CommitEntry{{
					Key:      requestedKey,
					Result:   &cache.ActionResult{Status: "success"},
					Manifest: &cache.Manifest{},
				}},
			})
			if err == nil {
				t.Fatalf("expected %q error", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// fakePresence is a set-backed PresenceCache for tests.
type fakePresence map[string]bool

func (p fakePresence) Present(digest string) bool { return p[digest] }

func TestStoreBatch_SkipsKnownPresent(t *testing.T) {
	in, src, digests := batchFixture()
	// A warm cache: the server already has all three blobs, and the client knows
	// two of them from a prior run.
	f := newFakeCAS(digests...)
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	known := fakePresence{digests[0]: true, digests[1]: true}

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	out, err := c.StoreBatch(context.Background(), in, src, known, WritePath{})
	if err != nil {
		t.Fatalf("StoreBatch: %v", err)
	}

	if out.BlobsUploaded != 0 {
		t.Errorf("nothing should upload when all blobs are present, got %d", out.BlobsUploaded)
	}
	for _, e := range in {
		if !out.Committed[e.Key] {
			t.Errorf("entry %s not committed", e.Key)
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	// The two known blobs were never even probed; only the unknown one was.
	if f.findMissingSeen[digests[0]] || f.findMissingSeen[digests[1]] {
		t.Errorf("known-present blobs must be skipped from find-missing, saw %v", f.findMissingSeen)
	}
	if !f.findMissingSeen[digests[2]] {
		t.Errorf("the unknown blob %s should be probed", digests[2])
	}
	// Confirmed excludes the known-skipped blobs (the caller already has them).
	if slices.Contains(out.Confirmed, digests[0]) || slices.Contains(out.Confirmed, digests[1]) {
		t.Errorf("Confirmed should exclude known-skipped blobs, got %v", out.Confirmed)
	}
	if !slices.Contains(out.Confirmed, digests[2]) {
		t.Errorf("Confirmed should include the probed blob %s", digests[2])
	}
}

func TestUploadGrant_Fetch(t *testing.T) {
	f := newFakeCAS()
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	grant, err := c.UploadGrant(context.Background())
	if err != nil {
		t.Fatalf("UploadGrant: %v", err)
	}
	if !strings.Contains(grant.URLTemplate, cache.DigestPlaceholder) {
		t.Errorf("grant template %q must contain the digest placeholder", grant.URLTemplate)
	}
	if grant.Method != cache.TransferPut || grant.ConditionalExistsStatus != 412 {
		t.Errorf("grant = %+v, want PUT + conditional 412", grant)
	}
}

func TestStoreBatch_DirectUploadsAndCommits(t *testing.T) {
	f := newFakeCAS()
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	in, src, digests := batchFixture()
	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	grant, err := c.UploadGrant(context.Background())
	if err != nil {
		t.Fatalf("UploadGrant: %v", err)
	}

	out, err := c.StoreBatch(context.Background(), in, src, nil, WritePath{Grant: grant})
	if err != nil {
		t.Fatalf("StoreBatch: %v", err)
	}

	if out.BlobsUploaded != 3 {
		t.Errorf("BlobsUploaded = %d, want 3", out.BlobsUploaded)
	}
	for _, e := range in {
		if !out.Committed[e.Key] {
			t.Errorf("entry %s not committed", e.Key)
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	// The direct path PUTs straight to CAS — no find-missing round trip.
	if f.nFindMissing != 0 {
		t.Errorf("direct path must not call find-missing, got %d", f.nFindMissing)
	}
	if f.nDirectPut != 3 {
		t.Errorf("expected 3 direct PUTs, got %d", f.nDirectPut)
	}
	if f.nCommitBatch < 1 {
		t.Errorf("expected commit-batch, got %d", f.nCommitBatch)
	}
	for _, d := range digests {
		if _, ok := f.uploaded[d]; !ok {
			t.Errorf("blob %s not uploaded", d)
		}
	}
	// The grant carried no credential, so the direct PUT carries none either —
	// the client sends only the grant's headers, nothing extra.
	if f.directPutAuth != "" {
		t.Errorf("direct PUT should only carry grant headers, saw Authorization %q", f.directPutAuth)
	}
}

func TestStoreBatch_DirectDedupsExisting(t *testing.T) {
	in, src, digests := batchFixture()
	f := newFakeCAS(digests[0]) // already present → conditional create rejects it
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	grant, err := c.UploadGrant(context.Background())
	if err != nil {
		t.Fatalf("UploadGrant: %v", err)
	}
	out, err := c.StoreBatch(context.Background(), in, src, nil, WritePath{Grant: grant})
	if err != nil {
		t.Fatalf("StoreBatch: %v", err)
	}

	if out.BlobsUploaded != 2 {
		t.Errorf("BlobsUploaded = %d, want 2 (one already present)", out.BlobsUploaded)
	}
	if out.BytesDeduped != int64(len(src[digests[0]])) {
		t.Errorf("BytesDeduped = %d, want %d", out.BytesDeduped, len(src[digests[0]]))
	}
	for _, e := range in {
		if !out.Committed[e.Key] {
			t.Errorf("entry %s not committed", e.Key)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, uploaded := f.uploaded[digests[0]]; uploaded {
		t.Errorf("already-present blob %s should not have been re-uploaded", digests[0])
	}
}

func TestStoreBatch_DirectKnownPresentSkipAndRecovery(t *testing.T) {
	in, src, digests := batchFixture()
	// Server has entries 1 and 2's blobs, not entry 0's; the client wrongly
	// believes all three are present (entry 0's was evicted).
	f := newFakeCAS(digests[1], digests[2])
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	known := fakePresence{digests[0]: true, digests[1]: true, digests[2]: true}
	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	grant, err := c.UploadGrant(context.Background())
	if err != nil {
		t.Fatalf("UploadGrant: %v", err)
	}
	out, err := c.StoreBatch(context.Background(), in, src, known, WritePath{Grant: grant})
	if err != nil {
		t.Fatalf("StoreBatch: %v", err)
	}

	for _, e := range in {
		if !out.Committed[e.Key] {
			t.Errorf("entry %s not committed (recovery should re-upload its blob)", e.Key)
		}
	}
	if out.BlobsUploaded != 1 {
		t.Errorf("BlobsUploaded = %d, want 1 (only the evicted blob)", out.BlobsUploaded)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nDirectPut != 1 {
		t.Errorf("expected 1 direct PUT (the evicted blob), got %d", f.nDirectPut)
	}
	if _, ok := f.uploaded[digests[0]]; !ok {
		t.Errorf("evicted blob %s should have been re-uploaded on recovery", digests[0])
	}
}

func TestStoreBatch_RecoversWhenKnownBlobEvicted(t *testing.T) {
	in, src, digests := batchFixture()
	// The server has entries 1 and 2's blobs, but NOT entry 0's — yet the client
	// wrongly believes entry 0's blob is present (the cache evicted it since the
	// prior run).
	f := newFakeCAS(digests[1], digests[2])
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	known := fakePresence{digests[0]: true}

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	out, err := c.StoreBatch(context.Background(), in, src, known, WritePath{})
	if err != nil {
		t.Fatalf("StoreBatch: %v", err)
	}

	// All entries commit — entry 0 only after the recovery pass re-uploads its
	// evicted blob.
	for _, e := range in {
		if !out.Committed[e.Key] {
			t.Errorf("entry %s not committed (recovery should have re-uploaded its blob)", e.Key)
		}
	}
	if out.BlobsUploaded != 1 {
		t.Errorf("BlobsUploaded = %d, want 1 (the evicted blob)", out.BlobsUploaded)
	}
	if !slices.Contains(out.Confirmed, digests[0]) {
		t.Errorf("the recovered blob should be confirmed, got %v", out.Confirmed)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if !bytes.Equal(f.uploaded[digests[0]], src[digests[0]]) {
		t.Errorf("the evicted blob %s should have been re-uploaded", digests[0])
	}
	// Two commit-batch round trips: the optimistic pass + the recovery pass.
	if f.nCommitBatch != 2 {
		t.Errorf("expected 2 commit-batch round trips (optimistic + recovery), got %d", f.nCommitBatch)
	}
}
