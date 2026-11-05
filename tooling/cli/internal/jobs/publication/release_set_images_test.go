package publication

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	distribution "go.putnami.dev/protocol/distribution"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/releaseset"
)

// evidenceProviderEnv makes the test binary a release-set provider that
// records the private evidence files it receives into the directory the
// variable names, then answers with FAKE_RESPONSE.
const evidenceProviderEnv = "PUTNAMI_PUBLICATION_EVIDENCE_PROVIDER"

const (
	observedImagesFile  = "images.json"
	observedMembersFile = "members.json"
)

// runEvidenceProvider copies each evidence file the provider call received
// into dir. A file the call did not receive leaves no copy.
func runEvidenceProvider(dir string) int {
	for env, name := range map[string]string{
		runtimeproto.ReleaseSetPublishedImagesFileEnv: observedImagesFile,
		runtimeproto.ReleaseSetMembersFileEnv:         observedMembersFile,
	} {
		path := os.Getenv(env)
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	fmt.Println(os.Getenv("FAKE_RESPONSE"))
	return 0
}

// providerEvidence is what a provider process received through its private
// evidence files.
type providerEvidence struct {
	images         []releaseset.PublishedImage
	imagesPresent  bool
	members        []releaseset.MemberEvidence
	membersPresent bool
}

// observeProviderEvidence releases request through the SDK client to a
// provider process that answers response, and returns the evidence files that
// process received for ctx.
func observeProviderEvidence(t *testing.T, ctx context.Context, request *distribution.ReleaseRequest, response *distribution.ReleaseResponse) providerEvidence {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	client := releaseset.NewClient(executable)
	client.TempDir = dir
	client.Env = append(os.Environ(), evidenceProviderEnv+"="+dir, "FAKE_RESPONSE="+string(encoded))
	if _, err := client.Release(ctx, request); err != nil {
		t.Fatalf("release through the provider process: %v", err)
	}
	var observed providerEvidence
	if data, err := os.ReadFile(filepath.Join(dir, observedImagesFile)); err == nil {
		observed.imagesPresent = true
		if observed.images, err = runtimeproto.ParseReleaseSetPublishedImages(data); err != nil {
			t.Fatalf("published images the provider received: %v", err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(dir, observedMembersFile)); err == nil {
		observed.membersPresent = true
		var envelope struct {
			Members []releaseset.MemberEvidence `json:"members"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			t.Fatalf("member evidence the provider received: %v", err)
		}
		observed.members = envelope.Members
	}
	return observed
}

// releaseFixture is a valid release request and the response a provider gives it.
func releaseFixture(t *testing.T) (*distribution.ReleaseRequest, *distribution.ReleaseResponse) {
	t.Helper()
	head := releaseSetHead(t)["canary"]
	request := &distribution.ReleaseRequest{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		ReleaseSet:      *head.ReleaseSet,
		Channels:        []distribution.ChannelRequest{{Name: "canary", Visibility: distribution.VisibilityInternal}},
		Visibility:      distribution.VisibilityChain{Repo: distribution.VisibilityInternal},
	}
	response, err := (&fakeReleaseSetProvider{}).Release(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	return request, response
}

// imageEvidenceProvider releases through a provider process and keeps the
// evidence that process received.
type imageEvidenceProvider struct {
	fakeReleaseSetProvider
	t        *testing.T
	observed providerEvidence
}

func (p *imageEvidenceProvider) Release(ctx context.Context, request *distribution.ReleaseRequest) (*distribution.ReleaseResponse, error) {
	response, err := p.fakeReleaseSetProvider.Release(ctx, request)
	if err != nil {
		return nil, err
	}
	p.observed = observeProviderEvidence(p.t, ctx, request, response)
	return response, nil
}

func TestFinalizerCarriesSelectedNativeImageEvidenceToProvider(t *testing.T) {
	member := releaseset.PlannedMember{
		Ecosystem: "oci", Coordinate: "workspace/service", ProjectID: "/apps/service",
		Version: "1.0.0", SourceRevision: testRevision, SelectionFingerprint: digestFor('a'),
		Selected: true, Dependencies: []distribution.ReleaseSetDependency{},
	}
	plan := &releaseset.Plan{Namespace: "workspace", Channels: []string{"canary"}, Members: []releaseset.PlannedMember{member}}
	provider := &imageEvidenceProvider{t: t}
	results := publishedResults(plan, digestFor('c'))
	(&ReleaseSetRun{plan: plan, provider: provider}).Finalizer(t.Context())(results)
	if result := results[releaseSetResultKey]; result == nil || result.Status != "success" {
		t.Fatalf("release result = %+v", result)
	}
	if !provider.observed.imagesPresent || len(provider.observed.images) != 1 || provider.observed.images[0].Project != "apps/service" || provider.observed.images[0].Digest != digestFor('c') {
		t.Fatalf("private image evidence = %+v, present=%v", provider.observed.images, provider.observed.imagesPresent)
	}
}

func TestImageEvidenceNeverProjectsInheritedOrAmbiguousMembers(t *testing.T) {
	member := releaseset.PlannedMember{Ecosystem: "oci", Coordinate: "workspace/service", ProjectID: "/apps/service", Version: "1.0.0"}
	set := &distribution.ReleaseSet{Members: []distribution.ReleaseSetMember{{Ecosystem: "oci", Coordinate: member.Coordinate, Version: member.Version, ArtifactDigest: digestFor('c')}}}
	plan := &releaseset.Plan{Members: []releaseset.PlannedMember{member}}
	request, response := releaseFixture(t)
	imagesPresent := func() bool {
		return observeProviderEvidence(t, withSelectedImageEvidence(t.Context(), plan, set), request, response).imagesPresent
	}
	if imagesPresent() {
		t.Fatal("inherited image became same-run evidence")
	}
	member.Selected = true
	plan.Members = []releaseset.PlannedMember{member, member}
	if imagesPresent() {
		t.Fatal("ambiguous project became image evidence")
	}
	plan.Members = []releaseset.PlannedMember{member}
	set.Members[0].Version = "other"
	if imagesPresent() {
		t.Fatal("different immutable member became image evidence")
	}
}

func TestFinalizerPreservesConfigProducerRouteWithoutCoordinateInference(t *testing.T) {
	image := releaseset.PlannedMember{Ecosystem: "oci", Coordinate: "workspace/service", ProjectID: "/apps/service", Version: "1.0.0", SourceRevision: testRevision, SelectionFingerprint: digestFor('a'), Selected: true, Dependencies: []distribution.ReleaseSetDependency{}}
	config := image
	config.Ecosystem = "put"
	config.Coordinate = "workspace/unrelated-name"
	plan := &releaseset.Plan{Namespace: "workspace", Channels: []string{"canary"}, Members: []releaseset.PlannedMember{image, config}}
	provider := &imageEvidenceProvider{t: t}
	run := &ReleaseSetRun{plan: plan, provider: provider, routes: map[string]releaseMemberRoute{
		releaseset.MemberKey(image.Ecosystem, image.Coordinate):   {projectID: image.ProjectID, publisher: "@putnami/cloud", publishCommand: "publish", publishStep: "cloud-image"},
		releaseset.MemberKey(config.Ecosystem, config.Coordinate): {projectID: config.ProjectID, publisher: "@putnami/cloud", publishCommand: "publish", publishStep: "cloud-publish-config"},
	}}
	results := map[string]*jobmodel.JobResult{"image": {Status: "success", Events: []jobmodel.RawJobEvent{publishedMemberEvent(image, digestFor('b'))}}, "config": {Status: "success", Events: []jobmodel.RawJobEvent{publishedMemberEvent(config, digestFor('c'))}}}
	run.Finalizer(t.Context())(results)
	if result := results[releaseSetResultKey]; result == nil || result.Status != "success" {
		t.Fatalf("result=%+v", result)
	}
	if len(provider.observed.members) != 2 {
		t.Fatalf("members=%+v", provider.observed.members)
	}
	found := false
	for _, member := range provider.observed.members {
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
