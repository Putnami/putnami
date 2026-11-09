package swift

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

// stubSwiftBuild puts first on PATH a swift that logs its arguments, prints
// the version SWIFT_STUB_RELEASE names in the swift.org format, lists the SDKs
// it installed, and writes the executable Server and a resource bundle where a
// release build of the static SDK puts them. It answers the log.
func stubSwiftBuild(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "swift.log")
	state := t.TempDir()
	writeFile(t, bin, "swift", `#!/bin/sh
echo "$PWD: swift $*" >> "`+log+`"
case "$1" in
--version) echo "${SWIFT_STUB_BANNER:-Swift version ${SWIFT_STUB_RELEASE:-6.4.2} (swift-${SWIFT_STUB_RELEASE:-6.4.2}-RELEASE)}"; echo "Target: x86_64-unknown-linux-gnu";;
sdk) case "$2" in
  list) if [ -f "`+state+`/sdk" ]; then cat "`+state+`/sdk"; else echo "No Swift SDKs are currently installed."; fi;;
  install) echo "swift-${SWIFT_STUB_RELEASE:-6.4.2}-RELEASE_static-linux-0.1.0" > "`+state+`/sdk";;
  esac;;
build) out="$PWD/.build/out/Products/Release-staticlinux-x86_64"
  for a in "$@"; do if [ "$a" = "--show-bin-path" ]; then echo "Building for production..."; echo "$out"; exit 0; fi; done
  mkdir -p "$out/Server_Server.resources/Public" "$out/Server.swiftmodule"
  printf 'ELF' > "$out/Server"; chmod 755 "$out/Server"
  echo "body {}" > "$out/Server_Server.resources/Public/app.css"
  : > "$out/Server.swiftmodule/x";;
esac
`)
	if err := os.Chmod(filepath.Join(bin, "swift"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// serverPackage writes the executable package server under root.
func serverPackage(t *testing.T, root string) {
	t.Helper()
	writeFile(t, root, "server/Package.swift", `// swift-tools-version:6.0
import PackageDescription

let package = Package(
    name: "Server",
    products: [.executable(name: "Server", targets: ["Server"])],
    targets: [.executableTarget(name: "Server")]
)
`)
	writeFile(t, root, "server/Sources/Server/main.swift", "print(\"up\")\n")
}

func TestPackageAssemblesTheImageOfAStaticExecutable(t *testing.T) {
	spectest.Proves(t, feature, "packages-an-application-image", "package-assembles-the-image-of-a-static-executable")
	log := stubSwiftBuild(t)
	root := t.TempDir()
	serverPackage(t, root)
	host, base := imageRegistry(t)
	// A bundle an earlier build of another product left in the bin path.
	writeFile(t, root, "server/.build/out/Products/Release-staticlinux-x86_64/Server_Admin.resources/admin.css", "a {}\n")

	status, _, err := runServerJob(t, root, PackageDockerJob(stack(t)), "--docker-base-image", base)
	if status != "OK" || err != nil {
		t.Fatalf("package: %s %v", status, err)
	}
	ran := readFileString(t, log)
	sdk := staticSDKs["6.4.2"]
	for _, want := range []string{
		"swift sdk install https://download.swift.org/swift-6.4.2-release/static-sdk/swift-6.4.2-RELEASE/swift-6.4.2-RELEASE_static-linux-0.1.0.artifactbundle.tar.gz --checksum " + sdk.checksum + "\n",
		"swift build -c release --swift-sdk swift-6.4.2-RELEASE_static-linux-0.1.0 --triple x86_64-swift-linux-musl --force-resolved-versions --product Server -Xlinker -s\n",
	} {
		if !strings.Contains(ran, want) {
			t.Errorf("swift did not run %q:\n%s", want, ran)
		}
	}
	var manifest pkgmeta.DockerManifest
	data, err := os.ReadFile(filepath.Join(root, ".putnami/out/server/package/docker/manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Image != "server" || manifest.Digest == "" || manifest.Platform != defaultPlatform {
		t.Fatalf("manifest = %+v", manifest)
	}

	// An installed SDK is not installed again.
	if status, _, err := runServerJob(t, root, PackageDockerJob(stack(t)), "--docker-base-image", base); status != "OK" || err != nil {
		t.Fatalf("package again: %s %v", status, err)
	}
	if n := strings.Count(readFileString(t, log), "swift sdk install"); n != 1 {
		t.Errorf("the SDK was installed %d times", n)
	}

	spectest.Proves(t, feature, "publishes-an-application-image", "publish-pushes-the-application-image")
	status, _, err = runServerJob(t, root, PublishDockerJob(), "--docker-registry", host+"/acme")
	if status != "OK" || err != nil {
		t.Fatalf("publish: %s %v", status, err)
	}
	image := remoteImage(t, host+"/acme/server@"+manifest.Digest)
	files := imageFiles(t, image)
	for _, want := range []string{"app/Server", "app/Server_Server.resources/Public/app.css"} {
		if !files[want] {
			t.Errorf("the image has no %s", want)
		}
	}
	if files["app/Server.swiftmodule/x"] || files["app/Server_Admin.resources/admin.css"] {
		t.Errorf("the image holds build products other than the executable and its resources: %v", files)
	}
	config, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if config.Config.User != imageUser || !slices.Equal(config.Config.Entrypoint, []string{"/app/Server"}) || !slices.Contains(config.Config.Env, "PORT="+defaultImagePort) {
		t.Errorf("config = %+v", config.Config)
	}
	if _, ok := config.Config.ExposedPorts[defaultImagePort+"/tcp"]; !ok {
		t.Errorf("the image exposes %v", config.Config.ExposedPorts)
	}
}

func TestPackageBuildsForTheArm64Platform(t *testing.T) {
	log := stubSwiftBuild(t)
	root := t.TempDir()
	serverPackage(t, root)
	_, base := imageRegistry(t)
	if status, _, err := runServerJob(t, root, PackageDockerJob(stack(t)), "--docker-base-image", base, "--platform", "linux/arm64"); status != "OK" || err != nil {
		t.Fatalf("package: %s %v", status, err)
	}
	if !strings.Contains(readFileString(t, log), "--swift-sdk swift-6.4.2-RELEASE_static-linux-0.1.0 --triple aarch64-swift-linux-musl --force-resolved-versions --product Server") {
		t.Errorf("swift ran %s", readFileString(t, log))
	}
}

func TestPackageRefusesWhatItCannotBuildStatic(t *testing.T) {
	stubSwiftBuild(t)
	root := t.TempDir()
	serverPackage(t, root)
	for name, c := range map[string]struct {
		env  map[string]string
		args []string
		code string
	}{
		"the Swift of Xcode": {
			env:  map[string]string{"SWIFT_STUB_BANNER": "Apple Swift version 6.2 (swiftlang-6.2.0.19.9 clang-1700.3.19.1)"},
			code: CodeStaticSDKUnavailable,
		},
		"a release with no pin": {env: map[string]string{"SWIFT_STUB_RELEASE": "5.10.1"}, code: CodeStaticSDKUnavailable},
		"a base with no digest": {args: []string{"--docker-base-image", "gcr.io/distroless/static:nonroot"}, code: CodeBaseNotPinned},
		"a platform with no SDK target": {
			args: []string{"--platform", "linux/s390x"},
			code: CodeImageFailed,
		},
	} {
		t.Run(name, func(t *testing.T) {
			for key, value := range c.env {
				t.Setenv(key, value)
			}
			status, _, err := runServerJob(t, root, PackageDockerJob(stack(t)), c.args...)
			if status != "FAILED" || err == nil || !strings.Contains(err.Error(), c.code) {
				t.Errorf("%s: %s %v", name, status, err)
			}
		})
	}
}

func TestStaticSDKForNamesThePinnedBundle(t *testing.T) {
	sdk, err := staticSDKFor("6.3")
	if err != nil {
		t.Fatal(err)
	}
	if sdk.ID != "swift-6.3-RELEASE_static-linux-0.1.0" ||
		sdk.URL != "https://download.swift.org/swift-6.3-release/static-sdk/swift-6.3-RELEASE/swift-6.3-RELEASE_static-linux-0.1.0.artifactbundle.tar.gz" ||
		sdk.Checksum != staticSDKs["6.3"].checksum {
		t.Errorf("sdk = %+v", sdk)
	}
	if _, err := staticSDKFor("6.9.9"); err == nil || !strings.Contains(err.Error(), "6.4.2") {
		t.Errorf("an unpinned release names the pinned ones: %v", err)
	}
}

func TestADryRunPackageRecordsTheChannelAndRunsNoSwift(t *testing.T) {
	log := stubSwiftBuild(t)
	root := t.TempDir()
	serverPackage(t, root)
	if status, _, err := runServerJob(t, root, PackageDockerJob(stack(t)), "--dry-run"); status != "OK" || err != nil {
		t.Fatalf("dry run: %s %v", status, err)
	}
	if ran, _ := os.ReadFile(log); strings.Contains(string(ran), "swift build") || strings.Contains(string(ran), "swift sdk") {
		t.Errorf("a dry run ran %s", ran)
	}
	if _, err := os.Stat(filepath.Join(root, ".putnami/out/server/package/docker/channel.json")); err != nil {
		t.Errorf("no channel record: %v", err)
	}
}

func TestOnlyAnApplicationPackagesAndPublishes(t *testing.T) {
	for _, kind := range []string{"library", ""} {
		ctx := &pctx.Context{Project: pctx.Project{Name: "lib", Path: "lib", Type: kind}}
		if status, _, err := PackageDockerJob(stack(t))(ctx, jsonl.New(), nil); status != "SKIP" || err != nil {
			t.Errorf("package of %q: %s %v", kind, status, err)
		}
		if status, _, err := PublishDockerJob()(ctx, jsonl.New(), []string{"--docker"}); status != "SKIP" || err != nil {
			t.Errorf("publish of %q: %s %v", kind, status, err)
		}
	}
}

func TestAnApplicationThatPublishesAnImageIsAReleaseSetMember(t *testing.T) {
	root := t.TempDir()
	serverPackage(t, root)
	writeFile(t, root, "lib/Package.swift", `let package = Package(name: "Lib", targets: [.target(name: "Lib")])`)
	writeFile(t, root, "putnami.workspace.json", `{"registries": {"oci": {"publish": "ghcr.io/acme/apps"}}}`)
	members := func() map[string]string {
		s := stack(t)
		result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{"server", "lib"}})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, p := range result.Projects {
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
					out[p.Path] = member.Ecosystem + ":" + member.Coordinate
				}
			}
		}
		return out
	}
	if got := members(); len(got) != 0 {
		t.Errorf("an application that publishes no image is a member: %v", got)
	}
	writeFile(t, root, "server/putnami.json", `{"options": {"publish": {"docker": true}}}`)
	writeFile(t, root, "lib/putnami.json", `{"options": {"publish": {"docker": true}}}`)
	if got := members(); len(got) != 1 || got["server"] != "oci:acme/apps/server" {
		t.Errorf("members = %v", got)
	}
}

