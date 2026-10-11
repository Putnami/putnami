package remotecache

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	cache "go.putnami.dev/protocol/cache"
)

// Storage-token transfers: a transfer may be a plain object URL
// whose credential is a storage token in its Headers. An upload of that kind is
// create-only, and storage answers 412 when the object already exists.

// tokenBucket is a stand-in for object storage reached with a storage token. It
// records the headers of every request, serves one object, and answers each PUT
// with a set status.
type tokenBucket struct {
	mu        sync.Mutex
	putStatus int
	object    []byte
	headers   []http.Header
	bodies    [][]byte
}

func (s *tokenBucket) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.headers = append(s.headers, r.Header.Clone())
		s.bodies = append(s.bodies, body)
		status, object := s.putStatus, s.object
		s.mu.Unlock()

		if r.Method == http.MethodPut {
			w.WriteHeader(status)
			return
		}
		// A ranged GET, the one form the cache server asks for: bytes=first-last.
		var first, last int
		if n, _ := parseRange(r.Header.Get("Range"), &first, &last); n == 2 && last < len(object) {
			w.Header().Set("Content-Length", strconv.Itoa(last-first+1))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(object[first : last+1])
			return
		}
		_, _ = w.Write(object)
	})
}

func parseRange(header string, first, last *int) (int, error) {
	spec, ok := strings.CutPrefix(header, "bytes=")
	if !ok {
		return 0, nil
	}
	a, b, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, nil
	}
	var err error
	if *first, err = strconv.Atoi(a); err != nil {
		return 0, err
	}
	if *last, err = strconv.Atoi(b); err != nil {
		return 1, err
	}
	return 2, nil
}

func (s *tokenBucket) last(t *testing.T) (http.Header, []byte) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.headers) == 0 {
		t.Fatal("storage received no request")
	}
	return s.headers[len(s.headers)-1], s.bodies[len(s.bodies)-1]
}

func replyJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// createOnlyPut is the transfer the cache server issues for a digest storage
// does not hold, with the precondition header spelled as given.
func createOnlyPut(url, digest, precondition string) cache.BlobTransfer {
	return cache.BlobTransfer{
		Digest: digest,
		URL:    url,
		Method: cache.TransferPut,
		Headers: map[string]string{
			"Authorization": "Bearer storage-token",
			precondition:    "0",
			"Content-Type":  "application/octet-stream",
		},
	}
}

// TestUploadBlob_CreateOnlyConflictIsStored: storage refuses a create-only PUT
// with 412 when the object exists. Another writer stored the digest first, so
// the upload is done, however the server spelled the header.
func TestUploadBlob_CreateOnlyConflictIsStored(t *testing.T) {
	content := []byte("a built output")
	for _, precondition := range []string{"x-goog-if-generation-match", "X-Goog-If-Generation-Match", "X-GOOG-IF-GENERATION-MATCH"} {
		t.Run(precondition, func(t *testing.T) {
			store := &tokenBucket{putStatus: http.StatusPreconditionFailed}
			srv := httptest.NewServer(store.handler())
			defer srv.Close()

			c := NewClient("https://cache.example", "user-token", WithHTTPClient(srv.Client()))
			up := createOnlyPut(srv.URL+"/bucket/delivery/cache/org/cas/sha256/ab", cache.DigestOf(content), precondition)
			if err := c.UploadBlob(context.Background(), up, bytes.NewReader(content), int64(len(content))); err != nil {
				t.Fatalf("UploadBlob: %v, want a 412 on a create-only transfer read as stored", err)
			}

			// The transfer's headers reach storage verbatim, and the user's
			// own token never does.
			got, body := store.last(t)
			if got.Get("Authorization") != "Bearer storage-token" {
				t.Errorf("Authorization=%q want the transfer's storage token", got.Get("Authorization"))
			}
			if got.Get("x-goog-if-generation-match") != "0" || got.Get("Content-Type") != "application/octet-stream" {
				t.Errorf("headers=%v want the precondition and the content type", got)
			}
			if !bytes.Equal(body, content) {
				t.Errorf("body=%q want the blob", body)
			}
		})
	}
}

