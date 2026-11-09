package jobs

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"os/exec"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

const imageFeature = "python/experimental-workspace-integration"

// fakeUV is a uv that logs each command to $FAKE_UV_LOG. `python find` answers
// a python that prints $FAKE_PYTHON_VERSION, `export` prints $FAKE_UV_EXPORT,
// and `pip install --target -r` installs fastapi for a registry requirements
// file, and a package for each staged directory a local one names. It logs the
// local requirement lines to local.log and the staged files to staged.log.
const fakeUV = `#!/bin/sh
echo "$PWD|$*" >> "$FAKE_UV_LOG"
case "$1" in
python)
	printf '#!/bin/sh\necho "${FAKE_PYTHON_VERSION:-3.13.4}"\n' > "$FAKE_UV_DIR/python"
	chmod +x "$FAKE_UV_DIR/python"
	echo "$FAKE_UV_DIR/python"
	;;
export)
	cat "$FAKE_UV_EXPORT"
	;;
pip)
	target=""
	previous=""
	for arg in "$@"; do
		[ "$previous" = "--target" ] && target="$arg"
		if [ "$previous" = "-r" ] && [ "$(basename "$arg")" = "local-requirements.txt" ]; then
			while IFS= read -r line; do
				echo "$line" >> "$FAKE_UV_DIR/local.log"
				staged="${line%% ;*}"
				(cd "$staged" && find . -type f) >> "$FAKE_UV_DIR/staged.log"
				package=$(basename "$staged")
				mkdir -p "$target/$package" "$target/$package-0.1.0.dist-info"
				echo "# $package" > "$target/$package/__init__.py"
				echo "{\"url\": \"file://$staged\"}" > "$target/$package-0.1.0.dist-info/direct_url.json"
				date +%s%N > "$target/$package-0.1.0.dist-info/uv_cache.json"
				printf '%s\n' "$package/__init__.py,sha256=a,1" "$package-0.1.0.dist-info/direct_url.json,sha256=b,2" "$package-0.1.0.dist-info/uv_cache.json,sha256=c,3" "$package-0.1.0.dist-info/RECORD,," > "$target/$package-0.1.0.dist-info/RECORD"
			done < "$arg"
		elif [ "$previous" = "-r" ]; then
			mkdir -p "$target/fastapi" && echo "app = 1" > "$target/fastapi/__init__.py"
			mkdir -p "$target/bin" && echo "#!/machine/python" > "$target/bin/fastapi"
		fi
		previous="$arg"
	done
	;;
esac
`

// registryExport is what uv export answers for the application shop: a
// registry requirement with its hashes, the application itself and a path
// dependency with a marker.
const registryExport = `./apps/shop
./libs/discount ; python_full_version >= '3.11'
fastapi==0.141.1 \
    --hash=sha256:aaaa \
    --hash=sha256:bbbb
colorama==0.4.6 ; sys_platform == 'win32' \
    --hash=sha256:cccc
`

// prepareUV installs the fake uv and answers the file it logs to. The files
// it stages and the local requirements it reads are logged next to it.
func prepareUV(t *testing.T, export string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake uv is a shell script")
	}
	dir := t.TempDir()
	uv := filepath.Join(dir, "uv")
	if err := os.WriteFile(uv, []byte(fakeUV), 0o755); err != nil { //nolint:gosec // a test program
		t.Fatal(err)
	}
	exportFile := filepath.Join(dir, "export.txt")
	if err := os.WriteFile(exportFile, []byte(export), 0o644); err != nil { //nolint:gosec // a test file
		t.Fatal(err)
	}
	log := filepath.Join(dir, "uv.log")
	t.Setenv("FAKE_UV_LOG", log)
	t.Setenv("FAKE_UV_DIR", dir)
	t.Setenv("FAKE_UV_EXPORT", exportFile)
	stubResolveUV(t, func(string) (string, error) { return uv, nil })
	return log
}

func writeImageFile(t *testing.T, root, rel, content string) {
	t.Helper()
	file := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil { //nolint:gosec // a test file
		t.Fatal(err)
	}
}

