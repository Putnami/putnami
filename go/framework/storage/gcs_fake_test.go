package storage

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGCS is an in-memory emulator of the minimal subset of the Google Cloud
// Storage JSON API exercised by GCSBackend. It is hand-rolled on top of httptest
// so the data-plane tests need no external fake-server dependency and the
// backend talks to it exactly as it would to storage.googleapis.com.
//
// Routes are matched by recognizable path fragments (".../o", ".../o/{object}",
// "/upload/...", ".../rewriteTo/...") rather than fixed absolute prefixes,
// because the backend derives URLs from the supplied endpoint.
type fakeGCS struct {
	t       *testing.T
	mu      sync.Mutex
	objects map[string]*fakeObject // key: bucket + "/" + name
	server  *httptest.Server
	debug   bool
	// failStatus, when non-zero, makes every request respond with this HTTP
	// status and a JSON error body (used to exercise error-handling branches).
	failStatus int
	// failDownload, when true, makes only the raw byte-download path fail, while
	// object-metadata (Attrs) still succeeds. This isolates the reader error
	// branch from the attrs path.
	failDownload bool
}

type fakeObject struct {
	Bucket      string
	Name        string
	Data        []byte
	ContentType string
	Updated     time.Time
	Metadata    map[string]string
}

// objJSON renders a storage#object resource. CRITICAL: "size" must be a string.
func (o *fakeObject) objJSON() map[string]any {
	m := map[string]any{
		"kind":        "storage#object",
		"id":          o.Bucket + "/" + o.Name,
		"name":        o.Name,
		"bucket":      o.Bucket,
		"etag":        fmt.Sprintf("etag-%s", o.Name),
		"size":        strconv.Itoa(len(o.Data)),
		"contentType": o.ContentType,
		"updated":     o.Updated.Format(time.RFC3339),
		"timeCreated": o.Updated.Format(time.RFC3339),
		"generation":  "1",
	}
	if len(o.Metadata) > 0 {
		m["metadata"] = o.Metadata
	}
	return m
}

func newFakeGCS(t *testing.T) *fakeGCS {
	t.Helper()
	f := &fakeGCS{t: t, objects: make(map[string]*fakeObject)}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

// backend builds a native GCSBackend wired to the fake server with
// authentication disabled (the fake ignores the Authorization header).
func (f *fakeGCS) backend(t *testing.T) *GCSBackend {
	t.Helper()
	return &GCSBackend{
		httpClient:   f.server.Client(),
		endpoint:     f.server.URL,
		metadataHost: f.server.URL,
		iamEndpoint:  f.server.URL,
		tokens:       staticTokenSource(""),
	}
}

// setFail makes every subsequent request return HTTP 403 Forbidden.
func (f *fakeGCS) setFail() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failStatus = http.StatusForbidden
}

// setFailDownload makes only raw byte-download requests fail; metadata succeeds.
func (f *fakeGCS) setFailDownload(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failDownload = v
}

func (f *fakeGCS) put(name string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects["bucket/"+name] = &fakeObject{
		Bucket:      "bucket",
		Name:        name,
		Data:        data,
		ContentType: "application/octet-stream",
		Updated:     time.Now().UTC().Truncate(time.Second),
	}
}

func (f *fakeGCS) writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// notFound returns a JSON error body mapped to a 404 status.
func (f *fakeGCS) notFound(w http.ResponseWriter) {
	f.writeJSON(w, http.StatusNotFound, map[string]any{
		"error": map[string]any{
			"code":    404,
			"message": "Not Found",
			"errors": []map[string]any{
				{"reason": "notFound", "message": "Not Found"},
			},
		},
	})
}

