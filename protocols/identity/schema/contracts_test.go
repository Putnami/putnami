package identity_test

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	contracts "go.putnami.dev/protocol/contracts"
	diag "go.putnami.dev/protocol/diagnostic"
)

// loadManifest strict-parses and validates the committed manifest, failing the
// test on any error diagnostic.
func loadManifest(t *testing.T) (*contracts.Manifest, []byte) {
	t.Helper()
	data, err := os.ReadFile(contracts.ManifestFilename)
	if err != nil {
		t.Fatalf("read committed manifest: %v", err)
	}
	m, diags := contracts.ParseAndValidateManifest(data)
	if m == nil || diag.HasErrors(diags) {
		t.Fatalf("committed manifest failed strict parse + validation: %v", diags)
	}
	return m, data
}

// TestManifestIdentity pins the contract identity and the protocol version the
// manifest is authored against.
func TestManifestIdentity(t *testing.T) {
	m, _ := loadManifest(t)
	if m.Name != "go.putnami.dev/protocol/identity" {
		t.Errorf("manifest name = %q, want %q", m.Name, "go.putnami.dev/protocol/identity")
	}
	if m.ProtocolVersion != contracts.ProtocolVersion {
		t.Errorf("manifest protocolVersion = %d, want %d", m.ProtocolVersion, contracts.ProtocolVersion)
	}
}

// TestManifestVocabulary pins the claims and principal kinds the frameworks
// actually consume, so a vocabulary change is a deliberate, reviewed edit.
func TestManifestVocabulary(t *testing.T) {
	m, _ := loadManifest(t)

	wantClaims := []string{"sub", "iss", "client_id", "roles", "scope", "aud", "exp"}
	if len(m.Claims) != len(wantClaims) {
		t.Fatalf("manifest declares %d claims, want %d", len(m.Claims), len(wantClaims))
	}
	for i, want := range wantClaims {
		if m.Claims[i].Name != want {
			t.Errorf("claims[%d] = %q, want %q", i, m.Claims[i].Name, want)
		}
	}

	wantKinds := []string{"user", "apikey"}
	if len(m.PrincipalKinds) != len(wantKinds) {
		t.Fatalf("manifest declares %d principal kinds, want %d", len(m.PrincipalKinds), len(wantKinds))
	}
	for i, want := range wantKinds {
		if m.PrincipalKinds[i].Name != want {
			t.Errorf("principalKinds[%d] = %q, want %q", i, m.PrincipalKinds[i].Name, want)
		}
	}

	// The enums carry the adoptable name constants the Go framework auth code
	// consumes; the generated ClaimName*/PrincipalKind* constants must keep the
	// same constant name -> wire value mapping the hand-written literals used, so a
	// rename or a wire-value change is a deliberate, reviewed edit.
	wantEnums := []struct {
		name   string
		values [][2]string // {constant name, wire value}
	}{
		{
			name: "ClaimName",
			values: [][2]string{
				{"Sub", "sub"},
				{"Iss", "iss"},
				{"ClientId", "client_id"},
				{"Roles", "roles"},
				{"Scope", "scope"},
				{"Aud", "aud"},
				{"Exp", "exp"},
			},
		},
		{
			name: "PrincipalKind",
			values: [][2]string{
				{"User", "user"},
				{"ApiKey", "apikey"},
			},
		},
		// The authorization-decision vocabulary the security middleware records
		// (allow / deny_*). The generated AuthDecision* constants are the single
		// source aliased by go/framework/security observe.go and mirrored by the
		// TypeScript framework, so a rename or wire-value change is caught here.
		{
			name: "AuthDecision",
			values: [][2]string{
				{"Allow", "allow"},
				{"DenyUnauthenticated", "deny_unauthenticated"},
				{"DenyClient", "deny_client"},
				{"DenyScope", "deny_scope"},
				{"DenyRole", "deny_role"},
				{"DenyGuard", "deny_guard"},
			},
		},
	}
	if len(m.Enums) != len(wantEnums) {
		t.Fatalf("manifest declares %d enums, want %d", len(m.Enums), len(wantEnums))
	}
	for i, want := range wantEnums {
		got := m.Enums[i]
		if got.Name != want.name {
			t.Errorf("enums[%d].name = %q, want %q", i, got.Name, want.name)
		}
		if len(got.Values) != len(want.values) {
			t.Errorf("enum %q declares %d values, want %d", want.name, len(got.Values), len(want.values))
			continue
		}
		for j, wv := range want.values {
			if got.Values[j].Name != wv[0] || got.Values[j].Value != wv[1] {
				t.Errorf("enum %q values[%d] = (%q -> %q), want (%q -> %q)",
					want.name, j, got.Values[j].Name, got.Values[j].Value, wv[0], wv[1])
			}
		}
	}

	// The framework hard-codes no scopes or grants; declaring any here would
	// fabricate issuer-side vocabulary that lives outside this repository.
	if len(m.Scopes) != 0 || len(m.Capabilities) != 0 || len(m.Grants) != 0 {
		t.Errorf("manifest declares scopes/capabilities/grants (%d/%d/%d); the framework is a token consumer and hard-codes none",
			len(m.Scopes), len(m.Capabilities), len(m.Grants))
	}
}

// TestCommittedArtifactsAreFresh regenerates every committed artifact in memory
// and asserts byte-identity, mirroring `putnami contracts check` in the regular
// test gate: the canonical IR, Go type twin, JSON Schema, and generated docs.
func TestCommittedArtifactsAreFresh(t *testing.T) {
	m, committed := loadManifest(t)

	canonical, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("canonicalize manifest: %v", err)
	}
	canonical = append(canonical, '\n')
	if !bytes.Equal(committed, canonical) {
		t.Errorf("%s is not in canonical form; run `putnami contracts generate --project /protocols/identity`", contracts.ManifestFilename)
	}

	goSrc, err := contracts.EmitGo(m)
	if err != nil {
		t.Fatalf("emit Go twin: %v", err)
	}
	assertArtifact(t, "contracts.gen.go", []byte(goSrc))

	schema, err := contracts.MarshalJSONSchema(m)
	if err != nil {
		t.Fatalf("render JSON Schema: %v", err)
	}
	assertArtifact(t, "contracts.schema.json", schema)
	assertArtifact(t, "contracts.md", contracts.RenderMarkdown(m))
}

// assertArtifact fails the test when the committed artifact at path differs
// from a fresh generation.
func assertArtifact(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read committed artifact %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("committed artifact %s is stale; run `putnami contracts generate --project /protocols/identity`", path)
	}
}
