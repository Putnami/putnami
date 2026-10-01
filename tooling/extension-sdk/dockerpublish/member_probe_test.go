package dockerpublish

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/memberprobe"
)

// stubManifestHead replaces the probe's manifest HEAD for a test about another
// behavior, so a dry run against a made-up registry name reaches no network.
func stubManifestHead(t *testing.T, err error) {
	t.Helper()
	original := probeManifestHead
	probeManifestHead = func(string, ...crane.Option) (*v1.Descriptor, error) { return nil, err }
	t.Cleanup(func() { probeManifestHead = original })
}

// dryRunProbes runs one dry-run publication and returns the probes it emitted,
// read back through the strict reader the orchestrator uses. It fails the test
// when the dry run does not succeed or claims a publication.
func dryRunProbes(t *testing.T, publish func() (string, map[string]any, error)) []extproto.MemberProbe {
	t.Helper()
	events := captureEvents(t, func() {
		status, data, err := publish()
		if err != nil || status != "OK" {
			t.Fatalf("dry run = (%q, %+v, %v), want OK: the orchestrator fails the run, not the task", status, data, err)
		}
	})
	if members := eventsOfKind(events, extproto.PublishedMemberEventKind); len(members) != 0 {
		t.Fatalf("a dry run emitted publication evidence: %+v", members)
	}
	probeEvents := eventsOfKind(events, extproto.MemberProbeEventKind)
	probes := make([]extproto.MemberProbe, 0, len(probeEvents))
	for _, event := range probeEvents {
		probe, err := memberprobe.Decode(event)
		if err != nil {
			t.Fatalf("member-probe %+v does not pass the strict reader: %v", event, err)
		}
		probes = append(probes, *probe)
	}
	return probes
}

func oneProbe(t *testing.T, probes []extproto.MemberProbe) extproto.MemberProbe {
	t.Helper()
	if len(probes) != 1 {
		t.Fatalf("the dry run emitted %d probes, want one per member: %+v", len(probes), probes)
	}
	return probes[0]
}

// assertReadOnly fails when any request is neither a GET nor a HEAD.
func assertReadOnly(t *testing.T, calls []privateOCITestCall) {
	t.Helper()
	if len(calls) == 0 {
		t.Fatal("the dry run asked the registry nothing")
	}
	for _, call := range calls {
		if call.Method != http.MethodGet && call.Method != http.MethodHead {
			t.Fatalf("the dry run sent %s %s; a probe only reads", call.Method, call.Path)
		}
	}
}

