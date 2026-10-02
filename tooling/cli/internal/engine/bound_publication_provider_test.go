package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	protocoljob "go.putnami.dev/protocol/job"
	registry "go.putnami.dev/protocol/registry"
	runner "go.putnami.dev/protocol/runner"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/npmpublish"
	"go.putnami.dev/sdk/extension/publicationoutbox"
	"go.putnami.dev/tooling/cli/internal/credentialprovider"
	"go.putnami.dev/tooling/cli/internal/credentialprovider/providertest"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// A bound request publishes through a publication-v1 provider as a local run
// of the same commit does. These tests drive Engine.Run over a git workspace
// whose one project publishes one npm member: a real extension runtime, the
// real planner, the real release-set coordinator and the engine's own upload.
// The doubles are the boundaries: the credential provider (providertest),
// the npm registry (an httptest server), and the legacy release-set provider
// a plan-only run resolves through (serveReleaseSetFixtureProvider). They set
// the environment, so none is parallel.

// boundPublicationPackEnv routes a child of this test binary to the
// publication job of the bound publication fixture: it packs the member into
// the outbox the engine hands it, as a language extension's job does.
const boundPublicationPackEnv = "PUTNAMI_ENGINE_BOUND_PUBLICATION_PACK"

// The member the fixture publishes, the project that owns it, and the publish
// credential the provider issues for the fixture registry.
const (
	boundPublicationCoordinate = "@fixture/app"
	boundPublicationProject    = "/app"
	boundPublicationBearer     = "bound-publication-bearer"
	boundPublicationOpenKey    = "putnami:publish~open"
)

// packBoundPublicationMember packs the fixture member at the version the
// engine stamped for its project, and returns the process exit code.
func packBoundPublicationMember() int {
	if err := packBoundPublication(); err != nil {
		fmt.Fprintln(os.Stderr, "bound publication pack:", err)
		return 1
	}
	return 0
}

func packBoundPublication() error {
	stamp, err := os.ReadFile(filepath.Join(os.Getenv("PUTNAMI_PROJECT_ROOT"), ".gen", "version.json"))
	if err != nil {
		return err
	}
	var version struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(stamp, &version); err != nil {
		return err
	}
	registry, err := declaredNPMRegistry()
	if err != nil {
		return err
	}
	writer, err := publicationoutbox.WriterFromEnv()
	if err != nil {
		return err
	}
	tarball, err := writer.WriteFile("npm/app.tgz", []byte("tarball of "+boundPublicationCoordinate+"@"+version.Version))
	if err != nil {
		return err
	}
	manifest, err := json.Marshal(struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}{boundPublicationCoordinate, version.Version})
	if err != nil {
		return err
	}
	manifestFile, err := writer.WriteFile("npm/app.json", manifest)
	if err != nil {
		return err
	}
	if err := writer.Add(extensionproto.OutboxMember{
		Ecosystem: extensionproto.OutboxEcosystemNPM, Coordinate: boundPublicationCoordinate,
		Version: version.Version, Project: boundPublicationProject,
		NPM: &extensionproto.OutboxNPM{Registry: registry, Tarball: tarball, Manifest: manifestFile},
	}); err != nil {
		return err
	}
	return writer.Commit()
}

// boundPublicationFixture is a git workspace on a commit tagged v1.0.0 whose
// project /app publishes @fixture/app to endpoint. Its build, package and
// publish commands chain, so the publish job waits for the build: a request
// whose barrier is build is valid. It returns the workspace root.
func boundPublicationFixture(t *testing.T, endpoint string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell provider fixture")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quoted := func(value string) string {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	root := t.TempDir()
	write := func(rel, content string, mode os.FileMode) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(wsproto.WorkspaceConfigFilename, fmt.Sprintf(
		`{"name":"bound-publication","includes":["app","provider"],"registries":{"npm":{"publish":%s}}}`, quoted(endpoint)), 0o644)
	write(".gitignore", ".putnami/\n.gen/\n", 0o644)
	write("app/putnami.json", `{"name":"@fixture/app","version":"1.0.0","extensions":["/provider"]}`, 0o644)
	write("app/marker.txt", "app\n", 0o644)
	write("provider/putnami.json", `{"name":"@fixture/provider"}`, 0o644)
	write("provider/putnami.extension.json", fmt.Sprintf(boundPublicationManifest, quoted(self), quoted(boundPublicationPackEnv)), 0o644)
	probe := fmt.Sprintf(`{"version":%d,"extension":"@fixture/provider","projects":[`+
		`{"path":"app","metadata":{"releaseSet":{"ecosystems":[`+
		`{"ecosystem":"npm","coordinate":%q,"packageStep":"artifact","publishStep":"artifact"}]}}}]}`,
		wsproto.ProbeProtocolVersion, boundPublicationCoordinate)
	write("provider/runtime", fmt.Sprintf(`#!/bin/sh
if [ "$1" = "__putnami" ] && [ "$2" = "runtime-info" ]; then
  printf '%%s\n' '{"extension":"@fixture/provider","version":"1.0.0","platform":"%s/%s","cliContract":4,"runtimeProtocol":2,"runtimeABI":1}'
  exit 0
fi
if [ "$1" = "__putnami" ] && [ "$2" = "workspace-probe" ]; then
  printf '%%s\n' '%s'
  exit 0
fi
exit 2
`, runtime.GOOS, runtime.GOARCH, probe), 0o755)

	initCLISelectionGitRepo(t, root)
	runCLISelectionGit(t, root, "tag", "v1.0.0")
	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })
	t.Setenv(releaseSetFixtureProviderEnv, "1")
	return root
}

