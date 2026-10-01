package keyring

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Error codes for strict parsing and validation. Tooling and adapters key off
// these — keep them in sync with ValidErrorCodes.
const (
	ErrorCodeParseError             = "keyring.parse_error"
	ErrorCodeUnknownField           = "keyring.unknown_field"
	ErrorCodeInvalidProtocolVersion = "keyring.invalid_protocol_version"

	// Key-state machine.
	ErrorCodeInvalidKeyState   = "keyring.invalid_key_state"
	ErrorCodeIllegalTransition = "keyring.illegal_transition"

	// JWK / keyring document.
	ErrorCodeMissingKeys        = "keyring.missing_keys"
	ErrorCodeInvalidKeyType     = "keyring.invalid_key_type"
	ErrorCodeMissingKeyParam    = "keyring.missing_key_param"
	ErrorCodeInvalidKeyEncoding = "keyring.invalid_key_encoding"

	// PHC digest grammar.
	ErrorCodeDigestStructure = "keyring.digest_structure"
	ErrorCodeDigestAlgorithm = "keyring.digest_algorithm"
	ErrorCodeDigestVersion   = "keyring.digest_version"
	ErrorCodeDigestParams    = "keyring.digest_params"
	ErrorCodeDigestEncoding  = "keyring.digest_encoding"
)

// ValidErrorCodes enumerates the canonical keyring-protocol error taxonomy.
var ValidErrorCodes = map[string]bool{
	ErrorCodeParseError:             true,
	ErrorCodeUnknownField:           true,
	ErrorCodeInvalidProtocolVersion: true,
	ErrorCodeInvalidKeyState:        true,
	ErrorCodeIllegalTransition:      true,
	ErrorCodeMissingKeys:            true,
	ErrorCodeInvalidKeyType:         true,
	ErrorCodeMissingKeyParam:        true,
	ErrorCodeInvalidKeyEncoding:     true,
	ErrorCodeDigestStructure:        true,
	ErrorCodeDigestAlgorithm:        true,
	ErrorCodeDigestVersion:          true,
	ErrorCodeDigestParams:           true,
	ErrorCodeDigestEncoding:         true,
}

// --- Key-state transition validation ---------------------------------------

// Transition validates a key-state change and returns a diagnostic when it is
// illegal. It is the canonical transition validator: from and to must both be
// recognized states, the edge must be in the legal-transition table, and a
// same-state edge is rejected (a transition must change state). An empty slice
// means the transition is legal.
func Transition(from, to KeyState) []diag.Diagnostic {
	if !from.Valid() {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidKeyState, "from",
			"unknown source key state %q", from)}
	}
	if !to.Valid() {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidKeyState, "to",
			"unknown target key state %q", to)}
	}
	if from == to {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeIllegalTransition, "to",
			"key state transition must change state: %q → %q is a no-op", from, to)}
	}
	if !CanTransition(from, to) {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeIllegalTransition, "to",
			"illegal key state transition %q → %q (allowed from %q: %v)", from, to, from, from.AllowedTransitions())}
	}
	return nil
}

// --- Private keyring document ----------------------------------------------

// ParsePrivateKeyring decodes a PrivateKeyring from JSON in strict mode (unknown
// fields rejected). A non-nil document is returned only when decoding produced no
// errors. Private key material is expected here (PrivateJWK carries the private
// fields), so private fields are NOT unknown fields for this shape.
func ParsePrivateKeyring(data []byte) (*PrivateKeyring, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var kr PrivateKeyring
	if err := dec.Decode(&kr); err != nil {
		return nil, []diag.Diagnostic{decodeError(err)}
	}
	return &kr, nil
}

// ParseJWKS decodes a PUBLIC JWKS from JSON in strict mode. Because the public
// JWK type has NO private-material fields, DisallowUnknownFields makes this
// parser REJECT any document that carries private material (d, p, q, dp, dq, qi,
// k) — the "private material never in public" invariant enforced from the parse
// side. A non-nil set is returned only when decoding produced no errors.
func ParseJWKS(data []byte) (*JWKS, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var jwks JWKS
	if err := dec.Decode(&jwks); err != nil {
		return nil, []diag.Diagnostic{decodeError(err)}
	}
	return &jwks, nil
}

