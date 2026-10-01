package oci

import (
	"archive/tar"
	"bytes"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// writeFile creates a file with content under dir and returns its path.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// syntheticBase builds a small deterministic base image in-memory: one layer,
// an env, a user, and a CMD (to verify the entrypoint reset).
func syntheticBase(t *testing.T) v1.Image {
	t.Helper()
	dir := t.TempDir()
	base := writeFile(t, dir, "base-file", "base-content")

	var buf bytes.Buffer
	if err := writeLayerTar(&buf, Layer{Files: []File{{Source: base, Path: "/etc/base-file", Mode: 0o644}}}); err != nil {
		t.Fatal(err)
	}
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cfg := cf.Config
	cfg.Env = []string{"PATH=/usr/bin", "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt"}
	cfg.User = "nonroot"
	cfg.Cmd = []string{"/bin/base-cmd"}
	img, err = mutate.Config(img, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func testSpec(binaryPath string) Spec {
	return Spec{
		BaseRef:      "gcr.io/example/base:tag@sha256:0000000000000000000000000000000000000000000000000000000000000000",
		Platform:     "linux/amd64",
		Layers:       []Layer{{Files: []File{{Source: binaryPath, Path: "/app/my-app", Mode: 0o755}}}},
		Env:          []string{"PORT=3000", "PATH=/custom/bin"},
		Entrypoint:   []string{"/app/my-app"},
		WorkingDir:   "/app",
		ExposedPorts: []string{"3000/tcp"},
	}
}

func TestAssemble_Deterministic(t *testing.T) {
	dir := t.TempDir()
	binary := writeFile(t, dir, "my-app", "binary-content")
	base := syntheticBase(t)
	spec := testSpec(binary)

	imgA, err := Assemble(spec, base, t.TempDir())
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	imgB, err := Assemble(spec, base, t.TempDir())
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	digestA, _ := imgA.Digest()
	digestB, _ := imgB.Digest()
	if digestA != digestB {
		t.Errorf("expected identical digests for identical inputs, got %s vs %s", digestA, digestB)
	}
	const wantDigest = "sha256:350021ac61f26ae30b8008fdbfb7e002c0c38af0e6ffd846f9f785b1becb2100"
	if digestA.String() != wantDigest {
		t.Errorf("image digest = %s, want compatibility digest %s", digestA, wantDigest)
	}
}

func TestAssemble_InputSensitivity(t *testing.T) {
	dir := t.TempDir()
	binary := writeFile(t, dir, "my-app", "binary-v1")
	base := syntheticBase(t)
	spec := testSpec(binary)

	img, err := Assemble(spec, base, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	baseline, _ := img.Digest()

	// Content change.
	writeFile(t, dir, "my-app", "binary-v2")
	img, err = Assemble(spec, base, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	changed, _ := img.Digest()
	if changed == baseline {
		t.Error("expected digest change when file content changes")
	}

	// Mode change (same content).
	writeFile(t, dir, "my-app", "binary-v1")
	modeSpec := testSpec(binary)
	modeSpec.Layers[0].Files[0].Mode = 0o644
	img, err = Assemble(modeSpec, base, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	modeChanged, _ := img.Digest()
	if modeChanged == baseline {
		t.Error("expected digest change when file mode changes")
	}

	// Env change.
	envSpec := testSpec(binary)
	envSpec.Env = append(envSpec.Env, "EXTRA=1")
	img, err = Assemble(envSpec, base, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	envChanged, _ := img.Digest()
	if envChanged == baseline {
		t.Error("expected digest change when env changes")
	}
}

func TestAssemble_ConfigSemantics(t *testing.T) {
	dir := t.TempDir()
	binary := writeFile(t, dir, "my-app", "binary")
	base := syntheticBase(t)

	img, err := Assemble(testSpec(binary), base, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cfg := cf.Config

	// Env merge: same key overrides in place, new key appends, base-only keys survive.
	wantEnv := []string{"PATH=/custom/bin", "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt", "PORT=3000"}
	if strings.Join(cfg.Env, ";") != strings.Join(wantEnv, ";") {
		t.Errorf("env = %v, want %v", cfg.Env, wantEnv)
	}
	// Entrypoint replaces and resets the base CMD (docker build semantics).
	if len(cfg.Entrypoint) != 1 || cfg.Entrypoint[0] != "/app/my-app" {
		t.Errorf("entrypoint = %v", cfg.Entrypoint)
	}
	if cfg.Cmd != nil {
		t.Errorf("expected base CMD reset when entrypoint is set, got %v", cfg.Cmd)
	}
	// User inherited from base when spec leaves it empty.
	if cfg.User != "nonroot" {
		t.Errorf("user = %q, want inherited nonroot", cfg.User)
	}
	if cfg.WorkingDir != "/app" {
		t.Errorf("workingDir = %q", cfg.WorkingDir)
	}
	if _, ok := cfg.ExposedPorts["3000/tcp"]; !ok {
		t.Errorf("exposedPorts = %v, want 3000/tcp", cfg.ExposedPorts)
	}
	// No real timestamps anywhere.
	if !cf.Created.Time.Equal(epoch) {
		t.Errorf("created = %v, want epoch", cf.Created.Time)
	}
	for _, h := range cf.History {
		if !h.Created.Time.Equal(epoch) && !h.Created.Time.IsZero() {
			t.Errorf("history entry has a real timestamp: %+v", h)
		}
	}
}

func TestWriteLayerTar_DeterministicAndShaped(t *testing.T) {
	dir := t.TempDir()
	a := writeFile(t, dir, "a.css", "a")
	b := writeFile(t, dir, "b.js", "b")
	layer := Layer{Files: []File{
		// Deliberately unsorted; the writer must sort.
		{Source: b, Path: "/app/.gen/public/b.js", Mode: 0o644},
		{Source: a, Path: "/app/.gen/public/a.css", Mode: 0o644},
	}}

	var bufA, bufB bytes.Buffer
	if err := writeLayerTar(&bufA, layer); err != nil {
		t.Fatal(err)
	}
	if err := writeLayerTar(&bufB, layer); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bufA.Bytes(), bufB.Bytes()) {
		t.Error("expected byte-identical tars for identical inputs")
	}

	var names []string
	tr := tar.NewReader(bytes.NewReader(bufA.Bytes()))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
		if !hdr.ModTime.Equal(epoch) {
			t.Errorf("%s: mtime = %v, want epoch", hdr.Name, hdr.ModTime)
		}
		if hdr.Uid != 0 || hdr.Gid != 0 {
			t.Errorf("%s: ownership = %d:%d, want 0:0", hdr.Name, hdr.Uid, hdr.Gid)
		}
	}
	want := []string{"app/", "app/.gen/", "app/.gen/public/", "app/.gen/public/a.css", "app/.gen/public/b.js"}
	if strings.Join(names, ";") != strings.Join(want, ";") {
		t.Errorf("entries = %v, want %v", names, want)
	}
}

func TestContentHash_SkipsStampContentButNotShape(t *testing.T) {
	dir := t.TempDir()
	binary := writeFile(t, dir, "my-app", "binary")
	stamp := writeFile(t, dir, "stamp.json", `{"contentHash":"first"}`)

	spec := testSpec(binary)
	spec.Layers = append(spec.Layers, Layer{Files: []File{{Source: stamp, Path: "/app/.gen/version.json", Mode: 0o644}}})

	hashA, err := ContentHash(spec, "/app/.gen/version.json")
	if err != nil {
		t.Fatal(err)
	}
	// Stamp content changes (it embeds this very hash) — hash must not.
	writeFile(t, dir, "stamp.json", `{"contentHash":"second"}`)
	hashB, err := ContentHash(spec, "/app/.gen/version.json")
	if err != nil {
		t.Fatal(err)
	}
	if hashA != hashB {
		t.Error("expected stamp content to be excluded from the content hash")
	}

	// But removing the stamp from the spec changes the image shape → hash.
	noStamp := testSpec(binary)
	hashC, err := ContentHash(noStamp)
	if err != nil {
		t.Fatal(err)
	}
	if hashC == hashA {
		t.Error("expected spec shape (stamp path) to affect the content hash")
	}
}

func TestContentHash_FramesFileContents(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()

	specA := Spec{Layers: []Layer{{Files: []File{
		{Source: writeFile(t, dirA, "one", "x/two\x00"), Path: "/one", Mode: 0o644},
		{Source: writeFile(t, dirA, "two", "y"), Path: "/two", Mode: 0o644},
	}}}}
	specB := Spec{Layers: []Layer{{Files: []File{
		{Source: writeFile(t, dirB, "one", "x"), Path: "/one", Mode: 0o644},
		{Source: writeFile(t, dirB, "two", "/two\x00y"), Path: "/two", Mode: 0o644},
	}}}}

	hashA, err := ContentHash(specA)
	if err != nil {
		t.Fatal(err)
	}
	hashB, err := ContentHash(specB)
	if err != nil {
		t.Fatal(err)
	}
	if hashA == hashB {
		t.Error("expected different file splits to produce different content hashes")
	}
}

func TestTarballLayer_IsContentAddressedAndAssembles(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "toolchain.tar")
	writeTar := func(content string) {
		f, err := os.Create(tarPath)
		if err != nil {
			t.Fatal(err)
		}
		tw := tar.NewWriter(f)
		if err := tw.WriteHeader(&tar.Header{Name: "usr/local/bin/tool", Mode: 0o755, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	writeTar("first")
	spec := Spec{BaseRef: testSpec("").BaseRef, Platform: "linux/amd64", Layers: []Layer{{Tarball: tarPath}}}
	hashA, err := ContentHash(spec)
	if err != nil {
		t.Fatal(err)
	}
	writeTar("second")
	hashB, err := ContentHash(spec)
	if err != nil {
		t.Fatal(err)
	}
	if hashA == hashB {
		t.Fatal("tarball byte change did not change the complete content hash")
	}
	if _, err := Assemble(spec, syntheticBase(t), t.TempDir()); err != nil {
		t.Fatalf("Assemble tarball layer: %v", err)
	}
}

func TestParseDigestReference_RejectsMutableAndNonSHA256Refs(t *testing.T) {
	valid := "example.com/base@sha256:" + strings.Repeat("a", 64)
	if _, err := ParseDigestReference(valid); err != nil {
		t.Fatalf("valid digest rejected: %v", err)
	}
	for _, ref := range []string{"example.com/base:latest", "example.com/base@sha512:" + strings.Repeat("a", 64)} {
		if _, err := ParseDigestReference(ref); err == nil {
			t.Errorf("mutable/non-sha256 ref %q accepted", ref)
		}
	}
}

func TestLayoutRoundTripAndDockerTarball(t *testing.T) {
	dir := t.TempDir()
	binary := writeFile(t, dir, "my-app", "binary")
	base := syntheticBase(t)

	workDir := t.TempDir()
	img, err := Assemble(testSpec(binary), base, workDir)
	if err != nil {
		t.Fatal(err)
	}
	wantDigest, _ := img.Digest()

	layoutDir := filepath.Join(t.TempDir(), "oci")
	digest, err := WriteLayout(layoutDir, img)
	if err != nil {
		t.Fatalf("WriteLayout: %v", err)
	}
	if digest != wantDigest.String() {
		t.Errorf("WriteLayout digest = %s, want %s", digest, wantDigest)
	}

	loaded, err := LoadFromLayout(layoutDir)
	if err != nil {
		t.Fatalf("LoadFromLayout: %v", err)
	}
	loadedDigest, _ := loaded.Digest()
	if loadedDigest != wantDigest {
		t.Errorf("layout round-trip digest = %s, want %s", loadedDigest, wantDigest)
	}

	tarPath := filepath.Join(t.TempDir(), "image.tar")
	if err := WriteDockerTarball(layoutDir, "myapp:c-abc123", tarPath); err != nil {
		t.Fatalf("WriteDockerTarball: %v", err)
	}
	fromTar, err := tarball.ImageFromPath(tarPath, nil)
	if err != nil {
		t.Fatalf("reading docker tarball back: %v", err)
	}
	tarDigest, _ := fromTar.Digest()
	if tarDigest != wantDigest {
		t.Errorf("docker tarball digest = %s, want %s", tarDigest, wantDigest)
	}
}

func TestFetchBase_PlatformSelectionAndOfflineCache(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	host := strings.TrimPrefix(srv.URL, "http://")

	// Push a two-platform index so platform selection is exercised.
	amd64, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	arm64, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	idx := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{Add: amd64, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}},
		mutate.IndexAddendum{Add: arm64, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}}},
	)
	indexRef, err := name.ParseReference(host + "/base:latest")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(indexRef, idx); err != nil {
		t.Fatal(err)
	}
	indexDigest, err := idx.Digest()
	if err != nil {
		t.Fatal(err)
	}
	pinnedRef := host + "/base@" + indexDigest.String()

	cacheDir := t.TempDir()
	got, err := FetchBase(pinnedRef, "linux/arm64", cacheDir)
	if err != nil {
		t.Fatalf("FetchBase: %v", err)
	}
	gotDigest, _ := got.Digest()
	wantDigest, _ := arm64.Digest()
	if gotDigest != wantDigest {
		t.Errorf("platform selection picked %s, want arm64 child %s", gotDigest, wantDigest)
	}

	// The registry goes away; the cached base must keep working (offline).
	srv.Close()
	cached, err := FetchBase(pinnedRef, "linux/arm64", cacheDir)
	if err != nil {
		t.Fatalf("FetchBase from cache: %v", err)
	}
	cachedDigest, _ := cached.Digest()
	if cachedDigest != wantDigest {
		t.Errorf("cached base digest = %s, want %s", cachedDigest, wantDigest)
	}
}

func TestFetchBase_RejectsUnpinnedRef(t *testing.T) {
	if _, err := FetchBase("gcr.io/distroless/static:nonroot", "linux/amd64", t.TempDir()); err == nil {
		t.Error("expected error for a base ref without a digest pin")
	}
}

// TestAssembledImagePushable pushes an assembled image to an in-memory
// registry and reads it back — proving the publish-side transport needs no
// docker daemon and the digest survives the round trip.
func TestAssembledImagePushable(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	dir := t.TempDir()
	binary := writeFile(t, dir, "my-app", "binary")
	img, err := Assemble(testSpec(binary), syntheticBase(t), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wantDigest, _ := img.Digest()

	ref := host + "/app:c-abc123def456"
	tag, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(tag, img); err != nil {
		t.Fatalf("remote.Write: %v", err)
	}
	pushedDigest, err := crane.Digest(ref)
	if err != nil {
		t.Fatal(err)
	}
	if pushedDigest != wantDigest.String() {
		t.Errorf("pushed digest = %s, want %s", pushedDigest, wantDigest)
	}
}