// boundPublicationManifest is the fixture extension. Its publish job is this
// test binary in the pack role; cloud-release-set lets a run without a
// publication-v1 provider resolve through the legacy fixture provider.
const boundPublicationManifest = `{
  "name": "@fixture/provider",
  "version": "1.0.0",
  "cliContract": 4,
  "runtime": {"executable": "runtime"},
  "workspace": {"markers": ["marker.txt"], "inputs": ["marker.txt"]},
  "ecosystems": [
    {
      "id": "npm",
      "coordinate": {"pattern": "^[a-z0-9@._/-]+$"},
      "version": {"pattern": "^[0-9A-Za-z][0-9A-Za-z.+-]*$", "ordering": "semver"},
      "channel": "native",
      "registries": {"type": "object"},
      "publish": "publish"
    }
  ],
  "commands": {
    "build": {"run": [{"id": "compile", "task": "noop"}]},
    "package": {"dependsOn": ["build"], "run": [{"id": "artifact", "task": "noop"}]},
    "publish": {"dependsOn": ["package"], "run": [{"id": "artifact", "task": "pack"}]},
    "cloud-release-set": {"visibility": "internal", "run": [{"id": "provider", "task": "noop"}]}
  },
  "tasks": {
    "noop": {"kind": "command", "command": "/bin/sh", "args": ["-c", "exit 0"], "cwd": "{workspaceRoot}", "cache": false},
    "pack": {"kind": "command", "command": %s, "cache": false, "timeoutMs": 60000, "env": {%s: "1"}}
  }
}`

// boundPublicationRegistry is an npm registry that stores what it is sent
// with boundPublicationBearer and refuses every other request.
type boundPublicationRegistry struct {
	server *httptest.Server

	mu       sync.Mutex
	tarballs map[string][]byte
	refused  int
}

func newBoundPublicationRegistry(t *testing.T) *boundPublicationRegistry {
	t.Helper()
	reg := &boundPublicationRegistry{tarballs: map[string][]byte{}}
	reg.server = httptest.NewServer(http.HandlerFunc(reg.serve))
	t.Cleanup(reg.server.Close)
	return reg
}

func (reg *boundPublicationRegistry) serve(w http.ResponseWriter, r *http.Request) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+boundPublicationBearer {
		reg.refused++
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var payload npmpublish.Payload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for version := range payload.Versions {
			for _, attachment := range payload.Attachments {
				data, err := base64.StdEncoding.DecodeString(attachment.Data)
				if err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				target, err := npmpublish.TarballURL(reg.server.URL, payload.Name, version)
				if err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				reg.tarballs[strings.TrimPrefix(target, reg.server.URL)] = data
			}
		}
		w.WriteHeader(http.StatusCreated)
	case http.MethodGet:
		data, ok := reg.tarballs[r.URL.EscapedPath()]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(data)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// take returns how many tarballs the registry stores and how many requests
// it refused, and empties it.
func (reg *boundPublicationRegistry) take() (stored, refused int) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	stored, refused = len(reg.tarballs), reg.refused
	reg.tarballs, reg.refused = map[string][]byte{}, 0
	return stored, refused
}

// runWithPublicationProvider runs req with a publish broker over provider
// installed as the run's publication provider, as the CLI installs it for an
// invocation whose providers name publish.
func runWithPublicationProvider(t *testing.T, req Request, provider *providertest.Provider) (SessionResult, string) {
	t.Helper()
	broker := credentialprovider.NewBroker(credentialprovider.Purposes([]string{runner.InvocationProviderPublish}),
		func(ctx context.Context) (*credentialprovider.Session, error) {
			session := credentialprovider.Connect(provider.Pipes())
			if _, err := session.Initialize(ctx, ""); err != nil {
				_ = session.Close()
				return nil, err
			}
			return session, nil
		})
	restore := broker.InstallPublication()
	defer func() {
		restore()
		_ = broker.Close()
	}()
	return runBound(t, req)
}

