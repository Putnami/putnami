package ruby

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/protocol/job"
	wsproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/tooling/stacks"
	"go.putnami.dev/tooling/stacks/stackstest"
)

// linuxHost makes the packaging host a Linux machine for the test.
func linuxHost(t *testing.T) {
	t.Helper()
	previous := hostOS
	hostOS = "linux"
	t.Cleanup(func() { hostOS = previous })
}

// imageApplication writes the Rails application shop under root, with files
// an image must leave out, and answers the files it must ship.
func imageApplication(t *testing.T, root string) []string {
	t.Helper()
	rackApp(t, root, "shop")
	stackstest.WriteFile(t, root, "shop/bin/rails", "#!/usr/bin/env ruby\n")
	if err := os.Chmod(filepath.Join(root, "shop/bin/rails"), 0o755); err != nil {
		t.Fatal(err)
	}
	stackstest.WriteFile(t, root, "shop/app/controllers/home_controller.rb", "class HomeController; end\n")
	stackstest.WriteFile(t, root, "shop/config/credentials.yml.enc", "sealed\n")
	stackstest.WriteFile(t, root, "shop/config/master.key", "secret\n")
	stackstest.WriteFile(t, root, "shop/config/credentials/production.key", "secret\n")
	stackstest.WriteFile(t, root, "shop/.env", "SECRET=1\n")
	stackstest.WriteFile(t, root, "shop/.env.production", "SECRET=1\n")
	stackstest.WriteFile(t, root, "shop/config/.env.example", "SECRET=\n")
	stackstest.WriteFile(t, root, "shop/spec/home_spec.rb", "RSpec.describe HomeController\n")
	stackstest.WriteFile(t, root, "shop/test/home_test.rb", "class HomeTest; end\n")
	stackstest.WriteFile(t, root, "shop/tmp/cache/bootsnap", "x\n")
	stackstest.WriteFile(t, root, "shop/log/development.log", "x\n")
	stackstest.WriteFile(t, root, "shop/storage/development.sqlite3", "x\n")
	stackstest.WriteFile(t, root, "shop/vendor/bundle/ruby/3.3.0/gems/rack-3.0.0/lib/rack.rb", "x\n")
	stackstest.WriteFile(t, root, "shop/vendor/javascript/chart.js", "x\n")
	stackstest.WriteFile(t, root, "shop/.bundle/config", "BUNDLE_PATH: vendor/bundle\n")
	return []string{"config.ru", "Gemfile", "Gemfile.lock", "bin/rails", "app/controllers/home_controller.rb", "config/credentials.yml.enc", "vendor/javascript/chart.js"}
}

// imageAt answers the image at reference and the tar headers of its files.
func imageAt(t *testing.T, reference string) (*v1.ConfigFile, map[string]*tar.Header) {
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
	files := map[string]*tar.Header{}
	reader := tar.NewReader(flat)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return config, files
		}
		if err != nil {
			t.Fatal(err)
		}
		files[strings.TrimSuffix(strings.TrimPrefix(header.Name, "/"), "/")] = header
	}
}

