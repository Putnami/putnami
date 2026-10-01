package dockerpublish

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	distribution "go.putnami.dev/protocol/distribution"
	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	job "go.putnami.dev/protocol/job"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/registrycred"
	"go.putnami.dev/sdk/extension/releaseset"
)

func imageReleasePlan(ctx *pctx.Context) *releaseset.Plan {
	return &releaseset.Plan{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "team", Channels: []string{"canary"},
		Heads: map[string]*distribution.ChannelHead{"canary": nil},
		Members: []releaseset.PlannedMember{{
			Ecosystem: "oci", Coordinate: "team/delivery-images-ci-runner", Version: "1.2.3-native",
			Dependencies: []distribution.ReleaseSetDependency{}, SourceRevision: strings.Repeat("a", 40),
			SelectionFingerprint: "sha256:" + strings.Repeat("b", 64),
			ProjectID:            "/" + ctx.Project.Path, Selected: true,
		}},
	}
}

func bindImageReleasePlan(t *testing.T, ctx *pctx.Context, plan *releaseset.Plan) {
	t.Helper()
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	ctx.Params = pctx.Params{releaseset.ContextParamName: data}
	ctx.Identity = &job.TaskIdentity{Project: job.ProjectIdentity{ID: "/" + ctx.Project.Path}}
}

func TestImagePublishNativePlanEmitsVerifiedMemberWithoutTags(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "oci-publish-by-digest", "image-member-matches-native-plan")
	spectest.Proves(t, "tooling/extension-authoring", "oci-publish-by-digest", "image-private-publish-writes-only-digest")
	ctx, manifest := newManagedImagePublishFixture(t)
	ctx.Workspace.Name = "acme-cloud"
	plan := imageReleasePlan(ctx)
	plan.Namespace = "cloud"
	plan.Members[0].Coordinate = "putnami/" + manifest.Image
	bindImageReleasePlan(t, ctx, plan)
	ctx.Params["registries"] = json.RawMessage(`{"oci":{"publish":"oci.putnami.dev/putnami"}}`)
	writeJSONFile(t, filepath.Join(pkgmeta.PackageOutputDir(ctx.WorkspaceRoot, ctx.Project.Path, "docker"), "manifest.json"), manifest)
	broker := newPrivateOCITestBroker(t, "")
	broker.manifestPolicy = func(selector string) bool { return selector == manifest.Digest }
	t.Setenv(privateOCIRegistryURLEnv, broker.server.URL+"/oci")
	old := registrycred.ResolveToken
	registrycred.ResolveToken = func(string) (string, string) { return privateOCITestToken, "" }
	t.Cleanup(func() { registrycred.ResolveToken = old })

	for _, wantStatus := range []string{"pushed", "reused"} {
		t.Run(wantStatus, func(t *testing.T) {
			events := captureEvents(t, func() {
				status, result, err := Publish(ctx, jsonl.New(), nil)
				if err != nil || status != "OK" || result["contentStatus"] != wantStatus {
					t.Fatalf("publish = %s, %+v, %v", status, result, err)
				}
				if result["image"] != managedOCIRegistry+"/"+plan.Members[0].Coordinate+"@"+manifest.Digest {
					t.Fatalf("published image = %v, want the plan's registry namespace", result["image"])
				}
			})
			members := eventsOfKind(events, extproto.PublishedMemberEventKind)
			if len(members) != 1 {
				t.Fatalf("members = %+v, want one verified member", members)
			}
			member := members[0]
			if eventString(t, member, "coordinate") != plan.Members[0].Coordinate ||
				eventString(t, member, "version") != plan.Members[0].Version ||
				eventString(t, member, "artifactDigest") != manifest.Digest {
				t.Fatalf("member = %+v, want exact planned identity and verified digest", member)
			}
			legacy := eventsOfKind(events, "published")
			if len(legacy) != 1 || legacy[0]["digestVerified"] != true || legacy[0]["imageDigest"] != manifest.Digest {
				t.Fatalf("legacy evidence = %+v, want verified publication alongside member", legacy)
			}
		})
	}
	puts := 0
	for _, call := range broker.recordedCalls() {
		if call.Path != "/oci/v2/" && !strings.HasPrefix(call.Path, "/oci/v2/"+plan.Members[0].Coordinate+"/") {
			t.Fatalf("publication escaped its planned repository: %+v", call)
		}
		if call.Method == http.MethodPut && strings.Contains(call.Path, "/manifests/") {
			puts++
			if !strings.HasSuffix(call.Path, "/manifests/"+manifest.Digest) {
				t.Fatalf("image publication wrote a tag: %+v", call)
			}
		}
	}
	if puts != 1 {
		t.Fatalf("manifest writes = %d, want one digest push and no write for reuse", puts)
	}
}

