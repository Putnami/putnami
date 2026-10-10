package distributioncli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	distributionproto "go.putnami.dev/protocol/distribution"
	extensionproto "go.putnami.dev/protocol/extension"
	put "go.putnami.dev/protocol/put"
	"go.putnami.dev/sdk/extension/publicationoutbox"
	"go.putnami.dev/sdk/extension/putpublish"
	"go.putnami.dev/sdk/extension/releaseset"
)

// recordingPutServer is a put-write registry fixture: it stores every blob it
// receives with its Content-Type and answers a publish with the payload it
// was sent, so a test can compare the direct path's bytes with a packed
// outbox.
type recordingPutServer struct {
	mu        sync.Mutex
	blobs     map[string]recordedBlob
	mediaType string
	payload   []byte
	channel   json.RawMessage
}

type recordedBlob struct {
	data        []byte
	contentType string
}

func newRecordingPutServer(t *testing.T) (*recordingPutServer, *httptest.Server) {
	t.Helper()
	recorder := &recordingPutServer{blobs: map[string]recordedBlob{}}
	srv := newPutTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.mu.Lock()
		defer recorder.mu.Unlock()
		segments := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if r.Method != http.MethodPost || len(segments) < 3 {
			t.Errorf("unexpected registry request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		packageRef := segments[len(segments)-3] + "/" + segments[len(segments)-2]
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		switch segments[len(segments)-1] {
		case "blobs":
			recorder.blobs[digestOf(body)] = recordedBlob{data: body, contentType: r.Header.Get("Content-Type")}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"digest": digestOf(body), "size": len(body)})
		case "publish":
			var request map[string]json.RawMessage
			if err := json.Unmarshal(body, &request); err != nil {
				t.Fatal(err)
			}
			recorder.mediaType = unquoteJSON(t, request["media_type"])
			recorder.payload = append([]byte(nil), request["payload"]...)
			recorder.channel = request["channel"]
			respondWithAtomicManifest(t, w, packageRef, request)
		default:
			t.Errorf("unexpected registry request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	t.Cleanup(srv.Close)
	return recorder, srv
}

// packedMember reads an outbox the way the engine does: the descriptor
// through the protocol's strict parser and validator, then every file the
// member names through the SDK reader, which refuses a size or digest that
// differs from the descriptor.
type packedMember struct {
	member   extensionproto.OutboxMember
	manifest []byte
	blobs    map[string][]byte
}

func readPackedMember(t *testing.T, outbox string) packedMember {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(outbox, extensionproto.PublicationOutboxDescriptor))
	if err != nil {
		t.Fatalf("read the outbox descriptor: %v", err)
	}
	descriptor, diagnostics := extensionproto.ParsePublicationOutbox(data)
	if len(diagnostics) != 0 {
		t.Fatalf("outbox descriptor is invalid: %v", diagnostics)
	}
	if diagnostics := extensionproto.ValidatePublicationOutbox(descriptor); len(diagnostics) != 0 {
		t.Fatalf("outbox descriptor is invalid: %v", diagnostics)
	}
	if len(descriptor.Members) != 1 {
		t.Fatalf("outbox members = %+v, want exactly the one planned member", descriptor.Members)
	}
	read, err := publicationoutbox.Read(outbox)
	if err != nil {
		t.Fatalf("engine reader refused the outbox: %v", err)
	}
	member := read.Descriptor.Members[0]
	if member.Put == nil || member.NPM != nil || member.Go != nil || member.OCI != nil {
		t.Fatalf("packed member = %+v, want exactly the put block", member)
	}
	manifest, err := read.ReadFile(member.Put.Manifest)
	if err != nil {
		t.Fatalf("read the packed manifest: %v", err)
	}
	blobs := map[string][]byte{}
	for _, blob := range member.Put.Blobs {
		content, err := read.ReadFile(blob.File())
		if err != nil {
			t.Fatalf("read packed blob %s: %v", blob.Path, err)
		}
		blobs[blob.Digest] = content
	}
	return packedMember{member: member, manifest: manifest, blobs: blobs}
}

// requireEngineAdmits applies the engine's publication-v1 rules to a packed
// member: admission of the planned kind for its ecosystem
// (admitPublicationMember), the planned identity (checkPacked), and the
// put-write/v1 upload check (putpublish.Check) the upload node runs before it
// sends anything. kind comes from the member's declared package and publish
// steps, as the engine derives it. It returns the archive platforms.
func requireEngineAdmits(t *testing.T, packed packedMember, planned releaseset.PlannedMember, packageStep, publishStep string) map[string]string {
	t.Helper()
	kind := releaseset.KindFor(planned.Ecosystem, packageStep, publishStep)
	profile, published := put.ProfileFor(kind)
	if !published || (packed.member.Ecosystem == extensionproto.OutboxEcosystemArchive) != (kind == distributionproto.KindArchive) {
		t.Fatalf("publication-v1 admits no %s member of kind %q", packed.member.Ecosystem, kind)
	}
	if packed.member.Ecosystem != string(planned.Ecosystem) || packed.member.Coordinate != planned.Coordinate ||
		packed.member.Version != planned.Version || packed.member.Project != planned.ProjectID || !strings.HasPrefix(packed.member.Project, "/") {
		t.Fatalf("packed member %+v is not the planned member %+v", packed.member, planned)
	}
	if packed.member.Put.MediaType != profile.ManifestMediaType {
		t.Fatalf("packed media type = %q, want the %s profile's %q", packed.member.Put.MediaType, kind, profile.ManifestMediaType)
	}
	member := putpublish.Member{
		Kind: kind, Coordinate: packed.member.Coordinate, Version: packed.member.Version,
		MediaType: packed.member.Put.MediaType, Manifest: packed.manifest,
	}
	for _, blob := range packed.member.Put.Blobs {
		content := packed.blobs[blob.Digest]
		member.Blobs = append(member.Blobs, putpublish.Blob{
			MediaType: blob.MediaType, Digest: blob.Digest, Size: blob.Size,
			Read: func() ([]byte, error) { return content, nil },
		})
	}
	platforms, err := putpublish.Check(member)
	if err != nil {
		t.Fatalf("the engine's upload check refuses the packed member: %v", err)
	}
	return platforms
}

// requireSameBytes proves the packed member carries exactly what the direct
// path uploaded: the manifest payload and media type the publish sent, and
// every referenced blob with the bytes and Content-Type of its upload.
func requireSameBytes(t *testing.T, packed packedMember, direct *recordingPutServer) {
	t.Helper()
	if !bytes.Equal(packed.manifest, direct.payload) {
		t.Fatalf("packed manifest = %s, direct publish payload = %s", packed.manifest, direct.payload)
	}
	if packed.member.Put.MediaType != direct.mediaType {
		t.Fatalf("packed media type = %q, direct publish media type = %q", packed.member.Put.MediaType, direct.mediaType)
	}
	if packed.member.Put.Manifest.Digest != digestOf(direct.payload) {
		t.Fatalf("packed manifest digest = %s, direct artifact digest = %s", packed.member.Put.Manifest.Digest, digestOf(direct.payload))
	}
	references, err := put.BlobReferences(direct.payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(packed.member.Put.Blobs) != len(references) {
		t.Fatalf("packed %d blobs, the manifest references %d", len(packed.member.Put.Blobs), len(references))
	}
	for _, blob := range packed.member.Put.Blobs {
		uploaded, ok := direct.blobs[blob.Digest]
		if !ok {
			t.Fatalf("packed blob %s was never uploaded by the direct path", blob.Digest)
		}
		if !bytes.Equal(packed.blobs[blob.Digest], uploaded.data) || blob.MediaType != uploaded.contentType || blob.Size != int64(len(uploaded.data)) {
			t.Fatalf("packed blob %s (%s, %d bytes) differs from its upload (%s, %d bytes)", blob.Digest, blob.MediaType, blob.Size, uploaded.contentType, len(uploaded.data))
		}
	}
}

func archiveMember(projectID, coordinate, version string) releaseset.PlannedMember {
	return archivePublishPlan(projectID, coordinate, version).Members[0]
}

func TestPackArchivesIntoOutboxPacksTheBytesTheDirectPathPublishes(t *testing.T) {
	gzipDir, gzipFiles := writeArchiveDir(t, "putnami-cloud", "darwin-arm64", "linux-x64")
	templateDir, templateFiles := writeArchiveDir(t, "my-template", "any")
	cliDir := t.TempDir()
	writeTarGzWithFile(t, cliDir, "putnami-darwin-arm64.tar.gz", "compiled/putnami", []byte("ELF darwin putnami 3.0.0"))
	writeTarGzWithFile(t, cliDir, "putnami-windows-x64.tar.gz", "compiled/putnami.exe", []byte("PE windows putnami 3.0.0"))
	unknownDir, unknownFiles := writeArchiveDir(t, "putnami-cloud", "linux-x64")
	if err := os.WriteFile(filepath.Join(unknownDir, "stray.tar.gz"), []byte("stray"), 0o600); err != nil {
		t.Fatal(err)
	}
	unknownFiles = append(unknownFiles, "stray.tar.gz")

	for _, test := range []struct {
		name string
		opts archivePublishOptions
	}{
		{"gzip archives", archivePublishOptions{
			Namespace: "putnami", Package: "cloud", Version: "1.2.3",
			ArchivesDir: gzipDir, Files: gzipFiles, Artifact: "putnami-cloud", MetaArtifact: "putnami-cloud",
		}},
		{"template fan-out", archivePublishOptions{
			Namespace: "putnami", Package: "my-template", Version: "0.1.0",
			ArchivesDir: templateDir, Files: templateFiles, Artifact: "my-template", MetaArtifact: "my-template", Template: true,
		}},
		{"CLI executables", archivePublishOptions{
			Namespace: "putnami", Package: "cli", Version: "3.0.0", ArchivesDir: cliDir,
			Files: []string{"putnami-darwin-arm64.tar.gz", "putnami-windows-x64.tar.gz"}, Artifact: "putnami", MetaArtifact: "putnami", BinaryName: "putnami",
		}},
		{"archive without a platform", archivePublishOptions{
			Namespace: "putnami", Package: "cloud", Version: "1.2.3",
			ArchivesDir: unknownDir, Files: unknownFiles, Artifact: "putnami-cloud", MetaArtifact: "putnami-cloud",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			direct, srv := newRecordingPutServer(t)
			published, err := publishArchivesToRegistry(context.Background(), srv.Client(), testArchivePublishCredential(srv.URL), test.opts)
			if err != nil {
				t.Fatalf("direct publish: %v", err)
			}

			outbox := filepath.Join(t.TempDir(), "outbox")
			planned := archiveMember("/tooling/archive", test.opts.Namespace+"/"+test.opts.Package, test.opts.Version)
			packedResult, err := packArchivesIntoOutbox(outbox, planned.ProjectID, test.opts)
			if err != nil {
				t.Fatalf("pack: %v", err)
			}
			packed := readPackedMember(t, outbox)
			requireSameBytes(t, packed, direct)
			platforms := requireEngineAdmits(t, packed, planned, "archives", "cloud-publish-archives")
			if packedResult.ArtifactDigest != published.ArtifactDigest || !reflect.DeepEqual(packedResult.Platforms, published.Platforms) ||
				!reflect.DeepEqual(platforms, published.Platforms) {
				t.Fatalf("packed result %+v (engine platforms %v), direct result %+v", packedResult, platforms, published)
			}
		})
	}
}

func TestPackArchivesIntoOutboxRefusesBeforeWritingADescriptor(t *testing.T) {
	dir, files := writeArchiveDir(t, "putnami", "darwin-arm64")
	muslDir, muslFiles := writeArchiveDir(t, "putnami", "linux-x64-musl")
	for _, test := range []struct {
		name   string
		outbox string
		opts   archivePublishOptions
		want   string
	}{
		{"CLI without a binary name", filepath.Join(t.TempDir(), "outbox"), archivePublishOptions{
			Namespace: "putnami", Package: "cli", Version: "1.0.0", ArchivesDir: dir, Files: files, Artifact: "putnami", MetaArtifact: "putnami",
		}, "--binary-name"},
		{"relative outbox", "relative/outbox", archivePublishOptions{
			Namespace: "putnami", Package: "go", Version: "1.0.0", ArchivesDir: dir, Files: files, Artifact: "putnami", MetaArtifact: "putnami",
		}, "absolute"},
		{"no platform", filepath.Join(t.TempDir(), "outbox"), archivePublishOptions{
			Namespace: "putnami", Package: "go", Version: "1.0.0", ArchivesDir: dir, Files: files, Artifact: "other", MetaArtifact: "other",
		}, "no archive artifacts"},
		{"invalid version", filepath.Join(t.TempDir(), "outbox"), archivePublishOptions{
			Namespace: "putnami", Package: "go", Version: "1.0 0", ArchivesDir: dir, Files: files, Artifact: "putnami", MetaArtifact: "putnami",
		}, "version"},
		{"platform key the upload check refuses", filepath.Join(t.TempDir(), "outbox"), archivePublishOptions{
			Namespace: "putnami", Package: "go", Version: "1.0.0", ArchivesDir: muslDir, Files: muslFiles, Artifact: "putnami", MetaArtifact: "putnami",
		}, `pack putnami/go@1.0.0: archive manifest: platform "linux-x64-musl"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := packArchivesIntoOutbox(test.outbox, "/tooling/archive", test.opts)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("pack error = %v, want %q", err, test.want)
			}
			if _, statErr := os.Stat(filepath.Join(test.outbox, extensionproto.PublicationOutboxDescriptor)); !os.IsNotExist(statErr) {
				t.Fatalf("a refused pack wrote a descriptor: %v", statErr)
			}
		})
	}
}

// Under publication-v1 the archive step packs its planned member, sends no
// request, and returns no published result, so the CLI emits no member event.
func TestPublishArchivesWithResultPacksUnderThePublicationOutbox(t *testing.T) {
	workspaceRoot := writeArchivePublishWorkspace(t)
	outbox := filepath.Join(t.TempDir(), "outbox")
	env := map[string]string{"PUTNAMI_HOME": t.TempDir(), extensionproto.PublicationOutboxEnv: outbox}
	plan := archivePublishPlan("/apps/cli", "putnami/cloud", "1.2.3")
	var results []any
	ioctx := clicore.IO{Client: refusingClient(t), Stdout: func(string) {}, Stderr: func(string) {}, JSON: func(v any) { results = append(results, v) }}

	published, err := PublishArchivesWithResult(map[string]any{"app": "cli", "json": true, "channel": "stable", releaseset.ContextParamName: plan}, nil, workspaceRoot, env, ioctx)
	if err != nil || published != nil {
		t.Fatalf("PublishArchivesWithResult = (%+v, %v), want a packed member and no published result", published, err)
	}
	packed := readPackedMember(t, outbox)
	requireEngineAdmits(t, packed, plan.Members[0], "archives", "cloud-publish-archives")
	if len(results) != 1 || resultPayloadMap(t, results[0])["status"] != "packed" ||
		resultPayloadMap(t, results[0])["artifact_digest"] != packed.member.Put.Manifest.Digest {
		t.Fatalf("results = %+v, want one packed result naming the manifest digest", results)
	}

	t.Run("without a plan", func(t *testing.T) {
		outbox := filepath.Join(t.TempDir(), "outbox")
		env := map[string]string{"PUTNAMI_HOME": t.TempDir(), extensionproto.PublicationOutboxEnv: outbox}
		published, err := PublishArchivesWithResult(map[string]any{"app": "cli"}, nil, workspaceRoot, env, clicore.IO{Client: refusingClient(t), Stdout: func(string) {}, Stderr: func(string) {}})
		if err == nil || !strings.Contains(err.Error(), "release-set plan") || published != nil {
			t.Fatalf("PublishArchivesWithResult = (%+v, %v), want a refusal", published, err)
		}
		if _, statErr := os.Stat(outbox); !os.IsNotExist(statErr) {
			t.Fatalf("a refused pack touched the outbox: %v", statErr)
		}
	})
}

func TestPackConfigMemberPacksTheBytesPublishConfigMemberPublishes(t *testing.T) {
	payload := []byte(`{"formatVersion":"config.authored-member.v1","values":{"region":"eu"}}`)
	publication := ConfigMemberPublication{Namespace: "workspace-native", Package: "my-app-config", Version: "1.2.3", Payload: payload}
	direct, srv := newRecordingPutServer(t)
	published, err := PublishConfigMember(map[string]any{"registry-put-url": srv.URL}, t.TempDir(), nativePutCredentialEnv(t, srv.URL), clicore.IO{Client: srv.Client()}, publication)
	if err != nil {
		t.Fatalf("direct publish: %v", err)
	}

	outbox := filepath.Join(t.TempDir(), "outbox")
	packedResult, err := PackConfigMember(outbox, "/apps/my-app", publication)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	packed := readPackedMember(t, outbox)
	requireSameBytes(t, packed, direct)
	planned := releaseset.PlannedMember{Ecosystem: "put", Coordinate: "workspace-native/my-app-config", Version: "1.2.3", ProjectID: "/apps/my-app"}
	requireEngineAdmits(t, packed, planned, "cloud-config-member", "cloud-publish-config")
	if len(packed.member.Put.Blobs) != 0 || !reflect.DeepEqual(packedResult, published) {
		t.Fatalf("packed result %+v with %d blobs, direct result %+v", packedResult, len(packed.member.Put.Blobs), published)
	}

	for _, invalid := range []ConfigMemberPublication{
		{Namespace: "bad/path", Package: "app-config", Version: "1", Payload: payload},
		{Namespace: "workspace-native", Package: "app-config", Version: " 1", Payload: payload},
		{Namespace: "workspace-native", Package: "app-config", Version: "1"},
	} {
		outbox := filepath.Join(t.TempDir(), "outbox")
		if _, err := PackConfigMember(outbox, "/apps/my-app", invalid); err == nil || !strings.Contains(err.Error(), "invalid immutable Put identity") {
			t.Fatalf("PackConfigMember(%+v) error = %v", invalid, err)
		}
		if _, statErr := os.Stat(outbox); !os.IsNotExist(statErr) {
			t.Fatalf("a refused pack touched the outbox: %v", statErr)
		}
	}
	if _, err := PackConfigMember(filepath.Join(t.TempDir(), "outbox"), "", publication); err == nil || !strings.Contains(err.Error(), "project") {
		t.Fatalf("PackConfigMember without a project id error = %v", err)
	}
}

func TestPublishSiteContentMembersPacksTheBytesTheDirectPathPublishes(t *testing.T) {
	ws := writeSiteContentProject(t, `"site-content"`)
	plan := siteContentPlan("cloud/doc-contents-platform", "canary")
	direct, srv := newRecordingPutServer(t)
	params := map[string]any{"app": "docs/example.dev", "json": true, "channel": "canary", releaseset.ContextParamName: plan}
	published, err := PublishSiteContentMembers(params, nil, ws, siteContentBrokerEnv(t, srv.URL), quietIO(srv.Client()))
	if err != nil || len(published) != 1 {
		t.Fatalf("direct publish = (%+v, %v)", published, err)
	}
	if string(direct.channel) != `"canary"` {
		t.Fatalf("direct publish channel = %s, want the plan channel", direct.channel)
	}

	outbox := filepath.Join(t.TempDir(), "outbox")
	env := map[string]string{"PUTNAMI_HOME": t.TempDir(), extensionproto.PublicationOutboxEnv: outbox}
	var results []any
	ioctx := clicore.IO{Client: refusingClient(t), Stdout: func(string) {}, Stderr: func(string) {}, JSON: func(v any) { results = append(results, v) }}
	packedResults, err := PublishSiteContentMembers(map[string]any{"app": "docs/example.dev", "json": true, "channel": "canary", releaseset.ContextParamName: plan}, nil, ws, env, ioctx)
	if err != nil || packedResults != nil {
		t.Fatalf("PublishSiteContentMembers = (%+v, %v), want a packed member and no published result", packedResults, err)
	}
	packed := readPackedMember(t, outbox)
	requireSameBytes(t, packed, direct)
	requireEngineAdmits(t, packed, plan.Members[0], "cloud-site-content", "cloud-publish-site-content")
	if len(results) != 1 || resultPayloadMap(t, results[0])["status"] != "packed" ||
		resultPayloadMap(t, results[0])["artifact_digest"] != published[0].ArtifactDigest {
		t.Fatalf("results = %+v, want one packed result with the direct artifact digest %s", results, published[0].ArtifactDigest)
	}
}

func TestPublishSiteContentMembersPacksExactlyOneMember(t *testing.T) {
	ws := writeSiteContentProject(t, `"site-content"`)
	section := filepath.Join(ws, "docs", "example.dev", "guides")
	if err := os.MkdirAll(section, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(section, "index.md"), []byte("# Guides\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := siteContentPlan("cloud/doc-contents-platform", "canary")
	guides := plan.Members[0]
	guides.Coordinate = "cloud/doc-contents-guides"
	plan.Members = append([]releaseset.PlannedMember{guides}, plan.Members...)
	outbox := filepath.Join(t.TempDir(), "outbox")
	env := map[string]string{"PUTNAMI_HOME": t.TempDir(), extensionproto.PublicationOutboxEnv: outbox}

	_, err := PublishSiteContentMembers(map[string]any{"app": "docs/example.dev", releaseset.ContextParamName: plan}, nil, ws, env, quietIO(refusingClient(t)))
	if err == nil || !strings.Contains(err.Error(), "packs exactly one") {
		t.Fatalf("PublishSiteContentMembers error = %v, want a refusal of two members", err)
	}
	if _, statErr := os.Stat(outbox); !os.IsNotExist(statErr) {
		t.Fatalf("a refused pack touched the outbox: %v", statErr)
	}
}

func TestPutOutboxPackerListsEachBlobDigestOnce(t *testing.T) {
	outbox := filepath.Join(t.TempDir(), "outbox")
	packer, err := newPutOutboxPacker(outbox)
	if err != nil {
		t.Fatal(err)
	}
	first, err := packer.addGzipBlobBytes([]byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := packer.addGzipBlobBytes([]byte("same"))
	if err != nil || second != first {
		t.Fatalf("second blob = (%s, %v), want the first digest %s", second, err, first)
	}
	payload, err := archiveManifestPayload(map[string]archiveBlob{"linux-x64": {Digest: first, Size: 4}, "darwin-arm64": {Digest: second, Size: 4}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := packer.commit(distributionproto.KindArchive, extensionproto.OutboxEcosystemArchive, "putnami/cloud", "1.2.3", "/cli", archiveManifestMediaType, payload); err != nil {
		t.Fatal(err)
	}
	packed := readPackedMember(t, outbox)
	if len(packed.member.Put.Blobs) != 1 {
		t.Fatalf("packed blobs = %+v, want one per digest", packed.member.Put.Blobs)
	}

	t.Run("a referenced blob that was not packed", func(t *testing.T) {
		packer, err := newPutOutboxPacker(filepath.Join(t.TempDir(), "outbox"))
		if err != nil {
			t.Fatal(err)
		}
		_, err = packer.commit(distributionproto.KindArchive, extensionproto.OutboxEcosystemArchive, "putnami/cloud", "1.2.3", "/cli", archiveManifestMediaType, payload)
		if err == nil || !strings.Contains(err.Error(), "references 1 blob") {
			t.Fatalf("commit error = %v", err)
		}
	})
	t.Run("a member the upload check refuses", func(t *testing.T) {
		outbox := filepath.Join(t.TempDir(), "outbox")
		packer, err := newPutOutboxPacker(outbox)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := packer.addGzipBlobBytes([]byte("same")); err != nil {
			t.Fatal(err)
		}
		// A config member publishes no archive manifest and references no blob.
		_, err = packer.commit(distributionproto.KindConfig, extensionproto.OutboxEcosystemPut, "putnami/cloud", "1.2.3", "/cli", archiveManifestMediaType, payload)
		if err == nil || !strings.Contains(err.Error(), "pack putnami/cloud@1.2.3: a member of kind config") {
			t.Fatalf("commit error = %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(outbox, extensionproto.PublicationOutboxDescriptor)); !os.IsNotExist(statErr) {
			t.Fatalf("a refused commit wrote a descriptor: %v", statErr)
		}
	})
	t.Run("an existing descriptor", func(t *testing.T) {
		if _, err := newPutOutboxPacker(outbox); err == nil || !strings.Contains(err.Error(), "descriptor") {
			t.Fatalf("second packer on a committed outbox error = %v", err)
		}
	})
}

func TestPublicationOutboxReadsTheEngineVariable(t *testing.T) {
	if got := PublicationOutbox(map[string]string{extensionproto.PublicationOutboxEnv: "/tmp/outbox"}); got != "/tmp/outbox" {
		t.Fatalf("PublicationOutbox = %q", got)
	}
	if got := PublicationOutbox(map[string]string{}); got != "" {
		t.Fatalf("PublicationOutbox without the variable = %q", got)
	}
}
