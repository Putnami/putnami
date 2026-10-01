package features

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	capabilityproto "go.putnami.dev/protocol/capabilities"
	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	workspaceproto "go.putnami.dev/protocol/workspace"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// scopedFixture builds the many-project shape every scoping property is argued
// against:
//
//	billing   — the SEED. Declares billing/invoice, whose one requirement is
//	            satisfied by evidence that does not live here.
//	shipping  — an unselected project that nevertheless produces the evidence
//	            for billing/invoice and owns the capability contribution that
//	            evidence names. Following it is the correctness requirement.
//	unrelated — an unselected project with its own feature, its own evidence,
//	            and a structurally broken evidence document. Nothing here may
//	            reach a scoped billing answer, and its error may not fail it.
func scopedFixture(t *testing.T) (*workspace.Workspace, *memoryReader) {
	t.Helper()
	binding := sourceBinding('b')
	billing := featureproto.Manifest{
		ProtocolVersion: featureproto.MinimumManifestProtocolVersion,
		Namespace:       "billing",
		Features: []featureproto.Feature{{
			ID: "billing/invoice", Type: featureproto.FeatureTypeFeature, Name: "Invoice export",
			Outcome: "Customers export issued invoices", Owner: "billing", Target: featureproto.MaturityCoded,
			Requirements: []featureproto.Requirement{{
				ID: "implementation", Stage: featureproto.MaturityCoded,
				EvidenceKinds: []featureproto.EvidenceKind{featureproto.EvidenceKindCapability},
			}},
		}},
	}
	unrelated := featureproto.Manifest{
		ProtocolVersion: featureproto.MinimumManifestProtocolVersion,
		Namespace:       "warehouse",
		Features: []featureproto.Feature{{
			ID: "warehouse/stock", Type: featureproto.FeatureTypeFeature, Name: "Stock levels",
			Outcome: "Operators see stock levels", Owner: "warehouse", Target: featureproto.MaturityModeled,
		}},
	}
	reference := capabilityproto.ContributionReference{
		OwnerProject: "shipping", Kind: capabilityproto.ContributionKindConfig, Key: "invoice",
	}
	// The record for the SEED's feature is authored in another project, which is
	// exactly the case a filesystem-wall scope would lose.
	crossProject := featureproto.EvidenceDocument{
		ProtocolVersion: featureproto.EvidenceProtocolVersion,
		Evidence: []featureproto.EvidenceRecord{{
			ID: "billing/invoice-proof", Feature: "billing/invoice", Requirement: "implementation",
			Stage: featureproto.MaturityCoded, Outcome: featureproto.EvidenceOutcomeSupports,
			Issuer:     featureproto.Issuer{Kind: featureproto.IssuerKindBuild, ID: "shipping-build"},
			Source:     featureproto.SourceSelector{Root: featureproto.LocationRootProject, OwnerProject: "shipping", Binding: binding},
			Subject:    featureproto.EvidenceSubject{Kind: featureproto.EvidenceKindCapability, Contribution: &reference},
			Provenance: featureproto.EvidenceProvenance{Root: featureproto.LocationRootProject, Path: "invoice.go"},
		}},
	}
	files := map[string][]byte{
		"billing/" + featureproto.ManifestFilename:      mustFeatureManifest(t, billing),
		"unrelated/" + featureproto.ManifestFilename:    mustFeatureManifest(t, unrelated),
		"shipping/schema/feature-evidence/billing.json": mustEvidence(t, crossProject),
		"shipping/schema/capabilities.json": mustCapabilitiesV2(t, "shipping", []capabilityproto.ConfigDefinitionV2{{
			Identity: capabilityproto.ContributionIdentity{
				OwnerProject: "shipping", Kind: capabilityproto.ContributionKindConfig, Key: "invoice",
			},
			Path: "invoice",
			Provenance: capabilityproto.ProvenanceV2{
				Project: "shipping", SourceKind: capabilityproto.SourceKindFramework,
				Declaration: capabilityproto.DeclarationLocation{Root: capabilityproto.LocationRootProject, Path: "config.go"},
			},
		}}),
		// Structurally broken, and wholly outside any selection that names
		// billing: a scoped run must neither report nor fail on it.
		"unrelated/schema/feature-evidence/broken.json": []byte(`{"protocolVersion":1,"evidence":[{"id":""}]}`),
	}
	ws := workspace.NewWorkspace("/workspace", &workspaceproto.Config{Name: "fixture"}, []*workspace.Project{
		{ID: "/billing", Name: "billing", Path: "billing", Version: "1.0.0"},
		{ID: "/shipping", Name: "shipping", Path: "shipping", Version: "1.0.0"},
		{ID: "/unrelated", Name: "unrelated", Path: "unrelated", Version: "1.0.0"},
	})
	return ws, &memoryReader{files: files, bindings: map[string]string{
		"billing": binding, "shipping": binding, "unrelated": binding,
	}}
}