func pushOtherImage(t *testing.T, ref string) string {
	t.Helper()
	image, err := random.Image(128, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := crane.Push(image, ref); err != nil {
		t.Fatalf("pushing %s: %v", ref, err)
	}
	digest, err := image.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return digest.String()
}

// A dry-run workload image publication asks the registry about the one tag the
// real publish writes, the version, with read-only requests, and reports each
// of the three answers a registry can give.
func TestDryRunProbesTheVersionTag(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "dry-run-member-probe", "oci-probe-is-read-only")
	dryRun := func(fixture dockerPublishFixture) func() (string, map[string]any, error) {
		return func() (string, map[string]any, error) {
			return Publish(fixture.ctx, jsonl.New(), []string{"--dry-run"})
		}
	}
	assertMember := func(t *testing.T, fixture dockerPublishFixture, probe extproto.MemberProbe) {
		t.Helper()
		if probe.Ecosystem != "oci" || probe.Coordinate != fixture.repository || probe.Version != fixture.version || probe.Registry != fixture.host {
			t.Fatalf("probe = %+v, want member oci %s at %s on %s", probe, fixture.repository, fixture.version, fixture.host)
		}
		if probe.ArtifactDigest != fixture.digest || !probe.Anonymous {
			t.Fatalf("probe = %+v, want the packaged digest %s and an anonymous request", probe, fixture.digest)
		}
	}

	t.Run("a version the registry does not hold is absent", func(t *testing.T) {
		fixture := newDockerPublishFixture(t)
		probe := oneProbe(t, dryRunProbes(t, dryRun(fixture)))
		assertMember(t, fixture, probe)
		if probe.State != extproto.MemberProbeAbsent {
			t.Fatalf("state = %q (%s), want absent", probe.State, probe.Reason)
		}
		assertReadOnly(t, fixture.registry.recordedCalls())
	})

	t.Run("the packaged digest is identical", func(t *testing.T) {
		fixture := newDockerPublishFixture(t)
		if status, data, err := Publish(fixture.ctx, jsonl.New(), nil); err != nil || status != "OK" {
			t.Fatalf("publish = (%q, %+v, %v), want OK", status, data, err)
		}
		before := len(fixture.registry.recordedCalls())
		probe := oneProbe(t, dryRunProbes(t, dryRun(fixture)))
		assertMember(t, fixture, probe)
		if probe.State != extproto.MemberProbeIdentical || probe.RegistryDigest != fixture.digest {
			t.Fatalf("probe = %+v, want identical at %s", probe, fixture.digest)
		}
		assertReadOnly(t, fixture.registry.recordedCalls()[before:])
	})

	t.Run("another digest is a conflict", func(t *testing.T) {
		fixture := newDockerPublishFixture(t)
		held := pushOtherImage(t, fixture.host+"/"+fixture.repository+":"+fixture.version)
		before := len(fixture.registry.recordedCalls())
		probe := oneProbe(t, dryRunProbes(t, dryRun(fixture)))
		assertMember(t, fixture, probe)
		if probe.State != extproto.MemberProbeConflict || probe.RegistryDigest != held || !strings.Contains(probe.Reason, "another digest") {
			t.Fatalf("probe = %+v, want a conflict with the held digest %s", probe, held)
		}
		assertReadOnly(t, fixture.registry.recordedCalls()[before:])
	})

	t.Run("an unreachable registry is unverified", func(t *testing.T) {
		fixture := newDockerPublishFixture(t)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		closed := listener.Addr().String()
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		fixture.ctx.Params["registries"] = json.RawMessage(`{"oci":{"publish":"` + closed + `"}}`)
		probe := oneProbe(t, dryRunProbes(t, dryRun(fixture)))
		if probe.State != extproto.MemberProbeUnverified || !strings.Contains(probe.Reason, "the registry could not be reached") {
			t.Fatalf("probe = %+v, want unverified with a reason that names the unreachable registry", probe)
		}
		if probe.Registry != closed || probe.Coordinate != fixture.repository || probe.Version != fixture.version {
			t.Fatalf("probe = %+v, want the member, the registry %s and the version", probe, closed)
		}
		if calls := fixture.registry.recordedCalls(); len(calls) != 0 {
			t.Fatalf("the dry run asked another registry: %+v", calls)
		}
	})

	// The managed registry answers an anonymous request with a challenge whose
	// realm is its production token endpoint, so its refusal cannot be replayed
	// on loopback. The two shapes go-containerregistry reports it in are built
	// here instead: a status with distribution codes, and a failed token
	// exchange.
	for name, tc := range map[string]struct {
		refusal    error
		wantReason string
	}{
		"a refused request is unverified": {
			&transport.Error{StatusCode: http.StatusForbidden, Errors: []transport.Diagnostic{
				{Code: transport.DeniedErrorCode, Message: "denied for Bearer eyJhbGciOi.payload.sig"},
			}},
			"403 Forbidden: DENIED to a request without a credential",
		},
		"a failed token exchange is unverified": {
			errors.New("GET https://registry.example.test/token: UNAUTHORIZED: authentication required"),
			"401 Unauthorized to a request without a credential",
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newDockerPublishFixture(t)
			stubManifestHead(t, tc.refusal)
			probe := oneProbe(t, dryRunProbes(t, dryRun(fixture)))
			if probe.State != extproto.MemberProbeUnverified || !strings.Contains(probe.Reason, tc.wantReason) {
				t.Fatalf("probe = %+v, want unverified with a reason containing %q", probe, tc.wantReason)
			}
			if strings.Contains(probe.Reason, "eyJhbGciOi") {
				t.Fatalf("reason %q repeats the registry's free text", probe.Reason)
			}
		})
	}

	t.Run("no registry is asked nothing", func(t *testing.T) {
		fixture := newDockerPublishFixture(t)
		t.Setenv("DOCKER_REGISTRY", "")
		fixture.ctx.Params["registries"] = json.RawMessage(`{}`)
		if probes := dryRunProbes(t, dryRun(fixture)); len(probes) != 0 {
			t.Fatalf("a dry run without a registry emitted probes: %+v", probes)
		}
	})
}

