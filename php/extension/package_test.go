package php

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	job "go.putnami.dev/protocol/job"
	"go.putnami.dev/tooling/stacks"
)

// application writes a Composer application with a front controller.
func application(t *testing.T, root string) {
	t.Helper()
	writeFile(t, root, "composer.json", `{"type": "project", "require": {"php": ">=8.4"}}`)
	writeFile(t, root, "composer.lock", `{"packages": []}`)
	writeFile(t, root, frontController, "<?php\n")
}

func TestPackageAssemblesAnImageOfAnApplicationAndItsProductionDependencies(t *testing.T) {
	spectest.Proves(t, feature, "packages-an-application-image", "package-assembles-an-image-of-an-application-and-its-production-dependencies")
	log, _ := prepareComposer(t)
	root := t.TempDir()
	application(t, root)
	t.Setenv("STUB_COMPOSER_FAILS", "1")

	status, _, err := runProjectJob(t, root, ".", PackageDockerJob(stack(t)), nil)
	if status != "FAILED" || err == nil || !strings.HasPrefix(err.Error(), CodeImageFailed) {
		t.Fatalf("package: %s %v", status, err)
	}
	lines := readLines(t, log)
	if len(lines) != 1 || !strings.HasSuffix(lines[0], "composer.phar install --no-interaction --no-progress --no-dev --no-scripts --optimize-autoloader") {
		t.Fatalf("composer ran with %q", lines)
	}
	if strings.HasPrefix(lines[0], root+":") {
		t.Errorf("Composer ran in the repository: %q", lines[0])
	}

	staged := t.TempDir()
	writeFile(t, staged, "vendor/autoload.php", "<?php\n")
	writeFile(t, staged, "bin/console", "#!/usr/bin/env php\n")
	if err := os.Chmod(filepath.Join(staged, "bin/console"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, staged, frontController, "<?php\n")
	base := "php:8.4.23-apache@sha256:abc"
	spec, err := imageSpec(staged, t.TempDir(), base, "linux/arm64", defaultImagePort)
	if err != nil {
		t.Fatal(err)
	}
	if spec.BaseRef != base || spec.Platform != "linux/arm64" || spec.User != imageUser || spec.WorkingDir != imageAppDir || len(spec.Layers) != 3 {
		t.Fatalf("spec = %+v", spec)
	}
	if !slices.Contains(spec.Env, "PORT="+defaultImagePort) || !slices.Contains(spec.ExposedPorts, defaultImagePort+"/tcp") {
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
	writeFile(t, root, "apps/shop/composer.json", `{"repositories": [{"type": "path", "url": "../../libs/*"}, {"type": "composer", "url": "https://x"}]}`)
	writeFile(t, root, "apps/shop/composer.lock", "{}")
	writeFile(t, root, "apps/shop/.env", "A=1\n")
	writeFile(t, root, "apps/shop/.env.local", "SECRET=1\n")
	writeFile(t, root, "apps/shop/.env.prod.local", "SECRET=1\n")
	writeFile(t, root, "apps/shop/vendor/x.php", "<?php\n")
	writeFile(t, root, "apps/shop/tests/ATest.php", "<?php\n")
	writeFile(t, root, "apps/shop/src/tests/Kept.php", "<?php\n")
	writeFile(t, root, "libs/discount/composer.json", "{}")
	writeFile(t, root, "libs/discount/.phpunit.cache/x", "")

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
	writeFile(t, outer, "lib/composer.json", "{}")
	writeFile(t, root, "composer.json", `{"repositories": [{"type": "path", "url": "../lib"}]}`)
	if _, err := stageApplication(root, root, t.TempDir()); err == nil || !strings.Contains(err.Error(), "outside the workspace") {
		t.Errorf("err = %v", err)
	}
}

func TestStagingReadsTheObjectFormOfRepositories(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "composer.json", `{"repositories": {"packagist.org": false}}`)
	if _, err := stageApplication(root, root, t.TempDir()); err != nil {
		t.Errorf("err = %v", err)
	}
}

func TestBaseImageUsesTheNamedDigestOrTheOfficialImage(t *testing.T) {
	prepareComposer(t)
	root := t.TempDir()
	step := stacks.RootStep{Dir: root, Env: os.Environ(), Program: func(name string) string { return name }}
	if got, err := baseImage(context.Background(), step, "registry.test/php@sha256:abc"); err != nil || got != "registry.test/php@sha256:abc" {
		t.Errorf("named: %s %v", got, err)
	}
	if _, err := baseImage(context.Background(), step, "php:8.4-apache"); err == nil || !strings.Contains(err.Error(), "has no digest") {
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
	if _, err := os.Stat(filepath.Join(root, ".putnami/out/package/docker/channel.json")); err != nil {
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

func TestPublishPushesTheApplicationImage(t *testing.T) {
	spectest.Proves(t, feature, "publishes-an-application-image", "publish-pushes-the-application-image")
	root := t.TempDir()
	status, data, err := runPublish(t, root)
	if status != "OK" || err != nil || data["dryRun"] != true {
		t.Errorf("publish: %s %v %v", status, data, err)
	}
}