// decodeError maps a json decode failure to the matching diagnostic code,
// distinguishing an unknown-field rejection from a generic parse error.
func decodeError(err error) diag.Diagnostic {
	msg := err.Error()
	if field, ok := strings.CutPrefix(msg, "json: unknown field "); ok {
		return diag.Errorf(ErrorCodeUnknownField, strings.Trim(field, `"`), "%s", msg)
	}
	return diag.Errorf(ErrorCodeParseError, "", "%s", msg)
}

// ValidatePrivateKeyring checks the document's structural invariants: a
// supported protocol version, at least one key, and per key a recognized key
// type, the required public parameters for that type, a recognized State when
// present, and base64url-decodable material.
func ValidatePrivateKeyring(kr *PrivateKeyring) []diag.Diagnostic {
	if kr == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "keyring is nil")}
	}
	diags := validateProtocolVersion(kr.ProtocolVersion)
	if len(kr.Keys) == 0 {
		diags = append(diags, diag.Errorf(ErrorCodeMissingKeys, "keys", "keyring requires at least one key"))
	}
	for i := range kr.Keys {
		diags = append(diags, validatePrivateJWK(i, &kr.Keys[i])...)
	}
	return diags
}

// ValidateJWKS checks a PUBLIC JWKS: a recognized key type per key (never "oct",
// which is symmetric and has no public projection), the required public
// parameters for that type, and base64url-decodable material.
func ValidateJWKS(jwks *JWKS) []diag.Diagnostic {
	if jwks == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "jwks is nil")}
	}
	var diags []diag.Diagnostic
	if len(jwks.Keys) == 0 {
		diags = append(diags, diag.Errorf(ErrorCodeMissingKeys, "keys", "jwks requires at least one key"))
	}
	for i := range jwks.Keys {
		diags = append(diags, validatePublicJWK(i, &jwks.Keys[i])...)
	}
	return diags
}

// validatePublicJWK validates a single public JWK's structure.
func validatePublicJWK(index int, k *JWK) []diag.Diagnostic {
	kt := KeyType(k.Kty)
	var diags []diag.Diagnostic
	if !kt.Valid() {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidKeyType, "keys",
			"key %d has unknown key type %q", index, k.Kty))
		return diags
	}
	if kt == KeyTypeOct {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidKeyType, "keys",
			"key %d is symmetric (oct) and cannot appear in a public JWKS", index))
		return diags
	}
	diags = append(diags, validatePublicParams(index, kt, k.Crv, k.X, k.Y, k.N, k.E)...)
	return diags
}

// validatePrivateJWK validates a single private JWK's structure.
func validatePrivateJWK(index int, k *PrivateJWK) []diag.Diagnostic {
	var diags []diag.Diagnostic
	kt := KeyType(k.Kty)
	if !kt.Valid() {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidKeyType, "keys",
			"key %d has unknown key type %q", index, k.Kty))
		return diags
	}
	if k.State != "" && !k.State.Valid() {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidKeyState, "keys",
			"key %d has unknown state %q", index, k.State))
	}
	switch kt {
	case KeyTypeOct:
		if k.K == "" {
			diags = append(diags, diag.Errorf(ErrorCodeMissingKeyParam, "keys",
				"key %d (oct) requires k", index))
		}
	default:
		diags = append(diags, validatePublicParams(index, kt, k.Crv, k.X, k.Y, k.N, k.E)...)
	}
	// Every present base64url parameter (public or private) must decode cleanly.
	// crv is a curve name, not base64, so it is excluded.
	params := []struct{ name, val string }{
		{"x", k.X}, {"y", k.Y}, {"n", k.N}, {"e", k.E},
		{"d", k.D}, {"p", k.P}, {"q", k.Q}, {"dp", k.Dp}, {"dq", k.Dq}, {"qi", k.Qi}, {"k", k.K},
	}
	for _, p := range params {
		if p.val == "" {
			continue
		}
		if !isBase64URL(p.val) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidKeyEncoding, "keys",
				"key %d parameter %q is not valid base64url", index, p.name))
		}
	}
	return diags
}

// validatePublicParams checks that a key type carries its required public
// parameters and that they decode as base64url.
func validatePublicParams(index int, kt KeyType, crv, x, y, n, e string) []diag.Diagnostic {
	var diags []diag.Diagnostic
	switch kt {
	case KeyTypeEC:
		if crv == "" {
			diags = append(diags, diag.Errorf(ErrorCodeMissingKeyParam, "keys", "key %d (EC) requires crv", index))
		}
		diags = append(diags, requireB64(index, "x", x)...)
		diags = append(diags, requireB64(index, "y", y)...)
	case KeyTypeRSA:
		diags = append(diags, requireB64(index, "n", n)...)
		diags = append(diags, requireB64(index, "e", e)...)
	}
	return diags
}

