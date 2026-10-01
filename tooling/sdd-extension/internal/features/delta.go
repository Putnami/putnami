package features

import (
	"encoding/json"
	"sort"

	featureproto "go.putnami.dev/protocol/features"
)

// Delta is the deliberately provisional comparison of two independently
// evaluated immutable snapshots.
type Delta struct {
	Compatibility     string                   `json:"compatibility"`
	Base              Revision                 `json:"base"`
	Head              Revision                 `json:"head"`
	Added             []FeatureAssessment      `json:"added"`
	Removed           []FeatureAssessment      `json:"removed"`
	Promoted          []MaturityDelta          `json:"promoted"`
	Regressed         []MaturityDelta          `json:"regressed"`
	Stale             []RequirementStateDelta  `json:"stale"`
	Contradicted      []RequirementStateDelta  `json:"contradicted"`
	NewlyUnclassified []ContributionAssessment `json:"newlyUnclassified"`
}

// MaturityDelta records a change in derived current maturity for one stable
// feature identity.
type MaturityDelta struct {
	FeatureID string                     `json:"featureId"`
	Name      string                     `json:"name"`
	From      featureproto.MaturityStage `json:"from"`
	To        featureproto.MaturityStage `json:"to"`
}

// RequirementStateDelta records one requirement that newly entered a stale or
// contradicted state at head.
type RequirementStateDelta struct {
	FeatureID     string                     `json:"featureId"`
	RequirementID string                     `json:"requirementId"`
	Stage         featureproto.MaturityStage `json:"stage"`
	From          VerificationState          `json:"from,omitempty"`
	To            VerificationState          `json:"to"`
}

// CompareSnapshots reports only the accepted delta categories. Both inputs are
// canonicalized as copies, so caller order and later mutation cannot influence
// the result.
func CompareSnapshots(base, head *Snapshot) *Delta {
	delta := &Delta{
		Compatibility:     SnapshotCompatibility,
		Added:             []FeatureAssessment{},
		Removed:           []FeatureAssessment{},
		Promoted:          []MaturityDelta{},
		Regressed:         []MaturityDelta{},
		Stale:             []RequirementStateDelta{},
		Contradicted:      []RequirementStateDelta{},
		NewlyUnclassified: []ContributionAssessment{},
	}
	if base == nil || head == nil {
		return delta
	}
	base = canonicalSnapshot(base)
	head = canonicalSnapshot(head)
	delta.Base = base.Revision
	delta.Head = head.Revision

	baseFeatures := make(map[string]FeatureAssessment, len(base.Features))
	headFeatures := make(map[string]FeatureAssessment, len(head.Features))
	for _, feature := range base.Features {
		baseFeatures[feature.ID] = feature
	}
	for _, feature := range head.Features {
		headFeatures[feature.ID] = feature
		previous, existed := baseFeatures[feature.ID]
		if !existed {
			delta.Added = append(delta.Added, feature)
			continue
		}
		fromRank, fromOK := featureproto.StageRank(previous.Current)
		toRank, toOK := featureproto.StageRank(feature.Current)
		if !fromOK || !toOK || fromRank == toRank {
			continue
		}
		change := MaturityDelta{FeatureID: feature.ID, Name: feature.Name, From: previous.Current, To: feature.Current}
		if toRank > fromRank {
			delta.Promoted = append(delta.Promoted, change)
		} else {
			delta.Regressed = append(delta.Regressed, change)
		}
	}
	for _, feature := range base.Features {
		if _, exists := headFeatures[feature.ID]; !exists {
			delta.Removed = append(delta.Removed, feature)
		}
	}

	baseRequirements := make(map[string]RequirementAssessment)
	for _, feature := range base.Features {
		for _, requirement := range feature.Requirements {
			baseRequirements[requirementDeltaKey(feature.ID, requirement.ID)] = requirement
		}
	}
	for _, feature := range head.Features {
		for _, requirement := range feature.Requirements {
			if requirement.State != VerificationStale && requirement.State != VerificationContradicted {
				continue
			}
			previous, existed := baseRequirements[requirementDeltaKey(feature.ID, requirement.ID)]
			if existed && previous.State == requirement.State {
				continue
			}
			change := RequirementStateDelta{
				FeatureID: feature.ID, RequirementID: requirement.ID, Stage: requirement.Stage, To: requirement.State,
			}
			if existed {
				change.From = previous.State
			}
			if requirement.State == VerificationStale {
				delta.Stale = append(delta.Stale, change)
			} else {
				delta.Contradicted = append(delta.Contradicted, change)
			}
		}
	}

	baseUnclassified := make(map[string]bool, len(base.Unclassified))
	for _, contribution := range base.Unclassified {
		baseUnclassified[contributionKey(contribution.Identity)] = true
	}
	seenHead := make(map[string]bool, len(head.Unclassified))
	for _, contribution := range head.Unclassified {
		key := contributionKey(contribution.Identity)
		if baseUnclassified[key] || seenHead[key] {
			continue
		}
		seenHead[key] = true
		delta.NewlyUnclassified = append(delta.NewlyUnclassified, canonicalContribution(contribution))
	}
	return canonicalDelta(delta)
}

