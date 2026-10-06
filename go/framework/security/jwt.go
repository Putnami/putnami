package security

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	phttp "go.putnami.dev/http"
	"go.putnami.dev/logger"
	identity "go.putnami.dev/protocol/identity/schema"
)

// --- JWT header parsing (shared by HS256 and JWKS paths) ---

// jwtHeader holds the parsed JWT JOSE header.
type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

// parseJWTHeader splits the token and decodes the JOSE header.
func parseJWTHeader(token string) (jwtHeader, []string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return jwtHeader{}, nil, errInvalidToken
	}
	headerBytes, err := base64URLDecode(parts[0])
	if err != nil {
		return jwtHeader{}, nil, errInvalidToken
	}
	var h jwtHeader
	if err := json.Unmarshal(headerBytes, &h); err != nil {
		return jwtHeader{}, nil, errInvalidToken
	}
	return h, parts, nil
}

// decodeJWTPayload decodes the base64url-encoded payload segment.
func decodeJWTPayload(part string) (map[string]any, error) {
	b, err := base64URLDecode(part)
	if err != nil {
		return nil, errInvalidToken
	}
	var payload map[string]any
	if err := json.Unmarshal(b, &payload); err != nil {
		return nil, errInvalidToken
	}
	return payload, nil
}

// checkExpiration checks the "exp" claim if present.
func checkExpiration(payload map[string]any) error {
	if exp, ok := payload[string(identity.ClaimNameExp)].(float64); ok {
		if time.Unix(int64(exp), 0).Before(time.Now()) {
			return errTokenExpired
		}
	}
	return nil
}

// checkAudience validates the "aud" claim against the expected audience.
// The "aud" claim can be a string or an array of strings per RFC 7519.
func checkAudience(payload map[string]any, audience string) error {
	if audience == "" {
		return nil
	}
	switch aud := payload[string(identity.ClaimNameAud)].(type) {
	case string:
		if aud == audience {
			return nil
		}
	case []any:
		for _, v := range aud {
			if s, ok := v.(string); ok && s == audience {
				return nil
			}
		}
	}
	return errAudienceMismatch
}

// checkIssuer validates the "iss" claim against the expected issuer.
func checkIssuer(payload map[string]any, issuer string) error {
	if issuer == "" {
		return nil
	}
	if iss, ok := payload[string(identity.ClaimNameIss)].(string); ok && iss == issuer {
		return nil
	}
	return errIssuerMismatch
}

// requireExpiration returns an error if a token lacks an "exp" claim and
// expiration is required.
func requireExpiration(payload map[string]any, required bool) error {
	if !required {
		return nil
	}
	if _, ok := payload[string(identity.ClaimNameExp)].(float64); !ok {
		return errMissingExpiration
	}
	return nil
}

