package storage

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.putnami.dev/errors"
)

// Endpoints and scopes for the native Google Cloud Storage HTTP client. They are
// fields on GCSBackend (not package globals) so tests can point them at an
// httptest server, but these defaults match production GCP.
const (
	gcsDefaultEndpoint   = "https://storage.googleapis.com"
	gcsDefaultMetadata   = "http://metadata.google.internal"
	gcsDefaultIAM        = "https://iamcredentials.googleapis.com"
	gcsDefaultTokenURI   = "https://oauth2.googleapis.com/token"
	gcsReadWriteScope    = "https://www.googleapis.com/auth/devstorage.read_write"
	gcsMetadataTokenPath = "/computeMetadata/v1/instance/service-accounts/default/token"
	gcsMetadataEmailPath = "/computeMetadata/v1/instance/service-accounts/default/email"
	// tokenExpiryGrace renews a cached token slightly before it actually expires
	// so an in-flight request never races the expiry.
	tokenExpiryGrace = 30 * time.Second
)

// tokenSource yields OAuth2 bearer tokens used to authenticate GCS requests. An
// empty token means the request is sent unauthenticated (used by tests against
// the in-memory fake server).
type tokenSource interface {
	token(ctx context.Context) (string, error)
}

// staticTokenSource always returns a fixed token. Used in tests and for the
// no-auth fake server (empty string disables the Authorization header).
type staticTokenSource string

func (s staticTokenSource) token(context.Context) (string, error) { return string(s), nil }

// cachedTokenSource memoizes a bearer token until shortly before it expires,
// re-fetching via fetch on demand. It is safe for concurrent use.
type cachedTokenSource struct {
	fetch func(ctx context.Context) (token string, expiry time.Time, err error)

	mu      sync.Mutex
	value   string
	expires time.Time
}

func (c *cachedTokenSource) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.value != "" && time.Now().Before(c.expires) {
		return c.value, nil
	}
	value, expiry, err := c.fetch(ctx)
	if err != nil {
		return "", err
	}
	c.value = value
	c.expires = expiry
	return value, nil
}

// oauthTokenResponse is the common token-endpoint reply shape.
type oauthTokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

// newMetadataTokenSource builds a token source backed by the GCP metadata
// server. This is the credential path for Workload Identity on Cloud Run / GKE
// and requires no static secret.
func newMetadataTokenSource(client *http.Client, metadataHost string) tokenSource {
	return &cachedTokenSource{fetch: func(ctx context.Context) (string, time.Time, error) {
		endpoint := strings.TrimRight(metadataHost, "/") + gcsMetadataTokenPath
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return "", time.Time{}, errors.Wrap(err, CodeStorageRequest,
				errors.String("backend", "gcs"), errors.String("op", "metadata_token"))
		}
		req.Header.Set("Metadata-Flavor", "Google")
		resp, err := client.Do(req)
		if err != nil {
			return "", time.Time{}, errors.Wrap(err, CodeStorageRequest,
				errors.String("backend", "gcs"), errors.String("op", "metadata_token"))
		}
		defer func() { _ = resp.Body.Close() }() //nolint:errcheck // token fetch; body close error is not actionable
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body) //nolint:errcheck // best-effort error detail
			return "", time.Time{}, errors.New(CodeStorageRequest, "metadata token request failed",
				errors.String("backend", "gcs"), errors.Int("status", resp.StatusCode), errors.String("body", string(body)))
		}
		var tok oauthTokenResponse
		if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
			return "", time.Time{}, errors.Wrap(err, CodeStorageRequest,
				errors.String("backend", "gcs"), errors.String("op", "metadata_token"))
		}
		return tok.AccessToken, tokenExpiry(tok.ExpiresIn), nil
	}}
}

// serviceAccountKey is the subset of a GCP service-account JSON key file used to
// mint OAuth2 tokens (via a signed JWT assertion) and to sign URLs locally.
type serviceAccountKey struct {
	Type         string `json:"type"`
	ClientEmail  string `json:"client_email"`
	PrivateKey   string `json:"private_key"`
	PrivateKeyID string `json:"private_key_id"`
	TokenURI     string `json:"token_uri"`
	ProjectID    string `json:"project_id"`
}

