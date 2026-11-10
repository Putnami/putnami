package ruby

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/dockerpublish"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/oci"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/tooling/stacks"
)

const (
	// PackageDockerJobName is the job that assembles an application image.
	PackageDockerJobName = "package-docker"
	// PublishDockerJobName is the job that pushes an application image.
	PublishDockerJobName = "publish-docker"
)

const (
	// CodeImageFailed is the diagnostic of an image that cannot be assembled.
	CodeImageFailed = "ruby.image_failed"
	// CodeBaseNotPinned is the diagnostic of a base image with no digest.
	CodeBaseNotPinned = "ruby.base_not_pinned"
	// CodeHostMismatch is the diagnostic of a packaging host that cannot
	// build the image's native extensions.
	CodeHostMismatch = "ruby.package_host_mismatch"
)

const (
	dockerChannel    = "docker"
	imageAppDir      = "/app"
	imageBundlePath  = "/usr/local/bundle"
	imageUser        = "65534:65534"
	imageUID         = 65534
	defaultImagePort = "8080"
	defaultPlatform  = "linux/amd64"
	bundleWithout    = "development:test"
)

// slimBases are the official Ruby slim images on Debian trixie, pinned by the
// digest of their multi-platform index, by Ruby minor version.
var slimBases = map[string]string{
	"3.2": "ruby:3.2.11-slim-trixie@sha256:84184c9e2c368885a1d0c93ad1953c33d81081058d274b87b4aa6f3e209e5d16",
	"3.3": "ruby:3.3.13-slim-trixie@sha256:db3ad6dd68a886d51a187fb9bf486052f2976cdbe3b750169c25092aba1297fa",
	"3.4": "ruby:3.4.12-slim-trixie@sha256:65680693964528b522a38d85ec5065538c7ab9e75268802aab9770f39ee56de0",
	"4.0": "ruby:4.0.9-slim-trixie@sha256:d8cb2c0714bdaba0f318af897a01d2dbde3cfa8e62d47e99cea55feb13c0e90d",
}

// rubyCPUs are the RubyGems CPU names of the image platforms.
var rubyCPUs = map[string]string{"amd64": "x86_64", "arm64": "aarch64"}

// hostOS is the operating system of the packaging host.
var hostOS = runtime.GOOS

// stagingExcludes are the top-level names of a stack root an image leaves out:
// what a run writes, what only tests and development read, and the bundle a
// machine installed.
var stagingExcludes = []string{".bundle", ".gen", ".git", ".putnami", "coverage", "log", "node_modules", "spec", "storage", "test", "tmp"}

// writableDirs are the application directories the image's user writes:
// Rails writes its pid file and caches under tmp/ and may log under log/.
var writableDirs = []string{"log", "tmp", "tmp/cache", "tmp/pids", "tmp/sockets"}

// hostProbe prints what the packaging host's Ruby builds native extensions
// for, one value per line: its version, the CPU, OS and libc of its RubyGems
// platform, its extension API directory, and the preprocessor flags mkmf
// writes in an extension's Makefile.
const hostProbe = `p = Gem::Platform.local; print [RUBY_VERSION, p.cpu, p.os, p.version.to_s, Gem.extension_api_version, RbConfig::MAKEFILE_CONFIG["CPPFLAGS"].to_s].join("\n")`

// rubyHost is what the packaging host's Ruby builds native extensions for.
type rubyHost struct {
	version, cpu, os, libc, extensionAPI, cppflags string
}

// minor is the Ruby minor version, such as 3.4.
func (h rubyHost) minor() string {
	parts := strings.SplitN(h.version, ".", 3)
	if len(parts) < 2 {
		return h.version
	}
	return parts[0] + "." + parts[1]
}

func (h rubyHost) String() string {
	platform := h.cpu + "-" + h.os
	if h.libc != "" {
		platform += "-" + h.libc
	}
	return fmt.Sprintf("%s with Ruby %s (%s)", hostOS, h.version, platform)
}

// PublishDockerJob pushes the packaged image through the shared publisher.
func PublishDockerJob() cli.JobFunc {
	return dockerpublish.Publish
}

