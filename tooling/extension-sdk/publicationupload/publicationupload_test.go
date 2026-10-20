package publicationupload_test

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
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"

	distribution "go.putnami.dev/protocol/distribution"
	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	put "go.putnami.dev/protocol/put"
	"go.putnami.dev/sdk/extension/npmpublish"
	"go.putnami.dev/sdk/extension/oci"
	"go.putnami.dev/sdk/extension/publicationoutbox"
	"go.putnami.dev/sdk/extension/publicationupload"
	"go.putnami.dev/sdk/extension/publicationupload/publicationuploadtest"
)

const (
	feature     = "tooling/extension-authoring"
	requirement = "upload-kit-uploads-what-the-engine-verified"
)

// testBearer holds characters a URL escapes, so an echo of its query escaping
// is caught too.
const testBearer = "kit+s3cret/bearer="

// bearers answers testBearer for every target and records the targets.
type bearers struct {
	mu      sync.Mutex
	targets []string
}

func (b *bearers) get(_ context.Context, target *url.URL) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.targets = append(b.targets, target.String())
	return testBearer, nil
}

func (b *bearers) asked() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.targets)
}

// packed is one member staged in its own outbox, the request that uploads it
// and what the registry stores for it.
type packed struct {
	request publicationupload.Request
	want    publicationupload.Result
}

// path is the absolute path of the outbox file rel names.
func (p packed) path(rel string) string {
	return filepath.Join(p.request.Outbox, filepath.FromSlash(rel))
}

// stage writes the member write returns into a new outbox and returns the
// outbox root and the member.
func stage(t *testing.T, write func(*publicationoutbox.Writer) extproto.OutboxMember) (string, extproto.OutboxMember) {
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
	return root, member
}