func TestImagePublishRejectsMismatchedPlanBeforeCredentials(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*releaseset.Plan)
	}{
		{"coordinate", func(plan *releaseset.Plan) { plan.Members[0].Coordinate = "team/other" }},
		{"project", func(plan *releaseset.Plan) { plan.Members[0].ProjectID = "/other" }},
		{"configured namespace", func(plan *releaseset.Plan) { plan.Members[0].Coordinate = "other/delivery-images-ci-runner" }},
		{"ambiguous namespaces", func(plan *releaseset.Plan) {
			other := plan.Members[0]
			other.Coordinate = "zz-other/delivery-images-ci-runner"
			plan.Members = append(plan.Members, other)
		}},
		{"unselected", func(plan *releaseset.Plan) {
			member := &plan.Members[0]
			member.Selected = false
			member.ArtifactDigest = "sha256:" + strings.Repeat("d", 64)
			base := &distribution.ReleaseSet{
				ProtocolVersion: distribution.ProtocolVersion, Namespace: plan.Namespace,
				Members: []distribution.ReleaseSetMember{{
					Ecosystem: member.Ecosystem, Coordinate: member.Coordinate, Version: member.Version,
					ArtifactDigest: member.ArtifactDigest, Dependencies: member.Dependencies,
					SourceRevision: member.SourceRevision, SelectionFingerprint: member.SelectionFingerprint,
				}},
			}
			ref, diagnostics := distribution.DeriveReleaseSetRef(base)
			if len(diagnostics) != 0 {
				t.Fatal(diagnostics)
			}
			plan.Heads["canary"] = &distribution.ChannelHead{Ref: ref, Generation: 1, ReleaseSet: base}
			if err := releaseset.ValidatePlan(plan); err != nil {
				t.Fatalf("inherited plan fixture: %v", err)
			}
		}},
		{"invalid", func(plan *releaseset.Plan) { plan.ProtocolVersion = 999 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, manifest := newManagedImagePublishFixture(t)
			plan := imageReleasePlan(ctx)
			test.mutate(plan)
			if test.name != "invalid" {
				if err := releaseset.ValidatePlan(plan); err != nil {
					t.Fatalf("plan fixture must be valid before checking target authority: %v", err)
				}
			}
			bindImageReleasePlan(t, ctx, plan)
			old := registrycred.ResolveToken
			registrycred.ResolveToken = func(string) (string, string) {
				t.Fatal("mismatched plan reached credential resolution")
				return "", ""
			}
			t.Cleanup(func() { registrycred.ResolveToken = old })
			events := captureEvents(t, func() {
				status, _, err := publishImmutableImageProject(ctx, jsonl.New(), false, "oci.putnami.dev/team", manifest)
				if status != "FAILED" || err == nil {
					t.Fatalf("publish = %s, %v, want plan refusal", status, err)
				}
				if test.name == "ambiguous namespaces" && !strings.Contains(err.Error(), "ambiguous selected OCI members") {
					t.Fatalf("ambiguous plan failed for the wrong reason: %v", err)
				}
				if test.name == "configured namespace" && !strings.Contains(err.Error(), "managed OCI registry override") {
					t.Fatalf("namespace mismatch failed for the wrong reason: %v", err)
				}
			})
			if members := eventsOfKind(events, extproto.PublishedMemberEventKind); len(members) != 0 {
				t.Fatalf("failed publication emitted member: %+v", members)
			}
		})
	}
}

