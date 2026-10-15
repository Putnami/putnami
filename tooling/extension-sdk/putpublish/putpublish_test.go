package putpublish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	distribution "go.putnami.dev/protocol/distribution"
	"go.putnami.dev/protocol/features/spectest"
	infra "go.putnami.dev/protocol/infra"
	put "go.putnami.dev/protocol/put"
)

const testToken = "target-token"

// fakeRegistry implements the put-write blob upload, publish and manifest
// read. It stores one blob per digest with the media type of its first
// upload, and a version is immutable: a second publish answers 409.
type fakeRegistry struct {
	mu        sync.Mutex
	server    *httptest.Server
	blobs     map[string]put.BlobReceipt
	manifests map[string]put.Manifest
	requests  []string
	published []put.PublishRequest
	// answer, when set, replaces the answer to the request it matches.
	answer func(w http.ResponseWriter, r *http.Request) bool
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	registry := &fakeRegistry{blobs: map[string]put.BlobReceipt{}, manifests: map[string]put.Manifest{}}
	registry.server = httptest.NewServer(http.HandlerFunc(registry.serve))
	t.Cleanup(registry.server.Close)
	return registry
}

func (f *fakeRegistry) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.EscapedPath())
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if f.answer != nil && f.answer(w, r) {
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	switch {
	case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "blobs":
		f.uploadBlob(w, r)
	case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "publish":
		f.publish(w, r, parts[0]+"/"+parts[1])
	case r.Method == http.MethodGet && len(parts) == 5 && parts[2] == "versions" && parts[4] == "manifest":
		manifest, ok := f.manifests[parts[0]+"/"+parts[1]+"@"+parts[3]]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, manifest)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeRegistry) uploadBlob(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(r.Body)
	if err != nil || len(data) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	digest := put.Digest(data)
	receipt, ok := f.blobs[digest]
	if !ok {
		receipt = put.BlobReceipt{ID: "blob-" + digest[7:15], Digest: digest, Size: int64(len(data)), MediaType: r.Header.Get("Content-Type"), CreatedAt: "2026-10-02T10:00:00Z"}
		f.blobs[digest] = receipt
	}
	writeJSON(w, http.StatusCreated, receipt)
}

