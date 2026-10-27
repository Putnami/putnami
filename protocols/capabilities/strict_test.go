package capabilities

import (
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
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

func TestParseManifest_Valid(t *testing.T) {
	data := []byte(`{"protocolVersion": 1, "project": "p"}`)
	m, diags := ParseManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	if m == nil || m.Project != "p" {
		t.Fatalf("manifest = %v, want project p", m)
	}
}

func TestValidateManifest_InvalidProtocolVersion(t *testing.T) {
	m := &Manifest{ProtocolVersion: 99, Project: "p"}
	if !findCode(ValidateManifest(m), ErrorCodeInvalidProtocolVersion) {
		t.Errorf("want %s", ErrorCodeInvalidProtocolVersion)
	}
}

func TestValidateManifest_MissingProject(t *testing.T) {
	m := &Manifest{ProtocolVersion: 1}
	if !findCode(ValidateManifest(m), ErrorCodeMissingProject) {
		t.Errorf("want %s", ErrorCodeMissingProject)
	}
}

func TestValidateManifest_MissingProvenanceProject(t *testing.T) {
	m := &Manifest{
		ProtocolVersion: 1,
		Project:         "p",
		InfraRequirements: []InfraRequirement{{
			Name: "primary", Kind: InfraKindDatabase,
			Provenance: Provenance{SourceKind: SourceKindFramework},
		}},
	}
	if !findCode(ValidateManifest(m), ErrorCodeMissingProvenance) {
		t.Errorf("want %s", ErrorCodeMissingProvenance)
	}
}

func TestValidateManifest_InvalidSourceKind(t *testing.T) {
	m := &Manifest{
		ProtocolVersion: 1,
		Project:         "p",
		InfraRequirements: []InfraRequirement{{
			Name: "primary", Kind: InfraKindDatabase,
			Provenance: Provenance{Project: "p", SourceKind: "robot"},
		}},
	}
	if !findCode(ValidateManifest(m), ErrorCodeInvalidSourceKind) {
		t.Errorf("want %s", ErrorCodeInvalidSourceKind)
	}
}

func TestValidateManifest_InvalidEnums(t *testing.T) {
	prov := Provenance{Project: "p", SourceKind: SourceKindManual}
	cases := []struct {
		name string
		m    *Manifest
		code string
	}{
		{"schemaKind", &Manifest{ProtocolVersion: 1, Project: "p",
			Schemas: []SchemaContribution{{Name: "n", Kind: "grpc", Provenance: prov}}},
			ErrorCodeInvalidSchemaKind},
		{"discovererKind", &Manifest{ProtocolVersion: 1, Project: "p",
			Discoverers: []Discoverer{{Name: "n", Kind: "wild", Provenance: prov}}},
			ErrorCodeInvalidDiscovererKind},
		{"infraKind", &Manifest{ProtocolVersion: 1, Project: "p",
			InfraRequirements: []InfraRequirement{{Name: "n", Kind: "queue", Provenance: prov}}},
			ErrorCodeInvalidInfraKind},
		{"probe", &Manifest{ProtocolVersion: 1, Project: "p",
			HealthContributors: []HealthContributor{{Name: "n", Probe: "warmth", Provenance: prov}}},
			ErrorCodeInvalidProbe},
		{"phase", &Manifest{ProtocolVersion: 1, Project: "p",
			LifecycleHooks: []LifecycleHook{{Name: "n", Phase: "middle", Provenance: prov}}},
			ErrorCodeInvalidPhase},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !findCode(ValidateManifest(tc.m), tc.code) {
				t.Errorf("want %s, got %v", tc.code, ValidateManifest(tc.m))
			}
		})
	}
}

func TestValidateManifest_RequiredCapability(t *testing.T) {
	prov := Provenance{Project: "p", SourceKind: SourceKindManual}

	empty := &Manifest{ProtocolVersion: 1, Project: "p",
		RequiredCapabilities: []RequiredCapability{{Name: "sql", Provenance: prov}}}
	if !findCode(ValidateManifest(empty), ErrorCodeMissingRequires) {
		t.Errorf("want %s for empty requires", ErrorCodeMissingRequires)
	}

	bad := &Manifest{ProtocolVersion: 1, Project: "p",
		RequiredCapabilities: []RequiredCapability{{
			Name: "sql", Requires: []CapabilityKind{"banana"}, Provenance: prov}}}
	if !findCode(ValidateManifest(bad), ErrorCodeInvalidCapabilityKind) {
		t.Errorf("want %s for unknown capability kind", ErrorCodeInvalidCapabilityKind)
	}

	ok := &Manifest{ProtocolVersion: 1, Project: "p",
		Migrations: []MigrationBundle{{
			Name: "sql", Datasource: "primary", Provenance: prov,
		}},
		HealthContributors: []HealthContributor{{
			Name: "primary", Probe: ProbeKindReadiness, Provenance: prov,
		}},
		RequiredCapabilities: []RequiredCapability{{
			Name:       "sql",
			Requires:   []CapabilityKind{CapabilityKindDatasource, CapabilityKindMigration, CapabilityKindReadiness},
			Provenance: prov}}}
	if diag.HasErrors(ValidateManifest(ok)) {
		t.Errorf("valid sql capability produced errors: %v", ValidateManifest(ok))
	}
}

func TestValidateManifest_MigrationNameIsNotProviderIdentityInV1(t *testing.T) {
	prov := Provenance{Project: "p", SourceKind: SourceKindFramework}
	m := &Manifest{
		ProtocolVersion: 1,
		Project:         "p",
		Migrations: []MigrationBundle{
			{Name: "secrets", Datasource: "primary", Provenance: prov},
			{Name: "secrets", Datasource: "replica", Provenance: prov},
		},
	}
	for _, d := range ValidateManifest(m) {
		if d.Code == ErrorCodeDuplicateProvider || d.Code == ErrorCodeConflictingProvider {
			t.Fatalf("v1 migration names do not carry kind and cannot establish identity: %v", d)
		}
	}
}

func TestValidErrorCodes_Membership(t *testing.T) {
	required := []string{
		ErrorCodeParseError,
		ErrorCodeUnknownField,
		ErrorCodeInvalidProtocolVersion,
		ErrorCodeMissingProject,
		ErrorCodeInvalidName,
		ErrorCodeMissingProvenance,
		ErrorCodeInvalidSourceKind,
		ErrorCodeInvalidSchemaKind,
		ErrorCodeInvalidDiscovererKind,
		ErrorCodeInvalidInfraKind,
		ErrorCodeInvalidProbe,
		ErrorCodeInvalidPhase,
		ErrorCodeInvalidCapabilityKind,
		ErrorCodeMissingRequires,
		ErrorCodeMissingRequiredProvider,
		ErrorCodeDuplicateProvider,
		ErrorCodeConflictingProvider,
	}
	for _, code := range required {
		if !ValidErrorCodes[code] {
			t.Errorf("ValidErrorCodes missing %q", code)
		}
	}
}
