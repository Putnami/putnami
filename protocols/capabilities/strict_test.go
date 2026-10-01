package capabilities

import (
	"strings"
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

func TestParseManifest_UnknownField(t *testing.T) {
	data := []byte(`{"protocolVersion": 1, "project": "p", "bogus": true}`)
	m, diags := ParseManifest(data)
	if m != nil {
		t.Error("manifest should be nil on parse error")
	}
	if !findCode(diags, ErrorCodeUnknownField) {
		t.Errorf("want %s, got %v", ErrorCodeUnknownField, diags)
	}
}

func TestParseManifest_InvalidJSON(t *testing.T) {
	_, diags := ParseManifest([]byte("{not json"))
	if !diag.HasErrors(diags) {
		t.Fatal("expected parse error")
	}
	if diags[0].Code != ErrorCodeParseError {
		t.Errorf("Code = %q, want %s", diags[0].Code, ErrorCodeParseError)
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

func TestValidateManifest_MissingRequiredProvider(t *testing.T) {
	prov := Provenance{Project: "p", Package: "example/sql", SourceKind: SourceKindFramework, EvidencePath: "src/sql.go"}
	m := &Manifest{
		ProtocolVersion: 1,
		Project:         "p",
		Migrations: []MigrationBundle{{
			Name: "sql", Datasource: "primary", Provenance: prov,
		}},
		RequiredCapabilities: []RequiredCapability{{
			Name: "sql", Requires: []CapabilityKind{
				CapabilityKindDatasource,
				CapabilityKindMigration,
				CapabilityKindReadiness,
			}, Provenance: prov,
		}},
	}
	diags := ValidateManifest(m)
	if !findCode(diags, ErrorCodeMissingRequiredProvider) {
		t.Fatalf("want %s, got %v", ErrorCodeMissingRequiredProvider, diags)
	}
	for _, d := range diags {
		if d.Code != ErrorCodeMissingRequiredProvider {
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
	first := Provenance{Project: "p", Package: "example/first", SourceKind: SourceKindFramework}
	second := Provenance{Project: "p", Package: "example/second", SourceKind: SourceKindFramework}
	m := &Manifest{
		ProtocolVersion: 1,
		Project:         "p",
		HealthContributors: []HealthContributor{
			{Name: "db", Probe: ProbeKindReadiness, Provenance: first},
			{Name: "db", Probe: ProbeKindReadiness, Provenance: second},
		},
		ConfigDefinitions: []ConfigDefinition{
			{Path: "iam", Fields: []ConfigField{{Name: "primary"}}, Provenance: first},
			{Path: "iam", Fields: []ConfigField{{Name: "replica"}}, Provenance: second},
		},
	}
	diags := ValidateManifest(m)
	if !findCode(diags, ErrorCodeDuplicateProvider) {
		t.Errorf("want %s, got %v", ErrorCodeDuplicateProvider, diags)
	}
	if !findCode(diags, ErrorCodeConflictingProvider) {
		t.Errorf("want %s, got %v", ErrorCodeConflictingProvider, diags)
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

func TestValidateManifest_InfraIdentityIncludesKind(t *testing.T) {
	first := Provenance{Project: "p", Package: "example/first", SourceKind: SourceKindFramework}
	second := Provenance{Project: "p", Package: "example/second", SourceKind: SourceKindFramework}
	crossKind := &Manifest{
		ProtocolVersion: 1,
		Project:         "p",
		InfraRequirements: []InfraRequirement{
			{Name: "primary", Kind: InfraKindDatabase, Provenance: first},
			{Name: "primary", Kind: InfraKindSecret, Provenance: second},
		},
	}
	if diags := ValidateManifest(crossKind); diag.HasErrors(diags) {
		t.Fatalf("same infra name across kinds is valid: %v", diags)
	}

	duplicate := *crossKind
	duplicate.InfraRequirements = append([]InfraRequirement(nil), crossKind.InfraRequirements...)
	duplicate.InfraRequirements[1].Kind = InfraKindDatabase
	if !findCode(ValidateManifest(&duplicate), ErrorCodeDuplicateProvider) {
		t.Fatal("same infra (kind, name) must be rejected as duplicate")
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