func writeFile(t *testing.T, writer *publicationoutbox.Writer, rel string, data []byte) extproto.OutboxFile {
	t.Helper()
	file, err := writer.WriteFile(rel, data)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

// npmMember is @acme/web@1.2.3 packed for the npm registry at endpoint.
func npmMember(t *testing.T, endpoint, content string) packed {
	t.Helper()
	tarball := []byte("tarball of @acme/web@1.2.3" + content)
	root, member := stage(t, func(w *publicationoutbox.Writer) extproto.OutboxMember {
		return extproto.OutboxMember{
			Ecosystem: extproto.OutboxEcosystemNPM, Coordinate: "@acme/web", Version: "1.2.3", Project: "/web",
			NPM: &extproto.OutboxNPM{
				Registry: endpoint, Tarball: writeFile(t, w, "npm/web.tgz", tarball),
				Manifest: writeFile(t, w, "npm/package.json", []byte(`{"name":"@acme/web","version":"1.2.3"}`)),
			},
		}
	})
	return packed{
		request: publicationupload.Request{Outbox: root, Member: member, Registry: endpoint},
		want:    publicationupload.Result{Digest: npmpublish.Digest(tarball)},
	}
}

// goMember is example.test/mod@v1.2.3 packed for the Go module origin at
// endpoint.
func goMember(t *testing.T, endpoint string) packed {
	t.Helper()
	zip := []byte("module zip of example.test/mod@v1.2.3")
	root, member := stage(t, func(w *publicationoutbox.Writer) extproto.OutboxMember {
		return extproto.OutboxMember{
			Ecosystem: extproto.OutboxEcosystemGo, Coordinate: "example.test/mod", Version: "v1.2.3", Project: "/mod",
			Go: &extproto.OutboxGo{
				Zip:  writeFile(t, w, "go/mod.zip", zip),
				Mod:  writeFile(t, w, "go/go.mod", []byte("module example.test/mod\n")),
				Info: writeFile(t, w, "go/mod.info", []byte(`{"Version":"v1.2.3"}`)),
			},
		}
	})
	return packed{
		request: publicationupload.Request{Outbox: root, Member: member, Registry: endpoint},
		want:    publicationupload.Result{Digest: npmpublish.Digest(zip)},
	}
}

// archiveMember is a release archive of two platforms packed for the Put
// registry at endpoint, with variant appended to its darwin archive.
func archiveMember(t *testing.T, endpoint, variant string) packed {
	t.Helper()
	linux, darwin := []byte("linux cli archive"), []byte("darwin cli archive"+variant)
	payload := []byte(fmt.Sprintf(`{"artifacts":{"darwin-arm64":{"digest":%q,"size":%d},"linux-amd64":{"digest":%q,"size":%d}}}`,
		put.Digest(darwin), len(darwin), put.Digest(linux), len(linux)))
	root, member := stage(t, func(w *publicationoutbox.Writer) extproto.OutboxMember {
		block := &extproto.OutboxPut{MediaType: put.ArchiveManifestMediaType, Manifest: writeFile(t, w, "put/manifest.json", payload)}
		for index, blob := range []struct {
			mediaType string
			data      []byte
		}{{put.GzipBlobMediaType, linux}, {put.BinaryBlobMediaType, darwin}} {
			file := writeFile(t, w, fmt.Sprintf("put/blob-%d", index), blob.data)
			block.Blobs = append(block.Blobs, extproto.OutboxPutBlob{Path: file.Path, Digest: file.Digest, Size: file.Size, MediaType: blob.mediaType})
		}
		return extproto.OutboxMember{Ecosystem: extproto.OutboxEcosystemArchive, Coordinate: "acme/cli", Version: "0.3.0", Project: "/cli", Put: block}
	})
	return packed{
		request: publicationupload.Request{Outbox: root, Member: member, Registry: endpoint, Kind: distribution.KindArchive},
		want: publicationupload.Result{
			Digest:    put.Digest(payload),
			Platforms: map[string]string{"darwin/arm64": put.Digest(darwin), "linux/amd64": put.Digest(linux)},
		},
	}
}

// ociMember is a random image packed as host/team/app, tagged 1.2.3.
func ociMember(t *testing.T, host string) packed {
	t.Helper()
	image, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(t.TempDir(), "layout")
	digest, err := oci.WriteLayout(layout, image)
	if err != nil {
		t.Fatal(err)
	}
	root, member := stage(t, func(w *publicationoutbox.Writer) extproto.OutboxMember {
		if err := w.CopyLayout("oci/layout", layout); err != nil {
			t.Fatal(err)
		}
		return extproto.OutboxMember{
			Ecosystem: extproto.OutboxEcosystemOCI, Coordinate: "team/app", Version: "1.2.3", Project: "/app",
			OCI: &extproto.OutboxOCI{Layout: "oci/layout", Repository: host + "/team/app", Digest: digest, Tags: []string{"1.2.3"}},
		}
	})
	return packed{
		request: publicationupload.Request{Outbox: root, Member: member},
		want:    publicationupload.Result{Digest: digest},
	}
}

func hostOf(t *testing.T, server *httptest.Server) string {
	t.Helper()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return target.Host
}

func writeOver(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Each ecosystem uploads with its own uploader to the registry the request
// names, and the result is the digest the registry stores: the npm
// tarball's, the Go module zip's, the Put manifest payload's with the
// archive's platforms, and the OCI manifest's. The bearer is asked once, for
// that registry.
func TestUploadAnswersTheDigestTheRegistryStores(t *testing.T) {
	spectest.Proves(t, feature, requirement, "each-ecosystem-uploads-and-answers-the-stored-digest")
	npm := publicationuploadtest.NewNPMRegistry(t, testBearer)
	gomod := publicationuploadtest.NewGoRegistry(t, testBearer)
	putRegistry := publicationuploadtest.NewPutRegistry(t, testBearer)
	ociServer := httptest.NewServer(registry.New())
	t.Cleanup(ociServer.Close)
	for name, tc := range map[string]struct {
		member packed
		target string
	}{
		"npm":     {member: npmMember(t, npm.Server.URL, ""), target: npm.Server.URL},
		"go":      {member: goMember(t, gomod.Server.URL), target: gomod.Server.URL},
		"archive": {member: archiveMember(t, putRegistry.Server.URL+"/", ""), target: putRegistry.Server.URL},
		"oci":     {member: ociMember(t, hostOf(t, ociServer)), target: "http://" + hostOf(t, ociServer)},
	} {
		t.Run(name, func(t *testing.T) {
			source := &bearers{}
			got, err := publicationupload.Upload(context.Background(), tc.member.request, source.get)
			if err != nil || got.Digest != tc.member.want.Digest || !maps.Equal(got.Platforms, tc.member.want.Platforms) {
				t.Fatalf("upload = %+v, %v; want %+v", got, err, tc.member.want)
			}
			if asked := source.asked(); !slices.Equal(asked, []string{tc.target}) {
				t.Fatalf("the bearer was asked for %v, want %s once", asked, tc.target)
			}
		})
	}
	if tarball, _ := npm.Stored("@acme/web", "1.2.3"); string(tarball) != "tarball of @acme/web@1.2.3" {
		t.Fatalf("the npm registry stores %q", tarball)
	}
	if zip, _ := gomod.Stored("example.test/mod", "v1.2.3"); string(zip) != "module zip of example.test/mod@v1.2.3" {
		t.Fatalf("the Go registry stores %q", zip)
	}
	if _, publishes := putRegistry.Recorded(); len(publishes) != 1 {
		t.Fatalf("the Put registry received %d publishes", len(publishes))
	}
}

// Upload reads the files the request's member names, never outbox.json: a
// descriptor rewritten to name other bytes for the same member uploads the
// member's own bytes.
func TestUploadReadsTheRequestMemberNotTheDescriptor(t *testing.T) {
	spectest.Proves(t, feature, requirement, "the-request-member-names-every-file-read")
	npm := publicationuploadtest.NewNPMRegistry(t, testBearer)
	member := npmMember(t, npm.Server.URL, "")
	other := []byte("other tarball")
	writeOver(t, member.path("npm/other.tgz"), other)
	rewritten := member.request.Member
	rewritten.NPM = &extproto.OutboxNPM{
		Registry: npm.Server.URL, Manifest: rewritten.NPM.Manifest,
		Tarball: extproto.OutboxFile{Path: "npm/other.tgz", Digest: npmpublish.Digest(other), Size: int64(len(other))},
	}
	descriptor, err := json.Marshal(extproto.PublicationOutbox{ProtocolVersion: extproto.PublicationOutboxVersion, Members: []extproto.OutboxMember{rewritten}})
	if err != nil {
		t.Fatal(err)
	}
	writeOver(t, member.path(extproto.PublicationOutboxDescriptor), descriptor)

	got, err := publicationupload.Upload(context.Background(), member.request, (&bearers{}).get)
	if err != nil || got.Digest != member.want.Digest {
		t.Fatalf("upload = %+v, %v; want %s", got, err, member.want.Digest)
	}
	if tarball, _ := npm.Stored("@acme/web", "1.2.3"); string(tarball) != "tarball of @acme/web@1.2.3" {
		t.Fatalf("the registry stores %q, not the member's tarball", tarball)
	}
}

// A version the registry already holds at the member's digest is reused; one
// it holds at another digest is refused.
func TestUploadReusesAVersionHeldAtTheSameDigest(t *testing.T) {
	spectest.Proves(t, feature, requirement, "a-version-held-at-the-same-digest-is-reused")
	ctx := context.Background()
	npm := publicationuploadtest.NewNPMRegistry(t, testBearer)
	putRegistry := publicationuploadtest.NewPutRegistry(t, testBearer)
	ociServer := httptest.NewServer(registry.New())
	t.Cleanup(ociServer.Close)
	image := ociMember(t, hostOf(t, ociServer))
	for name, tc := range map[string]struct {
		member, another packed
	}{
		"npm":     {member: npmMember(t, npm.Server.URL, ""), another: npmMember(t, npm.Server.URL, " changed")},
		"archive": {member: archiveMember(t, putRegistry.Server.URL, ""), another: archiveMember(t, putRegistry.Server.URL, " changed")},
		"oci":     {member: image, another: image},
	} {
		t.Run(name, func(t *testing.T) {
			for attempt := range 2 {
				got, err := publicationupload.Upload(ctx, tc.member.request, (&bearers{}).get)
				if err != nil || got.Digest != tc.member.want.Digest {
					t.Fatalf("upload %d = %+v, %v; want %s", attempt, got, err, tc.member.want.Digest)
				}
			}
			if tc.another.want.Digest == tc.member.want.Digest {
				return
			}
			if got, err := publicationupload.Upload(ctx, tc.another.request, (&bearers{}).get); err == nil {
				t.Fatalf("a version held at another digest uploaded as %+v", got)
			}
		})
	}
}

// countingServer is a registry that answers 500 to every request and counts
// them.
func countingServer(t *testing.T) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	return server, func() int {
		mu.Lock()
		defer mu.Unlock()
		return requests
	}
}

// An artifact whose size or digest is not its entry's is refused before any
// request reaches the registry. An npm, Go or Put member is refused before
// the bearer is asked; oci.PushLayout hashes an OCI manifest after the bearer
// is asked and before its first request.
func TestUploadRefusesATamperedArtifactBeforeAnyRequest(t *testing.T) {
	spectest.Proves(t, feature, requirement, "a-tampered-artifact-is-refused-before-any-request")
	for name, tc := range map[string]struct {
		member  func(server *httptest.Server) packed
		tamper  func(t *testing.T, member packed)
		refusal string
		asked   int
	}{
		"an npm tarball of the same size": {
			member: func(server *httptest.Server) packed { return npmMember(t, server.URL, "") },
			tamper: func(t *testing.T, member packed) {
				writeOver(t, member.path("npm/web.tgz"), []byte(strings.ToUpper("tarball of @acme/web@1.2.3")))
			},
			refusal: "digest mismatch",
		},
		"a longer npm manifest": {
			member: func(server *httptest.Server) packed { return npmMember(t, server.URL, "") },
			tamper: func(t *testing.T, member packed) {
				writeOver(t, member.path("npm/package.json"), []byte(`{"name":"@acme/web","version":"1.2.3","x":1}`))
			},
			refusal: "size mismatch",
		},
		"a Go module zip": {
			member: func(server *httptest.Server) packed { return goMember(t, server.URL) },
			tamper: func(t *testing.T, member packed) {
				writeOver(t, member.path("go/mod.zip"), []byte(strings.ToUpper("module zip of example.test/mod@v1.2.3")))
			},
			refusal: "digest mismatch",
		},
		"the second blob of an archive": {
			member: func(server *httptest.Server) packed { return archiveMember(t, server.URL, "") },
			tamper: func(t *testing.T, member packed) {
				writeOver(t, member.path("put/blob-1"), []byte("DARWIN CLI ARCHIVE"))
			},
			refusal: "digest mismatch",
		},
		"an archive manifest": {
			member: func(server *httptest.Server) packed { return archiveMember(t, server.URL, "") },
			tamper: func(t *testing.T, member packed) {
				writeOver(t, member.path("put/manifest.json"), []byte(`{}`))
			},
			refusal: "size mismatch",
		},
		"an OCI manifest": {
			member: func(server *httptest.Server) packed { return ociMember(t, hostOf(t, server)) },
			tamper: func(t *testing.T, member packed) {
				digest := strings.TrimPrefix(member.request.Member.OCI.Digest, "sha256:")
				path := member.path("oci/layout/blobs/sha256/" + digest)
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				writeOver(t, path, append(data, '\n'))
			},
			refusal: ociManifestRefusal,
			asked:   1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			server, requests := countingServer(t)
			member := tc.member(server)
			tc.tamper(t, member)
			source := &bearers{}
			got, err := publicationupload.Upload(context.Background(), member.request, source.get)
			if err == nil || !strings.Contains(err.Error(), tc.refusal) {
				t.Fatalf("upload of a tampered artifact = %+v, %v; want a refusal naming %q", got, err, tc.refusal)
			}
			if count := requests(); count != 0 {
				t.Fatalf("the registry received %d requests", count)
			}
			if asked := source.asked(); len(asked) != tc.asked {
				t.Fatalf("the bearer was asked for %v", asked)
			}
		})
	}
}