func scopeFor(ws *workspace.Workspace, names ...string) *Scope {
	selected := make([]*workspace.Project, 0, len(names))
	for _, name := range names {
		for _, project := range ws.Projects {
			if project.Name == name {
				selected = append(selected, project)
			}
		}
	}
	return NewProjectScope(selected)
}

// TestUnscopedEvaluationStillFailsOnAnyBrokenDocument is the control: without a
// selection the fixture is a failing workspace, so every "the scoped run is
// clean" assertion below measures scoping rather than a healthy tree.
func TestUnscopedEvaluationStillFailsOnAnyBrokenDocument(t *testing.T) {
	ws, reader := scopedFixture(t)
	result := Aggregate(Request{Workspace: ws, Reader: reader, Revision: Revision{Kind: RevisionKindWorktree}})
	if !diag.HasErrors(result.Diagnostics) {
		t.Fatalf("unscoped evaluation = %#v, want the unrelated project's broken evidence to fail it", result.Diagnostics)
	}
	if result.Scope.Scoped {
		t.Fatalf("unscoped result reports Scope = %+v", result.Scope)
	}
}

// TestScopedEvaluationFollowsCrossProjectEvidenceAndIgnoresUnrelatedFailures
// pins the two halves of "seeds, not a filesystem wall" at once: the selected
// feature's evidence is followed into a project nobody selected, and the broken
// document in a third project neither appears nor fails the run.
func TestScopedEvaluationFollowsCrossProjectEvidenceAndIgnoresUnrelatedFailures(t *testing.T) {
	ws, reader := scopedFixture(t)
	scope := scopeFor(ws, "billing")
	result := Aggregate(Request{Workspace: ws, Reader: reader, Revision: Revision{Kind: RevisionKindWorktree}, Scope: scope})
	if diag.HasErrors(result.Diagnostics) {
		t.Fatalf("scoped evaluation failed on work outside the selection: %#v", result.Diagnostics)
	}
	if result.Snapshot == nil || len(result.Snapshot.Features) != 1 {
		t.Fatalf("scoped snapshot = %#v, want only the selected feature", result.Snapshot)
	}
	feature := result.Snapshot.Features[0]
	if feature.ID != "billing/invoice" {
		t.Fatalf("selected feature = %q", feature.ID)
	}
	if feature.Current != featureproto.MaturityCoded {
		t.Fatalf("selected feature maturity = %q, want the cross-project evidence to still count", feature.Current)
	}
	requirement := feature.Requirements[0]
	if requirement.State != VerificationVerified || len(requirement.Evidence) != 1 {
		t.Fatalf("requirement = %#v, want the followed external record to verify it", requirement)
	}
	// Provenance of a followed record is the exact foreign path, not a rewritten
	// or anonymized one.
	if got := requirement.Evidence[0].Document; got != "shipping/schema/feature-evidence/billing.json" {
		t.Fatalf("evidence document = %q, want the exact external provenance", got)
	}
	if requirement.Evidence[0].Contribution == nil {
		t.Fatalf("followed evidence lost the contribution its correctness depends on")
	}
	if !result.Scope.Scoped {
		t.Fatal("scoped result does not report itself as scoped")
	}
	wantExternal := []string{"shipping/schema/capabilities.json", "shipping/schema/feature-evidence/billing.json"}
	if !equalStrings(result.Scope.ExternalRecords, wantExternal) {
		t.Fatalf("external records = %#v, want %#v", result.Scope.ExternalRecords, wantExternal)
	}
	if !equalStrings(result.Scope.SelectedFeatures, []string{"billing/invoice"}) {
		t.Fatalf("selected features = %#v", result.Scope.SelectedFeatures)
	}
}

