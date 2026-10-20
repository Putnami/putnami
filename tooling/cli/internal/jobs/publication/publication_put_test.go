package publication

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/cli/model/workspace"
	distribution "go.putnami.dev/protocol/distribution"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	put "go.putnami.dev/protocol/put"
	registryproto "go.putnami.dev/protocol/registry"
	"go.putnami.dev/sdk/extension/publicationoutbox"
	"go.putnami.dev/sdk/extension/publicationupload/publicationuploadtest"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/tooling/cli/internal/credentialprovider/providertest"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// putRegistry is the shared fake Put registry (publicationuploadtest),
// answering the publication bearer.
type putRegistry struct {
	*publicationuploadtest.PutRegistry
	server *httptest.Server
}

func newPutRegistry(t *testing.T) *putRegistry {
	t.Helper()
	reg := publicationuploadtest.NewPutRegistry(t, publicationBearer)
	return &putRegistry{PutRegistry: reg, server: reg.Server}
}

// recorded returns the requests and the publish bodies received so far.
func (reg *putRegistry) recorded() ([]string, [][]byte) {
	return reg.Recorded()
}

// putMemberFixture is one Put registry member a publication job packs: its
// identity, the publish step whose name gives it its kind
// (releaseset.KindFor), its kind, manifest media type, blobs and canonical
// manifest payload.
type putMemberFixture struct {
	ecosystem, coordinate, version, path, step, mediaType string
	kind                                                  distribution.MemberKind
	blobs                                                 []putBlobFixture
	payload                                               []byte
	// platforms are the archive platforms the payload names.
	platforms map[string]string
}

type putBlobFixture struct {
	mediaType string
	data      []byte
}

// putMemberFixtures is one member of every kind put-write/v1 publishes: a
// release archive of two platforms, a config, a migration and a doc member.
func putMemberFixtures() []putMemberFixture {
	linux, darwin := []byte("linux cli archive"), []byte("darwin cli archive")
	bundle, site := []byte("orders migration bundle"), []byte("doc site content")
	return []putMemberFixture{
		{
			ecosystem: extensionproto.OutboxEcosystemArchive, kind: distribution.KindArchive,
			coordinate: "putnami/cli", version: "0.3.0", path: "cli", step: "cloud-publish-archives", mediaType: put.ArchiveManifestMediaType,
			blobs: []putBlobFixture{{put.GzipBlobMediaType, linux}, {put.BinaryBlobMediaType, darwin}},
			payload: []byte(fmt.Sprintf(`{"artifacts":{"darwin-arm64":{"digest":%q,"size":%d},"linux-amd64":{"digest":%q,"size":%d}}}`,
				put.Digest(darwin), len(darwin), put.Digest(linux), len(linux))),
			platforms: map[string]string{"darwin/arm64": put.Digest(darwin), "linux/amd64": put.Digest(linux)},
		},
		{
			ecosystem: extensionproto.OutboxEcosystemPut, kind: distribution.KindConfig,
			coordinate: "putnami/settings", version: "2026.10.2", path: "settings", step: "cloud-publish-config", mediaType: put.ConfigManifestMediaType,
			payload: []byte(`{"keys":{"region":"eu"}}`),
		},
		{
			ecosystem: extensionproto.OutboxEcosystemPut, kind: distribution.KindMigration,
			coordinate: "putnami/orders-db", version: "0007", path: "orders-db", step: "cloud-publish-migration", mediaType: put.MigrationManifestMediaType,
			blobs:   []putBlobFixture{{put.MigrationBundleBlobMediaType, bundle}},
			payload: []byte(fmt.Sprintf(`{"blob_digest":%q,"database":"orders"}`, put.Digest(bundle))),
		},
		{
			ecosystem: extensionproto.OutboxEcosystemPut, kind: distribution.KindDoc,
			coordinate: "putnami/doc-contents-cli", version: "0.3.0", path: "docs", step: "cloud-publish-site-content", mediaType: put.DocManifestMediaType,
			blobs:   []putBlobFixture{{put.GzipBlobMediaType, site}},
			payload: []byte(fmt.Sprintf(`{"artifact":{"blob":%q},"site":"docs"}`, put.Digest(site))),
		},
	}
}

func (f putMemberFixture) key() string {
	return releaseset.MemberKey(distribution.Ecosystem(f.ecosystem), f.coordinate)
}

