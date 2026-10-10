package distributioncli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	distributionproto "go.putnami.dev/protocol/distribution"
	"go.putnami.dev/sdk/extension/releaseset"
)

func writeArchiveDir(t *testing.T, artifact string, platforms ...string) (dir string, files []string) {
	t.Helper()
	dir = t.TempDir()
	for _, platform := range platforms {
		name := artifact + "-" + platform + ".tar.gz"
		if err := os.WriteFile(filepath.Join(dir, name), []byte("gzip-bytes-"+platform), 0o600); err != nil {
			t.Fatal(err)
		}
		files = append(files, name)
	}
	sort.Strings(files)
	return dir, files
}

func writeTarGzWithFile(t *testing.T, dir, name, entryName string, content []byte) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: entryName, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func testArchivePublishCredential(baseURL string) resolvedRegistryToken {
	return resolvedRegistryToken{
		Token: "tok",
		Endpoint: RegistryEndpoint{
			Registry: RegistryPut,
			Host:     "put.example",
			URL:      baseURL,
		},
	}
}

// newPutTestServer serves handler the way put-server answers: labeled JSON
// unless the handler sets another media type.
func newPutTestServer(handler http.Handler) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		handler.ServeHTTP(w, r)
	}))
}

// immutablePutManifest and immutablePutVersion are the stored manifest and
// version put-server answers with, as a fixture writes them.
type immutablePutManifest struct {
	ID        string          `json:"id"`
	PackageID string          `json:"package_id"`
	MediaType string          `json:"media_type"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt string          `json:"created_at"`
}

type immutablePutVersion struct {
	ID         string `json:"id"`
	PackageID  string `json:"package_id"`
	Version    string `json:"version"`
	ManifestID string `json:"manifest_id"`
	State      string `json:"state"`
	Visibility string `json:"visibility"`
	CreatedAt  string `json:"created_at"`
}

func atomicManifestResponse(t *testing.T, packageRef string, body map[string]json.RawMessage) map[string]any {
	t.Helper()
	const packageID = "package-1"
	const manifestID = "manifest-1"
	const createdAt = "2026-09-06T05:00:00Z"
	return map[string]any{
		"package": packageRef,
		"version": immutablePutVersion{
			ID: "version-1", PackageID: packageID, Version: unquoteJSON(t, body["version"]),
			ManifestID: manifestID, State: "published", Visibility: "private", CreatedAt: createdAt,
		},
		"manifest": immutablePutManifest{
			ID: manifestID, PackageID: packageID,
			MediaType: unquoteJSON(t, body["media_type"]), Payload: body["payload"], CreatedAt: createdAt,
		},
		"channel": nil,
	}
}

func respondWithAtomicManifest(t *testing.T, w http.ResponseWriter, packageRef string, body map[string]json.RawMessage) {
	t.Helper()
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(atomicManifestResponse(t, packageRef, body)); err != nil {
		t.Fatal(err)
	}
}

func unquoteJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestPublishArchivesToRegistryPublishesImmutableManifest(t *testing.T) {
	var (
		blobUploads  int
		blobCTs      []string
		publishBody  map[string]json.RawMessage
		channelCalls int
	)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /put/{namespace}/{package}/blobs", func(w http.ResponseWriter, r *http.Request) {
		blobUploads++
		blobCTs = append(blobCTs, r.Header.Get("Content-Type"))
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Fatalf("blob authorization = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"digest": digestOf(body), "size": len(body)})
	})
	mux.HandleFunc("POST /put/{namespace}/{package}/publish", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Fatalf("publish authorization = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&publishBody); err != nil {
			t.Fatal(err)
		}
		respondWithAtomicManifest(t, w, r.PathValue("namespace")+"/"+r.PathValue("package"), publishBody)
	})
	mux.HandleFunc("PUT /put/{namespace}/{package}/channels/{channel}", func(w http.ResponseWriter, _ *http.Request) {
		channelCalls++
		w.WriteHeader(http.StatusNoContent)
	})
	srv := newPutTestServer(mux)
	defer srv.Close()

	dir, files := writeArchiveDir(t, "putnami-cloud", "darwin-arm64", "linux-x64")
	res, err := publishArchivesToRegistry(context.Background(), srv.Client(), testArchivePublishCredential(srv.URL), archivePublishOptions{
		Namespace: "putnami", Package: "cloud", Version: "1.2.3",
		ArchivesDir: dir, Files: files, Artifact: "putnami-cloud", MetaArtifact: "putnami-cloud",
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if blobUploads != 2 || channelCalls != 0 {
		t.Fatalf("blob uploads = %d, channel calls = %d; want 2, 0", blobUploads, channelCalls)
	}
	for _, contentType := range blobCTs {
		if contentType != "application/gzip" {
			t.Fatalf("blob Content-Type = %q, want application/gzip", contentType)
		}
	}
	if string(publishBody["channel"]) != `""` {
		t.Fatalf("publish channel = %s, want empty", publishBody["channel"])
	}
	if unquoteJSON(t, publishBody["media_type"]) != archiveManifestMediaType {
		t.Fatalf("media_type = %s", publishBody["media_type"])
	}
	var payload struct {
		Artifacts map[string]archiveBlob `json:"artifacts"`
	}
	if err := json.Unmarshal(publishBody["payload"], &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Artifacts) != 2 || payload.Artifacts["darwin-arm64"].Digest == "" || payload.Artifacts["linux-x64"].Digest == "" {
		t.Fatalf("payload artifacts = %+v", payload.Artifacts)
	}
	if res.Package != "putnami/cloud" || res.Version != "1.2.3" || res.ArtifactDigest != digestOf(publishBody["payload"]) {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.Platforms["darwin/arm64"] != res.Artifacts["darwin-arm64"].Digest || res.Platforms["linux/x64"] != res.Artifacts["linux-x64"].Digest {
		t.Fatalf("published-member platforms = %+v, artifacts = %+v", res.Platforms, res.Artifacts)
	}
}

func TestPublishArchivesTemplateFanout(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /put/{namespace}/{package}/blobs", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"digest": digestOf(body), "size": len(body)})
	})
	mux.HandleFunc("POST /put/{namespace}/{package}/publish", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		respondWithAtomicManifest(t, w, r.PathValue("namespace")+"/"+r.PathValue("package"), body)
	})
	srv := newPutTestServer(mux)
	defer srv.Close()

	dir, files := writeArchiveDir(t, "my-template", "any")
	res, err := publishArchivesToRegistry(context.Background(), srv.Client(), testArchivePublishCredential(srv.URL), archivePublishOptions{
		Namespace: "putnami", Package: "my-template", Version: "0.1.0",
		ArchivesDir: dir, Files: files, Artifact: "my-template", MetaArtifact: "my-template", Template: true,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(res.Artifacts) != len(archivePlatforms) || len(res.Platforms) != len(archivePlatforms) {
		t.Fatalf("template result = %+v, want every supported platform", res)
	}
}

func TestPublishArchivesCLIBinaryExtraction(t *testing.T) {
	var gotContentType string
	var gotBlobBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("POST /put/{namespace}/{package}/blobs", func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		gotBlobBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"digest": digestOf(gotBlobBody), "size": len(gotBlobBody)})
	})
	mux.HandleFunc("POST /put/{namespace}/{package}/publish", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		respondWithAtomicManifest(t, w, r.PathValue("namespace")+"/"+r.PathValue("package"), body)
	})
	srv := newPutTestServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	binaryBytes := []byte("ELF-BINARY-BYTES putnami 3.0.0")
	writeTarGzWithFile(t, dir, "putnami-darwin-arm64.tar.gz", "compiled/putnami", binaryBytes)
	res, err := publishArchivesToRegistry(context.Background(), srv.Client(), testArchivePublishCredential(srv.URL), archivePublishOptions{
		Namespace: "putnami", Package: "cli", Version: "3.0.0", ArchivesDir: dir,
		Files: []string{"putnami-darwin-arm64.tar.gz"}, Artifact: "putnami", MetaArtifact: "putnami", BinaryName: "putnami",
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if gotContentType != "application/octet-stream" || !bytes.Equal(gotBlobBody, binaryBytes) {
		t.Fatalf("uploaded Content-Type=%q bytes=%q", gotContentType, gotBlobBody)
	}
	if res.Platforms["darwin/arm64"] == "" {
		t.Fatalf("missing platform digest: %+v", res)
	}
}

func TestPublishArchivesCLIVersionMismatchFails(t *testing.T) {
	dir := t.TempDir()
	writeTarGzWithFile(t, dir, "putnami-darwin-arm64.tar.gz", "compiled/putnami", []byte("putnami 2.9.0"))
	_, err := publishArchivesToRegistry(context.Background(), http.DefaultClient, testArchivePublishCredential("http://unused"), archivePublishOptions{
		Namespace: "putnami", Package: "cli", Version: "3.0.0", ArchivesDir: dir,
		Files: []string{"putnami-darwin-arm64.tar.gz"}, Artifact: "putnami", MetaArtifact: "putnami", BinaryName: "putnami",
	})
	if err == nil || !strings.Contains(err.Error(), "putnami-darwin-arm64.tar.gz") || !strings.Contains(err.Error(), `"3.0.0"`) {
		t.Fatalf("error = %v, want archive and expected version", err)
	}
}

func TestPublishArchivesCLIRequiresBinaryName(t *testing.T) {
	dir, files := writeArchiveDir(t, "putnami", "darwin-arm64")
	_, err := publishArchivesToRegistry(context.Background(), http.DefaultClient, testArchivePublishCredential("http://unused"), archivePublishOptions{
		Namespace: "putnami", Package: "cli", Version: "1.0.0", ArchivesDir: dir,
		Files: files, Artifact: "putnami", MetaArtifact: "putnami",
	})
	if err == nil {
		t.Fatal("expected an error without --binary-name")
	}
}

// cliArchiveTestServer accepts blob uploads and the immutable publish, and
// records each uploaded body by digest.
func cliArchiveTestServer(t *testing.T) (*httptest.Server, map[string][]byte) {
	t.Helper()
	uploads := map[string][]byte{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /put/{namespace}/{package}/blobs", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		uploads[digestOf(body)] = body
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"digest": digestOf(body), "size": len(body)})
	})
	mux.HandleFunc("POST /put/{namespace}/{package}/publish", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		respondWithAtomicManifest(t, w, r.PathValue("namespace")+"/"+r.PathValue("package"), body)
	})
	srv := newPutTestServer(mux)
	t.Cleanup(srv.Close)
	return srv, uploads
}

// refusingClient fails the test on any request: a refused archive must stop
// the publish before anything reaches the registry.
func refusingClient(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{Transport: workflowRoundTrip(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("refused archive still reached the registry: %s %s", request.Method, request.URL)
		return nil, nil
	})}
}

// The framework packages the Windows CLI as compiled/putnami.exe (decision
// D-W2). The publisher uploads those exact bytes under windows-x64, and keeps
// reading compiled/putnami from the Unix archive of the same set. The file
// names are the ones `package --archives` writes for @putnami/cli, published
// with --binary-name putnami.
func TestPublishArchivesCLIWindowsArchiveUploadsTheExe(t *testing.T) {
	srv, uploads := cliArchiveTestServer(t)
	dir := t.TempDir()
	unixBinary := []byte("ELF-BINARY-BYTES putnami 3.0.0")
	windowsBinary := []byte("MZ-PE-BINARY-BYTES putnami 3.0.0")
	writeTarGzWithFile(t, dir, "putnami-cli-linux-x64.tar.gz", "compiled/putnami", unixBinary)
	writeTarGzWithFile(t, dir, "putnami-cli-windows-x64.tar.gz", "compiled/putnami.exe", windowsBinary)

	res, err := publishArchivesToRegistry(context.Background(), srv.Client(), testArchivePublishCredential(srv.URL), archivePublishOptions{
		Namespace: "putnami", Package: "cli", Version: "3.0.0", ArchivesDir: dir,
		Files:    []string{"putnami-cli-linux-x64.tar.gz", "putnami-cli-windows-x64.tar.gz"},
		Artifact: "putnami", MetaArtifact: "putnami-cli", BinaryName: "putnami",
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := res.Artifacts["windows-x64"].Digest; got != digestOf(windowsBinary) || !bytes.Equal(uploads[got], windowsBinary) {
		t.Fatalf("windows-x64 digest = %q, want the uploaded compiled/putnami.exe bytes %q", got, digestOf(windowsBinary))
	}
	if got := res.Artifacts["linux-x64"].Digest; got != digestOf(unixBinary) || !bytes.Equal(uploads[got], unixBinary) {
		t.Fatalf("linux-x64 digest = %q, want the uploaded compiled/putnami bytes %q", got, digestOf(unixBinary))
	}
	if len(res.Artifacts) != 2 || res.Platforms["windows/x64"] != digestOf(windowsBinary) {
		t.Fatalf("result = %+v, want exactly the linux-x64 and windows-x64 executables", res)
	}
}

// The .exe name belongs to Windows archives only. A Unix archive that holds
// only compiled/putnami.exe is refused, and so is a Windows archive that holds
// only compiled/putnami: neither reaches the registry.
func TestPublishArchivesCLIRefusesTheWrongExecutableName(t *testing.T) {
	for _, test := range []struct {
		name, file, entry, missing string
	}{
		{name: "exe in a linux archive", file: "putnami-linux-x64.tar.gz", entry: "compiled/putnami.exe", missing: "compiled/putnami not found"},
		{name: "exe in a darwin archive", file: "putnami-darwin-arm64.tar.gz", entry: "compiled/putnami.exe", missing: "compiled/putnami not found"},
		{name: "no exe in a windows archive", file: "putnami-windows-x64.tar.gz", entry: "compiled/putnami", missing: "compiled/putnami.exe not found"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTarGzWithFile(t, dir, test.file, test.entry, []byte("BINARY putnami 3.0.0"))
			_, err := publishArchivesToRegistry(context.Background(), refusingClient(t), testArchivePublishCredential("https://put.example"), archivePublishOptions{
				Namespace: "putnami", Package: "cli", Version: "3.0.0", ArchivesDir: dir,
				Files: []string{test.file}, Artifact: "putnami", MetaArtifact: "putnami", BinaryName: "putnami",
			})
			if err == nil || !strings.Contains(err.Error(), test.file) || !strings.Contains(err.Error(), test.missing) {
				t.Fatalf("error = %v, want %q for %s", err, test.missing, test.file)
			}
		})
	}
}

func TestCLIArchiveBinaryEntry(t *testing.T) {
	for _, test := range []struct{ platform, binaryName, want string }{
		{"windows-x64", "putnami", "compiled/putnami.exe"},
		{"windows-arm64", "putnami", "compiled/putnami.exe"},
		{"windows-x64", "putnami.exe", "compiled/putnami.exe"},
		{"windows-x64", "PUTNAMI.EXE", "compiled/PUTNAMI.EXE"},
		{"linux-x64", "putnami", "compiled/putnami"},
		{"linux-arm64", "putnami", "compiled/putnami"},
		{"darwin-x64", "putnami", "compiled/putnami"},
		{"darwin-arm64", "putnami", "compiled/putnami"},
		{"", "putnami", "compiled/putnami"},
		{"windowsish-x64", "putnami", "compiled/putnami"},
	} {
		if got := cliArchiveBinaryEntry(test.platform, test.binaryName); got != test.want {
			t.Errorf("cliArchiveBinaryEntry(%q, %q) = %q, want %q", test.platform, test.binaryName, got, test.want)
		}
	}
}

func TestPublishArchivesConflictRequiresExactManifestReadback(t *testing.T) {
	for _, test := range []struct {
		name      string
		mediaType string
		payload   func(json.RawMessage) json.RawMessage
		wantErr   bool
	}{
		{name: "exact", mediaType: archiveManifestMediaType, payload: func(value json.RawMessage) json.RawMessage { return value }},
		{name: "different bytes", mediaType: archiveManifestMediaType, payload: func(json.RawMessage) json.RawMessage { return json.RawMessage(`{"artifacts":{}}`) }, wantErr: true},
		{name: "different media type", mediaType: "application/json", payload: func(value json.RawMessage) json.RawMessage { return value }, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var publishedPayload json.RawMessage
			mux := http.NewServeMux()
			mux.HandleFunc("POST /put/{namespace}/{package}/blobs", func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]any{"digest": digestOf(body), "size": len(body)})
			})
			mux.HandleFunc("POST /put/{namespace}/{package}/publish", func(w http.ResponseWriter, r *http.Request) {
				var body map[string]json.RawMessage
				_ = json.NewDecoder(r.Body).Decode(&body)
				publishedPayload = append(json.RawMessage(nil), body["payload"]...)
				w.WriteHeader(http.StatusConflict)
			})
			mux.HandleFunc("GET /put/{namespace}/{package}/versions/{version}/manifest", func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != "Bearer tok" {
					t.Fatalf("readback authorization = %q", got)
				}
				_ = json.NewEncoder(w).Encode(immutablePutManifest{
					ID: "manifest-1", PackageID: "package-1",
					MediaType: test.mediaType, Payload: test.payload(publishedPayload), CreatedAt: "2026-09-06T05:00:00Z",
				})
			})
			srv := newPutTestServer(mux)
			defer srv.Close()

			dir, files := writeArchiveDir(t, "putnami-cloud", "linux-x64")
			res, err := publishArchivesToRegistry(context.Background(), srv.Client(), testArchivePublishCredential(srv.URL), archivePublishOptions{
				Namespace: "putnami", Package: "cloud", Version: "9.9.9", ArchivesDir: dir,
				Files: files, Artifact: "putnami-cloud", MetaArtifact: "putnami-cloud",
			})
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), "immutable version") {
					t.Fatalf("error = %v, want immutable collision", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("exact 409 readback: %v", err)
			}
			if res.ArtifactDigest != digestOf(publishedPayload) {
				t.Fatalf("artifact digest = %q, want %q", res.ArtifactDigest, digestOf(publishedPayload))
			}
		})
	}
}

func TestResolvePutCredentialsUsesMaterializedBearerDirectly(t *testing.T) {
	env := map[string]string{"PUTNAMI_HOME": t.TempDir()}
	endpoint := RegistryEndpoint{Registry: RegistryPut, Host: "put.putnami.dev", URL: "https://put.putnami.dev"}
	if err := WriteRegistriesState(env, &RegistriesState{Version: 1, Keys: []KeyRef{{Registry: RegistryPut, Host: endpoint.Host, URL: endpoint.URL}}}); err != nil {
		t.Fatal(err)
	}
	bearer := workflowJWT(map[string]any{"aud": "distribution", "scope": "put"})
	if err := materializeRegistryLease(env, endpoint, bearer); err != nil {
		t.Fatal(err)
	}
	ioctx := clicore.IO{Client: &http.Client{Transport: workflowRoundTrip(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected request during credential resolution: %s %s", request.Method, request.URL)
		return nil, nil
	})}}
	baseURL, token, err := resolvePutCredentials(nil, env, ioctx)
	if err != nil {
		t.Fatal(err)
	}
	if baseURL != endpoint.URL || token != bearer {
		t.Fatalf("baseURL=%q token=%q, want materialized bearer", baseURL, token)
	}
}

func archiveCommandFixture(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	projectDir := filepath.Join(ws, "tooling", "sdd-extension")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "putnami.json"), []byte(`{"name":"@putnami/sdd"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	packageDir := filepath.Join(ws, ".putnami", "out", "tooling", "sdd-extension", "package")
	archivesDir := filepath.Join(packageDir, "archives")
	if err := os.MkdirAll(archivesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "metadata.json"), []byte(`{"version":"0.1.0-test","artifact":"putnami-sdd","channels":["archives"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archivesDir, "putnami-sdd-linux-x64.tar.gz"), []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	return ws
}

func nativePutCredentialEnv(t *testing.T, baseURL string) map[string]string {
	t.Helper()
	env := map[string]string{"PUTNAMI_HOME": t.TempDir()}
	host := hostFromURL(baseURL)
	endpoint := RegistryEndpoint{Registry: RegistryPut, Host: host, URL: baseURL}
	if err := WriteRegistriesState(env, &RegistriesState{Version: 1, Keys: []KeyRef{{Registry: RegistryPut, Host: host, URL: baseURL}}}); err != nil {
		t.Fatal(err)
	}
	bearer := workflowJWT(map[string]any{"aud": "distribution", "scope": "put"})
	if err := materializeRegistryLease(env, endpoint, bearer); err != nil {
		t.Fatal(err)
	}
	return env
}

func TestPublishArchivesUsesNativeMachineCredentialWithoutLease(t *testing.T) {
	ws := archiveCommandFixture(t)

	env := map[string]string{"PUTNAMI_HOME": t.TempDir()}
	endpoint := RegistryEndpoint{Registry: RegistryPut, Host: "put.example", URL: "https://put.example"}
	if err := WriteRegistriesState(env, &RegistriesState{Version: 1, Keys: []KeyRef{{Registry: RegistryPut, Host: endpoint.Host, URL: endpoint.URL}}}); err != nil {
		t.Fatal(err)
	}
	bearer := workflowJWT(map[string]any{"aud": "distribution", "scope": "put"})
	if err := materializeRegistryLease(env, endpoint, bearer); err != nil {
		t.Fatal(err)
	}
	var calls []string
	ioctx := clicore.IO{Stdout: func(string) {}, Stderr: func(string) {}, Client: &http.Client{Transport: workflowRoundTrip(func(request *http.Request) (*http.Response, error) {
		calls = append(calls, request.Method+" "+request.URL.Path)
		if got := request.Header.Get("Authorization"); got != "Bearer "+bearer {
			t.Fatalf("authorization = %q", got)
		}
		switch request.URL.Path {
		case "/put/putnami/sdd/blobs":
			return workflowJSONResponse(http.StatusCreated, map[string]any{"digest": digestOf([]byte("archive")), "size": 7}), nil
		case "/put/putnami/sdd/publish":
			var body map[string]json.RawMessage
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if string(body["version"]) != `"0.1.0-test"` || string(body["channel"]) != `""` {
				t.Fatalf("immutable publish body = %v", body)
			}
			return workflowJSONResponse(http.StatusCreated, atomicManifestResponse(t, "putnami/sdd", body)), nil
		default:
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL)
			return nil, nil
		}
	})}}
	result, err := PublishArchivesWithResult(map[string]any{"app": "@putnami/sdd", "registry-put-url": endpoint.URL}, nil, ws, env, ioctx)
	if err != nil {
		t.Fatalf("publish archives with machine credential: %v", err)
	}
	if result == nil || result.Package != "putnami/sdd" || result.ArtifactDigest == "" {
		t.Fatalf("typed publish result = %+v", result)
	}
	want := []string{"POST /put/putnami/sdd/blobs", "POST /put/putnami/sdd/publish"}
	if len(calls) != len(want) || calls[0] != want[0] || calls[1] != want[1] {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestPublishArchivesRefusesRedirectWithoutPublishedResult(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			redirected := 0
			origin := newPutTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/replayed" {
					redirected++
					return
				}
				w.Header().Set("Location", "/replayed")
				w.WriteHeader(status)
			}))
			defer origin.Close()
			env := nativePutCredentialEnv(t, origin.URL)
			result, err := PublishArchivesWithResult(
				map[string]any{"app": "@putnami/sdd", "registry-put-url": origin.URL}, nil,
				archiveCommandFixture(t), env, clicore.IO{Client: origin.Client()},
			)
			if err == nil || result != nil {
				t.Fatalf("redirect result = %+v err=%v, want no published fact", result, err)
			}
			if redirected != 0 {
				t.Fatalf("same-authority redirect target received %d replayed requests", redirected)
			}
		})
	}
}

func TestPublishArchivesRejectsRetiredControlsBeforeDiscovery(t *testing.T) {
	for _, test := range []struct {
		name  string
		value any
	}{
		{name: "stable", value: false},
		{name: "archive-owner-workspace", value: ""},
		{name: "archiveOwnerWorkspace", value: "workspace"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := PublishArchives(map[string]any{"app": "missing", test.name: test.value}, nil, t.TempDir(), nil, clicore.IO{})
			if err == nil || !strings.Contains(err.Error(), "unavailable") {
				t.Fatalf("error = %v, want retired control refusal", err)
			}
		})
	}
}

func TestPublishArchivesCmd(t *testing.T) {
	ws := t.TempDir()
	projDir := filepath.Join(ws, "surfaces", "cli")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "putnami.json"), []byte(`{"name":"myext"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	capture := func() (clicore.IO, *any) {
		var got any
		return clicore.IO{Stdout: func(string) {}, Stderr: func(string) {}, JSON: func(value any) { got = value }, Client: http.DefaultClient}, &got
	}
	io1, got1 := capture()
	if err := PublishArchives(map[string]any{"app": "myext", "if-present": true, "json": true}, nil, ws, nil, io1); err != nil {
		t.Fatal(err)
	}
	if resultPayloadMap(t, *got1)["status"] != "skipped" {
		t.Fatalf("expected skipped, got %v", *got1)
	}
	io2, _ := capture()
	if err := PublishArchives(map[string]any{"app": "myext"}, nil, ws, nil, io2); err == nil {
		t.Fatal("missing metadata must fail")
	}
	pkgDir := filepath.Join(ws, ".putnami", "out", "surfaces", "cli", "package")
	if err := os.MkdirAll(filepath.Join(pkgDir, "archives"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "metadata.json"), []byte(`{"version":"1.2.3","artifact":"myext","channels":["archives"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "archives", "myext-linux-x64.tar.gz"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	io3, got3 := capture()
	published, err := PublishArchivesWithResult(map[string]any{"app": "myext", "dry-run": true, "json": true}, nil, ws, nil, io3)
	if err != nil {
		t.Fatal(err)
	}
	if published != nil {
		t.Fatalf("dry-run returned a published fact: %+v", published)
	}
	result := resultPayloadMap(t, *got3)
	if result["status"] != "dry-run" || result["package"] != "putnami/myext" || result["version"] != "1.2.3" {
		t.Fatalf("unexpected dry-run: %v", result)
	}
	if _, exists := result["channel"]; exists {
		t.Fatalf("dry-run exposed retired channel authority: %v", result)
	}
}

func TestPublishArchivesCmdUsesExtensionManifestName(t *testing.T) {
	ws := t.TempDir()
	projDir := filepath.Join(ws, "apps", "cli")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "putnami.json"), []byte(`{"name":"apps/cli"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "putnami.extension.json"), []byte(`{"name":"@putnami/cloud"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(ws, ".putnami", "out", "apps", "cli", "package")
	if err := os.MkdirAll(filepath.Join(pkgDir, "archives"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "metadata.json"), []byte(`{"version":"1.2.3","artifact":"apps-cli","channels":["archives"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "archives", "apps-cli-linux-x64.tar.gz"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got any
	ioctx := clicore.IO{Stdout: func(string) {}, Stderr: func(string) {}, JSON: func(value any) { got = value }, Client: http.DefaultClient}
	if err := PublishArchives(map[string]any{"app": "cli", "dry-run": true, "json": true}, nil, ws, nil, ioctx); err != nil {
		t.Fatal(err)
	}
	if resultPayloadMap(t, got)["package"] != "putnami/cloud" {
		t.Fatalf("dry-run package = %v", got)
	}
}

func TestRegistryRef(t *testing.T) {
	cases := []struct{ in, ns, pkg string }{
		{"@putnami/go", "putnami", "go"}, {"putnami", "putnami", "cli"},
		{"putnami-cloud", "putnami", "cloud"}, {"putnami-go", "putnami", "go"},
		{"acme", "putnami", "acme"}, {"@acme/widget", "acme", "widget"},
	}
	for _, test := range cases {
		ns, pkg := registryRef(test.in)
		if ns != test.ns || pkg != test.pkg {
			t.Errorf("registryRef(%q) = %q/%q, want %q/%q", test.in, ns, pkg, test.ns, test.pkg)
		}
		if got := ArchivePackageCoordinate(test.in); got != test.ns+"/"+test.pkg {
			t.Errorf("ArchivePackageCoordinate(%q) = %q, want %q", test.in, got, test.ns+"/"+test.pkg)
		}
	}
}

func TestPublishArchivesBindsGenericChannelAndTargetToStrictReleasePlanBeforeNetwork(t *testing.T) {
	workspaceRoot := writeArchivePublishWorkspace(t)
	plan := archivePublishPlan("/apps/cli", "putnami/cloud", "1.2.3")

	t.Run("matching coordinator channel and member", func(t *testing.T) {
		params := map[string]any{
			"app": "cli", "dry-run": true, "channel": "stable",
			releaseset.ContextParamName: plan,
		}
		published, err := PublishArchivesWithResult(params, nil, workspaceRoot, nil, clicore.IO{Stdout: func(string) {}, Stderr: func(string) {}})
		if err != nil || published != nil {
			t.Fatalf("PublishArchivesWithResult = (%+v, %v), want validated dry-run", published, err)
		}
	})

	for _, test := range []struct {
		name   string
		params map[string]any
		want   string
	}{
		{"direct channel", map[string]any{"app": "cli", "dry-run": true, "channel": "stable"}, "direct archive publishing"},
		{"empty direct channel", map[string]any{"app": "cli", "dry-run": true, "channel": ""}, "direct archive publishing"},
		{"false direct channel", map[string]any{"app": "cli", "dry-run": true, "channel": false}, "direct archive publishing"},
		{"different plan channel", map[string]any{"app": "cli", "dry-run": true, "channel": "canary", releaseset.ContextParamName: plan}, "must match"},
		{"one unplanned channel in list", map[string]any{"app": "cli", "channel": "stable,canary", releaseset.ContextParamName: plan}, "must match"},
		{"list without a channel", map[string]any{"app": "cli", "channel": " , ", releaseset.ContextParamName: plan}, "must match"},
		{"different planned coordinate", map[string]any{"app": "cli", "dry-run": true, releaseset.ContextParamName: archivePublishPlan("/apps/cli", "putnami/other", "1.2.3")}, "does not match planned member"},
		{"different planned version", map[string]any{"app": "cli", "dry-run": true, releaseset.ContextParamName: archivePublishPlan("/apps/cli", "putnami/cloud", "1.2.4")}, "does not match planned member"},
		{"different project", map[string]any{"app": "cli", "dry-run": true, releaseset.ContextParamName: archivePublishPlan("/other", "putnami/cloud", "1.2.3")}, "exactly one is required"},
		{"unsupported plan protocol", map[string]any{"app": "cli", "dry-run": true, releaseset.ContextParamName: map[string]any{"protocolVersion": distributionproto.ProtocolVersion + 1}}, "unsupported"},
		{"null plan", map[string]any{"app": "cli", releaseset.ContextParamName: nil}, "present but empty"},
		{"empty managed channel", map[string]any{"app": "cli", "channel": "", releaseset.ContextParamName: plan}, "must match"},
		{"non-string managed channel", map[string]any{"app": "cli", "channel": false, releaseset.ContextParamName: plan}, "must match"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// A real publication request must refuse these inputs before
			// credential resolution or a registry write, not only in dry-run.
			delete(test.params, "dry-run")
			requests := 0
			client := &http.Client{Transport: workflowRoundTrip(func(*http.Request) (*http.Response, error) {
				requests++
				return workflowJSONResponse(http.StatusInternalServerError, nil), nil
			})}
			published, err := PublishArchivesWithResult(test.params, nil, workspaceRoot, nil, clicore.IO{Client: client, Stdout: func(string) {}, Stderr: func(string) {}})
			if err == nil || !strings.Contains(err.Error(), test.want) || published != nil {
				t.Fatalf("PublishArchivesWithResult = (%+v, %v), want error containing %q", published, err, test.want)
			}
			if requests != 0 {
				t.Fatalf("invalid publication reached the network %d times", requests)
			}
		})
	}
}

func archivePublishPlan(projectID, coordinate, version string) *releaseset.Plan {
	const channel = "stable"
	return &releaseset.Plan{
		ProtocolVersion: distributionproto.ProtocolVersion,
		Namespace:       "putnami-cloud",
		Channels:        []string{channel},
		Heads:           map[string]*distributionproto.ChannelHead{channel: nil},
		Members: []releaseset.PlannedMember{{
			Ecosystem: "archive", Coordinate: coordinate, Version: version,
			Dependencies:   []distributionproto.ReleaseSetDependency{},
			SourceRevision: strings.Repeat("a", 40), SelectionFingerprint: "sha256:" + strings.Repeat("b", 64),
			Selected: true, ProjectID: projectID,
		}},
	}
}

func writeArchivePublishWorkspace(t *testing.T) string {
	t.Helper()
	workspaceRoot := t.TempDir()
	projectRoot := filepath.Join(workspaceRoot, "apps", "cli")
	packageRoot := filepath.Join(workspaceRoot, ".putnami", "out", "apps", "cli", "package")
	if err := os.MkdirAll(filepath.Join(packageRoot, "archives"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	for filename, data := range map[string]string{
		filepath.Join(projectRoot, "putnami.json"):                               `{"name":"apps/cli"}`,
		filepath.Join(projectRoot, "putnami.extension.json"):                     `{"name":"@putnami/cloud"}`,
		filepath.Join(packageRoot, "metadata.json"):                              `{"version":"1.2.3","artifact":"putnami-cloud","channels":["archives"]}`,
		filepath.Join(packageRoot, "archives", "putnami-cloud-linux-x64.tar.gz"): "archive",
	} {
		if err := os.WriteFile(filename, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return workspaceRoot
}

func TestExtractPlatform(t *testing.T) {
	cases := []struct{ filename, artifact, want string }{
		{"putnami-cloud-darwin-arm64.tar.gz", "putnami-cloud", "darwin-arm64"},
		{"putnami-cloud-linux-x64.tar.gz", "putnami-cloud", "linux-x64"},
		{"putnami-cloud.tar.gz", "putnami-cloud", ""}, {"other-linux-x64.tar.gz", "putnami-cloud", ""},
	}
	for _, test := range cases {
		if got := extractPlatform(test.filename, test.artifact); got != test.want {
			t.Errorf("extractPlatform(%q, %q) = %q, want %q", test.filename, test.artifact, got, test.want)
		}
	}
}

func TestExtractFileFromTarGz(t *testing.T) {
	dir := t.TempDir()
	archive := writeTarGzWithFile(t, dir, "a.tar.gz", "compiled/putnami", []byte("BINARY"))
	out, err := extractFileFromTarGz(archive, "compiled/putnami")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(out)
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "BINARY" {
		t.Fatalf("extracted %q", data)
	}
	if _, err := extractFileFromTarGz(archive, "compiled/missing"); err == nil {
		t.Fatal("expected missing entry error")
	}
}

// A gateway reset on the blob upload (502 before headers) is retried after a
// pause with the full body: the publish succeeds and the registry sees the
// same bytes twice. A registry that keeps answering 502 still fails the leg.
func TestUploadArchiveBlobRetriesGatewayResetWithFullBody(t *testing.T) {
	prev := archiveBlobUploadRetryPause
	archiveBlobUploadRetryPause = time.Millisecond
	t.Cleanup(func() { archiveBlobUploadRetryPause = prev })

	payload := []byte("archive-bytes")
	dir := t.TempDir()
	filePath := filepath.Join(dir, "cli.tar.gz")
	if err := os.WriteFile(filePath, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	var bodies [][]byte
	mux := http.NewServeMux()
	mux.HandleFunc("POST /put/{namespace}/{package}/blobs", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, body)
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("upstream connect error or disconnect/reset before headers"))
			return
		}
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{"digest": digestOf(body), "size": len(body)})
	})
	srv := newPutTestServer(mux)
	defer srv.Close()

	publisher, err := newPutPublisher(srv.Client(), srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	digest, size, err := uploadArchiveBlobFile(context.Background(), publisher, "putnami", "cli", filePath, "application/gzip")
	if err != nil {
		t.Fatalf("upload after one 502: %v", err)
	}
	if digest != digestOf(payload) || size != int64(len(payload)) {
		t.Fatalf("digest/size = %s/%d, want %s/%d", digest, size, digestOf(payload), len(payload))
	}
	if len(bodies) != 2 {
		t.Fatalf("attempts = %d, want 2", len(bodies))
	}
	for i, body := range bodies {
		if !bytes.Equal(body, payload) {
			t.Fatalf("attempt %d body = %q, want the full archive", i+1, body)
		}
	}

	always := 0
	mux2 := http.NewServeMux()
	mux2.HandleFunc("POST /put/{namespace}/{package}/blobs", func(w http.ResponseWriter, _ *http.Request) {
		always++
		w.WriteHeader(http.StatusBadGateway)
	})
	srv2 := newPutTestServer(mux2)
	defer srv2.Close()
	publisher2, err := newPutPublisher(srv2.Client(), srv2.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := uploadArchiveBlobFile(context.Background(), publisher2, "putnami", "cli", filePath, "application/gzip"); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("persistent 502: err = %v, want a 502 upload error", err)
	}
	if always != archiveBlobUploadAttempts {
		t.Fatalf("attempts on a persistent 502 = %d, want %d", always, archiveBlobUploadAttempts)
	}
}
