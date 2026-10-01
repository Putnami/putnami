package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	modelextension "go.putnami.dev/cli/model/extension"
	"go.putnami.dev/cli/model/workspace"
	ciproto "go.putnami.dev/protocol/ci"
	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	distribution "go.putnami.dev/protocol/distribution"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	protocoljob "go.putnami.dev/protocol/job"
	runtimeproto "go.putnami.dev/protocol/runtime"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/profiles"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/extension"
	putnamigit "go.putnami.dev/tooling/cli/internal/git"
)

const testRevision = "8d5edb7513d93b9165ba2a7cb48466d022fc3f63"

// testTree is the git tree the fake provenance reports for an opted-in plan.
const testTree = "c0ffee5f1e2d3c4b5a69788796a5b4c3d2e1f0a9"

func TestReleaseUsesDeclaredNamespaceIndependentlyOfWorkspaceName(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "release-set-channel-advance", "every-listed-head-is-resolved-once")
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing-head-%t", existing), func(t *testing.T) {
			ws, _, _ := releaseSetWorkspace(t)
			ws.Name = "local-checkout-name"
			useReleaseSetProvenance(t, nil)
			heads := emptyHead("canary")
			if existing {
				heads = releaseSetHead(t)
			}
			provider := &fakeReleaseSetProvider{heads: heads}
			useReleaseSetProviderFake(t, provider)
			options := releaseSetAllOptions()
			options.Policy = &ciproto.Distribution{Namespace: "putnami"}
			run, err := prepareReleaseSet(t, options, ws, ws.Projects, providerDiscovery())
			if err != nil {
				t.Fatal(err)
			}
			if len(provider.resolveRequests) != 1 || provider.resolveRequests[0].Namespace != "putnami" || run.plan.Namespace != "putnami" {
				t.Fatalf("resolve=%+v plan=%+v", provider.resolveRequests, run.plan)
			}
			results := publishedResults(run.plan, digestFor('c'))
			run.Finalizer(context.Background())(results)
			if result := results[releaseSetResultKey]; result == nil || result.Status != "success" {
				t.Fatalf("release = %+v", result)
			}
			if provider.releaseRequest.Namespace != "putnami" || ws.Name != "local-checkout-name" {
				t.Fatalf("publication namespace=%q workspace=%q", provider.releaseRequest.Namespace, ws.Name)
			}
		})
	}
}

// realReleaseSetProvenance is captured before any test overrides the seam, so
// the git-backed implementation can still be exercised directly.
var realReleaseSetProvenance = newReleaseSetProvenance

func TestRequestedReleaseSetModeRequiresExplicitOrPlannedPublish(t *testing.T) {
	base := ReleaseSetOptions{Commands: []string{"deploy"}, Impacted: true, Channels: []string{"canary"}}
	if got := RequestedReleaseSetMode(base); got != ReleaseSetDisabled {
		t.Fatalf("dependent deploy mode = %v, want disabled before planning", got)
	}
	base.PlannedPublish = true
	if got := RequestedReleaseSetMode(base); got != ReleaseSetImpacted {
		t.Fatalf("planned dependent publish mode = %v, want impacted", got)
	}
	base.Impacted = false
	if got := RequestedReleaseSetMode(base); got != ReleaseSetDisabled {
		t.Fatalf("planned dependent publish without selection mode = %v, want disabled", got)
	}
	base.All = true
	if got := RequestedReleaseSetMode(base); got != ReleaseSetAll {
		t.Fatalf("planned dependent --all mode = %v, want all", got)
	}
}

func TestReleaseSetPlanSelectsChangedMembersAndTheirDependents(t *testing.T) {
	ws, upstream, downstream := releaseSetWorkspace(t)
	head := releaseSetHead(t)

	t.Run("downstream change inherits upstream exactly", func(t *testing.T) {
		useReleaseSetProvenance(t, map[string]string{downstream.ID: digestFor('9')})
		run := prepareReleaseSetWithHead(t, releaseSetRequest(), ws, head)
		up, _ := run.plan.Member(distribution.Ecosystem("npm"), upstream.Name)
		down, _ := run.plan.Member(distribution.Ecosystem("npm"), downstream.Name)
		if up.Selected || up.Version != "1.0.0-old" || up.ArtifactDigest != digestFor('a') ||
			up.SourceRevision != testRevision || up.SelectionFingerprint != testFingerprint(upstream.ID) {
			t.Fatalf("unchanged upstream = %+v", up)
		}
		if !down.Selected || down.Version != "1.1.0" || down.ArtifactDigest != "" ||
			down.SourceRevision != testRevision || down.SelectionFingerprint != digestFor('9') {
			t.Fatalf("selected downstream = %+v", down)
		}
		if len(down.Dependencies) != 1 || down.Dependencies[0].Version != up.Version {
			t.Fatalf("downstream dependency = %+v, upstream = %+v", down.Dependencies, up)
		}
		if run.HeadRef() == nil || *run.HeadRef() != head["canary"].Ref {
			t.Fatalf("plan baseline = %+v, want the resolved head", run.HeadRef())
		}
	})

	t.Run("upstream change republishes its dependents", func(t *testing.T) {
		useReleaseSetProvenance(t, map[string]string{upstream.ID: digestFor('9')})
		run := prepareReleaseSetWithHead(t, releaseSetRequest(), ws, head)
		up, _ := run.plan.Member(distribution.Ecosystem("npm"), upstream.Name)
		down, _ := run.plan.Member(distribution.Ecosystem("npm"), downstream.Name)
		if !up.Selected || !down.Selected || up.Version != "1.1.0" || down.Dependencies[0].Version != up.Version {
			t.Fatalf("repackaged plan upstream=%+v downstream=%+v", up, down)
		}
		if down.SelectionFingerprint != testFingerprint(downstream.ID) {
			t.Fatalf("downstream fingerprint = %q, want its current tree", down.SelectionFingerprint)
		}
	})

	t.Run("nothing changed selects nothing", func(t *testing.T) {
		useReleaseSetProvenance(t, nil)
		run := prepareReleaseSetWithHead(t, releaseSetRequest(), ws, head)
		if !run.NoImpact() || len(run.plan.SelectedMembers()) != 0 || len(run.plan.Members) != 2 {
			t.Fatalf("no-impact plan = %+v", run.plan)
		}
		projects, extensions, err := run.ScopePlanning([]*workspace.Project{upstream, downstream}, nil, nil, testProfiles())
		if err != nil || projects != nil || extensions != nil {
			t.Fatalf("no-impact scope = %+v, %+v, %v; want nothing planned", projects, extensions, err)
		}

		// With verification commands, the no-impact scope keeps every selected
		// project and every verifying extension but strips each publish job:
		// the session publishes nothing. MemberProjectIDs names the projects the
		// coordinator accounts for, so the session's archive check skips them.
		verifier := &extension.ExtensionDescription{Name: "@putnami/verifier", Jobs: map[string]*extension.JobDefinition{
			"test":    {Name: "test"},
			"publish": {Name: "publish"},
		}}
		selected := []*workspace.Project{upstream, downstream}
		projects, extensions, err = run.ScopePlanning(selected, nil, []*extension.ExtensionDescription{verifier}, testProfiles(), "test", "publish")
		if err != nil || len(projects) != 2 || len(extensions) != 1 || extensions[0].Jobs["publish"] != nil || extensions[0].Jobs["test"] == nil {
			t.Fatalf("no-impact verification scope = %+v, %+v, %v; want both projects and a publish-free verifier", projects, extensions, err)
		}
		// A gate beside the publish verifies its own selection, and a gate
		// that selected nothing plans no project at all: the whole
		// workspace the coordinator keyed from is never the fallback.
		projects, extensions, err = run.ScopePlanning(selected, []*workspace.Project{downstream},
			[]*extension.ExtensionDescription{verifier}, testProfiles(), "test", "publish")
		if err != nil || len(projects) != 1 || projects[0] != downstream || len(extensions) != 1 {
			t.Fatalf("no-impact gate scope = %+v, %+v, %v; want the gate's one project", projects, extensions, err)
		}
		projects, extensions, err = run.ScopePlanning(selected, []*workspace.Project{},
			[]*extension.ExtensionDescription{verifier}, testProfiles(), "test", "publish")
		if err != nil || len(projects) != 0 || len(extensions) != 1 {
			t.Fatalf("no-impact empty-gate scope = %+v, %+v, %v; want no project", projects, extensions, err)
		}
		if got, want := run.MemberProjectIDs(), map[string]struct{}{upstream.ID: {}, downstream.ID: {}}; !maps.Equal(got, want) {
			t.Fatalf("member projects = %v, want %v", got, want)
		}
		if got := (*ReleaseSetRun)(nil).MemberProjectIDs(); got != nil {
			t.Fatalf("nil run member projects = %v, want nil", got)
		}
	})

	t.Run("--all republishes unchanged members", func(t *testing.T) {
		useReleaseSetProvenance(t, nil)
		run := prepareReleaseSetWithHead(t, releaseSetAllOptions(), ws, head)
		if !run.plan.SelectsEveryMember() || len(run.plan.Members) != 2 || run.plan.Baseline() == nil {
			t.Fatalf("--all plan = %+v", run.plan)
		}
		for _, member := range run.plan.Members {
			if member.Version != "1.1.0" || member.ArtifactDigest != "" || member.SelectionFingerprint != testFingerprint(member.ProjectID) {
				t.Fatalf("--all member %+v; want the candidate version with current provenance", member)
			}
		}
	})
}

func TestReleaseSetPlanRedefinesMembershipAgainstTheHead(t *testing.T) {
	ws, upstream, downstream := releaseSetWorkspace(t)
	useReleaseSetProvenance(t, nil)

	t.Run("member added since the head is selected", func(t *testing.T) {
		added := &workspace.Project{ID: "/added", Name: "@putnami/added", Version: "1.1.0", Type: "library", Metadata: releaseSetProjectMetadata(t, "npm", "@putnami/added")}
		wider := workspace.NewWorkspace(ws.Root, &wsproto.Config{Name: "putnami"}, []*workspace.Project{upstream, downstream, added})
		run := prepareReleaseSetWithHead(t, releaseSetRequest(), wider, releaseSetHead(t))
		member, ok := run.plan.Member(distribution.Ecosystem("npm"), added.Name)
		if !ok || !member.Selected || len(run.plan.SelectedMembers()) != 1 || len(run.plan.Members) != 3 {
			t.Fatalf("plan with a new member = %+v", run.plan.Members)
		}
	})

	t.Run("member dropped since the head leaves the set", func(t *testing.T) {
		head := releaseSetHead(t)
		head["canary"].ReleaseSet.Members = append(head["canary"].ReleaseSet.Members, distribution.ReleaseSetMember{
			Ecosystem: "npm", Coordinate: "@putnami/retired", Version: "1.0.0-old", ArtifactDigest: digestFor('c'),
			Dependencies: []distribution.ReleaseSetDependency{}, SourceRevision: testRevision, SelectionFingerprint: digestFor('7'),
		})
		refreshHeadRef(t, head["canary"])
		run := prepareReleaseSetWithHead(t, releaseSetRequest(), ws, head)
		if _, ok := run.plan.Member("npm", "@putnami/retired"); ok || len(run.plan.Members) != 2 || !run.NoImpact() {
			t.Fatalf("plan over a head with a retired member = %+v", run.plan.Members)
		}
	})

	t.Run("a member whose fingerprint is absent from the head is selected", func(t *testing.T) {
		head := releaseSetHead(t)
		for index := range head["canary"].ReleaseSet.Members {
			head["canary"].ReleaseSet.Members[index].SelectionFingerprint = digestFor('7')
		}
		refreshHeadRef(t, head["canary"])
		run := prepareReleaseSetWithHead(t, releaseSetRequest(), ws, head)
		if !run.plan.SelectsEveryMember() || run.HeadRef() == nil || *run.HeadRef() != head["canary"].Ref {
			t.Fatalf("plan over a head with foreign fingerprints = %+v", run.plan)
		}
	})

	t.Run("empty channel selects everything without a base", func(t *testing.T) {
		run := prepareReleaseSetWithHead(t, releaseSetRequest(), ws, emptyHead("canary"))
		if !run.plan.SelectsEveryMember() || run.plan.Baseline() != nil || run.HeadRef() != nil {
			t.Fatalf("plan over an empty channel = %+v", run.plan)
		}
	})
}

func TestPrepareReleaseSetFailsClosedOnInvalidHead(t *testing.T) {
	ws, _, _ := releaseSetWorkspace(t)
	useReleaseSetProvenance(t, nil)
	cases := []struct {
		name string
		edit func(map[string]*distribution.ChannelHead)
		want string
	}{
		{"unclosed dependency", func(heads map[string]*distribution.ChannelHead) {
			set := heads["canary"].ReleaseSet
			for index := range set.Members {
				if set.Members[index].Coordinate == "@putnami/downstream" {
					set.Members[index].Dependencies[0].Coordinate = "@putnami/missing"
				}
			}
			refreshHeadRef(t, heads["canary"])
		}, "invalid provider exchange"},
		{"ref mismatch", func(heads map[string]*distribution.ChannelHead) {
			heads["canary"].Ref.ID = "rs_" + strings.Repeat("f", 64)
		}, "invalid provider exchange"},
		{"foreign namespace", func(heads map[string]*distribution.ChannelHead) {
			heads["canary"].ReleaseSet.Namespace = "other"
			refreshHeadRef(t, heads["canary"])
		}, "invalid provider exchange"},
		{"generation zero", func(heads map[string]*distribution.ChannelHead) {
			heads["canary"].Generation = 0
		}, "invalid provider exchange"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			head := releaseSetHead(t)
			tc.edit(head)
			provider := &fakeReleaseSetProvider{heads: head}
			useReleaseSetProviderFake(t, provider)
			run, err := prepareReleaseSet(t, releaseSetRequest(), ws, ws.Projects, providerDiscovery())
			if err == nil || run != nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("run = %+v, error = %v, want containing %q", run, err, tc.want)
			}
		})
	}
}

func TestPrepareReleaseSetNoImpactConfirmsHeadThroughOneRelease(t *testing.T) {
	ws, _, _ := releaseSetWorkspace(t)
	useReleaseSetProvenance(t, nil)
	head := releaseSetHead(t)
	provider := &fakeReleaseSetProvider{heads: head}
	useReleaseSetProviderFake(t, provider)
	run, err := prepareReleaseSet(t, releaseSetRequest(), ws, ws.Projects, providerDiscovery())
	if err != nil || run == nil || !run.NoImpact() || run.Plan() == nil {
		t.Fatalf("no-impact preparation = %+v, %v", run, err)
	}
	if provider.resolveCalls != 1 {
		t.Fatalf("no-impact resolve calls = %d, want exactly one", provider.resolveCalls)
	}
	results := map[string]*JobResult{}
	run.Finalizer(context.Background())(results)
	result := results[releaseSetResultKey]
	if result == nil || result.Status != "success" {
		t.Fatalf("no-impact result = %+v", result)
	}
	outcome, ok := result.Data[runtimeproto.ReleaseSetResultDataKey].(distribution.ReleaseSetPublishOutcome)
	if !ok || outcome.Ref != head["canary"].Ref || outcome.Namespace != "putnami" || outcome.ProtocolVersion != distribution.ProtocolVersion {
		t.Fatalf("no-impact outcome = %+v, want the resolved immutable ref", outcome)
	}
	if outcome.Channels["canary"] == nil || outcome.Channels["canary"].Ref != head["canary"].Ref {
		t.Fatalf("no-impact channels = %+v, want canary at the confirmed head", outcome.Channels)
	}
	expected := provider.releaseRequest.Channels[0].Expected
	if provider.releaseCalls != 1 || len(provider.releaseRequest.Channels) != 1 || expected == nil || *expected != head["canary"].Ref {
		t.Fatalf("no-impact release = %+v (calls %d); want one release expecting the head", provider.releaseRequest, provider.releaseCalls)
	}
	if provider.releaseRequest.Visibility.Repo != distribution.VisibilityInternal ||
		provider.releaseRequest.Channels[0].Visibility != distribution.VisibilityInternal ||
		provider.releaseRequest.Visibility.Set != nil {
		t.Fatalf("no-impact visibility chain = %+v, want the neutral internal chain", provider.releaseRequest.Visibility)
	}
	if provider.releaseOutcomes[0] != distribution.ReleaseOutcomeAlreadyCurrent {
		t.Fatalf("no-impact release outcome = %v, want already-current", provider.releaseOutcomes[0])
	}
}

func TestPrepareReleaseSetNoImpactStillRequiresProvider(t *testing.T) {
	ws, _, _ := releaseSetWorkspace(t)
	useReleaseSetProvenance(t, nil)
	_, err := prepareReleaseSet(t, releaseSetRequest(), ws, ws.Projects, &extension.DiscoveryResult{})
	if err == nil || !errors.Is(err, releaseset.ErrProviderAbsent) {
		t.Fatalf("absent provider error = %v", err)
	}
}

func TestPrepareReleaseSetDryRunDoesNotClaimPublication(t *testing.T) {
	ws, _, _ := releaseSetWorkspace(t)
	useReleaseSetProvenance(t, nil)
	provider := &fakeReleaseSetProvider{heads: releaseSetHead(t)}
	useReleaseSetProviderFake(t, provider)
	request := releaseSetRequest()
	request.DryRun = true // terminal applyPublishDryRun's exact value
	run, err := prepareReleaseSet(t, request, ws, ws.Projects, providerDiscovery())
	if err != nil || run == nil || !run.NoImpact() || !run.dryRun {
		t.Fatalf("dry-run preparation = %+v, %v", run, err)
	}
	results := map[string]*JobResult{}
	run.Finalizer(context.Background())(results)
	if results[releaseSetResultKey] != nil || provider.releaseCalls != 0 {
		t.Fatalf("dry-run claimed publication: result=%+v provider=%+v", results[releaseSetResultKey], provider)
	}
}

