package storage

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.putnami.dev/ctxutil"
	"go.putnami.dev/errors"
)

// GCSConfig configures the Google Cloud Storage backend.
type GCSConfig struct {
	// ProjectID is the GCP project ID. Optional; object operations do not require
	// it because bucket names are globally unique.
	ProjectID string
	// CredentialsFile is the path to a service account JSON key file. Leave empty
	// to use Application Default Credentials: the GOOGLE_APPLICATION_CREDENTIALS
	// environment variable if set, otherwise the GCP metadata server (Workload
	// Identity on Cloud Run / GKE), which needs no static secret.
	CredentialsFile string
	// RequestTimeout bounds non-streaming control-plane operations (Put, Delete,
	// List, Exists, Stat) and the auth token fetches they trigger. Default: 60s. It does
	// NOT cap a streaming Get — that body read is bounded only by the caller's
	// context. Set to a negative value to disable the per-op cap.
	RequestTimeout time.Duration
}

// GCSBackend implements the Backend interface against Google Cloud Storage using
// the native JSON API over net/http. It has no third-party dependencies: auth
// (Workload Identity or a service-account key) and V4 URL signing are
// implemented directly against the wire format. Each logical bucket name maps
// directly to a GCS bucket.
type GCSBackend struct {
	httpClient     *http.Client
	endpoint       string // JSON/XML API base, default https://storage.googleapis.com
	metadataHost   string // GCP metadata server base, default http://metadata.google.internal
	iamEndpoint    string // IAM Credentials API base, default https://iamcredentials.googleapis.com
	tokens         tokenSource
	requestTimeout time.Duration // per-op cap for non-streaming operations; 0 means none
	signMu         sync.Mutex
	signEmail      string          // service-account email used in signed-URL credentials
	signKey        *rsa.PrivateKey // RSA key for local signing; nil means keyless (IAM SignBlob)
}

// Ensure GCSBackend implements Backend at compile time.
var (
	_ Backend = (*GCSBackend)(nil)
	_ Stater  = (*GCSBackend)(nil)
)

// NewGCSBackend creates a Google Cloud Storage backend. Authentication uses the
// service-account key at cfg.CredentialsFile when set (or the path in
// GOOGLE_APPLICATION_CREDENTIALS), otherwise Application Default Credentials via
// the metadata server (Workload Identity).
func NewGCSBackend(_ context.Context, cfg GCSConfig) (*GCSBackend, error) {
	requestTimeout := cfg.RequestTimeout
	if requestTimeout == 0 {
		requestTimeout = 60 * time.Second
	}
	backend := &GCSBackend{
		httpClient:     newStorageHTTPClient(),
		endpoint:       gcsDefaultEndpoint,
		metadataHost:   gcsDefaultMetadata,
		iamEndpoint:    gcsDefaultIAM,
		requestTimeout: requestTimeout,
	}

	credentialsFile := cfg.CredentialsFile
	if credentialsFile == "" {
		credentialsFile = os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	}

	if credentialsFile != "" {
		data, err := os.ReadFile(credentialsFile) //nolint:gosec // G304: credentials path is operator-provided configuration, not user input
		if err != nil {
			return nil, errors.Wrap(err, CodeStorageRequest,
				errors.String("backend", "gcs"), errors.String("op", "read_credentials"))
		}
		key, signKey, err := parseServiceAccountKey(data)
		if err != nil {
			return nil, err
		}
		backend.tokens = newServiceAccountTokenSource(backend.httpClient, key, signKey)
		backend.signEmail = key.ClientEmail
		backend.signKey = signKey
		return backend, nil
	}

	// Application Default Credentials via the metadata server. Signed URLs use the
	// IAM SignBlob API (the service-account email is resolved on demand).
	backend.tokens = newMetadataTokenSource(backend.httpClient, backend.metadataHost)
	return backend, nil
}

// base returns the API endpoint without a trailing slash.
func (b *GCSBackend) base() string {
	if b.endpoint == "" {
		return gcsDefaultEndpoint
	}
	return strings.TrimRight(b.endpoint, "/")
}

// gcsEscapeObject percent-encodes an object name for use as a single path
// segment in a JSON API URL. url.PathEscape encodes "/" as %2F, so nested keys
// stay within the {object} segment instead of being read as extra path parts.
func gcsEscapeObject(key string) string {
	return url.PathEscape(key)
}

