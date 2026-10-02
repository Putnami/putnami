package credentialcustody

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
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
	distribution "go.putnami.dev/protocol/distribution"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	put "go.putnami.dev/protocol/put"
	registry "go.putnami.dev/protocol/registry"
	runtimeproto "go.putnami.dev/protocol/runtime"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/publicationoutbox"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

// putCustodyMember is one Put registry member of the Put publication
// fixture: the project that owns it, its ecosystem, coordinate and kind, and
// the publish step that packs it. The step name gives the member its kind
// (releaseset.KindFor).
type putCustodyMember struct {
	project, ecosystem, coordinate, step string
	kind                                 distribution.MemberKind
}

// putCustodyMembers is one member of every kind put-write/v1 publishes.
var putCustodyMembers = []putCustodyMember{
	{project: "cli", ecosystem: "archive", coordinate: "fixture/cli", step: "cloud-publish-archives", kind: distribution.KindArchive},
	{project: "settings", ecosystem: "put", coordinate: "fixture/settings", step: "cloud-publish-config", kind: distribution.KindConfig},
	{project: "orders-db", ecosystem: "put", coordinate: "fixture/orders-db", step: "cloud-publish-migration", kind: distribution.KindMigration},
	{project: "docs", ecosystem: "put", coordinate: "fixture/doc-contents", step: "cloud-publish-site-content", kind: distribution.KindDoc},
}

// putCustodyBlob is one blob a member uploads.
type putCustodyBlob struct {
	mediaType string
	data      []byte
}

// content is what the member's publication job packs at version: the
// manifest media type, the canonical manifest payload, the blobs the payload
// references, and, for an archive, its platforms.
func (m putCustodyMember) content(version string) (mediaType string, payload []byte, blobs []putCustodyBlob, platforms map[string]string) {
	label := m.coordinate + "@" + version
	switch m.kind {
	case distribution.KindArchive:
		linux, darwin := []byte("linux archive of "+label), []byte("darwin archive of "+label)
		payload = fmt.Appendf(nil, `{"artifacts":{"darwin-arm64":{"digest":%q,"size":%d},"linux-amd64":{"digest":%q,"size":%d}}}`,
			put.Digest(darwin), len(darwin), put.Digest(linux), len(linux))
		return put.ArchiveManifestMediaType, payload, []putCustodyBlob{{put.GzipBlobMediaType, linux}, {put.BinaryBlobMediaType, darwin}},
			map[string]string{"darwin/arm64": put.Digest(darwin), "linux/amd64": put.Digest(linux)}
	case distribution.KindConfig:
		return put.ConfigManifestMediaType, fmt.Appendf(nil, `{"keys":{"region":"eu"},"release":%q}`, label), nil, nil
	case distribution.KindMigration:
		bundle := []byte("migration bundle of " + label)
		return put.MigrationManifestMediaType, fmt.Appendf(nil, `{"blob_digest":%q,"database":"orders"}`, put.Digest(bundle)),
			[]putCustodyBlob{{put.MigrationBundleBlobMediaType, bundle}}, nil
	default:
		site := []byte("site content of " + label)
		return put.DocManifestMediaType, fmt.Appendf(nil, `{"artifact":{"blob":%q},"site":"docs"}`, put.Digest(site)),
			[]putCustodyBlob{{put.GzipBlobMediaType, site}}, nil
	}
}

// runHostilePutPublicationRole is a publication job of the Put publication
// fixture: repository code. It probes for the credential as every hostile
// role does, then packs its project's member into the outbox the engine
// handed it.
func runHostilePutPublicationRole() int {
	if code := runHostileRole("publication"); code != 0 {
		return code
	}
	if err := packPutMember(); err != nil {
		fmt.Fprintf(os.Stderr, "custody put publication: pack: %v\n", err)
		return 1
	}
	return 0
}

