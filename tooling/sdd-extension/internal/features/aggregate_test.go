package features

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	capabilityproto "go.putnami.dev/protocol/capabilities"
	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	workspaceproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

type memoryReader struct {
	files       map[string][]byte
	fileErrors  map[string]error
	bindings    map[string]string
	reverseDirs bool
}

func (reader *memoryReader) ReadFile(root, relative string) ([]byte, error) {
	key := joinWorkspacePath(root, relative)
	if err := reader.fileErrors[key]; err != nil {
		return nil, err
	}
	data, ok := reader.files[key]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return append([]byte(nil), data...), nil
}

func (reader *memoryReader) ReadDir(root, relative string) ([]DirEntry, error) {
	prefix := joinWorkspacePath(root, relative) + "/"
	entries := make(map[string]bool)
	for filename := range reader.files {
		if !strings.HasPrefix(filename, prefix) {
			continue
		}
		remainder := strings.TrimPrefix(filename, prefix)
		name, rest, _ := strings.Cut(remainder, "/")
		entries[name] = entries[name] || rest != ""
	}
	if len(entries) == 0 {
		return nil, fs.ErrNotExist
	}
	result := make([]DirEntry, 0, len(entries))
	for name, directory := range entries {
		result = append(result, DirEntry{Name: name, IsDirectory: directory})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	if reader.reverseDirs {
		for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
			result[left], result[right] = result[right], result[left]
		}
	}
	return result, nil
}

func (reader *memoryReader) SourceBinding(root string) (string, error) {
	binding, ok := reader.bindings[root]
	if !ok {
		return "", errors.New("binding unavailable")
	}
	return binding, nil
}

func TestAggregateDependencyCopiesAreStableAndUnclassifiedRemainVisible(t *testing.T) {
	var golden []byte
	for iteration := 0; iteration < 20; iteration++ {
		request := dependencyFixture(t, iteration%3 == 0, false)
		result := Aggregate(request)
		if diag.HasErrors(result.Diagnostics) {
			t.Fatalf("Aggregate diagnostics contain errors: %#v", result.Diagnostics)
		}
		if result.Snapshot == nil {
			t.Fatal("Aggregate did not publish a snapshot")
		}
		if got := len(result.Snapshot.Features); got != 1 {
			t.Fatalf("Features = %d, want 1", got)
		}
		feature := result.Snapshot.Features[0]
		if feature.Current != featureproto.MaturityCoded {
			t.Fatalf("Current = %q, want coded", feature.Current)
		}
		requirement := feature.Requirements[0]
		if requirement.State != VerificationVerified || len(requirement.Evidence) != 1 {
			t.Fatalf("Requirement = %#v, want one verified evidence record", requirement)
		}
		contribution := requirement.Evidence[0].Contribution
		if contribution == nil {
			t.Fatal("Capability evidence did not retain its resolved contribution")
		}
		if requirement.Evidence[0].Document != "app-a/schema/feature-evidence/a.json" {
			t.Fatalf("Evidence document = %q", requirement.Evidence[0].Document)
		}
		wantContainers := []string{
			"app-a/schema/capabilities.json",
			"app-b/schema/capabilities.json",
			"dep/schema/capabilities.json",
		}
		if !equalStrings(contribution.Containers, wantContainers) {
			t.Fatalf("Containers = %#v, want %#v", contribution.Containers, wantContainers)
		}
		if got := len(result.Snapshot.Unclassified); got != 1 {
			t.Fatalf("Unclassified = %d, want 1", got)
		}
		if result.Snapshot.Unclassified[0].Identity.OwnerProject != "app-a" || result.Snapshot.Unclassified[0].Identity.Key != "orphan" {
			t.Fatalf("Unclassified contribution = %#v", result.Snapshot.Unclassified[0])
		}
		if countDiagnostic(result.Diagnostics, featureproto.WarningCodeUnclassifiedContribution) != 1 {
			t.Fatalf("Diagnostics = %#v, want one unclassified warning", result.Diagnostics)
		}
		data, err := MarshalSnapshot(result.Snapshot)
		if err != nil {
			t.Fatalf("MarshalSnapshot: %v", err)
		}
		if iteration == 0 {
			golden = data
		} else if !bytes.Equal(data, golden) {
			t.Fatalf("iteration %d produced nondeterministic bytes\nfirst:\n%s\ncurrent:\n%s", iteration, golden, data)
		}
	}
}

func TestAggregateFeatureAuthorityControlsOnlyUnclassifiedContributions(t *testing.T) {
	for _, test := range []struct {
		name               string
		authority          *workspaceproto.ProjectFeatureAuthority
		wantUnclassified   int
		wantUnresolvedCode int
	}{
		{name: "no authority", wantUnclassified: 1},
		{
			name:      "reviewed none",
			authority: &workspaceproto.ProjectFeatureAuthority{None: "the producer is deliberately technical support code"},
		},
		{
			name:      "resolved owner",
			authority: &workspaceproto.ProjectFeatureAuthority{Owner: "demo/outcome"},
		},
		{
			name:               "unresolved owner",
			authority:          &workspaceproto.ProjectFeatureAuthority{Owner: "demo/missing"},
			wantUnclassified:   1,
			wantUnresolvedCode: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := Aggregate(featureAuthorityFixture(t, test.authority))
			if diag.HasErrors(result.Diagnostics) || result.Snapshot == nil {
				t.Fatalf("Aggregate = %#v", result)
			}
			if got := len(result.Snapshot.Unclassified); got != test.wantUnclassified {
				t.Fatalf("Unclassified = %d, want %d", got, test.wantUnclassified)
			}
			if got := countDiagnostic(result.Diagnostics, featureproto.WarningCodeUnclassifiedContribution); got != test.wantUnclassified {
				t.Errorf("unclassified diagnostics = %d, want %d: %#v", got, test.wantUnclassified, result.Diagnostics)
			}
			if got := countDiagnostic(result.Diagnostics, featureproto.WarningCodeUnresolvedFeatureAuthority); got != test.wantUnresolvedCode {
				t.Errorf("unresolved-authority diagnostics = %d, want %d: %#v", got, test.wantUnresolvedCode, result.Diagnostics)
			}

			// Authority changes only how the otherwise-unmapped contribution is
			// classified. The mapped contribution must retain the exact evidence
			// and maturity assessment in every case.
			if got := len(result.Snapshot.Features); got != 1 {
				t.Fatalf("Features = %d, want 1", got)
			}
			requirement := result.Snapshot.Features[0].Requirements[0]
			if requirement.State != VerificationVerified || len(requirement.Evidence) != 1 {
				t.Fatalf("Requirement = %#v, want one verified evidence record", requirement)
			}
			if evidence := requirement.Evidence[0]; evidence.State != EvidenceActive ||
				evidence.Contribution == nil || evidence.Contribution.Identity.Key != "mapped" {
				t.Fatalf("Evidence = %#v, want the active mapped contribution", evidence)
			}
		})
	}
}