// pack writes the member's manifest and blobs into writer and returns the
// outbox member that names them.
func (f putMemberFixture) pack(t *testing.T, writer *publicationoutbox.Writer) extensionproto.OutboxMember {
	t.Helper()
	manifest, err := writer.WriteFile("put/manifest.json", f.payload)
	if err != nil {
		t.Fatal(err)
	}
	block := &extensionproto.OutboxPut{MediaType: f.mediaType, Manifest: manifest}
	for index, blob := range f.blobs {
		file, err := writer.WriteFile(fmt.Sprintf("put/blob-%d", index), blob.data)
		if err != nil {
			t.Fatal(err)
		}
		block.Blobs = append(block.Blobs, extensionproto.OutboxPutBlob{Path: file.Path, Digest: file.Digest, Size: file.Size, MediaType: blob.mediaType})
	}
	return extensionproto.OutboxMember{Ecosystem: f.ecosystem, Coordinate: f.coordinate, Version: f.version, Project: "/" + f.path, Put: block}
}

// addPutMember adds the member's project, whose publication job is the
// member's publish step and runs script, with registries.put.registry naming
// registry. The plan records the member's kind, as a plan with member
// attribution does.
func addPutMember(h *publicationHarness, f putMemberFixture, registry string, script fixtureScript) *jobmodel.ScheduledJob {
	h.t.Helper()
	job := h.addMember(f.ecosystem, f.coordinate, f.version, f.path, map[string]string{"put": registriesEntry("registry", registry)}, script)
	job.JobDef.StepID, job.JobDef.Name = f.step, "publish~"+f.step
	route := h.routes[f.key()]
	route.publishStep = f.step
	h.routes[f.key()] = route
	h.members[len(h.members)-1].Kind = f.kind
	return job
}

// packedPutMember adds the member with a publication job that packs it.
func packedPutMember(t *testing.T, h *publicationHarness, f putMemberFixture, registry string, before ...[]string) *jobmodel.ScheduledJob {
	t.Helper()
	outbox := stageOutbox(t, func(w *publicationoutbox.Writer) []extensionproto.OutboxMember {
		return []extensionproto.OutboxMember{f.pack(t, w)}
	})
	return addPutMember(h, f, registry, append(fixtureScript(before), []string{"copy-tree", outbox, "$PUTNAMI_PUBLICATION_OUTBOX"}))
}