func TestPackageAssemblesAnImageOfAnApplicationAndItsProductionBundle(t *testing.T) {
	spectest.Proves(t, feature, "packages-an-application-image", "package-assembles-an-image-of-an-application-and-its-production-bundle")
	linuxHost(t)
	log := rubyTools(t)
	machineCache(t)
	root := workspaceRoot(t)
	host, base := stackstest.ImageRegistry(t)
	shipped := imageApplication(t, root)

	status, _, err := runProjectJob(t, root, "shop", PackageDockerJob(stack(t)), nil, "--docker-base-image", base)
	if status != "OK" || err != nil {
		t.Fatalf("package: %s %v", status, err)
	}
	lines := readLines(t, log)
	if len(lines) != 1 || !strings.HasSuffix(lines[0], ": bundle install") || strings.HasPrefix(lines[0], root) {
		t.Fatalf("bundle install runs once, outside the repository: %q", lines)
	}
	env := readLines(t, log+".env")[0]
	for _, want := range []string{"BUNDLE_DEPLOYMENT=1", "BUNDLE_WITHOUT=development:test"} {
		if !slices.Contains(strings.Fields(env), want) {
			t.Errorf("env has no %s: %q", want, env)
		}
	}
	if strings.Contains(env, "BUNDLE_PATH="+root) || strings.Contains(env, "BUNDLE_APP_CONFIG="+root) {
		t.Errorf("Bundler writes in the repository: %q", env)
	}
	bundle := ""
	for _, field := range strings.Fields(env) {
		if value, ok := strings.CutPrefix(field, "BUNDLE_PATH="); ok {
			bundle = value
		}
	}
	if bundle == "" || !strings.Contains(env, "CONFIGURE_ARGS=--with-cppflags='-DSTUB -ffile-prefix-map="+bundle+"=/usr/local/bundle'") {
		t.Errorf("the extensions do not compile with the bundle mapped to the image's: %q", env)
	}
	var manifest pkgmeta.DockerManifest
	data, err := os.ReadFile(filepath.Join(root, ".putnami/out/shop/package/docker/manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Image != "shop" || manifest.Digest == "" || manifest.Platform != defaultPlatform {
		t.Fatalf("manifest = %+v", manifest)
	}

	// The image reaches the registry, and it holds what staging shipped.
	spectest.Proves(t, feature, "publishes-an-application-image", "publish-pushes-the-application-image")
	status, _, err = quietly(t, func() (string, map[string]any, error) {
		ctx := &pctx.Context{WorkspaceRoot: root, Project: pctx.Project{Name: "shop", Path: "shop", FullPath: filepath.Join(root, "shop")}}
		return PublishDockerJob()(ctx, jsonl.New(), []string{"--docker-registry", host + "/acme"})
	})
	if status != "OK" || err != nil {
		t.Fatalf("publish: %s %v", status, err)
	}
	config, files := imageAt(t, host+"/acme/shop@"+manifest.Digest)
	app := strings.TrimPrefix(imageAppDir, "/") + "/"
	for _, want := range shipped {
		if files[app+want] == nil {
			t.Errorf("the image has no %s", want)
		}
	}
	for _, left := range []string{"config/master.key", "config/credentials/production.key", ".env", ".env.production", "config/.env.example", "spec/home_spec.rb", "test/home_test.rb", "tmp/cache/bootsnap", "log/development.log", "storage/development.sqlite3", "vendor/bundle/ruby/3.3.0/gems/rack-3.0.0/lib/rack.rb", ".bundle/config"} {
		if files[app+left] != nil {
			t.Errorf("the image holds %s", left)
		}
	}
	gems := strings.TrimPrefix(imageBundlePath, "/") + "/ruby/3.3.0/"
	for _, want := range []string{"gems/rack-3.1.0/lib/rack.rb", "extensions/x86_64-linux/3.3.0/puma-6.4.0/gem.build_complete", "extensions/x86_64-linux/3.3.0/puma-6.4.0/puma_http11.so", "gems/puma-6.4.0/ext/puma_http11/puma_http11.c", "bundler/gems/tool-abc123/tool.rb", "bin/rackup"} {
		if files[gems+want] == nil {
			t.Errorf("the bundle layer has no %s", want)
		}
	}
	for _, left := range []string{"cache/rack-3.1.0.gem", "bundler/gems/tool-abc123/.git/HEAD", "extensions/x86_64-linux/3.3.0/puma-6.4.0/gem_make.out", "extensions/x86_64-linux/3.3.0/puma-6.4.0/mkmf.log", "gems/puma-6.4.0/ext/puma_http11/Makefile", "gems/puma-6.4.0/ext/puma_http11/puma_http11.o"} {
		if files[gems+left] != nil {
			t.Errorf("the bundle layer holds %s", left)
		}
	}
	if header := files[gems+"bin/rackup"]; header == nil || header.Mode != 0o755 {
		t.Errorf("rackup = %+v", header)
	}
	for _, dir := range writableDirs {
		if header := files[app+dir]; header == nil || header.Typeflag != tar.TypeDir || header.Uid != imageUID {
			t.Errorf("%s is not a directory of the image's user: %+v", dir, header)
		}
	}
	if config.Config.User != imageUser || config.Config.WorkingDir != imageAppDir || !slices.Equal(config.Config.Entrypoint, []string{"/bin/sh", "-c", `exec bundle exec rails server -b 0.0.0.0 -p "$PORT"`}) {
		t.Errorf("config = %+v", config.Config)
	}
	for _, want := range []string{"PORT=8080", "RAILS_ENV=production", "RACK_ENV=production", "RAILS_LOG_TO_STDOUT=1", "RAILS_SERVE_STATIC_FILES=1", "BUNDLE_DEPLOYMENT=1", "BUNDLE_PATH=/usr/local/bundle", "BUNDLE_WITHOUT=development:test"} {
		if !slices.Contains(config.Config.Env, want) {
			t.Errorf("the image env has no %s: %v", want, config.Config.Env)
		}
	}
	if _, exposed := config.Config.ExposedPorts["8080/tcp"]; !exposed {
		t.Errorf("exposed ports = %v", config.Config.ExposedPorts)
	}
}

func TestAStaticRubyBuildsExtensionsTheImagesRubyFinds(t *testing.T) {
	spectest.Proves(t, feature, "packages-an-application-image", "a-static-ruby-builds-extensions-the-images-ruby-finds")
	linuxHost(t)
	rubyTools(t)
	machineCache(t)
	root := workspaceRoot(t)
	host, base := stackstest.ImageRegistry(t)
	rackApp(t, root, "shop")
	t.Setenv("STUB_RUBY_HOST", `3.3.6\naarch64\nlinux\n\n3.3.0-static\n`)
	t.Setenv("STUB_EXTENSION_API", "3.3.0-static")
	status, _, err := runProjectJob(t, root, "shop", PackageDockerJob(stack(t)), nil, "--docker-base-image", base, "--platform", "linux/arm64")
	if status != "OK" || err != nil {
		t.Fatalf("package: %s %v", status, err)
	}
	var manifest pkgmeta.DockerManifest
	data, err := os.ReadFile(filepath.Join(root, ".putnami/out/shop/package/docker/manifest.json"))
	if err != nil || json.Unmarshal(data, &manifest) != nil {
		t.Fatalf("manifest: %v", err)
	}
	status, _, err = quietly(t, func() (string, map[string]any, error) {
		ctx := &pctx.Context{WorkspaceRoot: root, Project: pctx.Project{Name: "shop", Path: "shop", FullPath: filepath.Join(root, "shop")}}
		return PublishDockerJob()(ctx, jsonl.New(), []string{"--docker-registry", host + "/acme"})
	})
	if status != "OK" || err != nil {
		t.Fatalf("publish: %s %v", status, err)
	}
	config, files := imageAt(t, host+"/acme/shop@"+manifest.Digest)
	extensions := strings.TrimPrefix(imageBundlePath, "/") + "/ruby/3.3.0/extensions/x86_64-linux/"
	if files[extensions+"3.3.0/puma-6.4.0/gem.build_complete"] == nil || files[extensions+"3.3.0-static/puma-6.4.0/gem.build_complete"] != nil {
		t.Errorf("the extensions are not where a shared Ruby reads them")
	}
	if manifest.Platform != "linux/arm64" || !slices.Equal(config.Config.Entrypoint, []string{"/bin/sh", "-c", `exec bundle exec rackup -o 0.0.0.0 -p "$PORT"`}) {
		t.Errorf("platform %s, config %+v", manifest.Platform, config.Config)
	}
}

func TestPackagingTheSameTreeTwiceGivesTheSameImage(t *testing.T) {
	spectest.Proves(t, feature, "packages-an-application-image", "packaging-the-same-tree-twice-gives-the-same-image")
	linuxHost(t)
	rubyTools(t)
	machineCache(t)
	root := workspaceRoot(t)
	_, base := stackstest.ImageRegistry(t)
	imageApplication(t, root)
	digests := []string{}
	for range 2 {
		status, _, err := runProjectJob(t, root, "shop", PackageDockerJob(stack(t)), nil, "--docker-base-image", base)
		if status != "OK" || err != nil {
			t.Fatalf("package: %s %v", status, err)
		}
		var manifest pkgmeta.DockerManifest
		data, err := os.ReadFile(filepath.Join(root, ".putnami/out/shop/package/docker/manifest.json"))
		if err != nil || json.Unmarshal(data, &manifest) != nil {
			t.Fatalf("manifest: %v", err)
		}
		digests = append(digests, manifest.Digest)
	}
	if digests[0] == "" || digests[0] != digests[1] {
		t.Errorf("two packagings of one tree give %v", digests)
	}
}

func TestPackageKeepsTheConfigureArgumentsOfTheEnvironment(t *testing.T) {
	spectest.Proves(t, feature, "packages-an-application-image", "packaging-the-same-tree-twice-gives-the-same-image")
	linuxHost(t)
	log := rubyTools(t)
	machineCache(t)
	root := workspaceRoot(t)
	_, base := stackstest.ImageRegistry(t)
	rackApp(t, root, "shop")
	t.Setenv("CONFIGURE_ARGS", "--with-opt-dir=/opt")
	status, _, err := runProjectJob(t, root, "shop", PackageDockerJob(stack(t)), nil, "--docker-base-image", base)
	if status != "OK" || err != nil {
		t.Fatalf("package: %s %v", status, err)
	}
	if env := readLines(t, log+".env")[0]; !strings.Contains(env, "CONFIGURE_ARGS=--with-opt-dir=/opt --with-cppflags='-DSTUB -ffile-prefix-map=") {
		t.Errorf("the environment's arguments are lost: %q", env)
	}

	// A temporary directory no compiler flag carries still packages, with
	// the environment's arguments only.
	spaced := filepath.Join(t.TempDir(), "with space")
	if err := os.Mkdir(spaced, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", spaced)
	status, _, err = runProjectJob(t, root, "shop", PackageDockerJob(stack(t)), nil, "--docker-base-image", base)
	if status != "OK" || err != nil {
		t.Fatalf("package: %s %v", status, err)
	}
	if env := readLines(t, log+".env")[1]; !strings.Contains(env, "CONFIGURE_ARGS=--with-opt-dir=/opt ") && !strings.HasSuffix(env, "CONFIGURE_ARGS=--with-opt-dir=/opt") {
		t.Errorf("CONFIGURE_ARGS changes under %s: %q", spaced, env)
	}
}

func TestTheBundleLayerLeavesOutWhatTheImageNeverRuns(t *testing.T) {
	spectest.Proves(t, feature, "packages-an-application-image", "packaging-the-same-tree-twice-gives-the-same-image")
	cases := map[string]bool{
		"ruby/3.4.0/gems/puma-8.0.2/lib/puma/puma_http11.so":         true,
		"ruby/3.4.0/gems/puma-8.0.2/ext/puma_http11/http11_parser.c": true,
		"ruby/3.4.0/gems/puma-8.0.2/ext/puma_http11/extconf.rb":      true,
		"ruby/3.4.0/gems/puma-8.0.2/ext/puma_http11/Makefile":        false,
		"ruby/3.4.0/gems/puma-8.0.2/ext/puma_http11/http11_parser.o": false,
		"ruby/3.4.0/gems/puma-8.0.2/ext/puma_http11/mkmf.log":        false,
		"ruby/3.4.0/gems/json-3.1.0/ext/json/ext/parser/Makefile":    false,
		"ruby/3.4.0/gems/make-1.0.0/lib/make/Makefile":               true,
		"ruby/3.4.0/gems/ext": true,
		"ruby/3.4.0/extensions/aarch64-linux/3.4.0/puma-8.0.2/puma/puma_http11.so": true,
		"ruby/3.4.0/extensions/aarch64-linux/3.4.0/puma-8.0.2/gem.build_complete":  true,
		"ruby/3.4.0/extensions/aarch64-linux/3.4.0/puma-8.0.2/gem_make.out":        false,
		"ruby/3.4.0/extensions/aarch64-linux/3.4.0/json-3.1.0/mkmf.log":            false,
		"ruby/3.4.0/bundler/gems/tool-abc123/ext/tool/Makefile":                    false,
		"ruby/3.4.0/bundler/gems/tool-abc123/ext/tool/tool.c":                      true,
		"ruby/3.4.0/bundler/gems/tool-abc123/.git":                                 false,
		"ruby/3.4.0/bundler/gems/tool-abc123/lib/tool.rb":                          true,
		"ruby/3.4.0/cache/rack-3.1.0.gem":                                          false,
		"ruby/3.4.0/bin/rackup":                                                    true,
		"ruby/3.4.0/specifications/puma-8.0.2.gemspec":                             true,
		"bin/Makefile": true,
	}
	for rel, kept := range cases {
		if bundleFileKept(rel) != kept {
			t.Errorf("bundleFileKept(%s) = %v, want %v", rel, !kept, kept)
		}
	}
}

func TestTheExtensionsCompileWithTheBundleMappedToTheImagesPath(t *testing.T) {
	spectest.Proves(t, feature, "packages-an-application-image", "packaging-the-same-tree-twice-gives-the-same-image")
	cases := []struct {
		existing, cppflags, bundle, want string
		mapped                           bool
	}{
		{"", " $(DEFS) $(cppflags)", "/tmp/putnami-ruby-image-1/bundle", `--with-cppflags='$(DEFS) $(cppflags) -ffile-prefix-map=/tmp/putnami-ruby-image-1/bundle=/usr/local/bundle'`, true},
		{" --with-opt-dir=/opt ", "", "/tmp/x_y/bundle", `--with-opt-dir=/opt --with-cppflags='-ffile-prefix-map=/tmp/x_y/bundle=/usr/local/bundle'`, true},
		{"", `-DNAME='x'`, "/tmp/b", `--with-cppflags='-DNAME='\''x'\'' -ffile-prefix-map=/tmp/b=/usr/local/bundle'`, true},
		{"--with-opt-dir=/opt", "", "/tmp/my dir/bundle", "--with-opt-dir=/opt", false},
		{"", "", "/tmp/a=b/bundle", "", false},
	}
	for _, c := range cases {
		got, mapped := configureArgs(c.existing, c.cppflags, c.bundle)
		if got != c.want || mapped != c.mapped {
			t.Errorf("configureArgs(%q, %q, %q) = %q %v, want %q %v", c.existing, c.cppflags, c.bundle, got, mapped, c.want, c.mapped)
		}
	}
}

func TestPackageRefusesAHostThatCannotBuildTheImage(t *testing.T) {
	spectest.Proves(t, feature, "refuses-a-foreign-packaging-host", "package-refuses-a-host-that-cannot-build-the-images-native-extensions")
	cases := map[string]struct {
		os, host, platform string
	}{
		"macOS":                 {"darwin", `3.3.6\narm64\ndarwin\n23\n3.3.0\n`, "linux/arm64"},
		"another architecture":  {"linux", defaultHost, "linux/arm64"},
		"musl":                  {"linux", `3.3.6\nx86_64\nlinux\nmusl\n3.3.0\n`, "linux/amd64"},
		"an unknown target cpu": {"linux", defaultHost, "linux/riscv64"},
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			previous := hostOS
			hostOS = c.os
			t.Cleanup(func() { hostOS = previous })
			log := rubyTools(t)
			machineCache(t)
			root := workspaceRoot(t)
			rackApp(t, root, "shop")
			t.Setenv("STUB_RUBY_HOST", c.host)
			status, _, err := runProjectJob(t, root, "shop", PackageDockerJob(stack(t)), job.Params{"platform": json.RawMessage(`"` + c.platform + `"`)})
			if status != "FAILED" || err == nil || !strings.HasPrefix(err.Error(), CodeHostMismatch) || !strings.Contains(err.Error(), "package on a Linux runner of the target architecture") || !strings.Contains(err.Error(), c.platform) {
				t.Errorf("package: %s %v", status, err)
			}
			if lines := readLines(t, log); lines != nil {
				t.Errorf("bundle ran: %q", lines)
			}
		})
	}
}

