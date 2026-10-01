package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"strings"
	"testing"

	protocaps "go.putnami.dev/protocol/capabilities"
	protoinfra "go.putnami.dev/protocol/infra"
)

// The published contributor these tests describe is go.putnami.dev/events
// consumed as a RELEASED module. A test binary records only its own main module,
// so the published module graph is injected instead of linked — the resolution
// rule under test is the same either way.
const (
	externalEventsModule  = "go.putnami.dev/events"
	externalEventsVersion = "v0.1.0-8dc640802"
	externalEventsSymbol  = "go.putnami.dev/events.(*Plugin).Describe"

	// externalForkedModule is required by the workload and redirected by a
	// VERSIONED replace to externalForkModule: still a published contributor,
	// but imported under one path and compiled from another.
	externalForkedModule = "go.putnami.dev/queue"
	externalForkModule   = "example.com/fork/queue"
	externalForkVersion  = "v1.2.0"
)

func withPublishedModules(t *testing.T, modules ...publishedModule) {
	t.Helper()
	previous := capabilityPublishedModules
	capabilityPublishedModules = func() []publishedModule { return modules }
	t.Cleanup(func() { capabilityPublishedModules = previous })
}

// externalEventsPlugin stands in for a describer compiled from the published
// events module. capabilityDeclarationHint reports the compile-time source path
// the linker would have recorded, which is the only input the ownership rule
// takes from the producer.
type externalEventsPlugin struct{ file string }

func (*externalEventsPlugin) Name() string { return "events" }

func (p *externalEventsPlugin) capabilityDeclarationHint() (string, string, bool) {
	return p.file, externalEventsSymbol, true
}

func (*externalEventsPlugin) Describe(ctx *DescribeContext) error {
	return protoinfra.WriteSidecarIn(ctx.OutputDir, "events", protoinfra.PerProjectManifest{
		Events: &protoinfra.Events{Publishes: []string{"user.created"}},
	})
}

// TestPublishedModulesOfKeepsOnlyReleasedDependencies pins which dependencies
// are external contributors. The discriminator is NOT "is it replaced" — Go has
// two replacement forms and only one of them supplies local source. A versioned
// replacement redirects to another released module and stays external; a
// directory replacement (or a go.work sibling) is workspace source and must not
// satisfy a lookup a missing stamp should report.
func TestPublishedModulesOfKeepsOnlyReleasedDependencies(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{Path: "example.com/workload", Version: "(devel)"},
		Deps: []*debug.Module{
			{Path: externalEventsModule, Version: externalEventsVersion, Sum: "h1:0000"},
			{Path: externalEventsModule, Version: externalEventsVersion},
			{Path: "go.putnami.dev/app", Version: "(devel)"},
			{Path: "go.putnami.dev/sibling", Version: "v1.0.0", Replace: &debug.Module{Path: "../sibling"}},
			{Path: "go.putnami.dev/relative", Version: "v1.0.0", Replace: &debug.Module{Path: "./vendored-fork"}},
			// An absolute directory carries no version by Go's own rule; reject
			// it on the path even if one is somehow present.
			{Path: "go.putnami.dev/absolute", Version: "v1.0.0", Replace: &debug.Module{Path: "/opt/src/fork", Version: "v9.9.9"}},
			{Path: "go.putnami.dev/devel-fork", Version: "v1.0.0", Replace: &debug.Module{Path: "example.com/fork", Version: "(devel)"}},
			{Path: externalForkedModule, Version: "v1.0.0", Replace: &debug.Module{Path: externalForkModule, Version: externalForkVersion}},
			{Path: "go.putnami.dev/unresolved", Version: "^1.0.0"},
			{Path: "go.putnami.dev/unversioned", Version: ""},
			{Path: "", Version: "v1.0.0"},
			nil,
		},
	}
	got := publishedModulesOf(info)
	want := []publishedModule{
		{Path: externalEventsModule, Version: externalEventsVersion},
		{Path: externalForkModule, Version: externalForkVersion, ImportPath: externalForkedModule},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("publishedModulesOf = %#v, want %#v", got, want)
	}
	if modules := publishedModulesOf(nil); modules != nil {
		t.Fatalf("nil build info = %#v, want no modules", modules)
	}
}