// packPutMember packs the member of the job's project at the version the
// engine stamped for it.
func packPutMember() error {
	root := os.Getenv("PUTNAMI_PROJECT_ROOT")
	stamp, err := os.ReadFile(filepath.Join(root, ".gen", "version.json"))
	if err != nil {
		return err
	}
	var version struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(stamp, &version); err != nil {
		return err
	}
	index := slices.IndexFunc(putCustodyMembers, func(member putCustodyMember) bool { return member.project == filepath.Base(root) })
	if index < 0 {
		return fmt.Errorf("project %s owns no member", filepath.Base(root))
	}
	member := putCustodyMembers[index]
	mediaType, payload, blobs, _ := member.content(version.Version)
	writer, err := publicationoutbox.WriterFromEnv()
	if err != nil {
		return err
	}
	manifest, err := writer.WriteFile("put/manifest.json", payload)
	if err != nil {
		return err
	}
	block := &extensionproto.OutboxPut{MediaType: mediaType, Manifest: manifest}
	for index, blob := range blobs {
		file, err := writer.WriteFile(fmt.Sprintf("put/blob-%d", index), blob.data)
		if err != nil {
			return err
		}
		block.Blobs = append(block.Blobs, extensionproto.OutboxPutBlob{Path: file.Path, Digest: file.Digest, Size: file.Size, MediaType: blob.mediaType})
	}
	if err := writer.Add(extensionproto.OutboxMember{
		Ecosystem: member.ecosystem, Coordinate: member.coordinate, Version: version.Version,
		Project: "/" + member.project, Put: block,
	}); err != nil {
		return err
	}
	return writer.Commit()
}

// putPublicationFixture is a git workspace whose four projects each publish
// one Put registry member through a publication-v1 credential provider. Its
// local extension declares the archive and put ecosystems, the credential
// provider, and a publish command with one hostile publication step per
// kind, each active only in the project that owns that kind. The workspace
// does not opt into member attribution, so the engine reads each member's
// kind from its declared steps. The workspace's after-publish hook is a
// hostile hook. Every report and log lies outside every probed root.
type putPublicationFixture struct {
	wsRoot      string
	jobReports  map[string]string
	hookReport  string
	providerLog string
	ledger      string
}

