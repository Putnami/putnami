package credentialcustody

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
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

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	registry "go.putnami.dev/protocol/registry"
	runtimeproto "go.putnami.dev/protocol/runtime"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/npmpublish"
	"go.putnami.dev/sdk/extension/publicationoutbox"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/credentialprovider/providertest"
)

// The one member a publication fixture publishes, and the project the plan
// assigns it to.
const (
	publicationCoordinate = "@fixture/app"
	publicationProject    = "/app"
)

// runCredentialProviderRole is the credential provider of a publication
// fixture: an in-process provider (providertest) served on its standard
// streams. It negotiates publication-v1, answers every channel as empty, and
// issues custodyBearer as the publish credential for the hosts custodyHostsEnv
// names. When its session ends it appends every request it read to its log,
// one line each: the op, then the purpose of a credential request, then the
// code of a refusal.
func runCredentialProviderRole() int {
	var hosts []string
	if value := os.Getenv(custodyHostsEnv); value != "" {
		hosts = strings.Split(value, ",")
	}
	provider := providertest.New(providertest.Config{Bearer: custodyBearer, Hosts: hosts})
	provider.Serve(os.Stdin, os.Stdout)
	log := os.Getenv(custodyReportEnv)
	for _, call := range provider.Calls() {
		line := strings.TrimSpace(strings.Join([]string{string(call.Op), call.Purpose, call.Code}, " "))
		if err := appendLog(log, line); err != nil {
			return 1
		}
	}
	return 0
}

// runHostilePublicationRole is the publication job of a publication fixture:
// repository code. It probes for the credential as every hostile role does,
// then packs the fixture's npm member into the outbox the engine handed it,
// as the publication job of a language extension does.
func runHostilePublicationRole() int {
	if code := runHostileRole("publication"); code != 0 {
		return code
	}
	if err := packPublicationMember(); err != nil {
		fmt.Fprintf(os.Stderr, "custody publication: pack: %v\n", err)
		return 1
	}
	return 0
}

// packPublicationMember packs the fixture member at the version the engine
// stamped for its project.
func packPublicationMember() error {
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
	writer, err := publicationoutbox.WriterFromEnv()
	if err != nil {
		return err
	}
	tarball, err := writer.WriteFile("npm/app.tgz", []byte("tarball of "+publicationCoordinate+"@"+version.Version))
	if err != nil {
		return err
	}
	manifest, err := json.Marshal(map[string]string{"name": publicationCoordinate, "version": version.Version})
	if err != nil {
		return err
	}
	manifestFile, err := writer.WriteFile("npm/app.json", manifest)
	if err != nil {
		return err
	}
	if err := writer.Add(extensionproto.OutboxMember{
		Ecosystem: extensionproto.OutboxEcosystemNPM, Coordinate: publicationCoordinate,
		Version: version.Version, Project: publicationProject,
		NPM: &extensionproto.OutboxNPM{Tarball: tarball, Manifest: manifestFile},
	}); err != nil {
		return err
	}
	return writer.Commit()
}

// publicationFixture is a git workspace whose one project publishes one npm
// member through a publication-v1 credential provider. Its local extension
// declares the npm ecosystem, the credential provider, and a publish command
// whose one task is a hostile publication job; the workspace's after-publish
// hook is a hostile hook. Every report and log lies outside every probed root.
type publicationFixture struct {
	wsRoot      string
	jobReport   string
	hookReport  string
	providerLog string
}

