package security

import (
	"crypto"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"

	"go.putnami.dev/protocol/keyring"
)

// This file implements the JWS SIGNING primitives — the exact inverse of the
// verify path in jwt.go (verifyRSA256, verifyECDSA, parseJWTHeader). A token
// produced here must verify through the existing validateJWKS/verifyECDSA/
// verifyRSA256 path unchanged, so the header canonicalization, the base64url
// signing input, and the ECDSA R||S signature width all match what those
// functions expect. It performs no key management (see keyring.go); it only
// turns a signing input + a Go crypto private key into a compact JWS signature,
// projects a public key to a JOSE JWK, and parses a private JWK back to a Go key.

// JWS signature algorithm identifiers (the JOSE "alg" header values this signer
// emits). ES256 is the DEFAULT; RS256 is supported. They are the inverse of the
// alg dispatch in validateJWKS.
const (
	// AlgES256 is ECDSA using P-256 and SHA-256 (RFC 7518 §3.4). The signature is
	// the fixed-width R||S form verifyECDSA expects.
	AlgES256 = "ES256"
	// AlgRS256 is RSASSA-PKCS1-v1_5 using SHA-256 (RFC 7518 §3.3), the inverse of
	// verifyRSA256.
	AlgRS256 = "RS256"
)

// p256SigComponentLen is the fixed byte width of each of the R and S components
// in an ES256 JWS signature (RFC 7518 §3.4): the P-256 field size, 32 bytes. The
// full signature is exactly 2×this, the layout verifyECDSA(..., 32) decodes.
const p256SigComponentLen = 32

// joseHeader is the JWS Protected Header this signer emits. The field order
// (alg, typ, kid) is fixed by struct declaration order so the serialized header
// is deterministic for a given key; the verify path re-parses the header by
// field name (parseJWTHeader), so the order never affects verification.
type joseHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

// b64u encodes bytes as unpadded base64url (RFC 4648 §5) — the JOSE canonical
// encoding and the exact inverse of base64URLDecode on the verify side.
func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// buildSigningInput marshals the JOSE header for alg/kid and returns the JWS
// signing input base64url(header)."."base64url(payload) — the exact bytes the
// verify path reconstructs as parts[0]+"."+parts[1] before checking the
// signature. Centralizing header canonicalization here keeps sign and verify in
// lock-step.
func buildSigningInput(alg, kid string, payload []byte) (string, error) {
	hdr := joseHeader{Alg: alg, Typ: "JWT", Kid: kid}
	hdrJSON, err := json.Marshal(hdr)
	if err != nil {
		return "", fmt.Errorf("security: marshal JOSE header: %w", err)
	}
	return b64u(hdrJSON) + "." + b64u(payload), nil
}

// signES256 signs the JWS signing input with an ECDSA P-256 private key and
// returns the signature in the R||S FIXED-WIDTH form of RFC 7518 §3.4 — the
// exact 64-byte layout verifyECDSA(..., crypto.SHA256, 32) decodes (32-byte
// big-endian R followed by 32-byte big-endian S). big.Int.FillBytes zero-pads
// each component to the curve field width, so a short R or S is never truncated
// or left unpadded. ECDSA signing is randomized; do not expect byte-stable
// output — verify, never byte-compare.
func signES256(signingInput string, key *ecdsa.PrivateKey) ([]byte, error) {
	if key == nil {
		return nil, fmt.Errorf("security: ES256 sign: nil key")
	}
	if key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("security: ES256 requires a P-256 key, got %s", key.Curve.Params().Name)
	}
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return nil, fmt.Errorf("security: ES256 sign: %w", err)
	}
	sig := make([]byte, 2*p256SigComponentLen)
	r.FillBytes(sig[:p256SigComponentLen])
	s.FillBytes(sig[p256SigComponentLen:])
	return sig, nil
}

// signRS256 signs the JWS signing input with RSASSA-PKCS1-v1_5 and SHA-256 — the
// exact inverse of verifyRSA256.
func signRS256(signingInput string, key *rsa.PrivateKey) ([]byte, error) {
	if key == nil {
		return nil, fmt.Errorf("security: RS256 sign: nil key")
	}
	h := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
	if err != nil {
		return nil, fmt.Errorf("security: RS256 sign: %w", err)
	}
	return sig, nil
}

// --- Public-key → JOSE JWK projection --------------------------------------

// curveName maps a Go elliptic.Curve to its JOSE "crv" name.
func curveName(c elliptic.Curve) string {
	switch c {
	case elliptic.P256():
		return "P-256"
	case elliptic.P384():
		return "P-384"
	default:
		return c.Params().Name
	}
}

// ecPublicParams returns the base64url-encoded EC public coordinates (x, y),
// each left-padded to the curve field width per RFC 7518 §6.2.1 so
// ECDSAPublicKey reconstructs the exact point.
func ecPublicParams(pub *ecdsa.PublicKey) (x, y string) {
	size := (pub.Curve.Params().BitSize + 7) / 8
	xb := make([]byte, size)
	yb := make([]byte, size)
	pub.X.FillBytes(xb)
	pub.Y.FillBytes(yb)
	return b64u(xb), b64u(yb)
}

// rsaPublicParams returns the base64url-encoded RSA public parameters (n, e) per
// RFC 7518 §6.3.1.
func rsaPublicParams(pub *rsa.PublicKey) (n, e string) {
	eBytes := big.NewInt(int64(pub.E)).Bytes()
	return b64u(pub.N.Bytes()), b64u(eBytes)
}

// --- Private JWK → Go crypto key parse (store load path) -------------------