func (f *fakeRegistry) publish(w http.ResponseWriter, r *http.Request, coordinate string) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	request, diags := put.ParsePublishRequest(data)
	if request == nil || len(diags) > 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	references, err := put.BlobReferences(request.Payload)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	for _, reference := range references {
		if _, ok := f.blobs[reference]; !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}
	key := coordinate + "@" + request.Version
	if _, exists := f.manifests[key]; exists {
		w.WriteHeader(http.StatusConflict)
		return
	}
	f.published = append(f.published, *request)
	manifest := put.Manifest{ID: "manifest-1", PackageID: "package-1", MediaType: request.MediaType, Payload: request.Payload, CreatedAt: "2026-10-02T10:00:00Z"}
	f.manifests[key] = manifest
	writeJSON(w, http.StatusCreated, put.PublishResponse{
		Package:  coordinate,
		Version:  put.Version{ID: "version-1", PackageID: "package-1", Version: request.Version, ManifestID: "manifest-1", State: put.VersionStatePublished, Visibility: put.VisibilityPrivate, CreatedAt: "2026-10-02T10:00:00Z"},
		Manifest: manifest,
		Channel:  json.RawMessage("null"),
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func (f *fakeRegistry) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func testClient(t *testing.T) *http.Client {
	t.Helper()
	client, err := NewHTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// archiveMember is an archive of two platforms that share no blob.
func archiveMember() Member {
	linux := []byte("linux archive bytes")
	darwin := []byte("darwin archive bytes")
	payload := fmt.Sprintf(`{"artifacts":{"darwin-arm64":{"digest":%q,"size":%d},"linux-amd64":{"digest":%q,"size":%d}}}`,
		put.Digest(darwin), len(darwin), put.Digest(linux), len(linux))
	return Member{
		Kind: distribution.KindArchive, Coordinate: "acme/cli", Version: "1.2.3",
		MediaType: put.ArchiveManifestMediaType, Manifest: []byte(payload),
		Blobs: []Blob{BytesBlob(put.GzipBlobMediaType, linux), BytesBlob(put.BinaryBlobMediaType, darwin)},
	}
}

// deploymentDeclaration is a workload's deployment declaration in the
// canonical bytes infra.ParseDeployment accepts, which are the bytes the
// package step writes and the only ones a member of kind deployment carries.
func deploymentDeclaration() []byte {
	declaration := []byte(`{"protocolVersion":2,"workload":"acme/orders","runtime":{"scaling":{"max":2}}}`)
	if _, diagnostics := infra.ParseDeployment(declaration); len(diagnostics) != 0 {
		panic(fmt.Sprintf("the deployment declaration fixture is not canonical: %v", diagnostics))
	}
	return declaration
}

// members is one member of every kind put-write/v1 publishes.
func members() map[distribution.MemberKind]Member {
	bundle := []byte("migration bundle bytes")
	site := []byte("site content bytes")
	return map[distribution.MemberKind]Member{
		distribution.KindArchive: archiveMember(),
		distribution.KindConfig: {
			Kind: distribution.KindConfig, Coordinate: "acme/settings", Version: "2026.10.2",
			MediaType: put.ConfigManifestMediaType, Manifest: []byte(`{"keys":{"region":"eu"},"note":"\u003cdefault\u003e"}`),
		},
		distribution.KindMigration: {
			Kind: distribution.KindMigration, Coordinate: "acme/orders-db", Version: "0007",
			MediaType: put.MigrationManifestMediaType, Manifest: []byte(fmt.Sprintf(`{"blob_digest":%q,"database":"orders"}`, put.Digest(bundle))),
			Blobs: []Blob{BytesBlob(put.MigrationBundleBlobMediaType, bundle)},
		},
		distribution.KindDoc: {
			Kind: distribution.KindDoc, Coordinate: "acme/doc-contents", Version: "1.0.0+build.7",
			MediaType: put.DocManifestMediaType, Manifest: []byte(fmt.Sprintf(`{"artifact":{"blob":%q},"site":"docs"}`, put.Digest(site))),
			Blobs: []Blob{BytesBlob(put.GzipBlobMediaType, site)},
		},
		distribution.KindDeployment: {
			Kind: distribution.KindDeployment, Coordinate: "acme/orders-deployment", Version: "1.4.0",
			MediaType: put.DeploymentManifestMediaType, Manifest: deploymentDeclaration(),
		},
	}
}

func TestPublishStoresEveryKindWithoutAChannel(t *testing.T) {
	for kind, member := range members() {
		t.Run(string(kind), func(t *testing.T) {
			registry := newFakeRegistry(t)
			published, err := Publish(context.Background(), testClient(t), registry.server.URL, testToken, member)
			if err != nil {
				t.Fatal(err)
			}
			if published.Digest != put.Digest(member.Manifest) || published.Reused {
				t.Fatalf("Publish = %+v, want the digest %s of the stored payload and no reuse", published, put.Digest(member.Manifest))
			}
			if len(registry.published) != 1 || string(registry.published[0].Payload) != string(member.Manifest) {
				t.Fatalf("the registry received %v, want the payload byte for byte", registry.published)
			}
			if len(registry.blobs) != len(member.Blobs) {
				t.Fatalf("the registry stored %d blobs, want %d", len(registry.blobs), len(member.Blobs))
			}
			if kind == distribution.KindArchive {
				want := map[string]string{"linux/amd64": member.Blobs[0].Digest, "darwin/arm64": member.Blobs[1].Digest}
				if fmt.Sprint(published.Platforms) != fmt.Sprint(want) {
					t.Fatalf("platforms = %v, want %v", published.Platforms, want)
				}
			} else if published.Platforms != nil {
				t.Fatalf("a %s member answered platforms %v", kind, published.Platforms)
			}
		})
	}
}

func TestPublishReusesAVersionWithTheSameManifest(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "publication-outbox", "an-upload-reuses-a-version-at-the-same-digest")
	registry := newFakeRegistry(t)
	member := members()[distribution.KindMigration]
	first, err := Publish(context.Background(), testClient(t), registry.server.URL, testToken, member)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Publish(context.Background(), testClient(t), registry.server.URL, testToken, member)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Reused || second.Digest != first.Digest {
		t.Fatalf("the second publish = %+v, want a reuse at %s", second, first.Digest)
	}

	other := member
	other.Manifest = []byte(strings.Replace(string(member.Manifest), `"orders"`, `"billing"`, 1))
	if _, err := Publish(context.Background(), testClient(t), registry.server.URL, testToken, other); err == nil ||
		!strings.Contains(err.Error(), "already published with another manifest") {
		t.Fatalf("a version holding another manifest was accepted: %v", err)
	}
	otherType := members()[distribution.KindConfig]
	if _, err := Publish(context.Background(), testClient(t), registry.server.URL, testToken, otherType); err != nil {
		t.Fatal(err)
	}
	registry.mu.Lock()
	stored := registry.manifests["acme/settings@2026.10.2"]
	stored.MediaType = put.DocManifestMediaType
	registry.manifests["acme/settings@2026.10.2"] = stored
	registry.mu.Unlock()
	if _, err := Publish(context.Background(), testClient(t), registry.server.URL, testToken, otherType); err == nil {
		t.Fatal("a version stored with another media type was accepted")
	}
}

func TestPublishAcceptsABlobStoredWithAnotherMediaType(t *testing.T) {
	registry := newFakeRegistry(t)
	member := archiveMember()
	// Another publisher uploaded the linux bytes first, as a raw binary.
	first := member.Blobs[0]
	first.MediaType = put.BinaryBlobMediaType
	if err := UploadBlob(context.Background(), testClient(t), registry.server.URL, testToken, "other/tool", first); err != nil {
		t.Fatal(err)
	}
	if _, err := Publish(context.Background(), testClient(t), registry.server.URL, testToken, member); err != nil {
		t.Fatalf("a blob the registry already stored under another media type was refused: %v", err)
	}
}

func TestCheckRefusesAMemberBeforeAnyRequest(t *testing.T) {
	archive := archiveMember()
	edit := func(base Member, change func(*Member)) Member {
		base.Blobs = append([]Blob(nil), base.Blobs...)
		change(&base)
		return base
	}
	for name, tc := range map[string]struct {
		member Member
		want   string
	}{
		"an image kind":                     {edit(archive, func(m *Member) { m.Kind = distribution.KindImage }), `publishes no "image" member`},
		"a library kind":                    {edit(archive, func(m *Member) { m.Kind = distribution.KindLibrary }), `publishes no "library" member`},
		"another manifest media type":       {edit(archive, func(m *Member) { m.MediaType = put.ConfigManifestMediaType }), "publishes its manifest as"},
		"an uppercase coordinate":           {edit(archive, func(m *Member) { m.Coordinate = "Acme/cli" }), "coordinate"},
		"a coordinate of one segment":       {edit(archive, func(m *Member) { m.Coordinate = "cli" }), "coordinate"},
		"a version with a slash":            {edit(archive, func(m *Member) { m.Version = "1/2" }), "version"},
		"a spaced payload":                  {edit(archive, func(m *Member) { m.Manifest = append([]byte(" "), m.Manifest...) }), "manifest"},
		"an unescaped payload":              {edit(members()[distribution.KindConfig], func(m *Member) { m.Manifest = []byte(`{"note":"<default>"}`) }), "canonical form"},
		"a blob media type of another kind": {edit(archive, func(m *Member) { m.Blobs[0].MediaType = put.MigrationBundleBlobMediaType }), "uploads no"},
		"a config blob":                     {edit(members()[distribution.KindConfig], func(m *Member) { m.Blobs = []Blob{BytesBlob(put.GzipBlobMediaType, []byte("x"))} }), "uploads no"},
		"a deployment blob":                 {edit(members()[distribution.KindDeployment], func(m *Member) { m.Blobs = []Blob{BytesBlob(put.GzipBlobMediaType, []byte("x"))} }), "uploads no"},
		"a deployment of another protocol version": {edit(members()[distribution.KindDeployment], func(m *Member) {
			m.Manifest = []byte(strings.Replace(string(m.Manifest), `"protocolVersion":2`, `"protocolVersion":3`, 1))
		}), "deployment manifest"},
		"a deployment that is not a declaration": {edit(members()[distribution.KindDeployment], func(m *Member) { m.Manifest = []byte(`{"note":"x"}`) }), "deployment manifest"},
		"a deployment manifest media type":       {edit(members()[distribution.KindConfig], func(m *Member) { m.MediaType = put.DeploymentManifestMediaType }), "publishes its manifest as"},
		"a blob without a digest":                {edit(archive, func(m *Member) { m.Blobs[0].Digest = "sha256:x" }), "names no sha256 digest"},
		"an empty blob":                          {edit(archive, func(m *Member) { m.Blobs[0].Size = 0 }), "is empty"},
		"a repeated blob":                        {edit(archive, func(m *Member) { m.Blobs[1] = m.Blobs[0] }), "repeats"},
		"a missing blob":                         {edit(archive, func(m *Member) { m.Blobs = m.Blobs[:1] }), "must be the same blobs"},
		"an unreferenced blob": {edit(members()[distribution.KindDoc], func(m *Member) {
			m.Blobs = append(m.Blobs, BytesBlob(put.GzipBlobMediaType, []byte("extra")))
		}), "must be the same blobs"},
		"an archive size that is not the blob's": {edit(archive, func(m *Member) {
			m.Blobs[0] = BytesBlob(put.GzipBlobMediaType, []byte("linux archive bytez"))
			m.Manifest = []byte(strings.Replace(string(m.Manifest), put.Digest([]byte("linux archive bytes")), m.Blobs[0].Digest, 1))
			m.Manifest = []byte(strings.Replace(string(m.Manifest), `"size":19}`, `"size":20}`, 1))
		}), "names a blob of 20 bytes"},
		"an archive without artifacts": {edit(archive, func(m *Member) {
			m.Manifest = []byte(`{"blob_digest":"` + m.Blobs[0].Digest + `"}`)
			m.Blobs = m.Blobs[:1]
		}), "archive manifest"},
	} {
		t.Run(name, func(t *testing.T) {
			registry := newFakeRegistry(t)
			_, err := Publish(context.Background(), testClient(t), registry.server.URL, testToken, tc.member)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Publish = %v, want a refusal naming %q", err, tc.want)
			}
			if count := registry.requestCount(); count != 0 {
				t.Fatalf("a refused member sent %d requests", count)
			}
		})
	}
}

func TestPublishSendsOnlyTheBlobBytesItWasGiven(t *testing.T) {
	for name, tc := range map[string]struct {
		data []byte
		err  error
	}{
		"other bytes":        {data: []byte("site content bytez")},
		"fewer bytes":        {data: []byte("site content")},
		"an unreadable blob": {err: errors.New("the file moved")},
	} {
		t.Run(name, func(t *testing.T) {
			registry := newFakeRegistry(t)
			member := members()[distribution.KindDoc]
			member.Blobs[0].Read = func() ([]byte, error) { return tc.data, tc.err }
			if _, err := Publish(context.Background(), testClient(t), registry.server.URL, testToken, member); err == nil {
				t.Fatal("blob bytes that are not the blob were accepted")
			}
			if count := registry.requestCount(); count != 0 {
				t.Fatalf("%d requests were sent for bytes that are not the blob", count)
			}
		})
	}
}

func TestPublishRefusesAnAnswerThatNamesAnotherVersion(t *testing.T) {
	member := members()[distribution.KindDoc]
	otherDigest := "sha256:" + strings.Repeat("a", 64)
	for name, tc := range map[string]struct {
		answer func(w http.ResponseWriter, r *http.Request) bool
		want   string
	}{
		"a receipt of other bytes": {func(w http.ResponseWriter, r *http.Request) bool {
			if !strings.HasSuffix(r.URL.Path, "/blobs") {
				return false
			}
			writeJSON(w, http.StatusCreated, put.BlobReceipt{ID: "b", Digest: otherDigest, Size: 18, MediaType: put.GzipBlobMediaType, CreatedAt: "2026-10-02T10:00:00Z"})
			return true
		}, "another blob"},
		"a receipt with an unknown field": {func(w http.ResponseWriter, r *http.Request) bool {
			if !strings.HasSuffix(r.URL.Path, "/blobs") {
				return false
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"id":"b","digest":%q,"size":18,"media_type":"application/gzip","created_at":"2026-10-02T10:00:00Z","extra":1}`, member.Blobs[0].Digest)
			return true
		}, "invalid blob upload response"},
		"a blob upload refused": {func(w http.ResponseWriter, r *http.Request) bool {
			w.WriteHeader(http.StatusForbidden)
			return true
		}, "blob upload returned 403"},
		"a publish answer that moved a channel": {func(w http.ResponseWriter, r *http.Request) bool {
			if !strings.HasSuffix(r.URL.Path, "/publish") {
				return false
			}
			writeJSON(w, http.StatusCreated, put.PublishResponse{
				Package:  member.Coordinate,
				Version:  put.Version{ID: "v", PackageID: "p", Version: member.Version, ManifestID: "m", State: put.VersionStatePublished, Visibility: put.VisibilityPrivate, CreatedAt: "2026-10-02T10:00:00Z"},
				Manifest: put.Manifest{ID: "m", PackageID: "p", MediaType: member.MediaType, Payload: member.Manifest, CreatedAt: "2026-10-02T10:00:00Z"},
				Channel:  json.RawMessage(`{"name":"latest"}`),
			})
			return true
		}, "invalid publish response"},
		"a publish answer that stored another payload": {func(w http.ResponseWriter, r *http.Request) bool {
			if !strings.HasSuffix(r.URL.Path, "/publish") {
				return false
			}
			writeJSON(w, http.StatusCreated, put.PublishResponse{
				Package:  member.Coordinate,
				Version:  put.Version{ID: "v", PackageID: "p", Version: member.Version, ManifestID: "m", State: put.VersionStatePublished, Visibility: put.VisibilityPrivate, CreatedAt: "2026-10-02T10:00:00Z"},
				Manifest: put.Manifest{ID: "m", PackageID: "p", MediaType: member.MediaType, Payload: json.RawMessage(`{}`), CreatedAt: "2026-10-02T10:00:00Z"},
				Channel:  json.RawMessage("null"),
			})
			return true
		}, "another version"},
		"a conflict whose manifest cannot be read": {func(w http.ResponseWriter, r *http.Request) bool {
			switch {
			case strings.HasSuffix(r.URL.Path, "/publish"):
				w.WriteHeader(http.StatusConflict)
			case strings.HasSuffix(r.URL.Path, "/manifest"):
				w.WriteHeader(http.StatusNotFound)
			default:
				return false
			}
			return true
		}, "manifest read returned 404"},
		"a publish refused": {func(w http.ResponseWriter, r *http.Request) bool {
			if !strings.HasSuffix(r.URL.Path, "/publish") {
				return false
			}
			w.WriteHeader(http.StatusBadRequest)
			return true
		}, "publish returned 400"},
		"an answer above the bound": {func(w http.ResponseWriter, r *http.Request) bool {
			if !strings.HasSuffix(r.URL.Path, "/publish") {
				return false
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(strings.Repeat(" ", MaxResponseBytes+1)))
			return true
		}, "exceeds"},
		"a redirect": {func(w http.ResponseWriter, r *http.Request) bool {
			http.Redirect(w, r, "http://127.0.0.1:1/elsewhere", http.StatusTemporaryRedirect)
			return true
		}, "returned 307"},
	} {
		t.Run(name, func(t *testing.T) {
			registry := newFakeRegistry(t)
			registry.answer = tc.answer
			if _, err := Publish(context.Background(), testClient(t), registry.server.URL, testToken, member); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Publish = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

func TestPublishSendsTheBearerAndReadsTheVersionManifest(t *testing.T) {
	registry := newFakeRegistry(t)
	member := members()[distribution.KindDoc]
	if _, err := Publish(context.Background(), testClient(t), registry.server.URL, "another-token", member); err == nil ||
		!strings.Contains(err.Error(), "returned 401") {
		t.Fatalf("a refused bearer was accepted: %v", err)
	}
	if _, err := Publish(context.Background(), testClient(t), registry.server.URL, testToken, member); err != nil {
		t.Fatal(err)
	}
	if _, _, err := PublishManifest(context.Background(), testClient(t), registry.server.URL, testToken, member); err != nil {
		t.Fatal(err)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	last := registry.requests[len(registry.requests)-1]
	if last != "GET /acme/doc-contents/versions/1.0.0+build.7/manifest" {
		t.Fatalf("the manifest read was %q", last)
	}
	if _, _, err := PublishManifest(context.Background(), nil, registry.server.URL, testToken, member); err == nil {
		t.Fatal("a publish without a client was accepted")
	}
}

// Every call that sends a request checks its registry URL first: a URL that
// ValidateRegistryURL refuses sends nothing, and a trailing slash is dropped
// before the path is joined.
func TestEveryCallValidatesTheRegistryURLBeforeAnyRequest(t *testing.T) {
	member := members()[distribution.KindDoc]
	calls := map[string]func(registryURL string) error{
		"Publish": func(registryURL string) error {
			_, err := Publish(context.Background(), testClient(t), registryURL, testToken, member)
			return err
		},
		"UploadBlob": func(registryURL string) error {
			return UploadBlob(context.Background(), testClient(t), registryURL, testToken, member.Coordinate, member.Blobs[0])
		},
		"PublishManifest": func(registryURL string) error {
			_, _, err := PublishManifest(context.Background(), testClient(t), registryURL, testToken, member)
			return err
		},
		"ReadManifest": func(registryURL string) error {
			_, err := ReadManifest(context.Background(), testClient(t), registryURL, testToken, member.Coordinate, member.Version)
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			registry := newFakeRegistry(t)
			for _, refused := range []string{
				strings.Replace(registry.server.URL, "http://", "http://publisher:secret@", 1),
				registry.server.URL + "?secret",
				registry.server.URL + "#secret",
				strings.Replace(registry.server.URL, "http://", "ftp://", 1),
			} {
				if err := call(refused); err == nil || strings.Contains(err.Error(), "secret") {
					t.Errorf("%s(%q) = %v; want a refusal that does not print the URL", name, refused, err)
				}
			}
			if count := registry.requestCount(); count != 0 {
				t.Fatalf("a refused registry URL sent %d requests", count)
			}
		})
	}
	registry := newFakeRegistry(t)
	if _, err := Publish(context.Background(), testClient(t), registry.server.URL+"/", testToken, member); err != nil {
		t.Fatalf("a registry URL with a trailing slash was refused: %v", err)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for _, request := range registry.requests {
		if strings.Contains(request, "//") {
			t.Fatalf("request %q keeps the trailing slash of the registry URL", request)
		}
	}
}

func TestValidateRegistryURL(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"https://put.putnami.dev", "https://put.putnami.dev"},
		{" HTTPS://put.example.com/put/ ", "https://put.example.com/put"},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"http://registry.localhost", "http://registry.localhost"},
	} {
		got, err := ValidateRegistryURL(tc.raw)
		if err != nil || got != tc.want {
			t.Errorf("ValidateRegistryURL(%q) = %q, %v; want %q", tc.raw, got, err, tc.want)
		}
	}
	for _, raw := range []string{
		"", "put.putnami.dev", "http://put.putnami.dev", "ftp://put.putnami.dev",
		"https://user:secret@put.putnami.dev", "https://put.putnami.dev?token=x", "https://put.putnami.dev#x",
		"https://put.putnami.dev?", "mailto:put@putnami.dev",
	} {
		if got, err := ValidateRegistryURL(raw); err == nil {
			t.Errorf("ValidateRegistryURL(%q) = %q, want a refusal", raw, got)
		} else if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "token=") {
			t.Errorf("ValidateRegistryURL(%q) error carries the URL: %v", raw, err)
		}
	}
}

// testNow is the time an HTTP-date Retry-After is read against.
var testNow = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

// recordWaits makes retryPolicy record each wait instead of sleeping, with no
// jitter and testNow as the current time, until t ends.
func recordWaits(t *testing.T) *[]time.Duration {
	t.Helper()
	saved := retryPolicy
	t.Cleanup(func() { retryPolicy = saved })
	waits := &[]time.Duration{}
	retryPolicy.Jitter = func(time.Duration) time.Duration { return 0 }
	retryPolicy.Now = func() time.Time { return testNow }
	retryPolicy.Wait = func(ctx context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		return ctx.Err()
	}
	return waits
}

// failBlobUploads answers the first count blob uploads, or every one when
// count is negative, with fault, and hands the others to the registry. It
// returns the bytes of every blob upload the registry received.
func failBlobUploads(registry *fakeRegistry, count int, fault func(w http.ResponseWriter)) *[][]byte {
	sent := &[][]byte{}
	registry.answer = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/blobs") {
			return false
		}
		data, _ := io.ReadAll(r.Body)
		*sent = append(*sent, data)
		if count < 0 || len(*sent) <= count {
			fault(w)
			return true
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		return false
	}
	return sent
}

// answerStatus answers status with the header pairs given and a body that
// echoes the bearer.
func answerStatus(status int, header ...string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		for index := 0; index+1 < len(header); index += 2 {
			w.Header().Set(header[index], header[index+1])
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"Bearer ` + testToken + `"}`))
	}
}

// silentListener accepts connections and never writes to them, until t ends.
// It returns its address.
func silentListener(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var accepted []net.Conn
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range accepted {
			_ = conn.Close()
		}
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			accepted = append(accepted, conn)
			mu.Unlock()
		}
	}()
	return listener.Addr().String()
}

