package distributioncli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/sitecontent"
)

// writeSiteContentFixture lays down a small multi-file, nested doc tree so the
// determinism test exercises sorted multi-entry payloads, not just one file.
func writeSiteContentFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"index.md":          "# Putnami Cloud\n",
		"guides/deploy.md":  "# Deploy\n",
		"guides/publish.md": "# Publish\n",
	}
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func assembleFixtureBundle(t *testing.T, dir string) (*sitecontent.Manifest, []byte, []byte) {
	t.Helper()
	files, err := collectSiteContentFiles(dir, "/docs/platform")
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	manifest, manifestJSON, payload, err := assembleSiteContentBundle(files, "platform", "/docs/platform", "putnami-cloud", "abc123")
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	return manifest, manifestJSON, payload
}

// Determinism is a hard acceptance criterion: assembling the same tree twice
// must produce byte-identical tar.gz payloads (and therefore identical digests
// and published versions). Any per-run tar/gzip nondeterminism (mtimes,
// ownership, entry order) breaks content addressing and re-rolls the consumer.
func TestSiteContentBundleDeterministic(t *testing.T) {
	dir := writeSiteContentFixture(t)

	_, manifest1, payload1 := assembleFixtureBundle(t, dir)
	_, manifest2, payload2 := assembleFixtureBundle(t, dir)

	if !bytes.Equal(payload1, payload2) {
		t.Fatal("assembling the same tree twice produced different tar.gz payload bytes")
	}
	if !bytes.Equal(manifest1, manifest2) {
		t.Fatal("assembling the same tree twice produced different bundle.json bytes")
	}
	d1, d2 := sha256.Sum256(payload1), sha256.Sum256(payload2)
	if hex.EncodeToString(d1[:]) != hex.EncodeToString(d2[:]) {
		t.Fatal("payload digests differ across identical assemblies")
	}
}

// The assembled bundle must satisfy the published site-content-bundle/v1
// contract with zero error diagnostics: strict manifest parse + validation,
// payload verification, and every file served under the declared mount.
func TestSiteContentBundleSatisfiesContract(t *testing.T) {
	dir := writeSiteContentFixture(t)
	manifest, manifestJSON, payload := assembleFixtureBundle(t, dir)

	parsed, diags := sitecontent.ParseAndValidateManifest(manifestJSON)
	if diag.HasErrors(diags) {
		t.Fatalf("manifest failed contract validation:\n%s", formatDiagnostics(diags))
	}
	if parsed.FormatVersion != sitecontent.FormatVersion || parsed.Name != "platform" {
		t.Fatalf("parsed manifest = %+v, want formatVersion %s and name platform", parsed, sitecontent.FormatVersion)
	}

	// The publishable blob is the contract archive: bundle.json embedded at the
	// root, verified end-to-end by the consumer entry point.
	res := sitecontent.VerifyArchive(payload)
	if res.Bundle == nil {
		t.Fatalf("archive failed contract verification:\n%s", formatDiagnostics(res.Diagnostics))
	}
	if res.Bundle.Manifest.Name != "platform" {
		t.Fatalf("embedded manifest name = %q, want platform", res.Bundle.Manifest.Name)
	}
	if len(res.Bundle.Files) != 3 {
		t.Fatalf("archive payload files = %d, want 3 (bundle.json excluded)", len(res.Bundle.Files))
	}

	if len(manifest.Files) != 3 {
		t.Fatalf("manifest files = %d, want 3", len(manifest.Files))
	}
	wantPaths := []string{"docs/platform/guides/deploy.md", "docs/platform/guides/publish.md", "docs/platform/index.md"}
	for i, f := range manifest.Files {
		if f.Path != wantPaths[i] {
			t.Fatalf("files[%d].path = %q, want %q (sorted, mount-prefixed)", i, f.Path, wantPaths[i])
		}
		if _, ok := sitecontent.MountForPath(manifest.Mounts, f.Path); !ok {
			t.Fatalf("file %q is not served under a declared mount", f.Path)
		}
		if !sitecontent.ValidDigest(f.Digest) {
			t.Fatalf("files[%d].digest = %q is not sha256 bare-hex", i, f.Digest)
		}
	}
}