// TestScopedEvaluationDoesNotReadUnselectedCapabilityManifests proves the
// evaluation tier is genuinely narrowed rather than filtered afterwards: the
// reader errors on any artifact the run must not open, so touching one is a
// hard failure instead of an invisible cost.
func TestScopedEvaluationDoesNotReadUnselectedCapabilityManifests(t *testing.T) {
	ws, reader := scopedFixture(t)
	reader.files["unrelated/schema/capabilities.json"] = mustCapabilitiesV2(t, "unrelated", []capabilityproto.ConfigDefinitionV2{{
		Identity: capabilityproto.ContributionIdentity{
			OwnerProject: "unrelated", Kind: capabilityproto.ContributionKindConfig, Key: "stock",
		},
		Path: "stock",
		Provenance: capabilityproto.ProvenanceV2{
			Project: "unrelated", SourceKind: capabilityproto.SourceKindFramework,
			Declaration: capabilityproto.DeclarationLocation{Root: capabilityproto.LocationRootProject, Path: "config.go"},
		},
	}})
	unscoped := Aggregate(Request{Workspace: ws, Reader: reader, Revision: Revision{Kind: RevisionKindWorktree}})
	if !hasContainer(unscoped, "unrelated/schema/capabilities.json") && !diag.HasErrors(unscoped.Diagnostics) {
		t.Fatal("the control run never reached the unrelated capability manifest")
	}

	reader.fileErrors = map[string]error{
		"unrelated/schema/capabilities.json": errUnreadableArtifact,
	}
	result := Aggregate(Request{
		Workspace: ws, Reader: reader,
		Revision: Revision{Kind: RevisionKindWorktree}, Scope: scopeFor(ws, "billing"),
	})
	if diag.HasErrors(result.Diagnostics) {
		t.Fatalf("scoped run opened an unselected capability manifest: %#v", result.Diagnostics)
	}
}

// TestScopedEvaluationKeepsWorkspaceUniqueness pins that narrowing evaluation
// never weakens global identity: a second declaration of a SELECTED feature in
// an unselected project is still an error, still named by its own path, and
// still fails the scoped run.
func TestScopedEvaluationKeepsWorkspaceUniqueness(t *testing.T) {
	ws, reader := scopedFixture(t)
	duplicate := featureproto.Manifest{
		ProtocolVersion: featureproto.MinimumManifestProtocolVersion,
		Namespace:       "warehouse",
		Features: []featureproto.Feature{{
			ID: "billing/invoice", Type: featureproto.FeatureTypeFeature, Name: "Invoice export",
			Outcome: "Customers export issued invoices", Owner: "warehouse", Target: featureproto.MaturityModeled,
		}},
	}
	reader.files["unrelated/"+featureproto.ManifestFilename] = mustFeatureManifest(t, duplicate)
	result := Aggregate(Request{
		Workspace: ws, Reader: reader,
		Revision: Revision{Kind: RevisionKindWorktree}, Scope: scopeFor(ws, "billing"),
	})
	if !hasDiagnostic(result.Diagnostics, featureproto.ErrorCodeDuplicateFeature) {
		t.Fatalf("scoped run lost workspace uniqueness: %#v", result.Diagnostics)
	}
	encoded := diagnosticsText(t, result.Diagnostics)
	if !bytes.Contains(encoded, []byte("unrelated/"+featureproto.ManifestFilename)) {
		t.Fatalf("duplicate error lost the external declaration's provenance: %s", encoded)
	}
	if !containsString(result.Scope.ExternalRecords, "unrelated/"+featureproto.ManifestFilename) {
		t.Fatalf("external records = %#v, want the followed duplicate declaration", result.Scope.ExternalRecords)
	}
}

