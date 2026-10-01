package capabilities

import (
	"fmt"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// ManifestContainer associates a strict manifest with its canonical discovery
// path. Container identity is inspection metadata and never enters semantic
// contribution identity or copy equivalence.
type ManifestContainer struct {
	// Path is the canonical discovery path of the manifest container.
	Path string
	// Manifest is the strictly parsed manifest found at Path.
	Manifest *ManifestDocument
}

// ResolvedContribution is one logical owner-scoped contribution after
// byte-identical copies have coalesced.
type ResolvedContribution struct {
	// Identity is the semantic owner-scoped identity that was resolved.
	Identity ContributionIdentity
	// ProtocolVersion is the version of the selected canonical copy.
	ProtocolVersion int
	// CanonicalCopy is the deterministic JSON encoding of the selected entry.
	CanonicalCopy []byte
	// Containers lists every path holding an equivalent copy.
	Containers []string
}

type contributionCandidate struct {
	identity   ContributionIdentity
	version    int
	copy       []byte
	projection []byte
	container  string
}

// ValidateContributionReference validates the complete canonical reference
// tuple independently of any containing manifest.
func ValidateContributionReference(ref ContributionReference) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if strings.TrimSpace(ref.OwnerProject) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeMissingOwnerProject, "contribution.ownerProject", "ownerProject is required"))
	}
	if !ValidContributionKinds[ref.Kind] {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidContributionIdentity, "contribution.kind", "contribution kind %q is not in the v2 set", ref.Kind))
	}
	if strings.TrimSpace(ref.Key) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidContributionIdentity, "contribution.key", "contribution key is required"))
	}
	requiresSubkind := ref.Kind != ContributionKindConfig && ref.Kind != ContributionKindPackage && ref.Kind != ContributionKindRequiredCapability
	if requiresSubkind && strings.TrimSpace(ref.Subkind) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidContributionIdentity, "contribution.subkind", "subkind is required for contribution kind %q", ref.Kind))
	}
	if !requiresSubkind && ref.Subkind != "" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidContributionIdentity, "contribution.subkind", "subkind must be absent for contribution kind %q", ref.Kind))
	}
	return diags
}

// ResolveContribution searches every supplied manifest by semantic owner. It
// coalesces byte-identical cross-container copies, applies the deterministic
// v1/v2 projection rule, and never selects a winner by discovery order.
func ResolveContribution(ref ContributionReference, containers []ManifestContainer) (*ResolvedContribution, []diag.Diagnostic) {
	if diags := ValidateContributionReference(ref); diag.HasErrors(diags) {
		return nil, diags
	}
	ordered := append([]ManifestContainer(nil), containers...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	var matches []contributionCandidate
	sawV1Migration := false
	for _, container := range ordered {
		if container.Manifest == nil {
			continue
		}
		candidates, v1Migration := manifestCandidates(container.Path, container.Manifest, ref)
		matches = append(matches, candidates...)
		sawV1Migration = sawV1Migration || v1Migration
	}
	if len(matches) == 0 {
		if sawV1Migration {
			return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeV1Unreferenceable, "contribution", "v1 migration %q owned by %q has no wire-level migration kind and cannot be referenced", ref.Key, ref.OwnerProject)}
		}
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeUnresolvedReference, "contribution", "no contribution resolves identity %s", identityLabel(ref))}
	}

	seenContainer := make(map[string]bool)
	for _, candidate := range matches {
		if seenContainer[candidate.container] {
			return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeDuplicateContribution, "contribution", "manifest %q contains duplicate identity %s", candidate.container, identityLabel(ref))}
		}
		seenContainer[candidate.container] = true
	}

	var v1, v2 []contributionCandidate
	for _, candidate := range matches {
		if candidate.version == ProtocolVersionV2 {
			v2 = append(v2, candidate)
		} else {
			v1 = append(v1, candidate)
		}
	}
	conflict := false
	if len(v2) > 0 {
		for _, candidate := range v2[1:] {
			if !equalCanonical(v2[0].copy, candidate.copy) {
				conflict = true
			}
		}
		for _, candidate := range v1 {
			if !equalCanonical(v2[0].projection, candidate.projection) {
				conflict = true
			}
		}
	} else {
		for _, candidate := range v1[1:] {
			if !equalCanonical(v1[0].copy, candidate.copy) {
				conflict = true
			}
		}
	}
	containerPaths := make([]string, 0, len(seenContainer))
	for value := range seenContainer {
		containerPaths = append(containerPaths, value)
	}
	sort.Strings(containerPaths)
	if conflict {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeConflictingContributionCopy, "contribution", "contribution %s has conflicting copies in containers: %s", identityLabel(ref), strings.Join(containerPaths, ", "))}
	}
	var winner contributionCandidate
	if len(v2) > 0 {
		winner = v2[0]
	} else {
		winner = v1[0]
	}
	return &ResolvedContribution{Identity: ref, ProtocolVersion: winner.version, CanonicalCopy: append([]byte(nil), winner.copy...), Containers: containerPaths}, nil
}