// PackageDockerJob assembles the image of an application: a layer of its
// production bundle, a layer of its files and a layer of the directories its
// user writes, on a digest-pinned Ruby base. It needs no Docker daemon. The
// bundle is installed on the packaging host, so the host must be a Linux
// machine of the image's architecture.
func PackageDockerJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		if ctx.Project.Type == TypeLibrary {
			return "SKIP", nil, nil
		}
		root, err := stack.Selected(ctx, emit)
		if err != nil {
			return "FAILED", nil, err
		}
		if err := assembleImage(context.Background(), ctx, root, cli.ParseFlags(args), emit); err != nil {
			return "FAILED", nil, err
		}
		return "OK", nil, nil
	}
}

func assembleImage(run context.Context, ctx *pctx.Context, root stacks.RootStep, flags map[string]string, emit *jsonl.Emitter) error {
	fail := func(code string, err error) error {
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, code)
		return fmt.Errorf("%s: %w", code, err)
	}
	if filepath.Clean(root.Dir) != filepath.Clean(root.Project) {
		return fail(CodeImageFailed, fmt.Errorf("%s has its stack root at %s: an image packages an application with a Gemfile of its own", root.Project, root.Dir))
	}
	if !isFile(filepath.Join(root.Project, rackConfig)) {
		return fail(CodeNoRackConfig, fmt.Errorf("%s has no %s: the image has no application to serve", root.Project, rackConfig))
	}
	if !isFile(filepath.Join(root.Dir, gemfileLock)) {
		return fail(CodeImageFailed, fmt.Errorf("%s has no %s: an image installs the locked gems", root.Dir, gemfileLock))
	}
	if err := pathGemsInside(root.Dir); err != nil {
		return fail(CodeImageFailed, err)
	}
	host, err := probeHost(run, root)
	if err != nil {
		return fail(CodeImageFailed, err)
	}
	base, err := baseImage(host, cli.FlagString(flags, "docker-base-image", ctx.Params.String("docker-base-image", "dockerBaseImage")))
	if err != nil {
		return fail(CodeBaseNotPinned, err)
	}
	platform := cli.FlagString(flags, "platform", ctx.Params.String("platform"))
	if platform == "" {
		platform = defaultPlatform
	}

	output := filepath.Join(ctx.WorkspaceRoot, ".putnami", "out", ctx.Project.Path, "package", dockerChannel)
	name := stacks.ImageName(ctx.Project.Name)
	version := "0.0.0"
	if ctx.Version != nil && ctx.Version.Full != "" {
		version = ctx.Version.Full
	}
	record := func() error {
		return pkgmeta.WriteChannelRecord(output, pkgmeta.ChannelRecord{Version: version, Artifact: name, Channels: []string{dockerChannel}})
	}
	if cli.FlagBool(flags, "dry-run", ctx.Params.Bool("dry-run", false, "dryRun")) {
		emit.Info("dry run: would assemble an image of " + name + " on " + base + " for " + platform)
		if err := record(); err != nil {
			return fail(CodeImageFailed, err)
		}
		return nil
	}
	if err := checkHost(host, platform); err != nil {
		return fail(CodeHostMismatch, err)
	}

	workspace, err := os.MkdirTemp("", "putnami-ruby-image-")
	if err != nil {
		return fail(CodeImageFailed, err)
	}
	defer func() { _ = os.RemoveAll(workspace) }()
	staged := filepath.Join(workspace, "app")
	if err := copyTree(root.Dir, staged); err != nil {
		return fail(CodeImageFailed, err)
	}
	bundle := filepath.Join(workspace, "bundle")
	if err := installBundle(run, root, host, staged, bundle, filepath.Join(workspace, "config"), gemCache(ctx.WorkspaceRoot), emit); err != nil {
		return fail(CodeImageFailed, err)
	}
	if err := sharedExtensionDirs(bundle, host); err != nil {
		return fail(CodeImageFailed, err)
	}
	writable := filepath.Join(workspace, "writable.tar")
	if err := writeWritableDirs(writable); err != nil {
		return fail(CodeImageFailed, err)
	}
	spec, err := imageSpec(staged, bundle, writable, base, platform, defaultImagePort)
	if err != nil {
		return fail(CodeImageFailed, err)
	}
	plan, err := oci.NewImagePlan(spec, oci.PlanOptions{})
	if err != nil {
		return fail(CodeImageFailed, fmt.Errorf("plan the image: %w", err))
	}
	digest, err := oci.AssemblePlanToLayout(plan, filepath.Join(output, "oci"), oci.LayoutOptions{
		BaseCacheDir:  filepath.Join(ctx.WorkspaceRoot, ".putnami", "cache", "oci-base"),
		LayerCacheDir: oci.LayerCacheRootFromEnv(),
	})
	if err != nil {
		return fail(CodeImageFailed, fmt.Errorf("assemble the image: %w", err))
	}
	contentHash := plan.Identity[:12]
	tags := []string{name + ":c-" + contentHash}
	if tag := cli.FlagString(flags, "docker-tag", ctx.Params.String("docker-tag", "dockerTag")); tag != "" {
		tags = append(tags, name+":"+tag)
	}
	if cli.FlagBool(flags, "docker-load", ctx.Params.Bool("docker-load", false, "dockerLoad")) {
		if err := loadImage(filepath.Join(output, "oci"), tags); err != nil {
			return fail(CodeImageFailed, err)
		}
	}
	manifest, err := json.MarshalIndent(pkgmeta.DockerManifest{
		Image: name, Tags: tags, Version: version, ContentHash: contentHash, Digest: digest, Layout: "oci", Platform: spec.Platform,
	}, "", "  ")
	if err != nil {
		return fail(CodeImageFailed, err)
	}
	if err := os.WriteFile(filepath.Join(output, "manifest.json"), append(manifest, '\n'), 0o644); err != nil { //nolint:gosec // an output of the job
		return fail(CodeImageFailed, err)
	}
	if err := record(); err != nil {
		return fail(CodeImageFailed, err)
	}
	emit.Info(fmt.Sprintf("assembled the image %s (%s)", tags[0], digest))
	return nil
}

