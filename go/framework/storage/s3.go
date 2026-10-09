package storage

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.putnami.dev/ctxutil"
	"go.putnami.dev/errors"
)

// S3Config configures the S3 storage backend.
type S3Config struct {
	// Endpoint is the S3-compatible endpoint URL (e.g. "https://s3.amazonaws.com").
	// Required for non-AWS S3-compatible services (MinIO, DigitalOcean Spaces, etc.).
	Endpoint string
	// Region is the AWS region. Default: "us-east-1".
	Region string
	// AccessKey is the AWS access key ID for authentication.
	AccessKey string
	// SecretKey is the AWS secret access key for authentication.
	SecretKey string
	// Bucket is the S3 bucket name. All logical buckets are mapped as key prefixes within this bucket.
	Bucket string
	// RequestTimeout bounds non-streaming control-plane operations (Put, Delete,
	// List, Exists, Stat). Default: 30s. It does NOT cap a streaming Get — that body
	// read is bounded only by the caller's context, so large/slow downloads are
	// not aborted mid-stream. Set to a negative value to disable the per-op cap.
	RequestTimeout time.Duration
}

func (c S3Config) withDefaults() S3Config {
	if c.Region == "" {
		c.Region = "us-east-1"
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = 30 * time.Second
	}
	return c
}

// S3Backend implements the Backend interface using an S3-compatible service.
// This is a lightweight implementation using net/http for basic operations.
// For production use, consider wrapping the AWS SDK v2 client.
type S3Backend struct {
	config S3Config
	client *http.Client
}

// NewS3Backend creates an S3-compatible storage backend.
func NewS3Backend(config S3Config) *S3Backend {
	return &S3Backend{
		config: config.withDefaults(),
		client: newStorageHTTPClient(),
	}
}

func (b *S3Backend) objectURL(bucket, key string) string {
	return fmt.Sprintf("%s/%s/%s", strings.TrimRight(b.config.Endpoint, "/"), uriEncode(bucket), encodeS3Path(key))
}

// Put stores an object in S3. Sized readers are streamed directly to the wire
// with a Content-Length and UNSIGNED-PAYLOAD, which is the SigV4 single-payload
// mode. Unsized unsigned uploads may use HTTP chunked transfer. Unsized signed
// uploads are first spooled to a temporary file so the actual S3 request still
// has a Content-Length; generic HTTP chunked transfer is not valid with
// UNSIGNED-PAYLOAD and real S3 requires the separate aws-chunked SigV4 protocol
// for chunked signed bodies.
func (b *S3Backend) Put(ctx context.Context, bucket, key string, data io.Reader, meta *ObjectMetadata) (_ *PutResult, retErr error) {
	ctx, cancel := ctxutil.WithRequestTimeout(ctx, b.config.RequestTimeout)
	defer cancel()

	body := data
	contentLength, hasContentLength := readerContentLength(data)
	var tmp *os.File
	if b.hasCredentials() && !hasContentLength {
		var err error
		tmp, contentLength, err = spoolReaderToTemp(data)
		if err != nil {
			return nil, err
		}
		defer func() {
			name := tmp.Name()
			if cerr := tmp.Close(); cerr != nil && retErr == nil {
				retErr = errors.Wrap(cerr, CodeStorageRequest, errors.String("backend", "s3"), errors.String("op", "close_temp_upload"))
			}
			_ = os.Remove(name) //nolint:errcheck,gosec // best-effort cleanup of a local temp spool created with os.CreateTemp
		}()
		body = tmp
		hasContentLength = true
	}

	// Count bytes as they stream past so Size reflects the uploaded length
	// without retaining the body.
	counter := &countingReader{r: body}

	url := b.objectURL(bucket, key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, counter)
	if err != nil {
		return nil, errors.Wrap(err, CodeStorageRequest, errors.String("backend", "s3"), errors.String("method", "PUT"))
	}
	if hasContentLength {
		req.ContentLength = contentLength
	}

	if meta != nil && meta.ContentType != "" {
		req.Header.Set("Content-Type", meta.ContentType)
	}

	b.signRequest(req, unsignedPayload)

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, CodeStorageWrite, errors.String("backend", "s3"), errors.String("bucket", bucket), errors.String("key", key))
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && retErr == nil {
			retErr = errors.Wrap(cerr, CodeStorageRequest, errors.String("backend", "s3"), errors.String("op", "close_response"))
		}
	}()

	if resp.StatusCode >= 400 {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, errors.Wrapf(err, CodeStorageWrite, "put failed: read body", errors.String("backend", "s3"), errors.Int("status", resp.StatusCode))
		}
		return nil, errors.New(CodeStorageWrite, "put failed", errors.String("backend", "s3"), errors.Int("status", resp.StatusCode), errors.String("body", string(body)))
	}

	return &PutResult{
		Key:  key,
		Size: counter.n,
		ETag: resp.Header.Get("ETag"),
	}, nil
}