func identityLabel(identity ContributionIdentity) string {
	return fmt.Sprintf("(%q,%q,%q,%q)", identity.OwnerProject, identity.Kind, identity.Subkind, identity.Key)
}

func manifestCandidates(container string, document *ManifestDocument, ref ContributionReference) ([]contributionCandidate, bool) {
	if document.V2 != nil {
		return v2Candidates(container, document.V2, ref), false
	}
	if document.V1 != nil {
		return v1Candidates(container, document.V1, ref)
	}
	return nil, false
}

type projectionProvenance struct {
	Project    string     `json:"project"`
	Package    string     `json:"package,omitempty"`
	Version    string     `json:"version,omitempty"`
	SourceKind SourceKind `json:"sourceKind"`
}

func projectionV1(p Provenance) projectionProvenance {
	return projectionProvenance{p.Project, p.Package, p.Version, p.SourceKind}
}
func projectionV2(p ProvenanceV2) projectionProvenance {
	return projectionProvenance{p.Project, p.Package, p.Version, p.SourceKind}
}

func makeCandidate(container string, version int, identity ContributionIdentity, entry, projection any) contributionCandidate {
	copyBytes, _ := canonicalJSON(entry)
	projectionBytes, _ := canonicalJSON(projection)
	return contributionCandidate{identity: identity, version: version, copy: copyBytes, projection: projectionBytes, container: container}
}