// probeHost asks the selected Ruby what it builds native extensions for.
func probeHost(run context.Context, root stacks.RootStep) (rubyHost, error) {
	cmd := exec.CommandContext(run, root.Program("ruby"), "-e", hostProbe) //nolint:gosec // the selected Ruby
	cmd.Dir = root.Dir
	cmd.Env = root.Env
	out, err := cmd.Output()
	if err != nil {
		return rubyHost{}, fmt.Errorf("read the Ruby platform: %w", err)
	}
	fields := strings.Split(string(out), "\n")
	if len(fields) != 6 || fields[0] == "" {
		return rubyHost{}, fmt.Errorf("read the Ruby platform: ruby printed %q", out)
	}
	return rubyHost{version: fields[0], cpu: fields[1], os: fields[2], libc: fields[3], extensionAPI: fields[4], cppflags: fields[5]}, nil
}

// baseImage answers the named base, which must carry a digest, or the official
// slim image of the Ruby minor version that installs the bundle.
func baseImage(host rubyHost, named string) (string, error) {
	if named != "" {
		if _, err := oci.ParseDigestReference(named); err != nil {
			return "", fmt.Errorf("the base image %s has no valid digest: name it as image@sha256:<digest>: %w", named, err)
		}
		return named, nil
	}
	base, ok := slimBases[host.minor()]
	if !ok {
		return "", fmt.Errorf("no official image is pinned for Ruby %s: name a digest-pinned base with --docker-base-image", host.minor())
	}
	return base, nil
}

// checkHost refuses a packaging host whose native extensions the image cannot
// load: the bundle is built on the host, so the host must be a glibc Linux of
// the image's architecture. The image's Ruby minor is the host's, since the
// base follows it.
func checkHost(host rubyHost, platform string) error {
	parts := strings.Split(platform, "/")
	targetCPU := ""
	if len(parts) >= 2 && parts[0] == "linux" {
		targetCPU = rubyCPUs[parts[1]]
	}
	if hostOS == "linux" && host.os == "linux" && (host.libc == "" || host.libc == "gnu") && targetCPU != "" && host.cpu == targetCPU {
		return nil
	}
	return fmt.Errorf("the bundle's native extensions build for the packaging host, %s, and the image targets %s: package on a Linux runner of the target architecture", host, platform)
}