func (b *GCSBackend) objectURL(bucket, key string) string {
	return fmt.Sprintf("%s/storage/v1/b/%s/o/%s", b.base(), url.PathEscape(bucket), gcsEscapeObject(key))
}

func (b *GCSBackend) uploadURL(bucket string) string {
	return fmt.Sprintf("%s/upload/storage/v1/b/%s/o?uploadType=multipart", b.base(), url.PathEscape(bucket))
}

func (b *GCSBackend) listURL(bucket string) string {
	return fmt.Sprintf("%s/storage/v1/b/%s/o", b.base(), url.PathEscape(bucket))
}

func (b *GCSBackend) rewriteURL(bucket, source, destination string) string {
	return fmt.Sprintf("%s/storage/v1/b/%s/o/%s/rewriteTo/b/%s/o/%s",
		b.base(), url.PathEscape(bucket), gcsEscapeObject(source), url.PathEscape(bucket), gcsEscapeObject(destination))
}

// authorize attaches a bearer token to the request when a token source is
// configured and yields a non-empty token. A nil token source (tests) sends the
// request unauthenticated.
func (b *GCSBackend) authorize(ctx context.Context, req *http.Request) error {
	if b.tokens == nil {
		return nil
	}
	token, err := b.tokens.token(ctx)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return nil
}

// gcsObject maps a storage#object resource. The JSON API encodes size as a
// string, so it is parsed via size() rather than decoded as a number.
type gcsObject struct {
	Name        string            `json:"name"`
	Bucket      string            `json:"bucket"`
	ContentType string            `json:"contentType"`
	Size        string            `json:"size"`
	Etag        string            `json:"etag"`
	Updated     string            `json:"updated"`
	Metadata    map[string]string `json:"metadata"`
}

func (o *gcsObject) size() int64 {
	n, _ := strconv.ParseInt(o.Size, 10, 64) //nolint:errcheck // empty/garbage size yields 0, which is acceptable
	return n
}

func (o *gcsObject) updatedTime() time.Time {
	t, _ := time.Parse(time.RFC3339, o.Updated) //nolint:errcheck // empty/garbage time yields the zero value
	return t
}

// gcsListResponse maps a storage#objects list page.
type gcsListResponse struct {
	Items         []gcsObject `json:"items"`
	Prefixes      []string    `json:"prefixes"`
	NextPageToken string      `json:"nextPageToken"`
}

// Put stores an object in GCS using a streaming multipart upload, so the body is
// never buffered fully in memory.
func (b *GCSBackend) Put(ctx context.Context, bucket, key string, data io.Reader, meta *ObjectMetadata) (_ *PutResult, retErr error) {
	ctx, cancel := ctxutil.WithRequestTimeout(ctx, b.requestTimeout)
	defer cancel()

	resource := map[string]any{"name": key}
	contentType := "application/octet-stream"
	if meta != nil {
		if meta.ContentType != "" {
			resource["contentType"] = meta.ContentType
			contentType = meta.ContentType
		}
		if meta.CacheControl != "" {
			resource["cacheControl"] = meta.CacheControl
		}
		if meta.ContentDisposition != "" {
			resource["contentDisposition"] = meta.ContentDisposition
		}
		if len(meta.Custom) > 0 {
			resource["metadata"] = meta.Custom
		}
	}

	reader, writer := io.Pipe()
	multipartWriter := multipart.NewWriter(writer)
	boundary := multipartWriter.Boundary()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.uploadURL(bucket), reader)
	if err != nil {
		return nil, errors.Wrap(err, CodeStorageRequest, errors.String("backend", "gcs"), errors.String("method", "POST"))
	}
	req.Header.Set("Content-Type", "multipart/related; boundary="+boundary)
	if err := b.authorize(ctx, req); err != nil {
		return nil, err
	}

	go func() {
		writeErr := streamMultipartUpload(multipartWriter, resource, contentType, data)
		if writeErr != nil {
			_ = writer.CloseWithError(writeErr) //nolint:errcheck // io.PipeWriter.CloseWithError always returns nil
			return
		}
		_ = writer.Close() //nolint:errcheck // io.PipeWriter.Close always returns nil
	}()

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, CodeStorageWrite,
			errors.String("backend", "gcs"), errors.String("bucket", bucket), errors.String("key", key))
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && retErr == nil {
			retErr = errors.Wrap(cerr, CodeStorageRequest, errors.String("backend", "gcs"), errors.String("op", "close_response"))
		}
	}()

	if resp.StatusCode >= 400 {
		return nil, httpError(resp, CodeStorageWrite, bucket, key)
	}

	var obj gcsObject
	if err := json.NewDecoder(resp.Body).Decode(&obj); err != nil {
		return nil, errors.Wrap(err, CodeStorageWrite,
			errors.String("backend", "gcs"), errors.String("bucket", bucket), errors.String("key", key))
	}
	return &PutResult{Key: key, Size: obj.size(), ETag: obj.Etag}, nil
}