func TestAggregateFeatureRootCannotSuppressUnmappedContributionsWithFeatureAuthority(t *testing.T) {
	request := dependencyFixture(t, false, false)
	project := request.Workspace.ProjectByName("app-a")
	project.Config = &workspaceproto.ProjectConfig{
		Name: "app-a",
		FeatureAuthority: &workspaceproto.ProjectFeatureAuthority{
			None: "this declaration must not override the feature authored at this root",
		},
	}
	result := Aggregate(request)
	if diag.HasErrors(result.Diagnostics) || result.Snapshot == nil {
		t.Fatalf("Aggregate = %#v", result)
	}
	if got := len(result.Snapshot.Unclassified); got != 1 {
		t.Fatalf("Unclassified = %d, want the authored root's orphan", got)
	}
	if got := countDiagnostic(result.Diagnostics, featureproto.WarningCodeUnclassifiedContribution); got != 1 {
		t.Fatalf("unclassified diagnostics = %d, want 1: %#v", got, result.Diagnostics)
	}
}

func TestAggregateConflictingDependencyCopiesFailClosed(t *testing.T) {
	result := Aggregate(dependencyFixture(t, true, true))
	if result.Snapshot != nil {
		t.Fatal("conflicting contribution copies published a snapshot")
	}
	if !hasDiagnostic(result.Diagnostics, capabilityproto.ErrorCodeConflictingContributionCopy) {
		t.Fatalf("Diagnostics = %#v, want conflicting-copy error", result.Diagnostics)
	}
	encoded, err := json.Marshal(result.Diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	for _, container := range []string{"app-a/schema/capabilities.json", "app-b/schema/capabilities.json", "dep/schema/capabilities.json"} {
		if !bytes.Contains(encoded, []byte(container)) {
			t.Errorf("diagnostics do not report container %q: %s", container, encoded)
		}
	}
}

func TestAggregateReadsV1WithoutInventingMigrationIdentity(t *testing.T) {
	binding := sourceBinding('7')
	manifest := featureproto.Manifest{
		ProtocolVersion: featureproto.MinimumManifestProtocolVersion,
		Namespace:       "legacy",
		Features: []featureproto.Feature{{
			ID: "legacy/service", Type: featureproto.FeatureTypeFeature, Name: "Legacy service", Outcome: "The service remains inspectable", Owner: "platform", Target: featureproto.MaturityCoded,
			Requirements: []featureproto.Requirement{{ID: "implementation", Stage: featureproto.MaturityCoded, EvidenceKinds: []featureproto.EvidenceKind{featureproto.EvidenceKindCapability}}},
		}},
	}
	reference := capabilityproto.ContributionReference{OwnerProject: "dep", Kind: capabilityproto.ContributionKindConfig, Key: "service"}
	evidence := featureproto.EvidenceDocument{ProtocolVersion: featureproto.EvidenceProtocolVersion, Evidence: []featureproto.EvidenceRecord{{
		ID: "legacy/service-proof", Feature: "legacy/service", Requirement: "implementation", Stage: featureproto.MaturityCoded, Outcome: featureproto.EvidenceOutcomeSupports,
		Issuer:     featureproto.Issuer{Kind: featureproto.IssuerKindBuild, ID: "legacy-build"},
		Source:     featureproto.SourceSelector{Root: featureproto.LocationRootProject, OwnerProject: "dep", Binding: binding},
		Subject:    featureproto.EvidenceSubject{Kind: featureproto.EvidenceKindCapability, Contribution: &reference},
		Provenance: featureproto.EvidenceProvenance{Root: featureproto.LocationRootProject, Path: "features.go"},
	}}}
	legacyCapabilities := capabilityproto.Manifest{
		ProtocolVersion: capabilityproto.ProtocolVersion,
		Project:         "dep",
		ConfigDefinitions: []capabilityproto.ConfigDefinition{{
			Path: "service", Provenance: capabilityproto.Provenance{Project: "dep", SourceKind: capabilityproto.SourceKindFramework, EvidencePath: "config.go"},
		}},
		Migrations: []capabilityproto.MigrationBundle{{
			Name: "legacy-schema", Provenance: capabilityproto.Provenance{Project: "dep", SourceKind: capabilityproto.SourceKindFramework, EvidencePath: "migrations.go"},
		}},
	}
	files := map[string][]byte{
		"app/putnami.features.json":                 mustFeatureManifest(t, manifest),
		"app/schema/feature-evidence/evidence.json": mustEvidence(t, evidence),
		"dep/schema/capabilities.json":              mustCapabilitiesV1(t, legacyCapabilities),
	}
	ws := workspace.NewWorkspace("/workspace", &workspaceproto.Config{Name: "fixture"}, []*workspace.Project{
		{Name: "app", Path: "app", Version: "1.0.0"},
		{Name: "dep", Path: "dep", Version: "1.0.0"},
	})
	result := Aggregate(Request{Workspace: ws, Reader: &memoryReader{files: files, bindings: map[string]string{"dep": binding}}, Revision: Revision{Kind: RevisionKindWorktree}})
	if diag.HasErrors(result.Diagnostics) || result.Snapshot == nil {
		t.Fatalf("Aggregate = %#v", result)
	}
	evidenceAssessment := result.Snapshot.Features[0].Requirements[0].Evidence[0]
	if evidenceAssessment.State != EvidenceActive || evidenceAssessment.Contribution == nil || evidenceAssessment.Contribution.ProtocolVersion != capabilityproto.ProtocolVersion {
		t.Fatalf("v1 evidence assessment = %#v", evidenceAssessment)
	}
	if got := len(result.Snapshot.Unclassified); got != 1 {
		t.Fatalf("Unclassified = %d, want the v1 migration", got)
	}
	migration := result.Snapshot.Unclassified[0]
	if migration.Identity.Kind != capabilityproto.ContributionKindMigration || migration.Referenceable || migration.Identity.Subkind != "" {
		t.Fatalf("v1 migration = %#v, want visible but unreferenceable", migration)
	}
}

func TestAggregateEnumeratesEveryV1AndV2ContributionKind(t *testing.T) {
	v2, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "protocols", "capabilities", "fixtures", "v2", "valid", "all-kinds.json"))
	if err != nil {
		t.Fatal(err)
	}
	v1, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "protocols", "capabilities", "fixtures", "valid", "full.json"))
	if err != nil {
		t.Fatal(err)
	}
	zero := sourceBinding('0')
	files := map[string][]byte{
		"example/schema/capabilities.json": v2,
		"legacy/schema/capabilities.json":  v1,
	}
	ws := workspace.NewWorkspace("/workspace", &workspaceproto.Config{}, []*workspace.Project{
		{Name: "example", Path: "example", Version: "1.0.0"},
		{Name: "go.putnami.dev/example/iam", Path: "legacy", Version: "1.0.0"},
		{Name: "go.putnami.dev/database", Path: "database", Version: "1.4.0"},
	})
	result := Aggregate(Request{
		Workspace: ws,
		Reader: &memoryReader{
			files: files,
			bindings: map[string]string{
				"example":  zero,
				"database": zero,
			},
		},
		Revision: Revision{Kind: RevisionKindWorktree},
	})
	if diag.HasErrors(result.Diagnostics) || result.Snapshot == nil {
		t.Fatalf("Aggregate = %#v", result)
	}
	seen := make(map[capabilityproto.ContributionKind]bool)
	for _, contribution := range result.Snapshot.Unclassified {
		seen[contribution.Identity.Kind] = true
	}
	for kind := range capabilityproto.ValidContributionKinds {
		if !seen[kind] {
			t.Errorf("missing unclassified contribution kind %q", kind)
		}
	}
}

