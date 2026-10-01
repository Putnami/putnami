package capabilities

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

const zeroSourceBinding = "source-v1:sha256:0000000000000000000000000000000000000000000000000000000000000000"

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

func TestManifestV2EveryKindCanonicalAndStrict(t *testing.T) {
	m := sampleManifestV2()
	if got := ValidateManifestV2(m); diag.HasErrors(got) {
		t.Fatalf("valid v2 manifest: %v", got)
	}
	first, err := MarshalManifestV2(m)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(first, []byte(`"sourceBinding"`)) {
		t.Fatalf("canonical v2 manifest embeds a volatile source binding: %s", first)
	}
	if bytes.Contains(first, []byte(`"version"`)) || bytes.Contains(first, []byte(`"packageVersions"`)) {
		t.Fatalf("canonical v2 manifest embeds volatile dependency versions: %s", first)
	}
	if !bytes.Contains(first, []byte(`"packages"`)) {
		t.Fatalf("canonical v2 manifest omitted stable package providers: %s", first)
	}
	for range 100 {
		got, err := MarshalManifestV2(m)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, first) {
			t.Fatal("v2 canonical bytes changed across repeated serialization")
		}
	}
	doc, diags := ParseAndValidateManifestDocument(first)
	if doc == nil || doc.V2 == nil || diag.HasErrors(diags) {
		t.Fatalf("strict v2 round trip: %v", diags)
	}
	again, err := MarshalManifestV2(doc.V2)
	if err != nil || !bytes.Equal(first, again) {
		t.Fatalf("v2 canonical round trip changed bytes: %v", err)
	}
	legacy := bytes.Replace(first, []byte(`"sourceKind": "framework",`), []byte(`"sourceKind": "framework", "sourceBinding": "`+zeroSourceBinding+`",`), 1)
	legacyDoc, legacyDiags := ParseAndValidateManifestDocument(legacy)
	if legacyDoc == nil || legacyDoc.V2 == nil || diag.HasErrors(legacyDiags) {
		t.Fatalf("legacy sourceBinding read compatibility: %v", legacyDiags)
	}
	stable, err := MarshalManifestV2(legacyDoc.V2)
	if err != nil || bytes.Contains(stable, []byte(`"sourceBinding"`)) {
		t.Fatalf("canonicalizing a legacy manifest retained sourceBinding: %v\n%s", err, stable)
	}
	if bytes.Contains(stable, []byte(`"version"`)) || bytes.Contains(stable, []byte(`"packageVersions"`)) {
		t.Fatalf("canonicalizing a legacy manifest retained dependency versions: %v\n%s", err, stable)
	}
	if got := doc.V2.Migrations; len(got) != 2 || got[0].Kind != "code" || got[1].Kind != "sql" {
		t.Fatalf("migration identity sort = %#v", got)
	}
}