// The engine sends a release archive, a config, a migration and a doc member
// to the provider's upload, for the project's Put registry and with the kind
// the plan gives each; the provider uploads them as private immutable
// versions with no channel. Each published-member event, the released set and
// the release evidence carry the SHA-256 of the stored manifest payload, and
// the archive carries the platforms its packed manifest names. No
// publication job holds the bearer or reports a member.
func TestEngineUploadsPutRegistryMembers(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "uploads-go-to-the-credential-provider", "put-registry-members-go-to-the-session")
	reg := newPutRegistry(t)
	h := newPublicationHarness(t, providertest.Config{Bearer: publicationBearer, Hosts: []string{serverHost(t, reg.server)}})
	fixtures := putMemberFixtures()
	jobs := map[string]*jobmodel.ScheduledJob{}
	environs := map[string]string{}
	for _, f := range fixtures {
		environs[f.key()] = filepath.Join(h.root, f.path+"-environ")
		jobs[f.key()] = packedPutMember(t, h, f, reg.server.URL+"/", []string{"write-environ", environs[f.key()]})
	}
	planned, internal := h.plan(nil, fixedAncestry{testRevision}, nil)
	results := h.execute(context.Background(), planned, internal, nil)

	if outcome := results[releaseSetResultKey]; outcome == nil || outcome.Status != "success" {
		var uploads []string
		for _, f := range fixtures {
			uploads = append(uploads, resultMessage(results[h.uploadKey(jobs[f.key()])]))
		}
		t.Fatalf("release: %s; uploads %v", resultMessage(outcome), uploads)
	}
	head, moved := h.provider.Head("putnami", "canary")
	set, stored := h.provider.ReleaseSet(head)
	if !moved || !stored || len(set.Members) != len(fixtures) {
		t.Fatalf("released head %v (%t), set %+v", head, moved, set)
	}
	evidence := h.session.releases[0].Evidence.Members
	if len(h.session.releases) != 1 || len(evidence) != len(fixtures) {
		t.Fatalf("release evidence %+v", h.session.releases)
	}
	for _, f := range fixtures {
		job := jobs[f.key()]
		want := put.Digest(f.payload)
		if events := uploadEvents(t, results[job.Key()]); len(events) != 0 {
			t.Fatalf("the publication job %s reported %d published members", job.Key(), len(events))
		}
		events := uploadEvents(t, results[h.uploadKey(job)])
		if len(events) != 1 || events[0].Ecosystem != f.ecosystem || events[0].ArtifactDigest != want || !maps.Equal(events[0].Platforms, f.platforms) {
			t.Fatalf("upload of %s reported %+v; want %s with platforms %v", f.coordinate, events, want, f.platforms)
		}
		index := slices.IndexFunc(set.Members, func(member distribution.ReleaseSetMember) bool {
			return string(member.Ecosystem) == f.ecosystem && member.Coordinate == f.coordinate
		})
		if index < 0 || set.Members[index].ArtifactDigest != want || set.Members[index].Kind != f.kind || !maps.Equal(set.Members[index].Platforms, f.platforms) {
			t.Fatalf("released %s as %+v", f.coordinate, set.Members)
		}
		index = slices.IndexFunc(evidence, func(member registryproto.PublicationMemberEvidence) bool {
			return member.Ecosystem == f.ecosystem && member.Coordinate == f.coordinate
		})
		if index < 0 || evidence[index].Digest != want || evidence[index].Project != f.path {
			t.Fatalf("release evidence for %s: %+v", f.coordinate, evidence)
		}
		dump, err := os.ReadFile(environs[f.key()])
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(dump), publicationBearer) {
			t.Fatalf("the publication job of %s held the publish bearer", f.coordinate)
		}
	}
	requests, publishes := reg.recorded()
	if len(publishes) != len(fixtures) || len(requests) != len(fixtures)+4 {
		t.Fatalf("the registry received %v", requests)
	}
	for _, body := range publishes {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Fatal(err)
		}
		if keys := slices.Sorted(maps.Keys(fields)); !slices.Equal(keys, []string{"media_type", "payload", "version"}) {
			t.Fatalf("a publish request carried %v", keys)
		}
	}
	if ops := h.session.recorded(); slices.Index(ops, "open") > slices.Index(ops, "upload") {
		t.Fatalf("ops %v: an upload before open", ops)
	}
	for _, upload := range h.provider.Uploads() {
		index := slices.IndexFunc(fixtures, func(f putMemberFixture) bool { return f.coordinate == upload.Params.Member.Coordinate })
		if index < 0 || upload.Params.Kind != fixtures[index].kind || upload.Params.Registry != reg.server.URL {
			t.Fatalf("upload of %s: kind %q, registry %q", upload.Params.Member.Coordinate, upload.Params.Kind, upload.Params.Registry)
		}
	}
}

