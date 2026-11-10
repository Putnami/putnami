//go:build !windows

package rust

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/putnamihome"
	"go.putnami.dev/tooling/stacks"
)

const testVersion = "1.2.3-20260902173000-8e6fb533"

// packageContext is the job context of the crate tools/gen of a fixture.
func packageContext(t *testing.T, root string, params pctx.Params, publish ...string) *pctx.Context {
	t.Helper()
	var meta json.RawMessage
	for _, p := range probe(t, root).Projects {
		if p.Path == "tools/gen" {
			meta = p.Metadata
		}
	}
	ctx := &pctx.Context{WorkspaceRoot: root, Version: &pctx.Version{Full: "v" + testVersion}, Params: params, Project: pctx.Project{
		Name: "gen", Path: "tools/gen", FullPath: filepath.Join(root, "tools/gen"), Type: typeApplication,
		Metadata: map[string]json.RawMessage{ExtensionName: meta},
	}}
	if len(publish) > 0 {
		raw, err := json.Marshal(publish)
		if err != nil {
			t.Fatal(err)
		}
		ctx.Project.Publish = raw
	}
	return ctx
}

// stubCompiler replaces the Rust and the cross tools with scripts. zigbuild
// writes the binary cargo would and logs its arguments. It returns the log.
func stubCompiler(t *testing.T, root, metadata string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "tools.log")
	if metadata == "" {
		metadata = `{"target_directory":"` + root + `/target","packages":[{"name":"gen","default_run":null,"targets":[{"name":"gen","kind":["bin"]}]}]}`
	}
	writeFile(t, dir, "cargo", "#!/bin/sh\ncat <<'JSON'\n"+metadata+"\nJSON\n")
	writeFile(t, dir, "cargo-zigbuild", `#!/bin/sh
echo "$*" >> "`+log+`"
prev=""
for a in "$@"; do
  case "$prev" in
    --bin) bin=$a;;
    --target) target=$a;;
  esac
  prev=$a
done
triple=${target%.2.36}
ext=""
case "$triple" in *windows*) ext=.exe;; esac
mkdir -p "`+root+`/target/$triple/release"
printf 'binary of %s' "$triple" > "`+root+`/target/$triple/release/$bin$ext"
`)
	for _, f := range []string{"cargo", "cargo-zigbuild"} {
		if err := os.Chmod(filepath.Join(dir, f), 0o755); err != nil { //nolint:gosec // a test script
			t.Fatal(err)
		}
	}
	saved := provideCompiler
	provideCompiler = func(_ context.Context, _ stacks.Expected, _, env []string, _ func(string)) (compiler, error) {
		return compiler{cargo: filepath.Join(dir, "cargo"), zigbuild: filepath.Join(dir, "cargo-zigbuild"), env: env}, nil
	}
	t.Cleanup(func() { provideCompiler = saved })
	return log
}

func runCompile(t *testing.T, root string, ctx *pctx.Context) (string, error) {
	t.Helper()
	status, _, err := Jobs(stack(t))["package-compile"](ctx, jsonl.New(), nil)
	return status, err
}

