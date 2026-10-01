package features

import (
	"encoding/json"
	"fmt"
	"sort"

	capabilityproto "go.putnami.dev/protocol/capabilities"
	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
)

type contributionCatalog struct {
	all              []ContributionAssessment
	byIdentity       map[string]ContributionAssessment
	resolutionErrors map[string][]diag.Diagnostic
	diagnostics      []diag.Diagnostic
}

func buildContributionCatalog(containers []capabilityproto.ManifestContainer) contributionCatalog {
	catalog := contributionCatalog{
		byIdentity:       make(map[string]ContributionAssessment),
		resolutionErrors: make(map[string][]diag.Diagnostic),
	}
	identitySet := make(map[capabilityproto.ContributionIdentity]bool)
	for _, container := range containers {
		if container.Manifest == nil {
			continue
		}
		if container.Manifest.V2 != nil {
			for _, identity := range v2Identities(container.Manifest.V2) {
				identitySet[identity] = true
			}
			continue
		}
		if container.Manifest.V1 != nil {
			for _, identity := range v1ReferenceableIdentities(container.Manifest.V1) {
				identitySet[identity] = true
			}
			catalog.all = append(catalog.all, v1MigrationAssessments(container.Path, container.Manifest.V1)...)
		}
	}
	identities := make([]capabilityproto.ContributionIdentity, 0, len(identitySet))
	for identity := range identitySet {
		identities = append(identities, identity)
	}
	sort.Slice(identities, func(i, j int) bool {
		return capabilityproto.CompareContributionIdentity(identities[i], identities[j]) < 0
	})
	for _, identity := range identities {
		resolved, findings := capabilityproto.ResolveContribution(identity, containers)
		key := contributionKey(identity)
		if len(findings) > 0 {
			field := "capabilities#contribution"
			if len(containers) > 0 {
				field = containers[0].Path + "#contribution"
			}
			prefixed := prefixContributionDiagnostics(field, findings)
			catalog.resolutionErrors[key] = prefixed
			catalog.diagnostics = append(catalog.diagnostics, prefixed...)
			continue
		}
		assessment, err := contributionFromResolved(resolved)
		if err != nil {
			finding := diag.Errorf("capabilities.parse_error", "capabilities#contribution", "resolved contribution could not be normalized")
			catalog.resolutionErrors[key] = []diag.Diagnostic{finding}
			catalog.diagnostics = append(catalog.diagnostics, finding)
			continue
		}
		catalog.byIdentity[key] = assessment
		catalog.all = append(catalog.all, assessment)
	}
	validated := catalog.all[:0]
	for _, assessment := range catalog.all {
		findings := validateContributionInspectionMetadata(assessment)
		if len(findings) == 0 {
			validated = append(validated, assessment)
			continue
		}
		catalog.diagnostics = append(catalog.diagnostics, findings...)
		if assessment.Referenceable {
			key := contributionKey(assessment.Identity)
			delete(catalog.byIdentity, key)
			catalog.resolutionErrors[key] = findings
		}
	}
	catalog.all = validated
	sort.Slice(catalog.all, func(i, j int) bool { return compareContribution(catalog.all[i], catalog.all[j]) < 0 })
	return catalog
}

