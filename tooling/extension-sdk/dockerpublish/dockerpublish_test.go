package dockerpublish

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/oci"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/registrycred"
	"go.putnami.dev/sdk/extension/scratch"
)

// TestMain clears the registry routing the host process may carry. Under
// native publication the CI runner exports PUTNAMI_REGISTRY_OCI_URL as a
// loopback broker and DOCKER_REGISTRY may name a real registry; both would
// change which transport and host the code under test picks. Tests that
// need either value pin it with t.Setenv, which restores this cleared state.
// A publication outbox inherited from an enclosing publication would turn
// every managed publication under test into a pack, so it is cleared too.
// It also points DOCKER_CONFIG at a directory holding an empty config.json,
// so the default keychain never runs the developer's credential helper.
func TestMain(m *testing.M) {
	for _, key := range []string{privateOCIRegistryURLEnv, "DOCKER_REGISTRY", extproto.PublicationOutboxEnv} {
		if err := os.Unsetenv(key); err != nil {
			panic(err)
		}
	}
	// The default keychain loads the docker config and runs the credential
	// helper it names. A developer's helper can block on its desktop app,
	// which hangs these tests until the job timeout. An empty config.json in
	// DOCKER_CONFIG names no helper, and its presence keeps the keychain from
	// falling back to Podman's auth files.
	config, err := scratch.New("putnami-docker-config-")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(config.Path(), "config.json"), []byte("{}\n"), 0o600); err != nil {
		panic(err)
	}
	if err := os.Setenv("DOCKER_CONFIG", config.Path()); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = config.Remove()
	os.Exit(code)
}