func TestReleaseSetProtocolClientReceivesCapturedRunnerBearerWithoutAfter(t *testing.T) {
	const token = "release-set-runner-capability"
	ctx := capturedCapabilityContext(t, token, "lint,test,build,validate,validate-workspace")
	provider, err := newReleaseSetProvider(ctx, &extension.ResolvedProvider{
		ExtensionName: "provider",
		Version:       "0.1.0",
		Command:       distribution.ProviderCommandName,
	})
	if err != nil {
		t.Fatalf("newReleaseSetProvider: %v", err)
	}
	client, ok := provider.(*releaseset.Client)
	if !ok {
		t.Fatalf("release-set provider = %T, want framework SDK client", provider)
	}
	if got := envValues(client.Env, extensionproto.CloudTokenEnv); len(got) != 1 || got[0] != token {
		t.Fatalf("nested CLI bearer transport = %q, want exact captured bearer", got)
	}
	markers := envValues(client.Env, InternalReleaseSetProviderCapabilityEnv)
	if len(markers) != 1 || !strings.Contains(markers[0], `"extensionName":"provider"`) ||
		!strings.Contains(markers[0], `"command":"cloud-release-set"`) {
		t.Fatalf("nested CLI provider identity marker = %q, want exact resolved provider", markers)
	}
	if got := envValues(client.Env, extensionproto.CloudCapabilityAfterEnv); len(got) != 0 {
		t.Fatalf("nested CLI received runner AFTER control: %q", got)
	}

	tempDir := t.TempDir()
	observation := filepath.Join(tempDir, "capability-observation")
	providerProgram, providerEnv := fixtureCommand(t, fixtureScript{
		{"append-env", "$3|$PUTNAMI_CLOUD_TOKEN|${PUTNAMI_CLOUD_CAPABILITY_AFTER+x}\n", "$PUTNAMI_RELEASE_SET_CAPABILITY_LOG"},
		{"print-if", "$PUTNAMI_RELEASE_SET_RESOLVE_RESPONSE", "arg:3=resolve"},
		{"print-if", "$PUTNAMI_RELEASE_SET_RELEASE_RESPONSE", "arg:3=release"},
		{"exit-if", "91", "!arg:3=resolve", "!arg:3=release"},
	})
	client.Executable = providerProgram
	client.Env = append(client.Env, providerEnv...)
	client.TempDir = tempDir

	head := releaseSetHead(t)
	resolveResponse := distribution.ResolveResponse{ProtocolVersion: distribution.ProtocolVersion, Heads: head}
	releaseResponse := distribution.ReleaseResponse{
		ProtocolVersion: distribution.ProtocolVersion, Outcome: distribution.ReleaseOutcomeAlreadyCurrent,
		Current: map[string]*distribution.ChannelHead{"canary": {Ref: head["canary"].Ref, Generation: 3}},
	}
	encode := func(value any) string {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	client.Env = append(client.Env,
		"PUTNAMI_RELEASE_SET_CAPABILITY_LOG="+observation,
		"PUTNAMI_RELEASE_SET_RESOLVE_RESPONSE="+encode(resolveResponse),
		"PUTNAMI_RELEASE_SET_RELEASE_RESPONSE="+encode(releaseResponse),
	)
	if _, err := client.Resolve(ctx, &distribution.ResolveRequest{
		ProtocolVersion: distribution.ProtocolVersion, Namespace: "putnami", Channels: []string{"canary"},
	}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := client.Release(ctx, &distribution.ReleaseRequest{
		ProtocolVersion: distribution.ProtocolVersion, Namespace: "putnami",
		ReleaseSet: *head["canary"].ReleaseSet,
		Channels: []distribution.ChannelRequest{
			{Name: "canary", Expected: &head["canary"].Ref, Visibility: distribution.VisibilityInternal},
		},
		Visibility: distribution.VisibilityChain{Repo: distribution.VisibilityInternal},
	}); err != nil {
		t.Fatalf("Release: %v", err)
	}
	observed, err := os.ReadFile(observation)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(observed)), "\n")
	if len(lines) != 2 {
		t.Fatalf("provider observations = %q, want Resolve/Release", lines)
	}
	for index, operation := range []string{"resolve", "release"} {
		if want := operation + "|" + token + "|"; lines[index] != want {
			t.Fatalf("provider observation %d = %q, want %q (exact bearer, AFTER absent)", index, lines[index], want)
		}
	}
}

func TestReleaseSetProtocolClientTokenOnlyKeepsHistoricalParentAndUsesReservedChild(t *testing.T) {
	const token = "release-set-local-mac-token"
	t.Setenv(extensionproto.CloudTokenEnv, token)
	t.Setenv(extensionproto.CloudCapabilityAfterEnv, "")
	if err := os.Unsetenv(extensionproto.CloudCapabilityAfterEnv); err != nil {
		t.Fatal(err)
	}
	ctx := CaptureProcessCapabilities(context.Background())
	provider, err := newReleaseSetProvider(ctx, &extension.ResolvedProvider{
		ExtensionName: "provider",
		Version:       "0.1.0",
		Command:       distribution.ProviderCommandName,
	})
	if err != nil {
		t.Fatal(err)
	}
	client, ok := provider.(*releaseset.Client)
	if !ok {
		t.Fatalf("release-set provider = %T, want framework SDK client", provider)
	}
	if got := envValues(client.Env, extensionproto.CloudTokenEnv); len(got) != 1 || got[0] != token {
		t.Fatalf("token-only release-set child token = %q, want exact ambient token", got)
	}
	if got := envValues(client.Env, InternalReleaseSetProviderCapabilityEnv); len(got) != 1 {
		t.Fatalf("token-only release-set child marker = %q, want exact reserved-provider marker", got)
	}
	if got := envValues(client.Env, extensionproto.CloudCapabilityAfterEnv); len(got) != 0 {
		t.Fatalf("token-only release-set child AFTER = %q, want absent", got)
	}
	if got, present := os.LookupEnv(extensionproto.CloudTokenEnv); !present || got != token {
		t.Fatalf("token-only ambient value = %q, present %v; want preserved", got, present)
	}
}

func TestReleaseSetProtocolClientRejectsUnboundProviderBeforeChild(t *testing.T) {
	for name, provider := range map[string]*extension.ResolvedProvider{
		"missing": nil,
		"non-reserved": {
			ExtensionName: "provider",
			Version:       "0.1.0",
			Command:       "ordinary-cloud-command",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := newReleaseSetProvider(context.Background(), provider); err == nil {
				t.Fatal("unbound release-set provider created a protocol child")
			}
		})
	}
}

func TestReleaseSetScopePlanningKeepsOnlyMembersAndTheirPublishers(t *testing.T) {
	ws, upstream, downstream := releaseSetWorkspace(t)
	other := &workspace.Project{ID: "/service", Name: "service", Publish: []string{"docker"}}
	useReleaseSetProvenance(t, map[string]string{downstream.ID: digestFor('9')})
	run := prepareReleaseSetWithHead(t, releaseSetRequest(), ws, releaseSetHead(t))
	npmOwner := releaseSetOwnerExtension("npm-owner", distribution.Ecosystem("npm"))
	goOwner := releaseSetOwnerExtension("go-owner", distribution.Ecosystem("go"))
	publisher := releaseSetPublisherExtension("test-provider", "npm")
	provider := &modelextension.ExtensionDescription{Name: "provider", Jobs: map[string]*modelextension.JobDefinition{
		"publish": {Flags: map[string]modelextension.FlagDefinition{"config": {}}},
	}}
	projects, extensions, err := run.ScopePlanning(
		[]*workspace.Project{upstream, downstream, other}, nil,
		[]*extension.ExtensionDescription{provider, goOwner, publisher, npmOwner},
		testProfiles(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0] != downstream {
		t.Fatalf("scoped projects = %+v, want only downstream", projects)
	}
	if len(extensions) != 1 || extensions[0] != publisher {
		t.Fatalf("scoped extensions = %+v, want only the declaring publisher", extensions)
	}

	// The metadata envelope names the publisher. The profile owner may supply
	// registry semantics without supplying this project's publish jobs.
	_, _, err = run.ScopePlanning([]*workspace.Project{downstream}, nil, []*extension.ExtensionDescription{goOwner}, testProfiles())
	if err == nil || !strings.Contains(err.Error(), `published by "test-provider"`) {
		t.Fatalf("missing publisher error = %v", err)
	}
	undeclared := releaseSetPublisherExtension("test-provider")
	_, _, err = run.ScopePlanning([]*workspace.Project{downstream}, nil, []*extension.ExtensionDescription{undeclared}, testProfiles())
	if err == nil || !strings.Contains(err.Error(), "neither owns nor declares use") {
		t.Fatalf("undeclared ecosystem use error = %v", err)
	}
	incomplete := &modelextension.ExtensionDescription{Name: "test-provider", Uses: []string{"npm"}, Jobs: map[string]*modelextension.JobDefinition{
		"publish": {Flags: map[string]modelextension.FlagDefinition{"npm": {}}},
	}}
	_, _, err = run.ScopePlanning([]*workspace.Project{downstream}, nil, []*extension.ExtensionDescription{incomplete}, testProfiles())
	if err == nil || !strings.Contains(err.Error(), "does not declare package") {
		t.Fatalf("incomplete publisher error = %v", err)
	}
}

func TestReleaseSetScopePlanningRetainsVerificationProviders(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "release-set-members-by-ecosystem", "mixed-publication-retains-verification-without-extra-writes")
	for _, changed := range []bool{true, false} {
		t.Run(fmt.Sprintf("changed=%t", changed), func(t *testing.T) {
			ws, upstream, downstream := releaseSetWorkspace(t)
			changes := map[string]string{}
			if changed {
				changes[downstream.ID] = digestFor('9')
			}
			useReleaseSetProvenance(t, changes)
			run := prepareReleaseSetWithHead(t, releaseSetRequest(), ws, releaseSetHead(t))
			publisher := releaseSetPublisherExtension("test-provider", "npm")
			validationJob := &modelextension.JobDefinition{}
			validator := &extension.ExtensionDescription{Name: "validator", Jobs: map[string]*modelextension.JobDefinition{
				"validate": validationJob, "publish": {},
			}}
			unrequested := &extension.ExtensionDescription{Name: "unrequested", Jobs: map[string]*modelextension.JobDefinition{
				"publish": {},
			}}
			// The session verifies a project that owns no member, which is
			// what a gate command sharing the publish selects: it survives
			// whether or not a member changed.
			library := &workspace.Project{ID: "/library", Name: "@putnami/library", Type: "library"}
			projects, providers, err := run.ScopePlanning(
				[]*workspace.Project{upstream, downstream}, []*workspace.Project{library},
				[]*extension.ExtensionDescription{publisher, validator, unrequested},
				testProfiles(), "validate", "publish",
			)
			if err != nil {
				t.Fatal(err)
			}
			wantProjects, wantProviders := []string{library.ID}, 1
			if changed {
				wantProjects, wantProviders = []string{downstream.ID, library.ID}, 2
			}
			if got := projectIDsOf(projects); !slices.Equal(got, wantProjects) {
				t.Fatalf("verification scope = %v, want %v", got, wantProjects)
			}
			if len(providers) != wantProviders {
				t.Fatalf("verification scope = %d providers, want %d", len(providers), wantProviders)
			}
			retained := providers[len(providers)-1]
			if retained.Name != validator.Name || retained.Jobs["validate"] != validationJob || retained.Jobs["publish"] != nil {
				t.Fatalf("verification provider lost its command or retained publication: %+v", retained)
			}
			if validator.Jobs["publish"] == nil {
				t.Fatal("scoping mutated the installed extension descriptor")
			}
		})
	}
}

// A session that names a gate command beside the publish keeps the projects
// that command selected, whether or not they own a release-set member.
// The narrowing is the publish's alone: the coordinator still decides the
// members, and the library is planned beside them.
func TestReleaseSetScopePlanningKeepsVerificationProjectsBesideMembers(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "selection-before-plan", "a-channel-publish-narrows-itself-alone")
	ws, upstream, downstream := releaseSetWorkspace(t)
	library := &workspace.Project{ID: "/library", Name: "@putnami/library", Type: "library"}
	useReleaseSetProvenance(t, map[string]string{downstream.ID: digestFor('9')})
	run := prepareReleaseSetWithHead(t, releaseSetRequest(), ws, releaseSetHead(t))
	publisher := releaseSetPublisherExtension("test-provider", "npm")
	verifierJob := &modelextension.JobDefinition{}
	verifier := &extension.ExtensionDescription{Name: "verifier", Jobs: map[string]*modelextension.JobDefinition{
		"test": verifierJob, "publish": {},
	}}

	projects, extensions, err := run.ScopePlanning(
		[]*workspace.Project{upstream, downstream},
		[]*workspace.Project{library},
		[]*extension.ExtensionDescription{publisher, verifier},
		testProfiles(), "test", "publish",
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := projectIDsOf(projects), []string{downstream.ID, library.ID}; !slices.Equal(got, want) {
		t.Fatalf("mixed scope = %v, want the changed member beside the verified library %v", got, want)
	}
	// The extension rule is untouched: the member publisher keeps its publish
	// job, the verifier is retained for its own command with publication
	// stripped, and the installed descriptor is not mutated.
	if len(extensions) != 2 || extensions[0] != publisher {
		t.Fatalf("mixed extensions = %+v, want the member publisher first", extensions)
	}
	if retained := extensions[1]; retained.Name != verifier.Name ||
		retained.Jobs["test"] != verifierJob || retained.Jobs["publish"] != nil {
		t.Fatalf("verification provider = %+v, want its own command without publication", retained)
	}
	if verifier.Jobs["publish"] == nil {
		t.Fatal("scoping mutated the installed extension descriptor")
	}
}

// A publish-only session is unchanged: nil verification means no gate command
// shares the session, and the scope stays exactly the member owners.
func TestReleaseSetScopePlanningNilVerificationIsPublishOnly(t *testing.T) {
	ws, upstream, downstream := releaseSetWorkspace(t)
	other := &workspace.Project{ID: "/service", Name: "service", Publish: []string{"docker"}}
	useReleaseSetProvenance(t, map[string]string{downstream.ID: digestFor('9')})
	run := prepareReleaseSetWithHead(t, releaseSetRequest(), ws, releaseSetHead(t))
	publisher := releaseSetPublisherExtension("test-provider", "npm")

	projects, extensions, err := run.ScopePlanning(
		[]*workspace.Project{upstream, downstream, other}, nil,
		[]*extension.ExtensionDescription{publisher},
		testProfiles(), "publish",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0] != downstream {
		t.Fatalf("publish-only scope = %+v, want only the changed member owner", projects)
	}
	if len(extensions) != 1 || extensions[0] != publisher {
		t.Fatalf("publish-only extensions = %+v, want only the declaring publisher", extensions)
	}
	// The selection block a publish-only session reports is the coordinator's:
	// the head measured the impact, so the head is the baseline, and the
	// release-set half is every project the run plans.
	previous := ResolvedRunSelection(protocoljob.SelectionModeAll, "", "", []*workspace.Project{upstream, downstream, other})
	reported := run.RunSelection(previous, projects)
	if reported.Mode != protocoljob.SelectionModeImpacted || reported.BaselineSource != ReleaseSetHeadBaselineSource {
		t.Fatalf("publish-only selection = %+v, want the head-measured tier", reported)
	}
	if !slices.Equal(reported.ProjectIDs, reported.ReleaseSetProjects) {
		t.Fatalf("publish-only selection projects %v, release-set projects %v; want one list",
			reported.ProjectIDs, reported.ReleaseSetProjects)
	}
}

// The mixed session reports both halves, and the gate's own tier survives: the
// head decided the members, the caller's baseline decided the verification.
func TestReleaseSetRunSelectionReportsBothHalvesOfAMixedSession(t *testing.T) {
	ws, upstream, downstream := releaseSetWorkspace(t)
	library := &workspace.Project{ID: "/library", Name: "@putnami/library", Type: "library"}
	useReleaseSetProvenance(t, map[string]string{downstream.ID: digestFor('9')})
	run := prepareReleaseSetWithHead(t, releaseSetRequest(), ws, releaseSetHead(t))
	publisher := releaseSetPublisherExtension("test-provider", "npm")

	projects, _, err := run.ScopePlanning(
		[]*workspace.Project{upstream, downstream},
		[]*workspace.Project{library},
		[]*extension.ExtensionDescription{publisher},
		testProfiles(), "test", "publish",
	)
	if err != nil {
		t.Fatal(err)
	}
	gate := ResolvedRunSelection(protocoljob.SelectionModeImpacted, "origin/main", "trunk",
		[]*workspace.Project{library})
	reported := run.RunSelection(gate, projects)
	if reported.Mode != protocoljob.SelectionModeImpacted || reported.Baseline != "origin/main" ||
		reported.BaselineSource != "trunk" {
		t.Fatalf("mixed selection = %+v, want the gate's own mode and baseline tier", reported)
	}
	if got, want := reported.ProjectIDs, []string{downstream.ID, library.ID}; !slices.Equal(got, want) {
		t.Fatalf("mixed selection projects = %v, want the union %v", got, want)
	}
	if got, want := reported.ReleaseSetProjects, []string{downstream.ID}; !slices.Equal(got, want) {
		t.Fatalf("mixed selection release-set projects = %v, want the members %v", got, want)
	}
}

