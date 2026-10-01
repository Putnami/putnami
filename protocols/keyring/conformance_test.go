package keyring

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// loadFixture reads and JSON-decodes a fixture file into v (lenient decode: the
// language-neutral corpus carries documentation fields the Go structs ignore).
func loadFixture(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("decode fixture %s: %v", path, err)
	}
}

// hasCode reports whether any diagnostic carries the given code.
func hasCode(diags []diag.Diagnostic, code string) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

// --- Digest grammar corpus -------------------------------------------------

type digestCorpus struct {
	Valid []struct {
		ID         string `json:"id"`
		Digest     string `json:"digest"`
		Algorithm  string `json:"algorithm"`
		Version    int    `json:"version"`
		Iterations int    `json:"iterations"`
	} `json:"valid"`
	Invalid []struct {
		ID         string `json:"id"`
		Digest     string `json:"digest"`
		Reason     string `json:"reason"`
		ExpectCode string `json:"expectCode"`
	} `json:"invalid"`
}

// TestDigestCorpus drives fixtures/digests.json: every valid digest must
// strict-parse+validate clean, expose the declared fields, and round-trip
// through String(); every invalid digest must be rejected with the specific
// diagnostic code the fixture pins (so relaxing any one parser check turns a red
// fixture green and fails here).
func TestDigestCorpus(t *testing.T) {
	var c digestCorpus
	loadFixture(t, "fixtures/digests.json", &c)
	if len(c.Valid) == 0 || len(c.Invalid) == 0 {
		t.Fatal("digest corpus must have valid and invalid cases")
	}
	for _, v := range c.Valid {
		t.Run("valid/"+v.ID, func(t *testing.T) {
			d, diags := ParseAndValidateDigest(v.Digest)
			if diag.HasErrors(diags) {
				t.Fatalf("valid digest %q rejected: %v", v.Digest, diags)
			}
			if string(d.Algorithm) != v.Algorithm {
				t.Errorf("algorithm = %q, want %q", d.Algorithm, v.Algorithm)
			}
			if d.Version != v.Version {
				t.Errorf("version = %d, want %d", d.Version, v.Version)
			}
			if d.Iterations != v.Iterations {
				t.Errorf("iterations = %d, want %d", d.Iterations, v.Iterations)
			}
			if got := d.String(); got != v.Digest {
				t.Errorf("round-trip: String() = %q, want %q", got, v.Digest)
			}
		})
	}
	for _, inv := range c.Invalid {
		t.Run("invalid/"+inv.ID, func(t *testing.T) {
			_, diags := ParseDigest(inv.Digest)
			if !diag.HasErrors(diags) {
				t.Fatalf("invalid digest %q (%s) was accepted", inv.Digest, inv.Reason)
			}
			if !hasCode(diags, inv.ExpectCode) {
				t.Errorf("invalid digest %q should emit %q, got %v", inv.Digest, inv.ExpectCode, diags)
			}
		})
	}
}

// --- Key-state machine corpus ----------------------------------------------

type keyStateCorpus struct {
	States      []string `json:"states"`
	Terminal    []string `json:"terminal"`
	Transitions []struct {
		ID    string `json:"id"`
		From  string `json:"from"`
		To    string `json:"to"`
		Legal bool   `json:"legal"`
	} `json:"transitions"`
}

// TestKeyStateCorpus drives fixtures/key-states.json: the closed enum matches
// AllKeyStates, terminal states match KeyState.Terminal, and every declared
// transition's legality matches the Transition validator.
func TestKeyStateCorpus(t *testing.T) {
	var c keyStateCorpus
	loadFixture(t, "fixtures/key-states.json", &c)

	wantStates := make([]string, 0, len(AllKeyStates()))
	for _, s := range AllKeyStates() {
		wantStates = append(wantStates, string(s))
	}
	if !reflect.DeepEqual(c.States, wantStates) {
		t.Errorf("fixture states = %v, want %v", c.States, wantStates)
	}

	terminal := map[string]bool{}
	for _, s := range c.Terminal {
		terminal[s] = true
	}
	for _, s := range AllKeyStates() {
		if got := s.Terminal(); got != terminal[string(s)] {
			t.Errorf("KeyState(%q).Terminal() = %t, fixture terminal = %t", s, got, terminal[string(s)])
		}
	}

	for _, tc := range c.Transitions {
		t.Run(tc.ID, func(t *testing.T) {
			diags := Transition(KeyState(tc.From), KeyState(tc.To))
			legal := !diag.HasErrors(diags)
			if legal != tc.Legal {
				t.Errorf("Transition(%q, %q) legal = %t, want %t (diags: %v)", tc.From, tc.To, legal, tc.Legal, diags)
			}
			// For recognized states, CanTransition must agree with Transition.
			if KeyState(tc.From).Valid() && KeyState(tc.To).Valid() {
				if got := CanTransition(KeyState(tc.From), KeyState(tc.To)); got != tc.Legal {
					t.Errorf("CanTransition(%q, %q) = %t, want %t", tc.From, tc.To, got, tc.Legal)
				}
			}
		})
	}
}

