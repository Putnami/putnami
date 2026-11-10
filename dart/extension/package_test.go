package dart

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	job "go.putnami.dev/protocol/job"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/oci"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/tooling/stacks/stackstest"
	"go.putnami.dev/tooling/stacks/stackstest/imagetest"
)

// stubDartCompile puts first on PATH a dart that logs its arguments, prints
// the version DART_STUB_VERSION names, 3.8.0 without one, and writes an
// executable at the path -o names for dart compile exe. It answers the log.
// The real dart compile exe is the SDK's: the stub stands at that boundary.
func stubDartCompile(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "dart.log")
	stackstest.WriteFile(t, bin, "dart", `#!/bin/sh
echo "$PWD: dart $*" >> "`+log+`"
if [ "$1" = --version ]; then echo "Dart SDK version: ${DART_STUB_VERSION:-3.8.0} (stable) (Wed May 14 09:07:14 2025 -0700) on \"linux_x64\""; exit 0; fi
if [ "$1 $2" = "compile exe" ]; then
  prev=""
  for a in "$@"; do
    if [ "$prev" = "-o" ]; then printf 'ELF' > "$a"; fi
    prev="$a"
  done
fi
exit ${STUB_EXIT:-0}
`)
	if err := os.Chmod(filepath.Join(bin, "dart"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	// The SDK on PATH runs: an offline run installs none.
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	return log
}

// serverPackage writes under root the pure Dart application server, with the
// executables server and migrate, the library lib/, a pure Dart library
// beside it, and a Flutter app.
func serverPackage(t *testing.T, root string) {
	t.Helper()
	stackstest.WriteFile(t, root, "server/pubspec.yaml", "name: server\nenvironment:\n  sdk: ^3.8.0\n")
	stackstest.WriteFile(t, root, "server/pubspec.lock", "packages: {}\n")
	stackstest.WriteFile(t, root, "server/lib/server.dart", "String greet() => 'hi';\n")
	stackstest.WriteFile(t, root, "server/bin/server.dart", "void main() {}\n")
	stackstest.WriteFile(t, root, "server/bin/migrate.dart", "void main() {}\n")
	stackstest.WriteFile(t, root, "model/pubspec.yaml", "name: model\nenvironment:\n  sdk: ^3.8.0\n")
	stackstest.WriteFile(t, root, "model/pubspec.lock", "packages: {}\n")
	stackstest.WriteFile(t, root, "model/lib/model.dart", "class Model {}\n")
	stackstest.WriteFile(t, root, "app/pubspec.yaml", "name: app\nenvironment:\n  sdk: ^3.8.0\n  flutter: \">=3.32.0\"\ndependencies:\n  flutter:\n    sdk: flutter\n")
	stackstest.WriteFile(t, root, "app/pubspec.lock", "packages: {}\n")
	stackstest.WriteFile(t, root, "app/lib/main.dart", "void main() {}\n")
	stackstest.WriteFile(t, root, "app/bin/tool.dart", "void main() {}\n")
}

// probeServer probes the packages serverPackage writes.
func probeServer(t *testing.T, root string) map[string]wsproto.ProbeProject {
	t.Helper()
	s := stack(t)
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{"server", "model", "app"}})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]wsproto.ProbeProject{}
	for _, p := range result.Projects {
		out[p.Path] = p
	}
	return out
}

// runPackageJob runs a job for the package at rel under root, named and
// typed as the probe answers it, with params and args.
func runPackageJob(t *testing.T, root, rel string, jobFunc cli.JobFunc, params job.Params, args ...string) (status string, err error) {
	t.Helper()
	p, ok := probeServer(t, root)[rel]
	if !ok {
		t.Fatalf("the probe answers no %s", rel)
	}
	ctx := &pctx.Context{WorkspaceRoot: root, Params: params, Project: pctx.Project{
		Name: p.SourceName, Path: rel, FullPath: filepath.Join(root, rel), Type: p.Type,
		Metadata: map[string]json.RawMessage{ExtensionName: p.Metadata},
	}}
	quiet(t, func() { status, _, err = jobFunc(ctx, jsonl.New(), args) })
	return status, err
}

// compileRun matches the dart compile exe a package job runs.
var compileRun = regexp.MustCompile(`: dart compile exe (\S+) -o (\S+) --target-os linux --target-arch (\S+)\n`)