// TestScopedEvaluationIsDeterministic pins that the same selection over the
// same tree produces the same bytes, selection metadata included.
func TestScopedEvaluationIsDeterministic(t *testing.T) {
	var golden []byte
	var external []string
	for iteration := range 4 {
		ws, reader := scopedFixture(t)
		reader.reverseDirs = iteration%2 == 1
		result := Aggregate(Request{
			Workspace: ws, Reader: reader,
			Revision: Revision{Kind: RevisionKindWorktree}, Scope: scopeFor(ws, "billing"),
		})
		if result.Snapshot == nil {
			t.Fatalf("iteration %d produced no snapshot: %#v", iteration, result.Diagnostics)
		}
		data, err := MarshalSnapshot(result.Snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if iteration == 0 {
			golden, external = data, result.Scope.ExternalRecords
			continue
		}
		if !bytes.Equal(data, golden) {
			t.Fatalf("iteration %d drifted\nfirst:\n%s\ncurrent:\n%s", iteration, golden, data)
		}
		if !equalStrings(result.Scope.ExternalRecords, external) {
			t.Fatalf("iteration %d external records = %#v, want %#v", iteration, result.Scope.ExternalRecords, external)
		}
	}
}

// TestScopedSpecDiscoveryKeepsUniquenessAndDropsUnrelatedFailures pins the spec
// half of the same contract: every canonical location is still read so "one
// spec per feature" stays provable, while an unrelated broken document is
// attributed away instead of failing a scoped run.
func TestScopedSpecDiscoveryKeepsUniquenessAndDropsUnrelatedFailures(t *testing.T) {
	root := t.TempDir()
	writeSpecFile(t, root, "putnami.workspace.json", `{"name":"scoped","includes":["billing","shipping","unrelated"]}`)
	for _, project := range []string{"billing", "shipping", "unrelated"} {
		writeSpecFile(t, root, project+"/putnami.json", `{"name":"`+project+`","type":"application"}`)
	}
	writeSpecFile(t, root, "billing/specs/invoice.json", `{"protocolVersion":1,"feature":"billing/invoice","outcomes":["Customers export invoices"],"requirements":[]}`)
	// A spec for the SEED's feature, hosted by another project.
	writeSpecFile(t, root, "shipping/specs/invoice-detail.json", `{"protocolVersion":1,"feature":"billing/invoice-detail","outcomes":["Customers see line items"],"requirements":[]}`)
	writeSpecFile(t, root, "unrelated/specs/stock.json", `{"protocolVersion":1,"feature":"warehouse/stock","outcomes":["Operators see stock"],"requirements":[]}`)
	writeSpecFile(t, root, "unrelated/specs/broken.json", `{"protocolVersion":1,"feature":"warehouse/stock",}`)

	// Core reached the same shape through workspace.Load(root). An extension has
	// no loader — membership arrives on the wire — so the three members the
	// includes above declare are stated directly. Same resolved identities
	// (explicit name, ID from path, type from putnami.json), so every assertion
	// below still measures spec discovery rather than discovery of the tree.
	ws := workspace.NewWorkspace(root,
		&workspaceproto.Config{Name: "scoped", Includes: []string{"billing", "shipping", "unrelated"}},
		[]*workspace.Project{
			{ID: "/billing", Name: "billing", SourceName: "billing", Type: "application", Path: "billing"},
			{ID: "/shipping", Name: "shipping", SourceName: "shipping", Type: "application", Path: "shipping"},
			{ID: "/unrelated", Name: "unrelated", SourceName: "unrelated", Type: "application", Path: "unrelated"},
		})
	if unscoped := DiscoverSpecs(ws, nil); !diag.HasErrors(unscoped.Diagnostics) {
		t.Fatalf("unscoped discovery = %#v, want the broken document to be reported", unscoped.Diagnostics)
	}

	scope := scopeFor(ws, "billing")
	scope.SelectFeatures([]string{"billing/invoice", "billing/invoice-detail"})
	scoped := DiscoverSpecs(ws, scope)
	if diag.HasErrors(scoped.Diagnostics) {
		t.Fatalf("scoped discovery failed on a document outside the selection: %#v", scoped.Diagnostics)
	}
	// Every readable document is still returned, including the unrelated
	// project's: "one spec per feature" is a workspace-wide guarantee, and a
	// subset cannot prove it.
	if len(scoped.Specs) != 3 {
		t.Fatalf("scoped discovery read %d document(s), want every canonical location so uniqueness stays provable", len(scoped.Specs))
	}
	if len(scoped.Selected) != 2 {
		t.Fatalf("selected specs = %#v, want the seed's document plus the followed one", scoped.Selected)
	}
	if len(scoped.External) != 1 || scoped.External[0].Path != "shipping/specs/invoice-detail.json" {
		t.Fatalf("external specs = %#v, want the exact followed path", scoped.External)
	}
	if scoped.InScope("unrelated/specs/broken.json") {
		t.Fatal("a document detailing no selected feature was kept in scope")
	}
}

// errUnreadableArtifact makes "this run must not open that file" a hard failure
// rather than an invisible cost: the reader refuses it, so a widened read shows
// up as a diagnostic instead of as an unmeasured extra syscall.
var errUnreadableArtifact = errors.New("artifact must not be read under this selection")

func hasContainer(result Result, path string) bool {
	if result.Snapshot == nil {
		return false
	}
	for _, contribution := range result.Snapshot.Unclassified {
		for _, container := range contribution.Containers {
			if container == path {
				return true
			}
		}
	}
	return false
}

func diagnosticsText(t *testing.T, diagnostics []diag.Diagnostic) []byte {
	t.Helper()
	encoded, err := json.Marshal(diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func writeSpecFile(t *testing.T, root, relative, contents string) {
	t.Helper()
	target := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
