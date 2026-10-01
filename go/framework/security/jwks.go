package security

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"go.putnami.dev/errors"
)

// JWKS/JWK error codes.
const (
	CodeJWKParse      errors.Code = "security.jwk_parse"
	CodeJWKSFetch     errors.Code = "security.jwks_fetch"
	CodeJWKSDecode    errors.Code = "security.jwks_decode"
	CodeOIDCDiscovery errors.Code = "security.oidc_discovery"
	CodeInsecureURL   errors.Code = "security.insecure_url"
)

// JWK represents a single JSON Web Key.
type JWK struct {
	Kty string `json:"kty"` // Key type: "RSA" or "EC"
	Kid string `json:"kid"` // Key ID
	Use string `json:"use"` // Key use: "sig"
	Alg string `json:"alg"` // Algorithm: "RS256", "ES256"

	// RSA fields
	N string `json:"n"` // Modulus (base64url)
	E string `json:"e"` // Exponent (base64url)

	// ECDSA fields
	Crv string `json:"crv"` // Curve: "P-256"
	X   string `json:"x"`   // X coordinate (base64url)
	Y   string `json:"y"`   // Y coordinate (base64url)
}

// RSAPublicKey parses the JWK into an *rsa.PublicKey.
func (k *JWK) RSAPublicKey() (*rsa.PublicKey, error) {
	if k.Kty != "RSA" {
		return nil, errors.Newf(CodeJWKParse, "expected kty RSA, got %s", k.Kty)
	}
	nBytes, err := base64URLDecode(k.N)
	if err != nil {
		return nil, errors.Wrapf(err, CodeJWKParse, "decode modulus")
	}
	eBytes, err := base64URLDecode(k.E)
	if err != nil {
		return nil, errors.Wrapf(err, CodeJWKParse, "decode exponent")
	}
	n := new(big.Int).SetBytes(nBytes)
	e := new(big.Int).SetBytes(eBytes)
	if !e.IsInt64() {
		return nil, errors.New(CodeJWKParse, "exponent too large")
	}
	return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
}

// ECDSAPublicKey parses the JWK into an *ecdsa.PublicKey.
func (k *JWK) ECDSAPublicKey() (*ecdsa.PublicKey, error) {
	if k.Kty != "EC" {
		return nil, errors.Newf(CodeJWKParse, "expected kty EC, got %s", k.Kty)
	}
	var curve elliptic.Curve
	switch k.Crv {
	case "P-256":
		curve = elliptic.P256()
	case "P-384":
		curve = elliptic.P384()
	default:
		return nil, errors.Newf(CodeJWKParse, "unsupported curve %s", k.Crv)
	}
	xBytes, err := base64URLDecode(k.X)
	if err != nil {
		return nil, errors.Wrapf(err, CodeJWKParse, "decode x")
	}
	yBytes, err := base64URLDecode(k.Y)
	if err != nil {
		return nil, errors.Wrapf(err, CodeJWKParse, "decode y")
	}
	x := new(big.Int).SetBytes(xBytes)
	y := new(big.Int).SetBytes(yBytes)
	return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, nil
}

// JWKS is a JSON Web Key Set document: the JSON shape a JWKS endpoint serves
// ({"keys":[...]}) and the shape distributed out-of-band — e.g. through a
// config plane — to seed an offline verifier. See NewSeededJWKSFetcher and
// JWKSJWTConfig.SeedKeys.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// ParseJWKS decodes a JWKS document ({"keys":[...]}) from its JSON string form —
// the shape a config plane distributes to seed offline verifiers. An empty
// string yields an empty set and no error, so a caller can feed an optional
// config value straight in; malformed JSON returns the error for the caller to
// log or fail on. Individual keys are validated lazily at verification time
// (JWK.RSAPublicKey / ECDSAPublicKey), not here.
func ParseJWKS(doc string) (JWKS, error) {
	if doc == "" {
		return JWKS{}, nil
	}
	var jwks JWKS
	if err := json.Unmarshal([]byte(doc), &jwks); err != nil {
		return JWKS{}, errors.Wrapf(err, CodeJWKSDecode, "parse JWKS document")
	}
	return jwks, nil
}

// JWKSFetcher fetches and caches JSON Web Keys from a JWKS endpoint.
// Keys are cached by kid with a configurable TTL. On unknown kid,
// a single force-refresh is attempted (rate-limited to prevent abuse).
type JWKSFetcher struct {
	url           string
	cacheTTL      time.Duration
	allowInsecure bool

	mu           sync.RWMutex
	keys         map[string]*JWK
	lastFetch    time.Time
	lastRefresh  time.Time // rate-limit force-refreshes
	refreshDelay time.Duration

	// seed holds keys distributed out-of-band (e.g. a config plane), keyed by
	// kid. It is populated once at construction and never mutated, so it needs
	// no lock. A seeded kid is authoritative and verifies offline: GetKey never
	// reaches the network for it, and it survives a JWKS-endpoint outage
	// (stale-while-revalidate). nil for a plain, network-only fetcher.
	seed map[string]*JWK
}

