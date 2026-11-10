package dotnet

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"go.putnami.dev/protocol/features/spectest"
	job "go.putnami.dev/protocol/job"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/tooling/stacks"
	"go.putnami.dev/tooling/stacks/stackstest"
)

const webProject = `<Project Sdk="Microsoft.NET.Sdk.Web">
  <PropertyGroup><TargetFramework>net8.0</TargetFramework></PropertyGroup>
  <ItemGroup><ProjectReference Include="..\Pricing\Pricing.csproj" /></ItemGroup>
</Project>
`

// stubApp puts first on PATH a dotnet that prints 8.0.404 for --version,
// logs every other call with the port variables, answers msbuild with props,
// writes a published web application for publish, and exits with STUB_EXIT
// for run. The real SDK is the stack's own build: the stub stands at that
// boundary.
func stubApp(t *testing.T, props string) string {
	t.Helper()
	isolateSDKs(t)
	bin := t.TempDir()
	log := filepath.Join(bin, "dotnet.log")
	stackstest.WriteFile(t, bin, "props.json", props)
	stackstest.WriteFile(t, bin, "dotnet", `#!/bin/sh
if [ "$1" = "--version" ]; then echo 8.0.404; exit 0; fi
echo "$PWD: dotnet $* PORT=$PORT URLS=$ASPNETCORE_URLS" >> "`+log+`"
case "$1" in
  msbuild) cat "`+filepath.Join(bin, "props.json")+`"; exit 0;;
  publish)
    prev=""
    for a in "$@"; do
      if [ "$prev" = "--output" ]; then
        mkdir -p "$a/wwwroot"
        echo dll > "$a/Api.dll"; echo '{}' > "$a/Api.deps.json"; echo '{}' > "$a/appsettings.json"
        echo lib > "$a/Pricing.dll"; echo css > "$a/wwwroot/site.css"
        echo '{}' > "$a/putnami.json"; echo '{}' > "$a/global.json"; mkdir -p "$a/schema"; echo '{}' > "$a/schema/http-routes.json"
        echo '{"runtimeOptions": {"frameworks": [{"name": "Microsoft.NETCore.App"}, {"name": "'"${STUB_FRAMEWORK:-Microsoft.AspNetCore.App}"'"}]}}' > "$a/Api.runtimeconfig.json"
      fi
      prev="$a"
    done;;
  run) exit ${STUB_EXIT:-0};;
esac
`)
	if err := os.Chmod(filepath.Join(bin, "dotnet"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

const webProps = `{"Properties": {"TargetFramework": "net8.0", "TargetFrameworks": "", "AssemblyName": "Api", "UsingMicrosoftNETSdkWeb": "true"}}`

// appFixture is the fixture with src/Api as a web application.
func appFixture(t *testing.T) string {
	t.Helper()
	// The stub logs the directory a step runs in as the shell resolves it.
	root, err := filepath.EvalSymlinks(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	stackstest.WriteFile(t, root, "src/Api/Api.csproj", webProject)
	return root
}

// runAppJob runs a job for the project at rel as the CLI does, with params
// and the job arguments.
func runAppJob(t *testing.T, root, rel string, jobFunc cli.JobFunc, params job.Params, args ...string) (string, map[string]any, error) {
	t.Helper()
	var project pctx.Project
	for _, p := range probe(t, root).Projects {
		if p.Path == rel {
			project = pctx.Project{Name: p.SourceName, Path: rel, FullPath: filepath.Join(root, rel), Type: p.Type,
				Metadata: map[string]json.RawMessage{ExtensionName: p.Metadata}}
		}
	}
	if project.Path == "" {
		t.Fatalf("no project %s", rel)
	}
	ctx := &pctx.Context{WorkspaceRoot: root, Params: params, Project: project}
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

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestAWebOrExecutableProjectIsAnApplication(t *testing.T) {
	spectest.Proves(t, feature, "applications-are-told-apart", "a-web-or-executable-project-is-an-application")
	root := appFixture(t)
	stackstest.WriteFile(t, root, "src/Api/putnami.json", `{"options": {"publish": {"docker": true}}}`)
	stackstest.WriteFile(t, root, "putnami.workspace.json", `{"registries": {"oci": {"publish": "registry.test/acme"}}}`)
	types := map[string]string{}
	var member []releaseset.MemberDeclaration
	for _, p := range probe(t, root).Projects {
		types[p.Path] = p.Type
		var meta stacks.Metadata
		if err := json.Unmarshal(p.Metadata, &meta); err != nil {
			t.Fatal(err)
		}
		if meta.ReleaseSet != nil {
			member = append(member, meta.ReleaseSet.Ecosystems...)
		}
	}
	if types["src/Api"] != "application" || types["src/Pricing"] != "library" || types["tests/Pricing.Tests"] != "library" {
		t.Errorf("types = %v", types)
	}
	if len(member) != 1 || member[0].Ecosystem != "oci" || member[0].Coordinate != "acme/api" {
		t.Errorf("release-set members = %+v", member)
	}
}

func TestServeRunsDotnetRunOnThePort(t *testing.T) {
	spectest.Proves(t, feature, "serves-an-application", "serve-runs-dotnet-run-on-the-port")
	root := appFixture(t)
	log := stubApp(t, webProps)
	for _, params := range []job.Params{nil, {"port": json.RawMessage(`8081`)}} {
		if status, _, err := runAppJob(t, root, "src/Api", ServeJob(stack(t)), params); status != "OK" || err != nil {
			t.Fatalf("serve: %s %v", status, err)
		}
	}
	api := filepath.Join(root, "src/Api")
	lines := readLines(t, log)
	want := api + ": dotnet run --no-build --project " + filepath.Join(api, "Api.csproj") + " --no-launch-profile PORT="
	build := api + ": dotnet build " + filepath.Join(api, "Api.csproj")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], build) || !strings.HasPrefix(lines[1], want+"3000 URLS=http://0.0.0.0:3000") || !strings.HasPrefix(lines[3], want+"8081 URLS=http://0.0.0.0:8081") {
		t.Errorf("dotnet ran with %q", lines)
	}
	t.Setenv("STUB_EXIT", "5")
	if status, data, err := runAppJob(t, root, "src/Api", ServeJob(stack(t)), nil); status != "FAILED" || err != nil || data[exitCodeKey] != 5 {
		t.Errorf("a server that exits on its own fails: %s %v %v", status, data, err)
	}
}

func TestRunForwardsTheExitCodeOfTheApplication(t *testing.T) {
	spectest.Proves(t, feature, "runs-an-application-once", "run-forwards-the-exit-code-of-the-application")
	root := appFixture(t)
	log := stubApp(t, webProps)
	t.Setenv("STUB_EXIT", "7")
	status, data, err := runAppJob(t, root, "src/Api", RunJob(stack(t)), job.Params{"args": json.RawMessage(`"migrate --dry-run"`)})
	if status != "FAILED" || err != nil || data[exitCodeKey] != 7 {
		t.Fatalf("run: %s %v %v", status, data, err)
	}
	t.Setenv("STUB_EXIT", "")
	if status, data, err := runAppJob(t, root, "src/Api", RunJob(stack(t)), nil); status != "OK" || err != nil || data[exitCodeKey] != 0 {
		t.Fatalf("run: %s %v %v", status, data, err)
	}
	lines := readLines(t, log)
	manifest := filepath.Join(root, "src/Api/Api.csproj")
	if len(lines) != 4 || !strings.Contains(lines[0], "dotnet build "+manifest) || !strings.Contains(lines[1], "dotnet run --no-build --project "+manifest+" --no-launch-profile -- migrate --dry-run PORT=") ||
		!strings.Contains(lines[3], "dotnet run --no-build --project "+manifest+" --no-launch-profile PORT=") {
		t.Errorf("dotnet ran with %q", lines)
	}
}

func TestPackageAssemblesAnImageOfThePublishedApplication(t *testing.T) {
	spectest.Proves(t, feature, "packages-an-application-image", "package-assembles-an-image-of-the-published-application")
	root := appFixture(t)
	log := stubApp(t, webProps)
	host, base := stackstest.ImageRegistry(t)
	status, _, err := runAppJob(t, root, "src/Api", PackageDockerJob(stack(t)), nil, "--docker-base-image", base, "--platform", "linux/arm64")
	if status != "OK" || err != nil {
		t.Fatalf("package: %s %v", status, err)
	}
	lines := readLines(t, log)
	if len(lines) != 2 || !strings.Contains(lines[1], "dotnet publish "+filepath.Join(root, "src/Api/Api.csproj")+" --configuration Release --framework net8.0 --runtime linux-arm64 --self-contained false -p:UseAppHost=false --output ") {
		t.Fatalf("dotnet ran with %q", lines)
	}
	var manifest pkgmeta.DockerManifest
	data, err := os.ReadFile(filepath.Join(root, ".putnami/out/src/Api/package/docker/manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil || manifest.Image != "api" || manifest.Digest == "" || manifest.Platform != "linux/arm64" {
		t.Fatalf("manifest = %+v %v", manifest, err)
	}

	spectest.Proves(t, feature, "publishes-an-application-image", "publish-pushes-the-application-image")
	status, _, err = runAppJob(t, root, "src/Api", PublishDockerJob(), nil, "--docker-registry", host+"/acme")
	if status != "OK" || err != nil {
		t.Fatalf("publish: %s %v", status, err)
	}
	reference, err := name.ParseReference(host + "/acme/api@" + manifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	image, err := remote.Image(reference)
	if err != nil {
		t.Fatal(err)
	}
	files := imageLayerFiles(t, image)
	for _, want := range []string{"app/Api.dll", "app/Pricing.dll", "app/appsettings.json", "app/wwwroot/site.css"} {
		if !files[want] {
			t.Errorf("the image has no %s: %v", want, files)
		}
	}
	if files["app/putnami.json"] || files["app/global.json"] || files["app/schema/http-routes.json"] {
		t.Errorf("the image holds Putnami's project file or the SDK pin: %v", files)
	}
	config, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(config.Config.Entrypoint, " ") != "dotnet /app/Api.dll" || config.Config.User != "app" || config.Config.WorkingDir != "/app" ||
		!strings.Contains(strings.Join(config.Config.Env, " "), "ASPNETCORE_HTTP_PORTS=8080") {
		t.Errorf("config = %+v", config.Config)
	}

	// A library packages nothing; a base with no digest fails.
	if status, _, err := runAppJob(t, root, "src/Pricing", PackageDockerJob(stack(t)), nil); status != "SKIP" || err != nil {
		t.Errorf("a library packages nothing: %s %v", status, err)
	}
	if status, _, err := runAppJob(t, root, "src/Api", PackageDockerJob(stack(t)), nil, "--docker-base-image", host+"/base:latest"); status != "FAILED" || err == nil || !strings.HasPrefix(err.Error(), CodeBaseNotPinned) {
		t.Errorf("a base with no digest fails: %s %v", status, err)
	}
}

func TestTheOfficialBaseFollowsTheFrameworkAndTheSDK(t *testing.T) {
	for _, c := range []struct {
		web       bool
		framework string
		want      string
	}{
		{true, "net8.0", runtimeBases["aspnet"]["8.0"]},
		{false, "net10.0", runtimeBases["runtime"]["10.0"]},
	} {
		if got, err := baseImage(c.web, c.framework, ""); err != nil || got != c.want {
			t.Errorf("baseImage(%v, %s) = %s %v", c.web, c.framework, got, err)
		}
	}
	if _, err := baseImage(true, "net48", ""); err == nil {
		t.Errorf("a framework with no pinned image is an error")
	}
	if _, err := targetFramework(projectProperties{TargetFrameworks: "net8.0;net9.0"}, ""); err == nil {
		t.Errorf("several target frameworks need the framework flag")
	}
	if f, err := targetFramework(projectProperties{TargetFrameworks: "net8.0;net9.0"}, "net9.0"); err != nil || f != "net9.0" {
		t.Errorf("the flag names the framework: %s %v", f, err)
	}
}

// imageLayerFiles answers the paths of the files in an image.
func imageLayerFiles(t *testing.T, image v1.Image) map[string]bool {
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

func TestTheBaseFollowsThePublishedFrameworks(t *testing.T) {
	dir := t.TempDir()
	stackstest.WriteFile(t, dir, "Api.runtimeconfig.json", `{"runtimeOptions": {"framework": {"name": "Microsoft.NETCore.App"}, "frameworks": [{"name": "Microsoft.AspNetCore.App"}]}}`)
	if web, err := needsASPNET(dir, "Api"); err != nil || !web {
		t.Errorf("a framework reference to ASP.NET Core needs the aspnet base: %v %v", web, err)
	}
	stackstest.WriteFile(t, dir, "Tool.runtimeconfig.json", `{"runtimeOptions": {"framework": {"name": "Microsoft.NETCore.App"}}}`)
	if web, err := needsASPNET(dir, "Tool"); err != nil || web {
		t.Errorf("a console application needs the runtime base: %v %v", web, err)
	}
	stackstest.WriteFile(t, dir, "Desk.runtimeconfig.json", `{"runtimeOptions": {"framework": {"name": "Microsoft.WindowsDesktop.App"}}}`)
	if _, err := needsASPNET(dir, "Desk"); err == nil {
		t.Errorf("a Windows desktop application has no Linux image")
	}
}

func TestPropertiesAreReadPastAFirstRunBanner(t *testing.T) {
	root := appFixture(t)
	stubApp(t, "Welcome to .NET 8.0!\n---------------------\n"+webProps)
	status, _, err := runAppJob(t, root, "src/Api", PackageDockerJob(stack(t)), nil, "--dry-run")
	if status != "OK" || err != nil {
		t.Errorf("package --dry-run: %s %v", status, err)
	}
}
