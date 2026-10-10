package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	cloudkms "google.golang.org/api/cloudkms/v1"
	storagev1 "google.golang.org/api/storage/v1"
)

// newSnapshotStorageService and newSnapshotKMSService construct the GCP clients
// the snapshot fallback uses. They are swappable vars — like metadataReachableOnce
// — so tests point them at httptest servers without reaching real GCP.
var (
	newSnapshotStorageService = func(ctx context.Context) (*storagev1.Service, error) {
		return storagev1.NewService(ctx)
	}
	newSnapshotKMSService = func(ctx context.Context) (*cloudkms.Service, error) {
		return cloudkms.NewService(ctx)
	}
)

// snapshotObjectMaxBytes bounds the encrypted snapshot the loader will read
// into memory. A resolved config tree is kilobytes; this ceiling is a defense
// against a corrupt or hostile object, not a functional limit. It also stays
// well under Cloud KMS's 64 KiB Decrypt ciphertext limit so an oversized object
// fails with a clear read error rather than an opaque KMS 400.
const snapshotObjectMaxBytes = 256 * 1024

// loadConfigSnapshot fetches the durable last-known-good config snapshot the
// release path stamped for this workload and returns the decoded config tree.
//
// The snapshot object holds the base64 Cloud KMS ciphertext of the
// JSON-serialized resolved config tree (config + revealed secrets, exactly what
// the workload would have pulled from CONFIG_SERVER_URL with
// secretsMode=reveal) — stored verbatim as the string KMS Encrypt returns. That
// keeps one text-safe encoding across all three readers (Go writer, Go reader,
// TS reader), so the object survives the TS runtime's text-decoding sync HTTP
// path and needs no binary handling: object text → KMS Decrypt → base64
// plaintext → JSON → tree.
//
// The workload's own service account authenticates both the GCS read and the
// KMS Decrypt via Application Default Credentials; the release path grants it
// object-reader on its own snapshot object and cryptoKeyDecrypter on the key.
func loadConfigSnapshot(ctx context.Context, snapshotURI, kmsKey string, timeout time.Duration) (map[string]any, error) {
	bucket, object, err := parseGSURI(snapshotURI)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(kmsKey) == "" {
		return nil, fmt.Errorf("config snapshot KMS key is empty")
	}

	ciphertextB64, err := downloadSnapshotObject(ctx, bucket, object, timeout)
	if err != nil {
		return nil, err
	}

	plaintext, err := decryptSnapshot(ctx, kmsKey, ciphertextB64, timeout)
	if err != nil {
		return nil, err
	}

	var tree map[string]any
	if err := json.Unmarshal(plaintext, &tree); err != nil {
		return nil, fmt.Errorf("decode config snapshot payload: %w", err)
	}
	return tree, nil
}

// downloadSnapshotObject reads the encrypted snapshot object from GCS using the
// workload's ADC identity. The storage client is constructed on demand: the
// fallback path is rare (a config-server outage), so the loader pays no GCP
// client cost on the common remote-success boot.
func downloadSnapshotObject(ctx context.Context, bucket, object string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	svc, err := newSnapshotStorageService(ctx)
	if err != nil {
		return nil, fmt.Errorf("create storage client for config snapshot: %w", err)
	}
	resp, err := svc.Objects.Get(bucket, object).Context(ctx).Download()
	if err != nil {
		return nil, fmt.Errorf("download config snapshot gs://%s/%s: %w", bucket, object, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, snapshotObjectMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read config snapshot gs://%s/%s: %w", bucket, object, err)
	}
	if len(data) > snapshotObjectMaxBytes {
		return nil, fmt.Errorf("config snapshot gs://%s/%s exceeds %d bytes", bucket, object, snapshotObjectMaxBytes)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("config snapshot gs://%s/%s is empty", bucket, object)
	}
	return data, nil
}

// decryptSnapshot decrypts the base64 snapshot ciphertext with Cloud KMS. The
// object already holds the base64 string KMS Encrypt produced, which is exactly
// what the REST Decrypt Ciphertext field expects, so it is passed through
// verbatim; only the base64 plaintext KMS returns is decoded.
func decryptSnapshot(ctx context.Context, kmsKey string, ciphertextB64 []byte, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	svc, err := newSnapshotKMSService(ctx)
	if err != nil {
		return nil, fmt.Errorf("create KMS client for config snapshot: %w", err)
	}
	resp, err := svc.Projects.Locations.KeyRings.CryptoKeys.
		Decrypt(kmsKey, &cloudkms.DecryptRequest{
			Ciphertext: strings.TrimSpace(string(ciphertextB64)),
		}).
		Context(ctx).
		Do()
	if err != nil {
		return nil, fmt.Errorf("KMS decrypt config snapshot: %w", err)
	}
	plaintext, err := base64.StdEncoding.DecodeString(resp.Plaintext)
	if err != nil {
		return nil, fmt.Errorf("decode KMS plaintext for config snapshot: %w", err)
	}
	return plaintext, nil
}

// parseGSURI splits a gs://bucket/object URI into its bucket and object parts.
// It rejects any other scheme, a missing bucket, or a missing object so a
// misconfigured CONFIG_SNAPSHOT_URI fails with a clear error rather than an
// opaque GCS 404.
func parseGSURI(raw string) (bucket, object string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", fmt.Errorf("config snapshot URI is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("parse config snapshot URI %q: %w", raw, err)
	}
	if u.Scheme != "gs" {
		return "", "", fmt.Errorf("config snapshot URI %q must use the gs:// scheme", raw)
	}
	bucket = u.Host
	object = strings.TrimPrefix(u.Path, "/")
	if bucket == "" || object == "" {
		return "", "", fmt.Errorf("config snapshot URI %q must be gs://<bucket>/<object>", raw)
	}
	return bucket, object, nil
}