func TestSessionVersion(t *testing.T) {
	withVer := &pctx.Context{Version: &pctx.Version{Base: "1.0.0", Full: "1.0.0-abc1234"}}
	noVer := &pctx.Context{}
	tests := []struct {
		name     string
		ctx      *pctx.Context
		manifest string
		want     string
	}{
		{"the session version is the line's version", withVer, "9.9.9", "1.0.0-abc1234"},
		{"no version falls back to manifest", noVer, "9.9.9", "9.9.9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sessionVersion(tt.ctx, tt.manifest); got != tt.want {
				t.Errorf("sessionVersion() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDockerContentTag(t *testing.T) {
	tests := []struct {
		name     string
		manifest *pkgmeta.DockerManifest
		localRef string
		want     string
	}{
		{"content hash", &pkgmeta.DockerManifest{ContentHash: "deadbeef"}, "app:local", "c-deadbeef"},
		{"local ref tag", &pkgmeta.DockerManifest{}, "registry.io/app:v1.2.3", "v1.2.3"},
		{"fallback latest", &pkgmeta.DockerManifest{}, "app", "latest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dockerContentTag(tt.manifest, tt.localRef); got != tt.want {
				t.Errorf("dockerContentTag() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsImmutableDigest_RejectsMutableReferences(t *testing.T) {
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, test := range []struct {
		value string
		want  bool
	}{
		{value: digest, want: true},
		{value: "latest", want: false},
		{value: "team/app:1.2.3", want: false},
		{value: "sha256:ABCDEFaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", want: false},
		{value: "sha512:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", want: false},
		{value: "sha256:short", want: false},
	} {
		if got := isImmutableDigest(test.value); got != test.want {
			t.Errorf("isImmutableDigest(%q) = %t, want %t", test.value, got, test.want)
		}
	}
}

func TestPublish_DryRunAcceptsCacheRestoredCandidateWithoutSharedMetadata(t *testing.T) {
	t.Setenv("DOCKER_REGISTRY", "") // keep the registry empty so no credential shell-out

	dir := t.TempDir()
	projectPath := "proj"
	pkgDir := filepath.Join(dir, ".putnami", "out", projectPath, "package")
	if err := os.MkdirAll(filepath.Join(pkgDir, "docker"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "docker", "manifest.json"),
		[]byte(`{"image":"team/app","tags":["team/app:local"],"version":"1.2.3","contentHash":"abc123","digest":"","layout":""}`), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := &pctx.Context{
		WorkspaceRoot: dir,
		Project:       pctx.Project{Name: "team/app", Path: projectPath},
	}
	status, data, err := Publish(ctx, jsonl.New(), []string{"--dry-run"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Fatalf("status = %q, want OK", status)
	}
	if data["dryRun"] != true {
		t.Errorf("expected dryRun=true, got %+v", data)
	}
	if data["contentTag"] != "c-abc123" {
		t.Errorf("contentTag = %v, want c-abc123", data["contentTag"])
	}
}

func TestPublish_SkipsNoProject(t *testing.T) {
	status, _, err := Publish(&pctx.Context{}, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "SKIP" {
		t.Errorf("status = %q, want SKIP", status)
	}
}

func TestPublish_ImageProjectVerifiesImmutableContentWithoutSessionTags(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "local-image-publication-boundary", "publish-owns-only-verified-remote-evidence")
	server := httptest.NewServer(registry.New())
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	contentHash := strings.Repeat("c", 64)
	contentTag := "c-" + contentHash
	contentRef := host + "/team/base:" + contentTag
	tag, err := name.NewTag(contentRef, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	img, err := random.Image(512, 1)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	immutable := tag.Context().Digest(digest.String()).String()

	root := t.TempDir()
	projectPath := "images/base"
	packageDir := filepath.Join(root, ".putnami", "out", projectPath, "package")
	if err := os.MkdirAll(filepath.Join(packageDir, "docker"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := oci.WriteLayout(filepath.Join(packageDir, "docker", "oci"), img); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(packageDir, "docker", "manifest.json"), pkgmeta.DockerManifest{
		Image: "team/base", Tags: []string{"team/base:" + contentTag}, Version: contentTag,
		ContentHash: contentHash, Digest: digest.String(), Layout: "oci", Platform: "linux/amd64",
	})
	packageManifestPath := filepath.Join(packageDir, "docker", "manifest.json")
	packageBytesBefore, err := os.ReadFile(packageManifestPath)
	if err != nil {
		t.Fatal(err)
	}

	oldResolveToken := registrycred.ResolveToken
	registrycred.ResolveToken = func(string) (string, string) { return "", "" }
	defer func() { registrycred.ResolveToken = oldResolveToken }()
	ctx := &pctx.Context{
		WorkspaceRoot: root,
		Project:       pctx.Project{Name: "team/base", Path: projectPath, Type: "image"},
		Version:       &pctx.Version{Base: "9.9.9", Full: "9.9.9-session"},
		OutputPath:    filepath.Join(root, ".putnami", "out", projectPath, "publish"),
	}
	argv := []string{"--docker-registry=" + host}
	status, data, err := Publish(ctx, jsonl.New(), argv)
	if err != nil || status != "OK" {
		t.Fatalf("Publish() = (%q, %+v, %v), want OK", status, data, err)
	}
	if data["image"] != immutable || data["version"] != contentTag {
		t.Fatalf("Publish() identity = %+v, want immutable content identity", data)
	}
	status, reused, err := Publish(ctx, jsonl.New(), argv)
	if err != nil || status != "OK" {
		t.Fatalf("second Publish() = (%q, %+v, %v), want verified registry hit", status, reused, err)
	}
	if reused["contentStatus"] != "reused" || reused["cacheOutcome"] != "hit" || reused["digestReused"] != true {
		t.Fatalf("second Publish() = %+v, want truthful immutable reuse facts", reused)
	}
	packageBytesAfter, err := os.ReadFile(packageManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(packageBytesAfter) != string(packageBytesBefore) {
		t.Fatal("publish rewrote package-owned candidate bytes")
	}
	published, err := pkgmeta.ReadPublishedImageManifest(ctx.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if published.ImmutableRef != immutable || published.Digest != digest.String() ||
		published.CandidateDigest != digest.String() || !published.Verified {
		t.Fatalf("published image record = %+v", published)
	}
	refs, ok := data["refs"].([]string)
	if !ok || len(refs) != 1 || refs[0] != immutable {
		t.Fatalf("Publish() refs = %#v, want immutable ref only", data["refs"])
	}
	for _, forbidden := range []string{"9.9.9", "9.9.9-session", "latest"} {
		candidate, err := name.NewTag(host+"/team/base:"+forbidden, name.Insecure)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := remote.Head(candidate); err == nil {
			t.Fatalf("image project unexpectedly published mutable tag %q", forbidden)
		}
	}

	versionData, err := os.ReadFile(filepath.Join(root, projectPath, ".gen", "version.json"))
	if err != nil {
		t.Fatal(err)
	}
	var version map[string]any
	if err := json.Unmarshal(versionData, &version); err != nil {
		t.Fatal(err)
	}
	if version["image"] != immutable || version["image_digest"] != digest.String() || version["publish"] != nil {
		t.Fatalf("version.json = %+v, want immutable image fields and no promotion context", version)
	}
}

func TestPublishImageProjectUsesGenericDockerRegistryEnvironment(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	hash := strings.Repeat("b", 64)
	root := t.TempDir()
	projectPath := "delivery/images/ci-runner"
	dockerDir := pkgmeta.PackageOutputDir(root, projectPath, "docker")
	if err := os.MkdirAll(dockerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(dockerDir, "manifest.json"), pkgmeta.DockerManifest{
		Image: "delivery-images-ci-runner", Tags: []string{"delivery-images-ci-runner:c-" + hash},
		Version: "c-" + hash, ContentHash: hash, Digest: digest, Layout: "oci", Platform: "linux/amd64",
	})
	t.Setenv("DOCKER_REGISTRY", "registry.example.com/team")
	ctx := &pctx.Context{WorkspaceRoot: root, Project: pctx.Project{
		Name: "delivery/images/ci-runner", Path: projectPath, Type: "image",
	}}
	status, data, err := Publish(ctx, jsonl.New(), []string{"--dry-run"})
	if err != nil || status != "OK" {
		t.Fatalf("Publish() = (%q, %+v, %v), want generic dry run", status, data, err)
	}
	want := "registry.example.com/team/delivery-images-ci-runner@" + digest
	if data["image"] != want {
		t.Fatalf("image = %v, want generic DOCKER_REGISTRY target %q", data["image"], want)
	}
}

func TestPublishImageProjectPreservesNestedGenericRegistryPrecedence(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	hash := strings.Repeat("b", 64)
	root := t.TempDir()
	projectPath := "delivery/images/ci-runner"
	dockerDir := pkgmeta.PackageOutputDir(root, projectPath, "docker")
	if err := os.MkdirAll(dockerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(dockerDir, "manifest.json"), pkgmeta.DockerManifest{
		Image: "delivery-images-ci-runner", Tags: []string{"delivery-images-ci-runner:c-" + hash},
		Version: "c-" + hash, ContentHash: hash, Digest: digest, Layout: "oci", Platform: "linux/amd64",
	})
	t.Setenv("DOCKER_REGISTRY", "registry.example.com/environment")
	ctx := &pctx.Context{
		WorkspaceRoot: root,
		Project:       pctx.Project{Name: "delivery/images/ci-runner", Path: projectPath, Type: "image"},
		Params: pctx.Params{
			"image": json.RawMessage(`{"registry":"registry.example.com/legacy"}`),
		},
	}
	status, data, err := Publish(ctx, jsonl.New(), []string{"--dry-run"})
	if err != nil || status != "OK" {
		t.Fatalf("Publish() = (%q, %+v, %v), want nested generic dry run", status, data, err)
	}
	wantLegacy := "registry.example.com/legacy/delivery-images-ci-runner@" + digest
	if data["image"] != wantLegacy {
		t.Fatalf("image = %v, want nested compatibility target %q", data["image"], wantLegacy)
	}

	status, data, err = Publish(ctx, jsonl.New(), []string{"--dry-run", "--docker-registry=registry.example.com/explicit"})
	if err != nil || status != "OK" {
		t.Fatalf("explicit Publish() = (%q, %+v, %v), want OK", status, data, err)
	}
	wantExplicit := "registry.example.com/explicit/delivery-images-ci-runner@" + digest
	if data["image"] != wantExplicit {
		t.Fatalf("image = %v, want explicit target %q", data["image"], wantExplicit)
	}
}

// A dry-run package may write no manifest: the dry-run publish that follows it
// succeeds and pushes nothing. Without --dry-run the missing manifest still
// fails.
func TestPublish_DryRunSucceedsWhenPackageAssembledNoImage(t *testing.T) {
	t.Setenv("DOCKER_REGISTRY", "")
	ctx := &pctx.Context{
		WorkspaceRoot: t.TempDir(),
		Project:       pctx.Project{Name: "team/app", Path: "proj"},
	}

	status, data, err := Publish(ctx, jsonl.New(), []string{"--dry-run"})
	if err != nil || status != "OK" {
		t.Fatalf("dry run without a manifest: status = %q, err = %v, want OK", status, err)
	}
	if data["dryRun"] != true || data["refs"] != nil {
		t.Fatalf("dry run without a manifest listed refs: %+v", data)
	}

	status, _, err = Publish(ctx, jsonl.New(), nil)
	if err == nil || status != "FAILED" {
		t.Fatalf("publish without a manifest: status = %q, err = %v, want FAILED", status, err)
	}
}

func TestPublishClearsStaleOwnedEvidenceBeforeDryRunAndFailure(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name     string
		manifest string
	}{
		{name: "dry run", manifest: `{"image":"team/app","tags":["team/app:local"],"version":"1.0.0"}`},
		{name: "invalid candidate", manifest: `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			projectPath := "apps/example"
			dockerDir := pkgmeta.PackageOutputDir(root, projectPath, "docker")
			if err := os.MkdirAll(dockerDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dockerDir, "manifest.json"), []byte(tc.manifest), 0o644); err != nil {
				t.Fatal(err)
			}
			outputDir := filepath.Join(root, ".putnami", "out", projectPath, "publish")
			if err := pkgmeta.WritePublishedImageManifest(outputDir, pkgmeta.PublishedImageManifest{
				Image: "registry.example.com/team/app", ImmutableRef: "registry.example.com/team/app@" + digest,
				Digest: digest, CandidateDigest: digest, Verified: true,
			}); err != nil {
				t.Fatal(err)
			}
			ctx := &pctx.Context{WorkspaceRoot: root, OutputPath: outputDir, Project: pctx.Project{Name: "team/app", Path: projectPath}}
			_, _, _ = Publish(ctx, jsonl.New(), []string{"--dry-run"})
			if _, err := os.Stat(pkgmeta.PublishedImageManifestPath(outputDir)); !os.IsNotExist(err) {
				t.Fatalf("stale publish evidence survived %s: %v", tc.name, err)
			}
		})
	}
}

func TestResolveImagePublishTargetManagedAndGeneric(t *testing.T) {
	ctx := &pctx.Context{
		Workspace: pctx.Workspace{Name: "team"},
		Project:   pctx.Project{Name: "delivery/images/ci-runner"},
	}
	managed, err := resolveImagePublishTarget(ctx, "", "delivery-images-ci-runner")
	if err != nil {
		t.Fatal(err)
	}
	if managed.Repository != "oci.putnami.dev/team/delivery-images-ci-runner" ||
		managed.Host != managedOCIRegistry || !managed.Managed {
		t.Fatalf("managed target = %+v", managed)
	}
	if _, err := resolveImagePublishTarget(ctx, "oci.putnami.dev/wrong", "delivery-images-ci-runner"); err == nil {
		t.Fatal("mismatched managed namespace override was accepted")
	}
	generic, err := resolveImagePublishTarget(ctx, "registry.example.com/group", "delivery-images-ci-runner")
	if err != nil {
		t.Fatal(err)
	}
	if generic.Repository != "registry.example.com/group/delivery-images-ci-runner" || generic.Host != "registry.example.com" || generic.Managed {
		t.Fatalf("generic target = %+v", generic)
	}
}

func TestPublishImageProjectAsksTheHostSeamAndFailsClosed(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	hash := strings.Repeat("b", 64)
	manifest := &pkgmeta.DockerManifest{
		Image: "delivery-images-ci-runner", Tags: []string{"delivery-images-ci-runner:c-" + hash},
		Version: "c-" + hash, ContentHash: hash, Digest: digest, Layout: "oci", Platform: "linux/amd64",
	}
	ctx := &pctx.Context{
		WorkspaceRoot: t.TempDir(),
		Workspace:     pctx.Workspace{Name: "team"},
		Project:       pctx.Project{Name: "delivery/images/ci-runner", Path: "delivery/images/ci-runner", Type: "image"},
	}
	old := registrycred.ResolveToken
	var got string
	registrycred.ResolveToken = func(host string) (string, string) {
		got = host
		return "", "no credential for this registry"
	}
	defer func() { registrycred.ResolveToken = old }()

	status, _, err := publishImmutableImageProject(ctx, jsonl.New(), false, "", manifest)
	if status != "FAILED" || err == nil || !strings.Contains(err.Error(), "no credential for this registry") {
		t.Fatalf("publish = (%q, %v), want fail-closed credential error", status, err)
	}
	// The host is the whole request: no package, no action, no workspace.
	if got != managedOCIRegistry {
		t.Fatalf("seam host = %q, want %q", got, managedOCIRegistry)
	}
}

func TestResolveManagedDockerCredentialAsksByHost(t *testing.T) {
	oldHost := registrycred.ResolveToken
	defer func() { registrycred.ResolveToken = oldHost }()

	var hosts []string
	registrycred.ResolveToken = func(host string) (string, string) {
		hosts = append(hosts, host)
		return "host-bearer", ""
	}

	// Managed and unmanaged registries make the SAME call. The cloud decides
	// what a credential for a host is worth; the framework only names the host.
	if tok, _ := resolveManagedDockerCredential(managedOCIRegistry); tok != "host-bearer" {
		t.Fatalf("managed host = %q, want host-bearer", tok)
	}
	if tok, _ := resolveManagedDockerCredential("ghcr.io"); tok != "host-bearer" {
		t.Fatalf("unmanaged host = %q, want host-bearer", tok)
	}
	want := []string{managedOCIRegistry, "ghcr.io"}
	if len(hosts) != 2 || hosts[0] != want[0] || hosts[1] != want[1] {
		t.Fatalf("hosts = %v, want %v", hosts, want)
	}
}

func writeJSONFile(t *testing.T, filename string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// dockerPublishFixture stages a packaged, content-addressed image for a normal
// (non-image) project against a live local registry and returns the argv the
// publisher is called with.
type dockerPublishFixture struct {
	ctx         *pctx.Context
	host        string
	repository  string
	contentTag  string
	digest      string
	version     string
	registryURL string
	// registry records every request the local registry receives.
	registry *recordingRegistry
}

func newDockerPublishFixture(t *testing.T) dockerPublishFixture {
	t.Helper()
	recording := newRecordingRegistry()
	server := httptest.NewServer(recording)
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")

	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	contentHash := strings.Repeat("d", 64)
	contentTag := "c-" + contentHash

	root := t.TempDir()
	projectPath := "apps/api"
	packageDir := pkgmeta.PackageOutputDir(root, projectPath, "docker")
	if err := os.MkdirAll(packageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := oci.WriteLayout(filepath.Join(packageDir, "oci"), img); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(packageDir, "manifest.json"), pkgmeta.DockerManifest{
		Image: "team/app", Tags: []string{"team/app:" + contentTag}, Version: "0.0.0-stale",
		ContentHash: contentHash, Digest: digest.String(), Layout: "oci", Platform: "linux/amd64",
	})

	oldResolveToken := registrycred.ResolveToken
	registrycred.ResolveToken = func(string) (string, string) { return "", "" }
	t.Cleanup(func() { registrycred.ResolveToken = oldResolveToken })

	return dockerPublishFixture{
		ctx: &pctx.Context{
			WorkspaceRoot: root,
			Project:       pctx.Project{Name: "team/app", Path: projectPath},
			Version:       &pctx.Version{Base: "3.1.0", Full: "3.1.0-abc1234"},
			OutputPath:    filepath.Join(root, ".putnami", "out", projectPath, "publish"),
			Params: pctx.Params{
				"registries": json.RawMessage(`{"oci":{"publish":"` + host + `"}}`),
			},
		},
		host:        host,
		repository:  "team/app",
		contentTag:  contentTag,
		digest:      digest.String(),
		version:     "3.1.0-abc1234",
		registryURL: host,
		registry:    recording,
	}
}

// registryTags lists the tags the local registry holds for the repository.
func registryTags(t *testing.T, host, repository string) []string {
	t.Helper()
	repo, err := name.NewRepository(host+"/"+repository, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	tags, err := remote.List(repo)
	if err != nil {
		t.Fatalf("listing tags: %v", err)
	}
	return tags
}

// captureEvents runs fn with os.Stdout redirected to a temp file and returns the
// decoded JSONL events. A file rather than a pipe: a publish emits more than a
// pipe buffer holds, and a blocked writer would deadlock the test.
func captureEvents(t *testing.T, fn func()) []map[string]any {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = file
	fn()
	os.Stdout = original
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decoding %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

// eventsOfKind returns the artifact events carrying the given kind.
func eventsOfKind(events []map[string]any, kind string) []map[string]any {
	var found []map[string]any
	for _, event := range events {
		if event["type"] != "artifact" {
			continue
		}
		if event["kind"] == kind {
			found = append(found, event)
		}
	}
	return found
}

// A channel is a release-set decision applied once, after every member has a
// verified digest. A publisher that also wrote channel tags would move them
// member by member, so this pins that the version and the content tag are the
// only references a publication creates.
func TestDockerPublishTagsVersionOnly(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "oci-publish-by-digest", "version-is-the-only-tag")
	fixture := newDockerPublishFixture(t)

	status, data, err := Publish(fixture.ctx, jsonl.New(), nil)
	if err != nil || status != "OK" {
		t.Fatalf("Publish() = (%q, %+v, %v), want OK", status, data, err)
	}
	if data["version"] != fixture.version {
		t.Fatalf("published version = %v, want the session version %q", data["version"], fixture.version)
	}
	if _, present := data["channels"]; present {
		t.Fatalf("publish result still reports channels: %+v", data)
	}

	got := registryTags(t, fixture.host, fixture.repository)
	want := map[string]bool{fixture.contentTag: true, fixture.version: true}
	for _, tag := range got {
		if !want[tag] {
			t.Fatalf("registry holds unexpected tag %q; tags = %v", tag, got)
		}
		delete(want, tag)
	}
	if len(want) != 0 {
		t.Fatalf("registry is missing tags %v; tags = %v", want, got)
	}

	// The version is the only thing the publish context records: a hook reading
	// .gen/version.json can no longer believe a channel points at this image.
	versionData, err := os.ReadFile(filepath.Join(fixture.ctx.WorkspaceRoot, fixture.ctx.Project.Path, ".gen", "version.json"))
	if err != nil {
		t.Fatal(err)
	}
	var recorded struct {
		Publish map[string]any `json:"publish"`
	}
	if err := json.Unmarshal(versionData, &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded.Publish["version"] != fixture.version {
		t.Fatalf("publish context = %+v, want the session version", recorded.Publish)
	}
	if _, present := recorded.Publish["channels"]; present {
		t.Fatalf("publish context still records channels: %+v", recorded.Publish)
	}
}

// The release set is assembled from what exists in the registry under a verified
// digest, so the member event is emitted on the reused path exactly as on the
// pushed one.
func TestDockerPublishEmitsPublishedMember(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "oci-publish-by-digest", "member-event-carries-the-digest")
	fixture := newDockerPublishFixture(t)

	for _, path := range []string{"pushed", "reused"} {
		t.Run(path, func(t *testing.T) {
			var status string
			var err error
			events := captureEvents(t, func() {
				status, _, err = Publish(fixture.ctx, jsonl.New(), nil)
			})
			if err != nil || status != "OK" {
				t.Fatalf("Publish() = (%q, %v), want OK", status, err)
			}
			members := eventsOfKind(events, extproto.PublishedMemberEventKind)
			if len(members) != 1 {
				t.Fatalf("published-member events = %d, want exactly one", len(members))
			}
			// The publisher writes the member's fields at the top level of the
			// artifact event, so read them back the way a consumer would.
			event := members[0]
			member := extproto.PublishedMember{
				Ecosystem:      eventString(t, event, "ecosystem"),
				Coordinate:     eventString(t, event, "coordinate"),
				Version:        eventString(t, event, "version"),
				ArtifactDigest: eventString(t, event, "artifactDigest"),
			}
			if diagnostics := extproto.ValidatePublishedMember(&member); len(diagnostics) != 0 {
				t.Fatalf("member event %+v is not a valid published member: %v", event, diagnostics)
			}
			if member.Ecosystem != "oci" || member.Coordinate != fixture.repository ||
				member.Version != fixture.version || member.ArtifactDigest != fixture.digest {
				t.Fatalf("member = %+v, want the oci coordinate without the host and the verified digest", member)
			}
			// The human/renderer event is untouched by the member event.
			if len(eventsOfKind(events, "published")) != 1 {
				t.Fatalf("published events = %d, want exactly one", len(eventsOfKind(events, "published")))
			}
		})
	}
}

// The workspace registries entry is the single declared source for an endpoint.
// The argument still wins, and DOCKER_REGISTRY remains the floor for a workspace
// that declares nothing.
func TestDockerRegistryFromRegistriesEntry(t *testing.T) {
	declared := pctx.Params{"registries": json.RawMessage(`{"oci":{"publish":"registry.example.com/declared"},"npm":{"publish":"https://npm.example.test"}}`)}
	t.Setenv("DOCKER_REGISTRY", "registry.example.com/environment")

	for _, tc := range []struct {
		name   string
		argv   []string
		params pctx.Params
		want   string
	}{
		{name: "registries entry", params: declared, want: "registry.example.com/declared"},
		{name: "argument wins", argv: []string{"--docker-registry=registry.example.com/explicit"}, params: declared, want: "registry.example.com/explicit"},
		{name: "environment floor", params: pctx.Params{}, want: "registry.example.com/environment"},
		{name: "no oci entry falls through", params: pctx.Params{"registries": json.RawMessage(`{"npm":{"publish":"https://npm.example.test"}}`)}, want: "registry.example.com/environment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveDockerRegistry(cli.ParseFlags(tc.argv), &pctx.Context{Params: tc.params})
			if err != nil {
				t.Fatalf("resolveDockerRegistry() error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("resolveDockerRegistry() = %q, want %q", got, tc.want)
			}
		})
	}

	// A publish option is no longer a source: only the declared entry is.
	got, err := resolveDockerRegistry(nil, &pctx.Context{Params: pctx.Params{
		"dockerRegistry": json.RawMessage(`"registry.example.com/legacy"`),
	}})
	if err != nil || got != "registry.example.com/environment" {
		t.Fatalf("resolveDockerRegistry(publish option) = (%q, %v), want the environment floor", got, err)
	}
}

// eventString reads a string field of a runtime event.
func eventString(t *testing.T, event map[string]any, key string) string {
	t.Helper()
	value, _ := event[key].(string)
	return value
}

// Native authorization and channel projection use the repository's namespace,
// including when it is configured as part of the publication endpoint.
func TestPublishedMemberCoordinateRetainsRepositoryNamespace(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, qualifiedImage, registry, want string
	}{
		{name: "host only", qualifiedImage: "oci.example.com/putnami.dev", registry: "oci.example.com", want: "putnami.dev"},
		{name: "host and namespace", qualifiedImage: "oci.example.com/putnami/putnami.dev", registry: "oci.example.com/putnami", want: "putnami/putnami.dev"},
		{name: "nested namespace", qualifiedImage: "oci.example.com/a/b/app", registry: "oci.example.com/a/b", want: "a/b/app"},
		{name: "registry does not prefix the image, host still does", qualifiedImage: "oci.example.com/team/app", registry: "oci.example.com/putnami", want: "team/app"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := captureEvents(t, func() {
				emitPublishedMember(jsonl.New(), tc.qualifiedImage, tc.registry, "1.0.0", digest)
			})
			members := eventsOfKind(events, extproto.PublishedMemberEventKind)
			if len(members) != 1 {
				t.Fatalf("published-member events = %d, want exactly one", len(members))
			}
			if got := eventString(t, members[0], "coordinate"); got != tc.want {
				t.Fatalf("coordinate = %q, want %q", got, tc.want)
			}
		})
	}
}

// The emitted member names exactly the native repository that was uploaded.
func TestDockerPublishMemberCoordinateIncludesTheRegistryNamespace(t *testing.T) {
	fixture := newDockerPublishFixture(t)
	fixture.ctx.Params["registries"] = json.RawMessage(`{"oci":{"publish":"` + fixture.host + `/team"}}`)

	var status string
	var err error
	events := captureEvents(t, func() {
		status, _, err = Publish(fixture.ctx, jsonl.New(), nil)
	})
	if err != nil || status != "OK" {
		t.Fatalf("Publish() = (%q, %v), want OK", status, err)
	}
	members := eventsOfKind(events, extproto.PublishedMemberEventKind)
	if len(members) != 1 {
		t.Fatalf("published-member events = %d, want exactly one", len(members))
	}
	if got := eventString(t, members[0], "coordinate"); got != "team/"+fixture.repository {
		t.Fatalf("coordinate = %q, want %q", got, "team/"+fixture.repository)
	}
}
