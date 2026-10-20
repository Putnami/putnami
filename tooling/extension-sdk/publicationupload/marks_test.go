package publicationupload_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/npmpublish"
	"go.putnami.dev/sdk/extension/publicationoutbox"
	"go.putnami.dev/sdk/extension/publicationupload"
	"go.putnami.dev/sdk/extension/publicationupload/publicationuploadtest"
)

const marksRequirement = "upload-kit-tells-a-mismatch-from-a-conflict"

// marks reports which of the two marks err matches.
func marks(err error) (mismatch, conflict bool) {
	return errors.Is(err, publicationupload.ErrArtifactMismatch), errors.Is(err, publicationupload.ErrArtifactConflict)
}

// goModule is example.test/mod@v1.2.3 packed for the Go module origin at
// endpoint, with zip as its module zip.
func goModule(t *testing.T, endpoint, zip string) packed {
	t.Helper()
	root, member := stage(t, func(w *publicationoutbox.Writer) extproto.OutboxMember {
		return extproto.OutboxMember{
			Ecosystem: extproto.OutboxEcosystemGo, Coordinate: "example.test/mod", Version: "v1.2.3", Project: "/mod",
			Go: &extproto.OutboxGo{
				Zip:  writeFile(t, w, "go/mod.zip", []byte(zip)),
				Mod:  writeFile(t, w, "go/go.mod", []byte("module example.test/mod\n")),
				Info: writeFile(t, w, "go/mod.info", []byte(`{"Version":"v1.2.3"}`)),
			},
		}
	})
	return packed{
		request: publicationupload.Request{Outbox: root, Member: member, Registry: endpoint},
		want:    publicationupload.Result{Digest: npmpublish.Digest([]byte(zip))},
	}
}

// immutableVersions serves the Go registry reg, except that a second version
// PUT to the same path answers 409, as a registry whose versions are
// immutable does.
func immutableVersions(t *testing.T, reg *publicationuploadtest.GoRegistry) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	published := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/@v/") {
			mu.Lock()
			again := published[r.URL.Path]
			published[r.URL.Path] = true
			mu.Unlock()
			if again {
				w.WriteHeader(http.StatusConflict)
				return
			}
		}
		reg.Server.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

// An artifact whose size or digest is not its entry's fails the upload with
// an error that matches ErrArtifactMismatch and not ErrArtifactConflict, and
// keeps the text that names the file.
func TestUploadMarksATamperedArtifactAsAMismatch(t *testing.T) {
	spectest.Proves(t, feature, marksRequirement, "a-tampered-artifact-is-a-mismatch")
	server, _ := countingServer(t)
	for name, tc := range map[string]struct {
		member packed
		path   string
		data   func(original []byte) []byte
		text   string
	}{
		"an npm tarball": {
			member: npmMember(t, server.URL, ""), path: "npm/web.tgz",
			data: func(original []byte) []byte { return []byte(strings.ToUpper(string(original))) },
			text: `outbox file "npm/web.tgz" digest mismatch`,
		},
		"a Go module zip": {
			member: goMember(t, server.URL), path: "go/mod.zip",
			data: func(original []byte) []byte { return append(original, '!') },
			text: `outbox file "go/mod.zip" size mismatch`,
		},
		"an archive blob": {
			member: archiveMember(t, server.URL, ""), path: "put/blob-0",
			data: func(original []byte) []byte { return original[:len(original)-1] },
			text: `outbox file "put/blob-0" size mismatch`,
		},
		"an OCI manifest": {
			member: ociMember(t, hostOf(t, server)),
			data:   func(original []byte) []byte { return append(original, '\n') },
			text:   ociManifestRefusal,
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := tc.path
			if path == "" {
				path = "oci/layout/blobs/sha256/" + strings.TrimPrefix(tc.member.request.Member.OCI.Digest, "sha256:")
			}
			original, err := os.ReadFile(tc.member.path(path))
			if err != nil {
				t.Fatal(err)
			}
			writeOver(t, tc.member.path(path), tc.data(original))
			_, err = publicationupload.Upload(context.Background(), tc.member.request, (&bearers{}).get)
			if mismatch, conflict := marks(err); !mismatch || conflict {
				t.Fatalf("upload error %v: mismatch %t, conflict %t; want a mismatch alone", err, mismatch, conflict)
			}
			if !strings.Contains(err.Error(), tc.text) || strings.Contains(err.Error(), publicationupload.ErrArtifactMismatch.Error()) {
				t.Fatalf("upload error %q does not keep its own text %q", err, tc.text)
			}
		})
	}
}