func TestBaseImageUsesTheNamedDigestOrTheOfficialImage(t *testing.T) {
	spectest.Proves(t, feature, "packages-an-application-image", "the-base-is-the-official-ruby-slim-image-or-a-digest-pinned-base")
	host := rubyHost{version: "3.4.12"}
	named := "registry.test/ruby@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if got, err := baseImage(host, named); err != nil || got != named {
		t.Errorf("named: %s %v", got, err)
	}
	if _, err := baseImage(host, "ruby:3.4-slim"); err == nil || !strings.Contains(err.Error(), "no valid digest") {
		t.Errorf("no digest: %v", err)
	}
	if got, err := baseImage(host, ""); err != nil || got != slimBases["3.4"] || !strings.HasPrefix(got, "ruby:3.4.") {
		t.Errorf("official: %s %v", got, err)
	}
	if _, err := baseImage(rubyHost{version: "2.7.8"}, ""); err == nil || !strings.Contains(err.Error(), "--docker-base-image") {
		t.Errorf("unpinned: %v", err)
	}
	for minor, base := range slimBases {
		if !strings.HasPrefix(base, "ruby:"+minor+".") || !strings.Contains(base, "-slim-trixie@sha256:") {
			t.Errorf("%s: %s", minor, base)
		}
	}
}