func validateContributionInspectionMetadata(assessment ContributionAssessment) []diag.Diagnostic {
	base := "capabilities#provenance"
	if len(assessment.Containers) > 0 {
		base = assessment.Containers[0] + "#provenance"
	}
	checkPath := func(field, value string) *diag.Diagnostic {
		if value == "" {
			return nil
		}
		kind := validateRelativePath(value, false)
		if kind == "" {
			return nil
		}
		code := capabilityproto.ErrorCodeInvalidPath
		if kind == ReaderErrorPathEscape {
			code = capabilityproto.ErrorCodePathEscape
		}
		finding := diag.Errorf(code, base+"."+field, "inspection path is not a bounded contained relative slash path")
		return &finding
	}
	checkText := func(field, value string) *diag.Diagnostic {
		if boundedInspectionText(value, maxSemanticMetadataBytes) {
			return nil
		}
		finding := diag.Errorf(featureproto.ErrorCodeSensitiveContent, base+"."+field, "inspection metadata is not bounded safe text")
		return &finding
	}
	var diagnostics []diag.Diagnostic
	for field, value := range map[string]string{
		"identity.ownerProject": assessment.Identity.OwnerProject,
		"identity.subkind":      assessment.Identity.Subkind,
		"identity.key":          assessment.Identity.Key,
		"project":               assessment.Provenance.Project,
		"package":               assessment.Provenance.Package,
		"version":               assessment.Provenance.Version,
	} {
		if finding := checkText(field, value); finding != nil {
			diagnostics = append(diagnostics, *finding)
		}
	}
	for index, container := range assessment.Containers {
		if finding := checkPath(fmt.Sprintf("containers[%d]", index), container); finding != nil {
			diagnostics = append(diagnostics, *finding)
		}
	}
	if finding := checkPath("v1EvidencePath", assessment.Provenance.V1EvidencePath); finding != nil {
		diagnostics = append(diagnostics, *finding)
	}
	if assessment.Provenance.Declaration != nil {
		if finding := checkPath("declaration.path", assessment.Provenance.Declaration.Path); finding != nil {
			diagnostics = append(diagnostics, *finding)
		}
		if assessment.Provenance.Declaration.Symbol != "" {
			if finding := checkText("declaration.symbol", assessment.Provenance.Declaration.Symbol); finding != nil {
				diagnostics = append(diagnostics, *finding)
			}
		}
	}
	for index, artifact := range assessment.Provenance.Artifacts {
		if finding := checkPath(fmt.Sprintf("artifacts[%d].path", index), artifact.Path); finding != nil {
			diagnostics = append(diagnostics, *finding)
		}
	}
	return diagnostics
}

