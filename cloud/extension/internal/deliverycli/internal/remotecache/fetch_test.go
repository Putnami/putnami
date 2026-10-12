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
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	cache "go.putnami.dev/protocol/cache"
)

func TestDownloadBlob_DistinguishesGzipArtifactFromTransportCompression(t *testing.T) {
	original := bytes.Repeat([]byte("putnami cache payload\n"), 100)
	gzipped := gzipForFetchTest(t, original)
	if len(gzipped) >= len(original) {
		t.Fatal("fixture must shrink under gzip")
	}

	tests := []struct {
		name        string
		stored      []byte
		content     []byte
		digest      string
		contentSize int64
	}{
		{
			name:        "gzip artifact stays byte-identical",
			stored:      gzipped,
			content:     gzipped,
			digest:      cache.DigestOf(gzipped),
			contentSize: int64(len(gzipped)),
		},
		{
			name:        "transport gzip is decoded",
			stored:      gzipped,
			content:     original,
			digest:      cache.DigestOf(original),
			contentSize: int64(len(original)),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", strconv.Itoa(len(tt.stored)))
				_, _ = w.Write(tt.stored)
			}))
			defer srv.Close()

			client := NewClient(srv.URL, "")
			var dst bytes.Buffer
			n, err := client.DownloadBlob(context.Background(), cache.BlobTransfer{
				Digest: tt.digest, URL: srv.URL, Method: cache.TransferGet, SizeBytes: tt.contentSize,
			}, &dst)
			if err != nil {
				t.Fatalf("DownloadBlob: %v", err)
			}
			if n != int64(len(tt.content)) {
				t.Fatalf("downloaded %d bytes, want %d", n, len(tt.content))
			}
			if !bytes.Equal(dst.Bytes(), tt.content) {
				t.Fatal("downloaded bytes differ from content-addressed bytes")
			}
		})
	}
}

func TestVerifiedInlineBlob_DistinguishesGzipArtifactFromTransportCompression(t *testing.T) {
	original := bytes.Repeat([]byte("inline cache payload\n"), 100)
	gzipped := gzipForFetchTest(t, original)

	tests := []struct {
		name        string
		stored      []byte
		content     []byte
		digest      string
		contentSize int64
	}{
		{
			name:        "gzip artifact stays byte-identical",
			stored:      gzipped,
			content:     gzipped,
			digest:      cache.DigestOf(gzipped),
			contentSize: int64(len(gzipped)),
		},
		{
			name:        "transport gzip is decoded",
			stored:      gzipped,
			content:     original,
			digest:      cache.DigestOf(original),
			contentSize: int64(len(original)),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := verifiedInlineBlob(cache.InlineBlob{Digest: tt.digest, Data: tt.stored}, tt.contentSize)
			if err != nil {
				t.Fatalf("verifiedInlineBlob: %v", err)
			}
			if !bytes.Equal(got, tt.content) {
				t.Fatal("inline bytes differ from content-addressed bytes")
			}
		})
	}
}