// countingReader wraps an io.Reader, tallying the total bytes read so a streamed
// upload can report its Size without buffering the body.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	read, err := c.r.Read(p)
	c.n += int64(read)
	return read, err
}

func (b *S3Backend) hasCredentials() bool {
	return b.config.AccessKey != "" && b.config.SecretKey != ""
}

type lenReader interface {
	Len() int
}

func readerContentLength(r io.Reader) (int64, bool) {
	if lr, ok := r.(lenReader); ok {
		return int64(lr.Len()), true
	}
	seeker, ok := r.(io.Seeker)
	if !ok {
		return 0, false
	}
	cur, err := seeker.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, false
	}
	end, err := seeker.Seek(0, io.SeekEnd)
	if _, restoreErr := seeker.Seek(cur, io.SeekStart); err != nil || restoreErr != nil {
		return 0, false
	}
	if end < cur {
		return 0, false
	}
	return end - cur, true
}

func spoolReaderToTemp(r io.Reader) (*os.File, int64, error) {
	f, err := os.CreateTemp("", "putnami-s3-upload-*")
	if err != nil {
		return nil, 0, errors.Wrap(err, CodeStorageRequest, errors.String("backend", "s3"), errors.String("op", "create_temp_upload"))
	}
	n, err := io.Copy(f, r)
	if err != nil {
		name := f.Name()
		_ = f.Close()       //nolint:errcheck // returning the read/copy error
		_ = os.Remove(name) //nolint:errcheck,gosec // best-effort cleanup of a path returned by os.CreateTemp
		return nil, 0, errors.Wrap(err, CodeStorageRead, errors.String("backend", "s3"), errors.String("op", "spool_upload"))
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		name := f.Name()
		_ = f.Close()       //nolint:errcheck // returning the seek error
		_ = os.Remove(name) //nolint:errcheck,gosec // best-effort cleanup of a path returned by os.CreateTemp
		return nil, 0, errors.Wrap(err, CodeStorageRequest, errors.String("backend", "s3"), errors.String("op", "rewind_temp_upload"))
	}
	return f, n, nil
}

// Get retrieves an object from S3. The returned Body is a live stream, so its
// read is bounded only by ctx — not by a fixed client timeout — to avoid
// aborting large/slow downloads mid-stream.
func (b *S3Backend) Get(ctx context.Context, bucket, key string) (*GetResult, error) {
	url := b.objectURL(bucket, key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, errors.Wrap(err, CodeStorageRequest, errors.String("backend", "s3"), errors.String("method", "GET"))
	}

	b.signRequest(req, sha256Hex(nil))

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, CodeStorageRead, errors.String("backend", "s3"), errors.String("bucket", bucket), errors.String("key", key))
	}

	if resp.StatusCode == http.StatusNotFound {
		if err := resp.Body.Close(); err != nil {
			return nil, errors.Wrap(err, CodeStorageRequest, errors.String("backend", "s3"), errors.String("op", "close_response"))
		}
		return nil, nil
	}
	if resp.StatusCode >= 400 {
		body, err := io.ReadAll(resp.Body)
		if closeErr := resp.Body.Close(); closeErr != nil && err == nil {
			return nil, errors.Wrap(closeErr, CodeStorageRequest, errors.String("backend", "s3"), errors.String("op", "close_response"))
		}
		if err != nil {
			return nil, errors.Wrapf(err, CodeStorageRead, "get failed: read body", errors.String("backend", "s3"), errors.Int("status", resp.StatusCode))
		}
		return nil, errors.New(CodeStorageRead, "get failed", errors.String("backend", "s3"), errors.Int("status", resp.StatusCode), errors.String("body", string(body)))
	}

	return &GetResult{
		Key:         key,
		Body:        resp.Body,
		Size:        resp.ContentLength,
		ContentType: resp.Header.Get("Content-Type"),
		ETag:        resp.Header.Get("ETag"),
	}, nil
}

// Delete removes an object from S3.
func (b *S3Backend) Delete(ctx context.Context, bucket, key string) (retErr error) {
	ctx, cancel := ctxutil.WithRequestTimeout(ctx, b.config.RequestTimeout)
	defer cancel()

	url := b.objectURL(bucket, key)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return errors.Wrap(err, CodeStorageRequest, errors.String("backend", "s3"), errors.String("method", "DELETE"))
	}

	b.signRequest(req, sha256Hex(nil))

	resp, err := b.client.Do(req)
	if err != nil {
		return errors.Wrap(err, CodeStorageDelete, errors.String("backend", "s3"), errors.String("bucket", bucket), errors.String("key", key))
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil && retErr == nil {
			retErr = errors.Wrap(closeErr, CodeStorageRequest, errors.String("backend", "s3"), errors.String("op", "close_response"))
		}
	}()

	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return errors.Wrapf(err, CodeStorageDelete, "delete failed: read body", errors.String("backend", "s3"), errors.Int("status", resp.StatusCode))
		}
		return errors.New(CodeStorageDelete, "delete failed", errors.String("backend", "s3"), errors.Int("status", resp.StatusCode), errors.String("body", string(body)))
	}
	return nil
}

