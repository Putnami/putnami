package php

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"go.putnami.dev/protocol/features/spectest"
	job "go.putnami.dev/protocol/job"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/tooling/stacks"
	"go.putnami.dev/tooling/stacks/stackstest"
	"go.putnami.dev/tooling/stacks/stackstest/imagetest"
)

// application writes a Composer application with a front controller.
func application(t *testing.T, root string) {
	t.Helper()
	stackstest.WriteFile(t, root, "composer.json", `{"type": "project", "require": {"php": ">=8.4"}}`)
	stackstest.WriteFile(t, root, "composer.lock", `{"packages": []}`)
	stackstest.WriteFile(t, root, frontController, "<?php\n")
}

func TestPackageAssemblesAnImageOfAnApplicationAndItsProductionDependencies(t *testing.T) {
	spectest.Proves(t, feature, "packages-an-application-image", "package-assembles-an-image-of-an-application-and-its-production-dependencies")
	log, _ := prepareComposer(t)
	root := t.TempDir()
	host, base := imagetest.Registry(t)
	shipped := imageApplication(t, root)

	status, _, err := runShopJob(t, root, PackageDockerJob(stack(t)), "--docker-base-image", base)
	if status != "OK" || err != nil {
		t.Fatalf("package: %s %v", status, err)
	}
	lines := readLines(t, log)
	if len(lines) != 1 || !strings.HasSuffix(lines[0], "composer.phar install --no-interaction --no-progress --no-dev --no-scripts --optimize-autoloader") {
		t.Fatalf("composer ran with %q", lines)
	}
	if strings.HasPrefix(lines[0], filepath.Join(root, "shop")+":") {
		t.Errorf("Composer ran in the repository: %q", lines[0])
	}
	var manifest pkgmeta.DockerManifest
	data, err := os.ReadFile(filepath.Join(root, ".putnami/out/shop/~package/docker/manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Image != "shop" || manifest.Digest == "" {
		t.Fatalf("manifest = %+v", manifest)
	}

	// The image reaches the registry, and it holds what staging shipped.
	spectest.Proves(t, feature, "publishes-an-application-image", "publish-pushes-the-application-image")
	status, _, err = runShopJob(t, root, PublishDockerJob(), "--docker-registry", host+"/acme")
	if status != "OK" || err != nil {
		t.Fatalf("publish: %s %v", status, err)
	}
	published := imageFilesAt(t, host+"/acme/shop@"+manifest.Digest)
	for _, want := range shipped {
		if !published[strings.TrimPrefix(imageAppDir, "/")+"/"+want] {
			t.Errorf("the image has no %s", want)
		}
	}
	for _, secret := range []string{"auth.json", ".env.local", ".env.local.php", "config/secrets/prod/prod.decrypt.private.php", "tests/AppTest.php"} {
		if published[strings.TrimPrefix(imageAppDir, "/")+"/"+secret] {
			t.Errorf("the image holds %s", secret)
		}
	}
}

// imageApplication writes the application shop under root, with files an image
// must leave out, and answers the files it must ship.
func imageApplication(t *testing.T, root string) []string {
	t.Helper()
	application(t, filepath.Join(root, "shop"))
	stackstest.WriteFile(t, root, "shop/.env", "APP_ENV=prod\n")
	stackstest.WriteFile(t, root, "shop/auth.json", "{}")
	stackstest.WriteFile(t, root, "shop/.env.local", "SECRET=1\n")
	stackstest.WriteFile(t, root, "shop/.env.local.php", "<?php\n")
	stackstest.WriteFile(t, root, "shop/config/secrets/prod/prod.decrypt.private.php", "<?php\n")
	stackstest.WriteFile(t, root, "shop/tests/AppTest.php", "<?php\n")
	return []string{".env", frontController, "vendor/composer/installed.json"}
}

// imageFilesAt answers the paths of the files in the image at reference.
func imageFilesAt(t *testing.T, reference string) map[string]bool {
	t.Helper()
	parsed, err := name.ParseReference(reference)
	if err != nil {
		t.Fatal(err)
	}
	image, err := remote.Image(parsed)
	if err != nil {
		t.Fatal(err)
	}
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

// runShopJob runs a job for the application shop under root.
func runShopJob(t *testing.T, root string, jobFunc cli.JobFunc, args ...string) (string, map[string]any, error) {
	t.Helper()
	s := stack(t)
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{"shop"}})
	if err != nil || len(result.Projects) != 1 {
		t.Fatalf("probe: %v %+v", err, result)
	}
	ctx := &pctx.Context{WorkspaceRoot: root, Project: pctx.Project{
		Name: "shop", Path: "shop", FullPath: filepath.Join(root, "shop"),
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

func TestAStagedImageNeverCarriesASecretFile(t *testing.T) {
	for name, want := range map[string]bool{
		"auth.json": true, ".env.local": true, ".env.prod.local": true, ".env.local.php": true,
		"prod.decrypt.private.php": true, ".env": false, ".env.prod": false, "index.php": false,
	} {
		if got := secretFile(name); got != want {
			t.Errorf("secretFile(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestAnImageSplitsDependenciesFilesAndConfigurationIntoLayers(t *testing.T) {
	staged := t.TempDir()
	stackstest.WriteFile(t, staged, "vendor/autoload.php", "<?php\n")
	stackstest.WriteFile(t, staged, "bin/console", "#!/usr/bin/env php\n")
	if err := os.Chmod(filepath.Join(staged, "bin/console"), 0o755); err != nil {
		t.Fatal(err)
	}
	stackstest.WriteFile(t, staged, frontController, "<?php\n")
	base := "php:8.4.23-apache@sha256:abc"
	spec, err := imageSpec(staged, t.TempDir(), base, "linux/arm64", "8080")
	if err != nil {
		t.Fatal(err)
	}
	if spec.BaseRef != base || spec.Platform != "linux/arm64" || spec.User != imageUser || spec.WorkingDir != imageAppDir || len(spec.Layers) != 3 {
		t.Fatalf("spec = %+v", spec)
	}
	if !slices.Contains(spec.Env, "PORT="+"8080") || !slices.Contains(spec.ExposedPorts, "8080"+"/tcp") {
		t.Errorf("spec = %+v", spec)
	}
	want := map[string]int64{imageAppDir + "/vendor/autoload.php": 0o644}
	layer := func(i int) map[string]int64 {
		out := map[string]int64{}
		for _, f := range spec.Layers[i].Files {
			out[f.Path] = f.Mode
		}
		return out
	}
	if got := layer(0); len(got) != 1 || got[imageAppDir+"/vendor/autoload.php"] != want[imageAppDir+"/vendor/autoload.php"] {
		t.Errorf("dependencies = %v", got)
	}
	if got := layer(1); got[imageAppDir+"/bin/console"] != 0o755 || got[imageAppDir+"/public/index.php"] != 0o644 || len(got) != 2 {
		t.Errorf("files = %v", got)
	}
	if got := layer(2); len(got) != 2 || got["/etc/apache2/ports.conf"] != 0o644 {
		t.Errorf("configuration = %v", got)
	}
}

func TestStagingCopiesThePathRepositoryAndLeavesTheMachineOut(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "apps/shop")
	stackstest.WriteFile(t, root, "apps/shop/composer.json", `{"repositories": [{"type": "path", "url": "../../libs/*"}, {"type": "composer", "url": "https://x"}]}`)
	stackstest.WriteFile(t, root, "apps/shop/composer.lock", "{}")
	stackstest.WriteFile(t, root, "apps/shop/.env", "A=1\n")
	stackstest.WriteFile(t, root, "apps/shop/.env.local", "SECRET=1\n")
	stackstest.WriteFile(t, root, "apps/shop/.env.prod.local", "SECRET=1\n")
	stackstest.WriteFile(t, root, "apps/shop/vendor/x.php", "<?php\n")
	stackstest.WriteFile(t, root, "apps/shop/tests/ATest.php", "<?php\n")
	stackstest.WriteFile(t, root, "apps/shop/src/tests/Kept.php", "<?php\n")
	stackstest.WriteFile(t, root, "libs/discount/composer.json", "{}")
	stackstest.WriteFile(t, root, "libs/discount/.phpunit.cache/x", "")

	workspace := t.TempDir()
	staged, err := stageApplication(root, app, workspace)
	if err != nil {
		t.Fatal(err)
	}
	for rel, present := range map[string]bool{
		"composer.json": true, ".env": true, "src/tests/Kept.php": true,
		".env.local": false, ".env.prod.local": false, "vendor/x.php": false, "tests/ATest.php": false,
	} {
		if _, err := os.Stat(filepath.Join(staged, rel)); (err == nil) != present {
			t.Errorf("%s: present=%v, stat err %v", rel, present, err)
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, "libs/discount/composer.json")); err != nil {
		t.Errorf("the path repository is not staged: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "libs/discount/.phpunit.cache")); !os.IsNotExist(err) {
		t.Errorf("generated files are staged: %v", err)
	}
}

func TestStagingRefusesAPathRepositoryOutsideTheWorkspace(t *testing.T) {
	outer := t.TempDir()
	root := filepath.Join(outer, "repo")
	stackstest.WriteFile(t, outer, "lib/composer.json", "{}")
	stackstest.WriteFile(t, root, "composer.json", `{"repositories": [{"type": "path", "url": "../lib"}]}`)
	if _, err := stageApplication(root, root, t.TempDir()); err == nil || !strings.Contains(err.Error(), "outside the workspace") {
		t.Errorf("err = %v", err)
	}
}

func TestStagingReadsTheObjectFormOfRepositories(t *testing.T) {
	root := t.TempDir()
	stackstest.WriteFile(t, root, "composer.json", `{"repositories": {"packagist.org": false}}`)
	if _, err := stageApplication(root, root, t.TempDir()); err != nil {
		t.Errorf("err = %v", err)
	}
}

func TestBaseImageUsesTheNamedDigestOrTheOfficialImage(t *testing.T) {
	prepareComposer(t)
	root := t.TempDir()
	step := stacks.RootStep{Dir: root, Env: os.Environ(), Program: func(name string) string { return name }}
	if got, err := baseImage(context.Background(), step, "registry.test/php@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil || got != "registry.test/php@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("named: %s %v", got, err)
	}
	if _, err := baseImage(context.Background(), step, "php:8.4-apache"); err == nil || !strings.Contains(err.Error(), "no valid digest") {
		t.Errorf("no digest: %v", err)
	}
	if got, err := baseImage(context.Background(), step, ""); err != nil || got != apacheBases["8.4"] {
		t.Errorf("official: %s %v", got, err)
	}
	t.Setenv("STUB_PHP_MINOR", "7.4")
	step.Env = os.Environ()
	if _, err := baseImage(context.Background(), step, ""); err == nil || !strings.Contains(err.Error(), "--docker-base-image") {
		t.Errorf("unpinned: %v", err)
	}
}

func TestPackageRefusesWhatItCannotImage(t *testing.T) {
	prepareComposer(t)
	cases := map[string]struct {
		setup func(t *testing.T, root string)
		rel   string
		want  string
	}{
		"no front controller": {func(t *testing.T, root string) {
			application(t, root)
			if err := os.Remove(filepath.Join(root, frontController)); err != nil {
				t.Fatal(err)
			}
		}, ".", CodeNoFrontController},
		"no lock": {func(t *testing.T, root string) {
			application(t, root)
			if err := os.Remove(filepath.Join(root, composerLock)); err != nil {
				t.Fatal(err)
			}
		}, ".", CodeImageFailed},
		"a component of another stack root": {func(t *testing.T, root string) {
			copyFixture := fixture(t)
			if err := os.CopyFS(root, os.DirFS(copyFixture)); err != nil {
				t.Fatal(err)
			}
		}, "packages/pricing", CodeImageFailed},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			c.setup(t, root)
			status, _, err := runProjectJob(t, root, c.rel, PackageDockerJob(stack(t)), nil)
			if status != "FAILED" || err == nil || !strings.HasPrefix(err.Error(), c.want) {
				t.Errorf("package: %s %v", status, err)
			}
		})
	}
}

func TestPackageRefusesABaseWithNoDigest(t *testing.T) {
	prepareComposer(t)
	root := t.TempDir()
	application(t, root)
	status, _, err := runProjectJob(t, root, ".", PackageDockerJob(stack(t)), nil, "--docker-base-image", "php:8.4-apache")
	if status != "FAILED" || err == nil || !strings.HasPrefix(err.Error(), CodeBaseNotPinned) {
		t.Errorf("package: %s %v", status, err)
	}
}

func TestADryRunPackageRecordsTheChannelAndRunsNoComposer(t *testing.T) {
	log, _ := prepareComposer(t)
	root := t.TempDir()
	application(t, root)
	status, _, err := runProjectJob(t, root, ".", PackageDockerJob(stack(t)), job.Params{"dry-run": json.RawMessage(`true`)})
	if status != "OK" || err != nil {
		t.Fatalf("package: %s %v", status, err)
	}
	if lines := readLines(t, log); lines != nil {
		t.Errorf("Composer ran: %q", lines)
	}
	if _, err := os.Stat(filepath.Join(root, ".putnami/out/~package/docker/channel.json")); err != nil {
		t.Errorf("channel record: %v", err)
	}
}

func TestAPackagedLibraryIsSkipped(t *testing.T) {
	prepareComposer(t)
	root := fixture(t)
	jobFunc := PackageDockerJob(stack(t))
	ctx := libraryContext(root)
	status, _, err := jobFunc(ctx, nil, nil)
	if status != "SKIP" || err != nil {
		t.Errorf("package: %s %v", status, err)
	}
}

func TestPublishDryRunPushesNothing(t *testing.T) {
	root := t.TempDir()
	status, data, err := runPublish(t, root)
	if status != "OK" || err != nil || data["dryRun"] != true {
		t.Errorf("publish: %s %v %v", status, data, err)
	}
}

func TestAnApplicationThatPublishesAnImageIsAReleaseSetMember(t *testing.T) {
	root := t.TempDir()
	application(t, filepath.Join(root, "shop"))
	stackstest.WriteFile(t, root, "shop/putnami.json", `{"options": {"publish": {"docker": true}}}`)
	stackstest.WriteFile(t, root, "putnami.workspace.json", `{"registries": {"oci": {"publish": "registry.test/acme"}}}`)
	s := stack(t)
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{"shop"}})
	if err != nil || len(result.Projects) != 1 {
		t.Fatalf("probe: %v %+v", err, result)
	}
	if result.Projects[0].Type != "application" {
		t.Errorf("type = %q", result.Projects[0].Type)
	}
	meta := metadata(t, result.Projects[0])
	if meta.ReleaseSet == nil || len(meta.ReleaseSet.Ecosystems) != 1 {
		t.Fatalf("release set = %+v", meta.ReleaseSet)
	}
	member := meta.ReleaseSet.Ecosystems[0]
	if member.Ecosystem != "oci" || member.Coordinate != "acme/shop" || member.PackageStep != "docker" || member.PublishStep != "docker" {
		t.Errorf("member = %+v", member)
	}
}

func TestAProjectNamedWithUppercaseLettersPackagesALowercaseImage(t *testing.T) {
	prepareComposer(t)
	root := t.TempDir()
	application(t, filepath.Join(root, "Shop"))
	stackstest.WriteFile(t, root, "Shop/putnami.json", `{"publish": ["docker"]}`)
	stackstest.WriteFile(t, root, "putnami.workspace.json", `{"registries": {"oci": {"publish": "registry.test/acme"}}}`)
	s := stack(t)
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{"Shop"}})
	if err != nil || len(result.Projects) != 1 {
		t.Fatalf("probe: %v %+v", err, result)
	}
	// The shared publisher finds the release-set member of the packaged image
	// by its name.
	meta := metadata(t, result.Projects[0])
	if meta.ReleaseSet == nil || len(meta.ReleaseSet.Ecosystems) != 1 || meta.ReleaseSet.Ecosystems[0].Coordinate != "acme/shop" {
		t.Fatalf("release set = %+v", meta.ReleaseSet)
	}
	ctx := &pctx.Context{WorkspaceRoot: root, Params: job.Params{"dry-run": json.RawMessage(`true`)}, Project: pctx.Project{
		Name: "Shop", Path: "Shop", FullPath: filepath.Join(root, "Shop"),
		Metadata: map[string]json.RawMessage{ExtensionName: result.Projects[0].Metadata},
	}}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	stdout := os.Stdout
	os.Stdout = devnull
	status, _, err := PackageDockerJob(s)(ctx, jsonl.New(), nil)
	os.Stdout = stdout
	if status != "OK" || err != nil {
		t.Fatalf("package: %s %v", status, err)
	}
	index, err := pkgmeta.ReadChannelIndex(root, "Shop")
	if err != nil || index.Records[dockerChannel].Artifact != "shop" {
		t.Errorf("channel records = %+v, %v", index, err)
	}
}