// streamMultipartUpload writes the JSON metadata part followed by the streamed
// object bytes into a multipart/related body.
func streamMultipartUpload(writer *multipart.Writer, resource map[string]any, contentType string, data io.Reader) error {
	metaHeader := textproto.MIMEHeader{}
	metaHeader.Set("Content-Type", "application/json; charset=UTF-8")
	metaPart, err := writer.CreatePart(metaHeader)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(metaPart).Encode(resource); err != nil {
		return err
	}

	mediaHeader := textproto.MIMEHeader{}
	mediaHeader.Set("Content-Type", contentType)
	mediaPart, err := writer.CreatePart(mediaHeader)
	if err != nil {
		return err
	}
	if _, err := io.Copy(mediaPart, data); err != nil {
		return err
	}
	return writer.Close()
}

// Get retrieves an object from GCS. Returns (nil, nil) if not found. The
// returned Body is a live stream, so its read is bounded only by ctx — not by a
// fixed client timeout — to avoid aborting large/slow downloads mid-stream.
func (b *GCSBackend) Get(ctx context.Context, bucket, key string) (*GetResult, error) {
	attrsCtx, cancelAttrs := ctxutil.WithRequestTimeout(ctx, b.requestTimeout)
	attrs, status, err := b.getAttrs(attrsCtx, bucket, key)
	cancelAttrs()
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.objectURL(bucket, key)+"?alt=media", nil)
	if err != nil {
		return nil, errors.Wrap(err, CodeStorageRequest, errors.String("backend", "gcs"), errors.String("method", "GET"))
	}
	if err := b.authorize(ctx, req); err != nil {
		return nil, err
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, CodeStorageRead,
			errors.String("backend", "gcs"), errors.String("bucket", bucket), errors.String("key", key))
	}
	if resp.StatusCode == http.StatusNotFound {
		if cerr := resp.Body.Close(); cerr != nil {
			return nil, errors.Wrap(cerr, CodeStorageRequest, errors.String("backend", "gcs"), errors.String("op", "close_response"))
		}
		return nil, nil
	}
	if resp.StatusCode >= 400 {
		err := httpError(resp, CodeStorageRead, bucket, key)
		if cerr := resp.Body.Close(); cerr != nil {
			return nil, errors.Wrap(cerr, CodeStorageRequest, errors.String("backend", "gcs"), errors.String("op", "close_response"))
		}
		return nil, err
	}

	return &GetResult{
		Key:          key,
		Body:         resp.Body,
		Size:         attrs.size(),
		ContentType:  attrs.ContentType,
		ETag:         attrs.Etag,
		LastModified: attrs.updatedTime(),
		Metadata:     attrs.Metadata,
	}, nil
}

// getAttrs fetches an object's metadata. It returns the HTTP status so callers
// can distinguish a 404 (not found) from a successful fetch without treating it
// as an error.
func (b *GCSBackend) getAttrs(ctx context.Context, bucket, key string) (_ *gcsObject, status int, retErr error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.objectURL(bucket, key), nil)
	if err != nil {
		return nil, 0, errors.Wrap(err, CodeStorageRequest, errors.String("backend", "gcs"), errors.String("method", "GET"))
	}
	if err := b.authorize(ctx, req); err != nil {
		return nil, 0, err
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, 0, errors.Wrap(err, CodeStorageRead,
			errors.String("backend", "gcs"), errors.String("bucket", bucket), errors.String("key", key))
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && retErr == nil {
			retErr = errors.Wrap(cerr, CodeStorageRequest, errors.String("backend", "gcs"), errors.String("op", "close_response"))
		}
	}()

	if resp.StatusCode == http.StatusNotFound {
		return nil, http.StatusNotFound, nil
	}
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, httpError(resp, CodeStorageRead, bucket, key)
	}

	var obj gcsObject
	if err := json.NewDecoder(resp.Body).Decode(&obj); err != nil {
		return nil, resp.StatusCode, errors.Wrap(err, CodeStorageRead,
			errors.String("backend", "gcs"), errors.String("bucket", bucket), errors.String("key", key))
	}
	return &obj, resp.StatusCode, nil
}