// writePublicationFixture builds the fixture. self is the absolute path of
// this test binary, which the provider, the publication job and the hook run;
// endpoint is the npm registry the project publishes to.
func writePublicationFixture(t *testing.T, self, endpoint string) publicationFixture {
	t.Helper()
	reports := t.TempDir()
	fx := publicationFixture{
		wsRoot:      t.TempDir(),
		jobReport:   filepath.Join(reports, "publication.jsonl"),
		hookReport:  filepath.Join(reports, "hook.jsonl"),
		providerLog: filepath.Join(reports, "provider.log"),
	}
	target, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	hookCmd := fmt.Sprintf("%s=hook %s=%s exec %s",
		custodyRoleEnv, custodyReportEnv, shellQuote(fx.hookReport), shellQuote(self))
	write := func(rel, content string) {
		t.Helper()
		clitest.WriteFile(t, filepath.Join(fx.wsRoot, rel), content)
	}
	write(wsproto.WorkspaceConfigFilename, fmt.Sprintf(`{
  "name": "publication-ws",
  "includes": ["app", "publisher"],
  "registries": { "npm": { "publish": %s } },
  "hooks": { "commands": { "publish": { "after": [%s] } } }
}`, jsonString(endpoint), jsonString(hookCmd)))
	write(".gitignore", ".putnami/\n.gen/\n")
	write("app/putnami.json", `{"name":"@fixture/app","extensions":["/publisher"]}`)
	write("app/marker.txt", "app\n")
	write("publisher/putnami.json", `{"name":"@fixture/publisher"}`)
	write("publisher/putnami.extension.json", fmt.Sprintf(`{
  "name": "@fixture/publisher",
  "version": "1.0.0",
  "cliContract": %d,
  "runtime": { "executable": "runtime" },
  "workspace": { "markers": ["marker.txt"], "inputs": ["marker.txt"] },
  "ecosystems": [
    {
      "id": "npm",
      "coordinate": { "pattern": "^[a-z0-9@._/-]+$" },
      "version": { "pattern": "^[0-9A-Za-z][0-9A-Za-z.+-]*$", "ordering": "semver" },
      "channel": "native",
      "registries": { "type": "object" },
      "publish": "publish"
    }
  ],
  "commands": {
    "package": { "run": [{ "id": "artifact", "task": "noop" }] },
    "publish": { "dependsOn": ["package"], "run": [{ "id": "artifact", "task": "publication" }] },
    %s: { "description": "Serve credentials.", "run": [{ "id": "serve", "task": "credential-provider" }] }
  },
  "tasks": {
    "noop": { "kind": "command", "command": "/bin/sh", "args": ["-c", "exit 0"], "cwd": "{workspaceRoot}", "cache": false },
    "publication": {
      "kind": "command",
      "command": %s,
      "cache": false,
      "timeoutMs": 60000,
      "env": { %s: "publication", %s: %s }
    },
    "credential-provider": {
      "kind": "command",
      "command": %s,
      "cache": false,
      "env": { %s: "credential-provider", %s: %s, %s: %s }
    }
  }
}`,
		protocolcli.CurrentContract, jsonString(registry.CredentialProviderCommand),
		jsonString(self), jsonString(custodyRoleEnv), jsonString(custodyReportEnv), jsonString(fx.jobReport),
		jsonString(self), jsonString(custodyRoleEnv), jsonString(custodyReportEnv), jsonString(fx.providerLog),
		jsonString(custodyHostsEnv), jsonString(target.Host)))

	// The runtime answers the handshake and the workspace probe: the app owns
	// one npm member, packed and published by the step "artifact".
	probe := fmt.Sprintf(`{"version":%d,"extension":"@fixture/publisher","projects":[`+
		`{"path":"app","metadata":{"releaseSet":{"ecosystems":[`+
		`{"ecosystem":"npm","coordinate":%q,"packageStep":"artifact","publishStep":"artifact"}]}}}]}`,
		wsproto.ProbeProtocolVersion, publicationCoordinate)
	info := fmt.Sprintf(`{"extension":"@fixture/publisher","version":"1.0.0","platform":"%s/%s","cliContract":%d,"runtimeProtocol":%d,"runtimeABI":%d}`,
		runtime.GOOS, runtime.GOARCH, protocolcli.CurrentContract, runtimeproto.MaxKnownProtocolVersion, runtimeproto.RuntimeABIVersion)
	runtimePath := filepath.Join(fx.wsRoot, "publisher", "runtime")
	write("publisher/runtime", fmt.Sprintf(`#!/bin/sh
if [ "$1" = "__putnami" ] && [ "$2" = "runtime-info" ]; then
  printf '%%s\n' %s
  exit 0
fi
if [ "$1" = "__putnami" ] && [ "$2" = "workspace-probe" ]; then
  printf '%%s\n' %s
  exit 0
fi
exit 2
`, shellQuote(info), shellQuote(probe)))
	if err := os.Chmod(runtimePath, 0o755); err != nil {
		t.Fatal(err)
	}
	clitest.InitGitRepo(t, fx.wsRoot)
	return fx
}

// custodyNPMRegistry is an npm registry that stores what it is sent with
// custodyBearer and refuses every other request.
type custodyNPMRegistry struct {
	server *httptest.Server

	mu       sync.Mutex
	tarballs map[string]custodyTarball
	refused  int
}

// custodyTarball is one package version a custodyNPMRegistry stores.
type custodyTarball struct {
	name, version string
	data          []byte
}

