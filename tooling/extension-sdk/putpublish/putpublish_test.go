package putpublish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	distribution "go.putnami.dev/protocol/distribution"
	"go.putnami.dev/protocol/features/spectest"
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
		"a blob without a digest":           {edit(archive, func(m *Member) { m.Blobs[0].Digest = "sha256:x" }), "names no sha256 digest"},
		"an empty blob":                     {edit(archive, func(m *Member) { m.Blobs[0].Size = 0 }), "is empty"},
		"a repeated blob":                   {edit(archive, func(m *Member) { m.Blobs[1] = m.Blobs[0] }), "repeats"},
		"a missing blob":                    {edit(archive, func(m *Member) { m.Blobs = m.Blobs[:1] }), "must be the same blobs"},
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