// requireB64 reports a missing or malformed required base64url parameter.
func requireB64(index int, name, val string) []diag.Diagnostic {
	if val == "" {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeMissingKeyParam, "keys",
			"key %d requires %s", index, name)}
	}
	if !isBase64URL(val) {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidKeyEncoding, "keys",
			"key %d parameter %q is not valid base64url", index, name)}
	}
	return nil
}

// isBase64URL reports whether s decodes as unpadded base64url (RFC 4648 §5), the
// JOSE canonical encoding for JWK parameters.
func isBase64URL(s string) bool {
	_, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil
}

// validateProtocolVersion checks that v is the supported keyring protocol
// version.
func validateProtocolVersion(v int) []diag.Diagnostic {
	if v != ProtocolVersion {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidProtocolVersion, "protocolVersion",
			"protocolVersion %d is not supported by this parser (want %d)", v, ProtocolVersion)}
	}
	return nil
}

// ParseAndValidatePrivateKeyring runs strict parsing followed by structural
// validation.
func ParseAndValidatePrivateKeyring(data []byte) (*PrivateKeyring, []diag.Diagnostic) {
	kr, diags := ParsePrivateKeyring(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return kr, append(diags, ValidatePrivateKeyring(kr)...)
}

// ParseAndValidateJWKS runs strict parsing followed by structural validation.
func ParseAndValidateJWKS(data []byte) (*JWKS, []diag.Diagnostic) {
	jwks, diags := ParseJWKS(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return jwks, append(diags, ValidateJWKS(jwks)...)
}

// --- PHC digest grammar ----------------------------------------------------

// ParseDigest strictly parses a PHC-style credential digest string into a
// Digest. It rejects a malformed structure, an unknown algorithm, an
// unsupported version, out-of-range or malformed parameters, and non-canonical
// base64 (RFC 4648 §4 base64 without padding), each with a specific diagnostic
// code. A non-nil Digest is returned only when parsing produced no errors.
//
// Grammar:
//
//	$pbkdf2-sha256$v=1$i=<iterations>$<b64-salt>$<b64-hash>
//	$hmac-sha256$v=1$<b64-salt>$<b64-hash>
func ParseDigest(s string) (*Digest, []diag.Diagnostic) {
	if s == "" || s[0] != '$' {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeDigestStructure, "",
			"digest must be a non-empty string beginning with '$'")}
	}
	// A leading '$' yields an empty first segment; a trailing '$' or an empty
	// interior segment is a structural error caught by the segment-count checks.
	segs := strings.Split(s, "$")
	if len(segs) < 4 {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeDigestStructure, "",
			"digest has too few '$'-separated segments")}
	}

	alg := DigestAlgorithm(segs[1])
	if !alg.Valid() {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeDigestAlgorithm, "algorithm",
			"unknown digest algorithm %q", segs[1])}
	}

	ver, ok := parseKeyedInt(segs[2], "v")
	if !ok {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeDigestParams, "version",
			"malformed version segment %q (want v=<int>)", segs[2])}
	}
	if ver != DigestVersion {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeDigestVersion, "version",
			"unsupported digest version %d (want %d)", ver, DigestVersion)}
	}

	d := &Digest{Algorithm: alg, Version: ver}
	switch alg {
	case DigestPBKDF2SHA256:
		// $pbkdf2-sha256$v=1$i=<n>$<salt>$<hash> → 6 segments incl. leading empty.
		if len(segs) != 6 {
			return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeDigestStructure, "",
				"pbkdf2-sha256 digest must have the form $pbkdf2-sha256$v=1$i=<n>$<salt>$<hash>")}
		}
		iter, ok := parseKeyedInt(segs[3], "i")
		if !ok {
			return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeDigestParams, "iterations",
				"malformed iterations segment %q (want i=<int>)", segs[3])}
		}
		if iter < MinIterations || iter > MaxIterations {
			return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeDigestParams, "iterations",
				"iterations %d out of range [%d, %d]", iter, MinIterations, MaxIterations)}
		}
		d.Iterations = iter
		salt, hash, saltHashDiags := decodeSaltHash(segs[4], segs[5])
		if len(saltHashDiags) > 0 {
			return nil, saltHashDiags
		}
		d.Salt, d.Hash = salt, hash
	case DigestHMACSHA256:
		// $hmac-sha256$v=1$<salt>$<hash> → 5 segments incl. leading empty.
		if len(segs) != 5 {
			return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeDigestStructure, "",
				"hmac-sha256 digest must have the form $hmac-sha256$v=1$<salt>$<hash>")}
		}
		salt, hash, saltHashDiags := decodeSaltHash(segs[3], segs[4])
		if len(saltHashDiags) > 0 {
			return nil, saltHashDiags
		}
		d.Salt, d.Hash = salt, hash
	}
	return d, nil
}

