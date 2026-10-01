package jobs

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"

	"go.putnami.dev/cli/model/workspace"
	ciproto "go.putnami.dev/protocol/ci"
	distribution "go.putnami.dev/protocol/distribution"
	jobproto "go.putnami.dev/protocol/job"
	runtimeproto "go.putnami.dev/protocol/runtime"
	wsproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/dockerpublish"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/oci"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/registrycred"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/tooling/cli/internal/extension"
	workspaceprobe "go.putnami.dev/tooling/cli/internal/workspace"
)

// The SDK publisher, runtime event parser, and CLI finalizer are real. The OCI
// registry and its native admission policy are external boundary doubles; the
// production private broker and release-set persistence belong to Cloud.
func TestReleaseSetImageSDKPublishesPlannedMemberThroughPrivateBroker(t *testing.T) {
	testReleaseSetImageSDK(t, "")
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
	testReleaseSetImageSDK(t, binary)
}

func testReleaseSetImageSDK(t *testing.T, probeBinary string) {
	t.Helper()
	const projectPath = "delivery/images/ci-runner"
	const token = "image-sdk-test-private-capability"
	root := t.TempDir()
	config, policy := imageSDKWorkspaceConfig(t, root, projectPath)
	coordinate := imageSDKCoordinate(t, config.Registries, config.Name, projectPath)
	image, err := random.Image(512, 1)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := image.Digest()
	if err != nil {
		t.Fatal(err)
	}
	contentHash := strings.Repeat("e", 64)
	manifest := &pkgmeta.DockerManifest{
		Image: "delivery-images-ci-runner", Tags: []string{"delivery-images-ci-runner:c-" + contentHash},
		Version: "c-" + contentHash, ContentHash: contentHash, Digest: digest.String(), Layout: "oci", Platform: "linux/amd64",
	}
	dockerDir := pkgmeta.PackageOutputDir(root, projectPath, "docker")
	if _, err := oci.WriteLayout(filepath.Join(dockerDir, manifest.Layout), image); err != nil {
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
	registriesJSON, err := json.Marshal(registries)
	if err != nil {
		t.Fatal(err)
	}
	releaseNamespace := distributionNamespace(policy, ws.Name)
	t.Logf("workspace=%s OCI member=%s distribution namespace=%s", ws.Name, coordinate, releaseNamespace)
	// The temporary workspace is not a git clone. The established fixture seam
	// supplies source revision and task fingerprints, while preparation itself
	// resolves the provider and constructs the actual selected plan.
	useReleaseSetProvenance(t, nil)
	ctx := &pctx.Context{
		WorkspaceRoot: root,
		Workspace:     pctx.Workspace{Name: ws.Name, RootPath: root},
		Project:       pctx.Project{Name: project.Name, Path: project.Path, Type: project.Type},
		OutputPath:    filepath.Join(root, ".putnami", "out", projectPath, "publish"),
		Identity:      &jobproto.TaskIdentity{Project: jobproto.ProjectIdentity{ID: project.ID, Name: project.Name}},
		Params:        pctx.Params{"registries": registriesJSON},
	}
	broker := &imageSDKBrokerPolicyDouble{inner: registry.New(), coordinate: coordinate, digest: manifest.Digest, token: token}
	server := httptest.NewServer(broker)
	t.Cleanup(server.Close)
	t.Setenv("PUTNAMI_REGISTRY_OCI_URL", server.URL+"/oci")
	t.Setenv("DOCKER_REGISTRY", "")
	previousResolveToken := registrycred.ResolveToken
	registrycred.ResolveToken = func(host string) (string, string) {
		if host != strings.TrimPrefix(server.URL, "http://") {
			t.Errorf("credential host = %q, want the private broker", host)
			return "", "unexpected credential host"
		}
		return token, ""
	}
	t.Cleanup(func() { registrycred.ResolveToken = previousResolveToken })

	for _, attempt := range []string{"push", "hit"} {
		t.Run(attempt, func(t *testing.T) {
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
			planJSON, err := json.Marshal(run.Plan())
			if err != nil {
				t.Fatal(err)
			}
			ctx.Params[releaseset.ContextParamName] = planJSON
			before := len(broker.recordedRequests())
			events, contentStatus := publishImageSDKEvents(t, ctx)
			wantContentStatus := "pushed"
			if attempt == "hit" {
				wantContentStatus = "reused"
			}
			if contentStatus != wantContentStatus {
				t.Fatalf("contentStatus = %s, want %s", contentStatus, wantContentStatus)
			}
			legacyEvidence := 0
			for _, event := range events {
				if event.Type == "artifact" && event.Data["kind"] == "published" && event.Data["registry"] == "docker" {
					legacyEvidence++
					if event.Data["imageDigest"] != manifest.Digest || event.Data["digestVerified"] != true ||
						event.Data["immutableRef"] != "oci.putnami.dev/"+coordinate+"@"+manifest.Digest ||
						event.Data["contentStatus"] != wantContentStatus {
						t.Fatalf("legacy publication evidence = %+v", event.Data)
					}
				}
			}
			if legacyEvidence != 1 {
				t.Fatalf("legacy publication event count = %d, want 1", legacyEvidence)
			}
			results := map[string]*JobResult{member.ProjectID + ":publish": {Status: "success", Events: events}}
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
				t.Fatalf("released member = %+v, want the planned identity and verified image digest", got)
			}
			if head, ok := ledger.head(releaseNamespace, "canary"); !ok || head != outcome.Ref {
				t.Fatalf("canary head = %+v, %v, want %+v", head, ok, outcome.Ref)
			}
			manifestPuts := 0
			for _, request := range broker.recordedRequests()[before:] {
				if strings.Contains(request.path, "/manifests/") && !strings.HasSuffix(request.path, "/manifests/"+manifest.Digest) {
					t.Fatalf("publisher used a tag instead of the immutable digest: %+v", request)
				}
				if request.method == http.MethodPut && strings.Contains(request.path, "/manifests/") {
					manifestPuts++
				}
				if attempt == "hit" && request.method != http.MethodGet && request.method != http.MethodHead {
					t.Fatalf("digest hit performed a registry mutation: %+v", request)
				}
			}
			if attempt == "push" && manifestPuts != 1 {
				t.Fatalf("manifest PUT count = %d, want 1", manifestPuts)
			}
		})
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

func publishImageSDKEvents(t *testing.T, ctx *pctx.Context) ([]RawJobEvent, string) {
	t.Helper()
	output, err := os.CreateTemp(t.TempDir(), "sdk-events-")
	if err != nil {
		t.Fatal(err)
	}
	previousStdout := os.Stdout
	os.Stdout = output
	defer func() {
		os.Stdout = previousStdout
		_ = output.Close()
	}()
	status, data, err := dockerpublish.Publish(ctx, jsonl.NewForVersion(runtimeproto.ProtocolVersion2), nil)
	if err != nil || status != "OK" {
		t.Fatalf("SDK Publish = %s, %+v, %v", status, data, err)
	}
	raw, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	var events []RawJobEvent
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		event, ok := parseRawEvent(line)
		if !ok {
			t.Fatalf("SDK event rejected by CLI runtime parser: %s", line)
		}
		events = append(events, event)
	}
	contentStatus, ok := data["contentStatus"].(string)
	if !ok {
		t.Fatalf("SDK publication lacks a content status: %+v", data)
	}
	return events, contentStatus
}

type imageSDKBrokerRequest struct{ method, path string }

type imageSDKBrokerPolicyDouble struct {
	inner                     http.Handler
	coordinate, digest, token string
	mu                        sync.Mutex
	requests                  []imageSDKBrokerRequest
}

func (broker *imageSDKBrokerPolicyDouble) recordedRequests() []imageSDKBrokerRequest {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]imageSDKBrokerRequest(nil), broker.requests...)
}

