package swift

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/dockerpublish"
	"go.putnami.dev/sdk/extension/filelock"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/oci"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/putnamihome"
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
	CodeImageFailed = "swift.image_failed"
	// CodeBaseNotPinned is the diagnostic of a base image with no digest.
	CodeBaseNotPinned = "swift.base_not_pinned"
	// CodeStaticSDKUnavailable is the diagnostic of a Swift whose Static Linux
	// SDK the extension does not pin, or cannot use.
	CodeStaticSDKUnavailable = "swift.static_sdk_unavailable"
)

const (
	dockerChannel    = "docker"
	imageAppDir      = "/app"
	defaultImagePort = "8080"
	defaultPlatform  = "linux/amd64"
	// imageUser is the nonroot user of distroless, by number, so that any base
	// runs the executable as a non-root user.
	imageUser = "65532:65532"
	// staticBase is the distroless base of a static binary, pinned by digest:
	// it holds CA certificates and time zones, and runs as nonroot.
	staticBase = "gcr.io/distroless/static:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240"
)

// staticSDKs are the Static Linux SDK of each Swift release the extension
// supports: the SDK version of its artifact bundle and the bundle's SHA-256,
// as swift.org publishes them.
var staticSDKs = map[string]struct{ version, checksum string }{
	"6.3":   {"0.1.0", "d2078b69bdeb5c31202c10e9d8a11d6f66f82938b51a4b75f032ccb35c4c286c"},
	"6.3.1": {"0.1.0", "fac05271c1f7d060bd203240ce5251d5ca902d30ac899f553765dbb3a88b97ad"},
	"6.3.2": {"0.1.0", "3fd798bef6f4408f1ea5a6f94ce4d4052830c4326ab85ebc04f983f01b3da407"},
	"6.3.3": {"0.1.0", "87c3eaf908e67c0e13a84367119e12273cec1d2cd3d81f7d74bb36722d6b607b"},
	"6.4.0": {"0.1.0", "47d2fd89eebfdf9eb4d536b6710414297f755c17926cdebc4742c08982b40a9e"},
	"6.4.1": {"0.1.0", "cef5592d7430ed3c5ae1ddda479849c6f5252bd94a5723db1559f984caf38a7c"},
	"6.4.2": {"0.1.0", "3786b179936d9cc9e266dd506bd1654176358d7b25288e243359f4d17cc4dc5a"},
}

// staticTriples are the Swift SDK target triples of the image platforms.
var staticTriples = map[string]string{
	"linux/amd64": "x86_64-swift-linux-musl",
	"linux/arm64": "aarch64-swift-linux-musl",
}

// swiftRelease reads the swift.org release of `swift --version`: the tag in
// parentheses, such as (swift-6.4.2-RELEASE). The Swift of Xcode names a
// swiftlang build instead.
var swiftRelease = regexp.MustCompile(`\(swift-([0-9][0-9.]*)-RELEASE\)`)

// staticSDK names the artifact bundle of a release's Static Linux SDK.
type staticSDK struct {
	ID, URL, Checksum string
}

// staticSDKFor answers the pinned Static Linux SDK of a Swift release.
func staticSDKFor(release string) (staticSDK, error) {
	pin, ok := staticSDKs[release]
	if !ok {
		releases := make([]string, 0, len(staticSDKs))
		for r := range staticSDKs {
			releases = append(releases, r)
		}
		sort.Strings(releases)
		return staticSDK{}, fmt.Errorf("no Static Linux SDK is pinned for Swift %s: select one of %s", release, strings.Join(releases, ", "))
	}
	tag := "swift-" + release + "-RELEASE"
	id := tag + "_static-linux-" + pin.version
	return staticSDK{
		ID:       id,
		URL:      "https://download.swift.org/swift-" + release + "-release/static-sdk/" + tag + "/" + id + ".artifactbundle.tar.gz",
		Checksum: pin.checksum,
	}, nil
}

// PublishDockerJob pushes the packaged image of an application through the
// shared publisher. A library or an Xcode project publishes nothing.
func PublishDockerJob() cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		if !imageable(ctx) {
			return "SKIP", nil, nil
		}
		return dockerpublish.Publish(ctx, emit, args)
	}
}

// imageable reports whether the job's component may have an image: a package
// that is not a library.
func imageable(ctx *pctx.Context) bool {
	return ctx.Project.Type != "library" && isPackage(ctx.Project.FullPath)
}