func TestPackageCompilesEachPlatformItsChannelsNeed(t *testing.T) {
	spectest.Proves(t, feature, "package-builds-each-platform", "package-compiles-each-platform-its-channels-need")
	root := fixture(t)
	stubTools(t, root, "1.82.0", junitPass)
	log := stubCompiler(t, root, "")
	base := "--package gen --bin gen --locked --release --target "
	for name, tc := range map[string]struct {
		params  map[string]any
		publish []string
		want    []string
	}{
		"docker": {params: map[string]any{"docker": true}, want: []string{"x86_64-unknown-linux-gnu.2.36"}},
		"docker on arm": {
			params: map[string]any{"docker": true, "platform": "linux/arm64"},
			want:   []string{"aarch64-unknown-linux-gnu.2.36"},
		},
		"chosen archives and the image": {
			params: map[string]any{"docker": true, "archives": true, "platforms": "linux/amd64, windows/amd64"},
			want:   []string{"x86_64-unknown-linux-gnu.2.36", "x86_64-pc-windows-gnu"},
		},
		"the publish field": {publish: []string{"archives"}, want: []string{
			"x86_64-unknown-linux-gnu.2.36", "aarch64-unknown-linux-gnu.2.36", "x86_64-apple-darwin", "aarch64-apple-darwin", "x86_64-pc-windows-gnu",
		}},
		"no binary channel": {},
	} {
		if err := os.Remove(log); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		ctx := packageContext(t, root, rawParams(t, tc.params), tc.publish...)
		if status, err := runCompile(t, root, ctx); err != nil || status != "OK" {
			t.Fatalf("%s: status = %s, err = %v", name, status, err)
		}
		logged, _ := os.ReadFile(log) //nolint:gosec // the test's own log
		var got []string
		for _, line := range strings.Split(strings.TrimSpace(string(logged)), "\n") {
			if line != "" {
				got = append(got, strings.TrimPrefix(line, "zigbuild "+base))
			}
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: compiled %v, want %v", name, got, tc.want)
		}
	}
	ctx := packageContext(t, root, rawParams(t, map[string]any{"archives": true, "platforms": "windows/amd64"}))
	if status, err := runCompile(t, root, ctx); err != nil || status != "OK" {
		t.Fatalf("status = %s, err = %v", status, err)
	}
	copied := filepath.Join(pkgmeta.PackageOutputDir(root, "tools/gen", "bin"), "windows-x64", "gen.exe")
	if data, err := os.ReadFile(copied); err != nil || string(data) != "binary of x86_64-pc-windows-gnu" { //nolint:gosec // the test's own file
		t.Errorf("the copied binary is %q, %v", data, err)
	}
}

func TestALibraryCompilesNothing(t *testing.T) {
	spectest.Proves(t, feature, "package-builds-each-platform", "package-compiles-each-platform-its-channels-need")
	root := fixture(t)
	stubTools(t, root, "1.82.0", junitPass)
	log := stubCompiler(t, root, "")
	ctx := packageContext(t, root, rawParams(t, map[string]any{"archives": true, "docker": true}))
	ctx.Project.Type = typeLibrary
	jobs := Jobs(stack(t))
	// In name order: package-crate stages what publish-crates sends.
	for _, task := range slices.Sorted(maps.Keys(jobs)) {
		if status, _, err := jobs[task](ctx, jsonl.New(), nil); err != nil || status != "OK" {
			t.Errorf("%s: status = %s, err = %v", task, status, err)
		}
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("a library compiled")
	}
	if _, err := os.Stat(pkgmeta.PackageOutputDir(root, "tools/gen", channelArchives)); err == nil {
		t.Error("a library wrote archives")
	}
}

