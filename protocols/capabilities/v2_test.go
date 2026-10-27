package capabilities

import (
	"bytes"
	"slices"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func provenanceV2(project, declaration string) ProvenanceV2 {
	return ProvenanceV2{
		Project: project, Package: "go.putnami.dev/database", Version: "1.4.0", SourceKind: SourceKindFramework,
		Declaration: DeclarationLocation{Root: LocationRootProject, Path: declaration, Symbol: "Declaration"},
		Artifacts: []ArtifactLocation{
			{Root: LocationRootProject, Path: "z/output.json", Digest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
			{Root: LocationRootProject, Path: "a/output.json", Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		},
	}
}

func identity(owner string, kind ContributionKind, subkind, key string) ContributionIdentity {
	return ContributionIdentity{OwnerProject: owner, Kind: kind, Subkind: subkind, Key: key}
}

func sampleManifestV2() *ManifestV2 {
	const owner = "go.putnami.dev/example/iam"
	p := func(file string) ProvenanceV2 { return provenanceV2(owner, file) }
	return &ManifestV2{
		Schema: "https://putnami.dev/schemas/putnami-capabilities-v2.json", ProtocolVersion: ProtocolVersionV2, Project: owner,
		ConfigDefinitions: []ConfigDefinitionV2{{Identity: identity(owner, ContributionKindConfig, "", "database.default"), Path: "database.default", Fields: []ConfigField{{Name: "url", Type: "string"}, {Name: "password", Type: "string", Sensitive: true}}, Provenance: p("config.go")}},
		Schemas:           []SchemaContributionV2{{Identity: identity(owner, ContributionKindSchema, "route", "listUsers"), Name: "listUsers", Kind: SchemaKindRoute, Path: "/api/users", Provenance: p("routes.go")}},
		Discoverers:       []DiscovererV2{{Identity: identity(owner, ContributionKindDiscoverer, "source", "sqlSources"), Name: "sqlSources", Kind: DiscovererKindSource, Provenance: p("discover.go")}},
		Migrations: []MigrationBundleV2{
			{Identity: identity(owner, ContributionKindMigration, "sql", "iam"), Name: "iam", Kind: "sql", Datasource: "default", Digest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", Provenance: p("migrations.go")},
			{Identity: identity(owner, ContributionKindMigration, "code", "iam"), Name: "iam", Kind: "code", Provenance: p("code_migrations.go")},
		},
		InfraRequirements:    []InfraRequirementV2{{Identity: identity(owner, ContributionKindInfra, "database", "primary"), Name: "primary", Kind: InfraKindDatabase, Provenance: p("infra.go")}},
		HealthContributors:   []HealthContributorV2{{Identity: identity(owner, ContributionKindHealth, "readiness", "primaryDatabase"), Name: "primaryDatabase", Probe: ProbeKindReadiness, Provenance: p("health.go")}},
		LifecycleHooks:       []LifecycleHookV2{{Identity: identity(owner, ContributionKindLifecycle, "starter", "connectionPool"), Name: "connectionPool", Phase: LifecyclePhaseStarter, Provenance: p("lifecycle.go")}},
		Packages:             []PackageV2{{Identity: identity(owner, ContributionKindPackage, "", "go.putnami.dev/database"), Package: "go.putnami.dev/database", Provenance: p("go.mod")}},
		RequiredCapabilities: []RequiredCapabilityV2{{Identity: identity(owner, ContributionKindRequiredCapability, "", "sql"), Name: "sql", Requires: []CapabilityKind{CapabilityKindReadiness, CapabilityKindMigration, CapabilityKindDatasource}, Provenance: p("requirements.go")}},
	}
}

// closurePackage builds the package contribution shape a scheduler stamp
// produces for a reachable module: the package owns its own entry and declares
// it from its own project descriptor. Such an entry is exactly what made every
// workload's committed manifest depend on the whole dependency graph.
func closurePackage(pkg string) PackageV2 {
	return PackageV2{
		Identity: identity(pkg, ContributionKindPackage, "", pkg),
		Package:  pkg,
		Provenance: ProvenanceV2{
			Project: pkg, Package: pkg, SourceKind: SourceKindFramework,
			Declaration: DeclarationLocation{Root: LocationRootProject, Path: "putnami.json"},
		},
	}
}

// TestValidationObservesClosurePackagesEmissionScopesAway keeps validation
// honest: the emission projection must not hide a malformed entry a producer
// really wrote. Validation reads the document as written.
func TestValidationObservesClosurePackagesEmissionScopesAway(t *testing.T) {
	m := sampleManifestV2()
	broken := closurePackage("go.putnami.dev/unrelated")
	broken.Provenance.Declaration.Path = "../escape"
	m.Packages = append(m.Packages, broken)

	if findings := ValidateManifestV2(m); !findCode(findings, ErrorCodePathEscape) {
		t.Fatalf("validation dropped a closure entry instead of diagnosing it: %v", findings)
	}
	if canonical := CanonicalManifestV2(m); len(canonical.Packages) != 1 {
		t.Fatalf("emission kept the closure entry: %#v", canonical.Packages)
	}
}

// TestClosurePackagesStayReferenceable pins that scoping is an EMISSION rule
// only: a reference into a manifest that still carries closure entries (an older
// committed artifact, an index snapshot) resolves as before.
func TestClosurePackagesStayReferenceable(t *testing.T) {
	m := sampleManifestV2()
	entry := closurePackage("go.putnami.dev/unrelated")
	m.Packages = append(m.Packages, entry)

	resolved, findings := ResolveContribution(entry.Identity, []ManifestContainer{
		{Path: "workload/schema/capabilities.json", Manifest: &ManifestDocument{ProtocolVersion: 2, V2: m}},
	})
	if resolved == nil || diag.HasErrors(findings) {
		t.Fatalf("closure package contribution is no longer referenceable: %#v %v", resolved, findings)
	}
}

func TestManifestV2DiagnosticsAreCanonicalAcrossShuffledInvalidEntries(t *testing.T) {
	m := sampleManifestV2()
	m.ConfigDefinitions = []ConfigDefinitionV2{
		{Identity: identity("z-owner", ContributionKindConfig, "", "wrong"), Path: "z.config", Provenance: provenanceV2("z-owner", "z.go")},
		{Identity: identity("a-owner", ContributionKindConfig, "", "a.config"), Path: "a.config", Provenance: provenanceV2("a-owner", "a.go")},
	}
	m.ConfigDefinitions[1].Provenance.LegacySourceBinding = "invalid"
	m.RequiredCapabilities[0].Requires = []CapabilityKind{CapabilityKindSchema, CapabilityKindConfig, CapabilityKindHealth}

	first := diagnosticSignatures(ValidateManifestV2(m))
	slices.Reverse(m.ConfigDefinitions)
	slices.Reverse(m.RequiredCapabilities[0].Requires)
	second := diagnosticSignatures(ValidateManifestV2(m))
	if !slices.Equal(first, second) {
		t.Fatalf("shuffled invalid diagnostics changed\nfirst:  %v\nsecond: %v", first, second)
	}
}

func diagnosticSignatures(findings []diag.Diagnostic) []string {
	out := make([]string, len(findings))
	for i, finding := range findings {
		out[i] = finding.Code + "|" + finding.Field
	}
	return out
}

func TestManifestV2PinnedDiagnostics(t *testing.T) {
	tests := []struct {
		name, code string
		mutate     func(*ManifestV2)
	}{
		{"missing identity", ErrorCodeMissingContributionIdentity, func(m *ManifestV2) { m.ConfigDefinitions[0].Identity = ContributionIdentity{} }},
		{"owner mismatch", ErrorCodeOwnerProjectMismatch, func(m *ManifestV2) { m.ConfigDefinitions[0].Identity.OwnerProject = "another" }},
		{"identity mismatch", ErrorCodeIdentityMismatch, func(m *ManifestV2) { m.ConfigDefinitions[0].Identity.Key = "another" }},
		{"missing migration kind", ErrorCodeMissingMigrationKind, func(m *ManifestV2) { m.Migrations[0].Kind = ""; m.Migrations[0].Identity.Subkind = "" }},
		{"missing declaration", ErrorCodeMissingDeclaration, func(m *ManifestV2) { m.ConfigDefinitions[0].Provenance.Declaration = DeclarationLocation{} }},
		{"invalid legacy binding", ErrorCodeInvalidSourceBinding, func(m *ManifestV2) { m.ConfigDefinitions[0].Provenance.LegacySourceBinding = "git:abc" }},
		{"path escape", ErrorCodePathEscape, func(m *ManifestV2) { m.ConfigDefinitions[0].Provenance.Declaration.Path = "../secret" }},
		{"duplicate tuple", ErrorCodeDuplicateContribution, func(m *ManifestV2) { m.Migrations[1] = m.Migrations[0] }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := sampleManifestV2()
			test.mutate(m)
			if !findCode(ValidateManifestV2(m), test.code) {
				t.Fatalf("want %s, got %v", test.code, ValidateManifestV2(m))
			}
		})
	}
}

func TestSourceBindingGoldenAndExclusions(t *testing.T) {
	records := []SourceBindingFile{
		{Path: "z.sh", Mode: SourceModeExecutable, Digest: SourceDigest([]byte("echo z\n"))},
		{Path: "a.txt", Mode: SourceModeRegular, Digest: SourceDigest([]byte("a\n"))},
		{Path: "link", Mode: SourceModeSymlink, Digest: SourceDigest([]byte("a.txt"))},
		{Path: "vendor/dependency", Mode: SourceModeGitlink, Digest: "git:1111111111111111111111111111111111111111"},
		{Path: ".gen/schema/generated.json", Mode: SourceModeRegular, Digest: SourceDigest([]byte("generated"))},
		{Path: "nested/.gen/generated.json", Mode: SourceModeRegular, Digest: SourceDigest([]byte("generated"))},
		{Path: "schema/capabilities.json", Mode: SourceModeRegular, Digest: SourceDigest([]byte("output"))},
		{Path: "nested/schema/feature-evidence/a.json", Mode: SourceModeRegular, Digest: SourceDigest([]byte("evidence"))},
	}
	want := "source-v1:sha256:33ff6e16efb726685f63c102204dacf0c36ffe449419e1fc2798470ec06c6689"
	got, err := ComputeSourceBinding(records)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("source binding = %s, want %s", got, want)
	}
	slices.Reverse(records)
	shuffled, err := ComputeSourceBinding(records)
	if err != nil || shuffled != want {
		t.Fatalf("shuffled source binding = %s (%v)", shuffled, err)
	}
	canonical, err := CanonicalSourceBindingInput(records)
	if err != nil {
		t.Fatal(err)
	}
	for _, excluded := range [][]byte{[]byte(".gen"), []byte("capabilities.json"), []byte("feature-evidence")} {
		if bytes.Contains(canonical, excluded) {
			t.Fatalf("excluded output leaked into source binding input: %s", canonical)
		}
	}
}

func TestResolveContributionOwnerCopiesAndMixedVersions(t *testing.T) {
	const owner = "go.putnami.dev/example/dependency"
	ref := identity(owner, ContributionKindConfig, "", "database.default")
	v2 := &ManifestV2{ProtocolVersion: 2, Project: owner, ConfigDefinitions: []ConfigDefinitionV2{{Identity: ref, Path: ref.Key, Provenance: provenanceV2(owner, "config.go")}}}
	copyManifest := *v2
	copyManifest.Project = "go.putnami.dev/example/workload"
	containers := []ManifestContainer{
		{Path: "workload/schema/capabilities.json", Manifest: &ManifestDocument{ProtocolVersion: 2, V2: &copyManifest}},
		{Path: "dependency/schema/capabilities.json", Manifest: &ManifestDocument{ProtocolVersion: 2, V2: v2}},
	}
	resolved, findings := ResolveContribution(ref, containers)
	if resolved == nil || diag.HasErrors(findings) {
		t.Fatalf("resolve copies: %v", findings)
	}
	if want := []string{"dependency/schema/capabilities.json", "workload/schema/capabilities.json"}; !slices.Equal(resolved.Containers, want) {
		t.Fatalf("containers = %v", resolved.Containers)
	}

	v1 := &Manifest{ProtocolVersion: 1, Project: "another-container", ConfigDefinitions: []ConfigDefinition{{Path: ref.Key, Provenance: Provenance{Project: owner, Package: "go.putnami.dev/database", Version: "1.4.0", SourceKind: SourceKindFramework, EvidencePath: "legacy.go"}}}}
	resolved, findings = ResolveContribution(ref, append(containers, ManifestContainer{Path: "legacy/schema/capabilities.json", Manifest: &ManifestDocument{ProtocolVersion: 1, V1: v1}}))
	if resolved == nil || resolved.ProtocolVersion != 2 || diag.HasErrors(findings) {
		t.Fatalf("mixed projection: %#v %v", resolved, findings)
	}

	conflict := copyManifest
	conflict.ConfigDefinitions = append([]ConfigDefinitionV2(nil), copyManifest.ConfigDefinitions...)
	conflict.ConfigDefinitions[0].Provenance.Declaration.Path = "different.go"
	if got, diagnostics := ResolveContribution(ref, []ManifestContainer{{Path: "a.json", Manifest: containers[0].Manifest}, {Path: "b.json", Manifest: &ManifestDocument{ProtocolVersion: 2, V2: &conflict}}}); got != nil || !findCode(diagnostics, ErrorCodeConflictingContributionCopy) {
		t.Fatalf("conflict: %#v %v", got, diagnostics)
	}

	migrationRef := identity(owner, ContributionKindMigration, "sql", "billing")
	v1.Migrations = []MigrationBundle{{Name: "billing", Provenance: Provenance{Project: owner, SourceKind: SourceKindFramework}}}
	if got, diagnostics := ResolveContribution(migrationRef, []ManifestContainer{{Path: "legacy.json", Manifest: &ManifestDocument{ProtocolVersion: 1, V1: v1}}}); got != nil || !findCode(diagnostics, ErrorCodeV1Unreferenceable) {
		t.Fatalf("v1 migration: %#v %v", got, diagnostics)
	}
}
