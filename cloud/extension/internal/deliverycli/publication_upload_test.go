package deliverycli

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ociregistry "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"

	distribution "go.putnami.dev/protocol/distribution"
	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/protocol/oci"
	put "go.putnami.dev/protocol/put"
	registry "go.putnami.dev/protocol/registry"
	"go.putnami.dev/sdk/extension/npmpublish"
	sdkoci "go.putnami.dev/sdk/extension/oci"
	"go.putnami.dev/sdk/extension/publicationoutbox"
	"go.putnami.dev/sdk/extension/publicationupload"
	"go.putnami.dev/sdk/extension/publicationupload/publicationuploadtest"
)

const (
	uploadFeature     = "cloud/publication-uploader"
	uploadRequirement = "cloud-uploads-its-registries-in-the-provider-session"
	// uploadBearer is the write bearer Delivery mints when the plan opens.
	// The fake registries answer it alone.
	uploadBearer = "publish-bearer-1"
)

// recordingRegistry serves a registry and records how many requests it got
// and the Authorization headers they carried.
type recordingRegistry struct {
	server *httptest.Server

	mu             sync.Mutex
	requests       int
	authorizations []string
}

func newRecordingRegistry(t *testing.T, handler http.Handler) *recordingRegistry {
	t.Helper()
	rec := &recordingRegistry{}
	rec.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.requests++
		if authorization := r.Header.Get("Authorization"); authorization != "" && !slices.Contains(rec.authorizations, authorization) {
			rec.authorizations = append(rec.authorizations, authorization)
		}
		rec.mu.Unlock()
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(rec.server.Close)
	return rec
}

func (rec *recordingRegistry) host() string { return strings.TrimPrefix(rec.server.URL, "http://") }

func (rec *recordingRegistry) recorded() (int, []string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.requests, slices.Clone(rec.authorizations)
}

// uploadRig is a hosted session whose launcher exported four publication
// origins: the kit's fake npm, Go and Put registries, and an in-memory OCI
// registry. The Put origin also serves put-server's release-set routes.
type uploadRig struct {
	*pubRig
	npmRegistry *publicationuploadtest.NPMRegistry
	goRegistry  *publicationuploadtest.GoRegistry
	putRegistry *publicationuploadtest.PutRegistry
	ociRegistry *recordingRegistry
	goURL       string
	putURL      string
}

// newUploadRig starts the registries and a provider that serves them.
// goOrigin, when set, serves the Go origin in place of the fake Go registry;
// configure adjusts the server after its origins are set.
func newUploadRig(t *testing.T, goOrigin http.Handler, configure func(server *credentialServer)) *uploadRig {
	t.Helper()
	u := &uploadRig{
		npmRegistry: publicationuploadtest.NewNPMRegistry(t, uploadBearer),
		goRegistry:  publicationuploadtest.NewGoRegistry(t, uploadBearer),
		putRegistry: publicationuploadtest.NewPutRegistry(t, uploadBearer),
		ociRegistry: newRecordingRegistry(t, ociregistry.New()),
	}
	u.goURL = u.goRegistry.Server.URL
	if goOrigin != nil {
		server := httptest.NewServer(goOrigin)
		t.Cleanup(server.Close)
		u.goURL = server.URL
	}
	var releaseSets atomic.Pointer[fakePutServer]
	putOrigin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/put/_/release-sets/") {
			releaseSets.Load().serve(w, r)
			return
		}
		u.putRegistry.Server.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(putOrigin.Close)
	u.putURL = putOrigin.URL
	u.pubRig = newPubRig(t, func(server *credentialServer) {
		server.publication = publicationEndpointsFrom(map[string]string{
			publishPutURLEnv: u.putURL, publishNPMURLEnv: u.npmRegistry.Server.URL,
			publishGoURLEnv: u.goURL, publishOCIURLEnv: u.ociRegistry.server.URL,
		})
		if configure != nil {
			configure(server)
		}
	})
	releaseSets.Store(u.put)
	return u
}