const (
	defaultCacheTTL    = 1 * time.Hour
	defaultRefreshRate = 30 * time.Second
	fetchTimeout       = 10 * time.Second
	maxRedirects       = 10
)

// NewJWKSFetcher creates a fetcher for the given JWKS URL.
// If cacheTTL is 0, defaults to 1 hour. Unless allowInsecure is set, the URL
// must use https (loopback http is permitted for local development).
func NewJWKSFetcher(jwksURL string, cacheTTL time.Duration, allowInsecure bool) *JWKSFetcher {
	if cacheTTL == 0 {
		cacheTTL = defaultCacheTTL
	}
	return &JWKSFetcher{
		url:           jwksURL,
		cacheTTL:      cacheTTL,
		allowInsecure: allowInsecure,
		keys:          make(map[string]*JWK),
		refreshDelay:  defaultRefreshRate,
	}
}

// NewSeededJWKSFetcher creates a fetcher pre-populated with keys distributed
// out-of-band — e.g. a config plane's JWKS document — so JWT verification works
// before the JWKS endpoint is ever reached, and keeps working while it is down.
// Seeded keys are authoritative: GetKey never reaches the network for a seeded
// kid ("refresh only on unknown kid"). An UNKNOWN kid still triggers a single
// rate-limited refresh from jwksURL (key rotation). jwksURL may be empty for a
// fully offline verifier — an unknown kid then simply fails, and rotation
// requires re-seeding via config. cacheTTL bounds the freshness of
// network-fetched keys only; seeded keys never expire.
func NewSeededJWKSFetcher(jwksURL string, seed []JWK, cacheTTL time.Duration, allowInsecure bool) *JWKSFetcher {
	f := NewJWKSFetcher(jwksURL, cacheTTL, allowInsecure)
	if len(seed) > 0 {
		f.seed = make(map[string]*JWK, len(seed))
		for i := range seed {
			if seed[i].Kid == "" {
				continue
			}
			k := seed[i]
			f.seed[k.Kid] = &k
		}
	}
	return f
}

// GetKey returns the JWK for the given kid. It fetches from the JWKS
// endpoint if the cache is empty or expired. If kid is not found after
// a normal fetch, it force-refreshes once (rate-limited).
//
// For a seeded fetcher (NewSeededJWKSFetcher) a seeded kid is served offline
// without ever touching the network, and only an unknown kid reaches the
// endpoint, so a verifier keeps validating across a JWKS-endpoint outage.
func (f *JWKSFetcher) GetKey(kid string) (*JWK, error) {
	// Try cache first.
	f.mu.RLock()
	key, ok := f.keys[kid]
	expired := time.Since(f.lastFetch) > f.cacheTTL
	f.mu.RUnlock()

	if ok && !expired {
		return key, nil
	}

	// Seeded kids are authoritative and verify offline: a config-distributed key
	// never forces (nor waits on) a network fetch, so it survives an endpoint
	// outage. Only an unknown kid falls through to the fetch below (rotation).
	if seedKey, inSeed := f.seed[kid]; inSeed {
		return seedKey, nil
	}

	// No endpoint to refresh from (offline seed-only verifier): an unknown kid
	// cannot be resolved. Return before attempting a doomed fetch against "".
	if f.url == "" {
		return nil, errKeyNotFound
	}

	// Cache miss or expired: fetch.
	if err := f.fetch(); err != nil {
		// If we have a cached key and fetch failed, use stale cache.
		if ok {
			return key, nil
		}
		return nil, err
	}

	f.mu.RLock()
	key, ok = f.keys[kid]
	f.mu.RUnlock()
	if ok {
		return key, nil
	}

	// Kid not found — try force-refresh (key rotation scenario).
	if f.canRefresh() {
		if err := f.fetch(); err != nil {
			return nil, errKeyNotFound
		}
		f.mu.RLock()
		key, ok = f.keys[kid]
		f.mu.RUnlock()
		if ok {
			return key, nil
		}
	}

	return nil, errKeyNotFound
}

func (f *JWKSFetcher) canRefresh() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return time.Since(f.lastRefresh) > f.refreshDelay
}