func v2Candidates(container string, manifest *ManifestV2, ref ContributionReference) []contributionCandidate {
	// Historical v2 package/version entries remain readable and referenceable,
	// even though current canonical emission drops them — including the closure
	// entries emission now scopes away — so a reference into an older manifest
	// still resolves. Source bindings stay excluded because they were never part
	// of contribution equivalence.
	m := canonicalManifestV2(manifest, true, false, false)
	var out []contributionCandidate
	add := func(identity ContributionIdentity, entry, projection any) {
		if identity == ref {
			out = append(out, makeCandidate(container, ProtocolVersionV2, identity, entry, projection))
		}
	}
	for _, v := range m.ConfigDefinitions {
		add(v.Identity, v, struct {
			Path       string               `json:"path"`
			Fields     []ConfigField        `json:"fields,omitempty"`
			Provenance projectionProvenance `json:"provenance"`
		}{v.Path, v.Fields, projectionV2(v.Provenance)})
	}
	for _, v := range m.Schemas {
		add(v.Identity, v, struct {
			Name       string               `json:"name"`
			Kind       SchemaKind           `json:"kind"`
			Path       string               `json:"path,omitempty"`
			Provenance projectionProvenance `json:"provenance"`
		}{v.Name, v.Kind, v.Path, projectionV2(v.Provenance)})
	}
	for _, v := range m.Discoverers {
		add(v.Identity, v, struct {
			Name       string               `json:"name"`
			Kind       DiscovererKind       `json:"kind"`
			Provenance projectionProvenance `json:"provenance"`
		}{v.Name, v.Kind, projectionV2(v.Provenance)})
	}
	for _, v := range m.Migrations {
		add(v.Identity, v, struct {
			Name       string               `json:"name"`
			Datasource string               `json:"datasource"`
			Digest     string               `json:"digest"`
			Provenance projectionProvenance `json:"provenance"`
		}{v.Name, v.Datasource, v.Digest, projectionV2(v.Provenance)})
	}
	for _, v := range m.InfraRequirements {
		add(v.Identity, v, struct {
			Name       string               `json:"name"`
			Kind       InfraKind            `json:"kind"`
			Provenance projectionProvenance `json:"provenance"`
		}{v.Name, v.Kind, projectionV2(v.Provenance)})
	}
	for _, v := range m.HealthContributors {
		add(v.Identity, v, struct {
			Name       string               `json:"name"`
			Probe      ProbeKind            `json:"probe"`
			Provenance projectionProvenance `json:"provenance"`
		}{v.Name, v.Probe, projectionV2(v.Provenance)})
	}
	for _, v := range m.LifecycleHooks {
		add(v.Identity, v, struct {
			Name       string               `json:"name"`
			Phase      LifecyclePhase       `json:"phase"`
			Provenance projectionProvenance `json:"provenance"`
		}{v.Name, v.Phase, projectionV2(v.Provenance)})
	}
	for _, v := range m.Packages {
		add(v.Identity, v, struct {
			Package    string               `json:"package"`
			Provenance projectionProvenance `json:"provenance"`
		}{v.Package, projectionV2(v.Provenance)})
	}
	for _, v := range m.PackageVersions {
		add(v.Identity, v, struct {
			Package    string               `json:"package"`
			Version    string               `json:"version"`
			Provenance projectionProvenance `json:"provenance"`
		}{v.Package, v.Version, projectionV2(v.Provenance)})
	}
	for _, v := range m.RequiredCapabilities {
		add(v.Identity, v, struct {
			Name       string               `json:"name"`
			Requires   []CapabilityKind     `json:"requires"`
			Provenance projectionProvenance `json:"provenance"`
		}{v.Name, v.Requires, projectionV2(v.Provenance)})
	}
	for _, v := range m.DomainAccess {
		add(v.Identity, v, struct {
			Import     string                     `json:"import"`
			Mode       string                     `json:"mode"`
			Status     string                     `json:"status"`
			Transports []DomainAccessTransportV2  `json:"transports,omitempty"`
			Enforced   *DomainAccessEnforcementV2 `json:"enforced,omitempty"`
			Provenance projectionProvenance       `json:"provenance"`
		}{v.Import, v.Mode, v.Status, v.Transports, v.Enforced, projectionV2(v.Provenance)})
	}
	return out
}