func TestAStagedImageNeverCarriesASecretFile(t *testing.T) {
	for rel, want := range map[string]bool{
		"config/master.key": true, "config/credentials/production.key": true, ".env": true, ".env.example": true,
		"config/.env.local": true, ".envrc": true, "config/credentials.yml.enc": false, "config/credentials/production.yml.enc": false,
		"engines/a/config/master.key": false, "config.ru": false, "app/environment.rb": false,
	} {
		if got := secretFile(rel); got != want {
			t.Errorf("secretFile(%q) = %v, want %v", rel, got, want)
		}
	}
}

func TestPackageRefusesWhatItCannotImage(t *testing.T) {
	cases := map[string]struct {
		setup func(t *testing.T, root string)
		rel   string
		want  string
	}{
		"no lock": {func(t *testing.T, root string) {
			rackApp(t, root, "shop")
			if err := os.Remove(filepath.Join(root, "shop", gemfileLock)); err != nil {
				t.Fatal(err)
			}
		}, "shop", CodeImageFailed},
		"a path gem outside the application": {func(t *testing.T, root string) {
			rackApp(t, root, "shop")
			stackstest.WriteFile(t, root, "shop/Gemfile.lock", "PATH\n  remote: ../gems/audit\n  specs:\n    audit (0.1.0)\n\nGEM\n  remote: https://rubygems.org/\n  specs:\n\nBUNDLED WITH\n   2.5.22\n")
		}, "shop", CodeImageFailed},
		"an application below another stack root": {func(t *testing.T, root string) {
			stackstest.WriteFile(t, root, "Gemfile", "source \"https://rubygems.org\"\n")
			stackstest.WriteFile(t, root, "Gemfile.lock", "GEM\n  specs:\n\nBUNDLED WITH\n   2.5.22\n")
			stackstest.WriteFile(t, root, ".ruby-version", "3.3.6\n")
			stackstest.WriteFile(t, root, "packs/admin/package.yml", "enforce_dependencies: true\n")
			stackstest.WriteFile(t, root, "packs/admin/config.ru", "run ->(env) { [200, {}, []] }\n")
		}, "packs/admin", CodeImageFailed},
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			linuxHost(t)
			log := rubyTools(t)
			root := workspaceRoot(t)
			c.setup(t, root)
			status, _, err := runProjectJob(t, root, c.rel, PackageDockerJob(stack(t)), nil)
			if status != "FAILED" || err == nil || !strings.HasPrefix(err.Error(), c.want) {
				t.Errorf("package: %s %v", status, err)
			}
			if lines := readLines(t, log); lines != nil {
				t.Errorf("bundle ran: %q", lines)
			}
		})
	}
}