func TestAggregateArtifactFreshnessAndContainedReads(t *testing.T) {
	binding := sourceBinding('4')
	content := []byte("verified artifact\n")
	digest := sha256Digest(content)
	manifest := featureproto.Manifest{
		ProtocolVersion: featureproto.MinimumManifestProtocolVersion,
		Namespace:       "artifact",
		Features: []featureproto.Feature{{
			ID: "artifact/proof", Type: featureproto.FeatureTypeFeature, Name: "Artifact proof", Outcome: "Artifacts are verified", Owner: "platform", Target: featureproto.MaturityCoded,
			Requirements: []featureproto.Requirement{{ID: "implementation", Stage: featureproto.MaturityCoded, EvidenceKinds: []featureproto.EvidenceKind{featureproto.EvidenceKindArtifact}}},
		}},
	}
	record := featureproto.EvidenceRecord{
		ID: "artifact/proof-file", Feature: "artifact/proof", Requirement: "implementation", Stage: featureproto.MaturityCoded, Outcome: featureproto.EvidenceOutcomeSupports,
		Issuer:     featureproto.Issuer{Kind: featureproto.IssuerKindTest, ID: "artifact-test"},
		Source:     featureproto.SourceSelector{Root: featureproto.LocationRootProject, OwnerProject: "app", Binding: binding},
		Subject:    featureproto.EvidenceSubject{Kind: featureproto.EvidenceKindArtifact, Artifact: &featureproto.ArtifactSubject{Path: "dist/proof.txt", Digest: digest}},
		Provenance: featureproto.EvidenceProvenance{Root: featureproto.LocationRootProject, Path: "tests/proof_test.go"},
	}
	newRequest := func() Request {
		request := singleFeatureRequest(t, manifest, featureproto.EvidenceDocument{ProtocolVersion: featureproto.EvidenceProtocolVersion, Evidence: []featureproto.EvidenceRecord{record}}, map[string]string{"app": binding})
		request.Reader.(*memoryReader).files["app/dist/proof.txt"] = append([]byte(nil), content...)
		return request
	}

	t.Run("verified", func(t *testing.T) {
		result := Aggregate(newRequest())
		if diag.HasErrors(result.Diagnostics) || result.Snapshot == nil {
			t.Fatalf("Aggregate = %#v", result)
		}
		if got := result.Snapshot.Features[0].Requirements[0].State; got != VerificationVerified {
			t.Fatalf("State = %q, want verified", got)
		}
	})

	t.Run("digest mismatch", func(t *testing.T) {
		request := newRequest()
		request.Reader.(*memoryReader).files["app/dist/proof.txt"] = []byte("different")
		result := Aggregate(request)
		if diag.HasErrors(result.Diagnostics) || result.Snapshot == nil {
			t.Fatalf("Aggregate = %#v", result)
		}
		evidence := result.Snapshot.Features[0].Requirements[0].Evidence[0]
		if evidence.State != EvidenceStale || !containsString(evidence.StaleReasons, StaleArtifactDigest) {
			t.Fatalf("Evidence = %#v", evidence)
		}
	})

	t.Run("missing", func(t *testing.T) {
		request := newRequest()
		delete(request.Reader.(*memoryReader).files, "app/dist/proof.txt")
		result := Aggregate(request)
		if diag.HasErrors(result.Diagnostics) || result.Snapshot == nil {
			t.Fatalf("Aggregate = %#v", result)
		}
		evidence := result.Snapshot.Features[0].Requirements[0].Evidence[0]
		if evidence.State != EvidenceStale || !containsString(evidence.StaleReasons, StaleArtifactUnavailable) {
			t.Fatalf("Evidence = %#v", evidence)
		}
	})

	t.Run("symlink escape", func(t *testing.T) {
		request := newRequest()
		request.Reader.(*memoryReader).fileErrors = map[string]error{"app/dist/proof.txt": newReaderError(ReaderErrorSymlinkEscape, nil)}
		result := Aggregate(request)
		if result.Snapshot != nil || !hasDiagnostic(result.Diagnostics, featureproto.ErrorCodeSymlinkEscape) {
			t.Fatalf("Aggregate = %#v", result)
		}
	})
}

func TestAggregateReportsUnavailableAndMismatchedSourceBindingsAsStale(t *testing.T) {
	t.Run("unavailable", func(t *testing.T) {
		request := dependencyFixture(t, false, false)
		delete(request.Reader.(*memoryReader).bindings, "dep")
		result := Aggregate(request)
		if diag.HasErrors(result.Diagnostics) || result.Snapshot == nil {
			t.Fatalf("Aggregate = %#v", result)
		}
		if !hasDiagnostic(result.Diagnostics, capabilityproto.ErrorCodeSourceBindingUnavailable) ||
			!hasDiagnostic(result.Diagnostics, featureproto.ErrorCodeSourceBindingUnavailable) ||
			result.Snapshot.Features[0].Requirements[0].State != VerificationStale {
			t.Fatalf("Aggregate = %#v", result)
		}
	})

	t.Run("mismatch", func(t *testing.T) {
		request := dependencyFixture(t, false, false)
		request.Reader.(*memoryReader).bindings["dep"] = sourceBinding('9')
		result := Aggregate(request)
		if diag.HasErrors(result.Diagnostics) || result.Snapshot == nil {
			t.Fatalf("Aggregate = %#v", result)
		}
		if hasDiagnostic(result.Diagnostics, capabilityproto.ErrorCodeSourceBindingMismatch) || result.Snapshot.Features[0].Requirements[0].State != VerificationStale {
			t.Fatalf("Aggregate = %#v", result)
		}
	})
}

