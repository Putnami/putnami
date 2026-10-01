// Package keyring defines the Putnami canonical keyring vocabulary: the single
// cross-language contract for a signing keyring's key-state lifecycle, its
// private-keyring document and PUBLIC JWKS projection, and the PHC-style
// versioned credential-digest grammar. Go, TypeScript, and (later) Python
// adapters consume the same shapes, the same closed key-state machine, and the
// same digest string format instead of each hand-rolling unversioned PBKDF2
// strings and single-key JWKs.
//
// This package is vocabulary + declaration + strict parse/validate ONLY. It
// deliberately performs no cryptography: it does not sign, verify, hash, or
// derive keys. It defines the shapes, the closed enums, the legal key-state
// transition table, the private→public projection rule, and the digest
// grammar's parser/encoder. The signing, verifying, and hashing implementations
// that consume this vocabulary live in the frameworks (go/framework/security,
// go/framework/keyringstore, and the TypeScript security twin).
//
// Three vocabularies live here, deliberately kept in one module because a
// keyring owner needs all three together:
//
//   - KeyState: the closed key-state machine (active | retiring | revoked |
//     expired) with an explicit legal-transition table, a Transition validator,
//     and a String round-trip. Revoked is terminal.
//
//   - PrivateKeyring / JWKS: the private-keyring document (signing keys with
//     private material) and its PUBLIC JWKS projection. The structural invariant
//     — private key material (d, p, q, dp, dq, qi, k) NEVER appears in the public
//     projection — is enforced two-sided: the public JWK type has no private
//     fields (so PublicJWKS cannot emit them), and the strict public parser
//     rejects any private field as an unknown field.
//
//   - Digest: the PHC-style versioned credential-digest grammar
//     ($pbkdf2-sha256$v=1$i=<iterations>$<b64-salt>$<b64-hash> and
//     $hmac-sha256$v=1$<b64-salt>$<b64-hash>) with a strict parser and a
//     round-trip encoder.
//
// The JWK/JWKS shapes are modeled compatibly with the verify-side types in
// go/framework/security (JWK, JWKS, ParseJWKS): field names and JSON tags follow
// standard JOSE (kty, crv, x, y, n, e, kid, alg, use), so those consumers can
// adopt this package as the canonical vocabulary. See README.md for the digest
// grammar EBNF, the JWKS projection rule, and the fixture format.
package keyring

import (
	"encoding/base64"
	"fmt"
	"sort"
)

// ProtocolVersion is the current keyring-protocol version. It is stamped into a
// PrivateKeyring document and pinned by the JSON schemas. Bumped on any
// backwards-incompatible change to the document shapes, enums, or the transition
// table; adding an optional field within an existing shape does not bump it.
const ProtocolVersion = 1

// DigestVersion is the current version tag of the PHC-style digest grammar (the
// "v=1" segment). It is independent of ProtocolVersion so the digest grammar can
// evolve without churning the keyring document, and vice versa.
const DigestVersion = 1

// Iteration bounds for the pbkdf2-sha256 digest grammar. These are GRAMMAR
// bounds (a well-formed digest string carries a positive, sanely-capped
// iteration count) — not a security policy. A minimum-work policy for a given
// deployment is enforced by the hashing implementation that consumes this
// grammar, not by this parser.
const (
	// MinIterations is the smallest legal pbkdf2 iteration count. Zero or
	// negative iterations make no sense and are rejected as out-of-range.
	MinIterations = 1
	// MaxIterations caps the pbkdf2 iteration count so a hostile digest string
	// cannot request an unbounded amount of work from a verifier.
	MaxIterations = 10_000_000
)

// --- Key-state lifecycle ---------------------------------------------------

// KeyState is the lifecycle state of a signing key in a keyring. The enum is
// intentionally closed; adding a value requires a ProtocolVersion bump so
// consumers can decide how to react.
type KeyState string

// KeyState values.
const (
	// KeyStateActive is a key usable for BOTH signing new material and verifying.
	// A keyring's signing key is the active one.
	KeyStateActive KeyState = "active"
	// KeyStateRetiring is a key no longer used to sign new material but still
	// trusted for verification during a rollover grace window (so tokens signed
	// just before rotation still verify). It is published in the JWKS.
	KeyStateRetiring KeyState = "retiring"
	// KeyStateRevoked is a key that has been withdrawn — typically because it was
	// compromised. It MUST NOT be trusted for verification and is never published
	// in the JWKS. Revoked is terminal: a key is never un-revoked.
	KeyStateRevoked KeyState = "revoked"
	// KeyStateExpired is a key whose validity window elapsed naturally (it was not
	// compromised). It is no longer published for verification. An expired key may
	// still be revoked retroactively (e.g. a later-discovered compromise), so
	// expired is not terminal — only revoked is.
	KeyStateExpired KeyState = "expired"
)

