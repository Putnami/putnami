package jobs

import (
	"bytes"
	"encoding/json"
	"testing"

	"go.putnami.dev/cli/model/workspace"
	ciproto "go.putnami.dev/protocol/ci"
	distribution "go.putnami.dev/protocol/distribution"
	"go.putnami.dev/protocol/features/spectest"
	job "go.putnami.dev/protocol/job"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/releaseset"
)

// attributionPolicy is the putnami.ci.json distribution section of a
// repository that opted into member attribution (ADR 0005).
func attributionPolicy() *ciproto.Distribution {
	return &ciproto.Distribution{Namespace: "putnami", MemberAttribution: true}
}

// legacyPlannedMember is the plan member shape of an extension SDK built
// before ADR 0005 added `project` and `kind`. It stays here, frozen, so the
// test decodes the plan the way such an extension does: strictly, refusing any
// field it does not know.
type legacyPlannedMember struct {
	Ecosystem            distribution.Ecosystem              `json:"ecosystem"`
	Coordinate           string                              `json:"coordinate"`
	Version              string                              `json:"version"`
	ArtifactDigest       string                              `json:"artifactDigest,omitempty"`
	Dependencies         []distribution.ReleaseSetDependency `json:"dependencies"`
	SourceRevision       string                              `json:"sourceRevision"`
	SelectionFingerprint string                              `json:"selectionFingerprint"`
	Platforms            map[string]string                   `json:"platforms,omitempty"`
	Selected             bool                                `json:"selected"`
	ProjectID            string                              `json:"projectId,omitempty"`
}

// legacyReleaseSetMember is the released member shape of a provider or backend
// built before ADR 0005, for the same strict reading of the released set.
type legacyReleaseSetMember struct {
	Ecosystem            distribution.Ecosystem              `json:"ecosystem"`
	Coordinate           string                              `json:"coordinate"`
	Version              string                              `json:"version"`
	ArtifactDigest       string                              `json:"artifactDigest"`
	Dependencies         []distribution.ReleaseSetDependency `json:"dependencies"`
	SourceRevision       string                              `json:"sourceRevision"`
	SelectionFingerprint string                              `json:"selectionFingerprint"`
	Platforms            map[string]string                   `json:"platforms,omitempty"`
}

type legacyPlan struct {
	ProtocolVersion int                           `json:"protocolVersion"`
	Namespace       string                        `json:"namespace"`
	Channels        []string                      `json:"channels"`
	Heads           map[string]*legacyChannelHead `json:"heads"`
	Members         []legacyPlannedMember         `json:"members"`
}

type legacyChannelHead struct {
	Ref        distribution.ReleaseSetRef `json:"ref"`
	Generation uint64                     `json:"generation"`
	ReleaseSet *legacyReleaseSet          `json:"releaseSet,omitempty"`
}

type legacyReleaseSet struct {
	ProtocolVersion int                      `json:"protocolVersion"`
	Namespace       string                   `json:"namespace"`
	Members         []legacyReleaseSetMember `json:"members"`
}

