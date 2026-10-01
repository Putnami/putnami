package jobs

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	modelextension "go.putnami.dev/cli/model/extension"
	"go.putnami.dev/cli/model/workspace"
	distribution "go.putnami.dev/protocol/distribution"
	"go.putnami.dev/protocol/features/spectest"
	runtimeproto "go.putnami.dev/protocol/runtime"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// releaseSetSuccessLine is the result line a fixture task that succeeds prints.
const releaseSetSuccessLine = `{"v":2,"type":"result","data":{"status":"success"}}`

// These tests exercise the Putnami-owned publish handoff against a stateful
// in-memory provider. Cloud authorization, persistence, and deploy consumption
// remain Cloud-owned conformance/E2E concerns; reproducing them here would
// create a second, false implementation of that behavior.

func TestReleaseSetE2EEmptyChannelReleaseAndCloudlessCompatibility(t *testing.T) {
	ctx := context.Background()
	ledger := newReleaseSetE2ELedger()
	factoryCalls := useReleaseSetE2EProvider(t, ledger)
	useReleaseSetProvenance(t, nil)
	ws, upstream, downstream := releaseSetE2EWorkspace(t, "1.0.0")
	options := releaseSetE2EAllOptions()

	// Full publishing without a provider is the unchanged cloudless path: it
	// neither claims a release set nor tries to construct a provider client.
	run, err := PrepareReleaseSet(ctx, options, ws, []*workspace.Project{upstream, downstream}, &extension.DiscoveryResult{}, releaseSetTestFingerprints(t, ws))
	if err != nil || run != nil {
		t.Fatalf("cloudless full preparation = %+v, %v; want nil, nil", run, err)
	}
	if got := *factoryCalls; got != 0 {
		t.Fatalf("cloudless full constructed provider %d time(s)", got)
	}

	// The channel has no head yet: the one plan-time resolve answers a null
	// head and the release expects none.
	run = releaseSetE2EPrepare(t, ctx, options, ws, []*workspace.Project{upstream, downstream})
	if run.plan.Baseline() != nil || !run.plan.SelectsEveryMember() {
		t.Fatalf("empty-channel plan = %+v", run.plan)
	}
	if got := ledger.counts().resolve; got != 1 {
		t.Fatalf("empty-channel publish resolved the channel %d time(s), want exactly once", got)
	}

	results, registry := releaseSetE2EPublish(run.plan, '1', nil)
	run.Finalizer(ctx)(results)
	outcome := releaseSetE2EOutcome(t, results)
	if registry.packageCount(upstream.Name) != 1 || registry.uploadCount(upstream.Name) != 1 ||
		registry.packageCount(downstream.Name) != 1 || registry.uploadCount(downstream.Name) != 1 {
		t.Fatalf("empty-channel registry activity = %+v", registry)
	}
	if head, ok := ledger.head("putnami", "canary"); !ok || head != outcome.Ref {
		t.Fatalf("channel head = %+v, %v; outcome = %+v", head, ok, outcome.Ref)
	}
	requests := ledger.releaseRequestsSnapshot()
	if len(requests) != 1 || len(requests[0].Channels) != 1 || requests[0].Channels[0].Expected != nil ||
		requests[0].Channels[0].Name != "canary" || requests[0].Visibility.Repo != distribution.VisibilityInternal {
		t.Fatalf("release requests = %+v; want one expected:null with the npm projection", requests)
	}
	if outcome.Channels["canary"] == nil || outcome.Channels["canary"].Generation == 0 || outcome.ProtocolVersion != distribution.ProtocolVersion {
		t.Fatalf("outcome = %+v", outcome)
	}
	released := ledger.mustReleaseSet(t, outcome.Ref)
	for _, member := range released.Members {
		if member.SourceRevision != testRevision || member.SelectionFingerprint != testFingerprint("/"+strings.TrimPrefix(member.Coordinate, "@putnami/")) {
			t.Fatalf("released member %+v lacks the tree provenance", member)
		}
	}
}

// TestReleaseSetE2EArchiveRoutesItsPackageProviderBeforeCoordinatorCommit pins
// the Cloud archive route at the framework boundary. The package and publish
// subprocess fixtures have separate extension owners, the publish subprocess
// consumes bytes written by protocol/runtime's real emitter, and the internal
// release-set job commits before deploy may observe the result. The real Go
// packager, Put authorization, and Put persistence remain separate integration
// boundaries.
func TestReleaseSetE2EArchiveRoutesItsPackageProviderBeforeCoordinatorCommit(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "release-set-members-by-ecosystem", "selected-member-routes-exact-artifact-steps")
	ctx := context.Background()
	root := t.TempDir()
	packageMarker := filepath.Join(root, "archive-packaged")
	releaseMarker := filepath.Join(root, "archive-released")
	eventFile := filepath.Join(root, "published-member.jsonl")
	ledger := newReleaseSetE2ELedger()
	provider := &releaseSetBoundaryProvider{releaseSetE2ELedger: ledger, marker: releaseMarker}

	project := &workspace.Project{ID: "/cloud", Name: "@putnami/cloud", Path: "cloud", Version: "1.2.3", Type: "application"}
	if err := os.MkdirAll(filepath.Join(root, project.Path), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "cloud-workspace"}, []*workspace.Project{project})
	member := releaseset.PlannedMember{
		Ecosystem: "archive", Coordinate: "putnami/cloud", Version: project.Version,
		ProjectID: project.ID, Selected: true, SourceRevision: testRevision,
		SelectionFingerprint: testFingerprint(project.ID),
	}
	plan := &releaseset.Plan{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "cloud-workspace",
		Channels:        []string{"stable"},
		Heads:           map[string]*distribution.ChannelHead{"stable": nil},
		Members:         []releaseset.PlannedMember{member},
	}
	if err := releaseset.ValidatePlan(plan); err != nil {
		t.Fatalf("archive release plan: %v", err)
	}
	run := &ReleaseSetRun{
		provider: provider,
		plan:     plan,
		routes: map[string]releaseMemberRoute{
			releaseset.MemberKey(member.Ecosystem, member.Coordinate): {
				projectID: project.ID, publisher: "@putnami/cloud", packagePublisher: "@putnami/go",
				packageStep: "archives", publishCommand: "publish", publishStep: "cloud-publish-archives",
			},
		},
	}
	if err := os.WriteFile(eventFile, []byte(publishedMemberRuntimeLine(member, releaseSetE2EDigest('a'))), 0o600); err != nil {
		t.Fatal(err)
	}

	packager := &modelextension.ExtensionDescription{Name: "@putnami/go", Path: root}
	publisher := &modelextension.ExtensionDescription{Name: "@putnami/cloud", Path: root}
	packageJob := &ScheduledJob{Project: project, Extension: packager, JobDef: &modelextension.JobDefinition{
		Name: "package~archives", CommandName: "package", StepID: "archives", ExtensionName: packager.Name,
	}}
	fixtureTask(t, packageJob.JobDef, fixtureScript{{"write", "packaged\n", packageMarker}, {"print", releaseSetSuccessLine}})
	publishJob := &ScheduledJob{Project: project, Extension: publisher, JobDef: &modelextension.JobDefinition{
		Name: "publish~cloud-publish-archives", CommandName: "publish", StepID: "cloud-publish-archives", ExtensionName: publisher.Name,
	}, DependsOn: []string{packageJob.Key()}}
	fixtureTask(t, publishJob.JobDef, fixtureScript{
		{"exit-unless", "1", "nonempty:" + packageMarker},
		{"cat", eventFile},
		{"print", releaseSetSuccessLine},
	})
	deployJob := &ScheduledJob{Project: project, Extension: publisher, JobDef: &modelextension.JobDefinition{
		Name: "deploy~cloud", CommandName: "deploy", StepID: "cloud", ExtensionName: publisher.Name,
	}, DependsOn: []string{publishJob.Key()}}
	fixtureTask(t, deployJob.JobDef, fixtureScript{{"exit-unless", "1", "nonempty:" + releaseMarker}, {"print", releaseSetSuccessLine}})

	planned, err := run.AttachPlan([]*ScheduledJob{packageJob, publishJob, deployJob})
	if err != nil {
		t.Fatal(err)
	}
	planned, internal, err := run.AttachBarrier(planned)
	if err != nil {
		t.Fatal(err)
	}
	result := RunPlan(ctx, RunRequest{
		Workspace: ws, Plan: planned, InternalJobs: internal,
		Config: SchedulerConfig{MaxParallel: 1, NoCache: true}, Renderer: &mockRenderer{},
	})
	if !result.Success || result.Results[deployJob.Key()] == nil || result.Results[deployJob.Key()].Status != "success" {
		t.Fatalf("archive coordinator run success=%v package=%+v publish=%+v release=%+v deploy=%+v",
			result.Success, result.Results[packageJob.Key()], result.Results[publishJob.Key()], result.Results[releaseSetResultKey], result.Results[deployJob.Key()])
	}
	outcome := releaseSetE2EOutcome(t, result.Results)
	released := ledger.mustReleaseSet(t, outcome.Ref)
	if len(released.Members) != 1 || released.Members[0].Ecosystem != "archive" ||
		released.Members[0].Coordinate != "putnami/cloud" || released.Members[0].Version != "1.2.3" ||
		released.Members[0].ArtifactDigest != releaseSetE2EDigest('a') {
		t.Fatalf("coordinator committed archive member = %+v", released.Members)
	}
}