// With verification projects in the plan, a retained member publisher plans its
// publish jobs for every project it applies to. Those publications are outside
// the release transaction, so they are dropped together with the package steps
// that exist only to feed them — while the verification project keeps the gate
// nodes it was selected for, and a package step a kept node still needs stays.
func TestReleaseSetAttachPlanDropsNonMemberPublishNodes(t *testing.T) {
	downstream := &workspace.Project{ID: "/downstream", Name: "@putnami/downstream"}
	library := &workspace.Project{ID: "/library", Name: "@putnami/library"}
	publisher := &modelextension.ExtensionDescription{Name: "test-provider"}
	memberKey := releaseset.MemberKey("npm", "@putnami/downstream")
	run := &ReleaseSetRun{
		plan: &releaseset.Plan{Members: []releaseset.PlannedMember{{
			Ecosystem: "npm", Coordinate: "@putnami/downstream", ProjectID: downstream.ID, Selected: true,
		}}},
		routes: map[string]releaseMemberRoute{memberKey: {
			projectID: downstream.ID, publisher: publisher.Name, packagePublisher: publisher.Name,
			packageStep: "npm", publishCommand: "publish", publishStep: "npm",
		}},
		verification: &releaseVerificationScope{commands: []string{"test", "publish"}},
	}
	job := func(project *workspace.Project, command, step string) *ScheduledJob {
		return &ScheduledJob{
			Project: project, Extension: publisher,
			JobDef: &modelextension.JobDefinition{Name: command + "~" + step, CommandName: command, StepID: step},
		}
	}
	memberPackage := job(downstream, "package", "npm")
	memberPublish := job(downstream, "publish", "npm")
	libraryPackage := job(library, "package", "npm")
	libraryPublish := job(library, "publish", "npm")
	libraryPublish.DependsOn = []string{libraryPackage.Key()}
	libraryBuild := job(library, "build", "compile")
	libraryTest := job(library, "test", "test")
	libraryTest.DependsOn = []string{libraryBuild.Key()}
	// A publication whose command the coordinator does not know by name — no
	// member of this plan routes through it — and that the session never named:
	// nothing keeps it, and a publish-only session would not have planned it.
	libraryUpload := job(library, "upload", "archives")
	// The member's own package chain reaches the library's package step: a
	// wholly unchanged dependency's local preparation, never a registry write.
	memberPackage.DependsOn = []string{libraryPackage.Key()}
	memberPublish.SerializeAfter = []string{libraryPublish.Key()}

	scoped, err := run.AttachPlan([]*ScheduledJob{
		memberPackage, memberPublish, libraryPackage, libraryPublish, libraryBuild, libraryTest, libraryUpload,
	})
	if err != nil {
		t.Fatal(err)
	}
	kept := make(map[string]bool, len(scoped))
	for _, planned := range scoped {
		kept[planned.Key()] = true
	}
	for _, dropped := range []*ScheduledJob{libraryPublish, libraryUpload} {
		if kept[dropped.Key()] {
			t.Fatalf("%s survived: work outside both the release transaction and the session's commands", dropped.Key())
		}
	}
	for _, retained := range []*ScheduledJob{memberPackage, memberPublish, libraryBuild, libraryTest, libraryPackage} {
		if !kept[retained.Key()] {
			t.Fatalf("%s was dropped: the gate and the member chain both keep it", retained.Key())
		}
	}
	for _, planned := range scoped {
		for _, key := range slices.Concat(planned.DependsOn, planned.SerializeAfter) {
			if !kept[key] {
				t.Fatalf("kept job %s still names dropped %s", planned.Key(), key)
			}
		}
	}

	// A package step that only fed a dropped publication goes with it: nothing
	// the session keeps needs the artifact, and today's publish-only plan never
	// built one. The member's own two steps are rebuilt, because the
	// coordinator requires exactly one of each in every plan it is handed.
	orphan := job(library, "package", "npm")
	orphanPublish := job(library, "publish", "npm")
	orphanPublish.DependsOn = []string{orphan.Key()}
	scoped, err = run.AttachPlan([]*ScheduledJob{
		job(downstream, "package", "npm"), job(downstream, "publish", "npm"), orphan, orphanPublish,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, planned := range scoped {
		if planned.Project == library {
			t.Fatalf("%s survived with nothing left to need it", planned.Key())
		}
	}
}

// projectIDsOf renders a scoped project list as the ids a plan is keyed by.
func projectIDsOf(projects []*workspace.Project) []string {
	ids := make([]string, 0, len(projects))
	for _, project := range projects {
		if project != nil {
			ids = append(ids, project.ID)
		}
	}
	return ids
}

func TestReleaseSetScopePlanningKeepsDistinctPackagePublisher(t *testing.T) {
	ws, _, downstream := releaseSetWorkspace(t)
	downstream.Metadata = releaseSetProjectMembers(t, releaseset.MemberDeclaration{
		Ecosystem: "npm", Coordinate: downstream.Name,
		PackagePublisher: "go-packager", PackageStep: "archive", PublishStep: "npm",
	})
	useReleaseSetProvenance(t, map[string]string{downstream.ID: digestFor('9')})
	run := prepareReleaseSetWithHead(t, releaseSetRequest(), ws, releaseSetHead(t))
	publication := releaseSetPublisherExtension("test-provider", "npm")
	packager := &modelextension.ExtensionDescription{Name: "go-packager", Jobs: map[string]*modelextension.JobDefinition{"package": {}}}

	projects, extensions, err := run.ScopePlanning(
		[]*workspace.Project{downstream}, nil,
		[]*extension.ExtensionDescription{publication, packager},
		testProfiles(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0] != downstream {
		t.Fatalf("scoped projects = %+v, want downstream", projects)
	}
	if len(extensions) != 2 || extensions[0] != publication || extensions[1] != packager {
		t.Fatalf("scoped extensions = %+v, want publication and package providers", extensions)
	}

	_, _, err = run.ScopePlanning(
		[]*workspace.Project{downstream}, nil,
		[]*extension.ExtensionDescription{publication},
		testProfiles(),
	)
	if err == nil || !strings.Contains(err.Error(), `packaged by "go-packager"`) {
		t.Fatalf("missing package publisher error = %v", err)
	}
}

func TestReleaseSetScopePlanningNarrowsEmptyChannelPlanToMembers(t *testing.T) {
	ws, upstream, downstream := releaseSetWorkspace(t)
	useReleaseSetProvenance(t, nil)
	run := prepareReleaseSetWithHead(t, releaseSetRequest(), ws, emptyHead("canary"))
	publisher := releaseSetPublisherExtension("test-provider", "npm")
	otherProject := &workspace.Project{ID: "/service", Name: "service", Publish: []string{"docker"}}
	otherPublisher := &modelextension.ExtensionDescription{Name: "oci-owner", Jobs: map[string]*modelextension.JobDefinition{
		"publish": {Flags: map[string]modelextension.FlagDefinition{"docker": {}}},
	}}
	gotProjects, gotExtensions, err := run.ScopePlanning(
		[]*workspace.Project{upstream, downstream, otherProject}, nil,
		[]*extension.ExtensionDescription{publisher, otherPublisher},
		testProfiles(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotProjects) != 2 || gotProjects[0] != upstream || gotProjects[1] != downstream {
		t.Fatalf("empty-channel projects = %+v, want only release-set members", gotProjects)
	}
	if len(gotExtensions) != 1 || gotExtensions[0] != publisher {
		t.Fatalf("empty-channel extensions = %+v, want only the member publisher", gotExtensions)
	}
}

func TestReleaseSetScopePlanningUsesTheMetadataPublisherForBuiltinOCI(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "release-set-members-by-ecosystem", "member-plans-its-declaring-publisher")

	project := &workspace.Project{
		ID: "/service", Name: "service", Version: "1.1.0", Type: "application",
		Metadata: releaseSetProjectMetadata(t, "oci", "putnami/service"),
	}
	ws := workspace.NewWorkspace(t.TempDir(), &wsproto.Config{Name: "putnami"}, []*workspace.Project{project})
	useReleaseSetProvenance(t, map[string]string{project.ID: digestFor('9')})
	run := prepareReleaseSetWithHead(t, releaseSetAllOptions(), ws, emptyHead("canary"))
	publisher := releaseSetPublisherExtension("test-provider", "oci")

	projects, extensions, err := run.ScopePlanning(
		[]*workspace.Project{project}, nil, []*extension.ExtensionDescription{publisher}, testProfiles(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0] != project || len(extensions) != 1 || extensions[0] != publisher {
		t.Fatalf("scope = %+v, %+v; want the OCI member and its declaring publisher", projects, extensions)
	}
}

func TestReleaseSetImpactedPlansAgainstTheHeadNotTheGitSelection(t *testing.T) {
	ws, upstream, downstream := releaseSetWorkspace(t)
	useReleaseSetProvenance(t, map[string]string{upstream.ID: digestFor('9')})
	provider := &fakeReleaseSetProvider{heads: releaseSetHead(t)}
	useReleaseSetProviderFake(t, provider)

	// An explicit publish selects from the head: the git-derived selection
	// (downstream only) does not hide the changed upstream.
	run, err := prepareReleaseSet(t, releaseSetRequest(), ws, []*workspace.Project{downstream}, providerDiscovery())
	if err != nil || run == nil || len(run.plan.SelectedMembers()) != 2 {
		t.Fatalf("explicit impacted plan = %+v, %v; want both members selected", run, err)
	}
	if got := run.SelectedProjects(ws); len(got) != 2 || got[0] != downstream || got[1] != upstream {
		t.Fatalf("selected projects = %+v, want both in id order", got)
	}

	// A dependent publish is bounded by the publish jobs the deploy planned.
	request := releaseSetRequest()
	request.PlannedPublish = true
	_, err = prepareReleaseSet(t, request, ws, []*workspace.Project{downstream}, providerDiscovery())
	if err == nil || !strings.Contains(err.Error(), "requires republishing [npm/@putnami/upstream]") {
		t.Fatalf("dependent coverage error = %v", err)
	}
	run, err = prepareReleaseSet(t, request, ws, []*workspace.Project{upstream, downstream}, providerDiscovery())
	if err != nil || run == nil {
		t.Fatalf("covered dependent plan = %+v, %v", run, err)
	}
}

func TestBuildReleaseSetPlanDeduplicatesResolvedDependencyAliases(t *testing.T) {
	ws, upstream, downstream := releaseSetWorkspace(t)
	// Authored config and a provider probe may expose the same internal edge by
	// project ID and package name. Both resolve to one graph node and therefore
	// must produce one protocol dependency.
	downstream.Dependencies = []string{upstream.ID, upstream.Name}
	ws.Graph = workspace.BuildGraph(ws.Projects)
	plan := buildTestPlan(t, ReleaseSetOptions{Channels: []string{"canary"}, Impacted: true, Profiles: testProfiles()}, ws, nil)
	member, ok := plan.Member(distribution.Ecosystem("npm"), downstream.Name)
	if !ok {
		t.Fatalf("missing downstream member in %+v", plan.Members)
	}
	if len(member.Dependencies) != 1 || member.Dependencies[0].Coordinate != upstream.Name {
		t.Fatalf("downstream dependencies = %+v, want one %s dependency", member.Dependencies, upstream.Name)
	}
}

func TestBuildReleaseSetPlanExcludesReplaceOnlyImpactEdges(t *testing.T) {
	required := &workspace.Project{ID: "/required", Name: "go.putnami.dev/required", Version: "1.0.0", Type: "library", Metadata: releaseSetProjectMetadata(t, "go", "go.putnami.dev/required")}
	cacheOnly := &workspace.Project{ID: "/cache-only", Name: "go.putnami.dev/cache-only", Version: "1.0.0", Type: "library", Metadata: releaseSetProjectMetadata(t, "go", "go.putnami.dev/cache-only")}
	consumer := &workspace.Project{
		ID: "/consumer", Name: "go.putnami.dev/consumer", Version: "1.0.0", Type: "library", Publish: []string{"go"},
		// The graph contains both the real require and a replace-only edge used
		// for impact/cache invalidation.
		Dependencies: []string{required.Name, cacheOnly.Name},
		Metadata:     releaseSetProjectMetadata(t, "go", "go.putnami.dev/consumer", required.Name),
	}
	ws := workspace.NewWorkspace(t.TempDir(), &wsproto.Config{Name: "putnami"}, []*workspace.Project{required, cacheOnly, consumer})
	plan := buildTestPlan(t, releaseSetRequest(), ws, nil)
	member, ok := plan.Member(distribution.Ecosystem("go"), consumer.Name)
	if !ok || len(member.Dependencies) != 1 || member.Dependencies[0].Coordinate != required.Name {
		t.Fatalf("consumer release dependencies = %+v, want only actual require", member.Dependencies)
	}
	if got := ws.Graph.DependenciesOf(consumer.ID); len(got) != 2 {
		t.Fatalf("impact graph dependencies = %v, want both edges preserved", got)
	}
}

// A project contributes members only through its release-set metadata. One
// with none is not a member at all, and one naming an ecosystem no extension
// declares is an error that names both the project and the ecosystem — never a
// silently dropped member.
func TestReleaseProjectsReadsOnlyDeclaredMembers(t *testing.T) {
	silent := &workspace.Project{ID: "/library", Name: "@putnami/library", Type: "library"}
	ws := workspace.NewWorkspace(t.TempDir(), &wsproto.Config{Name: "putnami"}, []*workspace.Project{silent})
	candidates, err := releaseProjects(ws.Projects, testProfiles())
	if err != nil || len(candidates) != 0 {
		t.Fatalf("candidates = %+v, %v; want no member for a project that declares none", candidates, err)
	}

	unknown := &workspace.Project{
		ID: "/py", Name: "putnami-py", Type: "library",
		Metadata: releaseSetProjectMetadata(t, "pypi", "putnami-py"),
	}
	_, err = releaseProjects([]*workspace.Project{unknown}, testProfiles())
	if err == nil || !strings.Contains(err.Error(), `"/py"`) || !strings.Contains(err.Error(), `"pypi"`) {
		t.Fatalf("unknown ecosystem error = %v, want it to name the project and the ecosystem", err)
	}
}

// D13: the member key is (ecosystem, coordinate) and the project is
// provenance, so one project contributes several members — including two in
// one ecosystem — and every one of them is selected independently.
func TestOneProjectYieldsSeveralMembers(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "release-set-members-by-ecosystem", "one-project-yields-several-members")
	project := &workspace.Project{
		ID: "/service", Name: "@putnami/service", Version: "1.2.0", Type: "app",
		Metadata: releaseSetProjectMembers(t,
			releaseset.MemberDeclaration{Ecosystem: "npm", Coordinate: "@putnami/service", PackageStep: "npm", PublishStep: "npm"},
			releaseset.MemberDeclaration{Ecosystem: "npm", Coordinate: "@putnami/service-client", PackageStep: "npm-client", PublishStep: "npm-client"},
			releaseset.MemberDeclaration{Ecosystem: "oci", Coordinate: "putnami/service", PackageStep: "oci", PublishStep: "oci"},
		),
	}
	ws := workspace.NewWorkspace(t.TempDir(), &wsproto.Config{Name: "putnami"}, []*workspace.Project{project})
	useReleaseSetProvenance(t, nil)
	run := prepareReleaseSetWithHead(t, releaseSetAllOptions(), ws, emptyHead("canary"))
	if len(run.plan.Members) != 3 || !run.plan.SelectsEveryMember() {
		t.Fatalf("one-project plan = %+v, want three selected members", run.plan.Members)
	}
	for _, want := range []struct {
		ecosystem  distribution.Ecosystem
		coordinate string
	}{
		{"npm", "@putnami/service"},
		{"npm", "@putnami/service-client"},
		{"oci", "putnami/service"},
	} {
		member, ok := run.plan.Member(want.ecosystem, want.coordinate)
		if !ok || member.ProjectID != project.ID {
			t.Fatalf("member %s/%s = %+v, %v", want.ecosystem, want.coordinate, member, ok)
		}
	}
	// Each member carries the fingerprint of ITS OWN package task, so two
	// members of one project can be selected independently on a later run.
	if run.plan.Members[0].SelectionFingerprint == "" {
		t.Fatalf("member lost its selection fingerprint: %+v", run.plan.Members[0])
	}
}

// TestReleaseSetScopePlanningResolvesTheShippedGoOwner restores, for the go
// ecosystem, the pin T4 had to delete when extensionOwnsReleaseEcosystem was
// replaced by the profile registry: the SHIPPED manifest must be the one that
// owns the ecosystem it releases.
//
// The CLI names no ecosystem itself, so nothing else in the repository states
// that "go" belongs to @putnami/go. Resolving the real manifest here is the one
// check that the declaration the coordinator relies on is actually shipped —
// and that its version pattern is the reason the generic `v`-prefix rule in
// releaseCandidateVersion works, with no named-ecosystem branch anywhere.
// TestReleaseSetMembersRecordTheirSourceProjectAndKind pins the attribution a
// consumer holding only the published set needs: which project produced a
// member, and what role its artifact plays. Both travel from the plan into the
// final snapshot, and the kind comes from the declared publish step, never from
// the coordinate.
func TestReleaseSetMembersRecordTheirSourceProjectAndKind(t *testing.T) {
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
	ws := workspace.NewWorkspace(t.TempDir(), &wsproto.Config{Name: "putnami"}, []*workspace.Project{library, service})
	useReleaseSetProvenance(t, nil)
	options := releaseSetAllOptions()
	options.Policy = attributionPolicy()
	run := prepareReleaseSetWithHead(t, options, ws, emptyHead("canary"))

	image, ok := run.plan.Member(distribution.Ecosystem("oci"), "putnami/service")
	if !ok || image.Project != "apps/service" || image.Kind != distribution.KindImage {
		t.Fatalf("planned image member = %+v, %v; want apps/service classified as an image", image, ok)
	}
	packaged, ok := run.plan.Member(distribution.Ecosystem("npm"), "@putnami/web")
	if !ok || packaged.Project != "typescript/framework/web" || packaged.Kind != distribution.KindLibrary {
		t.Fatalf("planned npm member = %+v, %v; want typescript/framework/web classified as a library", packaged, ok)
	}

	final, err := reconcilePublishedReleaseSet(run.plan, publishedResults(run.plan, digestFor('d')))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for _, member := range final.Members {
		want := distribution.KindLibrary
		wantProject := "typescript/framework/web"
		if member.Ecosystem == distribution.Ecosystem("oci") {
			want, wantProject = distribution.KindImage, "apps/service"
		}
		if member.Project != wantProject || member.Kind != want {
			t.Fatalf("published member %+v; want project %q kind %q", member, wantProject, want)
		}
	}
	if diagnostics := distribution.ValidateReleaseSet(final); diag.HasErrors(diagnostics) {
		t.Fatalf("attributed snapshot rejected: %v", diagnostics)
	}
}

// TestReleaseSetMemberProjectIsOmittedWhenItIsNotRepresentable keeps a release
// possible in a workspace whose project path falls outside the protocol
// grammar: the member records no project instead of failing validation, because
// attribution is optional metadata and a publication must not be refused over
// the shape of a directory name.
func TestReleaseSetMemberProjectIsOmittedWhenItIsNotRepresentable(t *testing.T) {
	odd := &workspace.Project{
		ID: "/apps/my service", Name: "@putnami/odd", Version: "1.1.0", Type: "library",
		Metadata: releaseSetProjectMetadata(t, "npm", "@putnami/odd"),
	}
	ws := workspace.NewWorkspace(t.TempDir(), &wsproto.Config{Name: "putnami"}, []*workspace.Project{odd})
	useReleaseSetProvenance(t, nil)
	options := releaseSetAllOptions()
	options.Policy = attributionPolicy()
	run := prepareReleaseSetWithHead(t, options, ws, emptyHead("canary"))
	member, ok := run.plan.Member(distribution.Ecosystem("npm"), "@putnami/odd")
	if !ok || member.Project != "" || member.Kind != distribution.KindLibrary {
		t.Fatalf("member of an unrepresentable project = %+v, %v", member, ok)
	}
}

func TestReleaseSetScopePlanningResolvesTheShippedGoOwner(t *testing.T) {
	root := findJobsRepoRoot(t)
	path := filepath.Join(root, filepath.FromSlash("go/extension/putnami.extension.json"))
	manifest, parseDiagnostics := extensionproto.ParseManifest(mustReadFile(t, path))
	if manifest == nil || len(parseDiagnostics) != 0 {
		t.Fatalf("parse the shipped Go manifest: %v", parseDiagnostics)
	}
	registry, diagnostics := extensionproto.ResolveProfiles(
		[]extensionproto.NamedManifest{{Name: "@putnami/go", Manifest: manifest}},
		profiles.Builtin(),
	)
	if len(diagnostics) != 0 {
		t.Fatalf("shipped Go profiles are invalid: %v", diagnostics)
	}
	profile, owner, ok := registry.Profile("go")
	if !ok || owner != "@putnami/go" {
		t.Fatalf("go profile = %+v, owner %q, ok %v", profile, owner, ok)
	}
	if _, present := manifest.Commands[profile.Publish]; !present {
		t.Fatalf("the go profile publishes with %q, which the manifest does not declare", profile.Publish)
	}
	// oci is USED, never redeclared: two declarations of one ecosystem would
	// diverge on the first pattern edit.
	if !slices.Contains(manifest.Uses, "oci") {
		t.Fatalf("shipped Go manifest uses = %v, want oci", manifest.Uses)
	}
	// The generic rule in releaseCandidateVersion validates the bare version
	// against this pattern and falls back to the "v"-prefixed spelling. This is
	// what makes a Go member take vX.Y.Z with no ecosystem name in the CLI.
	if err := registry.ValidateVersion("go", "0.1.0-46554d75"); err == nil {
		t.Fatal("the shipped go profile accepted a bare version; the v-prefix fallback would never fire")
	}
	if err := registry.ValidateVersion("go", "v0.1.0-46554d75"); err != nil {
		t.Fatalf("the shipped go profile rejected the prefixed spelling: %v", err)
	}
	if err := registry.ValidateCoordinate("go", "go.putnami.dev/protocol/extension"); err != nil {
		t.Fatalf("the shipped go profile rejected a real module path: %v", err)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // a repository path resolved by the test
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func releaseSetOwnerExtension(name string, ecosystem distribution.Ecosystem) *modelextension.ExtensionDescription {
	flags := map[string]modelextension.FlagDefinition{string(ecosystem): {}}
	return &modelextension.ExtensionDescription{Name: name, Jobs: map[string]*modelextension.JobDefinition{
		"package": {Flags: flags},
		"publish": {Flags: flags},
	}}
}

func releaseSetPublisherExtension(name string, ecosystems ...string) *modelextension.ExtensionDescription {
	return &modelextension.ExtensionDescription{Name: name, Uses: ecosystems, Jobs: map[string]*modelextension.JobDefinition{
		"package": {},
		"publish": {},
	}}
}

// D14: `publish --channel a,b` resolves EVERY listed head in ONE resolve
// before planning. The first is the baseline impact is measured against; every
// other head is kept only as its own compare-and-swap expectation, so a release
// advances two channels that were read at the same instant.
func TestPrepareReleaseSetResolvesEveryListedChannel(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "release-set-channel-advance", "every-listed-head-is-resolved-once")
	ws, _, downstream := releaseSetWorkspace(t)
	useReleaseSetProvenance(t, map[string]string{downstream.ID: digestFor('9')})

	// canary has a head; next has none. Both are answered by one resolve.
	heads := releaseSetHead(t)
	heads["next"] = nil
	provider := &fakeReleaseSetProvider{heads: heads}
	useReleaseSetProviderFake(t, provider)

	options := releaseSetRequest()
	options.Channels = []string{"canary", "next"}
	run, err := prepareReleaseSet(t, options, ws, ws.Projects, providerDiscovery())
	if err != nil || run == nil {
		t.Fatalf("multi-channel preparation = %+v, %v", run, err)
	}
	if provider.resolveCalls != 1 || len(provider.resolveRequests) != 1 {
		t.Fatalf("resolve calls = %d, want exactly one for both channels", provider.resolveCalls)
	}
	if got := provider.resolveRequests[0].Channels; len(got) != 2 || got[0] != "canary" || got[1] != "next" {
		t.Fatalf("resolve request channels = %v, want both in the order named", got)
	}
	if len(run.plan.Heads) != 2 || run.plan.Heads["next"] != nil {
		t.Fatalf("plan heads = %+v, want one entry per channel with next empty", run.plan.Heads)
	}
	// The BASELINE is the first channel's head, so the unchanged upstream is
	// still inherited even though `next` has no head at all.
	if run.HeadRef() == nil || *run.HeadRef() != heads["canary"].Ref {
		t.Fatalf("baseline = %+v, want canary's head", run.HeadRef())
	}
	if len(run.plan.SelectedMembers()) != 1 {
		t.Fatalf("selected members = %+v, want only the changed downstream", run.plan.SelectedMembers())
	}

	// One release advances both channels, each from its own expectation.
	results := publishedResults(run.plan, digestFor('c'))
	run.Finalizer(context.Background())(results)
	if result := results[releaseSetResultKey]; result == nil || result.Status != "success" {
		t.Fatalf("multi-channel release = %+v", result)
	}
	request := provider.releaseRequest
	if len(request.Channels) != 2 || request.Channels[0].Name != "canary" || request.Channels[1].Name != "next" {
		t.Fatalf("release channels = %+v", request.Channels)
	}
	if request.Channels[0].Expected == nil || *request.Channels[0].Expected != heads["canary"].Ref {
		t.Fatalf("canary expectation = %+v, want its own resolved head", request.Channels[0].Expected)
	}
	if request.Channels[1].Expected != nil {
		t.Fatalf("next expectation = %+v, want null for an empty channel", request.Channels[1].Expected)
	}
	outcome := releaseSetOutcomeOf(t, results)
	if outcome.Channels["canary"] == nil || outcome.Channels["next"] == nil {
		t.Fatalf("outcome channels = %+v, want both advanced channels", outcome.Channels)
	}
}

// The channel list is parsed once, at the CLI boundary: unusable names are
// refused before anything is packaged.
func TestParseReleaseSetChannels(t *testing.T) {
	got, err := ParseReleaseSetChannels(" canary , next ,canary")
	if err != nil || len(got) != 2 || got[0] != "canary" || got[1] != "next" {
		t.Fatalf("ParseReleaseSetChannels = %v, %v; want the unique names in order", got, err)
	}
	if got, err := ParseReleaseSetChannels("  "); err != nil || got != nil {
		t.Fatalf("empty channel flag = %v, %v; want no channels and no error", got, err)
	}
	if _, err := ParseReleaseSetChannels("Canary"); err == nil {
		t.Fatal("a non-portable channel name was accepted")
	}
	if _, err := ParseReleaseSetChannels(","); err == nil {
		t.Fatal("a list that names no channel was accepted")
	}
	many := make([]string, distribution.MaxChannelsPerRelease+1)
	for index := range many {
		many[index] = fmt.Sprintf("c%d", index)
	}
	if _, err := ParseReleaseSetChannels(strings.Join(many, ",")); err == nil {
		t.Fatal("a list beyond the protocol bound was accepted")
	}
}

func releaseSetOutcomeOf(t *testing.T, results map[string]*JobResult) distribution.ReleaseSetPublishOutcome {
	t.Helper()
	outcome, ok := results[releaseSetResultKey].Data[runtimeproto.ReleaseSetResultDataKey].(distribution.ReleaseSetPublishOutcome)
	if !ok {
		t.Fatalf("release-set outcome = %+v", results[releaseSetResultKey])
	}
	return outcome
}

// shippedExtensionName reads the name discovery would give an extension whose
// manifest declares none: the name of its putnami.json project document.
func shippedExtensionName(t *testing.T, extensionRoot string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(extensionRoot, "putnami.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if document.Name == "" {
		t.Fatalf("%s declares no project name", extensionRoot)
	}
	return document.Name
}

// The shipped TypeScript extension is what OWNS the npm ecosystem. Nothing in
// the CLI names npm any more — the registry is built from installed manifests —
// so this is the pin that keeps the release path working: if the manifest ever
// loses its profile, or another extension claims the id, a release stops being
// able to find the package and publish jobs of an npm member.
func TestReleaseSetScopePlanningResolvesTheShippedNpmOwner(t *testing.T) {
	root := findJobsRepoRoot(t)
	path := filepath.Join(root, filepath.FromSlash("typescript/extension/putnami.extension.json"))
	manifest, err := extension.LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	resolved := extension.Resolve(manifest, filepath.Dir(path))
	// Discovery names an extension from its project document when the manifest
	// carries no name of its own, which is the shipped shape here.
	resolved.Name = shippedExtensionName(t, filepath.Dir(path))

	registry, err := extension.ResolveInstalledProfiles(&extension.DiscoveryResult{
		Extensions: []*extension.ExtensionDescription{resolved},
	})
	if err != nil {
		t.Fatalf("resolve profiles: %v", err)
	}
	profile, owner, known := registry.Profile("npm")
	if !known {
		t.Fatalf("npm is not declared by any shipped manifest; ids = %v", registry.IDs())
	}
	if owner != "@putnami/typescript" {
		t.Fatalf("npm owner = %q, want @putnami/typescript", owner)
	}
	// The owner must declare BOTH jobs a member needs: scope planning binds the
	// package task and the publish task of the profile's owner.
	for _, command := range []string{"package", profile.Publish} {
		if _, declared := resolved.Commands[command]; !declared {
			t.Fatalf("%s does not declare the %q command a release-set member needs", resolved.Name, command)
		}
	}
	if !registry.HasNativeChannel("npm") {
		t.Fatal("npm must project channels natively: a dist-tag is the npm spelling of a channel")
	}
	if err := registry.ValidateCoordinate("npm", "@putnami/web"); err != nil {
		t.Fatalf("shipped npm coordinate rejected: %v", err)
	}
	// `uses` is what makes the probe's oci member legitimate: the SDK owns that
	// profile because both language extensions publish images through it.
	if !slices.Contains(resolved.Uses, "oci") {
		t.Fatalf("%s uses = %v, want oci", resolved.Name, resolved.Uses)
	}
}

func TestPrepareReleaseSetProviderResolutionIsExactAndResolveOnce(t *testing.T) {
	ws, _, downstream := releaseSetWorkspace(t)
	useReleaseSetProvenance(t, map[string]string{downstream.ID: digestFor('9')})
	provider := &fakeReleaseSetProvider{heads: releaseSetHead(t)}
	useReleaseSetProviderFake(t, provider)

	_, err := prepareReleaseSet(t, releaseSetRequest(), ws, ws.Projects, &extension.DiscoveryResult{})
	if err == nil || !errors.Is(err, releaseset.ErrProviderAbsent) {
		t.Fatalf("absent provider error = %v", err)
	}
	legacyOnly := &modelextension.ExtensionDescription{Name: "legacy", Commands: map[string]string{"release-set": "wrong identity"}}
	_, err = prepareReleaseSet(t, releaseSetRequest(), ws, ws.Projects, &extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{legacyOnly}})
	if err == nil || !errors.Is(err, releaseset.ErrProviderAbsent) {
		t.Fatalf("legacy command should not resolve provider: %v", err)
	}
	run, err := prepareReleaseSet(t, releaseSetRequest(), ws, ws.Projects, providerDiscovery())
	if err != nil || run == nil {
		t.Fatalf("prepare with provider = %+v, %v", run, err)
	}
	if provider.resolveCalls != 1 {
		t.Fatalf("resolve calls = %d, want exactly 1", provider.resolveCalls)
	}

	other := &modelextension.ExtensionDescription{Name: "other", Commands: map[string]string{distribution.ProviderCommandName: "release sets"}}
	discovery := providerDiscovery()
	discovery.Extensions = append(discovery.Extensions, other)
	_, err = prepareReleaseSet(t, releaseSetRequest(), ws, ws.Projects, discovery)
	if err == nil || !errors.Is(err, modelextension.ErrProviderAmbiguous) {
		if err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("multiple-provider error = %v", err)
		}
	}
}

func TestPrepareReleaseSetEmptyChannelReleasesWithNullExpectation(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "release-set-channel-advance", "an-empty-channel-releases-with-a-null-expectation")
	ws, _, _ := releaseSetWorkspace(t)
	useReleaseSetProvenance(t, nil)
	provider := &fakeReleaseSetProvider{heads: emptyHead("canary")}
	useReleaseSetProviderFake(t, provider)

	// Provider absence preserves the legacy full publication path.
	run, err := prepareReleaseSet(t, releaseSetAllOptions(), ws, ws.Projects, &extension.DiscoveryResult{})
	if err != nil || run != nil {
		t.Fatalf("cloudless full preparation = %+v, %v; want legacy nil", run, err)
	}

	// The channel has no head: resolve answers null, every member is selected,
	// and the release expects no head.
	for name, options := range map[string]ReleaseSetOptions{"all": releaseSetAllOptions(), "impacted": releaseSetRequest()} {
		t.Run(name, func(t *testing.T) {
			provider.resolveCalls, provider.releaseCalls, provider.releaseRequest = 0, 0, nil
			run, err := prepareReleaseSet(t, options, ws, ws.Projects, providerDiscovery())
			if err != nil || run == nil || run.plan.Baseline() != nil || !run.plan.SelectsEveryMember() {
				t.Fatalf("empty-channel preparation = %+v, %v", run, err)
			}
			if provider.resolveCalls != 1 {
				t.Fatalf("resolved the channel %d time(s), want exactly once", provider.resolveCalls)
			}
			results := publishedResults(run.plan, digestFor('c'))
			run.Finalizer(context.Background())(results)
			if result := results[releaseSetResultKey]; result == nil || result.Status != "success" {
				t.Fatalf("empty-channel result = %+v", result)
			}
			if provider.releaseRequest == nil || len(provider.releaseRequest.Channels) != 1 ||
				provider.releaseRequest.Channels[0].Expected != nil {
				t.Fatalf("empty-channel release expected = %+v, want null", provider.releaseRequest)
			}
		})
	}
}

func TestReleaseConflictFailsWithoutOutcome(t *testing.T) {
	ws, _, downstream := releaseSetWorkspace(t)
	useReleaseSetProvenance(t, map[string]string{downstream.ID: digestFor('9')})
	head := releaseSetHead(t)
	observed := distribution.ReleaseSetRef{ID: "rs_" + strings.Repeat("e", 64), Digest: digestFor('e')}
	provider := &fakeReleaseSetProvider{
		heads: head, releaseOutcome: distribution.ReleaseOutcomeConflict,
		releaseCurrent: map[string]*distribution.ChannelHead{"canary": {Ref: observed, Generation: 9}},
	}
	useReleaseSetProviderFake(t, provider)
	run, err := prepareReleaseSet(t, releaseSetRequest(), ws, ws.Projects, providerDiscovery())
	if err != nil {
		t.Fatal(err)
	}
	results := publishedResults(run.plan, digestFor('c'))
	run.Finalizer(context.Background())(results)
	assertReleaseSetFailure(t, results)
	message := results[releaseSetResultKey].Error.Message
	for _, want := range []string{"no channel was advanced", "canary: expected " + head["canary"].Ref.ID, "observed " + observed.ID} {
		if !strings.Contains(message, want) {
			t.Fatalf("conflict message = %q, want it to contain %q", message, want)
		}
	}
}

func TestPrepareReleaseSetOnExistingHeadReleasesFromResolvedRef(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "release-set-channel-advance", "every-listed-head-is-resolved-once")
	ws, _, _ := releaseSetWorkspace(t)
	useReleaseSetProvenance(t, nil)
	head := releaseSetHead(t)
	provider := &fakeReleaseSetProvider{heads: head}
	useReleaseSetProviderFake(t, provider)

	run, err := prepareReleaseSet(t, releaseSetAllOptions(), ws, ws.Projects, providerDiscovery())
	if err != nil || run == nil || run.plan == nil {
		t.Fatalf("full preparation over an existing head = %+v, %v", run, err)
	}
	if provider.resolveCalls != 1 {
		t.Fatalf("full publish resolved the channel %d time(s), want exactly once", provider.resolveCalls)
	}
	if run.HeadRef() == nil || *run.HeadRef() != head["canary"].Ref || run.plan.Baseline() == nil {
		t.Fatalf("full plan over an existing head = %+v; want the resolved ref as its CAS base", run.plan)
	}
	if !run.plan.SelectsEveryMember() || len(run.plan.Members) != 2 {
		t.Fatalf("full plan members = %+v; want every member selected", run.plan.Members)
	}
	for _, member := range run.plan.Members {
		if member.Version != "1.1.0" || member.ArtifactDigest != "" {
			t.Fatalf("full plan member %+v; want the candidate version with no inherited digest", member)
		}
	}

	results := publishedResults(run.plan, digestFor('f'))
	run.Finalizer(context.Background())(results)
	result := results[releaseSetResultKey]
	if result == nil || result.Status != "success" {
		t.Fatalf("full publish over an existing head = %+v", result)
	}
	expected := provider.releaseRequest.Channels[0].Expected
	if provider.releaseRequest == nil || expected == nil || *expected != head["canary"].Ref {
		t.Fatalf("full publish CAS expected = %+v, want the resolved head %+v", provider.releaseRequest, head["canary"].Ref)
	}
	outcome := result.Data[runtimeproto.ReleaseSetResultDataKey].(distribution.ReleaseSetPublishOutcome)
	if outcome.Ref == head["canary"].Ref {
		t.Fatal("full publish released the head it started from")
	}
	if provider.releaseOutcomes[0] != distribution.ReleaseOutcomeReleased {
		t.Fatalf("release outcome = %v, want released", provider.releaseOutcomes[0])
	}
}

