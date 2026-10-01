package security

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"go.putnami.dev/protocol/keyring"
)

// This file implements the SIGN side of go/framework/security: an in-memory
// SigningKeyProvider that mints compact JWS tokens under a single active key,
// rotates keys atomically, and publishes the public JWKS a verifier consumes.
// It builds on the keyring vocabulary (go.putnami.dev/protocol/keyring): the
// closed key-state machine, the private→public projection (keyring.PublicJWKS),
// and the publishability rule. The signing crypto lives in sign.go, the exact
// inverse of the verify path in jwt.go. No private key material is ever logged,
// returned in an error, or emitted from a String method.

// DefaultOverlapWindow is the default retirement overlap: how long a rotated-out
// (retiring) key remains in the published JWKS so tokens signed just before a
// rotation still verify during the rollover. It also caps the JWKS handler's
// Cache-Control max-age — see JWKSHandler and the overlap ≥ cache-TTL invariant.
const DefaultOverlapWindow = 24 * time.Hour

// ErrNoSigningKey is the FAIL-CLOSED result of constructing a provider with no
// signing key material when ephemeral generation was not explicitly allowed. A
// serverless cold start with no configured key returns this rather than silently
// minting tokens under an untrusted, process-local key.
var ErrNoSigningKey = errors.New("security: no signing key configured and AllowEphemeral is false (refusing to mint tokens under an untrusted ephemeral key)")

// SigningKeyProvider mints compact JWS tokens under a single active signing key
// and publishes the public JWKS a verifier consumes. Implementations are safe
// for concurrent use: a signer or publisher never observes a partially-rotated
// keyring (zero or two active keys).
type SigningKeyProvider interface {
	// Sign returns a compact JWS (header.payload.signature) over claims, signed by
	// the current active key. The JOSE header carries alg, kid and typ:"JWT". The
	// token verifies through the standard validateJWKS path against PublicJWKS.
	Sign(claims map[string]any) (string, error)
	// ActiveKID returns the kid of the current active signing key.
	ActiveKID() string
	// ActiveAlg returns the JWS alg ("ES256"/"RS256") of the current active key.
	ActiveAlg() string
	// PublicJWKS returns the public JWKS: the active key plus every retiring key
	// still inside the overlap window. It never contains private key material.
	PublicJWKS() keyring.JWKS
	// OverlapWindow reports how long a retired key stays in the published JWKS. It
	// is part of the interface so JWKSHandler can clamp its Cache-Control max-age to
	// the REAL overlap of whatever provider it is given (the overlap ≥ cache-TTL
	// invariant), rather than guessing a default for a provider it cannot introspect.
	OverlapWindow() time.Duration
}

// RotatingSigningKeyProvider is a SigningKeyProvider that also exposes the
// WRITER side of the key lifecycle: rotating in a new active key (demoting the
// previous active key to retiring for the overlap window) and revoking a
// non-active key. It is the operator-facing rotation surface a caller drives to
// perform a key rollover through the public API; the in-memory provider
// implements it.
//
// Rotate and Revoke serialize with each other and publish their result with a
// single atomic swap, so a concurrent Sign/PublicJWKS never observes a
// partially-rotated keyring (zero or two active keys). Adding this interface is
// purely additive: it names methods the in-memory provider already has and does
// not change SigningKeyProvider or NewSigningKeyProvider.
type RotatingSigningKeyProvider interface {
	SigningKeyProvider
	// Rotate promotes newKey to the single active key and demotes the previous
	// active key to retiring for the overlap window. The demotion is validated
	// against the closed key-state machine; a rotated-in kid that already exists
	// is rejected.
	Rotate(newKey SigningKey) error
	// Revoke marks a NON-active key revoked (terminal); a revoked key leaves the
	// published JWKS immediately and never signs. The active key cannot be revoked
	// directly (that would leave the keyring with no active key) — rotate in a
	// replacement first.
	Revoke(kid string) error
}

// SigningKey describes one managed signing key: its kid, its JWS algorithm, its
// keyring lifecycle state, and the Go crypto private key. Exactly one of EC/RSA
// must be set and must match Alg (ES256 → EC P-256, RS256 → RSA). A zero Alg
// defaults to ES256 and a zero State defaults to active.
type SigningKey struct {
	Kid   string
	Alg   string
	State keyring.KeyState
	// RetiredAt is when a retiring key entered its overlap window. It is honored
	// only for a retiring key; a zero value on a retiring key defaults to "now" at
	// construction (a freshly-supplied retiring key starts its window then). Supply
	// it when rehydrating a persisted keyring so the overlap window is measured from
	// the real retirement instant rather than restarting on every reload.
	RetiredAt time.Time
	EC        *ecdsa.PrivateKey
	RSA       *rsa.PrivateKey
}

