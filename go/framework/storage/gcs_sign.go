package storage

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.putnami.dev/errors"
)

// GCS V4 signed-URL constants. The region is "auto" for global signing, matching
// the Cloud Storage V4 signer.
const (
	goog4Algorithm = "GOOG4-RSA-SHA256"
	goog4Region    = "auto"
)

// Ensure GCSBackend implements URLSigner at compile time.
var _ URLSigner = (*GCSBackend)(nil)

// SignedGetURL returns a GOOG4-RSA-SHA256 pre-signed URL for downloading an
// object. With a service-account key the URL is signed locally; under Workload
// Identity it is signed via the IAM SignBlob API.
func (b *GCSBackend) SignedGetURL(ctx context.Context, bucket, key string, opts SignedURLOptions) (string, error) {
	return b.signedURL(ctx, http.MethodGet, bucket, key, opts.Expiry, "")
}

// SignedPutURL returns a GOOG4-RSA-SHA256 pre-signed URL for uploading an object.
// When opts.ContentType is set it is bound into the signature so the upload is
// restricted to that content type.
func (b *GCSBackend) SignedPutURL(ctx context.Context, bucket, key string, opts SignedURLOptions) (string, error) {
	return b.signedURL(ctx, http.MethodPut, bucket, key, opts.Expiry, opts.ContentType)
}

// signedURL builds a V4 signed URL, choosing local RSA signing when a private
// key is available and falling back to keyless IAM SignBlob otherwise.
func (b *GCSBackend) signedURL(ctx context.Context, method, bucket, key string, expiry time.Duration, contentType string) (string, error) {
	if b.signKey != nil {
		return b.goog4SignedURL(ctx, method, bucket, key, b.signEmail, expiry, contentType, b.signLocally)
	}
	if b.tokens == nil {
		return "", errors.New(CodeStorageRequest, "signed URLs require credentials (a service-account key or Workload Identity)",
			errors.String("backend", "gcs"))
	}
	email, err := b.resolveSignerEmail(ctx)
	if err != nil {
		return "", err
	}
	if email == "" {
		return "", errors.New(CodeStorageRequest, "signed URLs require a service-account email", errors.String("backend", "gcs"))
	}
	sign := func(signCtx context.Context, payload []byte) ([]byte, error) {
		return b.signViaIAM(signCtx, email, payload)
	}
	return b.goog4SignedURL(ctx, method, bucket, key, email, expiry, contentType, sign)
}

