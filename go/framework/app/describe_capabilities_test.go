package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/config"
	"go.putnami.dev/migration"
	protocaps "go.putnami.dev/protocol/capabilities"
	protofeatures "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	protoinfra "go.putnami.dev/protocol/infra"
)

const capabilityProofProject = "go.putnami.dev/examples/capabilities-proof"

type capDBOptions struct {
	URL      string `json:"url"`
	Password string `json:"password" sensitive:"true"`
}

type capConfigPlugin struct {
	name string
	defs []config.Descriptor
}

func (p *capConfigPlugin) Name() string                           { return p.name }
func (p *capConfigPlugin) ConfigDefinitions() []config.Descriptor { return p.defs }

type capMigrationSource struct{ namespace, database string }

func (s capMigrationSource) Kind() migration.Kind { return migration.KindSQL }
func (s capMigrationSource) Namespace() string    { return s.namespace }
func (s capMigrationSource) InfraDatabases() []protoinfra.Database {
	return []protoinfra.Database{{Name: s.database, Engine: protoinfra.EnginePostgres, Schemas: []string{"public"}}}
}

type capMigrationPlugin struct {
	src   migration.Source
	calls int
}

func (p *capMigrationPlugin) Name() string { return "iam-feature" }
func (p *capMigrationPlugin) MigrationSources() []migration.Source {
	p.calls++
	return []migration.Source{p.src}
}

type capHealthPlugin struct{ name string }

func (p *capHealthPlugin) Name() string                      { return p.name }
func (p *capHealthPlugin) CheckHealth(context.Context) error { return nil }

type capReadinessPlugin struct{ name string }

func (p *capReadinessPlugin) Name() string                         { return p.name }
func (p *capReadinessPlugin) CheckReadiness(context.Context) error { return nil }

type capLifecyclePlugin struct{ name string }

func (p *capLifecyclePlugin) Name() string                         { return p.name }
func (p *capLifecyclePlugin) Start(context.Context, *Module) error { return nil }
func (p *capLifecyclePlugin) Stop(context.Context, *Module) error  { return nil }

type capRequirementPlugin struct{ name string }

func (p *capRequirementPlugin) Name() string { return p.name }
func (p *capRequirementPlugin) RequiredCapabilities() []CapabilityRequirement {
	return []CapabilityRequirement{{Name: "sql", Requires: []protocaps.CapabilityKind{
		protocaps.CapabilityKindLifecycle, protocaps.CapabilityKindHealth,
		protocaps.CapabilityKindInfra,
		protocaps.CapabilityKindMigration, protocaps.CapabilityKindDatasource,
		protocaps.CapabilityKindReadiness, protocaps.CapabilityKindDiscoverer,
		protocaps.CapabilityKindSchema, protocaps.CapabilityKindConfig,
		protocaps.CapabilityKindPackage,
	}}}
}

type capInventoryPlugin struct{}

func (*capInventoryPlugin) Name() string { return "inventory" }
func (*capInventoryPlugin) CapabilitySchemas() []CapabilitySchema {
	return []CapabilitySchema{
		{Name: "api-proto", Kind: protocaps.SchemaKindProto, Path: "schema/api.proto"},
		{Name: "listUsers", Kind: protocaps.SchemaKindRoute, Path: "/api/users"},
		{Name: "openapi", Kind: protocaps.SchemaKindOpenAPI, Path: "schema/openapi.json"},
	}
}
func (*capInventoryPlugin) CapabilityDiscoverers() []CapabilityDiscoverer { return nil }
func (*capInventoryPlugin) Describe(ctx *DescribeContext) error {
	if err := os.MkdirAll(filepath.Join(ctx.OutputDir, "schema"), 0o750); err != nil {
		return err
	}
	for _, path := range []string{"api.proto", "openapi.json"} {
		if err := os.WriteFile(filepath.Join(ctx.OutputDir, "schema", path), []byte("proof\n"), 0o600); err != nil {
			return err
		}
	}
	return protoinfra.WriteSidecarIn(ctx.OutputDir, "inventory", protoinfra.PerProjectManifest{
		Events:  &protoinfra.Events{Publishes: []string{"user.created"}, Subscribes: []protoinfra.Subscription{{Topic: "user.updated"}}},
		Storage: []protoinfra.StorageBucket{{Name: "avatars"}}, Secrets: []string{"oauth.client-secret"},
		ScheduledJobs: []protoinfra.ScheduledJob{{Name: "cleanup", Schedule: "0 2 * * *"}},
	})
}

// capabilityProofFeature is the authored intent the describe tests prove
// against. It is written to the project root rather than built in memory, so
// the producer exercises the same discovery a real build performs.
const capabilityProofFeature = `{
  "$schema": "https://putnami.dev/schemas/putnami-features.json",
  "protocolVersion": 1,
  "namespace": "capabilities",
  "features": [
    {
      "id": "capabilities/go-evidence",
      "type": "feature",
      "name": "Go capability evidence",
      "outcome": "A build proves a feature requirement from an exact capability contribution",
      "owner": "capabilities",
      "target": "coded",
      "requirements": [
        {
          "id": "implementation",
          "stage": "coded",
          "evidenceKinds": ["capability"]
        }
      ]
    }
  ]
}
`

func buildCapabilityApp() (*Application, *capMigrationPlugin) {
	a := New("capabilities-proof-app")
	a.Feature(Feature{
		ID: "capabilities/go-evidence", Name: "Go capability evidence",
		Outcome: "A build proves a feature requirement from an exact capability contribution",
		Owner:   "capabilities",
		Proves: []FeatureProof{{
			Requirement: "implementation",
			Contribution: ContributionRef{
				Kind: protocaps.ContributionKindSchema, Subkind: string(protocaps.SchemaKindRoute), Key: "listUsers",
			},
		}},
	})
	migrations := &capMigrationPlugin{src: capMigrationSource{namespace: "iam", database: "default"}}
	a.Use(&capConfigPlugin{name: "config-owner", defs: []config.Descriptor{config.Config[capDBOptions]("database.default").Descriptor()}})
	a.Use(migrations)
	a.Use(&capHealthPlugin{name: "diskSpace"})
	a.Use(&capReadinessPlugin{name: "primaryDatabase"})
	a.Use(&capLifecyclePlugin{name: "connectionPool"})
	a.Use(&capRequirementPlugin{name: "sql-requirement"})
	a.Use(&capInventoryPlugin{})
	return a, migrations
}

func testSourceBinding(label string) string {
	binding, err := protocaps.ComputeSourceBinding([]protocaps.SourceBindingFile{{Path: "source.go", Mode: protocaps.SourceModeRegular, Digest: protocaps.SourceDigest([]byte(label))}})
	if err != nil {
		panic(err)
	}
	return binding
}

func writeVersionStamp(t *testing.T, outDir string, extra ...generatedCapabilityPackage) {
	t.Helper()
	stamp := generatedVersionInfo{Name: capabilityProofProject, CapabilityPackages: append([]generatedCapabilityPackage{
		{Package: capabilityProofProject, Version: "0.1.0", SourceRoot: "go/samples/capabilities-proof", EvidencePath: "go/samples/capabilities-proof/putnami.json", SourceBinding: testSourceBinding("workload")},
		{Package: "go.putnami.dev/app", Version: "1.4.0", SourceRoot: "go/framework/app", EvidencePath: "go/framework/app/putnami.json", SourceBinding: testSourceBinding("app")},
	}, extra...)}
	data, err := json.Marshal(stamp)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "version.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	// A describer receives the project's .gen directory, so the producer reads
	// authored intent from its parent. Writing the manifest beside the output
	// directory reproduces that layout instead of stubbing discovery.
	if err := os.WriteFile(filepath.Join(filepath.Dir(outDir), protofeatures.ManifestFilename), []byte(capabilityProofFeature), 0o600); err != nil {
		t.Fatal(err)
	}
}