// ProviderConfig configures an in-memory SigningKeyProvider.
type ProviderConfig struct {
	// Keys is the set of managed signing keys. Exactly one must be in the active
	// state; a set with zero or more than one active key is rejected.
	Keys []SigningKey

	// OverlapWindow bounds how long a retired key stays in the published JWKS (and
	// thus the maximum Cache-Control max-age the JWKS handler may advertise).
	// Defaults to DefaultOverlapWindow when zero or negative.
	OverlapWindow time.Duration

	// AllowEphemeral permits generating an in-memory ephemeral active key when NO
	// signing key material is supplied. Default false: construction FAILS CLOSED
	// with ErrNoSigningKey rather than minting tokens under an untrusted,
	// process-local key. Set true (an explicit opt-in) only where an ephemeral,
	// non-persisted signer is acceptable (e.g. a throwaway local process).
	AllowEphemeral bool
}

// managedKey is one signing key the provider manages: a Go crypto private key,
// its JOSE identity (kid, alg) and its keyring lifecycle state. Exactly one of
// ec/rsa is non-nil, selected by alg. A managedKey is treated as IMMUTABLE once
// published in a providerState — a state change produces a new value (withState)
// rather than mutating in place — which is what makes the lock-free read path
// race-free. It never exposes private material: it has no exported fields, its
// String redacts, and only sign and publicJWK touch the private key.
type managedKey struct {
	kid       string
	alg       string
	state     keyring.KeyState
	retiredAt time.Time // when the key entered "retiring"; zero otherwise
	ephemeral bool      // true only for a process-generated ephemeral key (see generateEphemeralKey)
	ec        *ecdsa.PrivateKey
	rsa       *rsa.PrivateKey
}

// String renders a REDACTED description of the key. It never includes private
// key material, so a managed key can be safely logged or embedded in an error.
func (k *managedKey) String() string {
	return fmt.Sprintf("managedKey{kid:%q alg:%s state:%s}", k.kid, k.alg, k.state)
}

// withState returns a copy of the key in a new lifecycle state (and retiredAt),
// sharing the immutable crypto private key. It never mutates the receiver, so a
// concurrent reader holding the previous providerState sees a stable value.
func (k *managedKey) withState(state keyring.KeyState, retiredAt time.Time) *managedKey {
	nk := *k
	nk.state = state
	nk.retiredAt = retiredAt
	return &nk
}

// sign assembles a compact JWS over payload using this key. The header carries
// alg, kid and typ:"JWT"; the signature is produced by the alg's signer in
// sign.go, the inverse of the verify path.
func (k *managedKey) sign(payload []byte) (string, error) {
	signingInput, err := buildSigningInput(k.alg, k.kid, payload)
	if err != nil {
		return "", err
	}
	var sig []byte
	switch k.alg {
	case AlgES256:
		sig, err = signES256(signingInput, k.ec)
	case AlgRS256:
		sig, err = signRS256(signingInput, k.rsa)
	default:
		return "", fmt.Errorf("security: unsupported signing alg %q", k.alg)
	}
	if err != nil {
		return "", err
	}
	return signingInput + "." + b64u(sig), nil
}

// publicJWK projects the key's PUBLIC verification material to a JOSE JWK. The
// result structurally cannot carry private material (keyring.JWK has no private
// fields), so this projection can never leak the private key.
func (k *managedKey) publicJWK() keyring.JWK {
	jwk := keyring.JWK{Kid: k.kid, Use: "sig", Alg: k.alg}
	switch {
	case k.ec != nil:
		jwk.Kty = string(keyring.KeyTypeEC)
		jwk.Crv = curveName(k.ec.Curve)
		jwk.X, jwk.Y = ecPublicParams(&k.ec.PublicKey)
	case k.rsa != nil:
		jwk.Kty = string(keyring.KeyTypeRSA)
		jwk.N, jwk.E = rsaPublicParams(&k.rsa.PublicKey)
	}
	return jwk
}

