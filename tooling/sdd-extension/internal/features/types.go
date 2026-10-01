// Package features discovers repository-authored feature intent and evidence
// and derives a deterministic, provisional local assessment. It deliberately
// owns no CLI surface; commands adapt this internal result in a later layer.
package features

import (
	"encoding/json"
	"sort"

	capabilityproto "go.putnami.dev/protocol/capabilities"
	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
)

const (
	// SnapshotCompatibility marks the snapshot payload as unfrozen for every
	// consumer until a separate compatibility decision freezes it.
	SnapshotCompatibility = "provisional"

	// RevisionKindWorktree identifies an assessment of the caller's current
	// worktree. RevisionKindGit is reserved for the immutable reader added by
	// the revision-diff slice.
	RevisionKindWorktree = "worktree"
	RevisionKindGit      = "git"
)

// Revision identifies the repository state selected for evaluation. It never
// doubles as an evidence-freshness oracle; source bindings do that job.
type Revision struct {
	Kind   string `json:"kind"`
	Head   string `json:"head,omitempty"`
	Commit string `json:"commit,omitempty"`
}

// Result preserves diagnostics when structural errors prevent publication.
// Assessment warnings remain alongside a successfully published Snapshot.
type Result struct {
	Snapshot    *Snapshot
	Diagnostics []diag.Diagnostic
	// Scope explains what the evaluation actually covered. It is present on
	// every result, scoped or not, so a caller never has to infer the projection
	// from the counts.
	Scope ScopeSummary
}

// ScopeSummary is the deterministic selection metadata a scoped evaluation
// reports. Every field is sorted, so the same selection over the same tree
// produces the same bytes.
type ScopeSummary struct {
	// Scoped distinguishes a narrowed run from the whole-workspace default.
	Scoped bool `json:"scoped"`
	// SelectedRoots are the seed discovery roots, "" being the workspace root.
	SelectedRoots []string `json:"selectedRoots,omitempty"`
	// SelectedFeatures are the authored identities the seed roots declare.
	SelectedFeatures []string `json:"selectedFeatures,omitempty"`
	// ExternalRecords are the workspace paths outside the seed roots this run
	// followed because a selected feature's correctness depends on them.
	ExternalRecords []string `json:"externalRecords,omitempty"`
}

// Snapshot is the byte-stable internal projection consumed by the later CLI
// surface. Its compatibility marker is intentionally provisional.
type Snapshot struct {
	Compatibility  string                   `json:"compatibility"`
	Revision       Revision                 `json:"revision"`
	SourceBindings []SourceBinding          `json:"sourceBindings"`
	Features       []FeatureAssessment      `json:"features"`
	Unclassified   []ContributionAssessment `json:"unclassified"`
}

// SourceBinding records one exact semantic root that was evaluated. Absolute
// filesystem locations never enter the snapshot.
type SourceBinding struct {
	Root         capabilityproto.LocationRoot `json:"root"`
	OwnerProject string                       `json:"ownerProject,omitempty"`
	Package      string                       `json:"package,omitempty"`
	Version      string                       `json:"version,omitempty"`
	Binding      string                       `json:"binding,omitempty"`
	Unavailable  bool                         `json:"unavailable,omitempty"`
}

// FeatureAssessment combines authored intent with its derived current state.
type FeatureAssessment struct {
	ID           string                     `json:"id"`
	Type         featureproto.FeatureType   `json:"type"`
	Name         string                     `json:"name"`
	Outcome      string                     `json:"outcome"`
	Owner        string                     `json:"owner"`
	Source       string                     `json:"source"`
	Target       featureproto.MaturityStage `json:"target"`
	Current      featureproto.MaturityStage `json:"current"`
	Relations    []featureproto.Relation    `json:"relations"`
	Requirements []RequirementAssessment    `json:"requirements"`
}

// VerificationState is the closed derived requirement-state vocabulary.
type VerificationState string

const (
	VerificationVerified     VerificationState = "verified"
	VerificationMissing      VerificationState = "missing"
	VerificationStale        VerificationState = "stale"
	VerificationContradicted VerificationState = "contradicted"
)