// pathGemsInside refuses a bundle that names a path gem outside the stack
// root: an image holds the application directory only.
func pathGemsInside(stackRoot string) error {
	data, err := os.ReadFile(filepath.Join(stackRoot, gemfileLock)) //nolint:gosec // inside the stack root
	if err != nil {
		return err
	}
	section := ""
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		if line != "" && !strings.HasPrefix(line, " ") {
			section = strings.TrimSpace(line)
			continue
		}
		remote, ok := strings.CutPrefix(strings.TrimSpace(line), "remote: ")
		if section != "PATH" || !ok {
			continue
		}
		target := remote
		if !filepath.IsAbs(target) {
			target = filepath.Join(stackRoot, filepath.FromSlash(remote))
		}
		if rel, err := filepath.Rel(stackRoot, target); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("the bundle names the path gem at %s, outside %s: an image holds the application directory only", remote, stackRoot)
		}
	}
	return scanner.Err()
}

// installBundle installs the production bundle of the staged application into
// bundle, with Bundler's deployment settings and the Ruby the toolchain check
// selects. Bundler reads its local configuration from config, so neither the
// repository's .bundle nor the staged copy is read or written. With offline
// dependencies it installs from the gems on the machine and in the gem cache.
// The native extensions compile with the bundle directory mapped to the
// image's bundle path, so they do not record the temporary directory.
func installBundle(run context.Context, root stacks.RootStep, host rubyHost, staged, bundle, config, cache string, emit *jsonl.Emitter) error {
	step := root
	step.Dir = staged
	existing := ""
	for _, entry := range root.Env {
		if value, ok := strings.CutPrefix(entry, "CONFIGURE_ARGS="); ok {
			existing = value
		}
	}
	configure, mapped := configureArgs(existing, host.cppflags, bundle)
	if !mapped {
		emit.Warn("the native extensions record the temporary directory " + bundle + ", which a compiler flag cannot carry: two packagings give two images")
	}
	step.Env = append(slices.Clip(root.Env),
		"BUNDLE_GEMFILE="+filepath.Join(staged, "Gemfile"),
		"BUNDLE_APP_CONFIG="+config,
		"BUNDLE_PATH="+bundle,
		"BUNDLE_DEPLOYMENT=1",
		"BUNDLE_WITHOUT="+bundleWithout,
		"CONFIGURE_ARGS="+configure,
	)
	if cache != "" {
		step.Env = cacheEnv(step.Env, cache)
	}
	args := []string{"install"}
	if offlineDependencies() {
		args = append(args, "--local")
	}
	emit.Info(fmt.Sprintf("$ bundle %s with BUNDLE_DEPLOYMENT=1 BUNDLE_WITHOUT=%s in %s", strings.Join(args, " "), bundleWithout, staged))
	if err := runBundle(run, step, os.Stderr, args...); err != nil {
		return fmt.Errorf("bundle install: %w", err)
	}
	return nil
}

// configureArgs answers the CONFIGURE_ARGS under which mkmf compiles every
// native extension with -ffile-prefix-map from the bundle directory to the
// image's bundle path: the compiler writes the image's paths, not the
// temporary directory, in each extension's debug information, and so in its
// build ID. --with-cppflags replaces the Ruby's own preprocessor flags, so the
// value starts with them, after the arguments the environment already sets.
// It answers the existing arguments and false when the bundle path holds a
// character the flag cannot carry through a shell and a Makefile.
func configureArgs(existing, cppflags, bundle string) (string, bool) {
	existing = strings.TrimSpace(existing)
	if strings.ContainsFunc(bundle, func(r rune) bool { return !safePathRune(r) }) {
		return existing, false
	}
	flags := strings.TrimSpace(cppflags + " -ffile-prefix-map=" + bundle + "=" + imageBundlePath)
	arg := "--with-cppflags='" + strings.ReplaceAll(flags, "'", `'\''`) + "'"
	if existing != "" {
		return existing + " " + arg, true
	}
	return arg, true
}