// extractBearerToken extracts the token from "Bearer <token>".
func extractBearerToken(ctx *phttp.Context) string {
	auth := ctx.Header("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(auth, "Bearer ")
}

// --- HS256 JWT (existing) ---

// JWTConfig configures JWT bearer token authentication.
type JWTConfig struct {
	// Secret is the HMAC-SHA256 key used to validate token signatures.
	Secret string

	// Audience, if set, requires the "aud" claim to contain this value.
	Audience string

	// Issuer, if set, requires the "iss" claim to equal this value.
	Issuer string

	// AllowMissingExpiration permits tokens without an "exp" claim. By
	// default a missing "exp" is rejected, so a leaked bearer token cannot
	// be replayed indefinitely. Set true only for deliberately long-lived
	// tokens.
	AllowMissingExpiration bool
}

// JWT returns an identity resolver middleware that authenticates requests
// using a JWT bearer token in the Authorization header.
//
// Only HMAC-SHA256 (HS256) tokens are supported. The middleware validates:
//   - Token structure (three base64url-encoded segments)
//   - Algorithm header (must be HS256)
//   - HMAC signature
//   - Expiration ("exp" claim; required unless AllowMissingExpiration is set)
//   - Audience ("aud" claim) when Audience is set
//   - Issuer ("iss" claim) when Issuer is set
//
// On success, standard claims (sub, iss, scope, roles, client_id) are mapped
// to phttp.Claims fields; all other claims go into Claims.Extra.
//
// Usage:
//
//	server.Use(security.JWT(security.JWTConfig{
//	    Secret:   os.Getenv("JWT_SECRET"),
//	    Audience: "my-service",
//	    Issuer:   "https://auth.example.com",
//	}))
func JWT(cfg JWTConfig) phttp.Middleware {
	secretBytes := []byte(cfg.Secret)
	if len(secretBytes) == 0 {
		// Fail closed on an empty secret. An empty HMAC key is not a secret:
		// anyone can compute a valid HS256 signature under it and forge
		// arbitrary claims, so validating with it is a silent authentication
		// bypass. Mirror plugin.go's unconfigured path — log an error and
		// return a resolver that authenticates no one — instead of the
		// otherwise fail-closed module accepting forged tokens.
		decisionLogger().Error("security.JWT configured with an empty Secret — failing closed; all bearer tokens will be rejected (set a non-empty JWT secret)", nil)
		return IdentityResolver(func(ctx *phttp.Context) *phttp.Claims {
			return nil
		})
	}
	return IdentityResolver(func(ctx *phttp.Context) *phttp.Claims {
		token := extractBearerToken(ctx)
		if token == "" {
			return nil
		}
		claims, err := validateHMAC(token, secretBytes, cfg)
		if err != nil {
			return nil
		}
		return claims
	})
}

// validateHMAC validates an HMAC-SHA256 (HS256) JWT and returns claims.
func validateHMAC(token string, secret []byte, cfg JWTConfig) (*phttp.Claims, error) {
	header, parts, err := parseJWTHeader(token)
	if err != nil {
		return nil, err
	}
	if header.Alg != "HS256" {
		return nil, errUnsupportedAlg
	}

	// Verify HMAC signature.
	sig, err := base64URLDecode(parts[2])
	if err != nil {
		return nil, errInvalidToken
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(parts[0]))
	mac.Write([]byte("."))
	mac.Write([]byte(parts[1]))
	expected := mac.Sum(nil)
	if !hmac.Equal(sig, expected) {
		return nil, errInvalidSignature
	}

	payload, err := decodeJWTPayload(parts[1])
	if err != nil {
		return nil, err
	}
	if err := requireExpiration(payload, !cfg.AllowMissingExpiration); err != nil {
		return nil, err
	}
	if err := checkExpiration(payload); err != nil {
		return nil, err
	}
	if err := checkAudience(payload, cfg.Audience); err != nil {
		return nil, err
	}
	if err := checkIssuer(payload, cfg.Issuer); err != nil {
		return nil, err
	}
	return phttp.ClaimsFromMap(payload), nil
}

// --- JWKS JWT (RS256 / ES256) ---

// JWKSJWTConfig configures JWT authentication using a JWKS endpoint.
type JWKSJWTConfig struct {
	// JWKSURL is the URL to fetch the JSON Web Key Set from.
	// Mutually exclusive with Issuer.
	JWKSURL string

	// Issuer is the OIDC issuer URL. The JWKS URL is discovered
	// via /.well-known/openid-configuration.
	// Mutually exclusive with JWKSURL.
	Issuer string

	// CacheTTL controls how long JWKS keys are cached. Default: 1 hour.
	CacheTTL time.Duration

	// Audience, if set, validates the "aud" claim matches.
	Audience string

	// RequiredIssuer, if set, requires the "iss" claim to equal this value.
	// When Issuer (OIDC discovery) is configured the expected issuer defaults
	// to Issuer, so RequiredIssuer is only needed when authenticating via
	// JWKSURL directly, or to assert a different "iss" than the discovery URL.
	RequiredIssuer string

	// AllowInsecure permits fetching the JWKS/OIDC discovery document over
	// plaintext http from non-loopback hosts. By default only https (and
	// loopback http for local development) is allowed, so a network attacker
	// cannot substitute their own signing keys. Do not enable in production.
	AllowInsecure bool

	// AllowMissingExpiration permits tokens without an "exp" claim. By
	// default a missing "exp" is rejected, so a leaked bearer token cannot
	// be replayed indefinitely. Set true only for deliberately long-lived
	// tokens.
	AllowMissingExpiration bool

	// SeedKeys pre-populates the verifier with public keys distributed
	// out-of-band — e.g. through a config plane — so JWT verification works
	// before the JWKS endpoint is ever reached, and keeps working while it is
	// down. A seeded kid never waits for, nor triggers, a JWKS fetch or OIDC
	// discovery. On an unknown kid the verifier refreshes from JWKSURL/Issuer
	// (rate-limited), so key rotation continues to work: with Issuer, a failed
	// discovery is retried by a later unknown kid at least 5 seconds after the
	// failure, until it succeeds. When SeedKeys is set, JWKSURL/Issuer may be
	// omitted for a fully offline verifier (rotation then requires re-seeding via
	// config). See NewSeededJWKSFetcher.
	SeedKeys []JWK
}

// JWKSJWT returns an identity resolver middleware that authenticates requests
// using a JWT bearer token validated against a JWKS endpoint.
//
// Supported algorithms: RS256, ES256, ES384. HS256 is explicitly rejected
// to prevent algorithm confusion attacks.
//
// The "exp" claim is required by default so a leaked bearer token cannot be
// replayed indefinitely; set AllowMissingExpiration to accept tokens without
// it. This matches the HS256 path.
//
// The JWKS is fetched lazily on first request and cached with TTL.
// Unknown key IDs trigger a force-refresh (rate-limited).
//
// With Issuer set and JWKSURL empty, the first token that needs the JWKS
// discovers its URL, after the "iss" check. The resolver keeps a successful
// discovery for its lifetime, and concurrent tokens share one discovery. A
// failed discovery is logged, and the first token that needs the JWKS at least
// 5 seconds after the failure discovers again; tokens before then do no network
// I/O. Until a discovery succeeds, a token is refused unless its kid is in
// SeedKeys: a seeded kid verifies at once, without waiting for discovery.
// Without JWKSURL, Issuer, or SeedKeys there is no key source: the resolver
// logs an error once and refuses every token.
//
// The "iss" claim is enforced when an expected issuer is known: the OIDC
// Issuer used for key discovery doubles as the required "iss", and RequiredIssuer
// can set or override it (e.g. for the JWKSURL path). Without an expected issuer
// the "iss" claim is not checked — set RequiredIssuer to require it. The iss
// check runs before the JWKS is fetched, so chaining one JWKSJWT per issuer
// rejects a foreign-issuer token without force-refreshing an earlier resolver's
// keys.
//
// Usage:
//
//	server.Use(security.JWKSJWT(security.JWKSJWTConfig{
//	    Issuer: "https://accounts.google.com",
//	}))
func JWKSJWT(cfg JWKSJWTConfig) phttp.Middleware {
	return jwksJWT(cfg, time.Now)
}

// jwksDiscoveryRetryInterval is the minimum time between a failed build of a
// JWKSJWT resolver's key fetcher and the next build attempt.
const jwksDiscoveryRetryInterval = 5 * time.Second

// jwksJWT is JWKSJWT with the clock that times key-fetcher build retries.
func jwksJWT(cfg JWKSJWTConfig, clock func() time.Time) phttp.Middleware {
	// The OIDC issuer used for discovery doubles as the required "iss" unless
	// RequiredIssuer overrides it. This closes the asymmetry with the HS256
	// path (which always enforces iss) and prevents token confusion in
	// shared-JWKS deployments such as accounts.google.com.
	expectedIssuer := cfg.RequiredIssuer
	if expectedIssuer == "" {
		expectedIssuer = cfg.Issuer
	}

	if cfg.JWKSURL == "" && cfg.Issuer == "" && len(cfg.SeedKeys) == 0 {
		decisionLogger().Error("security.JWKSJWT configured without JWKSURL, Issuer, or SeedKeys — failing closed; all bearer tokens will be rejected", nil)
		return IdentityResolver(func(*phttp.Context) *phttp.Claims {
			return nil
		})
	}

	keys := &lazyJWKSFetcher{
		build:         func() (*JWKSFetcher, error) { return buildJWKSFetcher(cfg) },
		issuer:        cfg.Issuer,
		clock:         clock,
		retryInterval: jwksDiscoveryRetryInterval,
	}
	if cfg.JWKSURL == "" && cfg.Issuer != "" && len(cfg.SeedKeys) > 0 {
		if seeded := NewSeededJWKSFetcher("", cfg.SeedKeys, cfg.CacheTTL, cfg.AllowInsecure); len(seeded.seed) > 0 {
			keys.seeded = seeded
		}
	}

	return IdentityResolver(func(ctx *phttp.Context) *phttp.Claims {
		token := extractBearerToken(ctx)
		if token == "" {
			return nil
		}
		claims, err := validateJWKS(token, keys, cfg.Audience, expectedIssuer, !cfg.AllowMissingExpiration)
		if err != nil {
			return nil
		}
		return claims
	})
}

// buildJWKSFetcher builds the key fetcher of a JWKSJWT resolver whose config
// names at least one key source. With Issuer set and JWKSURL empty it discovers
// the JWKS URL and returns the discovery error. The fetcher holds SeedKeys when
// set; without JWKSURL and Issuer it has no URL and verifies seeded kids only.
func buildJWKSFetcher(cfg JWKSJWTConfig) (*JWKSFetcher, error) {
	url := cfg.JWKSURL
	if url == "" && cfg.Issuer != "" {
		discovered, err := DiscoverJWKSURL(cfg.Issuer, cfg.AllowInsecure)
		if err != nil {
			return nil, err
		}
		url = discovered
	}
	if len(cfg.SeedKeys) > 0 {
		return NewSeededJWKSFetcher(url, cfg.SeedKeys, cfg.CacheTTL, cfg.AllowInsecure), nil
	}
	return NewJWKSFetcher(url, cfg.CacheTTL, cfg.AllowInsecure), nil
}

// jwkSource returns the verification key for a kid.
type jwkSource interface {
	GetKey(kid string) (*JWK, error)
}

// lazyJWKSFetcher is the jwkSource of a JWKSJWT resolver. It builds the key
// fetcher on first use and keeps the first one built for its lifetime. A failed
// build is logged once and recorded with the time it failed: until
// retryInterval has elapsed, a lookup that needs the build returns that error
// without calling build, and the first one after that builds again. Concurrent
// lookups share one build: the build runs under mu, and every caller re-checks
// the state once it holds mu. Until a build succeeds, a kid that seeded holds
// resolves from seeded, without mu and without network I/O.
type lazyJWKSFetcher struct {
	build         func() (*JWKSFetcher, error)
	issuer        string
	clock         func() time.Time
	retryInterval time.Duration

	// seeded is a fetcher without a URL that holds the configured seed keys
	// when the build discovers the JWKS URL. It is nil otherwise, and it is set
	// before the first lookup and never replaced.
	seeded *JWKSFetcher

	// built is set once, under mu, and read without mu.
	built atomic.Pointer[JWKSFetcher]

	mu       sync.Mutex
	err      error
	failedAt time.Time
}

// GetKey returns the key for kid from the fetcher that resolves it.
func (l *lazyJWKSFetcher) GetKey(kid string) (*JWK, error) {
	f, err := l.fetcherFor(kid)
	if err != nil {
		return nil, err
	}
	return f.GetKey(kid)
}

// fetcherFor returns the built fetcher, or seeded when it holds kid and no
// fetcher is built yet. Otherwise it builds the fetcher, unless a failure is
// recorded within retryInterval, in which case it returns the recorded error.
func (l *lazyJWKSFetcher) fetcherFor(kid string) (*JWKSFetcher, error) {
	if f := l.built.Load(); f != nil {
		return f, nil
	}
	if l.seeded != nil {
		if _, ok := l.seeded.seed[kid]; ok {
			return l.seeded, nil
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if f := l.built.Load(); f != nil {
		return f, nil
	}
	if l.err != nil && l.clock().Sub(l.failedAt) < l.retryInterval {
		return nil, l.err
	}
	f, err := l.build()
	if err != nil {
		l.err = err
		l.failedAt = l.clock()
		l.logFailure(err)
		return nil, err
	}
	l.err = nil
	l.built.Store(f)
	return f, nil
}

// logFailure logs a failed build with the issuer, the cause, and the time
// until the next build. It logs at Warn when seeded keeps verifying seeded
// kids, and at Error when every token is refused.
func (l *lazyJWKSFetcher) logFailure(err error) {
	attrs := []slog.Attr{
		slog.String("issuer", l.issuer),
		slog.Duration("retryAfter", l.retryInterval),
	}
	if l.seeded != nil {
		decisionLogger().Warn("security.JWKSJWT OIDC discovery failed — only seeded keys verify until a later discovery succeeds",
			append(attrs, logger.ErrorAttr(err))...)
		return
	}
	decisionLogger().Error("security.JWKSJWT OIDC discovery failed — rejecting all bearer tokens until a later discovery succeeds", err, attrs...)
}

// validateJWKS validates a JWT using keys from keys.
func validateJWKS(token string, keys jwkSource, audience, issuer string, requireExp bool) (*phttp.Claims, error) {
	header, parts, err := parseJWTHeader(token)
	if err != nil {
		return nil, err
	}

	// Reject HS256 to prevent algorithm confusion attacks.
	if header.Alg == "HS256" {
		return nil, errUnsupportedAlg
	}

	payload, err := decodeJWTPayload(parts[1])
	if err != nil {
		return nil, err
	}

	// Gate the JWKS fetch on the "iss" claim, before any network I/O. With
	// resolvers chained one-per-issuer, a token meant for a later resolver
	// carries a foreign "iss" and a kid absent here; reaching GetKey would
	// force-refresh this issuer's JWKS on every such request — a hot-path stall
	// that can deadlock a cold, self-referential JWKS host. Rejecting on
	// the iss mismatch first lets the chain advance without that fetch. This is
	// routing, not trust: the signature is still verified below before any claim
	// is honored, so issuer enforcement is preserved.
	if err := checkIssuer(payload, issuer); err != nil {
		return nil, err
	}

	// Fetch the signing key.
	jwk, err := keys.GetKey(header.Kid)
	if err != nil {
		return nil, err
	}

	// Verify signature based on algorithm.
	signingInput := parts[0] + "." + parts[1]
	sig, err := base64URLDecode(parts[2])
	if err != nil {
		return nil, errInvalidToken
	}

	switch header.Alg {
	case "RS256":
		pubKey, err := jwk.RSAPublicKey()
		if err != nil {
			return nil, err
		}
		if err := verifyRSA256(signingInput, sig, pubKey); err != nil {
			return nil, err
		}
	case "ES256":
		pubKey, err := jwk.ECDSAPublicKey()
		if err != nil {
			return nil, err
		}
		if err := verifyECDSA(signingInput, sig, pubKey, crypto.SHA256, 32); err != nil {
			return nil, err
		}
	case "ES384":
		pubKey, err := jwk.ECDSAPublicKey()
		if err != nil {
			return nil, err
		}
		if err := verifyECDSA(signingInput, sig, pubKey, crypto.SHA384, 48); err != nil {
			return nil, err
		}
	default:
		return nil, errUnsupportedAlg
	}

	if err := requireExpiration(payload, requireExp); err != nil {
		return nil, err
	}
	if err := checkExpiration(payload); err != nil {
		return nil, err
	}
	if err := checkAudience(payload, audience); err != nil {
		return nil, err
	}
	return phttp.ClaimsFromMap(payload), nil
}

// --- Signature verification ---

// verifyRSA256 verifies an RSA PKCS#1 v1.5 signature with SHA-256.
func verifyRSA256(signingInput string, sig []byte, pubKey *rsa.PublicKey) error {
	h := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(pubKey, crypto.SHA256, h[:], sig); err != nil {
		return errInvalidSignature
	}
	return nil
}

// verifyECDSA verifies an ECDSA signature in the JWS R||S format (RFC 7518 §3.4).
// keySize is the byte length of each R and S component (32 for P-256, 48 for P-384).
func verifyECDSA(signingInput string, sig []byte, pubKey *ecdsa.PublicKey, hash crypto.Hash, keySize int) error {
	if len(sig) != keySize*2 {
		return errInvalidSignature
	}
	r := new(big.Int).SetBytes(sig[:keySize])
	s := new(big.Int).SetBytes(sig[keySize:])

	hasher := hash.New()
	hasher.Write([]byte(signingInput))
	digest := hasher.Sum(nil)

	if !ecdsa.Verify(pubKey, digest, r, s) {
		return errInvalidSignature
	}
	return nil
}

// --- Utilities ---

// base64URLDecode decodes base64url without padding.
func base64URLDecode(s string) ([]byte, error) {
	switch len(s) % 4 {
	case 2:
		s += "=="
	case 3:
		s += "="
	}
	return base64.URLEncoding.DecodeString(s)
}

// --- Sentinel errors ---

var (
	errInvalidToken      = &ValidationError{"invalid token format"}
	errInvalidSignature  = &ValidationError{"invalid signature"}
	errTokenExpired      = &ValidationError{"token expired"}
	errMissingExpiration = &ValidationError{"token missing expiration"}
	errUnsupportedAlg    = &ValidationError{"unsupported algorithm"}
	errKeyNotFound       = &ValidationError{"signing key not found"}
	errAudienceMismatch  = &ValidationError{"audience mismatch"}
	errIssuerMismatch    = &ValidationError{"issuer mismatch"}
)

// ValidationError represents an authentication validation error.
type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string { return e.Message }