// resetBeforeAnswer closes the connection without a response.
func resetBeforeAnswer(w http.ResponseWriter) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		panic("the test server cannot hijack a connection")
	}
	conn, _, err := hijacker.Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

func TestUploadBlobRetriesATransientFailure(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "publication-outbox", "a-blob-upload-retries-a-transient-failure")
	blob := members()[distribution.KindDoc].Blobs[0]
	for name, fault := range map[string]func(w http.ResponseWriter){
		"408":                answerStatus(http.StatusRequestTimeout),
		"429":                answerStatus(http.StatusTooManyRequests),
		"500":                answerStatus(http.StatusInternalServerError),
		"502":                answerStatus(http.StatusBadGateway),
		"503":                answerStatus(http.StatusServiceUnavailable),
		"504":                answerStatus(http.StatusGatewayTimeout),
		"a connection reset": resetBeforeAnswer,
	} {
		t.Run(name, func(t *testing.T) {
			waits := recordWaits(t)
			registry := newFakeRegistry(t)
			sent := failBlobUploads(registry, 1, fault)
			if err := UploadBlob(context.Background(), testClient(t), registry.server.URL, testToken, "acme/doc-contents", blob); err != nil {
				t.Fatalf("UploadBlob = %v, want the second attempt stored", err)
			}
			if len(*sent) != 2 || !bytes.Equal((*sent)[0], (*sent)[1]) || put.Digest((*sent)[1]) != blob.Digest {
				t.Fatalf("the registry received %d uploads, want the blob bytes twice", len(*sent))
			}
			if fmt.Sprint(*waits) != "[1s]" {
				t.Fatalf("waits = %v, want [1s]", *waits)
			}
		})
	}
}