// safePathRune reports whether a path character passes through a shell, a
// Makefile and a compiler flag unchanged.
func safePathRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+", r)
}

// sharedExtensionDirs renames the extension directories a static Ruby writes,
// extensions/<platform>/<api>-static, to the name a shared Ruby, such as the
// official image's, reads.
func sharedExtensionDirs(bundle string, host rubyHost) error {
	shared, static := strings.CutSuffix(host.extensionAPI, "-static")
	if !static {
		return nil
	}
	dirs, err := filepath.Glob(filepath.Join(bundle, "ruby", "*", "extensions", "*", host.extensionAPI))
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		if err := os.Rename(dir, filepath.Join(filepath.Dir(dir), shared)); err != nil {
			return err
		}
	}
	return nil
}

// copyTree copies a stack root without the staging excludes and vendor/bundle
// at its top level, and the files that hold secrets.
func copyTree(source, target string) error {
	return filepath.WalkDir(source, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, file)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(target, 0o755)
		}
		slashed := filepath.ToSlash(rel)
		if (!strings.Contains(slashed, "/") && slices.Contains(stagingExcludes, entry.Name())) || slashed == "vendor/bundle" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.IsDir() && secretFile(slashed) {
			return nil
		}
		destination := filepath.Join(target, rel)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o755)
		}
		return copyFile(file, destination)
	})
}

// secretFile reports the files no image holds, by their path under the stack
// root: Rails's master key and per-environment credential keys, and every
// .env file at any depth.
func secretFile(rel string) bool {
	if strings.HasPrefix(path.Base(rel), ".env") || rel == "config/master.key" {
		return true
	}
	matched, _ := path.Match("config/credentials/*.key", rel)
	return matched
}

// copyFile copies a file's content and its executable bit. A symbolic link is
// an error: the image's cache key does not cover what a link points to.
func copyFile(source, target string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symbolic link, which an image cannot hold: replace it with the file", source)
	}
	data, err := os.ReadFile(source) //nolint:gosec // inside the stack root
	if err != nil {
		return err
	}
	return os.WriteFile(target, data, fileMode(info)) //nolint:gosec // a staged copy
}