// imageRegistry starts a registry double holding a two-platform base image, and
// answers its host and the digest reference of the base.
func imageRegistry(t *testing.T) (string, string) {
	t.Helper()
	server := httptest.NewServer(registry.New())
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	amd64, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	arm64, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	index := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{Add: amd64, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}},
		mutate.IndexAddendum{Add: arm64, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}}},
	)
	reference, err := name.ParseReference(host + "/base:latest")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(reference, index); err != nil {
		t.Fatal(err)
	}
	digest, err := index.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return host, host + "/base@" + digest.String()
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

// imageFiles answers the paths of the files in an image.
func imageFiles(t *testing.T, image v1.Image) map[string]bool {
	t.Helper()
	flat := mutate.Extract(image)
	defer flat.Close()
	files := map[string]bool{}
	reader := tar.NewReader(flat)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		files[strings.TrimPrefix(header.Name, "/")] = true
	}
}

// runServerJob runs a job for the application server under root.
func runServerJob(t *testing.T, root string, jobFunc cli.JobFunc, args ...string) (string, map[string]any, error) {
	t.Helper()
	s := stack(t)
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{"server"}})
	if err != nil || len(result.Projects) != 1 {
		t.Fatalf("probe: %v %+v", err, result)
	}
	ctx := &pctx.Context{WorkspaceRoot: root, Project: pctx.Project{
		Name: "server", Path: "server", FullPath: filepath.Join(root, "server"), Type: result.Projects[0].Type,
		Metadata: map[string]json.RawMessage{ExtensionName: result.Projects[0].Metadata},
	}}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	stdout := os.Stdout
	os.Stdout = devnull
	defer func() { os.Stdout = stdout }()
	return jobFunc(ctx, jsonl.New(), args)
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
