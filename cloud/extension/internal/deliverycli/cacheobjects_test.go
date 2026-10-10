package deliverycli

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/cloud/extension/internal/deliverycli/internal/remotecache"
	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/protocol/cache/objectcachetest"
)

// objectNamespace is the namespace Go's GOCACHEPROG consumer uses.
const objectNamespace = "go-build"

// objectID returns a valid object id: lowercase hex, a Go action id's length.
func objectID(c byte) string { return strings.Repeat(string(c), 64) }

// shortExchangeDir returns a temporary blob-exchange directory short enough that
// the provider's socket path fits the platform sun_path limit, so a test
// exercises the ORDINARY placement: the socket directly under the exchange
// directory, which is what the protocol asks for and what core's own exchange
// directory usually allows. TestProviderObjectCache_RelocatesATooLongSocketPath
// covers the other branch.
func shortExchangeDir(t *testing.T) string {
	t.Helper()
	for _, base := range []string{"", "/tmp"} {
		dir, err := os.MkdirTemp(base, "pnobj")
		if err != nil {
			continue
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		if len(filepath.Join(dir, objectSocketName)) <= maxObjectSocketPath {
			return dir
		}
		_ = os.RemoveAll(dir)
	}
	//putnami:allow-skip reached only when no temporary directory fits the platform's socket path limit
	t.Skip("no temporary directory yields a socket path within the platform limit")
	return ""
}

// fakeObjectServer is a cache server that implements the object index plus the
// blob halves the object write path rides on. It records what it was asked, so a
// test can assert the SHAPE of a request (no asserted channel) and not merely
// its effect.
type fakeObjectServer struct {
	srv *httptest.Server

	mu sync.Mutex
	// objects is the keyed index: namespace\x00id -> record.
	objects map[string]fakeObject
	// blobs is the CAS: digest -> uncompressed bytes.
	blobs map[string][]byte
	// storeBodies keeps every objects/store request body verbatim.
	storeBodies [][]byte
	lookups     int
	stores      int
	// channel is the trust channel the server stamps, modeling the credential
	// it verified. A pull-request credential stamps hint.
	channel cache.Channel
	// capabilities is what the discovery probe advertises.
	capabilities []string
	// inlineBound is the largest object served inline; larger ones get a
	// presigned GET.
	inlineBound int64
	// crossWired maps namespace\x00id to the digest its DOWNLOAD should address
	// instead of its own, modeling a server that cross-wires a presign.
	crossWired map[string]string
	// storeGate, when non-nil, holds every objects/store until it is closed.
	storeGate <-chan struct{}
	// storeStatus, when non-zero, is answered to every objects/store instead of
	// indexing it — the shape of a server whose write gate refuses this run's
	// credential (403) or whose bearer has expired (401).
	storeStatus int
	// blobPuts counts the presigned uploads the CAS received, which is what makes
	// "a refused store cost ONE batch" observable: the bytes go up BEFORE the
	// index row is asked for, so a run that keeps offering batches keeps filling
	// the CAS with blobs no object row will ever name.
	blobPuts int
	// blobGate, when non-nil, holds every presigned PUT until it is closed, and
	// blobPutsStarted counts the PUTs that ARRIVED (before the gate). Together
	// they model the risk the probe gate carries: the batch holding the probe
	// stalls inside an upload that has no deadline of its own, and the question
	// is whether the other workers are stuck behind it.
	blobGate        <-chan struct{}
	blobPutsStarted int
}

type fakeObject struct {
	id      string
	digest  string
	size    int64
	meta    string
	channel cache.Channel
}

func newFakeObjectServer(t *testing.T) *fakeObjectServer {
	t.Helper()
	f := &fakeObjectServer{
		objects:     map[string]fakeObject{},
		blobs:       map[string][]byte{},
		crossWired:  map[string]string{},
		channel:     cache.ChannelTrusted,
		inlineBound: cache.MaxInlineBlobBytes,
		capabilities: []string{
			cache.CapabilityFindMissing, cache.CapabilityCommitBatch,
			cache.CapabilityDownloadBatch, cache.CapabilityObjectCache,
		},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeObjectServer) url() string { return f.srv.URL }

func (f *fakeObjectServer) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == cache.CapabilitiesPath:
		f.mu.Lock()
		caps := append([]string(nil), f.capabilities...)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(cache.CapabilitiesResponse{
			ProtocolVersion: cache.ProtocolVersion,
			Capabilities:    caps,
		})
	case r.URL.Path == "/v1/cache/objects/lookup":
		f.lookup(w, r)
	case r.URL.Path == "/v1/cache/objects/store":
		f.store(w, r)
	case r.URL.Path == cache.FindMissingPath:
		f.findMissing(w, r)
	case strings.HasPrefix(r.URL.Path, "/blob/"):
		f.blob(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeObjectServer) lookup(w http.ResponseWriter, r *http.Request) {
	var req remotecache.ObjectLookupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.lookups++
	resp := remotecache.ObjectLookupResponse{ProtocolVersion: cache.ProtocolVersion}
	for _, id := range req.IDs {
		obj, ok := f.objects[req.Namespace+"\x00"+id]
		if !ok {
			continue // a miss is an omission
		}
		content, ok := f.blobs[obj.digest]
		if !ok {
			continue // a dangling index row is a miss, never a phantom hit
		}
		record := remotecache.ObjectRecord{
			ID:        obj.id,
			Digest:    obj.digest,
			SizeBytes: obj.size,
			Meta:      obj.meta,
			Channel:   string(obj.channel),
			Producer:  string(cache.ProducerCI),
		}
		if obj.channel == cache.ChannelHint {
			record.Producer = string(cache.ProducerDeveloper)
		}
		if obj.size <= f.inlineBound {
			record.Blob = &cache.InlineBlob{Digest: obj.digest, Data: content}
		} else {
			served := obj.digest
			if other, wired := f.crossWired[req.Namespace+"\x00"+id]; wired {
				served = other
			}
			record.Download = &cache.BlobTransfer{
				Digest:    served,
				URL:       f.srv.URL + "/blob/" + strings.TrimPrefix(served, cache.DigestAlgorithm+":"),
				Method:    cache.TransferGet,
				SizeBytes: obj.size,
			}
		}
		resp.Objects = append(resp.Objects, record)
	}
	f.mu.Unlock()
	_ = json.NewEncoder(w).Encode(resp)
}

func (f *fakeObjectServer) store(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req remotecache.ObjectStoreRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	gate := f.storeGate
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	f.mu.Lock()
	f.stores++
	status := f.storeStatus
	f.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(cache.ErrorResponse{
			Code:    "forbidden",
			Message: "token is not authorized for cache.object.write",
		})
		return
	}
	f.mu.Lock()
	f.storeBodies = append(f.storeBodies, body)
	resp := remotecache.ObjectStoreResponse{ProtocolVersion: cache.ProtocolVersion}
	for _, obj := range req.Objects {
		_, present := f.blobs[obj.Digest]
		if present {
			// Provenance comes from the credential this server verified, never
			// from the request: the offer carries no channel at all.
			f.objects[req.Namespace+"\x00"+obj.ID] = fakeObject{
				id: obj.ID, digest: obj.Digest, size: obj.SizeBytes, meta: obj.Meta, channel: f.channel,
			}
		}
		resp.Results = append(resp.Results, remotecache.ObjectStoreResult{ID: obj.ID, Stored: present})
	}
	f.mu.Unlock()
	_ = json.NewEncoder(w).Encode(resp)
}

func (f *fakeObjectServer) findMissing(w http.ResponseWriter, r *http.Request) {
	var req cache.FindMissingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	resp := cache.FindMissingResponse{ProtocolVersion: cache.ProtocolVersion}
	f.mu.Lock()
	for _, b := range req.Blobs {
		if _, ok := f.blobs[b.Digest]; ok {
			continue // already in CAS: dedup, no upload
		}
		resp.Uploads = append(resp.Uploads, cache.BlobTransfer{
			Digest:    b.Digest,
			URL:       f.srv.URL + "/blob/" + strings.TrimPrefix(b.Digest, cache.DigestAlgorithm+":"),
			Method:    cache.TransferPut,
			SizeBytes: b.SizeBytes,
		})
	}
	f.mu.Unlock()
	_ = json.NewEncoder(w).Encode(resp)
}

// blob is the presigned transfer target: PUT stores the bytes (un-gzipping the
// transport layer, exactly as the real CAS reader does), GET serves them.
func (f *fakeObjectServer) blob(w http.ResponseWriter, r *http.Request) {
	digest := cache.DigestAlgorithm + ":" + strings.TrimPrefix(r.URL.Path, "/blob/")
	switch r.Method {
	case http.MethodPut:
		f.mu.Lock()
		f.blobPutsStarted++
		gate := f.blobGate
		f.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if zr, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
			if raw, err := io.ReadAll(zr); err == nil {
				body = raw
			}
		}
		f.mu.Lock()
		f.blobs[digest] = body
		f.blobPuts++
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		f.mu.Lock()
		content, ok := f.blobs[digest]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(content)
	default:
		http.Error(w, "unsupported", http.StatusMethodNotAllowed)
	}
}

// seed records an object and its bytes directly, so a get has a hit without a
// put first.
func (f *fakeObjectServer) seed(namespace, id, content, meta string, channel cache.Channel) string {
	digest := cache.DigestOf([]byte(content))
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blobs[digest] = []byte(content)
	f.objects[namespace+"\x00"+id] = fakeObject{
		id: id, digest: digest, size: int64(len(content)), meta: meta, channel: channel,
	}
	return digest
}

// crossWireDownload makes the record for (namespace, id) advertise ANOTHER
// object's presigned GET while keeping its own digest — the server-side
// inconsistency a provider must not resolve in the caller's favor.
func (f *fakeObjectServer) crossWireDownload(namespace, id, otherDigest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.crossWired[namespace+"\x00"+id] = otherDigest
}