func TestReleaseCandidateVersionMatchesPackageFallbackWhenMissing(t *testing.T) {
	project := &workspace.Project{ID: "/library", Name: "@putnami/library", Type: "library", Metadata: releaseSetProjectMetadata(t, "npm", "@putnami/library")}
	ws := workspace.NewWorkspace(t.TempDir(), &wsproto.Config{Name: "putnami"}, []*workspace.Project{project})
	stamped := ReleaseSetOptions{Channels: []string{"canary"}, Impacted: true, Profiles: testProfiles(),
		Versions: RunVersions{"": &JobContextVersion{Base: "0.0.0", Full: "0.0.0-next", Suffix: "next"}}}
	plan := buildTestPlan(t, stamped, ws, nil)
	if got := plan.Members[0].Version; got != "0.0.0-next" {
		t.Fatalf("candidate version = %q, want package fallback 0.0.0-next", got)
	}
	bare := buildTestPlan(t, ReleaseSetOptions{Channels: []string{"stable"}, Impacted: true, Profiles: testProfiles()}, ws, nil)
	if got := bare.Members[0].Version; got != "0.0.0" {
		t.Fatalf("candidate version without a version stamp = %q, want package fallback 0.0.0", got)
	}
}

// A candidate version's SHAPE belongs to the ecosystem profile: the go profile
// refuses a bare semver, so a Go module member takes the "v"-prefixed spelling
// without the CLI naming the go ecosystem anywhere.
func TestReleaseCandidateVersionFollowsTheEcosystemProfile(t *testing.T) {
	module := &workspace.Project{
		ID: "/module", Name: "go.putnami.dev/module", Version: "1.4.0", Type: "library",
		Metadata: releaseSetProjectMetadata(t, "go", "go.putnami.dev/module"),
	}
	ws := workspace.NewWorkspace(t.TempDir(), &wsproto.Config{Name: "putnami"}, []*workspace.Project{module})
	plan := buildTestPlan(t, releaseSetRequest(), ws, nil)
	if got := plan.Members[0].Version; got != "v1.4.0" {
		t.Fatalf("go candidate version = %q, want the profile's v-prefixed spelling", got)
	}
}