func TestPackageNeedsARackConfig(t *testing.T) {
	rubyTools(t)
	root := workspaceRoot(t)
	rackApp(t, root, "shop")
	s := stack(t)
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{"shop"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "shop", rackConfig)); err != nil {
		t.Fatal(err)
	}
	ctx := &pctx.Context{WorkspaceRoot: root, Project: pctx.Project{
		Name: "shop", Path: "shop", FullPath: filepath.Join(root, "shop"),
		Metadata: map[string]json.RawMessage{ExtensionName: result.Projects[0].Metadata},
	}}
	status, _, err := quietly(t, func() (string, map[string]any, error) { return PackageDockerJob(s)(ctx, jsonl.New(), nil) })
	if status != "FAILED" || err == nil || !strings.HasPrefix(err.Error(), CodeNoRackConfig) {
		t.Errorf("package: %s %v", status, err)
	}
}

func TestPackageRefusesABaseWithNoDigest(t *testing.T) {
	rubyTools(t)
	root := workspaceRoot(t)
	rackApp(t, root, "shop")
	status, _, err := runProjectJob(t, root, "shop", PackageDockerJob(stack(t)), nil, "--docker-base-image", "ruby:3.3-slim")
	if status != "FAILED" || err == nil || !strings.HasPrefix(err.Error(), CodeBaseNotPinned) {
		t.Errorf("package: %s %v", status, err)
	}
}