// blockStores holds every objects/store request until release is closed, so a
// test can observe what happens while uploads are still in flight.
func (f *fakeObjectServer) blockStores(release <-chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.storeGate = release
}

// blockBlobPuts holds every presigned upload until release is closed, modeling
// the first put of a run carrying a large linked test binary or a storage
// endpoint that stopped answering. Nothing in the client bounds that stream: the
// control-plane timeout is applied per authed round trip and deliberately not to
// blob transfers, and the HTTP client sets no timeout of its own.
func (f *fakeObjectServer) blockBlobPuts(release <-chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blobGate = release
}

// refuseStores answers every objects/store with status, modeling the write gate
// this branch put in front of the route: a credential that carries cache.write
// but not cache.object.write is refused 403 on every store, while its reads and
// its blob uploads keep working.
func (f *fakeObjectServer) refuseStores(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.storeStatus = status
}

func (f *fakeObjectServer) stats() (lookups, stores int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookups, f.stores
}

// uploadedBlobs is how many blobs the CAS received.
func (f *fakeObjectServer) uploadedBlobs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.blobPuts
}

// startedBlobPuts is how many presigned uploads ARRIVED, gated or not.
func (f *fakeObjectServer) startedBlobPuts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.blobPutsStarted
}

func (f *fakeObjectServer) storedObject(namespace, id string) (fakeObject, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	obj, ok := f.objects[namespace+"\x00"+id]
	return obj, ok
}

// startObjectSession opens a provider session against f with the object-cache
// capability advertised by core, and returns the session and the negotiated
// initialize result.
func startObjectSession(t *testing.T, f *fakeObjectServer, exchangeDir string, capabilities ...string) (*providerSession, *cache.InitializeResult) {
	t.Helper()
	return startObjectSessionLogged(t, f, exchangeDir, io.Discard, capabilities...)
}

// lockedWriter serializes writes so a test can read the provider log while the
// background upload workers are still writing to it.
type lockedWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *lockedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// startObjectSessionLogged is startObjectSession with the provider log captured.
func startObjectSessionLogged(t *testing.T, f *fakeObjectServer, exchangeDir string, logw io.Writer, capabilities ...string) (*providerSession, *cache.InitializeResult) {
	t.Helper()
	t.Setenv("PUTNAMI_CACHE_URL", f.url())
	t.Setenv("PUTNAMI_CACHE_TOKEN", "test-token")
	enableCacheWritesForTest(t)

	s := newProviderSession(t.TempDir(), logw)
	t.Cleanup(s.close)
	result := mustInit(t, s, &cache.InitializeParams{
		ProtocolVersion: cache.ProviderProtocolMinVersion,
		BlobExchangeDir: exchangeDir,
		Mode:            cache.ModeFull,
		Capabilities:    capabilities,
	})
	return s, result
}

// coreCapabilities is what core advertises for a live provider-backed run.
func coreCapabilities() []string {
	return []string{cache.CapabilityProviderProtocolV2, cache.CapabilityObjectCache}
}

// assertSocket checks what core checks while it processes the initialize result:
// the capability is echoed, the path is absolute, and os.Stat sees a socket AT
// THAT INSTANT. A provider that bound the listener lazily would fail here.
func assertSocket(t *testing.T, result *cache.InitializeResult, exchangeDir string) {
	t.Helper()
	if !advertisesObjectCache(result.Capabilities) {
		t.Fatalf("capabilities=%v want the object-cache echo; core ignores a socket offered without it", result.Capabilities)
	}
	if result.ObjectCacheSocket == "" {
		t.Fatal("objectCacheSocket is empty")
	}
	if !filepath.IsAbs(result.ObjectCacheSocket) {
		t.Fatalf("objectCacheSocket=%q must be absolute", result.ObjectCacheSocket)
	}
	if dir := filepath.Dir(result.ObjectCacheSocket); dir != exchangeDir {
		t.Fatalf("socket parent=%q want the exchange directory %q: a job derives the exchange directory as the socket's parent",
			dir, exchangeDir)
	}
	info, err := os.Stat(result.ObjectCacheSocket)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket mode=%v want a socket", info.Mode())
	}
}

func dialObjects(t *testing.T, path string) *objectcachetest.Client {
	t.Helper()
	client, err := objectcachetest.Dial(path)
	if err != nil {
		t.Fatalf("dial object socket: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestProviderObjectCache_ConformanceFixture drives the provider's own socket
// with the shared conformance client from protocols/cache: a real Unix socket,
// real JSONL framing, per-connection request ids, and real bytes handed through
// the blob-exchange directory.
func TestProviderObjectCache_ConformanceFixture(t *testing.T) {
	f := newFakeObjectServer(t)
	exchangeDir := shortExchangeDir(t)
	s, result := startObjectSession(t, f, exchangeDir, coreCapabilities()...)
	assertSocket(t, result, exchangeDir)

	client := dialObjects(t, result.ObjectCacheSocket)

	// A get for an unknown id is a clean, empty result — a miss, not an error.
	id := objectID('a')
	got, err := client.Get(&cache.ObjectGetParams{Namespace: objectNamespace, IDs: []string{id}})
	if err != nil {
		t.Fatalf("object-get: %v", err)
	}
	if len(got.Objects) != 0 {
		t.Fatalf("objects=%+v want none for an unknown id", got.Objects)
	}

	// A put: the caller stages the bytes in the exchange directory first, exactly
	// as a job process does, then offers the object.
	content := []byte("compiled export data for the conformance fixture")
	digest, err := objectcachetest.StageBlob(exchangeDir, content)
	if err != nil {
		t.Fatalf("stage blob: %v", err)
	}
	put, err := client.Put(&cache.ObjectPutParams{
		Namespace: objectNamespace,
		Objects:   []cache.ObjectPut{{ID: id, Digest: digest, Size: int64(len(content)), Meta: "output-id-1"}},
	})
	if err != nil {
		t.Fatalf("object-put: %v", err)
	}
	if put.Accepted != 1 {
		t.Fatalf("accepted=%d want 1", put.Accepted)
	}

	// A put acknowledges QUEUEING; the bytes are durable at summary.
	s.objects.drain()
	if _, ok := f.storedObject(objectNamespace, id); !ok {
		t.Fatal("summary must have drained the put: the server holds no object")
	}

	// Now the same id is a hit, its bytes staged in the exchange directory.
	got, err = client.Get(&cache.ObjectGetParams{Namespace: objectNamespace, IDs: []string{id}})
	if err != nil {
		t.Fatalf("object-get after put: %v", err)
	}
	if len(got.Objects) != 1 {
		t.Fatalf("objects=%+v want exactly one hit", got.Objects)
	}
	hit := got.Objects[0]
	if hit.ID != id || hit.Digest != digest || hit.Size != int64(len(content)) || hit.Meta != "output-id-1" {
		t.Errorf("hit=%+v want the stored id, digest, size, and meta", hit)
	}
	if hit.Channel != cache.ChannelTrusted || hit.Producer != cache.ProducerCI {
		t.Errorf("provenance=%s/%s want ci/trusted from the server, not from the caller", hit.Producer, hit.Channel)
	}
	path, ok := cache.BlobExchangePath(exchangeDir, hit.Digest)
	if !ok {
		t.Fatalf("invalid digest %s", hit.Digest)
	}
	staged, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("hit bytes must be at BlobExchangePath when the response is written: %v", err)
	}
	if !bytes.Equal(staged, content) {
		t.Errorf("staged bytes=%q want %q", staged, content)
	}
}

// TestProviderObjectCache_NamespacesDoNotShareAnIDSpace proves the namespace
// travels all the way through: the same id in two namespaces is two objects, and
// a get in one namespace never sees the other's.
func TestProviderObjectCache_NamespacesDoNotShareAnIDSpace(t *testing.T) {
	f := newFakeObjectServer(t)
	exchangeDir := shortExchangeDir(t)
	id := objectID('a')
	f.seed(objectNamespace, id, "go bytes", "go", cache.ChannelTrusted)
	f.seed("other-lang", id, "other bytes", "other", cache.ChannelTrusted)

	_, result := startObjectSession(t, f, exchangeDir, coreCapabilities()...)
	client := dialObjects(t, result.ObjectCacheSocket)

	for _, tc := range []struct{ namespace, meta string }{
		{objectNamespace, "go"},
		{"other-lang", "other"},
	} {
		got, err := client.Get(&cache.ObjectGetParams{Namespace: tc.namespace, IDs: []string{id}})
		if err != nil {
			t.Fatalf("object-get %s: %v", tc.namespace, err)
		}
		if len(got.Objects) != 1 || got.Objects[0].Meta != tc.meta {
			t.Fatalf("%s objects=%+v want its own object (meta %q)", tc.namespace, got.Objects, tc.meta)
		}
	}
	if _, ok := f.storedObject("unused-lang", id); ok {
		t.Error("an unseeded namespace must hold nothing")
	}
}

// TestProviderObjectCache_RefusesEverySessionOp is the boundary that keeps a job
// process from driving the session: only the two object ops are valid on the
// socket, so a job can never restore a task entry or publish a run marker.
func TestProviderObjectCache_RefusesEverySessionOp(t *testing.T) {
	f := newFakeObjectServer(t)
	exchangeDir := shortExchangeDir(t)
	_, result := startObjectSession(t, f, exchangeDir, coreCapabilities()...)
	assertSocket(t, result, exchangeDir)
	client := dialObjects(t, result.ObjectCacheSocket)

	refused := []cache.ProviderOp{
		cache.OpInitialize, cache.OpPrefetch, cache.OpRestore, cache.OpUpload,
		cache.OpMarkerLookup, cache.OpMarkerWrite, cache.OpSummary, cache.OpShutdown,
	}
	for _, op := range refused {
		resp, err := client.Call(op, map[string]any{})
		if err != nil {
			t.Fatalf("call %s: %v", op, err)
		}
		if resp.OK {
			t.Errorf("op %s was SERVED on the object socket; only the object ops may be", op)
			continue
		}
		if resp.Error == nil || resp.Error.Code != "unsupported-op" {
			t.Errorf("op %s error=%+v want code unsupported-op", op, resp.Error)
		}
	}
}

// TestProviderObjectCache_TrustCINeverServesAHint pins the trust rule: under --cache-trust ci a run sends [ChannelTrusted], and
// a hint object is omitted — indistinguishable from an unknown id.
func TestProviderObjectCache_TrustCINeverServesAHint(t *testing.T) {
	f := newFakeObjectServer(t)
	exchangeDir := shortExchangeDir(t)
	hintID, trustedID := objectID('a'), objectID('b')
	f.seed(objectNamespace, hintID, "developer bytes", "", cache.ChannelHint)
	f.seed(objectNamespace, trustedID, "ci bytes", "", cache.ChannelTrusted)

	_, result := startObjectSession(t, f, exchangeDir, coreCapabilities()...)
	assertSocket(t, result, exchangeDir)
	client := dialObjects(t, result.ObjectCacheSocket)

	got, err := client.Get(&cache.ObjectGetParams{
		Namespace:      objectNamespace,
		IDs:            []string{hintID, trustedID},
		AcceptChannels: []cache.Channel{cache.ChannelTrusted},
	})
	if err != nil {
		t.Fatalf("object-get: %v", err)
	}
	if len(got.Objects) != 1 || got.Objects[0].ID != trustedID {
		t.Fatalf("objects=%+v want only the trusted object under --cache-trust ci", got.Objects)
	}
	// The hint object's bytes must not even be staged: a filtered object is a
	// miss, so nothing about it reaches the caller.
	digest := cache.DigestOf([]byte("developer bytes"))
	if path, ok := cache.BlobExchangePath(exchangeDir, digest); ok {
		if _, err := os.Stat(path); err == nil {
			t.Error("a filtered object's bytes were staged; a rejected channel must be a pure miss")
		}
	}

	// An EMPTY accept list takes every channel, including the legacy empty one.
	got, err = client.Get(&cache.ObjectGetParams{Namespace: objectNamespace, IDs: []string{hintID, trustedID}})
	if err != nil {
		t.Fatalf("object-get with no filter: %v", err)
	}
	if len(got.Objects) != 2 {
		t.Fatalf("objects=%+v want both: an empty accept list accepts every channel", got.Objects)
	}
}

// TestProviderObjectCache_PutNeverAssertsAChannel pins criterion 3's write half
// at the WIRE: the store request the provider sends carries no provenance at
// all, so a pull-request credential cannot produce a trusted object however it
// asks. (What it does produce is the server's business — the hint stamp — which
// the server-side tests pin.)
func TestProviderObjectCache_PutNeverAssertsAChannel(t *testing.T) {
	f := newFakeObjectServer(t)
	f.mu.Lock()
	f.channel = cache.ChannelHint // a pull-request credential
	f.mu.Unlock()
	exchangeDir := shortExchangeDir(t)
	s, result := startObjectSession(t, f, exchangeDir, coreCapabilities()...)
	assertSocket(t, result, exchangeDir)
	client := dialObjects(t, result.ObjectCacheSocket)

	id := objectID('a')
	content := []byte("pull-request bytes")
	digest, err := objectcachetest.StageBlob(exchangeDir, content)
	if err != nil {
		t.Fatalf("stage blob: %v", err)
	}
	if _, err := client.Put(&cache.ObjectPutParams{
		Namespace: objectNamespace,
		Objects:   []cache.ObjectPut{{ID: id, Digest: digest, Size: int64(len(content))}},
	}); err != nil {
		t.Fatalf("object-put: %v", err)
	}
	s.objects.drain()

	f.mu.Lock()
	bodies := append([][]byte(nil), f.storeBodies...)
	f.mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("no objects/store request was sent")
	}
	for _, body := range bodies {
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("decode store body: %v", err)
		}
		objects, _ := payload["objects"].([]any)
		for _, raw := range objects {
			obj, _ := raw.(map[string]any)
			for _, forbidden := range []string{"channel", "producer", "producerIdentity"} {
				if _, present := obj[forbidden]; present {
					t.Errorf("objects/store carried %q=%v; a caller must never assert provenance", forbidden, obj[forbidden])
				}
			}
		}
	}
	// And the object it DID store is on the server's channel, not a trusted one.
	obj, ok := f.storedObject(objectNamespace, id)
	if !ok {
		t.Fatal("the object was not stored")
	}
	if obj.channel != cache.ChannelHint {
		t.Errorf("stored channel=%s want hint: trust comes from the credential", obj.channel)
	}
}