// parseECPrivateKey reconstructs a P-256 *ecdsa.PrivateKey from a private JWK
// carrying d (the private scalar). The public point is DERIVED from d via the
// curve base multiplication, so a caller cannot smuggle in a mismatched public
// key; when x/y are also present they must match the derived point, or the JWK
// is rejected.
func parseECPrivateKey(k keyring.PrivateJWK) (*ecdsa.PrivateKey, error) {
	if k.Crv != "P-256" {
		return nil, fmt.Errorf("security: EC key %q must be P-256, got %q", k.Kid, k.Crv)
	}
	if k.D == "" {
		return nil, fmt.Errorf("security: EC key %q missing private scalar d", k.Kid)
	}
	dBytes, err := base64URLDecode(k.D)
	if err != nil {
		return nil, fmt.Errorf("security: EC key %q decode d: %w", k.Kid, err)
	}
	// Derive and validate the public point from d via crypto/ecdh (the
	// non-deprecated path); NewPrivateKey also enforces 0 < d < N.
	x, y, err := deriveP256Public(dBytes)
	if err != nil {
		return nil, fmt.Errorf("security: EC key %q invalid private scalar: %w", k.Kid, err)
	}
	// When the JWK also carries x/y they must match the derived point, so a caller
	// cannot pair a private scalar with a foreign public key.
	if k.X != "" && k.Y != "" {
		gotX, xerr := base64URLDecode(k.X)
		gotY, yerr := base64URLDecode(k.Y)
		if xerr != nil || yerr != nil {
			return nil, fmt.Errorf("security: EC key %q decode x/y", k.Kid)
		}
		if new(big.Int).SetBytes(gotX).Cmp(x) != 0 || new(big.Int).SetBytes(gotY).Cmp(y) != 0 {
			return nil, fmt.Errorf("security: EC key %q public point does not match private scalar", k.Kid)
		}
	}
	priv := new(ecdsa.PrivateKey)
	priv.Curve = elliptic.P256()
	priv.D = new(big.Int).SetBytes(dBytes)
	priv.PublicKey.Curve = elliptic.P256()
	priv.PublicKey.X = x
	priv.PublicKey.Y = y
	return priv, nil
}

// deriveP256Public returns the public coordinates for a P-256 private scalar
// using crypto/ecdh (not the deprecated Curve.ScalarBaseMult). It left-pads the
// scalar to the field width and lets NewPrivateKey enforce 0 < d < N.
func deriveP256Public(dBytes []byte) (x, y *big.Int, err error) {
	const fieldLen = 32
	if len(dBytes) == 0 || len(dBytes) > fieldLen {
		return nil, nil, fmt.Errorf("private scalar has invalid length %d", len(dBytes))
	}
	padded := make([]byte, fieldLen)
	copy(padded[fieldLen-len(dBytes):], dBytes)
	ek, err := ecdh.P256().NewPrivateKey(padded)
	if err != nil {
		return nil, nil, err
	}
	pub := ek.PublicKey().Bytes() // uncompressed SEC1: 0x04 || X(32) || Y(32)
	if len(pub) != 1+2*fieldLen || pub[0] != 0x04 {
		return nil, nil, fmt.Errorf("unexpected ECDH public key encoding")
	}
	return new(big.Int).SetBytes(pub[1 : 1+fieldLen]), new(big.Int).SetBytes(pub[1+fieldLen:]), nil
}

// parseRSAPrivateKey reconstructs an *rsa.PrivateKey from a private JWK carrying
// n, e, d, p, q. Precompute derives the CRT values and Validate rejects an
// inconsistent key, so a corrupt private JWK cannot yield a signing key that
// produces unverifiable signatures.
func parseRSAPrivateKey(k keyring.PrivateJWK) (*rsa.PrivateKey, error) {
	if k.N == "" || k.E == "" || k.D == "" || k.P == "" || k.Q == "" {
		return nil, fmt.Errorf("security: RSA key %q missing a required parameter (n,e,d,p,q)", k.Kid)
	}
	nBytes, err := base64URLDecode(k.N)
	if err != nil {
		return nil, fmt.Errorf("security: RSA key %q decode n: %w", k.Kid, err)
	}
	eBytes, err := base64URLDecode(k.E)
	if err != nil {
		return nil, fmt.Errorf("security: RSA key %q decode e: %w", k.Kid, err)
	}
	dBytes, err := base64URLDecode(k.D)
	if err != nil {
		return nil, fmt.Errorf("security: RSA key %q decode d: %w", k.Kid, err)
	}
	pBytes, err := base64URLDecode(k.P)
	if err != nil {
		return nil, fmt.Errorf("security: RSA key %q decode p: %w", k.Kid, err)
	}
	qBytes, err := base64URLDecode(k.Q)
	if err != nil {
		return nil, fmt.Errorf("security: RSA key %q decode q: %w", k.Kid, err)
	}
	e := new(big.Int).SetBytes(eBytes)
	if !e.IsInt64() {
		return nil, fmt.Errorf("security: RSA key %q exponent too large", k.Kid)
	}
	priv := &rsa.PrivateKey{
		PublicKey: rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: int(e.Int64())},
		D:         new(big.Int).SetBytes(dBytes),
		Primes:    []*big.Int{new(big.Int).SetBytes(pBytes), new(big.Int).SetBytes(qBytes)},
	}
	priv.Precompute()
	if err := priv.Validate(); err != nil {
		return nil, fmt.Errorf("security: RSA key %q failed validation: %w", k.Kid, err)
	}
	return priv, nil
}