// A put or archive member is admitted only at a kind put-write/v1 publishes,
// and a release archive only in the archive ecosystem: every other selected
// member is refused before the plan opens. The kind is the one the member's
// declared steps give it, with or without member attribution.
func TestPublicationAdmitsPutRegistryMembersByKind(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "uploads-go-to-the-credential-provider", "put-registry-members-go-to-the-session")
	for _, tc := range []struct {
		ecosystem string
		kind      distribution.MemberKind
		refusal   string
	}{
		{ecosystem: "archive", kind: distribution.KindArchive},
		{ecosystem: "put", kind: distribution.KindConfig},
		{ecosystem: "put", kind: distribution.KindMigration},
		{ecosystem: "put", kind: distribution.KindDoc},
		{ecosystem: "put", kind: distribution.KindDeployment},
		{ecosystem: "npm", kind: distribution.KindLibrary},
		{ecosystem: "go"},
		{ecosystem: "oci", kind: distribution.KindImage},
		{ecosystem: "archive", kind: distribution.KindConfig, refusal: `uploads no archive member of kind "config"`},
		{ecosystem: "archive", refusal: `uploads no archive member of kind ""`},
		{ecosystem: "put", kind: distribution.KindArchive, refusal: `uploads no put member of kind "archive"`},
		{ecosystem: "put", kind: distribution.KindImage, refusal: `uploads no put member of kind "image"`},
		{ecosystem: "put", kind: distribution.KindLibrary, refusal: `uploads no put member of kind "library"`},
		{ecosystem: "put", refusal: `uploads no put member of kind ""`},
		{ecosystem: "pypi", kind: distribution.KindLibrary, refusal: "uploads npm, go, oci, put and archive members only"},
	} {
		t.Run(tc.ecosystem+"/"+string(tc.kind), func(t *testing.T) {
			err := admitPublicationMember(releaseset.PlannedMember{Ecosystem: distribution.Ecosystem(tc.ecosystem), Coordinate: "putnami/thing"}, tc.kind)
			switch {
			case tc.refusal == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.refusal != "" && (err == nil || !strings.Contains(err.Error(), tc.refusal) || !strings.Contains(err.Error(), "putnami/thing")):
				t.Fatalf("admission = %v, want a refusal naming the member that %s", err, tc.refusal)
			}
		})
	}

	// A put member whose publish step packs an archive, in a plan without
	// member attribution: its steps alone give it the archive kind.
	h := newPublicationHarness(t, providertest.Config{Bearer: publicationBearer})
	f := putMemberFixtures()[0]
	f.ecosystem = extensionproto.OutboxEcosystemPut
	addPutMember(h, f, "https://put.example.test", fixtureScript{})
	h.members[0].Kind = ""
	plan := &releaseset.Plan{
		ProtocolVersion: distribution.ProtocolVersion, Namespace: "putnami",
		Channels: []string{"canary"}, Heads: map[string]*distribution.ChannelHead{"canary": nil}, Members: slices.Clone(h.members),
	}
	h.run = &ReleaseSetRun{provider: sessionResolver{h.session}, publication: h.session, plan: plan, routes: h.routes, sourceRevision: testRevision, root: h.root}
	planned, err := h.run.AttachPlan(slices.Clone(h.jobs))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.run.BindPublication(fixedAncestry{testRevision}, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.run.AttachBarrier(planned); err == nil || !strings.Contains(err.Error(), `uploads no put member of kind "archive"`) {
		t.Fatalf("attach a put member of kind archive: %v", err)
	}
	if ops := h.session.recorded(); len(ops) != 0 {
		t.Fatalf("ops %v for a refused plan", ops)
	}
}

// Under publication-v1 a put or archive member is released only when an
// engine upload node uploaded it: a published-member event a publication job
// reports for one fails the release, and so does a selected member no upload
// node published. No channel moves.
func TestAPutMemberTheEngineDidNotUploadFailsTheRelease(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "uploads-go-to-the-credential-provider", "a-put-member-the-engine-did-not-upload-fails-the-release")
	for _, tc := range []struct {
		name    string
		packs   bool
		reports bool
		refusal string
	}{
		{name: "a job that reports its own member", reports: true, refusal: "were reported by a job the engine did not upload for: archive/putnami/cli by "},
		{name: "a job that packs and reports its member", packs: true, reports: true, refusal: "were reported by a job the engine did not upload for: archive/putnami/cli by "},
		{name: "a job that packs nothing", refusal: "the engine uploaded no artifact for 1 selected put or archive member(s): archive/putnami/cli"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := newPutRegistry(t)
			h := newPublicationHarness(t, providertest.Config{Bearer: publicationBearer, Hosts: []string{serverHost(t, reg.server)}})
			f := putMemberFixtures()[0]
			job := packedPutMember(t, h, f, reg.server.URL)
			planned, internal := h.plan(nil, fixedAncestry{testRevision}, nil)
			script := fixtureScript{}
			if tc.packs {
				outbox := stageOutbox(t, func(w *publicationoutbox.Writer) []extensionproto.OutboxMember {
					return []extensionproto.OutboxMember{f.pack(t, w)}
				})
				script = append(script, []string{"copy-tree", outbox, "$PUTNAMI_PUBLICATION_OUTBOX"})
			}
			if tc.reports {
				script = append(script, []string{"print", publishedMemberRuntimeLine(h.run.plan.SelectedMembers()[0], put.Digest(f.payload))})
			}
			job.JobDef.Env = nil
			fixtureTask(t, job.JobDef, script.then(fixtureScript{{"print", releaseSetSuccessLine}}))
			results := h.execute(context.Background(), planned, internal, nil)

			outcome := results[releaseSetResultKey]
			if outcome == nil || outcome.Status != "failed" || outcome.Error == nil || !strings.Contains(outcome.Error.Message, tc.refusal) {
				t.Fatalf("release: %s; upload %s", resultMessage(outcome), resultMessage(results[h.uploadKey(job)]))
			}
			if slices.Contains(h.session.recorded(), "release") {
				t.Fatalf("ops %v: the provider was asked to release", h.session.recorded())
			}
			if _, moved := h.provider.Head("putnami", "canary"); moved {
				t.Fatal("the channel moved")
			}
		})
	}
}

