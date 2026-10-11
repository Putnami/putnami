package remotecache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	cache "go.putnami.dev/protocol/cache"
)

func TestCompressedBody_CompressesCompressible(t *testing.T) {
	content := bytes.Repeat([]byte("putnami remote cache "), 1000)
	digest := cache.DigestOf(content)
	src := mapBlobSource{digest: content}

	body, length, cleanup, err := compressedBody(context.Background(), src, digest, int64(len(content)))
	if err != nil {
		t.Fatalf("compressedBody: %v", err)
	}
	defer cleanup() //nolint:errcheck

	if length >= int64(len(content)) {
		t.Errorf("a compressible blob should shrink: length %d, raw %d", length, len(content))
	}
	got, _ := io.ReadAll(body)
	if int64(len(got)) != length {
		t.Errorf("body length %d != reported length %d", len(got), length)
	}
	if len(got) < 2 || got[0] != 0x1f || got[1] != 0x8b {
		t.Errorf("compressed body should be gzip (magic 1f 8b), got % x", got[:2])
	}
	// It must decompress back to the original (round trip via the read path).
	dr, err := decompressBlobReader(bytes.NewReader(got))
	if err != nil {
		t.Fatal(err)
	}
	dec, _ := io.ReadAll(dr)
	if !bytes.Equal(dec, content) {
		t.Error("decompressed bytes do not match the original content")
	}
}

func TestCompressedBody_RawWhenNotSmaller(t *testing.T) {
	content := []byte("tiny") // gzip framing overhead exceeds the content → sent raw
	digest := cache.DigestOf(content)
	src := mapBlobSource{digest: content}

	body, length, cleanup, err := compressedBody(context.Background(), src, digest, int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup() //nolint:errcheck

	if length != int64(len(content)) {
		t.Errorf("an incompressible blob should stay raw: length %d, want %d", length, len(content))
	}
	got, _ := io.ReadAll(body)
	if !bytes.Equal(got, content) {
		t.Error("raw body should equal the original content")
	}
}

// TestCompressedBody_SpoolsLargeCompressible pins the streamed path: a blob
// past the former 16 MiB raw limit is gzipped through a spool file, the body is
// exactly the reported length and strictly smaller than the blob, it decodes
// back through the reader's length rule, and cleanup removes the spool.
func TestCompressedBody_SpoolsLargeCompressible(t *testing.T) {
	dir := spoolDir(t)
	content := compressible(16<<20 + 4096)
	digest := cache.DigestOf(content)

	body, length, cleanup, err := compressedBody(context.Background(), mapBlobSource{digest: content}, digest, int64(len(content)))
	if err != nil {
		t.Fatalf("compressedBody: %v", err)
	}
	if files := spoolFiles(t, dir); len(files) != 1 || !strings.HasPrefix(files[0], "putnami-cache-upload-") {
		t.Errorf("spool files while the body is open = %v, want one putnami-cache-upload-* file", files)
	}
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	if int64(len(got)) != length {
		t.Errorf("body length %d != reported length %d", len(got), length)
	}
	if length >= int64(len(content)) {
		t.Errorf("a compressible blob must be stored strictly smaller: length %d, raw %d", length, len(content))
	}
	if len(got) < 2 || got[0] != gzipMagic[0] || got[1] != gzipMagic[1] {
		t.Fatalf("spooled body should be gzip, got %d bytes", len(got))
	}
	dr, err := decodeBlobReader(bytes.NewReader(got), length, int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	if dec, _ := io.ReadAll(dr); !bytes.Equal(dec, content) {
		t.Error("the spooled body does not decode back to the blob")
	}
	if files := spoolFiles(t, dir); len(files) != 0 {
		t.Errorf("cleanup left spool files %v", files)
	}
}

// TestCompressedBody_StreamsLargeIncompressibleRaw pins the fallback: a large
// blob gzip cannot shrink is sent raw at its own length, and its spool is gone
// before the upload starts.
func TestCompressedBody_StreamsLargeIncompressibleRaw(t *testing.T) {
	dir := spoolDir(t)
	content := incompressible(compressInMemoryMaxBytes + 4096)
	digest := cache.DigestOf(content)

	body, length, cleanup, err := compressedBody(context.Background(), mapBlobSource{digest: content}, digest, int64(len(content)))
	if err != nil {
		t.Fatalf("compressedBody: %v", err)
	}
	defer cleanup() //nolint:errcheck
	if files := spoolFiles(t, dir); len(files) != 0 {
		t.Errorf("a raw upload must not hold a spool, found %v", files)
	}
	if length != int64(len(content)) {
		t.Errorf("raw length = %d, want %d", length, len(content))
	}
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Error("raw body should equal the original content")
	}
}

// TestCompressedBody_RefusesASourceOfAnotherSize pins that no body carries a
// length it does not send: a source that is not the declared size fails while
// the body is built, or while it is read, and leaves no spool behind.
func TestCompressedBody_RefusesASourceOfAnotherSize(t *testing.T) {
	small := []byte("declared blob bytes")
	large := compressible(compressInMemoryMaxBytes + 4096)
	random := incompressible(compressInMemoryMaxBytes + 4096)

	tests := []struct {
		name     string
		declared []byte   // the blob whose size is declared
		opens    [][]byte // what each successive OpenBlob yields
	}{
		{name: "small source longer", declared: small, opens: [][]byte{append(append([]byte{}, small...), 'x')}},
		{name: "small source shorter", declared: small, opens: [][]byte{small[1:]}},
		{name: "large compressible source shorter", declared: large, opens: [][]byte{large[1:]}},
		{name: "large compressible source longer", declared: large, opens: [][]byte{append(append([]byte{}, large...), large[:4096]...)}},
		{name: "large incompressible source longer", declared: random, opens: [][]byte{append(append([]byte{}, random...), 'x')}},
		{name: "large incompressible source shrinks before the upload", declared: random, opens: [][]byte{random, random[:len(random)-1]}},
		{name: "large incompressible source grows before the upload", declared: random, opens: [][]byte{random, append(append([]byte{}, random...), 'x')}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := spoolDir(t)
			src := &sequenceSource{opens: tt.opens}
			digest := cache.DigestOf(tt.declared)

			body, length, cleanup, err := compressedBody(context.Background(), src, digest, int64(len(tt.declared)))
			if err == nil {
				got, readErr := io.ReadAll(body)
				_ = cleanup()
				if readErr == nil {
					t.Fatalf("a source of another size produced a %d-byte body for length %d", len(got), length)
				}
			}
			if files := spoolFiles(t, dir); len(files) != 0 {
				t.Errorf("spool files left behind: %v", files)
			}
		})
	}
}