// RequirementAssessment retains every requirement, including requirements
// above the authored target. Claimed says whether it may raise current maturity.
type RequirementAssessment struct {
	ID            string                      `json:"id"`
	Stage         featureproto.MaturityStage  `json:"stage"`
	EvidenceKinds []featureproto.EvidenceKind `json:"evidenceKinds"`
	Claimed       bool                        `json:"claimed"`
	State         VerificationState           `json:"state"`
	Evidence      []EvidenceAssessment        `json:"evidence"`
}

// EvidenceState says whether a structurally valid assertion is current for
// this evaluation. StaleReasons contains bounded stable reason codes.
type EvidenceState string

const (
	EvidenceActive EvidenceState = "active"
	EvidenceStale  EvidenceState = "stale"
)

const (
	StaleSourceBindingUnavailable = "source-binding-unavailable"
	StaleSourceBindingMismatch    = "source-binding-mismatch"
	StaleContributionBinding      = "contribution-binding-mismatch"
	StaleArtifactUnavailable      = "artifact-unavailable"
	StaleArtifactDigest           = "artifact-digest-mismatch"
)

// EvidenceAssessment is safe inspection metadata: typed identities, relative
// paths, digests, and bounded protocol metadata only. It never contains source
// bytes, configuration values, or absolute paths.
type EvidenceAssessment struct {
	ID                         string                          `json:"id"`
	Document                   string                          `json:"document"`
	Outcome                    featureproto.EvidenceOutcome    `json:"outcome"`
	Issuer                     featureproto.Issuer             `json:"issuer"`
	Source                     featureproto.SourceSelector     `json:"source"`
	Subject                    featureproto.EvidenceSubject    `json:"subject"`
	Provenance                 featureproto.EvidenceProvenance `json:"provenance"`
	Persistent                 bool                            `json:"persistent,omitempty"`
	ObservedAt                 string                          `json:"observedAt,omitempty"`
	ObservedRepositoryRevision string                          `json:"observedRepositoryRevision,omitempty"`
	State                      EvidenceState                   `json:"state"`
	StaleReasons               []string                        `json:"staleReasons,omitempty"`
	Contribution               *ContributionAssessment         `json:"contribution,omitempty"`
}

// ContributionAssessment is one coalesced owner-scoped technical fact. V1
// migrations remain visible with Referenceable=false because their wire shape
// has no migration kind and therefore no complete identity.
type ContributionAssessment struct {
	Identity             capabilityproto.ContributionIdentity `json:"identity"`
	ProtocolVersion      int                                  `json:"protocolVersion"`
	Referenceable        bool                                 `json:"referenceable"`
	Containers           []string                             `json:"containers"`
	Provenance           ContributionProvenance               `json:"provenance"`
	CurrentSourceBinding string                               `json:"currentSourceBinding,omitempty"`
	SourceState          string                               `json:"sourceState,omitempty"`
}

// ContributionProvenance normalizes only bounded v1/v2 provenance fields. V1
// EvidencePath remains explicitly labeled and is never treated as a precise
// declaration.
type ContributionProvenance struct {
	Project        string                               `json:"project"`
	Package        string                               `json:"package,omitempty"`
	Version        string                               `json:"version,omitempty"`
	SourceKind     capabilityproto.SourceKind           `json:"sourceKind"`
	Declaration    *capabilityproto.DeclarationLocation `json:"declaration,omitempty"`
	Artifacts      []capabilityproto.ArtifactLocation   `json:"artifacts,omitempty"`
	V1EvidencePath string                               `json:"v1EvidencePath,omitempty"`
}

