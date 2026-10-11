package remotecache

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
)

// Blobs move gzip-compressed when that shrinks them, but the CAS key stays the
// UNCOMPRESSED content digest — compression is invisible to content addressing.
// A blob may therefore be stored raw or gzipped interchangeably (old raw blobs
// and new gzipped ones coexist in CAS), and the download path detects the
// encoding from the stored length and the gzip magic bytes and decompresses
// before verifying the digest. No Content-Encoding header is sent, so object
// stores never transcode on serve; the client owns the (de)compression end to
// end.
//
// Every blob is offered to gzip, whatever its size: the large ones are mostly
// executables, which shrink about twice. A stored gzip object is always
// strictly smaller than the blob it encodes, which is the rule the reader uses
// to tell it from a raw blob that happens to be a gzip file.

// compressInMemoryMaxBytes bounds the blob size compressed in memory. A larger
// blob streams through gzip into a temporary spool file, so memory stays
// bounded at any size; a smaller one skips the file, which a build's many small
// outputs would otherwise each pay for.
const compressInMemoryMaxBytes = 1 << 20 // 1 MiB

// gzipLevel trades CPU for ratio. On a 39 MiB linux/amd64 Go executable,
// BestSpeed spends 0.29 s of CPU to reach 48.9 % of the raw size and
// DefaultCompression 0.90 s to reach 45.1 %: three times the CPU on the CI
// machine that uploads, for 8 % fewer bytes.
const gzipLevel = gzip.BestSpeed

// spoolPattern names the temporary files large blobs compress into, in the OS
// temporary directory.
const spoolPattern = "putnami-cache-upload-*.gz"

// spoolBufferBytes batches the gzip writer's small writes into few file writes.
const spoolBufferBytes = 64 << 10

var gzipMagic = [2]byte{0x1f, 0x8b}

// errNotSmaller stops compression once the gzip stream reaches the blob's size:
// it can no longer end strictly smaller, so the blob is sent raw.
var errNotSmaller = errors.New("gzip stream is not smaller than the blob")

// compressedBody returns the request entity to PUT for a blob and its exact
// length, to send as the Content-Length. The entity is the gzip form when that
// is strictly smaller than the blob, else the raw bytes. The blob must be
// exactly sizeBytes long; a source that holds another count fails here or
// fails the body read, so no request ever carries a length it does not send.
//
// A blob up to compressInMemoryMaxBytes is compressed in memory. A larger one
// streams through gzip into a spool file that the returned cleanup removes;
// when gzip does not shrink it, the spool is removed at once and the blob
// streams raw from a second open of the source. Canceling ctx stops the
// compression. The returned cleanup releases the source and the spool and
// must be called once the body is no longer read, whatever the upload's
// outcome; on an error nothing is left to clean up.
func compressedBody(ctx context.Context, src BlobSource, digest string, sizeBytes int64) (body io.Reader, length int64, cleanup func() error, err error) {
	if src == nil {
		return nil, 0, nil, fmt.Errorf("blob %s: no blob source", digest)
	}
	if sizeBytes <= compressInMemoryMaxBytes {
		return compressInMemory(src, digest, sizeBytes)
	}
	return compressToSpool(ctx, src, digest, sizeBytes)
}

// compressInMemory is compressedBody for a blob small enough to buffer.
func compressInMemory(src BlobSource, digest string, sizeBytes int64) (io.Reader, int64, func() error, error) {
	rc, err := src.OpenBlob(digest)
	if err != nil {
		return nil, 0, nil, err
	}
	// One byte past the declared size, so a longer source is detected.
	raw, err := io.ReadAll(io.LimitReader(rc, sizeBytes+1))
	_ = rc.Close()
	if err != nil {
		return nil, 0, nil, err
	}
	if int64(len(raw)) != sizeBytes {
		return nil, 0, nil, sizeMismatch(digest, sizeBytes)
	}

	noop := func() error { return nil }
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzipLevel) // gzipLevel is a valid level: no error
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

// compressToSpool is compressedBody for a blob too large to buffer.
func compressToSpool(ctx context.Context, src BlobSource, digest string, sizeBytes int64) (io.Reader, int64, func() error, error) {
	rc, err := src.OpenBlob(digest)
	if err != nil {
		return nil, 0, nil, err
	}
	spool, err := os.CreateTemp("", spoolPattern)
	if err != nil {
		_ = rc.Close()
		return nil, 0, nil, fmt.Errorf("blob %s: create spool file: %w", digest, err)
	}
	removeSpool := func() error {
		return errors.Join(spool.Close(), os.Remove(spool.Name()))
	}

	raw, compressed, err := gzipTo(ctx, spool, rc, sizeBytes)
	_ = rc.Close()
	switch {
	case errors.Is(err, errNotSmaller):
		// Fall through to the raw stream below.
	case err != nil:
		_ = removeSpool()
		return nil, 0, nil, fmt.Errorf("blob %s: compress: %w", digest, err)
	case raw != sizeBytes:
		_ = removeSpool()
		return nil, 0, nil, sizeMismatch(digest, sizeBytes)
	default:
		// gzipTo stopped any stream that reached sizeBytes, so the spool is
		// strictly smaller than the blob. Each read of the entity is its own
		// section of the spool, so a replay never shares an offset with the
		// read it replaces.
		open := func() (io.ReadCloser, error) {
			section := io.NewSectionReader(spool, 0, compressed)
			return io.NopCloser(&exactReader{r: section, remaining: compressed}), nil
		}
		first, _ := open() // a section of an open file: no error
		return &streamedBody{Reader: first, reopen: open}, compressed, removeSpool, nil
	}

	// gzip does not shrink this blob (an archive, an image): free the spool
	// before the upload, and stream the blob as it is.
	_ = removeSpool()
	open := func() (io.ReadCloser, error) { return openExact(src, digest, sizeBytes) }
	first, err := open()
	if err != nil {
		return nil, 0, nil, err
	}
	return &streamedBody{Reader: first, reopen: open}, sizeBytes, first.Close, nil
}

