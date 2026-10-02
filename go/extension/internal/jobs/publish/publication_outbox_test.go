package publish

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	gomod "go.putnami.dev/protocol/gomod"
	job "go.putnami.dev/protocol/job"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/publicationoutbox"
	"go.putnami.dev/sdk/extension/registrycred"
)

// goOutboxModule is a packaged Go module: its artifacts sit in the package
// output directory, as the go packager writes them.
type goOutboxModule struct {
	ctx      *pctx.Context
	zip      []byte
	mod      []byte
	info     []byte
	digest   string
	requests *atomic.Int64
}

// newGoOutboxModule stages a packaged release-set member whose declared origin
// counts and refuses every request, under a malformed private broker that a
// credentialed publication would refuse.
func newGoOutboxModule(t *testing.T) goOutboxModule {
	t.Helper()
	t.Setenv("PUTNAMI_REGISTRY_TOKEN", "")
	workspace := t.TempDir()
	projectRoot := filepath.Join(workspace, "mod")
	module := goOutboxModule{
		zip:  []byte("exact immutable go module zip bytes"),
		mod:  []byte("module go.putnami.dev/mod\n\ngo 1.25\n"),
		info: []byte(`{"Version":"v1.2.3-canary.1","Time":"2026-01-02T03:04:05Z"}` + "\n"),
	}
	module.digest = fmt.Sprintf("sha256:%x", sha256.Sum256(module.zip))
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "go.mod"), module.mod, 0o644); err != nil {
		t.Fatal(err)
	}
	goDir := pkgmeta.PackageOutputDir(workspace, "mod", "go")
	zipPath := filepath.Join(goDir, "v1.2.3-canary.1.zip")
	modPath := filepath.Join(goDir, "v1.2.3-canary.1.mod")
	infoPath := filepath.Join(goDir, "v1.2.3-canary.1.info")
	writeGoPublishMetadata(t, workspace, "v1.2.3-canary.1", zipPath, modPath)
	for path, data := range map[string][]byte{zipPath: module.zip, modPath: module.mod, infoPath: module.info} {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	metadata, err := json.Marshal(pkgmeta.GoModuleMetadata{
		ModulePath: "go.putnami.dev/mod", Version: "v1.2.3-canary.1",
		ZipPath: zipPath, ModPath: modPath, InfoPath: infoPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(goDir, "module.json"), metadata, 0o644); err != nil {
		t.Fatal(err)
	}

	var requests atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "no request is expected", http.StatusInternalServerError)
	}))
	t.Cleanup(origin.Close)
	declareGoOrigin(t, workspace, origin.URL)
	module.requests = &requests
	t.Setenv(privateGoRegistryURLEnv, "http://localhost:8080/go")

	module.ctx = &pctx.Context{
		WorkspaceRoot: workspace,
		Project:       pctx.Project{Name: "go.putnami.dev/mod", Path: "mod", FullPath: projectRoot},
		Params:        managedGoSparseParams(t),
	}
	return module
}

// refuseRegistryAccess fails the test when the credential seam or the go
// toolchain download is used.
func refuseRegistryAccess(t *testing.T) {
	t.Helper()
	originalSeam, originalDownload := registrycred.ResolveToken, goModDownload
	t.Cleanup(func() { registrycred.ResolveToken, goModDownload = originalSeam, originalDownload })
	registrycred.ResolveToken = func(host string) (string, string) {
		t.Errorf("credential requested for %q during an outbox publication", host)
		return "", ""
	}
	goModDownload = func(_ context.Context, proxy, _, module, version string) (string, error) {
		t.Errorf("go mod download %s@%s via %s ran during an outbox publication", module, version, proxy)
		return "", errors.New("no download is expected")
	}
}

func assertNoGoPublicationEvents(t *testing.T, output string) {
	t.Helper()
	for _, event := range parsePublishEvents(t, output) {
		if event["kind"] == "published" || event["kind"] == extproto.PublishedMemberEventKind {
			t.Fatalf("outbox publication emitted %v: %#v", event["kind"], event)
		}
	}
}