// TestProviderObjectCache_NotNegotiatedWithoutCoreCapability proves both halves
// of the capability gate are required: a core that did not ask gets no socket
// and no echo.
func TestProviderObjectCache_NotNegotiatedWithoutCoreCapability(t *testing.T) {
	f := newFakeObjectServer(t)
	exchangeDir := shortExchangeDir(t)
	s, result := startObjectSession(t, f, exchangeDir, cache.CapabilityProviderProtocolV2)
	if result.ObjectCacheSocket != "" || advertisesObjectCache(result.Capabilities) {
		t.Fatalf("result=%+v want no socket and no echo when core did not advertise the capability", result)
	}
	if s.objects != nil {
		t.Error("no listener may exist when the capability was not negotiated")
	}
	if _, err := os.Stat(filepath.Join(exchangeDir, objectSocketName)); err == nil {
		t.Error("a socket file was created without the capability")
	}
}

// TestProviderObjectCache_NotNegotiatedWhenServerLacksObjectCache proves an
// older cache server degrades cleanly: no socket, so a compiler keeps its local
// cache instead of paying a round trip per compilation to be told 404.
func TestProviderObjectCache_NotNegotiatedWhenServerLacksObjectCache(t *testing.T) {
	f := newFakeObjectServer(t)
	f.mu.Lock()
	f.capabilities = []string{cache.CapabilityFindMissing, cache.CapabilityCommitBatch}
	f.mu.Unlock()
	exchangeDir := shortExchangeDir(t)
	s, result := startObjectSession(t, f, exchangeDir, coreCapabilities()...)
	if result.ObjectCacheSocket != "" || advertisesObjectCache(result.Capabilities) {
		t.Fatalf("result=%+v want no socket when the server does not serve objects", result)
	}
	if s.objects != nil {
		t.Error("no listener may exist when the server lacks the object index")
	}
	if !result.Ready {
		t.Error("the task cache must stay ready: only the object half degraded")
	}
}

// TestProviderObjectCache_NotNegotiatedWithoutACache proves a local-only run
// (no cache configured) opens no socket.
func TestProviderObjectCache_NotNegotiatedWithoutACache(t *testing.T) {
	t.Setenv("PUTNAMI_CACHE_URL", "")
	exchangeDir := shortExchangeDir(t)
	s := newProviderSession(t.TempDir(), io.Discard)
	t.Cleanup(s.close)
	result := mustInit(t, s, &cache.InitializeParams{
		ProtocolVersion: cache.ProviderProtocolMinVersion,
		BlobExchangeDir: exchangeDir,
		Capabilities:    coreCapabilities(),
	})
	if result.ObjectCacheSocket != "" || s.objects != nil {
		t.Fatalf("result=%+v want no object cache on a local-only run", result)
	}
}

// TestProviderObjectCache_ConcurrentConnections exercises the socket contract's
// concurrency clause: many connections, one per job process or more, each
// numbering its own request ids from 1.
func TestProviderObjectCache_ConcurrentConnections(t *testing.T) {
	f := newFakeObjectServer(t)
	exchangeDir := shortExchangeDir(t)
	const clients = 12
	ids := make([]string, clients)
	for i := range ids {
		ids[i] = fmt.Sprintf("%064x", i+1)
		f.seed(objectNamespace, ids[i], fmt.Sprintf("object %d", i), "", cache.ChannelTrusted)
	}

	_, result := startObjectSession(t, f, exchangeDir, coreCapabilities()...)
	assertSocket(t, result, exchangeDir)

	var wg sync.WaitGroup
	errs := make([]error, clients)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			client, err := objectcachetest.Dial(result.ObjectCacheSocket)
			if err != nil {
				errs[i] = err
				return
			}
			defer func() { _ = client.Close() }()
			// Two round trips per connection: the second proves the id space is
			// per connection (each client numbers from 1) rather than shared.
			for round := 0; round < 2; round++ {
				got, err := client.Get(&cache.ObjectGetParams{Namespace: objectNamespace, IDs: []string{ids[i]}})
				if err != nil {
					errs[i] = err
					return
				}
				if len(got.Objects) != 1 || got.Objects[0].ID != ids[i] {
					errs[i] = fmt.Errorf("client %d round %d got %+v", i, round, got.Objects)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("client %d: %v", i, err)
		}
	}
}

// TestProviderObjectCache_RefusesMismatchedStagedBytes proves the pre-upload
// integrity gate: a job that stages bytes which do not content-address to the
// digest it offered never gets them into the CAS, where they would poison that
// digest for every later reader.
func TestProviderObjectCache_RefusesMismatchedStagedBytes(t *testing.T) {
	f := newFakeObjectServer(t)
	exchangeDir := shortExchangeDir(t)
	s, result := startObjectSession(t, f, exchangeDir, coreCapabilities()...)
	assertSocket(t, result, exchangeDir)
	client := dialObjects(t, result.ObjectCacheSocket)

	// Stage "honest" bytes, then offer them under ANOTHER content's digest of the
	// same length — so the cheap acceptance check (present, right size) passes
	// and only the hash catches it.
	honest := []byte("aaaaaaaaaaaa")
	lie := []byte("bbbbbbbbbbbb")
	lieDigest := cache.DigestOf(lie)
	path, ok := cache.BlobExchangePath(exchangeDir, lieDigest)
	if !ok {
		t.Fatal("bad digest")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, honest, 0o600); err != nil {
		t.Fatalf("write staged bytes: %v", err)
	}

	id := objectID('a')
	put, err := client.Put(&cache.ObjectPutParams{
		Namespace: objectNamespace,
		Objects:   []cache.ObjectPut{{ID: id, Digest: lieDigest, Size: int64(len(lie))}},
	})
	if err != nil {
		t.Fatalf("object-put: %v", err)
	}
	if put.Accepted != 1 {
		t.Fatalf("accepted=%d want 1: acceptance is a local presence check, verification is the upload's job", put.Accepted)
	}
	s.objects.drain()
	if _, ok := f.storedObject(objectNamespace, id); ok {
		t.Error("mislabeled bytes reached the object index; the digest must be verified before upload")
	}
}