func TestPackageAssemblesTheImageOfAnAOTExecutable(t *testing.T) {
	spectest.Proves(t, feature, "packages-an-application-image", "package-assembles-the-image-of-an-aot-executable")
	log := stubDartCompile(t)
	root := tempDir(t)
	serverPackage(t, root)
	host, base := imagetest.Registry(t)

	if status, err := runPackageJob(t, root, "server", PackageDockerJob(stack(t)), nil, "--docker-base-image", base); status != "OK" || err != nil {
		t.Fatalf("package: %s %v", status, err)
	}
	m := compileRun.FindStringSubmatch(ran(t, log))
	if m == nil || m[1] != "bin/server.dart" || filepath.Base(m[2]) != "server" || m[3] != "x64" {
		t.Fatalf("dart compile exe ran as %q", m)
	}
	if _, err := os.Stat(m[2]); err == nil {
		t.Errorf("the job left its compiled executable %s behind", m[2])
	}
	var manifest pkgmeta.DockerManifest
	data, err := os.ReadFile(filepath.Join(root, ".putnami/out/server/package/docker/manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Image != "server" || manifest.Digest == "" || manifest.Platform != defaultPlatform || manifest.Layout != "oci" {
		t.Fatalf("manifest = %+v", manifest)
	}
	index, err := pkgmeta.ReadChannelIndex(root, "server")
	if err != nil || index.Records[dockerChannel].Artifact != "server" {
		t.Errorf("channel records = %+v, %v", index, err)
	}

	spectest.Proves(t, feature, "publishes-an-application-image", "publish-pushes-the-application-image")
	if status, err := runPackageJob(t, root, "server", PublishDockerJob(), nil, "--docker-registry", host+"/acme"); status != "OK" || err != nil {
		t.Fatalf("publish: %s %v", status, err)
	}
	image := remoteImage(t, host+"/acme/server@"+manifest.Digest)
	assertImageLayout(t, image, "server", "amd64")
}

func TestPackageCompilesForTheArm64Platform(t *testing.T) {
	log := stubDartCompile(t)
	root := tempDir(t)
	serverPackage(t, root)
	host, base := imagetest.Registry(t)
	if status, err := runPackageJob(t, root, "server", PackageDockerJob(stack(t)), job.Params{"platform": json.RawMessage(`"linux/arm64"`)}, "--docker-base-image", base); status != "OK" || err != nil {
		t.Fatalf("package: %s %v", status, err)
	}
	if m := compileRun.FindStringSubmatch(ran(t, log)); m == nil || m[3] != "arm64" {
		t.Fatalf("dart compile exe ran as %q", m)
	}
	var manifest pkgmeta.DockerManifest
	data, err := os.ReadFile(filepath.Join(root, ".putnami/out/server/package/docker/manifest.json"))
	if err != nil || json.Unmarshal(data, &manifest) != nil || manifest.Platform != "linux/arm64" {
		t.Fatalf("manifest = %s, %v", data, err)
	}
	if status, err := runPackageJob(t, root, "server", PublishDockerJob(), nil, "--docker-registry", host+"/acme"); status != "OK" || err != nil {
		t.Fatalf("publish: %s %v", status, err)
	}
	assertImageLayout(t, remoteImage(t, host+"/acme/server@"+manifest.Digest), "server", "arm64")
}

// assertImageLayout checks the layout of an application image: its last layer
// holds the executable /app/<executable> alone, mode 0755, on the base's
// architecture, run as the nonroot user in /app with PORT set and exposed.
func assertImageLayout(t *testing.T, image v1.Image, executable, arch string) {
	t.Helper()
	layers, err := image.Layers()
	if err != nil || len(layers) < 2 {
		t.Fatalf("layers = %d, %v", len(layers), err)
	}
	entries := layerEntries(t, layers[len(layers)-1])
	if len(entries) != 1 || entries["app/"+executable] != 0o755 {
		t.Errorf("the application layer holds %v, want app/%s alone, mode 0755", entries, executable)
	}
	config, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if config.Architecture != arch || config.OS != "linux" {
		t.Errorf("platform = %s/%s, want linux/%s", config.OS, config.Architecture, arch)
	}
	c := config.Config
	if c.User != imageUser || c.WorkingDir != imageAppDir || !slices.Equal(c.Entrypoint, []string{"/app/" + executable}) || !slices.Contains(c.Env, "PORT="+defaultImagePort) {
		t.Errorf("config = %+v", c)
	}
	if _, ok := c.ExposedPorts[defaultImagePort+"/tcp"]; !ok {
		t.Errorf("the image exposes %v", c.ExposedPorts)
	}
}

// layerEntries answers the files of a layer by path, with their mode.
func layerEntries(t *testing.T, layer v1.Layer) map[string]int64 {
	t.Helper()
	content, err := layer.Uncompressed()
	if err != nil {
		t.Fatal(err)
	}
	defer content.Close()
	entries := map[string]int64{}
	reader := tar.NewReader(content)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return entries
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			entries[strings.TrimPrefix(header.Name, "/")] = header.Mode & 0o7777
		}
	}
}

