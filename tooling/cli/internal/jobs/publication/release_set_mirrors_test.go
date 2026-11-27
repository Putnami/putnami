package publication

import (
	"bytes"
	"context"
	"maps"
	"strings"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	ciproto "go.putnami.dev/protocol/ci"
	distribution "go.putnami.dev/protocol/distribution"
	"go.putnami.dev/protocol/features/spectest"
)

func TestReleaseCarriesMirrorIntentOnAlreadyCurrentPublication(t *testing.T) {
	spectest.Proves(t, "cli/channels", "mirror-intent-follows-the-release", "already-current-release-carries-mirror-intent")
	ws, _, _ := releaseSetWorkspace(t)
	useReleaseSetProvenance(t, nil)
	provider := &fakeReleaseSetProvider{heads: releasedHead(t, "1.0.0")}
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
	results := map[string]*jobmodel.JobResult{}
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

// releasedHead is releaseSetHead with every member, and every edge between
// them, at version: the head a tagged release leaves on its channels.
func releasedHead(t *testing.T, version string) map[string]*distribution.ChannelHead {
	t.Helper()
	heads := releaseSetHead(t)
	for i := range heads["canary"].ReleaseSet.Members {
		member := &heads["canary"].ReleaseSet.Members[i]
		member.Version = version
		for j := range member.Dependencies {
			member.Dependencies[j].Version = version
		}
	}
	refreshHeadRef(t, heads["canary"])
	return heads
}

func useMirrorNotices(t *testing.T) *bytes.Buffer {
	t.Helper()
	var notices bytes.Buffer
	previous := mirrorNotices
	mirrorNotices = &notices
	t.Cleanup(func() { mirrorNotices = previous })
	return &notices
}

func TestPrereleaseMemberWithholdsMirrorIntent(t *testing.T) {
	spectest.Proves(t, "cli/channels", "mirror-intent-copies-released-versions-only", "prerelease-member-withholds-mirror-intent")
	notices := useMirrorNotices(t)
	ws, _, _ := releaseSetWorkspace(t)
	useReleaseSetProvenance(t, nil)
	// The canary head carries prerelease versions, the shape an untagged
	// commit stamps on the main and pull-request publications.
	provider := &fakeReleaseSetProvider{heads: releaseSetHead(t)}
	useReleaseSetProviderFake(t, provider)
	options := releaseSetRequest()
	options.Policy = &ciproto.Distribution{
		Namespace: "putnami",
		Registries: map[string]ciproto.RegistryPolicy{
			"npm": {Mirror: &ciproto.Mirror{To: "https://registry.npmjs.org"}},
			"oci": {Mirror: &ciproto.Mirror{To: "docker.io/putnami"}},
		},
	}
	run, err := prepareReleaseSet(t, options, ws, ws.Projects, providerDiscovery())
	if err != nil || run == nil {
		t.Fatalf("prepare: %+v, %v", run, err)
	}
	results := map[string]*jobmodel.JobResult{}
	run.Finalizer(context.Background())(results)
	if result := results[releaseSetResultKey]; result == nil || result.Status != "success" {
		t.Fatalf("a withheld mirror must not fail the release: %+v", result)
	}
	// npm has a prerelease member, so it requests no copy; oci has none, so
	// its destination travels unchanged and creates no copy either.
	want := map[string]distribution.MirrorTarget{"oci": {To: "docker.io/putnami"}}
	if provider.releaseCalls != 1 || !maps.Equal(provider.releaseRequest.Mirrors, want) {
		t.Fatalf("prerelease npm members reached the mirror: calls=%d mirrors=%+v", provider.releaseCalls, provider.releaseRequest.Mirrors)
	}
	notice := notices.String()
	if !strings.Contains(notice, "npm mirror to https://registry.npmjs.org withheld: @putnami/downstream 1.0.0-old is a prerelease version") ||
		strings.Count(notice, "\n") != 1 {
		t.Fatalf("withheld mirror must be reported once, naming the first prerelease member: %q", notice)
	}
}

func TestReleasedMirrorTargetsKeepsOnlyFullyReleasedEcosystems(t *testing.T) {
	targets := map[string]distribution.MirrorTarget{
		"npm": {To: "https://registry.npmjs.org"},
		"oci": {To: "docker.io/putnami"},
		"go":  {To: "https://proxy.example.com"},
	}
	members := []distribution.ReleaseSetMember{
		{Ecosystem: "npm", Coordinate: "@putnami/web", Version: "0.5.0"},
		{Ecosystem: "npm", Coordinate: "@putnami/cloud", Version: "0.5.1-20261126120000-abcdef1"},
		{Ecosystem: "oci", Coordinate: "putnami/site", Version: "0.5.0+build.7"},
		{Ecosystem: "go", Coordinate: "go.putnami.dev/app", Version: "v0.5.0"},
		{Ecosystem: "archive", Coordinate: "putnami/cli", Version: "0.5.1-rc.1"},
		// Another line of the go ecosystem, still at an untagged version: one
		// such member withholds the whole ecosystem, the released one included.
		{Ecosystem: "go", Coordinate: "go.putnami.dev/protocol", Version: "v0.5.1-20261126120000-abcdef1"},
	}
	kept, withheld := ReleasedMirrorTargets(targets, members)
	if want := map[string]distribution.MirrorTarget{"oci": targets["oci"]}; !maps.Equal(kept, want) {
		t.Fatalf("kept %+v, want %+v", kept, want)
	}
	if len(withheld) != 2 || withheld["npm"].Coordinate != "@putnami/cloud" || withheld["go"].Coordinate != "go.putnami.dev/protocol" {
		t.Fatalf("withheld %+v", withheld)
	}
	if targets["npm"].To == "" {
		t.Fatal("the declared targets must not be mutated")
	}
	for version, release := range map[string]bool{
		"0.5.0": true, "v1.2.3": true, "1.0.0+meta": true, "v10.20.30": true,
		"": false, "v": false, "latest": false, "1.0": false, "01.0.0": false, "1.0.0\t": false, "0.0.0": false,
		"1.0.0-old": false, "0.5.1-20261126120000-abcdef1": false, "v0.5.1-20261126120000-abcdef1": false,
		"0.5.1-20261126120000-abcdef1-dirty": false, "1.0.0-rc.1+meta": false,
	} {
		if got := isReleaseVersion(version); got != release {
			t.Errorf("isReleaseVersion(%q) = %v, want %v", version, got, release)
		}
	}
	if kept, withheld := ReleasedMirrorTargets(nil, members); kept != nil || withheld != nil {
		t.Fatalf("no declared target withholds nothing: %+v %+v", kept, withheld)
	}
}