// A package-root contribution must resolve from a canonical manifest, where
// capabilities canonicalization has cleared the legacy Provenance.Version. The
// resolver keyed its package index on name+version only, so the canonical
// (empty-version) shape could never match a workspace project keyed by name AND
// version — every package-root contribution reported "source binding
// unavailable" regardless of the tree. In this repository that was all 45
// capabilities.source_binding_unavailable warnings.
func TestAggregateResolvesPackageRootBindingWithoutTheLegacyVersion(t *testing.T) {
	definition := func(version string) capabilityproto.ConfigDefinitionV2 {
		return capabilityproto.ConfigDefinitionV2{
			Identity: capabilityproto.ContributionIdentity{OwnerProject: "app", Kind: capabilityproto.ContributionKindConfig, Key: "service"},
			Path:     "service",
			Provenance: capabilityproto.ProvenanceV2{
				Project: "app", Package: "dep", Version: version, SourceKind: capabilityproto.SourceKindFramework,
				Declaration: capabilityproto.DeclarationLocation{Root: capabilityproto.LocationRootPackage, Path: "src/config.go"},
			},
		}
	}

	// Authoring a version does not produce a versioned manifest: MarshalManifestV2
	// canonicalizes, and canonicalization strips Provenance.Version. Pin that here
	// so this test can never drift into asserting a shape no producer emits.
	encoded := mustCapabilitiesV2(t, "app", []capabilityproto.ConfigDefinitionV2{definition("1.0.0")})
	if bytes.Contains(encoded, []byte(`"version"`)) {
		t.Fatalf("canonical manifest still carries a provenance version: %s", encoded)
	}

	files := map[string][]byte{"app/schema/capabilities.json": encoded}
	projects := []*workspace.Project{
		{Name: "dep", Path: "dep", Version: "1.0.0"},
		{Name: "app", Path: "app", Version: "1.0.0", Dependencies: []string{"dep"}},
	}
	ws := workspace.NewWorkspace("/workspace", &workspaceproto.Config{Name: "fixture"}, projects)
	reader := &memoryReader{files: files, bindings: map[string]string{"dep": sourceBinding('d')}}
	result := Aggregate(Request{Workspace: ws, Reader: reader, Revision: Revision{Kind: RevisionKindWorktree}})

	if result.Snapshot == nil || diag.HasErrors(result.Diagnostics) {
		t.Fatalf("Aggregate = %#v", result)
	}
	if hasDiagnostic(result.Diagnostics, capabilityproto.ErrorCodeSourceBindingUnavailable) {
		t.Fatalf("package-root binding reported unavailable: %#v", result.Diagnostics)
	}
	if len(result.Snapshot.SourceBindings) != 1 {
		t.Fatalf("SourceBindings = %#v, want exactly one", result.Snapshot.SourceBindings)
	}
	binding := result.Snapshot.SourceBindings[0]
	if binding.Unavailable || binding.Binding != sourceBinding('d') {
		t.Fatalf("SourceBindings[0] = %#v, want the resolved dep binding", binding)
	}
}

func TestAggregateRejectsEscapingV1InspectionPaths(t *testing.T) {
	manifest := capabilityproto.Manifest{
		ProtocolVersion: capabilityproto.ProtocolVersion,
		Project:         "legacy",
		ConfigDefinitions: []capabilityproto.ConfigDefinition{{
			Path: "service",
			Provenance: capabilityproto.Provenance{
				Project: "legacy", SourceKind: capabilityproto.SourceKindFramework, EvidencePath: "/Users/private/secret.go",
			},
		}},
	}
	files := map[string][]byte{"legacy/schema/capabilities.json": mustCapabilitiesV1(t, manifest)}
	ws := workspace.NewWorkspace("/workspace", &workspaceproto.Config{}, []*workspace.Project{{Name: "legacy", Path: "legacy"}})
	result := Aggregate(Request{Workspace: ws, Reader: &memoryReader{files: files, bindings: map[string]string{}}, Revision: Revision{Kind: RevisionKindWorktree}})
	if result.Snapshot != nil || !hasDiagnostic(result.Diagnostics, capabilityproto.ErrorCodePathEscape) {
		t.Fatalf("Aggregate = %#v", result)
	}
	encoded, err := json.Marshal(result.Diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("/Users/private")) {
		t.Fatalf("diagnostics leaked the escaping path: %s", encoded)
	}
}

func TestAggregateRejectsUnboundedSnapshotMetadata(t *testing.T) {
	namespace := strings.Repeat("a", maxSemanticMetadataBytes+1)
	manifest := featureproto.Manifest{
		ProtocolVersion: featureproto.MinimumManifestProtocolVersion,
		Namespace:       namespace,
		Features: []featureproto.Feature{{
			ID: namespace + "/feature", Type: featureproto.FeatureTypeFeature, Name: "Feature", Outcome: "A bounded snapshot", Owner: "platform", Target: featureproto.MaturityModeled,
		}},
	}
	files := map[string][]byte{"app/putnami.features.json": mustFeatureManifest(t, manifest)}
	ws := workspace.NewWorkspace("/workspace", &workspaceproto.Config{}, []*workspace.Project{{Name: "app", Path: "app"}})
	result := Aggregate(Request{Workspace: ws, Reader: &memoryReader{files: files, bindings: map[string]string{}}, Revision: Revision{Kind: RevisionKindWorktree}})
	if result.Snapshot != nil || !hasDiagnostic(result.Diagnostics, featureproto.ErrorCodeSensitiveContent) {
		t.Fatalf("Aggregate = %#v", result)
	}
	encoded, err := json.Marshal(result.Diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(namespace)) {
		t.Fatalf("diagnostics leaked unbounded semantic metadata: %s", encoded)
	}
}

func TestAggregateRejectsUnboundedContributionMetadata(t *testing.T) {
	owner := strings.Repeat("p", maxSemanticMetadataBytes+1)
	manifest := capabilityproto.Manifest{
		ProtocolVersion: capabilityproto.ProtocolVersion,
		Project:         owner,
		ConfigDefinitions: []capabilityproto.ConfigDefinition{{
			Path: "service",
			Provenance: capabilityproto.Provenance{
				Project: owner, SourceKind: capabilityproto.SourceKindFramework, EvidencePath: "config.go",
			},
		}},
	}
	files := map[string][]byte{"legacy/schema/capabilities.json": mustCapabilitiesV1(t, manifest)}
	ws := workspace.NewWorkspace("/workspace", &workspaceproto.Config{}, []*workspace.Project{{Name: "legacy", Path: "legacy"}})
	result := Aggregate(Request{Workspace: ws, Reader: &memoryReader{files: files, bindings: map[string]string{}}, Revision: Revision{Kind: RevisionKindWorktree}})
	if result.Snapshot != nil || !hasDiagnostic(result.Diagnostics, featureproto.ErrorCodeSensitiveContent) {
		t.Fatalf("Aggregate = %#v", result)
	}
	encoded, err := json.Marshal(result.Diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(owner)) {
		t.Fatalf("diagnostics leaked unbounded contribution metadata: %s", encoded)
	}
}