// TestProviderObjectCache_DropsAnUnstagedOffer proves an object whose bytes are
// not in the exchange directory is not accepted, so the count a caller reads is
// honest.
func TestProviderObjectCache_DropsAnUnstagedOffer(t *testing.T) {
	f := newFakeObjectServer(t)
	exchangeDir := shortExchangeDir(t)
	_, result := startObjectSession(t, f, exchangeDir, coreCapabilities()...)
	client := dialObjects(t, result.ObjectCacheSocket)

	put, err := client.Put(&cache.ObjectPutParams{
		Namespace: objectNamespace,
		Objects: []cache.ObjectPut{{
			ID:     objectID('a'),
			Digest: cache.DigestOf([]byte("never staged")),
			Size:   12,
		}},
	})
	if err != nil {
		t.Fatalf("object-put: %v", err)
	}
	if put.Accepted != 0 {
		t.Fatalf("accepted=%d want 0 for bytes that were never staged", put.Accepted)
	}
}

// TestProviderObjectCache_ReadOnlySessionAcceptsNoPut proves the read-only cache
// posture (the pinned runner's) covers objects too: reads are served, writes are
// not.
func TestProviderObjectCache_ReadOnlySessionAcceptsNoPut(t *testing.T) {
	f := newFakeObjectServer(t)
	exchangeDir := shortExchangeDir(t)
	id := objectID('a')
	f.seed(objectNamespace, id, "ci bytes", "", cache.ChannelTrusted)

	t.Setenv("PUTNAMI_CACHE_URL", f.url())
	t.Setenv("PUTNAMI_CACHE_TOKEN", "test-token")
	t.Setenv(remotecache.ReadOnlyEnv, "true")

	s := newProviderSession(t.TempDir(), io.Discard)
	t.Cleanup(s.close)
	result := mustInit(t, s, &cache.InitializeParams{
		ProtocolVersion: cache.ProviderProtocolMinVersion,
		BlobExchangeDir: exchangeDir,
		Capabilities:    coreCapabilities(),
	})
	assertSocket(t, result, exchangeDir)
	client := dialObjects(t, result.ObjectCacheSocket)

	got, err := client.Get(&cache.ObjectGetParams{Namespace: objectNamespace, IDs: []string{id}})
	if err != nil {
		t.Fatalf("object-get: %v", err)
	}
	if len(got.Objects) != 1 {
		t.Fatalf("objects=%+v want the read served under a read-only cache", got.Objects)
	}

	content := []byte("developer bytes")
	digest, err := objectcachetest.StageBlob(exchangeDir, content)
	if err != nil {
		t.Fatalf("stage blob: %v", err)
	}
	put, err := client.Put(&cache.ObjectPutParams{
		Namespace: objectNamespace,
		Objects:   []cache.ObjectPut{{ID: objectID('b'), Digest: digest, Size: int64(len(content))}},
	})
	if err != nil {
		t.Fatalf("object-put: %v", err)
	}
	if put.Accepted != 0 {
		t.Fatalf("accepted=%d want 0 under a read-only cache", put.Accepted)
	}
	s.objects.drain()
	if _, stored := f.storedObject(objectNamespace, objectID('b')); stored {
		t.Error("a read-only session stored an object")
	}
}

// TestProviderObjectCache_ServesALargeObjectByPresignedGET proves an object past
// the inline bound still materializes — through its presigned GET, content
// verified on the way in.
func TestProviderObjectCache_ServesALargeObjectByPresignedGET(t *testing.T) {
	f := newFakeObjectServer(t)
	f.mu.Lock()
	f.inlineBound = 8 // force the presigned path with a small object
	f.mu.Unlock()
	exchangeDir := shortExchangeDir(t)
	id := objectID('a')
	content := "a compiled object past the inline bound"
	digest := f.seed(objectNamespace, id, content, "out", cache.ChannelTrusted)

	_, result := startObjectSession(t, f, exchangeDir, coreCapabilities()...)
	client := dialObjects(t, result.ObjectCacheSocket)

	got, err := client.Get(&cache.ObjectGetParams{Namespace: objectNamespace, IDs: []string{id}})
	if err != nil {
		t.Fatalf("object-get: %v", err)
	}
	if len(got.Objects) != 1 {
		t.Fatalf("objects=%+v want one hit", got.Objects)
	}
	path, ok := cache.BlobExchangePath(exchangeDir, digest)
	if !ok {
		t.Fatal("bad digest")
	}
	staged, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("bytes must be staged for a presigned hit: %v", err)
	}
	if string(staged) != content {
		t.Errorf("staged=%q want %q", staged, content)
	}
}

// TestProviderObjectCache_LookupFailureIsAMiss proves every failure mode of the
// object cache is a local build: a server fault reads as a cold cache, never as
// an error on the compiler's critical path.
func TestProviderObjectCache_LookupFailureIsAMiss(t *testing.T) {
	f := newFakeObjectServer(t)
	exchangeDir := shortExchangeDir(t)
	_, result := startObjectSession(t, f, exchangeDir, coreCapabilities()...)
	client := dialObjects(t, result.ObjectCacheSocket)

	// Take the server away mid-session: the index round trip now fails.
	f.srv.Close()

	got, err := client.Get(&cache.ObjectGetParams{Namespace: objectNamespace, IDs: []string{objectID('a')}})
	if err != nil {
		t.Fatalf("object-get must succeed with an empty result, not fail: %v", err)
	}
	if len(got.Objects) != 0 {
		t.Fatalf("objects=%+v want none", got.Objects)
	}
}

// TestProviderObjectCache_SocketClosesWithTheSession proves the socket serves
// for the session's lifetime and is removed at shutdown, so a later run never
// dials a dead path.
func TestProviderObjectCache_SocketClosesWithTheSession(t *testing.T) {
	f := newFakeObjectServer(t)
	exchangeDir := shortExchangeDir(t)
	t.Setenv("PUTNAMI_CACHE_URL", f.url())
	t.Setenv("PUTNAMI_CACHE_TOKEN", "test-token")
	enableCacheWritesForTest(t)

	s := newProviderSession(t.TempDir(), io.Discard)
	result := mustInit(t, s, &cache.InitializeParams{
		ProtocolVersion: cache.ProviderProtocolMinVersion,
		BlobExchangeDir: exchangeDir,
		Capabilities:    coreCapabilities(),
	})
	assertSocket(t, result, exchangeDir)

	s.close()
	if _, err := os.Stat(result.ObjectCacheSocket); err == nil {
		t.Error("the socket file survived the session")
	}
	if _, err := objectcachetest.Dial(result.ObjectCacheSocket); err == nil {
		t.Error("the socket still accepts connections after shutdown")
	}
}

// TestProviderObjectCache_SummaryDrainsQueuedPuts pins the wiring, not just the
// method: the contract acknowledges a put as QUEUED and promises the bytes are
// durable at summary, so the summary op itself must drain the upload queue.
func TestProviderObjectCache_SummaryDrainsQueuedPuts(t *testing.T) {
	f := newFakeObjectServer(t)
	exchangeDir := shortExchangeDir(t)
	s, result := startObjectSession(t, f, exchangeDir, coreCapabilities()...)
	assertSocket(t, result, exchangeDir)
	client := dialObjects(t, result.ObjectCacheSocket)

	id := objectID('a')
	content := []byte("bytes that must be durable at summary")
	digest, err := objectcachetest.StageBlob(exchangeDir, content)
	if err != nil {
		t.Fatalf("stage blob: %v", err)
	}
	if _, err := client.Put(&cache.ObjectPutParams{
		Namespace: objectNamespace,
		Objects:   []cache.ObjectPut{{ID: id, Digest: digest, Size: int64(len(content))}},
	}); err != nil {
		t.Fatalf("object-put: %v", err)
	}

	resp := s.summary(&cache.ProviderRequest{
		ProtocolVersion: cache.ProviderProtocolVersion, ID: 99, Op: cache.OpSummary,
	})
	if !resp.OK {
		t.Fatalf("summary not OK: %+v", resp.Error)
	}
	if _, ok := f.storedObject(objectNamespace, id); !ok {
		t.Fatal("summary returned before the queued put was durable")
	}

	// A put after the drain is refused rather than silently lost or, worse, sent
	// on a closed queue.
	late, err := client.Put(&cache.ObjectPutParams{
		Namespace: objectNamespace,
		Objects:   []cache.ObjectPut{{ID: objectID('b'), Digest: digest, Size: int64(len(content))}},
	})
	if err != nil {
		t.Fatalf("late object-put: %v", err)
	}
	if late.Accepted != 0 {
		t.Errorf("accepted=%d want 0 after the drain: the run can no longer promise durability", late.Accepted)
	}
}