// ociManifestRefusal is what oci.PushLayout says of a layout manifest whose
// bytes do not hash to the member's digest.
const ociManifestRefusal = "does not match expected digest"

// A member the outbox contract refuses, a request without a bearer source and
// an outbox that is not a directory are refused before any file is read and
// before the bearer is asked.
func TestUploadRefusesAMemberTheContractRefuses(t *testing.T) {
	spectest.Proves(t, feature, requirement, "a-tampered-artifact-is-refused-before-any-request")
	server, requests := countingServer(t)
	valid := npmMember(t, server.URL, "")
	withoutBlock := valid.request
	withoutBlock.Member.NPM = nil
	twoBlocks := valid.request
	twoBlocks.Member.Go = &extproto.OutboxGo{}
	unknown := valid.request
	unknown.Member.Ecosystem = "pypi"
	escaping := valid.request
	escaping.Member.NPM = &extproto.OutboxNPM{Registry: server.URL, Tarball: extproto.OutboxFile{Path: "../web.tgz", Digest: valid.request.Member.NPM.Tarball.Digest, Size: 1}, Manifest: valid.request.Member.NPM.Manifest}
	notADirectory := valid.request
	notADirectory.Outbox = valid.path(extproto.PublicationOutboxDescriptor)
	for name, tc := range map[string]struct {
		request publicationupload.Request
		refusal string
	}{
		"an npm member without its block":   {request: withoutBlock, refusal: "an npm member requires the npm block"},
		"a member with two blocks":          {request: twoBlocks, refusal: "member carries 2 ecosystem blocks"},
		"an unknown ecosystem":              {request: unknown, refusal: `ecosystem "pypi" is not`},
		"a path that leaves the outbox":     {request: escaping, refusal: "invalid publication member"},
		"an outbox that is not a directory": {request: notADirectory, refusal: "is not a directory"},
	} {
		t.Run(name, func(t *testing.T) {
			source := &bearers{}
			if got, err := publicationupload.Upload(context.Background(), tc.request, source.get); err == nil || !strings.Contains(err.Error(), tc.refusal) {
				t.Fatalf("upload = %+v, %v; want a refusal naming %q", got, err, tc.refusal)
			}
			if asked := source.asked(); len(asked) != 0 {
				t.Fatalf("the bearer was asked for %v", asked)
			}
		})
	}
	if _, err := publicationupload.Upload(context.Background(), valid.request, nil); err == nil {
		t.Fatal("an upload without a bearer source ran")
	}
	if count := requests(); count != 0 {
		t.Fatalf("the registry received %d requests", count)
	}
}