// shopWorkspace writes a uv workspace holding the application apps/shop and its
// path dependency libs/discount, with files an image must leave out, and
// answers the application files it must ship.
func shopWorkspace(t *testing.T) (string, []string) {
	t.Helper()
	// The fake uv logs the shell's physical working directory, so the root is
	// resolved too: on macOS the temporary directory sits behind /var -> /private/var.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeImageFile(t, root, "pyproject.toml", "[tool.uv.workspace]\nmembers = [\"apps/shop\", \"libs/discount\"]\n")
	writeImageFile(t, root, "uv.lock", "version = 1\n")
	writeImageFile(t, root, "apps/shop/pyproject.toml", "[project]\nname = \"shop\"\n")
	writeImageFile(t, root, "apps/shop/src/main.py", "print('shop')\n")
	writeImageFile(t, root, "apps/shop/src/shop/__init__.py", "")
	writeImageFile(t, root, "apps/shop/src/shop/build/plan.py", "")
	for _, left := range []string{
		".env", ".env.production", ".env.example", ".envrc", ".pypirc", "tests/test_shop.py", "src/shop/test_rules.py", "src/shop/rules_test.py",
		"src/conftest.py", "src/shop/__pycache__/x.pyc", ".venv/bin/python", "build/lib/x.py", "dist/shop.whl", ".gen/version.json",
	} {
		writeImageFile(t, root, "apps/shop/"+left, "left out\n")
	}
	writeImageFile(t, root, "libs/discount/pyproject.toml", "[project]\nname = \"discount\"\n")
	writeImageFile(t, root, "libs/discount/src/discount/__init__.py", "")
	writeImageFile(t, root, "libs/discount/build/stale.py", "")
	return root, []string{"pyproject.toml", "src/main.py", "src/shop/__init__.py", "src/shop/build/plan.py"}
}

// shopContext is the job context of the application apps/shop.
func shopContext(root string, closure ...string) *pctx.Context {
	refs := []pctx.ProjectRef{{ID: "/apps/shop", Name: "shop", Path: "apps/shop"}}
	for _, dependency := range closure {
		refs = append(refs, pctx.ProjectRef{ID: "/" + dependency, Name: path.Base(dependency), Path: dependency})
	}
	return &pctx.Context{WorkspaceRoot: root, Project: pctx.Project{
		Name: "shop", Path: "apps/shop", FullPath: filepath.Join(root, "apps/shop"), Type: "application", DependencyClosure: refs,
	}}
}

// runImageJob runs a job with its stdout discarded.
func runImageJob(t *testing.T, ctx *pctx.Context, job func(*pctx.Context, *jsonl.Emitter, []string) (string, map[string]any, error), args ...string) (string, error) {
	t.Helper()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	stdout := os.Stdout
	os.Stdout = devnull
	defer func() { os.Stdout = stdout }()
	status, _, err := job(ctx, jsonl.New(), args)
	return status, err
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

// remoteImage answers the files of the image at reference and its config.
func remoteImage(t *testing.T, reference string) (map[string]string, v1.Config) {
	t.Helper()
	parsed, err := name.ParseReference(reference)
	if err != nil {
		t.Fatal(err)
	}
	image, err := remote.Image(parsed)
	if err != nil {
		t.Fatal(err)
	}
	config, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	flat := mutate.Extract(image)
	defer flat.Close()
	files := map[string]string{}
	reader := tar.NewReader(flat)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return files, config.Config
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		files[strings.TrimPrefix(header.Name, "/")] = string(content)
	}
}

