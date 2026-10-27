package proof

import (
	"strings"
	"testing"

	"go.putnami.dev/protocol/capabilities"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

// findCode returns true if diags contains a diagnostic with the given code.
func findCode(diags []diag.Diagnostic, code string) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

func TestParseManifest_UnknownField(t *testing.T) {
	spectest.Proves(t, feature, "the-reader-is-strict-and-versioned", "an-undeclared-member-is-refused-with-no-manifest")
	data := []byte(`{"protocolVersion": 1, "project": "p", "bogus": true}`)
	m, diags := capabilities.ParseManifest(data)
	if m != nil {
		t.Error("manifest should be nil on parse error")
	}
	if !findCode(diags, capabilities.ErrorCodeUnknownField) {
		t.Errorf("want %s, got %v", capabilities.ErrorCodeUnknownField, diags)
	}
}

func TestParseManifest_InvalidJSON(t *testing.T) {
	spectest.Proves(t, feature, "the-reader-is-strict-and-versioned", "input-that-is-not-json-is-refused")
	_, diags := capabilities.ParseManifest([]byte("{not json"))
	if !diag.HasErrors(diags) {
		t.Fatal("expected parse error")
	}
	if diags[0].Code != capabilities.ErrorCodeParseError {
		t.Errorf("Code = %q, want %s", diags[0].Code, capabilities.ErrorCodeParseError)
	}
}

func TestValidateManifest_MissingRequiredProvider(t *testing.T) {
	spectest.Proves(t, feature, "a-required-capability-needs-a-provider-in-its-manifest", "a-missing-required-provider-is-reported-at-its-requirement")
	prov := capabilities.Provenance{Project: "p", Package: "example/sql", SourceKind: capabilities.SourceKindFramework, EvidencePath: "src/sql.go"}
	m := &capabilities.Manifest{
		ProtocolVersion: 1,
		Project:         "p",
		Migrations: []capabilities.MigrationBundle{{
			Name: "sql", Datasource: "primary", Provenance: prov,
		}},
		RequiredCapabilities: []capabilities.RequiredCapability{{
			Name: "sql", Requires: []capabilities.CapabilityKind{
				capabilities.CapabilityKindDatasource,
				capabilities.CapabilityKindMigration,
				capabilities.CapabilityKindReadiness,
			}, Provenance: prov,
		}},
	}
	diags := capabilities.ValidateManifest(m)
	if !findCode(diags, capabilities.ErrorCodeMissingRequiredProvider) {
		t.Fatalf("want %s, got %v", capabilities.ErrorCodeMissingRequiredProvider, diags)
	}
	for _, d := range diags {
		if d.Code != capabilities.ErrorCodeMissingRequiredProvider {
			continue
		}
		if d.Field != "requiredCapabilities[0].requires[2]" {
			t.Errorf("Field = %q, want requiredCapabilities[0].requires[2]", d.Field)
		}
		for _, context := range []string{`project "p"`, "src/sql.go", `contribute a "readiness" provider`} {
			if !strings.Contains(d.Message, context) {
				t.Errorf("Message = %q, want context %q", d.Message, context)
			}
		}
	}
}

func TestValidateManifest_DuplicateAndConflictingProviders(t *testing.T) {
	spectest.Proves(t, feature, "a-provider-identity-is-unique", "duplicate-and-conflicting-providers-are-reported")
	first := capabilities.Provenance{Project: "p", Package: "example/first", SourceKind: capabilities.SourceKindFramework}
	second := capabilities.Provenance{Project: "p", Package: "example/second", SourceKind: capabilities.SourceKindFramework}
	m := &capabilities.Manifest{
		ProtocolVersion: 1,
		Project:         "p",
		HealthContributors: []capabilities.HealthContributor{
			{Name: "db", Probe: capabilities.ProbeKindReadiness, Provenance: first},
			{Name: "db", Probe: capabilities.ProbeKindReadiness, Provenance: second},
		},
		ConfigDefinitions: []capabilities.ConfigDefinition{
			{Path: "iam", Fields: []capabilities.ConfigField{{Name: "primary"}}, Provenance: first},
			{Path: "iam", Fields: []capabilities.ConfigField{{Name: "replica"}}, Provenance: second},
		},
	}
	diags := capabilities.ValidateManifest(m)
	if !findCode(diags, capabilities.ErrorCodeDuplicateProvider) {
		t.Errorf("want %s, got %v", capabilities.ErrorCodeDuplicateProvider, diags)
	}
	if !findCode(diags, capabilities.ErrorCodeConflictingProvider) {
		t.Errorf("want %s, got %v", capabilities.ErrorCodeConflictingProvider, diags)
	}
}

func TestValidateManifest_InfraIdentityIncludesKind(t *testing.T) {
	spectest.Proves(t, feature, "a-provider-identity-is-unique", "an-infra-provider-is-keyed-by-kind-and-name")
	first := capabilities.Provenance{Project: "p", Package: "example/first", SourceKind: capabilities.SourceKindFramework}
	second := capabilities.Provenance{Project: "p", Package: "example/second", SourceKind: capabilities.SourceKindFramework}
	crossKind := &capabilities.Manifest{
		ProtocolVersion: 1,
		Project:         "p",
		InfraRequirements: []capabilities.InfraRequirement{
			{Name: "primary", Kind: capabilities.InfraKindDatabase, Provenance: first},
			{Name: "primary", Kind: capabilities.InfraKindSecret, Provenance: second},
		},
	}
	if diags := capabilities.ValidateManifest(crossKind); diag.HasErrors(diags) {
		t.Fatalf("same infra name across kinds is valid: %v", diags)
	}

	duplicate := *crossKind
	duplicate.InfraRequirements = append([]capabilities.InfraRequirement(nil), crossKind.InfraRequirements...)
	duplicate.InfraRequirements[1].Kind = capabilities.InfraKindDatabase
	if !findCode(capabilities.ValidateManifest(&duplicate), capabilities.ErrorCodeDuplicateProvider) {
		t.Fatal("same infra (kind, name) must be rejected as duplicate")
	}
}