// MarshalSnapshot emits canonical two-space-indented JSON and one trailing
// newline. It canonicalizes a copy so callers cannot make output depend on map,
// filesystem, cache, or concurrent completion order.
func MarshalSnapshot(snapshot *Snapshot) ([]byte, error) {
	canonical := canonicalSnapshot(snapshot)
	data, err := json.MarshalIndent(canonical, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func canonicalSnapshot(input *Snapshot) *Snapshot {
	if input == nil {
		return nil
	}
	out := *input
	out.SourceBindings = append([]SourceBinding{}, input.SourceBindings...)
	sort.Slice(out.SourceBindings, func(i, j int) bool {
		return compareSourceBinding(out.SourceBindings[i], out.SourceBindings[j]) < 0
	})
	out.Features = append([]FeatureAssessment{}, input.Features...)
	for i := range out.Features {
		out.Features[i].Relations = append([]featureproto.Relation{}, out.Features[i].Relations...)
		sort.Slice(out.Features[i].Relations, func(a, b int) bool {
			left, right := out.Features[i].Relations[a], out.Features[i].Relations[b]
			if left.Kind != right.Kind {
				return left.Kind < right.Kind
			}
			return left.Target < right.Target
		})
		out.Features[i].Requirements = append([]RequirementAssessment{}, out.Features[i].Requirements...)
		for j := range out.Features[i].Requirements {
			requirement := &out.Features[i].Requirements[j]
			requirement.EvidenceKinds = append([]featureproto.EvidenceKind(nil), requirement.EvidenceKinds...)
			sort.Slice(requirement.EvidenceKinds, func(a, b int) bool { return requirement.EvidenceKinds[a] < requirement.EvidenceKinds[b] })
			requirement.Evidence = append([]EvidenceAssessment{}, requirement.Evidence...)
			for k := range requirement.Evidence {
				requirement.Evidence[k].StaleReasons = append([]string(nil), requirement.Evidence[k].StaleReasons...)
				sort.Strings(requirement.Evidence[k].StaleReasons)
				if contribution := requirement.Evidence[k].Contribution; contribution != nil {
					copy := canonicalContribution(*contribution)
					requirement.Evidence[k].Contribution = &copy
				}
			}
			sort.Slice(requirement.Evidence, func(a, b int) bool { return requirement.Evidence[a].ID < requirement.Evidence[b].ID })
		}
		sort.Slice(out.Features[i].Requirements, func(a, b int) bool {
			left, _ := featureproto.StageRank(out.Features[i].Requirements[a].Stage)
			right, _ := featureproto.StageRank(out.Features[i].Requirements[b].Stage)
			if left != right {
				return left < right
			}
			return out.Features[i].Requirements[a].ID < out.Features[i].Requirements[b].ID
		})
	}
	sort.Slice(out.Features, func(i, j int) bool { return out.Features[i].ID < out.Features[j].ID })
	out.Unclassified = append([]ContributionAssessment{}, input.Unclassified...)
	for i := range out.Unclassified {
		out.Unclassified[i] = canonicalContribution(out.Unclassified[i])
	}
	sort.Slice(out.Unclassified, func(i, j int) bool { return compareContribution(out.Unclassified[i], out.Unclassified[j]) < 0 })
	return &out
}

func canonicalContribution(input ContributionAssessment) ContributionAssessment {
	out := input
	out.Containers = append([]string(nil), input.Containers...)
	sort.Strings(out.Containers)
	out.Provenance.Artifacts = append([]capabilityproto.ArtifactLocation(nil), input.Provenance.Artifacts...)
	sort.Slice(out.Provenance.Artifacts, func(i, j int) bool {
		left, right := out.Provenance.Artifacts[i], out.Provenance.Artifacts[j]
		if left.Root != right.Root {
			return left.Root < right.Root
		}
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		return left.Digest < right.Digest
	})
	if input.Provenance.Declaration != nil {
		declaration := *input.Provenance.Declaration
		out.Provenance.Declaration = &declaration
	}
	return out
}

func compareSourceBinding(left, right SourceBinding) int {
	leftKey := string(left.Root) + "\x00" + left.OwnerProject + "\x00" + left.Package + "\x00" + left.Version
	rightKey := string(right.Root) + "\x00" + right.OwnerProject + "\x00" + right.Package + "\x00" + right.Version
	if leftKey < rightKey {
		return -1
	}
	if leftKey > rightKey {
		return 1
	}
	return 0
}

func compareContribution(left, right ContributionAssessment) int {
	if compared := capabilityproto.CompareContributionIdentity(left.Identity, right.Identity); compared != 0 {
		return compared
	}
	leftContainer, rightContainer := "", ""
	if len(left.Containers) > 0 {
		leftContainer = left.Containers[0]
	}
	if len(right.Containers) > 0 {
		rightContainer = right.Containers[0]
	}
	if leftContainer < rightContainer {
		return -1
	}
	if leftContainer > rightContainer {
		return 1
	}
	return 0
}