func TestReleaseSetE2ESameSessionCommitsBeforeDependentDeploy(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	project := &workspace.Project{
		ID: "/app", Name: "@putnami/app", Path: "app", Version: "1.0.0",
		Type: "library", Publish: []string{"npm"},
		Metadata: releaseSetProjectMetadata(t, "npm", "@putnami/upstream"),
	}
	if err := os.MkdirAll(filepath.Join(root, project.Path), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "putnami"}, []*workspace.Project{project})
	marker := filepath.Join(root, "release-set-committed")
	provider := &releaseSetBoundaryProvider{releaseSetE2ELedger: newReleaseSetE2ELedger(), marker: marker}
	useReleaseSetProviderFake(t, provider)
	useReleaseSetProvenance(t, nil)

	run, err := PrepareReleaseSet(ctx, ReleaseSetOptions{
		Commands: []string{"publish", "deploy"}, All: true, Channels: []string{"canary"}, Profiles: testProfiles(),
	}, ws, []*workspace.Project{project}, releaseSetE2EDiscovery(), releaseSetTestFingerprints(t, ws))
	if err != nil || run == nil || run.Plan() == nil {
		t.Fatalf("prepare same-session release set = %+v, %v", run, err)
	}
	member := run.Plan().SelectedMembers()[0]
	ext := &modelextension.ExtensionDescription{Name: "@test/release-boundary", Path: root}
	publish := &ScheduledJob{
		Project: project, Extension: ext,
		JobDef: &modelextension.JobDefinition{
			Name: "publish~npm", CommandName: "publish", ExtensionName: ext.Name,
		},
	}
	fixtureTask(t, publish.JobDef, fixtureScript{
		{"print", fmt.Sprintf(`{"v":2,"type":"artifact","data":{"kind":"published-member","ecosystem":"npm","coordinate":"%s","version":"%s","artifactDigest":"%s"}}`,
			member.Coordinate, member.Version, releaseSetE2EDigest('c'))},
		{"print", releaseSetSuccessLine},
	})
	deploy := &ScheduledJob{
		Project: project, Extension: ext,
		JobDef: &modelextension.JobDefinition{
			Name: "deploy~cloud", CommandName: "deploy", ExtensionName: ext.Name,
		},
		DependsOn: []string{publish.Key()},
	}
	fixtureTask(t, deploy.JobDef, fixtureScript{
		{"print-if", `{"v":2,"type":"result","data":{"status":"failed","error":{"message":"release set was not committed before deploy"}}}`, "!nonempty:" + marker},
		{"exit-unless", "1", "nonempty:" + marker},
		{"print", releaseSetSuccessLine},
	})
	planned, internalJobs, err := run.AttachBarrier([]*ScheduledJob{publish, deploy})
	if err != nil {
		t.Fatalf("attach same-session release-set barrier: %v", err)
	}
	renderer := &mockRenderer{}
	result := RunPlan(ctx, RunRequest{
		Workspace: ws,
		Plan:      planned,
		Config: SchedulerConfig{
			MaxParallel: 1,
			NoCache:     true,
		},
		Renderer:     renderer,
		InternalJobs: internalJobs,
	})
	if !result.Success || result.Results[deploy.Key()] == nil || result.Results[deploy.Key()].Status != "success" {
		t.Fatalf("same-session deploy ran before release-set commit: success=%v deploy=%+v releaseSet=%+v error=%v",
			result.Success, result.Results[deploy.Key()], result.Results[releaseSetResultKey], releaseSetErrorMessage(result.Results[releaseSetResultKey]))
	}
	if data, err := os.ReadFile(marker); err != nil || !strings.Contains(string(data), "rs_") || !strings.Contains(string(data), "sha256:") {
		t.Fatalf("committed release-set marker = %q, %v", data, err)
	}
}

// TestReleaseSetBarrierCompletesTheDeployContractFromTheReleasedSet pins what
// replaced the barrier's private, project-keyed Docker proof (T13): the deploy
// handoff is now the released set itself. The barrier commits, then hands each
// same-session deploy the set ref and the member digests that workload owns.
func TestReleaseSetBarrierCompletesTheDeployContractFromTheReleasedSet(t *testing.T) {
	provider := &fakeReleaseSetProvider{releaseOutcome: distribution.ReleaseOutcomeAlreadyCurrent}
	plan := releaseSetE2ENoImpactOCIPlan(t)
	run := &ReleaseSetRun{provider: provider, plan: plan}
	workload := &workspace.Project{
		ID: "/svc/alpha", Name: "svc/alpha", Path: "svc/alpha",
		Metadata: releaseSetProjectMetadata(t, plan.Members[0].Ecosystem, plan.Members[0].Coordinate),
	}
	ext := &modelextension.ExtensionDescription{Name: "@putnami/go"}
	publish := &ScheduledJob{Project: workload, Extension: ext, JobDef: &modelextension.JobDefinition{
		Name: "publish~docker", CommandName: "publish", ExtensionName: ext.Name, Kind: "docker-publish",
	}}
	deploy := &ScheduledJob{Project: workload, Extension: ext, JobDef: &modelextension.JobDefinition{
		Name: "deploy~cloud", CommandName: "deploy", ExtensionName: ext.Name,
		BoundParams: modelextension.ParamMap{DeployTargetParamName: &DeployTarget{
			Environment: "prod",
			Workloads:   []DeployWorkloadTarget{{Project: workload.ID}},
		}},
	}, DependsOn: []string{publish.Key()}}
	planned, internal, err := run.AttachBarrier([]*ScheduledJob{publish, deploy})
	if err != nil || len(planned) != 3 {
		t.Fatalf("AttachBarrier = %d jobs, err=%v", len(planned), err)
	}
	result := internal[releaseSetResultKey](context.Background(), map[string]*JobResult{
		publish.Key(): {Status: "success"},
	})
	if result == nil || result.Status != "success" {
		t.Fatalf("release-set barrier result = %+v", result)
	}
	target, bound := deployTargetOf(deploy)
	if !bound || len(target.Workloads) != 1 {
		t.Fatalf("completed deploy contract = %+v, bound=%v", target, bound)
	}
	completed := target.Workloads[0]
	if completed.Project != workload.ID || completed.ReleaseSet.ID == "" || len(completed.Members) != 1 {
		t.Fatalf("completed workload contract = %+v", completed)
	}
	if completed.Members[0].Coordinate != plan.Members[0].Coordinate ||
		completed.Members[0].ArtifactDigest != plan.Members[0].ArtifactDigest {
		t.Fatalf("deploy member = %+v, want the released record", completed.Members[0])
	}
}

// TestReleaseSetBarrierFailsWhenAWorkloadIsNotInTheReleasedSet keeps the
// barrier fail-closed: a deploy whose workload the release never carried is a
// convergence on nothing, and it is refused after the release rather than
// handed an empty contract.
func TestReleaseSetBarrierFailsWhenAWorkloadIsNotInTheReleasedSet(t *testing.T) {
	provider := &fakeReleaseSetProvider{releaseOutcome: distribution.ReleaseOutcomeAlreadyCurrent}
	run := &ReleaseSetRun{provider: provider, plan: releaseSetE2ENoImpactPlan(t)}
	workload := &workspace.Project{
		ID: "/svc/alpha", Name: "svc/alpha", Path: "svc/alpha",
		Metadata: releaseSetProjectMetadata(t, "oci", "putnami/never-published"),
	}
	ext := &modelextension.ExtensionDescription{Name: "@putnami/go"}
	publish := &ScheduledJob{Project: workload, Extension: ext, JobDef: &modelextension.JobDefinition{
		Name: "publish~docker", CommandName: "publish", ExtensionName: ext.Name, Kind: "docker-publish",
	}}
	deploy := &ScheduledJob{Project: workload, Extension: ext, JobDef: &modelextension.JobDefinition{
		Name: "deploy~cloud", CommandName: "deploy", ExtensionName: ext.Name,
		BoundParams: modelextension.ParamMap{DeployTargetParamName: &DeployTarget{
			Environment: "prod",
			Workloads:   []DeployWorkloadTarget{{Project: workload.ID}},
		}},
	}, DependsOn: []string{publish.Key()}}
	_, internal, err := run.AttachBarrier([]*ScheduledJob{publish, deploy})
	if err != nil {
		t.Fatal(err)
	}
	result := internal[releaseSetResultKey](context.Background(), map[string]*JobResult{
		publish.Key(): {Status: "success"},
	})
	if result == nil || result.Status != "failed" {
		t.Fatalf("unpublished workload result = %+v, want a failed barrier", result)
	}
}