// TestVersionedReplacementResolvesTheSupplyingModule covers the replacement form
// that stays external. The recorded identity must name the module that supplied
// the code, while the import path — which a replacement never changes — has to
// keep answering package lookups and vendored declarations, because `go mod
// vendor` files source under the path it is imported by.
func TestVersionedReplacementResolvesTheSupplyingModule(t *testing.T) {
	withPublishedModules(t, publishedModule{Path: externalForkModule, Version: externalForkVersion, ImportPath: externalForkedModule})
	workloadBinding := testSourceBinding("workload")
	inv, err := newCapabilityInventory("example/workload", []generatedCapabilityPackage{
		{Package: "example/workload", Version: "1.0.0", SourceRoot: "workload", EvidencePath: "workload/putnami.json", SourceBinding: workloadBinding},
	})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := inv.identity(protocaps.ContributionKindInfra, string(protocaps.InfraKindEvents), "user.created")
	if err != nil {
		t.Fatal(err)
	}
	want := protocaps.ProvenanceV2{
		Project:    "example/workload",
		Package:    externalForkModule,
		SourceKind: protocaps.SourceKindFramework,
		Declaration: protocaps.DeclarationLocation{
			Root: protocaps.LocationRootPackage, Path: "plugin.go", Symbol: externalEventsSymbol,
		},
	}
	for _, file := range []string{
		// The module cache holds the module that actually supplied the code...
		"/home/ci/go/pkg/mod/" + externalForkModule + "@" + externalForkVersion + "/plugin.go",
		externalForkModule + "@" + externalForkVersion + "/plugin.go",
		// ...while the vendor tree files it under the requirement it replaced,
		// recording neither the replacement nor the version.
		"/build/consumer/vendor/" + externalForkedModule + "/plugin.go",
	} {
		got, provenanceErr := inv.provenance(&externalEventsPlugin{file: file}, identity, "Describe")
		if provenanceErr != nil {
			t.Fatalf("provenance for %q: %v", file, provenanceErr)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("provenance for %q = %#v, want %#v", file, got, want)
		}
	}
	// Package lookups answer on both paths: the runtime reports the import path
	// for a live producer, while an already-recorded manifest names the module
	// that supplied the code.
	for _, name := range []string{
		externalForkedModule, externalForkedModule + "/topics",
		externalForkModule, externalForkModule + "/topics",
	} {
		owner, ownerErr := inv.ownerForPackageName(name)
		if ownerErr != nil || owner.Package != externalForkModule || owner.Version != externalForkVersion {
			t.Fatalf("ownerForPackageName(%q) = %#v, %v, want the supplying module", name, owner, ownerErr)
		}
	}
}

// TestModuleRelativeSourcePathHonorsCacheEscaping pins the module-cache path
// encoding: the cache lower-cases upper-case letters behind "!" so two module
// paths differing only in case cannot collide on a case-insensitive filesystem,
// while a -trimpath build keeps the canonical path. Missing either encoding
// would leave a contributor unresolvable on one build mode only.
func TestModuleRelativeSourcePathHonorsCacheEscaping(t *testing.T) {
	module := publishedModule{Path: "github.com/BurntSushi/toml", Version: "v1.4.0"}
	for _, test := range []struct {
		file string
		want string
	}{
		{"/home/ci/go/pkg/mod/github.com/!burnt!sushi/toml@v1.4.0/decode.go", "decode.go"},
		{"github.com/BurntSushi/toml@v1.4.0/internal/tag/add.go", "internal/tag/add.go"},
	} {
		got, ok := moduleRelativeSourcePath(test.file, module)
		if !ok || got != test.want {
			t.Errorf("moduleRelativeSourcePath(%q) = %q, %v, want %q", test.file, got, ok, test.want)
		}
	}
	for _, file := range []string{
		"/home/ci/go/pkg/mod/github.com/!burnt!sushi/toml@v1.3.0/decode.go",
		"/home/ci/src/notgithub.com/BurntSushi/toml@v1.4.0/decode.go",
		"",
	} {
		if got, ok := moduleRelativeSourcePath(file, module); ok {
			t.Errorf("moduleRelativeSourcePath(%q) = %q, want no match", file, got)
		}
	}
}