// listBucketResult maps the S3 ListObjectsV2 XML response.
type listBucketResult struct {
	XMLName               xml.Name           `xml:"ListBucketResult"`
	IsTruncated           bool               `xml:"IsTruncated"`
	Contents              []listBucketObject `xml:"Contents"`
	CommonPrefixes        []listCommonPrefix `xml:"CommonPrefixes"`
	NextContinuationToken string             `xml:"NextContinuationToken"`
}

type listBucketObject struct {
	Key          string `xml:"Key"`
	Size         int64  `xml:"Size"`
	ETag         string `xml:"ETag"`
	LastModified string `xml:"LastModified"`
}

type listCommonPrefix struct {
	Prefix string `xml:"Prefix"`
}

// List returns objects in S3 matching the given options using the ListObjectsV2 API.
func (b *S3Backend) List(ctx context.Context, bucket string, opts *ListOptions) (_ *ListResult, retErr error) {
	ctx, cancel := ctxutil.WithRequestTimeout(ctx, b.config.RequestTimeout)
	defer cancel()

	if opts == nil {
		opts = &ListOptions{}
	}

	endpoint := strings.TrimRight(b.config.Endpoint, "/")
	u, err := url.Parse(fmt.Sprintf("%s/%s", endpoint, uriEncode(bucket)))
	if err != nil {
		return nil, errors.Wrap(err, CodeStorageList, errors.String("backend", "s3"), errors.String("bucket", bucket))
	}

	q := u.Query()
	q.Set("list-type", "2")
	if opts.Prefix != "" {
		q.Set("prefix", opts.Prefix)
	}
	if opts.Delimiter != "" {
		q.Set("delimiter", opts.Delimiter)
	}
	if opts.MaxKeys > 0 {
		q.Set("max-keys", strconv.Itoa(opts.MaxKeys))
	}
	if opts.ContinuationToken != "" {
		q.Set("continuation-token", opts.ContinuationToken)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.Wrap(err, CodeStorageRequest, errors.String("backend", "s3"), errors.String("method", "GET"))
	}

	b.signRequest(req, sha256Hex(nil))

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, CodeStorageList, errors.String("backend", "s3"), errors.String("bucket", bucket))
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && retErr == nil {
			retErr = errors.Wrap(cerr, CodeStorageRequest, errors.String("backend", "s3"), errors.String("op", "close_response"))
		}
	}()

	if resp.StatusCode >= 400 {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, errors.Wrapf(err, CodeStorageList, "list failed: read body", errors.String("backend", "s3"), errors.Int("status", resp.StatusCode))
		}
		return nil, errors.New(CodeStorageList, "list failed", errors.String("backend", "s3"), errors.Int("status", resp.StatusCode), errors.String("body", string(body)))
	}

	var xmlResult listBucketResult
	if err := xml.NewDecoder(resp.Body).Decode(&xmlResult); err != nil {
		return nil, errors.Wrap(err, CodeStorageList, errors.String("backend", "s3"), errors.String("op", "decode_xml"))
	}

	objects := make([]ObjectInfo, 0, len(xmlResult.Contents))
	for _, obj := range xmlResult.Contents {
		info := ObjectInfo{
			Key:  obj.Key,
			Size: obj.Size,
			ETag: obj.ETag,
		}
		if t, err := time.Parse(time.RFC3339, obj.LastModified); err == nil {
			info.LastModified = t
		}
		objects = append(objects, info)
	}
	sort.Sort(byKey(objects))

	prefixes := make([]string, 0, len(xmlResult.CommonPrefixes))
	for _, cp := range xmlResult.CommonPrefixes {
		prefixes = append(prefixes, cp.Prefix)
	}
	sort.Strings(prefixes)

	return &ListResult{
		Objects:           objects,
		Prefixes:          prefixes,
		IsTruncated:       xmlResult.IsTruncated,
		ContinuationToken: xmlResult.NextContinuationToken,
	}, nil
}

// Exists checks whether an object exists in S3 using a HEAD request.
func (b *S3Backend) Exists(ctx context.Context, bucket, key string) (bool, error) {
	info, _, err := b.head(ctx, bucket, key, "exists check failed")
	return info != nil, err
}