// Publishability is what the extension probe RECORDED, never a shape the CLI
// infers from project config: `project.publish` and `options.publish.<eco>` no
// longer make a project a release-set member, and a project that declares
// members through its metadata is one whatever its config says.
func TestReleaseProjectsIgnoresAuthoredPublishForms(t *testing.T) {
	authored := &workspace.Project{
		ID: "/runtime/libs/runtime", Name: "@putnami/cloud", Type: "library",
		Publish: []string{"npm"},
		Config:  &wsproto.ProjectConfig{Options: map[string]modelextension.ParamMap{"publish": {"npm": true}}},
	}
	candidates, err := releaseProjects([]*workspace.Project{authored}, testProfiles())
	if err != nil || len(candidates) != 0 {
		t.Fatalf("authored publish forms produced %+v, %v; want no member", candidates, err)
	}

	declared := *authored
	declared.Publish = nil
	declared.Metadata = releaseSetProjectMetadata(t, "npm", "@putnami/cloud")
	ws := workspace.NewWorkspace(t.TempDir(), &wsproto.Config{Name: "putnami"}, []*workspace.Project{&declared})
	useReleaseSetProvenance(t, nil)
	run := prepareReleaseSetWithHead(t, releaseSetAllOptions(), ws, emptyHead("canary"))
	if run.Plan() == nil || len(run.Plan().Members) != 1 {
		t.Fatalf("declared member plan = %+v, want one release-set member", run)
	}
	member := run.Plan().Members[0]
	if member.Ecosystem != "npm" || member.Coordinate != "@putnami/cloud" || member.Version != "0.0.0" || !member.Selected {
		t.Fatalf("declared member = %+v", member)
	}
}

func TestAttachReleaseSetPlanUsesOneSDKOwnedContextOnPackageAndPublish(t *testing.T) {
	ws, upstream, downstream := releaseSetWorkspace(t)
	useReleaseSetProvenance(t, map[string]string{downstream.ID: digestFor('9')})
	plan := prepareReleaseSetWithHead(t, releaseSetRequest(), ws, releaseSetHead(t)).plan
	shared := &modelextension.JobDefinition{}
	if err := json.Unmarshal([]byte(`{"BoundParams":{"existing":true}}`), shared); err != nil {
		t.Fatal(err)
	}
	planned := []*ScheduledJob{
		{Project: upstream, JobDef: &modelextension.JobDefinition{Name: "package~npm", CommandName: "package"}},
		{Project: downstream, JobDef: shared},
		{Project: downstream, JobDef: &modelextension.JobDefinition{Name: "publish~npm", CommandName: "publish"}},
	}
	planned[1].JobDef.Name = "package~npm"
	// The package job is a transitive pipeline dependency and therefore relies
	// on its `package~npm` plan name fallback rather than a stamped CommandName.
	if err := AttachReleaseSetPlan(planned, plan); err != nil {
		t.Fatal(err)
	}
	for _, job := range planned {
		raw, err := json.Marshal(job.JobDef.BoundParams[releaseset.ContextParamName])
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := releaseset.ParseParams(map[string]json.RawMessage{releaseset.ContextParamName: raw})
		if err != nil || parsed == nil {
			t.Fatalf("attached context = %+v, %v", parsed, err)
		}
		member, ok := parsed.Member(distribution.Ecosystem("npm"), job.Project.Name)
		if !ok {
			t.Fatalf("attached plan has no member for %s", job.Project.Name)
		}
		if job.Project == upstream && member.Selected {
			t.Fatal("unchanged upstream was marked selected")
		}
	}
	if _, mutated := shared.BoundParams[releaseset.ContextParamName]; mutated {
		t.Fatal("attachment mutated the shared discovered job definition")
	}
	if err := AttachReleaseSetPlan(planned[:1], plan); err == nil || !strings.Contains(err.Error(), "both package and publish") {
		t.Fatalf("missing publish job error = %v", err)
	}
}

func TestReleaseSetAttachPlanRoutesOneProjectToExactSelectedArtifactSteps(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "release-set-members-by-ecosystem", "selected-member-routes-exact-artifact-steps")
	project := &workspace.Project{ID: "/service", Name: "@putnami/service"}
	publisher := &modelextension.ExtensionDescription{Name: "@putnami/typescript"}
	plan := &releaseset.Plan{Members: []releaseset.PlannedMember{
		{Ecosystem: "npm", Coordinate: "@putnami/service", ProjectID: project.ID, Selected: true},
		{Ecosystem: "oci", Coordinate: "putnami/service", ProjectID: project.ID, Selected: false},
	}}
	run := &ReleaseSetRun{
		plan: plan,
		routes: map[string]releaseMemberRoute{
			releaseset.MemberKey("npm", "@putnami/service"): {
				projectID: project.ID, publisher: publisher.Name, packagePublisher: publisher.Name,
				packageStep: "npm", publishCommand: "publish", publishStep: "npm",
			},
			releaseset.MemberKey("oci", "putnami/service"): {
				projectID: project.ID, publisher: publisher.Name, packagePublisher: publisher.Name,
				packageStep: "docker", publishCommand: "publish", publishStep: "docker",
			},
		},
	}
	job := func(command, step string) *ScheduledJob {
		return &ScheduledJob{
			Project: project, Extension: publisher,
			JobDef: &modelextension.JobDefinition{Name: command + "~" + step, CommandName: command, StepID: step},
		}
	}
	packageNPM := job("package", "npm")
	packageDocker := job("package", "docker")
	publishNPM := job("publish", "npm")
	publishDocker := job("publish", "docker")
	deploy := &ScheduledJob{
		Project: project, Extension: &modelextension.ExtensionDescription{Name: "@putnami/cloud"},
		JobDef:    &modelextension.JobDefinition{Name: "deploy~service", CommandName: "deploy", StepID: "service"},
		DependsOn: []string{publishNPM.Key(), publishDocker.Key()},
	}

	scoped, err := run.AttachPlan([]*ScheduledJob{packageNPM, packageDocker, publishNPM, publishDocker, deploy})
	if err != nil {
		t.Fatal(err)
	}
	keys := make(map[string]bool, len(scoped))
	for _, planned := range scoped {
		keys[planned.Key()] = true
	}
	if keys[packageDocker.Key()] || keys[publishDocker.Key()] {
		t.Fatalf("unselected OCI steps survived: %v", keys)
	}
	if !keys[packageNPM.Key()] || !keys[publishNPM.Key()] {
		t.Fatalf("selected npm steps missing: %v", keys)
	}
	if !slices.Equal(deploy.DependsOn, []string{publishNPM.Key()}) {
		t.Fatalf("deploy dependencies = %v, want only selected publication %q", deploy.DependsOn, publishNPM.Key())
	}
	for _, planned := range []*ScheduledJob{packageNPM, publishNPM} {
		if planned.JobDef.BoundParams[releaseset.ContextParamName] == nil {
			t.Fatalf("%s has no release-set context", planned.Key())
		}
	}
	wantPublishJobKeys := map[string]string{publishNPM.Key(): releaseset.MemberKey("npm", "@putnami/service")}
	if !maps.Equal(run.publishJobKeys, wantPublishJobKeys) {
		t.Fatalf("recorded publish job keys = %v, want %v", run.publishJobKeys, wantPublishJobKeys)
	}

	ociKey := releaseset.MemberKey(distribution.Ecosystem("oci"), "putnami/service")
	ociRoute := run.routes[ociKey]
	ociRoute.publishStep = "npm"
	run.routes[ociKey] = ociRoute
	_, err = run.AttachPlan(nil)
	if err == nil || !strings.Contains(err.Error(), "cannot be selected independently") {
		t.Fatalf("shared publish route error = %v", err)
	}
	ociRoute.publishStep = "docker"
	run.routes[ociKey] = ociRoute

	_, err = run.AttachPlan([]*ScheduledJob{publishNPM})
	if err == nil || !strings.Contains(err.Error(), `package steps "npm"`) {
		t.Fatalf("missing exact package route error = %v", err)
	}
}

// A workload declares an image member and a migration member in one project;
// the migration member's package step is the same describe/generate node the
// project's shared build step depends on. A commit that re-selects only the
// image member must not remove that node: it is local preparation a kept job
// needs, never a registry write. Only the unselected member's publication
// goes, and the edges that pointed at it.
func TestReleaseSetAttachPlanKeepsUnselectedPackageStepAKeptJobNeeds(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "release-set-members-by-ecosystem", "selected-member-routes-exact-artifact-steps")
	project := &workspace.Project{ID: "/service", Name: "@putnami/service"}
	publisher := &modelextension.ExtensionDescription{Name: "@putnami/typescript"}
	run := &ReleaseSetRun{
		plan: &releaseset.Plan{Members: []releaseset.PlannedMember{
			{Ecosystem: "oci", Coordinate: "putnami/service", ProjectID: project.ID, Selected: true},
			{Ecosystem: "put", Coordinate: "putnami/service-migration", ProjectID: project.ID, Selected: false},
		}},
		routes: map[string]releaseMemberRoute{
			releaseset.MemberKey("oci", "putnami/service"): {
				projectID: project.ID, publisher: publisher.Name, packagePublisher: publisher.Name,
				packageStep: "docker", publishCommand: "publish", publishStep: "docker",
			},
			releaseset.MemberKey("put", "putnami/service-migration"): {
				projectID: project.ID, publisher: publisher.Name, packagePublisher: publisher.Name,
				packageStep: "generate", publishCommand: "publish", publishStep: "migration",
			},
		},
	}
	job := func(command, step string, dependsOn ...string) *ScheduledJob {
		return &ScheduledJob{
			Project: project, Extension: publisher,
			JobDef:    &modelextension.JobDefinition{Name: command + "~" + step, CommandName: command, StepID: step},
			DependsOn: dependsOn,
		}
	}
	generate := job("package", "generate")
	compile := job("build", "compile", generate.Key())
	packageDocker := job("package", "docker", compile.Key())
	publishDocker := job("publish", "docker", packageDocker.Key())
	publishMigration := job("publish", "migration", generate.Key())
	publishMigration.SerializeAfter = []string{publishDocker.Key()}
	deploy := &ScheduledJob{
		Project: project, Extension: &modelextension.ExtensionDescription{Name: "@putnami/cloud"},
		JobDef:         &modelextension.JobDefinition{Name: "deploy~service", CommandName: "deploy", StepID: "service"},
		DependsOn:      []string{publishDocker.Key(), publishMigration.Key()},
		SerializeAfter: []string{publishMigration.Key()},
	}

	scoped, err := run.AttachPlan([]*ScheduledJob{generate, compile, packageDocker, publishDocker, publishMigration, deploy})
	if err != nil {
		t.Fatal(err)
	}
	kept := make(map[string]bool, len(scoped))
	for _, planned := range scoped {
		kept[planned.Key()] = true
	}
	if kept[publishMigration.Key()] {
		t.Fatalf("unselected migration publication survived: %v", kept)
	}
	for _, retained := range []*ScheduledJob{generate, compile, packageDocker, publishDocker, deploy} {
		if !kept[retained.Key()] {
			t.Fatalf("%s was dropped: the kept build step depends on it", retained.Key())
		}
	}
	if !slices.Equal(compile.DependsOn, []string{generate.Key()}) {
		t.Fatalf("compile dependencies = %v, want the retained package step %q", compile.DependsOn, generate.Key())
	}
	if !slices.Equal(deploy.DependsOn, []string{publishDocker.Key()}) || len(deploy.SerializeAfter) != 0 {
		t.Fatalf("deploy edges = %v / %v, want only the selected publication", deploy.DependsOn, deploy.SerializeAfter)
	}
	for _, planned := range scoped {
		for _, key := range slices.Concat(planned.DependsOn, planned.SerializeAfter) {
			if !kept[key] {
				t.Fatalf("kept job %s still names dropped %s", planned.Key(), key)
			}
		}
	}
	if generate.JobDef.BoundParams[releaseset.ContextParamName] == nil {
		t.Fatalf("retained package step %s carries no release-set context", generate.Key())
	}

	// The same unselected package step goes when only its own dropped
	// publication needed it: nothing the plan keeps reaches it, so the
	// selection still spares the work of an artifact nobody publishes.
	orphan := job("package", "generate")
	orphanPublish := job("publish", "migration", orphan.Key())
	compileAlone := job("build", "compile")
	dockerAlone := job("package", "docker", compileAlone.Key())
	scoped, err = run.AttachPlan([]*ScheduledJob{orphan, orphanPublish, compileAlone, dockerAlone, job("publish", "docker", dockerAlone.Key())})
	if err != nil {
		t.Fatal(err)
	}
	for _, planned := range scoped {
		if planned == orphan || planned == orphanPublish {
			t.Fatalf("%s survived with nothing left to need it", planned.Key())
		}
	}
	if len(scoped) != 3 {
		t.Fatalf("scoped = %d jobs, want the build, package and publish chain of the selected member", len(scoped))
	}
}

func TestReleaseSetAttachPlanRoutesPackageAndPublicationToDistinctProviders(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "release-set-members-by-ecosystem", "distinct-package-provider-and-publication-owner-survive-final-scope")
	project := &workspace.Project{ID: "/cloud", Name: "@putnami/cloud"}
	packager := &modelextension.ExtensionDescription{Name: "@putnami/go"}
	publisher := &modelextension.ExtensionDescription{Name: "@putnami/cloud"}
	memberKey := releaseset.MemberKey("archive", "putnami/cloud")
	run := &ReleaseSetRun{
		plan: &releaseset.Plan{Members: []releaseset.PlannedMember{{
			Ecosystem: "archive", Coordinate: "putnami/cloud", ProjectID: project.ID, Selected: true,
		}}},
		routes: map[string]releaseMemberRoute{memberKey: {
			projectID: project.ID, publisher: publisher.Name,
			packagePublisher: packager.Name, packageStep: "archives",
			publishCommand: "cloud-publish-archives", publishStep: "cloud-publish-archives",
		}},
	}
	job := func(owner *modelextension.ExtensionDescription, command, step string) *ScheduledJob {
		return &ScheduledJob{
			Project: project, Extension: owner,
			JobDef: &modelextension.JobDefinition{Name: command + "~" + step, CommandName: command, StepID: step},
		}
	}
	packageJob := job(packager, "package", "archives")
	publishJob := job(publisher, "cloud-publish-archives", "cloud-publish-archives")

	planned, err := run.AttachPlan([]*ScheduledJob{packageJob, publishJob})
	if err != nil {
		t.Fatal(err)
	}
	if len(planned) != 2 || planned[0] != packageJob || planned[1] != publishJob {
		t.Fatalf("planned = %+v, want the package producer and publication owner exactly once", planned)
	}
}

func TestReleaseSetFinalizerOutcomesPartialDuplicateAndDryRun(t *testing.T) {
	ws, _, downstream := releaseSetWorkspace(t)
	useReleaseSetProvenance(t, map[string]string{downstream.ID: digestFor('9')})
	plan := prepareReleaseSetWithHead(t, releaseSetRequest(), ws, releaseSetHead(t)).plan
	validResults := publishedResults(plan, digestFor('c'))

	t.Run("released emits exactly one typed outcome", func(t *testing.T) {
		provider := &fakeReleaseSetProvider{}
		results := cloneResults(validResults)
		(&ReleaseSetRun{plan: plan, provider: provider}).Finalizer(context.Background())(results)
		result := results[releaseSetResultKey]
		if result == nil || result.Status != "success" || len(result.Data) != 1 || result.Data[runtimeproto.ReleaseSetResultDataKey] == nil {
			t.Fatalf("coordinator result = %+v", result)
		}
		if provider.releaseCalls != 1 {
			t.Fatalf("provider release calls = %d", provider.releaseCalls)
		}
		outcome := result.Data[runtimeproto.ReleaseSetResultDataKey].(distribution.ReleaseSetPublishOutcome)
		if outcome.ProtocolVersion != distribution.ProtocolVersion || outcome.Channels["canary"] == nil {
			t.Fatalf("outcome = %+v", outcome)
		}
		released := provider.releaseRequest.ReleaseSet
		for _, member := range released.Members {
			if member.SourceRevision != testRevision || member.SelectionFingerprint == "" {
				t.Fatalf("released member lost provenance: %+v", member)
			}
			if member.Coordinate == downstream.Name && member.ArtifactDigest != digestFor('c') {
				t.Fatalf("released downstream digest = %q", member.ArtifactDigest)
			}
		}
	})

	t.Run("already-current is idempotent success", func(t *testing.T) {
		provider := &fakeReleaseSetProvider{releaseOutcome: distribution.ReleaseOutcomeAlreadyCurrent}
		results := cloneResults(validResults)
		(&ReleaseSetRun{plan: plan, provider: provider}).Finalizer(context.Background())(results)
		if result := results[releaseSetResultKey]; result == nil || result.Status != "success" {
			t.Fatalf("idempotent result = %+v", result)
		}
	})

	t.Run("conflict fails without outcome", func(t *testing.T) {
		provider := &fakeReleaseSetProvider{releaseOutcome: distribution.ReleaseOutcomeConflict}
		results := cloneResults(validResults)
		(&ReleaseSetRun{plan: plan, provider: provider}).Finalizer(context.Background())(results)
		assertReleaseSetFailure(t, results)
	})

	t.Run("partial publication never writes", func(t *testing.T) {
		provider := &fakeReleaseSetProvider{}
		results := map[string]*JobResult{"down:publish": {Status: "success"}}
		(&ReleaseSetRun{plan: plan, provider: provider}).Finalizer(context.Background())(results)
		assertReleaseSetFailure(t, results)
		if provider.releaseCalls != 0 {
			t.Fatalf("partial publish called provider: %+v", provider)
		}
	})

	// A scheduler-skipped publish job is the one terminal status that reaches
	// the commit with no failure in the session: every publish~* skipped as
	// "dependency failed" behind a withheld capability grant. The finalizer
	// must name the job and its cause rather than report bare missing
	// records.
	t.Run("skipped publish job fails loudly with its cause", func(t *testing.T) {
		provider := &fakeReleaseSetProvider{}
		results := cloneResults(validResults)
		publishJobKeys := make(map[string]string)
		var skippedKey string
		for _, member := range plan.SelectedMembers() {
			skippedKey = member.ProjectID + ":publish"
			publishJobKeys[skippedKey] = releaseset.MemberKey(member.Ecosystem, member.Coordinate)
		}
		results[skippedKey] = &JobResult{Status: "skipped", Error: &JobError{Message: "dependency failed"}}
		run := &ReleaseSetRun{plan: plan, provider: provider, publishJobKeys: publishJobKeys}
		run.Finalizer(context.Background())(results)
		assertReleaseSetFailure(t, results)
		if provider.releaseCalls != 0 {
			t.Fatalf("skipped publish called provider: %+v", provider)
		}
		message := results[releaseSetResultKey].Error.Message
		for _, want := range []string{"publish job(s) were skipped", skippedKey, "dependency failed"} {
			if !strings.Contains(message, want) {
				t.Fatalf("skipped publish failure %q does not name %q", message, want)
			}
		}
		if strings.Contains(message, "missing verified records") {
			t.Fatalf("skipped publish failure fell through to the generic partial-publication error: %q", message)
		}
	})

	t.Run("pre-existing policy failure never writes", func(t *testing.T) {
		provider := &fakeReleaseSetProvider{}
		results := cloneResults(validResults)
		results["downstream:specs~verify"] = &JobResult{Status: "failed"}
		(&ReleaseSetRun{plan: plan, provider: provider}).Finalizer(context.Background())(results)
		if provider.releaseCalls != 0 {
			t.Fatalf("failed session gate called provider: %+v", provider)
		}
		if results[releaseSetResultKey] != nil {
			t.Fatalf("failed session gate emitted release-set outcome: %+v", results[releaseSetResultKey])
		}
	})

	t.Run("malformed digest never writes", func(t *testing.T) {
		provider := &fakeReleaseSetProvider{}
		results := publishedResults(plan, "sha256:BAD")
		(&ReleaseSetRun{plan: plan, provider: provider}).Finalizer(context.Background())(results)
		assertReleaseSetFailure(t, results)
	})

	// The published-member event is read strictly: a field this build does not
	// know would otherwise be dropped and its member recorded incomplete.
	for name, mutate := range map[string]func(map[string]any){
		"unknown field":     func(data map[string]any) { data["registry"] = "npm" },
		"missing digest":    func(data map[string]any) { delete(data, "artifactDigest") },
		"malformed digest":  func(data map[string]any) { data["artifactDigest"] = "sha256:BAD" },
		"foreign ecosystem": func(data map[string]any) { data["ecosystem"] = "NPM" },
	} {
		t.Run(name, func(t *testing.T) {
			provider := &fakeReleaseSetProvider{}
			results := publishedResults(plan, digestFor('c'))
			for _, result := range results {
				mutate(result.Events[0].Data)
			}
			(&ReleaseSetRun{plan: plan, provider: provider}).Finalizer(context.Background())(results)
			assertReleaseSetFailure(t, results)
			if provider.releaseCalls != 0 {
				t.Fatalf("invalid published member called provider: %+v", provider)
			}
		})
	}

	// The runtime envelope the JSONL reader flattens into every event is NOT a
	// member field, and its presence must not be read as a producer defect.
	t.Run("the flattened runtime envelope is not a member field", func(t *testing.T) {
		provider := &fakeReleaseSetProvider{}
		results := publishedResults(plan, digestFor('c'))
		for _, result := range results {
			result.Events[0].Data["type"] = EventTypeArtifact
			result.Events[0].Data["time"] = "2026-09-04T00:00:00.000Z"
		}
		(&ReleaseSetRun{plan: plan, provider: provider}).Finalizer(context.Background())(results)
		if result := results[releaseSetResultKey]; result == nil || result.Status != "success" {
			t.Fatalf("envelope-bearing event = %+v", result)
		}
	})

	t.Run("duplicate outcome never writes", func(t *testing.T) {
		provider := &fakeReleaseSetProvider{}
		results := cloneResults(validResults)
		rogue := &JobResult{Status: "success"}
		if err := json.Unmarshal([]byte(`{"releaseSet":"rogue"}`), &rogue.Data); err != nil {
			t.Fatal(err)
		}
		results["rogue"] = rogue
		(&ReleaseSetRun{plan: plan, provider: provider}).Finalizer(context.Background())(results)
		assertReleaseSetFailure(t, results)
	})

	t.Run("dry run is display-only", func(t *testing.T) {
		provider := &fakeReleaseSetProvider{}
		results := cloneResults(validResults)
		(&ReleaseSetRun{plan: plan, provider: provider, dryRun: true}).Finalizer(context.Background())(results)
		if provider.releaseCalls != 0 || results[releaseSetResultKey] != nil {
			t.Fatalf("dry run mutated provider/results: provider=%+v result=%+v", provider, results[releaseSetResultKey])
		}
	})
}