// TestPublishedContributorProvenanceIsBuildModeIndependent pins the whole point
// of the rule: the provenance of a published contributor must be a function of
// the module graph alone. A module-cache path, a -trimpath path, and a vendored
// path are the same contribution compiled three ways, so all three must produce
// byte-identical provenance — no GOMODCACHE, no host path, no build mode.
func TestPublishedContributorProvenanceIsBuildModeIndependent(t *testing.T) {
	withPublishedModules(t, publishedModule{Path: externalEventsModule, Version: externalEventsVersion})
	workloadBinding := testSourceBinding("workload")
	inv, err := newCapabilityInventory("example/workload", []generatedCapabilityPackage{
		{Package: "example/workload", Version: "1.0.0", SourceRoot: "workload", EvidencePath: "workload/putnami.json", SourceBinding: workloadBinding},
	})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := inv.identity(protocaps.ContributionKindInfra, string(protocaps.InfraKindEvents), "user.created")
	if err != nil {
		t.Fatal(err)
	}
	want := protocaps.ProvenanceV2{
		Project:    "example/workload",
		Package:    externalEventsModule,
		SourceKind: protocaps.SourceKindFramework,
		Declaration: protocaps.DeclarationLocation{
			Root: protocaps.LocationRootPackage, Path: "internal/topics/plugin.go", Symbol: externalEventsSymbol,
		},
	}
	for _, file := range []string{
		"/Users/someone/go/pkg/mod/" + externalEventsModule + "@" + externalEventsVersion + "/internal/topics/plugin.go",
		externalEventsModule + "@" + externalEventsVersion + "/internal/topics/plugin.go",
		"/build/consumer/vendor/" + externalEventsModule + "/internal/topics/plugin.go",
	} {
		got, provenanceErr := inv.provenance(&externalEventsPlugin{file: file}, identity, "Describe")
		if provenanceErr != nil {
			t.Fatalf("provenance for %q: %v", file, provenanceErr)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("provenance for %q = %#v, want %#v", file, got, want)
		}
	}

	// The entry must also be legal on the wire: a package-root declaration
	// carries its producer package without embedding the resolved module
	// version in the stable manifest.
	manifest := &protocaps.ManifestV2{
		ProtocolVersion: protocaps.ProtocolVersionV2, Project: "example/workload",
		InfraRequirements: []protocaps.InfraRequirementV2{{
			Identity: identity, Name: "user.created", Kind: protocaps.InfraKindEvents, Provenance: want,
		}},
	}
	if diags := protocaps.ValidateManifestV2(manifest); len(diags) > 0 {
		t.Fatalf("published-module provenance is not wire-legal: %v", diags)
	}
}