// publishJWK builds the PrivateJWK-shaped record (public parameters + the given
// lifecycle state, never private material) that is fed to keyring.PublicJWKS so
// the protocol's publishability/oct filter decides whether the key is emitted.
func (k *managedKey) publishJWK(state keyring.KeyState) keyring.PrivateJWK {
	pub := k.publicJWK()
	return keyring.PrivateJWK{
		Kty:   pub.Kty,
		Kid:   pub.Kid,
		Use:   pub.Use,
		Alg:   pub.Alg,
		State: state,
		Crv:   pub.Crv,
		X:     pub.X,
		Y:     pub.Y,
		N:     pub.N,
		E:     pub.E,
	}
}

// newManagedKey validates a SigningKey and builds its managed form. It enforces
// a non-empty kid, a supported alg (default ES256), a valid state (default
// active), and a matching crypto private key of the correct type/curve.
func newManagedKey(sk SigningKey) (*managedKey, error) {
	if sk.Kid == "" {
		return nil, fmt.Errorf("security: signing key requires a kid")
	}
	alg := sk.Alg
	if alg == "" {
		alg = AlgES256
	}
	state := sk.State
	if state == "" {
		state = keyring.KeyStateActive
	}
	if !state.Valid() {
		return nil, fmt.Errorf("security: signing key %q has invalid state %q", sk.Kid, state)
	}
	mk := &managedKey{kid: sk.Kid, alg: alg, state: state, retiredAt: sk.RetiredAt}
	switch alg {
	case AlgES256:
		if sk.EC == nil {
			return nil, fmt.Errorf("security: ES256 key %q requires an ECDSA private key", sk.Kid)
		}
		if sk.RSA != nil {
			return nil, fmt.Errorf("security: key %q supplies both EC and RSA material", sk.Kid)
		}
		if sk.EC.Curve != elliptic.P256() {
			return nil, fmt.Errorf("security: ES256 key %q must be P-256", sk.Kid)
		}
		mk.ec = sk.EC
	case AlgRS256:
		if sk.RSA == nil {
			return nil, fmt.Errorf("security: RS256 key %q requires an RSA private key", sk.Kid)
		}
		if sk.EC != nil {
			return nil, fmt.Errorf("security: key %q supplies both EC and RSA material", sk.Kid)
		}
		mk.rsa = sk.RSA
	default:
		return nil, fmt.Errorf("security: unsupported signing alg %q for key %q", alg, sk.Kid)
	}
	return mk, nil
}

// providerState is the IMMUTABLE snapshot a signer/publisher reads. It is
// replaced wholesale (never mutated in place) on every rotation via an atomic
// pointer swap, so a reader always sees a consistent set with exactly one active
// key — never zero, never two, mid-rotation.
type providerState struct {
	keys      []*managedKey
	activeKid string
}

// active returns the single active key, or nil if (impossibly) none is active.
func (s *providerState) active() *managedKey {
	for _, k := range s.keys {
		if k.kid == s.activeKid && k.state == keyring.KeyStateActive {
			return k
		}
	}
	return nil
}

// inMemoryProvider is the in-memory SigningKeyProvider. Writers (Rotate, Revoke)
// serialize on mu and publish a new providerState with an atomic Store; readers
// (Sign, PublicJWKS, ActiveKID, ActiveAlg) take no lock and just Load the
// current immutable snapshot. This copy-on-write swap is the atomic active-key
// switch: the transition from old→new state is a single atomic pointer store, so
// no reader can observe an intermediate keyring.
type inMemoryProvider struct {
	mu      sync.Mutex
	state   atomic.Pointer[providerState]
	overlap time.Duration
	clock   func() time.Time
}

// providerOption is an unexported test seam.
type providerOption func(*inMemoryProvider)

// withClock overrides the provider clock so window-expiry behavior can be tested
// deterministically. Unexported: production always uses time.Now.
func withClock(fn func() time.Time) providerOption {
	return func(p *inMemoryProvider) { p.clock = fn }
}

// NewSigningKeyProvider builds an in-memory SigningKeyProvider from cfg. It
// enforces the core invariants at construction: EXACTLY ONE active key, unique
// kids, and the fail-closed ephemeral policy (no key material + AllowEphemeral
// false ⇒ ErrNoSigningKey).
func NewSigningKeyProvider(cfg ProviderConfig) (SigningKeyProvider, error) {
	return newProvider(cfg)
}