func gzipForFetchTest(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// smallTransfers returns n distinct blobs of size bytes and their transfers.
func smallTransfers(n, size int) (map[string][]byte, []cache.BlobTransfer) {
	blobs := make(map[string][]byte, n)
	transfers := make([]cache.BlobTransfer, 0, n)
	for i := range n {
		content := bytes.Repeat([]byte{'.'}, size)
		copy(content, fmt.Sprintf("blob %d", i))
		digest := cache.DigestOf(content)
		blobs[digest] = content
		transfers = append(transfers, cache.BlobTransfer{
			Digest: digest, URL: "http://storage.invalid/" + digest, Method: cache.TransferGet, SizeBytes: int64(size),
		})
	}
	return blobs, transfers
}

// DownloadBatches keeps every request under both bounds, fills each request as
// far as they allow, and keeps the order of the blobs.
func TestDownloadBatchesKeepsEachRequestUnderBothBounds(t *testing.T) {
	sizes := func(chunks [][]cache.BlobTransfer) []int {
		out := make([]int, 0, len(chunks))
		for _, c := range chunks {
			out = append(out, len(c))
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		n     int
		size  int
		want  []int
		bytes int64
	}{
		{name: "no blob", n: 0, size: 1, want: nil},
		{name: "count bound", n: cache.MaxKeysPerRequest + 1, size: 10, want: []int{cache.MaxKeysPerRequest, 1}},
		{name: "byte bound filled exactly", n: 64, size: 64 << 10, want: []int{64}},
		{name: "byte bound", n: 33, size: 128 << 10, want: []int{32, 1}},
		{name: "largest inline blobs", n: 17, size: cache.MaxInlineBlobBytes, want: []int{16, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, transfers := smallTransfers(tc.n, tc.size)
			chunks := DownloadBatches(transfers)
			if got := sizes(chunks); !slices.Equal(got, tc.want) {
				t.Fatalf("requests hold %v blobs, want %v", got, tc.want)
			}
			var flat []cache.BlobTransfer
			for _, c := range chunks {
				var total int64
				for _, b := range c {
					total += b.SizeBytes
				}
				if total > maxDownloadBatchBytes || len(c) > cache.MaxKeysPerRequest {
					t.Fatalf("a request asks for %d blobs of %d bytes, over the bounds", len(c), total)
				}
				flat = append(flat, c...)
			}
			if !slices.EqualFunc(flat, transfers, func(a, b cache.BlobTransfer) bool { return a.Digest == b.Digest }) {
				t.Fatal("the requests do not hold the blobs in their order")
			}
		})
	}
}

// The bound leaves one download-batch answer far under the 32 MiB Cloud Run
// serves and the client reads: base64 adds a third, and each blob adds its
// digest and the JSON around it.
func TestADownloadBatchAnswerStaysFarUnderTheResponseLimits(t *testing.T) {
	const perBlobOverhead = 128
	worst := maxDownloadBatchBytes*4/3 + cache.MaxKeysPerRequest*perBlobOverhead
	if worst > 16<<20 || worst > maxResponseBytes/2 {
		t.Fatalf("a full download-batch answers in up to %d bytes, want at most 16 MiB", worst)
	}
}

// inlineServer answers download-batch with the blobs it holds, through answer,
// and records the digests of each request.
func inlineServer(t *testing.T, answer func(digest string) ([]byte, bool)) (*Client, func() [][]string) {
	t.Helper()
	var mu sync.Mutex
	var asked [][]string
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
		asked = append(asked, req.Digests)
		mu.Unlock()
		resp := cache.DownloadBatchResponse{ProtocolVersion: cache.ProtocolVersion}
		for _, d := range req.Digests {
			if data, ok := answer(d); ok {
				resp.Blobs = append(resp.Blobs, cache.InlineBlob{Digest: d, Data: data})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "tok", WithHTTPClient(srv.Client())), func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(asked)
	}
}

// DownloadInline sends one request, asks for a repeated blob once, and returns
// the verified bytes of the blobs the answer carries. A blob the answer leaves
// out is no error.
func TestDownloadInlineReturnsTheVerifiedBlobsOfTheAnswer(t *testing.T) {
	blobs, transfers := smallTransfers(3, 100)
	left := transfers[2].Digest
	c, asked := inlineServer(t, func(d string) ([]byte, bool) { return blobs[d], d != left })

	got, err := c.DownloadInline(context.Background(), append(transfers, transfers[0]))
	if err != nil {
		t.Fatalf("DownloadInline: %v", err)
	}
	if len(got) != 2 || !bytes.Equal(got[transfers[0].Digest], blobs[transfers[0].Digest]) || !bytes.Equal(got[transfers[1].Digest], blobs[transfers[1].Digest]) {
		t.Fatalf("DownloadInline returned %d blobs, want the 2 the answer carries", len(got))
	}
	if requests := asked(); len(requests) != 1 || len(requests[0]) != 3 {
		t.Fatalf("download-batch requests = %v, want one for the 3 distinct blobs", requests)
	}
}

// One blob that does not match its digest makes the whole answer an error, and
// no blob of it is returned.
func TestDownloadInlineRefusesAnAnswerWithABlobThatDoesNotVerify(t *testing.T) {
	blobs, transfers := smallTransfers(2, 100)
	bad := transfers[1].Digest
	c, _ := inlineServer(t, func(d string) ([]byte, bool) {
		if d == bad {
			return bytes.Repeat([]byte{'x'}, 100), true
		}
		return blobs[d], true
	})

	got, err := c.DownloadInline(context.Background(), transfers)
	if err == nil || !strings.Contains(err.Error(), "content-addresses to") || got != nil {
		t.Fatalf("DownloadInline = %d blobs, %v; want no blob and the digest error", len(got), err)
	}
}

// A hit whose small files hold more than one request's bytes gets them inline
// in several requests, each under the byte bound.
func TestBatchHitFetcher_AsksInRequestsOfAtMostTheByteBound(t *testing.T) {
	blobs, transfers := smallTransfers(33, 128<<10)
	c, asked := inlineServer(t, func(d string) ([]byte, bool) { b, ok := blobs[d]; return b, ok })
	fetch := c.BatchHitFetcher(context.Background(), cache.KeyResult{Key: testKey(1), Hit: true, Downloads: transfers})
	for _, tr := range transfers {
		rc, err := fetch(tr.Digest)
		if err != nil {
			t.Fatalf("fetch %s: %v", tr.Digest, err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil || !bytes.Equal(got, blobs[tr.Digest]) {
			t.Fatalf("fetch %s read %d bytes, %v", tr.Digest, len(got), err)
		}
	}
	requests := asked()
	sizes := make([]int, 0, len(requests))
	for _, r := range requests {
		sizes = append(sizes, len(r))
	}
	if want := []int{32, 1}; !slices.Equal(sizes, want) {
		t.Fatalf("download-batch asked for %v blobs, want %v", sizes, want)
	}
}