func TestReleaseSetE2ESameSessionNoImpactConfirmsExactHeadBeforeDeploy(t *testing.T) {
	ctx := context.Background()
	ledger := newReleaseSetE2ELedger()
	useReleaseSetE2EProvider(t, ledger)
	useReleaseSetProvenance(t, nil)
	ws, upstream, downstream := releaseSetE2EWorkspace(t, "1.0.0")
	head, _ := releaseSetE2EBootstrapWithWorkspace(t, ctx, ledger, ws, upstream, downstream)

	root := ws.Root
	marker := filepath.Join(root, "release-set-confirmed")
	provider := &releaseSetBoundaryProvider{releaseSetE2ELedger: ledger, marker: marker}
	useReleaseSetProviderFake(t, provider)
	run, err := PrepareReleaseSet(ctx, releaseSetE2EImpactedOptions(), ws, nil, releaseSetE2EDiscovery(), releaseSetTestFingerprints(t, ws))
	if err != nil || run == nil || !run.NoImpact() {
		t.Fatalf("prepare no-impact same-session release set = %+v, %v", run, err)
	}

	ext := &modelextension.ExtensionDescription{Name: "@test/release-boundary", Path: root}
	publish := &ScheduledJob{Project: upstream, Extension: ext, JobDef: &modelextension.JobDefinition{
		Name: "publish~noop", CommandName: "publish", ExtensionName: ext.Name,
	}}
	fixtureTask(t, publish.JobDef, fixtureScript{{"print", releaseSetSuccessLine}})
	deploy := &ScheduledJob{Project: upstream, Extension: ext, JobDef: &modelextension.JobDefinition{
		Name: "deploy~cloud", CommandName: "deploy", ExtensionName: ext.Name,
	}, DependsOn: []string{publish.Key()}}
	fixtureTask(t, deploy.JobDef, fixtureScript{{"exit-unless", "1", "nonempty:" + marker}, {"print", releaseSetSuccessLine}})
	planned, internalJobs, err := run.AttachBarrier([]*ScheduledJob{publish, deploy})
	if err != nil {
		t.Fatalf("attach no-impact release-set barrier: %v", err)
	}
	result := RunPlan(ctx, RunRequest{
		Workspace: ws, Plan: planned, InternalJobs: internalJobs,
		Config: SchedulerConfig{MaxParallel: 1, NoCache: true}, Renderer: &mockRenderer{},
	})
	if !result.Success || result.Results[deploy.Key()] == nil || result.Results[deploy.Key()].Status != "success" {
		t.Fatalf("no-impact deploy boundary = success:%v deploy:%+v releaseSet:%+v",
			result.Success, result.Results[deploy.Key()], result.Results[releaseSetResultKey])
	}
	requests := ledger.releaseRequestsSnapshot()
	last := requests[len(requests)-1]
	lastRef, _ := distribution.DeriveReleaseSetRef(&last.ReleaseSet)
	if len(last.Channels) != 1 || last.Channels[0].Expected == nil || *last.Channels[0].Expected != head || lastRef != head {
		t.Fatalf("no-impact confirmation = %+v, want expected=set=%+v", last, head)
	}
	outcomes := ledger.releaseOutcomesSnapshot()
	if outcomes[len(outcomes)-1] != distribution.ReleaseOutcomeAlreadyCurrent {
		t.Fatalf("no-impact confirmation outcome = %v, want already-current", outcomes[len(outcomes)-1])
	}
}

func TestReleaseSetE2EDownstreamOnlyKeepsUpstreamArtifactUnpublished(t *testing.T) {
	ctx := context.Background()
	ledger := newReleaseSetE2ELedger()
	useReleaseSetE2EProvider(t, ledger)
	useReleaseSetProvenance(t, nil)
	bootstrapRef, bootstrapSet := releaseSetE2EBootstrap(t, ctx, ledger)
	resolvesAfterBootstrap := ledger.counts().resolve

	// Only the downstream tree changed since the head.
	useReleaseSetProvenance(t, map[string]string{"/downstream": releaseSetE2EDigest('9')})
	ws, upstream, downstream := releaseSetE2EWorkspace(t, "1.1.0")
	run := releaseSetE2EPrepare(t, ctx, releaseSetE2EImpactedOptions(), ws, []*workspace.Project{downstream})
	if got := ledger.counts().resolve - resolvesAfterBootstrap; got != 1 {
		t.Fatalf("impacted publish resolved channel %d time(s), want exactly once", got)
	}
	if run.HeadRef() == nil || *run.HeadRef() != bootstrapRef {
		t.Fatalf("plan base ref = %+v, want %+v", run.HeadRef(), bootstrapRef)
	}

	plannedUpstream, ok := run.plan.Member(distribution.Ecosystem("npm"), upstream.Name)
	if !ok || plannedUpstream.Selected {
		t.Fatalf("planned upstream = %+v, %v", plannedUpstream, ok)
	}
	oldUpstream := releaseSetE2EMember(t, bootstrapSet, upstream.Name)
	if !releaseSetE2EMembersEqual(releaseSetE2EProtocolMember(plannedUpstream), oldUpstream) {
		t.Fatalf("unchanged upstream plan = %+v, want exact head record %+v", plannedUpstream, oldUpstream)
	}
	plannedDownstream, ok := run.plan.Member(distribution.Ecosystem("npm"), downstream.Name)
	if !ok || !plannedDownstream.Selected || len(plannedDownstream.Dependencies) != 1 ||
		plannedDownstream.Dependencies[0].Version != oldUpstream.Version || plannedDownstream.SelectionFingerprint != releaseSetE2EDigest('9') {
		t.Fatalf("planned downstream = %+v; old upstream = %+v", plannedDownstream, oldUpstream)
	}

	results, registry := releaseSetE2EPublish(run.plan, '2', nil)
	if registry.packageCount(upstream.Name) != 0 || registry.uploadCount(upstream.Name) != 0 {
		t.Fatalf("unchanged upstream was packaged/uploaded: %+v", registry)
	}
	if registry.packageCount(downstream.Name) != 1 || registry.uploadCount(downstream.Name) != 1 {
		t.Fatalf("downstream registry activity = %+v", registry)
	}
	run.Finalizer(ctx)(results)
	outcome := releaseSetE2EOutcome(t, results)
	finalSet := ledger.mustReleaseSet(t, outcome.Ref)
	finalUpstream := releaseSetE2EMember(t, finalSet, upstream.Name)
	finalDownstream := releaseSetE2EMember(t, finalSet, downstream.Name)
	if !releaseSetE2EMembersEqual(finalUpstream, oldUpstream) {
		t.Fatalf("published upstream changed: got %+v, want %+v", finalUpstream, oldUpstream)
	}
	if len(finalDownstream.Dependencies) != 1 || finalDownstream.Dependencies[0].Version != oldUpstream.Version {
		t.Fatalf("published downstream dependency = %+v, want upstream %q", finalDownstream.Dependencies, oldUpstream.Version)
	}

	// The same tree published again is a no-op: the head now records the
	// downstream fingerprint that was just released.
	again := releaseSetE2EPrepare(t, ctx, releaseSetE2EImpactedOptions(), ws, nil)
	if !again.NoImpact() {
		t.Fatalf("second publish of the same tree selected %+v", again.plan.SelectedMembers())
	}
}