func TestUploadBlobHonorsRetryAfter(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "publication-outbox", "a-blob-upload-retries-a-transient-failure")
	blob := members()[distribution.KindDoc].Blobs[0]
	for retryAfter, want := range map[string]string{
		"7": "[7s]", "0": "[1s]", "30": "[30s]",
		testNow.Add(5 * time.Second).Format(http.TimeFormat): "[5s]",
	} {
		t.Run(retryAfter, func(t *testing.T) {
			waits := recordWaits(t)
			registry := newFakeRegistry(t)
			sent := failBlobUploads(registry, 1, answerStatus(http.StatusServiceUnavailable, "Retry-After", retryAfter))
			if err := UploadBlob(context.Background(), testClient(t), registry.server.URL, testToken, "acme/doc-contents", blob); err != nil {
				t.Fatal(err)
			}
			if len(*sent) != 2 || fmt.Sprint(*waits) != want {
				t.Fatalf("%d uploads waited %v, want 2 uploads that waited %s", len(*sent), *waits, want)
			}
		})
	}
}

func TestUploadBlobStopsWhenRetryAfterAsksForMoreThanTheCap(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "publication-outbox", "a-blob-upload-retries-a-transient-failure")
	blob := members()[distribution.KindDoc].Blobs[0]
	for retryAfter, asked := range map[string]string{
		"3600":                 "1h0m0s",
		"99999999999999999999": "99999999999999999999s",
		testNow.Add(time.Hour).Format(http.TimeFormat): "1h0m0s",
	} {
		t.Run(retryAfter, func(t *testing.T) {
			waits := recordWaits(t)
			registry := newFakeRegistry(t)
			sent := failBlobUploads(registry, 1, answerStatus(http.StatusServiceUnavailable, "Retry-After", retryAfter))
			err := UploadBlob(context.Background(), testClient(t), registry.server.URL, testToken, "acme/doc-contents", blob)
			if want := "blob upload returned 503; the registry asked to wait " + asked + ", more than 30s"; err == nil || err.Error() != want {
				t.Fatalf("UploadBlob = %v, want %q", err, want)
			}
			if len(*sent) != 1 || len(*waits) != 0 {
				t.Fatalf("%d uploads waited %v, want one upload and no wait", len(*sent), *waits)
			}
		})
	}
}

