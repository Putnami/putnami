package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"go.putnami.dev/errors"
	"go.putnami.dev/inject"
	"go.putnami.dev/protocol/features/spectest"
	storageproto "go.putnami.dev/protocol/storage"
)

// fakeBackend is a recording, in-memory Backend test double. It captures every
// call's (op, bucket, key) so tests can assert the exact provider coordinates a
// binding resolved to, and backs a tiny store so round-trips work.
type fakeBackend struct {
	mu     sync.Mutex
	store  map[string][]byte
	calls  []string
	closed bool
	listFn func(bucket string, opts *ListOptions) (*ListResult, error)
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{store: make(map[string][]byte)}
}

func fakeKey(bucket, key string) string { return bucket + "\x00" + key }

func (f *fakeBackend) record(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeBackend) Put(_ context.Context, bucket, key string, data io.Reader, _ *ObjectMetadata) (*PutResult, error) {
	f.record("put %s %s", bucket, key)
	buf, err := io.ReadAll(data)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.store[fakeKey(bucket, key)] = buf
	f.mu.Unlock()
	return &PutResult{Key: key, Size: int64(len(buf))}, nil
}

func (f *fakeBackend) Get(_ context.Context, bucket, key string) (*GetResult, error) {
	f.record("get %s %s", bucket, key)
	f.mu.Lock()
	buf, ok := f.store[fakeKey(bucket, key)]
	f.mu.Unlock()
	if !ok {
		return nil, nil
	}
	return &GetResult{Key: key, Body: io.NopCloser(bytes.NewReader(buf)), Size: int64(len(buf))}, nil
}

func (f *fakeBackend) Delete(_ context.Context, bucket, key string) error {
	f.record("delete %s %s", bucket, key)
	f.mu.Lock()
	delete(f.store, fakeKey(bucket, key))
	f.mu.Unlock()
	return nil
}

func (f *fakeBackend) Exists(_ context.Context, bucket, key string) (bool, error) {
	f.record("exists %s %s", bucket, key)
	f.mu.Lock()
	_, ok := f.store[fakeKey(bucket, key)]
	f.mu.Unlock()
	return ok, nil
}

func (f *fakeBackend) List(_ context.Context, bucket string, opts *ListOptions) (*ListResult, error) {
	prefix := ""
	if opts != nil {
		prefix = opts.Prefix
	}
	f.record("list %s %s", bucket, prefix)
	if f.listFn != nil {
		return f.listFn(bucket, opts)
	}
	return &ListResult{}, nil
}

func (f *fakeBackend) Copy(_ context.Context, bucket, source, destination string) error {
	f.record("copy %s %s %s", bucket, source, destination)
	f.mu.Lock()
	defer f.mu.Unlock()
	buf, ok := f.store[fakeKey(bucket, source)]
	if !ok {
		return errors.New(CodeStorageNotFound, "source not found")
	}
	f.store[fakeKey(bucket, destination)] = buf
	return nil
}

func (f *fakeBackend) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

func (f *fakeBackend) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// signingFakeBackend is a fakeBackend that also signs URLs, so signed-URL
// remapping can be exercised.
type signingFakeBackend struct {
	*fakeBackend
}

func (s *signingFakeBackend) SignedGetURL(_ context.Context, bucket, key string, _ SignedURLOptions) (string, error) {
	s.record("signget %s %s", bucket, key)
	return "signed:get:" + bucket + ":" + key, nil
}

func (s *signingFakeBackend) SignedPutURL(_ context.Context, bucket, key string, _ SignedURLOptions) (string, error) {
	s.record("signput %s %s", bucket, key)
	return "signed:put:" + bucket + ":" + key, nil
}

// boundTo builds a bindingBackend wiring each logical name to (backend, bucket,
// prefix) directly, bypassing env parsing so remap behavior can be tested in
// isolation.
func boundTo(targets map[string]boundTarget, backends ...Backend) *bindingBackend {
	return &bindingBackend{targets: targets, backends: backends}
}