// Symlinks must be rejected, not followed: the contract forbids them and
// following one would smuggle content from outside the designated tree.
func TestSiteContentRejectsSymlinks(t *testing.T) {
	dir := writeSiteContentFixture(t)
	if err := os.Symlink(filepath.Join(dir, "index.md"), filepath.Join(dir, "link.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := collectSiteContentFiles(dir, "/docs/platform"); err == nil {
		t.Fatal("expected an error collecting a tree containing a symlink")
	}
}

// The publish leg: blob upload + atomic publish against an httptest put-server,
// pinning package path site-content/platform, channel stable, the manifest media
// type, and that the published payload is exactly the assembled bundle.json.
func TestPublishSiteContentToRegistry(t *testing.T) {
	var (
		blobPath     string
		blobCT       string
		blobBody     []byte
		blobLength   int64
		blobEncoding []string
		gotAuth      string
		publishPath  string
		publishBody  map[string]json.RawMessage
	)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /put/{namespace}/{package}/blobs", func(w http.ResponseWriter, r *http.Request) {
		blobPath = r.URL.Path
		blobCT = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("Authorization")
		blobLength, blobEncoding = r.ContentLength, r.TransferEncoding
		blobBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{"digest": digestOf(blobBody), "size": len(blobBody)})
	})
	mux.HandleFunc("POST /put/{namespace}/{package}/publish", func(w http.ResponseWriter, r *http.Request) {
		publishPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&publishBody)
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	srv := newPutTestServer(mux)
	defer srv.Close()

	dir := writeSiteContentFixture(t)
	_, manifestJSON, payload := assembleFixtureBundle(t, dir)
	sum := sha256.Sum256(payload)
	digestHex := hex.EncodeToString(sum[:])

	res, err := publishSiteContentToRegistry(context.Background(), srv.Client(), srv.URL, "tok", siteContentPublishOptions{
		Namespace: "site-content", Package: "platform", Version: digestHex[:12], Channel: "stable",
		Name: "platform", Mount: "/docs/platform", PayloadDigest: digestHex,
		ManifestJSON: manifestJSON, Payload: payload, Files: 3,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	if blobPath != "/put/site-content/platform/blobs" {
		t.Fatalf("blob path = %q, want /put/site-content/platform/blobs", blobPath)
	}
	if publishPath != "/put/site-content/platform/publish" {
		t.Fatalf("publish path = %q, want /put/site-content/platform/publish", publishPath)
	}
	if blobCT != "application/gzip" {
		t.Fatalf("blob Content-Type = %q, want application/gzip", blobCT)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("auth = %q, want Bearer tok", gotAuth)
	}
	if !bytes.Equal(blobBody, payload) {
		t.Fatal("uploaded blob differs from the assembled payload")
	}
	// The generated client streams an upload; the publisher still declares
	// its length, as the hand-written upload did, rather than chunk it.
	if blobLength != int64(len(payload)) || len(blobEncoding) != 0 {
		t.Fatalf("blob Content-Length = %d, Transfer-Encoding = %v; want %d and none", blobLength, blobEncoding, len(payload))
	}
	if string(publishBody["channel"]) != `"stable"` {
		t.Fatalf("channel = %s, want stable", publishBody["channel"])
	}
	if string(publishBody["media_type"]) != `"`+siteContentManifestMediaType+`"` {
		t.Fatalf("media_type = %s, want %s", publishBody["media_type"], siteContentManifestMediaType)
	}
	if string(publishBody["version"]) != `"`+digestHex[:12]+`"` {
		t.Fatalf("version = %s, want content-digest version %q", publishBody["version"], digestHex[:12])
	}
	// The published payload is the put-native registry wrapper, NOT raw
	// bundle.json. Its artifact.blob must equal the uploaded blob digest — this
	// is what makes the payload blob a package member (put-server's
	// packageReferencesBlob reads exactly this field); without it,
	// /put/site-content/platform/blobs/<digest> 404s after publish. The bundle
	// sub-object must remain the pristine, strict-valid contract manifest.
	var wrapper struct {
		Artifact struct {
			Blob      string `json:"blob"`
			MediaType string `json:"mediaType"`
			Size      int    `json:"size"`
		} `json:"artifact"`
		Bundle json.RawMessage `json:"bundle"`
	}
	if err := json.Unmarshal(publishBody["payload"], &wrapper); err != nil {
		t.Fatalf("published payload is not a site-content registry wrapper: %v", err)
	}
	if wrapper.Artifact.Blob != res.BlobDigest {
		t.Fatalf("artifact.blob = %q, want the uploaded blob digest %q (payload blob would 404 otherwise)", wrapper.Artifact.Blob, res.BlobDigest)
	}
	if wrapper.Artifact.MediaType != "application/gzip" || wrapper.Artifact.Size != len(payload) {
		t.Fatalf("artifact media/size = %q/%d, want application/gzip/%d", wrapper.Artifact.MediaType, wrapper.Artifact.Size, len(payload))
	}
	if !bytes.Equal(wrapper.Bundle, manifestJSON) {
		t.Fatalf("wrapper.bundle differs from the assembled bundle.json:\n%s\nvs\n%s", wrapper.Bundle, manifestJSON)
	}
	if _, diags := sitecontent.ParseAndValidateManifest(wrapper.Bundle); diag.HasErrors(diags) {
		t.Fatalf("wrapper.bundle is not a strict-valid contract manifest:\n%s", formatDiagnostics(diags))
	}

	if res.Package != "site-content/platform" || res.Version != digestHex[:12] || res.Channel != "stable" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.PayloadDigest != digestHex || res.BlobDigest != "sha256:"+digestHex {
		t.Fatalf("digests = %q / %q, want %q", res.PayloadDigest, res.BlobDigest, digestHex)
	}
}

// A digest disagreement between what we hashed and what the registry stored
// must fail the publish before the manifest is published: the version IS the
// content address.
func TestPublishSiteContentDigestMismatchFails(t *testing.T) {
	var published bool
	mux := http.NewServeMux()
	mux.HandleFunc("POST /put/{namespace}/{package}/blobs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"digest":"sha256:` + strings.Repeat("0", 64) + `","size":1}`))
	})
	mux.HandleFunc("POST /put/{namespace}/{package}/publish", func(w http.ResponseWriter, _ *http.Request) {
		published = true
		w.WriteHeader(201)
	})
	srv := newPutTestServer(mux)
	defer srv.Close()

	_, err := publishSiteContentToRegistry(context.Background(), srv.Client(), srv.URL, "tok", siteContentPublishOptions{
		Namespace: "site-content", Package: "platform", Version: "abc", Channel: "stable",
		PayloadDigest: strings.Repeat("f", 64), ManifestJSON: []byte(`{}`), Payload: []byte("x"),
	})
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("expected a digest mismatch error, got %v", err)
	}
	if published {
		t.Fatal("manifest must not be published after a payload digest mismatch")
	}
}