// uploadOffer is what an engine that uploads through the session offers.
var uploadOffer = []string{registry.CapabilityCredentialV1, registry.CapabilityPublicationV1, registry.CapabilityUploaderV1}

// initializeWith offers capabilities with runCredential and returns the echo,
// or the refusal when the provider refuses the initialize.
func (u *uploadRig) initializeWith(runCredential string, capabilities []string) ([]string, *registry.CredentialRefusal) {
	u.t.Helper()
	u.id++
	payload := map[string]any{"protocolVersion": 1, "capabilities": capabilities}
	if runCredential != "" {
		payload["runCredential"] = runCredential
	}
	line, err := json.Marshal(map[string]any{"protocolVersion": 1, "id": u.id, "op": "initialize", "payload": payload})
	if err != nil {
		u.t.Fatal(err)
	}
	u.h.send(string(line))
	response := u.h.next(registry.CredentialOpInitialize)
	if !response.OK {
		return nil, response.Error
	}
	result, err := registry.ParseCredentialInitializeResult(response.Payload)
	if err != nil {
		u.t.Fatalf("initialize result: %v", err)
	}
	u.h.negotiated = registry.NegotiatedCapabilities(capabilities, result.Capabilities)
	return result.Capabilities, nil
}

// started initializes a hosted session that negotiates uploader-v1.
func (u *uploadRig) started() *uploadRig {
	u.t.Helper()
	if echo, refusal := u.initializeWith(testRunCredential, uploadOffer); !slices.Equal(echo, uploadOffer) {
		u.t.Fatalf("echo = %v (%+v), want %v; stderr %s", echo, refusal, uploadOffer, u.h.stderr.String())
	}
	return u
}

// uploadPlan publishes one member of each ecosystem the session uploads.
func uploadPlan(t *testing.T) registry.PublicationPlan {
	t.Helper()
	member := func(ecosystem, coordinate, version, fingerprint string) registry.PublicationPlanMember {
		return registry.PublicationPlanMember{
			Ecosystem: distribution.Ecosystem(ecosystem), Coordinate: coordinate, Version: version,
			SourceRevision: pubSource, SelectionFingerprint: pubDigest(fingerprint),
		}
	}
	return pubSeal(t, registry.PublicationPlan{
		ProtocolVersion: registry.PublicationPlanProtocolVersion, Namespace: pubNamespace, SourceRevision: pubSource,
		Channels: []string{pubCanary, pubImmutable}, ImmutableChannel: pubImmutable,
		Members: []registry.PublicationPlanMember{
			member("archive", "acme/cli", "0.3.0", "1"),
			member("go", "go.acme.dev/mod", "v1.2.3", "2"),
			member("npm", "@acme/web", "1.2.3", "3"),
			member("oci", "acme/app", "1.2.3", "4"),
			member("put", "acme/settings", "2026.10.2", "5"),
		},
	})
}

// openedPlan opens uploadPlan.
func (u *uploadRig) openedPlan() *uploadRig {
	u.t.Helper()
	plan := uploadPlan(u.t)
	mustOpened(u.t, u.open(registry.OpenParams{Plan: plan, Ancestry: pubAncestry()}), plan.PlanDigest)
	return u
}

func (u *uploadRig) upload(params registry.UploadParams) *registry.CredentialResponse {
	u.t.Helper()
	return u.op(registry.CredentialOpUpload, params)
}

// registryRequests counts the requests every origin's registry received.
func (u *uploadRig) registryRequests() int {
	_, npmRequests := u.npmRegistry.Stored("", "")
	_, goRequests := u.goRegistry.Stored("", "")
	putRequests, _ := u.putRegistry.Recorded()
	ociRequests, _ := u.ociRegistry.recorded()
	return npmRequests + goRequests + len(putRequests) + ociRequests
}

// packedUpload is a member staged in its own outbox, the upload that names
// it, and what the registry stores for it.
type packedUpload struct {
	params    registry.UploadParams
	digest    string
	platforms map[string]string
}