func TestBindingBackendRemapsLogicalToProvider(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "isolation", "binding-remaps-logical-to-provider-bucket")
	fake := newFakeBackend()
	b := boundTo(map[string]boundTarget{
		"uploads": {backend: fake, bucket: "pn-acme-prod-uploads-a1b2c3"},
	}, fake)
	ctx := context.Background()

	res, err := b.Put(ctx, "uploads", "user-1/avatar.png", strings.NewReader("bytes"), nil)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if res.Key != "user-1/avatar.png" {
		t.Errorf("PutResult.Key = %q, want logical key", res.Key)
	}
	// The provider received the provider bucket name and the untouched key.
	if got := fake.calls[0]; got != "put pn-acme-prod-uploads-a1b2c3 user-1/avatar.png" {
		t.Errorf("provider Put call = %q", got)
	}

	get, err := b.Get(ctx, "uploads", "user-1/avatar.png")
	if err != nil || get == nil {
		t.Fatalf("Get: %v (nil=%v)", err, get == nil)
	}
	data, _ := io.ReadAll(get.Body)
	if string(data) != "bytes" {
		t.Errorf("round-trip body = %q, want %q", data, "bytes")
	}

	ok, err := b.Exists(ctx, "uploads", "user-1/avatar.png")
	if err != nil || !ok {
		t.Errorf("Exists = %v, %v; want true, nil", ok, err)
	}
	if err := b.Copy(ctx, "uploads", "user-1/avatar.png", "user-1/copy.png"); err != nil {
		t.Errorf("Copy: %v", err)
	}
	if err := b.Delete(ctx, "uploads", "user-1/avatar.png"); err != nil {
		t.Errorf("Delete: %v", err)
	}
}

func TestBindingBackendFailsClosedOnUnboundBucket(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "isolation", "unbound-logical-bucket-fails-closed")
	fake := newFakeBackend()
	b := boundTo(map[string]boundTarget{
		"uploads": {backend: fake, bucket: "prov-uploads"},
	}, fake)
	ctx := context.Background()

	// Every operation on an unbound logical bucket must error and never touch
	// the underlying backend (no silent literal-named target).
	if _, err := b.Put(ctx, "unknown", "k", strings.NewReader("x"), nil); !errors.Is(err, CodeStorageUnbound) {
		t.Errorf("Put err = %v (code %q), want %q", err, errors.GetCode(err), CodeStorageUnbound)
	}
	if _, err := b.Get(ctx, "unknown", "k"); !errors.Is(err, CodeStorageUnbound) {
		t.Errorf("Get err code = %q, want %q", errors.GetCode(err), CodeStorageUnbound)
	}
	if _, err := b.Exists(ctx, "unknown", "k"); !errors.Is(err, CodeStorageUnbound) {
		t.Errorf("Exists err code = %q, want %q", errors.GetCode(err), CodeStorageUnbound)
	}
	if err := b.Delete(ctx, "unknown", "k"); !errors.Is(err, CodeStorageUnbound) {
		t.Errorf("Delete err code = %q, want %q", errors.GetCode(err), CodeStorageUnbound)
	}
	if _, err := b.List(ctx, "unknown", nil); !errors.Is(err, CodeStorageUnbound) {
		t.Errorf("List err code = %q, want %q", errors.GetCode(err), CodeStorageUnbound)
	}
	if err := b.Copy(ctx, "unknown", "a", "b"); !errors.Is(err, CodeStorageUnbound) {
		t.Errorf("Copy err code = %q, want %q", errors.GetCode(err), CodeStorageUnbound)
	}
	if fake.callCount() != 0 {
		t.Errorf("underlying backend was called %d times for unbound bucket; want 0", fake.callCount())
	}
}

func TestBindingBackendAppliesKeyPrefix(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "isolation", "binding-applies-key-prefix")
	fake := newFakeBackend()
	// A shared provider bucket with a per-resource isolation prefix.
	b := boundTo(map[string]boundTarget{
		"exports": {backend: fake, bucket: "acme-shared-prod", prefix: "preview-42/exports/"},
	}, fake)
	ctx := context.Background()

	res, err := b.Put(ctx, "exports", "report.csv", strings.NewReader("data"), nil)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Provider sees prefixed key; caller sees the logical key back.
	if got := fake.calls[0]; got != "put acme-shared-prod preview-42/exports/report.csv" {
		t.Errorf("provider Put call = %q", got)
	}
	if res.Key != "report.csv" {
		t.Errorf("PutResult.Key = %q, want logical key %q", res.Key, "report.csv")
	}

	get, err := b.Get(ctx, "exports", "report.csv")
	if err != nil || get == nil {
		t.Fatalf("Get: %v", err)
	}
	if get.Key != "report.csv" {
		t.Errorf("GetResult.Key = %q, want %q", get.Key, "report.csv")
	}

	if err := b.Copy(ctx, "exports", "report.csv", "report-copy.csv"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if got := fake.calls[len(fake.calls)-1]; got != "copy acme-shared-prod preview-42/exports/report.csv preview-42/exports/report-copy.csv" {
		t.Errorf("provider Copy call = %q", got)
	}
}

