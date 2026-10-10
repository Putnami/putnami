package cloudcli

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	distributionproto "go.putnami.dev/protocol/distribution"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/publicationoutbox"
	"go.putnami.dev/sdk/extension/releaseset"
)

// refusingOutboxClient fails a test that reaches any registry: a publish job
// under publication-v1 holds no credential and uploads nothing itself.
func refusingOutboxClient(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("a packing publish step reached the registry: %s %s", request.Method, request.URL)
		return nil, nil
	})}
}

func refusingArtifact(t *testing.T) func(string, string, string, string, map[string]any) error {
	t.Helper()
	return func(id, _, kind, _ string, _ map[string]any) error {
		t.Fatalf("a packing publish step emitted artifact %s (%s); the engine reports the member", id, kind)
		return nil
	}
}

// The engine derives a member's kind from its ecosystem and declared steps
// (releaseset.KindFor) and admits it to publication-v1 only when put-write/v1
// has a profile for that kind. The Cloud publish steps that pack must
// map to the kinds whose bytes they pack.
func TestCloudPackingStepsMapToPublicationKinds(t *testing.T) {
	for _, test := range []struct {
		ecosystem                distributionproto.Ecosystem
		packageStep, publishStep string
		want                     distributionproto.MemberKind
	}{
		{cloudConfigEcosystem, cloudConfigPackageStep, cloudConfigPublishStep, distributionproto.KindConfig},
		{cloudArchiveEcosystem, cloudArchivePackageStep, cloudArchivePublishStep, distributionproto.KindArchive},
		{cloudArchiveEcosystem, cloudTemplatePackageStep, cloudArchivePublishStep, distributionproto.KindArchive},
		{cloudSiteContentEcosystem, cloudSiteContentPackageStep, cloudSiteContentPublishStep, distributionproto.KindDoc},
		{cloudDeploymentEcosystem, cloudDeploymentPackageStep, cloudDeploymentPublishStep, distributionproto.KindDeployment},
	} {
		if got := releaseset.KindFor(test.ecosystem, test.packageStep, test.publishStep); got != test.want {
			t.Errorf("KindFor(%s, %s, %s) = %q, want %q", test.ecosystem, test.packageStep, test.publishStep, got, test.want)
		}
	}
}