// A Go module whose credential the cloud supplies is packed under the outbox:
// the descriptor names its zip, go.mod and .info, and the job asks for no
// credential, sends no request and runs no `go mod download`.
func TestGoModulePublishWritesTheOutboxAndUploadsNothing(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "managed-publication-packs-only", "the-managed-route-packs-into-the-outbox-and-uploads-nothing")
	module := newGoOutboxModule(t)
	outboxRoot := filepath.Join(t.TempDir(), "outbox")
	t.Setenv(extproto.PublicationOutboxEnv, outboxRoot)
	refuseRegistryAccess(t)

	var status string
	var data map[string]any
	var err error
	output := capturePublishOutput(t, func() {
		status, data, err = goModule(module.ctx, jsonl.New(), nil)
	})
	if err != nil || status != "OK" || data["packed"] != true || data["artifactDigest"] != module.digest {
		t.Fatalf("goModule() = (%q, %+v, %v), want a packed OK at the zip digest", status, data, err)
	}
	if got := module.requests.Load(); got != 0 {
		t.Fatalf("origin requests = %d, want none", got)
	}
	assertNoGoPublicationEvents(t, output)

	outbox, err := publicationoutbox.Read(outboxRoot)
	if err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	if len(outbox.Descriptor.Members) != 1 {
		t.Fatalf("outbox members = %+v, want one", outbox.Descriptor.Members)
	}
	member := outbox.Descriptor.Members[0]
	if member.Ecosystem != extproto.OutboxEcosystemGo || member.Coordinate != "go.putnami.dev/mod" ||
		member.Version != "v1.2.3-canary.1" || member.Project != "/mod" || member.Go == nil || member.NPM != nil || member.OCI != nil {
		t.Fatalf("outbox member = %+v, want the planned Go member", member)
	}
	for _, file := range []struct {
		name string
		file extproto.OutboxFile
		want []byte
	}{
		{"zip", member.Go.Zip, module.zip},
		{"go.mod", member.Go.Mod, module.mod},
		{".info", member.Go.Info, module.info},
	} {
		got, err := outbox.ReadFile(file.file)
		if err != nil || string(got) != string(file.want) {
			t.Fatalf("outbox %s = (%q, %v), want the packaged bytes %q", file.name, got, err, file.want)
		}
	}
	if member.Go.Zip.Digest != module.digest {
		t.Fatalf("outbox zip digest = %s, want %s", member.Go.Zip.Digest, module.digest)
	}
}