func TestUploadBlobRetriesATLSHandshakeTimeout(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "publication-outbox", "a-blob-upload-retries-a-transient-failure")
	blob := members()[distribution.KindDoc].Blobs[0]
	waits := recordWaits(t)
	client := &http.Client{Transport: &http.Transport{TLSHandshakeTimeout: 50 * time.Millisecond}}
	err := UploadBlob(context.Background(), client, "https://"+silentListener(t), testToken, "acme/doc-contents", blob)
	if want := "blob upload: the registry did not answer after 4 attempts"; err == nil || err.Error() != want {
		t.Fatalf("UploadBlob = %v, want %q", err, want)
	}
	if fmt.Sprint(*waits) != "[1s 2s 4s]" {
		t.Fatalf("waits = %v, want [1s 2s 4s]", *waits)
	}
}

func TestUploadBlobDoesNotRetryAClientTimeout(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "publication-outbox", "a-blob-upload-retries-a-transient-failure")
	blob := members()[distribution.KindDoc].Blobs[0]
	waits := recordWaits(t)
	registry := newFakeRegistry(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var sent atomic.Int32
	registry.answer = func(http.ResponseWriter, *http.Request) bool {
		if sent.Add(1) == 1 {
			return false
		}
		<-release
		return true
	}
	// The first upload is answered, so the timed upload reuses its connection.
	client := testClient(t)
	if err := UploadBlob(context.Background(), client, registry.server.URL, testToken, "acme/doc-contents", blob); err != nil {
		t.Fatal(err)
	}
	timed := *client
	timed.Timeout = 100 * time.Millisecond
	err := UploadBlob(context.Background(), &timed, registry.server.URL, testToken, "acme/doc-contents", blob)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("UploadBlob = %v, want the client timeout", err)
	}
	if sent.Load() != 2 || len(*waits) != 0 {
		t.Fatalf("%d uploads waited %v, want one timed upload and no wait", sent.Load(), *waits)
	}
}