// --- Private→public keyring projection corpus ------------------------------

type keyringCorpus struct {
	Cases []struct {
		ID      string          `json:"id"`
		Private json.RawMessage `json:"private"`
		Public  json.RawMessage `json:"public"`
	} `json:"cases"`
}

// privateJWKFields is the closed set of private-material field names that must
// NEVER appear in a public JWKS projection.
var privateJWKFields = []string{"d", "p", "q", "dp", "dq", "qi", "k"}

// TestKeyringProjection drives fixtures/keyrings.json: projecting the private
// keyring must equal the fixture's expected public JWKS exactly, the projected
// output must strict-parse as a public JWKS, and it must contain none of the
// private-material field names — the structural invariant this test exists to
// pin.
func TestKeyringProjection(t *testing.T) {
	var c keyringCorpus
	loadFixture(t, "fixtures/keyrings.json", &c)
	if len(c.Cases) == 0 {
		t.Fatal("keyring corpus must have at least one case")
	}
	for _, tc := range c.Cases {
		t.Run(tc.ID, func(t *testing.T) {
			kr, diags := ParseAndValidatePrivateKeyring(tc.Private)
			if diag.HasErrors(diags) {
				t.Fatalf("private keyring rejected: %v", diags)
			}
			projected := PublicJWKS(kr)

			expected, ediags := ParseAndValidateJWKS(tc.Public)
			if diag.HasErrors(ediags) {
				t.Fatalf("expected public JWKS rejected: %v", ediags)
			}
			if !reflect.DeepEqual(projected, *expected) {
				gp, _ := json.MarshalIndent(projected, "", "  ")
				ge, _ := json.MarshalIndent(*expected, "", "  ")
				t.Fatalf("projection mismatch\n--- got ---\n%s\n--- want ---\n%s", gp, ge)
			}

			// The projected output must itself strict-parse as a public JWKS: a
			// private field would be rejected as an unknown field.
			raw, err := json.Marshal(projected)
			if err != nil {
				t.Fatal(err)
			}
			if _, pdiags := ParseAndValidateJWKS(raw); diag.HasErrors(pdiags) {
				t.Fatalf("projected JWKS does not strict-parse as public: %v", pdiags)
			}
			// Belt-and-suspenders: no private-material field name may appear as a
			// JSON key in the projected output.
			for _, field := range privateJWKFields {
				if strings.Contains(string(raw), `"`+field+`":`) {
					t.Errorf("projected JWKS leaks private field %q: %s", field, raw)
				}
			}
			if strings.Contains(string(raw), `"state":`) {
				t.Errorf("projected JWKS leaks keyring state: %s", raw)
			}
		})
	}
}

// --- JWT sign/verify vectors (structural) ----------------------------------

type jwtVector struct {
	ID        string          `json:"id"`
	Algorithm string          `json:"algorithm"`
	Kid       string          `json:"kid"`
	Token     string          `json:"token"`
	JWK       json.RawMessage `json:"jwk"`
	Reason    string          `json:"reason"`
}

type jwtCorpus struct {
	Valid   []jwtVector `json:"valid"`
	Invalid []jwtVector `json:"invalid"`
}