// TestReleaseSetE2EAttributionSurvivesAPartialRepublication is the consumer's
// question, asked of two consecutive publications: a consumer holding only the
// head must be able to say which member is a workload's image and which project
// produced each member — including after a run that republished one project and
// inherited everything else.
func TestReleaseSetE2EAttributionSurvivesAPartialRepublication(t *testing.T) {
	ctx := context.Background()
	ledger := newReleaseSetE2ELedger()
	useReleaseSetE2EProvider(t, ledger)
	useReleaseSetProvenance(t, nil)

	ws, library, service := releaseSetE2EAttributedWorkspace(t, "1.0.0")
	attributed := releaseSetE2EAllOptions()
	attributed.Policy = attributionPolicy()
	run := releaseSetE2EPrepare(t, ctx, attributed, ws, []*workspace.Project{library, service})
	results, _ := releaseSetE2EPublish(run.plan, 'a', nil)
	run.Finalizer(ctx)(results)
	first := ledger.mustReleaseSet(t, releaseSetE2EOutcome(t, results).Ref)
	firstImage := releaseSetE2EAttributedMember(t, first, "oci", "putnami/service")
	firstLibrary := releaseSetE2EAttributedMember(t, first, "npm", library.Name)
	if firstImage.Project != "apps/service" || firstImage.Kind != distribution.KindImage {
		t.Fatalf("published image = %+v; want project apps/service and kind image", firstImage)
	}
	if firstLibrary.Project != "typescript/framework/web" || firstLibrary.Kind != distribution.KindLibrary {
		t.Fatalf("published library = %+v; want project typescript/framework/web and kind library", firstLibrary)
	}

	// Only the service tree moved. The library member is inherited from the
	// head, and inheritance is verbatim: it keeps the attribution the previous
	// publication recorded, so a consumer never sees a member lose its project.
	useReleaseSetProvenance(t, map[string]string{"/apps/service": releaseSetE2EDigest('9')})
	next, _, nextService := releaseSetE2EAttributedWorkspace(t, "1.1.0")
	impacted := releaseSetE2EImpactedOptions()
	impacted.Policy = attributionPolicy()
	second := releaseSetE2EPrepare(t, ctx, impacted, next, []*workspace.Project{nextService})
	plannedLibrary, ok := second.plan.Member(distribution.Ecosystem("npm"), library.Name)
	if !ok || plannedLibrary.Selected {
		t.Fatalf("planned library = %+v, %v; want it inherited", plannedLibrary, ok)
	}
	if !releaseSetE2EMembersEqual(releaseSetE2EProtocolMember(plannedLibrary), firstLibrary) {
		t.Fatalf("inherited library = %+v, want the head record %+v", plannedLibrary, firstLibrary)
	}

	secondResults, registry := releaseSetE2EPublish(second.plan, 'b', nil)
	if registry.packageCount(library.Name) != 0 || registry.packageCount("putnami/service") != 1 {
		t.Fatalf("partial republication activity = %+v", registry)
	}
	second.Finalizer(ctx)(secondResults)
	head := ledger.mustReleaseSet(t, releaseSetE2EOutcome(t, secondResults).Ref)
	inherited := releaseSetE2EAttributedMember(t, head, "npm", library.Name)
	republished := releaseSetE2EAttributedMember(t, head, "oci", "putnami/service")
	if !releaseSetE2EMembersEqual(inherited, firstLibrary) {
		t.Fatalf("inherited library changed: %+v, want %+v", inherited, firstLibrary)
	}
	if republished.Version != "1.1.0" || republished.Project != "apps/service" || republished.Kind != distribution.KindImage {
		t.Fatalf("republished image = %+v; want 1.1.0 attributed to apps/service as an image", republished)
	}
	for _, member := range head.Members {
		if member.Project == "" || member.Kind == "" {
			t.Fatalf("member %+v reached the head without attribution", member)
		}
	}
	canonical, diagnostics := distribution.CanonicalReleaseSetBytes(&head)
	if len(diagnostics) != 0 {
		t.Fatalf("attributed head is not canonicalizable: %v", diagnostics)
	}
	t.Logf("attributed head: %s", canonical)
}

// releaseSetE2EAttributedWorkspace is the two-project shape the consumer plan
// groups by: a framework library published to npm, and a workload whose image
// is published to the OCI registry.
func releaseSetE2EAttributedWorkspace(t *testing.T, version string) (*workspace.Workspace, *workspace.Project, *workspace.Project) {
	t.Helper()
	library := &workspace.Project{
		ID: "/typescript/framework/web", Name: "@putnami/web", Version: version, Type: "library", Publish: []string{"npm"},
		Metadata: releaseSetProjectMetadata(t, "npm", "@putnami/web"),
	}
	service := &workspace.Project{
		ID: "/apps/service", Name: "service", Version: version, Type: "application", Publish: []string{"docker"},
		Metadata: releaseSetProjectMembers(t, releaseset.MemberDeclaration{
			Ecosystem: "oci", Coordinate: "putnami/service", PackageStep: "docker", PublishStep: "docker",
		}),
	}
	ws := workspace.NewWorkspace(t.TempDir(), &wsproto.Config{Name: "putnami"}, []*workspace.Project{library, service})
	return ws, library, service
}

func releaseSetE2EAttributedMember(t *testing.T, set distribution.ReleaseSet, ecosystem distribution.Ecosystem, coordinate string) distribution.ReleaseSetMember {
	t.Helper()
	for _, member := range set.Members {
		if member.Ecosystem == ecosystem && member.Coordinate == coordinate {
			return member
		}
	}
	t.Fatalf("member %s/%s is absent from %+v", ecosystem, coordinate, set)
	return distribution.ReleaseSetMember{}
}

func TestReleaseSetE2EUpstreamImpactRepackagesDownstream(t *testing.T) {
	ctx := context.Background()
	ledger := newReleaseSetE2ELedger()
	useReleaseSetE2EProvider(t, ledger)
	useReleaseSetProvenance(t, nil)
	releaseSetE2EBootstrap(t, ctx, ledger)

	useReleaseSetProvenance(t, map[string]string{"/upstream": releaseSetE2EDigest('9')})
	ws, upstream, downstream := releaseSetE2EWorkspace(t, "1.1.0")
	// The git-derived selection names only the upstream; the head requires the
	// dependent too and the plan widens to it.
	run := releaseSetE2EPrepare(t, ctx, releaseSetE2EImpactedOptions(), ws, []*workspace.Project{upstream})
	plannedUpstream, _ := run.plan.Member(distribution.Ecosystem("npm"), upstream.Name)
	plannedDownstream, _ := run.plan.Member(distribution.Ecosystem("npm"), downstream.Name)
	if !plannedUpstream.Selected || !plannedDownstream.Selected || len(plannedDownstream.Dependencies) != 1 ||
		plannedDownstream.Dependencies[0].Version != plannedUpstream.Version {
		t.Fatalf("repackage plan upstream=%+v downstream=%+v", plannedUpstream, plannedDownstream)
	}

	results, registry := releaseSetE2EPublish(run.plan, '3', nil)
	run.Finalizer(ctx)(results)
	outcome := releaseSetE2EOutcome(t, results)
	for _, coordinate := range []string{upstream.Name, downstream.Name} {
		if registry.packageCount(coordinate) != 1 || registry.uploadCount(coordinate) != 1 {
			t.Fatalf("%s registry activity = %+v", coordinate, registry)
		}
	}
	finalSet := ledger.mustReleaseSet(t, outcome.Ref)
	finalUpstream := releaseSetE2EMember(t, finalSet, upstream.Name)
	finalDownstream := releaseSetE2EMember(t, finalSet, downstream.Name)
	if len(finalDownstream.Dependencies) != 1 || finalDownstream.Dependencies[0].Version != finalUpstream.Version {
		t.Fatalf("final downstream dependency = %+v; upstream = %+v", finalDownstream.Dependencies, finalUpstream)
	}
}

func TestReleaseSetE2ENoImpactAndPartialPublicationFailClosed(t *testing.T) {
	ctx := context.Background()
	ledger := newReleaseSetE2ELedger()
	factoryCalls := useReleaseSetE2EProvider(t, ledger)
	useReleaseSetProvenance(t, nil)
	ws, upstream, downstream := releaseSetE2EWorkspace(t, "1.0.0")
	releaseSetE2EBootstrapWithWorkspace(t, ctx, ledger, ws, upstream, downstream)
	headBeforeNoImpact, _ := ledger.head("putnami", "canary")
	countsBeforeNoImpact := ledger.counts()

	run, err := PrepareReleaseSet(ctx, releaseSetE2EImpactedOptions(), ws, nil, releaseSetE2EDiscovery(), releaseSetTestFingerprints(t, ws))
	if err != nil || run == nil || !run.NoImpact() || run.Plan() == nil {
		t.Fatalf("no-impact preparation = %+v, %v; want a plan that selects nothing", run, err)
	}
	noImpactResults := map[string]*JobResult{}
	run.Finalizer(ctx)(noImpactResults)
	noImpactOutcome := releaseSetE2EOutcome(t, noImpactResults)
	countsAfterNoImpact := ledger.counts()
	headAfterNoImpact, _ := ledger.head("putnami", "canary")
	if *factoryCalls != 2 || countsAfterNoImpact.resolve != countsBeforeNoImpact.resolve+1 ||
		countsAfterNoImpact.release != countsBeforeNoImpact.release+1 ||
		noImpactOutcome.Ref != headBeforeNoImpact || headAfterNoImpact != headBeforeNoImpact {
		t.Fatalf("no-impact evidence/mutations: factory=%d outcome=%+v counts=%+v -> %+v head=%+v -> %+v",
			*factoryCalls, noImpactOutcome, countsBeforeNoImpact, countsAfterNoImpact, headBeforeNoImpact, headAfterNoImpact)
	}

	useReleaseSetProvenance(t, map[string]string{"/upstream": releaseSetE2EDigest('9')})
	ws, upstream, downstream = releaseSetE2EWorkspace(t, "1.1.0")
	run = releaseSetE2EPrepare(t, ctx, releaseSetE2EImpactedOptions(), ws, []*workspace.Project{upstream, downstream})
	headBefore, _ := ledger.head("putnami", "canary")
	countsBefore := ledger.counts()
	results, registry := releaseSetE2EPublish(run.plan, '4', map[string]bool{downstream.Name: true})
	run.Finalizer(ctx)(results)
	assertReleaseSetFailure(t, results)
	if registry.packageCount(upstream.Name) != 1 || registry.uploadCount(upstream.Name) != 1 ||
		registry.packageCount(downstream.Name) != 1 || registry.uploadCount(downstream.Name) != 0 {
		t.Fatalf("partial registry activity = %+v", registry)
	}
	headAfter, _ := ledger.head("putnami", "canary")
	countsAfter := ledger.counts()
	if headAfter != headBefore || countsAfter.release != countsBefore.release {
		t.Fatalf("partial publish mutated provider: head %v -> %v, counts %+v -> %+v", headBefore, headAfter, countsBefore, countsAfter)
	}
}