func boundedInspectionText(value string, maximum int) bool {
	if len(value) > maximum {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func prefixContributionDiagnostics(field string, findings []diag.Diagnostic) []diag.Diagnostic {
	result := make([]diag.Diagnostic, 0, len(findings))
	for _, finding := range findings {
		if finding.Field == "" || finding.Field == "contribution" {
			finding.Field = field
		} else {
			finding.Field = field + "." + finding.Field
		}
		result = append(result, finding)
	}
	return result
}

func contributionFromResolved(resolved *capabilityproto.ResolvedContribution) (ContributionAssessment, error) {
	var envelope struct {
		Identity   capabilityproto.ContributionIdentity `json:"identity"`
		Provenance json.RawMessage                      `json:"provenance"`
	}
	if err := json.Unmarshal(resolved.CanonicalCopy, &envelope); err != nil {
		return ContributionAssessment{}, err
	}
	assessment := ContributionAssessment{
		Identity:        resolved.Identity,
		ProtocolVersion: resolved.ProtocolVersion,
		Referenceable:   true,
		Containers:      append([]string(nil), resolved.Containers...),
	}
	if resolved.ProtocolVersion == capabilityproto.ProtocolVersionV2 {
		var provenance capabilityproto.ProvenanceV2
		if err := json.Unmarshal(envelope.Provenance, &provenance); err != nil {
			return ContributionAssessment{}, err
		}
		declaration := provenance.Declaration
		assessment.Provenance = ContributionProvenance{
			Project:     provenance.Project,
			Package:     provenance.Package,
			Version:     provenance.Version,
			SourceKind:  provenance.SourceKind,
			Declaration: &declaration,
			Artifacts:   append([]capabilityproto.ArtifactLocation(nil), provenance.Artifacts...),
		}
		return assessment, nil
	}
	var provenance capabilityproto.Provenance
	if err := json.Unmarshal(envelope.Provenance, &provenance); err != nil {
		return ContributionAssessment{}, err
	}
	assessment.Provenance = normalizeV1Provenance(provenance)
	return assessment, nil
}

func normalizeV1Provenance(provenance capabilityproto.Provenance) ContributionProvenance {
	return ContributionProvenance{
		Project:        provenance.Project,
		Package:        provenance.Package,
		Version:        provenance.Version,
		SourceKind:     provenance.SourceKind,
		V1EvidencePath: provenance.EvidencePath,
	}
}

func v2Identities(manifest *capabilityproto.ManifestV2) []capabilityproto.ContributionIdentity {
	if manifest == nil {
		return nil
	}
	var identities []capabilityproto.ContributionIdentity
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
	for _, value := range manifest.PackageVersions {
		identities = append(identities, value.Identity)
	}
	for _, value := range manifest.RequiredCapabilities {
		identities = append(identities, value.Identity)
	}
	for _, value := range manifest.DomainAccess {
		identities = append(identities, value.Identity)
	}
	return identities
}

func v1ReferenceableIdentities(manifest *capabilityproto.Manifest) []capabilityproto.ContributionIdentity {
	if manifest == nil {
		return nil
	}
	var identities []capabilityproto.ContributionIdentity
	for _, value := range manifest.ConfigDefinitions {
		identities = append(identities, capabilityproto.ContributionIdentity{OwnerProject: value.Provenance.Project, Kind: capabilityproto.ContributionKindConfig, Key: value.Path})
	}
	for _, value := range manifest.Schemas {
		identities = append(identities, capabilityproto.ContributionIdentity{OwnerProject: value.Provenance.Project, Kind: capabilityproto.ContributionKindSchema, Subkind: string(value.Kind), Key: value.Name})
	}
	for _, value := range manifest.Discoverers {
		identities = append(identities, capabilityproto.ContributionIdentity{OwnerProject: value.Provenance.Project, Kind: capabilityproto.ContributionKindDiscoverer, Subkind: string(value.Kind), Key: value.Name})
	}
	for _, value := range manifest.InfraRequirements {
		identities = append(identities, capabilityproto.ContributionIdentity{OwnerProject: value.Provenance.Project, Kind: capabilityproto.ContributionKindInfra, Subkind: string(value.Kind), Key: value.Name})
	}
	for _, value := range manifest.HealthContributors {
		identities = append(identities, capabilityproto.ContributionIdentity{OwnerProject: value.Provenance.Project, Kind: capabilityproto.ContributionKindHealth, Subkind: string(value.Probe), Key: value.Name})
	}
	for _, value := range manifest.LifecycleHooks {
		identities = append(identities, capabilityproto.ContributionIdentity{OwnerProject: value.Provenance.Project, Kind: capabilityproto.ContributionKindLifecycle, Subkind: string(value.Phase), Key: value.Name})
	}
	for _, value := range manifest.PackageVersions {
		identities = append(identities, capabilityproto.ContributionIdentity{OwnerProject: value.Provenance.Project, Kind: capabilityproto.ContributionKindPackage, Key: value.Package})
	}
	for _, value := range manifest.RequiredCapabilities {
		identities = append(identities, capabilityproto.ContributionIdentity{OwnerProject: value.Provenance.Project, Kind: capabilityproto.ContributionKindRequiredCapability, Key: value.Name})
	}
	return identities
}

func v1MigrationAssessments(container string, manifest *capabilityproto.Manifest) []ContributionAssessment {
	if manifest == nil {
		return nil
	}
	result := make([]ContributionAssessment, 0, len(manifest.Migrations))
	for _, value := range manifest.Migrations {
		result = append(result, ContributionAssessment{
			Identity: capabilityproto.ContributionIdentity{
				OwnerProject: value.Provenance.Project,
				Kind:         capabilityproto.ContributionKindMigration,
				Key:          value.Name,
			},
			ProtocolVersion: capabilityproto.ProtocolVersion,
			Referenceable:   false,
			Containers:      []string{container},
			Provenance:      normalizeV1Provenance(value.Provenance),
		})
	}
	return result
}

func contributionKey(identity capabilityproto.ContributionIdentity) string {
	return identity.OwnerProject + "\x00" + string(identity.Kind) + "\x00" + identity.Subkind + "\x00" + identity.Key
}