func TestAComposerPackageWithoutTheProjectTypeIsALibraryAndPublishesNoImage(t *testing.T) {
	root := t.TempDir()
	stackstest.WriteFile(t, root, "lib/composer.json", `{"name": "acme/lib", "require": {"php": ">=8.4"}}`)
	stackstest.WriteFile(t, root, "lib/putnami.json", `{"options": {"publish": {"docker": true}}}`)
	s := stack(t)
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{"lib"}})
	if err != nil || len(result.Projects) != 1 {
		t.Fatalf("probe: %v %+v", err, result)
	}
	if result.Projects[0].Type != "library" {
		t.Errorf("type = %q, want library", result.Projects[0].Type)
	}
	if meta := metadata(t, result.Projects[0]); meta.ReleaseSet != nil {
		t.Errorf("release set = %+v", meta.ReleaseSet)
	}
}

func TestPackageOfflineDisablesTheNetworkAndNeverDownloadsComposer(t *testing.T) {
	t.Setenv("PUTNAMI_OFFLINE_DEPENDENCIES", "1")
	if env := composerEnv(nil); !slices.Contains(env, "COMPOSER_DISABLE_NETWORK=1") {
		t.Errorf("env = %q", env)
	}
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	if _, err := provideComposer(context.Background()); err == nil || !strings.Contains(err.Error(), "offline dependencies") {
		t.Errorf("provideComposer = %v", err)
	}
}

func TestDepsUpgradeRefusesOfflineDependencies(t *testing.T) {
	t.Setenv("PUTNAMI_OFFLINE_DEPENDENCIES", "1")
	root := t.TempDir()
	emit := jsonl.New()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	stdout := os.Stdout
	os.Stdout = devnull
	defer func() { os.Stdout = stdout }()
	err = upgradeRoot(context.Background(), stacks.RootStep{Dir: root}, emit)
	if err == nil || !strings.HasPrefix(err.Error(), CodeComposerUpdateFailed) {
		t.Errorf("deps-upgrade = %v", err)
	}
}

func TestAStagedSymbolicLinkFailsPackaging(t *testing.T) {
	source := t.TempDir()
	stackstest.WriteFile(t, source, "public/app.js", "a")
	if err := os.Symlink("app.js", filepath.Join(source, "public/linked.js")); err != nil {
		t.Skipf("no symbolic links: %v", err)
	}
	err := copyTree(source, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("copyTree = %v", err)
	}
}