// writePutPublicationFixture builds the fixture. self is the absolute path of
// this test binary, which the provider, the publication jobs and the hook
// run; endpoint is the Put registry the workspace declares.
func writePutPublicationFixture(t *testing.T, self, endpoint string) putPublicationFixture {
	t.Helper()
	reports := t.TempDir()
	fx := putPublicationFixture{
		wsRoot:      t.TempDir(),
		jobReports:  map[string]string{},
		hookReport:  filepath.Join(reports, "hook.jsonl"),
		providerLog: filepath.Join(reports, "provider.log"),
		ledger:      filepath.Join(reports, "ledger.jsonl"),
	}
	target, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	write := func(rel string, content []byte) {
		t.Helper()
		clitest.WriteFile(t, filepath.Join(fx.wsRoot, rel), string(content))
	}
	hookCmd := fmt.Sprintf("%s=hook %s=%s exec %s",
		custodyRoleEnv, custodyReportEnv, shellQuote(fx.hookReport), shellQuote(self))
	includes := []string{jsonString("publisher")}
	var steps, tasks, probeProjects []string
	for _, member := range putCustodyMembers {
		includes = append(includes, jsonString(member.project))
		fx.jobReports[member.project] = filepath.Join(reports, member.project+".jsonl")
		marker := string(member.kind) + ".kind"
		write(member.project+"/putnami.json", fmt.Appendf(nil, `{"name":"@fixture/%s","extensions":["/publisher"]}`, member.project))
		write(member.project+"/marker.txt", []byte(member.project+"\n"))
		write(member.project+"/"+marker, []byte(member.coordinate+"\n"))
		task := "publication-" + string(member.kind)
		steps = append(steps, fmt.Sprintf(`{ "id": %s, "task": %s, "activation": { "files": [%s] } }`,
			jsonString(member.step), jsonString(task), jsonString(marker)))
		tasks = append(tasks, fmt.Sprintf(`%s: { "kind": "command", "command": %s, "cache": false, "timeoutMs": 60000, "env": { %s: "put-publication", %s: %s } }`,
			jsonString(task), jsonString(self), jsonString(custodyRoleEnv), jsonString(custodyReportEnv), jsonString(fx.jobReports[member.project])))
		probeProjects = append(probeProjects, fmt.Sprintf(
			`{"path":%q,"metadata":{"releaseSet":{"ecosystems":[{"ecosystem":%q,"coordinate":%q,"packageStep":"artifact","publishStep":%q}]}}}`,
			member.project, member.ecosystem, member.coordinate, member.step))
	}
	write(wsproto.WorkspaceConfigFilename, fmt.Appendf(nil, `{
  "name": "put-publication-ws",
  "includes": [%s],
  "registries": { "put": { "registry": %s } },
  "hooks": { "commands": { "publish": { "after": [%s] } } }
}`, strings.Join(includes, ", "), jsonString(endpoint), jsonString(hookCmd)))
	write(".gitignore", []byte(".putnami/\n.gen/\n"))
	write("publisher/putnami.json", []byte(`{"name":"@fixture/publisher"}`))
	// Both profiles mirror the ones @putnami/cloud declares for the archive
	// and put ecosystems.
	const profile = `{
      "id": %s,
      "coordinate": { "pattern": "^[a-z0-9][a-z0-9._-]{0,127}/[a-z0-9][a-z0-9._-]{0,127}$" },
      "version": { "pattern": "^[A-Za-z0-9][A-Za-z0-9._+-]{0,254}$", "ordering": "string" },
      "channel": "native",
      "registries": { "type": "object" },
      "publish": "publish"
    }`
	write("publisher/putnami.extension.json", fmt.Appendf(nil, `{
  "name": "@fixture/publisher",
  "version": "1.0.0",
  "cliContract": %d,
  "runtime": { "executable": "runtime" },
  "workspace": { "markers": ["marker.txt"], "inputs": ["marker.txt"] },
  "ecosystems": [%s, %s],
  "commands": {
    "package": { "run": [{ "id": "artifact", "task": "noop" }] },
    "publish": { "dependsOn": ["package"], "run": [%s] },
    %s: { "description": "Serve credentials.", "run": [{ "id": "serve", "task": "credential-provider" }] }
  },
  "tasks": {
    "noop": { "kind": "command", "command": "/bin/sh", "args": ["-c", "exit 0"], "cwd": "{workspaceRoot}", "cache": false },
    %s,
    "credential-provider": {
      "kind": "command",
      "command": %s,
      "cache": false,
      "env": { %s: "credential-provider", %s: %s, %s: %s, %s: %s }
    }
  }
}`,
		protocolcli.CurrentContract, fmt.Sprintf(profile, jsonString("archive")), fmt.Sprintf(profile, jsonString("put")),
		strings.Join(steps, ", "), jsonString(registry.CredentialProviderCommand), strings.Join(tasks, ",\n    "),
		jsonString(self), jsonString(custodyRoleEnv), jsonString(custodyReportEnv), jsonString(fx.providerLog),
		jsonString(custodyHostsEnv), jsonString(target.Host), jsonString(custodyLedgerEnv), jsonString(fx.ledger)))

	// The runtime answers the handshake and the workspace probe: each project
	// owns its one member, packaged by the step "artifact" and published by
	// the step of its kind.
	probe := fmt.Sprintf(`{"version":%d,"extension":"@fixture/publisher","projects":[%s]}`,
		wsproto.ProbeProtocolVersion, strings.Join(probeProjects, ","))
	info := fmt.Sprintf(`{"extension":"@fixture/publisher","version":"1.0.0","platform":"%s/%s","cliContract":%d,"runtimeProtocol":%d,"runtimeABI":%d}`,
		runtime.GOOS, runtime.GOARCH, protocolcli.CurrentContract, runtimeproto.MaxKnownProtocolVersion, runtimeproto.RuntimeABIVersion)
	write("publisher/runtime", fmt.Appendf(nil, `#!/bin/sh
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
	if err := os.Chmod(filepath.Join(fx.wsRoot, "publisher", "runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	clitest.InitGitRepo(t, fx.wsRoot)
	return fx
}

// custodyPutRegistry is a Put registry that serves put-write/v1 to
// custodyBearer and refuses every other request: one blob per digest,
// immutable versions, and the manifest read.
type custodyPutRegistry struct {
	server *httptest.Server

	mu        sync.Mutex
	blobs     map[string][]byte
	manifests map[string]put.Manifest
	publishes [][]byte
	refused   int
}

func newCustodyPutRegistry(t *testing.T) *custodyPutRegistry {
	t.Helper()
	reg := &custodyPutRegistry{blobs: map[string][]byte{}, manifests: map[string]put.Manifest{}}
	reg.server = httptest.NewServer(http.HandlerFunc(reg.serve))
	t.Cleanup(reg.server.Close)
	return reg
}

func (reg *custodyPutRegistry) serve(w http.ResponseWriter, r *http.Request) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+custodyBearer {
		reg.refused++
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	switch {
	case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "blobs" && len(body) > 0:
		digest := put.Digest(body)
		reg.blobs[digest] = body
		answerPut(w, http.StatusCreated, put.BlobReceipt{
			ID: "blob-" + digest[7:15], Digest: digest, Size: int64(len(body)),
			MediaType: r.Header.Get("Content-Type"), CreatedAt: "2026-10-02T10:00:00Z",
		})
	case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "publish":
		reg.publish(w, parts[0]+"/"+parts[1], body)
	case r.Method == http.MethodGet && len(parts) == 5 && parts[2] == "versions" && parts[4] == "manifest":
		manifest, ok := reg.manifests[parts[0]+"/"+parts[1]+"@"+parts[3]]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		answerPut(w, http.StatusOK, manifest)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// publish stores a version whose blobs are all uploaded; the strict request
// reader refuses a channel.
func (reg *custodyPutRegistry) publish(w http.ResponseWriter, coordinate string, body []byte) {
	reg.publishes = append(reg.publishes, body)
	request, diagnostics := put.ParsePublishRequest(body)
	if request == nil || len(diagnostics) > 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	references, err := put.BlobReferences(request.Payload)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	for _, reference := range references {
		if reg.blobs[reference] == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}
	key := coordinate + "@" + request.Version
	if _, exists := reg.manifests[key]; exists {
		w.WriteHeader(http.StatusConflict)
		return
	}
	manifest := put.Manifest{ID: "manifest-" + request.Version, PackageID: "package-" + coordinate, MediaType: request.MediaType, Payload: request.Payload, CreatedAt: "2026-10-02T10:00:00Z"}
	reg.manifests[key] = manifest
	answerPut(w, http.StatusCreated, put.PublishResponse{
		Package: coordinate,
		Version: put.Version{
			ID: "version-" + request.Version, PackageID: manifest.PackageID, Version: request.Version, ManifestID: manifest.ID,
			State: put.VersionStatePublished, Visibility: put.VisibilityPrivate, CreatedAt: "2026-10-02T10:00:00Z",
		},
		Manifest: manifest,
		Channel:  json.RawMessage("null"),
	})
}

func answerPut(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// stored returns the stored manifests by coordinate@version, the blobs by
// digest, every publish body, and how many requests the registry refused.
func (reg *custodyPutRegistry) stored() (map[string]put.Manifest, map[string][]byte, [][]byte, int) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return maps.Clone(reg.manifests), maps.Clone(reg.blobs), slices.Clone(reg.publishes), reg.refused
}

// readRelease reads the one release payload the provider ledger recorded.
func readRelease(t *testing.T, ledger string) registry.ReleaseParams {
	t.Helper()
	data, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatalf("the provider wrote no ledger: %v", err)
	}
	var releases []registry.ReleaseParams
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var entry ledgerEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("ledger line %q: %v", line, err)
		}
		if entry.Op != string(registry.CredentialOpRelease) {
			continue
		}
		var params registry.ReleaseParams
		if err := json.Unmarshal(entry.Payload, &params); err != nil {
			t.Fatalf("release payload: %v", err)
		}
		releases = append(releases, params)
	}
	if len(releases) != 1 {
		t.Fatalf("the ledger holds %d releases; want one", len(releases))
	}
	return releases[0]
}

// A publish run through a publication-v1 credential provider whose workspace
// holds a release archive, a config, a migration and a doc member. Each
// publication job, repository code, hunts for the publish credential, finds
// nothing, and packs its member. The engine uploads every member to the Put
// registry with the credential the provider issues it after open, as a
// private version with no channel, and releases the set. The released set,
// the release evidence and each upload node's published-member event carry
// the SHA-256 of the stored manifest payload, and the archive its platforms.
func TestEveryPutRegistryKindUploadsWithNoBearerInTheRepository(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/provider-publication", "put-registry-members-upload-in-the-engine", "every-put-registry-kind-uploads-with-no-bearer-in-the-repository")
	clitest.RequireShell(t)

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	reg := newCustodyPutRegistry(t)
	fx := writePutPublicationFixture(t, self, reg.server.URL)
	code, output := runEngine(t, self, fx.wsRoot, t.TempDir(), false,
		custodyArgsEnv+"=publish\n--all\n--channel\npr-0\n--providers\npublish")
	if code != 0 {
		t.Fatalf("publish exit=%d, want 0: %s\n%s", code, failureLines(output), output)
	}

	// The provider issued the publish credential after the plan opened, and
	// released the set once.
	calls := readLog(t, fx.providerLog)
	open := slices.Index(calls, string(registry.CredentialOpOpen))
	credential := slices.Index(calls, string(registry.CredentialOpCredential)+" "+registry.PurposePublish)
	release := slices.Index(calls, string(registry.CredentialOpRelease))
	if open < 0 || credential < open || release < credential {
		t.Errorf("provider calls = %q; want open, then the publish credential, then release", calls)
	}

	// Every member is stored, released and evidenced at the digest of its
	// stored payload, and only its upload node reported it.
	manifests, blobs, publishes, refused := reg.stored()
	params := readRelease(t, fx.ledger)
	set := params.Request.ReleaseSet
	events := publishedMemberEvents(t, fx.wsRoot)
	if refused != 0 || len(manifests) != len(putCustodyMembers) || len(set.Members) != len(putCustodyMembers) ||
		len(params.Evidence.Members) != len(putCustodyMembers) || len(events) != len(putCustodyMembers) {
		t.Fatalf("the registry stores %d versions and refused %d requests; the release names %d members with %d evidence; %d published-member events",
			len(manifests), refused, len(set.Members), len(params.Evidence.Members), len(events))
	}
	wantBlobs := 0
	for _, member := range putCustodyMembers {
		index := slices.IndexFunc(set.Members, func(released distribution.ReleaseSetMember) bool {
			return string(released.Ecosystem) == member.ecosystem && released.Coordinate == member.coordinate
		})
		if index < 0 {
			t.Fatalf("the release does not carry %s/%s: %+v", member.ecosystem, member.coordinate, set.Members)
		}
		released := set.Members[index]
		mediaType, payload, memberBlobs, platforms := member.content(released.Version)
		want := put.Digest(payload)
		wantBlobs += len(memberBlobs)
		if released.ArtifactDigest != want || !maps.Equal(released.Platforms, platforms) {
			t.Errorf("released %+v; want %s with platforms %v", released, want, platforms)
		}
		stored := manifests[member.coordinate+"@"+released.Version]
		if stored.MediaType != mediaType || string(stored.Payload) != string(payload) {
			t.Errorf("the registry stores %s@%s as %s %s; want %s %s", member.coordinate, released.Version, stored.MediaType, stored.Payload, mediaType, payload)
		}
		for _, blob := range memberBlobs {
			if string(blobs[put.Digest(blob.data)]) != string(blob.data) {
				t.Errorf("the registry does not store the %s blob of %s", blob.mediaType, member.coordinate)
			}
		}
		evidence := slices.IndexFunc(params.Evidence.Members, func(entry registry.PublicationMemberEvidence) bool {
			return entry.Ecosystem == member.ecosystem && entry.Coordinate == member.coordinate
		})
		if evidence < 0 || params.Evidence.Members[evidence].Digest != want || params.Evidence.Members[evidence].Project != member.project {
			t.Errorf("release evidence %+v; want %s for %s", params.Evidence.Members, want, member.coordinate)
		}
		event := slices.IndexFunc(events, func(event sessionMemberEvent) bool {
			return event.member == member.coordinate+"@"+released.Version
		})
		if event < 0 || !strings.HasSuffix(events[event].key, "~upload") || events[event].digest != want {
			t.Errorf("published-member events %+v; want the upload node of %s to report %s", events, member.coordinate, want)
		}
	}
	if len(blobs) != wantBlobs {
		t.Errorf("the registry stores %d blobs; want %d", len(blobs), wantBlobs)
	}
	for _, body := range publishes {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Fatal(err)
		}
		if keys := slices.Sorted(maps.Keys(fields)); !slices.Equal(keys, []string{"media_type", "payload", "version"}) {
			t.Errorf("a publish request carried %v; want version, media_type and payload only", keys)
		}
	}

	// No publication job and not the hook found the publish credential.
	for _, member := range putCustodyMembers {
		assertProbesFoundNothing(t, member.project+" publication", fx.jobReports[member.project])
	}
	assertNoProbeFoundTheBearer(t, "hook", readFindings(t, fx.hookReport))
}
