package telemetry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.putnami.dev/errors"
)

// Native GCP service-to-service auth for OTLPConfig.BearerTokenSource.
//
// A workload pushing OTLP to a private collector (Cloud Run behind IAM)
// authenticates with an audience-bound ID token from the GCP metadata server —
// Workload Identity, no static secret. This is the telemetry twin of the
// storage package's native GCS metadata auth: the platform-specific credential
// path lives in the framework so a workload needs no cloud SDK import.

const (
	// gcpIDTokenPath mints ID tokens for the attached service account. A
	// well-known, non-secret URL.
	gcpIDTokenPath = "/computeMetadata/v1/instance/service-accounts/default/identity"

	gcpDefaultMetadataHost = "http://metadata.google.internal"
	// gcpIDTokenRefreshLeeway refreshes a cached token while it still has this
	// much lifetime left, so an in-flight request never races the expiry.
	gcpIDTokenRefreshLeeway = 5 * time.Minute
	// gcpIDTokenFallbackTTL caches a token whose expiry cannot be parsed for
	// this long — short enough to never hand out a stale token (GCP ID tokens
	// live ~1h), long enough to amortize the metadata round trip.
	gcpIDTokenFallbackTTL  = 50 * time.Minute
	gcpIDTokenFetchTimeout = 2 * time.Second
	gcpIDTokenMaxBody      = 32 * 1024
)

// NewGCPIDTokenSource returns a BearerTokenSource that fetches GCP ID tokens
// from the metadata server, bound to the given audience (for a Cloud Run
// collector, its URL). Tokens are cached and refreshed shortly before expiry;
// the source is safe for concurrent use. Off GCP the metadata server is
// unreachable and every call errors, which the OTLP exporters treat as a
// collector failure: the batch is dropped and the workload is unaffected.
func NewGCPIDTokenSource(audience string) func(ctx context.Context) (string, error) {
	return newGCPIDTokenSource(&http.Client{Timeout: gcpIDTokenFetchTimeout}, gcpDefaultMetadataHost, audience)
}

// newGCPIDTokenSource is the test seam: the metadata host is injectable so an
// httptest.Server can stand in for the real endpoint.
func newGCPIDTokenSource(client *http.Client, metadataHost, audience string) func(ctx context.Context) (string, error) {
	var (
		mu     sync.Mutex
		cached string
		expiry time.Time
	)
	endpoint := strings.TrimRight(metadataHost, "/") + gcpIDTokenPath

	return func(ctx context.Context) (string, error) {
		if strings.TrimSpace(audience) == "" {
			return "", errors.Newf(CodeTelemetryExport, "gcp id token: audience is empty")
		}
		mu.Lock()
		if cached != "" && time.Until(expiry) > gcpIDTokenRefreshLeeway {
			token := cached
			mu.Unlock()
			return token, nil
		}
		mu.Unlock()

		q := url.Values{}
		q.Set("audience", audience)
		q.Set("format", "full")
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
		if err != nil {
			return "", errors.Wrapf(err, CodeTelemetryExport, "gcp id token: build request")
		}
		req.Header.Set("Metadata-Flavor", "Google")
		resp, err := client.Do(req)
		if err != nil {
			return "", errors.Wrapf(err, CodeTelemetryExport, "gcp id token: metadata fetch")
		}
		defer resp.Body.Close() //nolint:errcheck // best-effort close
		if resp.StatusCode != http.StatusOK {
			return "", errors.Newf(CodeTelemetryExport, "gcp id token: metadata server returned status %d", resp.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, gcpIDTokenMaxBody))
		if err != nil {
			return "", errors.Wrapf(err, CodeTelemetryExport, "gcp id token: read response")
		}
		token := strings.TrimSpace(string(body))
		if token == "" {
			return "", errors.Newf(CodeTelemetryExport, "gcp id token: empty body")
		}

		exp := parseJWTExpiry(token)
		if exp.IsZero() {
			exp = time.Now().Add(gcpIDTokenFallbackTTL)
		}
		mu.Lock()
		cached, expiry = token, exp
		mu.Unlock()
		return token, nil
	}
}

// parseJWTExpiry extracts the `exp` claim from an unverified JWT payload.
// Verification is the collector's job; the expiry only drives cache busting.
// Returns the zero time on any failure — the caller falls back to a
// conservative TTL.
func parseJWTExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return time.Time{}
		}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}