// Compile-time proof that the in-memory provider satisfies the writer-side
// rotation surface, so RotatingSigningKeyProvider stays in lock-step with the
// concrete Rotate/Revoke method signatures.
var _ RotatingSigningKeyProvider = (*inMemoryProvider)(nil)

// NewRotatingSigningKeyProvider builds a RotatingSigningKeyProvider: the same
// in-memory provider NewSigningKeyProvider returns, typed to also expose the
// Rotate/Revoke rollover surface through the public API. Every construction
// invariant is identical to NewSigningKeyProvider (exactly one active key,
// unique kids, the fail-closed ephemeral policy). Use it when a caller needs to
// drive the key lifecycle — e.g. an operator-triggered rotation flow, or the
// rotation conformance proof — rather than only sign and publish.
func NewRotatingSigningKeyProvider(cfg ProviderConfig) (RotatingSigningKeyProvider, error) {
	return newProvider(cfg)
}

func newProvider(cfg ProviderConfig, opts ...providerOption) (*inMemoryProvider, error) {
	overlap := cfg.OverlapWindow
	if overlap <= 0 {
		overlap = DefaultOverlapWindow
	}
	p := &inMemoryProvider{overlap: overlap, clock: time.Now}
	for _, o := range opts {
		o(p)
	}

	keys := make([]*managedKey, 0, len(cfg.Keys))
	seen := make(map[string]bool, len(cfg.Keys))
	activeCount := 0
	activeKid := ""
	for _, sk := range cfg.Keys {
		mk, err := newManagedKey(sk)
		if err != nil {
			return nil, err
		}
		if seen[mk.kid] {
			return nil, fmt.Errorf("security: duplicate kid %q", mk.kid)
		}
		seen[mk.kid] = true
		// A key supplied already-retiring starts its overlap window now, so it is
		// published during the window and swept out after it.
		if mk.state == keyring.KeyStateRetiring && mk.retiredAt.IsZero() {
			mk.retiredAt = p.clock()
		}
		if mk.state == keyring.KeyStateActive {
			activeCount++
			activeKid = mk.kid
		}
		keys = append(keys, mk)
	}

	switch {
	case activeCount == 1:
		// exactly one active — the invariant holds.
	case activeCount > 1:
		return nil, fmt.Errorf("security: keyring must have exactly one active key, found %d", activeCount)
	case len(cfg.Keys) == 0 && cfg.AllowEphemeral:
		mk, err := generateEphemeralKey()
		if err != nil {
			return nil, err
		}
		keys = append(keys, mk)
		activeKid = mk.kid
	case len(cfg.Keys) == 0:
		// No key material at all and ephemeral generation not allowed: fail closed.
		return nil, ErrNoSigningKey
	default:
		// Keys were supplied but none is active: a keyring with no active key cannot
		// sign. Do not silently generate one even under AllowEphemeral — the caller
		// provided material and simply mislabeled it.
		return nil, fmt.Errorf("security: keyring has %d key(s) but none is active", len(cfg.Keys))
	}

	p.state.Store(&providerState{keys: keys, activeKid: activeKid})
	return p, nil
}

// generateEphemeralKey mints a throwaway in-memory ES256 (P-256) active key. Its
// kid is a deterministic thumbprint of the public point, so the same key always
// advertises the same kid.
func generateEphemeralKey() (*managedKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("security: generate ephemeral key: %w", err)
	}
	return &managedKey{kid: ephemeralKID(&key.PublicKey), alg: AlgES256, state: keyring.KeyStateActive, ephemeral: true, ec: key}, nil
}

// ephemeralKID derives a stable kid from the public point (an EC JWK
// thumbprint-style digest), so an ephemeral key is identifiable without a
// configured kid and two processes never collide on a shared literal.
func ephemeralKID(pub *ecdsa.PublicKey) string {
	x, y := ecPublicParams(pub)
	sum := sha256.Sum256([]byte("EC|" + curveName(pub.Curve) + "|" + x + "|" + y))
	return "ephemeral-" + b64u(sum[:8])
}

// Sign implements SigningKeyProvider.
func (p *inMemoryProvider) Sign(claims map[string]any) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("security: marshal claims: %w", err)
	}
	k := p.state.Load().active()
	if k == nil {
		return "", errors.New("security: no active signing key")
	}
	return k.sign(payload)
}

// ActiveKID implements SigningKeyProvider.
func (p *inMemoryProvider) ActiveKID() string { return p.state.Load().activeKid }