func TestReleaseSetModePreservesCloudlessAllAndFailsClosedOnlyForImpactedChannel(t *testing.T) {
	cases := []struct {
		name    string
		options ReleaseSetOptions
		want    ReleaseSetMode
	}{
		{"provider-capable all", ReleaseSetOptions{Commands: []string{"publish"}, All: true, Channels: []string{"canary"}}, ReleaseSetAll},
		{"legacy impacted", ReleaseSetOptions{Commands: []string{"publish"}, Impacted: true}, ReleaseSetDisabled},
		{"impacted channel", releaseSetRequest(), ReleaseSetImpacted},
		{"non publish", ReleaseSetOptions{Commands: []string{"build"}, Impacted: true, Channels: []string{"canary"}}, ReleaseSetDisabled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RequestedReleaseSetMode(tc.options)
			if got != tc.want {
				t.Fatalf("requestedReleaseSetMode = %v, want %v", got, tc.want)
			}
		})
	}
}

// Provenance names the COMMIT, and the tree only when asked: whether a member
// is republished is decided by its selection fingerprint, which the engine
// derives from the package task's execution key, never from a tree read here.
func TestReleaseSetProvenanceReadsGitRevision(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(root, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "lib", "index.ts"), []byte("export const a = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "init")
	head := git("rev-parse", "HEAD")

	project := &workspace.Project{ID: "/lib", Name: "@putnami/lib", Path: "lib"}
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "putnami"}, []*workspace.Project{project})
	provenance, err := realReleaseSetProvenance(ws, false)
	if err != nil {
		t.Fatal(err)
	}
	if provenance.revision != head {
		t.Fatalf("revision = %q, want HEAD %q", provenance.revision, head)
	}
	if _, err := realReleaseSetProvenance(workspace.NewWorkspace(t.TempDir(), &wsproto.Config{Name: "putnami"}, nil), false); err == nil {
		t.Fatal("a workspace outside git produced a source revision")
	}
}

// PUTNAMI_SOURCE_REVISION names the commit a publication is bound to, which a
// runner that publishes a synthetic merge of that commit cannot read from
// HEAD. A commit the repository does not have is dated from
// PUTNAMI_SOURCE_COMMIT_TIME, so the override needs no checkout at all: the
// workspace here is a bare temporary directory. The test sets the environment,
// so it is not parallel.
func TestReleaseSetProvenanceFollowsTheSourceRevisionOverride(t *testing.T) {
	ws := workspace.NewWorkspace(t.TempDir(), &wsproto.Config{Name: "putnami"}, nil)

	t.Setenv(putnamigit.SourceRevisionEnv, testRevision)
	t.Setenv(putnamigit.SourceCommitTimeEnv, "1767323045")
	provenance, err := realReleaseSetProvenance(ws, false)
	if err != nil {
		t.Fatalf("provenance with the override: %v", err)
	}
	if provenance.revision != testRevision {
		t.Fatalf("revision = %q, want the override %q", provenance.revision, testRevision)
	}

	// A SHA-256 id is a valid override but not a Distribution source
	// revision: it meets the same refusal a 64-character HEAD does.
	t.Setenv(putnamigit.SourceRevisionEnv, strings.Repeat("ab", 32))
	if _, err := realReleaseSetProvenance(ws, false); err == nil || !strings.Contains(err.Error(), "requires a full commit revision, got") {
		t.Fatalf("provenance with a 64-character override = %v, want the protocol's shape refusal", err)
	}
	// A malformed override is refused by name, never read as HEAD.
	t.Setenv(putnamigit.SourceRevisionEnv, strings.ToUpper(testRevision))
	if _, err := realReleaseSetProvenance(ws, false); err == nil || !strings.Contains(err.Error(), putnamigit.SourceRevisionEnv) || !strings.Contains(err.Error(), "lowercase hex") {
		t.Fatalf("provenance with an uppercase override = %v, want an error naming the shape", err)
	}
	// Without a usable commit time, the version suffix cannot be stamped for
	// that commit, so the publication is refused rather than recorded under a
	// revision whose version fell back to 0.0.0.
	t.Setenv(putnamigit.SourceRevisionEnv, testRevision)
	for _, commitTime := range []string{"", "0", "yesterday"} {
		t.Setenv(putnamigit.SourceCommitTimeEnv, commitTime)
		if _, err := realReleaseSetProvenance(ws, false); err == nil || !strings.Contains(err.Error(), putnamigit.SourceCommitTimeEnv) {
			t.Fatalf("provenance with %s=%q = %v, want an error naming the variable", putnamigit.SourceCommitTimeEnv, commitTime, err)
		}
	}
	// Unset, a workspace outside git still fails as it does today.
	t.Setenv(putnamigit.SourceRevisionEnv, "")
	if _, err := realReleaseSetProvenance(ws, false); err == nil || !strings.Contains(err.Error(), "requires a git checkout") {
		t.Fatalf("provenance without a checkout = %v, want today's refusal", err)
	}
}

// The override reaches the plan: every member a real-provenance plan builds
// carries it as sourceRevision, and the run keeps the same copy for the
// runner's broker. Only the full-clone check is faked (the fixture is a
// temporary directory); the provenance is the real one. Not parallel: it sets
// the environment.
func TestReleaseSetPlanCarriesTheSourceRevisionOverride(t *testing.T) {
	const bound = "1d8f0c6e2b7a94f35c0e6d1b8a7f4c3e2d1b0a99"
	ws, _, _ := releaseSetWorkspace(t)
	useReleaseSetProvenance(t, nil)
	newReleaseSetProvenance = realReleaseSetProvenance
	t.Setenv(putnamigit.SourceRevisionEnv, bound)
	t.Setenv(putnamigit.SourceCommitTimeEnv, "1767323045")
	provider := &fakeReleaseSetProvider{heads: emptyHead("canary")}
	useReleaseSetProviderFake(t, provider)
	run, err := prepareReleaseSet(t, releaseSetAllOptions(), ws, ws.Projects, providerDiscovery())
	if err != nil {
		t.Fatal(err)
	}
	if run.sourceRevision != bound {
		t.Fatalf("run source revision = %q, want the override %q", run.sourceRevision, bound)
	}
	if len(run.plan.Members) == 0 {
		t.Fatal("the plan selected no member")
	}
	for _, member := range run.plan.Members {
		if member.SourceRevision != bound {
			t.Fatalf("member %s sourceRevision = %q, want the override %q", member.Coordinate, member.SourceRevision, bound)
		}
	}
}

// fakeReleaseSetProvider is a provider double. Release answers released
// (current = the submitted set on every requested channel) unless an outcome is
// forced; a submitted set already expected on every channel answers
// already-current.
type fakeReleaseSetProvider struct {
	heads           map[string]*distribution.ChannelHead
	releaseOutcome  distribution.ReleaseOutcome
	releaseCurrent  map[string]*distribution.ChannelHead
	resolveCalls    int
	resolveRequests []distribution.ResolveRequest
	releaseCalls    int
	releaseRequest  *distribution.ReleaseRequest
	releaseOutcomes []distribution.ReleaseOutcome
	// release answers a ReleaseID lookup: an immutable set is named by no
	// channel, so a deploy that takes --release reads this instead of a head.
	release *distribution.ChannelHead
}

func (provider *fakeReleaseSetProvider) Resolve(_ context.Context, request *distribution.ResolveRequest) (*distribution.ResolveResponse, error) {
	provider.resolveCalls++
	provider.resolveRequests = append(provider.resolveRequests, *request)
	if request.ReleaseID != "" {
		return &distribution.ResolveResponse{ProtocolVersion: distribution.ProtocolVersion, Release: provider.release}, nil
	}
	heads := make(map[string]*distribution.ChannelHead, len(request.Channels))
	for _, name := range request.Channels {
		heads[name] = provider.heads[name]
	}
	return &distribution.ResolveResponse{ProtocolVersion: distribution.ProtocolVersion, Heads: heads}, nil
}

func (provider *fakeReleaseSetProvider) Release(_ context.Context, request *distribution.ReleaseRequest) (*distribution.ReleaseResponse, error) {
	provider.releaseCalls++
	provider.releaseRequest = request
	ref, diagnostics := distribution.DeriveReleaseSetRef(&request.ReleaseSet)
	if len(diagnostics) != 0 {
		return nil, errors.New("invalid release fixture")
	}
	outcome := provider.releaseOutcome
	if outcome == "" {
		outcome = distribution.ReleaseOutcomeReleased
		everyChannelAlreadyAt := len(request.Channels) > 0
		for _, channel := range request.Channels {
			if channel.Expected == nil || *channel.Expected != ref {
				everyChannelAlreadyAt = false
			}
		}
		if everyChannelAlreadyAt {
			outcome = distribution.ReleaseOutcomeAlreadyCurrent
		}
	}
	provider.releaseOutcomes = append(provider.releaseOutcomes, outcome)
	response := &distribution.ReleaseResponse{
		ProtocolVersion: distribution.ProtocolVersion,
		Outcome:         outcome,
		Current:         make(map[string]*distribution.ChannelHead, len(request.Channels)),
	}
	for _, channel := range request.Channels {
		switch {
		case outcome != distribution.ReleaseOutcomeConflict:
			response.Current[channel.Name] = &distribution.ChannelHead{Ref: ref, Generation: 4}
		case provider.releaseCurrent != nil:
			response.Current[channel.Name] = provider.releaseCurrent[channel.Name]
		default:
			response.Current[channel.Name] = nil
		}
	}
	return response, nil
}

func useReleaseSetProviderFake(t *testing.T, provider releaseSetProvider) {
	t.Helper()
	previousFactory := newReleaseSetProvider
	newReleaseSetProvider = func(context.Context, *extension.ResolvedProvider) (releaseSetProvider, error) { return provider, nil }
	t.Cleanup(func() { newReleaseSetProvider = previousFactory })
}

// releaseSetTestOverrides is the per-test map of project id -> selection
// fingerprint. The engine computes these from package-task execution keys; a
// unit test states "this project's recipe moved" by naming it here, which is
// what useReleaseSetProvenance sets up.
var releaseSetTestOverrides map[string]string

// useReleaseSetProvenance replaces the two git preconditions of a release —
// the provenance revision and the full-clone check — with fixed answers, and
// records the selection-fingerprint overrides the option builders below apply:
// testFingerprint(projectID) unless overridden, so a test that overrides
// nothing sees no impact against releaseSetHead.
//
// A fixture workspace is a temporary directory rather than a clone, so both
// seams are replaced together: what the real ones do with a real checkout is
// tested where a real checkout exists.
func useReleaseSetProvenance(t *testing.T, overrides map[string]string) {
	t.Helper()
	previous := newReleaseSetProvenance
	previousClone := requireFullClone
	previousTree := currentReleaseSourceTree
	previousOverrides := releaseSetTestOverrides
	releaseSetTestOverrides = overrides
	newReleaseSetProvenance = func(_ *workspace.Workspace, withTree bool) (*releaseSetProvenance, error) {
		provenance := &releaseSetProvenance{revision: testRevision}
		if withTree {
			provenance.tree = testTree
		}
		return provenance, nil
	}
	requireFullClone = func(string) error { return nil }
	currentReleaseSourceTree = func(string) string { return testTree }
	t.Cleanup(func() {
		newReleaseSetProvenance = previous
		requireFullClone = previousClone
		currentReleaseSourceTree = previousTree
		releaseSetTestOverrides = previousOverrides
	})
}

func testFingerprint(projectID string) string {
	sum := sha256.Sum256([]byte("fingerprint:" + projectID))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// testProfiles is the ecosystem registry the release-set tests resolve
// against: npm owned by "npm-owner", go owned by "go-owner", plus the SDK's
// builtin oci. The CLI names no ecosystem itself, so every test that reaches
// the coordinator has to supply one of these.
var testProfiles = sync.OnceValue(func() *extensionproto.ProfileRegistry {
	owner := func(name, id, versionPattern string) extensionproto.NamedManifest {
		return extensionproto.NamedManifest{Name: name, Manifest: &extensionproto.Manifest{
			Name:     name,
			Commands: map[string]extensionproto.CommandDefinition{"package": {}, "publish": {}},
			Ecosystems: []extensionproto.EcosystemProfile{{
				ID:         id,
				Coordinate: extensionproto.PatternRule{Pattern: `^[A-Za-z0-9@._/-]+$`},
				Version:    extensionproto.VersionRule{Pattern: versionPattern, Ordering: extensionproto.OrderingSemver},
				Channel:    extensionproto.ChannelNative,
				Registries: json.RawMessage(`{"type":"object"}`),
				Publish:    "publish",
			}},
		}}
	}
	registry, diagnostics := extensionproto.ResolveProfiles([]extensionproto.NamedManifest{
		owner("npm-owner", "npm", `^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)*$`),
		owner("go-owner", "go", `^v[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)*$`),
	}, profiles.Builtin())
	if len(diagnostics) != 0 {
		panic(fmt.Sprintf("test ecosystem profiles are invalid: %v", diagnostics))
	}
	return registry
})

// releaseSetTestFingerprints is the map the engine hands the coordinator: one
// selection fingerprint per (ecosystem, coordinate) member of the workspace.
func releaseSetTestFingerprints(t *testing.T, ws *workspace.Workspace) map[string]string {
	t.Helper()
	members, err := ReleaseSetMembers(ws.Projects, testProfiles())
	if err != nil {
		t.Fatalf("release-set members: %v", err)
	}
	fingerprints := make(map[string]string, len(members))
	for key, project := range members {
		if override, ok := releaseSetTestOverrides[project.ID]; ok {
			fingerprints[key] = override
			continue
		}
		fingerprints[key] = testFingerprint(project.ID)
	}
	return fingerprints
}

func providerDiscovery() *extension.DiscoveryResult {
	return &extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{{
		Name: "provider", Commands: map[string]string{distribution.ProviderCommandName: "release sets"},
	}}}
}

// prepareReleaseSet is the one call site tests use: it supplies the workspace
// ecosystem profiles and the selection fingerprints exactly as the engine does.
func prepareReleaseSet(
	t *testing.T,
	options ReleaseSetOptions,
	ws *workspace.Workspace,
	selected []*workspace.Project,
	discovered *extension.DiscoveryResult,
) (*ReleaseSetRun, error) {
	t.Helper()
	if options.Versions == nil {
		options.Versions = testRunVersions(ws)
	}
	return PrepareReleaseSet(context.Background(), options, ws, selected, discovered, releaseSetTestFingerprints(t, ws))
}

// testRunVersions is the run's versions for a fixture workspace: one root line,
// at the version its projects state. The fixtures put the version on the
// projects because that is where a reader looks for it; the run resolves it per
// LINE, so one entry answers for the whole fixture.
func testRunVersions(ws *workspace.Workspace) RunVersions {
	for _, project := range ws.Projects {
		if project != nil && project.Version != "" {
			return RunVersions{"": &JobContextVersion{Base: project.Version, Full: project.Version}}
		}
	}
	return nil
}

func prepareReleaseSetWithHead(t *testing.T, options ReleaseSetOptions, ws *workspace.Workspace, heads map[string]*distribution.ChannelHead) *ReleaseSetRun {
	t.Helper()
	useReleaseSetProviderFake(t, &fakeReleaseSetProvider{heads: heads})
	run, err := prepareReleaseSet(t, options, ws, ws.Projects, providerDiscovery())
	if err != nil || run == nil || run.plan == nil {
		t.Fatalf("prepare release set = %+v, %v", run, err)
	}
	return run
}