func newCustodyNPMRegistry(t *testing.T) *custodyNPMRegistry {
	t.Helper()
	reg := &custodyNPMRegistry{tarballs: map[string]custodyTarball{}}
	reg.server = httptest.NewServer(http.HandlerFunc(reg.serve))
	t.Cleanup(reg.server.Close)
	return reg
}

func (reg *custodyNPMRegistry) serve(w http.ResponseWriter, r *http.Request) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+custodyBearer {
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
				reg.tarballs[payload.Name+"@"+version] = custodyTarball{name: payload.Name, version: version, data: data}
			}
		}
		w.WriteHeader(http.StatusCreated)
	case http.MethodGet:
		for _, tarball := range reg.tarballs {
			target, _ := npmpublish.TarballURL(reg.server.URL, tarball.name, tarball.version)
			if strings.TrimPrefix(target, reg.server.URL) == r.URL.EscapedPath() {
				_, _ = w.Write(tarball.data)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// stored returns the members the registry stores and how many requests it
// refused.
func (reg *custodyNPMRegistry) stored() ([]string, int) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	members := make([]string, 0, len(reg.tarballs))
	for member := range reg.tarballs {
		members = append(members, member)
	}
	slices.Sort(members)
	return members, reg.refused
}

// A run that publishes through a publication-v1 credential provider hands its
// publication job an outbox and no credential. The job, repository code,
// hunts for the publish credential in its environment, the user's home, the
// workspace, the temporary directory, every descriptor it holds, and, on
// Linux, the /proc environ, memory and descriptors of the engine and a ptrace
// attach, then packs its member. The engine then uploads that member with the
// credential the provider issues it after open, and an after-publish hook
// hunts again once the engine has held the credential. Every probe runs and
// finds nothing.
func TestHostilePublicationJobFindsNoPublishCredential(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/provider-publication", "uploads-run-in-the-engine", "a-hostile-publication-job-finds-no-credential")
	clitest.RequireShell(t)

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	npm := newCustodyNPMRegistry(t)
	fx := writePublicationFixture(t, self, npm.server.URL)
	code, output := runEngine(t, self, fx.wsRoot, t.TempDir(), false,
		custodyArgsEnv+"=publish\n--all\n--channel\npr-0\n--providers\npublish")
	if code != 0 {
		t.Fatalf("publish exit=%d, want 0\n%s", code, output)
	}

	// The engine uploaded the packed member with the provider's credential.
	members, refused := npm.stored()
	if len(members) != 1 || !strings.HasPrefix(members[0], publicationCoordinate+"@") || refused != 0 {
		t.Errorf("the registry stores %v and refused %d requests; want the one member, every request with the publish credential", members, refused)
	}
	// The provider issued that credential once, after the plan opened, and
	// released the set.
	calls := readLog(t, fx.providerLog)
	open := slices.Index(calls, string(registry.CredentialOpOpen))
	credential := slices.Index(calls, string(registry.CredentialOpCredential)+" "+registry.PurposePublish)
	release := slices.Index(calls, string(registry.CredentialOpRelease))
	if open < 0 || credential < open || release < credential {
		t.Errorf("provider calls = %q; want open, then the publish credential, then release", calls)
	}

	rootExempt := runtime.GOOS == "linux" && os.Geteuid() == 0
	platform := map[string]bool{}
	for _, probe := range platformSearchProbes() {
		platform[probe] = true
	}
	for role, report := range map[string]string{"publication": fx.jobReport, "hook": fx.hookReport} {
		findings := readFindings(t, report)
		for _, probe := range searchProbes() {
			f, ok := findings[probe]
			switch {
			case !ok:
				t.Errorf("%s: probe %q did not run", role, probe)
			case !f.Checked:
				t.Errorf("%s: probe %q recorded no check (detail %q)", role, probe, f.Detail)
			case slices.Contains(f.Found, "bearer"):
				if rootExempt && platform[probe] {
					t.Logf("%s: probe %q read the engine as root; root is outside the same-user threat model", role, probe)
					continue
				}
				t.Errorf("%s: probe %q found the publish credential (detail %q)", role, probe, f.Detail)
			}
		}
	}
	values := readFindings(t, fx.jobReport)[envValuesProbe]
	if values.Env["outbox"] == "" || values.Env["cloud"] != "" {
		t.Errorf("the publication job read outbox %q and a cloud token %q; want an outbox and no token", values.Env["outbox"], values.Env["cloud"])
	}
}