// Delete removes an object from GCS. Deleting a non-existent object is a no-op.
func (b *GCSBackend) Delete(ctx context.Context, bucket, key string) (retErr error) {
	ctx, cancel := ctxutil.WithRequestTimeout(ctx, b.requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, b.objectURL(bucket, key), nil)
	if err != nil {
		return errors.Wrap(err, CodeStorageRequest, errors.String("backend", "gcs"), errors.String("method", "DELETE"))
	}
	if err := b.authorize(ctx, req); err != nil {
		return err
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return errors.Wrap(err, CodeStorageDelete,
			errors.String("backend", "gcs"), errors.String("bucket", bucket), errors.String("key", key))
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && retErr == nil {
			retErr = errors.Wrap(cerr, CodeStorageRequest, errors.String("backend", "gcs"), errors.String("op", "close_response"))
		}
	}()

	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode >= 400 {
		return httpError(resp, CodeStorageDelete, bucket, key)
	}
	return nil
}

// Exists checks whether an object exists in GCS.
func (b *GCSBackend) Exists(ctx context.Context, bucket, key string) (bool, error) {
	ctx, cancel := ctxutil.WithRequestTimeout(ctx, b.requestTimeout)
	defer cancel()

	_, status, err := b.getAttrs(ctx, bucket, key)
	if err != nil {
		return false, err
	}
	return status != http.StatusNotFound, nil
}

// Stat returns the metadata of an object in GCS with one object metadata read
// (objects.get without media). It never lists the bucket.
func (b *GCSBackend) Stat(ctx context.Context, bucket, key string) (*ObjectInfo, error) {
	ctx, cancel := ctxutil.WithRequestTimeout(ctx, b.requestTimeout)
	defer cancel()

	attrs, status, err := b.getAttrs(ctx, bucket, key)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, statNotFound("gcs", bucket, key)
	}
	return &ObjectInfo{
		Key:          key,
		Size:         attrs.size(),
		ContentType:  attrs.ContentType,
		ETag:         attrs.Etag,
		LastModified: attrs.updatedTime(),
	}, nil
}

// List returns objects in a GCS bucket matching the given options. Pagination
// uses the object name as the continuation token, matching the other backends;
// listing stops as soon as MaxKeys objects are collected.
func (b *GCSBackend) List(ctx context.Context, bucket string, opts *ListOptions) (*ListResult, error) {
	ctx, cancel := ctxutil.WithRequestTimeout(ctx, b.requestTimeout)
	defer cancel()

	if opts == nil {
		opts = &ListOptions{}
	}
	result := &ListResult{}
	prefixSet := make(map[string]struct{})
	pageToken := ""

	for {
		page, err := b.listPage(ctx, bucket, opts, pageToken)
		if err != nil {
			return nil, err
		}

		done := false
		for i := range page.Items {
			item := &page.Items[i]
			if opts.ContinuationToken != "" && item.Name <= opts.ContinuationToken {
				continue
			}
			result.Objects = append(result.Objects, ObjectInfo{
				Key:          item.Name,
				Size:         item.size(),
				ContentType:  item.ContentType,
				ETag:         item.Etag,
				LastModified: item.updatedTime(),
			})
			if opts.MaxKeys > 0 && len(result.Objects) >= opts.MaxKeys {
				result.IsTruncated = true
				result.ContinuationToken = item.Name
				done = true
				break
			}
		}
		for _, prefix := range page.Prefixes {
			prefixSet[prefix] = struct{}{}
		}

		pageToken = page.NextPageToken
		if done || pageToken == "" {
			break
		}
	}

	result.Prefixes = make([]string, 0, len(prefixSet))
	for prefix := range prefixSet {
		result.Prefixes = append(result.Prefixes, prefix)
	}
	sort.Strings(result.Prefixes)
	return result, nil
}