func buildTestPlan(t *testing.T, options ReleaseSetOptions, ws *workspace.Workspace, heads map[string]*distribution.ChannelHead) *releaseset.Plan {
	t.Helper()
	useReleaseSetProvenance(t, releaseSetTestOverrides)
	candidates, err := releaseProjects(ws.Projects, testProfiles())
	if err != nil {
		t.Fatal(err)
	}
	provenance, err := newReleaseSetProvenance(ws, releaseMemberSourceTree(options.Policy))
	if err != nil {
		t.Fatal(err)
	}
	options.Fingerprints = releaseSetTestFingerprints(t, ws)
	if options.Versions == nil {
		options.Versions = testRunVersions(ws)
	}
	if heads == nil {
		heads = map[string]*distribution.ChannelHead{options.Channels[0]: nil}
	}
	plan, err := buildReleaseSetPlan(options, ws, candidates, heads, provenance)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func releaseSetWorkspace(t *testing.T) (*workspace.Workspace, *workspace.Project, *workspace.Project) {
	t.Helper()
	upstream := &workspace.Project{ID: "/upstream", Name: "@putnami/upstream", Version: "1.1.0", Type: "library", Metadata: releaseSetProjectMetadata(t, "npm", "@putnami/upstream")}
	downstream := &workspace.Project{ID: "/downstream", Name: "@putnami/downstream", Version: "1.1.0", Type: "library", Dependencies: []string{upstream.Name}, Metadata: releaseSetProjectMetadata(t, "npm", "@putnami/downstream", upstream.Name)}
	ws := workspace.NewWorkspace(t.TempDir(), &wsproto.Config{Name: "putnami"}, []*workspace.Project{upstream, downstream})
	return ws, upstream, downstream
}

// releaseSetProjectMetadata is what an extension probe writes: one member
// declaration per (ecosystem, coordinate) this project contributes.
func releaseSetProjectMetadata(t *testing.T, ecosystem distribution.Ecosystem, coordinate string, dependencies ...string) map[string]json.RawMessage {
	t.Helper()
	return releaseSetProjectMembers(t, releaseset.MemberDeclaration{
		Ecosystem: ecosystem, Coordinate: coordinate, PackageStep: string(ecosystem), PublishStep: string(ecosystem), Dependencies: sortedCopy(dependencies),
	})
}

func releaseSetProjectMembers(t *testing.T, declarations ...releaseset.MemberDeclaration) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(struct {
		ReleaseSet releaseset.ProjectMetadata `json:"releaseSet"`
	}{ReleaseSet: releaseset.ProjectMetadata{Ecosystems: declarations}})
	if err != nil {
		t.Fatal(err)
	}
	return map[string]json.RawMessage{"test-provider": raw}
}

func sortedCopy(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	copied := append([]string(nil), values...)
	sort.Strings(copied)
	return copied
}

// releaseSetHead is the channel head of releaseSetWorkspace: both members at
// 1.0.0-old with the fingerprints releaseSetTestFingerprints reports by
// default, so a test that overrides nothing sees no impact.
func releaseSetHead(t *testing.T) map[string]*distribution.ChannelHead {
	t.Helper()
	set := distribution.ReleaseSet{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		Members: []distribution.ReleaseSetMember{
			{Ecosystem: "npm", Coordinate: "@putnami/upstream", Version: "1.0.0-old", ArtifactDigest: digestFor('a'), Dependencies: []distribution.ReleaseSetDependency{}, SourceRevision: testRevision, SelectionFingerprint: testFingerprint("/upstream")},
			{Ecosystem: "npm", Coordinate: "@putnami/downstream", Version: "1.0.0-old", ArtifactDigest: digestFor('b'), Dependencies: []distribution.ReleaseSetDependency{{Ecosystem: "npm", Coordinate: "@putnami/upstream", Version: "1.0.0-old"}}, SourceRevision: testRevision, SelectionFingerprint: testFingerprint("/downstream")},
		},
	}
	heads := map[string]*distribution.ChannelHead{"canary": {Generation: 3, ReleaseSet: distribution.NormalizeReleaseSet(&set)}}
	refreshHeadRef(t, heads["canary"])
	return heads
}

// emptyHead is the answer for a channel that has none: a null head, never an
// error.
func emptyHead(channels ...string) map[string]*distribution.ChannelHead {
	heads := make(map[string]*distribution.ChannelHead, len(channels))
	for _, name := range channels {
		heads[name] = nil
	}
	return heads
}

func refreshHeadRef(t *testing.T, head *distribution.ChannelHead) {
	t.Helper()
	if head == nil || head.ReleaseSet == nil {
		return
	}
	ref, diagnostics := distribution.DeriveReleaseSetRef(head.ReleaseSet)
	if len(diagnostics) != 0 {
		// Invalid-head cases deliberately cannot refresh a ref. Retain a
		// well-formed placeholder so validation reports the head defect first.
		head.Ref = distribution.ReleaseSetRef{ID: "rs_" + strings.Repeat("0", 64), Digest: digestFor('0')}
		return
	}
	head.Ref = ref
}

func releaseSetRequest() ReleaseSetOptions {
	return ReleaseSetOptions{Commands: []string{"publish"}, Impacted: true, Channels: []string{"canary"}, Profiles: testProfiles()}
}

func releaseSetAllOptions() ReleaseSetOptions {
	return ReleaseSetOptions{Commands: []string{"publish"}, All: true, Channels: []string{"canary"}, Profiles: testProfiles()}
}

// publishedResults is what a publish job emits per member it actually
// published: one `published-member` artifact event carrying the member's own
// (ecosystem, coordinate), because one project may publish several members.
func publishedResults(plan *releaseset.Plan, digest string) map[string]*JobResult {
	results := make(map[string]*JobResult)
	for index, member := range plan.SelectedMembers() {
		memberDigest := digest
		if index > 0 {
			memberDigest = digestFor(byte('d' + index))
		}
		results[member.ProjectID+":publish"] = &JobResult{
			Status: "success",
			Events: []RawJobEvent{publishedMemberEvent(member, memberDigest)},
		}
	}
	return results
}

func publishedMemberEvent(member releaseset.PlannedMember, digest string) RawJobEvent {
	parsed, ok := parseRawEvent(strings.TrimSpace(publishedMemberRuntimeLine(member, digest)))
	if !ok {
		panic("published member runtime artifact failed strict parsing")
	}
	return parsed
}

func publishedMemberRuntimeLine(member releaseset.PlannedMember, digest string) string {
	event := RawJobEvent{}
	wire, err := json.Marshal(extensionproto.PublishedMember{
		Ecosystem:      string(member.Ecosystem),
		Coordinate:     member.Coordinate,
		Version:        member.Version,
		ArtifactDigest: digest,
	})
	if err != nil {
		panic(err)
	}
	if err := json.Unmarshal(wire, &event.Data); err != nil {
		panic(err)
	}
	var stream bytes.Buffer
	emitter := runtimeproto.NewEmitterForVersion(&stream, runtimeproto.MaxKnownProtocolVersion)
	if err := emitter.ArtifactData("release-set-member", member.Coordinate, extensionproto.PublishedMemberEventKind, "", event.Data); err != nil {
		panic(err)
	}
	return stream.String()
}

func cloneResults(input map[string]*JobResult) map[string]*JobResult {
	result := make(map[string]*JobResult, len(input))
	for key, value := range input {
		clone := *value
		clone.Data = maps.Clone(value.Data)
		clone.Events = append([]RawJobEvent(nil), value.Events...)
		result[key] = &clone
	}
	return result
}

func assertReleaseSetFailure(t *testing.T, results map[string]*JobResult) {
	t.Helper()
	result := results[releaseSetResultKey]
	if result == nil || result.Status != "failed" || result.Data != nil {
		t.Fatalf("release-set failure = %+v", result)
	}
}

func digestFor(char byte) string { return "sha256:" + strings.Repeat(string(char), 64) }