func TestBindingBackendListTranslatesPrefix(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "isolation", "binding-list-translates-prefix")
	fake := newFakeBackend()
	fake.listFn = func(_ string, opts *ListOptions) (*ListResult, error) {
		// Simulate a backend echoing full (prefixed) keys back.
		return &ListResult{
			Objects:           []ObjectInfo{{Key: opts.Prefix + "a.txt"}, {Key: opts.Prefix + "b.txt"}},
			Prefixes:          []string{opts.Prefix + "sub/"},
			IsTruncated:       true,
			ContinuationToken: "opaque-token",
		}, nil
	}
	b := boundTo(map[string]boundTarget{
		"exports": {backend: fake, bucket: "acme-shared-prod", prefix: "preview-42/exports/"},
	}, fake)

	res, err := b.List(context.Background(), "exports", &ListOptions{Prefix: "2026/"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// The provider was scoped to bindingPrefix + callerPrefix.
	if got := fake.calls[0]; got != "list acme-shared-prod preview-42/exports/2026/" {
		t.Errorf("provider List call = %q", got)
	}
	// Returned keys and common prefixes are stripped back to logical form.
	if got := []string{res.Objects[0].Key, res.Objects[1].Key}; got[0] != "2026/a.txt" || got[1] != "2026/b.txt" {
		t.Errorf("object keys = %v, want logical keys", got)
	}
	if res.Prefixes[0] != "2026/sub/" {
		t.Errorf("prefix = %q, want %q", res.Prefixes[0], "2026/sub/")
	}
	// The continuation token is opaque and round-trips untouched.
	if res.ContinuationToken != "opaque-token" {
		t.Errorf("ContinuationToken = %q, want untouched %q", res.ContinuationToken, "opaque-token")
	}
}

func TestBindingBackendSignedURLs(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "signed-access", "binding-grants-signed-urls-explicitly")
	signer := &signingFakeBackend{fakeBackend: newFakeBackend()}
	notGranted := &signingFakeBackend{fakeBackend: newFakeBackend()}
	plain := newFakeBackend()
	b := boundTo(map[string]boundTarget{
		"signed":      {backend: signer, bucket: "prov-signed", prefix: "p/", signedURLs: true},
		"not_granted": {backend: notGranted, bucket: "prov-denied"},
		"plain":       {backend: plain, bucket: "prov-plain", signedURLs: true},
	}, signer, notGranted, plain)
	ctx := context.Background()

	url, err := b.SignedGetURL(ctx, "signed", "doc.pdf", SignedURLOptions{})
	if err != nil {
		t.Fatalf("SignedGetURL: %v", err)
	}
	if url != "signed:get:prov-signed:p/doc.pdf" {
		t.Errorf("signed GET url = %q, want provider bucket + prefixed key", url)
	}

	// A backend that can technically sign still cannot mint URLs unless the
	// deployer granted the capability on this binding.
	if _, err := b.SignedGetURL(ctx, "not_granted", "doc.pdf", SignedURLOptions{}); !errors.Is(err, CodeStorageUnsupported) {
		t.Errorf("SignedGetURL err code = %q, want %q", errors.GetCode(err), CodeStorageUnsupported)
	}
	if notGranted.callCount() != 0 {
		t.Errorf("signing backend was called %d times without a signedUrls grant; want 0", notGranted.callCount())
	}
	// A bound bucket whose backend cannot sign fails with a clear, typed error.
	if _, err := b.SignedPutURL(ctx, "plain", "doc.pdf", SignedURLOptions{}); !errors.Is(err, CodeStorageUnsupported) {
		t.Errorf("SignedPutURL err code = %q, want %q", errors.GetCode(err), CodeStorageUnsupported)
	}
	// An unbound bucket still fails closed.
	if _, err := b.SignedGetURL(ctx, "nope", "doc.pdf", SignedURLOptions{}); !errors.Is(err, CodeStorageUnbound) {
		t.Errorf("SignedGetURL unbound err code = %q, want %q", errors.GetCode(err), CodeStorageUnbound)
	}
}

func TestBindingBackendCloseClosesAllBackends(t *testing.T) {
	a, c := newFakeBackend(), newFakeBackend()
	b := boundTo(map[string]boundTarget{
		"one": {backend: a, bucket: "prov-1"},
		"two": {backend: c, bucket: "prov-2"},
	}, a, c)
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !a.closed || !c.closed {
		t.Errorf("Close did not close all backends: a=%v c=%v", a.closed, c.closed)
	}
}

