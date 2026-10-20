package publication

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/cli/model/workspace"
	ciproto "go.putnami.dev/protocol/ci"
	distribution "go.putnami.dev/protocol/distribution"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/tooling/cli/internal/extension"
	workspaceprobe "go.putnami.dev/tooling/cli/internal/workspace"
)

// The release-set preparation and the CLI finalizer are real; the image
// publication job is the published-member event it reports. The SDK image
// publisher's own tests pin that it reports this event for the planned
// member at the digest it verified (dockerpublish).
func TestReleaseSetReleasesAPlannedImageMember(t *testing.T) {
	testReleaseSetImageMember(t, "")
}

// Build @putnami/go through ./putnamiw first, then bind that exact executable.
// This additional proof includes the real source probe; the ordinary test
// above starts from a member declaration fixture and still runs PrepareReleaseSet.
// PUTNAMI_IMAGE_SDK_PROJECT_FILE optionally binds an authored runner manifest
// verbatim instead of the minimal source fixture. PUTNAMI_IMAGE_SDK_WORKSPACE_FILE
// binds its workspace name and registries plus the sibling putnami.ci.json policy.
func TestReleaseSetImageSDKFromBuiltGoProbe(t *testing.T) {
	binary := os.Getenv("PUTNAMI_IMAGE_SDK_PROBE_BINARY")
	if binary == "" {
		t.Skip("requires PUTNAMI_IMAGE_SDK_PROBE_BINARY from an explicit Putnami @putnami/go build")
	}
	testReleaseSetImageMember(t, binary)
}

func testReleaseSetImageMember(t *testing.T, probeBinary string) {
	t.Helper()
	const projectPath = "delivery/images/ci-runner"
	root := t.TempDir()
	config, policy := imageSDKWorkspaceConfig(t, root, projectPath)
	coordinate := imageSDKCoordinate(t, config.Registries, config.Name, projectPath)
	contentHash := strings.Repeat("e", 64)
	manifest := &pkgmeta.DockerManifest{
		Image: "delivery-images-ci-runner", Tags: []string{"delivery-images-ci-runner:c-" + contentHash},
		Version: "c-" + contentHash, ContentHash: contentHash, Digest: digestFor('7'), Layout: "oci", Platform: "linux/amd64",
	}
	dockerDir := pkgmeta.PackageOutputDir(root, projectPath, "docker")
	if err := os.MkdirAll(dockerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dockerDir, "manifest.json"), manifestJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	project := &workspace.Project{
		ID: "/" + projectPath, Path: projectPath, Name: projectPath, Type: "image", Version: "1.2.3-native",
		Metadata: releaseSetProjectMembers(t, releaseset.MemberDeclaration{
			Ecosystem: "oci", Coordinate: coordinate, PackageStep: "image", PublishStep: "docker",
		}),
	}
	ws := workspace.NewWorkspace(root, config, []*workspace.Project{project})
	ws.Registries = config.Registries
	if probeBinary != "" {
		ws, project = loadImageSDKWorkspace(t, root, projectPath, probeBinary)
		project.Version = "1.2.3-native"
	}
	registries := project.Registries
	if len(registries) == 0 {
		registries = ws.Registries
	}
	coordinate = imageSDKCoordinate(t, registries, ws.Name, project.Name)
	releaseNamespace := distributionNamespace(policy, ws.Name)
	t.Logf("workspace=%s OCI member=%s distribution namespace=%s", ws.Name, coordinate, releaseNamespace)
	// The temporary workspace is not a git clone. The established fixture seam
	// supplies source revision and task fingerprints, while preparation itself
	// resolves the provider and constructs the actual selected plan.
	useReleaseSetProvenance(t, nil)

	ledger := newReleaseSetE2ELedger()
	useReleaseSetE2EProvider(t, ledger)
	options := releaseSetE2EAllOptions()
	options.Policy = policy
	run := releaseSetE2EPrepare(t, t.Context(), options, ws, []*workspace.Project{project})
	member, found := run.Plan().Member("oci", coordinate)
	if !found || !member.Selected || len(run.Plan().Members) != 1 || member.ProjectID != project.ID || member.Version != project.Version {
		t.Fatalf("prepared image member = %+v, found=%v; plan=%+v", member, found, run.Plan())
	}
	route := run.routes[releaseset.MemberKey(member.Ecosystem, member.Coordinate)]
	if route.packageStep != "image" || route.publishStep != "docker" || route.publishCommand != "publish" {
		t.Fatalf("prepared image route = %+v, want package/image and publish/docker", route)
	}
	events := []jobmodel.RawJobEvent{publishedMemberEvent(member, manifest.Digest)}
	results := map[string]*jobmodel.JobResult{member.ProjectID + ":publish": {Status: "success", Events: events}}
	run.Finalizer(context.Background())(results)
	outcome := releaseSetE2EOutcome(t, results)
	set := ledger.mustReleaseSet(t, outcome.Ref)
	if len(set.Members) != 1 {
		t.Fatalf("released members = %+v", set.Members)
	}
	got := set.Members[0]
	if got.Coordinate != coordinate || got.Ecosystem != "oci" || got.Version != member.Version ||
		got.ArtifactDigest != manifest.Digest || got.Project != projectPath || got.Kind != distribution.KindImage ||
		got.SourceRevision != testRevision || got.SelectionFingerprint != member.SelectionFingerprint {
		t.Fatalf("released member = %+v, want the planned identity and the reported image digest", got)
	}
	if head, ok := ledger.head(releaseNamespace, "canary"); !ok || head != outcome.Ref {
		t.Fatalf("canary head = %+v, %v, want %+v", head, ok, outcome.Ref)
	}
}

