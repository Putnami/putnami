package remotecache

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
)

// Blobs move gzip-compressed when that shrinks them, but the CAS key stays the
// UNCOMPRESSED content digest — compression is invisible to content addressing.
// A blob may therefore be stored raw or gzipped interchangeably (old raw blobs
// and new gzipped ones coexist in CAS), and the download path detects the
// encoding from the gzip magic bytes and decompresses before verifying the
// digest. No Content-Encoding header is sent, so object stores never transcode
// on serve; the client owns the (de)compression end to end.

// compressMaxBytes bounds the blob size buffered to gzip for upload. Larger
// blobs stream raw — they are usually already-compressed formats (images, wasm,
// archives) where gzip wouldn't help, and buffering them would cost memory.
const compressMaxBytes = 16 << 20 // 16 MiB

// gzipLevel trades CPU for ratio. Uploads run in the background on a bounded
// pool, so the default level's better ratio is worth the cycles.
const gzipLevel = gzip.DefaultCompression

var gzipMagic = [2]byte{0x1f, 0x8b}

// compressedBody returns the bytes to PUT for a blob and their content length.
// A blob at or below compressMaxBytes is gzip-compressed when that actually
// shrinks it (so already-compressed data is sent raw); larger blobs stream raw.
// The returned cleanup releases the source and must be called when the body is
// fully read.
func compressedBody(src BlobSource, digest string, sizeBytes int64) (body io.Reader, length int64, cleanup func() error, err error) {
	if src == nil {
		return nil, 0, nil, fmt.Errorf("blob %s: no blob source", digest)
	}
	rc, err := src.OpenBlob(digest)
	if err != nil {
		return nil, 0, nil, err
	}
	if sizeBytes > compressMaxBytes {
		return rc, sizeBytes, rc.Close, nil // stream large blobs raw, unbuffered
	}

	raw, err := io.ReadAll(io.LimitReader(rc, compressMaxBytes+1))
	_ = rc.Close()
	if err != nil {
		return nil, 0, nil, err
	}
	noop := func() error { return nil }
	if int64(len(raw)) > compressMaxBytes {
		return nil, 0, nil, fmt.Errorf("blob %s: bytes exceed declared size %d", digest, sizeBytes)
	}

	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzipLevel)
	if _, err := zw.Write(raw); err != nil {
		return nil, 0, nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, 0, nil, err
	}
	if buf.Len() < len(raw) {
		return bytes.NewReader(buf.Bytes()), int64(buf.Len()), noop, nil
	}
	return bytes.NewReader(raw), int64(len(raw)), noop, nil // gzip didn't help; send raw
}

// decodeBlobReader removes the cache transport's optional gzip layer without
// mistaking an artifact that is itself a gzip file for transport compression.
// Uploads are compressed only when that makes the stored object strictly
// smaller than the manifest's uncompressed size. A stored object whose wire
// length equals the declared content length is therefore raw, even when its
// first bytes are gzip magic (for example a generated *.js.gz artifact).
//
// When either length is unavailable we retain the legacy magic-byte fallback.
// The caller still verifies the decoded bytes against the content digest, so a
// false or malicious length can only turn the entry into a miss; it cannot make
// unaddressed bytes enter the local CAS.
func decodeBlobReader(r io.Reader, storedSize, contentSize int64) (io.Reader, error) {
	if contentSize > 0 && storedSize == contentSize {
		return r, nil
	}
	return decompressBlobReader(r)
}

// decompressBlobReader wraps r so the caller reads the blob's uncompressed
// content: a stream beginning with the gzip magic is decompressed, otherwise it
// is passed through unchanged. This lets raw and gzipped CAS objects be consumed
// identically without any out-of-band encoding signal.
func decompressBlobReader(r io.Reader) (io.Reader, error) {
	br := bufio.NewReader(r)
	magic, err := br.Peek(2)
	if err != nil {
		// Fewer than two bytes available (tiny or empty blob): it cannot be gzip,
		// which needs a multi-byte header. Pass the bytes through unchanged.
		return br, nil //nolint:nilerr // a short blob is valid, not an error
	}
	if magic[0] == gzipMagic[0] && magic[1] == gzipMagic[1] {
		return gzip.NewReader(br)
	}
	return br, nil
}