func TestPackageCompilesTheEntrypointServeStarts(t *testing.T) {
	spectest.Proves(t, feature, "chooses-the-entrypoint", "package-compiles-the-entrypoint-serve-starts")
	dir := tempDir(t)
	for _, f := range []string{"bin/server.dart", "bin/shop.dart", "bin/migrate.dart", "tool/serve.dart"} {
		stackstest.WriteFile(t, dir, f, "void main() {}\n")
	}
	remove := func(rel string) {
		if err := os.Remove(filepath.Join(dir, rel)); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct{ named, want string }{
		{"", "bin/server.dart"},
		{"migrate", "bin/migrate.dart"},
		{"bin/migrate.dart", "bin/migrate.dart"},
		{"./tool/serve.dart", "tool/serve.dart"},
	} {
		if got, err := imageEntrypoint(dir, "shop", c.named); err != nil || got != c.want {
			t.Errorf("entrypoint %q = %q, %v, want %s", c.named, got, err, c.want)
		}
	}
	for _, named := range []string{"absent", "bin/absent.dart", "../shop/bin/server.dart", "shop:migrate", "bin"} {
		if got, err := imageEntrypoint(dir, "shop", named); err == nil {
			t.Errorf("entrypoint %q = %q, want an error", named, got)
		}
	}
	// With no bin/server.dart, the executable named after the package runs,
	// as dart run starts it.
	remove("bin/server.dart")
	if got, err := imageEntrypoint(dir, "shop", ""); err != nil || got != "bin/shop.dart" {
		t.Errorf("entrypoint = %q, %v, want bin/shop.dart", got, err)
	}
	// Several, none of them named after the package: no choice.
	if _, err := imageEntrypoint(dir, "store", ""); err == nil || !strings.Contains(err.Error(), "migrate, shop") {
		t.Errorf("several executables: %v", err)
	}
	remove("bin/shop.dart")
	if got, err := imageEntrypoint(dir, "store", ""); err != nil || got != "bin/migrate.dart" {
		t.Errorf("the only executable = %q, %v", got, err)
	}

	// The job compiles the entrypoint a project names, and fails when it
	// cannot choose one.
	log := stubDartCompile(t)
	root := tempDir(t)
	serverPackage(t, root)
	_, base := imagetest.Registry(t)
	if status, err := runPackageJob(t, root, "server", PackageDockerJob(stack(t)), job.Params{"entrypoint": json.RawMessage(`"bin/migrate.dart"`)}, "--docker-base-image", base); status != "OK" || err != nil {
		t.Fatalf("package: %s %v", status, err)
	}
	if m := compileRun.FindStringSubmatch(ran(t, log)); m == nil || m[1] != "bin/migrate.dart" || filepath.Base(m[2]) != "migrate" {
		t.Errorf("dart compile exe ran as %q", m)
	}
	if err := os.Remove(filepath.Join(root, "server/bin/server.dart")); err != nil {
		t.Fatal(err)
	}
	stackstest.WriteFile(t, root, "server/bin/seed.dart", "void main() {}\n")
	status, err := runPackageJob(t, root, "server", PackageDockerJob(stack(t)), nil, "--dry-run")
	if status != "FAILED" || err == nil || !strings.Contains(err.Error(), CodeNoEntrypoint) {
		t.Errorf("several executables and no bin/server.dart: %s %v", status, err)
	}
}

func TestPackageRefusesAnOldSDKAPlatformAndAnUnpinnedBase(t *testing.T) {
	spectest.Proves(t, feature, "refuses-what-it-cannot-cross-compile", "package-refuses-an-old-sdk-a-platform-and-an-unpinned-base")
	log := stubDartCompile(t)
	root := tempDir(t)
	serverPackage(t, root)
	_, base := imagetest.Registry(t)
	for name, c := range map[string]struct {
		version string
		args    []string
		code    string
	}{
		"a Dart before 3.8":         {version: "3.7.3", args: []string{"--docker-base-image", base}, code: CodeCrossCompileUnsupported},
		"a platform with no target": {args: []string{"--platform", "linux/s390x"}, code: CodePlatformUnsupported},
		"a base with no digest":     {args: []string{"--docker-base-image", "gcr.io/distroless/cc-debian12:nonroot"}, code: CodeBaseNotPinned},
	} {
		t.Run(name, func(t *testing.T) {
			if c.version != "" {
				t.Setenv("DART_STUB_VERSION", c.version)
				stackstest.WriteFile(t, root, "server/pubspec.yaml", "name: server\nenvironment:\n  sdk: ^3.7.0\n")
				t.Cleanup(func() {
					stackstest.WriteFile(t, root, "server/pubspec.yaml", "name: server\nenvironment:\n  sdk: ^3.8.0\n")
				})
			}
			status, err := runPackageJob(t, root, "server", PackageDockerJob(stack(t)), nil, c.args...)
			if status != "FAILED" || err == nil || !strings.Contains(err.Error(), c.code) {
				t.Errorf("%s: %s %v", name, status, err)
			}
			if strings.Contains(ran(t, log), "compile exe") {
				t.Errorf("%s compiled", name)
			}
		})
	}
	for version, admitted := range map[string]bool{"3.8.0": true, "3.13.7": true, "4.0.0": true, "3.9.0-100.0.dev": true, "3.7.3": false, "2.19.6": false, "3": false} {
		if atLeast(version, crossCompileSince) != admitted {
			t.Errorf("Dart %s cross-compiles: %v", version, !admitted)
		}
	}
	// The default base is distroless cc, pinned by digest.
	if ref, err := oci.ParseDigestReference(glibcBase); err != nil || !strings.HasPrefix(glibcBase, "gcr.io/distroless/cc-debian12:nonroot@sha256:") {
		t.Errorf("the default base %s: %v %v", glibcBase, ref, err)
	}
}

func TestADryRunPackageRecordsTheChannelAndCompilesNothing(t *testing.T) {
	log := stubDartCompile(t)
	root := tempDir(t)
	serverPackage(t, root)
	if status, err := runPackageJob(t, root, "server", PackageDockerJob(stack(t)), job.Params{"dryRun": json.RawMessage(`true`)}); status != "OK" || err != nil {
		t.Fatalf("dry run: %s %v", status, err)
	}
	if got := ran(t, log); strings.Contains(got, "compile exe") || !strings.Contains(got, "dart --version") {
		t.Errorf("a dry run checks the SDK and compiles nothing:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(root, ".putnami/out/server/package/docker/channel.json")); err != nil {
		t.Errorf("no channel record: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".putnami/out/server/package/docker/oci")); err == nil {
		t.Errorf("a dry run assembled an image")
	}
}

func TestALibraryOrAFlutterPackageNeitherPackagesNorPublishes(t *testing.T) {
	log := stubDartCompile(t)
	root := tempDir(t)
	serverPackage(t, root)
	for _, rel := range []string{"model", "app"} {
		if status, err := runPackageJob(t, root, rel, PackageDockerJob(stack(t)), nil); status != "SKIP" || err != nil {
			t.Errorf("package of %s: %s %v", rel, status, err)
		}
		if status, err := runPackageJob(t, root, rel, PublishDockerJob(), nil, "--docker"); status != "SKIP" || err != nil {
			t.Errorf("publish of %s: %s %v", rel, status, err)
		}
	}
	if got := ran(t, log); got != "" {
		t.Errorf("a package with no image ran %s", got)
	}
}

func TestAnApplicationThatPublishesAnImageIsAReleaseSetMember(t *testing.T) {
	root := tempDir(t)
	serverPackage(t, root)
	stackstest.WriteFile(t, root, "putnami.workspace.json", `{"registries": {"oci": {"publish": "ghcr.io/acme/apps"}}}`)
	members := func() map[string]string {
		out := map[string]string{}
		for path, p := range probeServer(t, root) {
			var metadata struct {
				ReleaseSet *struct {
					Ecosystems []struct {
						Ecosystem, Coordinate string
					} `json:"ecosystems"`
				} `json:"releaseSet"`
			}
			if err := json.Unmarshal(p.Metadata, &metadata); err != nil {
				t.Fatal(err)
			}
			if metadata.ReleaseSet != nil {
				for _, member := range metadata.ReleaseSet.Ecosystems {
					out[path] = member.Ecosystem + ":" + member.Coordinate
				}
			}
		}
		return out
	}
	if got := members(); len(got) != 0 {
		t.Errorf("an application that publishes no image is a member: %v", got)
	}
	for _, rel := range []string{"server", "model", "app"} {
		stackstest.WriteFile(t, root, rel+"/putnami.json", `{"options": {"publish": {"docker": true}}}`)
	}
	if got := members(); len(got) != 1 || got["server"] != "oci:acme/apps/server" {
		t.Errorf("members = %v", got)
	}
}

func remoteImage(t *testing.T, reference string) v1.Image {
	t.Helper()
	parsed, err := name.ParseReference(reference)
	if err != nil {
		t.Fatal(err)
	}
	image, err := remote.Image(parsed)
	if err != nil {
		t.Fatal(err)
	}
	return image
}