// path is the absolute path of the outbox file rel names.
func (p packedUpload) path(rel string) string {
	return filepath.Join(p.params.Outbox, filepath.FromSlash(rel))
}

// stagedUpload writes the member write returns into a new outbox and returns
// the upload of that member to endpoint under uploadPlan.
func stagedUpload(t *testing.T, endpoint string, kind distribution.MemberKind, write func(*publicationoutbox.Writer) extproto.OutboxMember) registry.UploadParams {
	t.Helper()
	root := filepath.Join(t.TempDir(), "outbox")
	writer, err := publicationoutbox.NewWriter(root)
	if err != nil {
		t.Fatal(err)
	}
	member := write(writer)
	if err := writer.Add(member); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	return registry.UploadParams{PlanDigest: uploadPlan(t).PlanDigest, Registry: endpoint, Kind: kind, Outbox: root, Member: member}
}

func outboxFile(t *testing.T, writer *publicationoutbox.Writer, rel string, data []byte) extproto.OutboxFile {
	t.Helper()
	file, err := writer.WriteFile(rel, data)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

// npmUpload is @acme/web@1.2.3 for the npm registry at origin, with variant
// appended to its tarball.
func npmUpload(t *testing.T, origin, variant string) packedUpload {
	t.Helper()
	endpoint, err := extproto.ManagedNPMRegistry(origin)
	if err != nil {
		t.Fatal(err)
	}
	tarball := []byte("tarball of @acme/web@1.2.3" + variant)
	params := stagedUpload(t, endpoint, "", func(w *publicationoutbox.Writer) extproto.OutboxMember {
		return extproto.OutboxMember{
			Ecosystem: extproto.OutboxEcosystemNPM, Coordinate: "@acme/web", Version: "1.2.3", Project: "/web",
			NPM: &extproto.OutboxNPM{
				Registry: origin, Tarball: outboxFile(t, w, "npm/web.tgz", tarball),
				Manifest: outboxFile(t, w, "npm/package.json", []byte(`{"name":"@acme/web","version":"1.2.3"}`)),
			},
		}
	})
	return packedUpload{params: params, digest: npmpublish.Digest(tarball)}
}

// goUpload is go.acme.dev/mod@v1.2.3 for the Go module origin at origin.
func goUpload(t *testing.T, origin string) packedUpload {
	t.Helper()
	zip := []byte("module zip of go.acme.dev/mod@v1.2.3")
	params := stagedUpload(t, origin, "", func(w *publicationoutbox.Writer) extproto.OutboxMember {
		return extproto.OutboxMember{
			Ecosystem: extproto.OutboxEcosystemGo, Coordinate: "go.acme.dev/mod", Version: "v1.2.3", Project: "/mod",
			Go: &extproto.OutboxGo{
				Zip:  outboxFile(t, w, "go/mod.zip", zip),
				Mod:  outboxFile(t, w, "go/go.mod", []byte("module go.acme.dev/mod\n")),
				Info: outboxFile(t, w, "go/mod.info", []byte(`{"Version":"v1.2.3"}`)),
			},
		}
	})
	return packedUpload{params: params, digest: npmpublish.Digest(zip)}
}

// archiveUpload is the release archive acme/cli@0.3.0 of two platforms for
// the Put registry at origin, with variant appended to its darwin archive.
func archiveUpload(t *testing.T, origin, variant string) packedUpload {
	t.Helper()
	linux, darwin := []byte("linux cli archive"), []byte("darwin cli archive"+variant)
	payload := []byte(fmt.Sprintf(`{"artifacts":{"darwin-arm64":{"digest":%q,"size":%d},"linux-amd64":{"digest":%q,"size":%d}}}`,
		put.Digest(darwin), len(darwin), put.Digest(linux), len(linux)))
	params := stagedUpload(t, origin, distribution.KindArchive, func(w *publicationoutbox.Writer) extproto.OutboxMember {
		block := &extproto.OutboxPut{MediaType: put.ArchiveManifestMediaType, Manifest: outboxFile(t, w, "put/manifest.json", payload)}
		for index, blob := range []struct {
			mediaType string
			data      []byte
		}{{put.GzipBlobMediaType, linux}, {put.BinaryBlobMediaType, darwin}} {
			file := outboxFile(t, w, fmt.Sprintf("put/blob-%d", index), blob.data)
			block.Blobs = append(block.Blobs, extproto.OutboxPutBlob{Path: file.Path, Digest: file.Digest, Size: file.Size, MediaType: blob.mediaType})
		}
		return extproto.OutboxMember{Ecosystem: extproto.OutboxEcosystemArchive, Coordinate: "acme/cli", Version: "0.3.0", Project: "/cli", Put: block}
	})
	return packedUpload{
		params:    params,
		digest:    put.Digest(payload),
		platforms: map[string]string{"darwin/arm64": put.Digest(darwin), "linux/amd64": put.Digest(linux)},
	}
}

// configUpload is the config member acme/settings@2026.10.2 for the Put
// registry at origin.
func configUpload(t *testing.T, origin string) packedUpload {
	t.Helper()
	payload := []byte(`{"keys":{"region":"eu"}}`)
	params := stagedUpload(t, origin, distribution.KindConfig, func(w *publicationoutbox.Writer) extproto.OutboxMember {
		return extproto.OutboxMember{
			Ecosystem: extproto.OutboxEcosystemPut, Coordinate: "acme/settings", Version: "2026.10.2", Project: "/settings",
			Put: &extproto.OutboxPut{MediaType: put.ConfigManifestMediaType, Manifest: outboxFile(t, w, "put/manifest.json", payload)},
		}
	})
	return packedUpload{params: params, digest: put.Digest(payload)}
}

// ociUpload is a random image packed as host/acme/app, tagged 1.2.3, for the
// registry an OCI client reaches at host.
func ociUpload(t *testing.T, host string) packedUpload {
	t.Helper()
	image, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(t.TempDir(), "layout")
	digest, err := sdkoci.WriteLayout(layout, image)
	if err != nil {
		t.Fatal(err)
	}
	params := stagedUpload(t, oci.RegistryScheme(host)+"://"+host, "", func(w *publicationoutbox.Writer) extproto.OutboxMember {
		if err := w.CopyLayout("oci/layout", layout); err != nil {
			t.Fatal(err)
		}
		return extproto.OutboxMember{
			Ecosystem: extproto.OutboxEcosystemOCI, Coordinate: "acme/app", Version: "1.2.3", Project: "/app",
			OCI: &extproto.OutboxOCI{Layout: "oci/layout", Repository: host + "/acme/app", Digest: digest, Tags: []string{"1.2.3"}},
		}
	})
	return packedUpload{params: params, digest: digest}
}

// everyEcosystem is one upload of each ecosystem to the rig's origins.
func (u *uploadRig) everyEcosystem(t *testing.T) map[string]packedUpload {
	t.Helper()
	return map[string]packedUpload{
		"npm":     npmUpload(t, u.npmRegistry.Server.URL, ""),
		"go":      goUpload(t, u.goURL),
		"oci":     ociUpload(t, u.ociRegistry.host()),
		"archive": archiveUpload(t, u.putURL, ""),
		"put":     configUpload(t, u.putURL),
	}
}

func mustUploaded(t *testing.T, response *registry.CredentialResponse, want packedUpload) {
	t.Helper()
	if !response.OK {
		t.Fatalf("upload refused: %+v", response.Error)
	}
	result, err := registry.ParseUploadResult(response.Payload)
	if err != nil {
		t.Fatalf("upload result: %v", err)
	}
	if result.Digest != want.digest || !maps.Equal(result.Platforms, want.platforms) {
		t.Fatalf("upload answered %+v, want digest %s and platforms %v", result, want.digest, want.platforms)
	}
}

// answerText is the answer as the engine reads it.
func answerText(t *testing.T, response *registry.CredentialResponse) string {
	t.Helper()
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// The session echoes uploader-v1 only beside publication-v1, and only when
// the engine offers it. An upload in a session that did not negotiate it is
// refused, and reaches no registry and no Delivery route.
func TestUploaderEchoNeedsTheOfferBesidePublication(t *testing.T) {
	spectest.Proves(t, uploadFeature, uploadRequirement, "uploader-v1-is-echoed-only-beside-publication-v1")
	cases := []struct {
		name          string
		runCredential string
		offer         []string
		configure     func(server *credentialServer)
		echo          []string
	}{
		{name: "a hosted run with origins offered both", runCredential: testRunCredential, offer: uploadOffer, echo: uploadOffer},
		{
			name: "uploader-v1 not offered", runCredential: testRunCredential,
			offer: []string{registry.CapabilityCredentialV1, registry.CapabilityPublicationV1},
			echo:  []string{registry.CapabilityCredentialV1, registry.CapabilityPublicationV1},
		},
		{name: "no run credential", offer: uploadOffer, echo: []string{registry.CapabilityCredentialV1}},
		{
			name: "no publication origin", runCredential: testRunCredential, offer: uploadOffer, echo: []string{registry.CapabilityCredentialV1},
			configure: func(server *credentialServer) { server.publication = publicationEndpointsFrom(map[string]string{}) },
		},
		{name: "uploader-v1 offered without publication-v1", runCredential: testRunCredential, offer: []string{registry.CapabilityCredentialV1, registry.CapabilityUploaderV1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := newUploadRig(t, nil, tc.configure)
			echo, refusal := u.initializeWith(tc.runCredential, tc.offer)
			if tc.echo == nil {
				if refusal == nil || refusal.Code != refusalInvalidRequest {
					t.Fatalf("initialize = %v, %+v; want an invalid_request refusal", echo, refusal)
				}
				return
			}
			if !slices.Equal(echo, tc.echo) {
				t.Fatalf("echo = %v (%+v), want %v", echo, refusal, tc.echo)
			}
			if slices.Contains(echo, registry.CapabilityUploaderV1) {
				return
			}
			u.send(registry.CredentialOpUpload, npmUpload(t, u.npmRegistry.Server.URL, "").params)
			// The engine reads no answer to an op it did not negotiate; this
			// one is parsed as a credential-v1 refusal.
			refused := mustRefusal(t, u.h.next(registry.CredentialOpCredential), refusalInvalidRequest)
			if !strings.Contains(refused.Message, "did not negotiate") {
				t.Fatalf("refusal = %q, want the negotiation reason", refused.Message)
			}
			if requests := u.registryRequests(); requests != 0 {
				t.Fatalf("the registries received %d requests", requests)
			}
			if planning, publish := u.delivery.calls(); planning+publish != 0 {
				t.Fatalf("Delivery calls = %d, %d, want none", planning, publish)
			}
		})
	}
}

// One upload of each ecosystem, sent side by side, reaches its origin under
// the bearer the plan's open minted, and each answer is the digest the
// registry stores. No answer and no diagnostic carries the bearer.
func TestUploadAnswersTheStoredDigestOfEachEcosystem(t *testing.T) {
	spectest.Proves(t, uploadFeature, uploadRequirement, "each-ecosystem-uploads-and-answers-the-stored-digest")
	spectest.Proves(t, uploadFeature, uploadRequirement, "no-answer-carries-the-bearer")
	u := newUploadRig(t, nil, nil).started().openedPlan()
	uploads := u.everyEcosystem(t)
	ids := map[int64]string{}
	for name, upload := range uploads {
		ids[int64(u.send(registry.CredentialOpUpload, upload.params))] = name
	}
	texts := make([]string, 0, len(uploads))
	for id, response := range u.collect(len(uploads), registry.CredentialOpUpload) {
		name := ids[id]
		mustUploaded(t, response, uploads[name])
		texts = append(texts, answerText(t, response))
	}
	u.noSecret(texts...)

	if tarball, _ := u.npmRegistry.Stored("@acme/web", "1.2.3"); string(tarball) != "tarball of @acme/web@1.2.3" {
		t.Fatalf("the npm registry stores %q", tarball)
	}
	if zip, _ := u.goRegistry.Stored("go.acme.dev/mod", "v1.2.3"); string(zip) != "module zip of go.acme.dev/mod@v1.2.3" {
		t.Fatalf("the Go registry stores %q", zip)
	}
	if _, publishes := u.putRegistry.Recorded(); len(publishes) != 2 {
		t.Fatalf("the Put registry received %d publishes, want the archive's and the config's", len(publishes))
	}
	if _, authorizations := u.ociRegistry.recorded(); !slices.Equal(authorizations, []string{"Bearer " + uploadBearer}) {
		t.Fatalf("the OCI registry saw authorizations %v, want the plan's bearer alone", authorizations)
	}
	if _, publish := u.delivery.calls(); publish != 1 {
		t.Fatalf("Delivery minted %d write bearers, want the open's alone", publish)
	}
}

// Before any registry request, an upload is refused before open
// (plan_not_open), for another plan or a member the plan does not publish
// (plan_mismatch), for a registry outside the publication origins, even on
// the same host at another port (registry_not_served), and when Delivery
// refuses to renew the plan's bearer.
func TestUploadRefusesBeforeAnyRegistryRequest(t *testing.T) {
	spectest.Proves(t, uploadFeature, uploadRequirement, "upload-refusals-come-before-any-registry-request")
	u := newUploadRig(t, nil, nil).started()
	outside := newRecordingRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	refusal := mustRefusal(t, u.upload(npmUpload(t, u.npmRegistry.Server.URL, "").params), registry.RefusalPlanNotOpen)
	texts := []string{refusal.Message}
	u.openedPlan()

	anotherPlan := npmUpload(t, u.npmRegistry.Server.URL, "")
	anotherPlan.params.PlanDigest = pubPlan(t).PlanDigest
	unplanned := npmUpload(t, u.npmRegistry.Server.URL, "")
	unplanned.params.Member.Version = "1.2.4"
	unplannedImage := ociUpload(t, u.ociRegistry.host())
	unplannedImage.params.Member.Coordinate = "acme/other"
	unplannedImage.params.Member.OCI.Repository = u.ociRegistry.host() + "/acme/other"
	// An image goes to its repository's host, whatever registry the request
	// names.
	imageOutside := ociUpload(t, outside.host())
	imageOutside.params.Registry = u.ociRegistry.server.URL
	for _, tc := range []struct {
		name   string
		params registry.UploadParams
		code   string
	}{
		{name: "another plan", params: anotherPlan.params, code: registry.RefusalPlanMismatch},
		{name: "a version the plan does not publish", params: unplanned.params, code: registry.RefusalPlanMismatch},
		{name: "an image the plan does not publish", params: unplannedImage.params, code: registry.RefusalPlanMismatch},
		{name: "npm outside the origins", params: npmUpload(t, outside.server.URL, "").params, code: registry.RefusalRegistryNotServed},
		{name: "go outside the origins", params: goUpload(t, outside.server.URL).params, code: registry.RefusalRegistryNotServed},
		{name: "an archive outside the origins", params: archiveUpload(t, outside.server.URL, "").params, code: registry.RefusalRegistryNotServed},
		{name: "put outside the origins", params: configUpload(t, outside.server.URL).params, code: registry.RefusalRegistryNotServed},
		{name: "oci outside the origins", params: ociUpload(t, outside.host()).params, code: registry.RefusalRegistryNotServed},
		{name: "oci named by another host", params: ociUpload(t, "oci.example.test").params, code: registry.RefusalRegistryNotServed},
		{name: "oci outside the origins with an origin as its registry", params: imageOutside.params, code: registry.RefusalRegistryNotServed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refusal := mustRefusal(t, u.upload(tc.params), tc.code)
			texts = append(texts, refusal.Message)
		})
	}
	if _, publish := u.delivery.calls(); publish != 1 {
		t.Fatalf("Delivery minted %d write bearers, want the open's alone: a refused upload asks none", publish)
	}

	// Past half its lifetime the plan's bearer is renewed first; a renewal
	// Delivery refuses fails the upload with Delivery's refusal.
	u.delivery.set(func(d *fakeDelivery) {
		d.onPublish = func(_ int, w http.ResponseWriter, _ deliveryPlanMirror) bool {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "native publication run became terminal"})
			return true
		}
	})
	u.clock.Advance(151 * time.Second)
	refusal = mustRefusal(t, u.upload(npmUpload(t, u.npmRegistry.Server.URL, "").params), refusalPublicationRefused)
	texts = append(texts, refusal.Message)

	if requests := u.registryRequests(); requests != 0 {
		t.Fatalf("the origins' registries received %d requests", requests)
	}
	if requests, _ := outside.recorded(); requests != 0 {
		t.Fatalf("the registry outside the origins received %d requests", requests)
	}
	if !strings.Contains(u.h.stderr.String(), "upload: refused (registry_not_served)") {
		t.Fatalf("stderr = %q, want each refusal reported", u.h.stderr.String())
	}
	u.noSecret(texts...)
}