// ValidateDigest checks a programmatically-constructed Digest's structural
// invariants: a recognized algorithm, the supported version, a positive
// in-range iteration count for pbkdf2 (and no iterations for hmac), and a
// non-empty salt and hash. It lets a caller validate a Digest before Encoding.
func ValidateDigest(d *Digest) []diag.Diagnostic {
	if d == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeDigestStructure, "", "digest is nil")}
	}
	var diags []diag.Diagnostic
	if !d.Algorithm.Valid() {
		diags = append(diags, diag.Errorf(ErrorCodeDigestAlgorithm, "algorithm",
			"unknown digest algorithm %q", d.Algorithm))
	}
	if d.Version != DigestVersion {
		diags = append(diags, diag.Errorf(ErrorCodeDigestVersion, "version",
			"unsupported digest version %d (want %d)", d.Version, DigestVersion))
	}
	switch d.Algorithm {
	case DigestPBKDF2SHA256:
		if d.Iterations < MinIterations || d.Iterations > MaxIterations {
			diags = append(diags, diag.Errorf(ErrorCodeDigestParams, "iterations",
				"iterations %d out of range [%d, %d]", d.Iterations, MinIterations, MaxIterations))
		}
	case DigestHMACSHA256:
		if d.Iterations != 0 {
			diags = append(diags, diag.Errorf(ErrorCodeDigestParams, "iterations",
				"hmac-sha256 carries no work factor but iterations=%d", d.Iterations))
		}
	}
	if len(d.Salt) == 0 {
		diags = append(diags, diag.Errorf(ErrorCodeDigestParams, "salt", "digest salt must be non-empty"))
	}
	if len(d.Hash) == 0 {
		diags = append(diags, diag.Errorf(ErrorCodeDigestParams, "hash", "digest hash must be non-empty"))
	}
	return diags
}

// ParseAndValidateDigest runs strict parsing followed by structural validation.
func ParseAndValidateDigest(s string) (*Digest, []diag.Diagnostic) {
	d, diags := ParseDigest(s)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return d, append(diags, ValidateDigest(d)...)
}

// parseKeyedInt parses a "<key>=<int>" segment, returning the integer and
// whether the segment was well-formed with the expected key. It rejects leading
// '+', surrounding whitespace, and non-decimal digits so the parse is canonical.
func parseKeyedInt(seg, key string) (int, bool) {
	rest, ok := strings.CutPrefix(seg, key+"=")
	if !ok {
		return 0, false
	}
	// strconv.Atoi accepts a leading '+' and '-'; reject them so only a canonical
	// unsigned decimal parses.
	if rest == "" || rest[0] == '+' || rest[0] == '-' {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return n, true
}

// decodeSaltHash decodes the salt and hash segments as unpadded standard base64
// (RFC 4648 §4), rejecting a malformed encoding or an empty salt/hash.
func decodeSaltHash(saltSeg, hashSeg string) (salt, hash []byte, diags []diag.Diagnostic) {
	salt, err := base64.RawStdEncoding.DecodeString(saltSeg)
	if err != nil {
		return nil, nil, []diag.Diagnostic{diag.Errorf(ErrorCodeDigestEncoding, "salt",
			"salt is not valid unpadded standard base64: %v", err)}
	}
	if len(salt) == 0 {
		return nil, nil, []diag.Diagnostic{diag.Errorf(ErrorCodeDigestParams, "salt", "salt must be non-empty")}
	}
	hash, err = base64.RawStdEncoding.DecodeString(hashSeg)
	if err != nil {
		return nil, nil, []diag.Diagnostic{diag.Errorf(ErrorCodeDigestEncoding, "hash",
			"hash is not valid unpadded standard base64: %v", err)}
	}
	if len(hash) == 0 {
		return nil, nil, []diag.Diagnostic{diag.Errorf(ErrorCodeDigestParams, "hash", "hash must be non-empty")}
	}
	return salt, hash, nil
}