// The bearer goes to the registry the request names and nowhere else: a
// registry URL that carries a credential or sends the bearer in cleartext off
// loopback is refused before the bearer is asked, and a registry that
// redirects every request to another host never gets the bearer there.
func TestUploadSendsTheBearerOnlyToItsRegistry(t *testing.T) {
	spectest.Proves(t, feature, requirement, "the-bearer-goes-only-to-its-registry")
	ctx := context.Background()
	t.Run("a registry URL its ecosystem refuses", func(t *testing.T) {
		for _, endpoint := range []string{"https://publisher:secret@registry.example.test", "http://registry.example.test/secret", "ftp://registry.example.test/secret"} {
			for name, member := range map[string]packed{
				"npm":     npmMember(t, "https://registry.example.test", ""),
				"go":      goMember(t, "https://registry.example.test"),
				"archive": archiveMember(t, "https://registry.example.test", ""),
			} {
				member.request.Registry = endpoint
				source := &bearers{}
				got, err := publicationupload.Upload(ctx, member.request, source.get)
				if err == nil || strings.Contains(err.Error(), "secret") {
					t.Errorf("%s upload to %s = %+v, %v; want a refusal that does not print it", name, endpoint, got, err)
				}
				if asked := source.asked(); len(asked) != 0 {
					t.Errorf("%s upload to %s asked the bearer for %v", name, endpoint, asked)
				}
			}
		}
	})

	t.Run("a registry redirects to another host", func(t *testing.T) {
		var mu sync.Mutex
		var leaked []string
		other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			if authorization := r.Header.Get("Authorization"); authorization != "" {
				leaked = append(leaked, r.Method+" "+r.URL.Path+": "+authorization)
			}
			mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(other.Close)
		redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/v2/":
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="registry"`, other.URL))
				w.WriteHeader(http.StatusUnauthorized)
			case r.Method == http.MethodHead:
				w.WriteHeader(http.StatusNotFound)
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/blobs/uploads/"):
				w.Header().Set("Location", other.URL+"/upload/session")
				w.WriteHeader(http.StatusAccepted)
			default:
				http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
			}
		}))
		t.Cleanup(redirecting.Close)
		for name, member := range map[string]packed{
			"npm":     npmMember(t, redirecting.URL, ""),
			"go":      goMember(t, redirecting.URL),
			"archive": archiveMember(t, redirecting.URL, ""),
			"oci":     ociMember(t, hostOf(t, redirecting)),
		} {
			source := &bearers{}
			if got, err := publicationupload.Upload(ctx, member.request, source.get); err == nil {
				t.Errorf("%s uploaded as %+v through a registry that redirects every request", name, got)
			}
			if asked := source.asked(); len(asked) != 1 || asked[0] != "http://"+hostOf(t, redirecting) {
				t.Errorf("%s asked the bearer for %v", name, asked)
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if len(leaked) > 0 {
			t.Fatalf("another host received the bearer: %v", leaked)
		}
	})
}

// echoBody is a registry error body that repeats the request's Authorization
// header and the bearer, raw and query-escaped.
func echoBody(r *http.Request) string {
	echo := fmt.Sprintf("authorization %s raw %s escaped %s", r.Header.Get("Authorization"), testBearer, url.QueryEscape(testBearer))
	return fmt.Sprintf(`{"errors":[{"code":"DENIED","message":%q}],"error":%q,"digest":%q}`, echo, echo, testBearer)
}

// hostileRegistry answers every upload with an error body that echoes the
// bearer. A Go or Put blob upload succeeds with the bearer in its digest, a
// Put publish answers 409, and a Put manifest read answers the bearer in an
// unknown member.
func hostileRegistry(t *testing.T) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="registry"`, server.URL))
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(echoBody(r)))
		case r.Method == http.MethodHead:
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/-/blobs/upload"):
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"digest":%q}`, "sha256:"+strings.Repeat("e", 64)+testBearer)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/blobs"):
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"id":%q,"digest":%q,"size":3,"media_type":"application/gzip","created_at":"2026-10-02T10:00:00Z"}`,
				testBearer, "sha256:"+strings.Repeat("e", 64)+testBearer)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/publish"):
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(echoBody(r)))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/manifest"):
			_, _ = fmt.Fprintf(w, `{"id":"m","package_id":"p","media_type":%q,"payload":{"echo":%q},"created_at":"2026-10-02T10:00:00Z","echo":%q}`,
				put.ArchiveManifestMediaType, testBearer, echoBody(r))
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(echoBody(r)))
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// A registry that echoes the bearer in every answer fails each upload with an
// error that carries none of it.
func TestUploadErrorsNeverCarryTheBearer(t *testing.T) {
	spectest.Proves(t, feature, requirement, "no-error-carries-the-bearer")
	hostile := hostileRegistry(t)
	for name, member := range map[string]packed{
		"npm":     npmMember(t, hostile.URL, ""),
		"go":      goMember(t, hostile.URL),
		"archive": archiveMember(t, hostile.URL, ""),
		"oci":     ociMember(t, hostOf(t, hostile)),
	} {
		got, err := publicationupload.Upload(context.Background(), member.request, (&bearers{}).get)
		if err == nil {
			t.Errorf("%s uploaded as %+v to a registry that refuses every upload", name, got)
			continue
		}
		for _, secret := range []string{testBearer, url.QueryEscape(testBearer), "s3cret"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s error carries the bearer: %v", name, err)
			}
		}
	}
}