func TestParseBindings(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		wantErr errors.Code // "" means success
	}{
		{
			name: "valid multi-binding document",
			json: `{"protocolVersion":1,"bindings":[
				{"name":"uploads","backend":"gcs","bucket":"pn-prod-uploads-a1","identity":"workload","signedUrls":true},
				{"name":"exports","backend":"gcs","bucket":"shared","prefix":"preview/exports/"}
			],"providers":{"gcs":{"projectId":"acme"}}}`,
		},
		{
			name: "per-binding protocolVersion defaulted when omitted",
			json: `{"protocolVersion":1,"bindings":[{"name":"uploads","backend":"memory","bucket":"prov"}]}`,
		},
		{
			name:    "unknown envelope field rejected",
			json:    `{"protocolVersion":1,"bindings":[{"name":"u","backend":"memory","bucket":"p"}],"extra":true}`,
			wantErr: CodeStorageRequest,
		},
		{
			name:    "unknown binding field rejected",
			json:    `{"protocolVersion":1,"bindings":[{"name":"u","backend":"memory","bucket":"p","region":"x"}]}`,
			wantErr: CodeStorageRequest,
		},
		{
			name:    "unknown provider field rejected",
			json:    `{"protocolVersion":1,"bindings":[{"name":"u","backend":"s3","bucket":"p"}],"providers":{"s3":{"bucket":"ignored"}}}`,
			wantErr: CodeStorageRequest,
		},
		{
			name:    "trailing json value rejected",
			json:    `{"protocolVersion":1,"bindings":[{"name":"u","backend":"memory","bucket":"p"}]} {}`,
			wantErr: CodeStorageRequest,
		},
		{
			name:    "wrong envelope protocolVersion",
			json:    `{"protocolVersion":2,"bindings":[{"name":"u","backend":"memory","bucket":"p"}]}`,
			wantErr: CodeStorageRequest,
		},
		{
			name:    "no bindings",
			json:    `{"protocolVersion":1,"bindings":[]}`,
			wantErr: CodeStorageRequest,
		},
		{
			name:    "duplicate logical bucket",
			json:    `{"protocolVersion":1,"bindings":[{"name":"u","backend":"memory","bucket":"a"},{"name":"u","backend":"memory","bucket":"b"}]}`,
			wantErr: CodeStorageRequest,
		},
		{
			name:    "unsupported backend kind",
			json:    `{"protocolVersion":1,"bindings":[{"name":"u","backend":"azure","bucket":"p"}]}`,
			wantErr: CodeStorageRequest,
		},
		{
			name:    "missing bucket fails protocol validation",
			json:    `{"protocolVersion":1,"bindings":[{"name":"u","backend":"memory"}]}`,
			wantErr: CodeStorageRequest,
		},
		{
			name:    "invalid logical name fails protocol validation",
			json:    `{"protocolVersion":1,"bindings":[{"name":"Bad Name","backend":"memory","bucket":"p"}]}`,
			wantErr: CodeStorageRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, err := ParseBindings([]byte(tt.json))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ParseBindings: unexpected error: %v", err)
				}
				if doc == nil {
					t.Fatal("ParseBindings: nil doc on success")
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ParseBindings err = %v (code %q), want code %q", err, errors.GetCode(err), tt.wantErr)
			}
		})
	}
}

func TestBackendFromBindingsValidatesDocument(t *testing.T) {
	t.Run("duplicate logical names rejected", func(t *testing.T) {
		doc := &BindingsDocument{
			ProtocolVersion: BindingsProtocolVersion,
			Bindings: []storageproto.Binding{
				{Name: "uploads", Backend: "memory", Bucket: "prov-a"},
				{Name: "uploads", Backend: "memory", Bucket: "prov-b"},
			},
		}
		if _, err := BackendFromBindings(context.Background(), doc); !errors.Is(err, CodeStorageRequest) {
			t.Fatalf("BackendFromBindings err code = %q, want %q", errors.GetCode(err), CodeStorageRequest)
		}
	})

	t.Run("per-binding protocolVersion defaulted", func(t *testing.T) {
		doc := &BindingsDocument{
			ProtocolVersion: BindingsProtocolVersion,
			Bindings: []storageproto.Binding{
				{Name: "uploads", Backend: "memory", Bucket: "prov"},
			},
		}
		backend, err := BackendFromBindings(context.Background(), doc)
		if err != nil {
			t.Fatalf("BackendFromBindings: %v", err)
		}
		defer backend.Close() //nolint:errcheck // test cleanup
		if doc.Bindings[0].ProtocolVersion != storageproto.ProtocolVersion {
			t.Errorf("binding protocolVersion = %d, want %d", doc.Bindings[0].ProtocolVersion, storageproto.ProtocolVersion)
		}
	})
}