// listPage fetches a single page of a bucket listing.
func (b *GCSBackend) listPage(ctx context.Context, bucket string, opts *ListOptions, pageToken string) (_ *gcsListResponse, retErr error) {
	u, err := url.Parse(b.listURL(bucket))
	if err != nil {
		return nil, errors.Wrap(err, CodeStorageList, errors.String("backend", "gcs"), errors.String("bucket", bucket))
	}
	query := u.Query()
	if opts.Prefix != "" {
		query.Set("prefix", opts.Prefix)
	}
	if opts.Delimiter != "" {
		query.Set("delimiter", opts.Delimiter)
	}
	if pageToken != "" {
		query.Set("pageToken", pageToken)
	}
	u.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.Wrap(err, CodeStorageRequest, errors.String("backend", "gcs"), errors.String("method", "GET"))
	}
	if err := b.authorize(ctx, req); err != nil {
		return nil, err
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, CodeStorageList, errors.String("backend", "gcs"), errors.String("bucket", bucket))
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && retErr == nil {
			retErr = errors.Wrap(cerr, CodeStorageRequest, errors.String("backend", "gcs"), errors.String("op", "close_response"))
		}
	}()

	if resp.StatusCode >= 400 {
		return nil, httpError(resp, CodeStorageList, bucket, "")
	}

	var page gcsListResponse
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, errors.Wrap(err, CodeStorageList, errors.String("backend", "gcs"), errors.String("op", "decode_json"))
	}
	return &page, nil
}

// Copy duplicates an object within a GCS bucket using the server-side rewrite
// API, looping until a large rewrite completes.
func (b *GCSBackend) Copy(ctx context.Context, bucket, source, destination string) error {
	rewriteToken := ""
	for {
		done, nextToken, err := b.rewriteStep(ctx, bucket, source, destination, rewriteToken)
		if err != nil {
			return err
		}
		if done || nextToken == "" {
			return nil
		}
		rewriteToken = nextToken
	}
}

// rewriteStep performs one server-side rewrite request and reports whether the
// copy is complete.
func (b *GCSBackend) rewriteStep(ctx context.Context, bucket, source, destination, rewriteToken string) (done bool, nextToken string, retErr error) {
	endpoint := b.rewriteURL(bucket, source, destination)
	if rewriteToken != "" {
		endpoint += "?rewriteToken=" + url.QueryEscape(rewriteToken)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return false, "", errors.Wrap(err, CodeStorageRequest, errors.String("backend", "gcs"), errors.String("method", "POST"))
	}
	if err := b.authorize(ctx, req); err != nil {
		return false, "", err
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return false, "", errors.Wrap(err, CodeStorageCopy,
			errors.String("backend", "gcs"), errors.String("source", source), errors.String("destination", destination))
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && retErr == nil {
			retErr = errors.Wrap(cerr, CodeStorageRequest, errors.String("backend", "gcs"), errors.String("op", "close_response"))
		}
	}()

	if resp.StatusCode == http.StatusNotFound {
		return false, "", errors.New(CodeStorageNotFound, "source not found",
			errors.String("backend", "gcs"), errors.String("bucket", bucket), errors.String("key", source))
	}
	if resp.StatusCode >= 400 {
		return false, "", httpError(resp, CodeStorageCopy, bucket, source)
	}

	var rewrite struct {
		Done         bool   `json:"done"`
		RewriteToken string `json:"rewriteToken"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rewrite); err != nil {
		return false, "", errors.Wrap(err, CodeStorageCopy, errors.String("backend", "gcs"), errors.String("op", "decode_json"))
	}
	return rewrite.Done, rewrite.RewriteToken, nil
}

// Close releases idle HTTP connections held by the backend.
func (b *GCSBackend) Close() error {
	b.httpClient.CloseIdleConnections()
	return nil
}

// httpError reads a failed response body and wraps it as a structured error with
// the given code.
func httpError(resp *http.Response, code errors.Code, bucket, key string) error {
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return errors.Wrapf(readErr, code, "request failed: read body",
			errors.String("backend", "gcs"), errors.Int("status", resp.StatusCode))
	}
	return errors.New(code, "request failed",
		errors.String("backend", "gcs"), errors.String("bucket", bucket), errors.String("key", key),
		errors.Int("status", resp.StatusCode), errors.String("body", string(body)))
}
