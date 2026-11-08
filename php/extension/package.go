package php

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

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
	CodeImageFailed = "php.image_failed"
	// CodeBaseNotPinned is the diagnostic of a base image with no digest.
	CodeBaseNotPinned = "php.base_not_pinned"
)

const (
	dockerChannel    = "docker"
	imageAppDir      = "/var/www/html"
	imageUser        = "www-data"
	defaultImagePort = "8080"
	defaultPlatform  = "linux/amd64"
	vendorDir        = "vendor"
)

// apacheBases are the official PHP Apache images, pinned by digest, by PHP
// minor version.
var apacheBases = map[string]string{
	"8.3": "php:8.3.32-apache@sha256:a05f87f7f1e3927b9f3a44d64c01dfe15992328fa179bdfa72ad06e66769ff57",
	"8.4": "php:8.4.23-apache@sha256:ea61837b6dee7f5981cced9fdf6a7773c4ef695bc3237fb9da59d223df638ec2",
	"8.5": "php:8.5.8-apache@sha256:eacc0d98992683cb46e4f8f44b2418a0323855dc8b59d32dc54f7a9b90a966dd",
}

// composerAuth is the file that holds a project's registry tokens. Composer
// reads it at the stack root while it installs, and no image holds it.
const composerAuth = "auth.json"

// stagingExcludes are the top-level names of a stack root an image leaves out:
// the installed dependencies, generated files, and what only tests read.
var stagingExcludes = []string{".gen", ".git", ".phpunit.cache", ".putnami", "node_modules", "tests", "var", vendorDir}

// apachePorts and apacheSite replace the two Apache files that name the port
// and the document root. FallbackResource sends a path that is no file to the
// front controller without mod_rewrite, which the official image leaves off.
const (
	apachePorts = "Listen ${PORT}\n"
	apacheSite  = `<VirtualHost *:${PORT}>
	DocumentRoot ` + imageAppDir + `/public
	<Directory ` + imageAppDir + `/public>
		AllowOverride None
		Require all granted
		FallbackResource /index.php
	</Directory>
</VirtualHost>
`
)

// PublishDockerJob pushes the packaged image through the shared publisher.
func PublishDockerJob() cli.JobFunc {
	return dockerpublish.Publish
}

// PackageDockerJob assembles the image of an application: a layer of its
// production dependencies, a layer of its files, and a layer of Apache
// configuration, on a digest-pinned PHP Apache base. It needs no Docker daemon.
func PackageDockerJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		if ctx.Project.Type == "library" {
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
		return fail(CodeImageFailed, fmt.Errorf("%s has its stack root at %s: an image packages an application at its own stack root", root.Project, root.Dir))
	}
	if _, err := os.Stat(filepath.Join(root.Project, filepath.FromSlash(frontController))); err != nil {
		return fail(CodeNoFrontController, fmt.Errorf("%s has no %s: the image has no front controller", root.Project, frontController))
	}
	if !hasLock(root.Dir) {
		return fail(CodeImageFailed, fmt.Errorf("%s has no %s: an image installs the locked dependencies", root.Dir, composerLock))
	}
	base, err := baseImage(run, root, cli.FlagString(flags, "docker-base-image", ctx.Params.String("docker-base-image", "dockerBaseImage")))
	if err != nil {
		return fail(CodeBaseNotPinned, err)
	}

	output := filepath.Join(ctx.WorkspaceRoot, ".putnami", "out", ctx.Project.Path, "package", dockerChannel)
	name := imageName(ctx.Project.Name)
	version := "0.0.0"
	if ctx.Version != nil && ctx.Version.Full != "" {
		version = ctx.Version.Full
	}
	record := func() error {
		return pkgmeta.WriteChannelRecord(output, pkgmeta.ChannelRecord{Version: version, Artifact: name, Channels: []string{dockerChannel}})
	}
	if cli.FlagBool(flags, "dry-run", ctx.Params.Bool("dry-run", false, "dryRun")) {
		emit.Info("dry run: would assemble an image of " + name + " on " + base)
		if err := record(); err != nil {
			return fail(CodeImageFailed, err)
		}
		return nil
	}

	workspace, err := os.MkdirTemp("", "putnami-php-image-")
	if err != nil {
		return fail(CodeImageFailed, err)
	}
	defer func() { _ = os.RemoveAll(workspace) }()
	staged, err := stageApplication(ctx.WorkspaceRoot, root.Dir, workspace)
	if err != nil {
		return fail(CodeImageFailed, err)
	}
	phar, err := provideComposer(run)
	if err != nil {
		return fail(CodeImageFailed, err)
	}
	stagedRoot := root
	stagedRoot.Dir = staged
	env := append(composerEnv(root.Env), "COMPOSER_MIRROR_PATH_REPOS=1")
	emit.Info("$ composer install --no-dev --no-scripts --optimize-autoloader in " + staged)
	if err := runComposer(run, stagedRoot, env, phar, "install", "--no-interaction", "--no-progress", "--no-dev", "--no-scripts", "--optimize-autoloader"); err != nil {
		return fail(CodeImageFailed, fmt.Errorf("composer install: %w", err))
	}
	platform := cli.FlagString(flags, "platform", ctx.Params.String("platform"))
	if platform == "" {
		platform = defaultPlatform
	}
	spec, err := imageSpec(staged, workspace, base, platform, defaultImagePort)
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