// openExact opens the blob as a stream that yields exactly sizeBytes bytes.
func openExact(src BlobSource, digest string, sizeBytes int64) (io.ReadCloser, error) {
	rc, err := src.OpenBlob(digest)
	if err != nil {
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{&exactReader{r: rc, remaining: sizeBytes}, rc}, nil
}

// streamedBody is a request entity read from a file: the spool, or the blob
// itself. The embedded Reader yields it once; reopen yields it again from its
// first byte, for the transport to resend the request on a fresh connection
// (see replayable).
type streamedBody struct {
	io.Reader
	reopen func() (io.ReadCloser, error)
}

// replayable lets the HTTP transport resend req when body can be read again.
// Without it one HTTP/2 GOAWAY, or a kept-alive connection the server closed,
// fails the upload of a streamed blob, and a failed blob fails its whole
// batch. A buffered body needs nothing here: net/http replays the in-memory
// readers by itself.
func replayable(req *http.Request, body io.Reader) {
	if streamed, ok := body.(*streamedBody); ok {
		req.GetBody = streamed.reopen
	}
}

// gzipTo compresses r into w at gzipLevel and returns the bytes it read from r
// and the bytes it wrote to w. It stops with errNotSmaller as soon as the
// compressed stream reaches limit bytes, and with ctx's error once ctx is done.
func gzipTo(ctx context.Context, w io.Writer, r io.Reader, limit int64) (raw, compressed int64, err error) {
	buffered := bufio.NewWriterSize(w, spoolBufferBytes)
	bounded := &boundedWriter{w: buffered, limit: limit}
	zw, _ := gzip.NewWriterLevel(bounded, gzipLevel) // gzipLevel is a valid level: no error
	raw, err = io.Copy(zw, contextReader{ctx: ctx, r: r, report: uploadProgressFrom(ctx)})
	if err == nil {
		err = zw.Close()
	}
	if err == nil {
		err = buffered.Flush()
	}
	return raw, bounded.n, err
}

// boundedWriter passes writes through while their total stays below limit,
// and fails with errNotSmaller on the write that would reach it.
type boundedWriter struct {
	w        io.Writer
	n, limit int64
}

func (b *boundedWriter) Write(p []byte) (int, error) {
	if b.n+int64(len(p)) >= b.limit {
		return 0, errNotSmaller
	}
	n, err := b.w.Write(p)
	b.n += int64(n)
	return n, err
}

// contextReader fails its reads once ctx is done, so a canceled upload stops
// compressing instead of finishing a spool nobody will send. report, when
// set, is the context's upload progress hook: compressing a large blob takes
// seconds, and a caller that waits for uploads must see that it moves.
type contextReader struct {
	ctx    context.Context
	r      io.Reader
	report func()
}

func (c contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.r.Read(p)
	if n > 0 && c.report != nil {
		c.report()
	}
	return n, err
}

// exactReader yields the bytes of r and fails, rather than ending early or
// late, unless r holds exactly remaining bytes. HTTP/1.1 checks a request body
// against its Content-Length itself; HTTP/2 sends a short body as complete,
// so the upload enforces the length on its own side.
type exactReader struct {
	r         io.Reader
	remaining int64
}

func (e *exactReader) Read(p []byte) (int, error) {
	if e.remaining == 0 {
		// The declared length is sent: the source must be at its end.
		var extra [1]byte
		n, err := io.ReadFull(e.r, extra[:])
		if n > 0 {
			return 0, errors.New("blob source is longer than its declared size")
		}
		return 0, err // io.EOF on a source of the declared size
	}
	if int64(len(p)) > e.remaining {
		p = p[:e.remaining]
	}
	n, err := e.r.Read(p)
	e.remaining -= int64(n)
	if errors.Is(err, io.EOF) && e.remaining > 0 {
		return n, fmt.Errorf("blob source ended %d bytes short of its declared size: %w", e.remaining, io.ErrUnexpectedEOF)
	}
	if errors.Is(err, io.EOF) {
		err = nil // the next read confirms the source is at its end
	}
	return n, err
}

// sizeMismatch reports a blob source that does not hold the declared size.
func sizeMismatch(digest string, sizeBytes int64) error {
	return fmt.Errorf("blob %s: bytes are not the declared size %d", digest, sizeBytes)
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