func TestPackageRefusesWhatItCannotCompile(t *testing.T) {
	spectest.Proves(t, feature, "package-builds-each-platform", "package-compiles-each-platform-its-channels-need")
	root := fixture(t)
	stubTools(t, root, "1.82.0", junitPass)
	two := `{"target_directory":"x","packages":[{"name":"gen","targets":[{"name":"a","kind":["bin"]},{"name":"b","kind":["bin"]}]}]}`
	for name, tc := range map[string]struct {
		params   map[string]any
		metadata string
		want     string
	}{
		"unknown platform": {map[string]any{"archives": true, "platforms": "plan9/amd64"}, "", "does not know the platform"},
		"not linux":        {map[string]any{"docker": true, "platform": "darwin/arm64"}, "", "an image runs on Linux"},
		"unknown image":    {map[string]any{"docker": true, "platform": "plan9/amd64"}, "", "does not know the platform"},
		"two binaries":     {map[string]any{"docker": true}, two, "state package.default-run"},
		"no binary":        {map[string]any{"docker": true}, `{"packages":[{"name":"gen","targets":[{"name":"gen","kind":["lib"]}]}]}`, "no binary target"},
		"no package":       {map[string]any{"docker": true}, `{"packages":[]}`, "names no package"},
		"not json":         {map[string]any{"docker": true}, "nope", "cargo metadata"},
	} {
		stubCompiler(t, root, tc.metadata)
		_, err := runCompile(t, root, packageContext(t, root, rawParams(t, tc.params)))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestPackageChoosesTheDefaultRunOfSeveralBinaries(t *testing.T) {
	spectest.Proves(t, feature, "package-builds-each-platform", "package-compiles-each-platform-its-channels-need")
	root := fixture(t)
	stubTools(t, root, "1.82.0", junitPass)
	log := stubCompiler(t, root, `{"target_directory":"`+root+`/target","packages":[{"name":"gen","default_run":"b","targets":[{"name":"a","kind":["bin"]},{"name":"b","kind":["bin"]}]}]}`)
	if _, err := runCompile(t, root, packageContext(t, root, rawParams(t, map[string]any{"docker": true}))); err != nil {
		t.Fatal(err)
	}
	if logged, _ := os.ReadFile(log); !strings.Contains(string(logged), "--bin b ") { //nolint:gosec // the test's own log
		t.Errorf("zigbuild ran %s", logged)
	}
}

// wheelOf is a zip that holds an executable at path.
func wheelOf(t *testing.T, path string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	header := &zip.FileHeader{Name: path, Method: zip.Store}
	// The mode the wheel records stays 0644: the install makes it executable.
	header.SetMode(0o644)
	file, err := archive.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("#!/bin/sh\n")); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// stubCrossTools publishes the wheels on a local server and returns its count
// of downloads.
func stubCrossTools(t *testing.T) *atomic.Int32 {
	t.Helper()
	var downloads atomic.Int32
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	saved := crossTools
	t.Cleanup(func() { crossTools = saved })
	crossTools = nil
	for _, tool := range saved {
		wheelBytes := wheelOf(t, tool.subdir+"/"+tool.binary)
		digest := sha256.Sum256(wheelBytes)
		mux.HandleFunc("/"+tool.name, func(w http.ResponseWriter, _ *http.Request) {
			downloads.Add(1)
			_, _ = w.Write(wheelBytes)
		})
		tool.wheels = map[string]wheel{hostPlatform: {server.URL + "/" + tool.name, hex.EncodeToString(digest[:])}}
		crossTools = append(crossTools, tool)
	}
	return &downloads
}

func TestPackageInstallsZigbuildZigAndTheTargets(t *testing.T) {
	spectest.Proves(t, feature, "package-provides-its-cross-tools", "package-installs-zigbuild-zig-and-the-targets")
	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	log, _ := stubRustup(t, t.TempDir(), "1.82.0")
	downloads := stubCrossTools(t)
	expected := stacks.Expected{Version: "1.82.0"}
	targets := []string{"x86_64-unknown-linux-gnu", "x86_64-pc-windows-gnu"}
	tools, err := provideCompiler(context.Background(), expected, targets, []string{"PATH=/usr/bin", "RUSTUP_HOME=/machine"}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if downloads.Load() != 2 {
		t.Errorf("downloads = %d, want 2", downloads.Load())
	}
	info, err := os.Stat(tools.zigbuild)
	if err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("cargo-zigbuild: %v, %v", info, err)
	}
	ran, _ := os.ReadFile(log) //nolint:gosec // the test's own log
	name := "1.82.0-" + hostTriples[hostPlatform]
	if want := "target add --toolchain " + name + " " + strings.Join(targets, " "); !strings.Contains(string(ran), want) {
		t.Errorf("rustup ran %q, want %q", ran, want)
	}
	env := strings.Join(tools.env, "\n")
	if strings.Contains(env, "RUSTUP_HOME=/machine") || !strings.Contains(env, "RUSTUP_TOOLCHAIN="+name) {
		t.Errorf("env = %v", tools.env)
	}
	var path string
	for _, entry := range tools.env {
		if strings.HasPrefix(entry, "PATH=") {
			path = entry
		}
	}
	for _, want := range []string{filepath.Dir(tools.cargo), filepath.Dir(tools.zigbuild), filepath.Join("zig-"+zigVersion, "ziglang"), "/usr/bin"} {
		if !strings.Contains(path, want) {
			t.Errorf("PATH %s misses %s", path, want)
		}
	}

	// Installed once: the targets are there, and nothing downloads again.
	for _, target := range targets {
		if err := os.MkdirAll(filepath.Join(filepath.Dir(filepath.Dir(tools.cargo)), "lib", "rustlib", target), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	if _, err := provideCompiler(context.Background(), expected, targets, nil, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if downloads.Load() != 2 {
		t.Errorf("downloads = %d after the second install", downloads.Load())
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("rustup ran again for targets it has")
	}
}

func TestPackageNamesWhatIsMissingWithNoNetwork(t *testing.T) {
	spectest.Proves(t, feature, "package-provides-its-cross-tools", "package-installs-zigbuild-zig-and-the-targets")
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	t.Setenv(putnamihome.Env, t.TempDir())
	_, err := provideCompiler(context.Background(), stacks.Expected{Version: "1.82.0"}, []string{"x86_64-pc-windows-gnu"}, nil, func(string) {})
	for _, want := range []string{"needs a network", "Rust 1.82.0", "zig " + zigVersion, "cargo-zigbuild " + zigbuildVersion, "the Rust target x86_64-pc-windows-gnu"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
}

func TestPackageRefusesAToolWhoseDigestDiffers(t *testing.T) {
	spectest.Proves(t, feature, "package-provides-its-cross-tools", "package-installs-zigbuild-zig-and-the-targets")
	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	stubRustup(t, t.TempDir(), "1.82.0")
	stubCrossTools(t)
	crossTools[0].wheels[hostPlatform] = wheel{crossTools[0].wheels[hostPlatform].url, strings.Repeat("0", 64)}
	if _, err := provideCompiler(context.Background(), stacks.Expected{Version: "1.82.0"}, nil, nil, func(string) {}); err == nil || !strings.Contains(err.Error(), "install zig") {
		t.Fatalf("err = %v", err)
	}
	crossTools[0].wheels = nil
	if _, err := provideCompiler(context.Background(), stacks.Expected{Version: "1.82.0"}, nil, nil, func(string) {}); err == nil || !strings.Contains(err.Error(), "no zig is pinned") {
		t.Fatalf("err = %v", err)
	}
}

func TestProvideCompilerRefusesABadChannel(t *testing.T) {
	spectest.Proves(t, feature, "package-provides-its-cross-tools", "package-installs-zigbuild-zig-and-the-targets")
	if _, err := provideCompiler(context.Background(), stacks.Expected{Version: "--force"}, nil, nil, func(string) {}); err == nil {
		t.Fatal("a bad channel was accepted")
	}
}

func writeBinaries(t *testing.T, root string, suffixes ...string) {
	t.Helper()
	for _, suffix := range suffixes {
		name := "gen"
		if strings.HasPrefix(suffix, "windows") {
			name = "gen.exe"
		}
		writeFile(t, pkgmeta.PackageOutputDir(root, "tools/gen", "bin"), suffix+"/"+name, "binary "+suffix)
	}
}

func TestPackageWritesOneArchivePerPlatform(t *testing.T) {
	spectest.Proves(t, feature, "package-writes-archives", "package-writes-one-archive-per-platform")
	root := fixture(t)
	writeBinaries(t, root, pkgmeta.ArchivePlatformSuffixes()...)
	ctx := packageContext(t, root, rawParams(t, map[string]any{"archives": true}))
	ctx.Project.Name = "@acme/gen"
	if status, _, err := packageArchives(ctx, jsonl.New(), nil); err != nil || status != "OK" {
		t.Fatalf("status = %s, err = %v", status, err)
	}
	dir := pkgmeta.PackageOutputDir(root, "tools/gen", channelArchives)
	first := map[string][]byte{}
	for _, suffix := range pkgmeta.ArchivePlatformSuffixes() {
		archive := "acme-gen-" + suffix + ".tar.gz"
		data, err := os.ReadFile(filepath.Join(dir, archive)) //nolint:gosec // the test's own file
		if err != nil {
			t.Fatal(err)
		}
		first[archive] = data
		wantName := "gen"
		if strings.HasPrefix(suffix, "windows") {
			wantName = "gen.exe"
		}
		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		reader := tar.NewReader(gz)
		header, err := reader.Next()
		if err != nil {
			t.Fatal(err)
		}
		content, _ := io.ReadAll(reader)
		if header.Name != wantName || header.Mode != 0o755 || header.ModTime.Unix() != 0 || header.Uid != 0 || string(content) != "binary "+suffix {
			t.Errorf("%s: header %+v, content %q", archive, header, content)
		}
		if _, err := reader.Next(); err != io.EOF {
			t.Errorf("%s holds more than the binary", archive)
		}
	}
	// A second package gives the same bytes.
	if _, _, err := packageArchives(ctx, jsonl.New(), nil); err != nil {
		t.Fatal(err)
	}
	for archive, data := range first {
		again, _ := os.ReadFile(filepath.Join(dir, archive)) //nolint:gosec // the test's own file
		if !bytes.Equal(data, again) {
			t.Errorf("%s changed between two runs", archive)
		}
	}
	var metadata pkgmeta.PackageMetadata
	raw, err := os.ReadFile(pkgmeta.MetadataPath(root, "tools/gen"))
	if err != nil || json.Unmarshal(raw, &metadata) != nil {
		t.Fatalf("metadata: %v: %s", err, raw)
	}
	if metadata.Version != testVersion || metadata.Artifact != "acme-gen" || !slices.Equal(metadata.Channels, []string{"archives"}) {
		t.Errorf("metadata = %+v", metadata)
	}
	index, err := pkgmeta.ReadChannelIndex(root, "tools/gen")
	if err != nil || !index.HasChannel("archives") {
		t.Errorf("index = %+v, %v", index, err)
	}
}

func TestPackageArchivesFailsWithoutACompiledBinaryOrAVersion(t *testing.T) {
	spectest.Proves(t, feature, "package-writes-archives", "package-writes-one-archive-per-platform")
	root := fixture(t)
	ctx := packageContext(t, root, rawParams(t, map[string]any{"archives": true, "platforms": "linux/amd64"}))
	if _, _, err := packageArchives(ctx, jsonl.New(), nil); err == nil || !strings.Contains(err.Error(), "no binary for linux/amd64") {
		t.Errorf("err = %v", err)
	}
	writeFile(t, pkgmeta.PackageOutputDir(root, "tools/gen", "bin"), "linux-x64/a", "")
	writeFile(t, pkgmeta.PackageOutputDir(root, "tools/gen", "bin"), "linux-x64/b", "")
	if _, _, err := packageArchives(ctx, jsonl.New(), nil); err == nil || !strings.Contains(err.Error(), "want the one binary") {
		t.Errorf("err = %v", err)
	}
	ctx.Params = rawParams(t, map[string]any{"platforms": "plan9/amd64"})
	if _, _, err := packageArchives(ctx, jsonl.New(), nil); err == nil {
		t.Error("an unknown platform was accepted")
	}
	ctx.Version = nil
	if _, _, err := packageArchives(ctx, jsonl.New(), nil); err == nil || !strings.Contains(err.Error(), "release version is unknown") {
		t.Errorf("err = %v", err)
	}
	ctx.Version = &pctx.Version{Base: "1.0.0"}
	if version, err := releaseVersion(ctx); err != nil || version != "1.0.0" {
		t.Errorf("version = %s, %v", version, err)
	}
}

// baseImage publishes an empty image on a local registry and returns its
// digest-pinned reference.
func baseImage(t *testing.T) (string, string) {
	t.Helper()
	server := httptest.NewServer(registry.New())
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	ref, err := name.ParseReference(host + "/base:latest")
	if err != nil {
		t.Fatal(err)
	}
	// The image keeps the platform of its base.
	base, err := mutate.ConfigFile(empty.Image, &v1.ConfigFile{OS: "linux", Architecture: "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, base); err != nil {
		t.Fatal(err)
	}
	digest, err := base.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return host + "/base@" + digest.String(), host
}

func TestPackageAssemblesAnImageForThePlatform(t *testing.T) {
	spectest.Proves(t, feature, "package-assembles-an-image", "package-assembles-an-image-for-the-platform")
	root := fixture(t)
	writeBinaries(t, root, "linux-arm64")
	base, _ := baseImage(t)
	log := filepath.Join(t.TempDir(), "docker.log")
	stubs := t.TempDir()
	writeFile(t, stubs, "docker", "#!/bin/sh\necho \"$*\" >> \""+log+"\"\n")
	if err := os.Chmod(filepath.Join(stubs, "docker"), 0o755); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	t.Setenv("PATH", stubs+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx := packageContext(t, root, rawParams(t, map[string]any{
		"docker": true, "platform": "linux/arm64", "port": 8080, "docker-base-image": base, "docker-tag": "edge", "docker-load": true,
	}))
	ctx.Project.Name = "@acme/gen"
	status, data, err := packageDocker(ctx, jsonl.New(), nil)
	if err != nil || status != "OK" {
		t.Fatalf("status = %s, err = %v", status, err)
	}
	dir := pkgmeta.PackageOutputDir(root, "tools/gen", channelDocker)
	var manifest pkgmeta.DockerManifest
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json")) //nolint:gosec // the test's own file
	if err != nil || json.Unmarshal(raw, &manifest) != nil {
		t.Fatalf("manifest: %v: %s", err, raw)
	}
	content := "acme-gen:c-" + manifest.ContentHash
	if manifest.Image != "acme-gen" || !slices.Equal(manifest.Tags, []string{content, "acme-gen:edge"}) || manifest.Version != testVersion || manifest.Layout != "oci" || manifest.Platform != "linux/arm64" || manifest.Digest != data["digest"] {
		t.Errorf("manifest = %+v, data = %v", manifest, data)
	}
	index, err := layout.ImageIndexFromPath(filepath.Join(dir, "oci"))
	if err != nil {
		t.Fatal(err)
	}
	listed, err := index.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	image, err := index.Image(listed.Manifests[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	config, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if config.Architecture != "arm64" || !slices.Contains(config.Config.Env, "PORT=8080") || !slices.Equal(config.Config.Entrypoint, []string{"/app"}) {
		t.Errorf("config = %+v", config)
	}
	if _, ok := config.Config.ExposedPorts["8080/tcp"]; !ok {
		t.Errorf("ports = %v", config.Config.ExposedPorts)
	}
	logged, _ := os.ReadFile(log) //nolint:gosec // the test's own log
	if !strings.Contains(string(logged), "load -i ") || !strings.Contains(string(logged), "tag "+content+" acme-gen:edge") {
		t.Errorf("docker ran %s", logged)
	}
	index2, err := pkgmeta.ReadChannelIndex(root, "tools/gen")
	if err != nil || !index2.HasChannel("docker") {
		t.Errorf("index = %+v, %v", index2, err)
	}

	// The same binary gives the same image.
	_, again, err := packageDocker(ctx, jsonl.New(), nil)
	if err != nil || again["digest"] != data["digest"] {
		t.Errorf("digest %v, then %v: %v", data["digest"], again["digest"], err)
	}
}

func TestPackageDockerFailsWithAnUnknownPlatformOrNoBinary(t *testing.T) {
	spectest.Proves(t, feature, "package-assembles-an-image", "package-assembles-an-image-for-the-platform")
	root := fixture(t)
	ctx := packageContext(t, root, rawParams(t, map[string]any{"platform": "windows/amd64"}))
	if _, _, err := packageDocker(ctx, jsonl.New(), nil); err == nil || !strings.Contains(err.Error(), "an image runs on Linux") {
		t.Errorf("err = %v", err)
	}
	ctx.Params = rawParams(t, map[string]any{})
	if _, _, err := packageDocker(ctx, jsonl.New(), nil); err == nil || !strings.Contains(err.Error(), "no binary for linux/amd64") {
		t.Errorf("err = %v", err)
	}
	ctx.Version = nil
	if _, _, err := packageDocker(ctx, jsonl.New(), nil); err == nil {
		t.Error("no version was accepted")
	}
}