func (f *fakeGCS) handle(w http.ResponseWriter, r *http.Request) {
	if f.debug {
		f.t.Logf("FAKE %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
	}
	f.mu.Lock()
	fail := f.failStatus
	f.mu.Unlock()
	if fail != 0 {
		f.writeJSON(w, fail, map[string]any{
			"error": map[string]any{"code": fail, "message": "injected failure"},
		})
		return
	}
	path := r.URL.Path

	// 1) Multipart upload (writer). POST /upload/storage/v1/b/{bucket}/o?uploadType=multipart
	if r.Method == http.MethodPost && strings.Contains(path, "/upload/") && strings.HasSuffix(path, "/o") {
		f.handleUpload(w, r)
		return
	}

	// 2) Rewrite (copy). POST .../o/{src}/rewriteTo/b/{dstB}/o/{dst}
	if r.Method == http.MethodPost && strings.Contains(path, "/rewriteTo/") {
		f.handleRewrite(w, r)
		return
	}

	// 3) JSON object resource paths: .../b/{bucket}/o/{object}
	if obj, bucket, ok := parseObjectPath(path); ok {
		switch r.Method {
		case http.MethodGet:
			if r.URL.Query().Get("alt") == "media" {
				f.handleMediaDownload(w, r, bucket, obj)
				return
			}
			f.handleAttrs(w, r, bucket, obj)
			return
		case http.MethodDelete:
			f.handleDelete(w, r, bucket, obj)
			return
		}
	}

	// 4) List: GET .../b/{bucket}/o
	if r.Method == http.MethodGet && strings.HasSuffix(path, "/o") {
		f.handleList(w, r)
		return
	}

	f.t.Logf("FAKE unhandled %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
	http.Error(w, "unhandled", http.StatusNotImplemented)
}

// parseObjectPath extracts (object, bucket) from a path of the form
// ".../b/{bucket}/o/{object...}". The object may itself contain slashes (already
// path-escaped by the client, so it will not contain a literal extra "/o/").
func parseObjectPath(path string) (object, bucket string, ok bool) {
	idx := strings.Index(path, "/o/")
	if idx < 0 {
		return "", "", false
	}
	// bucket is the segment immediately preceding "/o/", after the last "/b/".
	left := path[:idx]
	bIdx := strings.LastIndex(left, "/b/")
	if bIdx < 0 {
		return "", "", false
	}
	bucket = left[bIdx+len("/b/"):]
	object = path[idx+len("/o/"):]
	// object is URL-escaped by the client, so decode it for the in-memory key.
	if dec, err := url.PathUnescape(object); err == nil {
		object = dec
	}
	if bucket == "" || object == "" {
		return "", "", false
	}
	return object, bucket, true
}

func (f *fakeGCS) handleUpload(w http.ResponseWriter, r *http.Request) {
	bucket := bucketFromUploadPath(r.URL.Path)
	if r.URL.Query().Get("uploadType") != "multipart" {
		f.t.Logf("FAKE upload: unexpected uploadType=%q", r.URL.Query().Get("uploadType"))
	}
	ct := r.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		f.t.Logf("FAKE upload: bad content-type %q: %v", ct, err)
		http.Error(w, "bad upload content-type", http.StatusBadRequest)
		return
	}
	mr := multipart.NewReader(r.Body, params["boundary"])

	// Part 1: JSON metadata. Part 2: object bytes.
	var meta struct {
		Name        string            `json:"name"`
		Bucket      string            `json:"bucket"`
		ContentType string            `json:"contentType"`
		Metadata    map[string]string `json:"metadata"`
	}
	var data []byte
	partContentType := ""
	idx := 0
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			f.t.Logf("FAKE upload: read part: %v", err)
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		b, err := io.ReadAll(part)
		if err != nil {
			// GCS commits nothing from an aborted or truncated upload body.
			f.t.Logf("FAKE upload: read part: %v", err)
			http.Error(w, "truncated multipart", http.StatusBadRequest)
			return
		}
		if idx == 0 {
			if e := json.Unmarshal(b, &meta); e != nil {
				f.t.Logf("FAKE upload: meta json: %v (body=%q)", e, string(b))
			}
		} else {
			data = b
			partContentType = part.Header.Get("Content-Type")
		}
		idx++
	}

	name := meta.Name
	if name == "" {
		name = r.URL.Query().Get("name")
	}
	if bucket == "" {
		bucket = meta.Bucket
	}
	contentType := meta.ContentType
	if contentType == "" {
		contentType = partContentType
	}

	f.mu.Lock()
	f.objects[bucket+"/"+name] = &fakeObject{
		Bucket:      bucket,
		Name:        name,
		Data:        data,
		ContentType: contentType,
		Updated:     time.Now().UTC().Truncate(time.Second),
		Metadata:    meta.Metadata,
	}
	obj := f.objects[bucket+"/"+name]
	f.mu.Unlock()

	f.writeJSON(w, http.StatusOK, obj.objJSON())
}

func (f *fakeGCS) handleAttrs(w http.ResponseWriter, _ *http.Request, bucket, name string) {
	f.mu.Lock()
	obj, ok := f.objects[bucket+"/"+name]
	f.mu.Unlock()
	if !ok {
		f.notFound(w)
		return
	}
	f.writeJSON(w, http.StatusOK, obj.objJSON())
}