// Stat returns the metadata of an object in S3 with one HEAD request
// (HeadObject). A status other than 200 and 404, such as the 301 of a
// wrong-region endpoint, is an error rather than an absent object.
func (b *S3Backend) Stat(ctx context.Context, bucket, key string) (*ObjectInfo, error) {
	info, status, err := b.head(ctx, bucket, key, "stat failed")
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, statNotFound("s3", bucket, key)
	}
	if info == nil {
		return nil, errors.New(CodeStorageRead, "stat failed",
			errors.String("backend", "s3"), errors.String("bucket", bucket),
			errors.String("key", key), errors.Int("status", status))
	}
	return info, nil
}

// head sends one HEAD request for an object and returns its status. It returns
// object information only for a 200, and an error for a status of 400 or more
// other than 404.
func (b *S3Backend) head(ctx context.Context, bucket, key, failure string) (_ *ObjectInfo, status int, retErr error) {
	ctx, cancel := ctxutil.WithRequestTimeout(ctx, b.config.RequestTimeout)
	defer cancel()

	url := b.objectURL(bucket, key)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return nil, 0, errors.Wrap(err, CodeStorageRequest, errors.String("backend", "s3"), errors.String("method", "HEAD"))
	}

	b.signRequest(req, sha256Hex(nil))

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, 0, errors.Wrap(err, CodeStorageRead, errors.String("backend", "s3"), errors.String("bucket", bucket), errors.String("key", key))
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil && retErr == nil {
			retErr = errors.Wrap(closeErr, CodeStorageRequest, errors.String("backend", "s3"), errors.String("op", "close_response"))
		}
	}()

	if resp.StatusCode == http.StatusNotFound {
		return nil, resp.StatusCode, nil
	}
	if resp.StatusCode >= 400 {
		// Surface 403 (missing s3:HeadObject grant / policy deny) and transient
		// 5xx as errors instead of collapsing them to "absent". A false-negative
		// existence check drives create/overwrite decisions and is a
		// data-integrity risk; this mirrors Get/Delete and GCSBackend.Exists.
		return nil, resp.StatusCode, errors.New(CodeStorageRead, failure,
			errors.String("backend", "s3"),
			errors.String("bucket", bucket),
			errors.String("key", key),
			errors.Int("status", resp.StatusCode))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, nil
	}

	lastModified, _ := http.ParseTime(resp.Header.Get("Last-Modified")) //nolint:errcheck // a missing or malformed date yields the zero time
	return &ObjectInfo{
		Key:          key,
		Size:         resp.ContentLength,
		ContentType:  resp.Header.Get("Content-Type"),
		ETag:         resp.Header.Get("ETag"),
		LastModified: lastModified,
	}, resp.StatusCode, nil
}

// Copy duplicates an object within S3. Uses get+put for simplicity.
func (b *S3Backend) Copy(ctx context.Context, bucket, source, destination string) (retErr error) {
	result, err := b.Get(ctx, bucket, source)
	if err != nil {
		return err
	}
	if result == nil {
		return errors.New(CodeStorageNotFound, "source not found", errors.String("backend", "s3"), errors.String("bucket", bucket), errors.String("key", source))
	}
	defer func() {
		if closeErr := result.Body.Close(); closeErr != nil && retErr == nil {
			retErr = errors.Wrap(closeErr, CodeStorageRequest, errors.String("backend", "s3"), errors.String("op", "close_source"))
		}
	}()

	_, err = b.Put(ctx, bucket, destination, result.Body, &ObjectMetadata{
		ContentType: result.ContentType,
	})
	return err
}

// Close releases the S3 client resources.
func (b *S3Backend) Close() error {
	b.client.CloseIdleConnections()
	return nil
}

// Ensure S3Backend implements URLSigner at compile time.
var (
	_ Backend   = (*S3Backend)(nil)
	_ Stater    = (*S3Backend)(nil)
	_ URLSigner = (*S3Backend)(nil)
)

// SignedGetURL returns a query-string SigV4 pre-signed URL for downloading an
// object. Requires AccessKey and SecretKey to be configured.
func (b *S3Backend) SignedGetURL(_ context.Context, bucket, key string, opts SignedURLOptions) (string, error) {
	return b.presignedURL(http.MethodGet, bucket, key, opts.Expiry, "")
}

// SignedPutURL returns a query-string SigV4 pre-signed URL for uploading an
// object. When opts.ContentType is set, it is bound into the signature so the
// upload is restricted to that content type. Requires credentials.
func (b *S3Backend) SignedPutURL(_ context.Context, bucket, key string, opts SignedURLOptions) (string, error) {
	return b.presignedURL(http.MethodPut, bucket, key, opts.Expiry, opts.ContentType)
}

// Ensure S3Backend and helpers implement the sort interface at compile time.
var _ sort.Interface = byKey(nil)

type byKey []ObjectInfo

func (a byKey) Len() int           { return len(a) }
func (a byKey) Less(i, j int) bool { return a[i].Key < a[j].Key }
func (a byKey) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