// TestValidateReleaseSetSelectionRejectsChannelWithoutReleaseSetSelection pins
// the fail-fast. `publish --projects X --channel canary` used to publish the
// artifacts through the legacy path and only then collect a registry
// `409 dist-tag canary is managed by the canonical release-set channel`,
// leaving the version published but unreferenced.
func TestValidateReleaseSetSelectionRejectsChannelWithoutReleaseSetSelection(t *testing.T) {
	err := ValidateReleaseSetSelection(ReleaseSetOptions{
		Commands: []string{"publish"},
		Channels: []string{"canary"},
	})
	if err == nil {
		t.Fatal("a channel without --all or --impacted must be refused before anything is published")
	}
	if !errors.Is(err, protocolcli.ErrUsage) {
		t.Errorf("error = %v, want a usage refusal", err)
	}
	for _, want := range []string{"canary", "--all", "--impacted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// PrepareReleaseSetRun owns the ordering around fingerprint resolution. It
// must apply the same fail-fast before touching a workspace or keying plan, so
// an invalid selection cannot be masked by an unrelated missing package route.
func TestPrepareReleaseSetRunRejectsUnsupportedSelectionBeforeFingerprinting(t *testing.T) {
	_, err := PrepareReleaseSetRun(context.Background(), ReleaseSetOptions{
		Commands: []string{"publish"},
		Channels: []string{"canary"},
	}, nil, nil, nil, nil, nil, nil, nil, nil, nil, false)
	if err == nil || !errors.Is(err, protocolcli.ErrUsage) {
		t.Fatalf("PrepareReleaseSetRun error = %v, want a usage refusal before fingerprinting", err)
	}
	for _, want := range []string{"canary", "--all", "--impacted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestValidateReleaseSetSelectionAllowsServableSelections(t *testing.T) {
	for name, options := range map[string]ReleaseSetOptions{
		"all":        {Commands: []string{"publish"}, Channels: []string{"canary"}, All: true},
		"impacted":   {Commands: []string{"publish"}, Channels: []string{"canary"}, Impacted: true},
		"no channel": {Commands: []string{"publish"}},

		"not a publish":  {Commands: []string{"build"}, Channels: []string{"canary"}},
		"planned expand": {Commands: []string{"deploy"}, Channels: []string{"canary"}, PlannedPublish: true, All: true},
	} {
		if err := ValidateReleaseSetSelection(options); err != nil {
			t.Errorf("%s: unexpected refusal: %v", name, err)
		}
	}
}

// taggedLineWorkspace is a two-line workspace: one npm member per line, with no
// dependency between them, so a cohort selection is visible on its own rather
// than through dependency propagation.
func taggedLineWorkspace(t *testing.T) (*workspace.Workspace, *workspace.Project, *workspace.Project) {
	t.Helper()
	web := &workspace.Project{
		ID: "/typescript/web", Name: "@putnami/web", Type: "library", Line: "typescript",
		Metadata: releaseSetProjectMetadata(t, "npm", "@putnami/web"),
	}
	cli := &workspace.Project{
		ID: "/tooling/cli", Name: "@putnami/cli", Type: "library", Line: "tooling",
		Metadata: releaseSetProjectMetadata(t, "npm", "@putnami/cli"),
	}
	ws := workspace.NewWorkspace(t.TempDir(), &wsproto.Config{Name: "putnami"}, []*workspace.Project{web, cli})
	return ws, web, cli
}

// taggedLineHead is the canary head of taggedLineWorkspace: both members at
// 0.2.0 with their current fingerprints, so nothing is impacted and only the
// tag can select a member.
func taggedLineHead(t *testing.T) map[string]*distribution.ChannelHead {
	t.Helper()
	set := distribution.ReleaseSet{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		Members: []distribution.ReleaseSetMember{
			{Ecosystem: "npm", Coordinate: "@putnami/web", Version: "0.2.0", ArtifactDigest: digestFor('a'), Dependencies: []distribution.ReleaseSetDependency{}, SourceRevision: testRevision, SelectionFingerprint: testFingerprint("/typescript/web")},
			{Ecosystem: "npm", Coordinate: "@putnami/cli", Version: "0.2.0", ArtifactDigest: digestFor('b'), Dependencies: []distribution.ReleaseSetDependency{}, SourceRevision: testRevision, SelectionFingerprint: testFingerprint("/tooling/cli")},
		},
	}
	heads := map[string]*distribution.ChannelHead{"canary": {Generation: 5, ReleaseSet: distribution.NormalizeReleaseSet(&set)}}
	refreshHeadRef(t, heads["canary"])
	return heads
}

// taggedLineVersions is the run's versions for taggedLineWorkspace: the
// typescript line is at its tag, the tooling line is an ordinary pre-release.
func taggedLineVersions() RunVersions {
	return RunVersions{
		"typescript": {Base: "0.3.0", Full: "0.3.0", Tag: "ts/v0.3.0", Tagged: true, Line: "typescript"},
		"tooling":    {Base: "0.2.1", Full: "0.2.1-20260904120000-abcdef1", Line: "tooling"},
	}
}

func taggedPublishOptions() ReleaseSetOptions {
	return ReleaseSetOptions{
		Commands: []string{"publish"}, Channels: []string{"canary"}, Profiles: testProfiles(),
		Tagged: true, Line: "typescript", Versions: taggedLineVersions(),
	}
}

func TestFullReleaseSetPlansEveryWorkspaceMember(t *testing.T) {
	options := ReleaseSetOptions{
		Commands: []string{"publish"}, Channels: []string{"canary"}, All: true,
	}
	if !ReleaseSetPlansEveryProject(options) {
		t.Fatal("publish --all --channel must plan every workspace member, including members hidden by default tag exclusions")
	}
	options.PlannedPublish = true
	if ReleaseSetPlansEveryProject(options) {
		t.Fatal("a dependent publish must remain bounded by its parent command's selection")
	}
}

// D1: a publish on a tagged commit is its LINE's cohort. Every member of the
// tagged line is repackaged at the tag's version even though its fingerprint
// matches the head exactly; every member of another line inherits its record.
func TestTaggedPublishSelectsTheLineCohort(t *testing.T) {
	spectest.Proves(t, "cli/channels", "tagged-publish-is-a-cohort", "cohort-selects-every-line-member")
	ws, _, _ := taggedLineWorkspace(t)
	useReleaseSetProvenance(t, nil)
	run := prepareReleaseSetWithHead(t, taggedPublishOptions(), ws, taggedLineHead(t))

	web, _ := run.plan.Member(distribution.Ecosystem("npm"), "@putnami/web")
	cli, _ := run.plan.Member(distribution.Ecosystem("npm"), "@putnami/cli")
	if !web.Selected || web.Version != "0.3.0" || web.ArtifactDigest != "" {
		t.Fatalf("tagged line member = %+v; want it republished at the tag's version", web)
	}
	if cli.Selected || cli.Version != "0.2.0" || cli.ArtifactDigest != digestFor('b') {
		t.Fatalf("other line member = %+v; want the head record inherited", cli)
	}
	// The selection is the tag's, not the fingerprint's: nothing changed.
	if len(run.plan.SelectedMembers()) != 1 {
		t.Fatalf("selected members = %+v, want only the tagged line", run.plan.SelectedMembers())
	}
	// A tagged publish plans every project of the workspace before the plan
	// narrows it back, exactly like an impacted one.
	if !ReleaseSetPlansEveryProject(taggedPublishOptions()) {
		t.Fatal("a tagged publish must let the coordinator select the projects")
	}
	if mode := RequestedReleaseSetMode(taggedPublishOptions()); mode != ReleaseSetAll {
		t.Fatalf("tagged mode = %v, want all", mode)
	}
}

// D2: the tag's channel is created immutable, with no expected head, beside
// every channel the caller named. The named channel keeps its own
// compare-and-swap expectation.
func TestTaggedPublishRequestsTheImmutableChannel(t *testing.T) {
	spectest.Proves(t, "cli/channels", "tagged-publish-is-a-cohort", "immutable-channel-is-requested")
	ws, _, _ := taggedLineWorkspace(t)
	useReleaseSetProvenance(t, nil)
	heads := taggedLineHead(t)
	provider := &fakeReleaseSetProvider{heads: heads}
	useReleaseSetProviderFake(t, provider)
	run, err := prepareReleaseSet(t, taggedPublishOptions(), ws, ws.Projects, providerDiscovery())
	if err != nil || run == nil {
		t.Fatalf("tagged preparation = %+v, %v", run, err)
	}
	if got := run.plan.Channels; len(got) != 2 || got[0] != "canary" || got[1] != "ts-v0.3.0" {
		t.Fatalf("plan channels = %v; want the named channel first and the tag's channel last", got)
	}
	if run.plan.Heads["ts-v0.3.0"] != nil {
		t.Fatalf("tag channel head = %+v, want none", run.plan.Heads["ts-v0.3.0"])
	}
	// The tag channel is never resolved: it does not exist yet by construction.
	if len(provider.resolveRequests) != 1 || len(provider.resolveRequests[0].Channels) != 1 {
		t.Fatalf("resolve requests = %+v, want only the named channel", provider.resolveRequests)
	}

	results := publishedResults(run.plan, digestFor('c'))
	run.Finalizer(context.Background())(results)
	if result := results[releaseSetResultKey]; result == nil || result.Status != "success" {
		t.Fatalf("tagged release = %+v", result)
	}
	request := provider.releaseRequest
	if len(request.Channels) != 2 {
		t.Fatalf("release channels = %+v, want both", request.Channels)
	}
	if request.Channels[0].Name != "canary" || request.Channels[0].Immutable ||
		request.Channels[0].Expected == nil || *request.Channels[0].Expected != heads["canary"].Ref {
		t.Fatalf("named channel request = %+v", request.Channels[0])
	}
	if request.Channels[1].Name != "ts-v0.3.0" || !request.Channels[1].Immutable || request.Channels[1].Expected != nil {
		t.Fatalf("tag channel request = %+v; want an immutable channel asserting no head", request.Channels[1])
	}
}

// A tagged publish needs a clean tree: the tag would otherwise name content
// that is not what was tagged.
func TestTaggedPublishRefusesADirtyTree(t *testing.T) {
	options := taggedPublishOptions()
	options.Versions["typescript"].IsDirty = true
	err := ValidateReleaseSetSelection(options)
	if err == nil || !strings.Contains(err.Error(), "clean tree") {
		t.Fatalf("dirty tagged publish = %v, want a usage refusal", err)
	}
}

// D31: a protected channel is refused up front. Only `putnami channel set`
// from a user moves it, so a publish naming it is a mistake worth naming
// before anything is packaged.
func TestPublishRefusesAProtectedChannel(t *testing.T) {
	spectest.Proves(t, "cli/channels", "protected-channels", "publish-refuses-a-protected-channel")
	options := releaseSetRequest()
	options.Channels = []string{"canary", "latest"}
	options.Policy = &ciproto.Distribution{
		Namespace: "putnami",
		Channels: map[string]ciproto.ChannelPolicy{
			"canary": {Visibility: "internal"},
			"latest": {Visibility: "public", Protected: true},
		},
	}
	err := ValidateReleaseSetSelection(options)
	if err == nil || !strings.Contains(err.Error(), "channel latest is protected") ||
		!errors.Is(err, protocolcli.ErrUsage) {
		t.Fatalf("protected-channel publish = %v, want a usage refusal naming channel set", err)
	}
	// The same refusal reaches the coordinator, before any provider call.
	ws, _, _ := releaseSetWorkspace(t)
	useReleaseSetProvenance(t, nil)
	provider := &fakeReleaseSetProvider{heads: releaseSetHead(t)}
	useReleaseSetProviderFake(t, provider)
	if _, err := prepareReleaseSet(t, options, ws, ws.Projects, providerDiscovery()); err == nil {
		t.Fatal("PrepareReleaseSet accepted a protected channel")
	}
	if provider.resolveCalls != 0 {
		t.Fatalf("resolve calls = %d, want none before the refusal", provider.resolveCalls)
	}
}

// D9: a publication refuses a checkout that cannot answer what version a
// commit carries. Publishing from a shallow clone would stamp a number
// computed from the fraction of history that happened to be fetched.
func TestPublishRefusesAShallowClone(t *testing.T) {
	ws, _, _ := releaseSetWorkspace(t)
	useReleaseSetProvenance(t, nil)
	requireFullClone = func(string) error { return fmt.Errorf("version and publish need a full clone with tags") }
	provider := &fakeReleaseSetProvider{heads: releaseSetHead(t)}
	useReleaseSetProviderFake(t, provider)
	_, err := prepareReleaseSet(t, releaseSetRequest(), ws, ws.Projects, providerDiscovery())
	if err == nil || !strings.Contains(err.Error(), "full clone") {
		t.Fatalf("shallow-clone publish = %v, want a refusal", err)
	}
	if provider.resolveCalls != 0 {
		t.Fatalf("resolve calls = %d, want none before the refusal", provider.resolveCalls)
	}
}

// BuildReleaseSetOptions is the ONE place the engine turns a run's flags into
// release-set options: the channels, the declared policy, and the line HEAD's
// tag releases.
func TestBuildReleaseSetOptionsReadsTheTagAndThePolicy(t *testing.T) {
	root := t.TempDir()
	document := `{"version":3,"commands":["build"],` +
		`"distribution":{"namespace":"putnami","visibility":"internal",` +
		`"registries":{"npm":{"mirror":{"to":"https://registry.npmjs.org"}}},` +
		`"channels":{"latest":{"visibility":"public","protected":true}}}}`
	if err := os.WriteFile(filepath.Join(root, "putnami.ci.json"), []byte(document), 0o644); err != nil {
		t.Fatal(err)
	}
	options, err := BuildReleaseSetOptions(ReleaseSetRequest{
		Commands: []string{"publish"}, WorkspaceRoot: root, Channel: "canary",
		Visibility: "public", Versions: taggedLineVersions(),
	})
	if err != nil {
		t.Fatalf("BuildReleaseSetOptions = %v", err)
	}
	if !options.Tagged || options.Line != "typescript" {
		t.Fatalf("tagged line = %v/%q, want the one tagged line", options.Tagged, options.Line)
	}
	if options.Visibility != "public" || options.Policy == nil || !options.Policy.Channels["latest"].Protected {
		t.Fatalf("options = %+v, want the declared policy and the set level", options)
	}
	if mirror := options.Policy.Registries["npm"].Mirror; mirror == nil || mirror.To != "https://registry.npmjs.org" {
		t.Fatalf("CI mirror destination was lost while building publication options: %+v", mirror)
	}

	// An ordinary build on a tagged commit is still an ordinary build, and
	// never consults the distribution policy: an invalid CI document must fail
	// `ci validate` and every publish, not the lint/test/build gate.
	build, err := BuildReleaseSetOptions(ReleaseSetRequest{
		Commands: []string{"build"}, WorkspaceRoot: root, Versions: taggedLineVersions(),
	})
	if err != nil || build.Tagged || build.Policy != nil {
		t.Fatalf("build on a tagged commit = %+v, %v; want no cohort and no policy", build, err)
	}

	// Two tagged lines at once: --scope names the one being released, and
	// without it the publish is refused rather than guessing.
	versions := taggedLineVersions()
	versions["tooling"] = &JobContextVersion{Base: "0.3.0", Full: "0.3.0", Tag: "tooling/v0.3.0", Tagged: true, Line: "tooling"}
	if _, err := BuildReleaseSetOptions(ReleaseSetRequest{
		Commands: []string{"publish"}, WorkspaceRoot: root, Versions: versions,
	}); err == nil || !errors.Is(err, protocolcli.ErrUsage) {
		t.Fatalf("two tagged lines = %v, want a usage refusal", err)
	}
	scoped, err := BuildReleaseSetOptions(ReleaseSetRequest{
		Commands: []string{"publish"}, WorkspaceRoot: root, Scope: "tooling", Versions: versions,
	})
	if err != nil || !scoped.Tagged || scoped.Line != "tooling" {
		t.Fatalf("--scope tooling = %+v, %v; want that line's cohort", scoped, err)
	}
	if _, err := BuildReleaseSetOptions(ReleaseSetRequest{
		Commands: []string{"publish"}, WorkspaceRoot: root, Scope: "toolng", Versions: versions,
	}); err == nil || !errors.Is(err, protocolcli.ErrUsage) || !strings.Contains(err.Error(), "toolng") {
		t.Fatalf("unknown --scope = %v, want a usage refusal naming the invalid line", err)
	}
}

// TestReleaseMemberProjectRecordsTheLogicalIdentity pins which of a project's
// two names reaches the wire. A transparent group folder lives in Path and is
// absent from ID, and the attribution a consumer matches against must be the
// one a selector can name: a ci document selects "apps/service", never
// "apps/(internal)/service", and MemberProjectPattern admits no parenthesis, so
// recording the physical path would record NOTHING for exactly the grouped
// projects attribution exists to serve.
func TestReleaseMemberProjectRecordsTheLogicalIdentity(t *testing.T) {
	grouped := &workspace.Project{
		ID:   workspace.ProjectIDFromPath("apps/(internal)/service"),
		Path: "apps/(internal)/service",
	}
	if grouped.ID != "/apps/service" {
		t.Fatalf("fixture identity = %q; the model no longer omits transparent group folders", grouped.ID)
	}
	if got := releaseMemberProject(grouped); got != "apps/service" {
		t.Fatalf("grouped project attribution = %q, want %q", got, "apps/service")
	}
	if distribution.IsMemberProject(grouped.Path) {
		t.Fatalf("the protocol grammar admits %q; recording a physical path would then be possible and this test's premise is stale", grouped.Path)
	}

	plain := &workspace.Project{ID: "/typescript/framework/web", Path: "typescript/framework/web"}
	if got := releaseMemberProject(plain); got != "typescript/framework/web" {
		t.Fatalf("plain project attribution = %q, want %q", got, "typescript/framework/web")
	}

	// A project whose id the grammar rejects records no attribution rather than
	// failing the release: attribution is optional metadata.
	if got := releaseMemberProject(&workspace.Project{ID: "/-apps"}); got != "" {
		t.Fatalf("ungrammatical project attribution = %q, want it unrecorded", got)
	}
	if got := releaseMemberProject(nil); got != "" {
		t.Fatalf("nil project attribution = %q, want it unrecorded", got)
	}
}

// D14, amended by protocols/distribution ADR 0006: a publication may measure
// impact against a channel it does NOT advance. The first push of a pull
// request advances an empty `pr-7` and reads `canary`, so it republishes what
// the branch changed instead of the whole workspace — and `canary`, which
// belongs to main, is never moved by a pull request.
func TestPrepareReleaseSetMeasuresAgainstBaselineChannel(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "release-set-channel-advance", "an-empty-channel-measures-against-the-named-baseline")
	ws, upstream, downstream := releaseSetWorkspace(t)
	useReleaseSetProvenance(t, map[string]string{downstream.ID: digestFor('9')})

	heads := releaseSetHead(t)
	heads["pr-7"] = nil
	provider := &fakeReleaseSetProvider{heads: heads}
	useReleaseSetProviderFake(t, provider)

	options := releaseSetRequest()
	options.Channels = []string{"pr-7"}
	options.BaselineChannel = "canary"
	run, err := prepareReleaseSet(t, options, ws, ws.Projects, providerDiscovery())
	if err != nil || run == nil {
		t.Fatalf("baseline-channel preparation = %+v, %v", run, err)
	}

	// ONE resolve, naming the advanced channel first and the baseline after it:
	// both heads come from the same read, or the plan would compare a head from
	// one instant against an expectation from another.
	if provider.resolveCalls != 1 || len(provider.resolveRequests) != 1 {
		t.Fatalf("resolve calls = %d, want exactly one for the advanced channel and its baseline", provider.resolveCalls)
	}
	if got := provider.resolveRequests[0].Channels; !slices.Equal(got, []string{"pr-7", "canary"}) {
		t.Fatalf("resolve request channels = %v, want pr-7 then canary", got)
	}

	// Only the changed member is republished; the unchanged one inherits the
	// baseline record verbatim, including the version and digest that were
	// published under canary.
	if len(run.plan.SelectedMembers()) != 1 {
		t.Fatalf("selected members = %+v, want only the changed downstream", run.plan.SelectedMembers())
	}
	up, _ := run.plan.Member(distribution.Ecosystem("npm"), upstream.Name)
	base := heads["canary"].ReleaseSet.Members
	inherited := base[0]
	if base[1].Coordinate == upstream.Name {
		inherited = base[1]
	}
	if up.Selected || up.Version != inherited.Version || up.ArtifactDigest != inherited.ArtifactDigest ||
		up.SourceRevision != inherited.SourceRevision || up.SelectionFingerprint != inherited.SelectionFingerprint {
		t.Fatalf("unchanged upstream = %+v, want canary's record %+v verbatim", up, inherited)
	}
	down, _ := run.plan.Member(distribution.Ecosystem("npm"), downstream.Name)
	if !down.Selected || len(down.Dependencies) != 1 || down.Dependencies[0].Version != inherited.Version {
		t.Fatalf("selected downstream = %+v, want a closed edge on canary's upstream version", down)
	}
	if run.HeadRef() == nil || *run.HeadRef() != heads["canary"].Ref {
		t.Fatalf("baseline = %+v, want canary's head", run.HeadRef())
	}
	if run.plan.BaselineChannel != "canary" || run.plan.BaselineChannelName() != "canary" {
		t.Fatalf("plan baseline channel = %q / %q, want canary", run.plan.BaselineChannel, run.plan.BaselineChannelName())
	}

	// The session says WHICH head it measured against: the set id is canary's,
	// and the tier tells a reader it came from a channel this run never moved.
	selection := run.RunSelection(&protocoljob.Selection{Mode: protocoljob.SelectionModeImpacted}, ws.Projects)
	if selection.Baseline != heads["canary"].Ref.ID || selection.BaselineSource != ReleaseSetBaselineChannelSource {
		t.Fatalf("session selection = %+v, want canary's set id under the baseline-channel tier", selection)
	}

	// The release advances pr-7 alone, from no head. canary is read, never
	// written: it is not a ChannelRequest at all.
	results := publishedResults(run.plan, digestFor('c'))
	run.Finalizer(context.Background())(results)
	if result := results[releaseSetResultKey]; result == nil || result.Status != "success" {
		t.Fatalf("baseline-channel release = %+v", result)
	}
	request := provider.releaseRequest
	if len(request.Channels) != 1 || request.Channels[0].Name != "pr-7" || request.Channels[0].Expected != nil {
		t.Fatalf("release channels = %+v, want pr-7 alone with a null expectation", request.Channels)
	}
	outcome := releaseSetOutcomeOf(t, results)
	if outcome.Channels["canary"] != nil {
		t.Fatalf("outcome advanced the baseline channel: %+v", outcome.Channels)
	}
}

// The named baseline is a pure FALLBACK. As soon as the advanced channel has a
// head of its own — every push after the first — the publication measures
// against it exactly as it did before a baseline could be named, so a member
// the pull request already republished is not republished again.
func TestPrepareReleaseSetPrefersTheAdvancedChannelsOwnHead(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "release-set-channel-advance", "an-advanced-channel-with-a-head-ignores-the-baseline")
	ws, _, downstream := releaseSetWorkspace(t)

	t.Run("own head wins", func(t *testing.T) {
		useReleaseSetProvenance(t, map[string]string{downstream.ID: digestFor('9')})
		own := releaseSetHead(t)
		heads := map[string]*distribution.ChannelHead{"pr-7": own["canary"], "canary": nil}
		provider := &fakeReleaseSetProvider{heads: heads}
		useReleaseSetProviderFake(t, provider)

		options := releaseSetRequest()
		options.Channels = []string{"pr-7"}
		options.BaselineChannel = "canary"
		run, err := prepareReleaseSet(t, options, ws, ws.Projects, providerDiscovery())
		if err != nil || run == nil {
			t.Fatalf("own-head preparation = %+v, %v", run, err)
		}
		if run.HeadRef() == nil || *run.HeadRef() != own["canary"].Ref {
			t.Fatalf("baseline = %+v, want pr-7's own head", run.HeadRef())
		}
		if run.plan.BaselineChannel != "" || run.plan.BaselineChannelName() != "pr-7" {
			t.Fatalf("plan baseline channel = %q / %q; an own-head plan names no baseline channel",
				run.plan.BaselineChannel, run.plan.BaselineChannelName())
		}
		if len(run.plan.SelectedMembers()) != 1 {
			t.Fatalf("selected members = %+v, want only what changed since pr-7's own head", run.plan.SelectedMembers())
		}
		selection := run.RunSelection(&protocoljob.Selection{Mode: protocoljob.SelectionModeImpacted}, ws.Projects)
		if selection.BaselineSource != ReleaseSetHeadBaselineSource {
			t.Fatalf("session selection = %+v, want the release-set-head tier", selection)
		}
	})

	t.Run("neither head selects everything", func(t *testing.T) {
		useReleaseSetProvenance(t, nil)
		provider := &fakeReleaseSetProvider{heads: emptyHead("pr-7", "canary")}
		useReleaseSetProviderFake(t, provider)

		options := releaseSetRequest()
		options.Channels = []string{"pr-7"}
		options.BaselineChannel = "canary"
		run, err := prepareReleaseSet(t, options, ws, ws.Projects, providerDiscovery())
		if err != nil || run == nil {
			t.Fatalf("empty-baseline preparation = %+v, %v", run, err)
		}
		if !run.plan.SelectsEveryMember() || run.HeadRef() != nil || run.plan.BaselineChannelName() != "" {
			t.Fatalf("plan over two empty channels = %+v", run.plan)
		}
		// A plan names a baseline channel exactly when that channel IS the
		// baseline. With nothing to inherit, the plan handed to the extensions
		// is byte-identical to the one it would have carried without the flag,
		// so an extension built before the member existed is only ever
		// confronted with it when the feature actually fired.
		if run.plan.BaselineChannel != "" || len(run.plan.Heads) != 1 {
			t.Fatalf("plan heads = %+v, baseline channel = %q; an unused baseline leaves no trace",
				run.plan.Heads, run.plan.BaselineChannel)
		}
	})
}

// A baseline names the head impact is measured against, so it is refused
// wherever nothing measures impact against a head, and wherever it could not be
// read at all: the flag is answered before anything is packaged.
func TestReleaseSetBaselineChannelIsRefusedWhereItCannotBeRead(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "release-set-channel-advance", "a-baseline-channel-that-cannot-be-read-is-refused")
	if _, err := ParseReleaseSetBaselineChannel("  ", []string{"canary"}); err != nil {
		t.Fatalf("an unset baseline = %v, want no error", err)
	}
	if got, err := ParseReleaseSetBaselineChannel(" canary ", []string{"pr-7"}); err != nil || got != "canary" {
		t.Fatalf("ParseReleaseSetBaselineChannel = %q, %v; want the trimmed name", got, err)
	}
	if _, err := ParseReleaseSetBaselineChannel("Canary", []string{"pr-7"}); err == nil {
		t.Fatal("a non-portable baseline channel was accepted")
	}
	if _, err := ParseReleaseSetBaselineChannel("canary", []string{"pr-7", "canary"}); err == nil {
		t.Fatal("a baseline this publication also advances was accepted")
	}

	// The ONE resolve names the advanced channels PLUS the baseline, and a
	// resolve request carries at most MaxChannelsPerRelease channels.
	full := make([]string, distribution.MaxChannelsPerRelease)
	for index := range full {
		full[index] = fmt.Sprintf("c%d", index)
	}
	if _, err := ParseReleaseSetBaselineChannel("canary", full); err == nil {
		t.Fatalf("%d advanced channels plus a baseline was accepted", distribution.MaxChannelsPerRelease)
	}
	if _, err := ParseReleaseSetBaselineChannel("canary", full[:len(full)-1]); err != nil {
		t.Fatalf("%d advanced channels plus a baseline = %v, want the exact bound accepted", len(full)-1, err)
	}

	// The flag is bound through the one request builder, so the usage error
	// arrives from the same place the channels are parsed.
	if _, err := BuildReleaseSetOptions(ReleaseSetRequest{
		Commands: []string{"publish"}, Channel: "canary", BaselineChannel: "canary", Impacted: true,
	}); !errors.Is(err, cmderr.ErrUsage) {
		t.Fatalf("BuildReleaseSetOptions = %v, want a usage error", err)
	}

	base := ReleaseSetOptions{Commands: []string{"publish"}, Channels: []string{"pr-7"}, BaselineChannel: "canary"}
	cases := []struct {
		name    string
		options ReleaseSetOptions
		refused bool
	}{
		{"impacted", withReleaseSetSelection(base, func(o *ReleaseSetOptions) { o.Impacted = true }), false},
		{"all", withReleaseSetSelection(base, func(o *ReleaseSetOptions) { o.All = true }), true},
		{"tagged", withReleaseSetSelection(base, func(o *ReleaseSetOptions) {
			o.Impacted, o.Tagged = true, true
			o.Versions = RunVersions{"": &JobContextVersion{Base: "1.0.0", Full: "1.0.0", Tagged: true, Tag: "v1.0.0"}}
		}), true},
		{"no selection flag", base, true},
		// No channel at all is not a release-set publication: the run falls to
		// the legacy per-package path, where nothing reads a head, so the flag
		// would be a silent no-op rather than a refusal. The document half
		// refuses the same shape (ci.invalid_baseline on a rule that publishes
		// nothing), and a runner can never render this one: Explain reports a
		// baseline only beside a non-empty publish.
		{"no channel", withReleaseSetSelection(base, func(o *ReleaseSetOptions) {
			o.Channels, o.Impacted = nil, true
		}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateReleaseSetSelection(tc.options)
			if refused := err != nil; refused != tc.refused {
				t.Fatalf("ValidateReleaseSetSelection = %v, refused = %t", err, tc.refused)
			}
			if tc.refused && !errors.Is(err, cmderr.ErrUsage) {
				t.Fatalf("ValidateReleaseSetSelection error = %v, want a usage error", err)
			}
		})
	}
}

func withReleaseSetSelection(options ReleaseSetOptions, edit func(*ReleaseSetOptions)) ReleaseSetOptions {
	copied := options
	copied.Channels = append([]string(nil), options.Channels...)
	edit(&copied)
	return copied
}