func describeCapabilityArtifacts(t *testing.T) ([]byte, []byte, *capMigrationPlugin) {
	t.Helper()
	out := t.TempDir()
	writeVersionStamp(t, out)
	a, migrations := buildCapabilityApp()
	if err := a.Describe(out, nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	manifest, err := os.ReadFile(filepath.Join(out, "schema", protocaps.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := os.ReadFile(filepath.Join(out, "schema", "feature-evidence", featureEvidenceFilename))
	if err != nil {
		t.Fatal(err)
	}
	return manifest, evidence, migrations
}

// TestDescribeCapabilitiesIgnoresNonContributingClosurePackages pins the
// single-project stability contract at the emitter: the scheduler
// stamps the workload's whole reachable package closure, but the committed
// manifest names only the packages that actually declared a contribution. A
// dependency edit anywhere else in the workspace grows the stamp and must not
// move one byte of this project's manifest.
func TestDescribeCapabilitiesIgnoresNonContributingClosurePackages(t *testing.T) {
	describeWith := func(extra ...generatedCapabilityPackage) []byte {
		t.Helper()
		out := t.TempDir()
		writeVersionStamp(t, out, extra...)
		a, _ := buildCapabilityApp()
		if err := a.Describe(out, nil); err != nil {
			t.Fatalf("Describe: %v", err)
		}
		manifest, err := os.ReadFile(filepath.Join(out, "schema", protocaps.ManifestFilename))
		if err != nil {
			t.Fatal(err)
		}
		return manifest
	}

	baseline := describeWith()
	widened := describeWith(generatedCapabilityPackage{
		Package: "go.putnami.dev/protocol/newly-reachable", Version: "0.1.0",
		SourceRoot: "protocols/newly-reachable", EvidencePath: "protocols/newly-reachable/putnami.json",
		SourceBinding: testSourceBinding("newly-reachable"),
	})
	if !bytes.Equal(baseline, widened) {
		t.Fatalf("a closure-only dependency re-stamped the manifest:\n--- baseline ---\n%s\n--- widened ---\n%s", baseline, widened)
	}

	document, diags := protocaps.ParseAndValidateManifestDocument(widened)
	if document == nil || document.V2 == nil || len(diags) != 0 {
		t.Fatalf("manifest: %#v %v", document, diags)
	}
	named := make([]string, 0, len(document.V2.Packages))
	for _, entry := range document.V2.Packages {
		named = append(named, entry.Package)
	}
	// The workload itself and go.putnami.dev/app, which declares every plugin
	// contribution in this proof — nothing else the stamp reaches.
	if want := []string{"go.putnami.dev/app", capabilityProofProject}; !slices.Equal(named, want) {
		t.Fatalf("committed packages = %v, want the contributing surface %v", named, want)
	}
}

func TestDescribeCapabilitiesV2AndFeatureEvidenceAreCanonicalDeterministic(t *testing.T) {
	firstManifest, firstEvidence, migrations := describeCapabilityArtifacts(t)
	if migrations.calls != 1 {
		t.Fatalf("MigrationSources calls = %d, want one frozen collection", migrations.calls)
	}
	document, diags := protocaps.ParseAndValidateManifestDocument(firstManifest)
	if document == nil || document.V2 == nil || len(diags) != 0 {
		t.Fatalf("v2 manifest: %#v %v", document, diags)
	}
	canonical, err := protocaps.MarshalManifestV2(document.V2)
	if err != nil || !bytes.Equal(firstManifest, canonical) {
		t.Fatalf("manifest not canonical: %v", err)
	}
	counts := []int{len(document.V2.ConfigDefinitions), len(document.V2.Schemas), len(document.V2.Discoverers), len(document.V2.Migrations), len(document.V2.InfraRequirements), len(document.V2.HealthContributors), len(document.V2.LifecycleHooks), len(document.V2.Packages), len(document.V2.RequiredCapabilities)}
	for i, count := range counts {
		if count == 0 {
			t.Errorf("representative v2 collection %d is empty", i)
		}
	}
	if len(document.V2.PackageVersions) != 0 || bytes.Contains(firstManifest, []byte(`"packageVersions"`)) || bytes.Contains(firstManifest, []byte(`"version"`)) {
		t.Fatalf("stable manifest contains resolved versions: %s", firstManifest)
	}
	for _, schema := range document.V2.Schemas {
		if schema.Identity.OwnerProject != capabilityProofProject {
			t.Errorf("local schema owner = %q", schema.Identity.OwnerProject)
		}
	}
	evidenceDocument, findings := protofeatures.ParseAndValidateEvidenceDocument(firstEvidence)
	if evidenceDocument == nil || len(findings) != 0 || len(evidenceDocument.Evidence) != 1 {
		t.Fatalf("feature evidence: %#v %v", evidenceDocument, findings)
	}
	evidenceCanonical, err := protofeatures.MarshalEvidenceDocument(evidenceDocument)
	if err != nil || !bytes.Equal(firstEvidence, evidenceCanonical) {
		t.Fatalf("evidence not canonical: %v", err)
	}
	for i := 0; i < 3; i++ {
		manifest, evidence, _ := describeCapabilityArtifacts(t)
		if !bytes.Equal(firstManifest, manifest) || !bytes.Equal(firstEvidence, evidence) {
			t.Fatalf("run %d is nondeterministic", i)
		}
	}
}

// TestGeneratedEvidenceCarriesNoMachineLocalValues pins that a committed
// record stays portable. The producer reads the build machine's working
// directory, temp directories, and absolute compile-time paths on its way to a
// declaration site, and any of them leaking into a durable artifact would make
// the committed bytes depend on who ran the build.
func TestGeneratedEvidenceCarriesNoMachineLocalValues(t *testing.T) {
	_, evidence, _ := describeCapabilityArtifacts(t)
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	forbidden := []string{os.TempDir(), "/var/folders", "/Users/", "/home/", "/private/", cwd}
	if home != "" {
		forbidden = append(forbidden, home)
	}
	for _, value := range forbidden {
		if value == "" {
			continue
		}
		if bytes.Contains(evidence, []byte(value)) {
			t.Errorf("generated evidence leaked the machine-local value %q:\n%s", value, evidence)
		}
	}
	document, findings := protofeatures.ParseAndValidateEvidenceDocument(evidence)
	if document == nil || len(findings) != 0 {
		t.Fatalf("generated evidence: %v", findings)
	}
	for _, record := range document.Evidence {
		if strings.HasPrefix(record.Provenance.Path, "/") || strings.Contains(record.Provenance.Path, "..") {
			t.Errorf("provenance path %q is not contained and project-relative", record.Provenance.Path)
		}
	}
}

func TestDescribeCapabilitiesZeroEvidenceRemovesStaleFragment(t *testing.T) {
	out := t.TempDir()
	writeVersionStamp(t, out)
	evidencePath := filepath.Join(out, "schema", "feature-evidence", featureEvidenceFilename)
	if err := os.MkdirAll(filepath.Dir(evidencePath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evidencePath, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := New("empty")
	if err := a.Describe(out, nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if _, err := os.Stat(evidencePath); !os.IsNotExist(err) {
		t.Fatalf("zero evidence retained stale fragment: %v", err)
	}
}

// TestRemovingAProofRemovesOnlyThisProducersEvidence covers the case the
// zero-feature test above cannot: the feature is still declared, its proofs are
// gone, and a sibling language producer has written its own fragment into the
// same directory. Removing a mapping must retract this producer's record and
// leave the sibling's bytes untouched — a producer owns its own output and
// nothing else in that directory.
func TestRemovingAProofRemovesOnlyThisProducersEvidence(t *testing.T) {
	out := t.TempDir()
	writeVersionStamp(t, out)
	app, _ := buildCapabilityApp()
	if err := app.Describe(out, nil); err != nil {
		t.Fatalf("Describe with a proof: %v", err)
	}
	evidencePath := filepath.Join(out, "schema", "feature-evidence", featureEvidenceFilename)
	if _, err := os.Stat(evidencePath); err != nil {
		t.Fatalf("a mapped proof must publish evidence: %v", err)
	}
	siblingPath := filepath.Join(out, "schema", "feature-evidence", "typescript-framework.json")
	sibling := []byte("{\n  \"protocolVersion\": 1,\n  \"evidence\": []\n}\n")
	if err := os.WriteFile(siblingPath, sibling, 0o600); err != nil {
		t.Fatal(err)
	}

	withoutProof, _ := buildCapabilityApp()
	withoutProof.Module.feature.Proves = nil
	if err := withoutProof.Describe(out, nil); err != nil {
		t.Fatalf("Describe without a proof: %v", err)
	}
	if _, err := os.Stat(evidencePath); !os.IsNotExist(err) {
		t.Fatalf("removing the mapping left ghost evidence: %v", err)
	}
	retained, err := os.ReadFile(filepath.Clean(siblingPath))
	if err != nil || !bytes.Equal(retained, sibling) {
		t.Fatalf("a sibling producer's evidence was disturbed: %v", err)
	}
}

func TestDescribeCapabilitiesFailureRemovesAllFinalAndTempArtifacts(t *testing.T) {
	type one struct {
		X string `json:"x"`
	}
	type two struct {
		Y string `json:"y"`
	}
	a := New("partial")
	a.Use(&capConfigPlugin{name: "a", defs: []config.Descriptor{config.Config[one]("dup").Descriptor()}})
	a.Use(&capConfigPlugin{name: "b", defs: []config.Descriptor{config.Config[two]("dup").Descriptor()}})
	out := t.TempDir()
	writeVersionStamp(t, out)
	paths := []string{filepath.Join(out, "schema", protocaps.ManifestFilename), filepath.Join(out, "schema", protocaps.ManifestFilename+".tmp"), filepath.Join(out, "schema", "feature-evidence", featureEvidenceFilename), filepath.Join(out, "schema", "feature-evidence", featureEvidenceFilename+".tmp")}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Describe(out, nil); err == nil {
		t.Fatal("expected duplicate contribution failure")
	}
	for _, path := range paths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("stale artifact %s survived: %v", path, err)
		}
	}
}

func TestDescribeCapabilitiesTargetFilterAndRuntimeSidecar(t *testing.T) {
	out := t.TempDir()
	writeVersionStamp(t, out)
	a, _ := buildCapabilityApp()
	if err := a.Describe(out, []string{"openapi"}); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(out, "schema", protocaps.ManifestFilename)
	if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
		t.Fatalf("target-filtered manifest exists: %v", err)
	}
	infraDir := filepath.Join(out, protoinfra.PerProjectManifestDir)
	if err := os.MkdirAll(infraDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(infraDir, "runtime.json"), []byte(`{"ingress":{"public":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a, _ = buildCapabilityApp()
	if err := a.Describe(out, nil); err != nil {
		t.Fatalf("runtime intent sidecar was parsed: %v", err)
	}
}

func TestDescribeCapabilitiesMissingAndDuplicateProvidersFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func() *Application
		code  string
	}{
		{name: "missing", code: protocaps.ErrorCodeMissingRequiredProvider, build: func() *Application {
			a := New("missing")
			a.Use(&capRequirementPlugin{name: "required"})
			return a
		}},
		{name: "duplicate", code: protocaps.ErrorCodeDuplicateContribution, build: func() *Application {
			a := New("duplicate")
			a.Use(&capHealthPlugin{name: "same"})
			a.Use(&capHealthPlugin{name: "same"})
			return a
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := t.TempDir()
			writeVersionStamp(t, out)
			err := tc.build().Describe(out, nil)
			if err == nil || !strings.Contains(err.Error(), tc.code) {
				t.Fatalf("error = %v, want %s", err, tc.code)
			}
		})
	}
}

func TestDescribeCapabilitiesMigrationIdentityConflictsFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sources []migration.Source
	}{
		{name: "sql datasource conflict", sources: []migration.Source{
			describeSchemaSource{kind: migration.KindSQL, ns: "iam", dbs: []protoinfra.Database{{Name: "primary", Engine: protoinfra.EnginePostgres}}},
			describeSchemaSource{kind: migration.KindSQL, ns: "iam", dbs: []protoinfra.Database{{Name: "replica", Engine: protoinfra.EnginePostgres}}},
		}},
		{name: "non sql duplicate", sources: []migration.Source{describeFakeSource{kind: "gcs", ns: "backups"}, describeFakeSource{kind: "gcs", ns: "backups"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := New("migration")
			a.Use(&describeContribPlugin{name: "owner", sources: tc.sources})
			out := t.TempDir()
			writeVersionStamp(t, out)
			err := a.Describe(out, nil)
			if err == nil || (!strings.Contains(err.Error(), protocaps.ErrorCodeConflictingContributionCopy) && !strings.Contains(err.Error(), protocaps.ErrorCodeDuplicateContribution)) {
				t.Fatalf("migration conflict error = %v", err)
			}
		})
	}
}

func TestDescribeCapabilitiesMigrationIdentityIncludesKind(t *testing.T) {
	a := New("migration-kinds")
	a.Use(&describeContribPlugin{name: "owner", sources: []migration.Source{
		describeFakeSource{kind: migration.KindSQL, ns: "shared"},
		describeFakeSource{kind: "gcs", ns: "shared"},
	}})
	out := t.TempDir()
	writeVersionStamp(t, out)
	if err := a.Describe(out, nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(out, "schema", protocaps.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	document, diags := protocaps.ParseAndValidateManifestDocument(data)
	if document == nil || document.V2 == nil || len(diags) != 0 {
		t.Fatalf("manifest: %#v %v", document, diags)
	}
	if len(document.V2.Migrations) != 2 {
		t.Fatalf("cross-kind migrations = %#v", document.V2.Migrations)
	}
	wantDiscoverers := map[string]bool{"gcs:shared": false, "sql:shared": false}
	for _, discoverer := range document.V2.Discoverers {
		if _, ok := wantDiscoverers[discoverer.Name]; ok {
			wantDiscoverers[discoverer.Name] = true
		}
	}
	for name, found := range wantDiscoverers {
		if !found {
			t.Errorf("missing source discoverer %q", name)
		}
	}
}

func TestCollectCapabilityDiscoverersRetainsNonSQLDuplicatesForValidation(t *testing.T) {
	a := New("go.putnami.dev/app")
	a.Use(&describeContribPlugin{name: "owner", sources: []migration.Source{describeFakeSource{kind: "gcs", ns: "backups"}, describeFakeSource{kind: "gcs", ns: "backups"}}})
	if err := a.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())
	inv, err := newCapabilityInventory("go.putnami.dev/app", []generatedCapabilityPackage{{Package: "go.putnami.dev/app", Version: "1.0.0", SourceRoot: "go/framework/app", EvidencePath: "go/framework/app/putnami.json", SourceBinding: testSourceBinding("app")}})
	if err != nil {
		t.Fatal(err)
	}
	discoverers, err := a.collectCapabilityDiscoverers(inv)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, discoverer := range discoverers {
		if discoverer.Name == "gcs:backups" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("non-SQL discoverers = %d, want 2", count)
	}
}

func TestCapabilityIdentityIsWorkloadScopedAndTrimpathDeclarationResolves(t *testing.T) {
	packages := func(project string) []generatedCapabilityPackage {
		return []generatedCapabilityPackage{{Package: project, Version: "1.0.0", SourceRoot: ".", EvidencePath: "putnami.json", SourceBinding: testSourceBinding(project)}}
	}
	one, err := newCapabilityInventory("example/one", packages("example/one"))
	if err != nil {
		t.Fatal(err)
	}
	two, err := newCapabilityInventory("example/two", packages("example/two"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := one.identity(protocaps.ContributionKindHealth, string(protocaps.ProbeKindHealth), "same")
	if err != nil {
		t.Fatal(err)
	}
	b, err := two.identity(protocaps.ContributionKindHealth, string(protocaps.ProbeKindHealth), "same")
	if err != nil {
		t.Fatal(err)
	}
	if a.OwnerProject == b.OwnerProject || a.Key != b.Key {
		t.Fatalf("workload identities collapsed: %#v %#v", a, b)
	}
	owner := generatedCapabilityPackage{Package: "go.putnami.dev/api", SourceRoot: "go/framework/api"}
	if path, err := declarationPathForPackage("go.putnami.dev/api/plugin.go", owner); err != nil || path != "plugin.go" {
		t.Fatalf("trimpath declaration = %q, %v", path, err)
	}
}

func wrappedCapabilityHealth(context.Context) error { return nil }

func TestHealthFuncProvenanceUsesWrappedFunctionDeclaration(t *testing.T) {
	inv, err := newCapabilityInventory("go.putnami.dev/app", []generatedCapabilityPackage{{
		Package: "go.putnami.dev/app", Version: "1.0.0", SourceRoot: "go/framework/app",
		EvidencePath: "go/framework/app/putnami.json", SourceBinding: testSourceBinding("app"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	probe := HealthFunc("wrapped", wrappedCapabilityHealth)
	identity, err := inv.identity(protocaps.ContributionKindHealth, string(protocaps.ProbeKindHealth), "wrapped")
	if err != nil {
		t.Fatal(err)
	}
	provenance, err := inv.provenance(probe, identity, "CheckHealth")
	if err != nil {
		t.Fatal(err)
	}
	if provenance.Declaration.Path != "describe_capabilities_test.go" || !strings.Contains(provenance.Declaration.Symbol, "wrappedCapabilityHealth") {
		t.Fatalf("wrapped probe declaration = %#v", provenance.Declaration)
	}
}

func TestPackageInventoryKeepsResolvedVersionsOutsideManifest(t *testing.T) {
	packages := []generatedCapabilityPackage{
		{Package: "example/workload", Version: "1.0.0", SourceRoot: "workload", EvidencePath: "workload/putnami.json", SourceBinding: testSourceBinding("workload")},
		{Package: "example/library", Version: "2.0.0", SourceRoot: "library", EvidencePath: "library/putnami.json", SourceBinding: testSourceBinding("library")},
	}
	inv, err := newCapabilityInventory("example/workload", packages)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.ordered) != 2 || inv.packages["example/library"].Version != "2.0.0" {
		t.Fatalf("scheduler package inventory = %#v", inv.ordered)
	}
}

type describeOnlyStorage struct{}

func (*describeOnlyStorage) Name() string { return "storage-only" }
func (*describeOnlyStorage) Describe(ctx *DescribeContext) error {
	return protoinfra.WriteSidecarIn(ctx.OutputDir, "storage-only", protoinfra.PerProjectManifest{Storage: []protoinfra.StorageBucket{{Name: "attachments"}}})
}

type describeDatabaseSidecar struct{}

func (*describeDatabaseSidecar) Name() string { return "database" }
func (*describeDatabaseSidecar) Describe(ctx *DescribeContext) error {
	return protoinfra.WriteSidecarIn(ctx.OutputDir, "database", protoinfra.PerProjectManifest{Databases: []protoinfra.Database{{Name: "default", Engine: protoinfra.EnginePostgres}}})
}

func TestDatabaseSidecarTakesPrecedenceOverMigrationFallback(t *testing.T) {
	out := t.TempDir()
	writeVersionStamp(t, out)
	a := New("database-precedence")
	a.Use(&capMigrationPlugin{src: capMigrationSource{namespace: "iam", database: "default"}})
	a.UseForDescribe(&describeDatabaseSidecar{})
	if err := a.Describe(out, nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(out, "schema", protocaps.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	document, diags := protocaps.ParseAndValidateManifestDocument(data)
	if document == nil || document.V2 == nil || len(diags) != 0 {
		t.Fatalf("manifest: %#v %v", document, diags)
	}
	var databases []protocaps.InfraRequirementV2
	for _, requirement := range document.V2.InfraRequirements {
		if requirement.Kind == protocaps.InfraKindDatabase && requirement.Name == "default" {
			databases = append(databases, requirement)
		}
	}
	if len(databases) != 1 || !strings.Contains(databases[0].Provenance.Declaration.Symbol, "describeDatabaseSidecar") {
		t.Fatalf("database precedence = %#v", databases)
	}
}

type describeLedgerSidecar struct{}

func (*describeLedgerSidecar) Name() string { return "workload-ledger" }
func (*describeLedgerSidecar) Describe(ctx *DescribeContext) error {
	return protoinfra.WriteSidecarIn(ctx.OutputDir, "workload-ledger", protoinfra.PerProjectManifest{Databases: []protoinfra.Database{{Name: "default", Engine: protoinfra.EnginePostgres, Schemas: []string{"ledger"}}}})
}

// TestDatabaseSidecarTakesPrecedenceOverWorkloadLocalSidecar pins the
// read-only-workload pattern: the database adapter declares the datasource
// while a workload-local describe-only sidecar supplies the schemas of a
// database whose migrations another workload owns. The adapter's declaration
// is canonical over ANY other sidecar, not just the migration fallback — two
// declarations of one database must not read as ambiguous when one of them is
// the adapter's.
func TestDatabaseSidecarTakesPrecedenceOverWorkloadLocalSidecar(t *testing.T) {
	out := t.TempDir()
	writeVersionStamp(t, out)
	a := New("database-vs-local-sidecar")
	a.UseForDescribe(&describeDatabaseSidecar{})
	a.UseForDescribe(&describeLedgerSidecar{})
	if err := a.Describe(out, nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(out, "schema", protocaps.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	document, diags := protocaps.ParseAndValidateManifestDocument(data)
	if document == nil || document.V2 == nil || len(diags) != 0 {
		t.Fatalf("manifest: %#v %v", document, diags)
	}
	var databases []protocaps.InfraRequirementV2
	for _, requirement := range document.V2.InfraRequirements {
		if requirement.Kind == protocaps.InfraKindDatabase && requirement.Name == "default" {
			databases = append(databases, requirement)
		}
	}
	if len(databases) != 1 || !strings.Contains(databases[0].Provenance.Declaration.Symbol, "describeDatabaseSidecar") {
		t.Fatalf("database-vs-local-sidecar precedence = %#v", databases)
	}
}

type namedMigrationPlugin struct {
	name string
	src  migration.Source
}

func (p *namedMigrationPlugin) Name() string { return p.name }
func (p *namedMigrationPlugin) MigrationSources() []migration.Source {
	return []migration.Source{p.src}
}

// TestAdapterDeclarationSkipsMigrationProducerResolution pins the
// several-sources-one-database pattern: two migration sources contribute
// namespaced schemas of the same datasource the database adapter declares.
// The migration sidecar's declaration cannot win over the adapter's, so its
// producer must not need to be unique — resolving it eagerly used to fail as
// "ambiguous producer declarations" for a fallback that would be discarded.
func TestAdapterDeclarationSkipsMigrationProducerResolution(t *testing.T) {
	out := t.TempDir()
	writeVersionStamp(t, out)
	a := New("multi-source-database")
	a.Use(&namedMigrationPlugin{name: "iam-feature", src: capMigrationSource{namespace: "iam", database: "default"}})
	a.Use(&namedMigrationPlugin{name: "history-feature", src: capMigrationSource{namespace: "history", database: "default"}})
	a.UseForDescribe(&describeDatabaseSidecar{})
	if err := a.Describe(out, nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(out, "schema", protocaps.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	document, diags := protocaps.ParseAndValidateManifestDocument(data)
	if document == nil || document.V2 == nil || len(diags) != 0 {
		t.Fatalf("manifest: %#v %v", document, diags)
	}
	var databases []protocaps.InfraRequirementV2
	for _, requirement := range document.V2.InfraRequirements {
		if requirement.Kind == protocaps.InfraKindDatabase && requirement.Name == "default" {
			databases = append(databases, requirement)
		}
	}
	if len(databases) != 1 || !strings.Contains(databases[0].Provenance.Declaration.Symbol, "describeDatabaseSidecar") {
		t.Fatalf("multi-source database = %#v", databases)
	}
}

func TestInfraSidecarsResolveConfigAndDescribeOnlyProducers(t *testing.T) {
	out := t.TempDir()
	writeVersionStamp(t, out)
	if err := protoinfra.WriteSidecarIn(out, "secrets", protoinfra.PerProjectManifest{Secrets: []string{"database.default.password"}}); err != nil {
		t.Fatal(err)
	}
	a := New("sidecars")
	a.Use(&capConfigPlugin{name: "config", defs: []config.Descriptor{config.Config[capDBOptions]("database.default").Descriptor()}})
	a.UseForDescribe(&describeOnlyStorage{})
	if err := a.Describe(out, nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(out, "schema", protocaps.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	document, diags := protocaps.ParseAndValidateManifestDocument(data)
	if document == nil || document.V2 == nil || len(diags) != 0 {
		t.Fatalf("manifest: %#v %v", document, diags)
	}
	want := map[string]bool{"secret:database.default.password": false, "storage:attachments": false}
	for _, requirement := range document.V2.InfraRequirements {
		key := string(requirement.Kind) + ":" + requirement.Name
		if _, ok := want[key]; ok {
			want[key] = true
			if requirement.Provenance.Package != "go.putnami.dev/app" || requirement.Provenance.Declaration.Root != protocaps.LocationRootPackage || requirement.Provenance.Declaration.Path == "" {
				t.Errorf("%s imprecise provenance: %#v", key, requirement.Provenance)
			}
		}
	}
	for key, found := range want {
		if !found {
			t.Errorf("missing infra requirement %s", key)
		}
	}
}

func TestCanonicalCapabilitySecretNameMatchesConfigGenerator(t *testing.T) {
	cases := map[string]string{
		"DB_PASSWORD":           "db_password",
		"clientSecret":          "client_secret",
		"auth.clientSecret":     "auth.client_secret",
		"server.http.apiKey":    "server.http.api_key",
		"Weird Name!":           "weird_name_",
		"_leadingUnderscore":    "leading_underscore",
		"STRIPE_WEBHOOK_SECRET": "stripe_webhook_secret",
		strings.Repeat("a", 65): strings.Repeat("a", 64),
	}
	for input, want := range cases {
		if got := canonicalCapabilitySecretName(input); got != want {
			t.Errorf("canonicalCapabilitySecretName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestDependencyMergeCoalescesCanonicalWireCopiesAndRejectsDivergence(t *testing.T) {
	root := t.TempDir()
	workloadRoot := filepath.Join(root, "workload")
	commonBinding := testSourceBinding("common")
	packages := []generatedCapabilityPackage{
		{Package: "example/workload", Version: "1.0.0", SourceRoot: "workload", EvidencePath: "workload/putnami.json", SourceBinding: testSourceBinding("workload")},
		{Package: "example/a", Version: "1.0.0", SourceRoot: "a", EvidencePath: "a/putnami.json", SourceBinding: testSourceBinding("a"), CapabilityManifestPath: "../a/schema/capabilities.json"},
		{Package: "example/b", Version: "1.0.0", SourceRoot: "b", EvidencePath: "b/putnami.json", SourceBinding: testSourceBinding("b"), CapabilityManifestPath: "../b/schema/capabilities.json"},
		{Package: "example/common", Version: "1.0.0", SourceRoot: "common", EvidencePath: "common/putnami.json", SourceBinding: commonBinding},
	}
	identity := protocaps.ContributionIdentity{OwnerProject: "example/common", Kind: protocaps.ContributionKindRequiredCapability, Key: "shared"}
	base := protocaps.RequiredCapabilityV2{Identity: identity, Name: "shared", Requires: []protocaps.CapabilityKind{protocaps.CapabilityKindSchema, protocaps.CapabilityKindConfig}, Provenance: protocaps.ProvenanceV2{Project: "example/common", Package: "example/common", Version: "1.0.0", SourceKind: protocaps.SourceKindFramework, Declaration: protocaps.DeclarationLocation{Root: protocaps.LocationRootProject, Path: "shared.go"}, Artifacts: []protocaps.ArtifactLocation{}}}
	writeDependency := func(project, path string, contribution protocaps.RequiredCapabilityV2) {
		provenance := contribution.Provenance
		m := &protocaps.ManifestV2{Schema: capabilitiesSchemaURL, ProtocolVersion: protocaps.ProtocolVersionV2, Project: project,
			ConfigDefinitions:    []protocaps.ConfigDefinitionV2{{Identity: protocaps.ContributionIdentity{OwnerProject: "example/common", Kind: protocaps.ContributionKindConfig, Key: "shared.config"}, Path: "shared.config", Provenance: provenance}},
			Schemas:              []protocaps.SchemaContributionV2{{Identity: protocaps.ContributionIdentity{OwnerProject: "example/common", Kind: protocaps.ContributionKindSchema, Subkind: string(protocaps.SchemaKindRoute), Key: "shared.route"}, Name: "shared.route", Kind: protocaps.SchemaKindRoute, Path: "/shared", Provenance: provenance}},
			RequiredCapabilities: []protocaps.RequiredCapabilityV2{contribution}}
		data, err := protocaps.MarshalManifestV2(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	aPath := filepath.Join(root, "a", "schema", protocaps.ManifestFilename)
	bPath := filepath.Join(root, "b", "schema", protocaps.ManifestFilename)
	if err := os.MkdirAll(filepath.Dir(aPath), 0o750); err != nil {
		t.Fatal(err)
	}
	v1Data, err := json.Marshal(protocaps.Manifest{Schema: "https://putnami.dev/schemas/putnami-capabilities.json", ProtocolVersion: protocaps.ProtocolVersion, Project: "example/a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(aPath, v1Data, 0o600); err != nil {
		t.Fatal(err)
	}
	preflightInv, err := newCapabilityInventory("example/workload", packages)
	if err != nil {
		t.Fatal(err)
	}
	if err := mergeDependencyCapabilityManifests(workloadRoot, "..", &protocaps.ManifestV2{}, preflightInv); err == nil || !strings.Contains(err.Error(), "protocol v1") {
		t.Fatalf("v1 dependency error = %v", err)
	}
	writeDependency("example/a", aPath, base)
	copy := base
	copy.Requires = []protocaps.CapabilityKind{protocaps.CapabilityKindConfig, protocaps.CapabilityKindSchema}
	copy.Provenance.Artifacts = nil
	writeDependency("example/b", bPath, copy)
	inv, err := newCapabilityInventory("example/workload", packages)
	if err != nil {
		t.Fatal(err)
	}
	target := protocaps.ManifestV2{Schema: capabilitiesSchemaURL, ProtocolVersion: protocaps.ProtocolVersionV2, Project: "example/workload"}
	if err := mergeDependencyCapabilityManifests(workloadRoot, "..", &target, inv); err != nil {
		t.Fatalf("merge wire-equivalent copies: %v", err)
	}
	if len(target.RequiredCapabilities) != 1 {
		t.Fatalf("coalesced copies = %d", len(target.RequiredCapabilities))
	}
	copy.Provenance.Declaration.Symbol = "Divergent"
	writeDependency("example/b", bPath, copy)
	target.RequiredCapabilities = nil
	if err := mergeDependencyCapabilityManifests(workloadRoot, "..", &target, inv); err == nil || !strings.Contains(err.Error(), "example/a, example/b") || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("divergence error = %v", err)
	}
	if err := mergeDependencyCapabilityManifests(workloadRoot, ".", &target, inv); err == nil || !strings.Contains(err.Error(), "escapes the stamped workspace root") {
		t.Fatalf("root escape error = %v", err)
	}
	outside := filepath.Join(t.TempDir(), protocaps.ManifestFilename)
	writeDependency("example/b", outside, base)
	if err := os.Remove(bPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, bPath); err != nil {
		t.Fatal(err)
	}
	if err := mergeDependencyCapabilityManifests(workloadRoot, "..", &target, inv); err == nil || !strings.Contains(err.Error(), "escapes the stamped workspace root") {
		t.Fatalf("symlink escape error = %v", err)
	}
}

func TestCapabilityMergeConflictIncludesLocalAndDependencyOrigins(t *testing.T) {
	identity := protocaps.ContributionIdentity{OwnerProject: "example/common", Kind: protocaps.ContributionKindRequiredCapability, Key: "shared"}
	local := protocaps.RequiredCapabilityV2{Identity: identity, Name: "shared"}
	target := []protocaps.RequiredCapabilityV2{local}
	state, err := newCapabilityMergeState("example/workload", &target, func(value protocaps.RequiredCapabilityV2) protocaps.ContributionIdentity { return value.Identity })
	if err != nil {
		t.Fatal(err)
	}
	if err := state.merge("example/a", []protocaps.RequiredCapabilityV2{local}); err != nil {
		t.Fatal(err)
	}
	divergent := local
	divergent.Name = "different"
	err = state.merge("example/b", []protocaps.RequiredCapabilityV2{divergent})
	if err == nil || !strings.Contains(err.Error(), "example/a, example/b, example/workload") {
		t.Fatalf("origin diagnostic = %v", err)
	}
}

func TestDependencyProvenanceIgnoresHistoricalProducerVersion(t *testing.T) {
	packages := []generatedCapabilityPackage{
		{Package: "example/dependency", Version: "1.0.0", SourceRoot: "dependency", EvidencePath: "dependency/putnami.json", SourceBinding: testSourceBinding("dependency")},
		{Package: "go.putnami.dev/app", Version: "2.0.0", SourceRoot: "app", EvidencePath: "app/putnami.json", SourceBinding: testSourceBinding("app")},
	}
	inv, err := newCapabilityInventory("example/dependency", packages)
	if err != nil {
		t.Fatal(err)
	}
	identity := protocaps.ContributionIdentity{OwnerProject: "example/dependency", Kind: protocaps.ContributionKindSchema, Subkind: string(protocaps.SchemaKindRoute), Key: "route"}
	manifest := &protocaps.ManifestV2{Schemas: []protocaps.SchemaContributionV2{{Identity: identity, Name: "route", Kind: protocaps.SchemaKindRoute, Provenance: protocaps.ProvenanceV2{Project: "example/dependency", Package: "go.putnami.dev/app", Version: "2.0.0", SourceKind: protocaps.SourceKindFramework, Declaration: protocaps.DeclarationLocation{Root: protocaps.LocationRootProject, Path: "routes.go"}}}}}
	if err := validateDependencyProvenance(manifest, inv); err != nil {
		t.Fatalf("dependency provenance: %v", err)
	}
	manifest.Schemas[0].Provenance.Version = "1.0.0"
	if err := validateDependencyProvenance(manifest, inv); err != nil {
		t.Fatalf("historical producer version rejected: %v", err)
	}
}

type runtimeEvidencePlugin struct{ configured, started, stopped int }

func (*runtimeEvidencePlugin) Name() string                               { return "runtime-evidence" }
func (p *runtimeEvidencePlugin) Configure(context.Context, *Module) error { p.configured++; return nil }
func (p *runtimeEvidencePlugin) Start(context.Context, *Module) error     { p.started++; return nil }
func (p *runtimeEvidencePlugin) Stop(context.Context, *Module) error      { p.stopped++; return nil }

// describeWithProof runs describe over the capability-proof app with its
// feature declaration replaced, and reports the error plus whether either
// artifact reached disk. A rejected proof must publish neither.
func describeWithProof(t *testing.T, feature Feature) (error, bool) {
	t.Helper()
	out := t.TempDir()
	writeVersionStamp(t, out)
	a, _ := buildCapabilityApp()
	a.Module.feature = nil
	a.Feature(feature)
	err := a.Describe(out, nil)
	published := false
	for _, path := range []string{
		filepath.Join(out, "schema", protocaps.ManifestFilename),
		filepath.Join(out, "schema", "feature-evidence", featureEvidenceFilename),
	} {
		if _, statErr := os.Stat(path); statErr == nil {
			published = true
		}
	}
	return err, published
}

func TestFeatureProofFailuresRefuseTheWholePublication(t *testing.T) {
	route := ContributionRef{Kind: protocaps.ContributionKindSchema, Subkind: string(protocaps.SchemaKindRoute), Key: "listUsers"}
	base := func(proofs ...FeatureProof) Feature {
		return Feature{
			ID: "capabilities/go-evidence", Name: "Go capability evidence",
			Outcome: "A build proves a feature requirement from an exact capability contribution",
			Owner:   "capabilities", Proves: proofs,
		}
	}
	cases := []struct {
		name    string
		feature Feature
		code    string
	}{
		{"unknown feature", func() Feature {
			f := base(FeatureProof{Requirement: "implementation", Contribution: route})
			f.ID = "capabilities/not-authored"
			return f
		}(), protofeatures.ErrorCodeUnknownFeature},
		{"unknown requirement", base(FeatureProof{Requirement: "not-declared", Contribution: route}), protofeatures.ErrorCodeUnknownRequirement},
		{"unpublished contribution", base(FeatureProof{
			Requirement:  "implementation",
			Contribution: ContributionRef{Kind: protocaps.ContributionKindSchema, Subkind: string(protocaps.SchemaKindRoute), Key: "neverEmitted"},
		}), protocaps.ErrorCodeUnresolvedReference},
		{"contradictory duplicate", base(
			FeatureProof{Requirement: "implementation", Contribution: route},
			FeatureProof{Requirement: "implementation", Contribution: ContributionRef{Kind: route.Kind, Subkind: route.Subkind, Key: route.Key}},
		), ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err, published := describeWithProof(t, testCase.feature)
			if testCase.name == "contradictory duplicate" {
				// Identical proofs coalesce rather than fail: two native
				// declarations may legitimately state the same association.
				if err != nil || !published {
					t.Fatalf("identical proofs must coalesce and publish, got err=%v published=%v", err, published)
				}
				return
			}
			if err == nil {
				t.Fatal("rejected proof must fail describe")
			}
			if !strings.Contains(err.Error(), testCase.code) {
				t.Fatalf("want %s in %v", testCase.code, err)
			}
			if published {
				t.Error("a rejected proof must publish neither artifact")
			}
		})
	}
}

// TestFeatureProofWithoutAResolvableDeclarationSiteFails covers the branch a
// full describe cannot reach: runtime.Caller always resolves inside the test
// binary's own package. A declaration whose call site could not be reduced to a
// contained project path must be named as such, not published with provenance a
// reader cannot follow.
func TestFeatureProofWithoutAResolvableDeclarationSiteFails(t *testing.T) {
	module := NewModule("orphan")
	module.feature = &Feature{
		ID: "capabilities/go-evidence", Name: "Go capability evidence",
		Outcome: "A build proves a feature requirement", Owner: "capabilities",
		Proves: []FeatureProof{{
			Requirement:  "implementation",
			Contribution: ContributionRef{Kind: protocaps.ContributionKindSchema, Subkind: string(protocaps.SchemaKindRoute), Key: "listUsers"},
		}},
	}
	mappings, err := collectFeatureProofMappings(module)
	if err == nil {
		t.Fatalf("an unresolvable declaration site must fail, got %d mappings", len(mappings))
	}
	if !strings.Contains(err.Error(), "declaration site") {
		t.Fatalf("error must name the declaration site, got %v", err)
	}
}

func TestFeatureProofRequiresARequirementID(t *testing.T) {
	module := NewModule("nameless")
	module.feature = &Feature{
		ID: "capabilities/go-evidence", Name: "Go capability evidence",
		Outcome: "A build proves a feature requirement", Owner: "capabilities",
		provenance: designSource{path: "main.go", symbol: "Feature"},
		Proves:     []FeatureProof{{Requirement: "  "}},
	}
	if _, err := collectFeatureProofMappings(module); err == nil || !strings.Contains(err.Error(), "must name a requirement") {
		t.Fatalf("a proof without a requirement must fail, got %v", err)
	}
}

func TestFeatureProofWithoutAnAuthoredManifestFailsPublication(t *testing.T) {
	out := t.TempDir()
	writeVersionStamp(t, out)
	if err := os.Remove(filepath.Join(filepath.Dir(out), protofeatures.ManifestFilename)); err != nil {
		t.Fatal(err)
	}
	a, _ := buildCapabilityApp()
	err := a.Describe(out, nil)
	if err == nil || !strings.Contains(err.Error(), protofeatures.ErrorCodeUnknownFeature) {
		t.Fatalf("a proof without authored intent must fail, got %v", err)
	}
}

func TestFeatureProofMetadataIsRuntimeInert(t *testing.T) {
	counts := func(proves []FeatureProof) (int, int, int) {
		t.Helper()
		plugin := &runtimeEvidencePlugin{}
		a := New("runtime").Run(func(context.Context) error { return nil })
		a.Feature(Feature{
			ID: "capabilities/go-evidence", Name: "Go capability evidence",
			Outcome: "A build proves a feature requirement", Owner: "capabilities", Proves: proves,
		})
		a.Use(plugin)
		if err := a.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := a.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		return plugin.configured, plugin.started, plugin.stopped
	}
	// A malformed proof names a requirement and a contribution that do not
	// exist. It must reach exactly the same runtime as no proof at all.
	withoutConfigured, withoutStarted, withoutStopped := counts(nil)
	withConfigured, withStarted, withStopped := counts([]FeatureProof{{
		Requirement:  "not-a-requirement",
		Contribution: ContributionRef{Kind: "not-a-kind", Key: ""},
	}})
	if withoutConfigured != withConfigured || withoutStarted != withStarted || withoutStopped != withStopped {
		t.Fatalf("proof metadata changed the lifecycle: %d/%d/%d vs %d/%d/%d",
			withoutConfigured, withoutStarted, withoutStopped, withConfigured, withStarted, withStopped)
	}
	if withConfigured != 1 || withStarted != 1 || withStopped != 1 {
		t.Fatalf("lifecycle counts = %d/%d/%d", withConfigured, withStarted, withStopped)
	}
}

func TestMalformedSchedulerMetadataFailsClosed(t *testing.T) {
	bad := [][]generatedCapabilityPackage{
		{}, {{Package: "example/workload", Version: "1.0.0"}},
		{{Package: "example/workload", Version: "workspace:*", SourceRoot: ".", EvidencePath: "putnami.json", SourceBinding: testSourceBinding("x")}},
		{{Package: "example/workload", Version: "1.0.0", SourceRoot: ".", EvidencePath: "putnami.json", SourceBindingUnavailable: true}},
		{{Package: "example/workload", Version: "1.0.0", SourceRoot: "../escape", EvidencePath: "putnami.json", SourceBinding: testSourceBinding("x")}},
	}
	for i, packages := range bad {
		if _, err := newCapabilityInventory("example/workload", packages); err == nil {
			t.Errorf("bad metadata %d accepted", i)
		}
	}
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "version.json"), []byte(`{"name":"example/workload","capabilityPackages":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := New("x").Describe(out, nil); err == nil || !strings.Contains(err.Error(), "scheduler metadata") {
		t.Fatalf("malformed stamp error = %v", err)
	}
}

func TestLegacySchedulerMetadataSkipsV2PublicationAndRemovesStaleArtifacts(t *testing.T) {
	packages := []generatedCapabilityPackage{
		{Package: "example/dependency", Version: "1.0.0", EvidencePath: "libs/dependency/go.mod", CapabilityManifestPath: "../../libs/dependency/schema/capabilities.json"},
		{Package: "example/workload", Version: "1.0.0", EvidencePath: "apps/workload/go.mod"},
	}
	if !legacyCapabilitySchedulerMetadata("example/workload", packages) {
		t.Fatal("complete pre-source-binding scheduler stamp was not recognized")
	}

	out := t.TempDir()
	stamp, err := json.Marshal(map[string]any{
		"name": "example/workload",
		"capabilityPackages": []map[string]string{
			{"package": packages[0].Package, "version": packages[0].Version, "evidencePath": packages[0].EvidencePath, "capabilityManifestPath": packages[0].CapabilityManifestPath},
			{"package": packages[1].Package, "version": packages[1].Version, "evidencePath": packages[1].EvidencePath},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "version.json"), stamp, 0o600); err != nil {
		t.Fatal(err)
	}
	stale := []string{
		filepath.Join(out, "schema", protocaps.ManifestFilename),
		filepath.Join(out, "schema", "feature-evidence", featureEvidenceFilename),
	}
	for _, path := range stale {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := New("x").Describe(out, nil); err != nil {
		t.Fatalf("legacy scheduler compatibility describe: %v", err)
	}
	for _, path := range stale {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("legacy scheduler left generated artifact %s: %v", path, err)
		}
	}
}

func TestLegacySchedulerMetadataRecognitionIsAllOrNothing(t *testing.T) {
	valid := generatedCapabilityPackage{Package: "example/workload", Version: "1.0.0", EvidencePath: "apps/workload/go.mod"}
	bad := [][]generatedCapabilityPackage{
		nil,
		{{Package: valid.Package, Version: valid.Version}},
		{{Package: valid.Package, Version: "workspace:*", EvidencePath: valid.EvidencePath}},
		{valid, valid},
		{{Package: "example/dependency", Version: valid.Version, EvidencePath: "libs/dependency/go.mod"}},
		{{Package: valid.Package, Version: valid.Version, EvidencePath: valid.EvidencePath, SourceRoot: "apps/workload"}},
		{{Package: valid.Package, Version: valid.Version, EvidencePath: valid.EvidencePath, SourceBinding: testSourceBinding("partial")}},
		{{Package: valid.Package, Version: valid.Version, EvidencePath: valid.EvidencePath, SourceBindingUnavailable: true}},
	}
	for index, packages := range bad {
		if legacyCapabilitySchedulerMetadata("example/workload", packages) {
			t.Errorf("malformed/partial scheduler stamp %d was accepted as legacy", index)
		}
	}
}

func TestPartialLegacySchedulerMetadataFailsDescribe(t *testing.T) {
	for name, metadata := range map[string]string{
		"missing binding":    `"sourceRoot":"apps/workload"`,
		"empty binding":      `"sourceRoot":"","sourceBinding":""`,
		"unknown field":      `"unexpected":"value"`,
		"null manifest path": `"capabilityManifestPath":null`,
	} {
		t.Run(name, func(t *testing.T) {
			out := t.TempDir()
			stamp := fmt.Sprintf(`{"name":"example/workload","capabilityPackages":[{"package":"example/workload","version":"1.0.0","evidencePath":"apps/workload/go.mod",%s}]}`, metadata)
			if err := os.WriteFile(filepath.Join(out, "version.json"), []byte(stamp), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := New("x").Describe(out, nil); err == nil || !strings.Contains(err.Error(), "scheduler metadata") {
				t.Fatalf("partial scheduler stamp error = %v", err)
			}
			if _, err := os.Stat(filepath.Join(out, "schema", protocaps.ManifestFilename)); !os.IsNotExist(err) {
				t.Fatalf("partial scheduler stamp published capability manifest: %v", err)
			}
		})
	}
}

// capDomainAccessPlugin is a stand-in for what `go.putnami.dev/app/darc` hands
// the describe pass. The real carrier cannot be used here: darc imports app, so
// an app test that imported darc would close a cycle. What this test owns is the
// EMITTER — that a contributed contract becomes one committed row with the same
// provenance every other contribution carries — and darc's own tests own the
// projection from a validated contract onto the descriptor.
type capDomainAccessPlugin struct{ contracts []DomainAccessContract }

func (*capDomainAccessPlugin) Name() string { return "domain-access" }
func (p *capDomainAccessPlugin) DomainAccessContracts() []DomainAccessContract {
	return p.contracts
}

// TestDescribeEmitsDomainAccessEvidence pins the evidence channel:
// a registered runtime-enforced contract contributes one machine row to the
// project's capability manifest, and describe stays its sole committer.
//
// The row is EVIDENCE. `putnami architecture validate` reads it beside the
// declared imports to tell a declared contract nothing implements from an
// implemented one nobody declared. It never authorizes a cross-domain
// dependency: only a reviewed manifest edit does.
// domainAccessContracts is one contributor's declaration, stated with its
// transports and its rows in a NON-canonical order. Emission has to be a
// function of the contracts, not of the order a plugin happened to list them in.
func domainAccessContracts() []DomainAccessContract {
	return []DomainAccessContract{
		{
			Import: "capabilities.workspace-context.v1",
			Mode:   "projection",
			Status: "active",
			Transports: []DomainAccessTransport{
				{Role: "updates", Kind: "event", Contract: "runtime.binding-changed.v1", Availability: "active"},
				{Role: "bootstrap", Kind: "api", Contract: "runtime.bindings.v1", Availability: "active"},
			},
			Enforced: DomainAccessEnforcement{
				MaxStaleness: "5m", OnMissing: "fail-closed", OnStale: "use-stale",
				Ordering: "source-version", LateEvents: "ignore-older", Deletion: "tombstone",
				Writer: "capabilities.projector", Rebuild: "bootstrap",
			},
		},
		{Import: "capabilities.protocol-contracts.v1", Mode: "reference", Status: "active"},
	}
}

// describeWithDomainAccess runs one whole describe over a fresh output
// directory and returns the committed capability manifest.
func describeWithDomainAccess(t *testing.T) []byte {
	t.Helper()
	out := t.TempDir()
	writeVersionStamp(t, out)
	a, _ := buildCapabilityApp()
	a.Use(&capDomainAccessPlugin{contracts: domainAccessContracts()})
	if err := a.Describe(out, nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(out, "schema", protocaps.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestDescribeEmitsDomainAccessEvidenceDeterministically pins that the rows are
// committed content: three describes of one app produce byte-identical
// manifests, and the file is exactly what the protocol's canonical writer
// produces.
//
// The general determinism test above cannot cover this — it describes an app
// with no domain-access contributor — and a committed artifact that moved
// between two runs of one build would re-stamp on every CI job.
func TestDescribeEmitsDomainAccessEvidenceDeterministically(t *testing.T) {
	spectest.Proves(t, "go/application-lifecycle", "darc-runtime-evidence",
		"the-emitted-evidence-rows-are-deterministic-bytes")
	first := describeWithDomainAccess(t)
	if !bytes.Contains(first, []byte(`"domainAccess"`)) {
		t.Fatalf("the manifest carries no domainAccess rows:\n%s", first)
	}
	document, diags := protocaps.ParseAndValidateManifestDocument(first)
	if document == nil || document.V2 == nil || len(diags) != 0 {
		t.Fatalf("v2 manifest: %#v %v", document, diags)
	}
	canonical, err := protocaps.MarshalManifestV2(document.V2)
	if err != nil || !bytes.Equal(first, canonical) {
		t.Fatalf("the emitted manifest is not the canonical rendering: %v", err)
	}
	for run := 0; run < 3; run++ {
		if again := describeWithDomainAccess(t); !bytes.Equal(first, again) {
			t.Fatalf("run %d produced different bytes:\n%s\n%s", run, first, again)
		}
	}
}

func TestDescribeEmitsDomainAccessEvidence(t *testing.T) {
	data := describeWithDomainAccess(t)
	manifest, diags := protocaps.ParseAndValidateManifestDocument(data)
	if manifest == nil || manifest.V2 == nil {
		t.Fatalf("the emitted manifest does not validate: %v", diags)
	}
	rows := manifest.V2.DomainAccess
	if len(rows) != 2 {
		t.Fatalf("domainAccess rows = %d, want one per contributed contract:\n%s", len(rows), data)
	}
	// Canonical order is the protocol's contribution identity — kind, then
	// subkind, then key — and the mode IS the subkind, so one project can enforce
	// two modes of one import without a collision. `projection` therefore sorts
	// before `reference`, whatever order the contributor listed them in.
	projection, reference := rows[0], rows[1]
	if projection.Identity.Subkind != "projection" || reference.Identity.Subkind != "reference" {
		t.Fatalf("rows are not in canonical identity order: %+v", rows)
	}
	if reference.Identity.Kind != protocaps.ContributionKindDomainAccess {
		t.Errorf("identity = %+v, want a domainAccess contribution kind", reference.Identity)
	}
	if reference.Identity.OwnerProject != capabilityProofProject {
		t.Errorf("owner = %q, want the workload that enforces the contract", reference.Identity.OwnerProject)
	}
	if reference.Identity.Key != reference.Import {
		t.Errorf("identity key = %q, want the import ID %q", reference.Identity.Key, reference.Import)
	}
	if reference.Enforced != nil {
		t.Errorf("enforced = %+v, want absent: a reference enforces none of those parameters", reference.Enforced)
	}
	if len(reference.Transports) != 0 {
		t.Errorf("transports = %+v, want none for a reference", reference.Transports)
	}

	if projection.Import != "capabilities.workspace-context.v1" || projection.Mode != "projection" || projection.Status != "active" {
		t.Errorf("projection row = %+v, want the declared import, mode and status verbatim", projection)
	}
	// Transports are sorted by role, so the order the contributor listed them in
	// is not observable in a committed file.
	if len(projection.Transports) != 2 || projection.Transports[0].Role != "bootstrap" || projection.Transports[1].Role != "updates" {
		t.Errorf("transports = %+v, want bootstrap then updates", projection.Transports)
	}
	if projection.Enforced == nil || projection.Enforced.MaxStaleness != "5m" || projection.Enforced.Writer != "capabilities.projector" {
		t.Errorf("enforced = %+v, want the declared parameters verbatim", projection.Enforced)
	}
	if projection.Provenance.SourceKind != protocaps.SourceKindFramework || projection.Provenance.Declaration.Path == "" {
		t.Errorf("provenance = %+v, want the same framework provenance every contribution carries", projection.Provenance)
	}
}

// TestDescribeEmitsNoDomainAccessWithoutAComponent is the other half of the
// channel: a project that registers no runtime-enforced contract publishes no
// row, so "declared but not implemented" stays a real finding instead of being
// masked by an empty collection.
func TestDescribeEmitsNoDomainAccessWithoutAComponent(t *testing.T) {
	data, _, _ := describeCapabilityArtifacts(t)
	manifest, _ := protocaps.ParseAndValidateManifestDocument(data)
	if manifest == nil || manifest.V2 == nil {
		t.Fatal("the emitted manifest does not validate")
	}
	if len(manifest.V2.DomainAccess) != 0 {
		t.Errorf("domainAccess = %+v, want none without a registered component", manifest.V2.DomainAccess)
	}
	if bytes.Contains(data, []byte("domainAccess")) {
		t.Error("the manifest carries an empty domainAccess key; an absent collection must stay absent")
	}
}
