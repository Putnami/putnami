package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	cloudkms "google.golang.org/api/cloudkms/v1"
	"google.golang.org/api/option"
	storagev1 "google.golang.org/api/storage/v1"
)

func TestParseGSURI(t *testing.T) {
	cases := []struct {
		name           string
		uri            string
		bucket, object string
		wantErr        bool
	}{
		{name: "ok nested", uri: "gs://my-bucket/config-snapshots/ws1/orders/rev.json.enc", bucket: "my-bucket", object: "config-snapshots/ws1/orders/rev.json.enc"},
		{name: "ok flat", uri: "gs://b/o", bucket: "b", object: "o"},
		{name: "empty", uri: "", wantErr: true},
		{name: "wrong scheme", uri: "https://b/o", wantErr: true},
		{name: "no object", uri: "gs://b", wantErr: true},
		{name: "no object trailing slash", uri: "gs://b/", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bucket, object, err := parseGSURI(tc.uri)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseGSURI(%q) = nil error, want error", tc.uri)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGSURI(%q) unexpected error: %v", tc.uri, err)
			}
			if bucket != tc.bucket || object != tc.object {
				t.Fatalf("parseGSURI(%q) = (%q, %q), want (%q, %q)", tc.uri, bucket, object, tc.bucket, tc.object)
			}
		})
	}
}

// snapshotStub stands up fake GCS + KMS endpoints and points the swappable
// client constructors at them. The stub models the snapshot encoding contract:
// the GCS object holds the base64 ciphertext string, and KMS Decrypt echoes it
// back as the base64 plaintext. To keep the test self-contained the fake KMS
// "decryption" is the identity function — the object IS base64(plaintext) — so
// the test exercises the base64/JSON/REST wiring end to end without real crypto.
type snapshotStub struct {
	storage *httptest.Server
	kms     *httptest.Server
	restore func()
}

func newSnapshotStub(t *testing.T, object []byte) *snapshotStub {
	t.Helper()
	storageSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Objects.Get(...).Download() issues GET .../b/{bucket}/o/{object}?alt=media.
		if r.Method != http.MethodGet {
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(object)
	}))
	kmsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ":decrypt") {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req cloudkms.DecryptRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Identity "decrypt": echo the ciphertext back as plaintext (both base64).
		resp, _ := json.Marshal(cloudkms.DecryptResponse{Plaintext: req.Ciphertext})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(resp)
	}))

	prevStorage, prevKMS := newSnapshotStorageService, newSnapshotKMSService
	newSnapshotStorageService = func(ctx context.Context) (*storagev1.Service, error) {
		return storagev1.NewService(ctx, option.WithoutAuthentication(),
			option.WithEndpoint(storageSrv.URL), option.WithHTTPClient(storageSrv.Client()))
	}
	newSnapshotKMSService = func(ctx context.Context) (*cloudkms.Service, error) {
		return cloudkms.NewService(ctx, option.WithoutAuthentication(),
			option.WithEndpoint(kmsSrv.URL), option.WithHTTPClient(kmsSrv.Client()))
	}
	return &snapshotStub{
		storage: storageSrv,
		kms:     kmsSrv,
		restore: func() {
			newSnapshotStorageService, newSnapshotKMSService = prevStorage, prevKMS
			storageSrv.Close()
			kmsSrv.Close()
		},
	}
}

func TestLoadConfigSnapshot(t *testing.T) {
	want := map[string]any{"service": map[string]any{"port": float64(8080)}, "flag": true}
	plaintext, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	object := []byte(base64.StdEncoding.EncodeToString(plaintext))
	stub := newSnapshotStub(t, object)
	defer stub.restore()

	got, err := loadConfigSnapshot(context.Background(),
		"gs://snapshots/config-snapshots/ws1/orders/rev.json.enc",
		"projects/p/locations/l/keyRings/putnami/cryptoKeys/ws1", 5*time.Second)
	if err != nil {
		t.Fatalf("loadConfigSnapshot: %v", err)
	}
	svc, _ := got["service"].(map[string]any)
	if svc == nil || svc["port"] != float64(8080) || got["flag"] != true {
		t.Fatalf("loadConfigSnapshot returned unexpected tree: %#v", got)
	}
}

func TestLoadConfigSnapshotEmptyKey(t *testing.T) {
	if _, err := loadConfigSnapshot(context.Background(), "gs://b/o", "", time.Second); err == nil {
		t.Fatal("expected error for empty KMS key")
	}
}