// decodeStrictly reads data into target the way every pre-attribution consumer
// does: encoding/json with unknown fields refused.
func decodeStrictly(t *testing.T, data []byte, target any) error {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func attributionWorkspace(t *testing.T) *workspace.Workspace {
	t.Helper()
	library := &workspace.Project{
		ID: "/typescript/framework/web", Name: "@putnami/web", Version: "1.1.0", Type: "library",
		Metadata: releaseSetProjectMetadata(t, "npm", "@putnami/web"),
	}
	service := &workspace.Project{
		ID: "/apps/service", Name: "service", Version: "1.1.0", Type: "application",
		Metadata: releaseSetProjectMembers(t, releaseset.MemberDeclaration{
			Ecosystem: "oci", Coordinate: "putnami/service", PackageStep: "docker", PublishStep: "docker",
		}),
	}
	return workspace.NewWorkspace(t.TempDir(), &wsproto.Config{Name: "putnami"}, []*workspace.Project{library, service})
}

// TestReleaseSetAttributionIsOffUntilTheRepositoryOptsIn pins the rollout
// contract of ADR 0005: the plan a publishing extension receives and the set
// the provider releases carry `project` and `kind` only when the repository
// declared `distribution.memberAttribution`. A publisher that emitted them by
// default broke every publication of a workspace whose pinned extension was
// built before the fields existed — the extension decodes the plan strictly
// and failed with `decode releaseSetPlan: json: unknown field "kind"`.
func TestReleaseSetAttributionIsOffUntilTheRepositoryOptsIn(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "release-set-attribution-opt-in", "default-plan-and-set-decode-under-a-pre-attribution-strict-reader")
	for _, policy := range []*ciproto.Distribution{nil, {Namespace: "putnami"}} {
		ws := attributionWorkspace(t)
		useReleaseSetProvenance(t, nil)
		options := releaseSetAllOptions()
		options.Policy = policy
		run := prepareReleaseSetWithHead(t, options, ws, emptyHead("canary"))
		for _, member := range run.plan.Members {
			if member.Project != "" || member.Kind != "" {
				t.Fatalf("policy %+v: planned member %+v carries attribution without the opt-in", policy, member)
			}
		}
		// The exact bytes the job context carries, read as a pre-attribution
		// extension SDK reads them.
		planJSON, err := json.Marshal(releaseset.ClonePlan(run.plan))
		if err != nil {
			t.Fatal(err)
		}
		var legacy legacyPlan
		if err := decodeStrictly(t, planJSON, &legacy); err != nil {
			t.Fatalf("policy %+v: a pre-attribution extension refuses the default plan: %v\n%s", policy, err, planJSON)
		}
		if len(legacy.Members) != 2 {
			t.Fatalf("legacy reader saw %d members, want 2", len(legacy.Members))
		}

		final, err := reconcilePublishedReleaseSet(run.plan, publishedResults(run.plan, digestFor('d')))
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		setJSON, err := json.Marshal(final)
		if err != nil {
			t.Fatal(err)
		}
		var legacySet legacyReleaseSet
		if err := decodeStrictly(t, setJSON, &legacySet); err != nil {
			t.Fatalf("policy %+v: a pre-attribution provider refuses the default released set: %v\n%s", policy, err, setJSON)
		}
	}
}

// TestReleaseSetAttributionFollowsTheRepositoryOptIn is the other half: once
// the repository declares memberAttribution, every selected member records its
// project and kind, and a reader built before the fields exists refuses the
// plan — which is exactly why the declaration is the operator's, made only
// after every pinned extension and the provider know the fields.
func TestReleaseSetAttributionFollowsTheRepositoryOptIn(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "release-set-attribution-opt-in", "opted-in-plan-records-project-and-kind")
	ws := attributionWorkspace(t)
	useReleaseSetProvenance(t, nil)
	options := releaseSetAllOptions()
	options.Policy = attributionPolicy()
	run := prepareReleaseSetWithHead(t, options, ws, emptyHead("canary"))
	image, ok := run.plan.Member(distribution.Ecosystem("oci"), "putnami/service")
	if !ok || image.Project != "apps/service" || image.Kind != distribution.KindImage {
		t.Fatalf("planned image member = %+v, %v; want apps/service classified as an image", image, ok)
	}
	planJSON, err := json.Marshal(releaseset.ClonePlan(run.plan))
	if err != nil {
		t.Fatal(err)
	}
	var legacy legacyPlan
	if err := decodeStrictly(t, planJSON, &legacy); err == nil {
		t.Fatalf("a pre-attribution reader accepted an attributed plan:\n%s", planJSON)
	}
}

// TestReleaseSetAttributionInheritsTheHeadVerbatimWithoutTheOptIn pins that
// turning the policy off never rewrites a member the run did not republish: an
// unchanged member keeps the attribution its head record carries, as every
// other field of the record, and only the republished member loses it.
func TestReleaseSetAttributionInheritsTheHeadVerbatimWithoutTheOptIn(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "release-set-attribution-opt-in", "inherited-member-keeps-head-attribution")
	ws := attributionWorkspace(t)
	useReleaseSetProvenance(t, nil)
	attributed := releaseSetAllOptions()
	attributed.Policy = attributionPolicy()
	first := prepareReleaseSetWithHead(t, attributed, ws, emptyHead("canary"))
	final, err := reconcilePublishedReleaseSet(first.plan, publishedResults(first.plan, digestFor('d')))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	head := &distribution.ChannelHead{Generation: 1, ReleaseSet: final}
	refreshHeadRef(t, head)

	// Only the service tree moved; the policy no longer opts in.
	useReleaseSetProvenance(t, map[string]string{"/apps/service": digestFor('9')})
	plain := releaseSetRequest()
	plain.Policy = &ciproto.Distribution{Namespace: "putnami"}
	second := prepareReleaseSetWithHead(t, plain, ws, map[string]*distribution.ChannelHead{"canary": head})
	library, ok := second.plan.Member(distribution.Ecosystem("npm"), "@putnami/web")
	if !ok || library.Selected || library.Project != "typescript/framework/web" || library.Kind != distribution.KindLibrary {
		t.Fatalf("inherited library = %+v, %v; want the head's attribution kept verbatim", library, ok)
	}
	image, ok := second.plan.Member(distribution.Ecosystem("oci"), "putnami/service")
	if !ok || !image.Selected || image.Project != "" || image.Kind != "" {
		t.Fatalf("republished image = %+v, %v; want it selected without attribution", image, ok)
	}
}