// TestWorkspaceOwnershipWinsOverPublishedModule is the no-regression guard: a
// contributor whose source the scheduler stamped keeps resolving to its exact
// workspace project even when a published module claims the same import path.
func TestWorkspaceOwnershipWinsOverPublishedModule(t *testing.T) {
	withPublishedModules(t,
		publishedModule{Path: externalEventsModule, Version: externalEventsVersion},
		publishedModule{Path: "go.putnami.dev/app", Version: "v9.9.9"},
	)
	appBinding := testSourceBinding("app")
	inv, err := newCapabilityInventory("go.putnami.dev/app", []generatedCapabilityPackage{{
		Package: "go.putnami.dev/app", Version: "1.0.0", SourceRoot: "go/framework/app",
		EvidencePath: "go/framework/app/putnami.json", SourceBinding: appBinding,
	}})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := inv.identity(protocaps.ContributionKindHealth, string(protocaps.ProbeKindHealth), "workspace")
	if err != nil {
		t.Fatal(err)
	}
	// capHealthPlugin is compiled from this repository, so its method source is
	// under the stamped source root, not inside any module.
	provenance, err := inv.provenance(&capHealthPlugin{name: "workspace"}, identity, "CheckHealth")
	if err != nil {
		t.Fatal(err)
	}
	if provenance.Package != "go.putnami.dev/app" ||
		provenance.Declaration.Root != protocaps.LocationRootProject {
		t.Fatalf("workspace-owned provenance = %#v, want the stamped project", provenance)
	}
}