func readLog(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log) //nolint:gosec // a test file
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestPackageAssemblesAnImageOfAnApplicationAndItsProductionDependencies(t *testing.T) {
	spectest.Proves(t, imageFeature, "packages-an-application-image", "package-assembles-an-image-of-an-application-and-its-production-dependencies")
	log := prepareUV(t, registryExport)
	root, shipped := shopWorkspace(t)
	host, base := imageRegistry(t)
	ctx := shopContext(root, "libs/discount")

	if status, err := runImageJob(t, ctx, PackageDocker, "--docker-base-image", base); status != "OK" || err != nil {
		t.Fatalf("package: %s %v", status, err)
	}
	manifest, err := pkgmeta.ReadDockerManifest(root, "apps/shop")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Image != "shop" || manifest.Digest == "" || manifest.Platform != "linux/amd64" {
		t.Fatalf("manifest = %+v", manifest)
	}

	t.Run("dependencies install as hashed wheels for the image platform", func(t *testing.T) {
		spectest.Proves(t, imageFeature, "image-installs-the-locked-wheels", "the-dependencies-install-as-hashed-wheels-for-the-image-platform")
		calls := readLog(t, log)
		if len(calls) != 4 {
			t.Fatalf("uv ran %q", calls)
		}
		for _, call := range calls {
			if dir, _, _ := strings.Cut(call, "|"); dir != root {
				t.Errorf("uv ran in %s, not at the workspace root: %q", dir, call)
			}
		}
		if !strings.HasSuffix(calls[1], "|export --frozen --no-dev --no-editable --no-header --no-annotate --package shop") {
			t.Errorf("export = %q", calls[1])
		}
		platform := "--python-platform x86_64-unknown-linux-gnu --python-version 3.13.4 --no-deps"
		if !strings.Contains(calls[2], platform+" --only-binary :all: --require-hashes -r ") {
			t.Errorf("registry install = %q", calls[2])
		}
		if !strings.Contains(calls[3], platform+" -r ") || strings.Contains(calls[3], "--only-binary") || strings.Contains(calls[3], "--require-hashes") {
			t.Errorf("path install = %q", calls[3])
		}
		locals := readLog(t, filepath.Join(filepath.Dir(log), "local.log"))
		if len(locals) != 2 || !strings.HasSuffix(locals[0], "/staged/apps/shop") ||
			!strings.HasSuffix(locals[1], "/staged/libs/discount ; python_full_version >= '3.11'") {
			t.Errorf("local requirements = %q", locals)
		}
		for _, local := range locals {
			if strings.HasPrefix(local, root) {
				t.Errorf("a path dependency was built in the repository: %q", local)
			}
		}
		if _, err := os.Stat(filepath.Join(root, "libs/discount/build/lib")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the repository was written to: %v", err)
		}
	})

	spectest.Proves(t, imageFeature, "publishes-an-application-image", "publish-pushes-the-application-image")
	if status, err := runImageJob(t, ctx, PublishDocker, "--docker-registry", host+"/acme"); status != "OK" || err != nil {
		t.Fatalf("publish: %s %v", status, err)
	}
	files, config := remoteImage(t, host+"/acme/shop@"+manifest.Digest)
	app := strings.TrimPrefix(ImageAppDir, "/") + "/"
	site := strings.TrimPrefix(ImageSitePackages, "/") + "/"
	for _, want := range shipped {
		if _, ok := files[app+want]; !ok {
			t.Errorf("the image has no %s", want)
		}
	}
	for _, want := range []string{"fastapi/__init__.py", "shop/__init__.py", "discount/__init__.py", "discount-0.1.0.dist-info/RECORD"} {
		if _, ok := files[site+want]; !ok {
			t.Errorf("the image has no installed %s", want)
		}
	}
	for file := range files {
		if strings.HasPrefix(file, site+"bin/") || strings.HasSuffix(file, "/direct_url.json") || strings.HasSuffix(file, "/uv_cache.json") {
			t.Errorf("the image holds %s", file)
		}
	}
	if record := files[site+"discount-0.1.0.dist-info/RECORD"]; strings.Contains(record, "direct_url.json") || strings.Contains(record, "uv_cache.json") || !strings.Contains(record, "discount/__init__.py") {
		t.Errorf("RECORD = %q", record)
	}
	if !slices.Equal(config.Entrypoint, []string{"python", ImageAppDir + "/src/main.py"}) || config.User != imageUser || config.WorkingDir != ImageAppDir {
		t.Errorf("config = %+v", config)
	}
	for _, want := range []string{"PORT=8080", "PYTHONPATH=" + ImageSitePackages, "PYTHONDONTWRITEBYTECODE=1"} {
		if !slices.Contains(config.Env, want) {
			t.Errorf("env %v has no %s", config.Env, want)
		}
	}

	t.Run("the image holds no tests and no secret files", func(t *testing.T) {
		spectest.Proves(t, imageFeature, "image-ships-the-application-files", "the-image-holds-no-tests-and-no-secret-files")
		// The packages built from staging see no test or secret file either.
		for _, staged := range readLog(t, filepath.Join(filepath.Dir(log), "staged.log")) {
			name := path.Base(staged)
			generated := strings.HasPrefix(staged, "./build/") || strings.HasPrefix(staged, "./dist/") || strings.HasPrefix(staged, "./.venv/") ||
				strings.HasPrefix(staged, "./.gen/") || strings.Contains(staged, "/__pycache__/")
			if TestFile(name) || SecretFile(name) || strings.HasPrefix(staged, "./tests/") || generated {
				t.Errorf("the staged build reads %s", staged)
			}
		}
		for file := range files {
			if !strings.HasPrefix(file, app) {
				continue
			}
			rel := strings.TrimPrefix(file, app)
			if !slices.Contains(shipped, rel) {
				t.Errorf("the image holds %s", rel)
			}
		}
	})
}