func TestAggregateRejectsDanglingDuplicateAndUnresolvedReferences(t *testing.T) {
	t.Run("dangling relation", func(t *testing.T) {
		manifest := featureproto.Manifest{
			ProtocolVersion: featureproto.MinimumManifestProtocolVersion,
			Namespace:       "core",
			Features: []featureproto.Feature{{
				ID: "core/root", Type: featureproto.FeatureTypeFeature, Name: "Root", Outcome: "Root exists", Owner: "core", Target: featureproto.MaturityModeled,
				Relations: []featureproto.Relation{{Kind: featureproto.RelationKindDependsOn, Target: "core/missing"}},
			}},
		}
		files := map[string][]byte{"app/putnami.features.json": mustFeatureManifest(t, manifest)}
		ws := workspace.NewWorkspace("/workspace", &workspaceproto.Config{}, []*workspace.Project{{Name: "app", Path: "app"}})
		result := Aggregate(Request{Workspace: ws, Reader: &memoryReader{files: files, bindings: map[string]string{}}, Revision: Revision{Kind: RevisionKindWorktree}})
		if result.Snapshot != nil || !hasDiagnostic(result.Diagnostics, featureproto.ErrorCodeDanglingRelation) {
			t.Fatalf("result = %#v", result)
		}
	})

	t.Run("duplicate feature", func(t *testing.T) {
		request := dependencyFixture(t, false, false)
		reader := request.Reader.(*memoryReader)
		reader.files["app-b/putnami.features.json"] = append([]byte(nil), reader.files["app-a/putnami.features.json"]...)
		result := Aggregate(request)
		if result.Snapshot != nil || !hasDiagnostic(result.Diagnostics, featureproto.ErrorCodeDuplicateFeature) {
			t.Fatalf("result = %#v", result)
		}
	})

	t.Run("unresolved capability", func(t *testing.T) {
		request := dependencyFixture(t, false, false)
		reader := request.Reader.(*memoryReader)
		delete(reader.files, "dep/schema/capabilities.json")
		delete(reader.files, "app-a/schema/capabilities.json")
		delete(reader.files, "app-b/schema/capabilities.json")
		result := Aggregate(request)
		if result.Snapshot != nil || !hasDiagnostic(result.Diagnostics, capabilityproto.ErrorCodeUnresolvedReference) {
			t.Fatalf("result = %#v", result)
		}
	})
}

func TestAggregatePinsMissingStaleContradictedAndContiguousMaturity(t *testing.T) {
	current := sourceBinding('2')
	stale := sourceBinding('1')
	manifest := featureproto.Manifest{
		ProtocolVersion: featureproto.MinimumManifestProtocolVersion,
		Namespace:       "billing",
		Features: []featureproto.Feature{{
			ID: "billing/export", Type: featureproto.FeatureTypeFeature, Name: "Export", Outcome: "Invoices can be exported", Owner: "billing", Target: featureproto.MaturityLiveVerified,
			Requirements: []featureproto.Requirement{
				{ID: "implementation", Stage: featureproto.MaturityCoded, EvidenceKinds: []featureproto.EvidenceKind{featureproto.EvidenceKindAttestation}},
				{ID: "integration", Stage: featureproto.MaturityWired, EvidenceKinds: []featureproto.EvidenceKind{featureproto.EvidenceKindAttestation}},
				{ID: "default", Stage: featureproto.MaturityDefaultOn, EvidenceKinds: []featureproto.EvidenceKind{featureproto.EvidenceKindAttestation}},
				{ID: "live", Stage: featureproto.MaturityLiveVerified, EvidenceKinds: []featureproto.EvidenceKind{featureproto.EvidenceKindAttestation}},
			},
		}},
	}
	document := featureproto.EvidenceDocument{ProtocolVersion: featureproto.EvidenceProtocolVersion, Evidence: []featureproto.EvidenceRecord{
		persistentAttestation("billing/implementation-proof", "implementation", featureproto.MaturityCoded, featureproto.EvidenceOutcomeSupports),
		nonPersistentAttestation("billing/integration-proof", "integration", featureproto.MaturityWired, featureproto.EvidenceOutcomeSupports, featureproto.IssuerKindTest, stale),
		persistentAttestation("billing/live-contradiction", "live", featureproto.MaturityLiveVerified, featureproto.EvidenceOutcomeContradicts),
	}}
	request := singleFeatureRequest(t, manifest, document, map[string]string{"": current})
	result := Aggregate(request)
	if diag.HasErrors(result.Diagnostics) || result.Snapshot == nil {
		t.Fatalf("Aggregate = %#v", result)
	}
	feature := result.Snapshot.Features[0]
	if feature.Current != featureproto.MaturityCoded {
		t.Fatalf("Current = %q, want coded", feature.Current)
	}
	states := make(map[string]VerificationState)
	for _, requirement := range feature.Requirements {
		states[requirement.ID] = requirement.State
	}
	want := map[string]VerificationState{
		"implementation": VerificationVerified,
		"integration":    VerificationStale,
		"default":        VerificationMissing,
		"live":           VerificationContradicted,
	}
	for requirement, state := range want {
		if states[requirement] != state {
			t.Errorf("state[%s] = %q, want %q", requirement, states[requirement], state)
		}
	}
	for _, code := range []string{featureproto.WarningCodeMissingEvidence, featureproto.WarningCodeStaleEvidence, featureproto.WarningCodeContradictedEvidence} {
		if !hasDiagnostic(result.Diagnostics, code) {
			t.Errorf("Diagnostics = %#v, want %s", result.Diagnostics, code)
		}
	}
}

func TestAggregateHumanAuthorityIsAdditionalToVerifiedRequirements(t *testing.T) {
	binding := sourceBinding('a')
	stages := featureproto.OrderedMaturityStages()[1:]
	manifest := featureproto.Manifest{
		ProtocolVersion: featureproto.MinimumManifestProtocolVersion,
		Namespace:       "release",
		Features: []featureproto.Feature{{
			ID: "release/automation", Type: featureproto.FeatureTypeFeature, Name: "Automation", Outcome: "Releases are automated", Owner: "release", Target: featureproto.MaturityGA,
		}},
	}
	var evidence []featureproto.EvidenceRecord
	for index, stage := range stages {
		requirement := "stage-" + strings.ReplaceAll(string(stage), "-", "")
		manifest.Features[0].Requirements = append(manifest.Features[0].Requirements, featureproto.Requirement{ID: requirement, Stage: stage, EvidenceKinds: []featureproto.EvidenceKind{featureproto.EvidenceKindAttestation}})
		if stage == featureproto.MaturityGA {
			evidence = append(evidence, nonPersistentAttestation("release/proof-ga", requirement, stage, featureproto.EvidenceOutcomeSupports, featureproto.IssuerKindBuild, binding))
		} else {
			record := persistentAttestation("release/proof-"+string(rune('a'+index)), requirement, stage, featureproto.EvidenceOutcomeSupports)
			evidence = append(evidence, record)
		}
	}
	result := Aggregate(singleFeatureRequest(t, manifest, featureproto.EvidenceDocument{ProtocolVersion: featureproto.EvidenceProtocolVersion, Evidence: evidence}, map[string]string{"": binding}))
	if diag.HasErrors(result.Diagnostics) || result.Snapshot == nil {
		t.Fatalf("Aggregate = %#v", result)
	}
	if got := result.Snapshot.Features[0].Current; got != featureproto.MaturityDesignPartnerProven {
		t.Fatalf("Current = %q, want design-partner-proven", got)
	}
	if got := result.Snapshot.Features[0].Requirements[len(stages)-1].State; got != VerificationVerified {
		t.Fatalf("GA requirement state = %q, want verified", got)
	}
	if !hasDiagnostic(result.Diagnostics, featureproto.WarningCodeMissingHumanAuthority) {
		t.Fatalf("Diagnostics = %#v, want missing-human-authority warning", result.Diagnostics)
	}
}