// A 409 on publish (same content already published) must repoint the channel —
// the idempotent republish path.
func TestPublishSiteContent409MovesChannel(t *testing.T) {
	var (
		channelPutChannel string
		channelPutVersion string
	)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /put/{namespace}/{package}/blobs", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{"digest": digestOf(body), "size": len(body)})
	})
	mux.HandleFunc("POST /put/{namespace}/{package}/publish", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"error":"version already published"}`))
	})
	mux.HandleFunc("PUT /put/{namespace}/{package}/channels/{channel}", func(w http.ResponseWriter, r *http.Request) {
		channelPutChannel = r.PathValue("channel")
		var body struct {
			Version string `json:"version"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		channelPutVersion = body.Version
		_, _ = w.Write([]byte(`{"id":"channel-1","name":"stable","package_id":"package-1","target_version_id":"version-1"}`))
	})
	srv := newPutTestServer(mux)
	defer srv.Close()

	payload := []byte("payload-bytes")
	sum := sha256.Sum256(payload)
	digestHex := hex.EncodeToString(sum[:])
	if _, err := publishSiteContentToRegistry(context.Background(), srv.Client(), srv.URL, "tok", siteContentPublishOptions{
		Namespace: "site-content", Package: "platform", Version: digestHex[:12], Channel: "stable",
		PayloadDigest: digestHex, ManifestJSON: []byte(`{}`), Payload: payload,
	}); err != nil {
		t.Fatalf("a 409 on publish must fall back to a channel update, got %v", err)
	}
	if channelPutChannel != "stable" || channelPutVersion != digestHex[:12] {
		t.Fatalf("channel PUT = %q@%q, want stable@%s", channelPutChannel, channelPutVersion, digestHex[:12])
	}
}