// TestUploadBlob_CreateOnlyStoresWhenAbsent: the ordinary outcome of a
// create-only PUT is a 200 and the bytes in storage.
func TestUploadBlob_CreateOnlyStoresWhenAbsent(t *testing.T) {
	content := []byte("a built output")
	store := &tokenBucket{putStatus: http.StatusOK}
	srv := httptest.NewServer(store.handler())
	defer srv.Close()

	c := NewClient("https://cache.example", "user-token", WithHTTPClient(srv.Client()))
	up := createOnlyPut(srv.URL+"/bucket/object", cache.DigestOf(content), "x-goog-if-generation-match")
	if err := c.UploadBlob(context.Background(), up, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("UploadBlob: %v", err)
	}
	if _, body := store.last(t); !bytes.Equal(body, content) {
		t.Errorf("body=%q want the blob", body)
	}
}

// TestUploadBlob_RefusalsStillFail pins the two limits of the 412 rule. Without
// the precondition header a 412 is a refusal like any other: a signed PUT never
// answers one for an object that exists. And with the header, every status but
// 412 keeps its meaning: a 403 is a token that may not write there, never a
// stored blob.
func TestUploadBlob_RefusalsStillFail(t *testing.T) {
	content := []byte("a built output")
	digest := cache.DigestOf(content)
	signed := func(url string) cache.BlobTransfer {
		return cache.BlobTransfer{
			Digest: digest, URL: url, Method: cache.TransferPut,
			Headers: map[string]string{"Content-Type": "application/octet-stream"},
		}
	}
	createOnly := func(url string) cache.BlobTransfer {
		return createOnlyPut(url, digest, "x-goog-if-generation-match")
	}
	for name, tc := range map[string]struct {
		status   int
		transfer func(url string) cache.BlobTransfer
	}{
		"412 on a signed PUT": {http.StatusPreconditionFailed, signed},
		"412 with no header at all": {http.StatusPreconditionFailed, func(url string) cache.BlobTransfer {
			return cache.BlobTransfer{Digest: digest, URL: url, Method: cache.TransferPut}
		}},
		"403 on a create-only PUT":       {http.StatusForbidden, createOnly},
		"401 on a create-only PUT":       {http.StatusUnauthorized, createOnly},
		"404 on a create-only PUT":       {http.StatusNotFound, createOnly},
		"409 on a create-only PUT":       {http.StatusConflict, createOnly},
		"429 on a create-only PUT":       {http.StatusTooManyRequests, createOnly},
		"503 on a create-only PUT":       {http.StatusServiceUnavailable, createOnly},
		"304 on a create-only PUT":       {http.StatusNotModified, createOnly},
		"403 on a signed PUT, as before": {http.StatusForbidden, signed},
	} {
		t.Run(name, func(t *testing.T) {
			store := &tokenBucket{putStatus: tc.status}
			srv := httptest.NewServer(store.handler())
			defer srv.Close()

			c := NewClient("https://cache.example", "user-token", WithHTTPClient(srv.Client()))
			err := c.UploadBlob(context.Background(), tc.transfer(srv.URL+"/bucket/object"), bytes.NewReader(content), int64(len(content)))
			if err == nil {
				t.Fatalf("UploadBlob succeeded on HTTP %d, want an error", tc.status)
			}
			if !strings.Contains(err.Error(), strconv.Itoa(tc.status)) {
				t.Errorf("error %q should name the status", err)
			}
			if strings.Contains(err.Error(), "storage-token") {
				t.Errorf("error %q carries the storage token", err)
			}
		})
	}
}

// TestCreateOnly reads the precondition off a transfer's headers by name, in
// any case, and off nothing else.
func TestCreateOnly(t *testing.T) {
	for name, tc := range map[string]struct {
		headers map[string]string
		want    bool
	}{
		"lower case":           {map[string]string{"x-goog-if-generation-match": "0"}, true},
		"canonical case":       {map[string]string{"X-Goog-If-Generation-Match": "0"}, true},
		"beside other headers": {map[string]string{"Authorization": "Bearer t", "x-goog-if-generation-match": "0"}, true},
		"a token alone":        {map[string]string{"Authorization": "Bearer t"}, false},
		"another precondition": {map[string]string{"x-goog-if-metageneration-match": "0", "If-Match": "*"}, false},
		"a value, not a name":  {map[string]string{"X-Note": "x-goog-if-generation-match"}, false},
		"no headers":           {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := createOnly(cache.BlobTransfer{Headers: tc.headers}); got != tc.want {
				t.Errorf("createOnly=%v want %v", got, tc.want)
			}
		})
	}
}

