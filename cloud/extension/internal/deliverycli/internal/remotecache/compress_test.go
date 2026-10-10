package remotecache

import (
	"bytes"
	"io"
	"testing"

	cache "go.putnami.dev/protocol/cache"
)

func TestCompressedBody_CompressesCompressible(t *testing.T) {
	content := bytes.Repeat([]byte("putnami remote cache "), 1000)
	digest := cache.DigestOf(content)
	src := mapBlobSource{digest: content}

	body, length, cleanup, err := compressedBody(src, digest, int64(len(content)))
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

	body, length, cleanup, err := compressedBody(src, digest, int64(len(content)))
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

func TestCompressedBody_StreamsLargeRaw(t *testing.T) {
	content := []byte("large blob streamed raw without buffering")
	digest := cache.DigestOf(content)
	src := mapBlobSource{digest: content}

	// A declared size over the threshold takes the stream-raw branch.
	body, length, cleanup, err := compressedBody(src, digest, compressMaxBytes+1)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup() //nolint:errcheck

	if length != compressMaxBytes+1 {
		t.Errorf("stream-raw length = %d, want %d", length, compressMaxBytes+1)
	}
	got, _ := io.ReadAll(body)
	if !bytes.Equal(got, content) {
		t.Error("stream-raw body should equal the original content")
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