func TestAnImageIsTheSameOnEachRun(t *testing.T) {
	prepareUV(t, registryExport)
	root, _ := shopWorkspace(t)
	_, base := imageRegistry(t)
	ctx := shopContext(root, "libs/discount")
	var digests []string
	for range 2 {
		if status, err := runImageJob(t, ctx, PackageDocker, "--docker-base-image", base); status != "OK" || err != nil {
			t.Fatalf("package: %s %v", status, err)
		}
		manifest, err := pkgmeta.ReadDockerManifest(root, "apps/shop")
		if err != nil {
			t.Fatal(err)
		}
		digests = append(digests, manifest.Digest)
	}
	if digests[0] != digests[1] {
		t.Errorf("two runs assembled %v", digests)
	}
}

func TestABaseImageWithNoDigestFails(t *testing.T) {
	spectest.Proves(t, imageFeature, "packages-an-application-image", "a-base-image-with-no-digest-fails")
	named := "registry.test/python@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if got, err := baseImage(named, "3.13"); err != nil || got != named {
		t.Errorf("named: %s %v", got, err)
	}
	if got, err := baseImage("", "3.12"); err != nil || got != slimBases["3.12"] {
		t.Errorf("official: %s %v", got, err)
	}
	if _, err := baseImage("", "3.9"); err == nil || !strings.Contains(err.Error(), "--docker-base-image") {
		t.Errorf("unpinned minor: %v", err)
	}
	for minor, base := range slimBases {
		if !strings.HasPrefix(base, "python:"+minor+".") || !strings.Contains(base, "-slim@sha256:") {
			t.Errorf("the base of %s is %s", minor, base)
		}
	}

	log := prepareUV(t, registryExport)
	root, _ := shopWorkspace(t)
	status, err := runImageJob(t, shopContext(root, "libs/discount"), PackageDocker, "--docker-base-image", "python:3.13-slim")
	if status != "FAILED" || err == nil || !strings.Contains(err.Error(), CodeBaseNotPinned) {
		t.Fatalf("package: %s %v", status, err)
	}
	for _, call := range readLog(t, log) {
		if strings.Contains(call, "|pip ") {
			t.Errorf("uv installed with an unpinned base: %q", call)
		}
	}
}

func TestAPathDependencyOutsideTheDependencyClosureFails(t *testing.T) {
	spectest.Proves(t, imageFeature, "image-installs-the-locked-wheels", "a-path-dependency-outside-the-dependency-closure-fails")
	log := prepareUV(t, registryExport)
	root, _ := shopWorkspace(t)
	status, err := runImageJob(t, shopContext(root), PackageDocker)
	if status != "FAILED" || err == nil || !strings.Contains(err.Error(), CodePathDependencyOutsideClosure) || !strings.Contains(err.Error(), "/libs/discount") {
		t.Fatalf("package: %s %v", status, err)
	}
	for _, call := range readLog(t, log) {
		if strings.Contains(call, "|pip ") {
			t.Errorf("uv installed: %q", call)
		}
	}
}

func TestAPackagedLibraryIsSkipped(t *testing.T) {
	log := prepareUV(t, registryExport)
	root, _ := shopWorkspace(t)
	ctx := shopContext(root)
	ctx.Project.Type = "library"
	if status, err := runImageJob(t, ctx, PackageDocker); status != "SKIP" || err != nil {
		t.Fatalf("package: %s %v", status, err)
	}
	if calls := readLog(t, log); len(calls) != 0 {
		t.Errorf("uv ran %q", calls)
	}
}