// A failed upload answers why: an artifact whose size or digest is not the
// member's is artifact_digest_mismatch, a registry that holds another
// artifact at the version is artifact_conflict, and any other failure is
// upload_failed. No refusal carries the bearer.
func TestUploadFailuresAnswerTheirRefusal(t *testing.T) {
	spectest.Proves(t, uploadFeature, uploadRequirement, "upload-failures-map-to-their-refusal-codes")
	forbidden := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	u := newUploadRig(t, forbidden, nil).started().openedPlan()
	held := func(t *testing.T, upload packedUpload) {
		t.Helper()
		bearer := func(context.Context, *url.URL) (string, error) { return uploadBearer, nil }
		if _, err := publicationupload.Upload(context.Background(), publicationupload.Request{
			Outbox: upload.params.Outbox, Member: upload.params.Member, Registry: upload.params.Registry, Kind: upload.params.Kind,
		}, bearer); err != nil {
			t.Fatalf("the registry does not hold the other artifact: %v", err)
		}
	}
	tamperedTarball := npmUpload(t, u.npmRegistry.Server.URL, "")
	writeOutbox(t, tamperedTarball.path("npm/web.tgz"), []byte(strings.ToUpper("tarball of @acme/web@1.2.3")))
	tamperedImage := ociUpload(t, u.ociRegistry.host())
	manifest := tamperedImage.path("oci/layout/blobs/sha256/" + strings.TrimPrefix(tamperedImage.digest, "sha256:"))
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeOutbox(t, manifest, append(data, '\n'))

	var texts []string
	for _, tc := range []struct {
		name   string
		upload packedUpload
		code   string
		before func(t *testing.T)
	}{
		{name: "a tampered npm tarball", upload: tamperedTarball, code: registry.RefusalArtifactDigestMismatch},
		{name: "a tampered OCI manifest", upload: tamperedImage, code: registry.RefusalArtifactDigestMismatch},
		{
			name: "another npm tarball at the version", upload: npmUpload(t, u.npmRegistry.Server.URL, ""), code: registry.RefusalArtifactConflict,
			before: func(t *testing.T) { held(t, npmUpload(t, u.npmRegistry.Server.URL, " held")) },
		},
		{
			name: "another archive manifest at the version", upload: archiveUpload(t, u.putURL, ""), code: registry.RefusalArtifactConflict,
			before: func(t *testing.T) { held(t, archiveUpload(t, u.putURL, " held")) },
		},
		{name: "a Go registry that refuses the upload", upload: goUpload(t, u.goURL), code: registry.RefusalUploadFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.before != nil {
				tc.before(t)
			}
			requests := u.registryRequests()
			refusal := mustRefusal(t, u.upload(tc.upload.params), tc.code)
			texts = append(texts, refusal.Message)
			if after := u.registryRequests(); tc.code == registry.RefusalArtifactDigestMismatch && after != requests {
				t.Fatalf("a tampered artifact reached the registries with %d requests", after-requests)
			}
		})
	}
	u.noSecret(texts...)
}

func writeOutbox(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