func TestADryRunPackageRecordsTheChannelAndInstallsNothing(t *testing.T) {
	log := rubyTools(t)
	root := workspaceRoot(t)
	rackApp(t, root, "shop")
	status, _, err := runProjectJob(t, root, "shop", PackageDockerJob(stack(t)), job.Params{"dry-run": json.RawMessage(`true`)})
	if status != "OK" || err != nil {
		t.Fatalf("package: %s %v", status, err)
	}
	if lines := readLines(t, log); lines != nil {
		t.Errorf("bundle ran: %q", lines)
	}
	if _, err := os.Stat(filepath.Join(root, ".putnami/out/shop/package/docker/channel.json")); err != nil {
		t.Errorf("channel record: %v", err)
	}
}

func TestPublishDryRunPushesNothing(t *testing.T) {
	root := t.TempDir()
	status, data, err := quietly(t, func() (string, map[string]any, error) {
		ctx := &pctx.Context{WorkspaceRoot: root, Project: pctx.Project{Name: "shop", Path: "shop", FullPath: filepath.Join(root, "shop")}}
		return PublishDockerJob()(ctx, jsonl.New(), []string{"--docker-registry", "registry.test/acme", "--dry-run"})
	})
	if status != "OK" || err != nil || data["dryRun"] != true {
		t.Errorf("publish: %s %v %v", status, data, err)
	}
}