func TestADryRunPackageRecordsTheChannelAndInstallsNothing(t *testing.T) {
	log := prepareUV(t, registryExport)
	root, _ := shopWorkspace(t)
	if status, err := runImageJob(t, shopContext(root), PackageDocker, "--dry-run"); status != "OK" || err != nil {
		t.Fatalf("package: %s %v", status, err)
	}
	index, err := pkgmeta.ReadChannelIndex(root, "apps/shop")
	if err != nil || !index.HasChannel(DockerChannel) {
		t.Fatalf("channels: %+v %v", index, err)
	}
	if calls := readLog(t, log); len(calls) != 1 || !strings.HasSuffix(calls[0], "|python find") {
		t.Errorf("uv ran %q", calls)
	}
}

func TestPackageRefusesWhatItCannotImage(t *testing.T) {
	prepareUV(t, registryExport)
	for label, c := range map[string]struct {
		args  []string
		setup func(t *testing.T, root string, ctx *pctx.Context)
		want  string
	}{
		"no entrypoint":                     {[]string{"--entrypoint", "src/server.py"}, func(*testing.T, string, *pctx.Context) {}, CodeNoEntrypoint},
		"an entrypoint outside the project": {[]string{"--entrypoint", "../../pyproject.toml"}, func(*testing.T, string, *pctx.Context) {}, CodeNoEntrypoint},
		"no lock": {nil, func(t *testing.T, root string, _ *pctx.Context) {
			if err := os.Remove(filepath.Join(root, "uv.lock")); err != nil {
				t.Fatal(err)
			}
		}, CodeImageFailed},
		"a platform with no wheels": {[]string{"--platform", "windows/amd64"}, func(*testing.T, string, *pctx.Context) {}, CodeImageFailed},
	} {
		t.Run(label, func(t *testing.T) {
			root, _ := shopWorkspace(t)
			ctx := shopContext(root, "libs/discount")
			c.setup(t, root, ctx)
			status, err := runImageJob(t, ctx, PackageDocker, c.args...)
			if status != "FAILED" || err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("package: %s %v, want %s", status, err, c.want)
			}
		})
	}
}

func TestTheImageRunsTheEntrypointOptionOfPackage(t *testing.T) {
	root, _ := shopWorkspace(t)
	writeImageFile(t, root, "apps/shop/src/shop/server.py", "")
	ctx := shopContext(root)
	if got, err := imageEntrypoint(ctx, nil); err != nil || got != defaultEntrypoint {
		t.Errorf("default: %s %v", got, err)
	}
	// The serve options are another command's: the image does not read them.
	ctx.Project.Options = map[string]json.RawMessage{"@putnami/python:serve": json.RawMessage(`{"entrypoint": "src/shop/server.py"}`)}
	if got, err := imageEntrypoint(ctx, nil); err != nil || got != defaultEntrypoint {
		t.Errorf("serve options: %s %v", got, err)
	}
	ctx.Params = pctx.Params{"entrypoint": json.RawMessage(`"src/shop/server.py"`)}
	if got, err := imageEntrypoint(ctx, nil); err != nil || got != "src/shop/server.py" {
		t.Errorf("package option: %s %v", got, err)
	}
	if got, err := imageEntrypoint(ctx, map[string]string{"entrypoint": "./src/main.py"}); err != nil || got != "src/main.py" {
		t.Errorf("flag: %s %v", got, err)
	}
}

func TestSplitRequirementsSeparatesTheRegistryFromLocalPaths(t *testing.T) {
	registry, locals := splitRequirements("# comment\n" + registryExport + "file:///abs/vendor/lib ; python_version >= '3.11'\n")
	want := []localRequirement{
		{Path: "./apps/shop"},
		{Path: "./libs/discount", Marker: "python_full_version >= '3.11'"},
		{Path: "/abs/vendor/lib", Marker: "python_version >= '3.11'"},
	}
	if !slices.Equal(locals, want) {
		t.Errorf("locals = %+v", locals)
	}
	wantRegistry := "fastapi==0.141.1 \\\n    --hash=sha256:aaaa \\\n    --hash=sha256:bbbb\ncolorama==0.4.6 ; sys_platform == 'win32' \\\n    --hash=sha256:cccc\n"
	if registry != wantRegistry {
		t.Errorf("registry =\n%s", registry)
	}
}

