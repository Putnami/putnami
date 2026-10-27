package proof

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"go.putnami.dev/protocol/capabilities"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

const zeroSourceBinding = "source-v1:sha256:0000000000000000000000000000000000000000000000000000000000000000"

func provenanceV2(project, declaration string) capabilities.ProvenanceV2 {
	return capabilities.ProvenanceV2{
		Project: project, Package: "go.putnami.dev/database", Version: "1.4.0", SourceKind: capabilities.SourceKindFramework,
		Declaration: capabilities.DeclarationLocation{Root: capabilities.LocationRootProject, Path: declaration, Symbol: "Declaration"},
		Artifacts: []capabilities.ArtifactLocation{
			{Root: capabilities.LocationRootProject, Path: "z/output.json", Digest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
			{Root: capabilities.LocationRootProject, Path: "a/output.json", Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		},
	}
}

func identity(owner string, kind capabilities.ContributionKind, subkind, key string) capabilities.ContributionIdentity {
	return capabilities.ContributionIdentity{OwnerProject: owner, Kind: kind, Subkind: subkind, Key: key}
}

func sampleManifestV2() *capabilities.ManifestV2 {
	const owner = "go.putnami.dev/example/iam"
	p := func(file string) capabilities.ProvenanceV2 { return provenanceV2(owner, file) }
	return &capabilities.ManifestV2{
		Schema: "https://putnami.dev/schemas/putnami-capabilities-v2.json", ProtocolVersion: capabilities.ProtocolVersionV2, Project: owner,
		ConfigDefinitions: []capabilities.ConfigDefinitionV2{{Identity: identity(owner, capabilities.ContributionKindConfig, "", "database.default"), Path: "database.default", Fields: []capabilities.ConfigField{{Name: "url", Type: "string"}, {Name: "password", Type: "string", Sensitive: true}}, Provenance: p("config.go")}},
		Schemas:           []capabilities.SchemaContributionV2{{Identity: identity(owner, capabilities.ContributionKindSchema, "route", "listUsers"), Name: "listUsers", Kind: capabilities.SchemaKindRoute, Path: "/api/users", Provenance: p("routes.go")}},
		Discoverers:       []capabilities.DiscovererV2{{Identity: identity(owner, capabilities.ContributionKindDiscoverer, "source", "sqlSources"), Name: "sqlSources", Kind: capabilities.DiscovererKindSource, Provenance: p("discover.go")}},
		Migrations: []capabilities.MigrationBundleV2{
			{Identity: identity(owner, capabilities.ContributionKindMigration, "sql", "iam"), Name: "iam", Kind: "sql", Datasource: "default", Digest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", Provenance: p("migrations.go")},
			{Identity: identity(owner, capabilities.ContributionKindMigration, "code", "iam"), Name: "iam", Kind: "code", Provenance: p("code_migrations.go")},
		},
		InfraRequirements:    []capabilities.InfraRequirementV2{{Identity: identity(owner, capabilities.ContributionKindInfra, "database", "primary"), Name: "primary", Kind: capabilities.InfraKindDatabase, Provenance: p("infra.go")}},
		HealthContributors:   []capabilities.HealthContributorV2{{Identity: identity(owner, capabilities.ContributionKindHealth, "readiness", "primaryDatabase"), Name: "primaryDatabase", Probe: capabilities.ProbeKindReadiness, Provenance: p("health.go")}},
		LifecycleHooks:       []capabilities.LifecycleHookV2{{Identity: identity(owner, capabilities.ContributionKindLifecycle, "starter", "connectionPool"), Name: "connectionPool", Phase: capabilities.LifecyclePhaseStarter, Provenance: p("lifecycle.go")}},
		Packages:             []capabilities.PackageV2{{Identity: identity(owner, capabilities.ContributionKindPackage, "", "go.putnami.dev/database"), Package: "go.putnami.dev/database", Provenance: p("go.mod")}},
		RequiredCapabilities: []capabilities.RequiredCapabilityV2{{Identity: identity(owner, capabilities.ContributionKindRequiredCapability, "", "sql"), Name: "sql", Requires: []capabilities.CapabilityKind{capabilities.CapabilityKindReadiness, capabilities.CapabilityKindMigration, capabilities.CapabilityKindDatasource}, Provenance: p("requirements.go")}},
	}
}

func TestManifestV2EveryKindCanonicalAndStrict(t *testing.T) {
	spectest.Proves(t, feature, "v2-emission-drops-volatile-state", "v2-canonical-bytes-are-stable-and-carry-no-volatile-state")
	m := sampleManifestV2()
	if got := capabilities.ValidateManifestV2(m); diag.HasErrors(got) {
		t.Fatalf("valid v2 manifest: %v", got)
	}
	first, err := capabilities.MarshalManifestV2(m)
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
		got, err := capabilities.MarshalManifestV2(m)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, first) {
			t.Fatal("v2 canonical bytes changed across repeated serialization")
		}
	}
	doc, diags := capabilities.ParseAndValidateManifestDocument(first)
	if doc == nil || doc.V2 == nil || diag.HasErrors(diags) {
		t.Fatalf("strict v2 round trip: %v", diags)
	}
	again, err := capabilities.MarshalManifestV2(doc.V2)
	if err != nil || !bytes.Equal(first, again) {
		t.Fatalf("v2 canonical round trip changed bytes: %v", err)
	}
	legacy := bytes.Replace(first, []byte(`"sourceKind": "framework",`), []byte(`"sourceKind": "framework", "sourceBinding": "`+zeroSourceBinding+`",`), 1)
	legacyDoc, legacyDiags := capabilities.ParseAndValidateManifestDocument(legacy)
	if legacyDoc == nil || legacyDoc.V2 == nil || diag.HasErrors(legacyDiags) {
		t.Fatalf("legacy sourceBinding read compatibility: %v", legacyDiags)
	}
	stable, err := capabilities.MarshalManifestV2(legacyDoc.V2)
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
	spectest.Proves(t, feature, "v2-emission-drops-volatile-state", "a-resolved-version-change-keeps-the-v2-bytes")
	first := sampleManifestV2()
	second := sampleManifestV2()
	first.Packages = nil
	second.Packages = nil
	first.PackageVersions = []capabilities.PackageVersionV2{{
		Identity: identity(first.Project, capabilities.ContributionKindPackage, "", "go.putnami.dev/database"),
		Package:  "go.putnami.dev/database", Version: "1.4.0", Provenance: provenanceV2(first.Project, "go.mod"),
	}}
	second.PackageVersions = []capabilities.PackageVersionV2{{
		Identity: identity(second.Project, capabilities.ContributionKindPackage, "", "go.putnami.dev/database"),
		Package:  "go.putnami.dev/database", Version: "9.9.9", Provenance: provenanceV2(second.Project, "go.mod"),
	}}
	second.ConfigDefinitions[0].Provenance.Version = "9.9.9"
	second.PackageVersions[0].Version = "9.9.9"
	second.PackageVersions[0].Provenance.Version = "9.9.9"

	firstBytes, err := capabilities.MarshalManifestV2(first)
	if err != nil {
		t.Fatal(err)
	}
	secondBytes, err := capabilities.MarshalManifestV2(second)
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
func closurePackage(pkg string) capabilities.PackageV2 {
	return capabilities.PackageV2{
		Identity: identity(pkg, capabilities.ContributionKindPackage, "", pkg),
		Package:  pkg,
		Provenance: capabilities.ProvenanceV2{
			Project: pkg, Package: pkg, SourceKind: capabilities.SourceKindFramework,
			Declaration: capabilities.DeclarationLocation{Root: capabilities.LocationRootProject, Path: "putnami.json"},
		},
	}
}

// TestCanonicalPackagesAreScopedToTheCapabilitySurface pins the committed-shape
// rule of doc/adr/0001-committed-manifest-scope.md: canonical emission keeps the
// package contributions the manifest can justify from its own contents — the
// project itself and the packages that declared a contribution — and drops
// reachable-closure entries that contribute nothing to this manifest.
func TestCanonicalPackagesAreScopedToTheCapabilitySurface(t *testing.T) {
	spectest.Proves(t, feature, "packages-stay-on-the-capability-surface", "canonical-packages-are-scoped-to-the-capability-surface")
	m := sampleManifestV2()
	// The declaring package of every sample contribution ("go.putnami.dev/database")
	// plus a package the workload merely links.
	m.Packages = append(m.Packages, closurePackage("go.putnami.dev/database"), closurePackage("go.putnami.dev/unrelated"))

	surface := capabilities.CapabilitySurfaceV2(m)
	for _, want := range []string{m.Project, "go.putnami.dev/database"} {
		if !surface[want] {
			t.Errorf("capability surface is missing %q: %v", want, surface)
		}
	}
	if surface["go.putnami.dev/unrelated"] {
		t.Errorf("a package that declares no contribution entered the surface: %v", surface)
	}

	canonical := capabilities.CanonicalManifestV2(m)
	kept := make([]string, 0, len(canonical.Packages))
	for _, entry := range canonical.Packages {
		kept = append(kept, entry.Package)
	}
	want := []string{"go.putnami.dev/database", "go.putnami.dev/database"} // workload-owned entry + database-owned entry
	if !slices.Equal(kept, want) {
		t.Fatalf("canonical packages = %v, want %v", kept, want)
	}
	if scoped := capabilities.ScopePackagesToContributionsV2(m); len(scoped) != 2 {
		t.Fatalf("ScopePackagesToContributionsV2 = %#v, want the two surface entries", scoped)
	}
}

// TestCanonicalBytesIgnoreClosureOnlyPackageChurn is the protocol-level
// reproducer: adding or removing a dependency edge that contributes nothing must
// not move one byte of a committed manifest. When the closure was enumerated
// verbatim, an edit in an unrelated project re-stamped every workload's tracked
// manifest.
func TestCanonicalBytesIgnoreClosureOnlyPackageChurn(t *testing.T) {
	spectest.Proves(t, feature, "packages-stay-on-the-capability-surface", "a-closure-only-package-changes-no-byte")
	before := sampleManifestV2()
	after := sampleManifestV2()
	after.Packages = append(after.Packages,
		closurePackage("go.putnami.dev/protocol/newly-added"),
		closurePackage("go.putnami.dev/protocol/also-new"))

	beforeBytes, err := capabilities.MarshalManifestV2(before)
	if err != nil {
		t.Fatal(err)
	}
	afterBytes, err := capabilities.MarshalManifestV2(after)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeBytes, afterBytes) {
		t.Fatalf("a closure-only dependency edit re-stamped the manifest:\nbefore:\n%s\nafter:\n%s", beforeBytes, afterBytes)
	}

	// Idempotence: the projection is a fixed point, so a manifest re-read from
	// canonical bytes canonicalizes to the same bytes.
	doc, diags := capabilities.ParseAndValidateManifestDocument(afterBytes)
	if doc == nil || doc.V2 == nil || diag.HasErrors(diags) {
		t.Fatalf("scoped manifest failed strict validation: %v", diags)
	}
	again, err := capabilities.MarshalManifestV2(doc.V2)
	if err != nil || !bytes.Equal(afterBytes, again) {
		t.Fatalf("scoped canonical form is not a fixed point: %v", err)
	}
}

func TestManifestDocumentVersionDispatch(t *testing.T) {
	spectest.Proves(t, feature, "the-reader-is-strict-and-versioned", "the-document-reader-dispatches-on-the-exact-version-token")
	v1, err := os.ReadFile(filepath.Join("..", "fixtures", "valid", "minimal.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc, diags := capabilities.ParseAndValidateManifestDocument(v1)
	if doc == nil || doc.V1 == nil || doc.ProtocolVersion != 1 || diag.HasErrors(diags) {
		t.Fatalf("v1 dispatch: %#v %v", doc, diags)
	}

	valid := []string{
		`{"protocolVersion":1,"project":"p"}`,
		`{"protocolVersion":2,"project":"p"}`,
	}
	for _, value := range valid {
		if parsed, findings := capabilities.ParseManifestDocument([]byte(value)); parsed == nil || diag.HasErrors(findings) {
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
		if parsed, findings := capabilities.ParseManifestDocument([]byte(value)); parsed != nil || !findCode(findings, capabilities.ErrorCodeInvalidProtocolVersion) {
			t.Fatalf("%s: %#v %v", value, parsed, findings)
		}
	}
	data, _ := capabilities.MarshalManifestV2(sampleManifestV2())
	data = bytes.Replace(data, []byte(`"project": "go.putnami.dev/example/iam"`), []byte(`"unknown": true, "project": "go.putnami.dev/example/iam"`), 1)
	if _, findings := capabilities.ParseManifestDocument(data); !findCode(findings, capabilities.ErrorCodeUnknownField) {
		t.Fatalf("unknown v2 field: %v", findings)
	}
}

func TestManifestV2RequiredProviderCompletenessIsContainerScoped(t *testing.T) {
	spectest.Proves(t, feature, "a-required-capability-needs-a-provider-in-its-manifest", "v2-completeness-is-scoped-to-the-manifest")
	m := sampleManifestV2()
	const owner = "go.putnami.dev/example/iam"
	m.HealthContributors = append(m.HealthContributors,
		capabilities.HealthContributorV2{Identity: identity(owner, capabilities.ContributionKindHealth, "health", "service"), Name: "service", Probe: capabilities.ProbeKindHealth, Provenance: provenanceV2(owner, "health.go")},
		capabilities.HealthContributorV2{Identity: identity(owner, capabilities.ContributionKindHealth, "liveness", "service"), Name: "service", Probe: capabilities.ProbeKindLiveness, Provenance: provenanceV2(owner, "liveness.go")},
	)
	m.RequiredCapabilities[0].Requires = []capabilities.CapabilityKind{
		capabilities.CapabilityKindPackage,
		capabilities.CapabilityKindLifecycle,
		capabilities.CapabilityKindReadiness,
		capabilities.CapabilityKindLiveness,
		capabilities.CapabilityKindHealth,
		capabilities.CapabilityKindInfra,
		capabilities.CapabilityKindDatasource,
		capabilities.CapabilityKindMigration,
		capabilities.CapabilityKindDiscoverer,
		capabilities.CapabilityKindSchema,
		capabilities.CapabilityKindConfig,
	}
	available := capabilities.AvailableProviderKindsV2(m)
	for _, kind := range m.RequiredCapabilities[0].Requires {
		if !available[kind] {
			t.Errorf("provider projection missing %q", kind)
		}
	}
	if findings := capabilities.ValidateManifestV2(m); diag.HasErrors(findings) {
		t.Fatalf("all mapped providers should satisfy the requirement: %v", findings)
	}

	// Completeness is scoped to the aggregate manifest, as in v1. A copied
	// dependency-owned provider in this container satisfies the workload-owned
	// requirement; an entry that exists only in another manifest does not.
	dependency := "go.putnami.dev/example/dependency"
	container := &capabilities.ManifestV2{
		ProtocolVersion: 2,
		Project:         owner,
		ConfigDefinitions: []capabilities.ConfigDefinitionV2{{
			Identity:   identity(dependency, capabilities.ContributionKindConfig, "", "dependency.config"),
			Path:       "dependency.config",
			Provenance: provenanceV2(dependency, "config.go"),
		}},
		RequiredCapabilities: []capabilities.RequiredCapabilityV2{{
			Identity: identity(owner, capabilities.ContributionKindRequiredCapability, "", "configured"),
			Name:     "configured", Requires: []capabilities.CapabilityKind{capabilities.CapabilityKindConfig}, Provenance: provenanceV2(owner, "requirements.go"),
		}},
	}
	if findings := capabilities.ValidateManifestV2(container); findCode(findings, capabilities.ErrorCodeMissingRequiredProvider) {
		t.Fatalf("dependency-owned provider in the same container must satisfy requirement: %v", findings)
	}
	container.ConfigDefinitions = nil
	findings := capabilities.ValidateManifestV2(container)
	if !findCode(findings, capabilities.ErrorCodeMissingRequiredProvider) {
		t.Fatalf("provider outside the container must not satisfy requirement: %v", findings)
	}
	for _, finding := range findings {
		if finding.Code == capabilities.ErrorCodeMissingRequiredProvider && (finding.Field != "requiredCapabilities[0].requires[0]" || !bytes.Contains([]byte(finding.Message), []byte(`project "go.putnami.dev/example/iam"`))) {
			t.Fatalf("missing-provider diagnostic lost stable scope context: %v", finding)
		}
	}
}