func TestReleaseSetE2ERepeatedAllPublishReleasesFromExistingHead(t *testing.T) {
	ctx := context.Background()
	ledger := newReleaseSetE2ELedger()
	useReleaseSetE2EProvider(t, ledger)
	useReleaseSetProvenance(t, nil)
	bootstrapRef, _ := releaseSetE2EBootstrap(t, ctx, ledger)
	resolvesAfterBootstrap := ledger.counts().resolve

	// The next main commit publishes everything again. The channel already
	// has a head: the plan resolves that head exactly once, repackages every
	// member, and releases by CAS from it.
	ws, upstream, downstream := releaseSetE2EWorkspace(t, "1.0.1")
	run := releaseSetE2EPrepare(t, ctx, releaseSetE2EAllOptions(), ws, []*workspace.Project{upstream, downstream})
	if run.HeadRef() == nil || *run.HeadRef() != bootstrapRef || run.plan.Baseline() == nil {
		t.Fatalf("second full plan = %+v; want the first head as its CAS base", run.plan)
	}
	if got := ledger.counts().resolve - resolvesAfterBootstrap; got != 1 {
		t.Fatalf("second full publish resolved the channel %d time(s), want exactly once", got)
	}
	if !run.plan.SelectsEveryMember() || len(run.plan.Members) != 2 {
		t.Fatalf("second full plan members = %+v; want every member selected", run.plan.Members)
	}

	results, registry := releaseSetE2EPublish(run.plan, '2', nil)
	run.Finalizer(ctx)(results)
	outcome := releaseSetE2EOutcome(t, results)
	if outcome.Ref == bootstrapRef {
		t.Fatal("second full publish reported the first ref as its outcome")
	}
	if registry.packageCount(upstream.Name) != 1 || registry.uploadCount(upstream.Name) != 1 ||
		registry.packageCount(downstream.Name) != 1 || registry.uploadCount(downstream.Name) != 1 {
		t.Fatalf("second full publish registry activity = %+v", registry)
	}
	if head, ok := ledger.head("putnami", "canary"); !ok || head != outcome.Ref {
		t.Fatalf("channel head = %+v, %v; outcome = %+v", head, ok, outcome.Ref)
	}
	requests := ledger.releaseRequestsSnapshot()
	if len(requests) != 2 || requests[0].Channels[0].Expected != nil ||
		requests[1].Channels[0].Expected == nil || *requests[1].Channels[0].Expected != bootstrapRef {
		t.Fatalf("release requests = %+v; want [expected:null, expected:%s]", requests, bootstrapRef.ID)
	}
	published := ledger.mustReleaseSet(t, outcome.Ref)
	for _, member := range published.Members {
		if member.Version != "1.0.1" {
			t.Fatalf("published member %+v; want every member at the new full version", member)
		}
	}

	// A third full publish from the same tree keeps releasing from the new head.
	thirdWorkspace, thirdUpstream, thirdDownstream := releaseSetE2EWorkspace(t, "1.0.2")
	third := releaseSetE2EPrepare(t, ctx, releaseSetE2EAllOptions(), thirdWorkspace, []*workspace.Project{thirdUpstream, thirdDownstream})
	if third.HeadRef() == nil || *third.HeadRef() != outcome.Ref {
		t.Fatalf("third full plan base = %+v, want %+v", third.HeadRef(), outcome.Ref)
	}
}

// D14: acceptance is atomic across every listed channel. A publication naming
// two channels, one of which another writer moved between the resolve and the
// release, writes NOTHING — not the set, not the channel that was still where
// the plan left it — and the failure names every head that moved.
func TestConflictOnOneChannelWritesNothing(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "release-set-channel-advance", "a-conflict-on-one-channel-writes-nothing")
	ctx := context.Background()
	ledger := newReleaseSetE2ELedger()
	useReleaseSetE2EProvider(t, ledger)
	useReleaseSetProvenance(t, nil)
	bootstrapRef, _ := releaseSetE2EBootstrap(t, ctx, ledger)

	// stable starts where canary is; both are resolved by one publication.
	ledger.setHead("putnami", "stable", bootstrapRef)

	useReleaseSetProvenance(t, map[string]string{"/downstream": releaseSetE2EDigest('9')})
	ws, _, downstream := releaseSetE2EWorkspace(t, "1.1.0")
	run := releaseSetE2EPrepare(t, ctx, releaseSetE2EImpactedOptions("canary", "stable"), ws, []*workspace.Project{downstream})
	if len(run.plan.Channels) != 2 || run.plan.Heads["stable"] == nil {
		t.Fatalf("two-channel plan = %+v", run.plan)
	}

	// Another writer moves stable alone, after the resolve.
	otherWorkspace, otherUpstream, otherDownstream := releaseSetE2EWorkspace(t, "1.0.5")
	moved, _ := releaseSetE2EBootstrapWithWorkspace(t, ctx, ledger, otherWorkspace, otherUpstream, otherDownstream)
	ledger.setHead("putnami", "stable", moved)
	canaryBefore, _ := ledger.head("putnami", "canary")

	results, _ := releaseSetE2EPublish(run.plan, '7', nil)
	run.Finalizer(ctx)(results)

	result := results[releaseSetResultKey]
	if result == nil || result.Status != "failed" || result.Data != nil {
		t.Fatalf("conflicting two-channel release = %+v", result)
	}
	if !strings.Contains(result.Error.Message, "stable: expected "+bootstrapRef.ID) ||
		!strings.Contains(result.Error.Message, "observed "+moved.ID) {
		t.Fatalf("conflict message = %q; want it to name the head that moved", result.Error.Message)
	}
	if head, _ := ledger.head("putnami", "canary"); head != canaryBefore {
		t.Fatalf("canary advanced despite a conflict on stable: %+v -> %+v", canaryBefore, head)
	}
	if head, _ := ledger.head("putnami", "stable"); head != moved {
		t.Fatalf("stable head = %+v, want the other writer's %+v", head, moved)
	}
}

