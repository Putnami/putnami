package contracts

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// sampleManifest builds a representative contract IR exercising every node
// kind: two enums, a tagged union with one struct-ref variant and one inline
// variant, several structs whose fields reference primitives and named types,
// config fields with typed defaults, scopes, a capability that draws on a
// scope, a grant that confers the capability, claims, principal kinds, and
// discovery metadata. It is the shared input for the byte-stability and
// canonical-form guards.
func sampleManifest() *Manifest {
	return &Manifest{
		Schema:          "https://putnami.dev/schemas/putnami-contracts.json",
		ProtocolVersion: ProtocolVersion,
		Name:            "go.putnami.dev/example/payments",
		Enums: []Enum{{
			Name:        "Currency",
			Description: "ISO 4217 currency code subset.",
			Values: []EnumValue{
				{Name: "USD", Value: "usd", Description: "United States dollar."},
				{Name: "EUR", Value: "eur"},
			},
		}},
		Unions: []Union{{
			Name:          "PaymentMethod",
			Description:   "How a payment is tendered.",
			Discriminator: "kind",
			Variants: []UnionVariant{
				{Tag: "card", Description: "A card payment.", Struct: "CardPayment"},
				{Tag: "bank", Fields: []Field{
					{Name: "accountName", Type: "string"},
					{Name: "iban", Type: "string"},
				}},
			},
		}},
		Structs: []Struct{
			{
				Name:        "Money",
				Description: "A monetary amount in a currency.",
				Fields: []Field{
					{Name: "amount", Type: "int", Description: "Amount in minor units."},
					{Name: "currency", Type: "Currency"},
				},
			},
			{
				Name: "CardPayment",
				Fields: []Field{
					{Name: "last4", Type: "string"},
					{Name: "brand", Type: "string", Optional: true},
				},
			},
			{
				Name: "Invoice",
				Fields: []Field{
					{Name: "id", Type: "string"},
					{Name: "total", Type: "Money"},
					{Name: "method", Type: "PaymentMethod"},
					{Name: "lineItems", Type: "Money", Repeated: true},
				},
			},
		},
		ConfigFields: []ConfigField{
			{Name: "endpoint", Type: "string", Description: "Payments API base URL.", Required: true, Default: "https://api.example.com"},
			{Name: "timeoutSeconds", Type: "int", Default: 30},
			{Name: "sandbox", Type: "bool", Default: true},
			{Name: "apiKey", Type: "string", Sensitive: true},
		},
		Scopes: []Scope{
			{Name: "payments:read", Description: "Read payments."},
			{Name: "payments:write", Description: "Create and modify payments."},
		},
		Capabilities: []Capability{{
			Name:        "processPayments",
			Description: "Charge and refund payments.",
			Scopes:      []string{"payments:write"},
		}},
		Grants: []Grant{{
			Name:        "paymentsAdmin",
			Description: "Full payments administration.",
			Capability:  "processPayments",
		}},
		Claims: []Claim{
			{Name: "sub", Type: "string", Description: "Subject principal.", Required: true},
			{Name: "tenantId", Type: "string"},
		},
		PrincipalKinds: []PrincipalKind{
			{Name: "user", Description: "An interactive human principal."},
			{Name: "service", Description: "A machine principal."},
		},
		Discovery: &DiscoveryMetadata{
			Title:   "Payments Contract",
			Summary: "The payments vocabulary for the example workload.",
			Version: "1.0.0",
			Tags:    []string{"payments", "billing"},
		},
	}
}

// canonical returns the canonical serialization of m: json.MarshalIndent with
// two-space indentation and a trailing newline. This is the exact byte form
// both the Go and the TypeScript emitters must reproduce.
func canonical(t *testing.T, m *Manifest) []byte {
	t.Helper()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return append(data, '\n')
}

// TestSerialization_Stable100 verifies the canonical serialization of the same
// manifest is byte-identical across 100 marshals — no map iteration or other
// nondeterminism leaks into the wire form.
func TestSerialization_Stable100(t *testing.T) {
	m := sampleManifest()
	first := canonical(t, m)
	for i := range 100 {
		if got := canonical(t, m); !bytes.Equal(got, first) {
			t.Fatalf("iteration %d: serialization changed\nfirst: %s\ngot:   %s", i, first, got)
		}
	}
}

// TestSerialization_CanonicalByteForm pins the exact canonical bytes: the
// committed fixture fixtures/valid/full.json must equal the Go serialization of
// sampleManifest(). This is the cross-language contract — the TypeScript
// emitter (Slice 3) must produce this same file byte-for-byte.
func TestSerialization_CanonicalByteForm(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("fixtures", "valid", "full.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := canonical(t, sampleManifest())
	if !bytes.Equal(got, want) {
		t.Fatalf("canonical serialization does not match fixtures/valid/full.json.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestRoundTrip_Idempotent verifies parse → marshal → parse → marshal is
// byte-stable for every valid fixture: re-serializing a parsed manifest yields
// the same bytes the second time, so the wire form is a fixed point.
func TestRoundTrip_Idempotent(t *testing.T) {
	files, err := filepath.Glob("fixtures/valid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no valid fixtures found")
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			m1, diags := ParseManifest(data)
			if m1 == nil {
				t.Fatalf("parse %s: %v", path, diags)
			}
			once := canonical(t, m1)
			m2, _ := ParseManifest(once)
			if m2 == nil {
				t.Fatalf("re-parse %s produced nil manifest", path)
			}
			twice := canonical(t, m2)
			if !bytes.Equal(once, twice) {
				t.Fatalf("%s: round-trip not idempotent\nonce:  %s\ntwice: %s", path, once, twice)
			}
		})
	}
}
