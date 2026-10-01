package jobs

import (
	"context"
	"reflect"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestPlanProcessCapabilityGatesIncludesEveryFunctionalLeaf(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "dependency-plan", "runner-publication-functional-gates")
	root := t.TempDir()
	project := &workspace.Project{ID: "/app", Name: "app", Path: "."}
	upstream := &workspace.Project{ID: "/lib", Name: "lib", Path: "."}
	build := capabilityTestJob(root, upstream, "build~describe", "build", "", "")
	lint := capabilityTestJob(root, project, "lint~check", "lint", "", "")
	publish := capabilityTestJob(root, project, "publish~push", "publish", "", extensionproto.SideEffectsRegistry)
	publish.SerializeAfter = []string{lint.Key()}
	planned := []*ScheduledJob{build, lint, publish}
	ctx := capturedCapabilityContext(t, "test-local-capability", "lint,build")
	if _, err := AuthorizeProcessCapabilities(ctx, planned, true); err == nil {
		t.Fatal("ungated input unexpectedly authorized")
	}
	if err := PlanProcessCapabilityGates(ctx, planned); err != nil {
		t.Fatal(err)
	}
	want := dedupeSorted([]string{build.Key(), lint.Key()})
	if !reflect.DeepEqual(publish.DependsOn, want) {
		t.Fatalf("publication dependencies = %v, want %v", publish.DependsOn, want)
	}
	if _, err := AuthorizeProcessCapabilities(ctx, planned, true); err != nil {
		t.Fatal(err)
	}
	if err := PlanProcessCapabilityGates(ctx, planned); err != nil || !reflect.DeepEqual(publish.DependsOn, want) {
		t.Fatalf("gate planning is not idempotent: %v", err)
	}
}

func TestPlanProcessCapabilityGatesCannotInventAuthorityOrNoopProof(t *testing.T) {
	project := &workspace.Project{ID: "/app", Name: "app", Path: "."}
	publish := capabilityTestJob(t.TempDir(), project, "publish~push", "publish", "", extensionproto.SideEffectsRegistry)
	if err := PlanProcessCapabilityGates(context.Background(), []*ScheduledJob{publish}); err != nil || len(publish.DependsOn) != 0 {
		t.Fatalf("uncaptured context changed the plan: %v", err)
	}
	ctx := capturedCapabilityContext(t, "test-local-capability", "test")
	if err := PlanProcessCapabilityGates(ctx, []*ScheduledJob{publish}); err != nil {
		t.Fatal(err)
	}
	if _, err := AuthorizeProcessCapabilities(ctx, []*ScheduledJob{publish}, true); err == nil {
		t.Fatal("missing test gate acquired a fabricated no-op proof")
	}
	gate := capabilityTestJob(t.TempDir(), project, "test~run", "test", "", "", publish.Key())
	if err := PlanProcessCapabilityGates(ctx, []*ScheduledJob{gate, publish}); err == nil {
		t.Fatal("a gate depending on publication must fail closed")
	}
	if len(publish.DependsOn) != 0 {
		t.Fatal("rejected candidate changed the original plan")
	}
}
