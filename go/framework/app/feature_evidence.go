package app

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	protocaps "go.putnami.dev/protocol/capabilities"
	diag "go.putnami.dev/protocol/diagnostic"
	protofeatures "go.putnami.dev/protocol/features"
)

const featureEvidenceFilename = "go-framework.json"

// featureEvidenceIssuerID names this producer on every record it emits. The
// issuer kind is build: the assertion is that a build observed the fact, not
// that a framework asserted it.
const featureEvidenceIssuerID = "go.putnami.dev/app"

// maxAuthoredManifestBytes bounds the authored intent read during describe.
// The document is a small semantic catalog; anything larger is a defect rather
// than a scale requirement, and describe must not be a path for reading
// arbitrarily large project files into memory.
const maxAuthoredManifestBytes = 1 << 20

// collectFeatureProofMappings walks the module tree and turns every native
// Module.Feature(...) declaration into protocol mappings.
//
// Provenance is the feature declaration's own call site, captured by
// callerDesignSource when the module declared it — never a value an author
// writes. A declaration whose call site could not be reduced to a contained
// project-relative path is refused rather than published without provenance:
// evidence a reader cannot locate is worse than no evidence.
func collectFeatureProofMappings(root *Module) ([]protofeatures.GeneratedEvidenceMapping, error) {
	var mappings []protofeatures.GeneratedEvidenceMapping
	var walk func(module *Module) error
	walk = func(module *Module) error {
		if feature := module.feature; feature != nil && len(feature.Proves) > 0 {
			source := feature.provenance.protocol()
			if source == nil || source.Path == "" {
				return fmt.Errorf("feature %q proves a requirement but its declaration site could not be resolved to a project-relative path", feature.ID)
			}
			for index, proof := range feature.Proves {
				if strings.TrimSpace(proof.Requirement) == "" {
					return fmt.Errorf("feature %q proof[%d] must name a requirement", feature.ID, index)
				}
				mappings = append(mappings, protofeatures.GeneratedEvidenceMapping{
					Feature:     feature.ID,
					Requirement: proof.Requirement,
					Kind:        proof.Contribution.Kind,
					Subkind:     proof.Contribution.Subkind,
					Key:         proof.Contribution.Key,
					Provenance: protofeatures.EvidenceProvenance{
						Root: protofeatures.LocationRootProject, Path: source.Path, Symbol: source.Symbol,
					},
				})
			}
		}
		for _, child := range module.modules {
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root); err != nil {
		return nil, err
	}
	return mappings, nil
}

// readAuthoredFeatureManifest reads putnami.features.json from the project
// root. Absence is not an error here: it makes every mapping an unknown-feature
// failure in the resolver, which is a clearer report than "no manifest" from a
// project that declared no proof at all.
func readAuthoredFeatureManifest(projectRoot string) (*protofeatures.Manifest, error) {
	path := filepath.Join(projectRoot, protofeatures.ManifestFilename)
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("authored feature manifest %s is not a regular file", protofeatures.ManifestFilename)
	}
	if info.Size() > maxAuthoredManifestBytes {
		return nil, fmt.Errorf("authored feature manifest %s exceeds the bounded describe read limit", protofeatures.ManifestFilename)
	}
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	manifest, findings := protofeatures.ParseAndValidateManifest(data)
	if manifest == nil || diag.HasErrors(findings) {
		return nil, fmt.Errorf("authored feature manifest %s is not valid:\n%s", protofeatures.ManifestFilename, formatFeatureDiagnostics(findings))
	}
	return manifest, nil
}

// buildFeatureEvidenceDocument emits generated evidence for the explicit
// mappings declared beside this project's native features.
//
// It runs after the capability manifest is canonical and validated, so every
// record resolves against the exact manifest that ships beside it. The
// resolver owns the association rules; this function only supplies what the
// build knows — the published identities, the owner project, and its computed
// source binding.
func buildFeatureEvidenceDocument(projectRoot string, root *Module, manifest *protocaps.ManifestV2, inventory *capabilityInventory) (*protofeatures.EvidenceDocument, error) {
	mappings, err := collectFeatureProofMappings(root)
	if err != nil {
		return nil, err
	}
	if len(mappings) == 0 {
		return nil, nil
	}
	// A record states which source produced it. A build that makes no source
	// claim has nothing to state, so it writes no evidence, and the readers
	// report the project's evidence as unavailable.
	if inventory.makesNoSourceClaim(manifest.Project) {
		return nil, nil
	}
	binding, err := inventory.bindingForOwner(manifest.Project)
	if err != nil {
		return nil, err
	}
	authored, err := readAuthoredFeatureManifest(projectRoot)
	if err != nil {
		return nil, err
	}
	document, findings := protofeatures.BuildGeneratedEvidence(protofeatures.GeneratedEvidenceInput{
		Issuer: protofeatures.Issuer{Kind: protofeatures.IssuerKindBuild, ID: featureEvidenceIssuerID},
		Source: protofeatures.SourceSelector{
			Root: protofeatures.LocationRootProject, OwnerProject: manifest.Project, Binding: binding,
		},
		Authored:      authored,
		Contributions: publishedContributionIdentities(manifest),
		Mappings:      mappings,
	})
	if document == nil {
		return nil, fmt.Errorf("feature evidence generation failed:\n%s", formatFeatureDiagnostics(findings))
	}
	return document, nil
}

// publishedContributionIdentities lists every identity the manifest publishes,
// including the ones merged in from dependencies. Ownership is enforced by the
// resolver rather than filtered here, so a mapping that names a dependency's
// contribution reports "unresolved" against the real published set instead of
// silently disappearing from a pre-filtered one.
func publishedContributionIdentities(manifest *protocaps.ManifestV2) []protocaps.ContributionIdentity {
	identities := make([]protocaps.ContributionIdentity, 0,
		len(manifest.ConfigDefinitions)+len(manifest.Schemas)+len(manifest.Discoverers)+
			len(manifest.Migrations)+len(manifest.InfraRequirements)+len(manifest.HealthContributors)+
			len(manifest.LifecycleHooks)+len(manifest.Packages)+len(manifest.RequiredCapabilities))
	for _, value := range manifest.ConfigDefinitions {
		identities = append(identities, value.Identity)
	}
	for _, value := range manifest.Schemas {
		identities = append(identities, value.Identity)
	}
	for _, value := range manifest.Discoverers {
		identities = append(identities, value.Identity)
	}
	for _, value := range manifest.Migrations {
		identities = append(identities, value.Identity)
	}
	for _, value := range manifest.InfraRequirements {
		identities = append(identities, value.Identity)
	}
	for _, value := range manifest.HealthContributors {
		identities = append(identities, value.Identity)
	}
	for _, value := range manifest.LifecycleHooks {
		identities = append(identities, value.Identity)
	}
	for _, value := range manifest.Packages {
		identities = append(identities, value.Identity)
	}
	for _, value := range manifest.RequiredCapabilities {
		identities = append(identities, value.Identity)
	}
	return identities
}

func formatFeatureDiagnostics(findings []diag.Diagnostic) string {
	lines := make([]string, 0, len(findings))
	for _, finding := range findings {
		lines = append(lines, fmt.Sprintf("[%s] %s: %s", finding.Code, finding.Field, finding.Message))
	}
	return strings.Join(lines, "\n")
}

func capabilityIdentityLabel(identity protocaps.ContributionIdentity) string {
	return fmt.Sprintf("(%q,%q,%q,%q)", identity.OwnerProject, identity.Kind, identity.Subkind, identity.Key)
}