func TestAggregateRejectsLexicalAndSymlinkEscapesWithoutLeakingHostPaths(t *testing.T) {
	lexicalWorkspace := workspace.NewWorkspace("/workspace", &workspaceproto.Config{}, []*workspace.Project{{Name: "escape", Path: "../escape"}})
	lexical := Aggregate(Request{Workspace: lexicalWorkspace, Reader: &memoryReader{files: map[string][]byte{}, bindings: map[string]string{}}, Revision: Revision{Kind: RevisionKindWorktree}})
	if lexical.Snapshot != nil || !hasDiagnostic(lexical.Diagnostics, featureproto.ErrorCodePathEscape) {
		t.Fatalf("lexical escape result = %#v", lexical)
	}

	if runtime.GOOS == "windows" {
		t.Skip("symlink containment fixture is not portable to Windows")
	}
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	projectRoot := filepath.Join(root, "project")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "secret.json")
	if err := os.WriteFile(outside, []byte(`{"secret":"must-not-leak"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(projectRoot, featureproto.ManifestFilename)); err != nil {
		t.Fatal(err)
	}
	reader, err := NewOSReader(root)
	if err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewWorkspace(root, &workspaceproto.Config{}, []*workspace.Project{{Name: "project", Path: "project"}})
	result := Aggregate(Request{Workspace: ws, Reader: reader, Revision: Revision{Kind: RevisionKindWorktree}})
	if result.Snapshot != nil || !hasDiagnostic(result.Diagnostics, featureproto.ErrorCodeSymlinkEscape) {
		t.Fatalf("symlink escape result = %#v", result)
	}
	encoded, err := json.Marshal(result.Diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{parent, outside, "must-not-leak"} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("diagnostics leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestAggregateBoundsArtifactReads(t *testing.T) {
	reader := &memoryReader{
		files:    map[string][]byte{featureproto.ManifestFilename: make([]byte, MaxReadBytes+1)},
		bindings: map[string]string{},
	}
	ws := workspace.NewWorkspace("/workspace", &workspaceproto.Config{}, nil)
	result := Aggregate(Request{Workspace: ws, Reader: reader, Revision: Revision{Kind: RevisionKindWorktree}})
	if result.Snapshot != nil || !hasDiagnostic(result.Diagnostics, featureproto.ErrorCodeSensitiveContent) {
		t.Fatalf("bounded read result = %#v", result)
	}
}

func TestAggregateValidatesRevisionMetadataAndCanonicalizesEmptyCollections(t *testing.T) {
	ws := workspace.NewWorkspace("/workspace", &workspaceproto.Config{}, nil)
	reader := &memoryReader{files: map[string][]byte{}, bindings: map[string]string{}}
	invalid := Aggregate(Request{
		Workspace: ws,
		Reader:    reader,
		Revision:  Revision{Kind: RevisionKindGit, Commit: "/Users/secret/revision"},
	})
	if invalid.Snapshot != nil || !hasDiagnostic(invalid.Diagnostics, featureproto.ErrorCodeInvalidSubject) {
		t.Fatalf("invalid revision result = %#v", invalid)
	}
	encodedDiagnostics, err := json.Marshal(invalid.Diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encodedDiagnostics, []byte("/Users/secret")) {
		t.Fatalf("revision diagnostic leaked untrusted metadata: %s", encodedDiagnostics)
	}

	valid := Aggregate(Request{
		Workspace: ws,
		Reader:    reader,
		Revision:  Revision{Kind: RevisionKindGit, Commit: "git:" + strings.Repeat("a", 40)},
	})
	if diag.HasErrors(valid.Diagnostics) || valid.Snapshot == nil {
		t.Fatalf("valid Git revision result = %#v", valid)
	}
	data, err := MarshalSnapshot(valid.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"\"sourceBindings\": []", "\"features\": []", "\"unclassified\": []"} {
		if !bytes.Contains(data, []byte(field)) {
			t.Errorf("canonical snapshot does not contain %s: %s", field, data)
		}
	}
}

func dependencyFixture(t *testing.T, reverse, conflict bool) Request {
	t.Helper()
	depBinding := sourceBinding('d')
	appBinding := sourceBinding('a')
	dep := capabilityproto.ConfigDefinitionV2{
		Identity: capabilityproto.ContributionIdentity{OwnerProject: "dep", Kind: capabilityproto.ContributionKindConfig, Key: "service"},
		Path:     "service",
		Provenance: capabilityproto.ProvenanceV2{
			Project: "dep", SourceKind: capabilityproto.SourceKindFramework,
			Declaration: capabilityproto.DeclarationLocation{Root: capabilityproto.LocationRootProject, Path: "src/config.ts"},
		},
	}
	appCopy := dep
	if conflict {
		appCopy.Fields = []capabilityproto.ConfigField{{Name: "different", Type: "string"}}
	}
	orphan := capabilityproto.ConfigDefinitionV2{
		Identity: capabilityproto.ContributionIdentity{OwnerProject: "app-a", Kind: capabilityproto.ContributionKindConfig, Key: "orphan"},
		Path:     "orphan",
		Provenance: capabilityproto.ProvenanceV2{
			Project: "app-a", SourceKind: capabilityproto.SourceKindFramework,
			Declaration: capabilityproto.DeclarationLocation{Root: capabilityproto.LocationRootProject, Path: "src/orphan.ts"},
		},
	}
	manifest := featureproto.Manifest{
		ProtocolVersion: featureproto.MinimumManifestProtocolVersion,
		Namespace:       "billing",
		Features: []featureproto.Feature{{
			ID: "billing/export", Type: featureproto.FeatureTypeFeature, Name: "Export", Outcome: "Invoices can be exported", Owner: "billing", Target: featureproto.MaturityCoded,
			Requirements: []featureproto.Requirement{{ID: "implementation", Stage: featureproto.MaturityCoded, EvidenceKinds: []featureproto.EvidenceKind{featureproto.EvidenceKindCapability}}},
		}},
	}
	evidence := featureproto.EvidenceDocument{ProtocolVersion: featureproto.EvidenceProtocolVersion, Evidence: []featureproto.EvidenceRecord{{
		ID: "billing/export-proof", Feature: "billing/export", Requirement: "implementation", Stage: featureproto.MaturityCoded, Outcome: featureproto.EvidenceOutcomeSupports,
		Issuer:     featureproto.Issuer{Kind: featureproto.IssuerKindBuild, ID: "typescript-build"},
		Source:     featureproto.SourceSelector{Root: featureproto.LocationRootProject, OwnerProject: "dep", Binding: depBinding},
		Subject:    featureproto.EvidenceSubject{Kind: featureproto.EvidenceKindCapability, Contribution: &capabilityproto.ContributionReference{OwnerProject: "dep", Kind: capabilityproto.ContributionKindConfig, Key: "service"}},
		Provenance: featureproto.EvidenceProvenance{Root: featureproto.LocationRootProject, Path: "src/features.ts"},
	}}}
	files := map[string][]byte{
		"app-a/putnami.features.json":                     mustFeatureManifest(t, manifest),
		"app-a/schema/feature-evidence/a.json":            mustEvidence(t, evidence),
		"app-a/schema/feature-evidence/z-empty.json":      []byte("{\n  \"protocolVersion\": 1,\n  \"evidence\": []\n}\n"),
		"dep/schema/capabilities.json":                    mustCapabilitiesV2(t, "dep", []capabilityproto.ConfigDefinitionV2{dep}),
		"app-a/schema/capabilities.json":                  mustCapabilitiesV2(t, "app-a", []capabilityproto.ConfigDefinitionV2{dep, orphan}),
		"app-b/schema/capabilities.json":                  mustCapabilitiesV2(t, "app-b", []capabilityproto.ConfigDefinitionV2{appCopy}),
		"app-a/fixtures/putnami.features.json":            []byte(`{"invalid":true}`),
		"app-a/.gen/schema/feature-evidence/ignored.json": []byte(`{"invalid":true}`),
		"undeclared/putnami.features.json":                []byte(`{"invalid":true}`),
		"undeclared/schema/feature-evidence/ignored.json": []byte(`{"invalid":true}`),
	}
	projects := []*workspace.Project{
		{Name: "dep", Path: "dep", Version: "1.0.0"},
		{Name: "app-a", Path: "app-a", Version: "1.0.0", Dependencies: []string{"dep"}},
		{Name: "app-b", Path: "app-b", Version: "1.0.0", Dependencies: []string{"dep"}},
	}
	if reverse {
		projects[0], projects[2] = projects[2], projects[0]
	}
	ws := workspace.NewWorkspace("/workspace", &workspaceproto.Config{Name: "fixture"}, projects)
	return Request{
		Workspace: ws,
		Reader: &memoryReader{
			files:       files,
			bindings:    map[string]string{"dep": depBinding, "app-a": appBinding},
			reverseDirs: reverse,
		},
		Revision: Revision{Kind: RevisionKindWorktree, Head: "git:" + strings.Repeat("1", 40)},
	}
}

func featureAuthorityFixture(t *testing.T, authority *workspaceproto.ProjectFeatureAuthority) Request {
	t.Helper()
	binding := sourceBinding('e')
	mapped := capabilityproto.ConfigDefinitionV2{
		Identity: capabilityproto.ContributionIdentity{OwnerProject: "producer", Kind: capabilityproto.ContributionKindConfig, Key: "mapped"},
		Path:     "mapped",
		Provenance: capabilityproto.ProvenanceV2{
			Project: "producer", SourceKind: capabilityproto.SourceKindFramework,
			Declaration: capabilityproto.DeclarationLocation{Root: capabilityproto.LocationRootProject, Path: "src/config.ts"},
		},
	}
	orphan := mapped
	orphan.Identity.Key = "orphan"
	orphan.Path = "orphan"
	manifest := featureproto.Manifest{
		ProtocolVersion: featureproto.MinimumManifestProtocolVersion,
		Namespace:       "demo",
		Features: []featureproto.Feature{{
			ID: "demo/outcome", Type: featureproto.FeatureTypeFeature, Name: "Outcome", Outcome: "The mapped contribution is evidenced", Owner: "team", Target: featureproto.MaturityCoded,
			Requirements: []featureproto.Requirement{{ID: "implementation", Stage: featureproto.MaturityCoded, EvidenceKinds: []featureproto.EvidenceKind{featureproto.EvidenceKindCapability}}},
		}},
	}
	evidence := featureproto.EvidenceDocument{ProtocolVersion: featureproto.EvidenceProtocolVersion, Evidence: []featureproto.EvidenceRecord{{
		ID: "demo/outcome-proof", Feature: "demo/outcome", Requirement: "implementation", Stage: featureproto.MaturityCoded, Outcome: featureproto.EvidenceOutcomeSupports,
		Issuer:     featureproto.Issuer{Kind: featureproto.IssuerKindBuild, ID: "fixture-build"},
		Source:     featureproto.SourceSelector{Root: featureproto.LocationRootProject, OwnerProject: "producer", Binding: binding},
		Subject:    featureproto.EvidenceSubject{Kind: featureproto.EvidenceKindCapability, Contribution: &capabilityproto.ContributionReference{OwnerProject: "producer", Kind: capabilityproto.ContributionKindConfig, Key: "mapped"}},
		Provenance: featureproto.EvidenceProvenance{Root: featureproto.LocationRootProject, Path: "src/config.ts"},
	}}}
	files := map[string][]byte{
		"feature-owner/putnami.features.json":                 mustFeatureManifest(t, manifest),
		"feature-owner/schema/feature-evidence/evidence.json": mustEvidence(t, evidence),
		"producer/schema/capabilities.json":                   mustCapabilitiesV2(t, "producer", []capabilityproto.ConfigDefinitionV2{mapped, orphan}),
	}
	producerConfig := &workspaceproto.ProjectConfig{Name: "producer", FeatureAuthority: authority}
	ws := workspace.NewWorkspace("/workspace", &workspaceproto.Config{Name: "fixture"}, []*workspace.Project{
		{ID: "/feature-owner", Name: "feature-owner", Path: "feature-owner", Version: "1.0.0"},
		{ID: "/producer", Name: "producer", Path: "producer", Version: "1.0.0", Config: producerConfig},
	})
	return Request{
		Workspace: ws,
		Reader:    &memoryReader{files: files, bindings: map[string]string{"producer": binding}},
		Revision:  Revision{Kind: RevisionKindWorktree},
	}
}

func singleFeatureRequest(t *testing.T, manifest featureproto.Manifest, evidence featureproto.EvidenceDocument, bindings map[string]string) Request {
	t.Helper()
	files := map[string][]byte{
		"app/putnami.features.json":                 mustFeatureManifest(t, manifest),
		"app/schema/feature-evidence/evidence.json": mustEvidence(t, evidence),
	}
	ws := workspace.NewWorkspace("/workspace", &workspaceproto.Config{Name: "fixture"}, []*workspace.Project{{Name: "app", Path: "app", Version: "1.0.0"}})
	return Request{Workspace: ws, Reader: &memoryReader{files: files, bindings: bindings}, Revision: Revision{Kind: RevisionKindWorktree}}
}

func persistentAttestation(id, requirement string, stage featureproto.MaturityStage, outcome featureproto.EvidenceOutcome) featureproto.EvidenceRecord {
	return featureproto.EvidenceRecord{
		ID: id, Feature: strings.Split(id, "/")[0] + "/" + featureNameForEvidence(id), Requirement: requirement, Stage: stage, Outcome: outcome,
		Issuer:     featureproto.Issuer{Kind: featureproto.IssuerKindHuman, ID: "product-council"},
		Source:     featureproto.SourceSelector{Root: featureproto.LocationRootWorkspace, Binding: sourceBinding('0')},
		Subject:    featureproto.EvidenceSubject{Kind: featureproto.EvidenceKindAttestation, Attestation: &featureproto.AttestationSubject{Claim: "proof"}},
		Provenance: featureproto.EvidenceProvenance{Root: featureproto.LocationRootWorkspace, Path: "decisions/features.md"},
		Persistent: true,
	}
}

func nonPersistentAttestation(id, requirement string, stage featureproto.MaturityStage, outcome featureproto.EvidenceOutcome, issuer featureproto.IssuerKind, binding string) featureproto.EvidenceRecord {
	return featureproto.EvidenceRecord{
		ID: id, Feature: strings.Split(id, "/")[0] + "/" + featureNameForEvidence(id), Requirement: requirement, Stage: stage, Outcome: outcome,
		Issuer:     featureproto.Issuer{Kind: issuer, ID: "evidence-producer"},
		Source:     featureproto.SourceSelector{Root: featureproto.LocationRootWorkspace, Binding: binding},
		Subject:    featureproto.EvidenceSubject{Kind: featureproto.EvidenceKindAttestation, Attestation: &featureproto.AttestationSubject{Claim: "proof"}},
		Provenance: featureproto.EvidenceProvenance{Root: featureproto.LocationRootWorkspace, Path: "evidence/features.json"},
	}
}

func featureNameForEvidence(id string) string {
	prefix := strings.Split(id, "/")[0]
	switch prefix {
	case "billing":
		return "export"
	case "release":
		return "automation"
	default:
		return "feature"
	}
}

func sourceBinding(character byte) string {
	return "source-v1:sha256:" + strings.Repeat(string(character), 64)
}

func mustFeatureManifest(t *testing.T, manifest featureproto.Manifest) []byte {
	t.Helper()
	data, err := featureproto.MarshalManifest(&manifest)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustEvidence(t *testing.T, document featureproto.EvidenceDocument) []byte {
	t.Helper()
	data, err := featureproto.MarshalEvidenceDocument(&document)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustCapabilitiesV2(t *testing.T, project string, definitions []capabilityproto.ConfigDefinitionV2) []byte {
	t.Helper()
	data, err := capabilityproto.MarshalManifestV2(&capabilityproto.ManifestV2{ProtocolVersion: capabilityproto.ProtocolVersionV2, Project: project, ConfigDefinitions: definitions})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustCapabilitiesV1(t *testing.T, manifest capabilityproto.Manifest) []byte {
	t.Helper()
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func hasDiagnostic(diagnostics []diag.Diagnostic, code string) bool {
	return countDiagnostic(diagnostics, code) > 0
}

func countDiagnostic(diagnostics []diag.Diagnostic, code string) int {
	count := 0
	for _, finding := range diagnostics {
		if finding.Code == code {
			count++
		}
	}
	return count
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func sha256Digest(data []byte) string {
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// TestBindingResolverResolvesAVersionedPackageFromAWireBuiltView is the
// end-to-end pin on the second gap that got closed.
//
// An evidence SourceSelector carries an exact package AND version, and the
// resolver's primary index is keyed on both. A wire-built view whose projects
// had no version could therefore never match the versioned key: it fell through
// to the versionless index, found nothing under the versioned selector, and the
// binding degraded to "package source binding unavailable" — a silent narrowing
// of exactly the corpus that qualifies its evidence, while the versionless half
// beside it kept resolving. `ProjectRef.version` is what makes the primary key
// reachable from a job subprocess.
func TestBindingResolverResolvesAVersionedPackageFromAWireBuiltView(t *testing.T) {
	ws, scoped := workspace.FromContext(&pctx.Context{
		WorkspaceRoot: "/workspace",
		Workspace:     pctx.Workspace{Name: "fixture", Version: "1.0.0"},
		SelectedProjects: []pctx.ProjectRef{
			{ID: "/dep", Name: "dep", Version: "1.0.0", Path: "dep", FullPath: "/workspace/dep"},
			{ID: "/unversioned", Name: "unversioned", Path: "unversioned", FullPath: "/workspace/unversioned"},
		},
		Selection: &pctx.Selection{
			Mode: pctx.SelectionModeAll, ProjectIDs: []string{"/dep", "/unversioned"},
		},
	})
	if scoped {
		t.Fatal("the fixture selection is the whole workspace")
	}
	reader := &memoryReader{
		files:    map[string][]byte{},
		bindings: map[string]string{"dep": sourceBinding('d'), "unversioned": sourceBinding('u')},
	}
	resolver := newBindingResolver(ws, reader)

	versioned := resolver.resolve(featureproto.SourceSelector{
		Root: capabilityproto.LocationRootPackage, Package: "dep", Version: "1.0.0",
	})
	if versioned.err != nil {
		t.Fatalf("a version-qualified package selector did not resolve: %v", versioned.err)
	}
	if versioned.rootPath != "dep" || versioned.binding != sourceBinding('d') {
		t.Fatalf("versioned binding = %+v, want dep's", versioned)
	}

	// A selector naming a version the workspace does not carry must still FAIL.
	// The version is a constraint, not decoration: resolving it by name anyway
	// would report a binding for a package the selector did not ask for.
	wrong := resolver.resolve(featureproto.SourceSelector{
		Root: capabilityproto.LocationRootPackage, Package: "dep", Version: "9.9.9",
	})
	if wrong.err == nil {
		t.Fatalf("a selector naming version 9.9.9 resolved to %+v", wrong)
	}

	// A project the wire carried no version for keeps the versionless
	// fallback: "nobody said" is matched by name alone, never against an
	// invented version.
	bare := resolver.resolve(featureproto.SourceSelector{
		Root: capabilityproto.LocationRootPackage, Package: "unversioned",
	})
	if bare.err != nil || bare.rootPath != "unversioned" {
		t.Fatalf("versionless binding = %+v, want the name-only match", bare)
	}
}