// A registry that already holds another artifact at the member's version
// fails the upload with an error that matches ErrArtifactConflict and not
// ErrArtifactMismatch. An OCI manifest is addressed by its digest, so an
// image has no such conflict.
func TestUploadMarksAnotherArtifactAtTheVersionAsAConflict(t *testing.T) {
	spectest.Proves(t, feature, marksRequirement, "another-artifact-at-the-version-is-a-conflict")
	npm := publicationuploadtest.NewNPMRegistry(t, testBearer)
	putRegistry := publicationuploadtest.NewPutRegistry(t, testBearer)
	goServer := immutableVersions(t, publicationuploadtest.NewGoRegistry(t, testBearer))
	for name, tc := range map[string]struct {
		first, another packed
		text           string
	}{
		"npm": {
			first: npmMember(t, npm.Server.URL, ""), another: npmMember(t, npm.Server.URL, " changed"),
			text: "published npm artifact size mismatch",
		},
		"go": {
			first: goModule(t, goServer.URL, "module zip one"), another: goModule(t, goServer.URL, "module zip two"),
			text: "downloaded module zip digest",
		},
		"archive": {
			first: archiveMember(t, putRegistry.Server.URL, ""), another: archiveMember(t, putRegistry.Server.URL, " changed"),
			text: "is already published with another manifest",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := publicationupload.Upload(context.Background(), tc.first.request, (&bearers{}).get); err != nil || got.Digest != tc.first.want.Digest {
				t.Fatalf("first upload = %+v, %v; want %s", got, err, tc.first.want.Digest)
			}
			_, err := publicationupload.Upload(context.Background(), tc.another.request, (&bearers{}).get)
			if mismatch, conflict := marks(err); !conflict || mismatch {
				t.Fatalf("upload error %v: mismatch %t, conflict %t; want a conflict alone", err, mismatch, conflict)
			}
			if !strings.Contains(err.Error(), tc.text) || strings.Contains(err.Error(), publicationupload.ErrArtifactConflict.Error()) {
				t.Fatalf("upload error %q does not keep its own text %q", err, tc.text)
			}
		})
	}
}

// A registry that fails every request, a bearer source that fails and a
// member the contract refuses fail the upload with an error that matches
// neither mark.
func TestUploadMarksNoOtherFailure(t *testing.T) {
	spectest.Proves(t, feature, marksRequirement, "any-other-failure-is-neither")
	server, _ := countingServer(t)
	refused := httptest.NewServer(registry.New())
	t.Cleanup(refused.Close)
	withoutBlock := npmMember(t, server.URL, "")
	withoutBlock.request.Member.NPM = nil
	failingSource := func(context.Context, *url.URL) (string, error) { return "", errors.New("no bearer") }
	for name, tc := range map[string]struct {
		member packed
		source publicationupload.BearerFunc
	}{
		"npm to a failing registry":        {member: npmMember(t, server.URL, ""), source: (&bearers{}).get},
		"go to a failing registry":         {member: goMember(t, server.URL), source: (&bearers{}).get},
		"archive to a failing registry":    {member: archiveMember(t, server.URL, ""), source: (&bearers{}).get},
		"oci to a failing registry":        {member: ociMember(t, hostOf(t, server)), source: (&bearers{}).get},
		"npm with a failing bearer source": {member: npmMember(t, server.URL, ""), source: failingSource},
		"oci with a failing bearer source": {member: ociMember(t, hostOf(t, refused)), source: failingSource},
		"a member the contract refuses":    {member: withoutBlock, source: (&bearers{}).get},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := publicationupload.Upload(context.Background(), tc.member.request, tc.source)
			if err == nil {
				t.Fatal("the upload succeeded")
			}
			if mismatch, conflict := marks(err); mismatch || conflict {
				t.Fatalf("upload error %v: mismatch %t, conflict %t; want neither", err, mismatch, conflict)
			}
		})
	}
}
