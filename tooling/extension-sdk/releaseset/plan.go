// Package releaseset owns the provider-neutral extension half of release-set
// publication: the immutable plan carried in a job context, the selection
// fingerprint every member records, and the bounded client for the optional
// distribution/release-set provider.
package releaseset

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	distribution "go.putnami.dev/protocol/distribution"
	job "go.putnami.dev/protocol/job"
)

// ContextParamName is the one structured job-context parameter through which
// the orchestrator gives language extensions a sparse release plan. It is not a
// user flag and must never be synthesized by a language-specific command.
const ContextParamName = "releaseSetPlan"

const placeholderArtifactDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

// Plan is the immutable answer produced before any package task is planned.
//
// Channels lists every channel this publication advances, in the order the
// caller named them; the FIRST is the baseline when it has a head. Heads
// carries the head resolved for each of them — one entry per channel, a nil
// entry meaning that channel has no head yet — so the release can
// compare-and-swap every channel from its own expectation without resolving
// anything a second time. BaselineChannel names one further channel, read and
// never released, whose head is carried in Heads beside them.
//
// Members is always the next full snapshot: selected members carry their
// candidate version, the provenance of the tree being published, and an empty
// ArtifactDigest until publication; unchanged members inherit every field of
// the baseline head's record.
type Plan struct {
	ProtocolVersion int                                  `json:"protocolVersion"`
	Namespace       string                               `json:"namespace"`
	Channels        []string                             `json:"channels"`
	Heads           map[string]*distribution.ChannelHead `json:"heads"`
	Members         []PlannedMember                      `json:"members"`
	// BaselineChannel is the channel a plan over an empty first channel
	// measured against. Its head is carried in Heads and it is never
	// released. Empty when the first channel's own head was the baseline,
	// which is every plan that names none.
	BaselineChannel string `json:"baselineChannel,omitempty"`
}

// PlannedMember is one full-snapshot member in a Plan. ProjectID is an
// orchestrator identity used only to attach the plan to the correct task;
// (ecosystem, coordinate) remains the distribution authority, and one project
// may contribute several members. SourceRevision and SelectionFingerprint are
// the provenance every member records, and SourceTree the optional git tree a
// selected member was built from; Platforms is filled by publication for
// members published as a multi-platform set.
//
// Project and Kind are the protocol attribution the published set carries, so
// a consumer holding only that set can answer "which member is this project's
// image, which is its config". They are the coordinator's statement about the
// member, republished or not: an opted-in plan stamps every member it declares
// from the workspace's declarations, and only a plan built without the opt-in
// carries an unchanged member's head values through unchanged.
type PlannedMember struct {
	Ecosystem            distribution.Ecosystem              `json:"ecosystem"`
	Coordinate           string                              `json:"coordinate"`
	Version              string                              `json:"version"`
	ArtifactDigest       string                              `json:"artifactDigest,omitempty"`
	Dependencies         []distribution.ReleaseSetDependency `json:"dependencies"`
	SourceRevision       string                              `json:"sourceRevision"`
	SelectionFingerprint string                              `json:"selectionFingerprint"`
	Platforms            map[string]string                   `json:"platforms,omitempty"`
	Project              string                              `json:"project,omitempty"`
	Kind                 distribution.MemberKind             `json:"kind,omitempty"`
	SourceTree           string                              `json:"sourceTree,omitempty"`
	Selected             bool                                `json:"selected"`
	ProjectID            string                              `json:"projectId,omitempty"`
}

// MemberKey is the unambiguous identity of a release-set member.
func MemberKey(ecosystem distribution.Ecosystem, coordinate string) string {
	return string(ecosystem) + "\x00" + coordinate
}

// Baseline is the head impact is measured against: the head of the first
// channel named by the publication, or, when that channel has no head, the
// head of the named BaselineChannel. It is nil when neither has one.
//
// The first channel's own head comes first so that every publication that
// already has one behaves exactly as it did before a baseline could be named:
// the named channel is a fallback for the first publish into an empty channel,
// never a redirection of an established one.
func (p *Plan) Baseline() *distribution.ChannelHead {
	if p == nil || len(p.Channels) == 0 {
		return nil
	}
	if head := p.Heads[p.Channels[0]]; head != nil {
		return head
	}
	if p.BaselineChannel == "" {
		return nil
	}
	return p.Heads[p.BaselineChannel]
}

