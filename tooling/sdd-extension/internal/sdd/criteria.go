package sdd

import (
	"sort"

	featureproto "go.putnami.dev/protocol/features"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// This file is the extension's half of the settled spec-gate split
// (decision 9, fixed): @putnami/sdd judges what a run is EXPECTED to
// prove, and only core — which holds every job's results — collects
// observations and sanctions. The projection built here is therefore derived
// exclusively from durable manifests and specs, carries no observation, no
// policy, and no verdict, and is emitted by the specs-validate task as a
// bounded declared artifact core can join through the pure evaluator.

// BuildSpecValidationWithCriteria runs the same validation as
// BuildSpecValidationResult and additionally derives the executable-criteria
// projection from the one repository load, so the two answers can never come
// from two different discovery passes. The projection is nil while the
// repository is structurally broken: criteria derived from documents the
// validator refused would project identities nobody agreed to.
func BuildSpecValidationWithCriteria(ws *workspace.Workspace, selection Selection) (SpecValidationReport, *featureproto.SpecCriteriaProjection, error) {
	repository, err := loadSpecRepository(ws, selection)
	if err != nil {
		return SpecValidationReport{}, nil, err
	}
	report, verdict := buildSpecValidationFromRepository(repository)
	if verdict != nil {
		return report, nil, verdict
	}
	return report, criteriaProjectionFromRepository(repository), nil
}

// criteriaProjectionFromRepository projects the selected specs onto the
// (feature, requirement) join the gate consumes: every textual requirement
// identity, and the same-ID authored feature requirements wherever the feature
// is authored. Requirements the spec never states are deliberately absent.
func criteriaProjectionFromRepository(repository *specRepository) *featureproto.SpecCriteriaProjection {
	authored := authoredFeaturesByID(repository.manifests)
	projection := &featureproto.SpecCriteriaProjection{
		ProtocolVersion: featureproto.SpecCriteriaProjectionProtocolVersion,
		Groups:          make([]featureproto.SpecCriteriaGroup, 0, len(repository.discovery.Selected)),
	}
	seen := make(map[string]bool, len(repository.discovery.Selected))
	for _, discovered := range repository.discovery.Selected {
		spec := discovered.Spec
		if spec == nil || seen[spec.Feature] {
			continue
		}
		seen[spec.Feature] = true
		identities := textualRequirementIDs(spec)
		if len(identities) == 0 {
			continue
		}
		group := featureproto.SpecCriteriaGroup{
			Feature:          spec.Feature,
			Spec:             discovered.Path,
			SpecRequirements: identities,
		}
		if feature, exists := authored[spec.Feature]; exists {
			for _, requirement := range feature.Requirements {
				if containsIdentity(identities, requirement.ID) {
					group.Requirements = append(group.Requirements, requirement)
				}
			}
		}
		projection.Groups = append(projection.Groups, group)
	}
	return featureproto.CanonicalSpecCriteriaProjection(projection)
}

// authoredFeaturesByID indexes every authored feature. Manifests are read in
// sorted path order and the first declaration wins, matching the stability
// rule the spec repository already applies to duplicate documents; a genuine
// duplicate is a validation error that prevents the projection from being
// built at all.
func authoredFeaturesByID(manifests []featureproto.ManifestSource) map[string]featureproto.Feature {
	sources := append([]featureproto.ManifestSource(nil), manifests...)
	sort.Slice(sources, func(i, j int) bool { return sources[i].Path < sources[j].Path })
	features := make(map[string]featureproto.Feature)
	for _, source := range sources {
		if source.Manifest == nil {
			continue
		}
		canonical := featureproto.CanonicalManifest(source.Manifest)
		for _, feature := range canonical.Features {
			if _, exists := features[feature.ID]; !exists {
				features[feature.ID] = feature
			}
		}
	}
	return features
}

func textualRequirementIDs(spec *featureproto.Spec) []string {
	identities := make([]string, 0, len(spec.Requirements))
	seen := make(map[string]bool, len(spec.Requirements))
	for _, requirement := range spec.Requirements {
		if requirement.ID == "" || seen[requirement.ID] {
			continue
		}
		seen[requirement.ID] = true
		identities = append(identities, requirement.ID)
	}
	sort.Strings(identities)
	return identities
}

func containsIdentity(identities []string, id string) bool {
	for _, candidate := range identities {
		if candidate == id {
			return true
		}
	}
	return false
}