// Valid reports whether s is a recognized key state.
func (s KeyState) Valid() bool {
	switch s {
	case KeyStateActive, KeyStateRetiring, KeyStateRevoked, KeyStateExpired:
		return true
	default:
		return false
	}
}

// String returns the canonical wire form of the key state, giving a String
// round-trip: KeyState(s.String()) == s for every valid state.
func (s KeyState) String() string { return string(s) }

// Publishable reports whether a key in this state belongs in the PUBLIC JWKS a
// verifier fetches. Only active and retiring keys are publishable: a verifier
// must accept signatures from the active key and from a retiring key during
// rollover grace, but must never see a revoked (compromised) or expired
// (past-validity) key. This is the fail-closed rule the public projection bakes
// in so no consumer can accidentally advertise a distrusted key.
func (s KeyState) Publishable() bool {
	switch s {
	case KeyStateActive, KeyStateRetiring:
		return true
	default:
		return false
	}
}

// legalTransitions is the closed legal-transition table for the key-state
// machine: it maps each state to the set of states it may legally move to. The
// design invariant is that revocation is always reachable from any non-revoked
// state (a compromise can be discovered at any time), and that revoked is the
// only terminal state (a key is never un-revoked, re-activated, or un-expired).
//
//	active   → retiring | expired | revoked
//	retiring →            expired | revoked
//	expired  →                      revoked   (retroactive compromise disclosure)
//	revoked  → ∅                              (terminal)
//
// A transition must change state: a same-state edge (e.g. active→active) is not
// in the table and is rejected by Transition.
var legalTransitions = map[KeyState]map[KeyState]bool{
	KeyStateActive:   {KeyStateRetiring: true, KeyStateExpired: true, KeyStateRevoked: true},
	KeyStateRetiring: {KeyStateExpired: true, KeyStateRevoked: true},
	KeyStateExpired:  {KeyStateRevoked: true},
	KeyStateRevoked:  {},
}

// CanTransition reports whether a key may legally move from state from to state
// to. It returns false for an unknown state, and false for a same-state edge (a
// transition must change state).
func CanTransition(from, to KeyState) bool {
	return legalTransitions[from][to]
}

// Terminal reports whether state s permits no further transitions. Only revoked
// is terminal.
func (s KeyState) Terminal() bool {
	dests, ok := legalTransitions[s]
	return ok && len(dests) == 0
}