// goog4SignedURL constructs the canonical request and string-to-sign for a V4
// query-signed URL, then signs it with the supplied signer. The signer receives
// the raw string-to-sign bytes and returns the RSA signature (the local and IAM
// signers both produce RSA-PKCS1v15 over its SHA-256).
func (b *GCSBackend) goog4SignedURL(
	ctx context.Context,
	method, bucket, key, email string,
	expiry time.Duration,
	contentType string,
	sign func(ctx context.Context, payload []byte) ([]byte, error),
) (string, error) {
	if email == "" {
		return "", errors.New(CodeStorageRequest, "signed URLs require a service-account email", errors.String("backend", "gcs"))
	}
	if expiry <= 0 {
		expiry = time.Hour
	}

	endpoint := b.base()
	host := "storage.googleapis.com"
	if parsed, err := url.Parse(endpoint); err == nil && parsed.Host != "" {
		host = parsed.Host
	}

	now := time.Now().UTC()
	date := now.Format("20060102")
	timestamp := now.Format("20060102T150405Z")
	scope := date + "/" + goog4Region + "/storage/goog4_request"

	signedHeaders := "host"
	canonicalHeaders := "host:" + host + "\n"
	if method == http.MethodPut && contentType != "" {
		signedHeaders = "content-type;host"
		canonicalHeaders = "content-type:" + contentType + "\nhost:" + host + "\n"
	}

	query := url.Values{}
	query.Set("X-Goog-Algorithm", goog4Algorithm)
	query.Set("X-Goog-Credential", email+"/"+scope)
	query.Set("X-Goog-Date", timestamp)
	query.Set("X-Goog-Expires", strconv.Itoa(int(expiry.Seconds())))
	query.Set("X-Goog-SignedHeaders", signedHeaders)
	canonicalQuery := canonicalQueryValues(query)

	path := "/" + uriEncode(bucket) + "/" + encodeS3Path(key)
	canonicalRequest := strings.Join([]string{
		method,
		path,
		canonicalQuery,
		canonicalHeaders,
		signedHeaders,
		"UNSIGNED-PAYLOAD",
	}, "\n")

	stringToSign := strings.Join([]string{
		goog4Algorithm,
		timestamp,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	signature, err := sign(ctx, []byte(stringToSign))
	if err != nil {
		return "", err
	}

	return endpoint + path + "?" + canonicalQuery + "&X-Goog-Signature=" + hex.EncodeToString(signature), nil
}

// signLocally signs the string-to-sign with the configured RSA private key.
func (b *GCSBackend) signLocally(_ context.Context, payload []byte) ([]byte, error) {
	digest := sha256.Sum256(payload)
	signature, err := rsa.SignPKCS1v15(rand.Reader, b.signKey, crypto.SHA256, digest[:])
	if err != nil {
		return nil, errors.Wrap(err, CodeStorageRequest, errors.String("backend", "gcs"), errors.String("op", "sign"))
	}
	return signature, nil
}

// signViaIAM signs the string-to-sign through the IAM Credentials signBlob API,
// which signs with the service account's managed key — no private key on the
// host. signBlob hashes the payload with SHA-256 internally, matching the local
// signer.
func (b *GCSBackend) signViaIAM(ctx context.Context, email string, payload []byte) ([]byte, error) {
	body, err := json.Marshal(map[string]string{"payload": base64.StdEncoding.EncodeToString(payload)})
	if err != nil {
		return nil, errors.Wrap(err, CodeStorageRequest, errors.String("backend", "gcs"), errors.String("op", "sign_blob"))
	}
	endpoint := fmt.Sprintf("%s/v1/projects/-/serviceAccounts/%s:signBlob", b.iamBase(), url.PathEscape(email))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, errors.Wrap(err, CodeStorageRequest, errors.String("backend", "gcs"), errors.String("op", "sign_blob"))
	}
	req.Header.Set("Content-Type", "application/json")
	if err := b.authorize(ctx, req); err != nil {
		return nil, err
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, CodeStorageRequest, errors.String("backend", "gcs"), errors.String("op", "sign_blob"))
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // signing call; body close error is not actionable
	if resp.StatusCode >= 400 {
		detail, _ := io.ReadAll(resp.Body) //nolint:errcheck // best-effort error detail
		return nil, errors.New(CodeStorageRequest, "sign blob request failed",
			errors.String("backend", "gcs"), errors.Int("status", resp.StatusCode), errors.String("body", string(detail)))
	}
	var signed struct {
		SignedBlob string `json:"signedBlob"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&signed); err != nil {
		return nil, errors.Wrap(err, CodeStorageRequest, errors.String("backend", "gcs"), errors.String("op", "sign_blob"))
	}
	signature, err := base64.StdEncoding.DecodeString(signed.SignedBlob)
	if err != nil {
		return nil, errors.Wrap(err, CodeStorageRequest, errors.String("backend", "gcs"), errors.String("op", "sign_blob"))
	}
	return signature, nil
}

// resolveSignerEmail returns the service-account email used in signed-URL
// credentials, fetching it from the metadata server when not already known.
func (b *GCSBackend) resolveSignerEmail(ctx context.Context) (_ string, retErr error) {
	b.signMu.Lock()
	defer b.signMu.Unlock()

	if b.signEmail != "" {
		return b.signEmail, nil
	}
	endpoint := strings.TrimRight(b.metadataBase(), "/") + gcsMetadataEmailPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", errors.Wrap(err, CodeStorageRequest, errors.String("backend", "gcs"), errors.String("op", "metadata_email"))
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return "", errors.Wrap(err, CodeStorageRequest, errors.String("backend", "gcs"), errors.String("op", "metadata_email"))
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && retErr == nil {
			retErr = errors.Wrap(cerr, CodeStorageRequest, errors.String("backend", "gcs"), errors.String("op", "close_response"))
		}
	}()
	if resp.StatusCode >= 400 {
		return "", httpError(resp, CodeStorageRequest, "", "")
	}
	email, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", errors.Wrap(err, CodeStorageRequest, errors.String("backend", "gcs"), errors.String("op", "metadata_email"))
	}
	b.signEmail = strings.TrimSpace(string(email))
	return b.signEmail, nil
}

// iamBase returns the IAM Credentials API base without a trailing slash.
func (b *GCSBackend) iamBase() string {
	if b.iamEndpoint == "" {
		return gcsDefaultIAM
	}
	return strings.TrimRight(b.iamEndpoint, "/")
}

// metadataBase returns the metadata server base without a trailing slash.
func (b *GCSBackend) metadataBase() string {
	if b.metadataHost == "" {
		return gcsDefaultMetadata
	}
	return strings.TrimRight(b.metadataHost, "/")
}