func TestManagedConfigPublishPacksIntoThePublicationOutbox(t *testing.T) {
	root, prepared := configMemberPackageFixture(t)
	outbox := filepath.Join(t.TempDir(), "outbox")
	params := configMemberPlan(prepared, "sha256:"+strings.Repeat("f", 64))
	var stdout []string
	err := publishConfig(params, nil, root, map[string]string{"PUTNAMI_HOME": t.TempDir(), extensionproto.PublicationOutboxEnv: outbox}, IO{
		Client: refusingOutboxClient(t), Artifact: refusingArtifact(t),
		Stdout: func(line string) { stdout = append(stdout, line) },
	})
	if err != nil {
		t.Fatal(err)
	}
	packed, err := publicationoutbox.Read(outbox)
	if err != nil {
		t.Fatalf("engine reader refused the outbox: %v", err)
	}
	if len(packed.Descriptor.Members) != 1 {
		t.Fatalf("outbox members = %+v, want exactly the planned Config member", packed.Descriptor.Members)
	}
	member := packed.Descriptor.Members[0]
	if member.Ecosystem != extensionproto.OutboxEcosystemPut || member.Coordinate != prepared.Coordinate ||
		member.Version != prepared.Version || member.Project != prepared.ProjectID || member.Put == nil ||
		member.Put.MediaType != "application/vnd.putnami.config.authored-member+json" || len(member.Put.Blobs) != 0 {
		t.Fatalf("packed member = %+v, want the planned Config member with no blob", member)
	}
	manifest, err := packed.ReadFile(member.Put.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(manifest, prepared.Member.Bytes) || member.Put.Manifest.Digest != prepared.Member.Descriptor.ContentDigest {
		t.Fatalf("packed manifest %s (%s), want the packaged member bytes (%s)", manifest, member.Put.Manifest.Digest, prepared.Member.Descriptor.ContentDigest)
	}
	if got := strings.Join(stdout, "\n"); !strings.Contains(got, "Packed Config member workspace-native/my-app-config@1.2.3 into the publication outbox.") {
		t.Fatalf("stdout = %q", got)
	}
}

func TestUnmanagedConfigPublishRefusesThePublicationOutbox(t *testing.T) {
	root, _ := configMemberPackageFixture(t)
	outbox := filepath.Join(t.TempDir(), "outbox")
	err := publishConfig(map[string]any{"app": "my-app", "namespace": "workspace-native", "env": "prod"}, nil, root,
		map[string]string{"PUTNAMI_HOME": t.TempDir(), extensionproto.PublicationOutboxEnv: outbox},
		IO{Client: refusingOutboxClient(t), Artifact: refusingArtifact(t)})
	if err == nil || !strings.Contains(err.Error(), "the job carries no plan") {
		t.Fatalf("publishConfig error = %v, want a refusal without a plan", err)
	}
	if _, statErr := os.Stat(outbox); !os.IsNotExist(statErr) {
		t.Fatalf("a refused publish touched the outbox: %v", statErr)
	}
}

// RunMain is the entry point the engine runs for the archive publish step.
// Under publication-v1 it packs the planned member and emits no published
// member event: the engine reports the member after its own upload.
func TestPublishArchivesUnderThePublicationOutboxEmitsNoMember(t *testing.T) {
	workspaceRoot := archiveMemberWorkspace(t)
	outbox := filepath.Join(t.TempDir(), "outbox")
	contextFile := filepath.Join(t.TempDir(), "context.json")
	writeJSONFile(t, contextFile, map[string]any{
		"workspaceRoot": workspaceRoot,
		"params": map[string]any{
			"app": "apps/cli", "channel": "stable",
			releaseset.ContextParamName: &releaseset.Plan{
				ProtocolVersion: distributionproto.ProtocolVersion,
				Namespace:       "putnami-cloud",
				Channels:        []string{"stable"},
				Heads:           map[string]*distributionproto.ChannelHead{"stable": nil},
				Members: []releaseset.PlannedMember{{
					Ecosystem: "archive", Coordinate: "putnami/cloud", Version: "1.2.3",
					SourceRevision: strings.Repeat("a", 40), SelectionFingerprint: "sha256:" + strings.Repeat("b", 64),
					ProjectID: "/apps/cli", Selected: true,
				}},
			},
		},
	})
	var output []string
	exitCode := RunMain([]string{"publish-archives", "--putnamiContext", contextFile}, IO{
		Env:    map[string]string{"PUTNAMI_HOME": t.TempDir(), extensionproto.PublicationOutboxEnv: outbox},
		Client: refusingOutboxClient(t),
		Stdout: func(line string) { output = append(output, line) },
		Stderr: func(string) {},
	})
	if exitCode != ExitSuccess {
		t.Fatalf("RunMain exit = %d, output = %v", exitCode, output)
	}
	if artifacts := eventsOfType(decodeEvents(t, output), "artifact"); len(artifacts) != 0 {
		t.Fatalf("artifact events = %+v, want none under publication-v1", artifacts)
	}
	packed, err := publicationoutbox.Read(outbox)
	if err != nil {
		t.Fatalf("engine reader refused the outbox: %v", err)
	}
	if len(packed.Descriptor.Members) != 1 {
		t.Fatalf("outbox members = %+v", packed.Descriptor.Members)
	}
	member := packed.Descriptor.Members[0]
	if member.Ecosystem != extensionproto.OutboxEcosystemArchive || member.Coordinate != "putnami/cloud" ||
		member.Version != "1.2.3" || member.Project != "/apps/cli" || member.Put == nil || len(member.Put.Blobs) == 0 {
		t.Fatalf("packed member = %+v, want the planned archive member", member)
	}
}