// TestProviderObjectCache_ShutdownDoesNotWaitOnAnIdleClient proves shutdown is
// bounded even when a job process left a connection open. Closing the listener
// alone only stops new dials — the serving goroutine would stay parked in a
// blocking read, and the provider would hang until core force-terminated it.
func TestProviderObjectCache_ShutdownDoesNotWaitOnAnIdleClient(t *testing.T) {
	f := newFakeObjectServer(t)
	exchangeDir := shortExchangeDir(t)
	t.Setenv("PUTNAMI_CACHE_URL", f.url())
	t.Setenv("PUTNAMI_CACHE_TOKEN", "test-token")
	enableCacheWritesForTest(t)

	s := newProviderSession(t.TempDir(), io.Discard)
	result := mustInit(t, s, &cache.InitializeParams{
		ProtocolVersion: cache.ProviderProtocolMinVersion,
		BlobExchangeDir: exchangeDir,
		Capabilities:    coreCapabilities(),
	})
	assertSocket(t, result, exchangeDir)

	// Two clients: one that spoke and went quiet, one that never spoke. Neither
	// closes its connection — exactly what an exiting job leaves behind.
	idle, err := objectcachetest.Dial(result.ObjectCacheSocket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := idle.Get(&cache.ObjectGetParams{Namespace: objectNamespace, IDs: []string{objectID('a')}}); err != nil {
		t.Fatalf("object-get: %v", err)
	}
	silent, err := objectcachetest.Dial(result.ObjectCacheSocket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = silent // deliberately never used: it holds an open, silent connection

	done := make(chan struct{})
	go func() {
		s.close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("session shutdown blocked on an idle socket client")
	}
}

// TestProviderObjectCache_GetBatchIsOneRoundTrip proves the batching promise:
// a whole wave of ids costs ONE index round trip, which is the point of the
// contract's batched get.
func TestProviderObjectCache_GetBatchIsOneRoundTrip(t *testing.T) {
	f := newFakeObjectServer(t)
	exchangeDir := shortExchangeDir(t)
	ids := make([]string, 64)
	for i := range ids {
		ids[i] = fmt.Sprintf("%064x", i+1)
		f.seed(objectNamespace, ids[i], fmt.Sprintf("object %d", i), "", cache.ChannelTrusted)
	}
	_, result := startObjectSession(t, f, exchangeDir, coreCapabilities()...)
	client := dialObjects(t, result.ObjectCacheSocket)

	got, err := client.Get(&cache.ObjectGetParams{Namespace: objectNamespace, IDs: ids})
	if err != nil {
		t.Fatalf("object-get: %v", err)
	}
	if len(got.Objects) != len(ids) {
		t.Fatalf("objects=%d want %d", len(got.Objects), len(ids))
	}
	if lookups, _ := f.stats(); lookups != 1 {
		t.Errorf("index lookups=%d want exactly 1 for a batch of %d ids", lookups, len(ids))
	}
}

// TestProviderObjectCache_RestagesPoisonedBytes proves a hit's bytes are the
// object's bytes even when something already sits at that content address.
//
// This is the consequence of the exchange directory's new writer set. Before the
// object cache, only core and the provider wrote there and a stat-and-size check
// was enough to skip a re-download. The socket contract makes every JOB PROCESS
// a writer of that directory, so "a file of the right size is already present"
// became a claim a peer can manufacture — and the provider promises its caller
// that the bytes at the digest it reports ARE that digest's content.
func TestProviderObjectCache_RestagesPoisonedBytes(t *testing.T) {
	f := newFakeObjectServer(t)
	exchangeDir := shortExchangeDir(t)
	_, result := startObjectSession(t, f, exchangeDir, coreCapabilities()...)
	client := dialObjects(t, result.ObjectCacheSocket)

	id := objectID('a')
	content := "the real object bytes"
	digest := f.seed(objectNamespace, id, content, "out-1", cache.ChannelTrusted)

	// A hostile job pre-stages same-length junk at the object's content address.
	path, ok := cache.BlobExchangePath(exchangeDir, digest)
	if !ok {
		t.Fatal("bad digest")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	poison := strings.Repeat("X", len(content))
	if err := os.WriteFile(path, []byte(poison), 0o600); err != nil {
		t.Fatalf("write poison: %v", err)
	}

	got, err := client.Get(&cache.ObjectGetParams{Namespace: objectNamespace, IDs: []string{id}})
	if err != nil {
		t.Fatalf("object-get: %v", err)
	}
	if len(got.Objects) != 1 {
		t.Fatalf("objects=%d want 1", len(got.Objects))
	}
	staged, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read staged: %v", err)
	}
	if string(staged) != content {
		t.Fatalf("staged=%q want %q: a size-matching file a peer planted must be re-staged, not reported as the hit",
			staged, content)
	}
}

// TestProviderObjectCache_RefusesACrossWiredDownload proves a served record
// whose presigned GET addresses OTHER content is a miss.
//
// The download is content-verified against the TRANSFER's digest while the bytes
// land at the RECORD's digest. Those agree on every correct response, so a
// server that cross-wires them — a bug, or a compromised server — would
// otherwise park object B's verified bytes at object A's content address, which
// is the one phantom hit no downstream digest check can catch.
func TestProviderObjectCache_RefusesACrossWiredDownload(t *testing.T) {
	f := newFakeObjectServer(t)
	exchangeDir := shortExchangeDir(t)
	_, result := startObjectSession(t, f, exchangeDir, coreCapabilities()...)
	client := dialObjects(t, result.ObjectCacheSocket)

	id := objectID('a')
	wanted := strings.Repeat("w", int(cache.MaxInlineBlobBytes)+1) // too large to inline: takes the download branch
	digest := f.seed(objectNamespace, id, wanted, "out-1", cache.ChannelTrusted)
	other := f.seed(objectNamespace, objectID('b'), strings.Repeat("o", int(cache.MaxInlineBlobBytes)+1), "out-2", cache.ChannelTrusted)
	f.crossWireDownload(objectNamespace, id, other)

	got, err := client.Get(&cache.ObjectGetParams{Namespace: objectNamespace, IDs: []string{id}})
	if err != nil {
		t.Fatalf("object-get: %v", err)
	}
	if len(got.Objects) != 0 {
		t.Fatalf("objects=%d want 0: a record whose download addresses other content is a miss", len(got.Objects))
	}
	path, ok := cache.BlobExchangePath(exchangeDir, digest)
	if !ok {
		t.Fatal("bad digest")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("bytes landed at the requested object's content address from a cross-wired download")
	}
}

// TestProviderObjectCache_RelocatesATooLongSocketPath proves the object cache
// survives an exchange directory whose path leaves no room for the socket.
//
// It is not a corner case: core's exchange directory is a per-run temp directory
// under the machine-global store, which on a default macOS install already
// measures 93 of the platform's ~104 bytes. A home directory a dozen characters
// longer would have disabled the compiler cache for every build on that machine.
// What the relocation must preserve is the only thing a client can derive — the
// exchange directory is the socket's PARENT — so object bytes move with it.
func TestProviderObjectCache_RelocatesATooLongSocketPath(t *testing.T) {
	f := newFakeObjectServer(t)
	deep := filepath.Join(shortExchangeDir(t), strings.Repeat("d", 120))
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Skipf("cannot create a deep exchange directory: %v", err)
	}
	if len(filepath.Join(deep, objectSocketName)) <= maxObjectSocketPath {
		t.Fatal("the fixture directory is not long enough to exercise the fallback")
	}
	s, result := startObjectSession(t, f, deep, coreCapabilities()...)
	// assertSocket's own contract: parent == the exchange directory bytes travel
	// through, which after a relocation is the fallback directory, not core's.
	assertSocket(t, result, s.objects.exchangeDir)
	if s.objects.exchangeDir == deep {
		t.Fatal("the socket stayed under the too-long exchange directory")
	}

	client := dialObjects(t, result.ObjectCacheSocket)
	id := objectID('a')
	content := "relocated object"
	f.seed(objectNamespace, id, content, "out-1", cache.ChannelTrusted)
	got, err := client.Get(&cache.ObjectGetParams{Namespace: objectNamespace, IDs: []string{id}})
	if err != nil {
		t.Fatalf("object-get: %v", err)
	}
	if len(got.Objects) != 1 {
		t.Fatalf("objects=%d want 1 through the relocated socket", len(got.Objects))
	}
	// The bytes are where a client derives them: the socket's parent.
	path, ok := cache.BlobExchangePath(filepath.Dir(result.ObjectCacheSocket), got.Objects[0].Digest)
	if !ok {
		t.Fatal("bad digest")
	}
	staged, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("a hit's bytes must be readable from the socket's parent: %v", err)
	}
	if string(staged) != content {
		t.Fatalf("staged=%q want %q", staged, content)
	}
}