// TestUnownedContributorStaysActionable keeps the failure mode the rule must
// NOT swallow: a contributor that is neither stamped nor published is a real
// workspace misconfiguration and still fails closed.
func TestUnownedContributorStaysActionable(t *testing.T) {
	withPublishedModules(t, publishedModule{Path: externalEventsModule, Version: externalEventsVersion})
	inv, err := newCapabilityInventory("example/workload", []generatedCapabilityPackage{
		{Package: "example/workload", Version: "1.0.0", SourceRoot: "workload", EvidencePath: "workload/putnami.json", SourceBinding: testSourceBinding("workload")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inv.ownerFor(protocaps.ContributionIdentity{}); err == nil ||
		!strings.Contains(err.Error(), "published module") {
		t.Fatalf("contributor resolution error = %v, want an actionable published-module report", err)
	}
	if _, err := inv.ownerForPackageName("example.com/unowned"); err == nil ||
		!strings.Contains(err.Error(), "published module") {
		t.Fatalf("producer resolution error = %v, want an actionable published-module report", err)
	}
	// A file that names the module but escapes its root is not a declaration
	// inside that module either.
	owner := externalCapabilityPackage(publishedModule{Path: externalEventsModule, Version: externalEventsVersion})
	if _, err := declarationPathForPackage(externalEventsModule+"@"+externalEventsVersion+"/", owner); err == nil {
		t.Fatal("an empty module-relative declaration path must not resolve")
	}
}

// TestDependencyManifestCarriesPublishedModuleProvenance covers the aggregation
// half: a workspace dependency that itself composes the published module ships
// exactly this provenance pairing, and merging it into the workload must accept
// it without demanding a scheduler stamp the module can never have.
func TestDependencyManifestCarriesPublishedModuleProvenance(t *testing.T) {
	withPublishedModules(t, publishedModule{Path: externalEventsModule, Version: externalEventsVersion})
	dependencyBinding := testSourceBinding("dependency")
	inv, err := newCapabilityInventory("example/workload", []generatedCapabilityPackage{
		{Package: "example/workload", Version: "1.0.0", SourceRoot: "workload", EvidencePath: "workload/putnami.json", SourceBinding: testSourceBinding("workload")},
		{Package: "example/dependency", Version: "1.0.0", SourceRoot: "dependency", EvidencePath: "dependency/putnami.json", SourceBinding: dependencyBinding},
	})
	if err != nil {
		t.Fatal(err)
	}
	identity := protocaps.ContributionIdentity{OwnerProject: "example/dependency", Kind: protocaps.ContributionKindInfra, Subkind: string(protocaps.InfraKindEvents), Key: "user.created"}
	provenance := protocaps.ProvenanceV2{
		Project: "example/dependency", Package: externalEventsModule,
		SourceKind:  protocaps.SourceKindFramework,
		Declaration: protocaps.DeclarationLocation{Root: protocaps.LocationRootPackage, Path: "plugin.go", Symbol: externalEventsSymbol},
	}
	manifest := &protocaps.ManifestV2{InfraRequirements: []protocaps.InfraRequirementV2{{
		Identity: identity, Name: "user.created", Kind: protocaps.InfraKindEvents, Provenance: provenance,
	}}}
	if err := validateDependencyProvenance(manifest, inv); err != nil {
		t.Fatalf("published-module dependency provenance rejected: %v", err)
	}
	// Historical manifests may still carry a resolved version. It is no longer
	// part of stable provenance and must not make an otherwise resolvable
	// producer fail aggregation.
	manifest.InfraRequirements[0].Provenance.Version = "v0.0.9-stale"
	if err := validateDependencyProvenance(manifest, inv); err != nil {
		t.Fatalf("historical published version rejected: %v", err)
	}

	// A module this build does not link at all remains unresolvable.
	manifest.InfraRequirements[0].Provenance.Package = "example.com/never-linked"
	if err := validateDependencyProvenance(manifest, inv); err == nil || !strings.Contains(err.Error(), "published module") {
		t.Fatalf("unknown producer = %v, want an actionable published-module report", err)
	}
}

// writeExternalWorkloadStamp stamps the workload project and nothing else: the
// scheduler reaches workspace projects through putnami.json, so a workload that
// consumes released framework modules has exactly one entry.
func writeExternalWorkloadStamp(t *testing.T, outDir string) {
	t.Helper()
	stamp := generatedVersionInfo{Name: capabilityProofProject, CapabilityPackages: []generatedCapabilityPackage{{
		Package: capabilityProofProject, Version: "0.1.0", SourceRoot: "go/samples/capabilities-proof",
		EvidencePath: "go/samples/capabilities-proof/putnami.json", SourceBinding: testSourceBinding("workload"),
	}}}
	data, err := json.Marshal(stamp)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "version.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestDescribePublishesExternalContributorOwnership is the end-to-end proof: a
// workload whose only infra producer is a published module completes describe
// and publishes a valid manifest that addresses the producer by module identity.
// Without that module graph the same describe fails closed, which is the
// regression this rule repairs and the report it must keep for a genuinely
// unowned contributor.
func TestDescribePublishesExternalContributorOwnership(t *testing.T) {
	producer := &externalEventsPlugin{
		file: "/Users/someone/go/pkg/mod/" + externalEventsModule + "@" + externalEventsVersion + "/plugin.go",
	}
	withPublishedModules(t)
	unresolved := t.TempDir()
	writeExternalWorkloadStamp(t, unresolved)
	blind := New("external-events")
	blind.UseForDescribe(producer)
	err := blind.Describe(unresolved, nil)
	if err == nil || !strings.Contains(err.Error(), "is not provided by a published module") {
		t.Fatalf("describe without a module graph = %v, want an actionable ownership failure", err)
	}

	withPublishedModules(t, publishedModule{Path: externalEventsModule, Version: externalEventsVersion})
	out := t.TempDir()
	writeExternalWorkloadStamp(t, out)
	a := New("external-events")
	a.UseForDescribe(producer)
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
	var found *protocaps.InfraRequirementV2
	for index, requirement := range document.V2.InfraRequirements {
		if requirement.Kind == protocaps.InfraKindEvents && requirement.Name == "user.created" {
			found = &document.V2.InfraRequirements[index]
		}
	}
	if found == nil {
		t.Fatalf("published module contribution missing: %#v", document.V2.InfraRequirements)
	}
	want := protocaps.ProvenanceV2{
		Project: capabilityProofProject, Package: externalEventsModule,
		SourceKind:  protocaps.SourceKindFramework,
		Declaration: protocaps.DeclarationLocation{Root: protocaps.LocationRootPackage, Path: "plugin.go", Symbol: externalEventsSymbol},
	}
	if !reflect.DeepEqual(found.Provenance, want) {
		t.Fatalf("published contributor provenance = %#v, want %#v", found.Provenance, want)
	}
}
