package jobs

import (
	"context"
	"maps"
	"testing"

	ciproto "go.putnami.dev/protocol/ci"
	distribution "go.putnami.dev/protocol/distribution"
	"go.putnami.dev/protocol/features/spectest"
)

func TestReleaseCarriesMirrorIntentOnAlreadyCurrentPublication(t *testing.T) {
	spectest.Proves(t, "cli/channels", "mirror-intent-follows-the-release", "already-current-release-carries-mirror-intent")
	ws, _, _ := releaseSetWorkspace(t)
	useReleaseSetProvenance(t, nil)
	provider := &fakeReleaseSetProvider{heads: releaseSetHead(t)}
	useReleaseSetProviderFake(t, provider)
	policy := &ciproto.Distribution{
		Namespace: "putnami", Visibility: "internal",
		Registries: map[string]ciproto.RegistryPolicy{
			"npm": {Visibility: "private", Mirror: &ciproto.Mirror{To: "https://registry.npmjs.org"}},
			"oci": {Mirror: &ciproto.Mirror{To: "docker.io/putnami"}},
			"go":  {Visibility: "public"},
		},
	}
	options := releaseSetRequest()
	options.Policy = policy
	run, err := prepareReleaseSet(t, options, ws, ws.Projects, providerDiscovery())
	if err != nil || run == nil || !run.NoImpact() {
		t.Fatalf("mirror intent must not force rebuilding the set: %+v, %v", run, err)
	}
	// The accepted publication uses the policy captured at planning time, even
	// if another caller later changes the parsed CI configuration in memory.
	policy.Registries["npm"].Mirror.To = "https://other.example.com"
	results := map[string]*JobResult{}
	run.Finalizer(context.Background())(results)
	if result := results[releaseSetResultKey]; result == nil || result.Status != "success" {
		t.Fatalf("already-current mirror release failed: %+v", result)
	}
	want := map[string]distribution.MirrorTarget{
		"npm": {To: "https://registry.npmjs.org"},
		"oci": {To: "docker.io/putnami"},
	}
	if provider.releaseCalls != 1 || !maps.Equal(provider.releaseRequest.Mirrors, want) {
		t.Fatalf("mirror intent must travel in the single release call: calls=%d request=%+v", provider.releaseCalls, provider.releaseRequest)
	}
	if len(provider.releaseOutcomes) != 1 || provider.releaseOutcomes[0] != distribution.ReleaseOutcomeAlreadyCurrent {
		t.Fatalf("mirrors changed channel acceptance: %v", provider.releaseOutcomes)
	}
	if provider.releaseRequest.Visibility.Repo != distribution.VisibilityInternal || provider.releaseRequest.Visibility.Registries["npm"] != distribution.VisibilityPrivate {
		t.Fatalf("mirror intent widened visibility before provider resolution: %+v", provider.releaseRequest.Visibility)
	}
}

func TestMirrorPolicyIsOptionalAndRejectsInvalidDestinations(t *testing.T) {
	for _, policy := range []*ciproto.Distribution{nil, {}, {Registries: map[string]ciproto.RegistryPolicy{"npm": {Visibility: "public"}}}} {
		if targets, err := BuildMirrorTargets(policy); err != nil || targets != nil {
			t.Fatalf("undeclared mirror should create no intent: %+v, %v", targets, err)
		}
	}
	whitespace := &ciproto.Distribution{Registries: map[string]ciproto.RegistryPolicy{
		"npm": {Mirror: &ciproto.Mirror{To: "  https://registry.npmjs.org/putnami  "}},
	}}
	targets, err := BuildMirrorTargets(whitespace)
	if err != nil || targets["npm"].To != "https://registry.npmjs.org/putnami" {
		t.Fatalf("mirror whitespace must match CI validation: targets=%+v err=%v", targets, err)
	}
	policy := &ciproto.Distribution{Registries: map[string]ciproto.RegistryPolicy{
		"npm": {Mirror: &ciproto.Mirror{To: "https://user:secret@registry.example.com"}},
	}}
	if targets, err := BuildMirrorTargets(policy); err == nil || targets != nil {
		t.Fatalf("credential-bearing mirror policy accepted: %+v, %v", targets, err)
	}
	ws, _, _ := releaseSetWorkspace(t)
	useReleaseSetProvenance(t, nil)
	provider := &fakeReleaseSetProvider{heads: releaseSetHead(t)}
	useReleaseSetProviderFake(t, provider)
	options := releaseSetRequest()
	options.Policy = policy
	if run, err := prepareReleaseSet(t, options, ws, ws.Projects, providerDiscovery()); err == nil || run != nil {
		t.Fatalf("release planned despite invalid mirror policy: %+v, %v", run, err)
	}
	if provider.releaseCalls != 0 {
		t.Fatalf("invalid destination caused a release: %d", provider.releaseCalls)
	}
}