func (f *JWKSFetcher) fetch() error {
	resp, err := secureGet(f.url, f.allowInsecure)
	if err != nil {
		return errors.Wrapf(err, CodeJWKSFetch, "fetch JWKS", errors.String("url", f.url))
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close on HTTP response

	if resp.StatusCode != http.StatusOK {
		return errors.Newf(CodeJWKSFetch, "JWKS endpoint returned status %d", resp.StatusCode)
	}

	const maxJWKSSize = 1 << 20 // 1 MiB
	var jwks JWKS
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJWKSSize)).Decode(&jwks); err != nil {
		return errors.Wrapf(err, CodeJWKSDecode, "decode JWKS response")
	}

	keys := make(map[string]*JWK, len(jwks.Keys))
	for i := range jwks.Keys {
		k := &jwks.Keys[i]
		if k.Kid != "" {
			keys[k.Kid] = k
		}
	}

	f.mu.Lock()
	f.keys = keys
	f.lastFetch = time.Now()
	f.lastRefresh = time.Now()
	f.mu.Unlock()

	return nil
}

// DiscoverJWKSURL fetches the OIDC discovery document from the issuer's
// well-known endpoint and returns the jwks_uri. Both the issuer and the
// discovered jwks_uri must use https unless allowInsecure is set (loopback
// http is always permitted for local development).
func DiscoverJWKSURL(issuer string, allowInsecure bool) (string, error) {
	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := fetchOIDCDiscovery(issuer, allowInsecure, &doc); err != nil {
		return "", err
	}
	return requireDiscoveredEndpoint(doc.JWKSURI, "jwks_uri", allowInsecure)
}

// requireDiscoveredEndpoint validates an endpoint URL pulled from a discovery
// document: it must be present and satisfy the secure-URL scheme policy (the
// defense against a metadata document advertising a plaintext http endpoint).
// name labels the field in error messages. It is shared by DiscoverJWKSURL and
// DiscoverIntrospectionURL so both discovered endpoints enforce the same policy.
func requireDiscoveredEndpoint(endpoint, name string, allowInsecure bool) (string, error) {
	if endpoint == "" {
		return "", errors.Newf(CodeOIDCDiscovery, "no %s in discovery document", name)
	}
	if err := requireSecureURL(endpoint, allowInsecure); err != nil {
		return "", errors.Wrapf(err, CodeOIDCDiscovery, "insecure "+name+" in discovery document")
	}
	return endpoint, nil
}

// fetchOIDCDiscovery fetches the issuer's OpenID/OAuth metadata document from
// the well-known endpoint and decodes the requested fields into out. It is the
// shared fetch behind DiscoverJWKSURL and DiscoverIntrospectionURL, so the
// scheme policy, size cap, and status handling stay identical across the two
// discovered endpoints.
func fetchOIDCDiscovery(issuer string, allowInsecure bool, out any) error {
	discoveryURL := issuer + "/.well-known/openid-configuration"
	resp, err := secureGet(discoveryURL, allowInsecure)
	if err != nil {
		return errors.Wrapf(err, CodeOIDCDiscovery, "fetch OIDC discovery", errors.String("url", discoveryURL))
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close on HTTP response

	if resp.StatusCode != http.StatusOK {
		return errors.Newf(CodeOIDCDiscovery, "OIDC discovery returned status %d", resp.StatusCode)
	}

	const maxDiscoverySize = 1 << 20 // 1 MiB
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxDiscoverySize)).Decode(out); err != nil {
		return errors.Wrapf(err, CodeOIDCDiscovery, "decode OIDC discovery")
	}
	return nil
}

// secureClient builds an HTTP client whose redirects are bounded by maxRedirects
// and pinned to the secure-URL scheme policy, so an https endpoint cannot be
// bounced to plaintext http mid-exchange. It is shared by secureGet (key/OIDC
// discovery fetches) and the introspection resolver so the redirect policy stays
// identical across every security HTTP call.
func secureClient(timeout time.Duration, allowInsecure bool) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.Newf(CodeInsecureURL, "stopped after %d redirects", maxRedirects)
			}
			return requireSecureURL(req.URL.String(), allowInsecure)
		},
	}
}

// secureGet performs an HTTP GET after validating that the URL scheme is
// permitted, and constrains redirects to the same scheme policy so an https
// endpoint cannot be bounced to plaintext http for the actual key fetch.
func secureGet(rawURL string, allowInsecure bool) (*http.Response, error) {
	if err := requireSecureURL(rawURL, allowInsecure); err != nil {
		return nil, err
	}
	return secureClient(fetchTimeout, allowInsecure).Get(rawURL)
}

// requireSecureURL rejects key-endpoint URLs whose scheme is not https.
// Plaintext http is permitted only for loopback hosts (local development)
// or when allowInsecure is explicitly set.
func requireSecureURL(rawURL string, allowInsecure bool) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return errors.Wrapf(err, CodeInsecureURL, "parse URL")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if allowInsecure || isLoopbackHost(u.Hostname()) {
			return nil
		}
		return errors.Newf(CodeInsecureURL, "insecure scheme for key endpoint %q: use https or set AllowInsecure", u.Redacted())
	default:
		return errors.Newf(CodeInsecureURL, "unsupported scheme %q for key endpoint: use https", u.Scheme)
	}
}

// isLoopbackHost reports whether host refers to the loopback interface.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