// ActiveAlg implements SigningKeyProvider.
func (p *inMemoryProvider) ActiveAlg() string {
	if k := p.state.Load().active(); k != nil {
		return k.alg
	}
	return ""
}

// PublicJWKS implements SigningKeyProvider. It projects the current key set
// through keyring.PublicJWKS, which drops every non-publishable (revoked/expired)
// and symmetric key and copies only public fields. A retiring key past the
// overlap window is marked expired here so the projection drops it: this is the
// READ-TIME window guard that removes a stale key even without a rotation.
func (p *inMemoryProvider) PublicJWKS() keyring.JWKS {
	st := p.state.Load()
	now := p.clock()
	kr := &keyring.PrivateKeyring{
		ProtocolVersion: keyring.ProtocolVersion,
		Keys:            make([]keyring.PrivateJWK, 0, len(st.keys)),
	}
	for _, k := range st.keys {
		state := k.state
		if state == keyring.KeyStateRetiring && now.Sub(k.retiredAt) > p.overlap {
			state = keyring.KeyStateExpired
		}
		kr.Keys = append(kr.Keys, k.publishJWK(state))
	}
	return keyring.PublicJWKS(kr)
}

// OverlapWindow reports the retirement overlap. It satisfies SigningKeyProvider so
// JWKSHandler can clamp its Cache-Control max-age to this provider's real overlap.
func (p *inMemoryProvider) OverlapWindow() time.Duration { return p.overlap }

// Rotate promotes newKey to the single active key and demotes the previous
// active key to retiring for the overlap window. It is the atomic active-key
// switch: the whole transition is published with one atomic pointer store while
// mu is held, so a concurrent signer/publisher never observes zero or two active
// keys. Any retiring key already past the overlap window is swept out (it leaves
// the JWKS). The demotion is validated against the closed key-state machine.
func (p *inMemoryProvider) Rotate(newKey SigningKey) error {
	if newKey.State != "" && newKey.State != keyring.KeyStateActive {
		return fmt.Errorf("security: a rotated-in key must be active, got %q", newKey.State)
	}
	newKey.State = keyring.KeyStateActive
	mk, err := newManagedKey(newKey)
	if err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	old := p.state.Load()
	now := p.clock()

	newKeys := make([]*managedKey, 0, len(old.keys)+1)
	for _, k := range old.keys {
		if k.kid == mk.kid {
			return fmt.Errorf("security: rotated-in kid %q already exists in the keyring", mk.kid)
		}
		switch k.state {
		case keyring.KeyStateActive:
			if !keyring.CanTransition(keyring.KeyStateActive, keyring.KeyStateRetiring) {
				return fmt.Errorf("security: illegal active→retiring transition")
			}
			newKeys = append(newKeys, k.withState(keyring.KeyStateRetiring, now))
		case keyring.KeyStateRetiring:
			// Past-window retiring keys leave the keyring (and thus the JWKS).
			if now.Sub(k.retiredAt) > p.overlap {
				continue
			}
			newKeys = append(newKeys, k)
		default:
			// Expired/revoked keys are neither signed with nor published: drop them.
			continue
		}
	}
	newKeys = append(newKeys, mk)
	p.state.Store(&providerState{keys: newKeys, activeKid: mk.kid})
	// Count the successful rotation (secret-safe: event label only, no key
	// material — see keyring_observe.go).
	recordKeyringEvent(keyringEventRotate)
	return nil
}

// Revoke marks a NON-active key revoked (terminal). A revoked key is never
// published and never signs. The active key cannot be revoked directly — doing
// so would leave the keyring with no active key — so callers must Rotate in a
// replacement first. The transition is validated against the key-state machine.
func (p *inMemoryProvider) Revoke(kid string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	old := p.state.Load()

	found := false
	newKeys := make([]*managedKey, 0, len(old.keys))
	for _, k := range old.keys {
		if k.kid != kid {
			newKeys = append(newKeys, k)
			continue
		}
		found = true
		if k.state == keyring.KeyStateActive {
			return fmt.Errorf("security: cannot revoke the active key %q; rotate to a new active key first", kid)
		}
		if !keyring.CanTransition(k.state, keyring.KeyStateRevoked) {
			return fmt.Errorf("security: cannot revoke key %q in state %s", kid, k.state)
		}
		newKeys = append(newKeys, k.withState(keyring.KeyStateRevoked, k.retiredAt))
	}
	if !found {
		return fmt.Errorf("security: no key with kid %q", kid)
	}
	p.state.Store(&providerState{keys: newKeys, activeKid: old.activeKid})
	// Count the successful revocation (secret-safe: event label only, no key
	// material — see keyring_observe.go).
	recordKeyringEvent(keyringEventRevoke)
	return nil
}