// structuralJWTCheck performs the checks THIS module can make without doing any
// cryptography: a token has three non-empty base64url segments, an asymmetric
// header alg, a public JWK that strict-parses (carrying no private material), and
// a header alg/kid that agree with that JWK. Cryptographic signature
// verification belongs to the framework security packages; these vectors are
// their corpus.
func structuralJWTCheck(v jwtVector) []diag.Diagnostic {
	var diags []diag.Diagnostic
	add := func(field, format string, args ...any) {
		diags = append(diags, diag.Errorf("jwt.structural", field, format, args...))
	}
	parts := strings.Split(v.Token, ".")
	if len(parts) != 3 {
		add("token", "token must have exactly 3 segments, got %d", len(parts))
		return diags
	}
	for i, p := range parts {
		if p == "" {
			add("token", "segment %d is empty", i)
			continue
		}
		if _, err := base64.RawURLEncoding.DecodeString(p); err != nil {
			add("token", "segment %d is not base64url: %v", i, err)
		}
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return diags
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(hb, &hdr); err != nil {
		add("header", "JOSE header is not JSON: %v", err)
		return diags
	}
	if hdr.Alg == "" || hdr.Alg == "none" || strings.HasPrefix(hdr.Alg, "HS") {
		add("alg", "header alg %q is not asymmetric; a public JWKS cannot verify it", hdr.Alg)
	}
	jwksDoc := append(append([]byte(`{"keys":[`), v.JWK...), []byte(`]}`)...)
	parsed, jd := ParseAndValidateJWKS(jwksDoc)
	diags = append(diags, jd...)
	if !diag.HasErrors(jd) && parsed != nil && len(parsed.Keys) == 1 {
		jwk := parsed.Keys[0]
		if jwk.Alg != hdr.Alg {
			add("alg", "public JWK alg %q does not match header alg %q", jwk.Alg, hdr.Alg)
		}
		if jwk.Kid != hdr.Kid {
			add("kid", "public JWK kid %q does not match header kid %q", jwk.Kid, hdr.Kid)
		}
	}
	return diags
}

// TestJWTVectors drives fixtures/jwt-vectors.json: every valid vector passes the
// structural checks (and its header alg matches the declared algorithm); every
// invalid vector fails at least one structural check.
func TestJWTVectors(t *testing.T) {
	var c jwtCorpus
	loadFixture(t, "fixtures/jwt-vectors.json", &c)
	if len(c.Valid) == 0 || len(c.Invalid) == 0 {
		t.Fatal("jwt corpus must have valid and invalid vectors")
	}
	// Require both asymmetric families to be represented.
	families := map[string]bool{}
	for _, v := range c.Valid {
		families[v.Algorithm] = true
	}
	for _, want := range []string{"ES256", "RS256"} {
		if !families[want] {
			t.Errorf("jwt corpus is missing a valid %s vector", want)
		}
	}
	for _, v := range c.Valid {
		t.Run("valid/"+v.ID, func(t *testing.T) {
			if diags := structuralJWTCheck(v); diag.HasErrors(diags) {
				t.Fatalf("valid vector %s failed structural checks: %v", v.ID, diags)
			}
			hdr := decodeHeaderAlg(t, v.Token)
			if hdr != v.Algorithm {
				t.Errorf("header alg %q != declared algorithm %q", hdr, v.Algorithm)
			}
		})
	}
	for _, v := range c.Invalid {
		t.Run("invalid/"+v.ID, func(t *testing.T) {
			if diags := structuralJWTCheck(v); !diag.HasErrors(diags) {
				t.Fatalf("invalid vector %s (%s) passed structural checks", v.ID, v.Reason)
			}
		})
	}
}

// decodeHeaderAlg extracts the alg from a JWT's JOSE header for the valid-path
// assertion.
func decodeHeaderAlg(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var hdr struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(hb, &hdr); err != nil {
		t.Fatalf("parse header: %v", err)
	}
	return hdr.Alg
}

// --- Error taxonomy --------------------------------------------------------

// TestErrorCodesRegistered asserts every error code the validators can emit is
// registered in ValidErrorCodes and that the registry has no extras, so the
// taxonomy stays complete.
func TestErrorCodesRegistered(t *testing.T) {
	codes := []string{
		ErrorCodeParseError,
		ErrorCodeUnknownField,
		ErrorCodeInvalidProtocolVersion,
		ErrorCodeInvalidKeyState,
		ErrorCodeIllegalTransition,
		ErrorCodeMissingKeys,
		ErrorCodeInvalidKeyType,
		ErrorCodeMissingKeyParam,
		ErrorCodeInvalidKeyEncoding,
		ErrorCodeDigestStructure,
		ErrorCodeDigestAlgorithm,
		ErrorCodeDigestVersion,
		ErrorCodeDigestParams,
		ErrorCodeDigestEncoding,
	}
	for _, code := range codes {
		if !ValidErrorCodes[code] {
			t.Errorf("error code %q is not registered in ValidErrorCodes", code)
		}
	}
	if len(codes) != len(ValidErrorCodes) {
		t.Errorf("ValidErrorCodes has %d entries, expected %d", len(ValidErrorCodes), len(codes))
	}
}