// TestCompressedBody_CanceledContextRemovesSpool pins that a canceled upload
// stops compressing and removes its spool.
func TestCompressedBody_CanceledContextRemovesSpool(t *testing.T) {
	dir := spoolDir(t)
	content := compressible(compressInMemoryMaxBytes + 4096)
	digest := cache.DigestOf(content)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, _, err := compressedBody(ctx, mapBlobSource{digest: content}, digest, int64(len(content)))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if files := spoolFiles(t, dir); len(files) != 0 {
		t.Errorf("spool files left behind: %v", files)
	}
}

// TestUpload_LargeCompressibleRoundTrips drives both PUT paths — the presigned
// transfer find-missing returns and the direct grant — with a blob past the
// former 16 MiB limit. Storage receives gzip whose Content-Length is the bytes
// sent (not the transfer's uncompressed size), the download path decodes it
// back to the blob, and no spool survives the upload.
func TestUpload_LargeCompressibleRoundTrips(t *testing.T) {
	content := compressible(16<<20 + 4096)
	digest := cache.DigestOf(content)
	hexDigest := strings.TrimPrefix(digest, cache.DigestAlgorithm+":")
	src := mapBlobSource{digest: content}

	uploads := map[string]func(c *Client, url string) error{
		"presigned transfer": func(c *Client, url string) error {
			return c.uploadFrom(context.Background(), cache.BlobTransfer{
				Digest: digest, URL: url + "/o/" + hexDigest, Method: cache.TransferPut, SizeBytes: int64(len(content)),
			}, src, int64(len(content)))
		},
		"direct grant": func(c *Client, url string) error {
			_, err := c.uploadDirect(context.Background(), &cache.UploadGrant{
				URLTemplate: url + "/o/" + cache.DigestPlaceholder, Method: cache.TransferPut,
			}, cache.BlobRef{Digest: digest, SizeBytes: int64(len(content))}, src)
			return err
		},
	}
	for name, upload := range uploads {
		t.Run(name, func(t *testing.T) {
			dir := spoolDir(t)
			store := newObjectStore()
			srv := httptest.NewServer(store)
			defer srv.Close()
			c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))

			if err := upload(c, srv.URL); err != nil {
				t.Fatalf("upload: %v", err)
			}
			stored, declared := store.get(hexDigest)
			if declared != int64(len(stored)) {
				t.Errorf("Content-Length %d, body %d bytes", declared, len(stored))
			}
			if len(stored) < 2 || len(stored) >= len(content) || stored[0] != gzipMagic[0] || stored[1] != gzipMagic[1] {
				t.Errorf("stored object should be gzip and smaller than %d bytes, got %d bytes", len(content), len(stored))
			}
			if files := spoolFiles(t, dir); len(files) != 0 {
				t.Errorf("spool files left behind: %v", files)
			}

			var dst bytes.Buffer
			n, err := c.DownloadBlob(context.Background(), cache.BlobTransfer{
				Digest: digest, URL: srv.URL + "/o/" + hexDigest, Method: cache.TransferGet, SizeBytes: int64(len(content)),
			}, &dst)
			if err != nil {
				t.Fatalf("DownloadBlob: %v", err)
			}
			if n != int64(len(content)) || !bytes.Equal(dst.Bytes(), content) {
				t.Error("the downloaded blob differs from the uploaded one")
			}
		})
	}
}

