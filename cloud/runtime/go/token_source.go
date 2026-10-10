package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// TokenSource resolves the bearer token sent on each request to the
// config / secrets resolve endpoints. Implementations may cache, refresh,
// or read env on every call — callers must not assume a stable string.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// EnvVarTokenSource reads a bearer token from the named environment
// variable on every Token() call. Preserves the CONFIG_SERVER_TOKEN
// behavior for non-cloud runtimes.
type EnvVarTokenSource struct {
	Name string
}

// NewEnvVarTokenSource returns a TokenSource that reads from the given env var.
func NewEnvVarTokenSource(name string) *EnvVarTokenSource {
	return &EnvVarTokenSource{Name: name}
}

// Token reads the configured environment variable.
func (s *EnvVarTokenSource) Token(_ context.Context) (string, error) {
	if s == nil || s.Name == "" {
		return "", errors.New("EnvVarTokenSource: name is empty")
	}
	v := strings.TrimSpace(os.Getenv(s.Name))
	if v == "" {
		return "", fmt.Errorf("EnvVarTokenSource: %s is unset", s.Name)
	}
	return v, nil
}

// GcpMetadataTokenSource fetches a GCP ID token from the metadata server,
// bound to the configured audience. Tokens are cached in-memory and
// refreshed when exp − now < 5 minutes.
type GcpMetadataTokenSource struct {
	Audience string
	Client   *http.Client // optional; defaults to a 2s-timeout client

	mu     sync.Mutex
	cached string
	expiry time.Time
}

// NewGcpMetadataTokenSource returns a TokenSource that fetches GCP ID
// tokens from the metadata server bound to the given audience.
func NewGcpMetadataTokenSource(audience string) *GcpMetadataTokenSource {
	return &GcpMetadataTokenSource{
		Audience: audience,
		Client:   &http.Client{Timeout: metadataDefaultTimeout},
	}
}

const (
	// metadataIDTokenURL is the GCE/Cloud Run metadata-server endpoint
	// that mints ID tokens for the attached service account. It is a
	// well-known, non-secret URL — gosec G101 flags the "Token" in the
	// name as a credential, hence the nolint below.
	//nolint:gosec // G101: URL constant, not a credential
	metadataIDTokenURL = "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity"

	metadataRefreshLeeway  = 5 * time.Minute
	metadataDefaultTimeout = 2 * time.Second
	metadataFallbackTTL    = 50 * time.Minute
)

// Token returns a cached metadata-server token, refreshing it when the
// remaining lifetime drops below the refresh leeway.
func (s *GcpMetadataTokenSource) Token(ctx context.Context) (string, error) {
	if s == nil || s.Audience == "" {
		return "", errors.New("GcpMetadataTokenSource: audience is empty")
	}
	s.mu.Lock()
	if s.cached != "" && time.Until(s.expiry) > metadataRefreshLeeway {
		tok := s.cached
		s.mu.Unlock()
		return tok, nil
	}
	s.mu.Unlock()

	client := s.Client
	if client == nil {
		// Fallback for zero-value struct literals; the constructor sets
		// a default client so connection reuse holds across refreshes.
		client = &http.Client{Timeout: metadataDefaultTimeout}
	}

	q := url.Values{}
	q.Set("audience", s.Audience)
	q.Set("format", "full")
	reqURL := metadataIDTokenURL + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")

	resp, err := client.Do(req) //nolint:gosec // G704: target is operator-configured (CONFIG_SERVER_URL) or the GCP metadata server, not user-tainted
	if err != nil {
		return "", fmt.Errorf("metadata token fetch: %w", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			slog.Warn("failed to close metadata response body", slog.String("error", cerr.Error()))
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metadata token fetch: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(body))
	if tok == "" {
		return "", errors.New("metadata token fetch: empty body")
	}

	// GCP ID tokens are valid for ~1h. Parse exp from the JWT to know
	// when to refresh; on parse failure, default to 50 minutes — short
	// enough to never hand out a stale token, long enough to amortize
	// the round trip.
	exp := parseJWTExpiry(tok)
	if exp.IsZero() {
		exp = time.Now().Add(metadataFallbackTTL)
	}

	s.mu.Lock()
	s.cached = tok
	s.expiry = exp
	s.mu.Unlock()
	return tok, nil
}

// parseJWTExpiry extracts the `exp` claim from an unverified JWT payload.
// Verification is the resource server's job; we only need the expiry to
// drive cache busting. Returns the zero time on any failure — callers
// fall back to a conservative default.
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
	if err := json.Unmarshal(raw, &claims); err != nil {
		return time.Time{}
	}
	if claims.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}

// MetadataServerReachable returns true when the GCP metadata server
// answers a TCP probe within `timeout`. Used by DiscoverRemoteSource to
// pick between env-var and metadata-server token paths.
func MetadataServerReachable(timeout time.Duration) bool {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.Dial("tcp", "metadata.google.internal:80")
	if err != nil {
		return false
	}
	if cerr := conn.Close(); cerr != nil {
		slog.Warn("failed to close metadata probe conn", slog.String("error", cerr.Error()))
	}
	return true
}