// MarshalDelta emits canonical two-space-indented JSON with one trailing
// newline, mirroring MarshalSnapshot's deterministic test surface.
func MarshalDelta(delta *Delta) ([]byte, error) {
	data, err := json.MarshalIndent(canonicalDelta(delta), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func canonicalDelta(input *Delta) *Delta {
	if input == nil {
		return nil
	}
	output := *input
	output.Added = append([]FeatureAssessment{}, input.Added...)
	output.Removed = append([]FeatureAssessment{}, input.Removed...)
	output.Promoted = append([]MaturityDelta{}, input.Promoted...)
	output.Regressed = append([]MaturityDelta{}, input.Regressed...)
	output.Stale = append([]RequirementStateDelta{}, input.Stale...)
	output.Contradicted = append([]RequirementStateDelta{}, input.Contradicted...)
	output.NewlyUnclassified = append([]ContributionAssessment{}, input.NewlyUnclassified...)
	for index := range output.NewlyUnclassified {
		output.NewlyUnclassified[index] = canonicalContribution(output.NewlyUnclassified[index])
	}
	sort.Slice(output.Added, func(i, j int) bool { return output.Added[i].ID < output.Added[j].ID })
	sort.Slice(output.Removed, func(i, j int) bool { return output.Removed[i].ID < output.Removed[j].ID })
	sort.Slice(output.Promoted, func(i, j int) bool { return output.Promoted[i].FeatureID < output.Promoted[j].FeatureID })
	sort.Slice(output.Regressed, func(i, j int) bool { return output.Regressed[i].FeatureID < output.Regressed[j].FeatureID })
	sort.Slice(output.Stale, func(i, j int) bool { return compareRequirementDelta(output.Stale[i], output.Stale[j]) < 0 })
	sort.Slice(output.Contradicted, func(i, j int) bool {
		return compareRequirementDelta(output.Contradicted[i], output.Contradicted[j]) < 0
	})
	sort.Slice(output.NewlyUnclassified, func(i, j int) bool {
		return compareContribution(output.NewlyUnclassified[i], output.NewlyUnclassified[j]) < 0
	})
	return &output
}

func requirementDeltaKey(featureID, requirementID string) string {
	return featureID + "\x00" + requirementID
}

func compareRequirementDelta(left, right RequirementStateDelta) int {
	if left.FeatureID != right.FeatureID {
		if left.FeatureID < right.FeatureID {
			return -1
		}
		return 1
	}
	leftRank, _ := featureproto.StageRank(left.Stage)
	rightRank, _ := featureproto.StageRank(right.Stage)
	if leftRank != rightRank {
		if leftRank < rightRank {
			return -1
		}
		return 1
	}
	if left.RequirementID < right.RequirementID {
		return -1
	}
	if left.RequirementID > right.RequirementID {
		return 1
	}
	return 0
}