func TestAnApplicationThatPublishesAnImageIsAReleaseSetMember(t *testing.T) {
	root := t.TempDir()
	rackApp(t, root, "shop")
	stackstest.WriteFile(t, root, "shop/putnami.json", `{"options": {"publish": {"docker": true}}}`)
	stackstest.WriteFile(t, root, "putnami.workspace.json", `{"registries": {"oci": {"publish": "registry.test/acme"}}}`)
	stackstest.WriteFile(t, root, "gems/audit/audit.gemspec", "Gem::Specification.new do |s|\n  s.name = \"audit\"\nend\n")
	stackstest.WriteFile(t, root, "gems/audit/Gemfile", "source \"https://rubygems.org\"\ngemspec\n")
	stackstest.WriteFile(t, root, "gems/audit/putnami.json", `{"publish": ["docker"]}`)
	s := stack(t)
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{"gems/audit", "shop"}})
	if err != nil || len(result.Projects) != 2 {
		t.Fatalf("probe: %v %+v", err, result)
	}
	for _, p := range result.Projects {
		var meta stacks.Metadata
		if err := json.Unmarshal(p.Metadata, &meta); err != nil {
			t.Fatal(err)
		}
		switch p.Path {
		case "shop":
			if p.Type != TypeApplication || meta.ReleaseSet == nil || len(meta.ReleaseSet.Ecosystems) != 1 {
				t.Fatalf("shop: type %q, release set %+v", p.Type, meta.ReleaseSet)
			}
			member := meta.ReleaseSet.Ecosystems[0]
			if member.Ecosystem != "oci" || member.Coordinate != "acme/shop" || member.PackageStep != "docker" || member.PublishStep != "docker" {
				t.Errorf("member = %+v", member)
			}
		case "gems/audit":
			if p.Type != TypeLibrary || meta.ReleaseSet != nil {
				t.Errorf("a gem publishes no image: type %q, release set %+v", p.Type, meta.ReleaseSet)
			}
		}
	}
}