// TestReleaseSetAttributionStampsCarriedOverMembersFromThePlan pins the
// ADR 0005 rule 3: attribution is the plan's statement
// about a member, not a property of the publication that produced it. A head
// published before the repository opted in carries bare members; the first
// opted-in plan over it attributes every member the workspace declares, the
// ones it carries over included, and the set sent to the provider carries the
// same values. The artifact record of the carried-over member is still the
// head's, verbatim.
func TestReleaseSetAttributionStampsCarriedOverMembersFromThePlan(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "release-set-attribution-opt-in", "opted-in-plan-attributes-carried-over-members")
	ws := attributionWorkspace(t)
	useReleaseSetProvenance(t, nil)
	plain := releaseSetAllOptions()
	plain.Policy = &ciproto.Distribution{Namespace: "putnami"}
	first := prepareReleaseSetWithHead(t, plain, ws, emptyHead("canary"))
	bare, err := reconcilePublishedReleaseSet(first.plan, publishedResults(first.plan, digestFor('d')))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for _, member := range bare.Members {
		if member.Project != "" || member.Kind != "" {
			t.Fatalf("head published without the opt-in carries attribution: %+v", member)
		}
	}
	head := &distribution.ChannelHead{Generation: 1, ReleaseSet: bare}
	refreshHeadRef(t, head)

	// Only the service tree moved; the repository now opts in.
	useReleaseSetProvenance(t, map[string]string{"/apps/service": digestFor('9')})
	attributed := releaseSetRequest()
	attributed.Policy = attributionPolicy()
	second := prepareReleaseSetWithHead(t, attributed, ws, map[string]*distribution.ChannelHead{"canary": head})
	library, ok := second.plan.Member(distribution.Ecosystem("npm"), "@putnami/web")
	if !ok || library.Selected {
		t.Fatalf("library = %+v, %v; want it carried over unselected", library, ok)
	}
	if library.Project != "typescript/framework/web" || library.Kind != distribution.KindLibrary {
		t.Fatalf("carried-over library = %+v; want the plan's attribution although this run does not republish it", library)
	}
	var headLibrary distribution.ReleaseSetMember
	for _, member := range bare.Members {
		if member.Ecosystem == "npm" {
			headLibrary = member
		}
	}
	if library.Version != headLibrary.Version || library.ArtifactDigest != headLibrary.ArtifactDigest ||
		library.SourceRevision != headLibrary.SourceRevision || library.SelectionFingerprint != headLibrary.SelectionFingerprint {
		t.Fatalf("carried-over library = %+v; want the head's artifact record %+v verbatim", library, headLibrary)
	}
	image, ok := second.plan.Member(distribution.Ecosystem("oci"), "putnami/service")
	if !ok || !image.Selected || image.Project != "apps/service" || image.Kind != distribution.KindImage {
		t.Fatalf("republished image = %+v, %v; want it selected and attributed", image, ok)
	}

	// The set handed to the provider states the same attribution for every
	// member, and the plan that states it is what every extension validates.
	final, err := reconcilePublishedReleaseSet(second.plan, publishedResults(second.plan, digestFor('e')))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for _, member := range final.Members {
		wantProject, wantKind, wantDigest := "apps/service", distribution.KindImage, digestFor('e')
		if member.Ecosystem == "npm" {
			wantProject, wantKind, wantDigest = "typescript/framework/web", distribution.KindLibrary, headLibrary.ArtifactDigest
		}
		if member.Project != wantProject || member.Kind != wantKind || member.ArtifactDigest != wantDigest {
			t.Fatalf("released member %+v; want project %q kind %q digest %q", member, wantProject, wantKind, wantDigest)
		}
	}
	planJSON, err := json.Marshal(releaseset.ClonePlan(second.plan))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := releaseset.ParseParams(job.Params{releaseset.ContextParamName: planJSON}); err != nil {
		t.Fatalf("an extension refuses the plan that attributes a carried-over member: %v", err)
	}
}