// A packed member the Put write protocol refuses for its planned kind is
// refused by the provider's upload before any byte reaches the registry. The
// refusal fails the upload with the code the provider named, and nothing is
// released.
func TestARefusedPutUploadFailsTheMember(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "uploads-go-to-the-credential-provider", "a-refused-upload-fails-the-member")
	for name, tc := range map[string]struct {
		change  func(*putMemberFixture)
		refusal string
	}{
		"a manifest media type of another kind": {
			change:  func(f *putMemberFixture) { f.mediaType = put.ConfigManifestMediaType },
			refusal: "a member of kind archive publishes its manifest as " + put.ArchiveManifestMediaType,
		},
		"a blob the manifest does not reference": {
			change: func(f *putMemberFixture) {
				f.blobs = append(f.blobs, putBlobFixture{put.GzipBlobMediaType, []byte("unreferenced")})
			},
			refusal: "the manifest references 2 blobs and the member carries 3 others",
		},
		"a payload the registry would re-encode": {
			change:  func(f *putMemberFixture) { f.payload = append([]byte(" "), f.payload...) },
			refusal: "manifest: ",
		},
		"an archive size another than its blob's": {
			change: func(f *putMemberFixture) {
				f.payload = []byte(strings.Replace(string(f.payload), fmt.Sprintf(`"size":%d`, len("linux cli archive")), `"size":1`, 1))
			},
			refusal: "archive manifest: platform linux-amd64 names a blob of 1 bytes",
		},
	} {
		t.Run(name, func(t *testing.T) {
			reg := newPutRegistry(t)
			h := newPublicationHarness(t, providertest.Config{Bearer: publicationBearer, Hosts: []string{serverHost(t, reg.server)}})
			f := putMemberFixtures()[0]
			tc.change(&f)
			job := packedPutMember(t, h, f, reg.server.URL)
			planned, internal := h.plan(nil, fixedAncestry{testRevision}, nil)
			results := h.execute(context.Background(), planned, internal, nil)

			if upload := results[h.uploadKey(job)]; upload == nil || upload.Status != "failed" || upload.Error == nil ||
				!strings.Contains(upload.Error.Message, tc.refusal) || !strings.Contains(upload.Error.Message, registryproto.RefusalUploadFailed) {
				t.Fatalf("upload: %s; want a %s refusal that says %q", resultMessage(upload), registryproto.RefusalUploadFailed, tc.refusal)
			}
			if requests, _ := reg.recorded(); len(requests) != 0 {
				t.Fatalf("the registry received %v", requests)
			}
			if ops := h.session.recorded(); slices.Contains(ops, "release") {
				t.Fatalf("ops %v after a refused member", ops)
			}
		})
	}
}

// A Put blob whose bytes are not the bytes its descriptor names is refused by
// digest: the provider hashes every blob of the member before it sends one,
// so the registry receives no blob, not even the intact one listed before it.
func TestATamperedPutBlobReachesNoRegistry(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "uploads-go-to-the-credential-provider", "a-tampered-artifact-is-refused-by-digest")
	reg := newPutRegistry(t)
	h := newPublicationHarness(t, providertest.Config{Bearer: publicationBearer, Hosts: []string{serverHost(t, reg.server)}})
	f := putMemberFixtures()[0]
	outbox := stageOutbox(t, func(w *publicationoutbox.Writer) []extensionproto.OutboxMember {
		return []extensionproto.OutboxMember{f.pack(t, w)}
	})
	// The second blob keeps its size and changes its bytes.
	tampered := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tampered, "put"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tampered, "put", "blob-1"), []byte(strings.ToUpper(string(f.blobs[1].data))), 0o644); err != nil {
		t.Fatal(err)
	}
	job := addPutMember(h, f, reg.server.URL, fixtureScript{
		{"copy-tree", outbox, "$PUTNAMI_PUBLICATION_OUTBOX"},
		{"copy-tree", tampered, "$PUTNAMI_PUBLICATION_OUTBOX"},
	})
	planned, internal := h.plan(nil, fixedAncestry{testRevision}, nil)
	results := h.execute(context.Background(), planned, internal, nil)

	if upload := results[h.uploadKey(job)]; upload == nil || upload.Status != "failed" || upload.Error == nil ||
		!strings.Contains(upload.Error.Message, "digest mismatch") || !strings.Contains(upload.Error.Message, registryproto.RefusalArtifactDigestMismatch) {
		t.Fatalf("upload of a tampered blob: %s", resultMessage(upload))
	}
	if requests, _ := reg.recorded(); len(requests) != 0 {
		t.Fatalf("the registry received %v", requests)
	}
	if ops := h.session.recorded(); slices.Contains(ops, "release") {
		t.Fatalf("ops %v after a tampered blob", ops)
	}
}

