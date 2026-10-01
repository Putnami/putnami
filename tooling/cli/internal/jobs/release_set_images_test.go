package jobs

import (
	"context"
	"testing"

	distribution "go.putnami.dev/protocol/distribution"
	"go.putnami.dev/sdk/extension/releaseset"
)

type imageEvidenceProvider struct {
	fakeReleaseSetProvider
	images  []releaseset.PublishedImage
	present bool
	members []releaseset.MemberEvidence
}

func (p *imageEvidenceProvider) Release(ctx context.Context, request *distribution.ReleaseRequest) (*distribution.ReleaseResponse, error) {
	p.images, p.present = releaseset.PublishedImagesFromContext(ctx)
	p.members, _ = releaseset.MemberEvidenceFromContext(ctx)
	return p.fakeReleaseSetProvider.Release(ctx, request)
}

func TestFinalizerCarriesSelectedNativeImageEvidenceToProvider(t *testing.T) {
	member := releaseset.PlannedMember{
		Ecosystem: "oci", Coordinate: "workspace/service", ProjectID: "/apps/service",
		Version: "1.0.0", SourceRevision: testRevision, SelectionFingerprint: digestFor('a'),
		Selected: true, Dependencies: []distribution.ReleaseSetDependency{},
	}
	plan := &releaseset.Plan{Namespace: "workspace", Channels: []string{"canary"}, Members: []releaseset.PlannedMember{member}}
	provider := &imageEvidenceProvider{}
	results := publishedResults(plan, digestFor('c'))
	(&ReleaseSetRun{plan: plan, provider: provider}).Finalizer(t.Context())(results)
	if result := results[releaseSetResultKey]; result == nil || result.Status != "success" {
		t.Fatalf("release result = %+v", result)
	}
	if !provider.present || len(provider.images) != 1 || provider.images[0].Project != "apps/service" || provider.images[0].Digest != digestFor('c') {
		t.Fatalf("private image evidence = %+v, present=%v", provider.images, provider.present)
	}
}

func TestImageEvidenceNeverProjectsInheritedOrAmbiguousMembers(t *testing.T) {
	member := releaseset.PlannedMember{Ecosystem: "oci", Coordinate: "workspace/service", ProjectID: "/apps/service", Version: "1.0.0"}
	set := &distribution.ReleaseSet{Members: []distribution.ReleaseSetMember{{Ecosystem: "oci", Coordinate: member.Coordinate, Version: member.Version, ArtifactDigest: digestFor('c')}}}
	plan := &releaseset.Plan{Members: []releaseset.PlannedMember{member}}
	if _, present := releaseset.PublishedImagesFromContext(withSelectedImageEvidence(t.Context(), plan, set)); present {
		t.Fatal("inherited image became same-run evidence")
	}
	member.Selected = true
	plan.Members = []releaseset.PlannedMember{member, member}
	if _, present := releaseset.PublishedImagesFromContext(withSelectedImageEvidence(t.Context(), plan, set)); present {
		t.Fatal("ambiguous project became image evidence")
	}
	plan.Members = []releaseset.PlannedMember{member}
	set.Members[0].Version = "other"
	if _, present := releaseset.PublishedImagesFromContext(withSelectedImageEvidence(t.Context(), plan, set)); present {
		t.Fatal("different immutable member became image evidence")
	}
}

func TestFinalizerPreservesConfigProducerRouteWithoutCoordinateInference(t *testing.T) {
	image := releaseset.PlannedMember{Ecosystem: "oci", Coordinate: "workspace/service", ProjectID: "/apps/service", Version: "1.0.0", SourceRevision: testRevision, SelectionFingerprint: digestFor('a'), Selected: true, Dependencies: []distribution.ReleaseSetDependency{}}
	config := image
	config.Ecosystem = "put"
	config.Coordinate = "workspace/unrelated-name"
	plan := &releaseset.Plan{Namespace: "workspace", Channels: []string{"canary"}, Members: []releaseset.PlannedMember{image, config}}
	provider := &imageEvidenceProvider{}
	run := &ReleaseSetRun{plan: plan, provider: provider, routes: map[string]releaseMemberRoute{
		releaseset.MemberKey(image.Ecosystem, image.Coordinate):   {projectID: image.ProjectID, publisher: "@putnami/cloud", publishCommand: "publish", publishStep: "cloud-image"},
		releaseset.MemberKey(config.Ecosystem, config.Coordinate): {projectID: config.ProjectID, publisher: "@putnami/cloud", publishCommand: "publish", publishStep: "cloud-publish-config"},
	}}
	results := map[string]*JobResult{"image": {Status: "success", Events: []RawJobEvent{publishedMemberEvent(image, digestFor('b'))}}, "config": {Status: "success", Events: []RawJobEvent{publishedMemberEvent(config, digestFor('c'))}}}
	run.Finalizer(t.Context())(results)
	if result := results[releaseSetResultKey]; result == nil || result.Status != "success" {
		t.Fatalf("result=%+v", result)
	}
	if len(provider.members) != 2 {
		t.Fatalf("members=%+v", provider.members)
	}
	found := false
	for _, member := range provider.members {
		if member.Coordinate == config.Coordinate {
			found = true
			if member.Project != "apps/service" || member.Step != "cloud-publish-config" || member.Publisher != "@putnami/cloud" || member.Digest != digestFor('c') {
				t.Fatalf("Config evidence=%+v", member)
			}
		}
	}
	if !found {
		t.Fatal("Config producer evidence missing")
	}
}