// baseImage answers the named base, which must carry a digest, or the official
// image of the PHP minor version the machine resolved the dependencies with.
func baseImage(run context.Context, root stacks.RootStep, named string) (string, error) {
	if named != "" {
		if _, err := oci.ParseDigestReference(named); err != nil {
			return "", fmt.Errorf("the base image %s has no valid digest: name it as image@sha256:<digest>: %w", named, err)
		}
		return named, nil
	}
	cmd := exec.CommandContext(run, root.Program("php"), "-r", `echo PHP_MAJOR_VERSION.".".PHP_MINOR_VERSION;`) //nolint:gosec // the selected PHP
	cmd.Dir = root.Dir
	cmd.Env = root.Env
	minor, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("read the PHP version: %w", err)
	}
	base, ok := apacheBases[strings.TrimSpace(string(minor))]
	if !ok {
		return "", fmt.Errorf("no official image is pinned for PHP %s: name a digest-pinned base with --docker-base-image", strings.TrimSpace(string(minor)))
	}
	return base, nil
}

// stageApplication copies the stack root into workspace at its own path, and
// each path repository outside it at its own path, so that the relative URLs
// of composer.json resolve. It answers the staged stack root.
func stageApplication(workspaceRoot, stackRoot, workspace string) (string, error) {
	rel, err := filepath.Rel(workspaceRoot, stackRoot)
	if err != nil {
		return "", err
	}
	staged := filepath.Join(workspace, rel)
	if err := copyTree(stackRoot, staged); err != nil {
		return "", err
	}
	auth := filepath.Join(stackRoot, composerAuth)
	if _, err := os.Stat(auth); err == nil {
		if err := copyFile(auth, filepath.Join(staged, composerAuth)); err != nil {
			return "", err
		}
	}
	repositories, err := pathRepositories(stackRoot)
	if err != nil {
		return "", err
	}
	for _, repository := range repositories {
		relative, err := filepath.Rel(workspaceRoot, repository)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("the path repository %s is outside the workspace", repository)
		}
		within, err := filepath.Rel(stackRoot, repository)
		if err == nil && within != ".." && !strings.HasPrefix(within, ".."+string(filepath.Separator)) {
			continue
		}
		if err := copyTree(repository, filepath.Join(workspace, relative)); err != nil {
			return "", err
		}
	}
	return staged, nil
}