// The Put registry an upload goes to is the project's registries.put.registry,
// or the default Put registry, as an HTTPS endpoint (HTTP only on loopback)
// with no credential, query or fragment. A refusal does not print the
// declared value.
func TestPutRegistryEndpoint(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "uploads-go-to-the-credential-provider", "put-registry-members-go-to-the-session")
	project := func(entry string) *workspace.Project {
		if entry == "" {
			return &workspace.Project{}
		}
		return &workspace.Project{Registries: map[string]json.RawMessage{extension.PutRegistryEcosystem: json.RawMessage(entry)}}
	}
	for _, tc := range []struct{ entry, want string }{
		{"", extension.DefaultPutRegistryURL},
		{`{"download":"https://mirror.example.test"}`, extension.DefaultPutRegistryURL},
		{registriesEntry("registry", "https://put.example.test/base/"), "https://put.example.test/base"},
		{registriesEntry("registry", "http://127.0.0.1:8080"), "http://127.0.0.1:8080"},
	} {
		if got, err := putRegistryEndpoint(project(tc.entry)); err != nil || got != tc.want {
			t.Errorf("putRegistryEndpoint(%s) = %q, %v; want %q", tc.entry, got, err, tc.want)
		}
	}
	for _, entry := range []string{
		registriesEntry("registry", "https://publisher:secret@put.example.test"),
		registriesEntry("registry", "http://put.example.test/secret"),
		registriesEntry("registry", "https://put.example.test/?secret"),
		registriesEntry("registry", "ftp://put.example.test/secret"),
		`{"registry":["secret"]}`,
	} {
		if got, err := putRegistryEndpoint(project(entry)); err == nil || strings.Contains(err.Error(), "secret") {
			t.Errorf("putRegistryEndpoint(%s) = %q, %v; want a refusal that does not print it", entry, got, err)
		}
	}
}

// An archive member's platforms are the ones its packed manifest names: an
// answer that names other platforms fails the upload, reports no published
// member and releases nothing.
func TestAnArchiveAnsweredWithOtherPlatformsFailsTheUpload(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "uploads-go-to-the-credential-provider", "the-answered-digest-must-be-the-packed-one")
	reg := newPutRegistry(t)
	h := newPublicationHarness(t, providertest.Config{
		Bearer: publicationBearer, Hosts: []string{serverHost(t, reg.server)},
		UploadAnswer: func(_ registryproto.UploadParams, answer registryproto.UploadResult) registryproto.UploadResult {
			answer.Platforms = map[string]string{"linux/amd64": answer.Platforms["linux/amd64"]}
			return answer
		},
	})
	job := packedPutMember(t, h, putMemberFixtures()[0], reg.server.URL)
	planned, internal := h.plan(nil, fixedAncestry{testRevision}, nil)
	results := h.execute(context.Background(), planned, internal, nil)

	upload := results[h.uploadKey(job)]
	if upload == nil || upload.Status != "failed" || upload.Error == nil ||
		!strings.Contains(upload.Error.Message, "platforms for the archive member putnami/cli other than its packed manifest names") {
		t.Fatalf("upload answered with other platforms: %s", resultMessage(upload))
	}
	if events := uploadEvents(t, upload); len(events) != 0 || slices.Contains(h.session.recorded(), "release") {
		t.Fatalf("events %+v, ops %v after a refused answer", events, h.session.recorded())
	}
}
