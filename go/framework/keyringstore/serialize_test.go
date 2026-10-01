package keyringstore

import (
	"go.putnami.dev/protocol/features/spectest"

	"encoding/json"
	"testing"

	"go.putnami.dev/protocol/keyring"
	"go.putnami.dev/protocol/transaction"
)

// TestRowJWKRoundTrip pins the keyring↔row (de)serialization: a private JWK
// carrying full private material survives rowFromJWK → jwkFromRow unchanged, and
// the private fields are NOT dropped (the row is the owner document). This is
// the DB-free core the Save/Load paths reuse.
func TestRowJWKRoundTrip(t *testing.T) {
	spectest.Proves(t, "go/signing-key-rotation", "durable-owner-document", "the-row-mapping-round-trips-the-full-jwk")
	orig := keyring.PrivateJWK{
		Kty:   string(keyring.KeyTypeEC),
		Kid:   "ec-1",
		Use:   "sig",
		Alg:   "ES256",
		State: keyring.KeyStateActive,
		Crv:   "P-256",
		X:     "public-x",
		Y:     "public-y",
		D:     "PRIVATE-SCALAR-d", // private material must round-trip
	}

	row, err := rowFromJWK("tenant-a", orig)
	if err != nil {
		t.Fatalf("rowFromJWK: %v", err)
	}
	if row.keyringID != "tenant-a" {
		t.Fatalf("keyringID = %q, want tenant-a", row.keyringID)
	}
	if row.kid != orig.Kid || row.alg != orig.Alg || row.kty != orig.Kty {
		t.Fatalf("row scalar columns = %+v, want kid/alg/kty from %+v", row, orig)
	}
	if row.state != keyring.KeyStateActive {
		t.Fatalf("row.state = %q, want active", row.state)
	}
	// The private field must be present in the serialized material.
	if !json.Valid([]byte(row.material)) {
		t.Fatalf("material is not valid JSON: %q", row.material)
	}

	got, err := jwkFromRow(row)
	if err != nil {
		t.Fatalf("jwkFromRow: %v", err)
	}
	if got != orig {
		t.Fatalf("round-trip mismatch:\n got  %+v\n want %+v", got, orig)
	}
	if got.D == "" {
		t.Fatal("round-trip dropped the private scalar d")
	}
}

// TestRowStateColumnIsAuthoritative proves that jwkFromRow stamps the row's
// state COLUMN over the (possibly stale) state embedded in the material JSON.
// This models a rotation that moved the key active→retiring in the state column
// while the material document — written earlier — still says active: Load must
// reflect the column.
func TestRowStateColumnIsAuthoritative(t *testing.T) {
	spectest.Proves(t, "go/signing-key-rotation", "durable-owner-document", "the-state-column-is-authoritative-over-the-material")
	// material serialized while the key was active…
	activeDoc := keyring.PrivateJWK{Kty: "EC", Kid: "ec-1", Alg: "ES256", State: keyring.KeyStateActive}
	material, err := json.Marshal(activeDoc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// …but the row's state column has since been transitioned to retiring.
	row := signingKeyRow{kid: "ec-1", state: keyring.KeyStateRetiring, alg: "ES256", kty: "EC", material: string(material)}

	got, err := jwkFromRow(row)
	if err != nil {
		t.Fatalf("jwkFromRow: %v", err)
	}
	if got.State != keyring.KeyStateRetiring {
		t.Fatalf("Load state = %q, want retiring (the column is authoritative)", got.State)
	}
}

// TestRowFromJWKDefaultsState confirms an owner document that omits state
// persists as an active key, so a minimal keyring is usable after Save/Load.
func TestRowFromJWKDefaultsState(t *testing.T) {
	row, err := rowFromJWK("k", keyring.PrivateJWK{Kty: "EC", Kid: "x"})
	if err != nil {
		t.Fatalf("rowFromJWK: %v", err)
	}
	if row.state != keyring.KeyStateActive {
		t.Fatalf("defaulted state = %q, want active", row.state)
	}
}

// TestSuccessorInstalledMapping pins the transaction.Outcome mapping the Rotate
// non-applied branch relies on: ONLY OutcomeApplied installs a successor; every
// other outcome leaves the keyring untouched. This is the DB-free assertion of
// the fail-safe taxonomy.
func TestSuccessorInstalledMapping(t *testing.T) {
	cases := []struct {
		outcome transaction.Outcome
		want    bool
	}{
		{transaction.OutcomeApplied, true},
		{transaction.OutcomeAlreadyConsumedConflict, false},
		{transaction.OutcomeNotFound, false},
		{transaction.OutcomeRetryableSerializationFailure, false},
	}
	for _, tc := range cases {
		if got := SuccessorInstalled(tc.outcome); got != tc.want {
			t.Errorf("SuccessorInstalled(%q) = %v, want %v", tc.outcome, got, tc.want)
		}
	}
}