func TestAProjectNamedWithUppercaseLettersPackagesALowercaseImage(t *testing.T) {
	rubyTools(t)
	root := workspaceRoot(t)
	rackApp(t, root, "Shop")
	stackstest.WriteFile(t, root, "Shop/putnami.json", `{"publish": ["docker"]}`)
	stackstest.WriteFile(t, root, "putnami.workspace.json", `{"registries": {"oci": {"publish": "registry.test/acme"}}}`)
	if status, _, err := runProjectJob(t, root, "Shop", PackageDockerJob(stack(t)), job.Params{"dry-run": json.RawMessage(`true`)}); status != "OK" || err != nil {
		t.Fatalf("package: %s %v", status, err)
	}
	index, err := pkgmeta.ReadChannelIndex(root, "Shop")
	if err != nil || index.Records[dockerChannel].Artifact != "shop" {
		t.Fatalf("channel records = %+v, %v", index, err)
	}
	// The shared publisher finds the release-set member of the packaged image
	// by its name.
	s := stack(t)
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{"Shop"}})
	if err != nil || len(result.Projects) != 1 {
		t.Fatalf("probe: %v %+v", err, result)
	}
	var meta stacks.Metadata
	if err := json.Unmarshal(result.Projects[0].Metadata, &meta); err != nil || meta.ReleaseSet == nil || len(meta.ReleaseSet.Ecosystems) != 1 {
		t.Fatalf("release set: %v %s", err, result.Projects[0].Metadata)
	}
	if member := meta.ReleaseSet.Ecosystems[0]; member.Coordinate != "acme/shop" {
		t.Errorf("member = %+v", member)
	}
}

func TestAStagedSymbolicLinkFailsPackaging(t *testing.T) {
	source := t.TempDir()
	stackstest.WriteFile(t, source, "public/app.js", "a")
	if err := os.Symlink("app.js", filepath.Join(source, "public/linked.js")); err != nil {
		t.Skipf("no symbolic links: %v", err)
	}
	if err := copyTree(source, t.TempDir()); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("copyTree = %v", err)
	}
}