func v1Candidates(container string, manifest *Manifest, ref ContributionReference) ([]contributionCandidate, bool) {
	var out []contributionCandidate
	add := func(identity ContributionIdentity, entry, projection any) {
		if identity != ref {
			return
		}
		out = append(out, makeCandidate(container, ProtocolVersion, identity, entry, projection))
	}
	for _, v := range manifest.ConfigDefinitions {
		identity := ContributionIdentity{v.Provenance.Project, ContributionKindConfig, "", v.Path}
		add(identity, struct {
			Identity   ContributionIdentity `json:"identity"`
			Path       string               `json:"path"`
			Fields     []ConfigField        `json:"fields,omitempty"`
			Provenance Provenance           `json:"provenance"`
		}{identity, v.Path, v.Fields, v.Provenance}, struct {
			Path       string               `json:"path"`
			Fields     []ConfigField        `json:"fields,omitempty"`
			Provenance projectionProvenance `json:"provenance"`
		}{v.Path, v.Fields, projectionV1(v.Provenance)})
	}
	for _, v := range manifest.Schemas {
		identity := ContributionIdentity{v.Provenance.Project, ContributionKindSchema, string(v.Kind), v.Name}
		add(identity, struct {
			Identity   ContributionIdentity `json:"identity"`
			Name       string               `json:"name"`
			Kind       SchemaKind           `json:"kind"`
			Path       string               `json:"path,omitempty"`
			Provenance Provenance           `json:"provenance"`
		}{identity, v.Name, v.Kind, v.Path, v.Provenance}, struct {
			Name       string               `json:"name"`
			Kind       SchemaKind           `json:"kind"`
			Path       string               `json:"path,omitempty"`
			Provenance projectionProvenance `json:"provenance"`
		}{v.Name, v.Kind, v.Path, projectionV1(v.Provenance)})
	}
	for _, v := range manifest.Discoverers {
		identity := ContributionIdentity{v.Provenance.Project, ContributionKindDiscoverer, string(v.Kind), v.Name}
		add(identity, struct {
			Identity   ContributionIdentity `json:"identity"`
			Name       string               `json:"name"`
			Kind       DiscovererKind       `json:"kind"`
			Provenance Provenance           `json:"provenance"`
		}{identity, v.Name, v.Kind, v.Provenance}, struct {
			Name       string               `json:"name"`
			Kind       DiscovererKind       `json:"kind"`
			Provenance projectionProvenance `json:"provenance"`
		}{v.Name, v.Kind, projectionV1(v.Provenance)})
	}
	v1Migration := false
	if ref.Kind == ContributionKindMigration {
		for _, v := range manifest.Migrations {
			if v.Provenance.Project == ref.OwnerProject && v.Name == ref.Key {
				v1Migration = true
			}
		}
	}
	for _, v := range manifest.InfraRequirements {
		identity := ContributionIdentity{v.Provenance.Project, ContributionKindInfra, string(v.Kind), v.Name}
		add(identity, struct {
			Identity   ContributionIdentity `json:"identity"`
			Name       string               `json:"name"`
			Kind       InfraKind            `json:"kind"`
			Provenance Provenance           `json:"provenance"`
		}{identity, v.Name, v.Kind, v.Provenance}, struct {
			Name       string               `json:"name"`
			Kind       InfraKind            `json:"kind"`
			Provenance projectionProvenance `json:"provenance"`
		}{v.Name, v.Kind, projectionV1(v.Provenance)})
	}
	for _, v := range manifest.HealthContributors {
		identity := ContributionIdentity{v.Provenance.Project, ContributionKindHealth, string(v.Probe), v.Name}
		add(identity, struct {
			Identity   ContributionIdentity `json:"identity"`
			Name       string               `json:"name"`
			Probe      ProbeKind            `json:"probe"`
			Provenance Provenance           `json:"provenance"`
		}{identity, v.Name, v.Probe, v.Provenance}, struct {
			Name       string               `json:"name"`
			Probe      ProbeKind            `json:"probe"`
			Provenance projectionProvenance `json:"provenance"`
		}{v.Name, v.Probe, projectionV1(v.Provenance)})
	}
	for _, v := range manifest.LifecycleHooks {
		identity := ContributionIdentity{v.Provenance.Project, ContributionKindLifecycle, string(v.Phase), v.Name}
		add(identity, struct {
			Identity   ContributionIdentity `json:"identity"`
			Name       string               `json:"name"`
			Phase      LifecyclePhase       `json:"phase"`
			Provenance Provenance           `json:"provenance"`
		}{identity, v.Name, v.Phase, v.Provenance}, struct {
			Name       string               `json:"name"`
			Phase      LifecyclePhase       `json:"phase"`
			Provenance projectionProvenance `json:"provenance"`
		}{v.Name, v.Phase, projectionV1(v.Provenance)})
	}
	for _, v := range manifest.PackageVersions {
		identity := ContributionIdentity{v.Provenance.Project, ContributionKindPackage, "", v.Package}
		add(identity, struct {
			Identity   ContributionIdentity `json:"identity"`
			Package    string               `json:"package"`
			Version    string               `json:"version"`
			Provenance Provenance           `json:"provenance"`
		}{identity, v.Package, v.Version, v.Provenance}, struct {
			Package    string               `json:"package"`
			Version    string               `json:"version"`
			Provenance projectionProvenance `json:"provenance"`
		}{v.Package, v.Version, projectionV1(v.Provenance)})
	}
	for _, v := range manifest.RequiredCapabilities {
		identity := ContributionIdentity{v.Provenance.Project, ContributionKindRequiredCapability, "", v.Name}
		add(identity, struct {
			Identity   ContributionIdentity `json:"identity"`
			Name       string               `json:"name"`
			Requires   []CapabilityKind     `json:"requires"`
			Provenance Provenance           `json:"provenance"`
		}{identity, v.Name, v.Requires, v.Provenance}, struct {
			Name       string               `json:"name"`
			Requires   []CapabilityKind     `json:"requires"`
			Provenance projectionProvenance `json:"provenance"`
		}{v.Name, v.Requires, projectionV1(v.Provenance)})
	}
	return out, v1Migration
}