func TestManifestV2CanonicalBytesIgnoreResolvedDependencyVersionChanges(t *testing.T) {
	first := sampleManifestV2()
	second := sampleManifestV2()
	first.Packages = nil
	second.Packages = nil
	first.PackageVersions = []PackageVersionV2{{
		Identity: identity(first.Project, ContributionKindPackage, "", "go.putnami.dev/database"),
		Package:  "go.putnami.dev/database", Version: "1.4.0", Provenance: provenanceV2(first.Project, "go.mod"),
	}}
	second.PackageVersions = []PackageVersionV2{{
		Identity: identity(second.Project, ContributionKindPackage, "", "go.putnami.dev/database"),
		Package:  "go.putnami.dev/database", Version: "9.9.9", Provenance: provenanceV2(second.Project, "go.mod"),
	}}
	second.ConfigDefinitions[0].Provenance.Version = "9.9.9"
	second.PackageVersions[0].Version = "9.9.9"
	second.PackageVersions[0].Provenance.Version = "9.9.9"

	firstBytes, err := MarshalManifestV2(first)
	if err != nil {
		t.Fatal(err)
	}
	secondBytes, err := MarshalManifestV2(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstBytes, secondBytes) {
		t.Fatalf("resolved dependency version rewrote canonical v2 bytes:\nfirst:\n%s\nsecond:\n%s", firstBytes, secondBytes)
	}
	if !bytes.Contains(firstBytes, []byte(`"packages"`)) || bytes.Contains(firstBytes, []byte(`"packageVersions"`)) {
		t.Fatalf("historical package provider was not migrated canonically: %s", firstBytes)
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

// TestCanonicalPackagesAreScopedToTheCapabilitySurface pins the committed-shape
// rule of doc/adr/0001-committed-manifest-scope.md: canonical emission keeps the
// package contributions the manifest can justify from its own contents — the
// project itself and the packages that declared a contribution — and drops
// reachable-closure entries that contribute nothing to this manifest.
func TestCanonicalPackagesAreScopedToTheCapabilitySurface(t *testing.T) {
	m := sampleManifestV2()
	// The declaring package of every sample contribution ("go.putnami.dev/database")
	// plus a package the workload merely links.
	m.Packages = append(m.Packages, closurePackage("go.putnami.dev/database"), closurePackage("go.putnami.dev/unrelated"))

	surface := CapabilitySurfaceV2(m)
	for _, want := range []string{m.Project, "go.putnami.dev/database"} {
		if !surface[want] {
			t.Errorf("capability surface is missing %q: %v", want, surface)
		}
	}
	if surface["go.putnami.dev/unrelated"] {
		t.Errorf("a package that declares no contribution entered the surface: %v", surface)
	}

	canonical := CanonicalManifestV2(m)
	kept := make([]string, 0, len(canonical.Packages))
	for _, entry := range canonical.Packages {
		kept = append(kept, entry.Package)
	}
	want := []string{"go.putnami.dev/database", "go.putnami.dev/database"} // workload-owned entry + database-owned entry
	if !slices.Equal(kept, want) {
		t.Fatalf("canonical packages = %v, want %v", kept, want)
	}
	if scoped := ScopePackagesToContributionsV2(m); len(scoped) != 2 {
		t.Fatalf("ScopePackagesToContributionsV2 = %#v, want the two surface entries", scoped)
	}
}

// TestCanonicalBytesIgnoreClosureOnlyPackageChurn is the protocol-level
// reproducer: adding or removing a dependency edge that contributes nothing must
// not move one byte of a committed manifest. When the closure was enumerated
// verbatim, an edit in an unrelated project re-stamped every workload's tracked
// manifest.
func TestCanonicalBytesIgnoreClosureOnlyPackageChurn(t *testing.T) {
	before := sampleManifestV2()
	after := sampleManifestV2()
	after.Packages = append(after.Packages,
		closurePackage("go.putnami.dev/protocol/newly-added"),
		closurePackage("go.putnami.dev/protocol/also-new"))

	beforeBytes, err := MarshalManifestV2(before)
	if err != nil {
		t.Fatal(err)
	}
	afterBytes, err := MarshalManifestV2(after)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeBytes, afterBytes) {
		t.Fatalf("a closure-only dependency edit re-stamped the manifest:\nbefore:\n%s\nafter:\n%s", beforeBytes, afterBytes)
	}

	// Idempotence: the projection is a fixed point, so a manifest re-read from
	// canonical bytes canonicalizes to the same bytes.
	doc, diags := ParseAndValidateManifestDocument(afterBytes)
	if doc == nil || doc.V2 == nil || diag.HasErrors(diags) {
		t.Fatalf("scoped manifest failed strict validation: %v", diags)
	}
	again, err := MarshalManifestV2(doc.V2)
	if err != nil || !bytes.Equal(afterBytes, again) {
		t.Fatalf("scoped canonical form is not a fixed point: %v", err)
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

func TestManifestDocumentVersionDispatch(t *testing.T) {
	v1, err := os.ReadFile(filepath.Join("fixtures", "valid", "minimal.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc, diags := ParseAndValidateManifestDocument(v1)
	if doc == nil || doc.V1 == nil || doc.ProtocolVersion != 1 || diag.HasErrors(diags) {
		t.Fatalf("v1 dispatch: %#v %v", doc, diags)
	}

	valid := []string{
		`{"protocolVersion":1,"project":"p"}`,
		`{"protocolVersion":2,"project":"p"}`,
	}
	for _, value := range valid {
		if parsed, findings := ParseManifestDocument([]byte(value)); parsed == nil || diag.HasErrors(findings) {
			t.Fatalf("valid exact version token %s: %#v %v", value, parsed, findings)
		}
	}

	invalid := []string{
		`{"project":"p"}`,
		`{"protocolVersion":null,"project":"p"}`,
		`{"protocolVersion":"2","project":"p"}`,
		`{"protocolVersion":1.5,"project":"p"}`,
		`{"protocolVersion":1.0,"project":"p"}`,
		`{"protocolVersion":2.0,"project":"p"}`,
		`{"protocolVersion":1e0,"project":"p"}`,
		`{"protocolVersion":2e0,"project":"p"}`,
		`{"protocolVersion":3,"project":"p"}`,
		`{"nested":{"protocolVersion":2},"project":"p"}`,
		`{"protocolVersion":1,"protocolVersion":2,"project":"p"}`,
	}
	for _, value := range invalid {
		if parsed, findings := ParseManifestDocument([]byte(value)); parsed != nil || !findCode(findings, ErrorCodeInvalidProtocolVersion) {
			t.Fatalf("%s: %#v %v", value, parsed, findings)
		}
	}
	data, _ := MarshalManifestV2(sampleManifestV2())
	data = bytes.Replace(data, []byte(`"project": "go.putnami.dev/example/iam"`), []byte(`"unknown": true, "project": "go.putnami.dev/example/iam"`), 1)
	if _, findings := ParseManifestDocument(data); !findCode(findings, ErrorCodeUnknownField) {
		t.Fatalf("unknown v2 field: %v", findings)
	}
}

func TestManifestV2RequiredProviderCompletenessIsContainerScoped(t *testing.T) {
	m := sampleManifestV2()
	const owner = "go.putnami.dev/example/iam"
	m.HealthContributors = append(m.HealthContributors,
		HealthContributorV2{Identity: identity(owner, ContributionKindHealth, "health", "service"), Name: "service", Probe: ProbeKindHealth, Provenance: provenanceV2(owner, "health.go")},
		HealthContributorV2{Identity: identity(owner, ContributionKindHealth, "liveness", "service"), Name: "service", Probe: ProbeKindLiveness, Provenance: provenanceV2(owner, "liveness.go")},
	)
	m.RequiredCapabilities[0].Requires = []CapabilityKind{
		CapabilityKindPackage,
		CapabilityKindLifecycle,
		CapabilityKindReadiness,
		CapabilityKindLiveness,
		CapabilityKindHealth,
		CapabilityKindInfra,
		CapabilityKindDatasource,
		CapabilityKindMigration,
		CapabilityKindDiscoverer,
		CapabilityKindSchema,
		CapabilityKindConfig,
	}
	available := AvailableProviderKindsV2(m)
	for _, kind := range m.RequiredCapabilities[0].Requires {
		if !available[kind] {
			t.Errorf("provider projection missing %q", kind)
		}
	}
	if findings := ValidateManifestV2(m); diag.HasErrors(findings) {
		t.Fatalf("all mapped providers should satisfy the requirement: %v", findings)
	}

	// Completeness is scoped to the aggregate manifest, as in v1. A copied
	// dependency-owned provider in this container satisfies the workload-owned
	// requirement; an entry that exists only in another manifest does not.
	dependency := "go.putnami.dev/example/dependency"
	container := &ManifestV2{
		ProtocolVersion: 2,
		Project:         owner,
		ConfigDefinitions: []ConfigDefinitionV2{{
			Identity:   identity(dependency, ContributionKindConfig, "", "dependency.config"),
			Path:       "dependency.config",
			Provenance: provenanceV2(dependency, "config.go"),
		}},
		RequiredCapabilities: []RequiredCapabilityV2{{
			Identity: identity(owner, ContributionKindRequiredCapability, "", "configured"),
			Name:     "configured", Requires: []CapabilityKind{CapabilityKindConfig}, Provenance: provenanceV2(owner, "requirements.go"),
		}},
	}
	if findings := ValidateManifestV2(container); findCode(findings, ErrorCodeMissingRequiredProvider) {
		t.Fatalf("dependency-owned provider in the same container must satisfy requirement: %v", findings)
	}
	container.ConfigDefinitions = nil
	findings := ValidateManifestV2(container)
	if !findCode(findings, ErrorCodeMissingRequiredProvider) {
		t.Fatalf("provider outside the container must not satisfy requirement: %v", findings)
	}
	for _, finding := range findings {
		if finding.Code == ErrorCodeMissingRequiredProvider && (finding.Field != "requiredCapabilities[0].requires[0]" || !bytes.Contains([]byte(finding.Message), []byte(`project "go.putnami.dev/example/iam"`))) {
			t.Fatalf("missing-provider diagnostic lost stable scope context: %v", finding)
		}
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