func TestReleaseSetE2ETwoConcurrentAllWritersHaveOneCASWinner(t *testing.T) {
	ctx := context.Background()
	ledger := newReleaseSetE2ELedger()
	useReleaseSetE2EProvider(t, ledger)
	useReleaseSetProvenance(t, nil)
	bootstrapRef, _ := releaseSetE2EBootstrap(t, ctx, ledger)

	firstWorkspace, firstUpstream, firstDownstream := releaseSetE2EWorkspace(t, "1.1.1")
	secondWorkspace, secondUpstream, secondDownstream := releaseSetE2EWorkspace(t, "1.1.2")
	firstRun := releaseSetE2EPrepare(t, ctx, releaseSetE2EAllOptions(), firstWorkspace, []*workspace.Project{firstUpstream, firstDownstream})
	secondRun := releaseSetE2EPrepare(t, ctx, releaseSetE2EAllOptions(), secondWorkspace, []*workspace.Project{secondUpstream, secondDownstream})
	firstResults, _ := releaseSetE2EPublish(firstRun.plan, '7', nil)
	secondResults, _ := releaseSetE2EPublish(secondRun.plan, '8', nil)

	start := make(chan struct{})
	var ready, done sync.WaitGroup
	ready.Add(2)
	done.Add(2)
	finalize := func(run *ReleaseSetRun, results map[string]*JobResult) {
		defer done.Done()
		ready.Done()
		<-start
		run.Finalizer(ctx)(results)
	}
	go finalize(firstRun, firstResults)
	go finalize(secondRun, secondResults)
	ready.Wait()
	close(start)
	done.Wait()

	successes, failures := 0, 0
	var winner distribution.ReleaseSetRef
	for _, results := range []map[string]*JobResult{firstResults, secondResults} {
		result := results[releaseSetResultKey]
		switch {
		case result != nil && result.Status == "success":
			successes++
			winner = releaseSetE2EOutcome(t, results).Ref
		case result != nil && result.Status == "failed" && result.Data == nil:
			failures++
			if !strings.Contains(result.Error.Message, "canary: expected "+bootstrapRef.ID) {
				t.Fatalf("losing full writer message = %q; want the resolved head it expected", result.Error.Message)
			}
		default:
			t.Fatalf("concurrent full writer result = %+v", result)
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("concurrent full outcomes: success=%d failure=%d", successes, failures)
	}
	if head, ok := ledger.head("putnami", "canary"); !ok || head != winner || head == bootstrapRef {
		t.Fatalf("CAS head = %+v, %v; winner = %+v", head, ok, winner)
	}
}

func TestReleaseSetE2ETwoConcurrentImpactedWritersHaveOneCASWinner(t *testing.T) {
	ctx := context.Background()
	ledger := newReleaseSetE2ELedger()
	useReleaseSetE2EProvider(t, ledger)
	useReleaseSetProvenance(t, nil)
	releaseSetE2EBootstrap(t, ctx, ledger)

	useReleaseSetProvenance(t, map[string]string{"/downstream": releaseSetE2EDigest('9')})
	firstWorkspace, _, firstDownstream := releaseSetE2EWorkspace(t, "1.1.1")
	secondWorkspace, _, secondDownstream := releaseSetE2EWorkspace(t, "1.1.2")
	firstRun := releaseSetE2EPrepare(t, ctx, releaseSetE2EImpactedOptions(), firstWorkspace, []*workspace.Project{firstDownstream})
	secondRun := releaseSetE2EPrepare(t, ctx, releaseSetE2EImpactedOptions(), secondWorkspace, []*workspace.Project{secondDownstream})
	firstResults, _ := releaseSetE2EPublish(firstRun.plan, '5', nil)
	secondResults, _ := releaseSetE2EPublish(secondRun.plan, '6', nil)

	start := make(chan struct{})
	var ready, done sync.WaitGroup
	ready.Add(2)
	done.Add(2)
	finalize := func(run *ReleaseSetRun, results map[string]*JobResult) {
		defer done.Done()
		ready.Done()
		<-start
		run.Finalizer(ctx)(results)
	}
	go finalize(firstRun, firstResults)
	go finalize(secondRun, secondResults)
	ready.Wait()
	close(start)
	done.Wait()

	successes, failures := 0, 0
	var winner distribution.ReleaseSetRef
	for _, results := range []map[string]*JobResult{firstResults, secondResults} {
		result := results[releaseSetResultKey]
		switch {
		case result != nil && result.Status == "success":
			successes++
			winner = releaseSetE2EOutcome(t, results).Ref
		case result != nil && result.Status == "failed" && result.Data == nil:
			failures++
		default:
			t.Fatalf("concurrent writer result = %+v", result)
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("concurrent outcomes: success=%d failure=%d", successes, failures)
	}
	if head, ok := ledger.head("putnami", "canary"); !ok || head != winner {
		t.Fatalf("CAS head = %+v, %v; winner = %+v", head, ok, winner)
	}
	outcomes := ledger.releaseOutcomesSnapshot()
	if len(outcomes) != 3 || releaseSetE2ECountOutcome(outcomes[1:], distribution.ReleaseOutcomeReleased) != 1 ||
		releaseSetE2ECountOutcome(outcomes[1:], distribution.ReleaseOutcomeConflict) != 1 {
		t.Fatalf("concurrent CAS outcomes = %v", outcomes)
	}
}

func TestReleaseSetE2EPublishOutcomeRemainsPinnedWhenChannelMoves(t *testing.T) {
	ctx := context.Background()
	ledger := newReleaseSetE2ELedger()
	useReleaseSetE2EProvider(t, ledger)
	useReleaseSetProvenance(t, nil)
	releaseSetE2EBootstrap(t, ctx, ledger)

	useReleaseSetProvenance(t, map[string]string{"/downstream": releaseSetE2EDigest('9')})
	ws, _, downstream := releaseSetE2EWorkspace(t, "1.1.0")
	run := releaseSetE2EPrepare(t, ctx, releaseSetE2EImpactedOptions(), ws, []*workspace.Project{downstream})
	results, _ := releaseSetE2EPublish(run.plan, '7', nil)
	run.Finalizer(ctx)(results)
	publishOutcome := releaseSetE2EOutcome(t, results)
	handoffRef := publishOutcome.Ref
	resolveCallsAtHandoff := ledger.counts().resolve

	// Model a later writer moving canary. The deployment consumer itself is
	// Cloud-owned; Putnami's contract is the captured handoff ref, never another
	// channel resolution after this point.
	movedSet := ledger.mustReleaseSet(t, handoffRef)
	for index := range movedSet.Members {
		if movedSet.Members[index].Coordinate == downstream.Name {
			movedSet.Members[index].Version = "1.2.0"
			movedSet.Members[index].ArtifactDigest = releaseSetE2EDigest('8')
		}
	}
	movedSet = *distribution.NormalizeReleaseSet(&movedSet)
	releaseResponse, err := ledger.Release(ctx, &distribution.ReleaseRequest{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		ReleaseSet:      movedSet,
		Channels: []distribution.ChannelRequest{
			{Name: "canary", Expected: &handoffRef, Visibility: distribution.VisibilityInternal},
		},
		Visibility: distribution.VisibilityChain{Repo: distribution.VisibilityInternal},
	})
	if err != nil || releaseResponse.Outcome != distribution.ReleaseOutcomeReleased {
		t.Fatalf("external channel move = %+v, %v", releaseResponse, err)
	}
	if head, _ := ledger.head("putnami", "canary"); head == handoffRef {
		t.Fatalf("channel did not move away from handoff ref %+v", handoffRef)
	}
	if got := releaseSetE2EOutcome(t, results).Ref; got != handoffRef {
		t.Fatalf("publish handoff changed after channel move: got %+v, want %+v", got, handoffRef)
	}
	if got := ledger.counts().resolve; got != resolveCallsAtHandoff {
		t.Fatalf("handoff re-resolved channel: resolve calls %d -> %d", resolveCallsAtHandoff, got)
	}
	if _, ok := ledger.releaseSet(handoffRef); !ok {
		t.Fatalf("immutable handoff set %s disappeared after channel move", handoffRef.ID)
	}
}

// releaseSetE2ELedger is a stateful provider: resolve answers each requested
// head or null, release stores the set and advances EVERY listed channel by
// compare-and-swap in one transaction — a conflict on any one of them writes
// nothing on any of them — and stamps a monotone generation per channel.
type releaseSetE2ELedger struct {
	mu              sync.Mutex
	sets            map[string]distribution.ReleaseSet
	heads           map[string]distribution.ReleaseSetRef
	generations     map[string]uint64
	resolveCalls    int
	releaseCalls    int
	releaseRequests []distribution.ReleaseRequest
	releaseOutcomes []distribution.ReleaseOutcome
}

type releaseSetBoundaryProvider struct {
	*releaseSetE2ELedger
	marker string
}

func (provider *releaseSetBoundaryProvider) Release(ctx context.Context, request *distribution.ReleaseRequest) (*distribution.ReleaseResponse, error) {
	response, err := provider.releaseSetE2ELedger.Release(ctx, request)
	if err != nil {
		return nil, err
	}
	if response.Outcome == distribution.ReleaseOutcomeReleased || response.Outcome == distribution.ReleaseOutcomeAlreadyCurrent {
		head := response.Current[request.Channels[0].Name]
		if err := os.WriteFile(provider.marker, []byte(head.Ref.ID+"\n"+head.Ref.Digest+"\n"), 0o600); err != nil {
			return nil, err
		}
	}
	return response, nil
}

type releaseSetE2ECounts struct {
	resolve int
	release int
}

func newReleaseSetE2ELedger() *releaseSetE2ELedger {
	return &releaseSetE2ELedger{
		sets:        make(map[string]distribution.ReleaseSet),
		heads:       make(map[string]distribution.ReleaseSetRef),
		generations: make(map[string]uint64),
	}
}

func (ledger *releaseSetE2ELedger) Resolve(_ context.Context, request *distribution.ResolveRequest) (*distribution.ResolveResponse, error) {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	ledger.resolveCalls++
	heads := make(map[string]*distribution.ChannelHead, len(request.Channels))
	for _, channel := range request.Channels {
		key := releaseSetE2EChannelKey(request.Namespace, channel)
		head, ok := ledger.heads[key]
		if !ok {
			heads[channel] = nil
			continue
		}
		set, ok := ledger.sets[head.ID]
		if !ok {
			return nil, fmt.Errorf("release-set %s is absent", head.ID)
		}
		heads[channel] = &distribution.ChannelHead{
			Ref: head, Generation: ledger.generations[key], ReleaseSet: distribution.NormalizeReleaseSet(&set),
		}
	}
	return &distribution.ResolveResponse{ProtocolVersion: distribution.ProtocolVersion, Heads: heads}, nil
}

func (ledger *releaseSetE2ELedger) Release(_ context.Context, request *distribution.ReleaseRequest) (*distribution.ReleaseResponse, error) {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	ledger.releaseCalls++
	copyRequest := *request
	copyRequest.Channels = append([]distribution.ChannelRequest(nil), request.Channels...)
	for index, channel := range copyRequest.Channels {
		if channel.Expected != nil {
			expected := *channel.Expected
			copyRequest.Channels[index].Expected = &expected
		}
	}
	ledger.releaseRequests = append(ledger.releaseRequests, copyRequest)

	set := distribution.NormalizeReleaseSet(&request.ReleaseSet)
	ref, diagnostics := distribution.DeriveReleaseSetRef(set)
	if len(diagnostics) != 0 {
		return nil, fmt.Errorf("invalid release-set fixture: %v", diagnostics)
	}

	// Acceptance is ATOMIC across every listed channel: decide first, write
	// afterwards, so a conflict on one channel leaves every other head where
	// it was.
	outcome := distribution.ReleaseOutcomeAlreadyCurrent
	for _, channel := range request.Channels {
		key := releaseSetE2EChannelKey(request.Namespace, channel.Name)
		current, exists := ledger.heads[key]
		switch {
		case exists && current == ref:
			// already at the released set
		case channel.Expected == nil && !exists, channel.Expected != nil && exists && current == *channel.Expected:
			if outcome != distribution.ReleaseOutcomeConflict {
				outcome = distribution.ReleaseOutcomeReleased
			}
		default:
			outcome = distribution.ReleaseOutcomeConflict
		}
	}
	if outcome != distribution.ReleaseOutcomeConflict {
		if _, stored := ledger.sets[ref.ID]; !stored {
			ledger.sets[ref.ID] = *set
		}
		for _, channel := range request.Channels {
			key := releaseSetE2EChannelKey(request.Namespace, channel.Name)
			if ledger.heads[key] != ref {
				ledger.heads[key] = ref
				ledger.generations[key]++
			}
		}
	}
	ledger.releaseOutcomes = append(ledger.releaseOutcomes, outcome)
	response := &distribution.ReleaseResponse{
		ProtocolVersion: distribution.ProtocolVersion,
		Outcome:         outcome,
		Current:         make(map[string]*distribution.ChannelHead, len(request.Channels)),
	}
	for _, channel := range request.Channels {
		key := releaseSetE2EChannelKey(request.Namespace, channel.Name)
		current, exists := ledger.heads[key]
		if !exists {
			response.Current[channel.Name] = nil
			continue
		}
		response.Current[channel.Name] = &distribution.ChannelHead{Ref: current, Generation: ledger.generations[key]}
	}
	return response, nil
}

func (ledger *releaseSetE2ELedger) counts() releaseSetE2ECounts {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	return releaseSetE2ECounts{resolve: ledger.resolveCalls, release: ledger.releaseCalls}
}

func (ledger *releaseSetE2ELedger) head(namespace, channel string) (distribution.ReleaseSetRef, bool) {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	ref, ok := ledger.heads[releaseSetE2EChannelKey(namespace, channel)]
	return ref, ok
}

// setHead moves a channel out of band, as another writer would.
func (ledger *releaseSetE2ELedger) setHead(namespace, channel string, ref distribution.ReleaseSetRef) {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	key := releaseSetE2EChannelKey(namespace, channel)
	ledger.heads[key] = ref
	ledger.generations[key]++
}

func (ledger *releaseSetE2ELedger) releaseSet(ref distribution.ReleaseSetRef) (distribution.ReleaseSet, bool) {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	set, ok := ledger.sets[ref.ID]
	if !ok {
		return distribution.ReleaseSet{}, false
	}
	return *distribution.NormalizeReleaseSet(&set), true
}

func (ledger *releaseSetE2ELedger) mustReleaseSet(t *testing.T, ref distribution.ReleaseSetRef) distribution.ReleaseSet {
	t.Helper()
	set, ok := ledger.releaseSet(ref)
	if !ok {
		t.Fatalf("release-set %s is absent", ref.ID)
	}
	return set
}

func (ledger *releaseSetE2ELedger) releaseRequestsSnapshot() []distribution.ReleaseRequest {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	return append([]distribution.ReleaseRequest(nil), ledger.releaseRequests...)
}

func (ledger *releaseSetE2ELedger) releaseOutcomesSnapshot() []distribution.ReleaseOutcome {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	return append([]distribution.ReleaseOutcome(nil), ledger.releaseOutcomes...)
}

type releaseSetE2ERegistry struct {
	packaged map[string][]string
	uploaded map[string][]string
}

func releaseSetE2EPublish(plan *releaseset.Plan, digestChar byte, skipUpload map[string]bool) (map[string]*JobResult, *releaseSetE2ERegistry) {
	results := make(map[string]*JobResult)
	registry := &releaseSetE2ERegistry{packaged: make(map[string][]string), uploaded: make(map[string][]string)}
	for index, member := range plan.SelectedMembers() {
		registry.packaged[member.Coordinate] = append(registry.packaged[member.Coordinate], member.Version)
		if skipUpload[member.Coordinate] {
			continue
		}
		registry.uploaded[member.Coordinate] = append(registry.uploaded[member.Coordinate], member.Version)
		digest := releaseSetE2EDigest(digestChar + byte(index))
		results[member.ProjectID+":publish"] = &JobResult{
			Status: "success",
			Events: []RawJobEvent{publishedMemberEvent(member, digest)},
		}
	}
	return results, registry
}

func (registry *releaseSetE2ERegistry) packageCount(coordinate string) int {
	return len(registry.packaged[coordinate])
}

func (registry *releaseSetE2ERegistry) uploadCount(coordinate string) int {
	return len(registry.uploaded[coordinate])
}

func releaseSetE2EPrepare(
	t *testing.T,
	ctx context.Context,
	options ReleaseSetOptions,
	ws *workspace.Workspace,
	selected []*workspace.Project,
) *ReleaseSetRun {
	t.Helper()
	if options.Versions == nil {
		options.Versions = testRunVersions(ws)
	}
	run, err := PrepareReleaseSet(ctx, options, ws, selected, releaseSetE2EDiscovery(), releaseSetTestFingerprints(t, ws))
	if err != nil || run == nil {
		t.Fatalf("prepare release-set = %+v, %v", run, err)
	}
	return run
}

func releaseSetE2EBootstrap(t *testing.T, ctx context.Context, ledger *releaseSetE2ELedger) (distribution.ReleaseSetRef, distribution.ReleaseSet) {
	t.Helper()
	ws, upstream, downstream := releaseSetE2EWorkspace(t, "1.0.0")
	return releaseSetE2EBootstrapWithWorkspace(t, ctx, ledger, ws, upstream, downstream)
}

func releaseSetE2EBootstrapWithWorkspace(
	t *testing.T,
	ctx context.Context,
	ledger *releaseSetE2ELedger,
	ws *workspace.Workspace,
	upstream, downstream *workspace.Project,
) (distribution.ReleaseSetRef, distribution.ReleaseSet) {
	t.Helper()
	run := releaseSetE2EPrepare(t, ctx, releaseSetE2EAllOptions(), ws, []*workspace.Project{upstream, downstream})
	results, _ := releaseSetE2EPublish(run.plan, 'a', nil)
	run.Finalizer(ctx)(results)
	outcome := releaseSetE2EOutcome(t, results)
	return outcome.Ref, ledger.mustReleaseSet(t, outcome.Ref)
}

// releaseSetE2ENoImpactPlan is a plan over releaseSetHead that selects
// nothing: every member's fingerprint matches the head's record.
func releaseSetE2ENoImpactPlan(t *testing.T) *releaseset.Plan {
	t.Helper()
	ws, _, _ := releaseSetWorkspace(t)
	head := releaseSetHead(t)
	plan := buildTestPlan(t, releaseSetRequest(), ws, head)
	if len(plan.SelectedMembers()) != 0 {
		t.Fatalf("no-impact fixture selected %+v", plan.SelectedMembers())
	}
	return plan
}

func releaseSetE2ENoImpactOCIPlan(t *testing.T) *releaseset.Plan {
	t.Helper()
	member := distribution.ReleaseSetMember{
		Ecosystem: "oci", Coordinate: "putnami/svc-alpha", Version: "1.0.0",
		ArtifactDigest: digestFor('a'), Dependencies: []distribution.ReleaseSetDependency{},
		SourceRevision: testRevision, SelectionFingerprint: testFingerprint("/svc/alpha"),
	}
	set := distribution.NormalizeReleaseSet(&distribution.ReleaseSet{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		Members:         []distribution.ReleaseSetMember{member},
	})
	ref, diagnostics := distribution.DeriveReleaseSetRef(set)
	if len(diagnostics) != 0 {
		t.Fatalf("derive OCI no-impact head: %v", diagnostics)
	}
	plan := &releaseset.Plan{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		Channels:        []string{"canary"},
		Heads: map[string]*distribution.ChannelHead{
			"canary": {Ref: ref, Generation: 1, ReleaseSet: set},
		},
		Members: []releaseset.PlannedMember{{
			Ecosystem: member.Ecosystem, Coordinate: member.Coordinate,
			Version: member.Version, ArtifactDigest: member.ArtifactDigest,
			Dependencies: member.Dependencies, SourceRevision: member.SourceRevision,
			SelectionFingerprint: member.SelectionFingerprint,
		}},
	}
	if err := releaseset.ValidatePlan(plan); err != nil {
		t.Fatalf("OCI no-impact plan: %v", err)
	}
	return plan
}

func releaseSetE2EOutcome(t *testing.T, results map[string]*JobResult) distribution.ReleaseSetPublishOutcome {
	t.Helper()
	result := results[releaseSetResultKey]
	if result == nil || result.Status != "success" || len(result.Data) != 1 {
		t.Fatalf("release-set outcome result = %+v", result)
	}
	raw, ok := result.Data[runtimeproto.ReleaseSetResultDataKey]
	if !ok {
		t.Fatalf("release-set outcome data = %+v", result.Data)
	}
	outcome, ok := raw.(distribution.ReleaseSetPublishOutcome)
	if !ok {
		t.Fatalf("release-set outcome type = %T", raw)
	}
	return outcome
}

func releaseSetE2EWorkspace(t *testing.T, version string) (*workspace.Workspace, *workspace.Project, *workspace.Project) {
	t.Helper()
	upstream := &workspace.Project{
		ID: "/upstream", Name: "@putnami/upstream", Version: version, Type: "library", Publish: []string{"npm"},
		Metadata: releaseSetProjectMetadata(t, "npm", "@putnami/upstream"),
	}
	downstream := &workspace.Project{
		ID: "/downstream", Name: "@putnami/downstream", Version: version, Type: "library", Publish: []string{"npm"},
		Dependencies: []string{upstream.Name},
		Metadata:     releaseSetProjectMetadata(t, "npm", "@putnami/downstream", upstream.Name),
	}
	ws := workspace.NewWorkspace(t.TempDir(), &wsproto.Config{Name: "putnami"}, []*workspace.Project{upstream, downstream})
	return ws, upstream, downstream
}

func releaseSetE2EMember(t *testing.T, set distribution.ReleaseSet, coordinate string) distribution.ReleaseSetMember {
	t.Helper()
	for _, member := range set.Members {
		if member.Ecosystem == distribution.Ecosystem("npm") && member.Coordinate == coordinate {
			return member
		}
	}
	t.Fatalf("member npm/%s is absent from %+v", coordinate, set)
	return distribution.ReleaseSetMember{}
}

func releaseSetE2EProtocolMember(member releaseset.PlannedMember) distribution.ReleaseSetMember {
	return distribution.ReleaseSetMember{
		Ecosystem: member.Ecosystem, Coordinate: member.Coordinate, Version: member.Version,
		ArtifactDigest:       member.ArtifactDigest,
		Dependencies:         append([]distribution.ReleaseSetDependency(nil), member.Dependencies...),
		SourceRevision:       member.SourceRevision,
		SelectionFingerprint: member.SelectionFingerprint,
		Platforms:            maps.Clone(member.Platforms),
		Project:              member.Project,
		Kind:                 member.Kind,
	}
}

func releaseSetE2EMembersEqual(first, second distribution.ReleaseSetMember) bool {
	return first.Ecosystem == second.Ecosystem && first.Coordinate == second.Coordinate &&
		first.Version == second.Version && first.ArtifactDigest == second.ArtifactDigest &&
		slices.Equal(first.Dependencies, second.Dependencies) &&
		first.SourceRevision == second.SourceRevision && first.SelectionFingerprint == second.SelectionFingerprint &&
		maps.Equal(first.Platforms, second.Platforms) &&
		first.Project == second.Project && first.Kind == second.Kind
}

func useReleaseSetE2EProvider(t *testing.T, ledger *releaseSetE2ELedger) *int {
	t.Helper()
	previousFactory := newReleaseSetProvider
	factoryCalls := 0
	newReleaseSetProvider = func(context.Context, *extension.ResolvedProvider) (releaseSetProvider, error) {
		factoryCalls++
		return ledger, nil
	}
	t.Cleanup(func() { newReleaseSetProvider = previousFactory })
	return &factoryCalls
}

func releaseSetE2EDiscovery() *extension.DiscoveryResult {
	return &extension.DiscoveryResult{Extensions: []*modelextension.ExtensionDescription{{
		Name: "cloud", Commands: map[string]string{distribution.ProviderCommandName: "release sets"},
	}}}
}

func releaseSetE2EImpactedOptions(channels ...string) ReleaseSetOptions {
	if len(channels) == 0 {
		channels = []string{"canary"}
	}
	return ReleaseSetOptions{Commands: []string{"publish"}, Impacted: true, Channels: channels, Profiles: testProfiles()}
}

func releaseSetE2EAllOptions(channels ...string) ReleaseSetOptions {
	if len(channels) == 0 {
		channels = []string{"canary"}
	}
	return ReleaseSetOptions{Commands: []string{"publish"}, All: true, Channels: channels, Profiles: testProfiles()}
}

func releaseSetE2EChannelKey(namespace, channel string) string { return namespace + "\x00" + channel }

func releaseSetE2EDigest(char byte) string {
	hex := "0123456789abcdef"
	return "sha256:" + strings.Repeat(string(hex[int(char)%len(hex)]), 64)
}

func releaseSetE2ECountOutcome(outcomes []distribution.ReleaseOutcome, want distribution.ReleaseOutcome) int {
	count := 0
	for _, outcome := range outcomes {
		if outcome == want {
			count++
		}
	}
	return count
}

func releaseSetErrorMessage(result *JobResult) string {
	if result == nil || result.Error == nil {
		return ""
	}
	return result.Error.Message
}

// The behavior the baseline channel exists for, end to end against the stateful
// in-memory provider: the FIRST publish into an empty `pr-7` measured against
// `canary` packages and uploads exactly the members whose recipe changed, and
// leaves `canary` where main put it.
func TestReleaseSetE2EFirstPullRequestPublishUploadsOnlyWhatChanged(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "release-set-channel-advance", "a-first-publish-against-a-baseline-uploads-only-what-changed")
	ctx := context.Background()
	ledger := newReleaseSetE2ELedger()
	useReleaseSetE2EProvider(t, ledger)
	useReleaseSetProvenance(t, nil)
	canaryRef, canarySet := releaseSetE2EBootstrap(t, ctx, ledger)

	// A pull request changes the downstream project only.
	useReleaseSetProvenance(t, map[string]string{"/downstream": releaseSetE2EDigest('9')})
	ws, upstream, downstream := releaseSetE2EWorkspace(t, "1.1.0")
	options := releaseSetE2EImpactedOptions("pr-7")
	options.BaselineChannel = "canary"
	run := releaseSetE2EPrepare(t, ctx, options, ws, []*workspace.Project{upstream, downstream})

	if run.HeadRef() == nil || *run.HeadRef() != canaryRef {
		t.Fatalf("plan base ref = %+v, want canary's head %+v", run.HeadRef(), canaryRef)
	}
	results, registry := releaseSetE2EPublish(run.plan, '2', nil)
	if registry.packageCount(upstream.Name) != 0 || registry.uploadCount(upstream.Name) != 0 {
		t.Fatalf("the first pull-request publish repackaged an unchanged member: %+v", registry)
	}
	if registry.packageCount(downstream.Name) != 1 || registry.uploadCount(downstream.Name) != 1 {
		t.Fatalf("changed member registry activity = %+v", registry)
	}

	run.Finalizer(ctx)(results)
	outcome := releaseSetE2EOutcome(t, results)
	finalSet := ledger.mustReleaseSet(t, outcome.Ref)
	oldUpstream := releaseSetE2EMember(t, canarySet, upstream.Name)
	if !releaseSetE2EMembersEqual(releaseSetE2EMember(t, finalSet, upstream.Name), oldUpstream) {
		t.Fatalf("inherited upstream changed: got %+v, want canary's record %+v",
			releaseSetE2EMember(t, finalSet, upstream.Name), oldUpstream)
	}
	if head, ok := ledger.head("putnami", "pr-7"); !ok || head != outcome.Ref {
		t.Fatalf("pr-7 head = %+v, %v; outcome = %+v", head, ok, outcome.Ref)
	}
	// canary is READ, never advanced: a pull request does not move the channel
	// that belongs to main.
	if head, ok := ledger.head("putnami", "canary"); !ok || head != canaryRef {
		t.Fatalf("canary head = %+v, %v; want the bootstrap ref %+v untouched", head, ok, canaryRef)
	}
	requests := ledger.releaseRequestsSnapshot()
	if final := requests[len(requests)-1]; len(final.Channels) != 1 || final.Channels[0].Name != "pr-7" ||
		final.Channels[0].Expected != nil {
		t.Fatalf("release channels = %+v, want pr-7 alone with a null expectation", final.Channels)
	}
}