func TestImageManagedTargetKeepsStandaloneNamespaceRestriction(t *testing.T) {
	ctx, manifest := newManagedImagePublishFixture(t)
	ctx.Workspace.Name = "acme-cloud"
	if _, err := resolveImagePublishTarget(ctx, "oci.putnami.dev/putnami", manifest.Image); err == nil {
		t.Fatal("standalone publication accepted a namespace unrelated to its workspace")
	}
	target, err := resolveImagePublishTarget(ctx, "oci.putnami.dev/acme-cloud", manifest.Image)
	if err != nil || target.Repository != "oci.putnami.dev/acme-cloud/"+manifest.Image {
		t.Fatalf("standalone target = %+v, %v", target, err)
	}
}

func TestImagePublicationUsesTypedProjectIdentity(t *testing.T) {
	ctx, manifest := newManagedImagePublishFixture(t)
	plan := imageReleasePlan(ctx)
	bindImageReleasePlan(t, ctx, plan)
	ctx.Project.Path = "(group)/" + ctx.Project.Path
	target, err := resolveImagePublishTarget(ctx, "", manifest.Image)
	if err != nil {
		t.Fatal(err)
	}
	// The physical path contains a transparent folder while the typed identity
	// and the planned owner keep the same canonical logical project ID.
	version, err := imagePublicationVersion(ctx, target, manifest.Version)
	if err != nil || version != plan.Members[0].Version {
		t.Fatalf("typed publication identity = %q, %v", version, err)
	}
	ctx.Identity = nil
	if _, err := imagePublicationVersion(ctx, target, manifest.Version); err == nil {
		t.Fatal("native publication accepted an absent typed identity")
	}
	old := registrycred.ResolveToken
	registrycred.ResolveToken = func(string) (string, string) {
		t.Fatal("missing typed identity reached credential resolution")
		return "", ""
	}
	t.Cleanup(func() { registrycred.ResolveToken = old })
	for _, identity := range []*job.TaskIdentity{nil, {}} {
		ctx.Identity = identity
		status, _, err := publishImmutableImageProject(ctx, jsonl.New(), false, "", manifest)
		if status != "FAILED" || err == nil {
			t.Fatalf("publish with missing typed identity = %s, %v", status, err)
		}
	}
}

func TestImagePublishEmitsNoMemberWithoutVerifiedPublication(t *testing.T) {
	for _, mode := range []string{"dry-run", "failed-push", "digest-mismatch"} {
		t.Run(mode, func(t *testing.T) {
			ctx, manifest := newManagedImagePublishFixture(t)
			bindImageReleasePlan(t, ctx, imageReleasePlan(ctx))
			broker := newPrivateOCITestBroker(t, "")
			if mode == "failed-push" {
				broker.hostileLocation = "https://attacker.invalid/v2/team/runner/blobs/uploads/escape"
			}
			if mode == "digest-mismatch" {
				manifest.Digest = "sha256:" + strings.Repeat("f", 64)
			}
			t.Setenv(privateOCIRegistryURLEnv, broker.server.URL+"/oci")
			old := registrycred.ResolveToken
			registrycred.ResolveToken = func(string) (string, string) { return privateOCITestToken, "" }
			t.Cleanup(func() { registrycred.ResolveToken = old })
			events := captureEvents(t, func() {
				status, _, err := publishImmutableImageProject(ctx, jsonl.New(), mode == "dry-run", "", manifest)
				if mode == "dry-run" {
					if status != "OK" || err != nil {
						t.Fatalf("dry run = %s, %v", status, err)
					}
				} else if status != "FAILED" || err == nil {
					t.Fatalf("unverified publication = %s, %v", status, err)
				}
			})
			if len(eventsOfKind(events, extproto.PublishedMemberEventKind)) != 0 || len(eventsOfKind(events, "published")) != 0 {
				t.Fatalf("unverified publication emitted evidence: %+v", events)
			}
		})
	}
}