// TestPublishSiteContent409ChannelFailureIsFatal pins the fail-loud contract for
// site content: the runtime overlay resolves the bundle through the channel, so
// a bundle published under a channel that never moved must not report success.
func TestPublishSiteContent409ChannelFailureIsFatal(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /put/{namespace}/{package}/blobs", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{"digest": digestOf(body), "size": len(body)})
	})
	mux.HandleFunc("POST /put/{namespace}/{package}/publish", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"error":"version already published"}`))
	})
	mux.HandleFunc("PUT /put/{namespace}/{package}/channels/{channel}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	})
	srv := newPutTestServer(mux)
	defer srv.Close()

	payload := []byte("payload-bytes")
	sum := sha256.Sum256(payload)
	digestHex := hex.EncodeToString(sum[:])
	_, err := publishSiteContentToRegistry(context.Background(), srv.Client(), srv.URL, "tok", siteContentPublishOptions{
		Namespace: "site-content", Package: "platform", Version: digestHex[:12], Channel: "stable",
		PayloadDigest: digestHex, ManifestJSON: []byte(`{}`), Payload: payload,
	})
	if err == nil {
		t.Fatal("a failed channel re-point must fail the site-content publish")
	}
	if !strings.Contains(err.Error(), "stable") {
		t.Fatalf("error = %q, want it to name the channel that did not move", err.Error())
	}
	// put-server's own words survive the generated client.
	if !strings.Contains(err.Error(), `update channel stable: 500 Internal Server Error: {"error":"boom"}`) {
		t.Fatalf("error = %q, want put-server's refusal with its status line", err.Error())
	}
}

// TestPublishSiteContentToRegistryRefusals pins how a refused or malformed
// put-server answer reads, leg by leg.
func TestPublishSiteContentToRegistryRefusals(t *testing.T) {
	payload := []byte("payload-bytes")
	sum := sha256.Sum256(payload)
	digestHex := hex.EncodeToString(sum[:])
	for _, test := range []struct {
		name    string
		blobs   http.HandlerFunc
		publish http.HandlerFunc
		want    string
	}{
		{
			name: "upload refused",
			blobs: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"namespace not granted"}`))
			},
			want: `upload site-content blob: 403 Forbidden: {"error":"namespace not granted"}`,
		},
		{
			name: "upload answers no digest",
			blobs: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"size":1}`))
			},
			want: "upload site-content blob: invalid response (no digest)",
		},
		{
			name: "upload answers outside the contract",
			blobs: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"digest":42}`))
			},
			want: "upload site-content blob: invalid response (no digest)",
		},
		{
			name: "publish refused",
			publish: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"payload is required"}`))
			},
			want: `publish site-content bundle: 400 Bad Request: {"error":"payload is required"}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			mux := http.NewServeMux()
			blobs := test.blobs
			if blobs == nil {
				blobs = func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(map[string]any{"digest": digestOf(body), "size": len(body)})
				}
			}
			mux.HandleFunc("POST /put/{namespace}/{package}/blobs", blobs)
			if test.publish != nil {
				mux.HandleFunc("POST /put/{namespace}/{package}/publish", test.publish)
			}
			srv := newPutTestServer(mux)
			defer srv.Close()
			_, err := publishSiteContentToRegistry(context.Background(), srv.Client(), srv.URL, "tok", siteContentPublishOptions{
				Namespace: "site-content", Package: "platform", Version: digestHex[:12], Channel: "stable",
				PayloadDigest: digestHex, ManifestJSON: []byte(`{}`), Payload: payload,
			})
			if err == nil || err.Error() != test.want {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

// seedPlatformDocs lays down docs/example.dev/platform/index.md in a fresh
// workspace and returns the workspace and its platform section.
func seedPlatformDocs(t *testing.T) (string, SiteContentSection) {
	t.Helper()
	ws := t.TempDir()
	sectionDir := filepath.Join(ws, "docs", "example.dev", "platform")
	if err := os.MkdirAll(sectionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sectionDir, "index.md"), []byte("# Platform\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sections, err := DeriveSiteContentSections(filepath.Join(ws, "docs", "example.dev"), "example.dev", "", map[string]string{})
	if err != nil || len(sections) != 1 {
		t.Fatalf("derive the seeded section = (%+v, %v)", sections, err)
	}
	return ws, sections[0]
}

// AssembleSiteContentSection versions a bundle by its content address and
// reports an empty section as not shipped.
func TestAssembleSiteContentSection(t *testing.T) {
	ws, section := seedPlatformDocs(t)
	bundle, shipped, err := AssembleSiteContentSection(section, "abc123")
	if err != nil || !shipped {
		t.Fatalf("assemble = (%v, %v), want a shipped bundle", shipped, err)
	}
	if bundle.Section() != section {
		t.Fatalf("section = %+v, want %+v", bundle.Section(), section)
	}
	if len(bundle.Version()) != 12 || !strings.HasPrefix(bundle.PayloadDigest(), bundle.Version()) {
		t.Fatalf("version %q must be the 12-char prefix of payload digest %q", bundle.Version(), bundle.PayloadDigest())
	}
	if paths := bundle.FilePaths(); len(paths) != 1 || paths[0] != "docs/platform/index.md" {
		t.Fatalf("file paths = %v, want [docs/platform/index.md]", paths)
	}
	again, _, err := AssembleSiteContentSection(section, "abc123")
	if err != nil || again.Version() != bundle.Version() {
		t.Fatalf("identical content must give an identical version: %q vs %q (%v)", again.Version(), bundle.Version(), err)
	}

	empty := section
	empty.Dir = filepath.Join(ws, "docs", "example.dev", "empty")
	if err := os.MkdirAll(empty.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, shipped, err := AssembleSiteContentSection(empty, "abc123"); err != nil || shipped {
		t.Fatalf("empty section = (%v, %v), want not shipped", shipped, err)
	}
}

// DeriveSiteContentSections derives each section of one site, skips meta files
// and dot-directories, honors the only filter, and rejects an invalid slug and
// a slug another site already claimed.
func TestDeriveSiteContentSections(t *testing.T) {
	site := t.TempDir()
	for _, dir := range []string{"platform", "guides", ".gen"} {
		if err := os.MkdirAll(filepath.Join(site, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(site, "README.md"), []byte("# meta\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	claimed := map[string]string{}
	sections, err := DeriveSiteContentSections(site, "example.dev", "", claimed)
	if err != nil || len(sections) != 2 {
		t.Fatalf("sections = (%+v, %v), want guides and platform", sections, err)
	}
	if claimed["platform"] != "example.dev/platform" {
		t.Fatalf("claimed = %v, want platform owned by example.dev/platform", claimed)
	}
	if _, err := DeriveSiteContentSections(site, "putnami.cloud", "platform", claimed); err == nil || !strings.Contains(err.Error(), "duplicate section") {
		t.Fatalf("a slug another site claimed must be rejected, got %v", err)
	}
	only, err := DeriveSiteContentSections(site, "example.dev", "guides", map[string]string{})
	if err != nil || len(only) != 1 || only[0].Mount != "/docs/guides" || only[0].Package != "doc-contents-guides" {
		t.Fatalf("filtered sections = (%+v, %v), want [guides]", only, err)
	}
	if err := os.MkdirAll(filepath.Join(site, "Bad"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := DeriveSiteContentSections(site, "example.dev", "", map[string]string{}); err == nil {
		t.Fatal("an invalid section slug must be rejected")
	}
	if _, err := DeriveSiteContentSections(filepath.Join(site, "missing"), "example.dev", "", map[string]string{}); err == nil {
		t.Fatal("an unreadable site directory must be rejected")
	}
}

// Under native CI, PUTNAMI_REGISTRY_PUT_URL names the runner's loopback
// publication broker and PUTNAMI_CLOUD_TOKEN is the job's local capability.
// PublishSiteContentBundles must send every Put request to that broker with
// the capability, with no stored credential and no human session (a lookup
// of one fails with "no Put registry credential for 127.0.0.1:<port>").
func TestPublishSiteContentBundlesUsesNativePublicationBroker(t *testing.T) {
	const capability = "local_broker_capability_0123456789abcdef"
	var (
		paths []string
		auths []string
	)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /put/put/cloud/doc-contents-platform/blobs", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{"digest": digestOf(body), "size": len(body)})
	})
	mux.HandleFunc("POST /put/put/cloud/doc-contents-platform/publish", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	srv := newPutTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		auths = append(auths, r.Header.Get("Authorization"))
		mux.ServeHTTP(w, r)
	}))
	defer srv.Close()

	ws, section := seedPlatformDocs(t)
	bundle, _, err := AssembleSiteContentSection(section, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"PUTNAMI_HOME":             t.TempDir(), // no registries.json: no stored credential
		"PUTNAMI_REGISTRY_PUT_URL": srv.URL + "/put",
		"PUTNAMI_CLOUD_TOKEN":      capability,
	}
	ioctx := clicore.IO{Stdout: func(string) {}, Stderr: func(string) {}, JSON: func(any) {}, Client: srv.Client()}
	baseURL, results, err := PublishSiteContentBundles(map[string]any{}, ws, env, ioctx, []SiteContentBundle{bundle}, "stable")
	if err != nil {
		t.Fatalf("publish through the native broker: %v", err)
	}
	if baseURL != srv.URL+"/put" || len(results) != 1 || results[0].Package != "cloud/doc-contents-platform" ||
		results[0].Version != bundle.Version() || results[0].Channel != "stable" {
		t.Fatalf("publish = (%q, %+v)", baseURL, results)
	}

	want := []string{
		"POST /put/put/cloud/doc-contents-platform/blobs",
		"POST /put/put/cloud/doc-contents-platform/publish",
	}
	if strings.Join(paths, "\n") != strings.Join(want, "\n") {
		t.Fatalf("broker requests = %q, want %q", paths, want)
	}
	for i, auth := range auths {
		if auth != "Bearer "+capability {
			t.Fatalf("request %d authorization = %q, want the local capability", i, auth)
		}
	}
}

// A malformed local capability is configuration, not a reason to fall back:
// PublishSiteContentBundles must fail before any request leaves the process.
// A refused upload fails the publish.
func TestPublishSiteContentBundlesRefusals(t *testing.T) {
	requests := 0
	srv := newPutTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ws, section := seedPlatformDocs(t)
	bundle, _, err := AssembleSiteContentSection(section, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	ioctx := clicore.IO{Stdout: func(string) {}, Stderr: func(string) {}, JSON: func(any) {}, Client: srv.Client()}
	env := map[string]string{
		"PUTNAMI_HOME":             t.TempDir(),
		"PUTNAMI_REGISTRY_PUT_URL": srv.URL + "/put",
		"PUTNAMI_CLOUD_TOKEN":      "bad token",
	}
	if _, _, err := PublishSiteContentBundles(map[string]any{}, ws, env, ioctx, []SiteContentBundle{bundle}, "stable"); err == nil {
		t.Fatal("a malformed local capability must fail the publish")
	}
	if requests != 0 {
		t.Fatalf("broker received %d requests, want 0", requests)
	}

	env["PUTNAMI_CLOUD_TOKEN"] = "local_broker_capability_0123456789abcdef"
	if _, _, err := PublishSiteContentBundles(map[string]any{}, ws, env, ioctx, []SiteContentBundle{bundle}, "stable"); err == nil {
		t.Fatal("a refused upload must fail the publish")
	}
	if requests == 0 {
		t.Fatal("the upload never reached the broker")
	}
}