func (f *fakeGCS) handleMediaDownload(w http.ResponseWriter, _ *http.Request, bucket, name string) {
	f.mu.Lock()
	obj, ok := f.objects[bucket+"/"+name]
	dl := f.failDownload
	f.mu.Unlock()
	if dl {
		f.writeJSON(w, http.StatusForbidden, map[string]any{"error": map[string]any{"code": 403, "message": "download blocked"}})
		return
	}
	if !ok {
		f.notFound(w)
		return
	}
	w.Header().Set("Content-Type", obj.ContentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(obj.Data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(obj.Data)
}

func (f *fakeGCS) handleDelete(w http.ResponseWriter, _ *http.Request, bucket, name string) {
	f.mu.Lock()
	_, ok := f.objects[bucket+"/"+name]
	if ok {
		delete(f.objects, bucket+"/"+name)
	}
	f.mu.Unlock()
	if !ok {
		f.notFound(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeGCS) handleList(w http.ResponseWriter, r *http.Request) {
	bucket := bucketFromListPath(r.URL.Path)
	q := r.URL.Query()
	prefix := q.Get("prefix")
	delimiter := q.Get("delimiter")

	f.mu.Lock()
	names := make([]string, 0, len(f.objects))
	for k := range f.objects {
		names = append(names, k)
	}
	f.mu.Unlock()
	// GCS returns objects in lexicographic name order; replicate that so the
	// backend's continuation-token logic (which assumes ordering) is testable.
	sort.Strings(names)

	items := make([]map[string]any, 0, len(names))
	prefixSet := map[string]struct{}{}

	for _, key := range names {
		f.mu.Lock()
		obj := f.objects[key]
		f.mu.Unlock()
		if obj.Bucket != bucket {
			continue
		}
		if prefix != "" && !strings.HasPrefix(obj.Name, prefix) {
			continue
		}
		if delimiter != "" {
			rest := obj.Name[len(prefix):]
			if di := strings.Index(rest, delimiter); di >= 0 {
				prefixSet[prefix+rest[:di+len(delimiter)]] = struct{}{}
				continue
			}
		}
		items = append(items, obj.objJSON())
	}

	prefixes := make([]string, 0, len(prefixSet))
	for p := range prefixSet {
		prefixes = append(prefixes, p)
	}

	resp := map[string]any{"kind": "storage#objects"}
	if len(items) > 0 {
		resp["items"] = items
	}
	if len(prefixes) > 0 {
		resp["prefixes"] = prefixes
	}
	f.writeJSON(w, http.StatusOK, resp)
}

func (f *fakeGCS) handleRewrite(w http.ResponseWriter, r *http.Request) {
	srcBucket, srcObj, dstBucket, dstObj, ok := parseRewritePath(r.URL.Path)
	if !ok {
		f.t.Logf("FAKE rewrite: cannot parse %s", r.URL.Path)
		http.Error(w, "bad rewrite path", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	src, exists := f.objects[srcBucket+"/"+srcObj]
	if !exists {
		f.mu.Unlock()
		f.notFound(w)
		return
	}
	dst := &fakeObject{
		Bucket:      dstBucket,
		Name:        dstObj,
		Data:        append([]byte(nil), src.Data...),
		ContentType: src.ContentType,
		Updated:     time.Now().UTC().Truncate(time.Second),
		Metadata:    src.Metadata,
	}
	f.objects[dstBucket+"/"+dstObj] = dst
	f.mu.Unlock()

	f.writeJSON(w, http.StatusOK, map[string]any{
		"kind":                "storage#rewriteResponse",
		"done":                true,
		"totalBytesRewritten": strconv.Itoa(len(dst.Data)),
		"objectSize":          strconv.Itoa(len(dst.Data)),
		"resource":            dst.objJSON(),
	})
}

// --- small path helpers ---

func bucketFromUploadPath(path string) string {
	// .../b/{bucket}/o
	idx := strings.LastIndex(path, "/b/")
	if idx < 0 {
		return ""
	}
	rest := path[idx+len("/b/"):]
	rest = strings.TrimSuffix(rest, "/o")
	return rest
}

func bucketFromListPath(path string) string {
	// .../b/{bucket}/o
	idx := strings.LastIndex(path, "/b/")
	if idx < 0 {
		return ""
	}
	rest := path[idx+len("/b/"):]
	return strings.TrimSuffix(rest, "/o")
}

func parseRewritePath(path string) (srcBucket, srcObj, dstBucket, dstObj string, ok bool) {
	// .../b/{srcBucket}/o/{srcObj}/rewriteTo/b/{dstBucket}/o/{dstObj}
	rwIdx := strings.Index(path, "/rewriteTo/")
	if rwIdx < 0 {
		return
	}
	srcPart := path[:rwIdx]
	dstPart := path[rwIdx+len("/rewriteTo/"):]

	srcObjI := strings.Index(srcPart, "/o/")
	srcBI := strings.LastIndex(srcPart[:srcObjI], "/b/")
	if srcObjI < 0 || srcBI < 0 {
		return
	}
	srcBucket = srcPart[srcBI+len("/b/") : srcObjI]
	srcObj, _ = url.PathUnescape(srcPart[srcObjI+len("/o/"):])

	dstObjI := strings.Index(dstPart, "/o/")
	dstBI := strings.Index(dstPart, "b/")
	if dstObjI < 0 || dstBI < 0 {
		return
	}
	dstBucket = dstPart[dstBI+len("b/") : dstObjI]
	dstObj, _ = url.PathUnescape(dstPart[dstObjI+len("/o/"):])

	ok = srcBucket != "" && srcObj != "" && dstBucket != "" && dstObj != ""
	return
}