// When package assembled no image, the release-set plan still names the member:
// the dry run asks about the planned coordinate and version, and a version the
// registry already holds is a conflict because no local digest exists.
func TestDryRunProbesThePlannedMemberWithoutAnAssembledImage(t *testing.T) {
	fixture := newDockerPublishFixture(t)
	ctx := fixture.ctx
	ctx.WorkspaceRoot = t.TempDir() // a workspace where package wrote no manifest
	plan := imageReleasePlan(ctx)
	plan.Members[0].Coordinate = "team/app"
	plan.Members[0].Version = "4.0.0"
	other := plan.Members[0]
	other.Coordinate = "team/other"
	other.ProjectID = "/apps/other"
	plan.Members = append(plan.Members, other)
	bindImageReleasePlan(t, ctx, plan)
	ctx.Params["registries"] = json.RawMessage(`{"oci":{"publish":"` + fixture.host + `"}}`)
	dryRun := func() (string, map[string]any, error) { return Publish(ctx, jsonl.New(), []string{"--dry-run"}) }

	probe := oneProbe(t, dryRunProbes(t, dryRun))
	if probe.State != extproto.MemberProbeAbsent || probe.Coordinate != "team/app" || probe.Version != "4.0.0" ||
		probe.Registry != fixture.host || probe.ArtifactDigest != "" {
		t.Fatalf("probe = %+v, want the planned member absent, with no local digest", probe)
	}

	held := pushOtherImage(t, fixture.host+"/team/app:4.0.0")
	before := len(fixture.registry.recordedCalls())
	probe = oneProbe(t, dryRunProbes(t, dryRun))
	if probe.State != extproto.MemberProbeConflict || probe.RegistryDigest != held || !strings.Contains(probe.Reason, "built no artifact to compare") {
		t.Fatalf("probe = %+v, want a conflict that says the dry run built no artifact", probe)
	}
	assertReadOnly(t, fixture.registry.recordedCalls()[before:])
}

// An image project is addressed by its digest, so the dry run asks about that
// digest: the registry holds exactly these bytes, or it does not.
func TestDryRunProbesAnImageProjectByDigest(t *testing.T) {
	recording := newRecordingRegistry()
	host := strings.TrimPrefix(newTestServer(t, recording), "http://")
	ctx, manifest := newManagedImagePublishFixture(t)
	stubResolveToken(t, "")
	publish := func(dryRun bool) func() (string, map[string]any, error) {
		return func() (string, map[string]any, error) {
			return publishImmutableImageProject(ctx, jsonl.New(), dryRun, host, manifest)
		}
	}

	probe := oneProbe(t, dryRunProbes(t, publish(true)))
	if probe.State != extproto.MemberProbeAbsent || probe.Coordinate != manifest.Image || probe.Version != manifest.Version ||
		probe.Registry != host || probe.ArtifactDigest != manifest.Digest {
		t.Fatalf("probe = %+v, want the image absent at its content version", probe)
	}
	assertReadOnly(t, recording.recordedCalls())

	if status, data, err := publish(false)(); err != nil || status != "OK" {
		t.Fatalf("publish = (%q, %+v, %v), want OK", status, data, err)
	}
	before := len(recording.recordedCalls())
	probe = oneProbe(t, dryRunProbes(t, publish(true)))
	if probe.State != extproto.MemberProbeIdentical || probe.RegistryDigest != manifest.Digest {
		t.Fatalf("probe = %+v, want identical at %s", probe, manifest.Digest)
	}
	calls := recording.recordedCalls()[before:]
	assertReadOnly(t, calls)
	if heads := manifestHeads(calls); len(heads) != 1 || !strings.HasSuffix(heads[0], "/manifests/"+manifest.Digest) {
		t.Fatalf("the dry run sent manifest HEADs %v, want one, by digest", heads)
	}
}
