package remotecache

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
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