// A submitter that plans without the provider's capability computes the
// expected plan of a bound publish. The executing engine, whose provider
// echoes publication-v1, accepts that plan, then opens, uploads and releases
// through the provider, and opens the plan digest a local run of the same
// commit opens.
func TestBoundRequestPublishesThroughThePublicationProvider(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "bound-requests-publish-through-the-provider", "a-bound-request-opens-uploads-and-releases-the-local-plan")
	npm := newBoundPublicationRegistry(t)
	root := boundPublicationFixture(t, npm.server.URL)
	target, err := url.Parse(npm.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	versions, err := jobs.BuildRunVersions(ws, nil)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := runner.NewParam("pr-0")
	if err != nil {
		t.Fatal(err)
	}
	commands, params := []string{"build", "publish"}, map[string]runner.ParamValue{"channel": channel}
	request := func(global GlobalFlags) Request {
		native, err := runner.NativeParams(params)
		if err != nil {
			t.Fatal(err)
		}
		return Request{
			WorkspaceRoot: root, Config: wsproto.Load(root), Commands: commands,
			CommandParams: native, Global: global, Stdout: io.Discard,
		}
	}

	// The submitter plans with no publication provider: the legacy plan.
	submitted := request(GlobalFlags{Projects: boundPublicationProject, Plan: true, NoCache: true})
	submission, stderr := runBound(t, submitted)
	if submission.ExitCode != ExitSuccess {
		t.Fatalf("the submitter's plan = exit %d, stderr %q", submission.ExitCode, stderr)
	}
	expected, err := expectedPlan(submission.Plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range expected.Tasks {
		if task.Identity.Key == boundPublicationOpenKey || strings.HasSuffix(task.Identity.Key, "~upload") {
			t.Fatalf("the submitter planned the engine node %s", task.Identity.Key)
		}
	}

	// A local run of the commit opens the plan digest the bound run must open.
	local := providertest.New(providertest.Config{Bearer: boundPublicationBearer, Hosts: []string{target.Host}})
	result, stderr := runWithPublicationProvider(t, request(GlobalFlags{Projects: boundPublicationProject, NoCache: true}), local)
	if result.ExitCode != ExitSuccess || len(local.Opens()) != 1 {
		t.Fatalf("the local publish = exit %d with %d opens, stderr %q", result.ExitCode, len(local.Opens()), stderr)
	}
	localDigest := local.Opens()[0].Plan.PlanDigest
	if stored, refused := npm.take(); stored != 1 || refused != 0 {
		t.Fatalf("the local publish stored %d tarballs and the registry refused %d requests", stored, refused)
	}

	bound := request(GlobalFlags{Projects: boundPublicationProject, NoCache: true})
	bound.Portable = &PortableExecution{Request: runner.ExecutionRequest{
		Invocation: runner.InvocationBlock{Commands: commands, Params: params, Publication: &runner.PublicationBlock{Barrier: []string{"build"}}},
		Selection:  runner.SelectionBlock{Mode: protocoljob.SelectionModeProjects, Scoped: true, Projects: []string{boundPublicationProject}},
		Source:     runner.SourceBlock{Versions: lineVersions(versions)},
		Plan:       expected,
	}}
	provider := providertest.New(providertest.Config{Bearer: boundPublicationBearer, Hosts: []string{target.Host}})
	result, stderr = runWithPublicationProvider(t, bound, provider)
	if result.ExitCode != ExitSuccess {
		t.Fatalf("the bound publish = exit %d, stderr %q", result.ExitCode, stderr)
	}
	opens := provider.Opens()
	if len(opens) != 1 || opens[0].Plan.PlanDigest != localDigest {
		t.Fatalf("the bound publish opened %d plans, want one with the local digest %s: %+v", len(opens), localDigest, opens)
	}
	if stored, refused := npm.take(); stored != 1 || refused != 0 {
		t.Errorf("the bound publish stored %d tarballs and the registry refused %d requests; want one upload with the publish credential", stored, refused)
	}
	var calls []string
	for _, call := range provider.Calls() {
		calls = append(calls, strings.TrimSpace(string(call.Op)+" "+call.Purpose))
	}
	open := slices.Index(calls, string(registry.CredentialOpOpen))
	credential := slices.Index(calls, string(registry.CredentialOpCredential)+" "+registry.PurposePublish)
	release := slices.Index(calls, string(registry.CredentialOpRelease))
	if open < 0 || credential < open || release < credential || provider.Releases() != 1 {
		t.Errorf("provider calls = %q with %d releases; want open, then the publish credential, then one release", calls, provider.Releases())
	}
	keys := planKeys(t, result.Plan)
	assertPlanned(t, keys, boundPublicationOpenKey, "/app:publish~artifact~upload")
}

// declaredNPMRegistry is the registries.npm.publish value of the fixture
// workspace, one level above the job's project: the registry a managed npm job
// resolves and records in its outbox member.
func declaredNPMRegistry() (string, error) {
	config, err := os.ReadFile(filepath.Join(os.Getenv("PUTNAMI_PROJECT_ROOT"), "..", wsproto.WorkspaceConfigFilename))
	if err != nil {
		return "", err
	}
	var declared struct {
		Registries struct {
			NPM struct {
				Publish string `json:"publish"`
			} `json:"npm"`
		} `json:"registries"`
	}
	if err := json.Unmarshal(config, &declared); err != nil {
		return "", err
	}
	return declared.Registries.NPM.Publish, nil
}