// --- Keyring store (interface + in-memory implementation) ------------------

// KeyringStore is the persistence SEAM for a signing keyring: it loads the
// owner-side PrivateKeyring at startup and saves it after a rotation. This
// package ships the in-memory implementation; go.putnami.dev/keyringstore owns
// the durable, database-backed store. A store traffics in *keyring.PrivateKeyring — the canonical,
// serializable owner document — so every backend shares one contract and no
// backend handles Go crypto types directly.
type KeyringStore interface {
	// Load returns the persisted owner keyring. It returns ErrNoSigningKey when the
	// store holds no keyring, so the fail-closed policy applies uniformly.
	Load(ctx context.Context) (*keyring.PrivateKeyring, error)
	// Save persists the owner keyring, replacing any previous document.
	Save(ctx context.Context, kr *keyring.PrivateKeyring) error
}

// InMemoryKeyringStore is a process-local KeyringStore backed by a single
// in-memory document. It is safe for concurrent use and deep-copies on Load and
// Save so a caller cannot mutate the stored document by retaining a returned
// pointer. Intended for tests and single-process use; use
// go.putnami.dev/keyringstore for durable shared ownership.
type InMemoryKeyringStore struct {
	mu sync.RWMutex
	kr *keyring.PrivateKeyring
}

// NewInMemoryKeyringStore creates a store seeded with an optional initial
// keyring (deep-copied). Pass nil for an empty store.
func NewInMemoryKeyringStore(kr *keyring.PrivateKeyring) *InMemoryKeyringStore {
	s := &InMemoryKeyringStore{}
	if kr != nil {
		s.kr = cloneKeyring(kr)
	}
	return s
}

// Load implements KeyringStore.
func (s *InMemoryKeyringStore) Load(_ context.Context) (*keyring.PrivateKeyring, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.kr == nil {
		return nil, ErrNoSigningKey
	}
	return cloneKeyring(s.kr), nil
}