// inputExcludes reads the exclusions of the application input of package-docker.
func inputExcludes(t *testing.T) ([]string, map[string]json.RawMessage) {
	t.Helper()
	data, err := os.ReadFile("../../putnami.extension.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Tasks map[string]struct {
			Inputs map[string]json.RawMessage `json:"inputs"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	inputs := manifest.Tasks["package-docker"].Inputs
	var application struct {
		From  string   `json:"from"`
		Files []string `json:"files"`
	}
	if err := json.Unmarshal(inputs["application"], &application); err != nil || application.From != "project" {
		t.Fatalf("application input: %s %v", inputs["application"], err)
	}
	var excludes []string
	for _, pattern := range application.Files {
		if exclude, ok := strings.CutPrefix(pattern, "!"); ok {
			excludes = append(excludes, exclude)
		}
	}
	return excludes, inputs
}

// excludedBy reports whether a project-relative file matches one of the
// exclusion forms the input uses: dir/**, **/dir/**, **/file and ../**.
func excludedBy(excludes []string, rel string) bool {
	parts := strings.Split(rel, "/")
	for _, exclude := range excludes {
		switch {
		case exclude == "../**":
			if parts[0] == ".." {
				return true
			}
		case strings.HasPrefix(exclude, "**/") && strings.HasSuffix(exclude, "/**"):
			if slices.Contains(parts[:len(parts)-1], strings.TrimSuffix(strings.TrimPrefix(exclude, "**/"), "/**")) {
				return true
			}
		case strings.HasPrefix(exclude, "**/"):
			if parts[len(parts)-1] == strings.TrimPrefix(exclude, "**/") {
				return true
			}
		case strings.HasSuffix(exclude, "/**"):
			if len(parts) > 1 && parts[0] == strings.TrimSuffix(exclude, "/**") {
				return true
			}
		}
	}
	return false
}

func TestTheImageCacheKeyCoversTheFilesItShips(t *testing.T) {
	spectest.Proves(t, imageFeature, "image-ships-the-application-files", "the-image-cache-key-covers-the-files-it-ships")
	excludes, inputs := inputExcludes(t)
	var closure, workspace struct {
		From  string   `json:"from"`
		Files []string `json:"files"`
	}
	if json.Unmarshal(inputs["pathDependencies"], &closure) != nil || closure.From != "closure" {
		t.Errorf("the path dependencies are not an input: %s", inputs["pathDependencies"])
	}
	if json.Unmarshal(inputs["workspaceConfig"], &workspace) != nil || workspace.From != "workspace" ||
		!slices.Contains(workspace.Files, "uv.lock") || !slices.Contains(workspace.Files, "pyproject.toml") {
		t.Errorf("the uv workspace files are not an input: %s", inputs["workspaceConfig"])
	}
	if _, ok := inputs["parameters"]; !ok {
		t.Error("the parameters are not an input")
	}

	// Each file an image or a staged build reads is keyed.
	root, shipped := shopWorkspace(t)
	shipped = append(shipped, "tests/test_shop.py", "src/conftest.py", "src/shop/build/plan.py")
	for _, rel := range shipped {
		if excludedBy(excludes, rel) {
			t.Errorf("%s is read by the image but not keyed", rel)
		}
	}
	// Each exclusion is something no image and no staged build reads.
	err := filepath.WalkDir(filepath.Join(root, "apps/shop"), func(file string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		rel, err := filepath.Rel(filepath.Join(root, "apps/shop"), file)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if excludedBy(excludes, rel) && !leftOutOfStaging(rel) {
			t.Errorf("%s is staged but not keyed", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// leftOutOfStaging reports whether a staged build leaves a project file out.
func leftOutOfStaging(rel string) bool {
	parts := strings.Split(rel, "/")
	for i := range parts {
		entry := fakeEntry{name: parts[i], dir: i < len(parts)-1}
		if (treeFilter{}).skip(strings.Join(parts[:i+1], "/"), entry) {
			return true
		}
	}
	return false
}

type fakeEntry struct {
	name string
	dir  bool
}

func (e fakeEntry) Name() string               { return e.name }
func (e fakeEntry) IsDir() bool                { return e.dir }
func (e fakeEntry) Type() os.FileMode          { return 0 }
func (e fakeEntry) Info() (os.FileInfo, error) { return nil, errors.New("no info") }

func TestSecretAndTestFilesAreRecognized(t *testing.T) {
	for file, want := range map[string]bool{
		".env": true, ".env.local": true, ".env.production": true, ".env.example": true, ".envrc": true, ".pypirc": true, ".netrc": true,
		"settings.py": false, "environment.py": false, "env.py": false,
	} {
		if got := SecretFile(file); got != want {
			t.Errorf("SecretFile(%q) = %v, want %v", file, got, want)
		}
	}
	for file, want := range map[string]bool{
		"test_api.py": true, "api_test.py": true, "conftest.py": true, "testing.py": false, "contest.py": false, "test_data.json": false,
	} {
		if got := TestFile(file); got != want {
			t.Errorf("TestFile(%q) = %v, want %v", file, got, want)
		}
	}
}

func TestOnlyTheLocalPackagesUVExportsAreBuilt(t *testing.T) {
	log := prepareUV(t, "./libs/discount\n")
	root, _ := shopWorkspace(t)
	_, base := imageRegistry(t)
	if status, err := runImageJob(t, shopContext(root, "libs/discount"), PackageDocker, "--docker-base-image", base); status != "OK" || err != nil {
		t.Fatalf("package: %s %v", status, err)
	}
	locals := readLog(t, filepath.Join(filepath.Dir(log), "local.log"))
	if len(locals) != 1 || !strings.HasSuffix(locals[0], "/staged/libs/discount") {
		t.Errorf("a project with no build system was built: %q", locals)
	}
	for _, call := range readLog(t, log) {
		if strings.Contains(call, "--require-hashes") {
			t.Errorf("a registry install ran with no registry requirement: %q", call)
		}
	}
}

func TestANestedDirectoryTheKeyDoesNotWalkShipsOnlyItsGitCandidates(t *testing.T) {
	spectest.Proves(t, imageFeature, "image-ships-the-application-files", "the-image-cache-key-covers-the-files-it-ships")
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root, _ := shopWorkspace(t)
	writeImageFile(t, root, ".gitignore", "dist/\n.env*\n")
	for _, rel := range []string{"src/shop/static/dist/bundle.js", "src/shop/vendor/lib.py", "src/shop/out/data.json", "src/shop/vendor/ignored.py"} {
		writeImageFile(t, root, "apps/shop/"+rel, "x\n")
	}
	writeImageFile(t, root, "apps/shop/src/shop/vendor/.gitignore", "ignored.py\n")
	for _, args := range [][]string{{"init", "-q"}, {"add", "apps/shop/src/shop/vendor/lib.py"}} {
		command := exec.Command("git", args...)
		command.Dir = root
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	project := filepath.Join(root, "apps/shop")
	filter, err := newTreeFilter(project)
	if err != nil || filter.candidates == nil {
		t.Fatalf("filter: %+v %v", filter, err)
	}
	files, err := treeFiles(project, ImageAppDir, filter.skip)
	if err != nil {
		t.Fatal(err)
	}
	shipped := map[string]bool{}
	for _, file := range files {
		shipped[strings.TrimPrefix(file.Path, ImageAppDir+"/")] = true
	}
	// A tracked file and an untracked file Git does not ignore are keyed
	// through the Git candidates; an ignored one is keyed by nothing.
	for rel, want := range map[string]bool{
		"src/shop/vendor/lib.py": true, "src/shop/out/data.json": true,
		"src/shop/static/dist/bundle.js": false, "src/shop/vendor/ignored.py": false, "src/main.py": true,
	} {
		if shipped[rel] != want {
			t.Errorf("%s shipped = %v, want %v", rel, shipped[rel], want)
		}
	}
	staging := t.TempDir()
	if err := copyTree(project, staging); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(staging, "src/shop/static/dist/bundle.js")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the staged build reads an unkeyed file: %v", err)
	}
}

func TestTheDependenciesAreSelectedForThePythonOfTheBase(t *testing.T) {
	if got := imagePythonVersion(slimBases["3.13"], "3.13.4"); got != "3.13.18" {
		t.Errorf("official base: %s", got)
	}
	if got := imagePythonVersion("registry.test/python@sha256:aa", "3.12.7"); got != "3.12.7" {
		t.Errorf("named base: %s", got)
	}
	if got := minorOf("3.14.0rc1"); got != "3.14" {
		t.Errorf("minor: %s", got)
	}
}