// BaselineChannelName returns the channel Baseline() came from, "" when the
// baseline is empty. The session report names it.
func (p *Plan) BaselineChannelName() string {
	if p == nil || len(p.Channels) == 0 {
		return ""
	}
	if p.Heads[p.Channels[0]] != nil {
		return p.Channels[0]
	}
	if p.BaselineChannel != "" && p.Heads[p.BaselineChannel] != nil {
		return p.BaselineChannel
	}
	return ""
}

// baselineSet returns the baseline's full snapshot, or nil.
func (p *Plan) baselineSet() *distribution.ReleaseSet {
	baseline := p.Baseline()
	if baseline == nil {
		return nil
	}
	return baseline.ReleaseSet
}

// SelectedMembers returns copies of selected members in canonical order.
func (p *Plan) SelectedMembers() []PlannedMember {
	if p == nil {
		return nil
	}
	selected := make([]PlannedMember, 0)
	for _, member := range p.Members {
		if member.Selected {
			selected = append(selected, cloneMember(member))
		}
	}
	return selected
}

// SelectsEveryMember reports whether the plan is a full publication of its
// snapshot: every member is selected and none inherits from a head. It is
// false for an empty plan, which selects nothing.
func (p *Plan) SelectsEveryMember() bool {
	if p == nil || len(p.Members) == 0 {
		return false
	}
	for _, member := range p.Members {
		if !member.Selected {
			return false
		}
	}
	return true
}

// Member resolves one member by its protocol identity and returns a copy.
func (p *Plan) Member(ecosystem distribution.Ecosystem, coordinate string) (PlannedMember, bool) {
	if p == nil {
		return PlannedMember{}, false
	}
	key := MemberKey(ecosystem, coordinate)
	for _, member := range p.Members {
		if MemberKey(member.Ecosystem, member.Coordinate) == key {
			return cloneMember(member), true
		}
	}
	return PlannedMember{}, false
}

// FromContext reads and validates the SDK-owned plan nested in a job
// context. Absence is a supported full-publication answer.
func FromContext(ctx *job.Context) (*Plan, error) {
	if ctx == nil {
		return nil, nil
	}
	return ParseParams(ctx.Params)
}