// pathRepositories answers the directories of the path repositories the
// composer.json of root names.
func pathRepositories(root string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(root, "composer.json")) //nolint:gosec // inside the stack root
	if err != nil {
		return nil, err
	}
	var manifest struct {
		Repositories json.RawMessage `json:"repositories"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("composer.json: %w", err)
	}
	var repositories []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	}
	// Composer also takes an object here, which names no path repository this
	// job reads.
	if json.Unmarshal(manifest.Repositories, &repositories) != nil {
		return nil, nil
	}
	var found []string
	for _, repository := range repositories {
		if repository.Type != "path" {
			continue
		}
		matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(repository.URL)))
		if err != nil {
			return nil, fmt.Errorf("the path repository %s: %w", repository.URL, err)
		}
		found = append(found, matches...)
	}
	return found, nil
}

// copyTree copies a tree, without the staging excludes at its top level and
// the files that hold secrets, at any depth.
func copyTree(source, target string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(target, 0o755)
		}
		if strings.Count(rel, string(filepath.Separator)) == 0 && slices.Contains(stagingExcludes, entry.Name()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.IsDir() && secretFile(entry.Name()) {
			return nil
		}
		destination := filepath.Join(target, rel)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o755)
		}
		return copyFile(path, destination)
	})
}

// secretFile reports the files no image holds: Composer's registry tokens,
// Symfony's local environment files and its production decryption keys.
func secretFile(name string) bool {
	return name == composerAuth || name == ".env.local" || name == ".env.local.php" ||
		(strings.HasPrefix(name, ".env.") && strings.HasSuffix(name, ".local")) ||
		strings.HasSuffix(name, ".decrypt.private.php")
}

// copyFile copies a file's content and its executable bit, following a
// symbolic link to a file. A link to a directory is an error: nothing can tell
// what the image should hold.
func copyFile(source, target string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a link to a directory, which an image cannot hold", source)
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

// imageSpec describes the application's image.
func imageSpec(staged, workspace, base, platform, port string) (oci.Spec, error) {
	dependencies, err := imageFiles(staged, func(rel string) bool {
		return rel == vendorDir || strings.HasPrefix(rel, vendorDir+string(filepath.Separator))
	})
	if err != nil {
		return oci.Spec{}, err
	}
	files, err := imageFiles(staged, func(rel string) bool {
		return rel != composerAuth && rel != vendorDir && !strings.HasPrefix(rel, vendorDir+string(filepath.Separator))
	})
	if err != nil {
		return oci.Spec{}, err
	}
	configuration := filepath.Join(workspace, "apache")
	if err := os.MkdirAll(configuration, 0o755); err != nil {
		return oci.Spec{}, err
	}
	var apache []oci.File
	for name, content := range map[string]struct{ text, path string }{
		"ports.conf": {apachePorts, "/etc/apache2/ports.conf"},
		"site.conf":  {apacheSite, "/etc/apache2/sites-enabled/000-default.conf"},
	} {
		source := filepath.Join(configuration, name)
		if err := os.WriteFile(source, []byte(content.text), 0o644); err != nil { //nolint:gosec // a staged copy
			return oci.Spec{}, err
		}
		apache = append(apache, oci.File{Source: source, Path: content.path, Mode: 0o644})
	}
	slices.SortFunc(apache, func(a, b oci.File) int { return strings.Compare(a.Path, b.Path) })
	return oci.Spec{
		BaseRef:  base,
		Platform: platform,
		Layers:   []oci.Layer{{Files: dependencies}, {Files: files}, {Files: apache}},
		// The application is read-only for www-data, so Symfony writes its
		// cache and logs where it can.
		Env:          []string{"PORT=" + port, "APP_ENV=prod", "APP_CACHE_DIR=/tmp/app/cache", "APP_LOG_DIR=/tmp/app/log"},
		Entrypoint:   []string{"docker-php-entrypoint", "apache2-foreground"},
		WorkingDir:   imageAppDir,
		User:         imageUser,
		ExposedPorts: []string{port + "/tcp"},
	}, nil
}

// imageFiles lists the staged files whose path keep accepts.
func imageFiles(staged string, keep func(rel string) bool) ([]oci.File, error) {
	var files []oci.File
	err := filepath.WalkDir(staged, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(staged, path)
		if err != nil || rel == "." || entry.IsDir() || !keep(rel) {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			if info, err = os.Stat(path); err != nil {
				return err
			}
			if info.IsDir() {
				return fmt.Errorf("%s is a link to a directory, which an image cannot hold", rel)
			}
		}
		files = append(files, oci.File{Source: path, Path: imageAppDir + "/" + filepath.ToSlash(rel), Mode: int64(fileMode(info))})
		return nil
	})
	return files, err
}

// loadImage loads the assembled image into the local Docker daemon under its
// tags.
func loadImage(layout string, tags []string) error {
	archive, err := os.CreateTemp("", "putnami-php-image-*.tar")
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
