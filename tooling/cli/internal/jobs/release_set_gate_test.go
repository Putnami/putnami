package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// decodedManifestLiteral returns what a manifest's session-prerequisite param
// carries: the decoded JSON document, not the coordinator's typed plan. It is
// decoded rather than hand-built so the fixture keeps the shape a manifest
// actually produces, even when the document names this job's own project.
func decodedManifestLiteral(t *testing.T, document string) any {
	t.Helper()
	var decoded any
	if err := json.Unmarshal([]byte(document), &decoded); err != nil {
		t.Fatalf("decode manifest literal: %v", err)
	}
	return decoded
}

// asReleaseSetMember binds a plan the way attachReleaseSetContext does, so the
// job reads as a release-set member publication.
func asReleaseSetMember(job *ScheduledJob) *ScheduledJob {
	job.JobDef.BoundParams = extension.ParamMap{releaseset.ContextParamName: &releaseset.Plan{
		Members: []releaseset.PlannedMember{{ProjectID: job.Project.ID}},
	}}
	return job
}

// TestPlanProcessCapabilityGatesIgnoreAManifestSpelledReleaseSetKey pins that
// only the coordinator's typed plan naming the job's project exempts a publish:
// a manifest literal under the same key, or a plan for another project, keeps
// the AFTER gate.
func TestPlanProcessCapabilityGatesIgnoreAManifestSpelledReleaseSetKey(t *testing.T) {
	root := t.TempDir()
	app := &workspace.Project{ID: "/app", Name: "app", Path: "."}
	for _, testCase := range []struct {
		name  string
		bound any
	}{
		{name: "manifest literal", bound: decodedManifestLiteral(t, `{"members":[{"projectId":"/app"}]}`)},
		{name: "plan for another project", bound: &releaseset.Plan{Members: []releaseset.PlannedMember{{ProjectID: "/other"}}}},
		{name: "nil plan", bound: (*releaseset.Plan)(nil)},
	} {
		name, bound := testCase.name, testCase.bound
		t.Run(name, func(t *testing.T) {
			lint := capabilityTestJob(root, app, "lint~check", "lint", "", "")
			publish := capabilityTestJob(root, app, "publish~push", "publish", "", extensionproto.SideEffectsRegistry)
			publish.JobDef.BoundParams = extension.ParamMap{releaseset.ContextParamName: bound}
			ctx := capturedCapabilityContext(t, "test-local-capability", "lint")
			planned := []*ScheduledJob{lint, publish}
			if err := PlanProcessCapabilityGates(ctx, planned); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(publish.DependsOn, []string{lint.Key()}) {
				t.Fatalf("publish dependencies = %v, want the AFTER gate %v", publish.DependsOn, []string{lint.Key()})
			}
		})
	}
}

// TestPlanProcessCapabilityGatesLeaveMemberPublicationToTheReleaseSetStamp
// pins ADR 0023: a release-set member publish flows from its own package
// chain, and the AFTER leaves gate the release-set stamp (here the
// same-session barrier) instead.
func TestPlanProcessCapabilityGatesLeaveMemberPublicationToTheReleaseSetStamp(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "dependency-plan", "runner-publication-functional-gates")
	root := t.TempDir()
	app := &workspace.Project{ID: "/app", Name: "app", Path: "."}
	lib := &workspace.Project{ID: "/lib", Name: "lib", Path: "."}
	libTest := capabilityTestJob(root, lib, "test~run", "test", "", "")
	lint := capabilityTestJob(root, app, "lint~check", "lint", "", "")
	build := capabilityTestJob(root, app, "build~compile", "build", "", "")
	pkg := capabilityTestJob(root, app, "package~docker", "package", "", "", build.Key())
	publish := asReleaseSetMember(capabilityTestJob(root, app, "publish~docker", "publish", "", extensionproto.SideEffectsRegistry, pkg.Key()))
	barrier := capabilityTestJob(root, app, "publish~release-set", "publish", "", extensionproto.SideEffectsCloud, publish.Key())
	planned := []*ScheduledJob{libTest, lint, build, pkg, publish, barrier}

	ctx := capturedCapabilityContext(t, "test-local-capability", "lint,test,build")
	if err := PlanProcessCapabilityGates(ctx, planned); err != nil {
		t.Fatal(err)
	}
	if want := []string{pkg.Key()}; !reflect.DeepEqual(publish.DependsOn, want) {
		t.Fatalf("member publish dependencies = %v, want only its package chain %v", publish.DependsOn, want)
	}
	if want := dedupeSorted([]string{publish.Key(), libTest.Key(), lint.Key(), build.Key()}); !reflect.DeepEqual(barrier.DependsOn, want) {
		t.Fatalf("release-set stamp dependencies = %v, want every gate leaf %v", barrier.DependsOn, want)
	}
	authorization, err := AuthorizeProcessCapabilities(ctx, planned, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := authorization.requiredSuccessesByJob[publish.Key()]; !reflect.DeepEqual(got, dedupeSorted([]string{pkg.Key(), build.Key()})) {
		t.Fatalf("member publish required successes = %v, want its own ancestors only", got)
	}
	if got, want := authorization.requiredSuccessesByJob[barrier.Key()], dedupeSorted([]string{build.Key(), lint.Key(), pkg.Key(), publish.Key(), libTest.Key()}); !reflect.DeepEqual(got, want) {
		t.Fatalf("release-set stamp required successes = %v, want every planned ancestor %v", got, want)
	}

	// A publish outside any release set keeps the AFTER gate.
	outside := capabilityTestJob(root, lib, "publish~push", "publish", "", extensionproto.SideEffectsRegistry)
	if _, err := AuthorizeProcessCapabilities(ctx, []*ScheduledJob{libTest, lint, build, outside}, true); err == nil || !strings.Contains(err.Error(), "is not functionally after required leaf") {
		t.Fatalf("publish outside a release set was not gated: %v", err)
	}
}