// TestStoreBatch_CreateOnlyConflictCommits runs the whole write exchange: the
// server grants a create-only PUT, storage answers 412 because another run
// stored the blob first, and the entry still commits.
func TestStoreBatch_CreateOnlyConflictCommits(t *testing.T) {
	store := &tokenBucket{putStatus: http.StatusPreconditionFailed}
	storage := httptest.NewServer(store.handler())
	defer storage.Close()

	key, content, in, src := storeFixture()
	digest := cache.DigestOf(content)
	var committed atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc(cache.FindMissingPath, func(w http.ResponseWriter, _ *http.Request) {
		replyJSON(w, cache.FindMissingResponse{
			ProtocolVersion: cache.ProtocolVersion,
			Uploads:         []cache.BlobTransfer{createOnlyPut(storage.URL+"/bucket/object", digest, "x-goog-if-generation-match")},
		})
	})
	mux.HandleFunc(cache.CommitBatchPath, func(w http.ResponseWriter, _ *http.Request) {
		committed.Store(true)
		replyJSON(w, cache.CommitBatchResponse{
			ProtocolVersion: cache.ProtocolVersion,
			Results:         []cache.CommitResult{{Key: key, Committed: true}},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := NewClient(server.URL, "user-token", WithHTTPClient(server.Client()))
	out, err := c.StoreBatch(context.Background(), []StoreInput{in}, src, nil, WritePath{})
	if err != nil {
		t.Fatalf("StoreBatch: %v", err)
	}
	if !committed.Load() || !out.Committed[key] {
		t.Fatalf("committed=%v outcome=%+v want the entry committed after the 412", committed.Load(), out)
	}
	got, _ := store.last(t)
	if got.Get("Authorization") != "Bearer storage-token" {
		t.Errorf("Authorization=%q want the transfer's storage token, never the user's", got.Get("Authorization"))
	}
}

// TestDownloadBlob_SendsTheStorageTokenBesideTheRange: a direct download of a
// packed blob carries two headers, the storage token and the byte range, and
// both reach storage. The user's own token does not.
func TestDownloadBlob_SendsTheStorageTokenBesideTheRange(t *testing.T) {
	member := []byte("the second member")
	pack := append(append([]byte("first member|"), member...), []byte("|third member")...)
	first := len("first member|")
	store := &tokenBucket{object: pack}
	srv := httptest.NewServer(store.handler())
	defer srv.Close()

	c := NewClient("https://cache.example", "user-token", WithHTTPClient(srv.Client()))
	var dst bytes.Buffer
	n, err := c.DownloadBlob(context.Background(), cache.BlobTransfer{
		Digest: cache.DigestOf(member),
		URL:    srv.URL + "/bucket/delivery/cache/org/packs/00ff",
		Method: cache.TransferGet,
		Headers: map[string]string{
			"Authorization": "Bearer storage-token",
			"Range":         "bytes=" + strconv.Itoa(first) + "-" + strconv.Itoa(first+len(member)-1),
		},
		SizeBytes: int64(len(member)),
	}, &dst)
	if err != nil {
		t.Fatalf("DownloadBlob: %v", err)
	}
	if n != int64(len(member)) || !bytes.Equal(dst.Bytes(), member) {
		t.Fatalf("downloaded %q want the member's bytes", dst.Bytes())
	}
	got, _ := store.last(t)
	if got.Get("Authorization") != "Bearer storage-token" {
		t.Errorf("Authorization=%q want the transfer's storage token", got.Get("Authorization"))
	}
	if got.Get("Range") == "" {
		t.Error("the Range header was dropped")
	}
}
