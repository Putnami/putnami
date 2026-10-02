package sdk

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/random"

	distribution "go.putnami.dev/protocol/distribution"
	"go.putnami.dev/protocol/features/spectest"
	put "go.putnami.dev/protocol/put"
	"go.putnami.dev/sdk/extension/gomodpublish"
	"go.putnami.dev/sdk/extension/npmpublish"
	"go.putnami.dev/sdk/extension/oci"
	"go.putnami.dev/sdk/extension/putpublish"
)

// testBearer holds characters a URL escapes, so an echo of its query escaping
// is caught too.
const testBearer = "s3cret+bearer/value="

// echoBody returns a registry error body that repeats the request's
// Authorization header and the bearer, raw and query-escaped.
func echoBody(r *http.Request) string {
	echo := fmt.Sprintf("authorization %s raw %s escaped %s", r.Header.Get("Authorization"), testBearer, url.QueryEscape(testBearer))
	return fmt.Sprintf(`{"errors":[{"code":"DENIED","message":%q}],"error":%q,"digest":%q}`, echo, echo, testBearer)
}

// hostileRegistry answers every upload with an error body that echoes the
// bearer. A gomod or Put blob upload succeeds with the bearer in its digest, a
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
				put.MigrationManifestMediaType, testBearer, echoBody(r))
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(echoBody(r)))
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// recordingReporter keeps every diagnostic and log line of a Go upload.
type recordingReporter struct {
	mu    sync.Mutex
	lines []string
}

func (r *recordingReporter) Diagnostic(severity, message, _ string, _ int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, severity+": "+message)
}

func (r *recordingReporter) Log(level, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, level+": "+message)
}

func writeLayout(t *testing.T) (string, string) {
	t.Helper()
	img, err := random.Image(128, 1)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "layout")
	digest, err := oci.WriteLayout(dir, img)
	if err != nil {
		t.Fatal(err)
	}
	return dir, digest
}

// putMember is a migration member: a manifest and the bundle it references.
func putMember() putpublish.Member {
	bundle := []byte("migration bundle")
	return putpublish.Member{
		Kind: distribution.KindMigration, Coordinate: "acme/orders-db", Version: "0007",
		MediaType: put.MigrationManifestMediaType, Manifest: []byte(fmt.Sprintf(`{"blob_digest":%q}`, put.Digest(bundle))),
		Blobs: []putpublish.Blob{putpublish.BytesBlob(put.MigrationBundleBlobMediaType, bundle)},
	}
}

// uploads runs every uploader against registry with testBearer and returns the
// text each one produced: its error, and for Go its report.
func uploads(t *testing.T, registry string) map[string]string {
	t.Helper()
	ctx := context.Background()
	npmClient, err := npmpublish.NewHTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	goClient, err := gomodpublish.NewHTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	text := func(err error) string {
		if err == nil {
			return ""
		}
		return err.Error()
	}
	results := map[string]string{}

	artifact := npmpublish.Artifact{Name: "@acme/web", Version: "1.0.0", Manifest: []byte(`{"name":"@acme/web","version":"1.0.0"}`), Tarball: []byte("tgz")}
	payload, err := npmpublish.BuildPayload(artifact)
	if err != nil {
		t.Fatal(err)
	}
	results["npm put"] = text(npmpublish.Put(ctx, npmClient, registry, testBearer, payload))
	_, probeErr := npmpublish.Probe(ctx, npmClient, registry, testBearer, "@acme/web", "1.0.0", npmpublish.Digest([]byte("tgz")), 3)
	results["npm probe"] = text(probeErr)
	_, publishErr := npmpublish.Publish(ctx, npmClient, registry, testBearer, artifact)
	results["npm publish"] = text(publishErr)

	module := gomodpublish.Module{Path: "go.acme.dev/api", Version: "v1.0.0", Zip: []byte("zip"), Mod: []byte("module go.acme.dev/api\n")}
	for name, run := range map[string]func(gomodpublish.Reporter) error{
		"go upload": func(reporter gomodpublish.Reporter) error {
			_, err := gomodpublish.UploadZip(ctx, goClient, reporter, registry, testBearer, module.Path, module.Zip)
			return err
		},
		"go version": func(reporter gomodpublish.Reporter) error {
			_, err := gomodpublish.PublishVersion(ctx, goClient, reporter, registry, testBearer, module.Path, module.Version, module.Mod, gomodpublish.Digest(module.Zip), true)
			return err
		},
		"go verify zip": func(gomodpublish.Reporter) error {
			return gomodpublish.VerifyZip(ctx, goClient, registry, testBearer, module.Path, module.Version, gomodpublish.Digest(module.Zip))
		},
		"go verify mod": func(gomodpublish.Reporter) error {
			return gomodpublish.VerifyMod(ctx, goClient, registry, testBearer, module.Path, module.Version, module.Mod)
		},
		"go publish": func(reporter gomodpublish.Reporter) error {
			_, _, err := gomodpublish.Publish(ctx, goClient, reporter, registry, testBearer, module)
			return err
		},
	} {
		reporter := &recordingReporter{}
		err := run(reporter)
		results[name] = text(err) + "\n" + strings.Join(reporter.lines, "\n")
	}

	putClient, err := putpublish.NewHTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	member := putMember()
	results["put blob"] = text(putpublish.UploadBlob(ctx, putClient, registry, testBearer, member.Coordinate, member.Blobs[0]))
	_, _, manifestErr := putpublish.PublishManifest(ctx, putClient, registry, testBearer, member)
	results["put manifest"] = text(manifestErr)
	_, readErr := putpublish.ReadManifest(ctx, putClient, registry, testBearer, member.Coordinate, member.Version)
	results["put read"] = text(readErr)
	_, putErr := putpublish.Publish(ctx, putClient, registry, testBearer, member)
	results["put publish"] = text(putErr)

	dir, digest := writeLayout(t)
	host := strings.TrimPrefix(strings.TrimPrefix(registry, "http://"), "https://")
	_, pushErr := oci.PushLayout(ctx, dir, oci.LayoutTarget{Repository: host + "/team/app", Digest: digest, Tags: []string{"1.0.0"}}, testBearer)
	results["oci push"] = text(pushErr)
	return results
}

func TestUploadersNeverFormatTheBearer(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "publication-outbox", "the-bearer-reaches-only-its-registry-and-no-error")
	t.Run("a hostile registry echoes the bearer", func(t *testing.T) {
		registry := hostileRegistry(t)
		for name, output := range uploads(t, registry.URL) {
			if strings.TrimSpace(output) == "" {
				t.Errorf("%s succeeded against a registry that refuses every upload", name)
			}
			for _, secret := range []string{testBearer, url.QueryEscape(testBearer), "s3cret"} {
				if strings.Contains(output, secret) {
					t.Errorf("%s formatted the bearer: %s", name, output)
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
		defer other.Close()
		registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		defer registry.Close()

		for name, output := range uploads(t, registry.URL) {
			if strings.TrimSpace(output) == "" {
				t.Errorf("%s succeeded against a registry that redirects every upload", name)
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if len(leaked) > 0 {
			t.Fatalf("another host received the bearer: %v", leaked)
		}
	})
}