// PackageDockerJob assembles the image of an application: its executable
// product, built static with the Static Linux SDK, and the resource bundles
// beside it, on a digest-pinned distroless base. It needs no Docker daemon.
// It builds under the lock of the stack root and copies what it built out of
// the build directory before it releases the lock. A library or an Xcode
// project packages nothing.
func PackageDockerJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		if !imageable(ctx) {
			return "SKIP", nil, nil
		}
		if err := packageImage(stack, ctx, cli.ParseFlags(args), emit); err != nil {
			return "FAILED", nil, err
		}
		return "OK", nil, nil
	}
}

func packageImage(stack stacks.Stack, ctx *pctx.Context, flags map[string]string, emit *jsonl.Emitter) error {
	base := cli.FlagString(flags, "docker-base-image", ctx.Params.String("docker-base-image", "dockerBaseImage"))
	if base == "" {
		base = staticBase
	} else if _, err := oci.ParseDigestReference(base); err != nil {
		return fail(emit, CodeBaseNotPinned, fmt.Errorf("the base image %s has no valid digest: name it as image@sha256:<digest>: %w", base, err))
	}
	platform := cli.FlagString(flags, "platform", ctx.Params.String("platform"))
	if platform == "" {
		platform = defaultPlatform
	}
	triple, ok := staticTriples[platform]
	if !ok {
		return fail(emit, CodeImageFailed, fmt.Errorf("the platform %s has no Static Linux SDK target: use linux/amd64 or linux/arm64", platform))
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
		emit.Info("dry run: would assemble an image of " + name + " for " + platform + " on " + base)
		if err := record(); err != nil {
			return fail(emit, CodeImageFailed, err)
		}
		return nil
	}

	staging, err := os.MkdirTemp("", "putnami-swift-image-")
	if err != nil {
		return fail(emit, CodeImageFailed, err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	var product string
	err = stack.Locked(ctx, emit, func(run context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
		if product, err = executableAt(root, ctx, emit); err != nil {
			return err
		}
		return buildStatic(run, root, product, triple, staging, emit)
	})
	if err != nil {
		return err
	}
	return assembleImage(ctx, flags, staging, product, base, platform, output, name, version, record, emit)
}

// buildStatic builds the executable product with the pinned Static Linux SDK
// of the selected Swift for triple, and copies it and the resource bundles
// beside it into staging.
func buildStatic(run context.Context, root stacks.RootStep, product, triple, staging string, emit *jsonl.Emitter) error {
	sdk, err := selectStaticSDK(run, root, emit)
	if err != nil {
		return err
	}
	// The bin path keeps the resource bundles of every product built before,
	// so the job removes them and the build writes those of this product.
	selection := []string{"-c", "release", "--swift-sdk", sdk.ID, "--triple", triple}
	binPath, err := swiftOutput(run, root, append(append([]string{"build"}, selection...), "--show-bin-path")...)
	if err != nil {
		return fail(emit, CodeImageFailed, err)
	}
	binPath = strings.TrimSpace(lastLine(binPath))
	if err := removeResourceBundles(binPath); err != nil {
		return fail(emit, CodeImageFailed, err)
	}
	build := append(append([]string{"build"}, selection...), "--force-resolved-versions", "--product", product, "-Xlinker", "-s")
	emit.Info("$ swift " + strings.Join(build, " "))
	if _, err := swiftOutput(run, root, build...); err != nil {
		return fail(emit, CodeImageFailed, err)
	}
	if err := stageBuild(binPath, product, staging); err != nil {
		return fail(emit, CodeImageFailed, err)
	}
	return nil
}

// assembleImage assembles the staged executable and bundles into the OCI
// layout of output, records it, and loads it into Docker when asked.
func assembleImage(ctx *pctx.Context, flags map[string]string, staging, product, base, platform, output, name, version string, record func() error, emit *jsonl.Emitter) error {
	spec, err := imageSpec(staging, product, base, platform, defaultImagePort)
	if err != nil {
		return fail(emit, CodeImageFailed, err)
	}
	plan, err := oci.NewImagePlan(spec, oci.PlanOptions{})
	if err != nil {
		return fail(emit, CodeImageFailed, fmt.Errorf("plan the image: %w", err))
	}
	digest, err := oci.AssemblePlanToLayout(plan, filepath.Join(output, "oci"), oci.LayoutOptions{
		BaseCacheDir:  filepath.Join(ctx.WorkspaceRoot, ".putnami", "cache", "oci-base"),
		LayerCacheDir: oci.LayerCacheRootFromEnv(),
	})
	if err != nil {
		return fail(emit, CodeImageFailed, fmt.Errorf("assemble the image: %w", err))
	}
	contentHash := plan.Identity[:12]
	tags := []string{name + ":c-" + contentHash}
	if tag := cli.FlagString(flags, "docker-tag", ctx.Params.String("docker-tag", "dockerTag")); tag != "" {
		tags = append(tags, name+":"+tag)
	}
	if cli.FlagBool(flags, "docker-load", ctx.Params.Bool("docker-load", false, "dockerLoad")) {
		if err := loadImage(filepath.Join(output, "oci"), tags); err != nil {
			return fail(emit, CodeImageFailed, err)
		}
	}
	manifest, err := json.MarshalIndent(pkgmeta.DockerManifest{
		Image: name, Tags: tags, Version: version, ContentHash: contentHash, Digest: digest, Layout: "oci", Platform: spec.Platform,
	}, "", "  ")
	if err != nil {
		return fail(emit, CodeImageFailed, err)
	}
	if err := os.WriteFile(filepath.Join(output, "manifest.json"), append(manifest, '\n'), 0o644); err != nil { //nolint:gosec // an output of the job
		return fail(emit, CodeImageFailed, err)
	}
	if err := record(); err != nil {
		return fail(emit, CodeImageFailed, err)
	}
	emit.Info(fmt.Sprintf("assembled the image %s (%s)", tags[0], digest))
	return nil
}

// selectStaticSDK answers the pinned Static Linux SDK of the selected Swift,
// and installs it with its checksum when swift sdk list does not name it.
func selectStaticSDK(run context.Context, root stacks.RootStep, emit *jsonl.Emitter) (staticSDK, error) {
	version, err := swiftOutput(run, root, "--version")
	if err != nil {
		return staticSDK{}, fail(emit, CodeStaticSDKUnavailable, err)
	}
	m := swiftRelease.FindStringSubmatch(version)
	if m == nil {
		return staticSDK{}, fail(emit, CodeStaticSDKUnavailable, fmt.Errorf("%s is no swift.org release, and only a swift.org toolchain builds with the Static Linux SDK: select one with swiftly or the swift.org installer", strings.TrimSpace(firstLine(version))))
	}
	sdk, err := staticSDKFor(m[1])
	if err != nil {
		return staticSDK{}, fail(emit, CodeStaticSDKUnavailable, err)
	}
	installed, err := swiftOutput(run, root, "sdk", "list")
	if err != nil {
		return staticSDK{}, fail(emit, CodeStaticSDKUnavailable, err)
	}
	if slices.Contains(strings.Fields(installed), sdk.ID) {
		return sdk, nil
	}
	// One install of a bundle runs at a time on the machine: a job that waited
	// finds it installed.
	lock, err := lockSDKInstall(root, sdk.ID)
	if err != nil {
		return staticSDK{}, fail(emit, CodeStaticSDKUnavailable, err)
	}
	defer func() { _ = lock.Release() }()
	if installed, err = swiftOutput(run, root, "sdk", "list"); err != nil {
		return staticSDK{}, fail(emit, CodeStaticSDKUnavailable, err)
	}
	if slices.Contains(strings.Fields(installed), sdk.ID) {
		return sdk, nil
	}
	emit.Info("$ swift sdk install " + sdk.URL + " --checksum " + sdk.Checksum)
	if _, err := swiftOutput(run, root, "sdk", "install", sdk.URL, "--checksum", sdk.Checksum); err != nil {
		return staticSDK{}, fail(emit, CodeStaticSDKUnavailable, err)
	}
	return sdk, nil
}

// lockSDKInstall takes the machine's lock on the install of a Swift SDK
// bundle, under locks/ of the Putnami home, or of the system's temporary
// directory when the environment names no home.
func lockSDKInstall(root stacks.RootStep, id string) (*filelock.Lock, error) {
	lookup := func(key string) string {
		prefix := key + "="
		for i := len(root.Env) - 1; i >= 0; i-- {
			if strings.HasPrefix(root.Env[i], prefix) {
				return root.Env[i][len(prefix):]
			}
		}
		return ""
	}
	home := putnamihome.Resolve(lookup)
	if home == "" {
		home = filepath.Join(os.TempDir(), "putnami")
	}
	dir := filepath.Join(home, "locks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return filelock.Acquire(filepath.Join(dir, "swift-sdk-"+id+".lock"), true, false)
}

// removeResourceBundles removes the .resources and .bundle directories of a
// bin path. A bin path that does not exist yet holds none.
func removeResourceBundles(binPath string) error {
	entries, err := os.ReadDir(binPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() && isResourceBundle(entry.Name()) {
			if err := os.RemoveAll(filepath.Join(binPath, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func isResourceBundle(name string) bool {
	return strings.HasSuffix(name, ".resources") || strings.HasSuffix(name, ".bundle")
}

// swiftOutput runs the selected swift in the package directory and answers
// its standard output; its standard error goes to stderr.
func swiftOutput(run context.Context, root stacks.RootStep, args ...string) (string, error) {
	cmd := exec.CommandContext(run, root.Program("swift"), args...) //nolint:gosec // the selected Swift
	cmd.Dir = root.Project
	cmd.Env = root.Env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("swift %s: %w", strings.Join(args, " "), err)
	}
	return out.String(), nil
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return line
}

// stageBuild copies the executable product and the resource bundles of a bin
// path into staging, with the same names.
func stageBuild(binPath, product, staging string) error {
	executable := filepath.Join(binPath, product)
	info, err := os.Stat(executable)
	if err != nil || info.IsDir() {
		return fmt.Errorf("swift build wrote no executable %s", executable)
	}
	if err := copyFile(executable, filepath.Join(staging, product), 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(binPath)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !isResourceBundle(entry.Name()) {
			continue
		}
		bundle := filepath.Join(binPath, entry.Name())
		err := filepath.WalkDir(bundle, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			info, err := os.Stat(path)
			if err != nil {
				return err
			}
			if info.IsDir() {
				return fmt.Errorf("%s is a link to a directory, which an image cannot hold", path)
			}
			rel, err := filepath.Rel(binPath, path)
			if err != nil {
				return err
			}
			mode := fs.FileMode(0o644)
			if info.Mode()&0o111 != 0 {
				mode = 0o755
			}
			return copyFile(path, filepath.Join(staging, rel), mode)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// copyFile copies the content of a file, through a link, with mode.
func copyFile(source, target string, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	data, err := os.ReadFile(source) //nolint:gosec // a product of the job's build
	if err != nil {
		return err
	}
	return os.WriteFile(target, data, mode) //nolint:gosec // a staged copy
}

// imageSpec describes the application's image: the executable at
// /app/<product>, and each resource bundle SwiftPM wrote beside it under
// /app, where Bundle.module finds it, from the staged build.
func imageSpec(staged, product, base, platform, port string) (oci.Spec, error) {
	executable := filepath.Join(staged, product)
	info, err := os.Stat(executable)
	if err != nil || info.IsDir() {
		return oci.Spec{}, fmt.Errorf("no executable %s was staged", executable)
	}
	entrypoint := imageAppDir + "/" + product
	files := []oci.File{{Source: executable, Path: entrypoint, Mode: 0o755}}
	resources, err := resourceBundles(staged)
	if err != nil {
		return oci.Spec{}, err
	}
	layers := []oci.Layer{{Files: files}}
	if len(resources) > 0 {
		layers = append([]oci.Layer{{Files: resources}}, layers...)
	}
	return oci.Spec{
		BaseRef:      base,
		Platform:     platform,
		Layers:       layers,
		Env:          []string{"PORT=" + port},
		Entrypoint:   []string{entrypoint},
		WorkingDir:   imageAppDir,
		User:         imageUser,
		ExposedPorts: []string{port + "/tcp"},
	}, nil
}

// resourceBundles lists the files of the .resources and .bundle directories of
// a staged build, the resource bundles of the product's targets.
func resourceBundles(staged string) ([]oci.File, error) {
	entries, err := os.ReadDir(staged)
	if err != nil {
		return nil, err
	}
	var files []oci.File
	for _, entry := range entries {
		if !entry.IsDir() || !isResourceBundle(entry.Name()) {
			continue
		}
		err := filepath.WalkDir(filepath.Join(staged, entry.Name()), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			info, err := os.Stat(path)
			if err != nil {
				return err
			}
			if info.IsDir() {
				return fmt.Errorf("%s is a link to a directory, which an image cannot hold", path)
			}
			rel, err := filepath.Rel(staged, path)
			if err != nil {
				return err
			}
			mode := int64(0o644)
			if info.Mode()&0o111 != 0 {
				mode = 0o755
			}
			files = append(files, oci.File{Source: path, Path: imageAppDir + "/" + filepath.ToSlash(rel), Mode: mode})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return files, nil
}

// imageName derives an image name from a project name: an image name is one
// lowercase path segment.
func imageName(project string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(project, "@"), "/", "-"))
}

// loadImage loads the assembled image into the local Docker daemon under its
// tags.
func loadImage(layout string, tags []string) error {
	archive, err := os.CreateTemp("", "putnami-swift-image-*.tar")
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