// TestFetchFallsBackToSnapshot verifies the fetch() wiring: a required source
// whose config server is unreachable resolves the durable snapshot instead of
// failing startup.
func TestFetchFallsBackToSnapshot(t *testing.T) {
	want := map[string]any{"db": map[string]any{"host": "snap"}}
	plaintext, _ := json.Marshal(want)
	object := []byte(base64.StdEncoding.EncodeToString(plaintext))
	stub := newSnapshotStub(t, object)
	defer stub.restore()

	src := NewRemoteConfigSource(RemoteSourceConfig{
		// A server URL that fails immediately (connection refused).
		ServerURL:      "http://127.0.0.1:1",
		AppName:        "orders",
		Environment:    "prod",
		Required:       true,
		RetryBudget:    -1, // disable retries so the test fails fast to the fallback
		Timeout:        2 * time.Second,
		SnapshotURI:    "gs://snapshots/config-snapshots/ws1/orders/rev.json.enc",
		SnapshotKMSKey: "projects/p/locations/l/keyRings/putnami/cryptoKeys/ws1",
	})
	got, err := src.Load()
	if err != nil {
		t.Fatalf("Load with snapshot fallback: %v", err)
	}
	db, _ := got["db"].(map[string]any)
	if db == nil || db["host"] != "snap" {
		t.Fatalf("expected snapshot config, got: %#v", got)
	}
}

// TestFetchNoSnapshotStillFails verifies the unchanged terminal semantics: a
// required source with no snapshot configured fails exactly as before.
func TestFetchNoSnapshotStillFails(t *testing.T) {
	src := NewRemoteConfigSource(RemoteSourceConfig{
		ServerURL:   "http://127.0.0.1:1",
		AppName:     "orders",
		Environment: "prod",
		Required:    true,
		RetryBudget: -1,
		Timeout:     2 * time.Second,
	})
	if _, err := src.Load(); err == nil {
		t.Fatal("expected required source with no snapshot to fail")
	}
}

func TestSnapshotFallbackDisabledWhenURIEmpty(t *testing.T) {
	src := NewRemoteConfigSource(RemoteSourceConfig{SnapshotURI: ""})
	if _, ok := src.loadSnapshotFallback(context.Background(), terminalRemoteFailure("x", nil)); ok {
		t.Fatal("loadSnapshotFallback should return ok=false when SnapshotURI is empty")
	}
}

// A terminal failure — revoked config access (401/403) or deleted/unresolved
// config (404, resolved:false) — must NOT fall back to the stale, secret-bearing
// snapshot even when one is configured; it stays terminal exactly as before the
// feature. The snapshot is only for outage-class (retryable) failures.
func TestSnapshotFallbackSkippedOnTerminalFailure(t *testing.T) {
	src := NewRemoteConfigSource(RemoteSourceConfig{
		SnapshotURI:    "gs://snapshots/config-snapshots/ws1/orders/rev.json.enc",
		SnapshotKMSKey: "projects/p/locations/l/keyRings/putnami/cryptoKeys/ws1",
	})
	// A configured snapshot + a terminal failure must not be loaded (no GCP client
	// is even reached — the fallback is refused before any snapshot read).
	if _, ok := src.loadSnapshotFallback(context.Background(), terminalRemoteFailure("config-server did not resolve config", nil)); ok {
		t.Fatal("loadSnapshotFallback must refuse a terminal failure")
	}
}

// TestFetchTerminalFailureDoesNotFallBack pins the end-to-end wiring: a required
// source whose config server returns a terminal 403 (access revoked) fails the
// boot instead of booting on a stale snapshot.
func TestFetchTerminalFailureDoesNotFallBack(t *testing.T) {
	object := []byte(base64.StdEncoding.EncodeToString([]byte(`{"db":{"host":"snap"}}`)))
	stub := newSnapshotStub(t, object)
	defer stub.restore()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	src := NewRemoteConfigSource(RemoteSourceConfig{
		ServerURL:      srv.URL,
		AppName:        "orders",
		Environment:    "prod",
		Required:       true,
		RetryBudget:    -1,
		Timeout:        2 * time.Second,
		SnapshotURI:    "gs://snapshots/config-snapshots/ws1/orders/rev.json.enc",
		SnapshotKMSKey: "projects/p/locations/l/keyRings/putnami/cryptoKeys/ws1",
	})
	if _, err := src.Load(); err == nil {
		t.Fatal("a terminal 403 must fail the boot, not fall back to a stale snapshot")
	}
}