func TestBackendFromBindingsRoundTripMemory(t *testing.T) {
	doc, err := ParseBindings([]byte(`{"protocolVersion":1,"bindings":[
		{"name":"uploads","backend":"memory","bucket":"prov-uploads"}
	]}`))
	if err != nil {
		t.Fatalf("ParseBindings: %v", err)
	}
	backend, err := BackendFromBindings(context.Background(), doc)
	if err != nil {
		t.Fatalf("BackendFromBindings: %v", err)
	}
	defer backend.Close() //nolint:errcheck // test cleanup

	ctx := context.Background()
	if _, err := backend.Put(ctx, "uploads", "k.txt", strings.NewReader("hi"), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := backend.Get(ctx, "uploads", "k.txt")
	if err != nil || got == nil {
		t.Fatalf("Get: %v (nil=%v)", err, got == nil)
	}
	data, _ := io.ReadAll(got.Body)
	if string(data) != "hi" {
		t.Errorf("round-trip body = %q, want %q", data, "hi")
	}
	// Unbound bucket fails closed even through the constructed backend.
	if _, err := backend.Get(ctx, "other", "k.txt"); !errors.Is(err, CodeStorageUnbound) {
		t.Errorf("unbound Get err code = %q, want %q", errors.GetCode(err), CodeStorageUnbound)
	}
}

func TestDiscoverBackend(t *testing.T) {
	t.Run("unset returns nil backend", func(t *testing.T) {
		t.Setenv(EnvBindings, "")
		backend, err := DiscoverBackend(context.Background())
		if err != nil {
			t.Fatalf("DiscoverBackend: %v", err)
		}
		if backend != nil {
			t.Errorf("backend = %v, want nil when %s unset", backend, EnvBindings)
		}
	})

	t.Run("set builds a working backend", func(t *testing.T) {
		t.Setenv(EnvBindings, `{"protocolVersion":1,"bindings":[{"name":"uploads","backend":"memory","bucket":"prov"}]}`)
		backend, err := DiscoverBackend(context.Background())
		if err != nil {
			t.Fatalf("DiscoverBackend: %v", err)
		}
		if backend == nil {
			t.Fatal("backend is nil")
		}
		defer backend.Close() //nolint:errcheck // test cleanup
		if _, err := backend.Put(context.Background(), "uploads", "k", strings.NewReader("x"), nil); err != nil {
			t.Errorf("Put through discovered backend: %v", err)
		}
	})

	t.Run("malformed value errors", func(t *testing.T) {
		t.Setenv(EnvBindings, `{not json`)
		if _, err := DiscoverBackend(context.Background()); err == nil {
			t.Error("DiscoverBackend: want error for malformed value")
		}
	})
}

func TestPluginProvidesBoundBackend(t *testing.T) {
	t.Run("resolves backend when bindings are injected", func(t *testing.T) {
		t.Setenv(EnvBindings, `{"protocolVersion":1,"bindings":[{"name":"uploads","backend":"memory","bucket":"prov"}]}`)
		p := NewPlugin()
		c := inject.NewContainer("test", nil)
		for _, reg := range p.Provides() {
			if err := c.Register(reg); err != nil {
				t.Fatalf("Register: %v", err)
			}
		}
		v, err := c.Get(inject.TokenOf[Backend]())
		if err != nil {
			t.Fatalf("resolve storage.Backend: %v", err)
		}
		backend, ok := v.(Backend)
		if !ok {
			t.Fatalf("resolved value is %T, want storage.Backend", v)
		}
		if _, err := backend.Put(context.Background(), "uploads", "k", strings.NewReader("x"), nil); err != nil {
			t.Errorf("Put through provided backend: %v", err)
		}
		if p.backend == nil {
			t.Fatal("plugin did not retain resolved backend for lifecycle cleanup")
		}
		if err := c.Close(); err != nil {
			t.Fatalf("Close container: %v", err)
		}
		if p.backend != nil {
			t.Error("plugin backend was not cleared after container close")
		}
	})

	t.Run("fails closed when no bindings are injected", func(t *testing.T) {
		t.Setenv(EnvBindings, "")
		c := inject.NewContainer("test", nil)
		for _, reg := range NewPlugin().Provides() {
			if err := c.Register(reg); err != nil {
				t.Fatalf("Register: %v", err)
			}
		}
		if _, err := c.Get(inject.TokenOf[Backend]()); !errors.Is(err, CodeStorageUnbound) {
			t.Errorf("resolve err code = %q, want %q", errors.GetCode(err), CodeStorageUnbound)
		}
	})
}