func TestUploadBlobFailsAfterTheAttemptBound(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "publication-outbox", "a-blob-upload-retries-a-transient-failure")
	blob := members()[distribution.KindDoc].Blobs[0]
	for name, tc := range map[string]struct {
		fault func(w http.ResponseWriter)
		want  string
	}{
		"a persistent 502":   {answerStatus(http.StatusBadGateway), "blob upload returned 502 after 4 attempts"},
		"a persistent reset": {resetBeforeAnswer, "blob upload: the registry did not answer after 4 attempts"},
	} {
		t.Run(name, func(t *testing.T) {
			waits := recordWaits(t)
			registry := newFakeRegistry(t)
			sent := failBlobUploads(registry, -1, tc.fault)
			err := UploadBlob(context.Background(), testClient(t), registry.server.URL, testToken, "acme/doc-contents", blob)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("UploadBlob = %v, want %q", err, tc.want)
			}
			if len(*sent) != 4 || fmt.Sprint(*waits) != "[1s 2s 4s]" {
				t.Fatalf("%d uploads waited %v, want 4 uploads that waited [1s 2s 4s]", len(*sent), *waits)
			}
		})
	}
}

func TestUploadBlobFailsAtOnceOnAnotherStatus(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "publication-outbox", "a-blob-upload-retries-a-transient-failure")
	blob := members()[distribution.KindDoc].Blobs[0]
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict, http.StatusNotImplemented} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			waits := recordWaits(t)
			registry := newFakeRegistry(t)
			sent := failBlobUploads(registry, -1, answerStatus(status))
			err := UploadBlob(context.Background(), testClient(t), registry.server.URL, testToken, "acme/doc-contents", blob)
			if want := fmt.Sprintf("blob upload returned %d", status); err == nil || err.Error() != want {
				t.Fatalf("UploadBlob = %v, want %q", err, want)
			}
			if len(*sent) != 1 || len(*waits) != 0 {
				t.Fatalf("%d uploads waited %v, want one upload and no wait", len(*sent), *waits)
			}
		})
	}
}

