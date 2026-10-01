package keyring

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// canonical returns the canonical serialization of v: json.MarshalIndent with
// two-space indentation and a trailing newline. This is the exact byte form both
// the Go and a future TypeScript emitter must reproduce, mirroring
// protocols/transaction's determinism contract.
func canonical(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return append(data, '\n')
}

// sampleProjection loads the first keyring corpus case, strict-parses its
// private keyring, and returns the PUBLIC JWKS projection — the representative
// canonical artifact for the byte-stability guards.
func sampleProjection(t *testing.T) JWKS {
	t.Helper()
	var c keyringCorpus
	loadFixture(t, "fixtures/keyrings.json", &c)
	if len(c.Cases) == 0 {
		t.Fatal("keyring corpus is empty")
	}
	kr, diags := ParseAndValidatePrivateKeyring(c.Cases[0].Private)
	if diag.HasErrors(diags) {
		t.Fatalf("parse sample keyring: %v", diags)
	}
	return PublicJWKS(kr)
}

// TestPublicJWKS_CanonicalByteForm pins the exact canonical bytes of the public
// projection: the committed fixtures/equivalence/keyring.golden.json must equal
// the Go serialization of the sample keyring's projection. This is the
// cross-language contract — a future TypeScript projector must produce this same
// file byte-for-byte.
func TestPublicJWKS_CanonicalByteForm(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("fixtures", "equivalence", "keyring.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := canonical(t, sampleProjection(t))
	if !bytes.Equal(got, want) {
		t.Fatalf("canonical projection does not match fixtures/equivalence/keyring.golden.json.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestPublicJWKS_Stable100 verifies the canonical serialization of the same
// projection is byte-identical across 100 marshals — no nondeterminism (e.g. map
// ordering) leaks into the wire form.
func TestPublicJWKS_Stable100(t *testing.T) {
	jwks := sampleProjection(t)
	first := canonical(t, jwks)
	for i := range 100 {
		if got := canonical(t, jwks); !bytes.Equal(got, first) {
			t.Fatalf("iteration %d: serialization changed\nfirst: %s\ngot:   %s", i, first, got)
		}
	}
}

// TestDigest_RoundTripStable verifies the PHC digest wire form is a fixed point:
// for every valid fixture, parse → String reproduces the original string, and a
// second parse → String is byte-identical.
func TestDigest_RoundTripStable(t *testing.T) {
	var c digestCorpus
	loadFixture(t, "fixtures/digests.json", &c)
	for _, v := range c.Valid {
		t.Run(v.ID, func(t *testing.T) {
			d, diags := ParseDigest(v.Digest)
			if diag.HasErrors(diags) {
				t.Fatalf("parse: %v", diags)
			}
			once := d.String()
			if once != v.Digest {
				t.Fatalf("String() = %q, want %q", once, v.Digest)
			}
			d2, diags := ParseDigest(once)
			if diag.HasErrors(diags) {
				t.Fatalf("re-parse: %v", diags)
			}
			if twice := d2.String(); twice != once {
				t.Fatalf("round-trip not stable: once=%q twice=%q", once, twice)
			}
		})
	}
}

// TestAllowedTransitions_Deterministic verifies KeyState.AllowedTransitions is
// stable and sorted across repeated calls, even though the underlying table is a
// Go map (whose iteration order is not stable).
func TestAllowedTransitions_Deterministic(t *testing.T) {
	for _, s := range AllKeyStates() {
		first := s.AllowedTransitions()
		for i := 0; i < 100; i++ {
			got := s.AllowedTransitions()
			if len(got) != len(first) {
				t.Fatalf("state %q: length changed on iteration %d", s, i)
			}
			for j := range got {
				if got[j] != first[j] {
					t.Fatalf("state %q: order changed on iteration %d: %v vs %v", s, i, got, first)
				}
				if j > 0 && got[j-1] >= got[j] {
					t.Fatalf("state %q: not sorted ascending: %v", s, got)
				}
			}
		}
	}
}