// Save implements KeyringStore.
func (s *InMemoryKeyringStore) Save(_ context.Context, kr *keyring.PrivateKeyring) error {
	if kr == nil {
		return fmt.Errorf("security: cannot save a nil keyring")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kr = cloneKeyring(kr)
	return nil
}

// cloneKeyring deep-copies a keyring document. PrivateJWK is all value fields, so
// copying the Keys slice fully detaches the copy from the caller's document.
func cloneKeyring(kr *keyring.PrivateKeyring) *keyring.PrivateKeyring {
	out := &keyring.PrivateKeyring{Schema: kr.Schema, ProtocolVersion: kr.ProtocolVersion}
	if kr.Keys != nil {
		out.Keys = make([]keyring.PrivateJWK, len(kr.Keys))
		copy(out.Keys, kr.Keys)
	}
	return out
}

// NewSigningKeyProviderFromStore loads the owner keyring from store and builds an
// in-memory provider over its publishable (active/retiring) keys, parsing each
// PrivateJWK into a Go crypto private key. Revoked, expired and symmetric keys
// are skipped (they neither sign nor publish). The loaded keyring must contain
// exactly one active key, or construction fails.
//
// An EMPTY store fails closed with ErrNoSigningKey by default, but honors the
// explicit cfg.AllowEphemeral opt-in exactly as NewSigningKeyProvider does: with
// no persisted key and AllowEphemeral set, it mints a throwaway in-memory signer
// (the serverless cold-start case AllowEphemeral exists for). The flag is thus
// consistent across both constructors rather than silently inert here.
func NewSigningKeyProviderFromStore(ctx context.Context, store KeyringStore, cfg ProviderConfig) (SigningKeyProvider, error) {
	kr, err := store.Load(ctx)
	if err != nil {
		if errors.Is(err, ErrNoSigningKey) && cfg.AllowEphemeral {
			// No persisted key, but the caller opted into an ephemeral signer: fall
			// through to construction with no keys, which mints the ephemeral key.
			return NewSigningKeyProvider(cfg)
		}
		return nil, err
	}
	keys, err := signingKeysFromKeyring(kr)
	if err != nil {
		return nil, err
	}
	cfg.Keys = append(cfg.Keys, keys...)
	return NewSigningKeyProvider(cfg)
}

// signingKeysFromKeyring parses the publishable keys of an owner keyring into
// SigningKeys carrying Go crypto private keys.
func signingKeysFromKeyring(kr *keyring.PrivateKeyring) ([]SigningKey, error) {
	if kr == nil {
		return nil, fmt.Errorf("security: nil keyring")
	}
	out := make([]SigningKey, 0, len(kr.Keys))
	for i := range kr.Keys {
		pj := kr.Keys[i]
		state := pj.State
		if state == "" {
			state = keyring.KeyStateActive
		}
		// Only active/retiring keys carry into a live provider; revoked and expired
		// keys are neither signed with nor published.
		if !state.Publishable() {
			continue
		}
		// A retiring key carries its retirement instant so the overlap window is
		// measured from when it was actually retired, surviving a reload. A missing
		// or malformed instant leaves it zero, so newProvider defaults it to "now"
		// (the freshly-retired behavior).
		retiredAt := parseRetiredAt(pj.RetiredAt)
		switch keyring.KeyType(pj.Kty) {
		case keyring.KeyTypeEC:
			ec, err := parseECPrivateKey(pj)
			if err != nil {
				return nil, err
			}
			out = append(out, SigningKey{Kid: pj.Kid, Alg: algForJWK(pj, AlgES256), State: state, RetiredAt: retiredAt, EC: ec})
		case keyring.KeyTypeRSA:
			r, err := parseRSAPrivateKey(pj)
			if err != nil {
				return nil, err
			}
			out = append(out, SigningKey{Kid: pj.Kid, Alg: algForJWK(pj, AlgRS256), State: state, RetiredAt: retiredAt, RSA: r})
		case keyring.KeyTypeOct:
			// Symmetric keys cannot sign a JWS verified through a public JWKS.
			continue
		default:
			return nil, fmt.Errorf("security: unsupported key type %q for key %q", pj.Kty, pj.Kid)
		}
	}
	return out, nil
}

// algForJWK returns the JWK's declared alg, or fallback when unset.
func algForJWK(pj keyring.PrivateJWK, fallback string) string {
	if pj.Alg != "" {
		return pj.Alg
	}
	return fallback
}

// parseRetiredAt parses a keyring document's RFC 3339 retiredAt instant. An empty
// or malformed value yields the zero time — newProvider then treats a retiring key
// with a zero instant as freshly retired (window starts now), the safe fallback.
func parseRetiredAt(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// --- JWKS publication handler ----------------------------------------------

// JWKSHandlerConfig configures the JWKS publication handler.
type JWKSHandlerConfig struct {
	// MaxAge is the Cache-Control max-age advertised to verifiers. It is CLAMPED to
	// at most the provider's overlap window: a verifier must never cache the JWKS
	// longer than a retired key remains published, or it could keep trusting a key
	// past its retirement/revocation (overlap ≥ consumer cache TTL). Defaults to the
	// overlap window when zero or negative.
	MaxAge time.Duration
}

// JWKSHandler returns a net/http handler that serves the provider's public JWKS
// as application/json — the document a verifier's JWKSFetcher / ParseJWKS
// consumes. Every active and in-window retiring kid is present, so a verifier
// that hit an unknown kid and refetches (JWKSFetcher semantics) finds the
// rotated-in key.
//
// INVARIANT (overlap ≥ cache TTL): the Cache-Control max-age is clamped to the
// provider's overlap window, so a cached JWKS can never outlive a retired key's
// presence in it. Widen the overlap window to allow a longer cache — never the
// reverse — or a verifier could cache the JWKS past a key's revocation.
func JWKSHandler(p SigningKeyProvider, cfg JWKSHandlerConfig) http.Handler {
	overlap := p.OverlapWindow()
	if overlap <= 0 {
		overlap = DefaultOverlapWindow
	}
	maxAge := cfg.MaxAge
	if maxAge <= 0 || maxAge > overlap {
		maxAge = overlap
	}
	maxAgeSeconds := int(maxAge.Seconds())

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := json.Marshal(p.PublicJWKS())
		if err != nil {
			http.Error(w, "failed to encode JWKS", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", maxAgeSeconds))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			if _, err := w.Write(body); err != nil {
				return
			}
		}
	})
}