func TestUploadBlobStopsWhenTheContextEnds(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "publication-outbox", "a-blob-upload-retries-a-transient-failure")
	blob := members()[distribution.KindDoc].Blobs[0]
	t.Run("during the wait", func(t *testing.T) {
		recordWaits(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		retryPolicy.Wait = func(ctx context.Context, _ time.Duration) error {
			cancel()
			return ctx.Err()
		}
		registry := newFakeRegistry(t)
		sent := failBlobUploads(registry, -1, answerStatus(http.StatusBadGateway))
		if err := UploadBlob(ctx, testClient(t), registry.server.URL, testToken, "acme/doc-contents", blob); !errors.Is(err, context.Canceled) {
			t.Fatalf("UploadBlob = %v, want context.Canceled", err)
		}
		if len(*sent) != 1 {
			t.Fatalf("%d uploads were sent, want 1", len(*sent))
		}
	})
	t.Run("while the answer arrives", func(t *testing.T) {
		waits := recordWaits(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		registry := newFakeRegistry(t)
		sent := failBlobUploads(registry, -1, func(w http.ResponseWriter) {
			cancel()
			answerStatus(http.StatusBadGateway)(w)
		})
		// The client may see the answer or the cancellation first; either way
		// nothing is sent again.
		if err := UploadBlob(ctx, testClient(t), registry.server.URL, testToken, "acme/doc-contents", blob); err == nil {
			t.Fatal("UploadBlob succeeded after its context ended")
		}
		if len(*sent) != 1 || len(*waits) != 0 {
			t.Fatalf("%d uploads waited %v, want one upload and no wait", len(*sent), *waits)
		}
	})
}

// A publish whose answer is lost after the registry stored the version is
// sent again, answers 409, and is accepted as the same manifest.
func TestPublishManifestRetriesATransientFailure(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "publication-outbox", "a-publish-retries-a-transient-failure")
	member := members()[distribution.KindDoc]
	for name, stored := range map[string]bool{"the registry stored nothing": false, "the registry stored the version": true} {
		t.Run(name, func(t *testing.T) {
			waits := recordWaits(t)
			registry := newFakeRegistry(t)
			publishes := 0
			registry.answer = func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.HasSuffix(r.URL.Path, "/publish") {
					return false
				}
				publishes++
				if publishes > 1 {
					return false
				}
				if stored {
					registry.publish(httptest.NewRecorder(), r, member.Coordinate)
				}
				w.WriteHeader(http.StatusBadGateway)
				return true
			}
			published, err := Publish(context.Background(), testClient(t), registry.server.URL, testToken, member)
			if err != nil {
				t.Fatal(err)
			}
			if publishes != 2 || published.Digest != put.Digest(member.Manifest) || published.Reused != stored || len(*waits) != 1 {
				t.Fatalf("%d publishes answered %+v after %v, want 2 publishes and a reuse only when the version was stored", publishes, published, *waits)
			}
		})
	}
	waits := recordWaits(t)
	registry := newFakeRegistry(t)
	registry.answer = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/publish") {
			return false
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	}
	if _, err := Publish(context.Background(), testClient(t), registry.server.URL, testToken, member); err == nil ||
		err.Error() != "publish returned 503 after 4 attempts" || len(*waits) != 3 {
		t.Fatalf("Publish = %v after %v, want 503 after 4 attempts", err, *waits)
	}
	t.Run("the read-back after a 409", func(t *testing.T) {
		waits := recordWaits(t)
		registry := newFakeRegistry(t)
		if _, err := Publish(context.Background(), testClient(t), registry.server.URL, testToken, member); err != nil {
			t.Fatal(err)
		}
		reads := 0
		registry.answer = func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method != http.MethodGet {
				return false
			}
			reads++
			if reads > 1 {
				return false
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		published, err := Publish(context.Background(), testClient(t), registry.server.URL, testToken, member)
		if err != nil {
			t.Fatal(err)
		}
		if reads != 2 || !published.Reused || fmt.Sprint(*waits) != "[1s]" {
			t.Fatalf("%d reads answered %+v after %v, want 2 reads and a reuse", reads, published, *waits)
		}
	})
}