// TestUpload_FailedUploadRemovesSpool pins the error path: a refused PUT still
// removes the spool.
func TestUpload_FailedUploadRemovesSpool(t *testing.T) {
	dir := spoolDir(t)
	content := compressible(compressInMemoryMaxBytes + 4096)
	digest := cache.DigestOf(content)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	err := c.uploadFrom(context.Background(), cache.BlobTransfer{
		Digest: digest, URL: srv.URL + "/o/x", Method: cache.TransferPut,
	}, mapBlobSource{digest: content}, int64(len(content)))
	if err == nil {
		t.Fatal("expected the refused upload to fail")
	}
	if files := spoolFiles(t, dir); len(files) != 0 {
		t.Errorf("spool files left behind: %v", files)
	}
}

// TestUpload_StreamedBodyIsReplayable pins that a streamed entity can be sent
// again. The transport resends a request only when it can read the body again
// (after an HTTP/2 GOAWAY, on a kept-alive connection the server closed, on a
// 307); a redirect is the replay a test can force. Without it the client hands
// back the 307 and the upload fails.
func TestUpload_StreamedBodyIsReplayable(t *testing.T) {
	blobs := map[string][]byte{
		"gzip from the spool": compressible(compressInMemoryMaxBytes + 4096),
		"raw from the source": incompressible(compressInMemoryMaxBytes + 4096),
	}
	for name, content := range blobs {
		t.Run(name, func(t *testing.T) {
			dir := spoolDir(t)
			digest := cache.DigestOf(content)
			hexDigest := strings.TrimPrefix(digest, cache.DigestAlgorithm+":")
			store := newObjectStore()
			mux := http.NewServeMux()
			mux.Handle("/o/", store)
			mux.HandleFunc("/moved/", func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body) // the first send is read in full, then refused
				http.Redirect(w, r, "/o/"+strings.TrimPrefix(r.URL.Path, "/moved/"), http.StatusTemporaryRedirect)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
			err := c.uploadFrom(context.Background(), cache.BlobTransfer{
				Digest: digest, URL: srv.URL + "/moved/" + hexDigest, Method: cache.TransferPut, SizeBytes: int64(len(content)),
			}, mapBlobSource{digest: content}, int64(len(content)))
			if err != nil {
				t.Fatalf("upload through a redirect: %v", err)
			}

			stored, declared := store.get(hexDigest)
			if declared != int64(len(stored)) {
				t.Errorf("Content-Length %d, body %d bytes", declared, len(stored))
			}
			dr, err := decodeBlobReader(bytes.NewReader(stored), int64(len(stored)), int64(len(content)))
			if err != nil {
				t.Fatal(err)
			}
			if dec, _ := io.ReadAll(dr); !bytes.Equal(dec, content) {
				t.Error("the resent body does not decode back to the blob")
			}
			if files := spoolFiles(t, dir); len(files) != 0 {
				t.Errorf("spool files left behind: %v", files)
			}
		})
	}
}

func TestDecompressBlobReader_RawPassthrough(t *testing.T) {
	content := []byte("not gzipped at all")
	dr, err := decompressBlobReader(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(dr)
	if !bytes.Equal(got, content) {
		t.Error("a non-gzip stream should pass through unchanged")
	}
}

// spoolDir points the OS temporary directory, where large blobs spool, at a
// fresh directory and returns it.
func spoolDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, env := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(env, dir)
	}
	return dir
}

func spoolFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// compressible returns n bytes that gzip shrinks about as much as an
// executable: distinct lines, not one repeated run.
func compressible(n int) []byte {
	var b bytes.Buffer
	b.Grow(n + 64)
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "symbol %08d: putnami remote cache\n", i)
	}
	return b.Bytes()[:n]
}

// incompressible returns n bytes that gzip cannot shrink: a SHA-256 counter
// stream, deterministic across runs.
func incompressible(n int) []byte {
	out := make([]byte, 0, n+sha256.Size)
	var counter [8]byte
	for i := uint64(0); len(out) < n; i++ {
		binary.BigEndian.PutUint64(counter[:], i)
		sum := sha256.Sum256(counter[:])
		out = append(out, sum[:]...)
	}
	return out[:n]
}

// sequenceSource yields opens[i] on the i-th OpenBlob (the last one after
// that), to model a source that changes between the compression pass and the
// raw upload.
type sequenceSource struct {
	mu    sync.Mutex
	opens [][]byte
	n     int
}

func (s *sequenceSource) OpenBlob(string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := min(s.n, len(s.opens)-1)
	s.n++
	return io.NopCloser(bytes.NewReader(s.opens[i])), nil
}

// objectStore stands in for object storage behind presigned URLs: it keeps
// each PUT body as sent, with the Content-Length it declared, and serves it
// back on GET with its stored length.
type objectStore struct {
	mu       sync.Mutex
	objects  map[string][]byte
	declared map[string]int64
}

func newObjectStore() *objectStore {
	return &objectStore{objects: map[string][]byte{}, declared: map[string]int64{}}
}

func (s *objectStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/o/")
	switch r.Method {
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.objects[key] = body
		s.declared[key] = r.ContentLength
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		s.mu.Lock()
		body, ok := s.objects[key]
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *objectStore) get(key string) (body []byte, declared int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objects[key], s.declared[key]
}