// fileMode answers the mode an image gives a file: the executable bit is the
// only part of a local mode that Git keeps.
func fileMode(info fs.FileInfo) fs.FileMode {
	if info.Mode()&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

// writeWritableDirs writes a tar of the application directories the image's
// user writes, owned by that user, with the fixed time every layer has.
func writeWritableDirs(file string) error {
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, dir := range writableDirs {
		if err := writer.WriteHeader(&tar.Header{
			Typeflag: tar.TypeDir,
			Name:     strings.TrimPrefix(imageAppDir, "/") + "/" + dir + "/",
			Mode:     0o755,
			Uid:      imageUID,
			Gid:      imageUID,
			ModTime:  time.Unix(0, 0).UTC(),
		}); err != nil {
			return err
		}
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return os.WriteFile(file, buffer.Bytes(), 0o600)
}

// imageSpec describes the application's image.
func imageSpec(staged, bundle, writable, base, platform, port string) (oci.Spec, error) {
	dependencies, err := imageFiles(bundle, imageBundlePath, bundleFileKept)
	if err != nil {
		return oci.Spec{}, err
	}
	files, err := imageFiles(staged, imageAppDir, func(string) bool { return true })
	if err != nil {
		return oci.Spec{}, err
	}
	return oci.Spec{
		BaseRef:  base,
		Platform: platform,
		Layers:   []oci.Layer{{Files: dependencies}, {Files: files}, {Tarball: writable}},
		Env: []string{
			"PORT=" + port,
			"RAILS_ENV=production", "RACK_ENV=production",
			"RAILS_LOG_TO_STDOUT=1", "RAILS_SERVE_STATIC_FILES=1",
			"BUNDLE_DEPLOYMENT=1", "BUNDLE_PATH=" + imageBundlePath, "BUNDLE_WITHOUT=" + bundleWithout,
			// The image runs the Bundler it ships, whatever version the
			// lockfile names, so no Bundler is downloaded at boot.
			"BUNDLE_VERSION=system",
			// The user has no home directory; Bundler and the gems write
			// theirs under /tmp.
			"HOME=/tmp",
		},
		Entrypoint:   []string{"/bin/sh", "-c", "exec bundle exec " + strings.Join(imageServer(staged), " ")},
		WorkingDir:   imageAppDir,
		User:         imageUser,
		ExposedPorts: []string{port + "/tcp"},
	}, nil
}

// buildLogs are the files RubyGems and mkmf write while they build a native
// extension. They record the temporary directory, and nothing reads them at
// run time.
var buildLogs = []string{"gem_make.out", "mkmf.log"}

// bundleFileKept reports whether the image holds the file or directory at rel,
// a slash path under the bundle directory. The image runs none of what it
// leaves out: the .gem archives Bundler installed, the Git history of a git
// source, and what a native extension's build leaves behind, its logs, and
// the Makefile and object files under the gem's ext directory.
func bundleFileKept(rel string) bool {
	parts := strings.Split(rel, "/")
	if len(parts) < 3 || parts[0] != "ruby" {
		return true
	}
	name := parts[len(parts)-1]
	gem := 0
	switch {
	case parts[2] == "cache":
		return false
	case parts[2] == "extensions":
		return !slices.Contains(buildLogs, name)
	case parts[2] == "gems":
		gem = 3
	case len(parts) > 3 && parts[2] == "bundler" && parts[3] == "gems":
		if len(parts) > 5 && parts[5] == ".git" {
			return false
		}
		gem = 4
	default:
		return true
	}
	if len(parts) < gem+3 || parts[gem+1] != "ext" {
		return true
	}
	return name != "Makefile" && !slices.Contains(buildLogs, name) && !strings.HasSuffix(name, ".o")
}

// imageServer is the server command of the image, on the PORT its shell reads.
func imageServer(staged string) []string {
	if isFile(filepath.Join(staged, filepath.FromSlash(railsExecutable))) {
		return []string{"rails", "server", "-b", "0.0.0.0", "-p", `"$PORT"`}
	}
	return []string{"rackup", "-o", "0.0.0.0", "-p", `"$PORT"`}
}

// imageFiles lists the files under dir whose slash path keep accepts, at the
// same path under prefix in the image.
func imageFiles(dir, prefix string, keep func(rel string) bool) ([]oci.File, error) {
	var files []oci.File
	err := filepath.WalkDir(dir, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, file)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !keep(rel) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			if info, err = os.Stat(file); err != nil {
				return err
			}
			if info.IsDir() {
				return fmt.Errorf("%s is a link to a directory, which an image cannot hold", rel)
			}
		}
		files = append(files, oci.File{Source: file, Path: prefix + "/" + rel, Mode: int64(fileMode(info))})
		return nil
	})
	return files, err
}

// loadImage loads the assembled image into the local Docker daemon under its
// tags.
func loadImage(layout string, tags []string) error {
	archive, err := os.CreateTemp("", "putnami-ruby-image-*.tar")
	if err != nil {
		return err
	}
	_ = archive.Close()
	defer func() { _ = os.Remove(archive.Name()) }()
	if err := oci.WriteDockerTarball(layout, tags[0], archive.Name()); err != nil {
		return err
	}
	load := exec.Command("docker", "load", "-i", archive.Name()) //nolint:gosec // fixed program, a path from CreateTemp
	load.Stderr = os.Stderr
	if err := load.Run(); err != nil {
		return errors.New("docker load failed for " + tags[0])
	}
	for _, alias := range tags[1:] {
		tag := exec.Command("docker", "tag", tags[0], alias) //nolint:gosec // fixed program
		tag.Stderr = os.Stderr
		if err := tag.Run(); err != nil {
			return errors.New("docker tag failed for " + alias)
		}
	}
	return nil
}