// TestScheduler_MemberPublicationRunsWhileAnotherProjectFails pins the flow
// ADR 0023 asks for: one project's failed test neither delays nor blocks
// another project's member publication, which still receives its capability
// once its own package chain succeeded. The release-set finalizer, not the
// publish, is what a failed result withholds.
func TestScheduler_MemberPublicationRunsWhileAnotherProjectFails(t *testing.T) {
	const after = "lint,test,build"
	root := t.TempDir()
	app := &workspace.Project{ID: "/app", Name: "app", Path: "."}
	lib := &workspace.Project{ID: "/lib", Name: "lib", Path: "."}
	marker := filepath.Join(root, "published")
	libTest := capabilityTestJob(root, lib, "test~run", "test", "", "")
	capabilityProbe(t, failProbe, libTest)
	lint := capabilityTestJob(root, app, "lint~check", "lint", "", "")
	build := capabilityTestJob(root, app, "build~compile", "build", "", "")
	pkg := capabilityTestJob(root, app, "package~docker", "package", "", "", build.Key())
	capabilityProbe(t, passProbe, lint, build, pkg)
	publish := asReleaseSetMember(capabilityTestJob(root, app, "publish~docker", "publish", "", extensionproto.SideEffectsRegistry, pkg.Key()))
	publish.JobDef.Env = map[string]string{"PUTNAMI_EFFECT_MARKER": marker, "PUTNAMI_EXPECTED_CLOUD_TOKEN": "exact-member-capability"}
	// The member marks its publication only when it holds its exact capability.
	exact := "same:" + extensionproto.CloudTokenEnv + "=PUTNAMI_EXPECTED_CLOUD_TOKEN"
	capabilityProbe(t, fixtureScript{{"verdict", exact}, {"exit-unless", "0", exact}, {"append-env", "", "$PUTNAMI_EFFECT_MARKER"}}, publish)
	plan := []*ScheduledJob{libTest, lint, build, pkg, publish}

	ctx := capturedCapabilityContext(t, "exact-member-capability", after)
	authorization, err := AuthorizeProcessCapabilities(ctx, plan, true)
	if err != nil {
		t.Fatalf("AuthorizeProcessCapabilities: %v", err)
	}
	result := RunPlan(ctx, RunRequest{
		Workspace: &workspace.Workspace{Root: root, Name: "test-ws"},
		Plan:      plan,
		Config: SchedulerConfig{
			MaxParallel:     1,
			ContinueOnError: true,
			NoCache:         true,
		},
		Renderer:                       &mockRenderer{},
		ProcessCapabilityAuthorization: authorization,
	})
	if got := capabilityJobResult(t, libTest.Key(), result.Results[libTest.Key()]); got.Status != "failed" {
		t.Fatalf("lib test result: %s, want failed", capabilityResultDetail(got))
	}
	if got := capabilityJobResult(t, publish.Key(), result.Results[publish.Key()]); got.Status != "success" {
		t.Fatalf("member publish result: %s, want success with its capability despite the unrelated failure", capabilityResultDetail(got))
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("member publish did not run: %v", err)
	}
}

// TestScheduler_MemberPublicationWithAFailedPackageIsRefused pins strict
// admission for the exempted job: its own package chain failed, so under
// --continue-on-error it is skipped and never receives the capability.
func TestScheduler_MemberPublicationWithAFailedPackageIsRefused(t *testing.T) {
	root := t.TempDir()
	app := &workspace.Project{ID: "/app", Name: "app", Path: "."}
	marker := filepath.Join(root, "published")
	lint := capabilityTestJob(root, app, "lint~check", "lint", "", "")
	build := capabilityTestJob(root, app, "build~compile", "build", "", "")
	capabilityProbe(t, passProbe, lint, build)
	pkg := capabilityTestJob(root, app, "package~docker", "package", "", "", build.Key())
	capabilityProbe(t, failProbe, pkg)
	publish := asReleaseSetMember(capabilityTestJob(root, app, "publish~docker", "publish", "", extensionproto.SideEffectsRegistry, pkg.Key()))
	publish.JobDef.Env = map[string]string{"PUTNAMI_EFFECT_MARKER": marker}
	capabilityProbe(t, markerEffect, publish)
	plan := []*ScheduledJob{lint, build, pkg, publish}

	ctx := capturedCapabilityContext(t, "exact-member-capability", "lint,build")
	authorization, err := AuthorizeProcessCapabilities(ctx, plan, true)
	if err != nil {
		t.Fatalf("AuthorizeProcessCapabilities: %v", err)
	}
	result := RunPlan(ctx, RunRequest{
		Workspace: &workspace.Workspace{Root: root, Name: "test-ws"},
		Plan:      plan,
		Config: SchedulerConfig{
			MaxParallel:     1,
			ContinueOnError: true,
			NoCache:         true,
		},
		Renderer:                       &mockRenderer{},
		ProcessCapabilityAuthorization: authorization,
	})
	if got := capabilityJobResult(t, publish.Key(), result.Results[publish.Key()]); got.Status != "skipped" {
		t.Fatalf("member publish result: %s, want skipped after its own failed package", capabilityResultDetail(got))
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("member publish ran after its own failed package; marker stat error = %v", err)
	}
}