// goRegistry is a gomod-write registry that accepts one module version and
// serves its bytes back.
func goRegistry(t *testing.T, zip, mod []byte, digest string) (string, *atomic.Int64) {
	t.Helper()
	var uploads atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == gomod.BlobUploadPath("go.putnami.dev/mod"):
			assertBearer(t, r)
			uploads.Add(1)
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"digest":%q}`, digest)
		case r.Method == http.MethodPut && r.URL.Path == gomod.VersionPath("go.putnami.dev/mod", "v1.2.3-canary.1"):
			assertBearer(t, r)
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, ".zip"):
			assertBearer(t, r)
			_, _ = w.Write(zip)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, ".mod"):
			assertBearer(t, r)
			_, _ = w.Write(mod)
		default:
			t.Errorf("unexpected registry request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL, &uploads
}

// An explicit token names the user's own credential: the job uploads with it
// as before and writes no outbox.
func TestGoModulePublishWithAnExplicitTokenIgnoresTheOutbox(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "managed-publication-packs-only", "a-native-route-ignores-the-outbox")
	ctx, zip, mod, digest := managedGoPublishTestContext(t)
	origin, uploads := goRegistry(t, zip, mod, digest)
	declareGoOrigin(t, ctx.WorkspaceRoot, origin)
	stubGoModuleDownload(t)
	outboxRoot := filepath.Join(t.TempDir(), "outbox")
	t.Setenv(extproto.PublicationOutboxEnv, outboxRoot)

	var status string
	var err error
	output := capturePublishOutput(t, func() {
		status, _, err = goModule(ctx, jsonl.New(), nil)
	})
	if err != nil || status != "OK" {
		t.Fatalf("goModule() = (%q, %v), want OK", status, err)
	}
	if got := uploads.Load(); got != 1 {
		t.Fatalf("uploads = %d, want the job's own upload", got)
	}
	findPublishedGoEvent(t, output)
	if _, err := os.Stat(outboxRoot); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("outbox stat = %v, want no outbox with an explicit token", err)
	}
}

// treeState maps every path under root to its kind, mode and content digest.
func treeState(t *testing.T, root string) map[string]string {
	t.Helper()
	state := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			state[rel] = "dir " + info.Mode().String()
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		state[rel] = fmt.Sprintf("%s %x", info.Mode(), sha256.Sum256(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func assertTreeUnchanged(t *testing.T, root string, before map[string]string) {
	t.Helper()
	after := treeState(t, root)
	for path, state := range after {
		if before[path] != state {
			t.Errorf("%s changed under %s: %q, then %q", path, root, before[path], state)
		}
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			t.Errorf("%s was removed under %s", path, root)
		}
	}
}

// isolateTempDir points the process temporary directory at an empty directory
// and returns it. Call it after the test's first t.TempDir, which then keeps
// its own parent.
func isolateTempDir(t *testing.T) string {
	t.Helper()
	temp := filepath.Join(t.TempDir(), "tmp")
	if err := os.Mkdir(temp, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", temp)
	return temp
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the job left %d entries in the temporary directory", len(entries))
	}
}

// writeFakeImageLayout stages a packaged workload image: an OCI layout the job
// copies byte for byte and the manifest that names its digest.
func writeFakeImageLayout(t *testing.T, workspaceRoot, projectPath string) string {
	t.Helper()
	dockerDir := pkgmeta.PackageOutputDir(workspaceRoot, projectPath, "docker")
	digest := "sha256:" + strings.Repeat("c", 64)
	files := [][2]string{
		{"oci-layout", `{"imageLayoutVersion":"1.0.0"}`},
		{"index.json", `{"schemaVersion":2,"manifests":[]}`},
		{filepath.Join("blobs", "sha256", strings.Repeat("c", 64)), `{"schemaVersion":2}`},
	}
	for _, file := range files {
		path := filepath.Join(dockerDir, "oci", file[0])
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(file[1]), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := json.Marshal(pkgmeta.DockerManifest{
		Image: "mod", Tags: []string{"mod:c-" + strings.Repeat("d", 64)}, Version: "0.0.0-stale",
		ContentHash: strings.Repeat("d", 64), Digest: digest, Layout: "oci", Platform: "linux/amd64",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dockerDir, "manifest.json"), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	return digest
}

// A packing job writes its artifacts into the outbox and nowhere else: the
// workspace, its build outputs included, is unchanged and no temporary file
// survives the job.
func TestPublishJobWritesNothingOutsideTheOutbox(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "managed-publication-packs-only", "the-job-writes-nothing-outside-the-outbox")

	t.Run("go module", func(t *testing.T) {
		module := newGoOutboxModule(t)
		outboxRoot := filepath.Join(t.TempDir(), "outbox")
		t.Setenv(extproto.PublicationOutboxEnv, outboxRoot)
		refuseRegistryAccess(t)
		temp := isolateTempDir(t)
		before := treeState(t, module.ctx.WorkspaceRoot)

		status, _, err := Run(module.ctx, jsonl.New(), []string{"--go"})
		if err != nil || status != "OK" {
			t.Fatalf("Run(--go) = (%q, %v), want OK", status, err)
		}
		assertTreeUnchanged(t, module.ctx.WorkspaceRoot, before)
		assertEmptyDir(t, temp)
		if _, err := publicationoutbox.Read(outboxRoot); err != nil {
			t.Fatalf("read the outbox: %v", err)
		}
	})

	t.Run("docker", func(t *testing.T) {
		workspace := t.TempDir()
		ctx := &pctx.Context{
			WorkspaceRoot: workspace,
			OutputPath:    filepath.Join(workspace, ".putnami", "out", "mod", "publish"),
			Project:       pctx.Project{Name: "mod", Path: "mod"},
			Identity:      &job.TaskIdentity{Project: job.ProjectIdentity{ID: "/mod"}},
			Version:       &pctx.Version{Base: "1.2.3", Full: "1.2.3-canary.1"},
			Params:        pctx.Params{"registries": json.RawMessage(`{"oci":{"publish":"oci.putnami.dev/test"}}`)},
		}
		digest := writeFakeImageLayout(t, workspace, "mod")
		var brokerRequests atomic.Int64
		broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			brokerRequests.Add(1)
			http.Error(w, "no request is expected", http.StatusInternalServerError)
		}))
		t.Cleanup(broker.Close)
		t.Setenv("PUTNAMI_REGISTRY_OCI_URL", broker.URL+"/oci")
		outboxRoot := filepath.Join(t.TempDir(), "outbox")
		t.Setenv(extproto.PublicationOutboxEnv, outboxRoot)
		refuseRegistryAccess(t)
		temp := isolateTempDir(t)
		before := treeState(t, workspace)

		status, data, err := Run(ctx, jsonl.New(), []string{"--docker"})
		if err != nil || status != "OK" || data["packed"] != true {
			t.Fatalf("Run(--docker) = (%q, %+v, %v), want a packed OK", status, data, err)
		}
		if got := brokerRequests.Load(); got != 0 {
			t.Fatalf("broker requests = %d, want none", got)
		}
		assertTreeUnchanged(t, workspace, before)
		assertEmptyDir(t, temp)
		outbox, err := publicationoutbox.Read(outboxRoot)
		if err != nil {
			t.Fatalf("read the outbox: %v", err)
		}
		if len(outbox.Descriptor.Members) != 1 {
			t.Fatalf("outbox members = %+v, want one", outbox.Descriptor.Members)
		}
		member := outbox.Descriptor.Members[0]
		if member.OCI == nil || member.OCI.Repository != "oci.putnami.dev/test/mod" || member.OCI.Digest != digest ||
			member.Coordinate != "test/mod" || member.Version != "1.2.3-canary.1" || member.Project != "/mod" {
			t.Fatalf("outbox member = %+v, want the managed image at its packaged digest", member)
		}
	})
}

// Without the outbox, a Go module publication uploads, verifies, runs the
// consumer smoke and reports its member exactly as before.
func TestPublishWithoutTheOutboxIsUnchanged(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "managed-publication-packs-only", "without-the-outbox-publication-is-unchanged")
	ctx, zip, mod, digest := managedGoPublishTestContext(t)
	t.Setenv("PUTNAMI_REGISTRY_TOKEN", "")
	ctx.Params = managedGoSparseParams(t)
	origin, uploads := goRegistry(t, zip, mod, digest)
	declareGoOrigin(t, ctx.WorkspaceRoot, origin)
	originalSeam, originalDownload := registrycred.ResolveToken, goModDownload
	t.Cleanup(func() { registrycred.ResolveToken, goModDownload = originalSeam, originalDownload })
	var tokenHosts []string
	registrycred.ResolveToken = func(host string) (string, string) {
		tokenHosts = append(tokenHosts, host)
		return "test-token", ""
	}
	var smokes int
	goModDownload = func(context.Context, string, string, string, string) (string, error) {
		smokes++
		return "", nil
	}

	var status string
	var data map[string]any
	var err error
	output := capturePublishOutput(t, func() {
		status, data, err = goModule(ctx, jsonl.New(), nil)
	})
	if err != nil || status != "OK" || data["digestVerified"] != true {
		t.Fatalf("goModule() = (%q, %+v, %v), want a verified OK", status, data, err)
	}
	if _, packed := data["packed"]; packed {
		t.Fatalf("publication without an outbox reports a pack: %+v", data)
	}
	if uploads.Load() != 1 || len(tokenHosts) != 1 || smokes != 1 {
		t.Fatalf("uploads=%d credential hosts=%v smokes=%d, want one each", uploads.Load(), tokenHosts, smokes)
	}
	member := findPublishedGoMemberEvent(t, output)
	if member.Coordinate != "go.putnami.dev/mod" || member.ArtifactDigest != digest {
		t.Fatalf("published member = %+v, want the module at its verified digest", member)
	}
}