// parseServiceAccountKey parses a service-account JSON key file and its RSA
// private key.
func parseServiceAccountKey(data []byte) (*serviceAccountKey, *rsa.PrivateKey, error) {
	var key serviceAccountKey
	if err := json.Unmarshal(data, &key); err != nil {
		return nil, nil, errors.Wrap(err, CodeStorageRequest,
			errors.String("backend", "gcs"), errors.String("op", "parse_credentials"))
	}
	signKey, err := parseRSAPrivateKey(key.PrivateKey)
	if err != nil {
		return nil, nil, err
	}
	return &key, signKey, nil
}

// parseRSAPrivateKey decodes a PEM-encoded RSA private key in PKCS#8 or PKCS#1
// form, as found in GCP service-account key files.
func parseRSAPrivateKey(pemKey string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemKey))
	if block == nil {
		return nil, errors.New(CodeStorageRequest, "invalid private key PEM", errors.String("backend", "gcs"))
	}
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New(CodeStorageRequest, "private key is not RSA", errors.String("backend", "gcs"))
		}
		return rsaKey, nil
	}
	rsaKey, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.Wrap(err, CodeStorageRequest,
			errors.String("backend", "gcs"), errors.String("op", "parse_private_key"))
	}
	return rsaKey, nil
}

// newServiceAccountTokenSource builds a token source that exchanges a signed JWT
// assertion for an access token at the key's token endpoint (RFC 7523).
func newServiceAccountTokenSource(client *http.Client, key *serviceAccountKey, signKey *rsa.PrivateKey) tokenSource {
	return &cachedTokenSource{fetch: func(ctx context.Context) (string, time.Time, error) {
		assertion, err := buildJWTAssertion(key, signKey, time.Now())
		if err != nil {
			return "", time.Time{}, err
		}
		tokenURI := key.TokenURI
		if tokenURI == "" {
			tokenURI = gcsDefaultTokenURI
		}
		form := url.Values{}
		form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
		form.Set("assertion", assertion)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURI, strings.NewReader(form.Encode()))
		if err != nil {
			return "", time.Time{}, errors.Wrap(err, CodeStorageRequest,
				errors.String("backend", "gcs"), errors.String("op", "sa_token"))
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := client.Do(req)
		if err != nil {
			return "", time.Time{}, errors.Wrap(err, CodeStorageRequest,
				errors.String("backend", "gcs"), errors.String("op", "sa_token"))
		}
		defer func() { _ = resp.Body.Close() }() //nolint:errcheck // token fetch; body close error is not actionable
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body) //nolint:errcheck // best-effort error detail
			return "", time.Time{}, errors.New(CodeStorageRequest, "service account token request failed",
				errors.String("backend", "gcs"), errors.Int("status", resp.StatusCode), errors.String("body", string(body)))
		}
		var tok oauthTokenResponse
		if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
			return "", time.Time{}, errors.Wrap(err, CodeStorageRequest,
				errors.String("backend", "gcs"), errors.String("op", "sa_token"))
		}
		return tok.AccessToken, tokenExpiry(tok.ExpiresIn), nil
	}}
}

// buildJWTAssertion constructs and RS256-signs the JWT bearer assertion used to
// request an access token for a service account.
func buildJWTAssertion(key *serviceAccountKey, signKey *rsa.PrivateKey, now time.Time) (string, error) {
	audience := key.TokenURI
	if audience == "" {
		audience = gcsDefaultTokenURI
	}
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	if key.PrivateKeyID != "" {
		header["kid"] = key.PrivateKeyID
	}
	claims := map[string]any{
		"iss":   key.ClientEmail,
		"scope": gcsReadWriteScope,
		"aud":   audience,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", errors.Wrap(err, CodeStorageRequest, errors.String("backend", "gcs"), errors.String("op", "jwt_header"))
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", errors.Wrap(err, CodeStorageRequest, errors.String("backend", "gcs"), errors.String("op", "jwt_claims"))
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, signKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", errors.Wrap(err, CodeStorageRequest, errors.String("backend", "gcs"), errors.String("op", "jwt_sign"))
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// tokenExpiry converts a token's expires_in (seconds) into an absolute renewal
// deadline, applying the grace window. A non-positive value yields a deadline in
// the past so the next call always refreshes.
func tokenExpiry(expiresIn int) time.Time {
	if expiresIn <= 0 {
		return time.Now().Add(-time.Second)
	}
	return time.Now().Add(time.Duration(expiresIn)*time.Second - tokenExpiryGrace)
}