// Keep the three identities independent: the workspace name, the OCI registry
// namespace, and the release-set provider's CI policy namespace need not match.
func imageSDKWorkspaceConfig(t *testing.T, root, projectPath string) (*wsproto.Config, *ciproto.Distribution) {
	t.Helper()
	config := &wsproto.Config{Name: "team-workspace", Registries: map[string]json.RawMessage{"oci": json.RawMessage(`{"publish":"oci.putnami.dev/team-images"}`)}}
	policy := &ciproto.Distribution{Namespace: "team-release", MemberAttribution: true}
	if source := os.Getenv("PUTNAMI_IMAGE_SDK_WORKSPACE_FILE"); source != "" {
		raw, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		var authored wsproto.Config
		if err := json.Unmarshal(raw, &authored); err != nil {
			t.Fatal(err)
		}
		config.Name, config.Registries = authored.Name, authored.Registries
		policy, err = LoadDistributionPolicy(filepath.Dir(source))
		if err != nil || policy == nil {
			t.Fatalf("bound workspace CI distribution policy = %+v, %v", policy, err)
		}
		ciJSON, err := os.ReadFile(filepath.Join(filepath.Dir(source), ciproto.Filename))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ciproto.Filename), ciJSON, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("bound workspace identity/registries from %s and distribution policy from %s", source, ciproto.Filename)
	}
	config.Includes = []string{projectPath}
	// Synchronize binds the source Go extension declaration to the exact built
	// executable below; unrelated installed extensions are not part of this fixture.
	config.Extensions = wsproto.ExtensionsConfig{List: map[string]string{"@putnami/go": ""}}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "putnami.workspace.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return config, policy
}

func imageSDKCoordinate(t *testing.T, registries map[string]json.RawMessage, workspaceName, projectName string) string {
	t.Helper()
	normalize := func(value string) string {
		return strings.ToLower(strings.ReplaceAll(strings.Trim(strings.TrimPrefix(strings.TrimSpace(value), "@"), "/"), "/", "-"))
	}
	var registry struct {
		Publish string `json:"publish"`
	}
	if raw := registries["oci"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &registry); err != nil {
			t.Fatal(err)
		}
	}
	namespace := normalize(workspaceName)
	if _, configured, ok := strings.Cut(strings.TrimRight(registry.Publish, "/"), "/"); ok {
		namespace = configured
	}
	return namespace + "/" + normalize(projectName)
}

func loadImageSDKWorkspace(t *testing.T, root, projectPath, binary string) (*workspace.Workspace, *workspace.Project) {
	t.Helper()
	if !filepath.IsAbs(binary) {
		t.Fatal("PUTNAMI_IMAGE_SDK_PROBE_BINARY must name the absolute path of the built Go extension")
	}
	if err := os.MkdirAll(filepath.Join(root, projectPath), 0o755); err != nil {
		t.Fatal(err)
	}
	projectJSON := []byte(`{"name":"delivery/images/ci-runner","type":"image","publish":["docker"]}`)
	if projectFile := os.Getenv("PUTNAMI_IMAGE_SDK_PROJECT_FILE"); projectFile != "" {
		var err error
		projectJSON, err = os.ReadFile(projectFile)
		if err != nil {
			t.Fatalf("read bound image project manifest: %v", err)
		}
		t.Logf("probing exact authored project manifest %s (%d bytes)", projectFile, len(projectJSON))
	}
	// No go.mod: the runner is an authored image, not a Go source project.
	for path, data := range map[string]string{
		filepath.Join(projectPath, "putnami.json"): string(projectJSON),
	} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ext := extension.LoadExtensionFromDir(filepath.Join(findJobsRepoRoot(t), "go", "extension"), "/go/extension")
	if ext == nil {
		t.Fatal("load shipped Go extension declaration")
	}
	scope, ok := workspaceprobe.NewProviderScope(ext.Name, ext.Workspace)
	if !ok {
		t.Fatal("shipped Go extension has no workspace scope")
	}
	scanned, err := workspaceprobe.ScanProjectPathsWithProviders(root, []workspaceprobe.ProviderScope{scope})
	if err != nil || !slices.Contains(scanned, projectPath) {
		t.Fatalf("core scan = %v, %v; want the authored image without a go.mod", scanned, err)
	}
	ws, err := workspaceprobe.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	// Use the ordinary resolved-member path. In particular, do not manufacture
	// ProbeRequest.Paths or override Synchronize's candidate selection: this
	// proves core asks the Go provider about this image despite its go.mod marker.
	provider := &workspaceprobe.ExecProbeProvider{Extension: ext.Name, Executable: binary, Dir: root, Context: t.Context()}
	outcome, err := workspaceprobe.Synchronize(workspaceprobe.SyncRequest{
		Context: t.Context(), Workspace: ws, Reason: wsproto.ProbeReasonLoad,
		Providers: []workspaceprobe.ProviderBinding{{Scope: scope, Provider: provider}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(outcome.Probed, ext.Name) {
		t.Fatalf("core did not invoke the source Go probe: %+v", outcome)
	}
	if err := ws.AdoptValidatedProbeView(outcome.Merged); err != nil {
		t.Fatal(err)
	}
	project := ws.ProjectByPath(projectPath)
	if project == nil || project.Name != projectPath || project.Type != "image" || len(project.Metadata[ext.Name]) == 0 {
		t.Fatalf("core loaded image = %+v, want authored image with Go provider metadata", project)
	}
	return ws, project
}