// ParseParams is the lower-level form used by SDK adapters whose context type
// embeds protocol/job.Context directly. Unknown fields are ignored for additive
// orchestrator changes; ValidatePlan enforces the supported protocol version.
func ParseParams(params job.Params) (*Plan, error) {
	raw, ok := params[ContextParamName]
	if !ok {
		return nil, nil
	}
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, fmt.Errorf("%s is present but empty", ContextParamName)
	}
	if len(raw) > distribution.MaxJSONBytes {
		return nil, fmt.Errorf("%s exceeds the %d-byte protocol limit", ContextParamName, distribution.MaxJSONBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var plan Plan
	if err := decoder.Decode(&plan); err != nil {
		return nil, fmt.Errorf("decode %s: %w", ContextParamName, err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("decode %s: %w", ContextParamName, err)
	}
	if err := ValidatePlan(&plan); err != nil {
		return nil, err
	}
	return ClonePlan(&plan), nil
}

// ValidatePlan checks the channel binding, the resolved heads, full membership,
// exact dependency closure, the selected-member digest hole, member provenance,
// canonical ordering, and namespace binding.
//
// A plan over an existing baseline carries it as Heads[Channels[0]], or as
// Heads[BaselineChannel] when the first channel is still empty; every
// unselected member must inherit that head's artifact record exactly — its
// attribution is the plan's own and may differ from the head's — and a plan
// that selects nothing confirms the baseline's artifacts unchanged. A plan with
// no baseline head at all selects every member. Membership is free to add or drop coordinates
// relative to the baseline: the plan redefines the next snapshot.
//
// BaselineChannel is read and never released, so it must not also be one of the
// channels the plan advances: an advanced channel is already its own baseline,
// and naming it twice would state two different things about one head.
//
// A project is deliberately allowed to own several selected members: the member
// key is (ecosystem, coordinate) and the project is provenance, so one project
// publishing an npm package and an OCI image contributes two members of one
// publication.
func ValidatePlan(plan *Plan) error {
	if plan == nil {
		return fmt.Errorf("release-set plan is nil")
	}
	if plan.ProtocolVersion != distribution.ProtocolVersion {
		return fmt.Errorf("release-set plan protocolVersion %d is unsupported; the SDK supports %d", plan.ProtocolVersion, distribution.ProtocolVersion)
	}
	// Guarded on the empty string, which names no baseline: a malformed plan
	// carrying "" among its channels is a channel fault, and reporting it
	// against the baseline member would send a reader to the one field that is
	// correct.
	if plan.BaselineChannel != "" && slices.Contains(plan.Channels, plan.BaselineChannel) {
		return fmt.Errorf("release-set plan baseline channel %q is also advanced by the plan; a baseline is read and never released", plan.BaselineChannel)
	}
	if err := validatePlanHeads(plan); err != nil {
		return err
	}
	baseSet := plan.baselineSet()
	baseMembers := make(map[string]distribution.ReleaseSetMember)
	if baseSet != nil {
		if baseSet.Namespace != plan.Namespace {
			return fmt.Errorf("release-set plan baseline namespace %q does not match %q", baseSet.Namespace, plan.Namespace)
		}
		baseMembers = make(map[string]distribution.ReleaseSetMember, len(baseSet.Members))
		for _, member := range baseSet.Members {
			baseMembers[MemberKey(member.Ecosystem, member.Coordinate)] = member
		}
	}
	projection := distribution.ReleaseSet{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       plan.Namespace,
		Members:         make([]distribution.ReleaseSetMember, 0, len(plan.Members)),
	}
	previous := ""
	selected := 0
	for i, member := range plan.Members {
		key := MemberKey(member.Ecosystem, member.Coordinate)
		if previous != "" && key <= previous {
			return fmt.Errorf("release-set plan members are not unique canonical order at index %d", i)
		}
		previous = key
		projected := distribution.ReleaseSetMember{
			Ecosystem: member.Ecosystem, Coordinate: member.Coordinate,
			Version: member.Version, ArtifactDigest: member.ArtifactDigest,
			Dependencies:         append([]distribution.ReleaseSetDependency(nil), member.Dependencies...),
			SourceRevision:       member.SourceRevision,
			SelectionFingerprint: member.SelectionFingerprint,
			Platforms:            maps.Clone(member.Platforms),
			Project:              member.Project,
			Kind:                 member.Kind,
			SourceTree:           member.SourceTree,
		}
		if member.Selected {
			selected++
			if member.ProjectID == "" {
				return fmt.Errorf("selected release-set plan member %q has no project id", printableKey(key))
			}
			if member.ArtifactDigest != "" {
				return fmt.Errorf("selected release-set plan member %q already carries an artifact digest", printableKey(key))
			}
			if len(member.Platforms) != 0 {
				return fmt.Errorf("selected release-set plan member %q already carries platform digests", printableKey(key))
			}
			projected.ArtifactDigest = placeholderArtifactDigest
		} else {
			if baseSet == nil {
				return fmt.Errorf("release-set plan member %q is not selected but the baseline channel has no head to inherit from", printableKey(key))
			}
			baseMember, exists := baseMembers[key]
			if !exists {
				return fmt.Errorf("unchanged release-set plan member %q is absent from the baseline head", printableKey(key))
			}
			if member.ArtifactDigest == "" {
				return fmt.Errorf("unchanged release-set plan member %q did not inherit its artifact digest", printableKey(key))
			}
			if !sameArtifactRecord(projected, baseMember) {
				return fmt.Errorf("unchanged release-set plan member %q does not exactly inherit its baseline record", printableKey(key))
			}
		}
		projection.Members = append(projection.Members, projected)
	}
	if selected == 0 && baseSet == nil {
		return fmt.Errorf("release-set plan selects no members and has no baseline head to confirm")
	}
	if len(projection.Members) == 0 {
		return fmt.Errorf("release-set plan has no members")
	}
	if diagnostics := distribution.ValidateReleaseSet(&projection); hasDiagnosticErrors(diagnostics) {
		return fmt.Errorf("release-set plan is not a closed full snapshot: %s", firstDiagnostic(diagnostics))
	}
	return nil
}

// validatePlanHeads replays the resolve exchange the plan was built from: the
// channel list is a valid selector, every named channel has an entry — null for
// an empty channel — no unrequested channel is carried, and every present head
// recomputes its own ref from its snapshot.
//
// The selector is the channels the plan advances PLUS the baseline channel it
// only reads, because that is exactly the one resolve the publication made: the
// baseline's head has to be answered by the same read as the others, or the two
// heads would come from two instants.
func validatePlanHeads(plan *Plan) error {
	selector := &distribution.ResolveRequest{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       plan.Namespace,
		Channels:        planResolvedChannels(plan),
	}
	response := &distribution.ResolveResponse{
		ProtocolVersion: distribution.ProtocolVersion,
		Heads:           plan.Heads,
	}
	if plan.Heads == nil {
		// A nil map cannot say "this channel has no head" — it says nothing at
		// all — and the exchange validator would report the ambiguity as a
		// missing selector, which names the wrong defect.
		return fmt.Errorf("release-set plan carries no resolved heads; every listed channel needs an entry")
	}
	if diagnostics := distribution.ValidateResolveExchange(selector, response); hasDiagnosticErrors(diagnostics) {
		return fmt.Errorf("release-set plan channels are invalid: %s", firstDiagnostic(diagnostics))
	}
	return nil
}

// planResolvedChannels is the channel list the plan's one resolve named: the
// advanced channels in order, then the baseline when it is not already among
// them. ValidatePlan refuses a baseline that IS among them, so the guard here
// only keeps this helper independently correct.
func planResolvedChannels(plan *Plan) []string {
	channels := append([]string(nil), plan.Channels...)
	if plan.BaselineChannel != "" && !slices.Contains(channels, plan.BaselineChannel) {
		channels = append(channels, plan.BaselineChannel)
	}
	return channels
}

// sameArtifactRecord compares the artifact record of two members: identity,
// version, digest, dependencies, provenance (source tree included) and platform
// digests. An unchanged member must inherit ALL of them, because they belong to
// the publication that produced the artifact. Attribution is deliberately
// outside the comparison: the project that declares a member and the kind of
// artifact it is are the coordinator's statement about the member, made from
// the workspace it plans, and a head published before the repository opted in
// stays unattributed for exactly the members that never change unless the plan
// may state it for them. Both values are still validated on every member by
// ValidateReleaseSet.
func sameArtifactRecord(first, second distribution.ReleaseSetMember) bool {
	return first.Ecosystem == second.Ecosystem && first.Coordinate == second.Coordinate &&
		first.Version == second.Version && first.ArtifactDigest == second.ArtifactDigest &&
		slices.Equal(first.Dependencies, second.Dependencies) &&
		first.SourceRevision == second.SourceRevision && first.SourceTree == second.SourceTree &&
		first.SelectionFingerprint == second.SelectionFingerprint &&
		maps.Equal(first.Platforms, second.Platforms)
}

// ClonePlan makes a deep copy, preserving the immutability boundary between
// the orchestrator, cache-key projection, and each language extension.
func ClonePlan(plan *Plan) *Plan {
	if plan == nil {
		return nil
	}
	clone := *plan
	clone.Channels = append([]string(nil), plan.Channels...)
	if plan.Heads != nil {
		clone.Heads = make(map[string]*distribution.ChannelHead, len(plan.Heads))
		for name, head := range plan.Heads {
			clone.Heads[name] = cloneHead(head)
		}
	}
	clone.Members = make([]PlannedMember, len(plan.Members))
	for i, member := range plan.Members {
		clone.Members[i] = cloneMember(member)
	}
	return &clone
}

func cloneHead(head *distribution.ChannelHead) *distribution.ChannelHead {
	if head == nil {
		return nil
	}
	clone := *head
	if head.ReleaseSet != nil {
		clone.ReleaseSet = distribution.NormalizeReleaseSet(head.ReleaseSet)
	}
	return &clone
}

// NormalizePlan returns a deep copy with canonical member/dependency ordering.
func NormalizePlan(plan *Plan) *Plan {
	clone := ClonePlan(plan)
	if clone == nil {
		return nil
	}
	for i := range clone.Members {
		sort.Slice(clone.Members[i].Dependencies, func(a, b int) bool {
			return MemberKey(clone.Members[i].Dependencies[a].Ecosystem, clone.Members[i].Dependencies[a].Coordinate) <
				MemberKey(clone.Members[i].Dependencies[b].Ecosystem, clone.Members[i].Dependencies[b].Coordinate)
		})
	}
	sort.Slice(clone.Members, func(i, j int) bool {
		return MemberKey(clone.Members[i].Ecosystem, clone.Members[i].Coordinate) <
			MemberKey(clone.Members[j].Ecosystem, clone.Members[j].Coordinate)
	})
	return clone
}

func cloneMember(member PlannedMember) PlannedMember {
	member.Dependencies = append([]distribution.ReleaseSetDependency(nil), member.Dependencies...)
	member.Platforms = maps.Clone(member.Platforms)
	return member
}

func printableKey(key string) string { return strings.ReplaceAll(key, "\x00", "/") }