func (broker *imageSDKBrokerPolicyDouble) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	broker.mu.Lock()
	broker.requests = append(broker.requests, imageSDKBrokerRequest{method: request.Method, path: request.URL.Path})
	broker.mu.Unlock()
	if request.Header.Get("Authorization") != "Bearer "+broker.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if request.URL.Path != "/oci/v2/" && !strings.HasPrefix(request.URL.Path, "/oci/v2/"+broker.coordinate+"/") {
		http.Error(w, "unplanned repository", http.StatusForbidden)
		return
	}
	if _, selector, found := strings.Cut(request.URL.Path, "/manifests/"); found && selector != broker.digest {
		http.Error(w, "image publication must address its planned digest", http.StatusForbidden)
		return
	}
	upstreamRequest := request.Clone(request.Context())
	upstreamURL := *request.URL
	upstreamURL.Path = strings.TrimPrefix(request.URL.Path, "/oci")
	upstreamRequest.URL = &upstreamURL
	upstreamRequest.RequestURI = upstreamURL.RequestURI()
	recorder := httptest.NewRecorder()
	broker.inner.ServeHTTP(recorder, upstreamRequest)
	result := recorder.Result()
	defer result.Body.Close()
	for name, values := range result.Header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	if location := result.Header.Get("Location"); location != "" {
		parsed, err := url.Parse(location)
		if err != nil {
			http.Error(w, "invalid registry Location", http.StatusInternalServerError)
			return
		}
		resolved := (&url.URL{Scheme: "http", Host: request.Host}).ResolveReference(parsed)
		resolved.Path = "/oci" + resolved.Path
		w.Header().Set("Location", resolved.String())
	}
	w.WriteHeader(result.StatusCode)
	_, _ = io.Copy(w, result.Body)
}