// TestProviderObjectCache_APeerThatStopsReadingDoesNotStarveAnother is the
// head-of-line test: a job process that pipelines requests and never reads the
// answers must cost only itself.
//
// Two bounds carry it. The per-connection in-flight bound is taken on the read
// loop, so the flood becomes backpressure on the peer's own socket instead of a
// goroutine per queued line; and the response write deadline releases the
// session-wide request slots the stalled peer is holding. Without either, a
// single job silently disables every other job's compiler cache for the run —
// and silence, not a refusal, is what the other job's compiler waits on.
func TestProviderObjectCache_APeerThatStopsReadingDoesNotStarveAnother(t *testing.T) {
	objectWriteTimeout = 200 * time.Millisecond
	t.Cleanup(func() { objectWriteTimeout = 30 * time.Second })

	f := newFakeObjectServer(t)
	exchangeDir := shortExchangeDir(t)
	_, result := startObjectSession(t, f, exchangeDir, coreCapabilities()...)

	// A raw connection: write many requests, never read a byte of the answers.
	hostile, err := net.Dial("unix", result.ObjectCacheSocket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = hostile.Close() })
	line, err := json.Marshal(&cache.ProviderRequest{
		ProtocolVersion: cache.ProviderProtocolVersion,
		ID:              1,
		Op:              cache.OpObjectGet,
		Payload:         mustPayload(t, &cache.ObjectGetParams{Namespace: objectNamespace, IDs: []string{objectID('a')}}),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	flood := bytes.Repeat(append(line, '\n'), 2000)
	go func() { _, _ = hostile.Write(flood) }()

	// The flood is 2000 pipelined requests on one connection. Before the
	// per-connection bound was taken on the read loop, that produced a goroutine
	// per queued line (measured: ~49,700); with it, this connection can never
	// hold more than objectConnRequests. The ceiling is loose on purpose — the
	// test server, the well-behaved client and the runtime all add a few — but
	// three orders of magnitude below the regression it guards.
	const goroutineCeiling = 200
	before := runtime.NumGoroutine()

	// A well-behaved client must still be answered.
	id := objectID('b')
	f.seed(objectNamespace, id, "well-behaved", "out-1", cache.ChannelTrusted)
	good := dialObjects(t, result.ObjectCacheSocket)
	done := make(chan int, 1)
	go func() {
		got, gerr := good.Get(&cache.ObjectGetParams{Namespace: objectNamespace, IDs: []string{id}})
		if gerr != nil {
			done <- -1
			return
		}
		done <- len(got.Objects)
	}()
	select {
	case hits := <-done:
		if hits != 1 {
			t.Fatalf("hits=%d want 1 for the well-behaved client", hits)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a well-behaved client got no answer while a peer flooded the socket")
	}

	// The flood must not have become a goroutine per queued request.
	if grew := runtime.NumGoroutine() - before; grew > goroutineCeiling {
		t.Errorf("goroutines grew by %d under a 2000-request flood; the per-connection bound must be taken on the read loop", grew)
	}
}

// TestProviderObjectCache_SilentConnectionReleasesItsSlot proves a job that
// dials and never speaks does not hold a connection slot for the run. Enough of
// those would stop the accept loop, and every later job would read that as a
// cold cache.
func TestProviderObjectCache_SilentConnectionReleasesItsSlot(t *testing.T) {
	objectFirstRequestTimeout = 200 * time.Millisecond
	t.Cleanup(func() { objectFirstRequestTimeout = 30 * time.Second })

	f := newFakeObjectServer(t)
	exchangeDir := shortExchangeDir(t)
	s, result := startObjectSession(t, f, exchangeDir, coreCapabilities()...)

	silent, err := net.Dial("unix", result.ObjectCacheSocket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = silent.Close() })

	deadline := time.Now().Add(5 * time.Second)
	for {
		s.objects.connMu.Lock()
		live := len(s.objects.conns)
		s.objects.connMu.Unlock()
		if live == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("live connections=%d: a connection that never sent a request must be reclaimed", live)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestProviderObjectCache_DrainIsBounded proves the end-of-run object drain
// cannot spend core's whole summary budget.
//
// Core answers summary under 60s and discards the ENTIRE SummaryResult when that
// expires, so an object backlog against a slow cache server would cost the run
// its task-entry upload confirmations — trading what the cache exists for
// against an accelerator. The drain therefore gives up and says so.
func TestProviderObjectCache_DrainIsBounded(t *testing.T) {
	objectDrainTimeout = 150 * time.Millisecond
	t.Cleanup(func() { objectDrainTimeout = 20 * time.Second })

	f := newFakeObjectServer(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f.blockStores(release)

	exchangeDir := shortExchangeDir(t)
	s, result := startObjectSession(t, f, exchangeDir, coreCapabilities()...)
	client := dialObjects(t, result.ObjectCacheSocket)

	content := []byte("queued object")
	digest, err := objectcachetest.StageBlob(exchangeDir, content)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if _, err := client.Put(&cache.ObjectPutParams{
		Namespace: objectNamespace,
		Objects:   []cache.ObjectPut{{ID: objectID('a'), Digest: digest, Size: int64(len(content))}},
	}); err != nil {
		t.Fatalf("object-put: %v", err)
	}

	start := time.Now()
	s.objects.drain()
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("drain took %s; it must be bounded by objectDrainTimeout", elapsed)
	}
}

// stageObject stages one object's bytes in the exchange directory and returns
// the put the job process would offer for it.
func stageObject(t *testing.T, exchangeDir, id, content string) cache.ObjectPut {
	t.Helper()
	digest, err := objectcachetest.StageBlob(exchangeDir, []byte(content))
	if err != nil {
		t.Fatalf("stage %s: %v", id, err)
	}
	return cache.ObjectPut{ID: id, Digest: digest, Size: int64(len(content))}
}

// pinProbeWait fixes the probe wait for one test and restores it afterwards. It
// must be called BEFORE the session starts: cleanups run last-in-first-out, so
// the session (and its upload workers, which read the var) is then closed before
// the value moves back.
func pinProbeWait(t *testing.T, d time.Duration) {
	t.Helper()
	prev := objectWriteProbeWait
	objectWriteProbeWait = d
	t.Cleanup(func() { objectWriteProbeWait = prev })
}

// pinAuthRefusalGrace does the same for the 401 grace window, under the same
// ordering rule.
func pinAuthRefusalGrace(t *testing.T, d time.Duration) {
	t.Helper()
	prev := objectAuthRefusalGrace
	objectAuthRefusalGrace = d
	t.Cleanup(func() { objectAuthRefusalGrace = prev })
}

// TestProviderObjectCache_ARefusedStoreCostsOneBatch is the regression for the
// write path's worst shape: the store route is gated on a scope this run's
// credential does not carry, so every store answers 403.
//
// StoreObjects uploads the object BYTES into the CAS before it asks for the
// index row, so without a latch a cold build offers tens of thousands of objects
// and pushes every one of them into cas_blobs as a blob no object row will ever
// name — counted against the byte quotas whose sweep then evicts real
// action-cache blobs to make room for garbage nothing can read. Logging the
// refusal and returning is not enough; the next batch repeats it.
//
// The store is held open until every batch is queued, which is what makes the
// count exact rather than timing-dependent: one batch probes the write path, the
// others wait for its verdict instead of racing it.
func TestProviderObjectCache_ARefusedStoreCostsOneBatch(t *testing.T) {
	// The probe wait exists so a stalled upload cannot park the pipeline; it must
	// not fire while this test deliberately holds the store, or the count below
	// would measure the timeout rather than the latch.
	pinProbeWait(t, time.Minute)
	f := newFakeObjectServer(t)
	f.refuseStores(http.StatusForbidden)
	release := make(chan struct{})
	f.blockStores(release)

	exchangeDir := shortExchangeDir(t)
	logw := &lockedWriter{}
	s, result := startObjectSessionLogged(t, f, exchangeDir, logw, coreCapabilities()...)
	client := dialObjects(t, result.ObjectCacheSocket)

	const batches = 6
	for i := 0; i < batches; i++ {
		obj := stageObject(t, exchangeDir, objectID(byte('a'+i)), fmt.Sprintf("compiled output %d", i))
		put, err := client.Put(&cache.ObjectPutParams{Namespace: objectNamespace, Objects: []cache.ObjectPut{obj}})
		if err != nil {
			t.Fatalf("object-put %d: %v", i, err)
		}
		if put.Accepted != 1 {
			t.Fatalf("put %d accepted=%d; nothing is refused before the server has answered once", i, put.Accepted)
		}
	}

	close(release)    // the probe learns the verdict
	s.objects.drain() // every worker finishes: the latch is set or it never will be

	if _, stores := f.stats(); stores != 1 {
		t.Errorf("the server received %d store(s); a refusal must be learned ONCE and latched, not re-learned per batch", stores)
	}
	if got := f.uploadedBlobs(); got != 1 {
		t.Errorf("%d object blob(s) reached the CAS; only the probe batch may pay bytes for a refusal, "+
			"or every refused run fills cas_blobs with rows no object row names", got)
	}

	// A put after the latch is answered without a round trip, and it is REFUSED,
	// not dropped: the two have different causes and different fixes.
	obj := stageObject(t, exchangeDir, objectID('0'), "after the refusal")
	put, err := client.Put(&cache.ObjectPutParams{Namespace: objectNamespace, Objects: []cache.ObjectPut{obj}})
	if err != nil {
		t.Fatalf("object-put after the refusal: %v", err)
	}
	if put.Accepted != 0 {
		t.Fatalf("accepted=%d after a refused write path; a put the provider cannot store must not be acknowledged", put.Accepted)
	}
	s.objects.mu.Lock()
	refused, dropped, stored := s.objects.refused, s.objects.dropped, s.objects.stored
	s.objects.mu.Unlock()
	if refused == 0 || dropped != 0 {
		t.Errorf("refused=%d dropped=%d; a refusal is not a full queue and must be counted as its own cause", refused, dropped)
	}
	if stored != 0 {
		t.Errorf("stored=%d after every store was refused", stored)
	}

	// The run must not be able to look warm. A refused writer reports stored=0,
	// which is byte-identical to a fully deduped run unless the refusal is said.
	s.objects.report()
	log := logw.String()
	if !strings.Contains(log, "refused this run's object write (HTTP 403)") || !strings.Contains(log, "REFUSED") {
		t.Errorf("the provider log must state the refusal, its status, and its consequence, got %q", log)
	}
	// A 403 is a verdict on the credential's class, so the line has to NAME the
	// grant. "The server refused" sends the reader to the cache-server source to
	// learn that no service-account identity may hold the scope and that a
	// per-run lease is the only thing that carries it.
	if !strings.Contains(log, objectWriteScope) || !strings.Contains(log, "cache write lease") {
		t.Errorf("a 403 refusal must name the missing grant and what carries it, got %q", log)
	}
	// Said ONCE, not once per batch: six batches were queued.
	if n := strings.Count(log, "refused this run's object write"); n != 1 {
		t.Errorf("the refusal is stated %d times; it is latched, so it is stated once", n)
	}
}

// pinReadBreakerLookups shortens the read breaker's evidence threshold for a
// test, under the same ordering rule as the deadline pins above: set it before
// the session starts, restore it after.
func pinReadBreakerLookups(t *testing.T, n int) {
	t.Helper()
	prev := objectReadBreakerLookups
	objectReadBreakerLookups = n
	t.Cleanup(func() { objectReadBreakerLookups = prev })
}

// latchWriteRefusal drives one refused store so the run's write path is closed,
// which is the first of the two facts the read breaker needs.
func latchWriteRefusal(t *testing.T, s *providerSession, client *objectcachetest.Client, exchangeDir string) {
	t.Helper()
	obj := stageObject(t, exchangeDir, objectID('f'), "refused output")
	if _, err := client.Put(&cache.ObjectPutParams{Namespace: objectNamespace, Objects: []cache.ObjectPut{obj}}); err != nil {
		t.Fatalf("object-put: %v", err)
	}
	s.objects.drain()
	if !s.objects.writeRefusedLatched() {
		t.Fatal("the write path did not latch; the read breaker's premise is not set up")
	}
}

// missIDs returns n distinct ids the fake server was never seeded with.
func missIDs(n int) []string {
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, fmt.Sprintf("%064x", i+1))
	}
	return ids
}

// TestProviderObjectCache_ARefusedWriterStopsPayingForEmptyLookups is the
// regression test for the cost of empty lookups on a refused writer.
//
// A lookup is a SYNCHRONOUS round trip on the compiler's critical path.
// negotiateObjectCache already refuses to open the socket against a server with
// no object index rather than make every compilation pay a round trip to be told
// 404 — but that reasoning stopped at first contact. A run whose writes the
// server refuses stores nothing, so nothing it compiles can ever populate the
// namespace; once its answered lookups have served it zero objects, every later
// one is a round trip whose expected value is zero. The reported run spent 9812
// of them to be served 0 objects.
//
// The breaker declines those lookups; it never fails one. A closed path answers
// the same empty result a miss answers.
func TestProviderObjectCache_ARefusedWriterStopsPayingForEmptyLookups(t *testing.T) {
	pinProbeWait(t, time.Minute)
	pinReadBreakerLookups(t, 4)
	f := newFakeObjectServer(t)
	f.refuseStores(http.StatusForbidden)

	exchangeDir := shortExchangeDir(t)
	logw := &lockedWriter{}
	s, result := startObjectSessionLogged(t, f, exchangeDir, logw, coreCapabilities()...)
	client := dialObjects(t, result.ObjectCacheSocket)
	latchWriteRefusal(t, s, client, exchangeDir)

	for i, id := range missIDs(9) {
		got, err := client.Get(&cache.ObjectGetParams{Namespace: objectNamespace, IDs: []string{id}})
		if err != nil {
			t.Fatalf("object-get %d: %v", i, err)
		}
		// Every get is answered, before and after the breaker: a declined lookup
		// is a miss, not an error, so the compiler's behavior never changes.
		if len(got.Objects) != 0 {
			t.Fatalf("object-get %d served %d object(s) from an empty namespace", i, len(got.Objects))
		}
	}

	if lookups, _ := f.stats(); lookups != 4 {
		t.Errorf("the server received %d lookup(s); a refused writer that has been served nothing must stop asking after %d, "+
			"or every compilation keeps paying a round trip for a namespace it cannot warm", lookups, objectReadBreakerLookups)
	}
	if !s.objects.readClosedLatched() {
		t.Error("the read path is still open after the evidence threshold")
	}

	log := logw.String()
	if !strings.Contains(log, "No further lookups are issued this run") || !strings.Contains(log, objectWriteScope) {
		t.Errorf("closing the read path must be said once and must name the missing grant, got %q", log)
	}
	if n := strings.Count(log, "No further lookups are issued this run"); n != 1 {
		t.Errorf("the breaker is stated %d times; it is latched, so it is stated once", n)
	}

	// It must reach the summary line too: a run that stopped asking reports the
	// same "served 0 object(s)" as one that asked ten thousand times.
	s.objects.report()
	if summary := logw.String(); !strings.Contains(summary, "LOOKUPS CLOSED after 4 answered served 0 (5 id(s) not asked") {
		t.Errorf("the summary must say the breaker fired and what it saved, got %q", summary)
	}
}

// TestProviderObjectCache_TheReadBreakerNeedsARefusedWriter is the first half of
// what must NOT close the read path. Zero hits on their own are an ordinary cold
// run: it stores what it compiles, so the namespace it is reading is the one it
// is about to warm, and a later lookup in the same build can hit.
func TestProviderObjectCache_TheReadBreakerNeedsARefusedWriter(t *testing.T) {
	pinReadBreakerLookups(t, 2)
	f := newFakeObjectServer(t)

	exchangeDir := shortExchangeDir(t)
	s, result := startObjectSessionLogged(t, f, exchangeDir, io.Discard, coreCapabilities()...)
	client := dialObjects(t, result.ObjectCacheSocket)

	for i, id := range missIDs(6) {
		if _, err := client.Get(&cache.ObjectGetParams{Namespace: objectNamespace, IDs: []string{id}}); err != nil {
			t.Fatalf("object-get %d: %v", i, err)
		}
	}
	if lookups, _ := f.stats(); lookups != 6 {
		t.Errorf("the server received %d lookup(s) of 6; a cold run whose writes are accepted must keep reading", lookups)
	}
	if s.objects.readClosedLatched() {
		t.Error("the read path closed on a run that can still warm the namespace it is reading")
	}
}

// TestProviderObjectCache_TheReadBreakerNeedsZeroObjectsServed is the other
// half. A refused writer still reads a namespace some trusted run filled, and
// the pinned lanes exist to do exactly that — so one served object is proof
// the reads are worth their round trips, whatever the write verdict was.
func TestProviderObjectCache_TheReadBreakerNeedsZeroObjectsServed(t *testing.T) {
	pinProbeWait(t, time.Minute)
	pinReadBreakerLookups(t, 2)
	f := newFakeObjectServer(t)
	f.refuseStores(http.StatusForbidden)
	seeded := objectID('e')
	f.seed(objectNamespace, seeded, "warmed by a trusted run", "", cache.ChannelTrusted)

	exchangeDir := shortExchangeDir(t)
	s, result := startObjectSessionLogged(t, f, exchangeDir, io.Discard, coreCapabilities()...)
	client := dialObjects(t, result.ObjectCacheSocket)
	latchWriteRefusal(t, s, client, exchangeDir)

	got, err := client.Get(&cache.ObjectGetParams{Namespace: objectNamespace, IDs: []string{seeded}})
	if err != nil {
		t.Fatalf("object-get seeded: %v", err)
	}
	if len(got.Objects) != 1 {
		t.Fatalf("the seeded object was served %d time(s); the premise of this test is a hit", len(got.Objects))
	}
	for i, id := range missIDs(6) {
		if _, err := client.Get(&cache.ObjectGetParams{Namespace: objectNamespace, IDs: []string{id}}); err != nil {
			t.Fatalf("object-get %d: %v", i, err)
		}
	}
	if lookups, _ := f.stats(); lookups != 7 {
		t.Errorf("the server received %d lookup(s) of 7; one served object proves the reads are worth their round trips", lookups)
	}
	if s.objects.readClosedLatched() {
		t.Error("the read path closed on a namespace that served this run an object")
	}
}

// TestProviderObjectCache_ATransientStoreFailureDoesNotLatch is the first half of
// what must NOT latch: a 5xx, a timeout, or a malformed answer can differ on the
// next batch, so latching on one would turn a blip into a run-long outage of the
// compiler cache.
func TestProviderObjectCache_ATransientStoreFailureDoesNotLatch(t *testing.T) {
	f := newFakeObjectServer(t)
	f.refuseStores(http.StatusInternalServerError)

	exchangeDir := shortExchangeDir(t)
	s, result := startObjectSessionLogged(t, f, exchangeDir, io.Discard, coreCapabilities()...)
	client := dialObjects(t, result.ObjectCacheSocket)

	first := stageObject(t, exchangeDir, objectID('a'), "first batch")
	if put, err := client.Put(&cache.ObjectPutParams{Namespace: objectNamespace, Objects: []cache.ObjectPut{first}}); err != nil || put.Accepted != 1 {
		t.Fatalf("first put = %+v err=%v", put, err)
	}
	waitUntil(t, "the server answers the first store", func() bool {
		_, stores := f.stats()
		return stores >= 1
	})
	if s.objects.writeRefusedLatched() {
		t.Fatal("a 500 must not close the object write path for the run")
	}

	// The server recovers, and the next batch of the SAME run stores normally.
	f.refuseStores(0)
	second := stageObject(t, exchangeDir, objectID('b'), "second batch")
	if put, err := client.Put(&cache.ObjectPutParams{Namespace: objectNamespace, Objects: []cache.ObjectPut{second}}); err != nil || put.Accepted != 1 {
		t.Fatalf("second put = %+v err=%v", put, err)
	}
	s.objects.drain()
	if _, ok := f.storedObject(objectNamespace, objectID('b')); !ok {
		t.Error("a recovered server must index the next batch: the failure was transient, not a credential verdict")
	}
}

// TestProviderObjectCache_ATransient401DoesNotLatch is the second half, and the
// one that costs a production run: a 401 is NOT a property of the credential the
// way a 403 is.
//
// A publish burst once drove uncached delegation introspection past the auth
// server's rate limit and its breaker failed closed with 401 for about 30
// seconds. A 45-minute pinned main run that landed one objects store inside that
// window would, if 401 latched, persist zero compiler objects for the remaining
// 33 minutes — and its log would name an HTTP 401, pointing the operator at the
// lease scope instead of at a breaker.
func TestProviderObjectCache_ATransient401DoesNotLatch(t *testing.T) {
	f := newFakeObjectServer(t)
	f.refuseStores(http.StatusUnauthorized)

	exchangeDir := shortExchangeDir(t)
	logw := &lockedWriter{}
	s, result := startObjectSessionLogged(t, f, exchangeDir, logw, coreCapabilities()...)
	client := dialObjects(t, result.ObjectCacheSocket)

	first := stageObject(t, exchangeDir, objectID('a'), "inside the breaker window")
	if put, err := client.Put(&cache.ObjectPutParams{Namespace: objectNamespace, Objects: []cache.ObjectPut{first}}); err != nil || put.Accepted != 1 {
		t.Fatalf("first put = %+v err=%v", put, err)
	}
	// The provider's own line is the barrier, not the server's counter: the
	// counter moves before the client has even read the response.
	waitUntil(t, "the provider to record the 401", func() bool {
		return strings.Contains(logw.String(), "refused an object write (HTTP 401)")
	})
	if s.objects.writeRefusedLatched() {
		t.Fatal("a single 401 closed the object write path for the run; a transient auth failure must not cost 33 minutes of object cache")
	}
	if log := logw.String(); !strings.Contains(log, "the write path stays OPEN") {
		t.Errorf("the provider must say the refusal and that it did NOT close the path, got %q", log)
	}

	// The breaker closes, and the SAME run stores normally again.
	f.refuseStores(0)
	second := stageObject(t, exchangeDir, objectID('b'), "after the breaker closed")
	if put, err := client.Put(&cache.ObjectPutParams{Namespace: objectNamespace, Objects: []cache.ObjectPut{second}}); err != nil || put.Accepted != 1 {
		t.Fatalf("second put = %+v err=%v", put, err)
	}
	s.objects.drain()
	if _, ok := f.storedObject(objectNamespace, objectID('b')); !ok {
		t.Error("the run did not recover its object cache after a transient 401")
	}

	// The summary must report both halves: the object the blip cost, and the
	// fact that the path stayed open. A run that stored objects must never read
	// as one that persisted nothing.
	s.objects.report()
	log := logw.String()
	if !strings.Contains(log, "stored 1") || !strings.Contains(log, "REFUSED 1 (the server refused these writes; the write path stayed open)") {
		t.Errorf("the summary line must show the recovered store AND the refusal it survived, got %q", log)
	}
}

// TestProviderObjectCache_APersistent401Latches is the other side of that rule: a
// bearer that is genuinely dead must still stop costing the run round trips.
//
// The test is TIME, not a count: 30 seconds of a fail-closed breaker easily
// produces hundreds of consecutive 401s across four upload workers, so only
// "refusals have kept coming for longer than any known blip" separates a dead
// credential from a blip.
func TestProviderObjectCache_APersistent401Latches(t *testing.T) {
	const grace = 50 * time.Millisecond
	pinAuthRefusalGrace(t, grace)

	f := newFakeObjectServer(t)
	f.refuseStores(http.StatusUnauthorized)

	exchangeDir := shortExchangeDir(t)
	logw := &lockedWriter{}
	s, result := startObjectSessionLogged(t, f, exchangeDir, logw, coreCapabilities()...)
	client := dialObjects(t, result.ObjectCacheSocket)

	first := stageObject(t, exchangeDir, objectID('a'), "opens the streak")
	if put, err := client.Put(&cache.ObjectPutParams{Namespace: objectNamespace, Objects: []cache.ObjectPut{first}}); err != nil || put.Accepted != 1 {
		t.Fatalf("first put = %+v err=%v", put, err)
	}
	waitUntil(t, "the streak's first 401", func() bool {
		return strings.Contains(logw.String(), "refused an object write (HTTP 401)")
	})
	if s.objects.writeRefusedLatched() {
		t.Fatal("the first 401 of a streak must not latch; the grace window is what tells a dead bearer from a blip")
	}

	time.Sleep(2 * grace)
	second := stageObject(t, exchangeDir, objectID('b'), "outlasts the grace")
	if put, err := client.Put(&cache.ObjectPutParams{Namespace: objectNamespace, Objects: []cache.ObjectPut{second}}); err != nil || put.Accepted != 1 {
		t.Fatalf("second put = %+v err=%v", put, err)
	}
	waitUntil(t, "the write path to latch on the persistent 401", func() bool {
		return s.objects.writeRefusedLatched()
	})
	if log := logw.String(); !strings.Contains(log, "refused this run's object write (HTTP 401)") {
		t.Errorf("the latch must state its status, got %q", log)
	}
}

// TestProviderObjectCache_TheProbeWaitIsBounded proves the probe gate cannot park
// the pipeline.
//
// The batch holding the probe uploads its blobs BEFORE the store that yields the
// verdict, and that upload carries no deadline: the client applies its
// control-plane timeout per authed round trip, deliberately not to blob
// transfers, and sets no client-wide timeout. So the first put of a run — a
// large linked test binary, or a storage endpoint that stopped answering —
// would otherwise idle every other upload worker until the end-of-run drain gave
// up and the run persisted nothing. Three batches of wasted CAS bytes is the
// bounded cost; a stalled pipeline is not.
func TestProviderObjectCache_TheProbeWaitIsBounded(t *testing.T) {
	pinProbeWait(t, 100*time.Millisecond)

	f := newFakeObjectServer(t)
	release := make(chan struct{})
	defer close(release) // before the session's cleanup, so the drain is not gated
	f.blockBlobPuts(release)

	exchangeDir := shortExchangeDir(t)
	_, result := startObjectSession(t, f, exchangeDir, coreCapabilities()...)
	client := dialObjects(t, result.ObjectCacheSocket)

	for i, id := range []string{objectID('a'), objectID('b')} {
		obj := stageObject(t, exchangeDir, id, fmt.Sprintf("stalled upload %d", i))
		if put, err := client.Put(&cache.ObjectPutParams{Namespace: objectNamespace, Objects: []cache.ObjectPut{obj}}); err != nil || put.Accepted != 1 {
			t.Fatalf("put %d = %+v err=%v", i, put, err)
		}
	}

	waitUntil(t, "the second worker to upload while the probe is stalled", func() bool {
		return f.startedBlobPuts() >= 2
	})
	if _, stores := f.stats(); stores != 0 {
		t.Fatalf("stores=%d; the probe is supposed to be stalled inside its upload, so no verdict exists yet", stores)
	}
}

// TestProviderObjectCache_AProbeThatLearnedNothingHandsTheSlotOn pins the other
// half of the one-batch guarantee.
//
// A batch whose every staged digest fails verification returns BEFORE any store,
// so it produced no verdict. Releasing the probe slot there would let all four
// upload workers walk the write path at once and push their full object bytes
// into the CAS before the first 403 landed — "one batch pays for a refusal"
// silently becoming "one worker wave pays for it". The slot must go to the next
// batch instead.
func TestProviderObjectCache_AProbeThatLearnedNothingHandsTheSlotOn(t *testing.T) {
	pinProbeWait(t, time.Minute) // the bound must not stand in for the hand-off

	f := newFakeObjectServer(t)
	exchangeDir := shortExchangeDir(t)
	logw := &lockedWriter{}
	s, result := startObjectSessionLogged(t, f, exchangeDir, logw, coreCapabilities()...)
	client := dialObjects(t, result.ObjectCacheSocket)

	// A job process staged bytes that do not content-address to the digest it
	// offered. The provider accepts the put (a cheap local check) and discards
	// the batch on the upload worker, before any store.
	poisoned := stagePoisonedObject(t, exchangeDir, objectID('9'), "bytes that do not match their address")
	if put, err := client.Put(&cache.ObjectPutParams{Namespace: objectNamespace, Objects: []cache.ObjectPut{poisoned}}); err != nil || put.Accepted != 1 {
		t.Fatalf("poisoned put = %+v err=%v", put, err)
	}
	waitUntil(t, "the poisoned batch to be discarded before any store", func() bool {
		return strings.Contains(logw.String(), "content-addresses to")
	})
	if _, stores := f.stats(); stores != 0 {
		t.Fatalf("stores=%d; a batch with nothing verified must not reach the server", stores)
	}

	// Now the real thing: the write gate refuses this run's credential, and the
	// store is held open until every batch is queued.
	f.refuseStores(http.StatusForbidden)
	release := make(chan struct{})
	f.blockStores(release)
	const batches = 6
	for i := 0; i < batches; i++ {
		obj := stageObject(t, exchangeDir, objectID(byte('a'+i)), fmt.Sprintf("compiled output %d", i))
		if put, err := client.Put(&cache.ObjectPutParams{Namespace: objectNamespace, Objects: []cache.ObjectPut{obj}}); err != nil || put.Accepted != 1 {
			t.Fatalf("put %d = %+v err=%v", i, put, err)
		}
	}
	close(release)
	s.objects.drain()

	if _, stores := f.stats(); stores != 1 {
		t.Errorf("the server received %d store(s) after a probe that learned nothing; the slot must be handed on, not spent", stores)
	}
	if got := f.uploadedBlobs(); got != 1 {
		t.Errorf("%d object blob(s) reached the CAS; a probe that never stored must not release its worker wave into the CAS", got)
	}
	if !s.objects.writeRefusedLatched() {
		t.Error("the refusal was never latched")
	}
}

// stagePoisonedObject stages bytes that do NOT content-address to the digest the
// put offers, at the offered size — the one thing a job process can do that
// makes a batch reach the upload worker and produce no store at all.
func stagePoisonedObject(t *testing.T, exchangeDir, id, content string) cache.ObjectPut {
	t.Helper()
	obj := stageObject(t, exchangeDir, id, content)
	path, ok := cache.BlobExchangePath(exchangeDir, obj.Digest)
	if !ok {
		t.Fatalf("invalid staged digest %q", obj.Digest)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), len(content)), 0o600); err != nil {
		t.Fatalf("poison staged bytes: %v", err)
	}
	return obj
}

// waitUntil polls cond until it holds, failing the test with what it was waiting
// for. It exists for the one thing a put cannot report synchronously: the
// background upload's outcome.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// mustPayload marshals op params into a request payload.
func mustPayload(t *testing.T, params any) json.RawMessage {
	t.Helper()
	raw, err := cache.MarshalPayload(params)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return raw
}