// AllowedTransitions returns the states s may legally transition to, sorted for
// deterministic output (the underlying table is a Go map, whose iteration order
// is not stable). It returns an empty slice for a terminal or unknown state.
func (s KeyState) AllowedTransitions() []KeyState {
	dests := legalTransitions[s]
	out := make([]KeyState, 0, len(dests))
	for to := range dests {
		out = append(out, to)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// --- JSON Web Key shapes ---------------------------------------------------

// JWK is the PUBLIC JSON Web Key: public verification material only. Its field
// names and JSON tags follow standard JOSE and match go/framework/security.JWK
// so the verify side can adopt this package as the canonical vocabulary. It has
// NO private-material fields (d, p, q, dp, dq, qi, k), so it structurally cannot
// carry a private key — the public-projection invariant is enforced by the type
// itself, not by a runtime scrub.
type JWK struct {
	Kty string `json:"kty"`           // Key type: "RSA", "EC" (public); "oct" is symmetric and never public.
	Kid string `json:"kid,omitempty"` // Key ID.
	Use string `json:"use,omitempty"` // Public key use, e.g. "sig".
	Alg string `json:"alg,omitempty"` // Algorithm, e.g. "RS256", "ES256".

	// EC public parameters.
	Crv string `json:"crv,omitempty"` // Curve, e.g. "P-256".
	X   string `json:"x,omitempty"`   // X coordinate (base64url, no padding).
	Y   string `json:"y,omitempty"`   // Y coordinate (base64url, no padding).

	// RSA public parameters.
	N string `json:"n,omitempty"` // Modulus (base64url, no padding).
	E string `json:"e,omitempty"` // Exponent (base64url, no padding).
}

// JWKS is the PUBLIC JSON Web Key Set document ({"keys":[...]}) — the shape a
// JWKS endpoint serves and a config plane distributes to seed offline verifiers.
// It matches go/framework/security.JWKS so a verifier can consume a keyring's
// public projection directly. It carries no private material.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// PrivateJWK is a full JSON Web Key that MAY carry private key material and a
// keyring lifecycle State. It lives only inside a PrivateKeyring and is NEVER
// serialized into a public JWKS. Public() projects it to the public JWK,
// dropping every private field and the State.
type PrivateJWK struct {
	Kty   string   `json:"kty"`             // Key type: "RSA", "EC", or "oct".
	Kid   string   `json:"kid,omitempty"`   // Key ID.
	Use   string   `json:"use,omitempty"`   // Key use, e.g. "sig".
	Alg   string   `json:"alg,omitempty"`   // Algorithm, e.g. "RS256", "ES256", "HS256".
	State KeyState `json:"state,omitempty"` // Keyring lifecycle state (keyring-local; not a JOSE field).

	// RetiredAt is when a retiring key entered its rollover overlap window, as an
	// RFC 3339 timestamp. It is keyring-local lifecycle state (like State, not a
	// JOSE field) and is meaningful only for a retiring key — it lets a durable
	// store round-trip the retirement instant so the overlap window survives a
	// process restart rather than restarting on every reload. Empty for a key that
	// is not retiring. NEVER projected to the public JWK.
	RetiredAt string `json:"retiredAt,omitempty"`

	// EC public parameters.
	Crv string `json:"crv,omitempty"` // Curve, e.g. "P-256".
	X   string `json:"x,omitempty"`   // X coordinate (base64url, no padding).
	Y   string `json:"y,omitempty"`   // Y coordinate (base64url, no padding).

	// RSA public parameters.
	N string `json:"n,omitempty"` // Modulus (base64url, no padding).
	E string `json:"e,omitempty"` // Exponent (base64url, no padding).

	// PRIVATE key material — NEVER projected to the public JWK. Base64url,
	// no padding, per RFC 7518.
	D  string `json:"d,omitempty"`  // RSA/EC private exponent / EC private key.
	P  string `json:"p,omitempty"`  // RSA first prime factor.
	Q  string `json:"q,omitempty"`  // RSA second prime factor.
	Dp string `json:"dp,omitempty"` // RSA d mod (p-1).
	Dq string `json:"dq,omitempty"` // RSA d mod (q-1).
	Qi string `json:"qi,omitempty"` // RSA q^-1 mod p (CRT coefficient).
	K  string `json:"k,omitempty"`  // Symmetric ("oct") key material.
}

// Public projects a PrivateJWK to its PUBLIC JWK, copying only public fields and
// dropping every private-material field and the keyring State. This is the sole
// per-key projection function; PublicJWKS composes it with the publishable
// filter. Because the returned JWK has no private fields, the projection cannot
// leak private material even if a caller mis-populates the private JWK.
func (k PrivateJWK) Public() JWK {
	return JWK{
		Kty: k.Kty,
		Kid: k.Kid,
		Use: k.Use,
		Alg: k.Alg,
		Crv: k.Crv,
		X:   k.X,
		Y:   k.Y,
		N:   k.N,
		E:   k.E,
	}
}

// PrivateKeyring is a private-keyring document: the versioned set of signing
// keys, each of which may carry private material and a lifecycle State. It is
// the owner-side document; a verifier receives only its PublicJWKS projection.
type PrivateKeyring struct {
	Schema          string       `json:"$schema,omitempty"`
	ProtocolVersion int          `json:"protocolVersion"`
	Keys            []PrivateJWK `json:"keys"`
}

// PublicJWKS projects a private keyring to the PUBLIC JWKS a verifier fetches. It
// applies two rules, in order, per key:
//
//  1. Publishability: only active and retiring keys are published; revoked
//     (compromised) and expired (past-validity) keys are dropped — a verifier
//     must never see a distrusted key. See KeyState.Publishable.
//  2. Asymmetry: symmetric ("oct") keys are dropped — a symmetric key has no
//     public projection, and publishing its material would leak the secret.
//
// Every surviving key is projected with PrivateJWK.Public, so no private field
// can appear in the result. The output is a plain {"keys":[...]} JWKS compatible
// with go/framework/security.ParseJWKS.
func PublicJWKS(kr *PrivateKeyring) JWKS {
	out := JWKS{Keys: []JWK{}}
	if kr == nil {
		return out
	}
	for i := range kr.Keys {
		k := kr.Keys[i]
		// An empty State is treated as active for projection so a minimal keyring
		// (state omitted) still publishes its keys; validation flags an explicitly
		// invalid state separately.
		state := k.State
		if state == "" {
			state = KeyStateActive
		}
		if !state.Publishable() {
			continue
		}
		if k.Kty == string(KeyTypeOct) {
			continue
		}
		out.Keys = append(out.Keys, k.Public())
	}
	return out
}

// KeyType is a JWK key type. JWK.Kty and PrivateJWK.Kty are plain strings (to
// stay byte-compatible with go/framework/security.JWK), but the validator checks
// them against this closed set.
type KeyType string

// KeyType values.
const (
	KeyTypeRSA KeyType = "RSA"
	KeyTypeEC  KeyType = "EC"
	KeyTypeOct KeyType = "oct"
)

// Valid reports whether t is a recognized key type.
func (t KeyType) Valid() bool {
	switch t {
	case KeyTypeRSA, KeyTypeEC, KeyTypeOct:
		return true
	default:
		return false
	}
}

// --- PHC-style digest grammar ----------------------------------------------

// DigestAlgorithm names the algorithm of a PHC-style credential digest. The enum
// is intentionally closed; adding a value bumps DigestVersion semantics.
type DigestAlgorithm string

// DigestAlgorithm values.
const (
	// DigestPBKDF2SHA256 is PBKDF2 with HMAC-SHA256. Its grammar carries an
	// iteration count: $pbkdf2-sha256$v=1$i=<iterations>$<b64-salt>$<b64-hash>.
	DigestPBKDF2SHA256 DigestAlgorithm = "pbkdf2-sha256"
	// DigestHMACSHA256 is a keyed HMAC-SHA256 digest. It carries no work factor:
	// $hmac-sha256$v=1$<b64-salt>$<b64-hash>.
	DigestHMACSHA256 DigestAlgorithm = "hmac-sha256"
)

// Valid reports whether a is a recognized digest algorithm.
func (a DigestAlgorithm) Valid() bool {
	switch a {
	case DigestPBKDF2SHA256, DigestHMACSHA256:
		return true
	default:
		return false
	}
}

// Digest is a parsed PHC-style credential digest. Salt and Hash are the RAW
// decoded bytes; the wire form base64-encodes them with standard RFC 4648 §4
// base64 WITHOUT padding, per the PHC string format. Iterations is meaningful
// only for pbkdf2-sha256 (zero for hmac-sha256).
type Digest struct {
	// Algorithm is the digest's key-derivation algorithm (closed enum).
	Algorithm DigestAlgorithm
	// Version is the PHC "v=" format version for Algorithm.
	Version int
	// Iterations is the PBKDF2 work factor; meaningful only for pbkdf2-sha256
	// (zero for hmac-sha256).
	Iterations int
	// Salt is the raw (decoded) per-digest salt; base64-encoded on the wire.
	Salt []byte
	// Hash is the raw (decoded) derived output; base64-encoded on the wire.
	Hash []byte
}

// String renders the canonical PHC wire form of the digest. It is deterministic
// (fixed field order, RawStdEncoding), giving a round-trip: for any string s
// that ParseDigest accepts, ParseDigest(s).String() == s. An unknown algorithm
// renders the empty string; construct digests through ParseDigest or validate
// with ValidateDigest before encoding.
func (d Digest) String() string {
	salt := base64.RawStdEncoding.EncodeToString(d.Salt)
	hash := base64.RawStdEncoding.EncodeToString(d.Hash)
	switch d.Algorithm {
	case DigestPBKDF2SHA256:
		return fmt.Sprintf("$%s$v=%d$i=%d$%s$%s", d.Algorithm, d.Version, d.Iterations, salt, hash)
	case DigestHMACSHA256:
		return fmt.Sprintf("$%s$v=%d$%s$%s", d.Algorithm, d.Version, salt, hash)
	default:
		return ""
	}
}

// AllKeyStates returns every key state in canonical order. It exists so the
// conformance corpus and drift tests can enumerate the closed enum without
// hard-coding it.
func AllKeyStates() []KeyState {
	return []KeyState{KeyStateActive, KeyStateRetiring, KeyStateRevoked, KeyStateExpired}
}
