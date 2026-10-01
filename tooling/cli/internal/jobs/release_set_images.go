package jobs

import (
	"context"
	"strings"

	distribution "go.putnami.dev/protocol/distribution"
	"go.putnami.dev/sdk/extension/releaseset"
)

// withSelectedImageEvidence projects reconciled native OCI publications onto
// the existing private Cloud handoff contract. Inherited images are deliberately
// absent: their presence in a release set is not a publication by this run.
// The v1 image contract can represent only one OCI member per project. An
// ambiguous projection carries no evidence, leaving generic publication intact
// while a Cloud handoff requiring that evidence refuses it.
func withSelectedImageEvidence(ctx context.Context, plan *releaseset.Plan, set *distribution.ReleaseSet) context.Context {
	if plan == nil || set == nil {
		return ctx
	}
	final := make(map[string]distribution.ReleaseSetMember, len(set.Members))
	for _, member := range set.Members {
		final[releaseset.MemberKey(member.Ecosystem, member.Coordinate)] = member
	}
	images := make([]releaseset.PublishedImage, 0)
	seen := make(map[string]bool)
	for _, selected := range plan.SelectedMembers() {
		if selected.Ecosystem != distribution.Ecosystem("oci") {
			continue
		}
		project := strings.TrimPrefix(selected.ProjectID, "/")
		if project == "" || seen[project] {
			return ctx
		}
		member, ok := final[releaseset.MemberKey(selected.Ecosystem, selected.Coordinate)]
		if !ok || member.Version != selected.Version || member.SourceRevision != selected.SourceRevision || member.SelectionFingerprint != selected.SelectionFingerprint {
			return ctx
		}
		seen[project] = true
		images = append(images, releaseset.PublishedImage{Project: project, Digest: member.ArtifactDigest})
	}
	if len(images) == 0 {
		return ctx
	}
	return releaseset.WithPublishedImages(ctx, images)
}

// withMemberEvidence preserves the exact producer route from the frozen plan.
func (run *ReleaseSetRun) withMemberEvidence(ctx context.Context, set *distribution.ReleaseSet) context.Context {
	final := make(map[string]distribution.ReleaseSetMember, len(set.Members))
	for _, member := range set.Members {
		final[releaseset.MemberKey(member.Ecosystem, member.Coordinate)] = member
	}
	evidence := make([]releaseset.MemberEvidence, 0)
	for _, selected := range run.plan.SelectedMembers() {
		key := releaseset.MemberKey(selected.Ecosystem, selected.Coordinate)
		route, ok := run.routes[key]
		if !ok {
			continue
		}
		member, ok := final[key]
		if !ok {
			continue
		}
		evidence = append(evidence, releaseset.MemberEvidence{Project: strings.TrimPrefix(selected.ProjectID, "/"), Ecosystem: string(member.Ecosystem), Coordinate: member.Coordinate, Version: member.Version, Digest: member.ArtifactDigest, Publisher: route.publisher, Command: route.publishCommand, Step: route.publishStep})
	}
	if len(evidence) == 0 {
		return ctx
	}
	return releaseset.WithMemberEvidence(ctx, evidence)
}
